package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

const (
	// The observer is content-blind, so it only needs enough space for one
	// realistic wire frame and its held tail. Rejecting larger declarations
	// keeps a peer from turning an incomplete frame into an unbounded allocation.
	goldenWebSocketMaxFrameBytes    = 16 << 20
	goldenWebSocketMaxBufferedBytes = 32 << 20
)

// goldenWebSocketFrameParser reassembles complete wire frames without
// inspecting their payload. WebSocket frames are transport records, so the
// continuity pacer must never release a partial record to a client.
type goldenWebSocketFrameParser struct {
	pending []byte
}

func (p *goldenWebSocketFrameParser) append(payload []byte) ([][]byte, error) {
	if len(payload) > goldenWebSocketMaxBufferedBytes ||
		len(p.pending) > goldenWebSocketMaxBufferedBytes-len(payload) {
		return nil, errors.New("golden WebSocket frame buffer exceeds limit")
	}
	if len(payload) > 0 {
		p.pending = append(p.pending, payload...)
	}
	frames := make([][]byte, 0, 1)
	for len(p.pending) > 0 {
		frameLength, complete, err := goldenWebSocketFrameLength(p.pending)
		if err != nil {
			return nil, err
		}
		if !complete {
			break
		}
		frame := append([]byte(nil), p.pending[:frameLength]...)
		frames = append(frames, frame)
		p.pending = p.pending[frameLength:]
	}
	if len(p.pending) == 0 {
		p.pending = nil
	}
	return frames, nil
}

func goldenWebSocketFrameLength(payload []byte) (length int, complete bool, err error) {
	if len(payload) < 2 {
		return 0, false, nil
	}
	payloadLength := uint64(payload[1] & 0x7f)
	headerLength := 2
	switch payloadLength {
	case 126:
		if len(payload) < headerLength+2 {
			return 0, false, nil
		}
		payloadLength = uint64(binary.BigEndian.Uint16(payload[headerLength : headerLength+2]))
		headerLength += 2
	case 127:
		if len(payload) < headerLength+8 {
			return 0, false, nil
		}
		extended := binary.BigEndian.Uint64(payload[headerLength : headerLength+8])
		if extended&(uint64(1)<<63) != 0 {
			return 0, false, errors.New("golden WebSocket frame length overflows 63 bits")
		}
		payloadLength = extended
		headerLength += 8
	}
	if payload[1]&0x80 != 0 {
		headerLength += 4
	}
	maxInt := uint64(^uint(0) >> 1)
	if payloadLength > maxInt-uint64(headerLength) {
		return 0, false, errors.New("golden WebSocket frame length overflows int")
	}
	frameLength := headerLength + int(payloadLength)
	if frameLength > goldenWebSocketMaxFrameBytes {
		return 0, false, errors.New("golden WebSocket frame exceeds size limit")
	}
	if len(payload) < frameLength {
		return 0, false, nil
	}
	return frameLength, true, nil
}

// goldenWebSocketPacer applies the golden response gate to complete
// WebSocket frames. It shares supersession and release state with the generic
// response pacer, but uses one frame per pacing interval instead of splitting
// arbitrary byte slices.
type goldenWebSocketPacer struct {
	base         *goldenResponsePacer
	mu           sync.Mutex
	parser       goldenWebSocketFrameParser
	pending      [][]byte
	pendingBytes int
	started      bool
	sink         func([]byte) (int, error)
}

func newGoldenWebSocketPacer(base *goldenResponsePacer) *goldenWebSocketPacer {
	if base == nil {
		return nil
	}
	return &goldenWebSocketPacer{
		base: base,
	}
}

func (p *goldenWebSocketPacer) releaseRequest() {
	if p != nil {
		p.base.releaseRequest()
	}
}

func (p *goldenWebSocketPacer) hasPayload() bool {
	return p != nil && p.base.hasPayload()
}

func (p *goldenWebSocketPacer) write(
	ctx context.Context,
	payload []byte,
	write func([]byte) (int, error),
) (int, error) {
	if p == nil || len(payload) == 0 {
		return write(payload)
	}
	p.base.payloadSeen.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = write
	if p.base.wasSuperseded() {
		p.clearLocked()
		return len(payload), nil
	}
	bufferedBytes := p.pendingBytes + len(p.parser.pending)
	if bufferedBytes > goldenWebSocketMaxBufferedBytes ||
		len(payload) > goldenWebSocketMaxBufferedBytes-bufferedBytes {
		p.base.releaseRequest()
		return 0, errors.New("golden WebSocket frame buffer exceeds limit")
	}
	frames, err := p.parser.append(payload)
	if err != nil {
		p.base.releaseRequest()
		return 0, err
	}
	for _, frame := range frames {
		if p.pendingBytes > goldenWebSocketMaxBufferedBytes-len(frame) {
			p.base.releaseRequest()
			return 0, errors.New("golden WebSocket frame buffer exceeds limit")
		}
		p.pending = append(p.pending, frame)
		p.pendingBytes += len(frame)
		if err := p.flushEligibleLocked(ctx); err != nil {
			p.base.releaseRequest()
			return 0, err
		}
	}
	if err := p.flushEligibleLocked(ctx); err != nil {
		p.base.releaseRequest()
		return 0, err
	}
	return len(payload), nil
}

func (p *goldenWebSocketPacer) flushEligibleLocked(ctx context.Context) error {
	for len(p.pending) > 0 && p.shouldFlushFrameLocked() {
		if err := p.writeQueuedFrameLocked(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *goldenWebSocketPacer) writeQueuedFrameLocked(ctx context.Context) error {
	frame := p.pending[0]
	written, err := p.writeFrameLocked(ctx, frame)
	if written < 0 || written > len(frame) {
		return errors.New("golden WebSocket writer returned an invalid byte count")
	}
	if written > 0 {
		p.pendingBytes -= written
		if written == len(frame) {
			p.pending[0] = nil
			p.pending = p.pending[1:]
		} else {
			p.pending[0] = append([]byte(nil), frame[written:]...)
		}
	}
	if err != nil {
		return err
	}
	if written != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

func (p *goldenWebSocketPacer) shouldFlushFrameLocked() bool {
	if p.base.wasSuperseded() || p.base.isReleased() {
		return true
	}
	// Control frames must not wait behind the deployment gate. They carry
	// heartbeats and close handshakes, and delaying them can make an otherwise
	// healthy peer time out while data is held. Flush any preceding data frames
	// too, so the wire order stays intact.
	if p.hasPendingImmediateControlFrameLocked() || goldenWebSocketImmediateControlFramePrefix(p.parser.pending) {
		return true
	}
	// Include an incomplete trailing frame in the held tail. If there is only
	// one complete frame and no trailing bytes, retain it until release so a
	// finite response cannot complete early.
	tailBytes := p.pendingBytes + len(p.parser.pending)
	if tailBytes <= p.base.holdbackBytes {
		return false
	}
	return len(p.pending) > 1 || len(p.parser.pending) > 0
}

func (p *goldenWebSocketPacer) hasPendingImmediateControlFrameLocked() bool {
	for _, frame := range p.pending {
		if goldenWebSocketImmediateControlFrame(frame) {
			return true
		}
	}
	return false
}

func goldenWebSocketImmediateControlFramePrefix(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	opcode := payload[0] & 0x0f
	return opcode == 0x9 || opcode == 0xa
}

func goldenWebSocketImmediateControlFrame(frame []byte) bool {
	return goldenWebSocketImmediateControlFramePrefix(frame)
}

func (p *goldenWebSocketPacer) writeFrameLocked(ctx context.Context, frame []byte) (int, error) {
	if p.sink == nil {
		return 0, errors.New("golden WebSocket pacing sink is unavailable")
	}
	if p.base.wasSuperseded() {
		return len(frame), nil
	}
	if p.started && !p.base.isReleased() && !goldenWebSocketImmediateControlFrame(frame) &&
		!p.hasPendingImmediateControlFrameLocked() && !goldenWebSocketImmediateControlFramePrefix(p.parser.pending) {
		if err := p.base.delay.wait(ctx, p.base.gateReleased, p.base.requestReleased, p.base.interval); err != nil {
			return 0, err
		}
		if p.base.wasSuperseded() {
			return len(frame), nil
		}
	}
	// Complete frames are forwarded as whole records. The held tail keeps the
	// request open across the deployment boundary while the interval preserves
	// the pacing runway provided by the generic response pacer.
	p.started = true
	return goldenWriteAll(p.sink, frame)
}

func goldenWriteAll(write func([]byte) (int, error), payload []byte) (int, error) {
	total := 0
	for len(payload) > 0 {
		n, err := write(payload)
		if n < 0 || n > len(payload) {
			return total, errors.New("golden WebSocket writer returned an invalid byte count")
		}
		total += n
		payload = payload[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (p *goldenWebSocketPacer) clearLocked() {
	p.parser.pending = nil
	p.pending = nil
	p.pendingBytes = 0
}

func (p *goldenWebSocketPacer) waitAndFlush() error {
	if p == nil {
		return nil
	}
	select {
	case <-p.base.gateReleased:
	case <-p.base.requestReleased:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.base.wasSuperseded() {
		p.clearLocked()
		return nil
	}
	if len(p.parser.pending) != 0 {
		return errors.New("golden WebSocket stream ended with an incomplete frame")
	}
	for len(p.pending) > 0 {
		if err := p.writeQueuedFrameLocked(context.Background()); err != nil {
			return err
		}
	}
	return nil
}
