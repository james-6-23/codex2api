package promptfilter

import (
	"bytes"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
)

var ErrOutputBlocked = errors.New("stream output blocked by prompt filter")

type OutputScanner struct {
	cfg      Config
	pending  []byte
	semantic strings.Builder
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
	semantic := ExtractOutputText(data)
	if semantic == "" && !bytes.Contains(data, []byte("data:")) && !gjson.ValidBytes(bytes.TrimSpace(data)) {
		semantic = string(data)
	}
	if semantic != "" {
		s.semantic.WriteString(semantic)
	}
	verdict := InspectText(s.semantic.String(), s.cfg)
	blocked := verdict.TerminalStrictHit
	if !s.cfg.Advanced.Output.StrictOnly {
		blocked = verdict.Action == ActionBlock
	}
	if blocked {
		s.pending = nil
		s.semantic.Reset()
		return nil, ErrOutputBlocked
	}
	terminal := bytes.Contains(data, []byte("[DONE]")) || bytes.Contains(data, []byte(`"response.completed"`)) || bytes.Contains(data, []byte(`"message_stop"`))
	if terminal {
		release := append([]byte(nil), s.pending...)
		s.pending = nil
		s.semantic.Reset()
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
	verdict := InspectText(s.semantic.String(), s.cfg)
	blocked := verdict.TerminalStrictHit
	if !s.cfg.Advanced.Output.StrictOnly {
		blocked = verdict.Action == ActionBlock
	}
	if blocked {
		s.pending = nil
		s.semantic.Reset()
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
	verdict := InspectText(s.semantic.String(), s.cfg)
	blocked := verdict.TerminalStrictHit
	if !s.cfg.Advanced.Output.StrictOnly {
		blocked = verdict.Action == ActionBlock
	}
	if blocked {
		s.pending = nil
		s.semantic.Reset()
		return nil, ErrOutputBlocked
	}
	out := append([]byte(nil), s.pending...)
	s.pending = nil
	s.semantic.Reset()
	return out, nil
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
