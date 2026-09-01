package rules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestActiveAtWindow(t *testing.T) {
	r := Rule{From: "239.0.0.1:1000", To: "239.0.0.2:1000", Start: "19:00", End: "20:00"}
	base := time.Date(2026, 7, 20, 0, 0, 0, 0, time.Local)
	cases := []struct {
		h, m int
		want bool
	}{
		{18, 59, false},
		{19, 0, true},
		{19, 30, true},
		{20, 0, false}, // 结束时刻不含
		{12, 0, false},
	}
	for _, c := range cases {
		if got := r.activeAt(base.Add(time.Duration(c.h*60+c.m) * time.Minute)); got != c.want {
			t.Errorf("activeAt(%02d:%02d) = %v, want %v", c.h, c.m, got, c.want)
		}
	}
}

func TestActiveAtAllDay(t *testing.T) {
	r := Rule{Start: "08:00", End: "08:00"} // Start==End 表示全天
	if !r.activeAt(time.Date(2026, 7, 20, 3, 15, 0, 0, time.Local)) {
		t.Error("全天窗口（start==end）应当生效")
	}
}

func TestActiveAtMidnightCrossing(t *testing.T) {
	r := Rule{Start: "22:00", End: "02:00"}
	d1 := time.Date(2026, 7, 19, 0, 0, 0, 0, time.Local)
	d2 := d1.AddDate(0, 0, 1)

	if !r.activeAt(d1.Add(23*time.Hour + 30*time.Minute)) {
		t.Error("跨午夜窗口当晚 23:30 应生效")
	}
	if !r.activeAt(d2.Add(1*time.Hour + 30*time.Minute)) {
		t.Error("跨午夜窗口次日 01:30 应生效")
	}
	if r.activeAt(d1.Add(3 * time.Hour)) {
		t.Error("窗口外（次日 03:00）不应生效")
	}
	if r.activeAt(d1.Add(21 * time.Hour)) {
		t.Error("窗口外（当晚 21:00）不应生效")
	}
}

// 跨午夜窗口落在次日凌晨时，星期归属按窗口开始日（前一天）计算。
func TestMidnightCrossingDayAttribution(t *testing.T) {
	d1 := time.Date(2026, 7, 19, 0, 0, 0, 0, time.Local)
	d2 := d1.AddDate(0, 0, 1)

	r := Rule{Start: "22:00", End: "02:00", Days: []int{int(d1.Weekday())}}
	if !r.activeAt(d1.Add(23 * time.Hour)) {
		t.Error("开始日当晚 23:00 应生效")
	}
	if !r.activeAt(d2.Add(1 * time.Hour)) {
		t.Error("d2 凌晨 01:00 归属 d1 开始窗口，应生效")
	}

	// 星期限定为 d2 的星期：两段都不应生效
	r.Days = []int{int(d2.Weekday())}
	if r.activeAt(d2.Add(1 * time.Hour)) {
		t.Error("d2 凌晨 01:00 按开始日 d1 算星期，不满足限定，不应生效")
	}
	if r.activeAt(d1.Add(23 * time.Hour)) {
		t.Error("d1 当晚 23:00 不满足限定，不应生效")
	}
}

func TestResolve(t *testing.T) {
	s := openTemp(t)
	created, err := s.Add(Rule{
		Name: "夜间换源", Enabled: true,
		From: "239.69.1.107:10280", To: "239.69.1.108:10280",
		Start: "19:00", End: "20:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	in := time.Date(2026, 7, 20, 19, 30, 0, 0, time.Local)
	if got := s.Resolve("239.69.1.107:10280", in); got != created.To {
		t.Fatalf("窗口内应解析到 %s, got %s", created.To, got)
	}
	out := time.Date(2026, 7, 20, 21, 0, 0, 0, time.Local)
	if got := s.Resolve("239.69.1.107:10280", out); got != "239.69.1.107:10280" {
		t.Fatalf("窗口外应原样返回, got %s", got)
	}
	if got := s.Resolve("239.69.9.9:9999", in); got != "239.69.9.9:9999" {
		t.Fatalf("无关地址不应受影响, got %s", got)
	}

	// 停用规则不生效
	if _, err := s.Update(created.ID, Rule{Name: "x", Enabled: false, From: created.From, To: created.To, Start: "19:00", End: "20:00"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Resolve("239.69.1.107:10280", in); got != "239.69.1.107:10280" {
		t.Fatalf("停用规则不应生效, got %s", got)
	}
}

func TestAddRejectsInvalid(t *testing.T) {
	s := openTemp(t)
	invalid := []Rule{
		{From: "192.168.1.1:1000", To: "239.0.0.2:1000"},                                             // 非组播
		{From: "239.0.0.1:1000", To: "239.0.0.1:1000"},                                               // from==to
		{From: "239.0.0.1:1000", To: "239.0.0.2:1000", Start: "19:60", End: "9:00"},                  // 时间格式（分钟越界）
		{From: "239.0.0.1:1000", To: "239.0.0.2:1000", Start: "19:00", End: "20:00", Days: []int{7}}, // 星期越界
		{From: "bad", To: "239.0.0.2:1000"},
	}
	for i, r := range invalid {
		if _, err := s.Add(r); err == nil {
			t.Errorf("case %d: 应拒绝 %v", i, r)
		}
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("拒绝后不应有规则写入, got %d", n)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Add(Rule{Name: "a", Enabled: true, From: "239.1.1.1:100", To: "239.1.1.2:100", Start: "00:00", End: "23:59"})
	if err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List()
	if len(got) != 1 || !equalRules(got[0], created) {
		t.Fatalf("重开文件后规则不一致: %v", got)
	}

	if err := s2.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	s3, _ := Open(path)
	if n := len(s3.List()); n != 0 {
		t.Fatalf("删除未持久化: %d 条", n)
	}
	if _, err := s2.Update("no-such-id", Rule{Name: "x", From: "239.1.1.1:100", To: "239.1.1.2:100", Start: "00:00", End: "23:59"}); err == nil {
		t.Fatal("更新不存在的规则应报错")
	}
}

func TestOpenCorruptFileBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("损坏文件不应 fatal: %v", err)
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("损坏文件应以空规则起步, got %d", n)
	}
	entries, _ := os.ReadDir(dir)
	backups := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "rules.json.corrupt-") {
			backups++
		}
	}
	if backups != 1 {
		t.Fatalf("应有 1 个损坏备份, got %d", backups)
	}
}

// equalRules 深度比较规则（Rule 含切片，不能用 ==）。
func equalRules(a, b Rule) bool {
	return reflect.DeepEqual(a, b)
}
