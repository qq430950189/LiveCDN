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
#
#   如果自动公网 IP 检测失败或得到内网/回环地址，显式指定:
#     --public-ip=120.220.44.70
# ============================================================

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# --- 网络/地理信息辅助函数 ---
is_ipv4() {
  local ip="$1" part
  [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
  IFS='.' read -r -a parts <<< "$ip"
  for part in "${parts[@]}"; do
    [[ "$part" =~ ^[0-9]+$ ]] || return 1
    (( part >= 0 && part <= 255 )) || return 1
  done
}

is_public_ipv4() {
  local ip="$1" a b
  is_ipv4 "$ip" || return 1
  IFS='.' read -r a b _ <<< "$ip"
  case "$a" in
    0|10|127) return 1 ;;
    169) [[ "$b" == "254" ]] && return 1 ;;
    172) (( b >= 16 && b <= 31 )) && return 1 ;;
    192) [[ "$b" == "168" ]] && return 1 ;;
    224|225|226|227|228|229|230|231|232|233|234|235|236|237|238|239|240|241|242|243|244|245|246|247|248|249|250|251|252|253|254|255) return 1 ;;
  esac
  return 0
}

fetch_url() {
  local url="$1"
  if command -v curl &>/dev/null; then
    curl -fsSL --connect-timeout 4 --max-time 8 "$url" 2>/dev/null || true
  elif command -v wget &>/dev/null; then
    wget -qO- --timeout=8 "$url" 2>/dev/null || true
  fi
}

extract_first_public_ipv4() {
  awk '{
    while (match($0, /([0-9]{1,3}\.){3}[0-9]{1,3}/)) {
      ip = substr($0, RSTART, RLENGTH)
      split(ip, p, ".")
      if (p[1] <= 255 && p[2] <= 255 && p[3] <= 255 && p[4] <= 255 &&
          p[1] != 0 && p[1] != 10 && p[1] != 127 &&
          !(p[1] == 169 && p[2] == 254) &&
          !(p[1] == 172 && p[2] >= 16 && p[2] <= 31) &&
          !(p[1] == 192 && p[2] == 168) && p[1] < 224) {
        print ip
        exit
      }
      $0 = substr($0, RSTART + RLENGTH)
    }
  }'
}

china_region_zone() {
  local text="$1"
  case "$text" in
    *北京*|*天津*|*河北*|*山西*|*内蒙古*) echo "华北" ;;
    *上海*|*江苏*|*浙江*|*安徽*|*福建*|*江西*|*山东*|*台湾*) echo "华东" ;;
    *河南*|*湖北*|*湖南*) echo "华中" ;;
    *广东*|*广西*|*海南*|*香港*|*澳门*) echo "华南" ;;
    *重庆*|*四川*|*贵州*|*云南*|*西藏*) echo "西南" ;;
    *陕西*|*甘肃*|*青海*|*宁夏*|*新疆*) echo "西北" ;;
    *辽宁*|*吉林*|*黑龙江*) echo "东北" ;;
    *) echo "" ;;
  esac
}

normalize_china_location() {
  local text="$1"
  text="${text#中国}"
  text="${text//省/}"
  text="${text//市/}"
  text="${text//[[:space:]]/}"
  text="$(echo "$text" | xargs 2>/dev/null || echo "$text")"
  echo "$text"
}


# --- 参数解析 ---
TOKEN="" REGION="" ISP="" CONTROLLER_URL="" NODE_ID="" DOMAIN="" PORT="" BW_LIMIT="" PUBLIC_IP=""

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
    --public-ip=*)   PUBLIC_IP="${arg#*=}" ;;
  esac
done

[[ -z "$TOKEN" ]] && error "必须提供 --token 参数"
[[ -z "$CONTROLLER_URL" ]] && error "必须提供 --controller 参数"
CONTROLLER_URL="${CONTROLLER_URL%/}"

if [[ $(id -u) -eq 0 ]]; then
  SUDO=""
elif command -v sudo &>/dev/null; then
  SUDO="sudo"
else
  error "当前用户不是 root，且系统未安装 sudo；请用 root 执行安装脚本"
fi

# --- 系统检测 ---
info "检测系统环境..."
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) BINARY_ARCH="x86_64" ;;
  aarch64|arm64) BINARY_ARCH="aarch64" ;;
  armv7l)       error "当前发布脚本暂未生成 armv7 musl 二进制，请改用 x86_64/aarch64 节点或自行交叉编译" ;;
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

$SUDO mkdir -p "$INSTALL_DIR" "$CONFIG_DIR"

# --- 下载 Agent 二进制 ---
# musl 静态链接，不依赖任何系统库，直接下载即用。
# 注意：重装/升级时旧 agent 可能正在运行，不能直接 curl -o 覆盖正在执行的二进制，
# 否则即使 URL 可访问也可能因 Text file busy / 写入目标失败导致 curl 退出。
# 因此先下载到临时文件，验证后再原子替换目标路径。
BINARY_URL="${CONTROLLER_URL}/downloads/livecdn-agent-${BINARY_ARCH}-unknown-linux-musl"
BINARY_TMP="$INSTALL_DIR/.livecdn-agent-${BINARY_ARCH}.$$.$RANDOM.tmp"

cleanup_binary_tmp() {
  $SUDO rm -f "$BINARY_TMP" 2>/dev/null || true
}
trap cleanup_binary_tmp EXIT

stop_unmanaged_agent_processes() {
  command -v pgrep &>/dev/null || return 0

  local pids=""
  pids=$(pgrep -f "^${BINARY_PATH}( |$)" 2>/dev/null || true)
  [[ -z "$pids" ]] && return 0

  warn "发现未由当前 systemd 状态管理的旧 livecdn-agent 进程，准备停止: $pids"
  for pid in $pids; do
    [[ "$pid" == "$$" || "$pid" == "${BASHPID:-}" ]] && continue
    $SUDO kill "$pid" 2>/dev/null || true
  done

  for _ in {1..10}; do
    sleep 0.2
    pids=$(pgrep -f "^${BINARY_PATH}( |$)" 2>/dev/null || true)
    [[ -z "$pids" ]] && return 0
  done

  warn "旧 livecdn-agent 进程未正常退出，强制结束: $pids"
  for pid in $pids; do
    [[ "$pid" == "$$" || "$pid" == "${BASHPID:-}" ]] && continue
    $SUDO kill -9 "$pid" 2>/dev/null || true
  done
}

info "下载 Agent 二进制 (musl 静态链接, 零依赖)..."

DOWNLOAD_OK=0
DOWNLOAD_ERR=""
if command -v curl &>/dev/null; then
  if DOWNLOAD_ERR=$($SUDO curl -fsSL --connect-timeout 10 --max-time 120 -o "$BINARY_TMP" "$BINARY_URL" 2>&1); then
    DOWNLOAD_OK=1
  fi
elif command -v wget &>/dev/null; then
  if DOWNLOAD_ERR=$($SUDO wget --timeout=120 -O "$BINARY_TMP" "$BINARY_URL" 2>&1); then
    DOWNLOAD_OK=1
  fi
else
  DOWNLOAD_ERR="未找到 curl 或 wget"
fi

if [[ $DOWNLOAD_OK -eq 0 ]]; then
  error "下载失败: $BINARY_URL
  原因: ${DOWNLOAD_ERR:-未知错误}
  请确保:
  1. Controller 正在运行
  2. 二进制文件已发布到 /downloads/ 路径
  3. 网络连通
  4. 本机 $INSTALL_DIR 可写；如是重复安装，脚本会使用临时文件避免覆盖正在运行的二进制"
fi

$SUDO chmod 0755 "$BINARY_TMP"

# 快速验证可执行
if ! "$BINARY_TMP" --version &>/dev/null; then
  warn "下载的二进制无法执行，可能架构不匹配"
fi

$SUDO mv -f "$BINARY_TMP" "$BINARY_PATH"
trap - EXIT

# 验证二进制
BINARY_SIZE=$(du -h "$BINARY_PATH" | cut -f1)
info "Agent 二进制: $BINARY_SIZE (静态链接, 无需运行时)"

# --- 检测公网 IP 与地理信息 ---
info "检测公网 IP..."
CIP_OUTPUT=""
CIP_IP=""
CIP_ADDR=""
CIP_ISP=""

if [[ -n "$PUBLIC_IP" ]]; then
  if ! is_public_ipv4 "$PUBLIC_IP"; then
    error "--public-ip 必须是可公网访问的 IPv4，当前值: $PUBLIC_IP"
  fi
  info "使用手动指定公网IP: $PUBLIC_IP"
else
  # cip.cc 对中国线路可用性通常更好，优先用它同时拿 IP/地址/运营商。
  CIP_OUTPUT=$(fetch_url "http://cip.cc")
  if [[ -n "$CIP_OUTPUT" ]]; then
    CIP_IP=$(printf '%s\n' "$CIP_OUTPUT" | awk -F: '/^IP[[:space:]]*:/ {gsub(/[ \t]/,"",$2); print $2; exit}')
    CIP_ADDR=$(printf '%s\n' "$CIP_OUTPUT" | awk -F: '/^地址[[:space:]]*:/ {sub(/^[ \t]+/,"",$2); print $2; exit}')
    CIP_ISP=$(printf '%s\n' "$CIP_OUTPUT" | awk -F: '/^运营商[[:space:]]*:/ {sub(/^[ \t]+/,"",$2); print $2; exit}')
    if is_public_ipv4 "$CIP_IP"; then
      PUBLIC_IP="$CIP_IP"
    fi
  fi

  if [[ -z "$PUBLIC_IP" ]]; then
    for svc in "https://api.ipify.org" "https://ifconfig.me/ip" "https://ip.sb" "https://icanhazip.com"; do
      candidate=$(fetch_url "$svc" | extract_first_public_ipv4 || true)
      if is_public_ipv4 "$candidate"; then
        PUBLIC_IP="$candidate"
        break
      fi
    done
  fi
fi

if [[ -z "$PUBLIC_IP" ]]; then
  error "无法自动检测到公网 IPv4；请重新执行并显式传入 --public-ip=<公网IPv4>"
fi
info "公网IP: $PUBLIC_IP"

# --- 自动检测地区 ---
if [[ -z "$REGION" && -n "$CIP_ADDR" ]]; then
  normalized_addr=$(normalize_china_location "$CIP_ADDR")
  zone=$(china_region_zone "$CIP_ADDR")
  if [[ -n "$zone" && -n "$normalized_addr" ]]; then
    REGION="$zone-$normalized_addr"
  elif [[ -n "$normalized_addr" ]]; then
    REGION="$normalized_addr"
  fi
fi
if [[ -z "$ISP" && -n "$CIP_ISP" ]]; then
  ISP="$CIP_ISP"
fi

if [[ -z "$REGION" || -z "$ISP" ]]; then
  for api in "http://ip-api.com/json/$PUBLIC_IP?lang=zh-CN&fields=regionName,region,city,isp" \
             "https://ipapi.co/$PUBLIC_IP/json/"; do
    ip_info=$(fetch_url "$api")
    [[ -n "$ip_info" ]] && break
  done
  if [[ -n "${ip_info:-}" ]]; then
    if [[ -z "$REGION" ]]; then
      api_region=$(echo "$ip_info" | sed -n 's/.*"regionName"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
      [[ -z "$api_region" ]] && api_region=$(echo "$ip_info" | sed -n 's/.*"region"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
      api_city=$(echo "$ip_info" | sed -n 's/.*"city"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
      api_addr="$(normalize_china_location "$api_region$api_city")"
      api_zone=$(china_region_zone "$api_addr")
      [[ -n "$api_zone" && -n "$api_addr" ]] && REGION="$api_zone-$api_addr" || REGION="$api_addr"
    fi
    if [[ -z "$ISP" ]]; then
      ISP=$(echo "$ip_info" | sed -n 's/.*"isp"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
    fi
  fi
fi

[[ -z "$REGION" ]] && REGION="未知区域"
[[ -z "$ISP" ]] && ISP="未知运营商"
[[ -z "$PORT" ]] && PORT=9090
[[ -z "$BW_LIMIT" ]] && BW_LIMIT=10485760

info "地区: $REGION | 运营商: $ISP"

# --- 生成配置文件 ---
CONFIG_PATH="$CONFIG_DIR/agent.toml"
info "生成配置: $CONFIG_PATH"

$SUDO tee "$CONFIG_PATH" > /dev/null <<EOF
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

$SUDO tee /etc/systemd/system/livecdn-agent.service > /dev/null <<EOF
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

$SUDO systemctl daemon-reload
$SUDO systemctl enable livecdn-agent
if $SUDO systemctl is-active --quiet livecdn-agent; then
  info "检测到 livecdn-agent 已在运行，重启以应用新二进制和配置..."
  $SUDO systemctl restart livecdn-agent
else
  stop_unmanaged_agent_processes
  $SUDO systemctl start livecdn-agent
fi

# --- 等待启动 ---
info "等待启动..."
sleep 2

if $SUDO systemctl is-active --quiet livecdn-agent; then
  # 检查内存占用
  AGENT_PID=$($SUDO systemctl show livecdn-agent --property=MainPID --value 2>/dev/null || echo "0")
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
