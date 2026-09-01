package rtp

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestDepacketize(t *testing.T) {
	ts := makeTSPayload(7)
	tests := []struct {
		name    string
		packet  []byte
		wantErr bool
	}{
		{name: "minimal", packet: makeRTPPacket(1, 1, 0x80, nil, ts, 0)},
		{name: "marker", packet: makeRTPPacket(1, 1, 0x80, nil, ts, 0, 0xa1)},
		{name: "observed 0x90 extension", packet: makeRTPPacket(1, 1, 0x90, []byte{0x5a, 0x58, 0, 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, ts, 0)},
		{name: "csrc extension padding", packet: makeRTPPacket(1, 1, 0xb2, []byte{0, 0, 0, 1, 0, 0, 0, 2, 0x10, 0, 0, 1, 5, 6, 7, 8}, ts, 8)},
		{name: "short header", packet: []byte{0x80, 33}, wantErr: true},
		{name: "wrong version", packet: makeRTPPacket(1, 1, 0x40, nil, ts, 0), wantErr: true},
		{name: "wrong payload type", packet: makeRTPPacket(1, 1, 0x80, nil, ts, 0, 1), wantErr: true},
		{name: "truncated csrc", packet: makeRTPPacket(1, 1, 0x8f, nil, nil, 0), wantErr: true},
		{name: "truncated extension", packet: makeRTPPacket(1, 1, 0x90, []byte{0, 0, 0, 2, 1}, nil, 0), wantErr: true},
		{name: "bad padding", packet: makeBadPaddingPacket(ts), wantErr: true},
		{name: "unaligned TS", packet: makeRTPPacket(1, 1, 0x80, nil, append(ts, 0), 0), wantErr: true},
		{name: "bad TS sync", packet: makeRTPPacket(1, 1, 0x80, nil, append([]byte{0}, ts[1:]...), 0), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDepacketizer()
			got, err := d.Depacketize(tt.packet)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Depacketize() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 emitted payload, got %d", len(got))
			}
			if string(got[0]) != string(ts) {
				t.Fatal("MPEG-TS payload changed")
			}
		})
	}
}

func TestDepacketizerSequenceTracking(t *testing.T) {
	d := NewDepacketizer()
	ts := makeTSPayload(1)

	emit := func(seq uint16, ssrc uint32) ([][]byte, error) {
		return d.Depacketize(makeRTPPacket(seq, ssrc, 0x80, nil, ts, 0))
	}

	// 65535 → 0 回绕，正常输出
	if out, err := emit(65535, 1); err != nil || len(out) != 1 {
		t.Fatalf("seq 65535: out=%d err=%v", len(out), err)
	}
	if out, err := emit(0, 1); err != nil || len(out) != 1 {
		t.Fatalf("seq 0: out=%d err=%v", len(out), err)
	}

	// 间隙：seq 2 先到，缓冲等待（不输出、不报错）
	if out, err := emit(2, 1); err != nil || len(out) != 0 {
		t.Fatalf("gapped seq 2 should be buffered: out=%d err=%v", len(out), err)
	}
	// 重复
	if _, err := emit(2, 1); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate error = %v", err)
	}
	// 缺的 seq 1 到达：一次输出 [1, 2]
	out, err := emit(1, 1)
	if err != nil || len(out) != 2 {
		t.Fatalf("reorder: out=%d err=%v", len(out), err)
	}
	// 现在 seq 1 已过，再来的 1 才是过时包
	if _, err := emit(1, 1); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("out-of-order error = %v", err)
	}
	// SSRC 变化：状态重置，新流首包正常输出
	if out, err := emit(100, 2); err != nil || len(out) != 1 {
		t.Fatalf("SSRC change: out=%d err=%v", len(out), err)
	}

	stats := d.Stats()
	if stats.Packets != 5 || stats.SequenceGaps != 0 || stats.Duplicates != 1 || stats.OutOfOrder != 1 || stats.SSRCChanges != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestDepacketizerBufferOverflowDropsOldest(t *testing.T) {
	d := NewDepacketizer()
	ts := makeTSPayload(1)

	// seq 0 输出；seq 2..17 先到（16 个，超过 reorderLimit）
	if out, err := d.Depacketize(makeRTPPacket(0, 1, 0x80, nil, ts, 0)); err != nil || len(out) != 1 {
		t.Fatalf("seq 0: out=%d err=%v", len(out), err)
	}
	for seq := uint16(2); seq < 18; seq++ {
		if _, err := d.Depacketize(makeRTPPacket(seq, 1, 0x80, nil, ts, 0)); err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
	// 溢出后最旧的 2..(2..8 中部分) 被丢弃；seq 1 到达后能输出后续一段
	out, err := d.Depacketize(makeRTPPacket(1, 1, 0x80, nil, ts, 0))
	if err != nil || len(out) == 0 {
		t.Fatalf("seq 1: out=%d err=%v", len(out), err)
	}
	stats := d.Stats()
	if stats.SequenceGaps == 0 {
		t.Fatalf("expected dropped gaps, stats: %+v", stats)
	}
}

func TestDepacketizerSteadyStateJitter(t *testing.T) {
	// 固定抖动窗（成对 (2,1),(4,3)... 每对晚一步到达）不应产生丢包：
	// 0..39 全部且只按序输出一次。
	d := NewDepacketizer()
	ts := makeTSPayload(1)

	delivered := 0
	emit := func(seq uint16) {
		out, err := d.Depacketize(makeRTPPacket(seq, 1, 0x80, nil, ts, 0))
		if err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
		delivered += len(out)
	}

	emit(0)
	for seq := uint16(1); seq < 39; seq += 2 {
		emit(seq + 1)
		emit(seq)
	}
	emit(39)
	if delivered != 40 {
		t.Fatalf("steady-state jitter lost packets: delivered %d/40 (stats %+v)", delivered, d.Stats())
	}
	if d.Stats().SequenceGaps != 0 || d.Stats().OutOfOrder != 0 {
		t.Fatalf("no loss expected, stats: %+v", d.Stats())
	}
}

func makeTSPayload(packetCount int) []byte {
	payload := make([]byte, packetCount*tsPacketSize)
	for i := 0; i < packetCount; i++ {
		packet := payload[i*tsPacketSize : (i+1)*tsPacketSize]
		packet[0] = 0x47
		packet[1] = byte(i >> 8)
		packet[2] = byte(i)
		packet[3] = 0x10
	}
	return payload
}

func makeBadPaddingPacket(payload []byte) []byte {
	packet := makeRTPPacket(1, 1, 0x80, nil, payload, 1)
	packet[len(packet)-1] = 0xff
	return packet
}

func makeRTPPacket(seq uint16, ssrc uint32, first byte, extra, payload []byte, padding int, secondExtra ...byte) []byte {
	second := byte(mpegTSPayloadType)
	if len(secondExtra) > 0 {
		second = secondExtra[0]
	}
	packet := make([]byte, 12)
	packet[0] = first
	packet[1] = second
	binary.BigEndian.PutUint16(packet[2:4], seq)
	binary.BigEndian.PutUint32(packet[8:12], ssrc)
	packet = append(packet, extra...)
	packet = append(packet, payload...)
	if padding > 0 {
		packet[0] |= 0x20
		packet = append(packet, make([]byte, padding)...)
		packet[len(packet)-1] = byte(padding)
	}
	return packet
}
