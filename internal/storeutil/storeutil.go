// Package storeutil 提供 JSON 文件持久化的统一原子写与损坏容错加载。
//
// 约定：所有 JSON store 一律"内存 + mutex + tmp+rename 原子写"；文件损坏时
// 重命名备份为 <path>.corrupt-<unixts>、记录显著告警并以空配置起步，
// 不 fatal、不用内存脏值覆盖磁盘。
package storeutil

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

// WriteJSON 将 v 以缩进 JSON 原子写入 path（tmp + rename）。
func WriteJSON(path string, v interface{}, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadJSON 从 path 加载 JSON 到 v：
//   - 文件不存在或为空：返回 (false, nil)，v 保持零值
//   - 文件损坏：重命名备份为 <path>.corrupt-<unixns> 并记录告警日志，
//     返回 (true, nil)，v 保持零值（调用方以空配置起步）
//   - 读取 IO 错误：返回 (false, err)
//
// v 可实现自定义 UnmarshalJSON 做格式迁移（如新旧格式兼容）。
func LoadJSON(path string, v interface{}) (corrupted bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(data) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
		if rerr := os.Rename(path, backup); rerr != nil {
			log.Printf("[storeutil] %s 损坏且备份失败: %v，直接删除以恢复启动", path, rerr)
			if derr := os.Remove(path); derr != nil {
				return false, fmt.Errorf("%s 损坏且无法备份: %w", path, derr)
			}
		} else {
			log.Printf("[storeutil] %s 损坏，已备份为 %s，本次以空配置起步", path, backup)
		}
		return true, nil
	}
	return false, nil
}
