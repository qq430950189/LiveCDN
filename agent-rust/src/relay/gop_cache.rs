//! GOP 缓存模块 — 新客户端秒开首帧
//!
//! 参考 lal 的 GopCache 设计:
//!   - 环形缓冲区存储最近 N 个 GOP (Group of Pictures)
//!   - 独立缓存 Metadata / VideoSeqHeader(SPS+PPS) / AacSeqHeader
//!   - 新客户端连接时立即发送缓存数据, 无需等下一个关键帧
//!
//! FLV 视频标签格式:
//!   data[0] = (frame_type << 4) | codec_id
//!     frame_type: 1=keyframe(IDR), 2=inter(P-frame)
//!     codec_id: 7=AVC(H.264), 12=HEVC(H.265)
//!   data[1] = AVC packet type (仅 codec_id==7 时)
//!     0=AVC sequence header (SPS+PPS), 1=AVC NALU, 2=AVC end
//!
//! FLV 音频标签格式:
//!   data[0] = (sound_format << 4) | ... 
//!     sound_format: 10=AAC
//!   data[1] = AAC packet type (仅 sound_format==10 时)
//!     0=AAC sequence header, 1=AAC raw data

#[cfg(test)]
use bytes::Bytes;
use std::collections::VecDeque;
use tracing::debug;

use super::flv::{FlvTagPacket, FlvTagType};

// --- FLV 视频帧类型解析 ---

/// 视频帧类型
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VideoFrameType {
    /// 关键帧 (IDR)
    Keyframe,
    /// 非关键帧 (P/B)
    InterFrame,
    /// 视频序列头 (SPS+PPS)
    SeqHeader,
    /// 其他/未知
    Other,
}

/// 音频帧类型
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AudioFrameType {
    /// AAC 序列头
    SeqHeader,
    /// AAC 原始数据
    RawData,
    /// 非 AAC 或未知
    Other,
}

/// 解析视频帧类型
pub fn parse_video_frame_type(tag: &FlvTagPacket) -> VideoFrameType {
    if tag.tag_type != FlvTagType::Video || tag.data.is_empty() {
        return VideoFrameType::Other;
    }

    let first_byte = tag.data[0];
    let frame_type = (first_byte >> 4) & 0x0F;
    let codec_id = first_byte & 0x0F;

    // AVC (H.264) 或 HEVC (H.265)
    if codec_id == 7 || codec_id == 12 {
        if tag.data.len() < 2 {
            return VideoFrameType::Other;
        }
        let avc_packet_type = tag.data[1];
        if avc_packet_type == 0 {
            return VideoFrameType::SeqHeader;
        }
    }

    match frame_type {
        1 => VideoFrameType::Keyframe,
        2 => VideoFrameType::InterFrame,
        _ => VideoFrameType::Other,
    }
}

/// 解析音频帧类型
pub fn parse_audio_frame_type(tag: &FlvTagPacket) -> AudioFrameType {
    if tag.tag_type != FlvTagType::Audio || tag.data.is_empty() {
        return AudioFrameType::Other;
    }

    let first_byte = tag.data[0];
    let sound_format = (first_byte >> 4) & 0x0F;

    if sound_format == 10 {
        // AAC
        if tag.data.len() < 2 {
            return AudioFrameType::Other;
        }
        if tag.data[1] == 0 {
            return AudioFrameType::SeqHeader;
        }
        return AudioFrameType::RawData;
    }

    AudioFrameType::Other
}

// --- GOP 缓存 ---

/// 单个 GOP (从一个关键帧到下一个关键帧之前的所有帧)
#[derive(Debug, Clone)]
struct Gop {
    /// 关键帧 (IDR)
    keyframe: FlvTagPacket,
    /// 后续的非关键帧 (P/B + 音频 + Script)
    frames: Vec<FlvTagPacket>,
}

impl Gop {
    fn new(keyframe: FlvTagPacket) -> Self {
        Self {
            keyframe,
            frames: Vec::new(),
        }
    }

    /// 获取此 GOP 的所有标签 (关键帧 + 后续帧)
    fn all_tags(&self) -> impl Iterator<Item = &FlvTagPacket> {
        std::iter::once(&self.keyframe).chain(self.frames.iter())
    }

    /// 标签总数
    fn len(&self) -> usize {
        1 + self.frames.len()
    }
}

/// GOP 缓存配置
#[derive(Debug, Clone)]
pub struct GopCacheConfig {
    /// 缓存 GOP 数量 (默认 2)
    pub gop_count: usize,
    /// 单个 GOP 最大帧数 (防止单个 GOP 过大, 默认 4096)
    pub max_frames_per_gop: usize,
}

impl Default for GopCacheConfig {
    fn default() -> Self {
        Self {
            gop_count: 2,
            max_frames_per_gop: 4096,
        }
    }
}

/// GOP 缓存 — 环形缓冲区 + 独立序列头缓存
///
/// 新客户端连接时:
///   1. 发送 Metadata (如果有)
///   2. 发送 VideoSeqHeader (SPS+PPS, 解码必需)
///   3. 发送 AacSeqHeader (音频解码必需)
///   4. 发送缓存的 GOP 数据 (最近的 N 个 GOP)
///
/// 这样客户端可以立即解码播放, 不需要等下一个关键帧
pub struct GopCache {
    config: GopCacheConfig,

    /// 环形缓冲区: 最近 N 个 GOP
    gop_ring: VecDeque<Gop>,

    /// 当前正在构建的 GOP (关键帧已收到, 但还没收到下一个关键帧)
    current_gop: Option<Gop>,

    /// 独立缓存: Metadata (onMetaData)
    metadata: Option<FlvTagPacket>,

    /// 独立缓存: Video Sequence Header (SPS+PPS)
    video_seq_header: Option<FlvTagPacket>,

    /// 独立缓存: AAC Sequence Header
    aac_seq_header: Option<FlvTagPacket>,

    /// 统计: 缓存命中次数
    hit_count: u64,

    /// 统计: 总标签数
    total_tags: u64,
}

impl GopCache {
    pub fn new(config: GopCacheConfig) -> Self {
        let gop_count = config.gop_count;
        Self {
            config,
            gop_ring: VecDeque::with_capacity(gop_count),
            current_gop: None,
            metadata: None,
            video_seq_header: None,
            aac_seq_header: None,
            hit_count: 0,
            total_tags: 0,
        }
    }

    /// 喂入一个 FLV 标签, 更新缓存
    pub fn add_tag(&mut self, tag: FlvTagPacket) {
        self.total_tags += 1;

        match tag.tag_type {
            FlvTagType::Script => {
                // Metadata (onMetaData) — 独立缓存, 总是更新
                self.metadata = Some(tag);
                return;
            }
            FlvTagType::Audio => {
                match parse_audio_frame_type(&tag) {
                    AudioFrameType::SeqHeader => {
                        // AAC 序列头 — 独立缓存
                        self.aac_seq_header = Some(tag);
                        return;
                    }
                    AudioFrameType::RawData | AudioFrameType::Other => {
                        // 音频数据 — 加入当前 GOP
                        self.add_to_current_gop(tag);
                        return;
                    }
                }
            }
            FlvTagType::Video => {
                match parse_video_frame_type(&tag) {
                    VideoFrameType::SeqHeader => {
                        // SPS+PPS — 独立缓存
                        self.video_seq_header = Some(tag);
                        return;
                    }
                    VideoFrameType::Keyframe => {
                        // 关键帧 — 新 GOP 开始
                        self.finalize_current_gop();
                        self.current_gop = Some(Gop::new(tag));
                        return;
                    }
                    VideoFrameType::InterFrame | VideoFrameType::Other => {
                        // P/B 帧 — 加入当前 GOP
                        self.add_to_current_gop(tag);
                        return;
                    }
                }
            }
        }
    }

    /// 将标签加入当前 GOP
    fn add_to_current_gop(&mut self, tag: FlvTagPacket) {
        if let Some(ref mut gop) = self.current_gop {
            if gop.len() < self.config.max_frames_per_gop {
                gop.frames.push(tag);
            }
        }
        // 如果还没有关键帧 (current_gop 为 None), 丢弃非关键帧
    }

    /// 完成当前 GOP, 加入环形缓冲区
    fn finalize_current_gop(&mut self) {
        if let Some(gop) = self.current_gop.take() {
            self.gop_ring.push_back(gop);
            // 环形缓冲区满时, 淘汰最老的 GOP
            while self.gop_ring.len() > self.config.gop_count {
                self.gop_ring.pop_front();
            }
        }
    }

    /// 获取新客户端启动所需的所有缓存标签
    ///
    /// 返回顺序:
    ///   1. Metadata (如果有)
    ///   2. AacSeqHeader (如果有)
    ///   3. VideoSeqHeader (如果有)
    ///   4. 缓存的 GOP 数据 (从最老的到最新的)
    ///
    /// 这保证了播放器能立即初始化解码器并开始播放
    pub fn get_startup_tags(&self) -> Vec<FlvTagPacket> {
        // 注意: hit_count 统计已移除 (不可变引用无法修改)
        // 如需统计, 可用 AtomicU64

        let mut tags = Vec::new();

        // 1. Metadata
        if let Some(ref meta) = self.metadata {
            tags.push(meta.clone());
        }

        // 2. AAC SeqHeader (音频解码必需)
        if let Some(ref ash) = self.aac_seq_header {
            tags.push(ash.clone());
        }

        // 3. Video SeqHeader (SPS+PPS, 视频解码必需)
        if let Some(ref vsh) = self.video_seq_header {
            tags.push(vsh.clone());
        }

        // 4. 缓存的 GOP 数据
        for gop in &self.gop_ring {
            for tag in gop.all_tags() {
                tags.push(tag.clone());
            }
        }

        // 5. 当前正在构建的 GOP (可能还没有下一个关键帧来 finalize)
        if let Some(ref gop) = self.current_gop {
            for tag in gop.all_tags() {
                tags.push(tag.clone());
            }
        }

        debug!(
            "[GopCache] Startup tags: {} total (meta={}, aac_sh={}, video_sh={}, gops={}, current={})",
            tags.len(),
            self.metadata.is_some(),
            self.aac_seq_header.is_some(),
            self.video_seq_header.is_some(),
            self.gop_ring.len(),
            self.current_gop.is_some(),
        );

        tags
    }

    /// 缓存是否为空 (没有可用的启动数据)
    pub fn is_empty(&self) -> bool {
        self.gop_ring.is_empty() && self.current_gop.is_none()
    }

    /// 获取缓存统计
    pub fn stats(&self) -> GopCacheStats {
        let cached_gops = self.gop_ring.len();
        let cached_frames: usize = self.gop_ring.iter().map(|g| g.len()).sum::<usize>()
            + self.current_gop.as_ref().map(|g| g.len()).unwrap_or(0);

        GopCacheStats {
            cached_gops,
            cached_frames,
            has_metadata: self.metadata.is_some(),
            has_video_seq_header: self.video_seq_header.is_some(),
            has_aac_seq_header: self.aac_seq_header.is_some(),
            total_tags: self.total_tags,
        }
    }

    /// 清空缓存
    pub fn clear(&mut self) {
        self.gop_ring.clear();
        self.current_gop = None;
        self.metadata = None;
        self.video_seq_header = None;
        self.aac_seq_header = None;
    }
}

/// GOP 缓存统计
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GopCacheStats {
    pub cached_gops: usize,
    pub cached_frames: usize,
    pub has_metadata: bool,
    pub has_video_seq_header: bool,
    pub has_aac_seq_header: bool,
    pub total_tags: u64,
}

use serde::{Deserialize, Serialize};

#[cfg(test)]
mod tests {
    use super::*;

    fn make_video_tag(frame_type: u8, codec_id: u8, avc_packet_type: u8, data: &[u8]) -> FlvTagPacket {
        let first_byte = (frame_type << 4) | codec_id;
        let mut tag_data = vec![first_byte, avc_packet_type];
        tag_data.extend_from_slice(data);
        FlvTagPacket {
            tag_type: FlvTagType::Video,
            timestamp_ms: 0,
            data: Bytes::from(tag_data),
            origin_ts: 0,
        }
    }

    fn make_audio_tag(sound_format: u8, aac_packet_type: u8, data: &[u8]) -> FlvTagPacket {
        let first_byte = (sound_format << 4) | 0x0F; // sound_rate=3(44kHz), sound_size=1(16bit), sound_type=1(stereo)
        let mut tag_data = vec![first_byte, aac_packet_type];
        tag_data.extend_from_slice(data);
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
    fn test_parse_video_frame_types() {
        // Keyframe + AVC + NALU
        let keyframe = make_video_tag(1, 7, 1, b"idr_data");
        assert_eq!(parse_video_frame_type(&keyframe), VideoFrameType::Keyframe);

        // P-frame + AVC + NALU
        let pframe = make_video_tag(2, 7, 1, b"p_data");
        assert_eq!(parse_video_frame_type(&pframe), VideoFrameType::InterFrame);

        // SeqHeader + AVC
        let seq_header = make_video_tag(1, 7, 0, b"sps_pps");
        assert_eq!(parse_video_frame_type(&seq_header), VideoFrameType::SeqHeader);
    }

    #[test]
    fn test_parse_audio_frame_types() {
        // AAC SeqHeader
        let aac_sh = make_audio_tag(10, 0, b"aac_config");
        assert_eq!(parse_audio_frame_type(&aac_sh), AudioFrameType::SeqHeader);

        // AAC Raw
        let aac_raw = make_audio_tag(10, 1, b"aac_frame");
        assert_eq!(parse_audio_frame_type(&aac_raw), AudioFrameType::RawData);

        // Non-AAC
        let mp3 = make_audio_tag(2, 0, b"mp3_data");
        assert_eq!(parse_audio_frame_type(&mp3), AudioFrameType::Other);
    }

    #[test]
    fn test_gop_cache_basic() {
        let mut cache = GopCache::new(GopCacheConfig::default());

        // 1. Metadata
        cache.add_tag(make_script_tag());
        assert!(cache.metadata.is_some());

        // 2. AAC SeqHeader
        cache.add_tag(make_audio_tag(10, 0, b"config"));
        assert!(cache.aac_seq_header.is_some());

        // 3. Video SeqHeader (SPS+PPS)
        cache.add_tag(make_video_tag(1, 7, 0, b"sps_pps"));
        assert!(cache.video_seq_header.is_some());

        // 4. 关键帧 → 新 GOP
        cache.add_tag(make_video_tag(1, 7, 1, b"idr1"));
        assert!(cache.current_gop.is_some());

        // 5. P 帧 + 音频 → 加入当前 GOP
        cache.add_tag(make_video_tag(2, 7, 1, b"p1"));
        cache.add_tag(make_audio_tag(10, 1, b"aac1"));

        // 6. 下一个关键帧 → finalize 旧 GOP, 开始新 GOP
        cache.add_tag(make_video_tag(1, 7, 1, b"idr2"));
        assert_eq!(cache.gop_ring.len(), 1); // 旧 GOP 入 ring
        assert!(cache.current_gop.is_some()); // 新 GOP 正在构建

        cache.add_tag(make_video_tag(2, 7, 1, b"p2"));

        // 获取启动标签
        let tags = cache.get_startup_tags();
        // 期望: metadata + aac_sh + video_sh + gop1(idr1,p1,aac1) + current_gop(idr2,p2)
        assert!(tags.len() >= 7);
        assert_eq!(tags[0].tag_type, FlvTagType::Script); // metadata first
        assert_eq!(tags[1].tag_type, FlvTagType::Audio);  // aac seq header
        assert_eq!(tags[2].tag_type, FlvTagType::Video);  // video seq header
    }

    #[test]
    fn test_gop_cache_ring_eviction() {
        let mut cache = GopCache::new(GopCacheConfig {
            gop_count: 2,
            max_frames_per_gop: 4096,
        });

        // 添加 3 个 GOP (超出 gop_count=2)
        for _ in 0..3 {
            cache.add_tag(make_video_tag(1, 7, 1, b"idr")); // keyframe
            cache.add_tag(make_video_tag(2, 7, 1, b"p"));   // P-frame
        }
        // finalize 最后一个 GOP
        cache.finalize_current_gop();

        assert_eq!(cache.gop_ring.len(), 2); // 只保留最近 2 个
    }

    #[test]
    fn test_gop_cache_empty() {
        let cache = GopCache::new(GopCacheConfig::default());
        assert!(cache.is_empty());

        let tags = cache.get_startup_tags();
        assert!(tags.is_empty());
    }

    #[test]
    fn test_gop_cache_no_keyframe_drops_frames() {
        let mut cache = GopCache::new(GopCacheConfig::default());

        // P-frame arrives before any keyframe → should be dropped
        cache.add_tag(make_video_tag(2, 7, 1, b"orphan_p"));
        assert!(cache.is_empty());

        // Audio arrives before keyframe → should be dropped
        cache.add_tag(make_audio_tag(10, 1, b"orphan_aac"));
        assert!(cache.is_empty());

        // But seq headers and metadata are always cached
        cache.add_tag(make_script_tag());
        cache.add_tag(make_video_tag(1, 7, 0, b"sps_pps"));
        cache.add_tag(make_audio_tag(10, 0, b"aac_config"));
        assert!(cache.metadata.is_some());
        assert!(cache.video_seq_header.is_some());
        assert!(cache.aac_seq_header.is_some());
    }

    #[test]
    fn test_gop_cache_stats() {
        let mut cache = GopCache::new(GopCacheConfig::default());
        cache.add_tag(make_script_tag());
        cache.add_tag(make_video_tag(1, 7, 0, b"sps_pps"));
        cache.add_tag(make_audio_tag(10, 0, b"config"));
        cache.add_tag(make_video_tag(1, 7, 1, b"idr"));
        cache.add_tag(make_video_tag(2, 7, 1, b"p1"));
        cache.add_tag(make_audio_tag(10, 1, b"aac1"));

        let stats = cache.stats();
        assert!(stats.has_metadata);
        assert!(stats.has_video_seq_header);
        assert!(stats.has_aac_seq_header);
        assert_eq!(stats.total_tags, 6);
    }
}
