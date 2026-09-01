// Package settings 管理运行时配置，支持通过 Web 界面修改并持久化。
package settings

import (
	"log"
	"net"
	"sync"

	"iptv-udpproxy/internal/storeutil"
)

// Config 运行时配置。
type Config struct {
	// PPPoE 配置
	PPPoEUser   string `json:"pppoe_user"`
	PPPoEPass   string `json:"pppoe_pass"`
	PPPoEUnit   int    `json:"pppoe_unit"`
	PPPoEEnable bool   `json:"pppoe_enable"`
	OnDemand    bool   `json:"on_demand"`
	IdleTimeout int    `json:"idle_timeout"` // 秒

	// 网口配置
	McastIface string `json:"mcast_iface"`
	PPPoEIface string `json:"pppoe_iface"`

	// 其他配置
	RouteGuard bool `json:"route_guard"`
}

// NetworkInterface 网口信息。
type NetworkInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
	IsUp      bool     `json:"is_up"`
}

// Store 配置存储。
type Store struct {
	mu       sync.RWMutex
	cfg      Config
	path     string
	onChange func(cfg Config) // 配置变更回调
}

// New 创建配置存储。
func New(path string) *Store {
	return &Store{
		path: path,
		cfg: Config{
			PPPoEUnit:   60,
			IdleTimeout: 300,
			RouteGuard:  true,
		},
	}
}

// SetOnChange 设置配置变更回调。
func (s *Store) SetOnChange(fn func(cfg Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// Load 从文件加载配置。文件损坏时备份并以默认配置起步（显著告警，
// 不再静默覆盖用户数据）；文件不存在则使用默认配置。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	corrupted, err := storeutil.LoadJSON(s.path, &s.cfg)
	if err != nil {
		return err
	}
	if corrupted {
		log.Printf("[settings] 警告: 设置文件 %s 损坏已备份，本次使用默认配置，请在网页重新确认 PPPoE 等设置", s.path)
	}
	return nil
}

// Save 原子保存配置到文件。权限 0600：文件内含明文 PPPoE 密码。
func (s *Store) Save() error {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return storeutil.WriteJSON(s.path, cfg, 0o600)
}

// Get 获取当前配置。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update 更新配置并保存。onChange 在保存成功后【同步】调用：
// 调用方（Web 请求）返回前各管理器已应用新配置，避免"页面已保存但拨号参数未生效"的窗口。
func (s *Store) Update(cfg Config) error {
	s.mu.Lock()
	s.cfg = cfg
	onChange := s.onChange
	s.mu.Unlock()

	if err := s.Save(); err != nil {
		return err
	}

	log.Printf("[settings] 配置已更新")

	if onChange != nil {
		onChange(cfg)
	}

	return nil
}

// ListInterfaces 列出系统网络接口。
func ListInterfaces() ([]NetworkInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var result []NetworkInterface
	for _, iface := range ifaces {
		// 跳过 lo 和 docker 相关接口
		if iface.Name == "lo" || len(iface.Name) > 3 && iface.Name[:3] == "br-" {
			continue
		}

		ni := NetworkInterface{
			Name:      iface.Name,
			Addresses: []string{}, // 确保返回空数组而非 null
			IsUp:      iface.Flags&net.FlagUp != 0,
		}

		addrs, err := iface.Addrs()
		if err == nil {
			for _, addr := range addrs {
				ni.Addresses = append(ni.Addresses, addr.String())
			}
		}

		result = append(result, ni)
	}

	return result, nil
}
