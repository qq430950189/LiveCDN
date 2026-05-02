#!/bin/bash
# LiveCDN Agent 卸载脚本

set -euo pipefail

echo "正在卸载 LiveCDN Agent..."

# 停止服务
if systemctl is-active --quiet livecdn-agent 2>/dev/null; then
  sudo systemctl stop livecdn-agent
  echo "  服务已停止"
fi

# 禁用服务
if systemctl is-enabled --quiet livecdn-agent 2>/dev/null; then
  sudo systemctl disable livecdn-agent
  echo "  服务已禁用"
fi

# 删除 systemd 文件
sudo rm -f /etc/systemd/system/livecdn-agent.service
sudo systemctl daemon-reload
echo "  systemd 服务已删除"

# 删除二进制和配置
sudo rm -rf /opt/livecdn
sudo rm -rf /etc/livecdn
echo "  文件已删除"

echo ""
echo "LiveCDN Agent 已完全卸载"
