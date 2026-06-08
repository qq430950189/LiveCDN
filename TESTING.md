# LiveCDN 测试框架

> 版本: v1.0 | 更新: 2026-05-02
>
> 本文档为滚动测试计划，随测试结果持续更新。
> 每个测试项包含：目的、前置条件、执行步骤、通过标准、结果记录区。

---

## 一、测试环境

### 1.1 核心服务 (Docker, 一台 VPS)

```yaml
# docker-compose.yml
services:
  origin:       SRS:5    ports: 1935(RTMP) 8088(HTTP-FLV/HLS) 8890/udp(SRT)
  origin-backup: SRS:5   ports: 1936(RTMP) 8089(HTTP-FLV/HLS)
  controller:   Go       ports: 8080(API)
  web:          Nginx    ports: 3000(player.html)
```

### 1.2 边缘节点 (裸机/挂机宝)

边缘节点必须按真实部署方式以单二进制 + systemd 运行，不使用 Docker。

```bash
# 构建静态 Agent 二进制并发布到 ./binaries，供 Controller /downloads 下载
./deploy/build-binaries.sh

# 在边缘机器上部署
curl -fsSL http://CONTROLLER_IP:8080/install.sh | bash -s -- \
  --controller=http://CONTROLLER_IP:8080 \
  --token=reg-token-change-me \
  --public-ip=EDGE_PUBLIC_IP
```

### 1.3 测试工具

| 工具 | 用途 | 安装 |
|------|------|------|
| FFmpeg | 推流源 | `apt install ffmpeg` |
| ffplay | 验证拉流 | `apt install ffmpeg` |
| curl | API 测试 | 系统自带 |
| k6 | 压测 | `docker run grafana/k6` |
| toxiproxy | 混沌注入 | `docker run ghcr.io/shopify/toxiproxy` |
| ab/wrk | HTTP 压测 | `apt install apache2-utils` |

### 1.4 测试流地址

| 协议 | 地址 |
|------|------|
| RTMP 推流 | `rtmp://ORIGIN_IP:1935/live/test` |
| HTTP-FLV | `http://AGENT_IP:9090/live/test.flv` |
| WS-FLV | `ws://AGENT_IP:9090/ws/live` (mode=flv) |
| WS-encrypted | `ws://AGENT_IP:9090/ws/live` (mode=encrypted) |
| HLS | `http://ORIGIN_IP:8088/live/test.m3u8` |
| 调度 API | `http://CONTROLLER_IP:8080/api/player/dispatch` |

---

## 二、测试阶段总览

```
阶段 0: 环境搭建 ───────→ 阶段 1: 冒烟测试 ───────→ 阶段 2: 功能验证
   [ ] 核心 Docker 启动       [ ] 推流→拉流            [ ] 加密链路
   [ ] 裸机 Agent 注册        [ ] 调度 API             [ ] 主备切换
   [ ] 心跳正常               [ ] 播放器               [ ] 延迟档位

阶段 3: 性能基准 ───────→ 阶段 4: 容灾混沌 ───────→ 阶段 5: 长稳压测
   [ ] 延迟 P50/P95/P99      [ ] 节点宕机切换          [ ] 24h 不间断
   [ ] 资源占用               [ ] 网络异常              [ ] 内存泄漏
   [ ] 并发容量               [ ] 源站故障              [ ] 日志审计
```

---

## 阶段 0: 环境搭建

> 目标: 所有服务启动, Agent 成功注册到 Controller

### T0.1 核心服务启动

```bash
cd /path/to/live-cdn
docker compose up -d

# 验证 SRS
curl -s http://localhost:8088/api/v1/versions | head -1
# 预期: {"code":0,"server":"srs"...}

# 验证 Controller
curl -s http://localhost:8080/api/admin/nodes -H "Authorization: Bearer admin-token-change-me"
# 预期: {"nodes":[]}
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| SRS 1935 端口可达 | ☐ | |
| SRS 8088 端口可达 | ☐ | |
| Controller 8080 端口可达 | ☐ | |
| Web 3000 端口可达 | ☐ | |

### T0.2 Agent 编译部署

```bash
cd agent-rust
cross build --release --target x86_64-unknown-linux-musl
# 或: cargo build --release

# 拷贝到测试机并运行
./livecdn-agent configs/agent.toml
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| musl 编译成功 | ☐ | 二进制 ~3.5MB |
| Agent 启动无报错 | ☐ | |
| 日志出现 "已注册到调度中心" | ☐ | |

### T0.3 Agent 注册验证

```bash
curl -s http://CONTROLLER_IP:8080/api/admin/nodes \
  -H "Authorization: Bearer admin-token-change-me"
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 节点出现在列表中 | ☐ | |
| 节点状态为 online | ☐ | |
| region/isp 正确 | ☐ | |

### T0.4 心跳验证

```bash
# 等 10s, 观察心跳
curl -s http://CONTROLLER_IP:8080/api/admin/nodes \
  -H "Authorization: Bearer admin-token-change-me" | jq '.nodes[0].last_heartbeat'
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| last_heartbeat 持续更新 | ☐ | 每 5s 一次 |
| Agent /health 端点正常 | ☐ | `curl http://AGENT_IP:9090/health` |

**阶段 0 准出**: 4 个检查项全部通过

---

## 阶段 1: 冒烟测试

> 目标: 推流→拉流基本链路跑通

### T1.1 RTMP 推流到 SRS

```bash
ffmpeg -re -f lavfi \
  -i "testsrc2=size=1280x720:rate=30,drawtext=text='%{pts\:hms}':fontcolor=white:fontsize=36" \
  -f lavfi -i "sine=frequency=440:beep_factor=4" \
  -c:v libx264 -preset ultrafast -tune zerolatency \
  -g 60 -keyint_min 60 \
  -c:a aac -ar 44100 -ac 2 \
  -f flv rtmp://ORIGIN_IP:1935/live/test
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| FFmpeg 推流无报错 | ☐ | |
| SRS 日志出现 stream live/test | ☐ | |

### T1.2 直接从 SRS 拉流 (验证源站正常)

```bash
# HTTP-FLV
ffplay http://ORIGIN_IP:8088/live/test.flv

# HLS
ffplay http://ORIGIN_IP:8088/live/test.m3u8
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| HTTP-FLV 画面正常 | ☐ | |
| HLS 画面正常 | ☐ | 延迟较高, 正常 |
| 音频正常 | ☐ | |

### T1.3 调度 API 测试

```bash
curl -s -X POST http://CONTROLLER_IP:8080/api/player/dispatch \
  -H "Content-Type: application/json" \
  -d '{
    "stream_key": "test",
    "token": "test-viewer-001",
    "client_ip": "10.0.1.100"
  }'
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 返回 status 200 | ☐ | |
| 返回 nodes 数组非空 | ☐ | |
| 包含 session_id | ☐ | |
| 包含 flv_url / hls_url | ☐ | |
| latency_mode = "ultra" | ☐ | |

### T1.4 通过 Agent 拉流 (HTTP-FLV)

```bash
# 确保 Agent 已回源拉流
ffplay http://AGENT_IP:9090/live/test.flv
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| HTTP-FLV 画面正常 | ☐ | |
| Agent 日志出现 "[FLV] Connected" | ☐ | |
| Agent 日志出现 "HTTP-FLV 客户端连接" | ☐ | |
| 首帧时间 < 1s | ☐ | GOP 缓存生效 |

### T1.5 播放器页面测试

```
浏览器打开 http://CONTROLLER_IP:3000/player.html
输入 stream_key: test
点击 "Connect"
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| auto 模式自动选择协议 | ☐ | |
| HTTP-FLV 模式播放正常 | ☐ | |
| WS-FLV 模式播放正常 | ☐ | |
| Stats 面板显示 E2E 延迟 | ☐ | |
| Stats 面板显示节点信息 | ☐ | |

**阶段 1 准出**: T1.4 HTTP-FLV 拉流画面正常

---

## 阶段 2: 功能验证

> 目标: 每个功能模块独立验证

### T2.1 加密链路验证 (WS-encrypted)

```bash
# 播放器选择 "ws-encrypted" 模式
# 或手动 WS 测试
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| WS 握手成功 | ☐ | |
| 收到加密帧 | ☐ | 二进制数据 |
| 解密后画面正常 | ☐ | |
| 加密算法与配置一致 | ☐ | chacha20 / aes128 |

### T2.2 主备源切换

```bash
# 1. 正常推流到主源
# 2. 停止主源
docker stop livecdn-origin

# 3. 观察 Agent 是否自动切到备源
# 4. 恢复主源
docker start livecdn-origin
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 主源断开后 Agent 日志出现 "Switching to backup" | ☐ | |
| 备源接管后播放恢复 | ☐ | |
| 恢复主源后切回 | ☐ | |
| 切换期间客户端断流时长 | ☐ | ____ms |

### T2.3 延迟档位自动降级

```bash
# 1. ultra 模式播放正常
# 2. 用 toxiproxy 给 Agent 加 10% 丢包
# 3. 观察是否自动降级到 standard
# 4. 恢复网络, 观察是否自动升级回 ultra
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| ultra → standard 降级触发 | ☐ | 需要 stallRate > 5% 持续 30s |
| standard → ultra 升级触发 | ☐ | 需要无卡顿 120s |
| 播放器显示档位变化 | ☐ | |

### T2.4 节点准入机制

```bash
# 1. 新 Agent 注册
# 2. 检查节点状态应为 pending
# 3. 管理后台审批
# 4. 节点入调度池
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 新节点初始状态为 pending | ☐ | |
| pending 节点不被调度 | ☐ | |
| 审批后入调度池 | ☐ | |
| 管理后台可操作 | ☐ | |

### T2.5 域名切换池

```bash
# 添加域名
curl -X POST http://CONTROLLER_IP:8080/api/admin/domains \
  -H "Authorization: Bearer admin-token-change-me" \
  -d '{"domain":"cdn1.test.com"}'

# 切换域名
curl -X POST http://CONTROLLER_IP:8080/api/admin/domains/switch \
  -H "Authorization: Bearer admin-token-change-me" \
  -d '{"domain":"cdn1.test.com"}'
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 域名添加成功 | ☐ | |
| 域名列表正确 | ☐ | |
| 切换后新调度使用新域名 | ☐ | |

### T2.6 配置热更新

```bash
curl -X PUT http://CONTROLLER_IP:8080/api/admin/config \
  -H "Authorization: Bearer admin-token-change-me" \
  -d '{"bw_limit_threshold":0.9,"hb_interval":10}'
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 配置更新成功 | ☐ | |
| 新调度使用新参数 | ☐ | |
| Agent 不需要重启 | ☐ | |

### T2.7 bbolt 持久化

```bash
# 1. 确认有节点和流
# 2. 重启 Controller
docker restart livecdn-controller

# 3. 检查数据是否恢复
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 重启后节点信息保留 | ☐ | stale 节点标记 offline |
| 重启后流信息保留 | ☐ | |
| 30s 内 Agent 重连 | ☐ | |

### T2.8 健康自检

```bash
# 观察 Agent 日志, 每 60s 应有自检结果
# 或触发一个告警 (如磁盘满)
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 正常时日志 "All checks passed" | ☐ | |
| 磁盘 < 100MB 时报警 | ☐ | 需手动触发 |
| DNS 失败时报警 | ☐ | |

**阶段 2 准出**: T2.1 + T2.2 + T2.7 通过

---

## 阶段 3: 性能基准

> 目标: 量化核心指标, 验证设计承诺

### T3.1 端到端延迟测量

```bash
# 方法1: 播放器 stats 面板的 e2e_latency_ms
# 方法2: 手机秒表对拍 (推流端显示秒表, 播放端录屏计算差值)
# 方法3: ffmpeg 推流时间戳 vs 播放器 origin_ts

# 每种模式测 5 分钟, 取稳定值
```

| 模式 | P50 | P95 | P99 | 承诺值 | 通过? |
|------|-----|-----|-----|--------|-------|
| ultra (HTTP-FLV) | __ms | __ms | __ms | ≤2000ms | ☐ |
| standard (HLS) | __ms | __ms | __ms | ≤3000ms | ☐ |
| resilient (HLS) | __ms | __ms | __ms | ≤5000ms | ☐ |
| WS-FLV | __ms | __ms | __ms | ≤2000ms | ☐ |
| WS-encrypted | __ms | __ms | __ms | ≤2500ms | ☐ |

### T3.2 调度延迟

```bash
# k6 测调度 API
k6 run --vus 50 --duration 30s tests/e2e/k6/e2e.js
```

| 指标 | 值 | 承诺值 | 通过? |
|------|-----|--------|-------|
| P50 | __ms | < 200ms | ☐ |
| P95 | __ms | < 500ms | ☐ |
| P99 | __ms | < 1000ms | ☐ |

### T3.3 Agent 资源占用

```bash
# 启动 Agent, 0 客户端
top -p $(pgrep livecdn-agent) -b -n 1

# 逐步增加客户端到 50/100/200/500
# 每级稳定 2 分钟后测量
```

| 客户端数 | RSS (MB) | CPU % | 带宽 (Mbps) | 承诺值 | 通过? |
|----------|----------|-------|-------------|--------|-------|
| 0 | __ | __ | 0 | RSS<10MB | ☐ |
| 50 | __ | __ | __ | RSS<30MB, CPU<5% | ☐ |
| 100 | __ | __ | __ | RSS<50MB, CPU<10% | ☐ |
| 200 | __ | __ | __ | RSS<80MB, CPU<15% | ☐ |
| 500 | __ | __ | __ | CPU<30% | ☐ |

### T3.4 并发容量

```bash
# k6 压测
k6 run tests/e2e/k6/e2e.js
# 逐步提升到 500 VU
```

| 指标 | 值 | 阈值 | 通过? |
|------|-----|------|-------|
| 调度成功率 | __% | ≥99% | ☐ |
| 最大并发 VU | __ | ≥100 | ☐ |
| 调度超时率 | __% | <1% | ☐ |

### T3.5 GOP 缓存首帧时间

```bash
# 新客户端连接, 测量从请求到首帧显示的时间
# 方法: 浏览器 DevTools Network 面板, 看 .flv 请求的 TTFB + 首帧解码
```

| 场景 | 首帧时间 | 目标 | 通过? |
|------|----------|------|-------|
| 无 GOP 缓存 (等关键帧) | __ms | 参考 | ☐ |
| 有 GOP 缓存 | __ms | < 500ms | ☐ |
| 缓存命中率 | __% | > 90% | ☐ |

### T3.6 智能丢包效果

```bash
# 1. 给 Agent 注入 500ms 延迟 (toxiproxy)
# 2. 无智能丢包: 观察播放效果
# 3. 有智能丢包: 观察播放效果
# 对比: 是否出现黑屏, 花屏恢复时间
```

| 场景 | 黑屏 | 花屏 | 恢复时间 | 通过? |
|------|------|------|----------|-------|
| 无丢包策略 + 背压 | ☐ | ☐ | __s | — |
| 智能丢包 + 背压 | ☐ | ☐ | __s | 无黑屏 ☐ |

**阶段 3 准出**: T3.1 + T3.3 通过 (核心承诺达标)

---

## 阶段 4: 容灾与混沌

> 目标: 验证系统在异常下的行为

### T4.1 Agent 节点宕机

```bash
# 1. 3 个 Agent 正常服务
# 2. kill -9 Agent-1
# 3. 观察调度是否自动切到 Agent-2/3
# 4. 观察已有客户端是否断流
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| Controller 检测到节点离线 | ☐ | 心跳超时后 |
| 新调度分配到存活节点 | ☐ | |
| 已有客户端断流 | ☐/不可避免 | |
| 断流时长 | __s | |

### T4.2 源站故障

```bash
# 1. 正常播放中
# 2. docker stop livecdn-origin
# 3. 观察是否切到备源
# 4. docker start livecdn-origin
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 主源断开后 Agent 切备源 | ☐ | |
| 备源不可用时回退主源重试 | ☐ | |
| 恢复时间 | __s | |
| 客户端恢复播放 | ☐ | |

### T4.3 网络丢包 5%

```bash
# toxiproxy 注入 5% 丢包
curl -X POST http://TOXIPROXY_IP:8474/proxies \
  -d '{"name":"agent1","upstream":"agent-1:9090"}'
curl -X POST http://TOXIPROXY_IP:8474/proxies/agent1/toxics \
  -d '{"type":"loss","attributes":{"probability":0.05}}'
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 播放是否中断 | ☐ | |
| 丢包率是否上报到 Controller | ☐ | |
| 是否触发降级 | ☐ | 5% 丢包 → 可能不降级 |

### T4.4 网络延迟 500ms

```bash
curl -X POST http://TOXIPROXY_IP:8474/proxies/agent1/toxics \
  -d '{"type":"latency","attributes":{"latency":500}}'
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| E2E 延迟增加量 | __ms | ≈500ms + 原延迟 |
| 是否触发降级 | ☐ | |

### T4.5 Controller 重启

```bash
docker restart livecdn-controller
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| Agent 自动重连 | ☐ | |
| bbolt 数据恢复 | ☐ | |
| 恢复时间 | __s | |

### T4.6 会话粘性验证

```bash
# 同一 client_ip 连续请求 10 次调度
for i in $(seq 1 10); do
  curl -s -X POST http://CONTROLLER_IP:8080/api/player/dispatch \
    -H "Content-Type: application/json" \
    -d '{"stream_key":"test","token":"sticky-test","client_ip":"10.0.1.100"}' \
    | jq '.nodes[0].domain'
done
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 同 IP 10 次分配到同一节点 | ☐ | 命中率 ≥ 9/10 |
| 不同 IP 可能分配到不同节点 | ☐ | |

### T4.7 Agent OOM 场景

```bash
# 限制 Agent 内存 32MB (systemd MemoryMax=32M)
# 并发 200 客户端拉流
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 健康自检报警 | ☐ | memory_pressure = true |
| Agent 不崩溃 | ☐ | 或 OOM 被 systemd 重启 |
| 智能丢包生效 | ☐ | 背压下 P 帧被丢弃 |

**阶段 4 准出**: T4.1 + T4.2 通过 (核心容灾能力)

---

## 阶段 5: 长稳压测

> 目标: 验证长时间运行的稳定性

### T5.1 24 小时不间断

```bash
# 1. FFmpeg 持续推流
# 2. 50 VU 持续拉流
# 3. 每 10 分钟检查一次状态
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| 24h 无崩溃 | ☐ | |
| 内存无持续增长 | ☐ | RSS 波动 < ±20% |
| 调度成功率 > 99% | ☐ | |
| 平均 E2E 延迟波动 < ±30% | ☐ | |

### T5.2 内存泄漏检测

```bash
# 每 1 小时记录 Agent RSS
while true; do
  echo "$(date): $(ps -o rss= -p $(pgrep livecdn-agent)) KB"
  sleep 3600
done
```

| 时间 | RSS (MB) | 备注 |
|------|----------|------|
| 0h | __ | |
| 4h | __ | |
| 8h | __ | |
| 12h | __ | |
| 24h | __ | |
| 增长率 | __MB/h | < 1MB/h 合格 |

### T5.3 频繁上下线

```bash
# 每 5 分钟: 启动新 Agent → 注册 → 拉流 → 停止
# 循环 20 次
```

| 检查项 | 结果 | 备注 |
|--------|------|------|
| Controller 节点列表无残留 | ☐ | |
| bbolt 数据无膨胀 | ☐ | |
| 调度不受影响 | ☐ | |

**阶段 5 准出**: T5.1 通过 (24h 无崩溃 + 内存稳定)

---

## 三、测试结果汇总

> 完成测试后填写此表

| 阶段 | 状态 | 通过/总计 | 关键问题 |
|------|------|-----------|----------|
| 0 环境搭建 | ☐ | __/__ | |
| 1 冒烟测试 | ☐ | __/__ | |
| 2 功能验证 | ☐ | __/__ | |
| 3 性能基准 | ☐ | __/__ | |
| 4 容灾混沌 | ☐ | __/__ | |
| 5 长稳压测 | ☐ | __/__ | |

### 核心指标达标情况

| 指标 | 目标 | 实测 | 达标? |
|------|------|------|-------|
| E2E 延迟 (ultra) | ≤2s | __ms | ☐ |
| E2E 延迟 (standard) | ≤3s | __ms | ☐ |
| 调度成功率 | ≥99% | __% | ☐ |
| 调度 P95 延迟 | <500ms | __ms | ☐ |
| Agent RSS (idle) | <10MB | __MB | ☐ |
| Agent RSS (500 clients) | <80MB | __MB | ☐ |
| 首帧时间 (GOP cache) | <500ms | __ms | ☐ |
| 容灾切换时间 | <10s | __s | ☐ |

---

## 四、问题追踪

> 测试中发现的问题记录在此

| # | 阶段 | 问题描述 | 严重度 | 状态 | 修复 PR |
|---|------|----------|--------|------|---------|
| 1 | | | 🔴/🟡/🟢 | ☐ | |

---

## 五、滚动更新日志

| 日期 | 更新内容 |
|------|----------|
| 2026-05-02 | 初始版本, 框架搭建 |
| | |
