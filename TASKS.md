# LiveCDN 开发任务清单

> 原则：单直播间推流框架，高性能低占用，API/接口优先，前端简陋即可
>
> 技术选型：Rust(Agent) + Go(Controller) + C++/SRS(Origin)
>
> 部署架构：
> - **核心服务** (Docker, 跑在一台稳定 VPS 上): Controller + Origin + Web
> - **边缘节点** (裸机, NAT 挂机宝上直接跑): 单二进制 3.5MB, musl 静态链接, 零依赖
>
> 延迟目标：
> - **极速模式** (默认): 800-1200ms (SRT推流 + HTTP-FLV/WS分发 + 100ms缓冲)
> - **标准模式** (自动降级): 1500-2500ms (RTMP推流 + LL-HLS + 500ms缓冲)
> - **抗弱网模式** (自动降级): 3000-5000ms (RTMP + HLS 4s分片 + 2s缓冲)
> - **实际承诺值**: 稳定 ≤2s (极速模式), 500ms 为理论上限
> - **核心原则**: 延迟越低越好，档位本质是自动降级策略而非用户选择

---

## Phase 1: 基础架构 ✅

### 1.1 Rust Agent（边缘节点 - 裸机运行）
- [x] Cargo 项目 + 依赖 (tokio/hyper/tungstenite/ring/reqwest-rustls)
- [x] 二进制协议层 (Magic=0x4854 + 增量解码器)
- [x] 加密引擎 (ChaCha20-Poly1305 / AES-128-GCM + HKDF-SHA256 + 随机 nonce)
- [x] HLS 回源拉取器 (reqwest HTTP/2 + m3u8 解析 + TS 去重)
- [x] WebSocket 服务 (tokio-tungstenite + 握手认证)
- [x] 心跳上报 (RTT 探测 + 5s 间隔)
- [x] 配置管理 (TOML + 环境变量)
- [x] 伪装网站 (内嵌博客 HTML)
- [x] 优雅关闭 (信号处理 + 连接排空)
- [x] **musl 静态链接** (3.5MB, 零系统依赖)
- [x] **reqwest rustls** (不依赖系统 OpenSSL)

### 1.2 Go Controller（调度中心 - Docker 部署）
- [x] Store 层 (内存后端)
- [x] 调度算法 (阈值→同网→加权随机→1主2备)
- [x] 节点投诉冷却池
- [x] 流生命周期管理
- [x] 密钥下发 (HMAC-SHA256 签名)
- [x] Token 管理
- [x] 质量上报 → 自动隔离
- [x] 状态页面
- [x] zerolog 结构化日志
- [x] Agent 二进制下载接口
- [x] **Prometheus 指标导出** (/metrics, text format)
- [x] **SRS 推流鉴权钩子** (on_publish/on_unpublish)
- [ ] Redis 后端 (可选, 单直播间不需要)
- [ ] OpenAPI 文档

### 1.3 加密与安全
- [x] HKDF-SHA256 密钥派生
- [x] 每分片独立密钥 (seq_num 参与 HKDF info)
- [x] ChaCha20-Poly1305 / AES-128-GCM
- [x] HMAC-SHA256 令牌签名
- [x] TLS 1.3 (Caddy 前置)
- [x] **密钥轮换** (10min 自动换, KeyRotator)
- [x] **nonce 随机化** (ring SystemRandom, 嵌入密文头部)

---

## Phase 2: 核心功能 ✅

### 2.1 Agent 流中继
- [x] m3u8 解析 + TS 去重
- [x] Ring Buffer + broadcast 零拷贝
- [x] **带宽整形** (令牌桶限速 + ±15% 随机抖动)
- [x] **回源断线重连** (指数退避 2s→60s + ±25% 抖动)

### 2.2 调度与容灾
- [x] 调度算法
- [x] 节点 RTT 探测
- [x] 死点自动隔离
- [x] **IP 地理库** (内置 CIDR 映射, 可扩展 ip2region)
- [x] **会话粘性** (30min 同节点 + 到点强制轮换)

### 2.3 源站
- [x] SRS 配置 (HLS 2s 分片 + 6s 滑动窗口)
- [x] **推流鉴权** (SRS http_hooks → Controller on_publish)

---

## Phase 3: 运维与部署 ✅

### 3.1 核心服务 (Docker)
- [x] docker-compose.yml (origin + controller + web)
- [x] Go Controller Dockerfile
- [x] Agent 裸机部署链路 (不提供可用 Agent Docker 镜像，CI/发布使用 musl 二进制构建)

### 3.2 边缘节点 (裸机部署)
- [x] musl 静态二进制 (3.5MB, 零系统依赖)
- [x] install.sh 一键部署
- [x] systemd 服务 (MemoryMax=48M, CPUQuota=30%)
- [x] build-binaries.sh 多平台构建脚本
- [x] Caddy 反代配置
- [x] **卸载脚本** (uninstall.sh)
- [x] **Ansible playbook** (批量管理 + 模板配置)

### 3.3 监控
- [x] **Prometheus metrics** (Controller /metrics 端点)
- [x] **Grafana 仪表盘** (JSON 配置)
- [x] **告警规则** (节点离线/调度失败/带宽超限)

---

## Phase 4: 测试 ✅

### 4.1 单元测试
- [x] Rust crypto (ChaCha20/AES roundtrip, HKDF, TokenSigner, 随机nonce)
- [x] Rust 协议帧 (encode/decode/incremental)
- [x] Rust m3u8 解析
- [x] Rust 带宽整形 (令牌桶/退避)
- [x] Go Store (注册/心跳/健康/过期/流/令牌/会话/投诉冷却)
- [x] Go 调度 (基本/同区优先/无节点/过载过滤/丢包过滤/fallback/URL构建)

### 4.2 集成测试
- [ ] 端到端推流→拉流验证 (需要部署后实测)
- [ ] 节点故障自动切换测试
- [ ] 加密/解密全链路验证
- [ ] 多客户端并发压测

### 4.3 性能基准
- [ ] Agent 内存占用基准
- [ ] Agent CPU 占用基准
- [ ] Controller 调度延迟基准
- [ ] 端到端延迟基准

---

## Phase 5: Web UI 管理后台 ✅

### 5.1 后端 API 扩展
- [x] DELETE /api/admin/nodes/:id (移除节点)
- [x] PUT /api/admin/nodes/:id/status (修改节点状态)
- [x] GET /api/admin/sessions (会话列表)
- [x] GET /api/admin/config (系统配置查看)
- [x] POST /api/admin/stream/start (管理员开播)
- [x] POST /api/admin/stream/stop (管理员停播)
- [x] POST /api/admin/keyrotate (手动触发密钥轮换)
- [x] Store 层扩展 (RemoveNode/SetNodeStatus/GetAllSessions/GetAllStreams/SessionCount)

### 5.2 前端管理面板 (单文件 SPA)
- [x] 登录页面 (Admin Token 认证)
- [x] 仪表盘 (节点/流/观众/带宽概览, 最低延迟/最繁忙节点)
- [x] 节点管理 (列表/搜索/详情/状态切换/删除)
- [x] 流管理 (列表/开播/停播/详情/密钥轮换)
- [x] 会话查看 (活跃会话列表)
- [x] 系统配置 (只读展示网络/安全/调度参数)
- [x] 5s 自动刷新
- [x] 响应式布局 (移动端侧边栏折叠)
- [x] go:embed 嵌入 HTML (单二进制, 无额外静态文件)

---

## Phase 6: 架构升级 — 传输协议 + 调度 + 持久化 + 安全

> 目标：将端到端延迟从 6-15s 压缩到 1.2-2.5s (标准模式)
> 参考：业内 LL-HLS / HTTP-FLV 双轨方案、一致性哈希调度、bbolt 持久化

### 6.1 传输协议升级 — LL-HLS + HTTP-FLV 双轨 ✅
- [x] SRS 配置升级: LL-HLS (200ms part + fMP4 + CMAF) + SRT 推流入口 + HTTP-FLV 输出
- [x] Agent HTTP-FLV 回源: reqwest bytes_stream + FLV tag 解析 (relay/flv.rs)
- [x] 协议帧 Origin Timestamp 字段 (8B, 穿透到客户端用于实时延迟测量)
- [x] Agent 备源支持: Config.backup_origin_url, 主源挂了自动切
- [x] Agent HTTP-FLV 输出: /live/{stream_key}.flv 端点, TCP 层流式输出 (零拷贝)
- [x] Agent WS-FLV 模式: 握手 mode=flv, 发送原始 FLV 标签 (mpegts.js 兼容)
- [x] FLV 输出序列化: flv_output.rs (build_flv_header + build_flv_tag_wire + 测试)
- [x] GOP 缓存: 新客户端秒开首帧 (环形缓冲区 + Metadata/SPS+PPS/AacSeqHeader 独立缓存, 参考 lal)
- [x] 智能丢包: 背压时优先丢 P 帧, 保留关键帧/音频 (参考 livego DropPacket)
- [x] 客户端播放器: 集成 mpegts.js (HTTP-FLV) + hls.js (LL-HLS) + 自动协商降级
- [x] 播放器模式选择: auto/http-flv/ws-flv/hls/ws-encrypted 五种模式
- [x] 调度返回 FLV/HLS URL: NodeEndpoint 增加 flv_url/hls_url 字段
- [ ] Agent LL-HLS 输出端 (fMP4 partial segment, 兼容 iOS, 可选)

### 6.2 延迟档位管理 — 自动降级策略 ✅
- [x] StreamInfo 增加 LatencyMode 字段 (ultra/standard/resilient)
- [x] 默认 ultra (极速), 卡顿自动降档, 恢复后自动升档
- [x] LatencyController: 自动降级 (stallRate>5% 持续 30s → 降一档)
- [x] LatencyController: 自动恢复 (无卡顿 120s → 升一档)
- [x] AutoDowngradeRules / AutoUpgradeRules 规则配置
- [x] Controller 延迟档位 API: PUT /api/admin/stream/:id/mode (管理员 override)
- [x] 质量上报集成自动降级: handleQualityReport 触发 LatencyController
- [x] Agent 根据档位动态调整 Ring Buffer 窗口 (200ms/800ms/2000ms)
- [x] 客户端根据档位动态调整 target_buffer_ms (300/1000/3000ms)
- [x] 播放速率自适应 (playbackRate P控制器, 缓冲水位±100ms 收敛)
- [x] Web 管理后台: 档位切换 (含说明: 默认极速, 卡顿自动降档)

### 6.3 调度算法增强 — 一致性哈希 ✅
- [x] 集成 hashring (serialx/hashring)
- [x] ConsistentHashScheduler: clientIP 映射主节点, 顺时针取备节点
- [x] 扩缩容时仅重定向 1/N 观众
- [x] 保留加权随机作为 fallback
- [x] 虚拟节点均匀性测试: 50 虚拟节点/物理节点, CV < 30%

### 6.4 健康度综合打分 ✅
- [x] NodeHealthScore: 0.4×(1-bw) + 0.3×rtt + 0.2×(1-loss) + 0.1×stability
- [x] stability 参数由外部传入 (最近 1 小时无故障时长比例)

### 6.5 Controller 状态持久化 — bbolt ✅
- [x] bbolt 嵌入式 KV 存储 (单文件持久化, 零运维)
- [x] BoltStore 包装 MemoryStore: 读操作委托内存, 写操作同步持久化
- [x] 节点注册信息、流元数据、Token 映射写入 bbolt
- [x] Controller 重启秒恢复 (不再需要全部重连, stale 节点标记为 offline)
- [x] 30s 定时快照: StartSaveLoop 后台协程
- [ ] 可选: NATS JetStream (未来 Controller 集群场景)
- [ ] 预留 Redis 接口但暂不实现 (可选, 单直播间不需要)

### 6.6 安全增强 ✅
- [x] 节点准入机制: 新注册节点进入 pending 状态, 管理员 Web UI 审批后才入调度池
- [x] Agent 自更新: 启动时检查 Controller 版本号 → 自动下载 → 原子替换 → 重启
- [x] Agent 自更新: 回滚机制 (更新失败自动回退到上一版本)
- [x] obfstr 依赖已添加 (编译时字符串加密)
- [x] Controller 域名切换池 (多域名轮询 + 秒级切换, API: GET/POST/DELETE /api/admin/domains)

### 6.7 源站升级 ✅
- [x] SRS LL-HLS 配置 (200ms part + fMP4/CMAF)
- [x] SRT 推流入口 (端口 8890, latency=200ms)
- [x] HTTP-FLV 输出 (http_remux)
- [x] DVR 录制配置预留
- [x] SRS 主备配置: 主 Origin forward → 备 Origin (srs-backup.conf)
- [x] Agent 多源回源: FlvPuller with_backup, 主源失败自动切备源
- [ ] 可选: mediamtx 替代方案 (Go 单二进制, 全协议支持)

### 6.7 监控升级 — OpenTelemetry + 链路追踪
- [ ] Go Controller 集成 go.opentelemetry.io/otel, HTTP handler 自动埋 trace
- [ ] Rust Agent 集成 opentelemetry-rust + tracing crate
- [ ] TraceID 穿透: 观众请求调度时生成, Controller/Agent/客户端同一 traceID
- [ ] Grafana 延迟分布面板 (P50/P95/P99 端到端延迟)
- [ ] 三档延迟实测基准: 同一条流分别在三种模式下测延迟分布
- [ ] 跨运营商延迟矩阵: 电信/移动/联通互测

### 6.8 集成测试自动化
- [x] Docker Compose mini 全套环境 (1 Origin + 1 Controller + 3 Agent)
- [x] k6 压测脚本: 调度成功率 >99%、端到端延迟 <3s
- [x] toxiproxy Chaos 测试: 节点丢包/断网/慢响应模拟
- [ ] 会话粘性 + 自动切换在真实故障下的验证

### 6.9 伪装层可插拔传输 (中期)
- [x] Transport trait 抽象: 统一"流帧管道", 下层挂载多种 transport
- [x] PlainWsTransport 实现 (已有 WS 逻辑重构)
- [x] TransportRegistry 全局注册表 + TransportFactory 工厂模式 (参考 Xray-core)
- [x] PlainWS 完整 dial 实现 (WS 连接 + 帧收发)
- [x] 分层连接抽象: SecurityLayer (TLS/REALITY) + Transport (WS/H2/gRPC)
- [ ] RealityTransport 实现 (XTLS Reality, 参考 Xray-core)
- [ ] Controller 下发协议切换指令, Agent/客户端无感升级

### 6.10 工程化增强 — 稳定性 + 可演化
- [x] Agent 健康自检: 磁盘/时钟/DNS/内存/cert, 异常上报 Controller
- [x] 协议版本兼容: Magic 后 1B version, Controller 维护 N-2 兼容
- [x] 配置热更新: Controller 推送 config-update, Agent 原子替换 in-memory
- [x] 容量规划与主动限流: 全网带宽预算 + 单节点上限 + 排队池
- [x] GOP 缓存: 环形缓冲区 + 独立 Metadata/SPS+PPS/AacSeqHeader 缓存, 新客户端秒开首帧 (参考 lal)
- [x] 智能丢包: 背压时优先丢 P 帧, 保留关键帧/音频 (参考 livego DropPacket)
- [x] BackpressureTracker: 背压状态跟踪 + 三级丢包策略 (轻度/中度/重度)
- [x] 审计日志: 管理员操作/节点准入/密钥轮换/域管理/配置热更新 (bbolt bucket + HMAC-SHA256 签名防篡改)
- [x] IPv6 双栈监听: listen_ipv6 配置 + 第二 TcpListener + Agent 注册上报 public_ipv6
- [x] Agent 级联回源: cascade_upstream 配置 + RegisterPayload 扩展 + X-LiveCDN-Agent 级联标识 + HTTP-FLV 级联检测
- [x] Agent Mesh 组网 (P2P优先): CascadeSelector 参考 Tailscale betterAddr() + Controller 构建/分发 AgentMeshMap + 心跳携带 mesh_map + 动态路径选择(地域+延迟+负载) + 滞后性防抖动 + IPv6优先
- [x] Mesh V2: Controller 控制拓扑 + Agent 本地择优 (candidate_upstreams 替代全量 map)
- [x] Mesh Tier 分层: Tier0(Origin)/Tier1(优选CDN,直连Origin)/Tier2(边缘CDN,从Tier1拉流) + 禁止同层/反向拉流
- [x] Mesh 环路防护: LoopGuard 检测路径中是否包含自身 + Tier 规则天然防环路
- [x] Mesh 迟滞控制: switch_cooldown(30s) + min_score_improvement(15%) + max_switch_per_5min(3) + 硬故障快速切换
- [x] Mesh 级联端点: /cascade/{stream}.flv 独立于 /live/ + Agent token 鉴权 + X-LiveCDN-Agent 头检测
- [x] Mesh 心跳扩展: cascade_depth/children_count/cascade_egress_bw/current_upstream/stream_lag_ms
- [x] Mesh 评分V2: 6维评分 (locality 25% + isp 20% + capacity 15% + stability 15% + ipv6 10% + topology 15%) — 参考 Tailscale betterAddr 网络加分体系
- [x] Mesh 容量预算: cascade_egress_bw 30%限额 + max_children基于带宽计算 + bw_remaining_mbps
- [x] MeshPolicy: Controller 下发切换策略 (max_depth/cooldown/min_improvement/ping_interval/TTL/max_switch)
- [x] MigrationAdvice: Controller 迁移建议 (parent_overloaded/upstream_offline/topology_rebalance)
- [x] 调度器 P2C 选择: Power-of-Two-Choices 替代加权随机 (参考 Envoy Least Request + Nginx Random Two) — 兼顾负载均衡和随机性, 避免热点
- [x] 调度器异常检测: Envoy Outlier Detection 模式 — 连续5次失败→驱逐(30s×次数, 上限5min), 最大驱逐比例30%, 自动恢复
- [x] 调度器动态权重: Nginx 三权分离 — effective_weight 失败衰减(×0.5) + 成功慢恢复(+0.1/30s), 新节点慢启动(0.5→1.0)
- [x] Mesh 拓扑偏好: Tier1直连Origin加分(0.3) + 同区同ISP加分(0.5) + IPv6加分(0.1) — 参考 Tailscale betterAddr 网络加分
- [x] 边缘自治: Agent 本地 Mesh Ping 探测循环(10s) — 主动探测候选上游RTT, 不等 Controller 心跳
- [x] 边缘自治: Agent 上游故障检测 — FlvPuller 连续失败3次→判定硬故障→立即切换, 不受迟滞限制
- [x] 边缘自治: Agent 本地 RTT 修正评分 — Mesh Ping 结果影响候选评分(RTT<10ms +20%, >200ms -30%)
- [x] 中心编排+边缘自治: Controller 负责授权/约束/候选集, Agent 负责本地探测/优选/故障切换
- [ ] 开播体检: 主播到 Origin RTT/丢包检测 + 编码参数推荐
- [ ] 反向 SLA 文档: 承诺/不承诺/风险声明

---

## Phase 7: Agent Mesh 级联分发

### 7.1 MeshMap 控制面 ✅
- [x] Controller 维护 candidate_upstreams: 只下发当前 Agent 的候选上游 (不全量)
- [x] 心跳响应携带 mesh_epoch + candidate_upstreams + MeshPolicy + MigrationAdvice
- [x] candidate_upstreams 包含: node_id/url/region/isp/tier/score_hint/max_children/bw_remaining
- [x] Controller 为每个 Agent 推荐 3 个候选上游 (同区同ISP + 同区 + 兜底)
- [x] MeshPolicy 下发: max_depth/switch_cooldown/min_score_improvement 等

### 7.2 分层拓扑与环路防护 ✅
- [x] 节点 Tier 分层: Tier0(Origin)/Tier1(优选CDN,直连Origin)/Tier2(边缘CDN,从Tier1拉流)
- [x] max_depth 限制, 默认 2 (Origin→Tier1→Tier2)
- [x] 禁止同层或反向拉流 (tier < selfTier 规则)
- [x] LoopGuard 环路检测: 路径中包含自身 node_id 时断开并上报
- [x] /cascade/ 独立级联端点, Agent token 鉴权

### 7.3 CascadeSelector V2 ✅
- [x] 综合评分: locality/isp/latency/capacity/stability (5维, Controller hint 70% + Agent 本地 30%)
- [x] 迟滞切换: switch_cooldown + min_score_improvement + max_switch_per_5min
- [x] 硬故障快速切换: 上游下线时不受冷却限制
- [x] fallback 到 Origin (OriginFallback)

### 7.4 Cascade 端点与统计 ✅
- [x] 独立级联端点: /cascade/{stream}.flv (区别于用户 /live/)
- [x] Agent token 鉴权: X-LiveCDN-Agent 头检测
- [x] cascade_egress_bw 与 viewer_bw 分开统计 (心跳上报)
- [x] max_children / bw_remaining_mbps 限制 (Controller 计算)

### 7.5 热备与故障切换 (待实现)
- [ ] primary_upstream + standby_upstream 双连接
- [ ] 上游不可用时 standby 秒级切换 (<800ms)
- [ ] standby 定期 stream_probe, 不全量拉流
- [ ] 切换事件上报 Controller

### 7.6 拓扑再平衡 (待实现)
- [ ] Controller 周期性计算拓扑健康度
- [ ] 过载父节点自动卸载部分 children
- [ ] 小批量迁移, 单轮不超过 5%-10%
- [ ] MigrationAdvice 增加 not_before 时间窗口
- [ ] Agent 按 not_before 延迟执行迁移

### 7.7 增量 Map 与优化 (待实现)
- [ ] mesh_epoch delta map: 心跳只返回变化的候选 (MeshUpdate: seq + changed + removed + snapshot_hash)
- [ ] 全量兜底: GET /api/agent/mesh/full, Agent 发现 snapshot_hash 不一致时主动请求
- [ ] Agent 缓存 candidate_upstreams TTL 过期自动回退 Origin
- [ ] Mesh Ping 降维: Controller 汇总 RTT 矩阵 → Agent 粗筛 → 只对前3候选做实际 ping (O(n²)→O(n))
- [ ] Mesh Ping 三级探测: connect_rtt + stream_probe_ms(首帧耗时) + upstream_live_lag_ms(流延迟)
- [ ] 协议帧增加 hop_depth/path 字段 (环路检测)
- [ ] Controller 级联树预计算: 按 region+isp 分组 → 选骨干节点 → 分配上游 (类似带约束的最小生成树)
- [ ] 上游断开4级降级链: 重试原上游 → 备选1 → 备选2 → 直连Origin, Ring Buffer 兜底切换无感
- [ ] 集成测试: 3层拓扑 (Origin→优选CDN→边缘CDN→用户) 端到端验证
- [ ] 性能基准: 级联 vs 直连 Origin 延迟对比 + 级联带宽节省率
