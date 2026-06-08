package common

import "time"

// NodeStatus represents the current state of an edge node
type NodeStatus string

const (
	NodeStatusOnline  NodeStatus = "online"
	NodeStatusBusy    NodeStatus = "busy"
	NodeStatusOffline NodeStatus = "offline"
	NodeStatusCooling NodeStatus = "cooling"
	NodeStatusPending NodeStatus = "pending" // 新注册节点待审批
)

// LatencyMode 延迟档位 — 自适应降级，默认极速
//
// 设计原则：延迟越低越好，档位本质是自动降级策略而非用户选择。
// 系统始终从 ultra 开始，仅在客户端持续卡顿时自动降档。
// 管理员手动切换只是调试/特殊场景的 override。
type LatencyMode string

const (
	LatencyUltra     LatencyMode = "ultra"     // 极速: 800-1200ms (默认，始终先尝试)
	LatencyStandard  LatencyMode = "standard"  // 标准: 1500-2500ms (自动降级第一档)
	LatencyResilient LatencyMode = "resilient" // 抗弱网: 3000-5000ms (自动降级第二档)
)

// AutoDowngradeRules 自动降级规则
// 客户端持续卡顿 → Controller 自动切换到更高缓冲档位
var AutoDowngradeRules = []DowngradeRule{
	{From: LatencyUltra, To: LatencyStandard, StallThreshold: 0.05, DurationSec: 30},
	{From: LatencyStandard, To: LatencyResilient, StallThreshold: 0.05, DurationSec: 30},
}

// AutoUpgradeRules 自动恢复规则 (卡顿消失后逐步恢复到更低延迟)
var AutoUpgradeRules = []UpgradeRule{
	{From: LatencyResilient, To: LatencyStandard, StableSec: 120},
	{From: LatencyStandard, To: LatencyUltra, StableSec: 120},
}

// DowngradeRule 降级规则
type DowngradeRule struct {
	From           LatencyMode `json:"from"`
	To             LatencyMode `json:"to"`
	StallThreshold float64     `json:"stall_threshold"` // 卡顿率阈值 (5%)
	DurationSec    int         `json:"duration_sec"`    // 持续时间 (秒)
}

// UpgradeRule 恢复规则
type UpgradeRule struct {
	From      LatencyMode `json:"from"`
	To        LatencyMode `json:"to"`
	StableSec int         `json:"stable_sec"` // 无卡顿持续秒数
}

// LatencyModeConfig 各档位的缓冲参数
var LatencyModeConfigs = map[LatencyMode]LatencyConfig{
	LatencyUltra:     {AgentBufferMs: 200, ClientBufferMs: 300, PlaybackRateMin: 0.95, PlaybackRateMax: 1.05},
	LatencyStandard:  {AgentBufferMs: 800, ClientBufferMs: 1000, PlaybackRateMin: 0.95, PlaybackRateMax: 1.05},
	LatencyResilient: {AgentBufferMs: 2000, ClientBufferMs: 3000, PlaybackRateMin: 0.90, PlaybackRateMax: 1.10},
}

// LatencyConfig 延迟档位参数
type LatencyConfig struct {
	AgentBufferMs   int     `json:"agent_buffer_ms"`   // Agent Ring Buffer 窗口 (ms)
	ClientBufferMs  int     `json:"client_buffer_ms"`  // 客户端目标缓冲 (ms)
	PlaybackRateMin float64 `json:"playback_rate_min"` // 播放速率下限
	PlaybackRateMax float64 `json:"playback_rate_max"` // 播放速率上限
}

// NodeInfo holds all metadata about an edge node
type NodeInfo struct {
	NodeID      string     `json:"node_id"`
	PublicIP    string     `json:"public_ip"`
	PublicIPv6  string     `json:"public_ipv6,omitempty"` // IPv6 地址, 核心<->Agent 走 IPv6 降成本
	Port        int        `json:"port"`
	Region      string     `json:"region"`
	ISP         string     `json:"isp"`
	BWUsed      int64      `json:"bw_used"`  // bytes/sec current (total egress)
	BWLimit     int64      `json:"bw_limit"` // bytes/sec max
	OnlineUsers int        `json:"online_users"`
	RTT         int        `json:"rtt"`       // ms to origin
	LossRate    float64    `json:"loss_rate"` // 0.0-1.0
	Status      NodeStatus `json:"status"`
	LastHB      time.Time  `json:"last_heartbeat"`
	Domain      string     `json:"domain"`   //伪装域名
	Protocol    string     `json:"protocol"` // ws/grpc/reality
	WsPath      string     `json:"ws_path"`  // websocket path
	TLSEnabled  bool       `json:"tls_enabled"`
	Version     string     `json:"version"`
	// 级联回源: 允许其他 Agent 从本 Agent 拉流, 减少核心带宽
	CascadeEnabled  bool `json:"cascade_enabled"`  // 是否允许作为级联上游
	CascadeUpstream bool `json:"cascade_upstream"` // 是否为级联下游 (从其他Agent拉流)
	// Mesh Tier 分层 (防止环路, 只能从低tier拉流)
	CascadeTier     int    `json:"cascade_tier"`      // 0=直连Origin(Tier1), 1=从Tier1拉(Tier2), ...
	CascadeDepth    int    `json:"cascade_depth"`     // 当前级联深度 (心跳上报)
	ChildrenCount   int    `json:"children_count"`    // 当前下游Agent数 (心跳上报)
	CascadeEgressBW int64  `json:"cascade_egress_bw"` // 级联出带宽 bytes/sec (心跳上报)
	CurrentUpstream string `json:"current_upstream"`  // 当前上游NodeID (心跳上报)

	// --- 动态权重 (参考 Nginx 三权分离: weight/effective_weight/current_weight) ---
	// 调度器使用 effective_weight 做实际调度, 而非静态配置
	// 失败时衰减, 成功时逐步恢复, 实现慢启动和故障降权
	EffectiveWeight  float64   `json:"effective_weight"`  // 动态有效权重 (初始=1.0)
	ConsecutiveFails int       `json:"consecutive_fails"` // 连续失败次数 (异常检测)
	LastFailAt       time.Time `json:"last_fail_at"`      // 最近一次失败时间
}

// SessionInfo represents an active viewer session
type SessionInfo struct {
	SessionID  string    `json:"session_id"`
	StreamKey  string    `json:"stream_key"`
	NodeID     string    `json:"node_id"`
	ClientIP   string    `json:"client_ip"`
	StartTime  time.Time `json:"start_time"`
	LastActive time.Time `json:"last_active"`
}

// StreamInfo represents a live stream
type StreamInfo struct {
	StreamKey   string      `json:"stream_key"`
	Title       string      `json:"title"`
	OriginURL   string      `json:"origin_url"`   // HLS origin URL
	EncryptKey  string      `json:"encrypt_key"`  // base64 encoded
	EncryptIV   string      `json:"encrypt_iv"`   // base64 encoded
	CipherSuite string      `json:"cipher_suite"` // chacha20-poly1305 / aes-128-cbc
	IsLive      bool        `json:"is_live"`
	CreatedAt   time.Time   `json:"created_at"`
	LatencyMode LatencyMode `json:"latency_mode"` // 延迟档位
}

// DispatchRequest from player client
type DispatchRequest struct {
	StreamKey string `json:"stream_key" binding:"required"`
	Token     string `json:"token" binding:"required"`
	ClientIP  string `json:"client_ip"`
}

// DispatchResponse returned to player client
type DispatchResponse struct {
	Nodes       []NodeEndpoint `json:"nodes"`
	EncryptKey  string         `json:"encrypt_key"`
	CipherSuite string         `json:"cipher_suite"`
	SessionID   string         `json:"session_id"`
	LatencyMode LatencyMode    `json:"latency_mode"`
	LatencyConf LatencyConfig  `json:"latency_conf"`
}

// NodeEndpoint is a single node address returned to client
type NodeEndpoint struct {
	NodeID   string `json:"node_id"`
	URL      string `json:"url"`     // wss://domain/path or https://domain/path
	FLVURL   string `json:"flv_url"` // http://domain:port/live/{stream_key}.flv
	HLSURL   string `json:"hls_url"` // http://domain:port/live/{stream_key}/stream.m3u8
	Domain   string `json:"domain"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // ws / grpc / hls
	WsPath   string `json:"ws_path"`  // /ws/live
	Region   string `json:"region"`
	ISP      string `json:"isp"`
	Priority int    `json:"priority"` // 0=primary, 1,2=backup
	// 级联 & IPv6
	CascadeRelayURL string `json:"cascade_relay_url,omitempty"` // 级联拉流地址 (Agent从Agent拉流)
	IPv6FLVURL      string `json:"ipv6_flv_url,omitempty"`      // IPv6 FLV地址 (Agent间通信)
}

// HeartbeatRequest from agent to controller
type HeartbeatRequest struct {
	NodeID      string  `json:"node_id" binding:"required"`
	BWUsed      int64   `json:"bw_used"`
	BWLimit     int64   `json:"bw_limit"`
	OnlineUsers int     `json:"online_users"`
	RTT         int     `json:"rtt"`
	LossRate    float64 `json:"loss_rate"`
	Version     string  `json:"version"`
	// Mesh 级联心跳字段
	CascadeDepth    int    `json:"cascade_depth"`     // 当前级联深度
	ChildrenCount   int    `json:"children_count"`    // 下游Agent数
	CascadeEgressBW int64  `json:"cascade_egress_bw"` // 级联出带宽 bytes/sec
	CurrentUpstream string `json:"current_upstream"`  // 当前上游NodeID
	StreamLagMs     int    `json:"stream_lag_ms"`     // 当前流延迟(ms), 基于origin_timestamp
}

// QualityReport from player when node has issues
type QualityReport struct {
	SessionID  string  `json:"session_id"`
	NodeID     string  `json:"node_id"`
	StallRate  float64 `json:"stall_rate"` // 0.0-1.0
	RTT        int     `json:"rtt"`
	E2ELatency int     `json:"e2e_latency"` // 端到端延迟 (ms), 客户端计算
	Error      string  `json:"error,omitempty"`
}

// RegisterRequest from agent on startup
type RegisterRequest struct {
	NodeID     string `json:"node_id" binding:"required"`
	PublicIP   string `json:"public_ip" binding:"required"`
	PublicIPv6 string `json:"public_ipv6,omitempty"` // IPv6 地址
	Port       int    `json:"port" binding:"required"`
	Region     string `json:"region" binding:"required"`
	ISP        string `json:"isp" binding:"required"`
	BWLimit    int64  `json:"bw_limit"`
	Domain     string `json:"domain"`
	Protocol   string `json:"protocol"`
	WsPath     string `json:"ws_path"`
	TLSEnabled bool   `json:"tls_enabled"`
	Token      string `json:"token" binding:"required"` // registration token
	// 级联回源
	CascadeEnabled  bool `json:"cascade_enabled"`  // 允许其他Agent从本节点拉流
	CascadeUpstream bool `json:"cascade_upstream"` // 本节点是级联下游
}

// StreamStartRequest from broadcaster
type StreamStartRequest struct {
	Title       string      `json:"title" binding:"required"`
	AccessToken string      `json:"access_token" binding:"required"`
	LatencyMode LatencyMode `json:"latency_mode"` // 可选 override, 默认 ultra (自动降级)
}

// StreamStartResponse returned to broadcaster
type StreamStartResponse struct {
	StreamKey   string `json:"stream_key"`
	PushURL     string `json:"push_url"`     // rtmp://origin/live
	HLSUrl      string `json:"hls_url"`      // https://origin/live/stream.m3u8
	ViewerToken string `json:"viewer_token"` // 分享给观众的token
}
