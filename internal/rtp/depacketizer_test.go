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
			if string(got) != string(ts) {
				t.Fatal("MPEG-TS payload changed")
			}
		})
	}
}

func TestDepacketizerSequenceTracking(t *testing.T) {
	d := NewDepacketizer()
	ts := makeTSPayload(1)

	for _, seq := range []uint16{65535, 0} {
		if _, err := d.Depacketize(makeRTPPacket(seq, 1, 0x80, nil, ts, 0)); err != nil {
			t.Fatalf("sequence %d: %v", seq, err)
		}
	}
	// Sequence gap: packet accepted, gap counted.
	if _, err := d.Depacketize(makeRTPPacket(2, 1, 0x80, nil, ts, 0)); err != nil {
		t.Fatalf("sequence gap should not error: %v", err)
	}
	if _, err := d.Depacketize(makeRTPPacket(2, 1, 0x80, nil, ts, 0)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := d.Depacketize(makeRTPPacket(1, 1, 0x80, nil, ts, 0)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("out-of-order error = %v", err)
	}
	if _, err := d.Depacketize(makeRTPPacket(100, 2, 0x80, nil, ts, 0)); err != nil {
		t.Fatalf("SSRC change: %v", err)
	}

	stats := d.Stats()
	if stats.Packets != 4 || stats.SequenceGaps != 1 || stats.Duplicates != 1 || stats.OutOfOrder != 1 || stats.SSRCChanges != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
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
