// Package limits 管理频道观看时长限制规则和全局备选地址池。
//
// 当某频道的累计观看时长（历史 + 当前流实时）超过每日或每周上限时，
// 返回超限结果，调用方可从备选池中随机选取替换源进行无缝切换。
package limits

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"iptv-udpproxy/internal/stats"
	"iptv-udpproxy/internal/storeutil"
)

// Limit 一条观看时长限制规则。
type Limit struct {
	ID        string `json:"id"`
	Address   string `json:"address"`    // 受限频道地址 "239.69.1.100:10000"
	DailyMax  int    `json:"daily_max"`  // 每日最大秒数，0=不限
	WeeklyMax int    `json:"weekly_max"` // 每周最大秒数，0=不限
	Enabled   bool   `json:"enabled"`
}

// ExceedResult 超限检查结果。
type ExceedResult struct {
	Exceeded   bool   `json:"exceeded"`
	LimitType  string `json:"limit_type"`  // "daily" or "weekly"
	CurrentSec int64  `json:"current_sec"` // 当前累计秒数
	MaxSec     int64  `json:"max_sec"`     // 上限秒数
}

// PoolConfig 全局备选地址池。
type PoolConfig struct {
	Addresses []string `json:"addresses"`
}

// Store 限制规则存储，带 JSON 持久化。
type Store struct {
	mu     sync.Mutex
	path   string
	limits []Limit
	pool   PoolConfig
}

// Open 加载或初始化限制规则文件。损坏时备份后以空配置起步；
// 兼容新格式 {limits, pool} 与旧格式纯 []Limit。
func Open(path string) (*Store, error) {
	s := &Store{
		path:   path,
		limits: []Limit{},
		pool:   PoolConfig{Addresses: []string{}},
	}
	var fd fileData
	if _, err := storeutil.LoadJSON(path, &fd); err != nil {
		return nil, err
	}
	s.limits = fd.limits
	if s.limits == nil {
		s.limits = []Limit{}
	}
	s.pool = fd.pool
	if s.pool.Addresses == nil {
		s.pool.Addresses = []string{}
	}
	return s, nil
}

// fileData 限制文件磁盘格式（新/旧格式双兼容）。
type fileData struct {
	limits []Limit
	pool   PoolConfig
}

func (f *fileData) UnmarshalJSON(data []byte) error {
	var wrapper struct {
		Limits []Limit    `json:"limits"`
		Pool   PoolConfig `json:"pool"`
	}
	// 文件是 JSON 对象即视为新格式（含空规则+空池的合法落盘内容），
	// 避免合法空文件被误判损坏；对象解析失败才尝试旧版纯数组格式。
	if err := json.Unmarshal(data, &wrapper); err == nil {
		f.limits = wrapper.Limits
		f.pool = wrapper.Pool
		return nil
	}
	var arr []Limit
	if err := json.Unmarshal(data, &arr); err != nil {
		// 既非新格式也非数组（含非法 JSON）：报错让 LoadJSON 走损坏备份
		return fmt.Errorf("无法识别的限制文件格式")
	}
	f.limits = arr
	f.pool = PoolConfig{Addresses: []string{}}
	return nil
}

// ListLimits 返回所有限制规则副本。
func (s *Store) ListLimits() []Limit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Limit, len(s.limits))
	copy(out, s.limits)
	return out
}

// AddLimit 校验并新增一条限制规则。
func (s *Store) AddLimit(l Limit) (Limit, error) {
	if err := validateLimit(&l); err != nil {
		return Limit{}, err
	}
	l.ID = newID()
	l.Enabled = true
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = append(s.limits, l)
	return l, s.save()
}

// UpdateLimit 按 ID 更新限制规则。
func (s *Store) UpdateLimit(id string, l Limit) (Limit, error) {
	if err := validateLimit(&l); err != nil {
		return Limit{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.limits {
		if s.limits[i].ID == id {
			l.ID = id
			s.limits[i] = l
			return l, s.save()
		}
	}
	return Limit{}, fmt.Errorf("限制规则 %s 不存在", id)
}

// DeleteLimit 按 ID 删除限制规则。
func (s *Store) DeleteLimit(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.limits {
		if s.limits[i].ID == id {
			s.limits = append(s.limits[:i], s.limits[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("限制规则 %s 不存在", id)
}

// GetPool 返回备选地址池副本。
func (s *Store) GetPool() PoolConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.pool.Addresses))
	copy(out, s.pool.Addresses)
	return PoolConfig{Addresses: out}
}

// SetPool 更新备选地址池。
func (s *Store) SetPool(pool PoolConfig) error {
	// 校验地址格式
	for _, addr := range pool.Addresses {
		if _, err := netip.ParseAddrPort(addr); err != nil {
			return fmt.Errorf("备选地址 %q 格式无效: %w", addr, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pool = pool
	return s.save()
}

// HasLimit 检查指定频道是否有启用的限制规则。
func (s *Store) HasLimit(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.limits {
		if s.limits[i].Enabled && s.limits[i].Address == address {
			return true
		}
	}
	return false
}

// CheckLimit 检查指定频道是否超限。
// liveElapsed 为当前流的实时时长（time.Since），stats 提供历史数据。
func (s *Store) CheckLimit(address string, statStore *stats.Store, liveElapsed time.Duration, now time.Time) *ExceedResult {
	s.mu.Lock()
	var lim Limit
	found := false
	for i := range s.limits {
		if s.limits[i].Enabled && s.limits[i].Address == address {
			lim = s.limits[i] // 值拷贝：锁外读取不与并发 UpdateLimit 竞争
			found = true
			break
		}
	}
	s.mu.Unlock()

	if !found {
		return nil
	}

	liveSec := int64(liveElapsed.Seconds())
	today := now.Format("2006-01-02")

	// 检查每日限制
	if lim.DailyMax > 0 {
		historical := statStore.HistoricalSum(address, today)
		total := historical + liveSec
		if total >= int64(lim.DailyMax) {
			return &ExceedResult{
				Exceeded:   true,
				LimitType:  "daily",
				CurrentSec: total,
				MaxSec:     int64(lim.DailyMax),
			}
		}
	}

	// 检查每周限制
	if lim.WeeklyMax > 0 {
		year, week := now.ISOWeek()
		historical := statStore.HistoricalSumWeek(address, year, week)
		total := historical + liveSec
		if total >= int64(lim.WeeklyMax) {
			return &ExceedResult{
				Exceeded:   true,
				LimitType:  "weekly",
				CurrentSec: total,
				MaxSec:     int64(lim.WeeklyMax),
			}
		}
	}

	return nil
}

// PickReplacement 从备选池中随机选取一个不同于当前地址的组播地址。
// 返回 (选中地址, 是否有可用地址)。
func (s *Store) PickReplacement(currentAddr string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pool.Addresses) == 0 {
		return "", false
	}
	// 过滤掉当前地址
	candidates := make([]string, 0, len(s.pool.Addresses))
	for _, a := range s.pool.Addresses {
		if a != currentAddr {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		// 池中只有当前地址，仍使用它（避免无源可切）
		candidates = s.pool.Addresses
	}
	b := make([]byte, 1)
	_, _ = rand.Read(b)
	idx := int(b[0]) % len(candidates)
	return candidates[idx], true
}

func validateLimit(l *Limit) error {
	if l.Address == "" {
		return fmt.Errorf("频道地址不能为空")
	}
	if _, err := netip.ParseAddrPort(l.Address); err != nil {
		return fmt.Errorf("频道地址 %q 格式无效: %w", l.Address, err)
	}
	if l.DailyMax < 0 || l.WeeklyMax < 0 {
		return fmt.Errorf("限制时长不能为负数")
	}
	if l.DailyMax == 0 && l.WeeklyMax == 0 {
		return fmt.Errorf("每日和每周限制不能同时为 0")
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// save 原子持久化到磁盘，调用方需持有 s.mu。
func (s *Store) save() error {
	return storeutil.WriteJSON(s.path, struct {
		Limits []Limit    `json:"limits"`
		Pool   PoolConfig `json:"pool"`
	}{
		Limits: s.limits,
		Pool:   s.pool,
	}, 0o644)
}
