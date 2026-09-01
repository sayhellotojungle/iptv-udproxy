// Package rtp parses RTP packets carrying MPEG-TS payloads.
package rtp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	mpegTSPayloadType = 33
	tsPacketSize      = 188
	reorderLimit      = 8 // 重排窗口：仅缓存 lastSeq+1..lastSeq+8，超窗新到的包直接丢弃并计入 SequenceGaps
)

var (
	ErrDuplicate  = errors.New("重复的 RTP 包")
	ErrOutOfOrder = errors.New("过时的 RTP 包")
)

// Stats is a snapshot of RTP input diagnostics.
type Stats struct {
	Packets      uint64
	Malformed    uint64
	SequenceGaps uint64
	Duplicates   uint64
	OutOfOrder   uint64
	SSRCChanges  uint64
}

// Depacketizer 剥离 RTP 头并按序列号顺序输出 MPEG-TS 负载。
//
// 内置小型重排缓冲：吸收链路常见乱序（固定抖动窗），一次到达可能触发
// 多个已缓冲 TS 负载按序输出；窗口上限 reorderLimit（至多缓冲 8 个包），
// 超窗新到的包丢弃并计入 SequenceGaps，TS 层的不连续由上层（stream_switcher）检测。
type Depacketizer struct {
	stats   Stats
	haveSeq bool
	lastSeq uint16
	ssrc    uint32
	// pending: seq(> lastSeq) -> 已校验的 TS 负载，等待前面的包到齐后按序输出
	pending map[uint16][]byte
}

// NewDepacketizer creates an RTP MPEG-TS depacketizer.
func NewDepacketizer() *Depacketizer {
	return &Depacketizer{pending: make(map[uint16][]byte)}
}

// Stats returns the current diagnostic counters.
func (d *Depacketizer) Stats() Stats {
	return d.stats
}

// Depacketize 校验一个 RTP v2 包；返回按序可输出的 TS 负载（可能为空，
// 表示该包被缓冲等待前面的包、被判定为过时/重复、或超窗被丢弃；
// 可能多个，表示补上了重排间隙）。
func (d *Depacketizer) Depacketize(datagram []byte) ([][]byte, error) {
	if len(datagram) < 12 {
		return nil, d.malformed("RTP 头不足 12 字节")
	}
	if datagram[0]>>6 != 2 {
		return nil, d.malformed("RTP 版本不是 2")
	}
	if datagram[1]&0x7f != mpegTSPayloadType {
		return nil, d.malformed(fmt.Sprintf("RTP payload type 是 %d，不是 MPEG-TS(33)", datagram[1]&0x7f))
	}

	headerLen := 12 + int(datagram[0]&0x0f)*4
	if headerLen > len(datagram) {
		return nil, d.malformed("RTP CSRC 列表超出数据包")
	}

	if datagram[0]&0x10 != 0 {
		if headerLen+4 > len(datagram) {
			return nil, d.malformed("RTP 扩展头不完整")
		}
		extensionWords := int(binary.BigEndian.Uint16(datagram[headerLen+2 : headerLen+4]))
		headerLen += 4 + extensionWords*4
		if headerLen > len(datagram) {
			return nil, d.malformed("RTP 扩展数据超出数据包")
		}
	}

	payloadEnd := len(datagram)
	if datagram[0]&0x20 != 0 {
		padding := int(datagram[len(datagram)-1])
		if padding == 0 || padding > payloadEnd-headerLen {
			return nil, d.malformed("RTP padding 长度无效")
		}
		payloadEnd -= padding
	}
	if payloadEnd <= headerLen {
		return nil, d.malformed("RTP MPEG-TS payload 为空")
	}

	payload := datagram[headerLen:payloadEnd]
	if err := validateTSPayload(payload); err != nil {
		return nil, d.malformed(err.Error())
	}

	seq := binary.BigEndian.Uint16(datagram[2:4])
	ssrc := binary.BigEndian.Uint32(datagram[8:12])
	if !d.haveSeq || ssrc != d.ssrc {
		// SSRC 变化 = 换了一路流：重置序列状态，丢弃旧缓冲
		if d.haveSeq {
			d.stats.SSRCChanges++
		}
		d.resetState(seq, ssrc)
	}

	switch delta := seq - d.lastSeq; {
	case delta == 0:
		d.stats.Duplicates++
		return nil, ErrDuplicate
	case delta >= 0x8000:
		// 早于已输出的包（真乱序到达的滞後包），丢弃
		d.stats.OutOfOrder++
		return nil, ErrOutOfOrder
	case int(delta) > reorderLimit:
		// 超出重排窗（lastSeq+1 .. lastSeq+reorderLimit）：中间缺口已无法补回，
		// 持有只会拖延输出，丢弃并计一次缺口（TS 层 CC 断由上层检测）
		d.stats.SequenceGaps++
		return nil, nil
	}

	if _, held := d.pending[seq]; held {
		// 已缓冲、尚未输出的包重复到达
		d.stats.Duplicates++
		return nil, ErrDuplicate
	}
	d.pending[seq] = payload
	return d.flushInOrder(), nil
}

// resetState 用新流的第一个包建立序列基线（让该包视为"顺序"到达）。
func (d *Depacketizer) resetState(seq uint16, ssrc uint32) {
	d.haveSeq = true
	d.lastSeq = seq - 1
	d.ssrc = ssrc
	d.pending = make(map[uint16][]byte)
}

// flushInOrder 按序输出所有已缓冲且前面不再缺包的负载。
func (d *Depacketizer) flushInOrder() [][]byte {
	var out [][]byte
	for {
		next := d.lastSeq + 1
		payload, ok := d.pending[next]
		if !ok {
			break
		}
		delete(d.pending, next)
		d.lastSeq = next
		d.stats.Packets++
		out = append(out, payload)
	}
	return out
}

func (d *Depacketizer) malformed(message string) error {
	d.stats.Malformed++
	return errors.New(message)
}

func validateTSPayload(payload []byte) error {
	if len(payload)%tsPacketSize != 0 {
		return fmt.Errorf("MPEG-TS payload 长度 %d 不是 188 的整数倍", len(payload))
	}
	for offset := 0; offset < len(payload); offset += tsPacketSize {
		if payload[offset] != 0x47 {
			return fmt.Errorf("MPEG-TS 在偏移 %d 缺少同步字节", offset)
		}
	}
	return nil
}
