//! 带宽整形模块
//! 令牌桶限速 + 随机抖动 (让流量曲线更像人类浏览行为)

use std::time::{Duration, Instant};
use tokio::sync::Mutex;
use std::sync::Arc;

/// 令牌桶限速器
pub struct TokenBucket {
    rate: f64,             // bytes/sec
    tokens: f64,           // 当前令牌数
    max_tokens: f64,       // 最大令牌数 (允许突发)
    last_refill: Instant,  // 上次补充时间
    jitter_range: f64,     // 随机抖动范围 (0.0-1.0)
}

impl TokenBucket {
    pub fn new(rate_bytes_per_sec: u64) -> Self {
        Self {
            rate: rate_bytes_per_sec as f64,
            tokens: rate_bytes_per_sec as f64, // 初始满桶
            max_tokens: rate_bytes_per_sec as f64 * 2.0, // 允许 2 秒突发
            last_refill: Instant::now(),
            jitter_range: 0.15, // ±15% 抖动
        }
    }

    /// 尝试消费 n 字节的令牌
    /// 返回需要等待的时间 (0 表示无需等待)
    pub fn consume(&mut self, n: u64) -> Duration {
        self.refill();

        let n_f = n as f64;
        if self.tokens >= n_f {
            self.tokens -= n_f;
            Duration::ZERO
        } else {
            // 不足，计算等待时间
            let deficit = n_f - self.tokens;
            let wait_secs = deficit / self.rate;
            // 添加随机抖动
            let jitter = if self.jitter_range > 0.0 {
                let r = pseudo_random() * self.jitter_range * 2.0 - self.jitter_range;
                wait_secs * r
            } else {
                0.0
            };
            let total = wait_secs + jitter;
            self.tokens = 0.0;
            Duration::from_secs_f64(total.max(0.0))
        }
    }

    /// 获取当前可用带宽 (bytes/sec)
    pub fn available_rate(&self) -> u64 {
        self.rate as u64
    }

    /// 更新限速
    pub fn set_rate(&mut self, rate_bytes_per_sec: u64) {
        self.rate = rate_bytes_per_sec as f64;
        self.max_tokens = rate_bytes_per_sec as f64 * 2.0;
    }

    fn refill(&mut self) {
        let now = Instant::now();
        let elapsed = now.duration_since(self.last_refill).as_secs_f64();
        self.tokens = (self.tokens + elapsed * self.rate).min(self.max_tokens);
        self.last_refill = now;
    }
}

/// 共享的限速器 (Arc<Mutex>)
pub type SharedBucket = Arc<Mutex<TokenBucket>>;

pub fn shared_bucket(rate_bytes_per_sec: u64) -> SharedBucket {
    Arc::new(Mutex::new(TokenBucket::new(rate_bytes_per_sec)))
}

/// 简单伪随机 (不需要 rand crate)
fn pseudo_random() -> f64 {
    use std::sync::atomic::{AtomicU64, Ordering};
    static SEED: AtomicU64 = AtomicU64::new(12345);
    let mut s = SEED.load(Ordering::Relaxed);
    // xorshift64
    s ^= s << 13;
    s ^= s >> 7;
    s ^= s << 17;
    SEED.store(s, Ordering::Relaxed);
    // 映射到 [0, 1)
    (s as f64) / (u64::MAX as f64)
}

/// 指数退避重连策略
pub struct ExponentialBackoff {
    initial: Duration,
    max: Duration,
    multiplier: f64,
    attempt: u32,
}

impl ExponentialBackoff {
    pub fn new(initial: Duration, max: Duration) -> Self {
        Self {
            initial,
            max,
            multiplier: 2.0,
            attempt: 0,
        }
    }

    /// 获取下次重试的等待时间，并递增 attempt
    pub fn next(&mut self) -> Duration {
        let base = self.initial.as_secs_f64() * self.multiplier.powi(self.attempt as i32);
        self.attempt += 1;
        let secs = base.min(self.max.as_secs_f64());
        // 添加 ±25% 抖动
        let jitter = secs * 0.25 * (pseudo_random() * 2.0 - 1.0);
        Duration::from_secs_f64((secs + jitter).max(0.0))
    }

    /// 重置 (连接成功后调用)
    pub fn reset(&mut self) {
        self.attempt = 0;
    }

    /// 当前重试次数
    pub fn attempt(&self) -> u32 {
        self.attempt
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_token_bucket_immediate() {
        let mut bucket = TokenBucket::new(1_000_000); // 1MB/s
        let wait = bucket.consume(1000); // 消费 1KB
        assert_eq!(wait, Duration::ZERO);
    }

    #[test]
    fn test_token_bucket_overflow() {
        let mut bucket = TokenBucket::new(100); // 100B/s, very slow
        let wait = bucket.consume(1000); // 尝试消费 1KB
        assert!(wait.as_millis() > 0);
    }

    #[test]
    fn test_backoff_increasing() {
        let mut bo = ExponentialBackoff::new(Duration::from_secs(1), Duration::from_secs(60));
        let d1 = bo.next();
        let d2 = bo.next();
        let d3 = bo.next();
        assert!(d2 > d1);
        assert!(d3 > d2);
    }

    #[test]
    fn test_backoff_reset() {
        let mut bo = ExponentialBackoff::new(Duration::from_secs(1), Duration::from_secs(60));
        bo.next();
        bo.next();
        assert_eq!(bo.attempt(), 2);
        bo.reset();
        assert_eq!(bo.attempt(), 0);
    }
}
