// Package routeguard 周期性检查并修正系统路由表。
//
// 核心职责：
// 1. 删除 ppp 接口上的默认路由（确保默认路由始终走内网口）。
// 2. 修正组播口的 rp_filter 设置（需要设为 2 或 0）。
package routeguard

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Guard 路由守卫。
type Guard struct {
	mu          sync.RWMutex
	mcastIface  string        // IPTV 组播口
	pppIface    string        // ppp 接口名
	rpFilterFix bool          // 是否修正 rp_filter
	blockPPPoE  bool          // 是否阻止 ppp 接口入站流量
	done        chan struct{} // nil = 未运行
	warnedAt    time.Time     // rp_filter 写失败的日志抑制窗口
}

// New 创建路由守卫。
func New(mcastIface, pppIface string, rpFilterFix bool) *Guard {
	return &Guard{
		mcastIface:  mcastIface,
		pppIface:    pppIface,
		rpFilterFix: rpFilterFix,
	}
}

// UpdateConfig 更新网口配置。
func (g *Guard) UpdateConfig(mcastIface, pppIface string, rpFilterFix bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mcastIface = mcastIface
	g.pppIface = pppIface
	g.rpFilterFix = rpFilterFix
	log.Printf("[guard] 配置已更新: mcast=%s, ppp=%s, rpFilterFix=%v", mcastIface, pppIface, rpFilterFix)
}

// SetBlockPPPoE 设置是否阻止 ppp 接口入站流量。
func (g *Guard) SetBlockPPPoE(block bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blockPPPoE = block
	log.Printf("[guard] ppp 接口入站阻止: %v", block)
}

// Start 启动周期性路由检查。重复调用无副作用（已在运行则忽略）。
func (g *Guard) Start(interval time.Duration) {
	g.mu.Lock()
	if g.done != nil {
		g.mu.Unlock()
		return
	}
	done := make(chan struct{})
	g.done = done
	g.mu.Unlock()
	go g.loop(interval, done)
	log.Printf("[guard] 路由守卫已启动，检查间隔 %s", interval)
}

// Stop 停止守卫。幂等：重复调用安全；停止后可再次 Start。
func (g *Guard) Stop() {
	g.mu.Lock()
	done := g.done
	g.done = nil
	g.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// RunOnce 执行一次路由检查与修正。
func (g *Guard) RunOnce() {
	g.mu.RLock()
	mcastIface := g.mcastIface
	pppIface := g.pppIface
	rpFilterFix := g.rpFilterFix
	blockPPPoE := g.blockPPPoE
	g.mu.RUnlock()

	g.fixDefaultRoute(pppIface)
	if rpFilterFix {
		g.fixRPFilter(mcastIface)
	}
	if blockPPPoE && pppIface != "" {
		g.blockPPPoEInput(pppIface)
	} else if !blockPPPoE && pppIface != "" {
		// 关闭阻止后撤销我们添加的 DROP 规则（iptables 规则跨进程重启留存）
		g.unblockPPPoEInput(pppIface)
	}
}

func (g *Guard) loop(interval time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			g.RunOnce()
		}
	}
}

// fixDefaultRoute 删除 ppp 接口上的默认路由。
func (g *Guard) fixDefaultRoute(pppIface string) {
	if pppIface == "" {
		return
	}
	// 列出所有默认路由
	out, err := exec.Command("ip", "route", "show", "default").CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 精确匹配 ppp 接口名
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) && fields[i+1] == pppIface {
				log.Printf("[guard] 删除 ppp 默认路由: %s", line)
				args := append([]string{"route", "del"}, fields...)
				_ = exec.Command("ip", args...).Run()
				break
			}
		}
	}
}

// fixRPFilter 将组播口的 rp_filter 设为 2（loose mode）。
// 注意：容器内 /proc/sys 只读，rp_filter 必须在宿主机上持久设置，
// 本函数只是尽力而为 + 失败告警（每 60 秒至多一条，避免刷屏）。
func (g *Guard) fixRPFilter(mcastIface string) {
	if mcastIface == "" {
		return
	}
	// 网口不存在（尚未 up 或已被改名）时静默跳过，等下个周期
	if _, err := os.Stat("/sys/class/net/" + mcastIface); err != nil {
		return
	}

	// 只设置指定的组播口
	path := "/proc/sys/net/ipv4/conf/" + mcastIface + "/rp_filter"
	current := readFileTrim(path)
	if current == "" || current == "0" || current == "2" {
		return
	}
	log.Printf("[guard] 设置 %s = 2 (当前 %s)", path, current)
	if err := exec.Command("sysctl", "-w",
		"net.ipv4.conf."+mcastIface+".rp_filter=2").Run(); err != nil {
		g.mu.Lock()
		if time.Since(g.warnedAt) > 60*time.Second {
			g.warnedAt = time.Now()
			g.mu.Unlock()
			log.Printf("[guard] 设置 rp_filter 失败: %v (需要在宿主机上设置)", err)
		} else {
			g.mu.Unlock()
		}
	}
}

// blockPPPoEInput 在 ppp 接口上添加 iptables 规则阻止所有入站流量。
func (g *Guard) blockPPPoEInput(pppIface string) {
	// 检查是否已有规则（避免重复添加）
	checkCmd := exec.Command("iptables", "-C", "INPUT", "-i", pppIface, "-j", "DROP")
	if checkCmd.Run() == nil {
		return // 规则已存在
	}

	// 添加规则阻止 ppp 接口的所有入站流量
	log.Printf("[guard] 添加 iptables 规则阻止 %s 接口入站流量", pppIface)
	if err := exec.Command("iptables", "-A", "INPUT", "-i", pppIface, "-j", "DROP").Run(); err != nil {
		g.iptablesWarn(err)
	}
}

// unblockPPPoEInput 移除 ppp 接口的入站阻止规则（仅在规则存在时移除）。
func (g *Guard) unblockPPPoEInput(pppIface string) {
	cmd := exec.Command("iptables", "-C", "INPUT", "-i", pppIface, "-j", "DROP")
	if cmd.Run() != nil {
		return // 规则不存在，无事可做
	}
	log.Printf("[guard] 移除 iptables 规则（停止阻止 %s 接口入站流量）", pppIface)
	if err := exec.Command("iptables", "-D", "INPUT", "-i", pppIface, "-j", "DROP").Run(); err != nil {
		g.iptablesWarn(err)
	}
}

// iptablesWarn 记录 iptables 失败日志（60 秒抑制窗口，避免周期任务刷屏）。
func (g *Guard) iptablesWarn(err error) {
	g.mu.Lock()
	if time.Since(g.warnedAt) > 60*time.Second {
		g.warnedAt = time.Now()
		g.mu.Unlock()
		log.Printf("[guard] iptables 操作失败: %v (需要容器有 NET_ADMIN 权限)", err)
	} else {
		g.mu.Unlock()
	}
}

func readFileTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
