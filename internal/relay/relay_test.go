package relay

import (
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"

	"iptv-udpproxy/internal/rules"
)

type fakePPPoE struct {
	enabled, up bool
	started     int
	activity    int
	idle        int
}

func (f *fakePPPoE) NotifyActivity()          { f.activity++ }
func (f *fakePPPoE) NotifyIdle(time.Duration) { f.idle++ }
func (f *fakePPPoE) IsUp() bool               { return f.up }
func (f *fakePPPoE) IsEnabled() bool          { return f.enabled }
func (f *fakePPPoE) Start() error             { f.started++; return nil }
func (f *fakePPPoE) SetOnDemand(bool)         {}
func (f *fakePPPoE) SetEnabled(bool)          {}

// TestAcquireSourceTriggersPPPoEDial 录制开组播源前应触发按需拨号
// （网口不存在使 reader 创建失败，但拨号必须先发生）。
func TestAcquireSourceTriggersPPPoEDial(t *testing.T) {
	rs, err := rules.Open(filepath.Join(t.TempDir(), "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := New("no-such-iface-xyz", rs)
	fake := &fakePPPoE{enabled: true, up: false}
	m.SetPPPoE(fake)
	if _, err := m.AcquireSource("239.1.1.1:1000", true); err == nil {
		t.Fatal("无网口时应失败")
	}
	if fake.started != 1 {
		t.Fatalf("应触发一次 PPPoE 拨号, started=%d", fake.started)
	}
}

// TestIdleIfConsumersGone 消费者判定：无流且无订阅者才空闲。
func TestIdleIfConsumersGone(t *testing.T) {
	m := &Manager{
		ifaceName: "lo",
		readers:   map[string]*readerEntry{},
		streams:   map[string]*StreamInfo{},
	}
	if idle, _ := m.idleIfConsumersGone(); !idle {
		t.Fatal("无消费者应空闲")
	}
	m.streams["s1"] = &StreamInfo{}
	if idle, _ := m.idleIfConsumersGone(); idle {
		t.Fatal("有活跃流不应空闲")
	}
}

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
		{path: "/rtp/192.168.1.100:10000", wantErr: true}, // 单播地址应拒绝
		{path: "/udp/240.0.0.1:1000", wantErr: true},      // 保留段（非 224/4）应拒绝
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
	if len(got) != 1 {
		t.Fatalf("expected 1 TS payload, got %d", len(got))
	}
	if len(got[0]) != len(ts) || got[0][0] != 0x47 {
		t.Fatalf("decoded length=%d first=%02x", len(got[0]), got[0][0])
	}
	for i := range got[0] {
		if got[0][i] != ts[i] {
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
