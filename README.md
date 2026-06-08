# LiveCDN

LiveCDN 是一个面向直播分发的边缘 CDN 原型项目。它的目标是：用一台稳定 VPS 运行源站、调度中心和 Web 播放器，再让低成本/NAT 边缘机器以单二进制 Agent 的方式接入，由 Controller 根据节点健康、带宽、RTT、丢包和会话状态为观众调度可用边缘节点。

> 当前状态：项目已经具备核心构建、Docker 部署、裸机 Agent 安装、调度、心跳、质量反馈和基础测试链路，适合进入小规模实机冒烟与灰度验证；不建议跳过测试直接生产全量上线。

## 架构概览

```text
主播/推流端
  │ RTMP
  ▼
SRS Origin ──────────────┐
  │ HTTP-FLV/HLS          │
  ▼                       │
Controller                │
  │ 调度/鉴权/心跳/密钥     │
  ▼                       │
Web Player ───────► Edge Agent(s)
                    │ HTTP-FLV / WebSocket
                    ▼
                  观众
```

核心组件：

- **Controller（Go）**：节点注册、心跳、观众调度、直播流管理、质量上报、管理 API、Prometheus 指标、Agent 安装资源发布。
- **Origin / Origin Backup（SRS）**：RTMP 推流入口，提供 HTTP-FLV/HLS 源站能力。
- **Web（Nginx 静态页）**：播放器和管理页面。
- **Rust Agent**：推荐的边缘节点程序，裸机运行，负责伪装站、WebSocket/HTTP-FLV 拉流、级联回源、心跳、自更新等。
- **Go Agent**：早期兼容实现，保留用于开发和对照测试。

## 目录结构

```text
cmd/controller/          Go Controller 入口
cmd/agent/               Go Agent 入口
internal/controller/     Controller 核心逻辑、调度、存储、API
internal/agent/          Go Agent 逻辑
agent-rust/              Rust Agent
configs/                 示例配置
deploy/                  安装、卸载、构建、监控和 Ansible 部署脚本
docker/                  Controller / Agent Dockerfile
origin/                  SRS 配置
web/                     播放器和管理静态页面
binaries/                构建后放置 Agent 二进制的目录（本地生成，不提交）
```

## 环境要求

### 核心 VPS

- Docker + Docker Compose v2
- 开放端口：
  - `1935/tcp`：RTMP 推流
  - `8080/tcp`：Controller API、`/install.sh`、`/downloads/*`
  - `8088/tcp`：SRS HTTP-FLV/HLS
  - `3000/tcp`：Web 播放器
  - `1985/tcp`：SRS API（可按需要限制访问）

### 构建 Agent 的机器

- Rust toolchain
- `x86_64-unknown-linux-musl` target
- 如需 ARM：`aarch64-unknown-linux-musl` target

安装 target 示例：

```bash
rustup target add x86_64-unknown-linux-musl
rustup target add aarch64-unknown-linux-musl
```

## 快速开始

### 1. 修改配置

编辑 `configs/controller.yaml`，至少替换：

```yaml
reg_token: "请替换为强随机 Agent 注册令牌"
admin_token: "请替换为强随机管理员令牌"
```

实机部署时还需要确认：

```yaml
origin_addr: "http://origin:8080"
rtmp_origin_addr: "origin:1935"
binary_dir: "./binaries"
install_script_path: "./deploy/install.sh"
```

在 Docker Compose 中，Controller 容器会通过环境变量把二进制目录和安装脚本路径固定到：

```text
BINARY_DIR=/app/binaries
INSTALL_SCRIPT_PATH=/app/deploy/install.sh
```

### 2. 构建 Agent 二进制

```bash
./deploy/build-binaries.sh
```

脚本会输出到仓库根目录的 `binaries/`，例如：

```text
binaries/livecdn-agent-x86_64-unknown-linux-musl
binaries/livecdn-agent-aarch64-unknown-linux-musl
```

`docker-compose.yml` 已把本机 `./binaries` 挂载到 Controller 容器的 `/app/binaries`。因此 Controller 启动后会自动提供：

```text
http://<controller>:8080/downloads/livecdn-agent-x86_64-unknown-linux-musl
http://<controller>:8080/downloads/livecdn-agent-aarch64-unknown-linux-musl
```

### 3. 启动核心服务

```bash
mkdir -p binaries
docker compose up -d --build
```

检查服务：

```bash
curl -f http://127.0.0.1:8080/status
curl -f http://127.0.0.1:8080/install.sh
curl -I http://127.0.0.1:8080/downloads/livecdn-agent-x86_64-unknown-linux-musl
curl -f http://127.0.0.1:8088/api/v1/versions
```

如果 `/install.sh` 是 404，通常说明 Controller 镜像或 Compose 配置不是最新；如果 `/downloads/...` 是 404，通常说明 `binaries/` 目录中还没有对应架构的 Agent 二进制。

### 4. 安装边缘 Agent

在边缘节点执行：

```bash
curl -fsSL http://<Controller公网IP或域名>:8080/install.sh | bash -s -- \
  --token=<reg_token> \
  --controller=http://<Controller公网IP或域名>:8080 \
  --region=华东 \
  --isp=电信
```

如果自动检测公网 IP 失败，或检测结果是 `127.0.0.1` / 内网地址，请显式指定公网 IPv4：

```bash
curl -fsSL http://<Controller公网IP或域名>:8080/install.sh | bash -s -- \
  --token=<reg_token> \
  --controller=http://<Controller公网IP或域名>:8080 \
  --public-ip=<边缘节点公网IPv4> \
  --region=山东枣庄 \
  --isp=移动
```

安装脚本会：

1. 检测 CPU 架构。
2. 从 `${CONTROLLER_URL}/downloads/livecdn-agent-${ARCH}-unknown-linux-musl` 下载 Rust Agent。
3. 优先通过 `cip.cc` 检测公网 IP、地区和运营商；失败时尝试多个纯 IP 服务；仍失败时要求使用 `--public-ip` 手动指定。
4. 写入 `/etc/livecdn/agent.toml`。
5. 创建并启动 `livecdn-agent.service`。

常用管理命令：

```bash
systemctl status livecdn-agent
journalctl -u livecdn-agent -f
systemctl restart livecdn-agent
```

### 5. 打开管理后台

如果通过 `docker-compose.yml` 的 `web` 服务访问静态管理页，地址通常是：

```text
http://<Web主机>:3000/admin.html
```

静态页运行在 `:3000`，Controller API 运行在 `:8080`。管理页会在从非 Controller 端口打开时默认请求同主机 `:8080`，也可以在登录页手动填写 Controller API 地址，或通过 query 参数指定：

```text
http://<Web主机>:3000/admin.html?controller=http://<Controller主机>:8080
http://<Web主机>:3000/admin.html?api_port=9090
```

### 6. 开播和拉流冒烟

使用管理员 API 创建直播：

```bash
curl -s http://127.0.0.1:8080/api/broadcast/start \
  -H 'Content-Type: application/json' \
  -d '{"title":"smoke","access_token":"<admin_token>"}'
```

返回中会包含 `push_url`、`stream_key`、`viewer_token`。用 FFmpeg 推测试流：

```bash
ffmpeg -re -f lavfi \
  -i "testsrc2=size=1280x720:rate=30,drawtext=text='%{pts\\:hms}':fontcolor=white:fontsize=36" \
  -f lavfi -i "sine=frequency=440:beep_factor=4" \
  -c:v libx264 -preset ultrafast -tune zerolatency \
  -g 60 -keyint_min 60 \
  -c:a aac -ar 44100 -ac 2 \
  -f flv '<push_url>'
```

然后打开播放器：

```text
http://<Web主机>:3000/player.html
```

播放器同样会在从 `:3000` 打开时默认调用同主机 `:8080` 的 Controller API；自定义端口或跨主机部署时可使用：

```text
http://<Web主机>:3000/player.html?controller=http://<Controller主机>:8080
http://<Web主机>:3000/player.html?api_port=9090
```

## 重要路由

| 路由 | 说明 |
| --- | --- |
| `GET /status` | Controller 状态页 |
| `GET /metrics` | Prometheus 指标 |
| `GET /install.sh` | Agent 一键安装脚本 |
| `GET /downloads/livecdn-agent-*-unknown-linux-musl` | Agent 二进制下载 |
| `POST /api/agent/register` | Agent 注册 |
| `POST /api/agent/heartbeat` | Agent 心跳，需要注册 token |
| `GET /api/agent/streams/:key` | Agent 获取直播流元数据，需要注册 token |
| `POST /api/player/dispatch` | 观众调度 |
| `POST /api/player/report` | 播放质量上报 |
| `POST /api/player/session/end` | 播放会话结束 |
| `POST /api/broadcast/start` | 创建直播 |
| `POST /api/broadcast/stop` | 停止直播 |
| `GET /api/admin/*` | 管理 API，需要管理员 token |

## 常见问题排查

### `/install.sh` 返回 404

请确认：

1. 已使用最新代码重新构建 Controller 镜像：`docker compose up -d --build controller`。
2. `docker-compose.yml` 中存在 `./deploy/install.sh:/app/deploy/install.sh:ro` 挂载。
3. Controller 环境变量包含 `INSTALL_SCRIPT_PATH=/app/deploy/install.sh`。
4. 容器内文件存在：`docker compose exec controller ls -l /app/deploy/install.sh`。

### `/downloads/livecdn-agent-...` 返回 404

请确认：

1. 已执行 `./deploy/build-binaries.sh`。
2. 仓库根目录存在 `binaries/livecdn-agent-x86_64-unknown-linux-musl` 或对应 ARM 文件。
3. `docker-compose.yml` 中存在 `./binaries:/app/binaries:ro` 挂载。
4. Controller 环境变量包含 `BINARY_DIR=/app/binaries`。

### URL 能下载，但安装脚本仍提示下载失败

如果你手动访问 `/downloads/livecdn-agent-...` 可以下载，但重复执行安装脚本失败，常见原因是旧 `livecdn-agent` 正在运行，直接覆盖 `/opt/livecdn/livecdn-agent` 可能触发 `Text file busy` 或目标文件写入失败。当前脚本已改为先下载到 `/opt/livecdn/.livecdn-agent-*.tmp`，验证后再原子替换目标二进制；如果 systemd 服务已运行会自动 `restart`，如果发现遗留的非 systemd 旧进程也会先停止再启动服务。

如果仍失败，请查看错误中的 `原因:` 行，重点检查 `/opt/livecdn` 是否可写、磁盘空间是否足够，以及是否有安全策略阻止在该目录创建临时文件。

### 安装脚本检测到 `127.0.0.1` 或无法检测公网 IP

当前安装脚本不会再把 `127.0.0.1` 当作公网 IP 写入 Agent 配置。它会优先解析 `curl cip.cc` 的输出，并过滤回环、内网、链路本地和组播地址。如果自动检测失败，请使用：

```bash
--public-ip=<边缘节点公网IPv4>
```

同时建议显式传入 `--region` 和 `--isp`，这样 Controller 调度时能更准确地区分中国大区、省市和运营商线路。

### Docker Compose 提示 `version` obsolete

新版 Compose 不再需要顶层 `version` 字段，本项目已移除。

### Agent 注册后一直 pending

新节点默认可能处于待审核状态。可通过管理 API 或管理页面审批节点，或者根据测试需要调整节点状态。

### Controller 重启后 Agent 心跳 404/410

Rust Agent 和 Go Agent 会在心跳收到 `404` 或 `410` 时自动重新注册。若仍无法恢复，请检查注册 token、Controller URL、节点网络连通性和 Agent 日志。

## 开发与测试

Go 测试：

```bash
go test ./...
```

Rust 测试：

```bash
cd agent-rust
cargo test
cargo check
```

脚本语法检查：

```bash
bash -n deploy/install.sh
bash -n deploy/uninstall.sh
bash -n deploy/build-binaries.sh
```

Docker Compose 配置检查：

```bash
docker compose config
```

## 安全注意事项

- 生产和公网测试必须替换 `reg_token` 与 `admin_token`。
- 建议使用 HTTPS/WSS 终止代理保护 Controller、Web 和 Agent 入口。
- 管理 API 不建议直接暴露公网，可使用防火墙、VPN、反向代理鉴权或内网访问。
- Agent 下载目录只允许发布 `livecdn-agent-*-unknown-linux-musl` 命名的文件，避免任意文件下载。
- `binaries/` 为构建产物目录，建议纳入发布流程但不要提交大二进制到 Git。

## 当前建议的实机验证顺序

1. `docker compose up -d --build` 启动核心服务。
2. 验证 `/status`、`/install.sh`、`/downloads/...`。
3. 安装 1 台边缘 Agent。
4. 在管理 API 中确认节点注册和心跳。
5. FFmpeg 推一路测试流。
6. 通过 Web 播放器拉流。
7. 查看 `/metrics`、Agent 日志和 Controller 日志。
8. 再增加第二台 Agent，验证调度、负载和异常恢复。
