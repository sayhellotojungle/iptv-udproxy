// Package relay 是 UDProxy 核心：将组播流转为单播 HTTP 响应。
//
// 流程：HTTP handler 解析请求中的组播地址 → 规则引擎解析实际源 →
// 管理对应 Reader（加入/离开组播组）→ 把组播数据写入 HTTP ResponseWriter。
package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"iptv-udpproxy/internal/limits"
	"iptv-udpproxy/internal/mcast"
	"iptv-udpproxy/internal/rtp"
	"iptv-udpproxy/internal/rules"
	"iptv-udpproxy/internal/stats"
)

// SetIface 动态更新组播监听网口（Web 设置变更时同步，避免新 reader 使用旧接口）。
func (m *Manager) SetIface(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ifaceName = name
}

// StreamInfo 当前活跃流信息。
type StreamInfo struct {
	ID          string    `json:"id"`
	RemoteAddr  string    `json:"remote_addr"`
	OrigAddr    string    `json:"orig_addr"`
	OrigName    string    `json:"orig_name,omitempty"`
	RealAddr    string    `json:"real_addr"`
	RealName    string    `json:"real_name,omitempty"`
	StartTime   time.Time `json:"start_time"`
	BytesSent   int64     `json:"bytes_sent"`
	InputMode   string    `json:"input_mode"`
	InputErrors uint64    `json:"input_errors"`
	QueueErrors uint64    `json:"queue_errors"`
}

// PPPoEManager PPPoE 管理器接口，用于按需拨号。
type PPPoEManager interface {
	NotifyActivity()
	NotifyIdle(idleTimeout time.Duration)
	IsUp() bool
	IsEnabled() bool
	Start() error
	SetOnDemand(enable bool)
	SetEnabled(enable bool)
}

// ChannelStore 频道存储接口。
type ChannelStore interface {
	GetName(address string) string
}

// Manager 管理所有组播 reader 和活跃流。
type Manager struct {
	ifaceName string
	rules     *rules.Store
	pppoe     PPPoEManager
	channels  ChannelStore
	stats     *stats.Store
	limits    *limits.Store

	mu          sync.Mutex
	readers     map[string]*readerEntry
	streams     map[string]*StreamInfo
	counter     int64
	idleTimeout time.Duration // 空闲断开超时
}

type readerEntry struct {
	reader *mcast.Reader
	ch     chan mcast.Packet
	refCnt int
}

type inputMode uint8

const (
	modeRTP inputMode = iota
	modeUDP
)

func (m inputMode) String() string {
	if m == modeRTP {
		return "rtp"
	}
	return "udp"
}

type inputDecoder struct {
	mode inputMode
	rtp  *rtp.Depacketizer
}

func newInputDecoder(mode inputMode) *inputDecoder {
	d := &inputDecoder{mode: mode}
	if mode == modeRTP {
		d.rtp = rtp.NewDepacketizer()
	}
	return d
}

func (d *inputDecoder) Decode(datagram []byte) ([]byte, error) {
	if d.mode == modeRTP {
		return d.rtp.Depacketize(datagram)
	}
	if len(datagram) == 0 || len(datagram)%188 != 0 {
		return nil, fmt.Errorf("UDP MPEG-TS 长度 %d 不是 188 的整数倍", len(datagram))
	}
	for offset := 0; offset < len(datagram); offset += 188 {
		if datagram[offset] != 0x47 {
			return nil, fmt.Errorf("UDP MPEG-TS 在偏移 %d 缺少同步字节", offset)
		}
	}
	return datagram, nil
}

// New 创建 Manager。
func New(ifaceName string, ruleStore *rules.Store) *Manager {
	return &Manager{
		ifaceName:   ifaceName,
		rules:       ruleStore,
		readers:     make(map[string]*readerEntry),
		streams:     make(map[string]*StreamInfo),
		idleTimeout: 5 * time.Minute, // 默认 5 分钟空闲断开
	}
}

// SetPPPoE 设置 PPPoE 管理器，用于按需拨号。
func (m *Manager) SetPPPoE(pppoe PPPoEManager) {
	m.pppoe = pppoe
}

// SetChannels 设置频道存储，用于查询频道名称。
func (m *Manager) SetChannels(channels ChannelStore) {
	m.channels = channels
}

// SetStats 设置观看时长统计存储。
func (m *Manager) SetStats(s *stats.Store) {
	m.stats = s
}

// SetLimits 设置观看时长限制存储。
func (m *Manager) SetLimits(l *limits.Store) {
	m.limits = l
}

// SetIdleTimeout 设置空闲断开超时时间。
func (m *Manager) SetIdleTimeout(d time.Duration) {
	m.idleTimeout = d
}

// StartCleanup 启动幽灵订阅清除的后台协程。
// 每 30 秒扫描一次所有 reader，清除连续发送失败超过阈值的幽灵订阅者，
// 同时回收无订阅者的空 reader。ctx 取消时退出。
func (m *Manager) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		const maxConsecutiveFails = 100 // 约 1-2 秒的连续失败（取决于包速率）
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sweepGhosts(maxConsecutiveFails)
			}
		}
	}()
}

// sweepGhosts 扫描所有 reader，清除幽灵订阅和空 reader。
func (m *Manager) sweepGhosts(maxFails int) {
	m.mu.Lock()
	// 收集需要检查的 reader（在锁内快照，避免遍历期间修改）
	type readerInfo struct {
		key   string
		entry *readerEntry
	}
	var readers []readerInfo
	for key, entry := range m.readers {
		readers = append(readers, readerInfo{key: key, entry: entry})
	}
	m.mu.Unlock()

	for _, ri := range readers {
		removed := ri.entry.reader.CleanupStale(maxFails)
		if removed > 0 {
			log.Printf("[relay] 清除 %s 的 %d 个幽灵订阅", ri.key, removed)
		}
	}

	// 回收无订阅者且无活跃 handler 的 reader（check + delete 原子化，防竞态）
	m.mu.Lock()
	for key, entry := range m.readers {
		if entry.refCnt <= 0 && entry.reader.SubCount() == 0 {
			entry.reader.Stop()
			delete(m.readers, key)
			log.Printf("[relay] 回收空 reader: %s", key)
		}
	}
	m.mu.Unlock()
}

// ActiveStreams 返回当前所有活跃流信息。
func (m *Manager) ActiveStreams() []StreamInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StreamInfo, 0, len(m.streams))
	for _, s := range m.streams {
		out = append(out, *s)
	}
	return out
}

// activeStreamCount 返回当前活跃流数量。
func (m *Manager) activeStreamCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// ServeHTTP 实现 http.Handler，处理 /rtp/ 和 /udp/ 请求。
//
// 动态换源算法（精准版）：
//  1. 检测到规则变化时，订阅新源的 reader
//  2. PAT/PMT 包无条件放行 - 播放器必须先拿到节目表才能解码
//  3. 只对视频 PID 包检测关键帧 - 避免音频包干扰
//  4. PUSI=1 时才检测 NAL - 精准定位帧起点
//  5. 检测到 IDR 或 SPS 后放行 - 确保播放器可解码
//  6. 整包放行 - 不切碎 TS 的 188 字节对齐
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mode, group, port, err := parseRequest(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	multicastAddr := fmt.Sprintf("%s:%d", group, port)

	// 按需拨号：如果 PPPoE 启用且未启动，先启动拨号
	if m.pppoe != nil && m.pppoe.IsEnabled() && !m.pppoe.IsUp() {
		log.Printf("[relay] PPPoE 未启动，尝试自动拨号...")
		if err := m.pppoe.Start(); err != nil {
			log.Printf("[relay] 自动拨号失败: %v", err)
		}
	}

	// 初始规则解析
	currentRealAddr := m.rules.Resolve(multicastAddr, time.Now())
	rulesResolvedAddr := currentRealAddr // 记录规则解析结果，用于判断是否命中规则换源

	// 连接建立时立即检查时长限制，在创建 reader 前确定最终目标地址
	// 仅当规则解析未改变地址时才检查时长限制，避免与规则换源同时生效
	var limitOverrideAddr string // 因时长限制切换到的备选地址，非空时跳过规则检查回切
	if m.limits != nil && currentRealAddr == multicastAddr {
		if result := m.limits.CheckLimit(multicastAddr, m.stats, 0, time.Now()); result != nil {
			if replacement, ok := m.limits.PickReplacement(currentRealAddr); ok {
				log.Printf("[relay] 频道 %s 已达到%s限制 (%d/%d秒)，直接使用备选源 %s",
					multicastAddr, result.LimitType, result.CurrentSec, result.MaxSec, replacement)
				currentRealAddr = replacement
				limitOverrideAddr = replacement
			} else {
				log.Printf("[relay] 频道 %s 已达到%s限制，但备选地址池为空，使用原源", multicastAddr, result.LimitType)
			}
		}
	}

	realGroup, realPort := splitAddr(currentRealAddr)

	log.Printf("[relay] %s 请求 %s → 实际 %s", r.RemoteAddr, multicastAddr, currentRealAddr)

	// 获取或创建 reader
	entry, err := m.getOrCreateReader(realGroup, realPort)
	if err != nil {
		http.Error(w, "组播加入失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { m.releaseReader(realGroup, realPort, entry) }()

	// 注册订阅。队列溢出时主动结束连接，不能继续输出已静默丢包的 TS。
	ch := make(chan mcast.Packet, 512)
	unsub, subErr := entry.reader.Subscribe(ch)
	defer func() { unsub() }()
	decoder := newInputDecoder(mode)

	// 创建流切换器，用于动态换源时精准检测关键帧
	streamSwitcher := mcast.NewStreamSwitcher()

	// 注册流信息
	streamID := m.newStreamID()
	info := &StreamInfo{
		ID:         streamID,
		RemoteAddr: r.RemoteAddr,
		OrigAddr:   multicastAddr,
		RealAddr:   currentRealAddr,
		StartTime:  time.Now(),
		InputMode:  mode.String(),
	}
	// 查询频道名称
	if m.channels != nil {
		info.OrigName = m.channels.GetName(multicastAddr)
		info.RealName = m.channels.GetName(currentRealAddr)
	}
	m.mu.Lock()
	m.streams[streamID] = info
	m.mu.Unlock()

	// 通知 PPPoE 有活跃流
	if m.pppoe != nil {
		m.pppoe.NotifyActivity()
	}

	defer func() {
		m.mu.Lock()
		delete(m.streams, streamID)
		streamCount := len(m.streams)
		m.mu.Unlock()
		log.Printf("[relay] 流结束 %s → %s, 发送 %d 字节", multicastAddr, currentRealAddr, info.BytesSent)

		// 记录观看时长
		if m.stats != nil {
			duration := time.Since(info.StartTime)
			m.stats.Add(multicastAddr, duration, info.StartTime)
		}

		// 通知 PPPoE 流结束，如果没有其他活跃流则启动空闲计时器
		if m.pppoe != nil && streamCount == 0 {
			m.pppoe.NotifyIdle(m.idleTimeout)
		}
	}()

	// 写 HTTP 响应头
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Connection", "close")
	w.Header().Set("Cache-Control", "no-cache,no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	// 无限直播流不能设置 Server.WriteTimeout；每次写入单独设置 deadline，
	// 避免客户端停止读取后 handler 永久阻塞并泄漏 reader 引用。
	responseController := http.NewResponseController(w)
	writeStream := func(data []byte) (int, error) {
		_ = responseController.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return w.Write(data)
	}

	// 流式转发，支持动态换源
	flusher, canFlush := w.(http.Flusher)
	ruleCheckTicker := time.NewTicker(3 * time.Second) // 每 3 秒检查一次规则变化
	defer ruleCheckTicker.Stop()

	// 定期 flush 替代逐包 flush，减少系统调用（约 60fps）
	flushTicker := time.NewTicker(16 * time.Millisecond)
	defer flushTicker.Stop()
	needFlush := false

	// 新源的 reader 和 channel（用于动态换源）
	var newEntry *readerEntry
	var newCh chan mcast.Packet
	var newSubErr <-chan error
	var newUnsub func()
	var newDecoder *inputDecoder
	var pendingRealAddr string // 待切换的目标地址
	var pendingGroup string
	var pendingPort int
	var switchTimeout *time.Timer // 切换超时计时器
	var limitSwitchPending bool   // 有待完成的限制触发切换

	// 切换超时时间：5秒内没有检测到关键帧则取消切换
	const switchTimeoutDuration = 5 * time.Second

	// 取消待切换的函数
	cancelPendingSwitch := func() {
		if newEntry != nil {
			log.Printf("[relay] 取消待切换到 %s", pendingRealAddr)
			newUnsub()
			m.releaseReader(pendingGroup, pendingPort, newEntry)
			newEntry = nil
			newCh = nil
			newUnsub = nil
			pendingRealAddr = ""
			pendingGroup = ""
			pendingPort = 0
		}
		if switchTimeout != nil {
			switchTimeout.Stop()
			switchTimeout = nil
		}
	}

	for {
		select {
		case pkt := <-ch:
			// /rtp/ 必须剥离 RTP 头、扩展和 padding；HTTP 中只允许写入纯 MPEG-TS。
			payload, decodeErr := decoder.Decode(pkt.Data)
			if decodeErr != nil {
				if errors.Is(decodeErr, rtp.ErrDuplicate) || errors.Is(decodeErr, rtp.ErrOutOfOrder) {
					continue
				}
				m.mu.Lock()
				info.InputErrors++
				m.mu.Unlock()
				log.Printf("[relay] %s 输入包无效: %v，结束连接以避免输出损坏的 TS", currentRealAddr, decodeErr)
				cancelPendingSwitch()
				return
			}
			output, switchErr := streamSwitcher.ProcessCurrent(payload)
			if switchErr != nil {
				log.Printf("[relay] %s MPEG-TS 处理失败: %v", currentRealAddr, switchErr)
				cancelPendingSwitch()
				return
			}
			if len(output) > 0 {
				n, err := writeStream(output)
				if err != nil {
					cancelPendingSwitch()
					return
				}
				m.mu.Lock()
				info.BytesSent += int64(n)
				m.mu.Unlock()
				needFlush = true
			}

		case pkt := <-newCh:
			payload, decodeErr := newDecoder.Decode(pkt.Data)
			if decodeErr != nil {
				if errors.Is(decodeErr, rtp.ErrDuplicate) || errors.Is(decodeErr, rtp.ErrOutOfOrder) {
					continue
				}
				log.Printf("[relay] 待切换源 %s 输入包无效: %v，取消本次切换", pendingRealAddr, decodeErr)
				cancelPendingSwitch()
				continue
			}
			// 候选源必须在真实 IDR PES 边界切换，并映射 PID、CC、PCR、PTS/DTS。
			batch, ready, switchErr := streamSwitcher.ProcessCandidate(payload)
			if switchErr != nil {
				log.Printf("[relay] 待切换源 %s 不兼容: %v，保持原源", pendingRealAddr, switchErr)
				cancelPendingSwitch()
				continue
			}
			if ready {
				log.Printf("[relay] 新源 %s IDR、节目结构和时间线已就绪，开始无缝切换", pendingRealAddr)
				if switchTimeout != nil {
					switchTimeout.Stop()
					switchTimeout = nil
				}

				// 先写入已经标准化的 PAT/PMT + IDR 批次，再转移 source 所有权。
				n, err := writeStream(batch)
				if err != nil {
					cancelPendingSwitch()
					return
				}
				m.mu.Lock()
				info.BytesSent += int64(n)
				m.mu.Unlock()
				needFlush = true
				if canFlush {
					flusher.Flush()
					needFlush = false
				}

				unsub()
				m.releaseReader(realGroup, realPort, entry)

				ch = newCh
				subErr = newSubErr
				unsub = newUnsub
				decoder = newDecoder
				entry = newEntry
				realGroup, realPort = pendingGroup, pendingPort
				currentRealAddr = pendingRealAddr

				newEntry = nil
				newCh = nil
				newSubErr = nil
				newUnsub = nil
				newDecoder = nil
				pendingRealAddr = ""
				pendingGroup = ""
				pendingPort = 0

				m.mu.Lock()
				info.RealAddr = currentRealAddr
				if m.channels != nil {
					info.RealName = m.channels.GetName(currentRealAddr)
				}
				m.mu.Unlock()
				log.Printf("[relay] 无缝换源完成: %s → %s", multicastAddr, currentRealAddr)
				// 如果是限制触发的切换，记录覆盖地址以阻止规则检查回切
				if limitSwitchPending {
					limitOverrideAddr = currentRealAddr
					limitSwitchPending = false
				}
			}

		case subErrValue := <-subErr:
			if subErrValue != nil {
				m.mu.Lock()
				info.QueueErrors++
				m.mu.Unlock()
				log.Printf("[relay] %s: %v，结束连接以避免花屏", currentRealAddr, subErrValue)
				cancelPendingSwitch()
				return
			}

		case newSubErrValue := <-newSubErr:
			if newSubErrValue != nil {
				log.Printf("[relay] 待切换源 %s: %v，取消本次切换", pendingRealAddr, newSubErrValue)
				cancelPendingSwitch()
			}

		case <-func() <-chan time.Time {
			if switchTimeout != nil {
				return switchTimeout.C
			}
			return nil
		}():
			// 切换超时：新源没有关键帧，取消切换
			log.Printf("[relay] 新源 %s 关键帧检测超时，取消切换", pendingRealAddr)
			cancelPendingSwitch()

		case <-flushTicker.C:
			// 定期 flush，减少系统调用开销
			if needFlush && canFlush {
				flusher.Flush()
				needFlush = false
			}

		case <-ruleCheckTicker.C:
			// 检查规则是否变化；等待期间目标再次变化时立即放弃旧候选源。
			newRealAddr := m.rules.Resolve(multicastAddr, time.Now())
			if pendingRealAddr != "" && newRealAddr != pendingRealAddr {
				cancelPendingSwitch()
				limitSwitchPending = false
			}
			// 如果限制规则被移除或禁用，清除限制覆盖状态
			if limitOverrideAddr != "" && m.limits != nil && !m.limits.HasLimit(multicastAddr) {
				log.Printf("[relay] 频道 %s 限制规则已移除，恢复正常规则检查", multicastAddr)
				limitOverrideAddr = ""
			}
			if newRealAddr != currentRealAddr && pendingRealAddr == "" && limitOverrideAddr == "" {
				// 检测到新的换源需求，且当前没有待处理的切换，且不在限制覆盖状态
				log.Printf("[relay] 检测到换源规则: %s: %s → %s，开始预连接新源", multicastAddr, currentRealAddr, newRealAddr)

				// 获取新源的 reader
				newGroup, newPort := splitAddr(newRealAddr)
				newEntryTmp, err := m.getOrCreateReader(newGroup, newPort)
				if err != nil {
					log.Printf("[relay] 新源 %s 连接失败: %v，保持原源", newRealAddr, err)
					continue
				}

				// 订阅新源，并为它维护独立 RTP 序列状态。
				newChTmp := make(chan mcast.Packet, 512)
				newUnsubTmp, newSubErrTmp := newEntryTmp.reader.Subscribe(newChTmp)

				// 保存待切换状态
				newEntry = newEntryTmp
				newCh = newChTmp
				newSubErr = newSubErrTmp
				newUnsub = newUnsubTmp
				newDecoder = newInputDecoder(mode)
				pendingRealAddr = newRealAddr
				pendingGroup = newGroup
				pendingPort = newPort

				// 开始等待新源的关键帧（重置所有 PID 状态）
				streamSwitcher.StartWaiting()

				// 启动超时计时器
				switchTimeout = time.NewTimer(switchTimeoutDuration)

				log.Printf("[relay] 新源 %s 已订阅，等待关键帧...", newRealAddr)
			}

			// 检查观看时长限制（仅在没有规则换源进行中、未处于限制覆盖状态、且规则未匹配时检查）
			if m.limits != nil && pendingRealAddr == "" && limitOverrideAddr == "" && rulesResolvedAddr == multicastAddr {
				liveElapsed := time.Since(info.StartTime)
				if result := m.limits.CheckLimit(multicastAddr, m.stats, liveElapsed, time.Now()); result != nil {
					replacement, ok := m.limits.PickReplacement(currentRealAddr)
					if ok {
						log.Printf("[relay] 频道 %s 达到%s限制 (%d/%d秒)，切换到备选源 %s",
							multicastAddr, result.LimitType, result.CurrentSec, result.MaxSec, replacement)

						limGroup, limPort := splitAddr(replacement)
						limEntry, err := m.getOrCreateReader(limGroup, limPort)
						if err != nil {
							log.Printf("[relay] 备选源 %s 连接失败: %v，保持原源", replacement, err)
						} else {
							limCh := make(chan mcast.Packet, 512)
							limUnsub, limSubErr := limEntry.reader.Subscribe(limCh)

							newEntry = limEntry
							newCh = limCh
							newSubErr = limSubErr
							newUnsub = limUnsub
							newDecoder = newInputDecoder(mode)
							pendingRealAddr = replacement
							pendingGroup = limGroup
							pendingPort = limPort
							limitSwitchPending = true

							streamSwitcher.StartWaiting()
							switchTimeout = time.NewTimer(switchTimeoutDuration)

							log.Printf("[relay] 备选源 %s 已订阅，等待关键帧...", replacement)
						}
					} else {
						log.Printf("[relay] 频道 %s 达到%s限制，但备选地址池为空，无法切换", multicastAddr, result.LimitType)
					}
				}
			}

		case <-r.Context().Done():
			// 客户端断开连接，清理资源
			cancelPendingSwitch()
			return
		}
	}
}

// getOrCreateReader 获取或创建组播 reader，引用计数 +1。
func (m *Manager) getOrCreateReader(group string, port int) (*readerEntry, error) {
	key := mcast.Key(group, port)
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.readers[key]; ok {
		e.refCnt++
		return e, nil
	}

	rd, err := mcast.NewReader(group, port, m.ifaceName)
	if err != nil {
		return nil, err
	}
	if err := rd.Start(); err != nil {
		return nil, err
	}
	e := &readerEntry{reader: rd, refCnt: 1}
	m.readers[key] = e
	return e, nil
}

// releaseReader 引用计数 -1，归零时停止并移除。
func (m *Manager) releaseReader(group string, port int, entry *readerEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := mcast.Key(group, port)
	entry.refCnt--
	if entry.refCnt <= 0 {
		entry.reader.Stop()
		delete(m.readers, key)
	}
}

func (m *Manager) newStreamID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	return fmt.Sprintf("s%d", m.counter)
}

// parseRequest 从 URL 路径解析组播地址。
// 支持：/rtp/239.69.1.123:10376  /udp/239.69.1.123:10376
func parseRequest(path string) (inputMode, string, int, error) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	if len(parts) != 2 {
		return 0, "", 0, fmt.Errorf("请求格式应为 /rtp/<group>:<port> 或 /udp/<group>:<port>")
	}
	var mode inputMode
	switch strings.ToLower(parts[0]) {
	case "rtp":
		mode = modeRTP
	case "udp":
		mode = modeUDP
	default:
		return 0, "", 0, fmt.Errorf("路径前缀应为 rtp 或 udp")
	}
	host, port := splitAddr(parts[1])
	if net.ParseIP(host) == nil || port <= 0 || port > 65535 {
		return 0, "", 0, fmt.Errorf("无效的组播地址或端口: %s", parts[1])
	}
	return mode, host, port, nil
}

func splitAddr(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// Ensure Manager implements http.Handler at compile time.
var _ http.Handler = (*Manager)(nil)
