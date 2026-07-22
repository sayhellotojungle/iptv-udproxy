FROM ubuntu:22.04

ENV TZ=Asia/Shanghai
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ppp \
        pppoe \
        iproute2 \
        iputils-ping \
        iptables \
        kmod \
        procps \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY iptv-proxy /usr/local/bin/iptv-proxy
COPY entrypoint.sh /entrypoint.sh
RUN sed -i 's/\r$//' /entrypoint.sh && chmod +x /entrypoint.sh

# 数据目录（规则文件、拨号配置）
VOLUME /data

# HTTP 监听端口
EXPOSE 18888

# PPPoE 需要 NET_ADMIN 权限（docker --cap-add NET_ADMIN --device /dev/net/tun）
ENTRYPOINT ["/entrypoint.sh"]
