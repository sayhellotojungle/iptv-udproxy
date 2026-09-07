// Package stats 记录每个频道的观看时长，支持按天/周/月查询和排名。
//
// 实现：内存聚合（地址 -> 日期 -> 秒）+ 30 秒定时落盘 + 90 天保留期裁剪，
// 替换旧版"每次 Add 持锁全量重写"（记录只增、文件随观看时长线性膨胀）。
package stats

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"iptv-udpproxy/internal/storeutil"
)

const (
	flushInterval = 30 * time.Second // 定时落盘间隔
	retentionDays = 90               // 历史数据保留天数
)

// Record 旧版单条观看记录（仅用于兼容加载旧文件）。
type Record struct {
	Address  string `json:"address"`
	Date     string `json:"date"`     // "2026-07-21"
	Duration int64  `json:"duration"` // 秒
}

// RankItem 频道排名项。
type RankItem struct {
	Address  string `json:"address"`
	Name     string `json:"name,omitempty"`
	Duration int64  `json:"duration"` // 秒
	Rank     int    `json:"rank"`
}

// PeriodResult 某时段的统计结果。
type PeriodResult struct {
	TotalSeconds int64      `json:"total_seconds"`
	Channels     []RankItem `json:"channels"`
}

// Store 观看时长存储：内存聚合 + 定时落盘。
type Store struct {
	mu        sync.Mutex
	path      string
	aggr      map[string]map[string]int64 // address -> "2006-01-02" -> 秒
	closeOnce sync.Once
}

// fileData 磁盘格式（新聚合格式 + 旧记录数组兼容）。
type fileData struct {
	aggr map[string]map[string]int64
}

// UnmarshalJSON 兼容两种格式：
//   - 新格式: {"addr": {"2026-07-21": 3600}}
//   - 旧格式: [{"address":..., "date":..., "duration":...}, ...]（合并聚合后加载）
func (f *fileData) UnmarshalJSON(data []byte) error {
	var m map[string]map[string]int64
	if err := json.Unmarshal(data, &m); err == nil {
		f.aggr = m
		return nil
	}
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	m = make(map[string]map[string]int64)
	for _, r := range records {
		if r.Address == "" || r.Date == "" || r.Duration <= 0 {
			continue
		}
		dm, ok := m[r.Address]
		if !ok {
			dm = make(map[string]int64)
			m[r.Address] = dm
		}
		dm[r.Date] += r.Duration
	}
	f.aggr = m
	return nil
}

// Open 加载或初始化观看时长存储。文件损坏时备份后以空统计起步，不报错。
func Open(path string) (*Store, error) {
	s := &Store{path: path, aggr: make(map[string]map[string]int64)}
	var fd fileData
	if _, err := storeutil.LoadJSON(path, &fd); err != nil {
		return nil, err
	}
	for addr, byDate := range fd.aggr {
		if len(byDate) == 0 {
			continue
		}
		s.aggr[addr] = byDate
	}
	return s, nil
}

// Start 启动后台定时落盘 goroutine，ctx 结束即退出。
func (s *Store) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Flush(); err != nil {
					log.Printf("[stats] 定时落盘失败: %v", err)
				}
			}
		}
	}()
}

// Close 幂等：末次落盘，确保退出不丢已聚合数据。
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		if err := s.Flush(); err != nil {
			log.Printf("[stats] 末次落盘失败: %v", err)
		}
	})
}

// Add 聚合一次观看时长（按 t 所在日期）。不立即落盘。
func (s *Store) Add(address string, duration time.Duration, t time.Time) {
	sec := int64(duration.Seconds())
	if sec <= 0 || address == "" {
		return
	}
	s.mu.Lock()
	byDate, ok := s.aggr[address]
	if !ok {
		byDate = make(map[string]int64)
		s.aggr[address] = byDate
	}
	byDate[t.Format("2006-01-02")] += sec
	s.mu.Unlock()
}

// AddRange 把 [start, end] 的观看时长按自然日分段聚合：跨天流计入各天实际停留的秒数。
// 不立即落盘。
func (s *Store) AddRange(address string, start, end time.Time) {
	if address == "" || !end.After(start) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for cur := start; cur.Before(end); {
		segEnd := time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, cur.Location()).AddDate(0, 0, 1)
		if end.Before(segEnd) {
			segEnd = end
		}
		if sec := int64(segEnd.Sub(cur).Seconds()); sec > 0 {
			byDate, ok := s.aggr[address]
			if !ok {
				byDate = make(map[string]int64)
				s.aggr[address] = byDate
			}
			byDate[cur.Format("2006-01-02")] += sec
		}
		cur = segEnd
	}
}

// Flush 立即落盘（并顺带裁剪过期数据）。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	return storeutil.WriteJSON(s.path, s.aggr, 0o644)
}

// ClearAll 清空全部统计并落盘。
func (s *Store) ClearAll() error {
	s.mu.Lock()
	s.aggr = make(map[string]map[string]int64)
	err := storeutil.WriteJSON(s.path, s.aggr, 0o644)
	s.mu.Unlock()
	return err
}

// HistoricalSum 返回某频道在指定日期的累计秒数（不含正在观看的当前流）。
func (s *Store) HistoricalSum(address string, date string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aggr[address][date]
}

// HistoricalSumWeek 返回某频道在指定 ISO 周（year, week）的累计秒数。
func (s *Store) HistoricalSumWeek(address string, year int, week int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for date, sec := range s.aggr[address] {
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		if y, w := t.ISOWeek(); y == year && w == week {
			total += sec
		}
	}
	return total
}

// Query 查询指定维度（day/week/month）的统计结果，nameFn 用于地址->频道名映射。
// top>0 时只返回前 top 名（total 仍为全量合计）。
func (s *Store) Query(period string, dateStr string, top int, nameFn func(string) string) PeriodResult {
	s.mu.Lock()
	aggr := s.aggr
	s.mu.Unlock()

	now := time.Now()
	var weekYear, weekNo int
	var monthYear int
	var month time.Month
	targetDate := now.Format("2006-01-02")
	switch period {
	case "week":
		t := parseOrNow(dateStr, now)
		weekYear, weekNo = t.ISOWeek()
	case "month":
		t := parseOrNow(dateStr, now)
		monthYear, month = t.Year(), t.Month()
	default: // "day"
		if dateStr != "" {
			targetDate = dateStr
		}
	}

	perAddr := make(map[string]int64)
	for addr, byDate := range aggr {
		var sec int64
		switch period {
		case "week":
			for d, v := range byDate {
				if t, err := time.Parse("2006-01-02", d); err == nil {
					if y, w := t.ISOWeek(); y == weekYear && w == weekNo {
						sec += v
					}
				}
			}
		case "month":
			for d, v := range byDate {
				if t, err := time.Parse("2006-01-02", d); err == nil {
					if t.Year() == monthYear && t.Month() == month {
						sec += v
					}
				}
			}
		default:
			sec = byDate[targetDate]
		}
		if sec > 0 {
			perAddr[addr] = sec
		}
	}

	var total int64
	items := make([]RankItem, 0, len(perAddr))
	for addr, sec := range perAddr {
		total += sec
		items = append(items, RankItem{Address: addr, Duration: sec})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Duration != items[j].Duration {
			return items[i].Duration > items[j].Duration
		}
		return items[i].Address < items[j].Address
	})
	for i := range items {
		items[i].Rank = i + 1
		if nameFn != nil {
			items[i].Name = nameFn(items[i].Address)
		}
	}
	if top > 0 && top < len(items) {
		items = items[:top]
	}
	return PeriodResult{TotalSeconds: total, Channels: items}
}

// pruneLocked 裁掉保留期之外的历史数据（"YYYY-MM-DD" 字典序即时间序）。
func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.AddDate(0, 0, -retentionDays).Format("2006-01-02")
	for addr, byDate := range s.aggr {
		for d := range byDate {
			if d < cutoff {
				delete(byDate, d)
			}
		}
		if len(byDate) == 0 {
			delete(s.aggr, addr)
		}
	}
}

func parseOrNow(dateStr string, now time.Time) time.Time {
	if dateStr == "" {
		return now
	}
	if t, err := time.Parse("2006-01-02", dateStr); err == nil {
		return t
	}
	return now
}
