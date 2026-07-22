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
)

var (
	ErrDuplicate  = errors.New("重复的 RTP 包")
	ErrOutOfOrder = errors.New("乱序的 RTP 包")
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

// Depacketizer removes RTP framing and returns an aligned MPEG-TS payload.
type Depacketizer struct {
	stats   Stats
	haveSeq bool
	lastSeq uint16
	ssrc    uint32
}

// NewDepacketizer creates an RTP MPEG-TS depacketizer.
func NewDepacketizer() *Depacketizer {
	return &Depacketizer{}
}

// Stats returns the current diagnostic counters.
func (d *Depacketizer) Stats() Stats {
	return d.stats
}

// Depacketize validates one RTP v2 packet and returns only its MPEG-TS payload.
func (d *Depacketizer) Depacketize(datagram []byte) ([]byte, error) {
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
	if d.haveSeq && ssrc == d.ssrc {
		delta := uint16(seq - d.lastSeq)
		switch {
		case delta == 0:
			d.stats.Duplicates++
			return nil, ErrDuplicate
		case delta >= 0x8000:
			d.stats.OutOfOrder++
			return nil, ErrOutOfOrder
		case delta > 1:
			d.stats.SequenceGaps += uint64(delta - 1)
			// 接受这个包，不要因此直接结束连接。
			// TS continuity counter 不连续由 stream_switcher 检测和处理。
		}
	} else if d.haveSeq {
		d.stats.SSRCChanges++
	}

	d.haveSeq = true
	d.lastSeq = seq
	d.ssrc = ssrc
	d.stats.Packets++
	return payload, nil
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
