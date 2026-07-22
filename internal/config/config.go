// Package config 负责从环境变量加载运行配置。
package config

import (
	"os"
	"strconv"
)

// Config 为全局配置。
type Config struct {
	Listen  string // HTTP 监听地址，例如 ":18888" 或 "192.168.67.1:18888"
	DataDir string // 规则等数据的持久化目录
}

// FromEnv 从环境变量构造配置，未设置的项使用默认值。
func FromEnv() *Config {
	return &Config{
		Listen:  envStr("LISTEN", ":18888"),
		DataDir: envStr("DATA_DIR", "/data"),
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
