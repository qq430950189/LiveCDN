#!/bin/bash
# ============================================================
# 构建 LiveCDN Agent 多平台二进制
# 
# 产出:
#   binaries/livecdn-agent-x86_64-unknown-linux-musl  (~3.5MB)
#   binaries/livecdn-agent-aarch64-unknown-linux-musl
#
# 所有二进制均为 musl 静态链接，零系统依赖
# 可直接 scp 到任意 Linux 机器运行
# ============================================================

set -euo pipefail

AGENT_DIR="$(cd "$(dirname "$0")/.." && pwd)/agent-rust"
OUTPUT_DIR="$(cd "$(dirname "$0")/.." && pwd)/binaries"

mkdir -p "$OUTPUT_DIR"

echo "=== 构建 LiveCDN Agent ==="
echo "源码: $AGENT_DIR"
echo "输出: $OUTPUT_DIR"
echo ""

# x86_64 (最常见的挂机宝架构)
echo "[1/2] 构建 x86_64-unknown-linux-musl ..."
cd "$AGENT_DIR"
cargo build --release --target x86_64-unknown-linux-musl
cp "target/x86_64-unknown-linux-musl/release/livecdn-agent" \
   "$OUTPUT_DIR/livecdn-agent-x86_64-unknown-linux-musl"
echo "  -> $(du -h "$OUTPUT_DIR/livecdn-agent-x86_64-unknown-linux-musl" | cut -f1)"

# aarch64 (ARM 服务器)
if rustup target list | grep -q "aarch64-unknown-linux-musl (installed)"; then
  echo "[2/2] 构建 aarch64-unknown-linux-musl ..."
  cargo build --release --target aarch64-unknown-linux-musl
  cp "target/aarch64-unknown-linux-musl/release/livecdn-agent" \
     "$OUTPUT_DIR/livecdn-agent-aarch64-unknown-linux-musl"
  echo "  -> $(du -h "$OUTPUT_DIR/livecdn-agent-aarch64-unknown-linux-musl" | cut -f1)"
else
  echo "[2/2] 跳过 aarch64 (未安装 target, 运行: rustup target add aarch64-unknown-linux-musl)"
fi

echo ""
echo "=== 构建完成 ==="
ls -lh "$OUTPUT_DIR/"
echo ""
echo "部署方式:"
echo "  1. docker-compose.yml 会把 ./binaries 挂载到 Controller 的 /app/binaries"
echo "  2. 确认可访问: http://controller:8080/downloads/livecdn-agent-x86_64-unknown-linux-musl"
echo "  3. 挂机宝上运行:"
echo "     curl -fsSL http://controller:8080/install.sh | bash -s -- \\"
echo "       --token=YOUR_TOKEN --controller=http://controller:8080"
echo ""
echo "  或直接 scp:"
echo "     scp binaries/livecdn-agent-x86_64-unknown-linux-musl root@挂机宝IP:/opt/livecdn/livecdn-agent"
