// Package rules 实现组播源替换规则的存储与时间窗匹配。
//
// 规则语义：在 Start~End 时间窗内（可选限定星期），把对 From 组播地址的
// 请求实际接到 To 组播地址上。时间窗支持跨午夜（如 23:30~01:00）。
package rules

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"iptv-udpproxy/internal/storeutil"
)

// Rule 一条换源规则。
type Rule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	From    string `json:"from"`  // 被替换的请求地址 "239.69.1.123:10376"
	To      string `json:"to"`    // 实际取流地址
	Start   string `json:"start"` // "19:00"
	End     string `json:"end"`   // "19:50"；等于 Start 表示全天生效
	Days    []int  `json:"days"`  // 0=周日..6=周六；空=每天
}

// Store 规则集合，带 JSON 文件持久化。
type Store struct {
	mu    sync.Mutex
	path  string
	rules []Rule
}

// Open 加载（或初始化）规则文件。文件损坏时备份后以空规则起步，不 fatal。
func Open(path string) (*Store, error) {
	s := &Store{path: path, rules: []Rule{}}
	if _, err := storeutil.LoadJSON(path, &s.rules); err != nil {
		return nil, err
	}
	if s.rules == nil {
		s.rules = []Rule{}
	}
	return s, nil
}

// List 返回规则副本。
func (s *Store) List() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}

// Add 校验并新增一条规则。
func (s *Store) Add(r Rule) (Rule, error) {
	if err := normalize(&r); err != nil {
		return Rule{}, err
	}
	r.ID = newID()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, r)
	return r, s.save()
}

// Update 按 ID 更新规则。
func (s *Store) Update(id string, r Rule) (Rule, error) {
	if err := normalize(&r); err != nil {
		return Rule{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == id {
			r.ID = id
			s.rules[i] = r
			return r, s.save()
		}
	}
	return Rule{}, fmt.Errorf("规则 %s 不存在", id)
}

// Delete 按 ID 删除规则。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == id {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("规则 %s 不存在", id)
}

// Resolve 返回 addr 在 t 时刻应实际使用的组播源。
// 无匹配规则时原样返回。命中多条时取第一条启用且生效的规则，不做链式解析。
func (s *Store) Resolve(addr string, t time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	normed, err := normAddr(addr)
	if err != nil {
		return addr
	}

	for _, r := range s.rules {
		if !r.Enabled {
			continue
		}
		if r.From != normed {
			continue
		}
		if r.activeAt(t) {
			return r.To
		}
	}
	return addr
}

// activeAt 判断规则时间窗在 t 时刻是否生效。
func (r Rule) activeAt(t time.Time) bool {
	sm, err1 := parseHM(r.Start)
	em, err2 := parseHM(r.End)
	if err1 != nil || err2 != nil {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	day := t

	switch {
	case sm == em: // 全天生效
	case sm < em:
		if now < sm || now >= em {
			return false
		}
	default: // 跨午夜，如 23:30~01:00
		if now >= sm {
			// 属于今天开始的窗口
		} else if now < em {
			day = t.AddDate(0, 0, -1) // 属于昨天开始的窗口，星期按开始日算
		} else {
			return false
		}
	}
	return r.dayOK(day.Weekday())
}

func (r Rule) dayOK(w time.Weekday) bool {
	if len(r.Days) == 0 {
		return true
	}
	for _, d := range r.Days {
		if d == int(w) {
			return true
		}
	}
	return false
}

func parseHM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// normalize 校验并规范化规则字段（地址转为规范 host:port 写法）。
func normalize(r *Rule) error {
	from, err := normAddr(r.From)
	if err != nil {
		return fmt.Errorf("原地址无效: %w", err)
	}
	to, err := normAddr(r.To)
	if err != nil {
		return fmt.Errorf("目标地址无效: %w", err)
	}
	if from == to {
		return fmt.Errorf("原地址与目标地址相同")
	}
	if _, err := parseHM(r.Start); err != nil {
		return fmt.Errorf("开始时间应为 HH:MM 格式: %q", r.Start)
	}
	if _, err := parseHM(r.End); err != nil {
		return fmt.Errorf("结束时间应为 HH:MM 格式: %q", r.End)
	}
	for _, d := range r.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("星期取值应为 0~6")
		}
	}
	r.From, r.To = from, to
	return nil
}

func normAddr(s string) (string, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return "", fmt.Errorf("%q 应为 组播IP:端口", s)
	}
	if !ap.Addr().Is4() || !ap.Addr().IsMulticast() {
		return "", fmt.Errorf("%q 不是 IPv4 组播地址", s)
	}
	return ap.String(), nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// save 原子持久化到磁盘，调用方需持有 s.mu。
func (s *Store) save() error {
	return storeutil.WriteJSON(s.path, s.rules, 0o644)
}
