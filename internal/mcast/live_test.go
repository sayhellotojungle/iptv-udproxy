package mcast

import (
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveSourceSwitch is opt-in because it requires two reachable, endless
// HTTP MPEG-TS sources. Run with IPTV_SOURCE_A and IPTV_SOURCE_B set.
func TestLiveSourceSwitch(t *testing.T) {
	urlA := os.Getenv("IPTV_SOURCE_A")
	urlB := os.Getenv("IPTV_SOURCE_B")
	if urlA == "" || urlB == "" {
		t.Skip("set IPTV_SOURCE_A and IPTV_SOURCE_B to run live switching")
	}

	bodyA := openLiveTS(t, urlA)
	defer bodyA.Close()
	bodyB := openLiveTS(t, urlB)
	defer bodyB.Close()

	switcher := NewStreamSwitcher()
	checker := newContinuityChecker()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		data := readLiveDatagram(t, bodyA)
		out, err := switcher.ProcessCurrent(data)
		if err != nil {
			t.Fatal(err)
		}
		checker.Push(t, out)
		if switcher.canonical != nil && len(switcher.canonicalSPS) > 0 && switcher.haveVideoPTS && switcher.havePCR {
			break
		}
	}
	if switcher.canonical == nil || len(switcher.canonicalSPS) == 0 {
		t.Fatal("source A did not produce a complete program profile and H.264 parameters")
	}

	switchLiveSource(t, switcher, bodyB, checker)
	for i := 0; i < 300; i++ {
		out, err := switcher.ProcessCurrent(readLiveDatagram(t, bodyB))
		if err != nil {
			t.Fatal(err)
		}
		checker.Push(t, out)
	}

	bodyA2 := openLiveTS(t, urlA)
	defer bodyA2.Close()
	switchLiveSource(t, switcher, bodyA2, checker)
	pushLiveCurrent(t, switcher, bodyA2, checker, 300)

	bodyB2 := openLiveTS(t, urlB)
	defer bodyB2.Close()
	switchLiveSource(t, switcher, bodyB2, checker)
	pushLiveCurrent(t, switcher, bodyB2, checker, 300)

	bodyA3 := openLiveTS(t, urlA)
	defer bodyA3.Close()
	switchLiveSource(t, switcher, bodyA3, checker)
	pushLiveCurrent(t, switcher, bodyA3, checker, 300)
}

func pushLiveCurrent(t *testing.T, switcher *StreamSwitcher, body io.Reader, checker *continuityChecker, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		out, err := switcher.ProcessCurrent(readLiveDatagram(t, body))
		if err != nil {
			t.Fatal(err)
		}
		checker.Push(t, out)
	}
}

func switchLiveSource(t *testing.T, switcher *StreamSwitcher, body io.Reader, checker *continuityChecker) {
	t.Helper()
	switcher.StartWaiting()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		out, ready, err := switcher.ProcessCandidate(readLiveDatagram(t, body))
		if err != nil {
			t.Fatalf("candidate rejected: %v", err)
		}
		if ready {
			checker.Push(t, out)
			return
		}
	}
	t.Fatal("candidate did not reach a compatible IDR before timeout")
}

func openLiveTS(t *testing.T, url string) io.ReadCloser {
	t.Helper()
	client := &http.Client{Timeout: 0}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	return resp.Body
}

func readLiveDatagram(t *testing.T, r io.Reader) []byte {
	t.Helper()
	data := make([]byte, 7*tsPacketSize)
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatalf("read live MPEG-TS: %v", err)
	}
	return data
}

type continuityChecker struct {
	last    map[uint16]byte
	have    map[uint16]bool
	lastPTS map[uint16]uint64
	havePTS map[uint16]bool
	lastPCR uint64
	havePCR bool
}

func newContinuityChecker() *continuityChecker {
	return &continuityChecker{
		last:    make(map[uint16]byte),
		have:    make(map[uint16]bool),
		lastPTS: make(map[uint16]uint64),
		havePTS: make(map[uint16]bool),
	}
}

func (c *continuityChecker) Push(t *testing.T, data []byte) {
	t.Helper()
	packets, err := splitTSPackets(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, packet := range packets {
		pid := packetPID(packet)
		if !packetHasPayload(packet) {
			continue
		}
		cc := packet[3] & 0x0f
		if c.have[pid] && cc != (c.last[pid]+1)&0x0f {
			t.Fatalf("PID %d continuity error: got %d after %d", pid, cc, c.last[pid])
		}
		c.last[pid] = cc
		c.have[pid] = true

		if pts, ok := packetPTS(packet); ok {
			if c.havePTS[pid] {
				ptsDiff := (pts - c.lastPTS[pid]) & clockMask
				var delta int64
				if ptsDiff <= clockMask/2 {
					delta = int64(ptsDiff)
				} else {
					delta = int64(clockMask+1) - int64(ptsDiff)
				}
				if delta < 0 {
					delta = -delta
				}
				if delta == 0 || delta > 180000 {
					t.Fatalf("PID %d PTS discontinuity: delta=%d", pid, delta)
				}
			}
			c.lastPTS[pid] = pts
			c.havePTS[pid] = true
		}
		if pcr, ok := packetPCRBase(packet); ok {
			if c.havePCR {
				pcrDiff := (pcr - c.lastPCR) & clockMask
				var delta int64
				if pcrDiff <= clockMask/2 {
					delta = int64(pcrDiff)
				} else {
					delta = int64(clockMask+1) - int64(pcrDiff)
				}
				if delta < 0 {
					delta = -delta
				}
				if delta == 0 || delta > 18000 {
					t.Fatalf("PCR discontinuity: delta=%d", delta)
				}
			}
			c.lastPCR = pcr
			c.havePCR = true
		}
	}
}
