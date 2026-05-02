//! 传输层注册表 — 参考 Xray-core 的 init() 自注册 + 全局注册表模式
//!
//! Xray 的 transport/internet 模块使用三层全局注册表:
//!   - Dialers: 出站连接器 (System → Proxy → Security → Transport)
//!   - Listeners: 入站监听器
//!   - Configs: 配置解析器
//!
//! 每种传输通过 init() 函数在编译时自注册到全局注册表,
//! 运行时通过名字查找并创建实例, 实现真正的可插拔。
//!
//! 本模块实现:
//!   - 全局 TransportRegistry (lazy-init singleton)
//!   - TransportFactory trait (自注册的工厂模式)
//!   - 分层连接: Security Layer (TLS/REALITY) + Transport Layer (WS/H2/gRPC)
//!   - PlainWS 完整实现 (替代 transport/mod.rs 中的占位实现)

use async_trait::async_trait;
use bytes::Bytes;
use std::collections::HashMap;
use std::fmt;
use std::sync::Arc;
use tokio::sync::RwLock;
use tracing::{debug, info, warn};

use crate::protocol::{Frame, FrameType};
use futures_util::{SinkExt, StreamExt};

// --- 错误类型 ---

#[derive(Debug)]
pub enum TransportError {
    ConnectionFailed(String),
    SendFailed(String),
    RecvFailed(String),
    NotConnected,
    UnsupportedTransport(String),
    RegistrationFailed(String),
    Timeout(String),
    TlsHandshakeFailed(String),
}

impl std::fmt::Display for TransportError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::ConnectionFailed(s) => write!(f, "connection failed: {}", s),
            Self::SendFailed(s) => write!(f, "send failed: {}", s),
            Self::RecvFailed(s) => write!(f, "recv failed: {}", s),
            Self::NotConnected => write!(f, "not connected"),
            Self::UnsupportedTransport(s) => write!(f, "unsupported transport: {}", s),
            Self::RegistrationFailed(s) => write!(f, "registration failed: {}", s),
            Self::Timeout(s) => write!(f, "timeout: {}", s),
            Self::TlsHandshakeFailed(s) => write!(f, "TLS handshake failed: {}", s),
        }
    }
}

impl std::error::Error for TransportError {}

// --- Transport 类型标识 ---

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum TransportType {
    PlainWS,
    Reality,
    Hysteria2,
    GRPC,
}

impl fmt::Display for TransportType {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::PlainWS => write!(f, "plain-ws"),
            Self::Reality => write!(f, "reality"),
            Self::Hysteria2 => write!(f, "hysteria2"),
            Self::GRPC => write!(f, "grpc"),
        }
    }
}

impl TransportType {
    pub fn from_str(s: &str) -> Option<Self> {
        match s {
            "plain-ws" | "ws" => Some(Self::PlainWS),
            "reality" => Some(Self::Reality),
            "hysteria2" => Some(Self::Hysteria2),
            "grpc" => Some(Self::GRPC),
            _ => None,
        }
    }
}

use serde::{Deserialize, Serialize};

// --- Transport 配置 ---

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TransportConfig {
    pub transport_type: TransportType,
    pub address: String,
    pub path: String,
    pub tls_enabled: bool,
    // Reality 特有配置
    pub reality_private_key: Option<String>,
    pub reality_short_id: Option<String>,
    pub reality_server_names: Option<Vec<String>>,
    pub reality_public_key: Option<String>,
}

// --- Transport 连接抽象 ---

/// 传输连接 — 单个活跃连接的抽象
#[async_trait]
pub trait TransportConn: Send + Sync {
    /// 发送帧
    async fn send_frame(&mut self, frame: &Frame) -> Result<(), TransportError>;

    /// 接收帧
    async fn recv_frame(&mut self) -> Result<Option<Frame>, TransportError>;

    /// 关闭连接
    async fn close(&mut self) -> Result<(), TransportError>;

    /// 连接类型
    fn transport_type(&self) -> TransportType;

    /// 连接是否活跃
    fn is_connected(&self) -> bool;
}

// --- Transport 工厂 trait ---

/// 传输层工厂 — 每种传输协议实现此 trait 并注册到全局注册表
///
/// 参考 Xray 的 transport/internet/registry.go:
///   - 每个 transport 包提供 init() 函数
///   - init() 调用 registry.RegisterDialer() 注册自己
///   - 运行时通过名字查找并创建实例
#[async_trait]
pub trait TransportFactory: Send + Sync {
    /// 传输类型标识
    fn transport_type(&self) -> TransportType;

    /// 创建出站连接 (dial)
    async fn dial(&self, config: &TransportConfig) -> Result<Box<dyn TransportConn>, TransportError>;

    /// 此传输是否需要安全层 (TLS/REALITY)
    fn requires_security_layer(&self) -> bool {
        false
    }

    /// 此传输的默认端口
    fn default_port(&self) -> u16 {
        80
    }
}

// --- 全局注册表 ---

/// 传输层全局注册表
///
/// 参考 Xray 的三注册表模式 (DialerRegistry / ListenerRegistry / ConfigRegistry)
/// 简化为单一注册表, 运行时通过 TransportType 查找工厂
pub struct TransportRegistry {
    factories: HashMap<TransportType, Box<dyn TransportFactory>>,
}

impl TransportRegistry {
    fn new() -> Self {
        Self {
            factories: HashMap::new(),
        }
    }

    /// 注册传输工厂
    pub fn register(&mut self, factory: Box<dyn TransportFactory>) -> Result<(), TransportError> {
        let tt = factory.transport_type();
        if self.factories.contains_key(&tt) {
            return Err(TransportError::RegistrationFailed(
                format!("transport type {} already registered", tt),
            ));
        }
        info!("[TransportRegistry] Registered transport: {}", tt);
        self.factories.insert(tt, factory);
        Ok(())
    }

    /// 查找工厂
    pub fn get(&self, tt: &TransportType) -> Option<&dyn TransportFactory> {
        self.factories.get(tt).map(|f| f.as_ref())
    }

    /// 列出已注册的传输类型
    pub fn list_registered(&self) -> Vec<TransportType> {
        self.factories.keys().copied().collect()
    }

    /// 使用工厂创建连接 (dial)
    pub async fn dial(&self, config: &TransportConfig) -> Result<Box<dyn TransportConn>, TransportError> {
        let factory = self.get(&config.transport_type).ok_or_else(|| {
            TransportError::UnsupportedTransport(
                format!("{} not registered", config.transport_type),
            )
        })?;
        factory.dial(config).await
    }
}

// 全局注册表 (lazy init)
use std::sync::OnceLock;

static GLOBAL_REGISTRY: OnceLock<Arc<RwLock<TransportRegistry>>> = OnceLock::new();

/// 获取全局注册表
pub fn global_registry() -> Arc<RwLock<TransportRegistry>> {
    GLOBAL_REGISTRY.get_or_init(|| {
        let mut registry = TransportRegistry::new();
        // 注册内置传输
        let _ = registry.register(Box::new(PlainWsTransportFactory));
        Arc::new(RwLock::new(registry))
    }).clone()
}

/// 便捷函数: 使用全局注册表创建连接
pub async fn dial_transport(config: &TransportConfig) -> Result<Box<dyn TransportConn>, TransportError> {
    let registry = global_registry();
    let reg = registry.read().await;
    reg.dial(config).await
}

// --- PlainWS Transport 实现 ---

/// PlainWS 工厂
struct PlainWsTransportFactory;

#[async_trait]
impl TransportFactory for PlainWsTransportFactory {
    fn transport_type(&self) -> TransportType {
        TransportType::PlainWS
    }

    async fn dial(&self, config: &TransportConfig) -> Result<Box<dyn TransportConn>, TransportError> {
        PlainWsConn::connect(config).await
    }

    fn requires_security_layer(&self) -> bool {
        false
    }

    fn default_port(&self) -> u16 {
        80
    }
}

/// PlainWS 连接 — 完整的 WebSocket 传输实现
///
/// 连接流程:
///   1. TCP 连接到目标地址
///   2. WebSocket 握手
///   3. 收发二进制帧 (协议帧)
struct PlainWsConn {
    sender: futures_util::stream::SplitSink<
        tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>,
        tokio_tungstenite::tungstenite::Message,
    >,
    receiver: futures_util::stream::SplitStream<
        tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>,
    >,
    connected: bool,
}

impl PlainWsConn {
    async fn connect(config: &TransportConfig) -> Result<Box<dyn TransportConn>, TransportError> {
        let scheme = if config.tls_enabled { "wss" } else { "ws" };
        let url = format!("{}://{}{}", scheme, config.address, config.path);

        debug!("[PlainWS] Connecting to {}", url);

        let (ws_stream, _) = tokio_tungstenite::connect_async(&url)
            .await
            .map_err(|e| TransportError::ConnectionFailed(format!("WS connect to {}: {}", url, e)))?;

        info!("[PlainWS] Connected to {}", url);

        let (sender, receiver) = ws_stream.split();

        Ok(Box::new(Self {
            sender,
            receiver,
            connected: true,
        }))
    }
}

#[async_trait]
impl TransportConn for PlainWsConn {
    async fn send_frame(&mut self, frame: &Frame) -> Result<(), TransportError> {
        if !self.connected {
            return Err(TransportError::NotConnected);
        }

        let data = frame.encode();
        let msg = tokio_tungstenite::tungstenite::Message::Binary(data.to_vec());
        self.sender.send(msg).await.map_err(|e| {
            self.connected = false;
            TransportError::SendFailed(format!("WS send: {}", e))
        })
    }

    async fn recv_frame(&mut self) -> Result<Option<Frame>, TransportError> {
        if !self.connected {
            return Err(TransportError::NotConnected);
        }

        loop {
            match self.receiver.next().await {
                Some(Ok(msg)) => {
                    match msg {
                        tokio_tungstenite::tungstenite::Message::Binary(data) => {
                            match Frame::decode(&data) {
                                Ok(Some((frame, _))) => return Ok(Some(frame)),
                                Ok(None) => {
                                    // 数据不完整, 继续读
                                    continue;
                                }
                                Err(e) => return Err(TransportError::RecvFailed(
                                    format!("frame decode: {}", e),
                                )),
                            }
                        }
                        tokio_tungstenite::tungstenite::Message::Close(_) => {
                            self.connected = false;
                            return Ok(None);
                        }
                        tokio_tungstenite::tungstenite::Message::Ping(_) => {
                            // Pong 会自动回复, 继续读
                            continue;
                        }
                        _ => continue, // Text/Pong 等忽略
                    }
                }
                Some(Err(e)) => {
                    self.connected = false;
                    return Err(TransportError::RecvFailed(format!("WS recv: {}", e)));
                }
                None => {
                    self.connected = false;
                    return Ok(None);
                }
            }
        }
    }

    async fn close(&mut self) -> Result<(), TransportError> {
        if !self.connected {
            return Ok(());
        }
        let _ = self.sender.send(tokio_tungstenite::tungstenite::Message::Close(None)).await;
        self.connected = false;
        Ok(())
    }

    fn transport_type(&self) -> TransportType {
        TransportType::PlainWS
    }

    fn is_connected(&self) -> bool {
        self.connected
    }
}

// --- 安全层抽象 ---

/// 安全层 — 在传输层之上增加 TLS/REALITY 等安全能力
///
/// 参考 Xray 的分层连接模型:
///   System → Proxy → Security → Transport
///
/// 当前只定义接口, 具体实现留给未来
#[async_trait]
pub trait SecurityLayer: Send + Sync {
    /// 安全层名称
    fn name(&self) -> &str;

    /// 包装传输连接, 增加安全层
    async fn wrap(&self, conn: Box<dyn TransportConn>) -> Result<Box<dyn TransportConn>, TransportError>;
}

// --- 协议切换指令 ---

/// Controller 下发的传输层切换指令
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TransportSwitchCommand {
    pub target_transport: TransportType,
    pub config: TransportConfig,
    pub deadline_secs: u32,
    pub reason: String,
}

impl TransportSwitchCommand {
    pub fn from_json(json: &str) -> Result<Self, serde_json::Error> {
        serde_json::from_str(json)
    }

    pub fn to_json(&self) -> String {
        serde_json::to_string(self).unwrap_or_default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_transport_type_display() {
        assert_eq!(TransportType::PlainWS.to_string(), "plain-ws");
        assert_eq!(TransportType::Reality.to_string(), "reality");
        assert_eq!(TransportType::Hysteria2.to_string(), "hysteria2");
        assert_eq!(TransportType::GRPC.to_string(), "grpc");
    }

    #[test]
    fn test_transport_type_from_str() {
        assert_eq!(TransportType::from_str("ws"), Some(TransportType::PlainWS));
        assert_eq!(TransportType::from_str("plain-ws"), Some(TransportType::PlainWS));
        assert_eq!(TransportType::from_str("reality"), Some(TransportType::Reality));
        assert_eq!(TransportType::from_str("unknown"), None);
    }

    #[test]
    fn test_registry_register_and_list() {
        let mut registry = TransportRegistry::new();
        let _ = registry.register(Box::new(PlainWsTransportFactory));

        let registered = registry.list_registered();
        assert_eq!(registered.len(), 1);
        assert!(registered.contains(&TransportType::PlainWS));
    }

    #[test]
    fn test_registry_duplicate_registration() {
        let mut registry = TransportRegistry::new();
        let _ = registry.register(Box::new(PlainWsTransportFactory));
        let result = registry.register(Box::new(PlainWsTransportFactory));
        assert!(result.is_err());
    }

    #[test]
    fn test_registry_get_factory() {
        let mut registry = TransportRegistry::new();
        let _ = registry.register(Box::new(PlainWsTransportFactory));

        let factory = registry.get(&TransportType::PlainWS);
        assert!(factory.is_some());
        assert_eq!(factory.unwrap().transport_type(), TransportType::PlainWS);

        let missing = registry.get(&TransportType::Reality);
        assert!(missing.is_none());
    }

    #[test]
    fn test_transport_switch_command() {
        let cmd = TransportSwitchCommand {
            target_transport: TransportType::Reality,
            config: TransportConfig {
                transport_type: TransportType::Reality,
                address: "reality.example.com:443".into(),
                path: "/ws/live".into(),
                tls_enabled: true,
                reality_private_key: Some("private_key".into()),
                reality_short_id: Some("abcd1234".into()),
                reality_server_names: Some(vec!["example.com".into()]),
                reality_public_key: Some("public_key".into()),
            },
            deadline_secs: 30,
            reason: "domain blocked".into(),
        };
        let json = cmd.to_json();
        let parsed = TransportSwitchCommand::from_json(&json).unwrap();
        assert_eq!(parsed.target_transport, TransportType::Reality);
        assert_eq!(parsed.deadline_secs, 30);
    }

    #[test]
    fn test_global_registry_has_plainws() {
        let registry = global_registry();
        let reg = registry.blocking_read();
        let registered = reg.list_registered();
        assert!(registered.contains(&TransportType::PlainWS));
    }
}
