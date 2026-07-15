package promptfilter

import (
	"bytes"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
)

var ErrOutputBlocked = errors.New("stream output blocked by prompt filter")

type OutputScanner struct {
	cfg          Config
	pending      []byte
	recordBuffer []byte
	semantic     []byte
}

func NewOutputScanner(cfg Config) *OutputScanner {
	cfg = NormalizeConfig(cfg)
	if !cfg.Enabled || !cfg.Advanced.Output.Enabled {
		return nil
	}
	return &OutputScanner{cfg: cfg}
}

func (s *OutputScanner) Push(data []byte) ([]byte, error) {
	if s == nil {
		return data, nil
	}
	s.pending = append(s.pending, data...)
	semantic, terminal := s.consumeRecords(data)
	s.appendSemantic(semantic)
	verdict := InspectText(string(s.semantic), s.cfg)
	blocked := verdict.TerminalStrictHit
	if !s.cfg.Advanced.Output.StrictOnly {
		blocked = verdict.Action == ActionBlock
	}
	if blocked {
		s.pending = nil
		s.semantic = nil
		s.recordBuffer = nil
		return nil, ErrOutputBlocked
	}
	if terminal {
		release := append([]byte(nil), s.pending...)
		s.pending = nil
		s.semantic = nil
		s.recordBuffer = nil
		return release, nil
	}
	keep := s.cfg.Advanced.Output.BufferBytes
	if len(s.pending) <= keep {
		return nil, nil
	}
	releaseLen := len(s.pending) - keep
	release := append([]byte(nil), s.pending[:releaseLen]...)
	s.pending = append(s.pending[:0], s.pending[releaseLen:]...)
	return release, nil
}

func (s *OutputScanner) Flush() ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	verdict := InspectText(string(s.semantic), s.cfg)
	blocked := verdict.TerminalStrictHit
	if !s.cfg.Advanced.Output.StrictOnly {
		blocked = verdict.Action == ActionBlock
	}
	if blocked {
		s.pending = nil
		s.semantic = nil
		s.recordBuffer = nil
		return nil, ErrOutputBlocked
	}
	// Transport Flush is not a semantic end-of-stream. Keep the safety window
	// until a terminal SSE event arrives so matches split across events remain detectable.
	return nil, nil
}

func (s *OutputScanner) Finalize() ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	if len(s.recordBuffer) > 0 {
		semantic, _ := extractOutputRecord(s.recordBuffer, false)
		s.appendSemantic(semantic)
		s.recordBuffer = nil
	}
	verdict := InspectText(string(s.semantic), s.cfg)
	blocked := verdict.TerminalStrictHit
	if !s.cfg.Advanced.Output.StrictOnly {
		blocked = verdict.Action == ActionBlock
	}
	if blocked {
		s.pending = nil
		s.semantic = nil
		return nil, ErrOutputBlocked
	}
	out := append([]byte(nil), s.pending...)
	s.pending = nil
	s.semantic = nil
	return out, nil
}

func (s *OutputScanner) appendSemantic(data []byte) {
	if len(data) == 0 {
		return
	}
	s.semantic = append(s.semantic, data...)
	window := s.cfg.Advanced.Output.BufferBytes + s.cfg.Advanced.Output.OverlapBytes
	if window < 1024 {
		window = 1024
	}
	if len(s.semantic) > window {
		s.semantic = append(s.semantic[:0], s.semantic[len(s.semantic)-window:]...)
	}
}

func (s *OutputScanner) consumeRecords(data []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(s.recordBuffer) == 0 && gjson.ValidBytes(trimmed) {
		return extractOutputRecord(trimmed, false)
	}
	if len(s.recordBuffer) == 0 && !bytes.Contains(data, []byte("data:")) && len(trimmed) > 0 && trimmed[0] != '{' && trimmed[0] != '[' {
		return append([]byte(nil), data...), false
	}
	s.recordBuffer = append(s.recordBuffer, data...)
	if complete := bytes.TrimSpace(s.recordBuffer); gjson.ValidBytes(complete) {
		semantic, terminal := extractOutputRecord(complete, false)
		s.recordBuffer = nil
		return semantic, terminal
	}
	var semantic []byte
	terminal := false
	for {
		index := bytes.IndexByte(s.recordBuffer, '\n')
		if index < 0 {
			break
		}
		line := append([]byte(nil), bytes.TrimSpace(s.recordBuffer[:index])...)
		s.recordBuffer = append(s.recordBuffer[:0], s.recordBuffer[index+1:]...)
		isDataRecord := bytes.HasPrefix(line, []byte("data:"))
		if isDataRecord {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		text, done := extractOutputRecord(line, isDataRecord)
		semantic = append(semantic, text...)
		terminal = terminal || done
	}
	return semantic, terminal
}

func extractOutputRecord(record []byte, allowDone bool) ([]byte, bool) {
	record = bytes.TrimSpace(record)
	if len(record) == 0 {
		return nil, false
	}
	if allowDone && bytes.Equal(record, []byte("[DONE]")) {
		return nil, true
	}
	if !gjson.ValidBytes(record) {
		return append([]byte(nil), record...), false
	}
	parsed := gjson.ParseBytes(record)
	eventType := parsed.Get("type").String()
	terminal := eventType == "response.completed" || eventType == "response.failed" || eventType == "message_stop"
	return []byte(ExtractOutputText(record)), terminal
}

func ExtractOutputText(data []byte) string {
	var out strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || !gjson.Valid(line) {
			continue
		}
		parsed := gjson.Parse(line)
		for _, path := range []string{"delta", "choices.0.delta.content", "delta.text", "content_block.text", "content.0.text"} {
			if value := parsed.Get(path); value.Exists() && value.Type == gjson.String {
				out.WriteString(value.String())
				break
			}
		}
	}
	return out.String()
}
