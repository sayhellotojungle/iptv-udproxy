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
	mcastIface  string // IPTV 组播口
	pppIface    string // ppp 接口名
	rpFilterFix bool   // 是否修正 rp_filter
	blockPPPoE  bool   // 是否阻止 ppp 接口入站流量
	done        chan struct{}
}

// New 创建路由守卫。
func New(mcastIface, pppIface string, rpFilterFix bool) *Guard {
	return &Guard{
		mcastIface:  mcastIface,
		pppIface:    pppIface,
		rpFilterFix: rpFilterFix,
		done:        make(chan struct{}),
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

// Start 启动周期性路由检查。
func (g *Guard) Start(interval time.Duration) {
	go g.loop(interval)
	log.Printf("[guard] 路由守卫已启动，检查间隔 %s", interval)
}

// Stop 停止守卫。
func (g *Guard) Stop() {
	close(g.done)
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
	}
}

func (g *Guard) loop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-g.done:
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
// 注意：这需要容器有修改 sysctl 的权限（需要挂载 /proc/sys 或使用 --privileged）
func (g *Guard) fixRPFilter(mcastIface string) {
	if mcastIface == "" {
		return
	}

	// 只设置指定的组播口
	path := "/proc/sys/net/ipv4/conf/" + mcastIface + "/rp_filter"
	current := readFileTrim(path)
	if current != "0" && current != "2" {
		log.Printf("[guard] 设置 %s = 2 (当前 %s)", path, current)
		if err := exec.Command("sysctl", "-w",
			"net.ipv4.conf."+mcastIface+".rp_filter=2").Run(); err != nil {
			log.Printf("[guard] 设置 rp_filter 失败: %v (需要在宿主机上设置)", err)
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
		log.Printf("[guard] 添加 iptables 规则失败: %v (需要容器有 NET_ADMIN 权限)", err)
	}
}

func readFileTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
