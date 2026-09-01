package config

import "testing"

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("LISTEN", "")
	t.Setenv("DATA_DIR", "")
	c := FromEnv()
	if c.Listen != ":18888" || c.DataDir != "/data" {
		t.Fatalf("默认值错误: %+v", c)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("LISTEN", "192.168.1.10:9000")
	t.Setenv("DATA_DIR", "/srv/iptv")
	c := FromEnv()
	if c.Listen != "192.168.1.10:9000" || c.DataDir != "/srv/iptv" {
		t.Fatalf("环境变量应生效: %+v", c)
	}
}
