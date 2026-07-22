#!/bin/sh
set -e

echo "[entrypoint] IPTV UDProxy 启动中..."

# ---- 网络前置条件 ----
# 开启 IP 转发（docker 内部转发需要）
sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1 || true

# 注意：rp_filter 需要在宿主机上设置，容器内 /proc/sys 是只读的
# 请在宿主机上执行以下命令（将 enp2s0-ovs 替换为你的 IPTV 口）：
#   echo 2 > /proc/sys/net/ipv4/conf/enp2s0-ovs/rp_filter
#   echo 2 > /proc/sys/net/ipv4/conf/all/rp_filter
# 或者添加到 /etc/sysctl.conf：
#   net.ipv4.conf.enp2s0-ovs.rp_filter = 2
#   net.ipv4.conf.all.rp_filter = 2

# ---- 创建数据目录 ----
mkdir -p /data

# ---- 启动主程序 ----
exec /usr/local/bin/iptv-proxy "$@"
