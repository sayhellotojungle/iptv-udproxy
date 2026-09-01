package channels

import (
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const sampleM3U = `#EXTM3U
#EXTINF:-1 tvg-id="cctv1" tvg-name="CCTV-1" tvg-logo="http://example.com/1.png" group-title="央视", CCTV-1综合
http://192.168.1.1:18888/rtp/239.254.96.96:8550
#EXTINF:-1 tvg-name="" group-title="卫视", 湖南卫视
http://192.168.1.1:18888/udp/239.69.1.123:10376
#EXTINF:-1, 无组播地址的频道
http://192.168.1.1:18888/movie/index.m3u8
some-junk-line
#EXTINF:-1 tvg-name="孤儿项", 没有 URL 的 EXTINF
`

func TestImportM3U(t *testing.T) {
	s := openTemp(t)
	if n := s.ImportM3U(sampleM3U); n != 2 {
		t.Fatalf("应导入 2 个频道, got %d", n)
	}

	c1, ok := s.Get("239.254.96.96:8550")
	if !ok || c1.Name != "CCTV-1" || c1.Group != "央视" || c1.Logo != "http://example.com/1.png" {
		t.Fatalf("rtp 频道信息错误: %+v ok=%v", c1, ok)
	}
	c2, ok := s.Get("239.69.1.123:10376")
	if !ok || c2.Name != "湖南卫视" || c2.Group != "卫视" {
		t.Fatalf("udp 频道（逗号回退名）错误: %+v ok=%v", c2, ok)
	}
	if got := s.GetName("239.254.96.96:8550"); got != "CCTV-1" {
		t.Fatalf("GetName = %q", got)
	}
	if _, ok := s.Get("239.9.9.9:1"); ok {
		t.Fatal("不存在的地址不应命中")
	}

	// 重复导入不覆盖、不新增
	if n := s.ImportM3U(sampleM3U); n != 0 {
		t.Fatalf("重复导入应无新增, got %d", n)
	}
	if n := len(s.List()); n != 2 {
		t.Fatalf("重复导入后应为 2 条, got %d", n)
	}
}

func TestAddUpdateDelete(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Add(Channel{Name: "", Address: "239.1.1.1:100"}); err == nil {
		t.Fatal("空名称应拒绝")
	}
	if _, err := s.Add(Channel{Name: "A", Address: ""}); err == nil {
		t.Fatal("空地址应拒绝")
	}
	created, err := s.Add(Channel{Name: "A", Address: "239.1.1.1:100", Group: "G1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Channel{Name: "B", Address: "239.1.1.1:100"}); err == nil {
		t.Fatal("重复地址应拒绝")
	}

	// 改地址：旧 key 移除、新 key 写入
	changed, err := s.Update(created.ID, Channel{Name: "A2", Address: "239.2.2.2:200", Group: "G1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("239.1.1.1:100"); ok {
		t.Fatal("旧地址应已移除")
	}
	if got, ok := s.Get("239.2.2.2:200"); !ok || got.Name != "A2" || got.ID != created.ID {
		t.Fatalf("新地址信息错误: %+v", got)
	}
	// 以相同地址保存（Web 界面保存的常态）应允许
	if _, err := s.Update(changed.ID, Channel{Name: "A2", Address: "239.2.2.2:200", Group: "G1"}); err != nil {
		t.Fatalf("同地址保存应允许: %v", err)
	}
	// 改为被其他频道占用的地址应拒绝
	other, err := s.Add(Channel{Name: "B", Address: "239.3.3.3:300", Group: "G1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(changed.ID, Channel{Name: "C", Address: "239.3.3.3:300"}); err == nil {
		t.Fatal("改为已占用地址应拒绝")
	}

	if err := s.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(other.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("删除后应为空, got %d", n)
	}
	if err := s.Delete(created.ID); err == nil {
		t.Fatal("重复删除应报错")
	}
}

func TestGenerateM3U(t *testing.T) {
	s := openTemp(t)
	s.ImportM3U(sampleM3U)
	out := s.GenerateM3U("192.168.1.1:18888")

	for _, want := range []string{
		"#EXTM3U",
		"http://192.168.1.1:18888/rtp/239.254.96.96:8550",
		"http://192.168.1.1:18888/rtp/239.69.1.123:10376", // udp 导入的频道导出统一走 /rtp/
		`group-title="央视"`,
		`tvg-logo="http://example.com/1.png"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("m3u 输出缺少 %q:\n%s", want, out)
		}
	}
}
