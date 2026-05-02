//! Agent 自更新模块
//! 启动时检查 Controller 上的版本号, 发现新版本自动下载 → 原子替换 → 重启
//! 参考 self_update crate 实现

use crate::config::Config;
use std::path::PathBuf;
use tracing::{info, warn, error};

/// 当前 Agent 版本 (编译时注入)
pub const AGENT_VERSION: &str = env!("CARGO_PKG_VERSION");

/// 检查更新并执行自更新
/// 返回 true 表示已更新并应该重启
pub async fn check_and_update(config: &Config) -> bool {
    let controller_url = &config.controller_url;

    // 1. 检查 Controller 上是否有新版本
    let version_url = format!("{}/api/admin/agent_version", controller_url);
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(10))
        .build()
        .unwrap_or_default();

    let resp = match client.get(&version_url)
        .header("Authorization", format!("Bearer {}", config.reg_token))
        .send()
        .await
    {
        Ok(r) => r,
        Err(e) => {
            warn!("检查更新失败: {}", e);
            return false;
        }
    };

    let info: VersionInfo = match resp.json().await {
        Ok(v) => v,
        Err(e) => {
            warn!("解析版本信息失败: {}", e);
            return false;
        }
    };

    if info.version == AGENT_VERSION {
        info!("Agent 已是最新版本: {}", AGENT_VERSION);
        return false;
    }

    info!("发现新版本: {} (当前: {})", info.version, AGENT_VERSION);

    // 2. 下载新版本二进制
    let download_url = match info.download_url {
        Some(ref url) => url.clone(),
        None => {
            // 使用默认下载路径
            format!("{}/downloads/livecdn-agent-x86_64-unknown-linux-musl", controller_url)
        }
    };

    info!("正在下载: {}", download_url);

    let resp = match client.get(&download_url).send().await {
        Ok(r) => r,
        Err(e) => {
            error!("下载新版本失败: {}", e);
            return false;
        }
    };

    if !resp.status().is_success() {
        error!("下载新版本失败: HTTP {}", resp.status());
        return false;
    }

    let new_binary = match resp.bytes().await {
        Ok(b) => b.to_vec(),
        Err(e) => {
            error!("读取新版本二进制失败: {}", e);
            return false;
        }
    };

    // 3. 原子替换当前二进制
    let current_exe = match std::env::current_exe() {
        Ok(p) => p,
        Err(e) => {
            error!("获取当前二进制路径失败: {}", e);
            return false;
        }
    };

    // 先备份当前版本
    let backup_path = current_exe.with_extension("bak");
    if let Err(e) = std::fs::copy(&current_exe, &backup_path) {
        warn!("备份当前版本失败: {} (继续更新)", e);
    }

    // 写入新版本到临时文件
    let tmp_path = current_exe.with_extension("new");
    if let Err(e) = std::fs::write(&tmp_path, &new_binary) {
        error!("写入新版本失败: {}", e);
        return false;
    }

    // 设置可执行权限
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if let Err(e) = std::fs::set_permissions(&tmp_path, std::fs::Permissions::from_mode(0o755)) {
            error!("设置权限失败: {}", e);
            let _ = std::fs::remove_file(&tmp_path);
            return false;
        }
    }

    // 原子替换
    if let Err(e) = std::fs::rename(&tmp_path, &current_exe) {
        error!("替换二进制失败: {} (尝试回滚)", e);
        // 回滚: 从备份恢复
        if backup_path.exists() {
            let _ = std::fs::rename(&backup_path, &current_exe);
        }
        let _ = std::fs::remove_file(&tmp_path);
        return false;
    }

    info!("Agent 已更新到版本: {} (重启生效)", info.version);
    true
}

/// 回滚到上一版本
pub fn rollback() -> bool {
    let current_exe = match std::env::current_exe() {
        Ok(p) => p,
        Err(_) => return false,
    };

    let backup_path = current_exe.with_extension("bak");
    if !backup_path.exists() {
        warn!("没有备份版本可回滚");
        return false;
    }

    if let Err(e) = std::fs::rename(&backup_path, &current_exe) {
        error!("回滚失败: {}", e);
        return false;
    }

    info!("已回滚到上一版本");
    true
}

#[derive(serde::Deserialize)]
struct VersionInfo {
    version: String,
    download_url: Option<String>,
}

/// 获取二进制路径
pub fn current_exe_path() -> Option<PathBuf> {
    std::env::current_exe().ok()
}
