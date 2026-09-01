package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "watchtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQueryDay(t *testing.T) {
	s := openTemp(t)
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, time.Local)
	date := day.Format("2006-01-02")

	s.Add("a:1", 90*time.Minute, day.Add(9*time.Hour))
	s.Add("a:1", 30*time.Minute, day.Add(15*time.Hour))
	s.Add("b:2", 15*time.Minute, day.Add(10*time.Hour))
	// 其他日期不计入
	s.Add("a:1", time.Hour, day.AddDate(0, 0, -1))

	res := s.Query("day", date, 0, nil)
	// a:1 120min(7200s) + b:2 15min(900s)；前一天的 1h 不计入
	if res.TotalSeconds != 8100 {
		t.Fatalf("day total = %d, want 8100", res.TotalSeconds)
	}
	if len(res.Channels) != 2 {
		t.Fatalf("应有 2 行排名, got %v", res.Channels)
	}
	if res.Channels[0].Address != "a:1" || res.Channels[0].Duration != 7200 || res.Channels[0].Rank != 1 {
		t.Fatalf("第 1 名错误: %+v", res.Channels[0])
	}
	if res.Channels[1].Address != "b:2" || res.Channels[1].Duration != 900 || res.Channels[1].Rank != 2 {
		t.Fatalf("第 2 名错误: %+v", res.Channels[1])
	}

	// top + nameFn
	res = s.Query("day", date, 1, func(addr string) string { return "名" + addr })
	if len(res.Channels) != 1 {
		t.Fatalf("top=1 应只返回 1 行, got %d", len(res.Channels))
	}
	if res.Channels[0].Name != "名a:1" {
		t.Fatalf("nameFn 未生效: %+v", res.Channels[0])
	}
	if res.TotalSeconds != 8100 {
		t.Fatalf("top 不影响总合计: %d", res.TotalSeconds)
	}
}

func TestQueryWeekMonth(t *testing.T) {
	s := openTemp(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	monday := mondayOf(now)

	s.Add("a:1", 10*time.Minute, monday)
	s.Add("a:1", 20*time.Minute, now)
	// 上周（必在本 ISO 周之外）
	s.Add("a:1", time.Hour, monday.AddDate(0, 0, -9))
	// 上月
	s.Add("a:1", 5*time.Minute, time.Date(2026, 6, 15, 8, 0, 0, 0, time.Local))

	wk := s.Query("week", now.Format("2006-01-02"), 0, nil)
	if wk.TotalSeconds != 1800 {
		t.Fatalf("week total = %d, want 1800（上周 1h 不入周）", wk.TotalSeconds)
	}
	mo := s.Query("month", now.Format("2006-01-02"), 0, nil)
	// 10+20+60=90min(5400s)：上周仍在同月内；上月 5min 不计入
	if mo.TotalSeconds != 5400 {
		t.Fatalf("month total = %d, want 5400（上月 5min 不入月）", mo.TotalSeconds)
	}

	// 指定日期的 day 查询（2026-07-20 为周一，即本 ISO 周起始日）
	res := s.Query("day", monday.Format("2006-01-02"), 0, nil)
	if res.TotalSeconds != 1800 {
		t.Fatalf("指定日 total = %d, want 1800", res.TotalSeconds)
	}
}

func TestHistoricalSums(t *testing.T) {
	s := openTemp(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	today := now.Format("2006-01-02")
	monday := mondayOf(now)

	s.Add("a:1", 30*time.Minute, now)
	// 同 ISO 周的另一个日期（周三），不与"今天"重叠
	s.Add("a:1", 30*time.Minute, monday.Add(48*time.Hour))

	if got := s.HistoricalSum("a:1", today); got != 1800 {
		t.Fatalf("HistoricalSum = %d, want 1800", got)
	}
	y, w := now.ISOWeek()
	if got := s.HistoricalSumWeek("a:1", y, w); got != 3600 {
		t.Fatalf("HistoricalSumWeek = %d, want 3600", got)
	}
	if got := s.HistoricalSum("nope:1", today); got != 0 {
		t.Fatalf("未知地址应为 0, got %d", got)
	}
}

func TestFlushAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watchtime.json")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, time.Local)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Add("a:1", 30*time.Minute, day)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.HistoricalSum("a:1", "2026-07-20"); got != 1800 {
		t.Fatalf("重开后数据丢失: %d", got)
	}
	s2.Close()
	s2.Close() // 幂等
}

func TestLegacyRecordsLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchtime.json")
	legacy := `[{"address":"a:1","date":"2026-07-20","duration":1200}]`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.HistoricalSum("a:1", "2026-07-20"); got != 1200 {
		t.Fatalf("旧格式记录加载失败: %d", got)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// flush 后应写回新格式，重开仍可读
	s2, _ := Open(path)
	if got := s2.HistoricalSum("a:1", "2026-07-20"); got != 1200 {
		t.Fatalf("新格式往返失败: %d", got)
	}
}

func TestClearAllAndPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watchtime.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 100 天前的数据应被 Flush 时的保留期裁剪掉
	s.aggr["old:1"] = map[string]int64{(now.AddDate(0, 0, -100)).Format("2006-01-02"): 9999}
	s.Add("new:1", time.Minute, now)

	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.aggr["old:1"]; ok {
		t.Fatal("100 天前的数据应被裁剪")
	}
	if _, ok := s.aggr["new:1"]; !ok {
		t.Fatal("近期数据不应被裁剪")
	}

	s3, _ := Open(s.path)
	if err := s3.ClearAll(); err != nil {
		t.Fatal(err)
	}
	if n := len(s3.aggr); n != 0 {
		t.Fatalf("ClearAll 后应为空, got %d", n)
	}
	s4, _ := Open(path)
	if n := len(s4.aggr); n != 0 {
		t.Fatalf("ClearAll 落盘后重开应为空, got %d", n)
	}
}

// Add 的入参防御：空地址 / 非正时长不聚合。
func TestAddGuards(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	s.Add("", time.Hour, now)
	s.Add("a:1", 0, now)
	s.Add("a:1", -time.Minute, now)
	if n := len(s.aggr); n != 0 {
		t.Fatalf("非法入参不应聚合: %v", s.aggr)
	}
}

func mondayOf(now time.Time) time.Time {
	m := now
	for m.Weekday() != time.Monday {
		m = m.AddDate(0, 0, -1)
	}
	return m
}
