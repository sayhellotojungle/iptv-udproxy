// Package mcast 负责加入组播组并读取 UDP 数据包。
//
// 每个 (Group, Port) 对应一个 reader；多个订阅同一组播地址的客户端
// 共享同一个 reader，reader 将每个包写入所有注册的 subscriber channel。
package mcast

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"

	"golang.org/x/net/ipv4"
)

// Packet 一条组播数据。
type Packet struct {
	Data []byte
	N    int
}

// ErrSubscriberOverflow 表示订阅者处理速度不足，继续转发会造成 MPEG-TS 静默丢包。
var ErrSubscriberOverflow = errors.New("组播订阅缓冲区已满")

// subscriber 订阅者元数据。
type subscriber struct {
	ch    chan<- Packet
	errCh chan error
}

// Reader 对应一个组播 (Group:Port)，加入组播组后循环读包并分发给订阅者。
type Reader struct {
	group net.IP
	port  int
	ifi   *net.Interface

	mu          sync.Mutex
	subscribers map[chan<- Packet]*subscriber
	conn        *net.UDPConn
	done        chan struct{}
}

// NewReader 创建一个组播 reader，不立即开始读取。
func NewReader(group string, port int, ifaceName string) (*Reader, error) {
	g := net.ParseIP(group)
	if g == nil || !g.IsMulticast() {
		return nil, fmt.Errorf("%s 不是有效的组播地址", group)
	}
	ifi, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("查找网口 %s 失败: %w", ifaceName, err)
	}
	return &Reader{
		group:       g,
		port:        port,
		ifi:         ifi,
		subscribers: make(map[chan<- Packet]*subscriber),
		done:        make(chan struct{}),
	}, nil
}

// Start 加入组播组并开始接收数据。
func (r *Reader) Start() error {
	addr := &net.UDPAddr{IP: r.group, Port: r.port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	// 设置 4MB UDP 接收缓冲区，避免高码率流丢包
	if err := conn.SetReadBuffer(4 * 1024 * 1024); err != nil {
		log.Printf("[mcast] 设置 UDP 接收缓冲区失败（将使用默认值）: %v", err)
	}
	p := ipv4.NewPacketConn(conn)
	if err := p.JoinGroup(r.ifi, &net.UDPAddr{IP: r.group}); err != nil {
		conn.Close()
		return fmt.Errorf("加入组播组 %s 失败: %w", r.group, err)
	}
	// 设置组播只从 IPTV 口接收
	if err := p.SetMulticastInterface(r.ifi); err != nil {
		conn.Close()
		return fmt.Errorf("设置组播接口失败: %w", err)
	}
	r.conn = conn
	go r.readLoop()
	return nil
}

// Stop 停止读取并释放资源。
func (r *Reader) Stop() {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	if r.conn != nil {
		r.conn.Close()
	}
}

// Subscribe 注册一个订阅者 channel；返回取消函数和异步错误 channel。
// 订阅缓冲区溢出时会移除该订阅并报告错误，避免继续输出已丢包的 MPEG-TS。
func (r *Reader) Subscribe(ch chan<- Packet) (func(), <-chan error) {
	errCh := make(chan error, 1)
	r.mu.Lock()
	r.subscribers[ch] = &subscriber{ch: ch, errCh: errCh}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subscribers, ch)
			r.mu.Unlock()
		})
	}, errCh
}

// SubCount 当前订阅者数量。
func (r *Reader) SubCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subscribers)
}

// CleanupStale 保留兼容接口。订阅溢出会在 readLoop 中立即移除，不再延迟清理。
func (r *Reader) CleanupStale(_ int) int {
	return 0
}

func (r *Reader) readLoop() {
	defer r.conn.Close()
	buf := make([]byte, 65535)
	for {
		select {
		case <-r.done:
			return
		default:
		}
		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-r.done:
				return
			default:
			}
			continue
		}
		// 复制一份，避免 buffer 覆盖
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		r.broadcast(pkt, n)
	}
}

func (r *Reader) broadcast(data []byte, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch, s := range r.subscribers {
		select {
		case s.ch <- Packet{Data: data, N: n}:
		default:
			// 静默丢包会直接破坏视频，移除慢订阅并通知 handler 结束连接。
			select {
			case s.errCh <- ErrSubscriberOverflow:
			default:
			}
			delete(r.subscribers, ch)
		}
	}
}

// Key 返回标识该 reader 的唯一字符串。
func Key(group string, port int) string {
	return netip.MustParseAddrPort(
		fmt.Sprintf("%s:%d", group, port),
	).String()
}
