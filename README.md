# IPTV UDProxy

> 🌐 English translation below is machine-generated. See [English Version](#english) for full English README.

基于 Go 的 IPTV 组播转单播服务，集成 PPPoE 拨号与动态源替换功能。可直接运行在飞牛OS 的 Docker 中。可以限制频道的观看时间，以及指定时间切换到其他源。防止小孩长时间霸占动画片以及限时观看，这是开发的主要目的。其次轻量化，用golang对内存需求小，环境依赖小，docker方便部署。不需要再开虚拟机来拨号以及代理。

A Go-based IPTV multicast-to-unicast service with integrated PPPoE dial-up and dynamic source switching. Runs directly in Docker on FnOS (飞牛OS). Its primary purpose is to limit children's TV watching time — restrict channel viewing duration and automatically switch to other sources at specified times to prevent kids from hogging cartoons all day. It is also lightweight: Go requires minimal memory and few dependencies, and Docker makes deployment effortless — no need to spin up a virtual machine just for dial-up and proxying.

## 功能特性

- **PPPoE 拨号**：在 IPTV 口上拨号，路由守卫确保系统默认路由始终走内网口
- **组播转单播**：监听 IPTV 口的组播流，通过 HTTP 提供单播访问
- **动态换源**：按时间窗自动替换组播源地址（如 19:00-19:50 将 A 频道替换为 B 频道），客户端无感知。
- **时长限制**：可根据日、周或分星期（周一至周日，设置后覆盖每日限制）时长进行限制，超出时长动态切换到其他源。可选启用「连续限制」：设置最大连续观看时长与休息时长，连续观看超限后切换到备选池，间隔休息时长后才能调回原频道（两次观看间隔不超过休息时长视为连续）。
- **EPG 节目单**：定时拉取 XMLTV 节目单（1-24 小时周期，支持本地 XML 文件来源与自定义请求头），按频道时间轴展示；节目搜索；频道可手工绑定节目单 ID 纠正自动匹配；对外提供 `/epg.xml` 端点供播放器下载（可勾选导出哪些频道、导出窗口可配置），导出的 m3u 自动填充 `url-tvg` 头与真实 tvg-id。
- **预约录制（测试版）**：按天/每周/每月（含每月最后一天）定时录制组播流为 `.ts` 文件；文件命名模板可自定义（如 `{title} {Y-m-d} {H:i}`）；支持从节目单网格一键创建一次性录制、点击节目"录制系列"按播出规律自动生成重复计划（无需 AI）；支持手动立即录制/停止；预/后卷、最大并发、流中断看门狗、文件保留策略（保留天数/容量上限）、录制结束 Webhook 通知均可配置；录制目录可自定义（切换后新录制写入新目录，旧文件保留原处、历史仍可见可下载）；保留剩余空间阈值（默认 10 GB，磁盘剩余不足时自动停录并在页面提示）；录制文件支持网页直接下载。
- **AI 录制（测试版）**：指定节目名，由 AI（OpenAI 兼容接口，Key/提示词可自定义）根据节目单缓存推断播出规律，自动生成重复录制计划（自动跳过与已有计划重复的项）。
- **Web 控制台**：中文管理页面，查看服务状态、活跃流、管理换源规则、节目单、录制
- **网页配置**：通过 Web 界面配置 PPPoE 账号密码、选择网络接口，无需修改配置文件

## Features

- **PPPoE Dial-up**: Dials on the IPTV port; a route guard ensures the system default route always goes through the LAN port.
- **Multicast to Unicast**: Listens to multicast streams on the IPTV port and serves them via HTTP unicast.
- **Dynamic Source Switching**: Automatically replaces multicast source addresses within time windows (e.g. 19:00–19:50 replaces Channel A with Channel B), transparent to clients.
- **Duration Limits**: Enforce daily, weekly, or per-weekday (Mon–Sun, overrides the daily limit when set) viewing time limits; when exceeded, streams are dynamically switched to other sources. Optional continuous-viewing limit: set a max continuous viewing time plus a rest period — once exceeded the stream switches to the backup pool, and the channel can only be tuned back after the rest period has elapsed (views separated by no more than the rest period count as continuous).
- **EPG Guide**: Periodically fetches XMLTV EPG (1–24 h interval; local XML file sources and custom request headers supported), displays a per-channel timeline; program search; per-channel manual EPG-ID binding to correct auto-matching; exposes `/epg.xml` for players (selectable channel subset, configurable export window); generated m3u auto-fills the `url-tvg` header and real tvg-ids.
- **Scheduled Recording (Beta)**: Records multicast streams to `.ts` files daily / weekly / monthly (incl. last-day-of-month); customizable filename template (e.g. `{title} {Y-m-d} {H:i}`); one-click one-off recording from the EPG grid, "record series" auto-infers recurring plans from the broadcast pattern (no AI needed); manual record-now/stop; configurable pre-/post-roll, max concurrency, dead-stream watchdog, file retention (age/cap) and recording-finished Webhook; custom recording directory (after switching, new recordings go to the new directory while old files stay put — still visible in history and downloadable); minimum-free-space threshold (default 10 GB — recording stops automatically and a banner warns when free disk space falls below it); recorded files downloadable from the web UI.
- **AI Recording (Beta)**: Give a program name; an OpenAI-compatible LLM (custom endpoint/key/prompt) infers the broadcast pattern from the cached EPG and generates recurring recording plans (duplicates of existing plans are skipped automatically).
- **Web Console**: Chinese-language management UI for service status, active streams, source-switching rules, EPG and recordings.
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
│   ├── api/                 # HTTP API 接口（含内嵌 Web 控制台/节目单/录制页面）
│   ├── channels/            # 频道管理（m3u 导入导出、tvg-id）
│   ├── config/              # 配置管理
│   ├── epg/                 # EPG 节目单（XMLTV 解析、定时拉取、/epg.xml 导出）
│   ├── limits/              # 资源限制
│   ├── mcast/               # 组播处理
│   ├── pppoe/               # PPPoE 拨号
│   ├── record/              # 预约录制（计划/调度/落盘/AI 生成）
│   ├── relay/               # 流中继
│   ├── routeguard/          # 路由守卫
│   ├── rtp/                 # RTP 协议处理
│   ├── rules/               # 换源规则
│   ├── settings/            # 设置管理
│   ├── stats/               # 观看时长统计
│   └── storeutil/           # JSON 持久化工具（原子写/损坏备份）
├── Dockerfile               # Docker 构建文件
├── docker-compose.yml       # Docker Compose 配置
├── entrypoint.sh            # 容器入口脚本
├── Makefile                 # 构建脚本
├── go.mod                   # Go 模块定义
└── go.sum                   # 依赖校验
```

## 请求格式

```
http://<飞牛内网IP>:18888/rtp/<组播IP>:<端口>     # RTP 封装的组播流（自动剥离 RTP 头）
http://<飞牛内网IP>:18888/udp/<组播IP>:<端口>     # 裸 MPEG-TS 组播流
```

示例：
```
http://192.168.1.1:18888/rtp/2.9.1.3:1376
http://192.168.1.1:18888/udp/239.254.96.161:9040
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
| `LISTEN` | `:18888` | HTTP 监听地址（`docker-compose.yml` 中设为 `0.0.0.0:18888`） |
| `DATA_DIR` | `/data` | 数据目录（规则文件、时长统计等） |

### 数据持久化

数据目录 `/data` 包含以下文件（全部为原子写入；损坏时自动重命名为 `*.corrupt-<时间戳>` 备份并以空配置起步）：

- `rules.json` - 换源规则配置
- `settings.json` - 服务配置（PPPoE 账号、网口选择等，权限 0600）
- `channels.json` - 频道名称/分组/Logo/tvg-id/输入模式映射（m3u 导入）
- `watchtime.json` - 观看时长统计（内存聚合 + 30 秒定时落盘，自动保留 90 天）
- `limits.json` - 时长限制规则 + 全局备选地址池 + 连续观看状态
- `epg_config.json` - 节目单配置（来源 URL/本地文件、拉取周期、导出频道勾选、导出窗口、自定义请求头、频道手工绑定）
- `epg_cache.json` - 节目单缓存（时间窗修剪后的频道/节目）
- `recplans.json` - 录制计划
- `record_config.json` - 录制设置（录制目录、预/后卷、并发上限、流中断看门狗、保留策略、保留剩余空间阈值、Webhook）
- `ai.json` - AI 接口配置（endpoint/key/提示词，权限 0600）
- `recordings.json` - 录制历史（最近 500 条，含孤儿文件对账记录）
- `recordings/` - 录制文件目录（.ts）

### 网络要求

- 容器使用 `host` 网络模式，直接访问物理网口
- 需要 `NET_ADMIN` 和 `NET_RAW` 权限
- 需要挂载 `/dev/net/tun` 和 `/dev/ppp` 设备

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN` | `:18888` | HTTP listen address (`docker-compose.yml` sets it to `0.0.0.0:18888`) |
| `DATA_DIR` | `/data` | Data directory (rule files, watch-time stats, etc.) |

### Data Persistence

The `/data` directory contains (all writes are atomic; a corrupt file is renamed to `*.corrupt-<timestamp>` and the service starts with empty data):

- `rules.json` — Source-switching rule configuration.
- `settings.json` — Service configuration (PPPoE credentials, interface selection, etc.; 0600 permissions).
- `channels.json` — Channel name/group/logo/tvg-id/input-mode mapping (m3u import).
- `watchtime.json` — Watch-time statistics (in-memory aggregation, flushed every 30 s, retained 90 days).
- `limits.json` — Duration-limit rules + global backup address pool + continuous-viewing session state.
- `epg_config.json` — EPG configuration (source URL/local file, fetch interval, export channel selection, export window, custom headers, manual channel binding).
- `epg_cache.json` — EPG cache (time-window-pruned channels/programs).
- `recplans.json` — Recording plans.
- `record_config.json` — Recording settings (recording directory, pre/post-roll, max concurrency, dead-stream watchdog, retention policy, min-free-space threshold, Webhook).
- `ai.json` — AI interface configuration (endpoint/key/prompt; 0600 permissions).
- `recordings.json` — Recording history (latest 500 entries, including orphan-file reconciliation records).
- `recordings/` — Recorded files directory (.ts).

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

### 节目单页 (`/epg`)

- 节目单来源配置（URL 或 `/data` 下本地 XML 文件、1-24 小时拉取周期、未来保留天数、导出窗口天数、附加请求头）、立即刷新、状态展示
- 导出频道勾选（m3u 频道与节目单自动结合：手工绑定 > m3u 的 tvg-id > 按名称归一化匹配，如 CCTV1HD→CCTV1、CCTV5HD→CCTV5+），未匹配频道可手工绑定节目单 ID 纠正；写入 `/epg.xml`
- 节目搜索：按标题/副标题跨频道检索（过去 1 天 + 未来 7 天），结果可一键录制
- 时间轴视图：按频道展示某天节目（已过/正在播/待播状态区分、当前时间红线、正在播标记），已计划录制的节目带 📹 标记；每行可"录制/停止录制"手动录制当前直播；点击节目可一键创建一次性录制，或"录制系列"按节目单出现规律推断每天/每周计划（无需 AI）
- 客户端下载地址展示（`http://<host>:18888/epg.xml`）；"m3u 自动填充 url-tvg" 开启后，`/api/m3u` 头部自动携带 `#EXTM3U url-tvg="..." x-tvg-url="..."`，且 tvg-id 输出真实节目单频道 ID

### 录制页 (`/record`)

- 录制计划管理：一次性 / 每天 / 每周指定星期 / 每月指定日期（1-31，支持"每月最后一天"；当月无该日期自动跳过；时间窗支持跨午夜）。过期的一次性计划自动禁用。相同频道/周期/时间的计划会被拒绝（防双份录制）
- 手动录制：不建计划直接录制当前直播流，可随时停止（标题默认取 EPG 当前节目名）
- 文件命名模板：`{title}` `{channel}` `{Y-m-d}` `{H:i}` `{Y} {m} {d} {H} {i} {s}` `{unix}`，默认 `{title} {Y-m-d} {H-i}`，标题自动取 EPG 当前节目名（无 EPG 时取频道名），实时预览。输入封装（rtp/udp）以频道记录为准，计划未显式指定时自动回退，避免裸 TS 流被按 RTP 解包落成空文件
- 录制设置：预卷/后卷（提前/延后录制的分钟数）、最大并发录制数、流中断看门狗（N 分钟无数据判定断流并收尾重试）、文件保留策略（保留 N 天 / 总容量上限 GB，超限从最旧删起）、录制结束/出错 Webhook 通知、录制目录（自定义，切换后新录制写入新目录，旧文件保留原处、历史仍可见可下载）、保留剩余空间（GB，默认 10，0 = 关闭：磁盘剩余 ≤ 阈值即视为空间满，开录前拦截——预约计划下周期自动重试、手动录制直接报错，录制中跌破阈值即停录，页面顶部红色横幅提示）。实时显示录制目录占用与磁盘剩余
- AI 录制：填写 OpenAI 兼容接口 + Key + 模型 + 程序名，由 AI 依据节目单缓存推断播出规律生成重复计划（提示词可自定义，默认内置一套要求严格 JSON 输出的提示词），结果展示推断依据并支持查看原始返回。Key 可随时清空。与已有计划重复的项自动跳过
- 正在录制（实时字节数、手动停止）与录制历史（最近 500 条，可下载/删除文件，自动刷新）。进程异常退出遗留的 `.ts` 启动时自动对账为"孤儿文件"历史条目。录制中若连续输入包无效（封装与流不匹配）或流中断达到看门狗阈值会自动收尾并在下个周期重试，不再静默落 0 字节文件

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

### EPG Page (`/epg`)

- EPG source configuration (URL or a local XML file under `/data`, 1–24 h fetch interval, keep-days, export window in days, extra request headers), manual refresh, fetch status.
- Export channel selection (m3u channels auto-joined with the EPG: manual binding > explicit m3u tvg-id > normalized-name matching, e.g. CCTV1HD→CCTV1); unmatched channels can be manually bound to an EPG ID to correct auto-matching; written to `/epg.xml`.
- Program search: cross-channel fuzzy search by title/sub-title (past 1 day + next 7 days); results can be recorded with one click.
- Timeline view per channel for a day (past/live/upcoming styling, red "now" line, now-playing marker); already-planned programs carry a 📹 mark; each row has a record/stop button for manual live recording; click a program to create a one-off recording, or use "record series" to infer a daily/weekly plan from the broadcast pattern (no AI needed).
- Client download URL display (`http://<host>:18888/epg.xml`); with "auto-fill url-tvg in m3u" enabled, `/api/m3u` header carries `#EXTM3U url-tvg="..." x-tvg-url="..."` and tvg-id outputs the real EPG channel IDs.

### Recording Page (`/record`)

- Recording plans: one-off / daily / weekly (specific weekdays) / monthly (specific days 1–31, incl. "last day of month"; months lacking that day are skipped; windows may cross midnight). Expired one-off plans are auto-disabled. Identical channel/schedule/time plans are rejected (prevents double recording).
- Manual recording: record the current live stream without creating a plan; stop it at any time (title defaults to the EPG program currently airing).
- Filename templates: `{title}` `{channel}` `{Y-m-d}` `{H:i}` `{Y} {m} {d} {H} {i} {s}` `{unix}`; default `{title} {Y-m-d} {H-i}`; title auto-resolved from the EPG program at start (channel name fallback), live preview. Input encapsulation (rtp/udp) follows the channel record; when a plan does not set it explicitly it falls back to the channel's mode, so a raw-TS stream is not mis-depacketized as RTP into an empty file.
- Recording settings: pre/post-roll (minutes to start earlier / stop later), max concurrent recordings, dead-stream watchdog (finalize & retry after N minutes with no data), file retention policy (keep N days / total size cap in GB, evicting oldest first), a Webhook for recording-finished/failed events, custom recording directory (after switching, new recordings go to the new directory while old files stay put — still visible in history and downloadable), and a minimum-free-space threshold in GB (default 10, 0 = off: when free disk space falls to or below the threshold the disk counts as full — new recordings are blocked before start, scheduled plans retry on the next cycle and manual recording errors out, in-progress recordings are stopped, and a red banner warns at the top of the page). Live display of recording-directory usage and free disk space.
- AI recording: OpenAI-compatible endpoint + key + model + program name; the LLM infers the broadcast pattern from the cached EPG and generates recurring plans (customizable prompt, strict-JSON output by default); results show the reasoning and the raw response. The API key can be cleared at any time. Items duplicating existing plans are skipped automatically.
- Active recordings (live byte count, manual stop) and history (latest 500, downloadable/deletable, auto-refreshing). `.ts` files left behind by an abnormal process exit are reconciled into "orphan file" history entries on startup. If the input stays invalid for a consecutive run (encapsulation/stream mismatch) or the stream goes dead past the watchdog threshold, the session is finalized and retried on the next cycle instead of silently writing a 0-byte file.

### Settings Page (`/settings`)

- PPPoE dial-up configuration (credentials, interface selection).
- On-demand dial-up settings.
- Network interface selection.
- Advanced settings (route guard).

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/rtp/<group>:<port>` | 组播流转单播，RTP 封装（主要功能）|
| GET | `/udp/<group>:<port>` | 组播流转单播，裸 MPEG-TS |
| GET | `/healthz` | 健康检查 |
| GET | `/api/status` | 服务状态（含 `limit_blocked` 限制告警列表）|
| GET | `/api/streams` | 活跃流列表 |
| GET/POST | `/api/rules` | 规则列表 / 新增规则 |
| PUT/DELETE | `/api/rules/<id>` | 更新 / 删除规则 |
| GET/POST | `/api/channels` | 频道列表 / 新增频道 |
| PUT/DELETE | `/api/channels/<id>` | 更新 / 删除频道 |
| POST | `/api/channels/import` | 导入 m3u（上传文件或 JSON `{"content": "..."}`）|
| GET | `/api/m3u` | 导出频道列表为 m3u 文件 |
| POST | `/api/pppoe/retry` | 重新拨号 |
| GET | `/api/interfaces` | 获取网口列表 |
| GET | `/api/settings` | 获取配置（密码脱敏，不返回明文）|
| PUT | `/api/settings` | 更新配置（密码留空/掩码 = 保持不变）|
| GET | `/api/watchtime?period=day\|week\|month&date=YYYY-MM-DD&top=N` | 观看时长统计（默认今天，`top` 限制排名条数）|
| DELETE | `/api/watchtime` | 清空观看时长统计 |
| GET/POST | `/api/limits` | 时长限制规则列表 / 新增 |
| PUT/DELETE | `/api/limits/<id>` | 更新 / 删除时长限制规则 |
| GET/PUT | `/api/limits/pool` | 全局备选地址池（限制触发时的切换目标）|
| GET/PUT | `/api/epg` | 节目单配置与状态（含拉取状态/缓存规模/导出地址）|
| POST | `/api/epg/refresh` | 立即拉取节目单 |
| GET | `/api/epg/channels` | m3u 频道与节目单的匹配视图（导出勾选 + 手工绑定 + 节目单频道列表）|
| POST | `/api/epg/binding` | 手工绑定/取消 m3u 频道与节目单 ID `{"address","epg_id"}` |
| GET | `/api/epg/programs?date=YYYY-MM-DD[&channel=<id>]` | 某日节目（时间轴数据，含已计划录制窗口）|
| GET | `/api/epg/search?q=关键词[&days=N]` | 跨频道节目搜索（默认未来 7 天）|
| GET | `/epg.xml` | 导出勾选频道的 XMLTV（客户端下载地址，按导出窗口裁剪）|
| GET/POST | `/api/record/plans` | 录制计划列表 / 新增（相同频道/周期/时间拒绝）|
| GET/PUT/DELETE | `/api/record/plans/<id>` | 计划详情 / 更新 / 删除 |
| POST | `/api/record/plans/<id>/stop` | 停止正在录制的计划/手动会话 |
| GET | `/api/record/active` | 正在录制的会话 |
| GET | `/api/record/history` | 录制历史 |
| DELETE | `/api/record/history/<id>` | 删除历史记录及文件 |
| POST | `/api/record/manual` | 手动立即录制 `{"address","title?"}` |
| GET/PUT | `/api/record/settings` | 录制设置（目录、预/后卷、并发、看门狗、保留策略、保留剩余空间、Webhook）|
| GET | `/api/record/disk` | 录制目录磁盘用量（含目录、保留阈值、是否已满）|
| GET | `/api/record/file?name=<文件名>` | 下载录制文件（支持 Range）|
| GET/PUT | `/api/record/ai` | AI 接口配置（key 脱敏）|
| POST | `/api/record/ai/generate` | AI 生成录制计划 `{"program":"新闻联播","channels":[]}`（重复项自动跳过）|
| POST | `/api/record/epg` | 从节目单节目创建一次性录制 `{"address","start","end","title"}` |
| POST | `/api/record/epg/series` | 按节目单规律推断创建重复计划（非 AI）`{"address","epg_id","title","start","end"}` |
| GET | `/api/record/preview?filename=&title=&channel=&at=` | 文件命名模板预览 |

所有错误响应统一为 JSON：`{"error": "..."}`。

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/rtp/<group>:<port>` | Multicast to unicast, RTP encapsulated (main function) |
| GET | `/udp/<group>:<port>` | Multicast to unicast, raw MPEG-TS |
| GET | `/healthz` | Health check |
| GET | `/api/status` | Service status (includes `limit_blocked` alert list) |
| GET | `/api/streams` | Active stream list |
| GET/POST | `/api/rules` | Rule list / add rule |
| PUT/DELETE | `/api/rules/<id>` | Update / delete rule |
| GET/POST | `/api/channels` | Channel list / add channel |
| PUT/DELETE | `/api/channels/<id>` | Update / delete channel |
| POST | `/api/channels/import` | Import m3u (file upload or JSON `{"content": "..."}`) |
| GET | `/api/m3u` | Export channels as an m3u file |
| POST | `/api/pppoe/retry` | Re-dial PPPoE |
| GET | `/api/interfaces` | List network interfaces |
| GET | `/api/settings` | Get configuration (password is masked, never returned in plaintext) |
| PUT | `/api/settings` | Update configuration (empty/masked password = keep current) |
| GET | `/api/watchtime?period=day\|week\|month&date=YYYY-MM-DD&top=N` | Watch-time statistics (defaults to today; `top` limits ranking rows) |
| DELETE | `/api/watchtime` | Clear watch-time statistics |
| GET/POST | `/api/limits` | Duration-limit rule list / add |
| PUT/DELETE | `/api/limits/<id>` | Update / delete duration-limit rule |
| GET/PUT | `/api/limits/pool` | Global backup address pool (switch target when a limit triggers) |
| GET/PUT | `/api/epg` | EPG configuration & status (fetch state, cache size, export URL) |
| POST | `/api/epg/refresh` | Fetch EPG immediately |
| GET | `/api/epg/channels` | m3u↔EPG channel match view (export selection + manual binding + EPG channel list) |
| POST | `/api/epg/binding` | Manually bind/unbind an m3u channel to an EPG ID `{"address","epg_id"}` |
| GET | `/api/epg/programs?date=YYYY-MM-DD[&channel=<id>]` | Programs of a day (timeline data, incl. already-planned recording windows) |
| GET | `/api/epg/search?q=keyword[&days=N]` | Cross-channel program search (next 7 days by default) |
| GET | `/epg.xml` | Exported XMLTV for selected channels (client download URL, pruned by export window) |
| GET/POST | `/api/record/plans` | Recording plan list / add (identical channel/schedule/time rejected) |
| GET/PUT/DELETE | `/api/record/plans/<id>` | Plan detail / update / delete |
| POST | `/api/record/plans/<id>/stop` | Stop an in-progress plan / manual session |
| GET | `/api/record/active` | Active recording sessions |
| GET | `/api/record/history` | Recording history |
| DELETE | `/api/record/history/<id>` | Delete history entry and file |
| POST | `/api/record/manual` | Manual record-now `{"address","title?"}` |
| GET/PUT | `/api/record/settings` | Recording settings (directory, pre/post-roll, concurrency, watchdog, retention, min-free space, Webhook) |
| GET | `/api/record/disk` | Recording directory disk usage (incl. dir, min-free threshold, full flag) |
| GET | `/api/record/file?name=<filename>` | Download a recorded file (Range supported) |
| GET/PUT | `/api/record/ai` | AI interface config (key masked) |
| POST | `/api/record/ai/generate` | Generate plans via AI `{"program":"News","channels":[]}` (duplicates skipped) |
| POST | `/api/record/epg` | One-off recording from an EPG program `{"address","start","end","title"}` |
| POST | `/api/record/epg/series` | Create a recurring plan inferred from the EPG pattern (no AI) `{"address","epg_id","title","start","end"}` |
| GET | `/api/record/preview?filename=&title=&channel=&at=` | Filename template preview |

All error responses are uniform JSON: `{"error": "..."}`.

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

## EPG 节目单与录制

### 工作原理

1. **拉取**：在节目单页配置来源（XMLTV URL 或 `/data` 下本地文件，可附加自定义请求头）与 1-24 小时的拉取周期，服务定时下载并解析，按"过去 1 天 + 未来 N 天（默认 7）"时间窗修剪后缓存（`epg_cache.json`）。拉取失败保留旧缓存并记录错误，下一周期自动重试。
2. **匹配**：m3u 频道优先按页面手工绑定的节目单 ID，其次按 `tvg-id` 绑定；都未命中时按归一化名称自动匹配（小写、去空格、去 HD/SD/4K 后缀、CCTV 编号精确回退）。
3. **导出**：`/epg.xml` 仅输出勾选的频道（未勾选 = 全部缓存频道），并限制在"过去 1 天 + 导出窗口天数（默认 3，0 = 全部缓存）"内，供播放器直接作为 EPG 源；`/api/m3u` 生成的播放列表自动携带 `url-tvg` 头与真实 tvg-id（含手工绑定结果）。
4. **录制**：录制计划按周期计算生效窗口（可配置预卷/后卷），窗口内自动打开该频道的组播流（与观看共享同一 refcount reader，并遵循换源规则），RTP 解包后按 MPEG-TS 原样落盘 `.ts` 到 `recordings/`。窗口结束/手动停止/流异常（含看门狗判定的断流）即收尾并写入历史，可触发 Webhook 通知。并发录制数、文件保留策略（保留天数/容量上限）均受录制设置约束。文件名由模板渲染，冲突时自动追加 `(2)`、`(3)`。
5. **其他入口**：节目搜索（`/api/epg/search`）与时间轴"录制系列"（按节目单中同标题节目的出现规律本地推断 daily/weekly，无需 AI）、手动立即录制（`/api/record/manual`，不建计划）。

### 计划示例

```json
// 每天 19:00-19:31
{"name":"新闻联播","channel":"239.69.1.123:10376","type":"daily","start":"19:00","end":"19:31","enabled":true}
// 每周日、三 20:00-21:30（0=周日）
{"name":"周末剧场","channel":"239.254.96.15:8064","type":"weekly","days":[0,3],"start":"20:00","end":"21:30","enabled":true}
// 每月 1、15 日 08:00-10:00
{"name":"法治片","channel":"239.254.96.106:10274","type":"monthly","days":[1,15],"start":"08:00","end":"10:00","enabled":true}
// 每月最后一天 20:00-22:00（-1 = 每月最后一天；31 日在 30 天的月份自动跳过）
{"name":"月末特别","channel":"239.254.96.28:8142","type":"monthly","days":[-1],"start":"20:00","end":"22:00","enabled":true}
```

`end <= start` 视为跨午夜（如 23:00~01:00）。

### AI 录制

- 配置：OpenAI 兼容的 `chat/completions` 完整 URL、API Key、模型名；系统提示词可自定义（留空 = 内置默认，要求 AI 只输出固定结构的 JSON：`{"plans":[{channel_id,name,type,days,date,start,end,reason}]}`）。
- 流程：输入节目名 → 服务端将缓存节目单中相关频道的时间线摘要喂给 AI → AI 依据播出规律推断 daily/weekly/monthly/once → 校验并映射回 m3u 频道后自动创建计划（来源标记为 `ai`），跳过项与原始返回在页面展示。
- 因为节目单缓存的时间范围有限（通常 1~4 天），默认提示词明确要求 AI 生成**重复**计划而非单次计划；特别节目才用 once。

## 路由策略说明

本程序的路由设计原则：

1. **PPPoE 不添加默认路由**：pppd 配置 `nodefaultroute`，从源头避免影响系统路由表
2. **路由守卫双重保障**：每 5 秒检查一次，确保默认路由始终走内网口
3. **PPPoE 仅用于 IPTV 认证**：拨号后 ppp 接口不承担任何系统流量
4. **按需拨号（可选）**：无活跃流时自动断开，有请求时自动拨号

> 说明：本服务**不需要** `net.ipv4.ip_forward`。组播包由内核投递给本进程（接收而非转发），
> 出去的 HTTP 单播由本进程新建并走默认路由，全程不涉及 IP 转发，因此也不会改写宿主机全局 sysctl。

## EPG Guide & Recording

### How It Works

1. **Fetch**: Configure the source (XMLTV URL or a local file under `/data`, with optional custom request headers) and a 1–24 h interval on the EPG page; the service periodically downloads, parses and prunes the EPG to a "past 1 day + future N days (default 7)" window, caching to `epg_cache.json`. On fetch failure the old cache is kept, the error recorded, and the next cycle retries automatically.
2. **Matching**: m3u channels bind to EPG channels via the page-level manual binding first, then `tvg-id`; if neither hits, normalized-name matching is used (lowercase, no spaces, HD/SD/4K suffixes stripped, exact CCTV-number fallback).
3. **Export**: `/epg.xml` only outputs the checked channels (unchecked = all cached channels), limited to "past 1 day + export window in days (default 3, 0 = full cache)" for players to use directly as an EPG source; the `/api/m3u` playlist auto-carries the `url-tvg` header and real tvg-ids (including manual bindings).
4. **Recording**: Plans compute their active window (configurable pre-/post-roll); inside the window the recorder opens the channel's multicast stream (sharing the same refcounted reader as live viewing, honoring source-switch rules), depacketizes RTP and writes raw MPEG-TS `.ts` files to `recordings/`. Window end / manual stop / stream error (incl. dead stream detected by the watchdog) finalizes and logs history, and can trigger a Webhook notification. Concurrency and file retention (age/size cap) are governed by the recording settings. Filenames render from the template; collisions append `(2)`, `(3)`.
5. **Other entry points**: program search (`/api/epg/search`) and the timeline "record series" (locally infers daily/weekly from the occurrences of the same title in the EPG, no AI needed), plus manual record-now (`/api/record/manual`, no plan created).

### Plan Examples

```json
// daily 19:00-19:31
{"name":"News","channel":"239.69.1.123:10376","type":"daily","start":"19:00","end":"19:31","enabled":true}
// weekly Sun & Wed 20:00-21:30 (0=Sunday)
{"name":"Weekend Drama","channel":"239.254.96.15:8064","type":"weekly","days":[0,3],"start":"20:00","end":"21:30","enabled":true}
// monthly 1st & 15th 08:00-10:00
{"name":"Law","channel":"239.254.96.106:10274","type":"monthly","days":[1,15],"start":"08:00","end":"10:00","enabled":true}
// last day of month 20:00-22:00 (-1 = last day; 31st auto-skips in 30-day months)
{"name":"Month-end Special","channel":"239.254.96.28:8142","type":"monthly","days":[-1],"start":"20:00","end":"22:00","enabled":true}
```

`end <= start` means crossing midnight (e.g. 23:00~01:00).

### AI Recording

- Config: full URL of an OpenAI-compatible `chat/completions` endpoint, API key, model; the system prompt is customizable (empty = built-in default, which demands a strict JSON output `{"plans":[{channel_id,name,type,days,date,start,end,reason}]}`).
- Flow: enter a program name → the server feeds the cached EPG timeline of the relevant channels to the LLM → the LLM infers the broadcast pattern (daily/weekly/monthly/once) → validated and mapped back to m3u channels, plans are auto-created (source marked `ai`); skipped items and the raw response are shown in the UI.
- Because the cached EPG range is limited (usually 1–4 days), the default prompt explicitly asks the AI for **recurring** plans; one-offs only for specials.

## Routing Strategy

Design principles for routing in this program:

1. **PPPoE does not add a default route**: pppd is configured with `nodefaultroute`, preventing it from affecting the system routing table at the source.
2. **Dual route guard protection**: Checks every 5 seconds to ensure the default route always goes through the LAN port.
3. **PPPoE is only for IPTV authentication**: After dialing, the ppp interface carries no system traffic.
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
