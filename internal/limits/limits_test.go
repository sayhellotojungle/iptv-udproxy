package limits

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"iptv-udpproxy/internal/stats"
)

type env struct {
	ls *Store
	st *stats.Store
}

func openEnv(t *testing.T) env {
	t.Helper()
	dir := t.TempDir()
	ls, err := Open(filepath.Join(dir, "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := stats.Open(filepath.Join(dir, "watchtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return env{ls: ls, st: st}
}

func mondayOf(now time.Time) time.Time {
	m := now
	for m.Weekday() != time.Monday {
		m = m.AddDate(0, 0, -1)
	}
	return m
}

func TestCheckLimitDaily(t *testing.T) {
	e := openEnv(t)
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.Local)
	// 当天历史 50 分钟
	e.st.Add("239.0.0.1:1000", 50*time.Minute, time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local))

	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.1:1000", DailyMax: 60 * 60}); err != nil {
		t.Fatal(err)
	}

	res := e.ls.CheckLimit("239.0.0.1:1000", e.st, 10*time.Minute, now)
	if res == nil || !res.Exceeded || res.LimitType != "daily" || res.MaxSec != 3600 {
		t.Fatalf("50min+10min 应触发每日限制, got %+v", res)
	}
	res = e.ls.CheckLimit("239.0.0.1:1000", e.st, 5*time.Minute, now)
	if res != nil {
		t.Fatalf("50min+5min 不应超限, got %+v", res)
	}
	if res := e.ls.CheckLimit("239.0.0.2:2000", e.st, time.Hour, now); res != nil {
		t.Fatalf("无限制规则的频道不应超限, got %+v", res)
	}
	// 停用规则不生效
	lims := e.ls.ListLimits()
	upd := lims[0]
	upd.Enabled = false
	if _, err := e.ls.UpdateLimit(upd.ID, upd); err != nil {
		t.Fatal(err)
	}
	if res := e.ls.CheckLimit("239.0.0.1:1000", e.st, time.Hour, now); res != nil {
		t.Fatalf("停用后不应超限, got %+v", res)
	}
}

func TestCheckLimitWeekly(t *testing.T) {
	e := openEnv(t)
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.Local)
	monday := mondayOf(now)

	// A：本周 3h；B：本周 3h + 上周 5h
	e.st.Add("239.0.0.1:1000", 3*time.Hour, monday)
	e.st.Add("239.0.0.2:1000", 3*time.Hour, monday)
	e.st.Add("239.0.0.2:1000", 5*time.Hour, monday.AddDate(0, 0, -9))

	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.1:1000", WeeklyMax: 14400}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.2:1000", WeeklyMax: 20000}); err != nil {
		t.Fatal(err)
	}

	res := e.ls.CheckLimit("239.0.0.1:1000", e.st, 100*time.Minute, now)
	// 10800 + 6000 = 16800 >= 14400
	if res == nil || res.LimitType != "weekly" {
		t.Fatalf("A 应触发每周限制, got %+v", res)
	}
	res = e.ls.CheckLimit("239.0.0.2:1000", e.st, 100*time.Minute, now)
	// 本周 10800 + 6000 = 16800 < 20000（上周 5h 不得计入，否则 21600 >= 20000）
	if res != nil {
		t.Fatalf("B 不应超限（上周时长不得计入）, got %+v", res)
	}
}

func TestPickReplacement(t *testing.T) {
	e := openEnv(t)

	if _, ok := e.ls.PickReplacement("239.0.0.1:1000"); ok {
		t.Fatal("空池应返回 false")
	}
	if err := e.ls.SetPool(PoolConfig{Addresses: []string{"239.0.0.1"}}); err == nil {
		t.Fatal("无端口地址应拒绝")
	}
	// 池中只有当前地址：仍返回当前地址，避免无源可切
	if err := e.ls.SetPool(PoolConfig{Addresses: []string{"239.0.0.1:1000"}}); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.ls.PickReplacement("239.0.0.1:1000"); !ok || got != "239.0.0.1:1000" {
		t.Fatalf("池仅含当前地址应返回当前地址, got %s ok=%v", got, ok)
	}

	if err := e.ls.SetPool(PoolConfig{Addresses: []string{"239.0.0.1:1000", "239.0.0.3:2000", "239.0.0.4:3000"}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"239.0.0.3:2000": true, "239.0.0.4:3000": true}
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		got, ok := e.ls.PickReplacement("239.0.0.1:1000")
		if !ok || !want[got] {
			t.Fatalf("第 %d 次抽取 %s ok=%v 非法", i, got, ok)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatalf("60 次抽取应出现至少 2 个备选, got %v", seen)
	}
}

func TestAddLimitValidation(t *testing.T) {
	e := openEnv(t)
	invalid := []Limit{
		{Address: ""},
		{Address: "no-port"},
		{Address: "239.0.0.1:1000", DailyMax: -1},
		{Address: "239.0.0.1:1000"}, // 双 0
	}
	for i, l := range invalid {
		if _, err := e.ls.AddLimit(l); err == nil {
			t.Errorf("case %d: 应拒绝 %+v", i, l)
		}
	}
	if n := len(e.ls.ListLimits()); n != 0 {
		t.Fatalf("拒绝后不应有规则: %d", n)
	}
}

func TestOpenLegacyFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.json")
	legacy := `[{"id":"x","address":"239.5.5.5:1000","daily_max":3600,"weekly_max":0,"enabled":true}]`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lims := s.ListLimits()
	if len(lims) != 1 || lims[0].Address != "239.5.5.5:1000" || lims[0].DailyMax != 3600 {
		t.Fatalf("旧格式加载失败: %v", lims)
	}
	if n := len(s.GetPool().Addresses); n != 0 {
		t.Fatalf("旧格式不应带池: %v", s.GetPool())
	}
}

// 空规则+空池是合法的落盘内容（用户删光规则），重开不应报错、不应产生 corrupt 备份。
func TestOpenEmptyNewFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	if err := os.WriteFile(path, []byte(`{"limits": [], "pool": {"addresses": []}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("空新格式文件不应报错: %v", err)
	}
	if n := len(s.ListLimits()); n != 0 {
		t.Fatalf("应为 0 条规则, got %d", n)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "limits.json.corrupt-") {
			t.Fatalf("合法空文件不应被备份: %s", e.Name())
		}
	}
}
