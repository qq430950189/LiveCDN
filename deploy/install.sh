#!/bin/bash
# ============================================================
# LiveCDN Agent 一键部署脚本 (裸机部署，无需 Docker)
# 
# 设计原则:
#   - 单二进制文件 (~3.5MB, musl 静态链接, 零系统依赖)
#   - 不安装 Docker / 不安装运行时 / 不编译
#   - 下载 → 配置 → systemd → 完事
#   - 适配 1 核 512M 的低配 NAT 挂机宝
#
# 用法:
#   curl -fsSL http://your-controller:8080/install.sh | bash -s -- \
#     --token=reg-token-change-me \
#     --controller=http://your-controller:8080 \
#     --region=华东 --isp=电信
# ============================================================

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# --- 参数解析 ---
TOKEN="" REGION="" ISP="" CONTROLLER_URL="" NODE_ID="" DOMAIN="" PORT="" BW_LIMIT=""

for arg in "$@"; do
  case $arg in
    --token=*)       TOKEN="${arg#*=}" ;;
    --region=*)      REGION="${arg#*=}" ;;
    --isp=*)         ISP="${arg#*=}" ;;
    --controller=*)  CONTROLLER_URL="${arg#*=}" ;;
    --node-id=*)     NODE_ID="${arg#*=}" ;;
    --domain=*)      DOMAIN="${arg#*=}" ;;
    --port=*)        PORT="${arg#*=}" ;;
    --bw-limit=*)    BW_LIMIT="${arg#*=}" ;;
  esac
done

[[ -z "$TOKEN" ]] && error "必须提供 --token 参数"
[[ -z "$CONTROLLER_URL" ]] && error "必须提供 --controller 参数"

# --- 系统检测 ---
info "检测系统环境..."
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) BINARY_ARCH="x86_64" ;;
  aarch64|arm64) BINARY_ARCH="aarch64" ;;
  armv7l)       BINARY_ARCH="armv7" ;;
  *)            error "不支持的架构: $ARCH" ;;
esac

# 检测内存
TOTAL_MEM=$(grep MemTotal /proc/meminfo 2>/dev/null | awk '{print $2}' || echo "0")
TOTAL_MEM_MB=$((TOTAL_MEM / 1024))
info "系统: $(uname -s) $ARCH | 内存: ${TOTAL_MEM_MB}MB"

if [[ $TOTAL_MEM_MB -lt 256 ]]; then
  warn "内存低于 256MB，可能影响性能"
fi

# --- 节点 ID ---
if [[ -z "$NODE_ID" ]]; then
  NODE_ID="edge-$(hostname | head -c 8)-$(head -c 4 /dev/urandom | xxd -p 2>/dev/null || echo $RANDOM)"
fi

# --- 安装目录 ---
INSTALL_DIR="/opt/livecdn"
CONFIG_DIR="/etc/livecdn"
BINARY_PATH="$INSTALL_DIR/livecdn-agent"

sudo mkdir -p "$INSTALL_DIR" "$CONFIG_DIR"

# --- 下载 Agent 二进制 ---
# musl 静态链接，不依赖任何系统库，直接下载即用
BINARY_URL="${CONTROLLER_URL}/downloads/livecdn-agent-${BINARY_ARCH}-unknown-linux-musl"

info "下载 Agent 二进制 (musl 静态链接, 零依赖)..."

DOWNLOAD_OK=0
if command -v curl &>/dev/null; then
  if sudo curl -fSL --connect-timeout 10 --max-time 120 -o "$BINARY_PATH" "$BINARY_URL" 2>/dev/null; then
    DOWNLOAD_OK=1
  fi
elif command -v wget &>/dev/null; then
  if sudo wget --timeout=120 -O "$BINARY_PATH" "$BINARY_URL" 2>/dev/null; then
    DOWNLOAD_OK=1
  fi
fi

if [[ $DOWNLOAD_OK -eq 0 ]]; then
  error "下载失败: $BINARY_URL
  请确保:
  1. Controller 正在运行
  2. 二进制文件已发布到 /downloads/ 路径
  3. 网络连通"
fi

sudo chmod +x "$BINARY_PATH"

# 验证二进制
BINARY_SIZE=$(du -h "$BINARY_PATH" | cut -f1)
info "Agent 二进制: $BINARY_SIZE (静态链接, 无需运行时)"

# 快速验证可执行
if ! "$BINARY_PATH" --version &>/dev/null; then
  warn "二进制无法执行，可能架构不匹配"
fi

# --- 检测公网 IP ---
info "检测公网 IP..."
PUBLIC_IP=""
for svc in ifconfig.me ip.sb icanhazip.com api.ipify.org; do
  PUBLIC_IP=$(curl -s4 --connect-timeout 3 "http://$svc" 2>/dev/null || true)
  [[ -n "$PUBLIC_IP" ]] && break
done
[[ -z "$PUBLIC_IP" ]] && PUBLIC_IP="127.0.0.1"
info "公网IP: $PUBLIC_IP"

# --- 自动检测地区 ---
if [[ -z "$REGION" ]]; then
  REGION="默认"
  for api in "http://ip-api.com/json/$PUBLIC_IP?lang=zh-CN&fields=region,isp" \
             "https://ipapi.co/$PUBLIC_IP/json/"; do
    ip_info=$(curl -s --connect-timeout 3 "$api" 2>/dev/null || true)
    [[ -n "$ip_info" ]] && break
  done
  if [[ -n "$ip_info" ]]; then
    REGION=$(echo "$ip_info" | grep -oP '"region"\s*:\s*"\K[^"]+' | head -1 || echo "")
    [[ -z "$ISP" ]] && ISP=$(echo "$ip_info" | grep -oP '"isp"\s*:\s*"\K[^"]+' | head -1 || echo "")
  fi
fi
[[ -z "$ISP" ]] && ISP="默认"
[[ -z "$PORT" ]] && PORT=9090
[[ -z "$BW_LIMIT" ]] && BW_LIMIT=10485760

info "地区: $REGION | 运营商: $ISP"

# --- 生成配置文件 ---
CONFIG_PATH="$CONFIG_DIR/agent.toml"
info "生成配置: $CONFIG_PATH"

cat > "$CONFIG_PATH" <<EOF
# LiveCDN Agent 配置 - 由 install.sh 自动生成
# 部署方式: 裸机运行 (无 Docker)
# 二进制: musl 静态链接，零系统依赖

node_id = "$NODE_ID"
public_ip = "$PUBLIC_IP"
port = $PORT
region = "$REGION"
isp = "$ISP"
bw_limit = $BW_LIMIT
domain = "${DOMAIN:-${NODE_ID}.livecdn.local}"
protocol = "ws"
ws_path = "/ws/live"
tls_enabled = false
controller_url = "$CONTROLLER_URL"
reg_token = "$TOKEN"
listen_addr = ":$PORT"
hb_interval_secs = 5
max_clients = 500
buffer_segments = 30
EOF

# --- systemd 服务 ---
info "创建 systemd 服务..."

sudo tee /etc/systemd/system/livecdn-agent.service > /dev/null <<EOF
[Unit]
Description=LiveCDN Edge Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$BINARY_PATH -c $CONFIG_PATH
Restart=always
RestartSec=3
LimitNOFILE=65536

# 资源限制 (保护低配机器)
MemoryMax=48M
CPUQuota=30%

# 安全
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=$INSTALL_DIR $CONFIG_DIR

# 日志
StandardOutput=journal
StandardError=journal
SyslogIdentifier=livecdn-agent

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable livecdn-agent
sudo systemctl start livecdn-agent

# --- 等待启动 ---
info "等待启动..."
sleep 2

if sudo systemctl is-active --quiet livecdn-agent; then
  # 检查内存占用
  AGENT_PID=$(systemctl show livecdn-agent --property=MainPID --value 2>/dev/null || echo "0")
  AGENT_RSS=""
  if [[ "$AGENT_PID" != "0" ]]; then
    AGENT_RSS=$(ps -o rss= -p "$AGENT_PID" 2>/dev/null | awk '{printf "%.1fMB", $1/1024}' || echo "unknown")
  fi

  echo ""
  echo -e "${GREEN}=====================================${NC}"
  echo -e "${GREEN}  Agent 启动成功!${NC}"
  echo -e "${GREEN}=====================================${NC}"
  echo ""
  echo "  节点ID:    $NODE_ID"
  echo "  地址:      $PUBLIC_IP:$PORT"
  echo "  地区:      $REGION ($ISP)"
  echo "  控制器:    $CONTROLLER_URL"
  echo "  二进制:    $BINARY_SIZE (静态链接)"
  echo "  内存占用:  ${AGENT_RSS:-unknown}"
  echo "  配置文件:  $CONFIG_PATH"
  echo ""
  echo "  管理命令:"
  echo "    状态:  systemctl status livecdn-agent"
  echo "    日志:  journalctl -u livecdn-agent -f"
  echo "    重启:  systemctl restart livecdn-agent"
  echo "    停止:  systemctl stop livecdn-agent"
  echo "    卸载:  systemctl stop livecdn-agent && systemctl disable livecdn-agent && rm -rf $INSTALL_DIR $CONFIG_DIR /etc/systemd/system/livecdn-agent.service"
  echo ""
  echo "  健康检查:  http://localhost:$PORT/health"
  echo "  伪装页面:  http://localhost:$PORT/"
else
  error "启动失败! 查看日志: journalctl -u livecdn-agent -n 50"
fi
