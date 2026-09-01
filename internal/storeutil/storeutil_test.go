package storeutil

import (
	"os"
	"path/filepath"
	"strings"
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
