// Package api 提供 HTTP API 和内嵌 Web 控制台。
package api

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"

	"iptv-udpproxy/internal/channels"
	"iptv-udpproxy/internal/limits"
	"iptv-udpproxy/internal/pppoe"
	"iptv-udpproxy/internal/relay"
	"iptv-udpproxy/internal/rules"
	"iptv-udpproxy/internal/settings"
	"iptv-udpproxy/internal/stats"
)

// Handler 所有 HTTP 路由。
type Handler struct {
	relay    *relay.Manager
	pppoe    *pppoe.Manager
	rules    *rules.Store
	channels *channels.Store
	settings *settings.Store
	stats    *stats.Store
	limits   *limits.Store
	version  string
}

// New 创建 Handler。
func New(relay *relay.Manager, pppoeMgr *pppoe.Manager, ruleStore *rules.Store, channelStore *channels.Store, settingsStore *settings.Store, statsStore *stats.Store, limitsStore *limits.Store, version string) *Handler {
	return &Handler{
		relay:    relay,
		pppoe:    pppoeMgr,
		rules:    ruleStore,
		channels: channelStore,
		settings: settingsStore,
		stats:    statsStore,
		limits:   limitsStore,
		version:  version,
	}
}

// RegisterRoutes 注册所有路由。
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// 组播流端点
	mux.Handle("/rtp/", h.relay)
	mux.Handle("/udp/", h.relay)

	// API
	mux.HandleFunc("/api/status", h.handleStatus)
	mux.HandleFunc("/api/streams", h.handleStreams)
	mux.HandleFunc("/api/rules", h.handleRules)
	mux.HandleFunc("/api/rules/", h.handleRuleByID)
	mux.HandleFunc("/api/channels", h.handleChannels)
	mux.HandleFunc("/api/channels/", h.handleChannelByID)
	mux.HandleFunc("/api/channels/import", h.handleChannelsImport)
	mux.HandleFunc("/api/m3u", h.handleM3U)
	mux.HandleFunc("/api/pppoe/retry", h.handlePPPoERetry)
	mux.HandleFunc("/api/interfaces", h.handleInterfaces)
	mux.HandleFunc("/api/settings", h.handleSettings)
	mux.HandleFunc("/api/watchtime", h.handleWatchtime)
	mux.HandleFunc("/api/limits", h.handleLimits)
	mux.HandleFunc("/api/limits/", h.handleLimitByID)
	mux.HandleFunc("/api/limits/pool", h.handlePool)

	// Web 控制台
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/settings", h.handleSettingsPage)
}

// ---------- API Handlers ----------

// GET /api/status 返回服务整体状态。
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	pppStatus := h.pppoe.Status()
	streams := h.relay.ActiveStreams()
	writeJSON(w, map[string]any{
		"version":       h.version,
		"pppoe":         pppStatus,
		"active_streams": len(streams),
	})
}

// GET /api/streams 返回当前活跃流列表。
func (h *Handler) handleStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	writeJSON(w, h.relay.ActiveStreams())
}

// GET /api/rules  → 列表；POST /api/rules → 新增。
func (h *Handler) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ruleList := h.rules.List()
		if ruleList == nil {
			ruleList = []rules.Rule{}
		}
		writeJSON(w, ruleList)
	case http.MethodPost:
		var rule rules.Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		created, err := h.rules.Add(rule)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, created)
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// GET/PUT/DELETE /api/rules/{id}
func (h *Handler) handleRuleByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	if id == "" {
		http.Error(w, "缺少规则 ID", 400)
		return
	}
	switch r.Method {
	case http.MethodGet:
		for _, rule := range h.rules.List() {
			if rule.ID == id {
				writeJSON(w, rule)
				return
			}
		}
		http.Error(w, "规则不存在", 404)
	case http.MethodPut:
		var rule rules.Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		updated, err := h.rules.Update(id, rule)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		writeJSON(w, updated)
	case http.MethodDelete:
		if err := h.rules.Delete(id); err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// POST /api/pppoe/retry 重新拨号。
func (h *Handler) handlePPPoERetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	go func() {
		if err := h.pppoe.Retry(); err != nil {
			log.Printf("[api] PPPoE 重试失败: %v", err)
		}
	}()
	writeJSON(w, map[string]string{"status": "retrying"})
}

// GET/POST /api/channels 频道列表/添加。
func (h *Handler) handleChannels(w http.ResponseWriter, r *http.Request) {
	// 检查是否是导入请求
	if r.URL.Path == "/api/channels/import" {
		h.handleChannelsImport(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		list := h.channels.List()
		if list == nil {
			list = []channels.Channel{}
		}
		writeJSON(w, list)
	case http.MethodPost:
		var ch channels.Channel
		if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		created, err := h.channels.Add(ch)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, created)
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// PUT/DELETE /api/channels/{id}
func (h *Handler) handleChannelByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/channels/")
	if id == "" || id == "import" {
		return
	}
	switch r.Method {
	case http.MethodPut:
		var ch channels.Channel
		if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		updated, err := h.channels.Update(id, ch)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		writeJSON(w, updated)
	case http.MethodDelete:
		if err := h.channels.Delete(id); err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// POST /api/channels/import 导入 m3u。
func (h *Handler) handleChannelsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取上传的文件或文本
	contentType := r.Header.Get("Content-Type")
	var content string

	if strings.Contains(contentType, "multipart/form-data") {
		// 文件上传
		r.ParseMultipartForm(10 << 20) // 10MB
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "读取文件失败: "+err.Error(), 400)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "读取文件失败: "+err.Error(), 400)
			return
		}
		content = string(data)
	} else {
		// JSON body
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		content = body.Content
	}

	if content == "" {
		http.Error(w, "内容不能为空", 400)
		return
	}

	count := h.channels.ImportM3U(content)
	writeJSON(w, map[string]any{"status": "ok", "imported": count})
}

// GET /api/m3u 生成 m3u 文件。
func (h *Handler) handleM3U(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	host := r.Host
	if host == "" {
		host = "localhost:18888"
	}

	content := h.channels.GenerateM3U(host)
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="iptv.m3u"`))
	w.Write([]byte(content))
}

// GET/PUT /api/settings 获取/更新配置。
func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := h.settings.Get()
		pppoeCfg := h.pppoe.GetConfig()
		// 合并 pppoe 配置
		cfg.PPPoEUser = pppoeCfg.User
		cfg.PPPoEPass = pppoeCfg.Pass
		cfg.PPPoEUnit = pppoeCfg.Unit
		cfg.PPPoEIface = pppoeCfg.Iface
		writeJSON(w, cfg)

	case http.MethodPut:
		var cfg settings.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}

		// 更新 PPPoE 管理器配置
		h.pppoe.UpdateConfig(pppoe.Config{
			Iface: cfg.PPPoEIface,
			User:  cfg.PPPoEUser,
			Pass:  cfg.PPPoEPass,
			Unit:  cfg.PPPoEUnit,
		})

		// 保存设置
		if err := h.settings.Update(cfg); err != nil {
			http.Error(w, "保存配置失败: "+err.Error(), 500)
			return
		}

		writeJSON(w, map[string]string{"status": "ok"})

	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// GET /api/interfaces 获取网络接口列表。
func (h *Handler) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	ifaces, err := settings.ListInterfaces()
	if err != nil {
		http.Error(w, "获取接口列表失败: "+err.Error(), 500)
		return
	}
	writeJSON(w, ifaces)
}

// GET /api/watchtime 查询观看时长统计。
func (h *Handler) handleWatchtime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "day"
	}
	dateStr := r.URL.Query().Get("date")
	top := 0
	if t := r.URL.Query().Get("top"); t != "" {
		fmt.Sscanf(t, "%d", &top)
	}
	nameFn := func(addr string) string {
		return h.channels.GetName(addr)
	}
	result := h.stats.Query(period, dateStr, top, nameFn)
	writeJSON(w, result)
}

// GET/POST /api/limits 限制规则列表/新增。
func (h *Handler) handleLimits(w http.ResponseWriter, r *http.Request) {
	// 检查子路径
	if r.URL.Path == "/api/limits/pool" {
		h.handlePool(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		list := h.limits.ListLimits()
		if list == nil {
			list = []limits.Limit{}
		}
		writeJSON(w, list)
	case http.MethodPost:
		var lim limits.Limit
		if err := json.NewDecoder(r.Body).Decode(&lim); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		created, err := h.limits.AddLimit(lim)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, created)
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// GET/PUT/DELETE /api/limits/{id}
func (h *Handler) handleLimitByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/limits/")
	if id == "" || id == "pool" {
		return
	}
	switch r.Method {
	case http.MethodGet:
		for _, lim := range h.limits.ListLimits() {
			if lim.ID == id {
				writeJSON(w, lim)
				return
			}
		}
		http.Error(w, "限制规则不存在", 404)
	case http.MethodPut:
		var lim limits.Limit
		if err := json.NewDecoder(r.Body).Decode(&lim); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		updated, err := h.limits.UpdateLimit(id, lim)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		writeJSON(w, updated)
	case http.MethodDelete:
		if err := h.limits.DeleteLimit(id); err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// GET/PUT /api/limits/pool 备选地址池。
func (h *Handler) handlePool(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/limits/pool" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, h.limits.GetPool())
	case http.MethodPut:
		var pool limits.PoolConfig
		if err := json.NewDecoder(r.Body).Decode(&pool); err != nil {
			http.Error(w, "JSON 解析失败: "+err.Error(), 400)
			return
		}
		if err := h.limits.SetPool(pool); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method Not Allowed", 405)
	}
}

// GET /settings 返回设置页面。
func (h *Handler) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	settingsTmpl.Execute(w, nil)
}

// GET / 返回 Web 控制台页面。
func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tmpl.Execute(w, nil)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ---------- HTML Template ----------

var tmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>IPTV UDProxy 控制台</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f172a;color:#e2e8f0;line-height:1.6}
.container{max-width:1200px;margin:0 auto;padding:16px}
h1{font-size:1.5rem;margin-bottom:16px;color:#38bdf8}
h2{font-size:1.1rem;margin:20px 0 12px;color:#7dd3fc}
.card{background:#1e293b;border-radius:12px;padding:16px;margin-bottom:16px;border:1px solid #334155}
.status-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:12px}
.stat-item{background:#0f172a;border-radius:8px;padding:12px;text-align:center}
.stat-value{font-size:1.5rem;font-weight:700;color:#22d3ee}
.stat-label{font-size:.85rem;color:#94a3b8;margin-top:4px}
.badge{display:inline-block;padding:2px 10px;border-radius:999px;font-size:.8rem;font-weight:600}
.badge-up{background:#059669;color:#fff}
.badge-error{background:#dc2626;color:#fff}
.badge-idle{background:#6b7280;color:#fff}
.badge-connecting{background:#d97706;color:#fff}
table{width:100%;border-collapse:collapse;margin-top:8px}
th,td{text-align:left;padding:8px 12px;border-bottom:1px solid #334155;font-size:.9rem}
th{color:#94a3b8;font-weight:500}
button{background:#2563eb;color:#fff;border:none;padding:6px 16px;border-radius:6px;cursor:pointer;font-size:.85rem;transition:background 0.2s}
button:hover{background:#1d4ed8}
button:disabled{background:#475569;cursor:not-allowed}
button.danger{background:#dc2626}
button.danger:hover{background:#b91c1c}
button.success{background:#059669}
button.success:hover{background:#047857}
button.sm{padding:4px 10px;font-size:.8rem}
input,select{background:#0f172a;border:1px solid #475569;color:#e2e8f0;padding:6px 10px;border-radius:6px;font-size:.85rem}
select{min-width:200px}
.form-row{display:flex;flex-wrap:wrap;gap:8px;margin-bottom:10px;align-items:center}
.form-row label{min-width:30px;color:#94a3b8;font-size:.85rem}
.days-group{display:flex;gap:4px}
.day-btn{width:36px;height:36px;display:flex;align-items:center;justify-content:center;background:#0f172a;border:2px solid #475569;border-radius:8px;color:#94a3b8;font-size:.85rem;cursor:pointer;transition:all 0.2s;user-select:none}
.day-btn:hover{border-color:#3b82f6;color:#e2e8f0}
.day-btn.active{background:#2563eb;border-color:#2563eb;color:#fff}
.day-btn input{display:none}
.empty{color:#64748b;font-style:italic;text-align:center;padding:20px}
#toast{position:fixed;top:20px;right:20px;padding:10px 20px;border-radius:8px;color:#fff;font-size:.9rem;display:none;z-index:999;animation:slideIn 0.3s ease}
.toast-ok{background:#059669}
.toast-err{background:#dc2626}
@keyframes slideIn{from{transform:translateX(100%);opacity:0}to{transform:translateX(0);opacity:1}}
.retry-btn{margin-left:12px;font-size:.75rem;padding:4px 12px}
.status-header{display:flex;align-items:center;justify-content:space-between}
.stream-count{background:#1e40af;color:#fff;padding:2px 8px;border-radius:12px;font-size:.75rem;margin-left:8px}
.nav{display:flex;gap:12px;margin-bottom:20px}
.nav a{color:#94a3b8;text-decoration:none;padding:6px 12px;border-radius:6px;transition:all 0.2s}
.nav a:hover{background:#334155;color:#e2e8f0}
.nav a.active{background:#1e40af;color:#fff}
.channel-name{color:#22d3ee;font-weight:500}
.import-area{margin-top:12px}
.import-area textarea{width:100%;height:120px;background:#0f172a;border:1px solid #475569;color:#e2e8f0;padding:8px;border-radius:6px;font-size:.85rem;resize:vertical}
.tabs{display:flex;gap:4px;margin-bottom:12px}
.tab-btn{padding:6px 16px;border:1px solid #475569;background:#0f172a;color:#94a3b8;border-radius:6px;cursor:pointer;font-size:.85rem;transition:all 0.2s}
.tab-btn:hover{border-color:#3b82f6;color:#e2e8f0}
.tab-btn.active{background:#2563eb;border-color:#2563eb;color:#fff}
.total-time{font-size:1.3rem;font-weight:700;color:#22d3ee;margin-bottom:8px}
.pool-area textarea{width:100%;height:80px;background:#0f172a;border:1px solid #475569;color:#e2e8f0;padding:8px;border-radius:6px;font-size:.85rem;resize:vertical;font-family:monospace}
</style>
</head>
<body>
<div class="container">
<div class="nav">
<a href="/" class="active">控制台</a>
<a href="/settings">设置</a>
</div>

<h1>IPTV UDProxy 控制台</h1>

<!-- 服务状态 -->
<div class="card" id="status-card">
<div class="status-header">
<h2>服务状态</h2>
</div>
<div class="status-grid" id="status-grid"></div>
</div>

<!-- 活跃流 -->
<div class="card">
<div class="status-header">
<h2>活跃转换流 <span class="stream-count" id="stream-count">0</span></h2>
<button onclick="loadStreams()" class="retry-btn">刷新</button>
</div>
<div id="streams-table"></div>
</div>

<!-- 观看统计 -->
<div class="card">
<h2>观看时长统计</h2>
<div class="tabs">
<button class="tab-btn active" onclick="switchWatchTab('day',this)">今天</button>
<button class="tab-btn" onclick="switchWatchTab('week',this)">本周</button>
<button class="tab-btn" onclick="switchWatchTab('month',this)">本月</button>
</div>
<div class="total-time" id="watch-total"></div>
<div id="watch-table"></div>
</div>

<!-- 时长限制 -->
<div class="card">
<div class="status-header">
<h2>时长限制</h2>
<div>
<button class="sm success" onclick="showAddLimit()">添加限制</button>
</div>
</div>
<div id="limits-table"></div>
<div id="limit-form-area" style="display:none">
<form id="limit-form" class="form-row" onsubmit="return addLimit(event)" style="margin-top:12px">
<label>频道</label><select id="limit-channel" name="channel" required><option value="">选择频道...</option></select>
<label>每日上限(秒)</label><input name="daily_max" type="number" value="0" min="0" style="width:80px">
<label>每周上限(秒)</label><input name="weekly_max" type="number" value="0" min="0" style="width:80px">
<button type="submit" class="success">保存</button>
<button type="button" onclick="document.getElementById('limit-form-area').style.display='none'" style="background:#475569">取消</button>
</form>
</div>
<h2 style="margin-top:20px">备选地址池</h2>
<p style="font-size:.85rem;color:#94a3b8;margin-bottom:8px">超限时从池中随机选取替换源，每行一个组播地址（如 239.69.1.200:10000）</p>
<div class="pool-area">
<textarea id="pool-content" placeholder="239.69.1.200:10000&#10;239.69.1.201:10000"></textarea>
<div style="margin-top:8px;text-align:right">
<button onclick="savePool()" class="success">保存池</button>
</div>
</div>
</div>

<!-- 换源规则 -->
<div class="card">
<h2>换源规则</h2>
<div id="rules-table"></div>
<h2 style="margin-top:20px">新增规则</h2>
<form id="rule-form" class="form-row" onsubmit="return addRule(event)">
<label>名称</label><input name="name" placeholder="如：晚间替换" required style="width:120px">
<label>原地址</label><select id="rule-from" name="from" onchange="document.getElementById('rule-from-input').value=this.value"><option value="">选择频道...</option></select>
<input id="rule-from-input" name="from_manual" placeholder="或手动输入 239.x.x.x:port" style="width:180px">
<label>目标地址</label><select id="rule-to" name="to" onchange="document.getElementById('rule-to-input').value=this.value"><option value="">选择频道...</option></select>
<input id="rule-to-input" name="to_manual" placeholder="或手动输入 239.x.x.x:port" style="width:180px">
<label>开始</label><input name="start" type="time" value="19:00" required>
<label>结束</label><input name="end" type="time" value="19:50" required>
<label>星期</label>
<div class="days-group" id="days-group">
<label class="day-btn" onclick="event.preventDefault();this.classList.toggle('active');this.querySelector('input').checked=this.classList.contains('active')"><input type="checkbox" name="days" value="0">日</label>
<label class="day-btn" onclick="event.preventDefault();this.classList.toggle('active');this.querySelector('input').checked=this.classList.contains('active')"><input type="checkbox" name="days" value="1">一</label>
<label class="day-btn" onclick="event.preventDefault();this.classList.toggle('active');this.querySelector('input').checked=this.classList.contains('active')"><input type="checkbox" name="days" value="2">二</label>
<label class="day-btn" onclick="event.preventDefault();this.classList.toggle('active');this.querySelector('input').checked=this.classList.contains('active')"><input type="checkbox" name="days" value="3">三</label>
<label class="day-btn" onclick="event.preventDefault();this.classList.toggle('active');this.querySelector('input').checked=this.classList.contains('active')"><input type="checkbox" name="days" value="4">四</label>
<label class="day-btn" onclick="event.preventDefault();this.classList.toggle('active');this.querySelector('input').checked=this.classList.contains('active')"><input type="checkbox" name="days" value="5">五</label>
<label class="day-btn" onclick="event.preventDefault();this.classList.toggle('active');this.querySelector('input').checked=this.classList.contains('active')"><input type="checkbox" name="days" value="6">六</label>
</div>
<button type="submit" class="success">添加规则</button>
</form>
</div>

<!-- 频道管理 -->
<div class="card">
<div class="status-header">
<h2>频道管理</h2>
<div>
<a href="/api/m3u" target="_blank"><button class="sm">下载 m3u</button></a>
<button class="sm" onclick="showImport()">导入 m3u</button>
<button class="sm success" onclick="showAddChannel()">添加频道</button>
</div>
</div>
<div id="channels-import" class="import-area" style="display:none">
<textarea id="m3u-content" placeholder="粘贴 m3u 文件内容..."></textarea>
<div style="margin-top:8px;text-align:right">
<button onclick="importM3U()" class="success">导入</button>
<button onclick="document.getElementById('channels-import').style.display='none'" style="background:#475569">取消</button>
</div>
</div>
<div id="channel-form-area" style="display:none">
<form id="channel-form" class="form-row" onsubmit="return addChannel(event)" style="margin-top:12px">
<label>名称</label><input name="name" placeholder="如：CCTV1" required style="width:120px">
<label>地址</label><input name="address" placeholder="239.254.96.96:8550" required style="width:200px">
<label>分组</label><input name="group" placeholder="如：央视、卫视" style="width:100px">
<label>Logo</label><input name="logo" placeholder="图标URL(可选)" style="width:200px">
<button type="submit" class="success">保存</button>
<button type="button" onclick="document.getElementById('channel-form-area').style.display='none'" style="background:#475569">取消</button>
</form>
</div>
<div id="channels-table"></div>
</div>

</div>
<div id="toast"></div>

<script>
const BADGE = {up:'badge-up',error:'badge-error',idle:'badge-idle',connecting:'badge-connecting'};
const LABEL = {up:'运行中',error:'错误',idle:'未拨号',connecting:'拨号中'};
let channelsList = [];

function esc(str) {
  if (str == null) return '';
  return String(str).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

function toast(msg, ok) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = ok ? 'toast-ok' : 'toast-err';
  t.style.display = 'block';
  setTimeout(() => t.style.display = 'none', 3000);
}

function retryPPPoE() {
  if (!confirm('确认重新拨号？')) return;
  fetch('/api/pppoe/retry', {method: 'POST'})
    .then(r => r.json())
    .then(() => toast('正在重新拨号...', true))
    .catch(e => toast('重试失败: ' + e.message, false));
}

function channelName(addr) {
  const ch = channelsList.find(c => c.address === addr);
  return ch ? ch.name : '';
}

function loadStatus() {
  fetch('/api/status').then(r => r.json()).then(d => {
    const g = document.getElementById('status-grid');
    const pp = d.pppoe || {};
    const stateClass = BADGE[pp.state] || '';
    const stateLabel = LABEL[pp.state] || esc(pp.state);
    const retryBtn = pp.state === 'error' || pp.state === 'idle' 
      ? '<button onclick="retryPPPoE()" class="retry-btn">重新拨号</button>' 
      : '';
    
    g.innerHTML = 
      '<div class="stat-item"><div class="stat-value">' + esc(d.version) + '</div><div class="stat-label">版本</div></div>' +
      '<div class="stat-item"><div class="stat-value"><span class="badge ' + stateClass + '">' + stateLabel + '</span>' + retryBtn + '</div><div class="stat-label">PPPoE (' + esc(pp.unit) + ')</div></div>' +
      '<div class="stat-item"><div class="stat-value">' + esc(pp.ip || '-') + '</div><div class="stat-label">拨号IP</div></div>' +
      '<div class="stat-item"><div class="stat-value">' + esc(pp.uptime || '-') + '</div><div class="stat-label">在线时长</div></div>' +
      '<div class="stat-item"><div class="stat-value">' + esc(d.active_streams) + '</div><div class="stat-label">活跃流数</div></div>';
    
    document.getElementById('stream-count').textContent = d.active_streams || 0;
  }).catch(e => {});
}

function loadStreams() {
  fetch('/api/streams').then(r => r.json()).then(list => {
    const d = document.getElementById('streams-table');
    if (!list || !list.length) {
      d.innerHTML = '<div class="empty">暂无活跃流</div>';
      return;
    }
    let h = '<table><tr><th>客户端</th><th>请求频道</th><th>实际频道</th><th>开始时间</th><th>已发送</th></tr>';
    list.forEach(s => {
      const origName = s.orig_name || channelName(s.orig_addr);
      const realName = s.real_name || channelName(s.real_addr);
      const origDisplay = origName ? '<span class="channel-name">' + esc(origName) + '</span><br><small>' + esc(s.orig_addr) + '</small>' : esc(s.orig_addr);
      const realDisplay = realName ? '<span class="channel-name">' + esc(realName) + '</span><br><small>' + esc(s.real_addr || s.orig_addr) + '</small>' : esc(s.real_addr || s.orig_addr);
      h += '<tr><td>' + esc(s.remote_addr) + '</td><td>' + origDisplay + '</td><td>' + realDisplay + '</td><td>' + esc(new Date(s.start_time).toLocaleTimeString()) + '</td><td>' + (s.bytes_sent / 1024).toFixed(1) + ' KB</td></tr>';
    });
    h += '</table>';
    d.innerHTML = h;
  }).catch(e => {});
}

function loadRules() {
  fetch('/api/rules').then(r => r.json()).then(list => {
    const d = document.getElementById('rules-table');
    if (!list || !list.length) {
      d.innerHTML = '<div class="empty">暂无规则</div>';
      return;
    }
    let h = '<table><tr><th>名称</th><th>原地址</th><th>目标</th><th>时间窗</th><th>星期</th><th>状态</th><th>操作</th></tr>';
    const DAYS = ['日', '一', '二', '三', '四', '五', '六'];
    list.forEach(r => {
      const days = r.days && r.days.length ? r.days.map(d => DAYS[d]).join(',') : '每天';
      const st = r.enabled ? '<span class="badge badge-up">启用</span>' : '<span class="badge badge-idle">禁用</span>';
      const fromName = channelName(r.from);
      const toName = channelName(r.to);
      const fromDisplay = fromName ? '<span class="channel-name">' + esc(fromName) + '</span><br><small>' + esc(r.from) + '</small>' : esc(r.from);
      const toDisplay = toName ? '<span class="channel-name">' + esc(toName) + '</span><br><small>' + esc(r.to) + '</small>' : esc(r.to);
      h += '<tr><td>' + esc(r.name) + '</td><td>' + fromDisplay + '</td><td>' + toDisplay + '</td><td>' + esc(r.start) + ' ~ ' + esc(r.end) + '</td><td>' + esc(days) + '</td><td>' + st + '</td>';
      h += '<td><button class="sm" onclick="toggleRule(\'' + esc(r.id) + '\',' + !r.enabled + ')">' + (r.enabled ? '禁用' : '启用') + '</button> ';
      h += '<button class="sm danger" onclick="delRule(\'' + esc(r.id) + '\')">删除</button></td></tr>';
    });
    h += '</table>';
    d.innerHTML = h;
  }).catch(e => {});
}

function loadChannels() {
  fetch('/api/channels').then(r => r.json()).then(list => {
    channelsList = list || [];
    updateChannelSelects();
    updateLimitChannelSelect();
    
    const d = document.getElementById('channels-table');
    if (!list || !list.length) {
      d.innerHTML = '<div class="empty">暂无频道，点击"导入 m3u"或"添加频道"</div>';
      return;
    }
    
    // 按分组显示
    const groups = {};
    list.forEach(ch => {
      const g = ch.group || '未分类';
      if (!groups[g]) groups[g] = [];
      groups[g].push(ch);
    });
    
    let h = '';
    Object.keys(groups).sort().forEach(group => {
      h += '<h3 style="margin:12px 0 8px;color:#7dd3fc;font-size:1rem">' + esc(group) + ' (' + groups[group].length + ')</h3>';
      h += '<table><tr><th>名称</th><th>地址</th><th>操作</th></tr>';
      groups[group].forEach(ch => {
        h += '<tr><td>' + esc(ch.name) + '</td><td>' + esc(ch.address) + '</td>';
        h += '<td><button class="sm" onclick="editChannel(\'' + esc(ch.id) + '\')">编辑</button> ';
        h += '<button class="sm danger" onclick="delChannel(\'' + esc(ch.id) + '\')">删除</button></td></tr>';
      });
      h += '</table>';
    });
    d.innerHTML = h;
  }).catch(e => {});
}

function updateChannelSelects() {
  const groups = {};
  channelsList.forEach(ch => {
    const g = ch.group || '未分类';
    if (!groups[g]) groups[g] = [];
    groups[g].push(ch);
  });
  
  ['rule-from', 'rule-to'].forEach(id => {
    const sel = document.getElementById(id);
    sel.innerHTML = '<option value="">选择频道...</option>';
    Object.keys(groups).sort().forEach(group => {
      const optgroup = document.createElement('optgroup');
      optgroup.label = group;
      groups[group].sort((a,b) => a.name.localeCompare(b.name)).forEach(ch => {
        const opt = new Option(ch.name + ' (' + ch.address + ')', ch.address);
        optgroup.appendChild(opt);
      });
      sel.appendChild(optgroup);
    });
  });
}

function addRule(e) {
  e.preventDefault();
  const f = e.target;
  const days = [];
  document.querySelectorAll('#days-group input[name="days"]:checked').forEach(cb => {
    days.push(parseInt(cb.value));
  });
  
  // 优先使用下拉选择的值，否则使用手动输入的值
  const from = f.from.value || f.from_manual.value;
  const to = f.to.value || f.to_manual.value;
  
  if (!from || !to) {
    toast('请选择或输入原地址和目标地址', false);
    return false;
  }
  
  const body = {
    name: f.name.value,
    from: from,
    to: to,
    start: f.start.value,
    end: f.end.value,
    days: days,
    enabled: true
  };
  fetch('/api/rules', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
    .then(r => {
      if (!r.ok) return r.text().then(t => { throw new Error(t); });
      return r.json();
    })
    .then(() => {
      toast('规则已添加', true);
      loadRules();
      f.reset();
      document.querySelectorAll('#days-group .day-btn').forEach(btn => btn.classList.remove('active'));
    })
    .catch(e => toast(e.message, false));
  return false;
}

function toggleRule(id, en) {
  fetch('/api/rules/' + id)
    .then(r => {
      if (!r.ok) return r.text().then(t => { throw new Error(t); });
      return r.json();
    })
    .then(rule => {
      rule.enabled = en;
      return fetch('/api/rules/' + id, {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(rule)
      });
    })
    .then(r => {
      if (!r.ok) return r.text().then(t => { throw new Error(t); });
      return r.json();
    })
    .then(() => { toast('已更新', true); loadRules(); })
    .catch(e => toast('更新失败: ' + e.message, false));
}

function delRule(id) {
  if (!confirm('确认删除此规则？')) return;
  fetch('/api/rules/' + id, {method: 'DELETE'})
    .then(() => { toast('已删除', true); loadRules(); })
    .catch(e => toast(e.message, false));
}

function showImport() {
  document.getElementById('channels-import').style.display = 'block';
  document.getElementById('channel-form-area').style.display = 'none';
}

function showAddChannel() {
  document.getElementById('channel-form-area').style.display = 'block';
  document.getElementById('channels-import').style.display = 'none';
}

function importM3U() {
  const content = document.getElementById('m3u-content').value;
  if (!content) { toast('请输入 m3u 内容', false); return; }
  fetch('/api/channels/import', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({content: content})
  })
    .then(r => r.json())
    .then(d => {
      toast('已导入 ' + d.imported + ' 个频道', true);
      document.getElementById('m3u-content').value = '';
      document.getElementById('channels-import').style.display = 'none';
      loadChannels();
    })
    .catch(e => toast('导入失败: ' + e.message, false));
}

function addChannel(e) {
  e.preventDefault();
  const f = e.target;
  const body = {
    name: f.name.value,
    address: f.address.value,
    group: f.group.value,
    logo: f.logo.value
  };
  fetch('/api/channels', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
    .then(r => {
      if (!r.ok) return r.text().then(t => { throw new Error(t); });
      return r.json();
    })
    .then(() => {
      toast('频道已添加', true);
      f.reset();
      document.getElementById('channel-form-area').style.display = 'none';
      loadChannels();
    })
    .catch(e => toast(e.message, false));
  return false;
}

function editChannel(id) {
  const ch = channelsList.find(c => c.id === id);
  if (!ch) return;
  const name = prompt('频道名称:', ch.name);
  if (name === null) return;
  const address = prompt('组播地址:', ch.address);
  if (address === null) return;
  const group = prompt('分组:', ch.group || '');
  if (group === null) return;
  
  fetch('/api/channels/' + id, {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({name, address, group, logo: ch.logo})
  })
    .then(r => { if (!r.ok) throw new Error('更新失败'); return r.json(); })
    .then(() => { toast('已更新', true); loadChannels(); })
    .catch(e => toast(e.message, false));
}

function delChannel(id) {
  if (!confirm('确认删除此频道？')) return;
  fetch('/api/channels/' + id, {method: 'DELETE'})
    .then(() => { toast('已删除', true); loadChannels(); })
    .catch(e => toast(e.message, false));
}

// ---- 观看统计 ----
let currentWatchPeriod = 'day';

function formatDuration(sec) {
  if (sec < 60) return sec + '秒';
  if (sec < 3600) return Math.floor(sec/60) + '分' + (sec%60) + '秒';
  return Math.floor(sec/3600) + '时' + Math.floor((sec%3600)/60) + '分';
}

function switchWatchTab(period, btn) {
  currentWatchPeriod = period;
  document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
  btn.classList.add('active');
  loadWatchtime();
}

function loadWatchtime() {
  fetch('/api/watchtime?period=' + currentWatchPeriod)
    .then(r => r.json())
    .then(d => {
      document.getElementById('watch-total').textContent = '总观看时长: ' + formatDuration(d.total_seconds || 0);
      const list = d.channels || [];
      const el = document.getElementById('watch-table');
      if (!list.length) {
        el.innerHTML = '<div class="empty">暂无观看记录</div>';
        return;
      }
      let h = '<table><tr><th>排名</th><th>频道</th><th>地址</th><th>观看时长</th></tr>';
      list.forEach(item => {
        const name = item.name ? '<span class="channel-name">' + esc(item.name) + '</span>' : '-';
        h += '<tr><td>' + item.rank + '</td><td>' + name + '</td><td>' + esc(item.address) + '</td><td>' + formatDuration(item.duration) + '</td></tr>';
      });
      h += '</table>';
      el.innerHTML = h;
    })
    .catch(e => {});
}

// ---- 时长限制 ----
function updateLimitChannelSelect() {
  const sel = document.getElementById('limit-channel');
  sel.innerHTML = '<option value="">选择频道...</option>';
  const groups = {};
  channelsList.forEach(ch => {
    const g = ch.group || '未分类';
    if (!groups[g]) groups[g] = [];
    groups[g].push(ch);
  });
  Object.keys(groups).sort().forEach(group => {
    const optgroup = document.createElement('optgroup');
    optgroup.label = group;
    groups[group].sort((a,b) => a.name.localeCompare(b.name)).forEach(ch => {
      const opt = new Option(ch.name + ' (' + ch.address + ')', ch.address);
      optgroup.appendChild(opt);
    });
    sel.appendChild(optgroup);
  });
}

function showAddLimit() {
  updateLimitChannelSelect();
  document.getElementById('limit-form-area').style.display = 'block';
}

function loadLimits() {
  fetch('/api/limits').then(r => r.json()).then(list => {
    const d = document.getElementById('limits-table');
    if (!list || !list.length) {
      d.innerHTML = '<div class="empty">暂无限制规则</div>';
      return;
    }
    let h = '<table><tr><th>频道</th><th>每日上限</th><th>每周上限</th><th>状态</th><th>操作</th></tr>';
    list.forEach(lim => {
      const name = channelName(lim.address) || lim.address;
      const daily = lim.daily_max > 0 ? formatDuration(lim.daily_max) : '-';
      const weekly = lim.weekly_max > 0 ? formatDuration(lim.weekly_max) : '-';
      const st = lim.enabled ? '<span class="badge badge-up">启用</span>' : '<span class="badge badge-idle">禁用</span>';
      h += '<tr><td><span class="channel-name">' + esc(name) + '</span><br><small>' + esc(lim.address) + '</small></td>';
      h += '<td>' + daily + '</td><td>' + weekly + '</td><td>' + st + '</td>';
      h += '<td><button class="sm" onclick="toggleLimit(\'' + esc(lim.id) + '\',' + !lim.enabled + ')">' + (lim.enabled ? '禁用' : '启用') + '</button> ';
      h += '<button class="sm danger" onclick="delLimit(\'' + esc(lim.id) + '\')">删除</button></td></tr>';
    });
    h += '</table>';
    d.innerHTML = h;
  }).catch(e => {});
}

function addLimit(e) {
  e.preventDefault();
  const f = e.target;
  const address = f.channel.value;
  if (!address) { toast('请选择频道', false); return false; }
  const body = {
    address: address,
    daily_max: parseInt(f.daily_max.value) || 0,
    weekly_max: parseInt(f.weekly_max.value) || 0,
    enabled: true
  };
  fetch('/api/limits', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body)
  })
    .then(r => {
      if (!r.ok) return r.text().then(t => { throw new Error(t); });
      return r.json();
    })
    .then(() => {
      toast('限制规则已添加', true);
      f.reset();
      document.getElementById('limit-form-area').style.display = 'none';
      loadLimits();
    })
    .catch(e => toast(e.message, false));
  return false;
}

function toggleLimit(id, en) {
  fetch('/api/limits/' + id)
    .then(r => { if (!r.ok) throw new Error('获取失败'); return r.json(); })
    .then(lim => {
      lim.enabled = en;
      return fetch('/api/limits/' + id, {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(lim)
      });
    })
    .then(r => { if (!r.ok) throw new Error('更新失败'); return r.json(); })
    .then(() => { toast('已更新', true); loadLimits(); })
    .catch(e => toast('更新失败: ' + e.message, false));
}

function delLimit(id) {
  if (!confirm('确认删除此限制规则？')) return;
  fetch('/api/limits/' + id, {method: 'DELETE'})
    .then(() => { toast('已删除', true); loadLimits(); })
    .catch(e => toast(e.message, false));
}

function loadPool() {
  fetch('/api/limits/pool').then(r => r.json()).then(pool => {
    document.getElementById('pool-content').value = (pool.addresses || []).join('\n');
  }).catch(e => {});
}

function savePool() {
  const content = document.getElementById('pool-content').value.trim();
  const addresses = content ? content.split('\n').map(s => s.trim()).filter(s => s) : [];
  fetch('/api/limits/pool', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({addresses: addresses})
  })
    .then(r => {
      if (!r.ok) return r.text().then(t => { throw new Error(t); });
      return r.json();
    })
    .then(() => toast('备选池已保存', true))
    .catch(e => toast(e.message, false));
}

// 初始化加载
loadChannels();
loadStatus();
loadStreams();
loadRules();
loadWatchtime();
loadLimits();
loadPool();

// 定时刷新
setInterval(() => { loadStatus(); loadStreams(); }, 5000);
setInterval(loadRules, 10000);
setInterval(loadWatchtime, 10000);
</script>
</body>
</html>
`))

// ---------- Settings Page Template ----------

var settingsTmpl = template.Must(template.New("settings").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>IPTV UDProxy 设置</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f172a;color:#e2e8f0;line-height:1.6}
.container{max-width:800px;margin:0 auto;padding:16px}
h1{font-size:1.5rem;margin-bottom:16px;color:#38bdf8}
h2{font-size:1.1rem;margin:20px 0 12px;color:#7dd3fc}
.card{background:#1e293b;border-radius:12px;padding:20px;margin-bottom:16px;border:1px solid #334155}
.form-group{margin-bottom:16px}
.form-group label{display:block;color:#94a3b8;font-size:.9rem;margin-bottom:6px}
.form-group input,.form-group select{width:100%;background:#0f172a;border:1px solid #475569;color:#e2e8f0;padding:10px 12px;border-radius:8px;font-size:.9rem}
.form-group input:focus,.form-group select:focus{outline:none;border-color:#3b82f6;box-shadow:0 0 0 2px rgba(59,130,246,0.2)}
.form-group .hint{font-size:.8rem;color:#64748b;margin-top:4px}
.form-row{display:grid;grid-template-columns:1fr 1fr;gap:16px}
.checkbox-group{display:flex;align-items:center;gap:8px}
.checkbox-group input[type="checkbox"]{width:18px;height:18px;cursor:pointer}
.checkbox-group label{margin:0;cursor:pointer}
button{background:#2563eb;color:#fff;border:none;padding:10px 24px;border-radius:8px;cursor:pointer;font-size:.9rem;font-weight:500;transition:background 0.2s}
button:hover{background:#1d4ed8}
button:disabled{background:#475569;cursor:not-allowed}
button.success{background:#059669}
button.success:hover{background:#047857}
.nav{display:flex;gap:12px;margin-bottom:20px}
.nav a{color:#94a3b8;text-decoration:none;padding:6px 12px;border-radius:6px;transition:all 0.2s}
.nav a:hover{background:#334155;color:#e2e8f0}
.nav a.active{background:#1e40af;color:#fff}
#toast{position:fixed;top:20px;right:20px;padding:10px 20px;border-radius:8px;color:#fff;font-size:.9rem;display:none;z-index:999;animation:slideIn 0.3s ease}
.toast-ok{background:#059669}
.toast-err{background:#dc2626}
@keyframes slideIn{from{transform:translateX(100%);opacity:0}to{transform:translateX(0);opacity:1}}
.interface-info{background:#0f172a;border-radius:6px;padding:8px 12px;margin-top:4px;font-size:.8rem;color:#22d3ee}
</style>
</head>
<body>
<div class="container">
<div class="nav">
<a href="/">控制台</a>
<a href="/settings" class="active">设置</a>
</div>

<h1>系统设置</h1>

<form id="settings-form" onsubmit="return saveSettings(event)">

<!-- PPPoE 设置 -->
<div class="card">
<h2>PPPoE 拨号设置</h2>

<div class="form-group">
<div class="checkbox-group">
<input type="checkbox" id="pppoe_enable" name="pppoe_enable">
<label for="pppoe_enable">启用 PPPoE 拨号</label>
</div>
</div>

<div class="form-group">
<label>拨号网口</label>
<select id="pppoe_iface" name="pppoe_iface"></select>
<div id="pppoe_iface_info" class="interface-info" style="display:none"></div>
</div>

<div class="form-row">
<div class="form-group">
<label>PPPoE 账号</label>
<input type="text" id="pppoe_user" name="pppoe_user" placeholder="如: user@iptv">
</div>
<div class="form-group">
<label>PPPoE 密码</label>
<input type="password" id="pppoe_pass" name="pppoe_pass" placeholder="密码">
</div>
</div>

<div class="form-group">
<label>PPPoE Unit 号</label>
<input type="number" id="pppoe_unit" name="pppoe_unit" value="60" min="0" max="100">
<div class="hint">接口名为 ppp{unit}，默认 60</div>
</div>

<div class="form-group">
<div class="checkbox-group">
<input type="checkbox" id="on_demand" name="on_demand">
<label for="on_demand">按需拨号（无流量时自动断开）</label>
</div>
</div>

<div class="form-group" id="idle_timeout_group" style="display:none">
<label>空闲断开超时（秒）</label>
<input type="number" id="idle_timeout" name="idle_timeout" value="300" min="60" max="3600">
<div class="hint">无活跃流后多久断开拨号，默认 300 秒</div>
</div>
</div>

<!-- 网口设置 -->
<div class="card">
<h2>网络接口设置</h2>

<div class="form-group">
<label>组播监听网口（IPTV 口）</label>
<select id="mcast_iface" name="mcast_iface"></select>
<div id="mcast_iface_info" class="interface-info" style="display:none"></div>
<div class="hint">用于接收 IPTV 组播流的网口</div>
</div>
</div>

<!-- 高级设置 -->
<div class="card">
<h2>高级设置</h2>

<div class="form-group">
<div class="checkbox-group">
<input type="checkbox" id="route_guard" name="route_guard" checked>
<label for="route_guard">路由守卫（防止默认路由走 ppp）</label>
</div>
<div class="hint">rp_filter 会自动设置为 loose mode，确保组播正常工作</div>
</div>
</div>

<div style="text-align:right">
<button type="submit" class="success">保存配置</button>
</div>

</form>
</div>
<div id="toast"></div>

<script>
function esc(str) {
  if (str == null) return '';
  return String(str).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

function toast(msg, ok) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = ok ? 'toast-ok' : 'toast-err';
  t.style.display = 'block';
  setTimeout(() => t.style.display = 'none', 3000);
}

// 加载网口列表
function loadInterfaces() {
  fetch('/api/interfaces')
    .then(r => r.json())
    .then(list => {
      const pppoeSelect = document.getElementById('pppoe_iface');
      const mcastSelect = document.getElementById('mcast_iface');
      
      pppoeSelect.innerHTML = '';
      mcastSelect.innerHTML = '';
      
      list.forEach(iface => {
        const addrs = iface.addresses || [];
        const status = iface.is_up ? ' [活跃]' : ' [未激活]';
        const addrStr = addrs.length ? ' (' + addrs[0] + ')' : '';
        const opt1 = new Option(iface.name + status + addrStr, iface.name);
        const opt2 = new Option(iface.name + status + addrStr, iface.name);
        pppoeSelect.add(opt1);
        mcastSelect.add(opt2);
      });
    })
    .catch(e => toast('加载网口列表失败: ' + e.message, false));
}

// 显示网口信息
function showInterfaceInfo(selectId, infoId) {
  const select = document.getElementById(selectId);
  const info = document.getElementById(infoId);
  select.addEventListener('change', () => {
    const selected = select.options[select.selectedIndex];
    if (selected) {
      info.textContent = '已选择: ' + selected.text;
      info.style.display = 'block';
    }
  });
}

// 加载配置
function loadSettings() {
  fetch('/api/settings')
    .then(r => r.json())
    .then(cfg => {
      document.getElementById('pppoe_enable').checked = cfg.pppoe_enable;
      document.getElementById('pppoe_user').value = cfg.pppoe_user || '';
      document.getElementById('pppoe_pass').value = cfg.pppoe_pass || '';
      document.getElementById('pppoe_unit').value = cfg.pppoe_unit || 60;
      document.getElementById('on_demand').checked = cfg.on_demand;
      document.getElementById('idle_timeout').value = cfg.idle_timeout || 300;
      document.getElementById('route_guard').checked = cfg.route_guard;
      
      // 设置选中的网口
      if (cfg.pppoe_iface) {
        const pppoeSelect = document.getElementById('pppoe_iface');
        for (let i = 0; i < pppoeSelect.options.length; i++) {
          if (pppoeSelect.options[i].value === cfg.pppoe_iface) {
            pppoeSelect.selectedIndex = i;
            break;
          }
        }
      }
      if (cfg.mcast_iface) {
        const mcastSelect = document.getElementById('mcast_iface');
        for (let i = 0; i < mcastSelect.options.length; i++) {
          if (mcastSelect.options[i].value === cfg.mcast_iface) {
            mcastSelect.selectedIndex = i;
            break;
          }
        }
      }
      
      // 更新按需拨号相关的显示
      updateOnDemandVisibility();
    })
    .catch(e => toast('加载配置失败: ' + e.message, false));
}

// 更新按需拨号相关显示
function updateOnDemandVisibility() {
  const onDemand = document.getElementById('on_demand').checked;
  document.getElementById('idle_timeout_group').style.display = onDemand ? 'block' : 'none';
}

// 保存配置
function saveSettings(e) {
  e.preventDefault();
  
  const cfg = {
    pppoe_enable: document.getElementById('pppoe_enable').checked,
    pppoe_iface: document.getElementById('pppoe_iface').value,
    pppoe_user: document.getElementById('pppoe_user').value,
    pppoe_pass: document.getElementById('pppoe_pass').value,
    pppoe_unit: parseInt(document.getElementById('pppoe_unit').value) || 60,
    on_demand: document.getElementById('on_demand').checked,
    idle_timeout: parseInt(document.getElementById('idle_timeout').value) || 300,
    mcast_iface: document.getElementById('mcast_iface').value,
    route_guard: document.getElementById('route_guard').checked
  };
  
  fetch('/api/settings', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(cfg)
  })
    .then(r => {
      if (!r.ok) return r.text().then(t => { throw new Error(t); });
      return r.json();
    })
    .then(() => toast('配置已保存', true))
    .catch(e => toast('保存失败: ' + e.message, false));
  
  return false;
}

// 绑定事件
document.getElementById('on_demand').addEventListener('change', updateOnDemandVisibility);

// 初始化
showInterfaceInfo('pppoe_iface', 'pppoe_iface_info');
showInterfaceInfo('mcast_iface', 'mcast_iface_info');
loadInterfaces();
setTimeout(loadSettings, 500); // 等待接口列表加载完成
</script>
</body>
</html>
`))
