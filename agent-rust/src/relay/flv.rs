//! HTTP-FLV 回源拉取器
//! 从源站 HTTP-FLV 长连接持续拉取 FLV tag → 广播原始 FLV 标签
//! 相比 HLS m3u8 轮询, 延迟从 6-15s 降低到 0.8-1.5s
//!
//! FLV 格式: [FLV Header 9B][PreviousTagSize 4B][Tag1][Tag2]...
//! FLV Tag:  [Tag Type 1B][Data Size 3B][Timestamp 3B][TimestampExt 1B][StreamID 3B][Data NB][PreviousTagSize 4B]
//!
//! 广播原始 FLV 标签数据, 加密在输出边界处理 (HTTP-FLV/WS)

use bytes::Bytes;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::{broadcast, RwLock, Semaphore};
use tokio::time;
use tracing::{debug, info, warn};

use super::backoff;

// --- FLV 常量 ---

pub const FLV_HEADER_LEN: usize = 9;     // 'FLV' + version + flags + header size
pub const TAG_HEADER_LEN: usize = 11;    // type(1) + data_size(3) + ts(3) + ts_ext(1) + stream_id(3)
pub const PREV_TAG_SIZE_LEN: usize = 4;

#[repr(u8)]
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum FlvTagType {
    Audio = 0x08,
    Video = 0x09,
    Script = 0x12,
}

impl TryFrom<u8> for FlvTagType {
    type Error = u8;
    fn try_from(v: u8) -> Result<Self, Self::Error> {
        match v {
            0x08 => Ok(Self::Audio),
            0x09 => Ok(Self::Video),
            0x12 => Ok(Self::Script),
            _ => Err(v),
        }
    }
}

/// FLV 标签广播包 (原始数据, 不加密)
/// 输出层 (HTTP-FLV/WS) 根据需要加密或直接发送
#[derive(Debug, Clone)]
pub struct FlvTagPacket {
    pub tag_type: FlvTagType,
    pub timestamp_ms: u32,
    pub data: Bytes,       // 原始标签数据 (未加密)
    pub origin_ts: u64,    // 源站时间戳 (Unix毫秒), 端到端延迟测量
}

/// HTTP-FLV 流拉取器
/// 从源站 HTTP-FLV 长连接拉取, 广播原始 FlvTagPacket
/// 支持主备切换: 主源失败自动切到备源
/// 集成 GOP 缓存: 新客户端秒开首帧 (参考 lal)
pub struct FlvPuller {
    stream_key: String,
    flv_url: String,              // e.g. http://origin:8080/live/stream.flv
    backup_flv_url: Option<String>, // 备源 FLV 地址
    broadcast_tx: broadcast::Sender<FlvTagPacket>,
    running: Arc<RwLock<bool>>,
    origin_semaphore: Arc<Semaphore>,
    bytes_sent: Arc<RwLock<u64>>,
    client_count: Arc<RwLock<usize>>,
    max_clients: usize,
    /// GOP 缓存 — 新客户端秒开 (独立缓存 Metadata/SPS+PPS/AacSeqHeader + 环形缓冲区)
    gop_cache: Arc<RwLock<super::gop_cache::GopCache>>,
}

impl FlvPuller {
    pub fn new(
        stream_key: String,
        flv_url: String,
        max_clients: usize,
    ) -> Self {
        Self::with_backup(stream_key, flv_url, None, max_clients)
    }

    pub fn with_backup(
        stream_key: String,
        flv_url: String,
        backup_flv_url: Option<String>,
        max_clients: usize,
    ) -> Self {
        let (broadcast_tx, _) = broadcast::channel(512);
        let gop_cache = super::gop_cache::GopCache::new(
            super::gop_cache::GopCacheConfig::default(),
        );

        Self {
            stream_key,
            flv_url,
            backup_flv_url,
            broadcast_tx,
            running: Arc::new(RwLock::new(false)),
            origin_semaphore: Arc::new(Semaphore::new(2)),
            bytes_sent: Arc::new(RwLock::new(0)),
            client_count: Arc::new(RwLock::new(0)),
            max_clients,
            gop_cache: Arc::new(RwLock::new(gop_cache)),
        }
    }

    /// 启动 HTTP-FLV 长连接拉取
    pub async fn start(&self) {
        let mut running = self.running.write().await;
        if *running {
            return;
        }
        *running = true;
        drop(running);

        let stream_key = self.stream_key.clone();
        let flv_url = self.flv_url.clone();
        let backup_url = self.backup_flv_url.clone();
        let broadcast_tx = self.broadcast_tx.clone();
        let running_flag = self.running.clone();
        let origin_sem = self.origin_semaphore.clone();
        let gop_cache = self.gop_cache.clone();

        tokio::spawn(async move {
            info!("[FLV] Starting pull for stream {}", &stream_key[..8.min(stream_key.len())]);

            let mut backoff = backoff::ExponentialBackoff::new(
                Duration::from_secs(2),
                Duration::from_secs(60),
            );
            let mut using_backup = false;

            loop {
                if !*running_flag.read().await {
                    break;
                }

                // 选择当前源: 主源 or 备源
                let current_url = if using_backup {
                    match &backup_url {
                        Some(url) => {
                            info!("[FLV] Using backup origin");
                            url.clone()
                        }
                        None => flv_url.clone(),
                    }
                } else {
                    flv_url.clone()
                };

                match pull_flv_stream(
                    &current_url,
                    &broadcast_tx,
                    &running_flag,
                    &origin_sem,
                    &gop_cache,
                ).await {
                    Ok(()) => {
                        info!("[FLV] Stream ended for {}", &stream_key[..8.min(stream_key.len())]);
                        break;
                    }
                    Err(e) => {
                        let wait = backoff.next();
                        warn!(
                            "[FLV] Connection lost (attempt {}): {}, retry in {:?}",
                            backoff.attempt(), e, wait
                        );

                        // 主源失败且备源可用, 切换到备源
                        if !using_backup && backup_url.is_some() {
                            info!("[FLV] Switching to backup origin");
                            using_backup = true;
                        } else if using_backup {
                            // 备源也失败了, 切回主源重试
                            info!("[FLV] Backup also failed, switching back to primary");
                            using_backup = false;
                        }

                        time::sleep(wait).await;
                    }
                }
            }

            info!("[FLV] Pull loop stopped for stream {}", &stream_key[..8.min(stream_key.len())]);
        });
    }

    pub async fn stop(&self) {
        let mut running = self.running.write().await;
        *running = false;
    }

    pub fn is_running(&self) -> bool {
        self.running.try_read().map(|r| *r).unwrap_or(false)
    }

    pub fn subscribe(&self) -> broadcast::Receiver<FlvTagPacket> {
        self.broadcast_tx.subscribe()
    }

    pub async fn add_client(&self) -> bool {
        let mut count = self.client_count.write().await;
        if *count >= self.max_clients {
            return false;
        }
        *count += 1;
        true
    }

    pub async fn remove_client(&self) {
        let mut count = self.client_count.write().await;
        if *count > 0 {
            *count -= 1;
        }
    }

    pub async fn client_count(&self) -> usize {
        *self.client_count.read().await
    }

    pub async fn record_bytes_sent(&self, n: u64) {
        let mut bytes = self.bytes_sent.write().await;
        *bytes += n;
    }

    pub async fn current_bandwidth(&self) -> u64 {
        *self.bytes_sent.read().await
    }

    pub async fn measure_rtt(&self) -> u32 {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(5))
            .build()
            .unwrap_or_default();
        let start = Instant::now();
        match client.head(&self.flv_url).send().await {
            Ok(_) => start.elapsed().as_millis() as u32,
            Err(_) => 9999,
        }
    }

    /// 获取 GOP 缓存的启动标签 (新客户端秒开首帧)
    /// 调用者负责先发送 FLV header, 再发送这些标签
    pub async fn get_startup_tags(&self) -> Vec<FlvTagPacket> {
        self.gop_cache.read().await.get_startup_tags()
    }

    /// 获取 GOP 缓存统计
    pub async fn gop_cache_stats(&self) -> super::gop_cache::GopCacheStats {
        self.gop_cache.read().await.stats()
    }
}

/// 从 HTTP-FLV 长连接持续读取 FLV tag, 广播原始 FlvTagPacket
/// 同时将标签喂入 GOP 缓存, 供新客户端秒开
async fn pull_flv_stream(
    flv_url: &str,
    broadcast_tx: &broadcast::Sender<FlvTagPacket>,
    running_flag: &Arc<RwLock<bool>>,
    _origin_sem: &Arc<Semaphore>,
    gop_cache: &Arc<RwLock<super::gop_cache::GopCache>>,
) -> Result<(), String> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(300)) // 长连接, 超时设长
        .pool_max_idle_per_host(1)
        .build()
        .map_err(|e| format!("build client: {}", e))?;

    let resp = client.get(flv_url)
        .header("Connection", "keep-alive")
        .header("X-LiveCDN-Agent", env!("CARGO_PKG_VERSION"))
        .send()
        .await
        .map_err(|e| format!("connect: {}", e))?;

    if !resp.status().is_success() {
        return Err(format!("HTTP {}", resp.status()));
    }

    info!("[FLV] Connected to {}", flv_url);

    // 用 bytes_stream 获取 chunked body 流
    let mut stream = resp.bytes_stream();
    use futures_util::StreamExt;

    let mut buffer = Vec::with_capacity(64 * 1024);
    let mut flv_header_parsed = false;
    let mut tag_counter: u64 = 0;

    while let Some(chunk_result) = stream.next().await {
        if !*running_flag.read().await {
            return Ok(());
        }

        let chunk = chunk_result.map_err(|e| format!("read chunk: {}", e))?;
        buffer.extend_from_slice(&chunk);

        // 解析 FLV
        loop {
            if !flv_header_parsed {
                if buffer.len() < FLV_HEADER_LEN {
                    break;
                }
                if &buffer[0..3] != b"FLV" {
                    return Err("not a valid FLV stream".into());
                }
                let header_size = u32::from_be_bytes([
                    0, buffer[5], buffer[6], buffer[7],
                ]) as usize;
                let skip = header_size + PREV_TAG_SIZE_LEN;
                if buffer.len() < skip {
                    break;
                }
                buffer.drain(..skip);
                flv_header_parsed = true;
                debug!("[FLV] Header parsed, skip={}", skip);
                continue;
            }

            // 解析 FLV Tag
            if buffer.len() < TAG_HEADER_LEN {
                break;
            }

            let tag_type_byte = buffer[0];
            let tag_type = match FlvTagType::try_from(tag_type_byte) {
                Ok(t) => t,
                Err(_) => {
                    debug!("[FLV] Unknown tag type: 0x{:02x}", tag_type_byte);
                    buffer.drain(..TAG_HEADER_LEN);
                    continue;
                }
            };

            let data_size = ((buffer[1] as u32) << 16)
                | ((buffer[2] as u32) << 8)
                | (buffer[3] as u32);

            let timestamp = ((buffer[4] as u32) << 16)
                | ((buffer[5] as u32) << 8)
                | (buffer[6] as u32)
                | ((buffer[7] as u32) << 24);

            let total_tag_len = TAG_HEADER_LEN + data_size as usize + PREV_TAG_SIZE_LEN;

            if buffer.len() < total_tag_len {
                break;
            }

            // 提取 tag data (原始, 不加密)
            let tag_data = Bytes::copy_from_slice(
                &buffer[TAG_HEADER_LEN..TAG_HEADER_LEN + data_size as usize]
            );

            buffer.drain(..total_tag_len);

            // Origin timestamp (毫秒)
            let origin_ts = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as u64;

            let tag_packet = FlvTagPacket {
                tag_type,
                timestamp_ms: timestamp,
                data: tag_data,
                origin_ts,
            };

            // 广播给所有订阅者
            let _ = broadcast_tx.send(tag_packet.clone());
            
            // 喂入 GOP 缓存 (新客户端秒开)
            gop_cache.write().await.add_tag(tag_packet);
            
            tag_counter += 1;

            if tag_counter % 100 == 0 {
                debug!(
                    "[FLV] Tag: type={:?}, size={}, ts={}ms, total={}",
                    tag_type, data_size, timestamp, tag_counter
                );
            }
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_flv_header_parse() {
        // FLV header: 'FLV' + version(1) + flags(5=audio+video) + header_size(9)
        let header = vec![
            b'F', b'L', b'V', 0x01, 0x05, 0x00, 0x00, 0x00, 0x09,
        ];
        assert_eq!(&header[0..3], b"FLV");
        assert_eq!(header[3], 1); // version
        assert_eq!(header[4], 5); // audio + video
    }

    #[test]
    fn test_flv_tag_header_parse() {
        // Simulated video tag header: type=0x09, data_size=100, ts=1234
        let mut tag_header = vec![0x09]; // video
        tag_header.extend_from_slice(&[0x00, 0x00, 0x64]); // data size = 100
        tag_header.extend_from_slice(&[0x00, 0x04, 0xD2]); // timestamp = 1234
        tag_header.push(0x00); // timestamp ext
        tag_header.extend_from_slice(&[0x00, 0x00, 0x00]); // stream id

        let tag_type = FlvTagType::try_from(tag_header[0]).unwrap();
        assert_eq!(tag_type, FlvTagType::Video);

        let data_size = ((tag_header[1] as u32) << 16)
            | ((tag_header[2] as u32) << 8)
            | (tag_header[3] as u32);
        assert_eq!(data_size, 100);

        let timestamp = ((tag_header[4] as u32) << 16)
            | ((tag_header[5] as u32) << 8)
            | (tag_header[6] as u32)
            | ((tag_header[7] as u32) << 24);
        assert_eq!(timestamp, 1234);
    }
}
