package proxy

import (
	"bytes"
	"fmt"
	"io"
)

// Grok native protocol streams must remain wire-transparent. The generic
// ReadSSEStream helper intentionally exposes only joined data fields, which is
// ideal for protocol translation but loses event/id/retry/comment fields and
// the Chat Completions [DONE] sentinel. rawGrokSSEFrame keeps both views: Raw is
// the exact upstream frame (including its original line endings and separator),
// while Data is the standard SSE data-field projection used only for terminal,
// visibility, and usage inspection.
type rawGrokSSEFrame struct {
	Raw     []byte
	Data    []byte
	Event   string
	HasData bool
	Done    bool
}

const (
	// A single native event can contain large reasoning/tool arguments, but an
	// unbounded line/frame would let a malformed upstream exhaust the process.
	grokMaxNativeSSEFrameBytes   = 16 << 20
	grokMaxNativeSSEPendingBytes = 16 << 20
)

func parseRawGrokSSEFrame(raw []byte) rawGrokSSEFrame {
	frame := rawGrokSSEFrame{Raw: raw}
	dataLines := make([][]byte, 0, 1)
	for remaining := raw; len(remaining) > 0; {
		lineEnd := bytes.IndexByte(remaining, '\n')
		var line []byte
		if lineEnd < 0 {
			line, remaining = remaining, nil
		} else {
			line, remaining = remaining[:lineEnd], remaining[lineEnd+1:]
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.Equal(line, []byte("event")) {
			frame.Event = ""
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			value := line[len("event:"):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			frame.Event = string(value)
			continue
		}
		if bytes.Equal(line, []byte("data")) {
			dataLines = append(dataLines, nil)
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := line[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		dataLines = append(dataLines, value)
	}
	if len(dataLines) == 0 {
		return frame
	}
	frame.HasData = true
	if len(dataLines) == 1 {
		frame.Data = dataLines[0]
	} else {
		frame.Data = bytes.Join(dataLines, []byte{'\n'})
	}
	frame.Done = bytes.Equal(frame.Data, []byte("[DONE]"))
	return frame
}

// readRawGrokSSEFrames incrementally frames SSE without normalizing a single
// byte. callback returning false stops successfully, matching ReadSSEStream.
func readRawGrokSSEFrames(body io.Reader, callback func(rawGrokSSEFrame) bool) error {
	if body == nil {
		return fmt.Errorf("nil SSE body")
	}
	readBuf := make([]byte, 64*1024)
	lineBuf := make([]byte, 0, 64*1024)
	frameBuf := make([]byte, 0, 64*1024)

	emit := func() bool {
		if len(frameBuf) == 0 {
			return true
		}
		keepReading := callback(parseRawGrokSSEFrame(frameBuf))
		frameBuf = frameBuf[:0]
		return keepReading
	}

	for {
		n, readErr := body.Read(readBuf)
		if n > 0 {
			lineBuf = append(lineBuf, readBuf[:n]...)
			consumed := 0
			for {
				relativeEnd := bytes.IndexByte(lineBuf[consumed:], '\n')
				if relativeEnd < 0 {
					break
				}
				lineEnd := consumed + relativeEnd
				lineWithEnding := lineBuf[consumed : lineEnd+1]
				frameBuf = append(frameBuf, lineWithEnding...)
				if len(frameBuf) > grokMaxNativeSSEFrameBytes {
					return fmt.Errorf("Grok SSE frame exceeds %d bytes", grokMaxNativeSSEFrameBytes)
				}
				line := lineBuf[consumed:lineEnd]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				consumed = lineEnd + 1
				if len(line) == 0 && !emit() {
					return nil
				}
			}
			if consumed > 0 {
				remaining := copy(lineBuf, lineBuf[consumed:])
				lineBuf = lineBuf[:remaining]
			}
			if len(frameBuf)+len(lineBuf) > grokMaxNativeSSEFrameBytes {
				return fmt.Errorf("Grok SSE frame exceeds %d bytes", grokMaxNativeSSEFrameBytes)
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			if len(lineBuf) > 0 {
				frameBuf = append(frameBuf, lineBuf...)
				if len(frameBuf) > grokMaxNativeSSEFrameBytes {
					return fmt.Errorf("Grok SSE frame exceeds %d bytes", grokMaxNativeSSEFrameBytes)
				}
			}
			if !emit() {
				return nil
			}
			return nil
		}
	}
}
