// Package mcast provides MPEG-TS source switching support.
package mcast

import (
	"bytes"
	"errors"
	"fmt"
	"log"
)

const (
	tsPacketSize   = 188
	tsSyncByte     = 0x47
	tsNullPID      = 0x1fff
	clockMask      = uint64(1<<33 - 1)
	spsWindowTicks = 2 * 90000 // 候选源 SPS/PPS 容忍窗：2 秒（90kHz 时基）
)

// StreamSwitcher keeps one stable MPEG-TS program identity while switching
// between transport-compatible H.264/MP2 sources.
type StreamSwitcher struct {
	canonical        *programProfile
	canonicalPAT     []byte
	canonicalPMT     []byte
	canonicalSPS     []byte
	canonicalPPS     []byte
	psiVersion       byte
	initialVideoES   []byte
	active           *sourceTransform
	candidate        *candidateState
	lastCC           map[uint16]byte
	lastVideoPTS     uint64
	haveVideoPTS     bool
	videoStep        uint64
	previousVideoPTS uint64
	havePreviousPTS  bool
	lastPCR          uint64
	havePCR          bool
	patPsi           psiReassembler // 当前源 PAT 跨包重组
	pmtPsi           psiReassembler // 当前源 PMT 跨包重组
}

type programProfile struct {
	programNumber uint16
	pmtPID        uint16
	pcrPID        uint16
	videoPID      uint16
	audioPID      uint16
	videoType     byte
	audioType     byte
}

type sourceTransform struct {
	profile           programProfile
	pidMap            map[uint16]uint16
	offset90          int64
	waitAudioPUSI     bool
	markDiscontinuity bool
}

type candidateState struct {
	pmtPID       uint16
	havePMTPID   bool
	profile      *programProfile
	packets      [][]byte
	videoES      []byte
	buffering    bool
	videoPTS     uint64
	haveVideoPTS bool
	sps          []byte
	pps          []byte
	spsFoundPTS  uint64 // 最近一次在 AU 中见到 SPS/PPS 的视频 PTS
	haveSPSPTS   bool
	foundIDR     bool
	firstPCR     uint64
	havePCR      bool
	lastCC       map[uint16]byte
	haveCC       map[uint16]bool
	patPsi       psiReassembler
	pmtPsi       psiReassembler
}

// psiReassembler 累积被拆到多个 TS 包的 PSI section 流（长 PMT 常见），
// 每凑齐一个完整 section（含 CRC）就交回给解析器。
type psiReassembler struct {
	buf []byte
}

// resetPSI 重开一段 PSI 重复（PUSI 包）：清空缓冲并消费 pointer field，
// 返回真正从 section 头（table_id）开始的字节。非 PUSI 续包直接 feed 原始 payload。
func (r *psiReassembler) resetPSI(payload []byte) []byte {
	r.buf = nil
	if len(payload) < 1 {
		return nil
	}
	ptr := int(payload[0])
	if ptr == 0xff || 1+ptr > len(payload) {
		return nil
	}
	return payload[1+ptr:]
}

// feed 追加一段 payload，返回其中完整的 sections。
func (r *psiReassembler) feed(chunk []byte) [][]byte {
	r.buf = append(r.buf, chunk...)
	var sections [][]byte
	for {
		for len(r.buf) > 0 && r.buf[0] == 0xff { // 跳过填充字节
			r.buf = r.buf[1:]
		}
		if len(r.buf) < 3 {
			break
		}
		total := 3 + (int(r.buf[1]&0x0f)<<8 | int(r.buf[2]))
		if total < 5 || len(r.buf) < total {
			break
		}
		sections = append(sections, append([]byte(nil), r.buf[:total]...))
		r.buf = r.buf[total:]
	}
	return sections
}

// NewStreamSwitcher creates a per-client switcher.
func NewStreamSwitcher() *StreamSwitcher {
	return &StreamSwitcher{lastCC: make(map[uint16]byte)}
}

// StartWaiting resets candidate-only state. Active output state is preserved.
func (s *StreamSwitcher) StartWaiting() {
	s.candidate = &candidateState{
		lastCC: make(map[uint16]byte),
		haveCC: make(map[uint16]bool),
	}
}

// AbortCandidate 取消等待中的候选源并释放其已缓冲的 TS 包。
// 切换取消（超时/不兼容/客户端断开）时必须调用，否则长 GOP 下缓冲数 MB 直到流程结束。
func (s *StreamSwitcher) AbortCandidate() {
	if s.candidate != nil {
		s.candidate.packets = nil
		s.candidate.videoES = nil
	}
	s.candidate = nil
}

// ProcessCurrent observes the initial source and transforms an activated
// replacement source back to the connection's canonical PID/timestamp space.
func (s *StreamSwitcher) ProcessCurrent(data []byte) ([]byte, error) {
	packets, err := splitTSPackets(data)
	if err != nil {
		return nil, err
	}
	if s.active == nil {
		for _, packet := range packets {
			s.observeInitial(packet)
			s.observeInitialOutput(packet)
		}
		return data, nil
	}
	return s.transformPackets(packets, s.active, false)
}

// ProcessCandidate consumes a replacement source. It returns a single
// canonical batch beginning with PAT/PMT and the PES containing a real H.264
// IDR. The same batch is never returned twice.
func (s *StreamSwitcher) ProcessCandidate(data []byte) ([]byte, bool, error) {
	if s.candidate == nil {
		return nil, false, errors.New("尚未开始等待候选源")
	}
	packets, err := splitTSPackets(data)
	if err != nil {
		return nil, false, err
	}
	c := s.candidate

	for _, packet := range packets {
		pid := packetPID(packet)
		if packetHasPayload(packet) {
			cc := packet[3] & 0x0f
			if c.haveCC[pid] && cc != (c.lastCC[pid]+1)&0x0f {
				c.packets = nil
				c.videoES = nil
				c.buffering = false
				c.foundIDR = false
				// TS 不连续（流重启）：旧 SPS/PPS 可能属于旧流结构，一并作废
				c.sps = nil
				c.pps = nil
				c.haveSPSPTS = false
			}
			c.lastCC[pid] = cc
			c.haveCC[pid] = true
		}
		if pid == 0 {
			if payload, ok := packetPayload(packet); ok {
				// 每个 PUSI 包开始一段新的 PSI 重复；长 PAT/PMT 跨包重组
				if packetPUSI(packet) {
					payload = c.patPsi.resetPSI(payload)
				}
				for _, section := range c.patPsi.feed(payload) {
					if program, pmtPID, ok := parsePAT(section); ok {
						c.pmtPID = pmtPID
						c.havePMTPID = true
						if c.profile != nil && c.profile.programNumber != program {
							return nil, false, fmt.Errorf("候选源 program number 从 %d 变为 %d", c.profile.programNumber, program)
						}
					}
				}
			}
		}
		if c.havePMTPID && pid == c.pmtPID {
			if payload, ok := packetPayload(packet); ok {
				if packetPUSI(packet) {
					payload = c.pmtPsi.resetPSI(payload)
				}
				for _, section := range c.pmtPsi.feed(payload) {
					if profile, ok := parsePMT(section, c.pmtPID); ok {
						c.profile = &profile
						if err := s.checkCompatibility(profile); err != nil {
							return nil, false, err
						}
					}
				}
			}
		}

		if c.profile == nil {
			continue
		}

		if pid == c.profile.videoPID && packetPUSI(packet) {
			pts, havePts := packetPTS(packet)
			// SPS/PPS 容忍窗：运营商常见做法是周期性单独插入 SPS/PPS（不与 IDR 同 AU）。
			// 只要 SPS/PPS 出现在最近 spsWindowTicks（2 秒）内就保留，
			// 让后续含 IDR 的 AU 可以复用；超出窗口的旧参数才作废。
			if c.haveSPSPTS && havePts && (pts-c.spsFoundPTS)&clockMask > spsWindowTicks {
				c.sps = nil
				c.pps = nil
				c.haveSPSPTS = false
			}
			c.packets = nil
			c.videoES = nil
			c.buffering = true
			c.foundIDR = false
			c.havePCR = false
			c.videoPTS, c.haveVideoPTS = pts, havePts
		}
		if c.buffering {
			c.packets = append(c.packets, bytes.Clone(packet))
			if !c.havePCR {
				c.firstPCR, c.havePCR = packetPCRBase(packet)
			}
		}
		if c.buffering && pid == c.profile.videoPID {
			payload, ok := elementaryPayload(packet)
			if ok {
				c.videoES = append(c.videoES, payload...)
				sps, pps, foundIDR := scanH264(c.videoES)
				if len(sps) > 0 {
					c.sps = sps
					c.spsFoundPTS = c.videoPTS
					c.haveSPSPTS = true
				}
				if len(pps) > 0 {
					c.pps = pps
				}
				c.foundIDR = foundIDR
			}
		}
	}

	if !c.foundIDR || !c.haveVideoPTS || !c.havePCR || len(c.sps) == 0 || len(c.pps) == 0 {
		return nil, false, nil
	}
	if len(s.canonicalSPS) == 0 || len(s.canonicalPPS) == 0 {
		return nil, false, nil
	}
	if !bytes.Equal(c.sps, s.canonicalSPS) || !bytes.Equal(c.pps, s.canonicalPPS) {
		// SPS/PPS 改变意味着分辨率或编码参数发生了变化。
		// 更新 canonical SPS/PPS，递增 PSI 版本，让客户端重新初始化解码器。
		s.canonicalSPS = bytes.Clone(c.sps)
		s.canonicalPPS = bytes.Clone(c.pps)
		s.psiVersion = (s.psiVersion + 1) & 0x1f
		s.canonicalPAT = nil
		s.canonicalPMT = nil
		if s.canonical != nil {
			s.canonicalPAT = buildPATPacket(s.canonical.programNumber, s.canonical.pmtPID, s.psiVersion)
			s.canonicalPMT = buildPMTPacket(s.canonical.programNumber, s.canonical.pmtPID, s.canonical.pcrPID, s.canonical.videoPID, s.canonical.audioPID, s.canonical.videoType, s.canonical.audioType, s.psiVersion)
		}
		log.Printf("[switcher] 候选源 SPS/PPS 与当前源不同，更新解码器参数并递增 PSI 版本至 %d", s.psiVersion)
	}

	step := s.videoStep
	if step == 0 || step > 90000 {
		step = 3600 // 25fps fallback; only used before two PTS samples are observed.
	}
	if !s.haveVideoPTS {
		return nil, false, errors.New("当前源尚未建立视频 PTS 时间线")
	}
	targetPTS := (s.lastVideoPTS + step) & clockMask
	offset := int64(targetPTS) - int64(c.videoPTS)
	if s.havePCR {
		adjustedPCR := addClock(c.firstPCR, offset)
		// Signed circular distance handles wraparound at 2^33.
		pcrDiff := (adjustedPCR - s.lastPCR) & clockMask
		var pcrGap int64
		if pcrDiff <= clockMask/2 {
			pcrGap = int64(pcrDiff)
		} else {
			pcrGap = int64(clockMask+1) - int64(pcrDiff)
		}
		if pcrGap < 0 {
			pcrGap = -pcrGap
		}
		// The supplied synchronized variants differ in PTS-to-PCR lead by about
		// 130ms. Keep the jump bounded to 200ms and explicitly signal the first
		// candidate PCR as discontinuous below.
		maxPCRGap := int64(18000)
		if pcrGap > maxPCRGap {
			return nil, false, fmt.Errorf("候选源 PCR 时间线不兼容，切换间隔为 %d ticks（上限 %d）", pcrGap, maxPCRGap)
		}
	}
	transform := s.makeTransform(*c.profile, offset)
	output, err := s.transformPackets(c.packets, transform, true)
	if err != nil {
		return nil, false, err
	}

	s.active = transform
	s.candidate = nil
	return output, true, nil
}

func (s *StreamSwitcher) checkCompatibility(candidate programProfile) error {
	if s.canonical == nil {
		return errors.New("当前源尚未解析出 PAT/PMT")
	}
	if candidate.programNumber != s.canonical.programNumber {
		return fmt.Errorf("候选源 program number %d 与当前源 %d 不一致", candidate.programNumber, s.canonical.programNumber)
	}
	if candidate.videoType != s.canonical.videoType || candidate.videoType != 0x1b {
		return fmt.Errorf("候选源视频类型 0x%02x 不兼容", candidate.videoType)
	}
	if candidate.audioType != s.canonical.audioType || (candidate.audioType != 0x03 && candidate.audioType != 0x04) {
		return fmt.Errorf("候选源音频类型 0x%02x 不兼容", candidate.audioType)
	}
	if candidate.pcrPID != candidate.videoPID {
		return errors.New("候选源 PCR 不在 H.264 视频 PID 上")
	}
	return nil
}

func (s *StreamSwitcher) makeTransform(source programProfile, offset int64) *sourceTransform {
	return &sourceTransform{
		profile:           source,
		offset90:          offset,
		waitAudioPUSI:     true,
		markDiscontinuity: true,
		pidMap: map[uint16]uint16{
			0:               0,
			source.pmtPID:   s.canonical.pmtPID,
			source.videoPID: s.canonical.videoPID,
			source.audioPID: s.canonical.audioPID,
			tsNullPID:       tsNullPID,
		},
	}
}

func (s *StreamSwitcher) observeInitial(packet []byte) {
	pid := packetPID(packet)
	if pid == 0 {
		if payload, ok := packetPayload(packet); ok {
			if packetPUSI(packet) {
				payload = s.patPsi.resetPSI(payload)
			}
			for _, section := range s.patPsi.feed(payload) {
				if program, pmtPID, ok := parsePAT(section); ok {
					if s.canonical == nil {
						s.canonical = &programProfile{programNumber: program, pmtPID: pmtPID}
					} else {
						s.canonical.programNumber = program
						s.canonical.pmtPID = pmtPID
					}
					if version, ok := psiVersion(packet); ok {
						s.psiVersion = version
					}
					s.canonicalPAT = buildPATPacket(program, pmtPID, s.psiVersion)
				}
			}
		}
	}
	if s.canonical != nil && pid == s.canonical.pmtPID {
		if payload, ok := packetPayload(packet); ok {
			if packetPUSI(packet) {
				payload = s.pmtPsi.resetPSI(payload)
			}
			for _, section := range s.pmtPsi.feed(payload) {
				if profile, ok := parsePMT(section, s.canonical.pmtPID); ok {
					*s.canonical = profile
					s.canonicalPMT = buildPMTPacket(profile.programNumber, profile.pmtPID, profile.pcrPID, profile.videoPID, profile.audioPID, profile.videoType, profile.audioType, s.psiVersion)
				}
			}
		}
	}
	if s.canonical == nil || s.canonical.videoPID == 0 || pid != s.canonical.videoPID {
		return
	}
	if len(s.canonicalSPS) > 0 && len(s.canonicalPPS) > 0 {
		return
	}
	if packetPUSI(packet) {
		s.initialVideoES = nil
	}
	payload, ok := elementaryPayload(packet)
	if !ok {
		return
	}
	s.initialVideoES = append(s.initialVideoES, payload...)
	sps, pps, _ := scanH264(s.initialVideoES)
	if len(sps) > 0 {
		s.canonicalSPS = bytes.Clone(sps)
	}
	if len(pps) > 0 {
		s.canonicalPPS = bytes.Clone(pps)
	}
}

func (s *StreamSwitcher) transformPackets(packets [][]byte, transform *sourceTransform, prependPSI bool) ([]byte, error) {
	if prependPSI && (s.canonicalPAT == nil || s.canonicalPMT == nil) {
		return nil, errors.New("当前源缺少可复用的 PAT/PMT")
	}
	// Validate the complete batch before advancing output continuity/timestamp state.
	// A rejected candidate must leave the still-active source completely untouched.
	for _, input := range packets {
		sourcePID := packetPID(input)
		if _, ok := transform.pidMap[sourcePID]; !ok || sourcePID == 0 || sourcePID == transform.profile.pmtPID || sourcePID == tsNullPID {
			continue
		}
		if err := validatePESTimestampLayout(input); err != nil {
			return nil, err
		}
	}

	capacity := len(packets) * tsPacketSize
	if prependPSI {
		capacity += 2 * tsPacketSize
	}
	output := make([]byte, 0, capacity)
	if prependPSI {
		for _, table := range [][]byte{s.canonicalPAT, s.canonicalPMT} {
			packet := bytes.Clone(table)
			s.rewriteContinuity(packet, packetPID(packet))
			s.observeTransformedOutput(packet)
			output = append(output, packet...)
		}
	}

	for _, input := range packets {
		sourcePID := packetPID(input)
		outputPID, ok := transform.pidMap[sourcePID]
		if !ok {
			continue
		}
		if sourcePID == transform.profile.audioPID && transform.waitAudioPUSI {
			if !packetPUSI(input) {
				continue
			}
			transform.waitAudioPUSI = false
		}

		var packet []byte
		switch sourcePID {
		case 0:
			if s.canonicalPAT == nil {
				continue
			}
			packet = bytes.Clone(s.canonicalPAT)
			outputPID = 0
		case transform.profile.pmtPID:
			if s.canonicalPMT == nil {
				continue
			}
			packet = bytes.Clone(s.canonicalPMT)
			outputPID = s.canonical.pmtPID
		default:
			packet = bytes.Clone(input)
			rewritePID(packet, outputPID)
			rewritePCR(packet, transform.offset90)
			if transform.markDiscontinuity && sourcePID == transform.profile.pcrPID {
				if _, ok := packetPCRBase(packet); ok && setDiscontinuityIndicator(packet) {
					transform.markDiscontinuity = false
				}
			}
			if err := rewritePESTimestamps(packet, transform.offset90); err != nil {
				return nil, err
			}
		}

		s.rewriteContinuity(packet, outputPID)
		s.observeTransformedOutput(packet)
		output = append(output, packet...)
	}
	return output, nil
}

func (s *StreamSwitcher) observeInitialOutput(packet []byte) {
	pid := packetPID(packet)
	if packetHasPayload(packet) {
		s.lastCC[pid] = packet[3] & 0x0f
	}
	s.observeTimestamps(packet)
}

func (s *StreamSwitcher) observeTransformedOutput(packet []byte) {
	s.observeTimestamps(packet)
}

func (s *StreamSwitcher) observeTimestamps(packet []byte) {
	if s.canonical == nil {
		return
	}
	pid := packetPID(packet)
	if pid == s.canonical.pcrPID {
		if pcr, ok := packetPCRBase(packet); ok {
			s.lastPCR = pcr
			s.havePCR = true
		}
	}
	if pid != s.canonical.videoPID || !packetPUSI(packet) {
		return
	}
	pts, ok := packetPTS(packet)
	if !ok {
		return
	}
	if s.havePreviousPTS {
		delta := (pts - s.previousVideoPTS) & clockMask
		if delta > 0 && delta <= 90000 {
			s.videoStep = delta
		}
	}
	s.previousVideoPTS = pts
	s.havePreviousPTS = true
	s.lastVideoPTS = pts
	s.haveVideoPTS = true
}

func (s *StreamSwitcher) rewriteContinuity(packet []byte, pid uint16) {
	current, exists := s.lastCC[pid]
	if packetHasPayload(packet) {
		if exists {
			current = (current + 1) & 0x0f
		} else {
			current = packet[3] & 0x0f
		}
		s.lastCC[pid] = current
	} else if !exists {
		current = packet[3] & 0x0f
		s.lastCC[pid] = current
	}
	packet[3] = packet[3]&0xf0 | current
}

func splitTSPackets(data []byte) ([][]byte, error) {
	if len(data) == 0 || len(data)%tsPacketSize != 0 {
		return nil, fmt.Errorf("MPEG-TS 数据长度 %d 不是 188 的整数倍", len(data))
	}
	packets := make([][]byte, 0, len(data)/tsPacketSize)
	for offset := 0; offset < len(data); offset += tsPacketSize {
		packet := data[offset : offset+tsPacketSize]
		if packet[0] != tsSyncByte {
			return nil, fmt.Errorf("MPEG-TS 在偏移 %d 缺少同步字节", offset)
		}
		packets = append(packets, packet)
	}
	return packets, nil
}

func packetPID(packet []byte) uint16 {
	return uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
}

func rewritePID(packet []byte, pid uint16) {
	packet[1] = packet[1]&0xe0 | byte(pid>>8)&0x1f
	packet[2] = byte(pid)
}

func packetPUSI(packet []byte) bool {
	return packet[1]&0x40 != 0
}

func packetHasPayload(packet []byte) bool {
	afc := packet[3] >> 4 & 0x03
	return afc == 1 || afc == 3
}

func packetPayload(packet []byte) ([]byte, bool) {
	afc := packet[3] >> 4 & 0x03
	if afc != 1 && afc != 3 {
		return nil, false
	}
	offset := 4
	if afc == 3 {
		if offset >= len(packet) {
			return nil, false
		}
		offset += 1 + int(packet[offset])
	}
	if offset > len(packet) {
		return nil, false
	}
	return packet[offset:], true
}

func elementaryPayload(packet []byte) ([]byte, bool) {
	payload, ok := packetPayload(packet)
	if !ok || len(payload) == 0 {
		return nil, false
	}
	if !packetPUSI(packet) {
		return payload, true
	}
	if len(payload) < 9 || !bytes.Equal(payload[:3], []byte{0, 0, 1}) {
		return nil, false
	}
	offset := 9 + int(payload[8])
	if offset > len(payload) {
		return nil, false
	}
	return payload[offset:], true
}

// parsePAT 解析一个完整 PAT section（含 CRC，可由 psiReassembler 跨包拼出）。
func parsePAT(section []byte) (uint16, uint16, bool) {
	if len(section) < 12 || section[0] != 0x00 {
		return 0, 0, false
	}
	sectionLength := int(section[1]&0x0f)<<8 | int(section[2])
	end := 3 + sectionLength - 4 // exclude CRC32
	if end > len(section) || end < 8 {
		return 0, 0, false
	}
	for offset := 8; offset+4 <= end; offset += 4 {
		program := uint16(section[offset])<<8 | uint16(section[offset+1])
		if program == 0 {
			continue
		}
		pmtPID := uint16(section[offset+2]&0x1f)<<8 | uint16(section[offset+3])
		return program, pmtPID, true
	}
	return 0, 0, false
}

// parsePMT 解析一个完整 PMT section（含 CRC，可由 psiReassembler 跨包拼出）。
func parsePMT(section []byte, pmtPID uint16) (programProfile, bool) {
	if len(section) < 16 || section[0] != 0x02 {
		return programProfile{}, false
	}
	sectionLength := int(section[1]&0x0f)<<8 | int(section[2])
	end := 3 + sectionLength - 4
	if end > len(section) || end < 12 {
		return programProfile{}, false
	}
	profile := programProfile{
		programNumber: uint16(section[3])<<8 | uint16(section[4]),
		pmtPID:        pmtPID,
		pcrPID:        uint16(section[8]&0x1f)<<8 | uint16(section[9]),
	}
	programInfoLength := int(section[10]&0x0f)<<8 | int(section[11])
	for offset := 12 + programInfoLength; offset+5 <= end; {
		streamType := section[offset]
		pid := uint16(section[offset+1]&0x1f)<<8 | uint16(section[offset+2])
		esInfoLength := int(section[offset+3]&0x0f)<<8 | int(section[offset+4])
		if offset+5+esInfoLength > end {
			return programProfile{}, false
		}
		switch streamType {
		case 0x1b, 0x24, 0x10, 0x02:
			if profile.videoPID == 0 {
				profile.videoPID = pid
				profile.videoType = streamType
			}
		case 0x03, 0x04:
			if profile.audioPID == 0 {
				profile.audioPID = pid
				profile.audioType = streamType
			}
		}
		offset += 5 + esInfoLength
	}
	if profile.videoPID == 0 || profile.audioPID == 0 {
		return programProfile{}, false
	}
	return profile, true
}

func psiVersion(packet []byte) (byte, bool) {
	payload, ok := packetPayload(packet)
	if !ok || !packetPUSI(packet) || len(payload) < 1 {
		return 0, false
	}
	offset := 1 + int(payload[0])
	if offset+3 > len(payload) {
		return 0, false
	}
	sectionLength := int(payload[offset+1]&0x0f)<<8 | int(payload[offset+2])
	if offset+3+sectionLength > len(payload) || sectionLength < 5 {
		return 0, false
	}
	return (payload[offset+5] >> 1) & 0x1f, true
}

func scanH264(data []byte) (sps, pps []byte, idr bool) {
	type start struct {
		code int
		nal  int
	}
	starts := make([]start, 0, 8)
	for i := 0; i+3 < len(data); {
		switch {
		case i+4 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1:
			starts = append(starts, start{code: i, nal: i + 4})
			i += 4
		case data[i] == 0 && data[i+1] == 0 && data[i+2] == 1:
			starts = append(starts, start{code: i, nal: i + 3})
			i += 3
		default:
			i++
		}
	}
	for i, st := range starts {
		if st.nal >= len(data) {
			continue
		}
		end := len(data)
		if i+1 < len(starts) {
			end = starts[i+1].code
		}
		if end <= st.nal {
			continue
		}
		nal := data[st.nal:end]
		switch nal[0] & 0x1f {
		case 5:
			idr = true
		case 7:
			sps = bytes.Clone(nal)
		case 8:
			pps = bytes.Clone(nal)
		}
	}
	return sps, pps, idr
}

func packetPTS(packet []byte) (uint64, bool) {
	payload, ok := packetPayload(packet)
	if !ok || !packetPUSI(packet) || len(payload) < 14 || !bytes.Equal(payload[:3], []byte{0, 0, 1}) {
		return 0, false
	}
	if payload[7]&0x80 == 0 || int(payload[8])+9 > len(payload) {
		return 0, false
	}
	return decodeTimestamp(payload[9:14]), true
}

func decodeTimestamp(field []byte) uint64 {
	return (uint64(field[0]>>1&0x07) << 30) |
		(uint64(field[1]) << 22) |
		(uint64(field[2]>>1) << 15) |
		(uint64(field[3]) << 7) |
		uint64(field[4]>>1)
}

func encodeTimestamp(field []byte, value uint64) {
	value &= clockMask
	field[0] = field[0]&0xf0 | byte(value>>30&0x07)<<1 | 1
	field[1] = byte(value >> 22)
	field[2] = byte(value>>15&0x7f)<<1 | 1
	field[3] = byte(value >> 7)
	field[4] = byte(value&0x7f)<<1 | 1
}

func addClock(value uint64, offset int64) uint64 {
	return uint64((int64(value) + offset) & int64(clockMask))
}

func validatePESTimestampLayout(packet []byte) error {
	payload, ok := packetPayload(packet)
	if !ok || !packetPUSI(packet) {
		return nil
	}
	if len(payload) < 3 || !bytes.Equal(payload[:3], []byte{0, 0, 1}) {
		return nil
	}
	if len(payload) < 9 {
		return errors.New("PES 头跨越 TS 包，无法安全重写时间戳")
	}
	headerEnd := 9 + int(payload[8])
	if headerEnd > len(payload) {
		return errors.New("PES 可选头跨越 TS 包，无法安全重写时间戳")
	}
	flags := payload[7] >> 6 & 0x03
	if (flags == 2 || flags == 3) && len(payload) < 14 {
		return errors.New("PTS 字段跨越 TS 包，无法安全重写")
	}
	if flags == 3 && len(payload) < 19 {
		return errors.New("DTS 字段跨越 TS 包，无法安全重写")
	}
	return nil
}

func rewritePESTimestamps(packet []byte, offset int64) error {
	if err := validatePESTimestampLayout(packet); err != nil {
		return err
	}
	payload, ok := packetPayload(packet)
	if !ok || !packetPUSI(packet) || len(payload) < 9 || !bytes.Equal(payload[:3], []byte{0, 0, 1}) {
		return nil
	}
	flags := payload[7] >> 6 & 0x03
	if flags == 2 || flags == 3 {
		encodeTimestamp(payload[9:14], addClock(decodeTimestamp(payload[9:14]), offset))
	}
	if flags == 3 {
		encodeTimestamp(payload[14:19], addClock(decodeTimestamp(payload[14:19]), offset))
	}
	return nil
}

func setDiscontinuityIndicator(packet []byte) bool {
	afc := packet[3] >> 4 & 0x03
	if afc != 2 && afc != 3 || len(packet) < 6 || packet[4] < 1 {
		return false
	}
	packet[5] |= 0x80
	return true
}

func packetPCRBase(packet []byte) (uint64, bool) {
	afc := packet[3] >> 4 & 0x03
	if afc != 2 && afc != 3 || len(packet) < 12 || packet[4] < 7 || packet[5]&0x10 == 0 {
		return 0, false
	}
	// MPEG-2 规范布局：b3=base 8..2(7位)+保留位；b4 最高两位=base 1..0
	field := packet[6:12]
	return uint64(field[0])<<25 |
		uint64(field[1])<<17 |
		uint64(field[2])<<9 |
		uint64((field[3]>>1)&0x7f)<<2 |
		uint64((field[4]>>7)&1)<<1 |
		uint64((field[4]>>6)&1), true
}

func rewritePCR(packet []byte, offset int64) {
	afc := packet[3] >> 4 & 0x03
	if afc != 2 && afc != 3 || len(packet) < 12 || packet[4] < 7 || packet[5]&0x10 == 0 {
		return
	}
	field := packet[6:12]
	// MPEG-2 规范布局：b3=base 8..2(7位)+保留1；b4=base 1..0+保留01+PCR_ext(0)
	base := uint64(field[0])<<25 |
		uint64(field[1])<<17 |
		uint64(field[2])<<9 |
		uint64((field[3]>>1)&0x7f)<<2 |
		uint64((field[4]>>7)&1)<<1 |
		uint64((field[4]>>6)&1)
	base = addClock(base, offset)
	field[0] = byte(base >> 25)
	field[1] = byte(base >> 17)
	field[2] = byte(base >> 9)
	field[3] = byte((base>>2)&0x7f)<<1 | 1
	field[4] = byte((base>>1)&0x03) << 6 // base 1..0 + PCR_ext=0
	field[5] = 0xff // 保留位 11111111
}

func buildPATPacket(program, pmtPID uint16, version byte) []byte {
	section := []byte{
		0x00, 0x00, 0x00, // table_id, section_syntax_indicator + section_length (placeholder)
		0x00, 0x01, // transport_stream_id（规范不要求与 program number 相同，固定 1）
		0xc0 | (version&0x1f)<<1 | 0x01, // reserved + version_number + current_next_indicator
		0x00, 0x00,                      // section_number, last_section_number
		byte(program >> 8), byte(program), // program_number
		0xe0 | byte(pmtPID>>8), byte(pmtPID), // reserved + PMT PID
	}
	sectionLength := len(section) - 3 + 4 // exclude header bytes before length, plus CRC32
	section[1] = 0xb0 | byte(sectionLength>>8)
	section[2] = byte(sectionLength)
	crc := mpegCRC32(section[:len(section)])
	section = append(section, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return makePSITSPacket(0, 0, section)
}

func buildPMTPacket(program, pmtPID, pcrPID, videoPID, audioPID uint16, videoType, audioType, version byte) []byte {
	section := []byte{
		0x02, 0x00, 0x00, // table_id, section_syntax_indicator + section_length (placeholder)
		byte(program >> 8), byte(program), // program_number
		0xc0 | (version&0x1f)<<1 | 0x01, // reserved + version_number + current_next_indicator
		0x00, 0x00,                      // section_number, last_section_number
		0xe0 | byte(pcrPID>>8), byte(pcrPID), // reserved + PCR PID
		0xf0, 0x00, // reserved + program_info_length (0)
		videoType, 0xe0 | byte(videoPID>>8), byte(videoPID), 0xf0, 0x00,
		audioType, 0xe0 | byte(audioPID>>8), byte(audioPID), 0xf0, 0x00,
	}
	sectionLength := len(section) - 3 + 4
	section[1] = 0xb0 | byte(sectionLength>>8)
	section[2] = byte(sectionLength)
	crc := mpegCRC32(section[:len(section)])
	section = append(section, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return makePSITSPacket(pmtPID, 0, section)
}

func makePSITSPacket(pid uint16, cc byte, section []byte) []byte {
	packet := make([]byte, tsPacketSize)
	packet[0] = 0x47
	packet[1] = 0x40 | byte(pid>>8)&0x1f
	packet[2] = byte(pid)
	packet[3] = 0x10 | cc&0x0f
	packet[4] = 0 // pointer field
	copy(packet[5:], section)
	return packet
}

var mpegCRC32Table [256]uint32

func init() {
	const poly = 0x04c11db7
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ poly
			} else {
				crc <<= 1
			}
		}
		mpegCRC32Table[i] = crc
	}
}

func mpegCRC32(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc = crc<<8 ^ mpegCRC32Table[byte(crc>>24)^b]
	}
	return crc
}
