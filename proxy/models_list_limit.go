package proxy

import (
	"errors"
	"fmt"
	"io"

	"github.com/codex2api/database"
)

var ErrModelsListResponseTooLarge = errors.New("models list response exceeds configured read limit")

// ReadModelsListBody reads one upstream model-list response and rejects
// limit+1 bytes instead of silently accepting truncated JSON.
func ReadModelsListBody(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = database.DefaultModelsListReadMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: maximum %d bytes", ErrModelsListResponseTooLarge, limit)
	}
	return body, nil
}
