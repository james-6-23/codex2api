package proxy

import (
	"bytes"

	"github.com/codex2api/security/promptfilter"
)

type wsPromptOutputBuffer struct {
	cfg      promptfilter.Config
	messages [][]byte
	size     int
}

func newWSPromptOutputBuffer(cfg promptfilter.Config) *wsPromptOutputBuffer {
	if !cfg.Enabled || !cfg.Advanced.Output.Enabled {
		return nil
	}
	return &wsPromptOutputBuffer{cfg: cfg}
}

func (b *wsPromptOutputBuffer) Push(message []byte) ([][]byte, error) {
	if b == nil {
		return [][]byte{message}, nil
	}
	b.messages = append(b.messages, append([]byte(nil), message...))
	b.size += len(message)
	if b.blocked() {
		b.messages = nil
		b.size = 0
		return nil, promptfilter.ErrOutputBlocked
	}
	var release [][]byte
	for len(b.messages) > 1 && b.size > b.cfg.Advanced.Output.BufferBytes {
		release = append(release, b.messages[0])
		b.size -= len(b.messages[0])
		b.messages = b.messages[1:]
	}
	return release, nil
}

func (b *wsPromptOutputBuffer) Flush() ([][]byte, error) {
	if b == nil {
		return nil, nil
	}
	if b.blocked() {
		b.messages = nil
		b.size = 0
		return nil, promptfilter.ErrOutputBlocked
	}
	out := b.messages
	b.messages = nil
	b.size = 0
	return out, nil
}

func (b *wsPromptOutputBuffer) blocked() bool {
	if b == nil {
		return false
	}
	var content bytes.Buffer
	for _, message := range b.messages {
		if text := promptfilter.ExtractOutputText(message); text != "" {
			content.WriteString(text)
		}
	}
	v := promptfilter.InspectText(content.String(), b.cfg)
	if b.cfg.Advanced.Output.StrictOnly {
		return v.TerminalStrictHit
	}
	return v.Action == promptfilter.ActionBlock
}
