package relay

import (
	"encoding/binary"
	"testing"
)

func TestParseRequestKeepsInputMode(t *testing.T) {
	tests := []struct {
		path     string
		wantMode inputMode
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{path: "/rtp/239.69.1.107:10280", wantMode: modeRTP, wantHost: "239.69.1.107", wantPort: 10280},
		{path: "/udp/239.254.96.161:9040", wantMode: modeUDP, wantHost: "239.254.96.161", wantPort: 9040},
		{path: "/http/239.1.1.1:1", wantErr: true},
		{path: "/rtp/not-an-ip:1", wantErr: true},
		{path: "/rtp/239.1.1.1:0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			mode, host, port, err := parseRequest(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode != tt.wantMode || host != tt.wantHost || port != tt.wantPort {
				t.Fatalf("got (%v, %s, %d)", mode, host, port)
			}
		})
	}
}

func TestRTPInputDecoderStripsObservedExtensionHeader(t *testing.T) {
	ts := make([]byte, 7*188)
	for offset := 0; offset < len(ts); offset += 188 {
		ts[offset] = 0x47
		ts[offset+3] = 0x10
	}
	rtpPacket := make([]byte, 32+len(ts))
	rtpPacket[0] = 0x90
	rtpPacket[1] = 33
	binary.BigEndian.PutUint16(rtpPacket[2:4], 1)
	binary.BigEndian.PutUint32(rtpPacket[8:12], 0x12345678)
	binary.BigEndian.PutUint16(rtpPacket[12:14], 0x5a58)
	binary.BigEndian.PutUint16(rtpPacket[14:16], 4)
	copy(rtpPacket[32:], ts)

	got, err := newInputDecoder(modeRTP).Decode(rtpPacket)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ts) || got[0] != 0x47 {
		t.Fatalf("decoded length=%d first=%02x", len(got), got[0])
	}
	for i := range got {
		if got[i] != ts[i] {
			t.Fatalf("payload differs at byte %d", i)
		}
	}
}

func TestUDPInputDecoderRequiresAlignedTS(t *testing.T) {
	decoder := newInputDecoder(modeUDP)
	if _, err := decoder.Decode([]byte{0x47}); err == nil {
		t.Fatal("expected alignment error")
	}
	packet := make([]byte, 188)
	packet[0] = 0x47
	if _, err := decoder.Decode(packet); err != nil {
		t.Fatal(err)
	}
}
