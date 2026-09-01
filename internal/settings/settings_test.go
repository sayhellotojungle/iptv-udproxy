package settings

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "settings.json"))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := s.Get()
	if cfg.PPPoEUnit != 60 || cfg.IdleTimeout != 300 || !cfg.RouteGuard {
		t.Fatalf("默认配置错误: %+v", cfg)
	}
}

func TestCorruptLoadBacksUpAndKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	if err := s.Load(); err != nil {
		t.Fatalf("损坏文件不应报错（应备份后回默认）: %v", err)
	}
	if s.Get().PPPoEUnit != 60 {
		t.Fatal("损坏后应使用默认配置")
	}
	entries, _ := os.ReadDir(dir)
	backups := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "settings.json.corrupt-") {
			backups++
		}
	}
	if backups != 1 {
		t.Fatalf("损坏文件应被备份, got %d 个备份", backups)
	}
}

func TestUpdateRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	s := New(path)
	want := Config{
		PPPoEUser: "user", PPPoEPass: `p@ss"word`, PPPoEUnit: 1, PPPoEEnable: true,
		OnDemand: true, IdleTimeout: 120,
		McastIface: "eth0", PPPoEIface: "eth1", RouteGuard: false,
	}
	if err := s.Update(want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" { // Windows 下 Go 不精确映射权限位
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("settings.json 应为 0600, got %o", perm)
		}
	}

	s2 := New(path)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); got != want {
		t.Fatalf("往返不一致:\n want %+v\n got  %+v", want, got)
	}
}

func TestUpdateCallsOnChangeSynchronously(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "settings.json"))
	called := make(chan Config, 1)
	s.SetOnChange(func(c Config) { called <- c })

	if err := s.Update(Config{PPPoEUnit: 42}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-called:
		if c.PPPoEUnit != 42 {
			t.Fatalf("回调参数错误: %+v", c)
		}
	default:
		t.Fatal("onChange 应在 Update 返回前同步调用")
	}
}
