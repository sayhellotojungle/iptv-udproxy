// Package settings 管理运行时配置，支持通过 Web 界面修改并持久化。
package settings

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"sync"
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

// Load 从文件加载配置。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在，使用默认配置
		}
		return err
	}

	return json.Unmarshal(data, &s.cfg)
}

// Save 保存配置到文件。
func (s *Store) Save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	// 原子写入
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}

// Get 获取当前配置。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update 更新配置并保存。
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
		go onChange(cfg)
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
