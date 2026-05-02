//! 智能丢包策略 — 背压时优先丢弃 P 帧, 保留关键帧/音频
//!
//! 参考 livego 的 DropPacket 设计:
//!   - 当客户端消费太慢 (背压/广播积压), 需要丢包缓解
//!   - 优先级: 关键帧 > 音频 > P/B 帧
//!   - 关键帧 (IDR + SPS/PPS) 绝不丢弃, 否则播放器黑屏
//!   - 音频一般不丢弃 (听感比画质更敏感)
//!   - P/B 帧首先丢弃 (丢失后仅短暂花屏, 下一个关键帧恢复)
//!
//! 背压检测:
//!   - broadcast::RecvError::Lagged → 客户端消费太慢
//!   - TCP send buffer 满 → write 阻塞
//!   - WS send 失败 → 连接级背压
//!
//! 策略:
//!   - 轻度背压 (lag < 10): 仅丢 P 帧
//!   - 中度背压 (lag 10-50): 丢 P 帧 + 非关键音频
//!   - 重度背压 (lag > 50): 丢弃直到下一个关键帧, 然后正常

use super::flv::{FlvTagPacket, FlvTagType};
use super::gop_cache::{parse_audio_frame_type, parse_video_frame_type, AudioFrameType, VideoFrameType};

/// 包优先级
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum PacketPriority {
    /// 最低: P/B 视频帧 (可丢弃, 仅短暂花屏)
    Low = 0,
    /// 中等: 非关键音频帧 (尽量保留)
    Medium = 1,
    /// 高: 关键帧 + 序列头 (绝不丢弃)
    High = 2,
}

/// 丢包策略配置
#[derive(Debug, Clone)]
pub struct DropPolicy {
    /// 轻度背压阈值 (lag < 此值仅丢 P 帧)
    pub light_pressure_threshold: u64,
    /// 中度背压阈值 (lag 在 light~medium 之间丢 P 帧 + 非关键音频)
    pub medium_pressure_threshold: u64,
    /// 是否启用音频丢包 (默认 false, 音频比画质更敏感)
    pub drop_audio_enabled: bool,
}

impl Default for DropPolicy {
    fn default() -> Self {
        Self {
            light_pressure_threshold: 10,
            medium_pressure_threshold: 50,
            drop_audio_enabled: false,
        }
    }
}

/// 根据标签内容判断优先级
pub fn get_packet_priority(tag: &FlvTagPacket) -> PacketPriority {
    match tag.tag_type {
        FlvTagType::Script => PacketPriority::High, // onMetaData 必须保留
        FlvTagType::Video => {
            match parse_video_frame_type(tag) {
                VideoFrameType::SeqHeader => PacketPriority::High, // SPS+PPS 绝不丢弃
                VideoFrameType::Keyframe => PacketPriority::High,   // IDR 绝不丢弃
                VideoFrameType::InterFrame => PacketPriority::Low,   // P/B 帧可丢弃
                VideoFrameType::Other => PacketPriority::Medium,     // 未知视频帧, 中等优先级
            }
        }
        FlvTagType::Audio => {
            match parse_audio_frame_type(tag) {
                AudioFrameType::SeqHeader => PacketPriority::High,   // AAC config 绝不丢弃
                AudioFrameType::RawData => PacketPriority::Medium,    // AAC 原始数据, 尽量保留
                AudioFrameType::Other => PacketPriority::Medium,      // 非 AAC 音频
            }
        }
    }
}

/// 判断是否应该丢弃此包
///
/// # 参数
/// - `tag`: FLV 标签
/// - `lag_count`: 当前积压数量 (来自 broadcast::RecvError::Lagged 的计数)
/// - `policy`: 丢包策略
///
/// # 返回
/// - `true`: 应该丢弃此包
/// - `false`: 应该保留此包
pub fn should_drop(tag: &FlvTagPacket, lag_count: u64, policy: &DropPolicy) -> bool {
    let priority = get_packet_priority(tag);

    // 高优先级包绝不丢弃
    if priority == PacketPriority::High {
        return false;
    }

    // 无背压, 不丢包
    if lag_count == 0 {
        return false;
    }

    match priority {
        PacketPriority::Low => {
            // P/B 帧: 只要有一点背压就丢
            lag_count > 0
        }
        PacketPriority::Medium => {
            // 音频帧: 只有严重背压且开启了音频丢包才丢
            policy.drop_audio_enabled && lag_count > policy.medium_pressure_threshold
        }
        PacketPriority::High => false, // 已在上面处理
    }
}

/// 背压状态跟踪器
///
/// 每个 HTTP-FLV / WS-FLV 客户端连接持有一个 BackpressureTracker,
/// 跟踪最近的背压情况, 为丢包决策提供依据
pub struct BackpressureTracker {
    /// 当前积压计数
    lag_count: u64,
    /// 连续丢包次数 (用于决定是否需要跳到下一个关键帧)
    consecutive_drops: u32,
    /// 总丢包数
    total_drops: u64,
    /// 总包数
    total_packets: u64,
    /// 丢包策略
    policy: DropPolicy,
}

impl BackpressureTracker {
    pub fn new(policy: DropPolicy) -> Self {
        Self {
            lag_count: 0,
            consecutive_drops: 0,
            total_drops: 0,
            total_packets: 0,
            policy,
        }
    }

    /// 使用默认策略创建
    pub fn with_default_policy() -> Self {
        Self::new(DropPolicy::default())
    }

    /// 记录背压 (从 broadcast lag 事件更新)
    pub fn report_lag(&mut self, lag: u64) {
        self.lag_count = lag;
    }

    /// 减少背压计数 (成功发送一个包后)
    pub fn relieve_pressure(&mut self) {
        if self.lag_count > 0 {
            self.lag_count = self.lag_count.saturating_sub(1);
        }
        self.consecutive_drops = 0;
    }

    /// 判断是否应该丢弃此包
    pub fn should_drop(&mut self, tag: &FlvTagPacket) -> bool {
        self.total_packets += 1;
        let drop = should_drop(tag, self.lag_count, &self.policy);
        if drop {
            self.consecutive_drops += 1;
            self.total_drops += 1;
        } else {
            self.consecutive_drops = 0;
        }
        drop
    }

    /// 是否处于严重背压状态 (建议跳到下一个关键帧)
    pub fn is_severe_pressure(&self) -> bool {
        self.lag_count > self.policy.medium_pressure_threshold
    }

    /// 获取丢包率
    pub fn drop_rate(&self) -> f64 {
        if self.total_packets == 0 {
            0.0
        } else {
            self.total_drops as f64 / self.total_packets as f64
        }
    }

    /// 获取统计信息
    pub fn stats(&self) -> BackpressureStats {
        BackpressureStats {
            lag_count: self.lag_count,
            consecutive_drops: self.consecutive_drops,
            total_drops: self.total_drops,
            total_packets: self.total_packets,
            drop_rate: self.drop_rate(),
            is_severe: self.is_severe_pressure(),
        }
    }
}

/// 背压统计
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct BackpressureStats {
    pub lag_count: u64,
    pub consecutive_drops: u32,
    pub total_drops: u64,
    pub total_packets: u64,
    pub drop_rate: f64,
    pub is_severe: bool,
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytes::Bytes;

    fn make_video_tag(frame_type: u8, codec_id: u8, avc_packet_type: u8) -> FlvTagPacket {
        let first_byte = (frame_type << 4) | codec_id;
        let tag_data = vec![first_byte, avc_packet_type, 0xAA];
        FlvTagPacket {
            tag_type: FlvTagType::Video,
            timestamp_ms: 0,
            data: Bytes::from(tag_data),
            origin_ts: 0,
        }
    }

    fn make_audio_tag(sound_format: u8, aac_packet_type: u8) -> FlvTagPacket {
        let first_byte = (sound_format << 4) | 0x0F;
        let tag_data = vec![first_byte, aac_packet_type, 0xBB];
        FlvTagPacket {
            tag_type: FlvTagType::Audio,
            timestamp_ms: 0,
            data: Bytes::from(tag_data),
            origin_ts: 0,
        }
    }

    fn make_script_tag() -> FlvTagPacket {
        FlvTagPacket {
            tag_type: FlvTagType::Script,
            timestamp_ms: 0,
            data: Bytes::from_static(b"onMetaData"),
            origin_ts: 0,
        }
    }

    #[test]
    fn test_priority_keyframe_never_dropped() {
        let keyframe = make_video_tag(1, 7, 1); // IDR
        let seq_header = make_video_tag(1, 7, 0); // SPS+PPS
        let policy = DropPolicy::default();

        // 即使严重背压, 关键帧和序列头也不丢
        assert!(!should_drop(&keyframe, 100, &policy));
        assert!(!should_drop(&seq_header, 100, &policy));
    }

    #[test]
    fn test_priority_pframe_dropped_on_any_pressure() {
        let pframe = make_video_tag(2, 7, 1); // P-frame
        let policy = DropPolicy::default();

        // 无背压: 不丢
        assert!(!should_drop(&pframe, 0, &policy));
        // 有背压: 丢
        assert!(should_drop(&pframe, 1, &policy));
        assert!(should_drop(&pframe, 100, &policy));
    }

    #[test]
    fn test_priority_audio_not_dropped_by_default() {
        let aac_raw = make_audio_tag(10, 1); // AAC raw
        let policy = DropPolicy::default();

        // 默认策略: 不丢音频 (drop_audio_enabled = false)
        assert!(!should_drop(&aac_raw, 1, &policy));
        assert!(!should_drop(&aac_raw, 100, &policy));
    }

    #[test]
    fn test_priority_audio_dropped_when_enabled() {
        let aac_raw = make_audio_tag(10, 1); // AAC raw
        let policy = DropPolicy {
            drop_audio_enabled: true,
            ..DropPolicy::default()
        };

        // 轻度背压: 不丢
        assert!(!should_drop(&aac_raw, 10, &policy));
        // 中度背压: 丢
        assert!(should_drop(&aac_raw, 51, &policy));
    }

    #[test]
    fn test_metadata_never_dropped() {
        let meta = make_script_tag();
        let policy = DropPolicy::default();

        assert!(!should_drop(&meta, 1000, &policy));
    }

    #[test]
    fn test_backpressure_tracker() {
        let mut tracker = BackpressureTracker::with_default_policy();

        // 无背压: P 帧不丢
        let pframe = make_video_tag(2, 7, 1);
        assert!(!tracker.should_drop(&pframe));

        // 有背压: P 帧丢弃
        tracker.report_lag(5);
        assert!(tracker.should_drop(&pframe));

        // 关键帧仍然不丢
        let keyframe = make_video_tag(1, 7, 1);
        assert!(!tracker.should_drop(&keyframe));

        // 缓解背压 (relieve_pressure 每次减1, 需要多次调用直到 lag=0)
        for _ in 0..5 {
            tracker.relieve_pressure();
        }
        assert!(!tracker.should_drop(&pframe));

        let stats = tracker.stats();
        assert_eq!(stats.total_drops, 1);
        assert!(stats.drop_rate > 0.0);
    }

    #[test]
    fn test_severe_pressure_detection() {
        let mut tracker = BackpressureTracker::new(DropPolicy {
            medium_pressure_threshold: 50,
            ..DropPolicy::default()
        });

        tracker.report_lag(30);
        assert!(!tracker.is_severe_pressure());

        tracker.report_lag(51);
        assert!(tracker.is_severe_pressure());
    }

    #[test]
    fn test_aac_seq_header_never_dropped() {
        let aac_sh = make_audio_tag(10, 0); // AAC SeqHeader
        let policy = DropPolicy {
            drop_audio_enabled: true,
            ..DropPolicy::default()
        };

        // 即使开启了音频丢包, SeqHeader 也不丢
        assert!(!should_drop(&aac_sh, 100, &policy));
    }
}
