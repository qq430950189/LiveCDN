//! 传输层抽象 — 可插拔传输协议
//! 统一"流帧管道", 下层挂载多种 transport
//!
//! 模块结构:
//!   - mod.rs: 兼容层, 重新导出 registry 中的类型
//!   - registry.rs: 全局注册表 + 工厂模式 (参考 Xray-core)
//!
//! 当前实现: PlainWS (完整 WebSocket 传输)
//! 未来实现: Reality / Hysteria2 / gRPC

// 重新导出 registry 中的所有公共类型
pub mod registry;

// 兼容性重新导出 — 现有代码使用的类型
pub use registry::{
    TransportType, TransportConfig, TransportConn, TransportError,
    TransportSwitchCommand, SecurityLayer,
    global_registry, dial_transport,
};
