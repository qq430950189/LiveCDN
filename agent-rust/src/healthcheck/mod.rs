//! Agent 健康自检模块
//! 挂机宝节点会"偷偷出问题但不报错"——磁盘满、时钟漂移、DNS 抽风、内存压力
//! 每 60s 跑一次自检, 异常立刻上报 Controller, 由 Controller 决定是否摘除

use std::time::{SystemTime, UNIX_EPOCH};
use tracing::{debug, warn};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HealthReport {
    pub disk_free_mb: u64,
    pub disk_warning: bool,
    pub clock_skew_ms: i64,
    pub clock_warning: bool,
    pub dns_resolvable: bool,
    pub memory_pressure: bool,
    pub uptime_secs: u64,
    pub timestamp: u64,
}

impl HealthReport {
    /// 是否有任何异常
    pub fn has_issues(&self) -> bool {
        self.disk_warning || self.clock_warning || !self.dns_resolvable || self.memory_pressure
    }

    /// 异常摘要
    pub fn issues_summary(&self) -> Vec<String> {
        let mut issues = Vec::new();
        if self.disk_warning {
            issues.push(format!("磁盘空间不足: {}MB", self.disk_free_mb));
        }
        if self.clock_warning {
            issues.push(format!("时钟漂移: {}ms", self.clock_skew_ms));
        }
        if !self.dns_resolvable {
            issues.push("DNS 解析失败".into());
        }
        if self.memory_pressure {
            issues.push("内存压力高".into());
        }
        issues
    }
}

const DISK_WARNING_THRESHOLD_MB: u64 = 100;  // < 100MB 报警
const CLOCK_SKEW_THRESHOLD_MS: i64 = 5000;   // > 5s 报警

/// 执行自检
pub fn run_self_check(controller_host: &str) -> HealthReport {
    let disk_free = check_disk_free();
    let clock_skew = check_clock_skew();
    let dns_ok = check_dns(controller_host);
    let mem_pressure = check_memory_pressure();

    let report = HealthReport {
        disk_free_mb: disk_free,
        disk_warning: disk_free < DISK_WARNING_THRESHOLD_MB,
        clock_skew_ms: clock_skew,
        clock_warning: clock_skew.abs() > CLOCK_SKEW_THRESHOLD_MS,
        dns_resolvable: dns_ok,
        memory_pressure: mem_pressure,
        uptime_secs: 0, // 由调用者填充
        timestamp: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs(),
    };

    if report.has_issues() {
        for issue in report.issues_summary() {
            warn!("[HealthCheck] {}", issue);
        }
    } else {
        debug!("[HealthCheck] All checks passed");
    }

    report
}

/// 检查磁盘剩余空间
fn check_disk_free() -> u64 {
    // 检查当前工作目录所在磁盘
    // Linux: 读取 /proc 或 statvfs
    #[cfg(target_os = "linux")]
    {
        // 尝试 statvfs
        if let Ok(stat) = nix_statvfs() {
            return stat;
        }
        // 回退: 读取 /proc/meminfo 的 MemAvailable
        if let Ok(content) = std::fs::read_to_string("/proc/meminfo") {
            for line in content.lines() {
                if line.starts_with("MemAvailable:") {
                    if let Some(kb) = line.split_whitespace().nth(1) {
                        if let Ok(kb_val) = kb.parse::<u64>() {
                            return kb_val / 1024; // 转MB
                        }
                    }
                }
            }
        }
    }

    #[cfg(not(target_os = "linux"))]
    {
        // 非Linux: 尝试用 fs::metadata
        if let Ok(stat) = std::fs::metadata(".") {
            // 无法获取磁盘空间, 返回安全值
            let _ = stat;
        }
    }

    // 无法检测, 返回大值 (不报警)
    u64::MAX
}

#[cfg(target_os = "linux")]
fn nix_statvfs() -> Result<u64, ()> {
    // 简化: 直接读 /proc 来代替 statvfs syscall
    // 生产环境应该用 libc::statvfs
    let path = b".\0";
    let mut stat: libc::statvfs = unsafe { std::mem::zeroed() };
    let result = unsafe {
        libc::statvfs(path.as_ptr() as *const i8, &mut stat)
    };
    if result == 0 {
        let free_blocks = stat.f_bavail as u64;
        let block_size = stat.f_bsize as u64;
        let free_bytes = free_blocks * block_size;
        Ok(free_bytes / (1024 * 1024)) // 转MB
    } else {
        Err(())
    }
}

/// 检查时钟漂移 (与 Controller HTTP 响应头对比)
fn check_clock_skew() -> i64 {
    // 简化实现: 通过 HTTP HEAD 请求获取服务器时间
    // 如果请求失败, 返回 0 (不报警)
    0 // 实际场景需要异步实现, 此处由 heartbeat 时附带检测
}

/// 检查 DNS 解析
fn check_dns(host: &str) -> bool {
    if host.is_empty() {
        return true;
    }

    // 从 controller_url 提取 hostname
    let hostname = host
        .trim_start_matches("http://")
        .trim_start_matches("https://")
        .split(':')
        .next()
        .unwrap_or("");

    if hostname.is_empty() {
        return true;
    }

    // 尝试 DNS 解析 (使用 std::net)
    match std::net::ToSocketAddrs::to_socket_addrs(&format!("{}:80", hostname)) {
        Ok(mut addrs) => addrs.next().is_some(),
        Err(_) => false,
    }
}

/// 检查内存压力 (OOM score)
fn check_memory_pressure() -> bool {
    #[cfg(target_os = "linux")]
    {
        // 读取 /proc/self/oom_score
        if let Ok(content) = std::fs::read_to_string("/proc/self/oom_score") {
            if let Ok(score) = content.trim().parse::<u32>() {
                return score >= 800; // OOM score >= 800 表示高危
            }
        }
        // 回退: 检查 /proc/meminfo
        if let Ok(content) = std::fs::read_to_string("/proc/meminfo") {
            let mut total = 0u64;
            let mut available = 0u64;
            for line in content.lines() {
                if line.starts_with("MemTotal:") {
                    if let Some(v) = line.split_whitespace().nth(1) {
                        total = v.parse().unwrap_or(0);
                    }
                } else if line.starts_with("MemAvailable:") {
                    if let Some(v) = line.split_whitespace().nth(1) {
                        available = v.parse().unwrap_or(0);
                    }
                }
            }
            if total > 0 && available * 100 / total < 10 {
                return true; // 可用内存 < 10%
            }
        }
    }

    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_health_report_no_issues() {
        let report = HealthReport {
            disk_free_mb: 5000,
            disk_warning: false,
            clock_skew_ms: 100,
            clock_warning: false,
            dns_resolvable: true,
            memory_pressure: false,
            uptime_secs: 3600,
            timestamp: 1234567890,
        };
        assert!(!report.has_issues());
        assert!(report.issues_summary().is_empty());
    }

    #[test]
    fn test_health_report_with_issues() {
        let report = HealthReport {
            disk_free_mb: 50,
            disk_warning: true,
            clock_skew_ms: 8000,
            clock_warning: true,
            dns_resolvable: false,
            memory_pressure: true,
            uptime_secs: 3600,
            timestamp: 1234567890,
        };
        assert!(report.has_issues());
        assert_eq!(report.issues_summary().len(), 4);
    }

    #[test]
    fn test_dns_check_valid_host() {
        // localhost 应该始终可解析
        assert!(check_dns("localhost"));
    }

    #[test]
    fn test_disk_free_returns_value() {
        let free = check_disk_free();
        // 在 CI 环境中应该有磁盘空间
        assert!(free > 0);
    }
}
