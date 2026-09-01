#!/bin/sh
set -e

echo "[entrypoint] IPTV UDProxy 启动中..."

# ---- 网络前置条件 ----
# 说明：本服务【不需要】net.ipv4.ip_forward，故不再改写宿主机全局 sysctl。
# 组播包由内核投递给本进程（接收，非转发）；出去的 HTTP 单播由本进程新建、
# 走默认路由出网，全程不经过 IP 转发路径。host 网络下容器内的 sysctl -w
# 实际改的是宿主机内核参数，改全局转发开关属于不必要的副作用。

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
