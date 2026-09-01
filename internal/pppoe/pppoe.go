// Package pppoe 管理 pppd 子进程生命周期。
package pppoe

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config PPPoE 配置。
type Config struct {
	Iface string // 拨号使用的物理网口
	User  string
	Pass  string
	Unit  int    // pppd unit 号，接口名为 ppp<Unit>
	Extra string // 追加给 pppd 的额外参数
}

// State 拨号状态。
type State string

const (
	StateIdle       State = "idle"
	StateConnecting State = "connecting"
	StateUp         State = "up"
	StateError      State = "error"
)

// Status PPPoE 当前状态。
type Status struct {
	State   State  `json:"state"`
	IP      string `json:"ip,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	Error   string `json:"error,omitempty"`
	Uptime  string `json:"uptime,omitempty"`
	Unit    int    `json:"unit"`
}

// Manager 管理 pppd 子进程。
type Manager struct {
	cfg     Config
	dataDir string

	mu         sync.Mutex
	status     Status
	cmd        *exec.Cmd
	starting   bool // 启动中占位：防止并发 Start 同时拉起两个 pppd
	startAt    time.Time
	stopCh     chan struct{}  // 用于通知 waitLoop 退出
	stopped    bool           // 标记是否主动停止
	onDemand   bool           // 按需拨号模式
	enabled    bool           // PPPoE 是否启用
	idleTimer  *time.Timer    // 空闲断开定时器
	waitWg     sync.WaitGroup // 跟踪 waitLoop goroutine
	generation uint64         // 每次 Start 递增，防止旧 waitLoop 覆盖状态

	autoReconnect   bool          // pppd 意外退出后自动重连
	reconnectDelay  time.Duration // 重连初始延迟
	reconnectMax    time.Duration // 重连最大延迟
	reconnectCancel chan struct{} // 取消重连
}

// New 创建 Manager，拨号未启动。
func New(cfg Config, dataDir string) *Manager {
	return &Manager{
		cfg:            cfg,
		dataDir:        dataDir,
		status:         Status{State: StateIdle, Unit: cfg.Unit},
		stopCh:         make(chan struct{}),
		enabled:        true, // 默认启用，由调用方根据设置决定
		autoReconnect:  true,
		reconnectDelay: 5 * time.Second,
		reconnectMax:   2 * time.Minute,
	}
}

// IfaceName 返回拨号接口名。
func (m *Manager) IfaceName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("ppp%d", m.cfg.Unit)
}

// Status 返回当前拨号状态。
func (m *Manager) Status() Status {
	m.mu.Lock()
	s := m.status
	isUp := s.State == StateUp
	startAt := m.startAt
	m.mu.Unlock()

	if isUp {
		s.Uptime = time.Since(startAt).Round(time.Second).String()
		s.IP = m.getIP()
	}
	return s
}

// Start 启动拨号，阻塞直到 pppd 初始化完成（最多 15 秒）。
// "启动中"占位在锁内完成，并发调用（手动重试 + 按需自动拨号 + 重连）
// 只会拉起一个 pppd。
func (m *Manager) Start() error {
	m.mu.Lock()
	if m.cmd != nil || m.starting {
		m.mu.Unlock()
		return fmt.Errorf("拨号进程已在运行或启动中")
	}
	m.starting = true
	m.generation++
	gen := m.generation
	m.status = Status{State: StateConnecting, Unit: m.cfg.Unit}
	m.stopped = false
	m.stopCh = make(chan struct{})
	// 取消正在进行的重连
	if m.reconnectCancel != nil {
		close(m.reconnectCancel)
		m.reconnectCancel = nil
	}
	cfg := m.cfg // 锁内取快照，之后写文件不再与 UpdateConfig 竞争
	m.mu.Unlock()

	fail := func(msg string) error {
		m.mu.Lock()
		m.starting = false
		m.mu.Unlock()
		m.setStateError(msg)
		return fmt.Errorf("%s", msg)
	}

	// 写入 pap-secrets / chap-secrets
	if err := m.writeSecrets(cfg); err != nil {
		return fail("写入认证文件失败: " + err.Error())
	}
	// 写入拨号配置
	if err := m.writeOptions(cfg); err != nil {
		return fail("写入拨号配置失败: " + err.Error())
	}

	// 启动 pppd
	cmd := exec.Command("pppd", "call", "iptv")
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	setSysProcAttr(cmd.SysProcAttr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 10 * time.Second // pppd 挂死不退时 10 秒后强制结束 Wait

	if err := cmd.Start(); err != nil {
		return fail("启动 pppd 失败: " + err.Error())
	}

	m.mu.Lock()
	if m.stopped {
		// Stop() 在启动窗口内被调用：不登记进程，直接杀掉
		m.starting = false
		m.mu.Unlock()
		if cmd.Process != nil {
			_ = killProcess(cmd.Process.Pid, syscall.SIGTERM)
		}
		return fmt.Errorf("拨号过程中被停止")
	}
	m.cmd = cmd
	m.starting = false
	m.startAt = time.Now()
	m.mu.Unlock()

	m.waitWg.Add(1)
	go m.waitLoop(cmd, gen)

	// 等待 ppp 接口就绪
	if err := m.waitForIface(15 * time.Second); err != nil {
		m.Stop()
		return err
	}

	// 设置 Up 状态前检查：如果期间发生了错误或被停止，不覆盖
	m.mu.Lock()
	if m.stopped || m.status.State == StateError {
		m.mu.Unlock()
		return fmt.Errorf("拨号过程中被中断")
	}
	m.status = Status{State: StateUp, Unit: m.cfg.Unit}
	m.mu.Unlock()

	log.Printf("[pppoe] 拨号成功，接口 %s 已就绪", m.IfaceName())
	return nil
}

// Stop 停止拨号。
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	cmd := m.cmd
	m.cmd = nil
	m.status = Status{State: StateIdle, Unit: m.cfg.Unit}
	close(m.stopCh)
	m.stopCh = make(chan struct{})
	// 取消正在进行的重连
	if m.reconnectCancel != nil {
		close(m.reconnectCancel)
		m.reconnectCancel = nil
	}
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = killProcess(cmd.Process.Pid, syscall.SIGTERM)
	}

	// 等待 waitLoop 确认进程已退出，确保 Stop() 返回后可以安全地重新 Start()
	m.waitWg.Wait()
}

// Retry 重新拨号（先停止再启动）。
func (m *Manager) Retry() error {
	m.Stop()
	// Stop() 已等待 waitLoop 完成，无需额外 sleep
	return m.Start()
}

// SetOnDemand 设置按需拨号模式。
func (m *Manager) SetOnDemand(enable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDemand = enable
}

// SetEnabled 设置是否启用 PPPoE。
func (m *Manager) SetEnabled(enable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = enable
}

// IsEnabled 返回 PPPoE 是否启用。
func (m *Manager) IsEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

// SetAutoReconnect 设置是否启用自动重连。
func (m *Manager) SetAutoReconnect(enable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoReconnect = enable
}

// NotifyActivity 通知有活跃流，取消空闲断开定时器。
func (m *Manager) NotifyActivity() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
}

// NotifyIdle 通知没有活跃流，启动空闲断开定时器。
func (m *Manager) NotifyIdle(idleTimeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.onDemand {
		return
	}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(idleTimeout, func() {
		log.Printf("[pppoe] 空闲超时，断开拨号")
		m.Stop()
	})
}

// IsUp 返回是否在线。
func (m *Manager) IsUp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil && m.status.State == StateUp
}

// UpdateConfig 更新配置（下次拨号生效）。
func (m *Manager) UpdateConfig(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	m.status.Unit = cfg.Unit
}

// GetConfig 获取当前配置。
func (m *Manager) GetConfig() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// waitLoop 等待 pppd 进程退出。gen 是启动时的 generation，用于防止旧 goroutine 覆盖新状态。
func (m *Manager) waitLoop(cmd *exec.Cmd, gen uint64) {
	defer m.waitWg.Done()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	m.mu.Lock()
	stopCh := m.stopCh
	m.mu.Unlock()

	select {
	case err := <-done:
		// pppd 自然退出
		m.mu.Lock()
		stopped := m.stopped
		currentGen := m.generation
		onDemand := m.onDemand
		if gen == currentGen && m.cmd == cmd {
			// 对应进程已退出，清空登记，允许重新 Start
			m.cmd = nil
		}
		m.mu.Unlock()

		if stopped {
			// 主动停止，不处理
			return
		}
		if gen != currentGen {
			// 旧的 waitLoop，不应覆盖新状态
			return
		}

		if onDemand {
			// 按需拨号：回 Idle 等下次流请求再拨，不卡 Error 状态
			m.mu.Lock()
			m.status = Status{State: StateIdle, Unit: m.cfg.Unit}
			m.mu.Unlock()
			log.Printf("[pppoe] pppd 已退出（按需模式），等待下次请求")
			return
		}

		// 非主动停止，异常退出
		msg := "pppd 异常退出"
		if err != nil {
			msg = "pppd 退出: " + err.Error()
		}
		m.setStateError(msg)

		// 自动重连
		m.mu.Lock()
		shouldReconnect := m.autoReconnect
		m.mu.Unlock()
		if shouldReconnect {
			m.scheduleReconnect()
		}

	case <-stopCh:
		// 收到停止信号，等待进程退出
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = killProcess(cmd.Process.Pid, syscall.SIGKILL)
			}
			<-done
		}
	}
}

// scheduleReconnect 安排自动重连，使用指数退避。
func (m *Manager) scheduleReconnect() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	cancel := make(chan struct{})
	m.reconnectCancel = cancel
	delay := m.reconnectDelay
	m.mu.Unlock()

	log.Printf("[pppoe] 将在 %s 后尝试自动重连", delay)

	go func() {
		select {
		case <-time.After(delay):
		case <-cancel:
			return
		}

		log.Printf("[pppoe] 开始自动重连...")
		if err := m.Start(); err != nil {
			log.Printf("[pppoe] 自动重连失败: %v", err)
			// 退避重试
			m.mu.Lock()
			nextDelay := delay * 2
			if nextDelay > m.reconnectMax {
				nextDelay = m.reconnectMax
			}
			m.reconnectDelay = nextDelay
			m.mu.Unlock()

			m.scheduleReconnect()
		} else {
			// 重连成功，重置退避
			m.mu.Lock()
			m.reconnectDelay = 5 * time.Second
			m.mu.Unlock()
		}
	}()
}

func (m *Manager) setStateError(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = Status{State: StateError, Error: msg, Unit: m.cfg.Unit}
	log.Printf("[pppoe] 错误: %s", msg)
}

// waitForIface 等待 ppp 接口出现。
func (m *Manager) waitForIface(timeout time.Duration) error {
	iface := m.IfaceName()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := os.Stat("/sys/class/net/" + iface)
		if err == nil {
			time.Sleep(time.Second)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("等待接口 %s 超时", iface)
}

// escapePPP 转义 pppd 配置引号串中的特殊字符（引号/反斜杠/换行）。
func escapePPP(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

// writeSecrets 写入 /etc/ppp/pap-secrets 和 chap-secrets。
// cfg 必须是 Start 锁内取的快照，避免与 UpdateConfig 数据竞争。
func (m *Manager) writeSecrets(cfg Config) error {
	line := fmt.Sprintf("\"%s\" * \"%s\" *\n", escapePPP(cfg.User), escapePPP(cfg.Pass))
	for _, f := range []string{"/etc/ppp/pap-secrets", "/etc/ppp/chap-secrets"} {
		if err := os.WriteFile(f, []byte(line), 0o600); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", f, err)
		}
	}
	return nil
}

// writeOptions 写入 pppd peer 配置文件。
// cfg 必须是 Start 锁内取的快照，避免与 UpdateConfig 数据竞争。
func (m *Manager) writeOptions(cfg Config) error {
	peersDir := "/etc/ppp/peers"
	_ = os.MkdirAll(peersDir, 0o700)

	opts := []string{
		"plugin pppoe.so " + cfg.Iface, // 绑定物理网口
		"noauth",
		"user \"" + escapePPP(cfg.User) + "\"",
		"password \"" + escapePPP(cfg.Pass) + "\"",
		fmt.Sprintf("unit %d", cfg.Unit),
		"nodefaultroute", // 明确禁止 pppd 添加默认路由，避免影响系统网络
		"noipdefault",
		"usepeerdns",
		"nodetach",
		"holdoff 10",
		"maxfail 0",
		"lcp-echo-interval 15",
		"lcp-echo-failure 3",
		"mtu 1492",
		"mru 1492",
	}
	if cfg.Extra != "" {
		opts = append(opts, cfg.Extra)
	}

	content := strings.Join(opts, "\n") + "\n"
	path := filepath.Join(peersDir, "iptv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	log.Printf("[pppoe] 已写入拨号配置 %s", path)
	return nil
}

// getIP 返回当前 ppp 接口的 IPv4 地址（不持锁）。
func (m *Manager) getIP() string {
	iface := m.IfaceName()
	out, err := exec.Command("ip", "-4", "addr", "show", iface).CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "inet ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1]
			}
		}
	}
	return ""
}
