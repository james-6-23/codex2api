package proxy

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

type modelsListErrorReader struct{}

func (modelsListErrorReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestReadModelsListBodyAcceptsExactLimitAndRejectsLimitPlusOne(t *testing.T) {
	body, err := ReadModelsListBody(strings.NewReader("1234"), 4)
	if err != nil {
		t.Fatalf("exact limit returned error: %v", err)
	}
	if string(body) != "1234" {
		t.Fatalf("body = %q, want 1234", body)
	}

	_, err = ReadModelsListBody(strings.NewReader("12345"), 4)
	if !errors.Is(err, ErrModelsListResponseTooLarge) {
		t.Fatalf("limit+1 error = %v, want ErrModelsListResponseTooLarge", err)
	}
}

func TestReadModelsListBodyPropagatesReadError(t *testing.T) {
	_, err := ReadModelsListBody(modelsListErrorReader{}, 4)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestModelsListReadLimitRuntimeSettings(t *testing.T) {
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	if got := DefaultRuntimeSettings().ModelsListReadMaxBytes; got != database.DefaultModelsListReadMaxBytes {
		t.Fatalf("default = %d, want %d", got, database.DefaultModelsListReadMaxBytes)
	}
	next := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		ModelsListReadMaxBytes: 32 << 20,
	})
	if got := next.ModelsListReadMaxBytes; got != 32<<20 {
		t.Fatalf("system setting = %d, want %d", got, 32<<20)
	}

	next.ModelsListReadMaxBytes = 0
	next = NormalizeRuntimeSettings(next)
	if got := next.ModelsListReadMaxBytes; got != database.DefaultModelsListReadMaxBytes {
		t.Fatalf("normalized zero = %d, want %d", got, database.DefaultModelsListReadMaxBytes)
	}
}
