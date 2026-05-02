//! LiveCDN Edge Agent
//! 高性能、低占用的直播边缘节点
//! 
//! 技术栈: Rust + Tokio + Hyper + Ring
//! 目标: <10MB RSS, <2% CPU idle

mod config;
mod crypto;
mod disguise;
mod healthcheck;
mod protocol;
mod relay;
mod transport;
mod updater;

use crate::config::Config;
use crate::disguise::DisguiseRouter;
use crate::protocol::{Frame, FrameType, HeartbeatPayload, RegisterPayload};
use crate::relay::{FlvPuller, StreamRelay, CascadeSelector, MeshMapResponse};
use crate::relay::flv_output;
use crate::relay::packet_drop::BackpressureTracker;
use crate::crypto::KeyInfo;

use futures_util::{SinkExt, StreamExt};
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use std::collections::HashMap;
use std::sync::Arc;
use tokio::net::TcpListener;
use tokio::sync::RwLock;
use tracing::{debug, error, info, warn};
use tracing_subscriber::EnvFilter;

type SharedRelays = Arc<RwLock<HashMap<String, Arc<StreamRelay>>>>;
type SharedFlvRelays = Arc<RwLock<HashMap<String, Arc<FlvPuller>>>>;

struct AppState {
    config: Config,
    relays: SharedRelays,
    flv_relays: SharedFlvRelays,
    disguise: DisguiseRouter,
    cascade_selector: CascadeSelector,
}

#[tokio::main]
async fn main() {
    // 初始化日志
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| EnvFilter::new("livecdn_agent=info")),
        )
        .json()
        .init();

    // 加载配置
    let config_path = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "configs/agent.toml".into());
    let config = Config::load(&config_path).unwrap_or_else(|e| {
        eprintln!("配置加载失败: {}", e);
        std::process::exit(1);
    });

    info!(
        node_id = %config.node_id,
        region = %config.region,
        isp = %config.isp,
        port = config.port,
        version = updater::AGENT_VERSION,
        "LiveCDN Agent 启动"
    );

    // 检查自更新 (如果更新成功，打印日志但不自动重启，等待 systemd 重启)
    if config.auto_update {
        let update_config = config.clone();
        tokio::spawn(async move {
            // 延迟 30 秒后再检查更新，避免启动时就大量请求
            tokio::time::sleep(std::time::Duration::from_secs(30)).await;
            match updater::check_and_update(&update_config).await {
                true => {
                    info!("Agent 已更新，将在下次重启时生效");
                }
                false => {}
            }
        });
    }

    let addr = config.listen_socket_addr().expect("无效监听地址");
    let state = Arc::new(AppState {
        config: config.clone(),
        relays: Arc::new(RwLock::new(HashMap::new())),
        flv_relays: Arc::new(RwLock::new(HashMap::new())),
        disguise: DisguiseRouter::new(None),
        cascade_selector: CascadeSelector::new(
            config.node_id.clone(),
            config.region.clone(),
            config.isp.clone(),
            !config.public_ipv6.is_empty(),
        ),
    });

    // 注册到调度中心
    let state_clone = state.clone();
    tokio::spawn(async move {
        if let Err(e) = register_with_controller(&state_clone.config).await {
            warn!("注册调度中心失败 (将在心跳中重试): {}", e);
        }
    });

    // 启动心跳上报
    let hb_state = state.clone();
    tokio::spawn(heartbeat_loop(hb_state));

    // 启动 Mesh Ping 探测循环 (边缘自治: Agent 本地探测候选上游延迟)
    // 参考 Tailscale Disco Ping + Envoy 主动健康检查
    let ping_state = state.clone();
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(
            std::time::Duration::from_secs(10) // 默认 10s 探测一次
        );
        // 首次延迟 15s, 等心跳拿到候选列表
        tokio::time::sleep(std::time::Duration::from_secs(15)).await;
        loop {
            interval.tick().await;
            ping_state.cascade_selector.run_mesh_ping().await;
        }
    });

    // 启动健康自检 (60s 间隔)
    let hc_state = state.clone();
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(std::time::Duration::from_secs(60));
        let start_time = std::time::Instant::now();
        loop {
            interval.tick().await;
            let report = healthcheck::run_self_check(&hc_state.config.controller_url);
            if report.has_issues() {
                for issue in report.issues_summary() {
                    warn!("[HealthCheck] {}", issue);
                }
            }
            let _ = start_time; // uptime tracking
        }
    });

    // 启动 IPv4 TCP 监听
    let listener_v4 = TcpListener::bind(addr).await.expect("绑定端口失败");
    info!(addr = %addr, "IPv4 服务启动");

    // 启动 IPv6 TCP 监听 (核心<->Agent 走 IPv6 更便宜)
    if let Some(v6_addr) = config.listen_ipv6_socket_addr() {
        match TcpListener::bind(v6_addr).await {
            Ok(v6_listener) => {
                info!(addr = %v6_addr, "IPv6 服务启动");
                let v6_state = state.clone();
                tokio::spawn(async move {
                    loop {
                        match v6_listener.accept().await {
                            Ok((stream, remote_addr)) => {
                                let state = v6_state.clone();
                                tokio::spawn(async move {
                                    if let Err(e) = handle_connection(stream, state).await {
                                        error!(?remote_addr, "IPv6 连接处理错误: {:?}", e);
                                    }
                                });
                            }
                            Err(e) => {
                                error!("IPv6 接受连接失败: {}", e);
                            }
                        }
                    }
                });
            }
            Err(e) => {
                warn!("IPv6 监听绑定失败 (将继续仅 IPv4): {}", e);
            }
        }
    }

    let (shutdown_tx, mut shutdown_rx) = tokio::sync::broadcast::channel::<()>(1);

    // 信号处理
    let shutdown_tx_clone = shutdown_tx.clone();
    tokio::spawn(async move {
        tokio::signal::ctrl_c().await.ok();
        info!("收到关闭信号，开始优雅关闭...");
        let _ = shutdown_tx_clone.send(());
    });

    // 接受 IPv4 连接
    loop {
        tokio::select! {
            accept_result = listener_v4.accept() => {
                match accept_result {
                    Ok((stream, remote_addr)) => {
                        let state = state.clone();
                        tokio::spawn(async move {
                            if let Err(e) = handle_connection(stream, state).await {
                                error!(?remote_addr, "连接处理错误: {:?}", e);
                            }
                        });
                    }
                    Err(e) => {
                        error!("接受连接失败: {}", e);
                    }
                }
            }
            _ = shutdown_rx.recv() => {
                info!("开始优雅关闭...");
                break;
            }
        }
    }

    // 停止所有中继
    let relays = state.relays.read().await;
    for (_, relay) in relays.iter() {
        relay.stop().await;
    }
    drop(relays);
    let flv_relays = state.flv_relays.read().await;
    for (_, relay) in flv_relays.iter() {
        relay.stop().await;
    }
    info!("所有中继已停止");

    tokio::time::sleep(std::time::Duration::from_secs(2)).await;
    info!("Agent 已关闭");
}

/// 处理单个 TCP 连接 - 检测 WebSocket vs HTTP-FLV vs HTTP
async fn handle_connection(
    stream: tokio::net::TcpStream,
    state: Arc<AppState>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    // 先 peek 前几个字节判断连接类型
    let mut buf = [0u8; 4096];
    stream.peek(&mut buf).await?;

    let peek_str = String::from_utf8_lossy(&buf);

    if peek_str.contains("Upgrade: websocket") || peek_str.contains("Upgrade: WebSocket") {
        // WebSocket 连接
        handle_websocket_connection(stream, state).await
    } else if is_flv_request(&peek_str) {
        // HTTP-FLV 请求 - TCP 层直接处理流式输出
        handle_http_flv_connection(stream, state).await
    } else {
        // 普通 HTTP 连接
        let io = TokioIo::new(stream);
        hyper::server::conn::http1::Builder::new()
            .serve_connection(
                io,
                service_fn(move |req| {
                    let state = state.clone();
                    handle_http_request(req, state)
                }),
            )
            .await?;
        Ok(())
    }
}

/// 检测是否是 HTTP-FLV 请求 (/live/ 或 /cascade/)
fn is_flv_request(peek_str: &str) -> bool {
    // GET /live/xxx.flv HTTP/1.1 或 GET /cascade/xxx.flv HTTP/1.1
    if let Some(line) = peek_str.lines().next() {
        line.contains(".flv") && line.starts_with("GET") &&
            (line.contains("/live/") || line.contains("/cascade/"))
    } else {
        false
    }
}

/// WebSocket 连接处理 (支持加密模式 + FLV 模式)
async fn handle_websocket_connection(
    stream: tokio::net::TcpStream,
    state: Arc<AppState>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let addr = stream.peer_addr()?;
    let ws_stream = tokio_tungstenite::accept_async(stream).await?;
    let (mut ws_sender, mut ws_receiver) = ws_stream.split();

    // 第一个消息: 握手
    let (stream_key, flv_mode) = match ws_receiver.next().await {
        Some(Ok(msg)) => {
            let text = msg.to_text()?;
            let handshake: serde_json::Value = serde_json::from_str(text)?;
            let key = handshake.get("stream_key")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string();
            let mode = handshake.get("mode")
                .and_then(|v| v.as_str())
                .unwrap_or("encrypted")
                .to_string();
            (key, mode == "flv")
        }
        _ => {
            warn!(?addr, "WebSocket 握手失败");
            return Ok(());
        }
    };

    if stream_key.is_empty() {
        let _ = ws_sender.send(tokio_tungstenite::tungstenite::Message::Text(
            r#"{"error":"missing stream_key"}"#.into(),
        )).await;
        return Ok(());
    }

    if flv_mode {
        // FLV 模式: 发送原始 FLV 标签数据 (低延迟, mpegts.js 兼容)
        handle_ws_flv_mode(stream_key, ws_sender, ws_receiver, &state).await
    } else {
        // 加密模式: 发送加密协议帧 (原有行为)
        handle_ws_encrypted_mode(stream_key, ws_sender, ws_receiver, &state).await
    }
}

/// WS FLV 模式 - 发送原始 FLV 数据
async fn handle_ws_flv_mode(
    stream_key: String,
    mut ws_sender: futures_util::stream::SplitSink<
        tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
        tokio_tungstenite::tungstenite::Message,
    >,
    mut ws_receiver: futures_util::stream::SplitStream<
        tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
    >,
    state: &Arc<AppState>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let relay = get_or_create_flv_relay(&stream_key, state).await?;

    if !relay.add_client().await {
        let _ = ws_sender.send(tokio_tungstenite::tungstenite::Message::Text(
            r#"{"error":"max clients reached"}"#.into(),
        )).await;
        return Ok(());
    }

    info!(stream = &stream_key[..8.min(stream_key.len())], "WS-FLV 客户端连接");

    // 发送 FLV header 作为第一条消息
    let flv_header = flv_output::build_flv_header();
    if ws_sender.send(tokio_tungstenite::tungstenite::Message::Binary(flv_header)).await.is_err() {
        relay.remove_client().await;
        return Ok(());
    }

    // 发送 GOP 缓存的启动标签 (新客户端秒开首帧)
    let startup_tags = relay.get_startup_tags().await;
    if !startup_tags.is_empty() {
        let mut startup_buf = Vec::with_capacity(64 * 1024);
        for tag in &startup_tags {
            startup_buf.extend_from_slice(&flv_output::build_flv_tag_wire(tag));
        }
        let startup_len = startup_buf.len();
        if ws_sender.send(tokio_tungstenite::tungstenite::Message::Binary(startup_buf)).await.is_err() {
            relay.remove_client().await;
            return Ok(());
        }
        relay.record_bytes_sent(startup_len as u64).await;
        info!(
            stream = &stream_key[..8.min(stream_key.len())],
            tags = startup_tags.len(),
            "[WS-FLV] Sent GOP cache startup tags"
        );
    }

    // 订阅广播
    let mut rx = relay.subscribe();
    let relay_clone = relay.clone();
    let stream_key_log = stream_key.clone();

    // 接收任务: 心跳/Pong
    let recv_handle = tokio::spawn(async move {
        while let Some(msg) = ws_receiver.next().await {
            match msg {
                Ok(tokio_tungstenite::tungstenite::Message::Ping(_)) => {}
                Ok(tokio_tungstenite::tungstenite::Message::Close(_)) => break,
                Err(_) => break,
                _ => {}
            }
        }
    });

    // 发送任务: 广播 FLV tags → 客户端 (集成智能丢包)
    let send_handle = tokio::spawn(async move {
        let mut bp_tracker = BackpressureTracker::with_default_policy();
        loop {
            match rx.recv().await {
                Ok(tag_packet) => {
                    // 智能丢包: 背压时优先丢 P 帧
                    if bp_tracker.should_drop(&tag_packet) {
                        continue;
                    }
                    let tag_bytes = flv_output::build_flv_tag_wire(&tag_packet);
                    let byte_len = tag_bytes.len();
                    let msg = tokio_tungstenite::tungstenite::Message::Binary(tag_bytes);
                    match ws_sender.send(msg).await {
                        Ok(_) => {
                            relay_clone.record_bytes_sent(byte_len as u64).await;
                            bp_tracker.relieve_pressure();
                        }
                        Err(_) => break,
                    }
                }
                Err(tokio::sync::broadcast::error::RecvError::Lagged(n)) => {
                    bp_tracker.report_lag(n);
                    debug!("[WS-FLV] Client lagged by {} packets, applying drop policy", n);
                    continue;
                }
                Err(_) => break,
            }
        }
    });

    tokio::select! {
        _ = recv_handle => {},
        _ = send_handle => {},
    }

    relay.remove_client().await;
    info!(stream = &stream_key_log[..8.min(stream_key_log.len())], "WS-FLV 客户端断开");

    Ok(())
}

/// WS 加密模式 - 发送加密协议帧 (原有行为)
async fn handle_ws_encrypted_mode(
    stream_key: String,
    mut ws_sender: futures_util::stream::SplitSink<
        tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
        tokio_tungstenite::tungstenite::Message,
    >,
    mut ws_receiver: futures_util::stream::SplitStream<
        tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
    >,
    state: &Arc<AppState>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let relay = get_or_create_relay(&stream_key, state).await?;

    if !relay.add_client().await {
        let _ = ws_sender.send(tokio_tungstenite::tungstenite::Message::Text(
            r#"{"error":"max clients reached"}"#.into(),
        )).await;
        return Ok(());
    }

    info!(stream = &stream_key[..8.min(stream_key.len())], "WebSocket 加密模式客户端连接");

    // 发送最新 m3u8
    if let Some(data) = relay.latest_m3u8().await {
        let frame = Frame::new(FrameType::Data, 0, data);
        let msg = tokio_tungstenite::tungstenite::Message::Binary(frame.encode().to_vec());
        let _ = ws_sender.send(msg).await;
    }

    // 订阅广播
    let mut rx = relay.subscribe();
    let relay_clone = relay.clone();
    let stream_key_log = stream_key.clone();

    let recv_handle = tokio::spawn(async move {
        while let Some(msg) = ws_receiver.next().await {
            match msg {
                Ok(tokio_tungstenite::tungstenite::Message::Ping(_)) => {}
                Ok(tokio_tungstenite::tungstenite::Message::Close(_)) => break,
                Err(_) => break,
                _ => {}
            }
        }
    });

    let send_handle = tokio::spawn(async move {
        while let Ok(data) = rx.recv().await {
            let msg = tokio_tungstenite::tungstenite::Message::Binary(data.to_vec());
            match ws_sender.send(msg).await {
                Ok(_) => {
                    relay_clone.record_bytes_sent(data.len() as u64).await;
                }
                Err(_) => break,
            }
        }
    });

    tokio::select! {
        _ = recv_handle => {},
        _ = send_handle => {},
    }

    relay.remove_client().await;
    info!(stream = &stream_key_log[..8.min(stream_key_log.len())], "WebSocket 加密模式客户端断开");

    Ok(())
}

/// HTTP-FLV 长连接处理
/// TCP 层直接处理流式输出, 绕过 hyper (避免 body 类型限制)
async fn handle_http_flv_connection(
    stream: tokio::net::TcpStream,
    state: Arc<AppState>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

    let mut reader = BufReader::new(stream);

    // 读取 HTTP 请求行
    let mut request_line = String::new();
    reader.read_line(&mut request_line).await?;

    // 从 URL 解析 stream key: GET /live/xxx.flv 或 GET /cascade/xxx.flv
    let stream_key = parse_stream_key_from_url(&request_line);

    // 检测是否为级联端点请求 (/cascade/ = Agent→Agent)
    let is_cascade_endpoint = request_line.contains("/cascade/");

    // 消费剩余 HTTP 头, 同时检测级联拉流标识
    let mut is_cascade_pull = is_cascade_endpoint;
    loop {
        let mut line = String::new();
        reader.read_line(&mut line).await?;
        if line == "\r\n" || line == "\n" { break; }
        // 检测其他 Agent 从本节点拉流 (级联)
        let lower = line.to_lowercase();
        if lower.starts_with("x-livecdn-agent:") {
            is_cascade_pull = true;
        }
    }

    // 级联端点权限检查: /cascade/ 只允许 Agent (带 X-LiveCDN-Agent 头)
    if is_cascade_endpoint && !is_cascade_pull {
        // 非 Agent 请求级联端点, 拒绝
        let mut stream = reader.into_inner();
        let resp = "HTTP/1.1 403 Forbidden\r\nContent-Length: 18\r\n\r\nAgent token required";
        stream.write_all(resp.as_bytes()).await?;
        return Ok(());
    }

    if stream_key.is_empty() {
        let mut stream = reader.into_inner();
        let resp = "HTTP/1.1 404 Not Found\r\nContent-Length: 9\r\n\r\nNot Found";
        stream.write_all(resp.as_bytes()).await?;
        return Ok(());
    }

    // 获取或创建 FLV 中继
    let relay = match get_or_create_flv_relay(&stream_key, &state).await {
        Ok(r) => r,
        Err(e) => {
            let mut stream = reader.into_inner();
            let body = format!("Service Unavailable: {}", e);
            let resp = format!(
                "HTTP/1.1 503 Service Unavailable\r\nContent-Length: {}\r\n\r\n{}",
                body.len(), body
            );
            stream.write_all(resp.as_bytes()).await?;
            return Ok(());
        }
    };

    if !relay.add_client().await {
        let mut stream = reader.into_inner();
        let resp = "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 15\r\n\r\nMax clients hit";
        stream.write_all(resp.as_bytes()).await?;
        return Ok(());
    }

    // 获取底层 TCP stream 用于写入
    let mut stream = reader.into_inner();
    let stream_key_log = stream_key.clone();

    // 写入 HTTP 响应头
    let cascade_header = if state.config.cascade_enabled || is_cascade_pull {
        "X-LiveCDN-Cascade: true\r\n"
    } else {
        ""
    };
    let header = format!(
        "HTTP/1.1 200 OK\r\n\
        Content-Type: video/x-flv\r\n\
        Cache-Control: no-cache, no-store\r\n\
        Access-Control-Allow-Origin: *\r\n\
        Access-Control-Allow-Headers: *\r\n\
        Access-Control-Expose-Headers: *\r\n\
        {}\
        \r\n",
        cascade_header
    );
    stream.write_all(header.as_bytes()).await?;

    if is_cascade_pull {
        info!(
            stream = &stream_key_log[..8.min(stream_key_log.len())],
            "[HTTP-FLV] 级联拉流连接 (Agent→Agent)"
        );
    }

    // 写入 FLV header
    let flv_header = flv_output::build_flv_header();
    stream.write_all(&flv_header).await?;

    // 发送 GOP 缓存的启动标签 (新客户端秒开首帧)
    // 包括: Metadata + AacSeqHeader + VideoSeqHeader(SPS+PPS) + 最近N个GOP
    let startup_tags = relay.get_startup_tags().await;
    if !startup_tags.is_empty() {
        let mut startup_buf = Vec::with_capacity(64 * 1024);
        for tag in &startup_tags {
            startup_buf.extend_from_slice(&flv_output::build_flv_tag_wire(tag));
        }
        if stream.write_all(&startup_buf).await.is_err() {
            relay.remove_client().await;
            return Ok(());
        }
        relay.record_bytes_sent(startup_buf.len() as u64).await;
        info!(
            stream = &stream_key_log[..8.min(stream_key_log.len())],
            tags = startup_tags.len(),
            "[HTTP-FLV] Sent GOP cache startup tags"
        );
    }

    // 订阅并持续推送 FLV 标签
    let mut rx = relay.subscribe();
    
    // 智能丢包跟踪器 (参考 livego: 背压时优先丢 P 帧, 保留关键帧/音频)
    let mut bp_tracker = BackpressureTracker::with_default_policy();

    loop {
        match rx.recv().await {
            Ok(tag_packet) => {
                // 智能丢包: 背压时优先丢 P 帧, 保留关键帧/音频
                if bp_tracker.should_drop(&tag_packet) {
                    continue;
                }
                
                let tag_bytes = flv_output::build_flv_tag_wire(&tag_packet);
                if stream.write_all(&tag_bytes).await.is_err() {
                    break; // 客户端断开
                }
                relay.record_bytes_sent(tag_bytes.len() as u64).await;
                bp_tracker.relieve_pressure();
            }
            Err(tokio::sync::broadcast::error::RecvError::Lagged(n)) => {
                // 客户端消费太慢, 更新背压计数
                bp_tracker.report_lag(n);
                debug!("[HTTP-FLV] Client lagged by {} packets, applying drop policy", n);
                continue;
            }
            Err(_) => break, // 通道关闭 (流结束)
        }
    }

    relay.remove_client().await;
    info!(stream = &stream_key_log[..8.min(stream_key_log.len())], "HTTP-FLV 客户端断开");

    Ok(())
}

/// 从 HTTP 请求 URL 解析 stream key
/// GET /live/xxx.flv HTTP/1.1 → xxx
/// GET /cascade/xxx.flv HTTP/1.1 → xxx (级联专用端点)
fn parse_stream_key_from_url(request_line: &str) -> String {
    let parts: Vec<&str> = request_line.split_whitespace().collect();
    if parts.len() < 2 { return String::new(); }

    let url = parts[1];
    // 去掉 .flv 后缀
    let path = url.trim_end_matches(".flv");
    // 去掉 /live/ 或 /cascade/ 前缀
    path.trim_start_matches("/live/")
        .trim_start_matches("/cascade/")
        .to_string()
}

/// 获取或创建 FLV 中继
/// P2P 优先: 优先从 CascadeSelector 选择的上游 Agent 拉流
/// 回退: 没有合适的 P2P 上游时直连 Origin
async fn get_or_create_flv_relay(
    stream_key: &str,
    state: &Arc<AppState>,
) -> Result<Arc<FlvPuller>, Box<dyn std::error::Error + Send + Sync>> {
    let mut relays = state.flv_relays.write().await;
    if let Some(relay) = relays.get(stream_key) {
        return Ok(relay.clone());
    }

    // P2P 优先: CascadeSelector 动态选择最优上游
    let cascade_url = state.cascade_selector.current_upstream_url().await;
    let flv_url = if let Some(ref upstream) = cascade_url {
        let url = upstream.replace("{stream_key}", stream_key);
        info!(
            stream = &stream_key[..8.min(stream_key.len())],
            upstream = &upstream[..20.min(upstream.len())],
            "[FLV] P2P级联回源 (Agent→Agent)"
        );
        url
    } else {
        // 回退到 Origin (类似 Tailscale DERP)
        state.config.origin_flv_url(stream_key)
    };

    let backup_url = state.config.backup_origin_url.as_ref()
        .map(|url| url.replace("{stream_key}", stream_key));
    let relay = FlvPuller::with_backup(
        stream_key.to_string(),
        flv_url,
        backup_url,
        state.config.max_clients,
    );
    relay.start().await;
    let relay = Arc::new(relay);
    relays.insert(stream_key.to_string(), relay.clone());

    Ok(relay)
}

/// 获取或创建流中继
async fn get_or_create_relay(
    stream_key: &str,
    state: &Arc<AppState>,
) -> Result<Arc<StreamRelay>, Box<dyn std::error::Error + Send + Sync>> {
    let mut relays = state.relays.write().await;
    if let Some(relay) = relays.get(stream_key) {
        return Ok(relay.clone());
    }

    // 创建新的中继
    // 实际场景: 从 controller 获取密钥信息
    // MVP: 自动生成密钥
    let key_info = KeyInfo::new(crate::crypto::CipherSuite::ChaCha20Poly1305)?;
    let origin_url = state.config.origin_hls_url(stream_key);

    let mut relay = StreamRelay::new(
        stream_key.to_string(),
        origin_url,
        key_info,
        state.config.max_clients,
        state.config.buffer_segments,
    );
    relay.start().await;
    let relay = Arc::new(relay);
    relays.insert(stream_key.to_string(), relay.clone());

    Ok(relay)
}

/// HTTP 请求处理
async fn handle_http_request(
    req: Request<hyper::body::Incoming>,
    state: Arc<AppState>,
) -> Result<Response<String>, hyper::Error> {
    let path = req.uri().path().to_string();
    let method = req.method().clone();

    // 1. 伪装网站
    if state.disguise.is_disguise_path(&path) {
        if let Some(resp) = state.disguise.serve(&path) {
            return Ok(resp);
        }
    }

    // 2. WebSocket 路径 (HTTP 层不处理，由 TCP 层接管)
    if path == state.config.ws_path && method == Method::GET {
        // 如果到这里说明不是 WebSocket 升级请求
        return Ok(Response::builder()
            .status(StatusCode::UPGRADE_REQUIRED)
            .header("Content-Type", "text/plain")
            .body("WebSocket upgrade required".into())
            .unwrap());
    }

    // 3. HLS 端点
    if path.starts_with("/live/") {
        return handle_hls(&path, &state).await;
    }

    // 4. 健康检查
    if path == "/health" {
        let relays = state.relays.read().await;
        let active_count = relays.len();
        let mut total_users = 0;
        for (_, r) in relays.iter() {
            total_users += r.client_count().await;
        }
        drop(relays);

        let flv_relays = state.flv_relays.read().await;
        let flv_count = flv_relays.len();
        for (_, r) in flv_relays.iter() {
            total_users += r.client_count().await;
        }
        drop(flv_relays);

        return Ok(Response::builder()
            .status(200)
            .header("Content-Type", "application/json")
            .body(serde_json::json!({
                "status": "ok",
                "node_id": state.config.node_id,
                "active_relays": active_count,
                "flv_relays": flv_count,
                "users": total_users,
                "ipv6": !state.config.public_ipv6.is_empty(),
                "cascade_enabled": state.config.cascade_enabled,
                "cascade_upstream": state.config.is_cascade_node(),
            }).to_string())
            .unwrap());
    }

    // 5. 404
    Ok(Response::builder()
        .status(StatusCode::NOT_FOUND)
        .header("Content-Type", "text/html")
        .body("<html><body><h1>404 Not Found</h1><p><a href=\"/\">Go Home</a></p></body></html>".into())
        .unwrap())
}

/// HLS 请求处理
async fn handle_hls(path: &str, state: &Arc<AppState>) -> Result<Response<String>, hyper::Error> {
    let parts: Vec<&str> = path.split('/').filter(|s| !s.is_empty()).collect();
    if parts.len() < 3 {
        return Ok(Response::builder()
            .status(StatusCode::NOT_FOUND)
            .body("Not Found".into())
            .unwrap());
    }

    let stream_key = parts[1];
    let filename = parts[2];

    let relays = state.relays.read().await;
    let relay = match relays.get(stream_key) {
        Some(r) => r,
        None => {
            return Ok(Response::builder()
                .status(StatusCode::NOT_FOUND)
                .body("Stream not found".into())
                .unwrap());
        }
    };

    if filename.ends_with(".m3u8") {
        match relay.latest_m3u8().await {
            Some(data) => {
                let b64 = base64_encode(&data);
                Ok(Response::builder()
                    .status(200)
                    .header("Content-Type", "application/vnd.apple.mpegurl")
                    .header("Cache-Control", "no-cache")
                    .header("Access-Control-Allow-Origin", "*")
                    .header("X-LiveCDN-Encrypted", "true")
                    .body(b64)
                    .unwrap())
            }
            None => Ok(Response::builder()
                .status(StatusCode::SERVICE_UNAVAILABLE)
                .body("No data yet".into())
                .unwrap()),
        }
    } else {
        Ok(Response::builder()
            .status(StatusCode::NOT_FOUND)
            .header("Content-Type", "text/plain")
            .body("Use WebSocket for stream data".into())
            .unwrap())
    }
}

/// 注册到调度中心
async fn register_with_controller(config: &Config) -> Result<(), String> {
    let client = reqwest::Client::new();
    let reg = RegisterPayload {
        node_id: config.node_id.clone(),
        public_ip: config.public_ip.clone(),
        public_ipv6: config.public_ipv6.clone(),
        port: config.port,
        region: config.region.clone(),
        isp: config.isp.clone(),
        bw_limit: config.bw_limit,
        domain: config.domain.clone(),
        protocol: config.protocol.clone(),
        ws_path: config.ws_path.clone(),
        tls_enabled: config.tls_enabled,
        token: config.reg_token.clone(),
        cascade_enabled: config.cascade_enabled,
        cascade_upstream: config.is_cascade_node(),
    };

    let url = format!("{}/api/agent/register", config.controller_url);
    let resp = client.post(&url)
        .json(&reg)
        .timeout(std::time::Duration::from_secs(10))
        .send()
        .await
        .map_err(|e| format!("注册请求失败: {}", e))?;

    if resp.status().is_success() {
        info!("已注册到调度中心");
        Ok(())
    } else {
        Err(format!("注册返回状态: {}", resp.status()))
    }
}

/// 心跳上报循环
async fn heartbeat_loop(state: Arc<AppState>) {
    let mut interval = tokio::time::interval(
        std::time::Duration::from_secs(state.config.hb_interval_secs)
    );

    loop {
        interval.tick().await;

        let relays = state.relays.read().await;
        let mut total_users = 0;
        let mut rtt = 0u32;
        for (_, relay) in relays.iter() {
            total_users += relay.client_count().await;
            if rtt == 0 {
                rtt = relay.measure_rtt().await;
            }
        }
        drop(relays);

        // FLV 中继也计入用户数和 RTT
        let flv_relays = state.flv_relays.read().await;
        for (_, relay) in flv_relays.iter() {
            total_users += relay.client_count().await;
            if rtt == 0 {
                rtt = relay.measure_rtt().await;
            }
        }
        drop(flv_relays);

        // 获取级联统计 (Controller 控制拓扑, Agent 本地择优)
        let cascade_stats = state.cascade_selector.cascade_stats().await;

        let hb = HeartbeatPayload {
            node_id: state.config.node_id.clone(),
            bw_used: 0,
            bw_limit: state.config.bw_limit,
            online_users: total_users,
            rtt_ms: rtt,
            loss_rate: 0.0,
            version: env!("CARGO_PKG_VERSION").to_string(),
            cascade_depth: cascade_stats.cascade_depth,
            children_count: 0,   // TODO: 从级联连接统计
            cascade_egress_bw: 0, // TODO: 从级联带宽统计
            current_upstream: cascade_stats.current_upstream,
            stream_lag_ms: 0,    // TODO: 从 FlvPuller 的 origin_ts 计算
        };

        let client = reqwest::Client::new();
        let url = format!("{}/api/agent/heartbeat", state.config.controller_url);

        match client.post(&url).json(&hb).timeout(std::time::Duration::from_secs(5)).send().await {
            Ok(resp) if resp.status().is_success() => {
                // 解析心跳响应中的 mesh_map (Controller 控制拓扑 + Agent 本地择优)
                if let Ok(body) = resp.text().await {
                    if let Ok(data) = serde_json::from_str::<serde_json::Value>(&body) {
                        if let Some(mesh_map_val) = data.get("mesh_map") {
                            if let Ok(mesh_resp) = serde_json::from_value::<MeshMapResponse>(mesh_map_val.clone()) {
                                state.cascade_selector.update_mesh_map(mesh_resp).await;
                            }
                        }
                    }
                }
                debug!("心跳上报成功");
            }
            Ok(resp) => {
                warn!("心跳返回异常: {}", resp.status());
            }
            Err(e) => {
                warn!("心跳上报失败: {}", e);
            }
        }
    }
}

fn base64_encode(data: &[u8]) -> String {
    use base64::engine::general_purpose::STANDARD;
    base64::Engine::encode(&STANDARD, data)
}
