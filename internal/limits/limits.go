// Package limits 管理频道观看时长限制规则和全局备选地址池。
//
// 当某频道的累计观看时长（历史 + 当前流实时）超过每日、每周或连续上限时，
// 返回超限结果，调用方可从备选池中随机选取替换源进行无缝切换。
// 连续限制：连续观看（两次观看间隔不超过休息时长视为连续）超过最大连续
// 时长后切换到备选池，须间隔超过休息时长未再观看该频道后才能调回。
package limits

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"time"

	"iptv-udpproxy/internal/stats"
	"iptv-udpproxy/internal/storeutil"
)

// Limit 一条观看时长限制规则。
type Limit struct {
	ID                string `json:"id"`
	Address           string `json:"address"`            // 受限频道地址 "239.69.1.100:10000"
	DailyMax          int    `json:"daily_max"`          // 每日最大秒数，0=不限
	WeeklyMax         int    `json:"weekly_max"`         // 每周最大秒数，0=不限
	WeekdayMax        [7]int `json:"weekday_max"`        // 分星期最大秒数，下标 0=周日..6=周六，0=未设置（按每日限制执行）
	ContinuousMax     int    `json:"continuous_max"`     // 最大连续观看秒数，0=未设置
	RestDuration      int    `json:"rest_duration"`      // 休息秒数：连续判定的间隔阈值，也是超限后可调回的等待时长
	ContinuousEnabled bool   `json:"continuous_enabled"` // 连续限制启用勾选
	Enabled           bool   `json:"enabled"`
}

// ExceedResult 超限检查结果。
type ExceedResult struct {
	Exceeded   bool   `json:"exceeded"`
	LimitType  string `json:"limit_type"`         // "daily"、"weekday"、"weekly" 或 "continuous"
	DayName    string `json:"day_name,omitempty"` // LimitType=="weekday" 时：命中的星期（如 "周一"）
	CurrentSec int64  `json:"current_sec"`        // 当前累计秒数
	MaxSec     int64  `json:"max_sec"`            // 上限秒数
}

// LimitDesc 返回日志用描述：每日 / 每周 / 具体星期 / 连续。
func (r *ExceedResult) LimitDesc() string {
	switch r.LimitType {
	case "weekday":
		return r.DayName
	case "weekly":
		return "每周"
	case "continuous":
		return "连续"
	default:
		return "每日"
	}
}

// weekdayNames 与 time.Weekday 下标对应的中文星期名。
var weekdayNames = [7]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// PoolConfig 全局备选地址池。
type PoolConfig struct {
	Addresses []string `json:"addresses"`
}

// SessionState 单频道连续观看状态：上次会话结束时间与当前连续链累计秒数。
type SessionState struct {
	LastEnd time.Time `json:"last_end"` // 上一次观看会话结束时间
	ContSec int64     `json:"cont_sec"` // 当前连续链已累积秒数（不含进行中的会话）
}

// Store 限制规则存储，带 JSON 持久化。
type Store struct {
	mu       sync.Mutex
	path     string
	limits   []Limit
	pool     PoolConfig
	sessions map[string]*SessionState
}

// Open 加载或初始化限制规则文件。损坏时备份后以空配置起步；
// 兼容新格式 {limits, pool, sessions} 与旧格式 {limits, pool} / 纯 []Limit。
func Open(path string) (*Store, error) {
	s := &Store{
		path:     path,
		limits:   []Limit{},
		pool:     PoolConfig{Addresses: []string{}},
		sessions: make(map[string]*SessionState),
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
	s.sessions = fd.sessions
	if s.sessions == nil {
		s.sessions = make(map[string]*SessionState)
	}
	return s, nil
}

// fileData 限制文件磁盘格式（新/旧格式双兼容）。
type fileData struct {
	limits   []Limit
	pool     PoolConfig
	sessions map[string]*SessionState
}

func (f *fileData) UnmarshalJSON(data []byte) error {
	var wrapper struct {
		Limits   []Limit                  `json:"limits"`
		Pool     PoolConfig               `json:"pool"`
		Sessions map[string]*SessionState `json:"sessions"`
	}
	// 文件是 JSON 对象即视为新格式（含空规则+空池的合法落盘内容），
	// 避免合法空文件被误判损坏；对象解析失败才尝试旧版纯数组格式。
	if err := json.Unmarshal(data, &wrapper); err == nil {
		f.limits = wrapper.Limits
		f.pool = wrapper.Pool
		f.sessions = wrapper.Sessions
		return nil
	}
	var arr []Limit
	if err := json.Unmarshal(data, &arr); err != nil {
		// 既非新格式也非数组（含非法 JSON）：报错让 LoadJSON 走损坏备份
		return fmt.Errorf("无法识别的限制文件格式")
	}
	f.limits = arr
	f.pool = PoolConfig{Addresses: []string{}}
	f.sessions = make(map[string]*SessionState)
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
// liveElapsed 为当前流的实时时长（time.Since），stats 提供历史数据；
// 跨天/跨周的流只按实际落入统计窗口（自然日 / ISO 周）的部分计入。
// 连续限制：当前会话起点距上次会话结束不超过休息时长时视为同一连续链，
// 累计秒数（链上已结束会话 + 当前会话实时）达到上限即超限。
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
	var contSec int64 // 当前连续链中已结束会话的累计秒数
	liveStart := now.Add(-liveElapsed)
	if found && lim.ContinuousEnabled && lim.ContinuousMax > 0 {
		contSec = s.chainContSecLocked(address, &lim, liveStart)
	}
	s.mu.Unlock()

	if !found {
		return nil
	}

	today := now.Format("2006-01-02")

	// 检查每日限制：当天星期若已自定义上限则覆盖每日上限，否则按每日执行
	dailyMax := lim.DailyMax
	weekdayHit := false
	if wd := lim.WeekdayMax[int(now.Weekday())]; wd > 0 {
		dailyMax = wd
		weekdayHit = true
	}
	if dailyMax > 0 {
		// 当前流只计今日窗口内的部分（跨午夜流不重复计入前一天）
		segment := liveInWindow(liveStart, startOfToday(now), now)
		historical := statStore.HistoricalSum(address, today)
		total := historical + segment
		if total >= int64(dailyMax) {
			res := &ExceedResult{
				Exceeded:   true,
				CurrentSec: total,
				MaxSec:     int64(dailyMax),
			}
			if weekdayHit {
				res.LimitType = "weekday"
				res.DayName = weekdayNames[int(now.Weekday())]
			} else {
				res.LimitType = "daily"
			}
			return res
		}
	}

	// 检查每周限制
	if lim.WeeklyMax > 0 {
		year, week := now.ISOWeek()
		// 当前流只计本周窗口内的部分（跨周流不重复计入上一周）
		segment := liveInWindow(liveStart, startOfISOWeek(now), now)
		historical := statStore.HistoricalSumWeek(address, year, week)
		total := historical + segment
		if total >= int64(lim.WeeklyMax) {
			return &ExceedResult{
				Exceeded:   true,
				LimitType:  "weekly",
				CurrentSec: total,
				MaxSec:     int64(lim.WeeklyMax),
			}
		}
	}

	// 检查连续限制：链上已累计 + 当前会话实时时长
	if lim.ContinuousEnabled && lim.ContinuousMax > 0 {
		total := contSec + int64(liveElapsed.Seconds())
		if total >= int64(lim.ContinuousMax) {
			return &ExceedResult{
				Exceeded:   true,
				LimitType:  "continuous",
				CurrentSec: total,
				MaxSec:     int64(lim.ContinuousMax),
			}
		}
	}

	return nil
}

// chainContSecLocked 返回频道当前连续链中已结束会话的累计秒数；
// 若本次会话起点距上次会话结束已超过休息时长，则链条断开并重置累计。
// 调用方需持有 s.mu。
func (s *Store) chainContSecLocked(address string, lim *Limit, liveStart time.Time) int64 {
	st, ok := s.sessions[address]
	if !ok {
		return 0
	}
	rest := time.Duration(lim.RestDuration) * time.Second
	if liveStart.Sub(st.LastEnd) > rest {
		st.ContSec = 0
	}
	return st.ContSec
}

// enabledLimitLocked 返回频道的启用限制规则，无则 nil。调用方需持有 s.mu。
func (s *Store) enabledLimitLocked(address string) *Limit {
	for i := range s.limits {
		if s.limits[i].Enabled && s.limits[i].Address == address {
			return &s.limits[i]
		}
	}
	return nil
}

// EndSession 记录一次观看会话结束，累积频道的连续观看状态并落盘。
// 无启用限制规则的频道不保留状态，防止状态表无限增长。
func (s *Store) EndSession(address string, start, end time.Time) {
	if address == "" || !end.After(start) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sessions[address]
	if st == nil {
		st = &SessionState{}
		s.sessions[address] = st
	}
	st.LastEnd = end
	st.ContSec += int64(end.Sub(start).Seconds())
	if s.enabledLimitLocked(address) == nil {
		delete(s.sessions, address)
		return
	}
	if err := s.save(); err != nil {
		log.Printf("[limits] 连续观看状态落盘失败: %v", err)
	}
}

// startOfToday 返回 now 当日 0 点。
func startOfToday(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// startOfISOWeek 返回 now 所在 ISO 周的周一 0 点。
func startOfISOWeek(now time.Time) time.Time {
	wd := int(now.Weekday())
	if wd == 0 {
		wd = 7 // 周日是一周的最后一天
	}
	daysFromMonday := wd - 1
	return startOfToday(now).AddDate(0, 0, -daysFromMonday)
}

// liveInWindow 返回当前流 [start, now] 落在 [windowStart, now] 内的秒数。
// 流起点早于窗口起点时只计窗口内的部分；否则计全段。
func liveInWindow(start, windowStart, now time.Time) int64 {
	if start.Before(windowStart) {
		start = windowStart
	}
	if !now.After(start) {
		return 0
	}
	return int64(now.Sub(start).Seconds())
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
	if l.ContinuousMax < 0 || l.RestDuration < 0 {
		return fmt.Errorf("连续限制时长不能为负数")
	}
	if l.ContinuousEnabled && l.ContinuousMax <= 0 {
		return fmt.Errorf("启用连续限制时，最大连续观看时长必须大于 0")
	}
	anyWeekday := false
	for _, v := range l.WeekdayMax {
		if v < 0 {
			return fmt.Errorf("分星期限制时长不能为负数")
		}
		if v > 0 {
			anyWeekday = true
		}
	}
	if l.DailyMax == 0 && l.WeeklyMax == 0 && !anyWeekday && !l.ContinuousEnabled {
		return fmt.Errorf("每日、每周、分星期和连续限制不能全部未设置")
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
		Limits   []Limit                  `json:"limits"`
		Pool     PoolConfig               `json:"pool"`
		Sessions map[string]*SessionState `json:"sessions"`
	}{
		Limits:   s.limits,
		Pool:     s.pool,
		Sessions: s.sessions,
	}, 0o644)
}
