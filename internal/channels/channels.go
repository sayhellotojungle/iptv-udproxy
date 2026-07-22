// Package channels 管理频道名称映射，支持 m3u 导入导出。
package channels

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Channel 频道信息。
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"` // 组播地址，如 239.254.96.96:8550
	Logo    string `json:"logo,omitempty"`
	Group   string `json:"group,omitempty"` // 分组，如 央视、卫视
}

// Store 频道存储。
type Store struct {
	mu       sync.RWMutex
	channels map[string]*Channel // key = address
	path     string
	counter  int64
}

// Open 打开或创建频道存储。
func Open(path string) (*Store, error) {
	s := &Store{
		channels: make(map[string]*Channel),
		path:     path,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []Channel
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for i := range list {
		s.channels[list[i].Address] = &list[i]
	}
	s.reindex()
	return s, nil
}

// reindex 重新计算 counter。
func (s *Store) reindex() {
	s.counter = 0
	for _, ch := range s.channels {
		var num int64
		if _, err := fmt.Sscanf(ch.ID, "c%d", &num); err == nil && num > s.counter {
			s.counter = num
		}
	}
}

// List 返回所有频道，按分组和名称排序。
func (s *Store) List() []Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Channel, 0, len(s.channels))
	for _, ch := range s.channels {
		out = append(out, *ch)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get 根据地址获取频道。
func (s *Store) Get(address string) *Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channels[address]
}

// GetName 根据地址获取频道名称，无名称返回空字符串。
func (s *Store) GetName(address string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ch, ok := s.channels[address]; ok && ch.Name != "" {
		return ch.Name
	}
	return ""
}

// Add 添加频道。
func (s *Store) Add(ch Channel) (Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ch.Address == "" {
		return Channel{}, fmt.Errorf("地址不能为空")
	}
	if ch.Name == "" {
		return Channel{}, fmt.Errorf("名称不能为空")
	}
	if _, exists := s.channels[ch.Address]; exists {
		return Channel{}, fmt.Errorf("地址 %s 已存在", ch.Address)
	}

	s.counter++
	ch.ID = fmt.Sprintf("c%d", s.counter)
	s.channels[ch.Address] = &ch
	return *s.channels[ch.Address], s.save()
}

// Update 更新频道。
func (s *Store) Update(id string, ch Channel) (Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var oldAddr string
	for addr, c := range s.channels {
		if c.ID == id {
			oldAddr = addr
			break
		}
	}
	if oldAddr == "" {
		return Channel{}, fmt.Errorf("频道 %s 不存在", id)
	}

	if ch.Address != oldAddr {
		if _, exists := s.channels[ch.Address]; exists {
			return Channel{}, fmt.Errorf("地址 %s 已存在", ch.Address)
		}
		delete(s.channels, oldAddr)
	}

	ch.ID = id
	s.channels[ch.Address] = &ch
	return *s.channels[ch.Address], s.save()
}

// Delete 删除频道。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for addr, ch := range s.channels {
		if ch.ID == id {
			delete(s.channels, addr)
			return s.save()
		}
	}
	return fmt.Errorf("频道 %s 不存在", id)
}

// ImportM3U 导入 m3u 文件内容，返回导入数量。
func (s *Store) ImportM3U(content string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	scanner := bufio.NewScanner(strings.NewReader(content))

	var currentName string
	var currentGroup string
	var currentLogo string

	reName := regexp.MustCompile(`tvg-name="([^"]*)"`)
	reGroup := regexp.MustCompile(`group-title="([^"]*)"`)
	reLogo := regexp.MustCompile(`tvg-logo="([^"]*)"`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "#EXTINF:") {
			// 解析频道信息
			currentName = ""
			currentGroup = ""
			currentLogo = ""

			if m := reName.FindStringSubmatch(line); m != nil {
				currentName = m[1]
			}
			if m := reGroup.FindStringSubmatch(line); m != nil {
				currentGroup = m[1]
			}
			if m := reLogo.FindStringSubmatch(line); m != nil {
				currentLogo = m[1]
			}

			// 如果 tvg-name 为空，尝试从逗号后面提取
			if currentName == "" {
				if idx := strings.LastIndex(line, ","); idx != -1 {
					currentName = strings.TrimSpace(line[idx+1:])
				}
			}
			continue
		}

		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		// 这是 URL 行，提取组播地址
		address := extractMulticastAddr(line)
		if address == "" || currentName == "" {
			currentName = ""
			continue
		}

		// 添加或更新频道
		if _, exists := s.channels[address]; !exists {
			s.counter++
			s.channels[address] = &Channel{
				ID:      fmt.Sprintf("c%d", s.counter),
				Name:    currentName,
				Address: address,
				Logo:    currentLogo,
				Group:   currentGroup,
			}
			count++
		}

		currentName = ""
	}

	s.save()
	return count
}

// extractMulticastAddr 从 URL 中提取组播地址。
func extractMulticastAddr(url string) string {
	// 格式: http://host:port/rtp/239.x.x.x:port
	re := regexp.MustCompile(`/rtp/(\d+\.\d+\.\d+\.\d+:\d+)`)
	if m := re.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
}

// GenerateM3U 生成 m3u 格式内容。
func (s *Store) GenerateM3U(host string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")

	// 按分组排序
	groups := make(map[string][]*Channel)
	var groupOrder []string
	for _, ch := range s.channels {
		group := ch.Group
		if group == "" {
			group = "未分类"
		}
		if _, exists := groups[group]; !exists {
			groupOrder = append(groupOrder, group)
		}
		groups[group] = append(groups[group], ch)
	}
	sort.Strings(groupOrder)

	id := 1
	for _, group := range groupOrder {
		channels := groups[group]
		sort.Slice(channels, func(i, j int) bool {
			return channels[i].Name < channels[j].Name
		})
		for _, ch := range channels {
			url := fmt.Sprintf("http://%s/rtp/%s", host, ch.Address)
			logo := ""
			if ch.Logo != "" {
				logo = fmt.Sprintf(` tvg-logo="%s"`, ch.Logo)
			}
			sb.WriteString(fmt.Sprintf(`#EXTINF:-1 tvg-id="%d" tvg-name="%s"%s group-title="%s", %s`, id, ch.Name, logo, group, ch.Name))
			sb.WriteString("\n")
			sb.WriteString(url)
			sb.WriteString("\n")
			id++
		}
	}

	return sb.String()
}

// save 保存到文件（调用方需持有锁）。
func (s *Store) save() error {
	list := make([]Channel, 0, len(s.channels))
	for _, ch := range s.channels {
		list = append(list, *ch)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}
