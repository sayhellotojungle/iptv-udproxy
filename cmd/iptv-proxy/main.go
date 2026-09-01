// IPTV UDProxy —— 组播转单播 + PPPoE 拨号 + 动态换源。
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"iptv-udpproxy/internal/api"
	"iptv-udpproxy/internal/channels"
	"iptv-udpproxy/internal/config"
	"iptv-udpproxy/internal/limits"
	"iptv-udpproxy/internal/pppoe"
	"iptv-udpproxy/internal/relay"
	"iptv-udpproxy/internal/routeguard"
	"iptv-udpproxy/internal/rules"
	"iptv-udpproxy/internal/settings"
	"iptv-udpproxy/internal/stats"
)

var version = "1.2"

// fallbackMcastIface 未配置组播网口时的兜底值（应通过 Web 界面配置，非通用默认）。
const fallbackMcastIface = "enp2s0-ovs"

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[iptv] ")

	cfg := config.FromEnv()
	log.Printf("启动 IPTV UDProxy %s", version)
	log.Printf("  监听地址: %s", cfg.Listen)
	log.Printf("  数据目录: %s", cfg.DataDir)

	// 初始化设置存储
	settingsPath := cfg.DataDir + "/settings.json"
	settingsStore := settings.New(settingsPath)
	if err := settingsStore.Load(); err != nil {
		log.Printf("[main] 加载设置文件失败: %v，使用默认配置", err)
	}

	// 获取当前设置
	appSettings := settingsStore.Get()

	// 初始化规则存储
	rulePath := cfg.DataDir + "/rules.json"
	ruleStore, err := rules.Open(rulePath)
	if err != nil {
		log.Fatalf("加载规则文件失败: %v", err)
	}
	log.Printf("  已加载 %d 条换源规则", len(ruleStore.List()))

	// 初始化频道存储
	channelPath := cfg.DataDir + "/channels.json"
	channelStore, err := channels.Open(channelPath)
	if err != nil {
		log.Fatalf("加载频道文件失败: %v", err)
	}
	log.Printf("  已加载 %d 个频道", len(channelStore.List()))

	// 初始化观看时长统计存储（损坏时已备份并以空统计起步）
	statsPath := cfg.DataDir + "/watchtime.json"
	statsStore, err := stats.Open(statsPath)
	if err != nil {
		log.Fatalf("加载观看时长文件失败: %v", err)
	}
	defer statsStore.Close() // 末次落盘，保证退出不丢已聚合数据

	// 初始化限制规则存储
	limitsPath := cfg.DataDir + "/limits.json"
	limitsStore, err := limits.Open(limitsPath)
	if err != nil {
		log.Fatalf("加载限制规则文件失败: %v", err)
	}
	log.Printf("  已加载 %d 条限制规则", len(limitsStore.ListLimits()))

	// 初始化 PPPoE
	pppoeCfg := pppoe.Config{
		Iface: appSettings.PPPoEIface,
		User:  appSettings.PPPoEUser,
		Pass:  appSettings.PPPoEPass,
		Unit:  appSettings.PPPoEUnit,
	}
	pppoeMgr := pppoe.New(pppoeCfg, cfg.DataDir)
	defer pppoeMgr.Stop() // 确保任何退出路径（含 HTTP 致命错误）都拆除 pppd/系统状态
	pppoeMgr.SetEnabled(appSettings.PPPoEEnable)
	pppoeMgr.SetOnDemand(appSettings.OnDemand)

	// 路由守卫（始终创建，配置可动态更新）
	mcastIface := appSettings.McastIface
	if mcastIface == "" {
		mcastIface = fallbackMcastIface
		log.Printf("[main] 警告: 未配置组播网口，兜底使用 %s，请在网页界面配置", mcastIface)
	}
	guard := routeguard.New(mcastIface, pppoeMgr.IfaceName(), true)
	guard.SetBlockPPPoE(appSettings.PPPoEEnable)

	// 如果配置了 PPPoE 且不是按需拨号，先执行拨号
	if appSettings.PPPoEEnable && !appSettings.OnDemand {
		log.Printf("[main] 开始 PPPoE 拨号...")
		if err := pppoeMgr.Start(); err != nil {
			log.Printf("[main] PPPoE 拨号失败: %v（服务继续运行，可通过网页重试）", err)
		}
	}

	// 优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 启动路由守卫
	guard.Start(5 * time.Second)
	defer guard.Stop()

	// 初始化 relay
	relayMgr := relay.New(mcastIface, ruleStore)
	relayMgr.SetPPPoE(pppoeMgr)
	relayMgr.SetChannels(channelStore)
	relayMgr.SetStats(statsStore)
	relayMgr.SetLimits(limitsStore)
	relayMgr.SetIdleTimeout(time.Duration(appSettings.IdleTimeout) * time.Second)

	// 统计存储：30 秒定时落盘，ctx 结束即停（末次落盘由 Close 完成）
	statsStore.Start(ctx)

	// 启动幽灵订阅清除（使用全局 context，关闭信号时自动退出）
	relayMgr.StartCleanup(ctx)

	// 设置配置变更回调
	settingsStore.SetOnChange(func(newSettings settings.Config) {
		pppoeMgr.UpdateConfig(pppoe.Config{
			Iface: newSettings.PPPoEIface,
			User:  newSettings.PPPoEUser,
			Pass:  newSettings.PPPoEPass,
			Unit:  newSettings.PPPoEUnit,
		})
		pppoeMgr.SetEnabled(newSettings.PPPoEEnable)
		pppoeMgr.SetOnDemand(newSettings.OnDemand)
		relayMgr.SetIdleTimeout(time.Duration(newSettings.IdleTimeout) * time.Second)

		mcastIface := newSettings.McastIface
		if mcastIface == "" {
			mcastIface = fallbackMcastIface
			log.Printf("[main] 警告: 未配置组播网口，兜底使用 %s", mcastIface)
		}
		relayMgr.SetIface(mcastIface)
		guard.UpdateConfig(mcastIface, pppoeMgr.IfaceName(), true)
		guard.SetBlockPPPoE(newSettings.PPPoEEnable)
		guard.RunOnce()

		log.Printf("[main] 运行时配置已更新")
	})

	// 注册 HTTP 路由
	handler := api.New(relayMgr, pppoeMgr, ruleStore, channelStore, settingsStore, statsStore, limitsStore, version)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	// HTTP 致命错误（如端口占用）不再 log.Fatalf 直接杀进程——那会跳过所有 defer
	// （guard/pppoe/stats 落盘）。改为通知主流程走统一关闭路径。
	httpErr := make(chan error, 1)
	go func() {
		log.Printf("[main] HTTP 服务已启动: %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			httpErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Println("[main] 收到关闭信号，正在停止...")
	case err := <-httpErr:
		log.Printf("[main] HTTP 服务异常，正在停止: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	// pppoe.Stop / statsStore.Close / guard.Stop 由 defer 完成
	log.Println("[main] 已退出")
}
