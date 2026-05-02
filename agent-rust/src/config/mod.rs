use serde::Deserialize;
use std::env;
use std::fs;
use std::path::Path;

#[derive(Debug, Deserialize, Clone)]
pub struct Config {
    pub node_id: String,
    pub public_ip: String,
    #[serde(default)]
    pub public_ipv6: String,           // IPv6 地址, 用于核心<->Agent 通信降成本
    pub port: u16,
    pub region: String,
    pub isp: String,
    #[serde(default = "default_bw_limit")]
    pub bw_limit: u64, // bytes/sec
    pub domain: String,
    #[serde(default = "default_protocol")]
    pub protocol: String,
    #[serde(default = "default_ws_path")]
    pub ws_path: String,
    #[serde(default)]
    pub tls_enabled: bool,
    pub controller_url: String,
    pub reg_token: String,
    #[serde(default = "default_listen")]
    pub listen_addr: String,
    #[serde(default)]
    pub listen_ipv6: Option<String>,    // IPv6 监听地址, 如 "[::]:9090"
    #[serde(default)]
    pub disguise_dir: Option<String>,
    #[serde(default = "default_hb_interval")]
    pub hb_interval_secs: u64,
    #[serde(default = "default_max_clients")]
    pub max_clients: usize,
    #[serde(default = "default_buffer_segments")]
    pub buffer_segments: usize,
    #[serde(default = "default_auto_update")]
    pub auto_update: bool,
    #[serde(default = "default_latency_mode")]
    pub latency_mode: String, // ultra / standard / resilient
    #[serde(default)]
    pub backup_origin_url: Option<String>, // 备源地址, 主源挂了自动切
    #[serde(default)]
    pub origin_flv_url: Option<String>,    // HTTP-FLV 回源模板, 如 http://origin:8080/live/{stream_key}.flv
    #[serde(default)]
    pub cascade_enabled: bool,             // 级联回源: 允许其他 Agent 从本 Agent 拉流
    #[serde(default)]
    pub cascade_upstream: Option<String>,  // 级联上游: 从另一个 Agent 拉流而非 Origin, 如 http://agent1:9090/live/{stream_key}.flv
}

fn default_bw_limit() -> u64 { 10 * 1024 * 1024 } // 10MB/s
fn default_protocol() -> String { "ws".into() }
fn default_ws_path() -> String { "/ws/live".into() }
fn default_listen() -> String { ":9090".into() }
fn default_hb_interval() -> u64 { 5 }
fn default_max_clients() -> usize { 500 }
fn default_buffer_segments() -> usize { 30 }
fn default_auto_update() -> bool { true }
fn default_latency_mode() -> String { "ultra".into() } // 默认极速，自动降级

impl Config {
    pub fn load(path: &str) -> Result<Self, Box<dyn std::error::Error>> {
        let mut cfg = if Path::new(path).exists() {
            let content = fs::read_to_string(path)?;
            toml::from_str(&content)?
        } else {
            Config::default_config()
        };

        // 环境变量覆盖
        if let Ok(v) = env::var("NODE_ID") { cfg.node_id = v; }
        if let Ok(v) = env::var("PUBLIC_IP") { cfg.public_ip = v; }
        if let Ok(v) = env::var("PUBLIC_IPV6") { cfg.public_ipv6 = v; }
        if let Ok(v) = env::var("PORT") { cfg.port = v.parse().unwrap_or(9090); }
        if let Ok(v) = env::var("REGION") { cfg.region = v; }
        if let Ok(v) = env::var("ISP") { cfg.isp = v; }
        if let Ok(v) = env::var("CONTROLLER_URL") { cfg.controller_url = v; }
        if let Ok(v) = env::var("REG_TOKEN") { cfg.reg_token = v; }
        if let Ok(v) = env::var("DOMAIN") { cfg.domain = v; }
        if let Ok(v) = env::var("CASCADE_ENABLED") { cfg.cascade_enabled = v == "true" || v == "1"; }
        if let Ok(v) = env::var("CASCADE_UPSTREAM") { cfg.cascade_upstream = Some(v); }

        // 自动生成 node_id
        if cfg.node_id.is_empty() {
            let hostname = hostname::get()
                .unwrap_or_else(|_| "unknown".into());
            cfg.node_id = format!("edge-{}", &hostname[..hostname.len().min(16)]);
        }

        Ok(cfg)
    }

    fn default_config() -> Self {
        Self {
            node_id: String::new(),
            public_ip: "127.0.0.1".into(),
            public_ipv6: String::new(),
            port: 9090,
            region: "默认".into(),
            isp: "默认".into(),
            bw_limit: default_bw_limit(),
            domain: "localhost".into(),
            protocol: default_protocol(),
            ws_path: default_ws_path(),
            tls_enabled: false,
            controller_url: "http://controller:8080".into(),
            reg_token: "change-me".into(),
            listen_addr: default_listen(),
            listen_ipv6: None,
            disguise_dir: None,
            hb_interval_secs: default_hb_interval(),
            max_clients: default_max_clients(),
            buffer_segments: default_buffer_segments(),
            auto_update: default_auto_update(),
            latency_mode: default_latency_mode(),
            backup_origin_url: None,
            origin_flv_url: None,
            cascade_enabled: false,
            cascade_upstream: None,
        }
    }

    pub fn listen_socket_addr(&self) -> Result<std::net::SocketAddr, Box<dyn std::error::Error>> {
        let addr = if self.listen_addr.starts_with(':') {
            format!("0.0.0.0{}", self.listen_addr)
        } else {
            self.listen_addr.clone()
        };
        Ok(addr.parse()?)
    }

    /// IPv6 监听地址 (核心<->Agent 通信走 IPv6 更便宜)
    pub fn listen_ipv6_socket_addr(&self) -> Option<std::net::SocketAddr> {
        if let Some(ref v6) = self.listen_ipv6 {
            let addr = if v6.starts_with(':') {
                format!("[::0]{}", v6)
            } else {
                v6.clone()
            };
            addr.parse().ok()
        } else {
            None
        }
    }

    pub fn origin_hls_url(&self, stream_key: &str) -> String {
        format!(
            "{}/live/{}/stream.m3u8",
            self.controller_url, stream_key
        )
    }

    /// HTTP-FLV 回源地址
    /// 优先级: cascade_upstream > origin_flv_url > 默认推导
    /// 级联模式: cascade_upstream = 其他 Agent 的地址, 减少核心带宽
    pub fn origin_flv_url(&self, stream_key: &str) -> String {
        // 级联模式: 从上游 Agent 拉流
        if let Some(ref upstream) = self.cascade_upstream {
            return upstream.replace("{stream_key}", stream_key);
        }
        // 显式配置
        if let Some(ref url) = self.origin_flv_url {
            return url.replace("{stream_key}", stream_key);
        }
        // 默认推导
        format!("{}/live/{}.flv", self.controller_url, stream_key)
    }

    /// 是否为级联节点 (从其他 Agent 拉流而非 Origin)
    pub fn is_cascade_node(&self) -> bool {
        self.cascade_upstream.is_some()
    }

    /// 是否允许其他 Agent 从本节点级联拉流
    pub fn is_cascade_relay(&self) -> bool {
        self.cascade_enabled
    }
}

// hostname 获取辅助
mod hostname {
    use std::process::Command;

    pub fn get() -> Result<String, Box<dyn std::error::Error>> {
        let output = Command::new("hostname").output()?;
        Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
    }
}
