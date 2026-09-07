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

func TestCheckLimitWeekday(t *testing.T) {
	e := openEnv(t)
	// 2026-07-20 为周一
	monday := time.Date(2026, 7, 20, 20, 0, 0, 0, time.Local)
	tuesday := monday.AddDate(0, 0, 1)

	// 每日上限 1h，周一自定义 2h
	wd := [7]int{}
	wd[time.Monday] = 2 * 60 * 60
	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.1:1000", DailyMax: 60 * 60, WeekdayMax: wd}); err != nil {
		t.Fatal(err)
	}

	e.st.Add("239.0.0.1:1000", 50*time.Minute, time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local))

	// 周一走自定义 2h：50min+20min=70min < 2h 不超限（按每日 1h 则已超限）
	if res := e.ls.CheckLimit("239.0.0.1:1000", e.st, 20*time.Minute, monday); res != nil {
		t.Fatalf("周一应使用周几上限，70min 不应超限, got %+v", res)
	}
	// 周一 50min+80min=130min >= 2h 触发，MaxSec 为周几上限，类型上报 weekday
	res := e.ls.CheckLimit("239.0.0.1:1000", e.st, 80*time.Minute, monday)
	if res == nil || !res.Exceeded || res.LimitType != "weekday" || res.DayName != "周一" || res.MaxSec != 7200 {
		t.Fatalf("周一 130min 应触发周几上限, got %+v", res)
	}

	// 周二未自定义 → 仍按每日 1h 执行
	e.st.Add("239.0.0.1:1000", 50*time.Minute, time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))
	res = e.ls.CheckLimit("239.0.0.1:1000", e.st, 20*time.Minute, tuesday)
	if res == nil || !res.Exceeded || res.LimitType != "daily" || res.MaxSec != 3600 {
		t.Fatalf("周二未自定义应按每日上限执行, got %+v", res)
	}
}

// 跨窗口流：实时时长按实际落入窗口（自然日 / ISO 周）的部分计入，不整段重复计算。
func TestCheckLimitCrossWindow(t *testing.T) {
	e := openEnv(t)

	// 跨午夜：流周一 23:00 开播，现周二 01:00（已播 2h，今日窗口内仅 1h）
	tue := time.Date(2026, 7, 21, 1, 0, 0, 0, time.Local)
	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.1:1000", DailyMax: 90 * 60}); err != nil {
		t.Fatal(err)
	}
	if res := e.ls.CheckLimit("239.0.0.1:1000", e.st, 2*time.Hour, tue); res != nil {
		t.Fatalf("跨午夜流应只计今日 1h，90min 每日上限不应超限, got %+v", res)
	}

	// 跨周：流上周日 23:00 开播，现周一 01:00（已播 2h，本周窗口内仅 1h）
	mon := time.Date(2026, 7, 20, 1, 0, 0, 0, time.Local)
	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.2:1000", WeeklyMax: 90 * 60}); err != nil {
		t.Fatal(err)
	}
	if res := e.ls.CheckLimit("239.0.0.2:1000", e.st, 2*time.Hour, mon); res != nil {
		t.Fatalf("跨周流应只计本周 1h，90min 每周上限不应超限, got %+v", res)
	}
}

// 仅设置周几上限（每日/每周均为 0）：只限指定星期，其余不限。
func TestCheckLimitWeekdayOnly(t *testing.T) {
	e := openEnv(t)
	saturday := time.Date(2026, 7, 18, 20, 0, 0, 0, time.Local) // 周六
	monday := time.Date(2026, 7, 20, 20, 0, 0, 0, time.Local)   // 周一

	wd := [7]int{}
	wd[time.Saturday] = 3600
	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.1:1000", WeekdayMax: wd}); err != nil {
		t.Fatalf("仅设置周几上限应合法: %v", err)
	}

	e.st.Add("239.0.0.1:1000", 50*time.Minute, time.Date(2026, 7, 18, 10, 0, 0, 0, time.Local))

	res := e.ls.CheckLimit("239.0.0.1:1000", e.st, 20*time.Minute, saturday)
	if res == nil || !res.Exceeded || res.MaxSec != 3600 {
		t.Fatalf("周六应触发周几上限, got %+v", res)
	}
	res = e.ls.CheckLimit("239.0.0.1:1000", e.st, 3*time.Hour, monday)
	if res != nil {
		t.Fatalf("周一未自定义且每日/每周为 0 不应超限, got %+v", res)
	}
}

// 连续限制：两次观看间隔不超过休息时长视为连续并累加；
// 累计达到上限即超限；间隔超过休息时长则链条断开，只计当前会话。
func TestCheckLimitContinuous(t *testing.T) {
	e := openEnv(t)
	t0 := time.Date(2026, 7, 20, 20, 0, 0, 0, time.Local)
	addr := "239.0.0.1:1000"

	if _, err := e.ls.AddLimit(Limit{
		Address:           addr,
		ContinuousMax:     3600, // 1 小时
		RestDuration:      1800, // 30 分钟
		ContinuousEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// 会话1：20:00-20:30
	if res := e.ls.CheckLimit(addr, e.st, 15*time.Minute, t0.Add(15*time.Minute)); res != nil {
		t.Fatalf("观看 15 分钟不应超限, got %+v", res)
	}
	e.ls.EndSession(addr, t0, t0.Add(30*time.Minute))

	// 会话2：20:35-20:55，间隔 5 分钟 ≤ 30 分钟 → 连续，累计 40 分钟不超限
	if res := e.ls.CheckLimit(addr, e.st, 10*time.Minute, t0.Add(45*time.Minute)); res != nil {
		t.Fatalf("连续 40 分钟不应超限, got %+v", res)
	}
	e.ls.EndSession(addr, t0.Add(35*time.Minute), t0.Add(55*time.Minute))

	// 会话3：21:00 起，间隔 5 分钟 → 连续，50 分钟 + 10 分钟 = 60 分钟触顶
	if res := e.ls.CheckLimit(addr, e.st, 5*time.Minute, t0.Add(65*time.Minute)); res != nil {
		t.Fatalf("连续 55 分钟不应超限, got %+v", res)
	}
	res := e.ls.CheckLimit(addr, e.st, 10*time.Minute, t0.Add(70*time.Minute))
	if res == nil || !res.Exceeded || res.LimitType != "continuous" || res.MaxSec != 3600 || res.CurrentSec != 3600 {
		t.Fatalf("连续 60 分钟应触发连续限制, got %+v", res)
	}
	e.ls.EndSession(addr, t0.Add(60*time.Minute), t0.Add(70*time.Minute))

	// 会话4：21:40 起，间隔恰好 30 分钟（=休息时长，未超过）→ 仍连续，累计已触顶
	if res := e.ls.CheckLimit(addr, e.st, 0, t0.Add(100*time.Minute)); res == nil || res.LimitType != "continuous" {
		t.Fatalf("间隔等于休息时长仍应视为连续且超限, got %+v", res)
	}

	// 会话5：21:50 起，间隔 40 分钟 > 30 分钟 → 链条断开，只计当前会话
	if res := e.ls.CheckLimit(addr, e.st, 0, t0.Add(110*time.Minute)); res != nil {
		t.Fatalf("间隔超过休息时长后链条应断开、不超限, got %+v", res)
	}
}

// 连续状态持久化：重启后休息时长的判定仍能引用上次会话结束时间。
func TestContinuousSessionPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	ls, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 7, 20, 20, 0, 0, 0, time.Local)
	addr := "239.0.0.1:1000"
	if _, err := ls.AddLimit(Limit{
		Address:           addr,
		ContinuousMax:     3600,
		RestDuration:      1800,
		ContinuousEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	ls.EndSession(addr, t0, t0.Add(30*time.Minute))

	ls2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := stats.Open(filepath.Join(dir, "watchtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 重启后 21:00 再观看（间隔恰为休息时长，仍连续），实时 30 分钟后累计触顶
	res := ls2.CheckLimit(addr, st, 30*time.Minute, t0.Add(90*time.Minute))
	if res == nil || res.LimitType != "continuous" || res.CurrentSec != 3600 {
		t.Fatalf("重启后休息时长内再观看应连续计满并超限, got %+v", res)
	}
}

// 无启用限制规则的频道结束会话后不保留状态，防止状态表无限增长。
func TestEndSessionPrunesWithoutLimit(t *testing.T) {
	e := openEnv(t)
	t0 := time.Date(2026, 7, 20, 20, 0, 0, 0, time.Local)
	e.ls.EndSession("239.0.0.1:1000", t0, t0.Add(10*time.Minute))
	if got := e.ls.sessions["239.0.0.1:1000"]; got != nil {
		t.Fatalf("无限制规则的频道不应保留连续状态, got %+v", got)
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
		{Address: "239.0.0.1:1000"}, // 每日/每周/周几/连续全未设置
		{Address: "239.0.0.1:1000", WeekdayMax: [7]int{0, 0, 0, 0, 0, -1, 0}},
		{Address: "239.0.0.1:1000", ContinuousMax: -5},
		{Address: "239.0.0.1:1000", RestDuration: -1},
		{Address: "239.0.0.1:1000", ContinuousEnabled: true, ContinuousMax: 0}, // 启用连续限制但未设上限
	}
	for i, l := range invalid {
		if _, err := e.ls.AddLimit(l); err == nil {
			t.Errorf("case %d: 应拒绝 %+v", i, l)
		}
	}
	if n := len(e.ls.ListLimits()); n != 0 {
		t.Fatalf("拒绝后不应有规则: %d", n)
	}
	// 仅设置周几上限是合法规则
	wd := [7]int{}
	wd[time.Saturday] = 1800
	if _, err := e.ls.AddLimit(Limit{Address: "239.0.0.1:1000", WeekdayMax: wd}); err != nil {
		t.Fatalf("仅设置周几上限应合法: %v", err)
	}
	// 仅启用连续限制也是合法规则
	if _, err := e.ls.AddLimit(Limit{
		Address:           "239.0.0.2:1000",
		ContinuousMax:     1800,
		RestDuration:      600,
		ContinuousEnabled: true,
	}); err != nil {
		t.Fatalf("仅启用连续限制应合法: %v", err)
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
