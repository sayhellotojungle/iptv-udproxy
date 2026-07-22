package rtp

import (
	"io"
	"net/http"
	"os"
	"testing"
)

// TestLiveObservedRTPFraming is opt-in. Set IPTV_RTP_RELAY_URL to the old
// server (still outputting raw RTP) to verify depacketization against real
// traffic. Set IPTV_TS_RELAY_URL to the new server to verify clean MPEG-TS.
func TestLiveObservedRTPFraming(t *testing.T) {
	rtpURL := os.Getenv("IPTV_RTP_RELAY_URL")
	tsURL := os.Getenv("IPTV_TS_RELAY_URL")
	if rtpURL != "" {
		t.Run("old RTP relay", func(t *testing.T) {
			testOldRTPRelay(t, rtpURL)
		})
	}
	if tsURL != "" {
		t.Run("new TS relay", func(t *testing.T) {
			testNewTSRelay(t, tsURL)
		})
	}
	if rtpURL == "" && tsURL == "" {
		t.Skip("set IPTV_RTP_RELAY_URL or IPTV_TS_RELAY_URL to run live framing verification")
	}
}

func testOldRTPRelay(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	depacketizer := NewDepacketizer()
	for i := 0; i < 100; i++ {
		datagram := make([]byte, 32+7*tsPacketSize)
		if _, err := io.ReadFull(resp.Body, datagram); err != nil {
			t.Fatal(err)
		}
		payload, err := depacketizer.Depacketize(datagram)
		if err != nil {
			t.Fatalf("datagram %d: %v", i, err)
		}
		if len(payload) != 7*tsPacketSize || payload[0] != 0x47 {
			t.Fatalf("datagram %d produced malformed TS", i)
		}
	}
}

func testNewTSRelay(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	buf := make([]byte, 7*tsPacketSize*50)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0x47 {
		t.Fatalf("first byte = 0x%02x, want 0x47 (MPEG-TS sync)", buf[0])
	}
	if len(buf)%tsPacketSize != 0 {
		t.Fatalf("length %d is not divisible by 188", len(buf))
	}
	for offset := 0; offset < len(buf); offset += tsPacketSize {
		if buf[offset] != 0x47 {
			t.Fatalf("byte %d = 0x%02x, want 0x47", offset, buf[offset])
		}
	}
}
