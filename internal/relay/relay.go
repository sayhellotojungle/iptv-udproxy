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
	"sort"
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
	// 达到观看限制但备选池为空、无法切换的频道（家长控制"静默失效"显式化）
	limitBlocked map[string]time.Time
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

// Decode 校验一个输入 datagram，返回按序可输出的 TS 负载批
// （RTP 模式下可能为空=被重排缓冲，或多个=补上了重排间隙）。
func (d *inputDecoder) Decode(datagram []byte) ([][]byte, error) {
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
	return [][]byte{datagram}, nil
}

// New 创建 Manager。
func New(ifaceName string, ruleStore *rules.Store) *Manager {
	return &Manager{
		ifaceName:    ifaceName,
		rules:        ruleStore,
		readers:      make(map[string]*readerEntry),
		streams:      make(map[string]*StreamInfo),
		limitBlocked: make(map[string]time.Time),
		idleTimeout:  5 * time.Minute, // 默认 5 分钟空闲断开
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idleTimeout = d
}

// setLimitBlocked 标记某频道处于"达到观看限制但备选池为空、无法切换"状态。
// 该状态会通过 /api/status 暴露到控制台，避免家长控制静默失效。
func (m *Manager) setLimitBlocked(addr string, blocked bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if blocked {
		if _, exists := m.limitBlocked[addr]; !exists {
			log.Printf("[relay] 警告: 频道 %s 已达到观看限制，但备选地址池为空，无法切换（请到控制台“时长限制”页配置备选池）", addr)
		}
		m.limitBlocked[addr] = time.Now()
	} else {
		delete(m.limitBlocked, addr)
	}
}

// LimitBlocked 返回当前"受限但无法切换"的频道地址列表（排序稳定，便于展示）。
func (m *Manager) LimitBlocked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.limitBlocked))
	for addr := range m.limitBlocked {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

// RecSource 录制等内部消费者使用的组播数据源。
type RecSource struct {
	Packets <-chan mcast.Packet
	Release func()
	ErrCh   <-chan error // 订阅溢出（MPEG-TS 静默丢包）时收到 ErrSubscriberOverflow
}

// AcquireSource 为非 HTTP 消费者（录制）打开一个组播源。
// 与观看流共享同一 refcount reader（避免同地址二次绑组播端口），
// 同样应用换源规则；rtp 仅表示期望的输入封装，实际解码由消费方完成。
func (m *Manager) AcquireSource(addr string, rtp bool) (RecSource, error) {
	// 按需拨号：与观看流一致。否则线路未拨时录制开了组播组却收不到包，只落 0 字节文件
	if m.pppoe != nil && m.pppoe.IsEnabled() && !m.pppoe.IsUp() {
		log.Printf("[relay] 录制: PPPoE 未启动，尝试自动拨号...")
		if err := m.pppoe.Start(); err != nil {
			log.Printf("[relay] 录制: PPPoE 自动拨号失败: %v（录制继续，可在网页重试）", err)
		}
	}

	// 录制跟随换源规则：若规则把 A 换成 B，录到的就是 B 的内容
	real := m.rules.Resolve(addr, time.Now())
	group, port := splitAddr(real)

	entry, err := m.getOrCreateReader(group, port)
	if err != nil {
		return RecSource{}, err
	}
	ch := make(chan mcast.Packet, 2048)
	unsub, subErr := entry.reader.Subscribe(ch)
	if m.pppoe != nil {
		m.pppoe.NotifyActivity()
	}
	return RecSource{
		Packets: ch,
		ErrCh:   subErr,
		Release: func() {
			unsub()
			m.releaseReader(group, port, entry)
			// 已无任何消费者（观看流或录制订阅）时启动空闲断线计时
			if m.pppoe != nil {
				if idle, to := m.idleIfConsumersGone(); idle {
					m.pppoe.NotifyIdle(to)
				}
			}
		},
	}, nil
}

// idleIfConsumersGone 当前是否已无任何流消费者（无活跃观看流，
// 且所有 reader 均无订阅者）；是则返回空闲超时。
func (m *Manager) idleIfConsumersGone() (bool, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.streams) > 0 {
		return false, 0
	}
	for _, e := range m.readers {
		if e.reader.SubCount() > 0 {
			return false, 0
		}
	}
	return true, m.idleTimeout
}

// StartCleanup 启动后台清理协程：每 30 秒回收一次无订阅者的空 reader。
// 注意：慢订阅/幽灵订阅由 reader 在广播溢出时即时移除（见 mcast.Reader.broadcast），
// 这里不再做延迟清理。ctx 取消时退出。
func (m *Manager) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sweepEmptyReaders()
			}
		}
	}()
}

// sweepEmptyReaders 回收无订阅者且无活跃 handler 的 reader（check + delete 原子化，防竞态）。
func (m *Manager) sweepEmptyReaders() {
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
					multicastAddr, result.LimitDesc(), result.CurrentSec, result.MaxSec, replacement)
				currentRealAddr = replacement
				limitOverrideAddr = replacement
				m.setLimitBlocked(multicastAddr, false)
			} else {
				log.Printf("[relay] 频道 %s 已达到%s限制，但备选地址池为空，使用原源", multicastAddr, result.LimitDesc())
				m.setLimitBlocked(multicastAddr, true)
			}
		} else {
			m.setLimitBlocked(multicastAddr, false)
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
		idleTimeout := m.idleTimeout
		m.mu.Unlock()
		log.Printf("[relay] 流结束 %s → %s, 发送 %d 字节", multicastAddr, currentRealAddr, info.BytesSent)

		// 记录观看时长（跨天流按自然日分段归属）
		if m.stats != nil {
			m.stats.AddRange(multicastAddr, info.StartTime, time.Now())
		}
		// 记录会话结束，供连续观看限制判定（间隔≤休息时长视为连续）
		if m.limits != nil {
			m.limits.EndSession(multicastAddr, info.StartTime, time.Now())
		}

		// 通知 PPPoE 流结束，如果没有其他活跃流则启动空闲计时器
		if m.pppoe != nil && streamCount == 0 {
			m.pppoe.NotifyIdle(idleTimeout)
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
	var switchTimeout *time.Timer       // 切换超时计时器
	var limitSwitchPending bool         // 有待完成的限制触发切换
	var ruleSwitchSuppressedLogged bool // 限制覆盖期间是否已提示过规则换源被挂起

	// 切换超时时间：5秒内没有检测到关键帧则取消切换
	const switchTimeoutDuration = 5 * time.Second
	// 连续无效包容忍阈值：IPTV 链路瞬态毛刺常见，单个包损坏不应直接断流
	const maxConsecutiveBadPkts = 10
	badPkts := 0 // 连续无效包计数，收到有效包归零

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
		limitSwitchPending = false
		// 释放候选源已缓冲的 TS 包（长 GOP 下可能达数 MB），仅连接断开才会释放之前是泄漏
		streamSwitcher.AbortCandidate()
	}

	for {
		select {
		case pkt := <-ch:
			// /rtp/ 必须剥离 RTP 头、扩展和 padding；HTTP 中只允许写入纯 MPEG-TS。
			payloads, decodeErr := decoder.Decode(pkt.Data)
			if decodeErr != nil {
				if errors.Is(decodeErr, rtp.ErrDuplicate) || errors.Is(decodeErr, rtp.ErrOutOfOrder) {
					continue
				}
				// 单个包损坏（链路瞬态毛刺）不断流：跳过该包继续，
				// 仅当连续达到阈值的包都无效时才认为流已损坏并断开。
				badPkts++
				m.mu.Lock()
				info.InputErrors++
				m.mu.Unlock()
				if badPkts == 1 || badPkts%10 == 0 {
					log.Printf("[relay] %s 输入包无效（连续 %d 个）: %v", currentRealAddr, badPkts, decodeErr)
				}
				if badPkts >= maxConsecutiveBadPkts {
					log.Printf("[relay] %s 连续 %d 个输入包无效，结束连接以避免输出损坏的 TS", currentRealAddr, badPkts)
					cancelPendingSwitch()
					return
				}
				continue
			}
			badPkts = 0
			for _, payload := range payloads {
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
			}

		case pkt := <-newCh:
			payloads, decodeErr := newDecoder.Decode(pkt.Data)
			if decodeErr != nil {
				if errors.Is(decodeErr, rtp.ErrDuplicate) || errors.Is(decodeErr, rtp.ErrOutOfOrder) {
					continue
				}
				log.Printf("[relay] 待切换源 %s 输入包无效: %v，取消本次切换", pendingRealAddr, decodeErr)
				cancelPendingSwitch()
				continue
			}
			// 候选源必须在真实 IDR PES 边界切换，并映射 PID、CC、PCR、PTS/DTS。
			// 一个 datagram 经重排后可能带出多个 TS 负载：ready 之前的喂给候选状态机，
			// ready 之后的已属于激活的新源，改走 ProcessCurrent。
			for i, payload := range payloads {
				batch, ready, switchErr := streamSwitcher.ProcessCandidate(payload)
				if switchErr != nil {
					log.Printf("[relay] 待切换源 %s 不兼容: %v，保持原源", pendingRealAddr, switchErr)
					cancelPendingSwitch()
					break
				}
				if !ready {
					continue
				}

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

				// 同一重排批中 ready 之后的负载已属于新源，用激活后状态继续处理
				for _, rest := range payloads[i+1:] {
					restOut, restErr := streamSwitcher.ProcessCurrent(rest)
					if restErr != nil {
						log.Printf("[relay] 新源 %s MPEG-TS 处理失败: %v", currentRealAddr, restErr)
						cancelPendingSwitch()
						return
					}
					if len(restOut) > 0 {
						restN, restWriteErr := writeStream(restOut)
						if restWriteErr != nil {
							cancelPendingSwitch()
							return
						}
						m.mu.Lock()
						info.BytesSent += int64(restN)
						m.mu.Unlock()
						needFlush = true
					}
				}
				break
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
				m.setLimitBlocked(multicastAddr, false)
			}
			if newRealAddr != currentRealAddr && pendingRealAddr == "" {
				if limitOverrideAddr != "" {
					// 时长限制覆盖期间，规则换源被挂起：限制优先级更高，
					// 直到限制规则被删除/禁用或覆盖解除后规则才重新生效。
					if !ruleSwitchSuppressedLogged {
						ruleSwitchSuppressedLogged = true
						log.Printf("[relay] 频道 %s 处于时长限制覆盖状态，规则换源 %s → %s 暂不生效", multicastAddr, currentRealAddr, newRealAddr)
					}
				} else {
					// 检测到新的换源需求，且当前没有待处理的切换，且不在限制覆盖状态
					log.Printf("[relay] 检测到换源规则: %s: %s → %s，开始预连接新源", multicastAddr, currentRealAddr, newRealAddr)

					// 获取新源的 reader
					newGroup, newPort := splitAddr(newRealAddr)
					newEntryTmp, err := m.getOrCreateReader(newGroup, newPort)
					if err != nil {
						log.Printf("[relay] 新源 %s 连接失败: %v，保持原源", newRealAddr, err)
					} else {
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
				}
			}

			// 检查观看时长限制（仅在没有规则换源进行中、未处于限制覆盖状态、且规则未匹配时检查）
			if m.limits != nil && pendingRealAddr == "" && limitOverrideAddr == "" && rulesResolvedAddr == multicastAddr {
				liveElapsed := time.Since(info.StartTime)
				if result := m.limits.CheckLimit(multicastAddr, m.stats, liveElapsed, time.Now()); result != nil {
					replacement, ok := m.limits.PickReplacement(currentRealAddr)
					if ok {
						log.Printf("[relay] 频道 %s 达到%s限制 (%d/%d秒)，切换到备选源 %s",
							multicastAddr, result.LimitDesc(), result.CurrentSec, result.MaxSec, replacement)

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
							m.setLimitBlocked(multicastAddr, false)
						}
					} else {
						log.Printf("[relay] 频道 %s 达到%s限制，但备选地址池为空，无法切换", multicastAddr, result.LimitDesc())
						m.setLimitBlocked(multicastAddr, true)
					}
				} else {
					m.setLimitBlocked(multicastAddr, false)
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
	ip := net.ParseIP(host)
	if ip == nil || port <= 0 || port > 65535 {
		return 0, "", 0, fmt.Errorf("无效的组播地址或端口: %s", parts[1])
	}
	if !ip.IsMulticast() {
		return 0, "", 0, fmt.Errorf("仅支持组播地址（224.0.0.0/4）: %s", host)
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
