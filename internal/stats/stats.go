// Package stats 记录每个频道的观看时长，支持按天/周/月查询和排名。
package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Record 一条观看记录（单次流会话结束时写入）。
type Record struct {
	Address  string `json:"address"`
	Date     string `json:"date"`      // "2026-07-21"
	Duration int64  `json:"duration"`   // 秒
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

// Store 观看时长存储，带 JSON 持久化。
type Store struct {
	mu      sync.Mutex
	path    string
	records []Record
}

// Open 加载或初始化观看时长存储。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.records); err != nil {
			return nil, fmt.Errorf("解析观看时长文件 %s 失败: %w", path, err)
		}
	}
	return s, nil
}

// Add 追加一条观看记录并持久化。
func (s *Store) Add(address string, duration time.Duration, t time.Time) {
	if duration <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, Record{
		Address:  address,
		Date:     t.Format("2006-01-02"),
		Duration: int64(duration.Seconds()),
	})
	s.save()
}

// SumByDate 返回某天每个频道的累计秒数。
func (s *Store) SumByDate(date string) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64)
	for _, r := range s.records {
		if r.Date == date {
			out[r.Address] += r.Duration
		}
	}
	return out
}

// SumByWeek 返回某 ISO 周（year, week）每个频道的累计秒数。
func (s *Store) SumByWeek(year int, week int) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64)
	for _, r := range s.records {
		t, err := time.Parse("2006-01-02", r.Date)
		if err != nil {
			continue
		}
		y, w := t.ISOWeek()
		if y == year && w == week {
			out[r.Address] += r.Duration
		}
	}
	return out
}

// SumByMonth 返回某月（year, month）每个频道的累计秒数。
func (s *Store) SumByMonth(year int, month time.Month) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64)
	for _, r := range s.records {
		t, err := time.Parse("2006-01-02", r.Date)
		if err != nil {
			continue
		}
		if t.Year() == year && t.Month() == month {
			out[r.Address] += r.Duration
		}
	}
	return out
}

// HistoricalSum 返回某频道在指定日期的历史累计秒数（不含当天正在观看的流）。
func (s *Store) HistoricalSum(address string, date string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, r := range s.records {
		if r.Address == address && r.Date == date {
			total += r.Duration
		}
	}
	return total
}

// HistoricalSumWeek 返回某频道在指定 ISO 周的历史累计秒数。
func (s *Store) HistoricalSumWeek(address string, year int, week int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, r := range s.records {
		if r.Address != address {
			continue
		}
		t, err := time.Parse("2006-01-02", r.Date)
		if err != nil {
			continue
		}
		y, w := t.ISOWeek()
		if y == year && w == week {
			total += r.Duration
		}
	}
	return total
}

// Query 查询指定维度的统计结果，nameFn 用于将地址映射为频道名。
func (s *Store) Query(period string, dateStr string, top int, nameFn func(string) string) PeriodResult {
	var m map[string]int64
	now := time.Now()

	switch period {
	case "week":
		t := now
		if dateStr != "" {
			if parsed, err := time.Parse("2006-01-02", dateStr); err == nil {
				t = parsed
			}
		}
		y, w := t.ISOWeek()
		m = s.SumByWeek(y, w)
	case "month":
		t := now
		if dateStr != "" {
			if parsed, err := time.Parse("2006-01-02", dateStr); err == nil {
				t = parsed
			}
		}
		m = s.SumByMonth(t.Year(), t.Month())
	default: // "day"
		date := now.Format("2006-01-02")
		if dateStr != "" {
			date = dateStr
		}
		m = s.SumByDate(date)
	}

	var total int64
	items := make([]RankItem, 0, len(m))
	for addr, sec := range m {
		total += sec
		name := ""
		if nameFn != nil {
			name = nameFn(addr)
		}
		items = append(items, RankItem{Address: addr, Name: name, Duration: sec})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Duration > items[j].Duration })
	for i := range items {
		items[i].Rank = i + 1
	}
	if top > 0 && top < len(items) {
		items = items[:top]
	}
	return PeriodResult{TotalSeconds: total, Channels: items}
}

// save 持久化到磁盘，调用方需持有 s.mu。
func (s *Store) save() {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return
	}
	os.Rename(tmpPath, s.path)
}
