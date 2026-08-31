package main

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestGoldenWebSocketPacerKeepsFramesIntactWhileGateIsHeld(t *testing.T) {
	gate := newGoldenResponseGate()
	base := gate.newResponsePacer("0123456789abcdef0123456789abcdef")
	pacer := newGoldenWebSocketPacer(base)
	if pacer == nil {
		t.Fatal("newGoldenWebSocketPacer returned nil")
	}

	var writes [][]byte
	sink := func(payload []byte) (int, error) {
		writes = append(writes, append([]byte(nil), payload...))
		return len(payload), nil
	}
	frames := make([][]byte, 0, 128)
	for index := 0; index < 128; index++ {
		frame := []byte{0x81, 0x01, byte('a' + index%26)}
		frames = append(frames, frame)
	}
	stream := bytes.Join(frames, nil)
	for offset := 0; offset < len(stream); {
		end := offset + 7
		if end > len(stream) {
			end = len(stream)
		}
		if n, err := pacer.write(context.Background(), stream[offset:end], sink); err != nil {
			t.Fatalf("write: %v", err)
		} else if n != end-offset {
			t.Fatalf("write count = %d, want %d", n, end-offset)
		}
		offset = end
	}

	if len(writes) == 0 {
		t.Fatal("pacer did not emit any complete frame while the stream was active")
	}
	for index, write := range writes {
		if len(write)%3 != 0 {
			t.Fatalf("write %d split a WebSocket frame: %d bytes", index, len(write))
		}
		for offset := 0; offset < len(write); offset += 3 {
			if write[offset] != 0x81 || write[offset+1] != 0x01 {
				t.Fatalf("write %d contains malformed frame at byte %d: %x", index, offset, write[offset:offset+3])
			}
		}
	}

	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatalf("waitAndFlush: %v", err)
	}
	var delivered bytes.Buffer
	for _, write := range writes {
		_, _ = delivered.Write(write)
	}
	if !bytes.Equal(delivered.Bytes(), stream) {
		t.Fatalf("delivered stream differs from input: got %d bytes, want %d", delivered.Len(), len(stream))
	}
}

func TestGoldenWebSocketFrameParserSupportsFragmentedExtendedFrames(t *testing.T) {
	parser := goldenWebSocketFrameParser{}
	payload := bytes.Repeat([]byte{'x'}, 126)
	frame := append([]byte{0x82, 126, 0, 126}, payload...)
	frame = append(frame, 0x88, 0x00)

	var frames [][]byte
	for _, chunk := range [][]byte{frame[:1], frame[1:3], frame[3:17], frame[17:]} {
		parsed, err := parser.append(chunk)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		frames = append(frames, parsed...)
	}
	if len(frames) != 2 {
		t.Fatalf("parsed %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0], frame[:len(frame)-2]) {
		t.Fatalf("extended frame changed during parsing")
	}
	if !bytes.Equal(frames[1], frame[len(frame)-2:]) {
		t.Fatalf("control frame changed during parsing")
	}
	if pending := parser.pending; len(pending) != 0 {
		t.Fatalf("parser retained %d bytes after complete frames", len(pending))
	}
}

func TestGoldenWebSocketFrameParserRejectsInvalidLength(t *testing.T) {
	parser := goldenWebSocketFrameParser{}
	_, err := parser.append([]byte{0x82, 0x7f, 0x80, 0, 0, 0, 0, 0, 0})
	if err == nil {
		t.Fatal("parser accepted a frame with the reserved 64-bit length bit set")
	}
	if err == io.EOF {
		t.Fatal("parser reported an incomplete frame instead of invalid length")
	}
}
