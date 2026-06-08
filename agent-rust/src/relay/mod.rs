//! 流中继模块
//! 支持两种回源模式:
//!   1. HTTP-FLV 长连接 (默认, 低延迟 0.8-1.5s)
//!   2. HLS m3u8 轮询 (兜底, 延迟 6-15s)
//!
//! 核心优化: 零拷贝(bytes::Bytes) + 无锁广播(tokio::sync::broadcast)

use bytes::Bytes;
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::{broadcast, RwLock, Semaphore};
use tokio::time;
use tracing::{error, info, warn};

pub mod backoff;
pub mod cascade_selector;
pub mod flv;
pub mod flv_output;
pub mod gop_cache;
pub mod packet_drop;

pub use flv::FlvPuller;
pub use cascade_selector::{CascadeSelector, MeshMapResponse};

use crate::crypto::{KeyInfo, StreamEncryptor};
use crate::protocol::{Frame, FrameType};

// --- m3u8 解析器 ---

#[derive(Debug, Clone)]
pub struct M3u8Info {
    pub target_duration: f64,
    pub media_sequence: u64,
    pub segments: Vec<M3u8Segment>,
}

#[derive(Debug, Clone)]
pub struct M3u8Segment {
    pub url: String,
    pub duration: f64,
    pub seq: u64,
}

/// 解析 m3u8 播放列表
pub fn parse_m3u8(content: &str, base_url: &str) -> M3u8Info {
    let mut target_duration = 2.0;
    let mut media_sequence = 0u64;
    let mut segments = Vec::new();
    let mut current_duration = 0.0;
    let mut seq_counter = media_sequence;

    for line in content.lines() {
        let line = line.trim();
        if line.starts_with("#EXT-X-TARGETDURATION:") {
            if let Ok(d) = line[23..].parse::<f64>() {
                target_duration = d;
            }
        } else if line.starts_with("#EXT-X-MEDIA-SEQUENCE:") {
            if let Ok(s) = line[22..].parse::<u64>() {
                media_sequence = s;
                seq_counter = s;
            }
        } else if line.starts_with("#EXTINF:") {
            let dur_str = line[8..].trim_end_matches(',');
            if let Ok(d) = dur_str.parse::<f64>() {
                current_duration = d;
            }
        } else if !line.is_empty() && !line.starts_with('#') {
            let url = resolve_url(base_url, line);
            segments.push(M3u8Segment {
                url,
                duration: current_duration,
                seq: seq_counter,
            });
            seq_counter += 1;
            current_duration = 0.0;
        }
    }

    M3u8Info {
        target_duration,
        media_sequence,
        segments,
    }
}

fn resolve_url(base: &str, relative: &str) -> String {
    if relative.starts_with("http://") || relative.starts_with("https://") {
        return relative.to_string();
    }
    if let Some(pos) = base.rfind('/') {
        format!("{}/{}", &base[..pos], relative)
    } else {
        relative.to_string()
    }
}

// --- 流缓存 ---

#[derive(Debug, Clone)]
pub struct CachedSegment {
    pub seq: u64,
    pub seg_type: SegType,
    pub data: Bytes,
    pub duration: f64,
    pub created_at: Instant,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum SegType {
    M3u8,
    Ts,
}

/// 流中继器 - 从源站拉流、加密、缓存、分发
pub struct StreamRelay {
    stream_key: String,
    origin_m3u8_url: String,
    encryptor: Arc<RwLock<StreamEncryptor>>,

    // 分片缓存
    segments: Arc<RwLock<Vec<CachedSegment>>>,
    max_segments: usize,

    // 已拉取 TS URL (去重)
    fetched_urls: Arc<RwLock<HashMap<String, u64>>>,

    // 广播通道
    broadcast_tx: broadcast::Sender<Bytes>,

    // 客户端计数
    client_count: Arc<RwLock<usize>>,
    max_clients: usize,

    // 回源并发控制
    origin_semaphore: Arc<Semaphore>,

    // 运行状态
    running: Arc<RwLock<bool>>,

    // 带宽统计
    bytes_sent: Arc<RwLock<u64>>,
}

impl StreamRelay {
    pub fn new(
        stream_key: String,
        origin_url: String,
        key_info: KeyInfo,
        max_clients: usize,
        max_segments: usize,
    ) -> Self {
        let (broadcast_tx, _) = broadcast::channel(256);

        Self {
            stream_key,
            origin_m3u8_url: origin_url,
            encryptor: Arc::new(RwLock::new(StreamEncryptor::new(key_info))),
            segments: Arc::new(RwLock::new(Vec::with_capacity(max_segments))),
            max_segments,
            fetched_urls: Arc::new(RwLock::new(HashMap::new())),
            broadcast_tx,
            client_count: Arc::new(RwLock::new(0)),
            max_clients,
            origin_semaphore: Arc::new(Semaphore::new(4)),
            running: Arc::new(RwLock::new(false)),
            bytes_sent: Arc::new(RwLock::new(0)),
        }
    }

    /// 启动回源拉流
    pub async fn start(&self) {
        let mut running = self.running.write().await;
        if *running {
            return;
        }
        *running = true;
        drop(running);

        let stream_key = self.stream_key.clone();
        let origin_url = self.origin_m3u8_url.clone();
        let encryptor = self.encryptor.clone();
        let segments = self.segments.clone();
        let max_segments = self.max_segments;
        let fetched_urls = self.fetched_urls.clone();
        let broadcast_tx = self.broadcast_tx.clone();
        let running_flag = self.running.clone();
        let origin_sem = self.origin_semaphore.clone();

        tokio::spawn(async move {
            info!("[Relay] Starting pull loop for stream {}", &stream_key[..8.min(stream_key.len())]);

            let client = reqwest_client();
            let mut interval = time::interval(Duration::from_secs(2));
            let mut last_m3u8_hash: u64 = 0;
            let mut seq_counter: u32 = 0;
            let mut backoff = backoff::ExponentialBackoff::new(
                Duration::from_secs(2),
                Duration::from_secs(60),
            );

            loop {
                if !*running_flag.read().await {
                    break;
                }
                interval.tick().await;

                // 拉取 m3u8
                let m3u8_data = match fetch_url(&client, &origin_url, &origin_sem).await {
                    Ok(d) => d,
                    Err(e) => {
                        let wait = backoff.next();
                        warn!("[Relay] Failed to fetch m3u8 (attempt {}): {}, retry in {:?}", backoff.attempt(), e, wait);
                        time::sleep(wait).await;
                        continue;
                    }
                };

                // 成功则重置退避
                backoff.reset();

                // 变化检测
                let current_hash = simple_hash(&m3u8_data);
                if current_hash == last_m3u8_hash {
                    continue;
                }
                last_m3u8_hash = current_hash;

                // 解析 m3u8
                let m3u8_str = String::from_utf8_lossy(&m3u8_data);
                let parsed = parse_m3u8(&m3u8_str, &origin_url);

                // 加密并缓存 m3u8
                {
                    let mut enc = encryptor.write().await;
                    match enc.encrypt(&m3u8_data) {
                        Ok(encrypted) => {
                            let cached = CachedSegment {
                                seq: seq_counter as u64,
                                seg_type: SegType::M3u8,
                                data: Bytes::from(encrypted),
                                duration: 0.0,
                                created_at: Instant::now(),
                            };

                            // 缓存
                            {
                                let mut segs = segments.write().await;
                                segs.push(cached.clone());
                                while segs.len() > max_segments {
                                    segs.remove(0);
                                }
                            }

                            // 广播 (带 Origin Timestamp)
                            let origin_ts = SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap_or_default()
                                .as_millis() as u64;
                            let frame = Frame::new_with_ts(FrameType::Data, seq_counter, origin_ts, cached.data.clone());
                            let encoded = frame.encode();
                            let _ = broadcast_tx.send(encoded);
                            seq_counter += 1;
                        }
                        Err(e) => {
                            error!("[Relay] Encrypt m3u8 failed: {}", e);
                            continue;
                        }
                    }
                }

                // 拉取新的 TS 分片
                for seg in &parsed.segments {
                    // 去重检查
                    {
                        let fetched = fetched_urls.read().await;
                        if fetched.contains_key(&seg.url) {
                            continue;
                        }
                    }

                    let ts_data = match fetch_url(&client, &seg.url, &origin_sem).await {
                        Ok(d) => d,
                        Err(e) => {
                            warn!("[Relay] Failed to fetch TS: {}", e);
                            continue;
                        }
                    };

                    // 加密 TS
                    let encrypted = {
                        let mut enc = encryptor.write().await;
                        match enc.encrypt(&ts_data) {
                            Ok(e) => e,
                            Err(e) => {
                                error!("[Relay] Encrypt TS failed: {}", e);
                                continue;
                            }
                        }
                    };

                    let cached = CachedSegment {
                        seq: seg.seq,
                        seg_type: SegType::Ts,
                        data: Bytes::from(encrypted),
                        duration: seg.duration,
                        created_at: Instant::now(),
                    };

                    // 缓存
                    {
                        let mut segs = segments.write().await;
                        segs.push(cached.clone());
                        while segs.len() > max_segments {
                            segs.remove(0);
                        }
                    }

                    // 记录已拉取
                    {
                        let mut fetched = fetched_urls.write().await;
                        fetched.insert(seg.url.clone(), seg.seq);
                        if fetched.len() > 100 {
                            // 简单清理: 移除前 20 条
                            let keys: Vec<String> = fetched.keys().take(20).cloned().collect();
                            for k in keys {
                                fetched.remove(&k);
                            }
                        }
                    }

                    // 广播 (带 Origin Timestamp)
                    let origin_ts = SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_millis() as u64;
                    let frame = Frame::new_with_ts(FrameType::Data, seq_counter, origin_ts, cached.data.clone());
                    let encoded = frame.encode();
                    let _ = broadcast_tx.send(encoded);
                    seq_counter += 1;
                }
            }

            info!("[Relay] Pull loop stopped for stream {}", &stream_key[..8.min(stream_key.len())]);
        });
    }

    /// 停止中继
    pub async fn stop(&self) {
        let mut running = self.running.write().await;
        *running = false;
    }

    pub fn is_running(&self) -> bool {
        self.running.try_read().map(|r| *r).unwrap_or(false)
    }

    /// 订阅广播
    pub fn subscribe(&self) -> broadcast::Receiver<Bytes> {
        self.broadcast_tx.subscribe()
    }

    /// 增加客户端计数
    pub async fn add_client(&self) -> bool {
        let mut count = self.client_count.write().await;
        if *count >= self.max_clients {
            return false;
        }
        *count += 1;
        true
    }

    /// 减少客户端计数
    pub async fn remove_client(&self) {
        let mut count = self.client_count.write().await;
        if *count > 0 {
            *count -= 1;
        }
    }

    /// 获取客户端数量
    pub async fn client_count(&self) -> usize {
        *self.client_count.read().await
    }

    /// 获取最新 m3u8 数据
    pub async fn latest_m3u8(&self) -> Option<Bytes> {
        let segs = self.segments.read().await;
        for seg in segs.iter().rev() {
            if seg.seg_type == SegType::M3u8 {
                return Some(seg.data.clone());
            }
        }
        None
    }

    /// 测量到源站的 RTT
    pub async fn measure_rtt(&self) -> u32 {
        let client = reqwest_client();
        let start = Instant::now();
        match client.head(&self.origin_m3u8_url).timeout(Duration::from_secs(5)).send().await {
            Ok(_) => start.elapsed().as_millis() as u32,
            Err(_) => 9999,
        }
    }

    /// 记录发送字节数
    pub async fn record_bytes_sent(&self, n: u64) {
        let mut bytes = self.bytes_sent.write().await;
        *bytes += n;
    }

    /// 获取当前带宽
    pub async fn current_bandwidth(&self) -> u64 {
        *self.bytes_sent.read().await
    }
}

// --- HTTP 客户端 ---

fn reqwest_client() -> reqwest::Client {
    reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .pool_max_idle_per_host(4)
        .pool_idle_timeout(Duration::from_secs(30))
        .build()
        .unwrap_or_default()
}

async fn fetch_url(
    client: &reqwest::Client,
    url: &str,
    sem: &Arc<Semaphore>,
) -> Result<Vec<u8>, String> {
    let _permit = sem.acquire().await.map_err(|e| e.to_string())?;

    let resp = client.get(url)
        .send()
        .await
        .map_err(|e| format!("HTTP error: {}", e))?;

    if !resp.status().is_success() {
        return Err(format!("HTTP {}", resp.status()));
    }

    resp.bytes()
        .await
        .map(|b| b.to_vec())
        .map_err(|e| format!("read body: {}", e))
}

fn simple_hash(data: &[u8]) -> u64 {
    use std::collections::hash_map::DefaultHasher;
    use std::hash::{Hash, Hasher};
    let mut hasher = DefaultHasher::new();
    data.hash(&mut hasher);
    hasher.finish()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_m3u8() {
        let m3u8 = r#"#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:100
#EXTINF:2.000,
stream-100.ts
#EXTINF:2.000,
stream-101.ts
#EXTINF:2.000,
stream-102.ts
"#;
        let parsed = parse_m3u8(m3u8, "http://origin/live/test/stream.m3u8");
        assert_eq!(parsed.target_duration, 2.0);
        assert_eq!(parsed.media_sequence, 100);
        assert_eq!(parsed.segments.len(), 3);
        assert_eq!(parsed.segments[0].seq, 100);
        assert_eq!(parsed.segments[0].duration, 2.0);
        assert_eq!(parsed.segments[0].url, "http://origin/live/test/stream-100.ts");
    }
}
