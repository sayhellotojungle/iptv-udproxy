package storeutil

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type sample struct {
	A int    `json:"a"`
	B string `json:"b"`
}

func TestWriteLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	if err := WriteJSON(path, sample{1, "hi"}, 0o644); err != nil {
		t.Fatal(err)
	}
	var got sample
	corr, err := LoadJSON(path, &got)
	if err != nil || corr {
		t.Fatalf("err=%v corrupted=%v", err, corr)
	}
	if got != (sample{1, "hi"}) {
		t.Fatalf("round trip 不一致: %+v", got)
	}
}

func TestLoadMissingAndEmpty(t *testing.T) {
	var got sample
	corr, err := LoadJSON(filepath.Join(t.TempDir(), "nope.json"), &got)
	if err != nil || corr {
		t.Fatalf("文件不存在: err=%v corrupted=%v", err, corr)
	}
	if got != (sample{}) {
		t.Fatalf("缺失应保持零值: %+v", got)
	}

	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	corr, err = LoadJSON(path, &got)
	if err != nil || corr {
		t.Fatalf("空文件: err=%v corrupted=%v", err, corr)
	}
}

func TestLoadCorruptBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := os.WriteFile(path, []byte("[1,2,"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got sample
	corr, err := LoadJSON(path, &got)
	if err != nil {
		t.Fatalf("损坏不应返回 err（应走备份）: %v", err)
	}
	if !corr {
		t.Fatal("应报告 corrupted=true")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("损坏文件应被重命名移走")
	}
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "x.json.corrupt-") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("应有 1 个备份, got %d", n)
	}
}

// TestWriteJSONConcurrentSamePath 回归：同一 path 并发落盘不得互相截断临时文件
// （此前固定用 <path>.tmp，两个写者会交替覆写同一临时文件，rename 后得到半截 JSON）。
func TestWriteJSONConcurrentSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	body := strings.Repeat("x", 4096)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := WriteJSON(path, sample{n, body}, 0o644); err != nil {
				t.Errorf("并发写失败: %v", err)
			}
		}(i)
	}
	wg.Wait()

	var got sample
	corr, err := LoadJSON(path, &got)
	if err != nil || corr {
		t.Fatalf("并发写后文件应完整: err=%v corrupted=%v", err, corr)
	}
	if got.B != body {
		t.Fatalf("内容被截断: len=%d", len(got.B))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("临时文件残留: %s", e.Name())
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("落盘权限应为 0644, got %v err=%v", fi.Mode(), err)
	}
}
