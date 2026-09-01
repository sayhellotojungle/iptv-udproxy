package mcast

import (
	"bytes"
	"testing"
)

func TestStreamSwitcherRemapsAndRestampsCandidate(t *testing.T) {
	s := NewStreamSwitcher()
	patA := makePATPacket(1, 256, 0)
	pmtA := makePMTPacket(1, 256, 176, 676, 0x1b, 0x04, 0)
	videoA1 := makeVideoPacket(176, 0, 100000, 40000, 0x64, true)
	videoA2 := makeVideoPacket(176, 1, 103600, 43600, 0x64, false)

	for _, data := range [][]byte{patA, pmtA, videoA1, videoA2} {
		out, err := s.ProcessCurrent(data)
		if err != nil {
			t.Fatalf("ProcessCurrent() error = %v", err)
		}
		if len(out) != tsPacketSize {
			t.Fatalf("initial output length = %d", len(out))
		}
	}

	s.StartWaiting()
	patB := makePATPacket(1, 256, 7)
	pmtB := makePMTPacket(1, 256, 180, 680, 0x1b, 0x04, 9)
	for _, data := range [][]byte{patB, pmtB} {
		if out, ready, err := s.ProcessCandidate(data); err != nil || ready || len(out) != 0 {
			t.Fatalf("candidate PSI: ready=%v len=%d err=%v", ready, len(out), err)
		}
	}

	candidate := makeVideoPacket(180, 3, 500000, 440000, 0x64, true)
	out, ready, err := s.ProcessCandidate(candidate)
	if err != nil {
		t.Fatalf("ProcessCandidate() error = %v", err)
	}
	if !ready {
		t.Fatal("candidate did not become ready on IDR")
	}
	packets, err := splitTSPackets(out)
	if err != nil {
		t.Fatalf("output alignment: %v", err)
	}
	if len(packets) != 3 {
		t.Fatalf("output packet count = %d, want 3", len(packets))
	}
	wantPIDs := []uint16{0, 256, 176}
	wantCC := []byte{1, 1, 2}
	for i, packet := range packets {
		if got := packetPID(packet); got != wantPIDs[i] {
			t.Errorf("packet %d PID = %d, want %d", i, got, wantPIDs[i])
		}
		if got := packet[3] & 0x0f; got != wantCC[i] {
			t.Errorf("packet %d CC = %d, want %d", i, got, wantCC[i])
		}
	}
	if pts, ok := packetPTS(packets[2]); !ok || pts != 107200 {
		t.Fatalf("candidate PTS = %d, ok=%v, want 107200", pts, ok)
	}
	if pcr, ok := packetPCRBase(packets[2]); !ok || pcr != 47200 {
		t.Fatalf("candidate PCR = %d, ok=%v, want 47200", pcr, ok)
	}
	if packets[2][5]&0x80 == 0 {
		t.Fatal("first candidate PCR packet is missing discontinuity_indicator")
	}

	// Subsequent B packets stay on canonical A PIDs and continue the same offset.
	out, err = s.ProcessCurrent(makeVideoPacket(180, 4, 503600, 443600, 0x64, false))
	if err != nil {
		t.Fatalf("transformed current source: %v", err)
	}
	packet := out[:tsPacketSize]
	if packetPID(packet) != 176 {
		t.Fatalf("active PID = %d, want 176", packetPID(packet))
	}
	if pts, ok := packetPTS(packet); !ok || pts != 110800 {
		t.Fatalf("active PTS = %d, ok=%v, want 110800", pts, ok)
	}
}

func TestStreamSwitcherRejectsIncompatibleCandidate(t *testing.T) {
	s := preparedSwitcher(t)
	s.StartWaiting()
	_, _, _ = s.ProcessCandidate(makePATPacket(1, 256, 0))
	_, _, err := s.ProcessCandidate(makePMTPacket(1, 256, 180, 680, 0x24, 0x04, 0))
	if err == nil {
		t.Fatal("expected HEVC topology error")
	}
}

func TestStreamSwitcherAcceptsSPSPPSChange(t *testing.T) {
	s := preparedSwitcher(t)
	s.StartWaiting()
	_, _, _ = s.ProcessCandidate(makePATPacket(1, 256, 0))
	_, _, _ = s.ProcessCandidate(makePMTPacket(1, 256, 180, 680, 0x1b, 0x04, 0))
	batch, ready, err := s.ProcessCandidate(makeVideoPacket(180, 3, 500000, 440000, 0x42, true))
	if err != nil {
		t.Fatalf("SPS/PPS change should be accepted: %v", err)
	}
	if !ready {
		t.Fatal("SPS/PPS change should trigger switch")
	}
	if len(batch) == 0 {
		t.Fatal("switch batch is empty")
	}
	if !bytes.Equal(s.canonicalSPS, []byte{0x67, 0x42, 0, 0x1f}) {
		t.Fatal("canonical SPS was not updated")
	}
}

func TestStreamSwitcherDoesNotSwitchOnSPSWithoutIDR(t *testing.T) {
	s := preparedSwitcher(t)
	s.StartWaiting()
	_, _, _ = s.ProcessCandidate(makePATPacket(1, 256, 0))
	_, _, _ = s.ProcessCandidate(makePMTPacket(1, 256, 180, 680, 0x1b, 0x04, 0))
	out, ready, err := s.ProcessCandidate(makeVideoPacket(180, 0, 500000, 440000, 0x64, false))
	if err != nil {
		t.Fatalf("ProcessCandidate() error = %v", err)
	}
	if ready || len(out) != 0 {
		t.Fatalf("SPS-only candidate switched: ready=%v len=%d", ready, len(out))
	}
}

func TestFailedCandidateTransformDoesNotAdvanceOutputState(t *testing.T) {
	s := preparedSwitcher(t)
	beforeCC := make(map[uint16]byte, len(s.lastCC))
	for pid, cc := range s.lastCC {
		beforeCC[pid] = cc
	}
	beforePTS, beforePCR := s.lastVideoPTS, s.lastPCR
	profile := programProfile{programNumber: 1, pmtPID: 256, pcrPID: 180, videoPID: 180, audioPID: 680, videoType: 0x1b, audioType: 0x04}
	transform := s.makeTransform(profile, -392800)

	_, err := s.transformPackets([][]byte{
		makeVideoPacket(180, 3, 500000, 440000, 0x64, true),
		makeSplitPESPacket(680, 4),
	}, transform, true)
	if err == nil {
		t.Fatal("expected candidate transformation error")
	}
	if s.lastVideoPTS != beforePTS || s.lastPCR != beforePCR {
		t.Fatal("failed transform changed timestamp anchors")
	}
	if !transform.waitAudioPUSI || !transform.markDiscontinuity {
		t.Fatal("failed transform changed candidate boundary state")
	}
	if len(s.lastCC) != len(beforeCC) {
		t.Fatal("failed transform changed continuity state")
	}
	for pid, want := range beforeCC {
		if got := s.lastCC[pid]; got != want {
			t.Fatalf("PID %d CC changed from %d to %d", pid, want, got)
		}
	}
}

func TestRewritePESTimestampsRejectsSplitHeader(t *testing.T) {
	packet := filledPacket()
	packet[0] = 0x47
	packet[1] = 0x40
	packet[3] = 0x30
	packet[4] = 175 // only eight payload bytes remain in this TS packet
	copy(packet[180:], []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80})
	if err := rewritePESTimestamps(packet, 3600); err == nil {
		t.Fatal("expected split PES header error")
	}
}

func preparedSwitcher(t *testing.T) *StreamSwitcher {
	t.Helper()
	s := NewStreamSwitcher()
	for _, packet := range [][]byte{
		makePATPacket(1, 256, 0),
		makePMTPacket(1, 256, 176, 676, 0x1b, 0x04, 0),
		makeVideoPacket(176, 0, 100000, 40000, 0x64, true),
		makeVideoPacket(176, 1, 103600, 43600, 0x64, false),
	} {
		if _, err := s.ProcessCurrent(packet); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func makePATPacket(program, pmtPID uint16, cc byte) []byte {
	return buildPATPacket(program, pmtPID, 0)
}

func makePMTPacket(program, pmtPID, videoPID, audioPID uint16, videoType, audioType, cc byte) []byte {
	return buildPMTPacket(program, pmtPID, videoPID, videoPID, audioPID, videoType, audioType, 0)
}

func makePSIPacket(pid uint16, cc byte, section []byte) []byte {
	return makePSITSPacket(pid, cc, section)
}

func makeSplitPESPacket(pid uint16, cc byte) []byte {
	packet := filledPacket()
	packet[0] = 0x47
	packet[1] = 0x40 | byte(pid>>8)&0x1f
	packet[2] = byte(pid)
	packet[3] = 0x30 | cc&0x0f
	packet[4] = 175 // only eight payload bytes remain in this TS packet
	copy(packet[180:], []byte{0, 0, 1, 0xc0, 0, 0, 0x80, 0x80})
	return packet
}

func makeVideoPacket(pid uint16, cc byte, pts, pcr uint64, spsByte byte, idr bool) []byte {
	packet := filledPacket()
	packet[0] = 0x47
	packet[1] = 0x40 | byte(pid>>8)&0x1f
	packet[2] = byte(pid)
	packet[3] = 0x30 | cc&0x0f
	packet[4] = 7
	packet[5] = 0x10
	writePCRBase(packet[6:12], pcr)

	payload := packet[12:]
	copy(payload, []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 0x05})
	payload[9] = 0x20
	encodeTimestamp(payload[9:14], pts)
	offset := 14
	for _, nal := range [][]byte{
		{0, 0, 0, 1, 0x67, spsByte, 0, 0x1f},
		{0, 0, 0, 1, 0x68, 0xee, 0x3c, 0x80},
	} {
		copy(payload[offset:], nal)
		offset += len(nal)
	}
	if idr {
		copy(payload[offset:], []byte{0, 0, 0, 1, 0x65, 0x88, 0x84})
	} else {
		copy(payload[offset:], []byte{0, 0, 0, 1, 0x41, 0x9a, 0x20})
	}
	return packet
}

func filledPacket() []byte {
	packet := make([]byte, tsPacketSize)
	for i := range packet {
		packet[i] = 0xff
	}
	return packet
}

// writePCRBase 按 MPEG-2 规范布局写入 PCR 字段（b3=base8..2+保留1，b4=base1..0+PCR_ext，b5=保留11111111）。
func writePCRBase(field []byte, base uint64) {
	base &= clockMask
	field[0] = byte(base >> 25)
	field[1] = byte(base >> 17)
	field[2] = byte(base >> 9)
	field[3] = byte((base>>2)&0x7f)<<1 | 1
	field[4] = byte((base>>1)&0x03) << 6 // base 1..0 + PCR_ext=0
	field[5] = 0xff
}
