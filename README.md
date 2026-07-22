# IPTV UDProxy

> 🌐 English translation below is machine-generated. See [English Version](#english) for full English README.

基于 Go 的 IPTV 组播转单播服务，集成 PPPoE 拨号与动态源替换功能。可直接运行在飞牛OS 的 Docker 中。可以限制频道的观看时间，以及指定时间切换到其他源。防止小孩长时间霸占动画片以及限时观看，这是开发的主要目的。其次轻量化，用golang对内存需求小，环境依赖小，docker方便部署。不需要再开虚拟机来拨号以及代理。

A Go-based IPTV multicast-to-unicast service with integrated PPPoE dial-up and dynamic source switching. Runs directly in Docker on FnOS (飞牛OS). Its primary purpose is to limit children's TV watching time — restrict channel viewing duration and automatically switch to other sources at specified times to prevent kids from hogging cartoons all day. It is also lightweight: Go requires minimal memory and few dependencies, and Docker makes deployment effortless — no need to spin up a virtual machine just for dial-up and proxying.

## 功能特性

- **PPPoE 拨号**：在 IPTV 口上拨号，路由守卫确保系统默认路由始终走内网口
- **组播转单播**：监听 IPTV 口的组播流，通过 HTTP 提供单播访问
- **动态换源**：按时间窗自动替换组播源地址（如 19:00-19:50 将 A 频道替换为 B 频道），客户端无感知。
- **时长限制**：可根据日或者周时长进行限制，超出时长动态切换到其他源。
- **Web 控制台**：中文管理页面，查看服务状态、活跃流、管理换源规则
- **网页配置**：通过 Web 界面配置 PPPoE 账号密码、选择网络接口，无需修改配置文件

## Features

- **PPPoE Dial-up**: Dials on the IPTV port; a route guard ensures the system default route always goes through the LAN port.
- **Multicast to Unicast**: Listens to multicast streams on the IPTV port and serves them via HTTP unicast.
- **Dynamic Source Switching**: Automatically replaces multicast source addresses within time windows (e.g. 19:00–19:50 replaces Channel A with Channel B), transparent to clients.
- **Duration Limits**: Enforce daily or weekly viewing time limits; when exceeded, streams are dynamically switched to other sources.
- **Web Console**: Chinese-language management UI for viewing service status, active streams, and managing source-switching rules.
- **Web-based Configuration**: Configure PPPoE credentials and select network interfaces through the web UI — no need to edit config files.

## 项目架构

```
┌─────────────────────────────────────────────────────────────────┐
│                        客户端请求                               │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    IPTV UDProxy 服务                           │
├─────────────────────────────────────────────────────────────────┤
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐            │
│  │ HTTP 服务   │  │ 组播监听    │  │ PPPoE 管理  │            │
│  │ (端口18888) │  │ (IPTV 口)   │  │ (拨号认证)  │            │
│  └─────────────┘  └─────────────┘  └─────────────┘            │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐            │
│  │ 流转换      │  │ 路由守卫    │  │ 规则管理    │            │
│  │ (组播→单播) │  │ (默认路由)  │  │ (动态换源)  │            │
│  └─────────────┘  └─────────────┘  └─────────────┘            │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                        输出结果                                │
├─────────────────────────────────────────────────────────────────┤
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐            │
│  │ HTTP 单播流 │  │ Web 控制台  │  │ 数据持久化  │            │
│  │ (客户端访问)│  │ (管理界面)  │  │ (配置存储)  │            │
│  └─────────────┘  └─────────────┘  └─────────────┘            │
└─────────────────────────────────────────────────────────────────┘
```

## 项目结构

```
iptv_udproxy/
├── cmd/iptv-proxy/          # 主程序入口
│   └── main.go
├── internal/                # 内部包
│   ├── api/                 # HTTP API 接口
│   ├── channels/            # 频道管理
│   ├── config/              # 配置管理
│   ├── limits/              # 资源限制
│   ├── mcast/               # 组播处理
│   ├── pppoe/               # PPPoE 拨号
│   ├── relay/               # 流中继
│   ├── routeguard/          # 路由守卫
│   ├── rtp/                 # RTP 协议处理
│   ├── rules/               # 换源规则
│   ├── settings/            # 设置管理
│   └── stats/               # 统计信息
├── Dockerfile               # Docker 构建文件
├── docker-compose.yml       # Docker Compose 配置
├── entrypoint.sh            # 容器入口脚本
├── Makefile                 # 构建脚本
├── go.mod                   # Go 模块定义
└── go.sum                   # 依赖校验
```

## 请求格式

```
http://<飞牛内网IP>:18888/rtp/<组播IP>:<端口>
```

示例：
```
http://192.168.1.1:18888/rtp/2.9.1.3:1376
```

## Request Format

```
http://<LAN-IP>:18888/rtp/<multicast-ip>:<port>
```

Example:
```
http://192.168.1.1:18888/rtp/2.9.1.3:1376
```

## 快速开始

### 部署方式

#### 前提条件

1. 飞牛OS 已配置两个网口：
   - **内网口**：系统默认路由走此口
   - **IPTV 口**：用于 PPPoE 拨号和组播监听
2. Docker 已安装

#### docker-compose（推荐）

```bash
# 下载项目
cd iptv_udproxy
自行编译golang程序（考虑到docker构建编译可能拉依赖超时等原因，自行编译或者直接下载release中编译好的更快捷方便）

# 准备文件
将编译好或者下载的iptv-proxy连同entrypoint.sh, Dockerfile, docker-compose.yml一并上传至飞牛或者其他linux中的某个文件夹。
注意：Dockerfile文件中预置了时区命令，如果不是东八区请自行修改。

# 构建并启动
docker-compose up -d --build
飞牛中就是compose菜单中新增项目，自定义名字，选择这个文件夹，使用这个compose文件，确定，再构建。

# 查看日志
docker logs -f iptv-proxy
```

## Quick Start

### Deployment

#### Prerequisites

1. FnOS has two network interfaces configured:
   - **LAN port**: System default route goes through this port.
   - **IPTV port**: Used for PPPoE dial-up and multicast listening.
2. Docker is installed.

#### docker-compose (Recommended)

```bash
# Download the project
cd iptv_udproxy
Compile the Go program yourself. (Building inside Docker may time out when fetching dependencies, so compiling locally or downloading a pre-built binary from Releases is faster and more convenient.)

# Prepare files
Upload the compiled (or downloaded) iptv-proxy along with entrypoint.sh, Dockerfile, and docker-compose.yml to a folder on FnOS or any other Linux machine.
Note: The Dockerfile contains a preset timezone command. If you are not in UTC+8, please modify it accordingly.

# Build and start
docker-compose up -d --build
In FnOS, go to the Compose menu, create a new project, give it a name, select the folder, use this compose file, confirm, then build.

# View logs
docker logs -f iptv-proxy
```

### 首次配置

1. 访问 `http://<飞牛内网IP>:18888/` 打开控制台
2. 点击顶部导航栏的 **设置** 进入配置页面
3. 在设置页面中：
   - 选择 IPTV 口（组播监听网口）
   - 启用 PPPoE 并选择拨号网口
   - 填入 PPPoE 账号密码
   - 可选：启用按需拨号（无流量时自动断开）
4. 点击 **保存配置**

### Initial Configuration

1. Visit `http://<LAN-IP>:18888/` to open the console.
2. Click **Settings** in the top navigation bar to enter the configuration page.
3. On the settings page:
   - Select the IPTV port (multicast listening interface).
   - Enable PPPoE and select the dial-up interface.
   - Enter your PPPoE username and password.
   - Optional: Enable on-demand dial-up (auto-disconnect when idle).
4. Click **Save Configuration**.

## 配置说明

### 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `LISTEN` | `0.0.0.0:18888` | HTTP 监听地址 |
| `DATA_DIR` | `/data` | 数据目录（规则文件、拨号配置） |

### 数据持久化

数据目录 `/data` 包含以下文件：

- `rules.json` - 换源规则配置
- `settings.json` - 服务配置（PPPoE 账号、网口选择等）

### 网络要求

- 容器使用 `host` 网络模式，直接访问物理网口
- 需要 `NET_ADMIN` 和 `NET_RAW` 权限
- 需要挂载 `/dev/net/tun` 和 `/dev/ppp` 设备

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN` | `0.0.0.0:18888` | HTTP listen address |
| `DATA_DIR` | `/data` | Data directory (rule files, dial-up config) |

### Data Persistence

The `/data` directory contains:

- `rules.json` — Source-switching rule configuration.
- `settings.json` — Service configuration (PPPoE credentials, interface selection, etc.).

### Network Requirements

- The container uses `host` network mode to directly access physical interfaces.
- Requires `NET_ADMIN` and `NET_RAW` capabilities.
- Requires mounting `/dev/net/tun` and `/dev/ppp` devices.

## Web 控制台

### 主页 (`/`)

- 服务状态（PPPoE 拨号状态、在线时长）
- 当前活跃的转换流列表
- 换源规则管理（增删改、启用/禁用）
- PPPoE 重新拨号按钮（状态异常时显示）

### 设置页 (`/settings`)

- PPPoE 拨号配置（账号密码、网口选择）
- 按需拨号设置
- 网络接口选择
- 高级设置（路由守卫）

## Web Console

### Home Page (`/`)

- Service status (PPPoE dial-up status, uptime).
- Currently active stream list.
- Source-switching rule management (add/edit/delete, enable/disable).
- PPPoE re-dial button (shown when status is abnormal).

### Settings Page (`/settings`)

- PPPoE dial-up configuration (credentials, interface selection).
- On-demand dial-up settings.
- Network interface selection.
- Advanced settings (route guard).

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/rtp/<group>:<port>` | 组播流转单播（主要功能）|
| GET | `/api/status` | 服务状态 |
| GET | `/api/streams` | 活跃流列表 |
| GET | `/api/rules` | 规则列表 |
| POST | `/api/rules` | 新增规则 |
| PUT | `/api/rules/<id>` | 更新规则 |
| DELETE | `/api/rules/<id>` | 删除规则 |
| POST | `/api/pppoe/retry` | 重新拨号 |
| GET | `/api/settings` | 获取配置 |
| PUT | `/api/settings` | 更新配置 |
| GET | `/api/interfaces` | 获取网口列表 |

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/rtp/<group>:<port>` | Multicast to unicast (main function) |
| GET | `/api/status` | Service status |
| GET | `/api/streams` | Active stream list |
| GET | `/api/rules` | Rule list |
| POST | `/api/rules` | Add rule |
| PUT | `/api/rules/<id>` | Update rule |
| DELETE | `/api/rules/<id>` | Delete rule |
| POST | `/api/pppoe/retry` | Re-dial PPPoE |
| GET | `/api/settings` | Get configuration |
| PUT | `/api/settings` | Update configuration |
| GET | `/api/interfaces` | List network interfaces |

## 动态换源规则

规则格式：在指定时间窗内，将对 A 组播地址的请求自动替换为 B 组播地址。

```json
{
  "name": "晚间替换示例",
  "from": "239.69.1.100:10000",
  "to": "239.69.1.200:10000",
  "start": "19:00",
  "end": "19:50",
  "days": [],
  "enabled": true
}
```

- `days`：空数组表示每天生效；`[1,2,3,4,5]` 表示周一至周五
- 时间窗支持跨午夜（如 `"start": "23:00", "end": "01:00"`）
- 到达时间窗自动切换，离开时间窗自动恢复，客户端完全无感知

## Dynamic Source Switching Rules

Rule format: Within a specified time window, requests to multicast address A are automatically replaced with multicast address B.

```json
{
  "name": "Evening switch example",
  "from": "239.69.1.100:10000",
  "to": "239.69.1.200:10000",
  "start": "19:00",
  "end": "19:50",
  "days": [],
  "enabled": true
}
```

- `days`: Empty array means every day; `[1,2,3,4,5]` means Monday through Friday.
- Time windows support spanning midnight (e.g. `"start": "23:00", "end": "01:00"`).
- Switches automatically when entering the time window and reverts when leaving — completely transparent to clients.

## 路由策略说明

本程序的路由设计原则：

1. **PPPoE 不添加默认路由**：pppd 配置 `nodefaultroute`，从源头避免影响系统路由表
2. **路由守卫双重保障**：每 5 秒检查一次，确保默认路由始终走内网口
3. **PPPoE 仅用于 IPTV 认证**：拨号后 ppp 接口不承担任何系统流量
4. **rp_filter 需要在宿主机设置**：由于容器内 /proc/sys 只读，需要在宿主机上将 IPTV 口的 rp_filter 设为 2（好像不用设置，我忘记了。）
5. **按需拨号（可选）**：无活跃流时自动断开，有请求时自动拨号

## Routing Strategy

Design principles for routing in this program:

1. **PPPoE does not add a default route**: pppd is configured with `nodefaultroute`, preventing it from affecting the system routing table at the source.
2. **Dual route guard protection**: Checks every 5 seconds to ensure the default route always goes through the LAN port.
3. **PPPoE is only for IPTV authentication**: After dialing, the ppp interface carries no system traffic.
4. **rp_filter must be set on the host**: Since `/proc/sys` is read-only inside the container, you need to set the IPTV port's rp_filter to 2 (maybe or not) on the host machine.
5. **On-demand dial-up (optional)**: Auto-disconnects when no streams are active; auto-dials when a request arrives.

## 故障排查

```bash
# 查看容器日志
docker logs -f iptv-proxy

# 检查 ppp 接口
docker exec iptv-proxy ip addr show ppp60

# 检查路由表
docker exec iptv-proxy ip route show

# 手动测试组播流
docker exec iptv-proxy curl -s http://localhost:18888/api/status

# 检查规则
docker exec iptv-proxy curl -s http://localhost:18888/api/rules

# 手动重新拨号
curl -X POST http://localhost:18888/api/pppoe/retry

# 查看配置
curl http://localhost:18888/api/settings
```

## Troubleshooting

```bash
# View container logs
docker logs -f iptv-proxy

# Check ppp interface
docker exec iptv-proxy ip addr show ppp60

# Check routing table
docker exec iptv-proxy ip route show

# Manually test multicast stream
docker exec iptv-proxy curl -s http://localhost:18888/api/status

# Check rules
docker exec iptv-proxy curl -s http://localhost:18888/api/rules

# Manually re-dial
curl -X POST http://localhost:18888/api/pppoe/retry

# View configuration
curl http://localhost:18888/api/settings
```

## 编译

```bash
# 使用 Makefile（推荐）
make build

# 或者手动编译（Linux amd64）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o iptv-proxy ./cmd/iptv-proxy

# Docker 构建
make docker
```

## Build

```bash
# Using Makefile (recommended)
make build

# Or manually compile (Linux amd64)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o iptv-proxy ./cmd/iptv-proxy

# Docker build
make docker
```

## 许可证

本项目采用 MIT 许可证 - 查看 [LICENSE](LICENSE) 文件了解详情。

## License

This project is licensed under the MIT License — see the [LICENSE](LICENSE) file for details.

## 致谢

- 感谢小米AI，mimo-v2.5-pro，开发10分钟，完善调试两天。

## Acknowledgements

- Thanks to Xiaomi AI, mimo-v2.5-pro — 10 minutes to develop, two days to debug and polish.
