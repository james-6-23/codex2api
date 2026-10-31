package proxy

import (
	"bufio"
	"errors"
	"io"
	"math"
)

// SSE wire limits are resource bounds, not a declaration that larger events
// are invalid upstream. Base64 media and large tool results can exceed them.
const maxSSELineBytes = 8 << 20

const maxSSEEventBytes = 8 << 20

var errSSELineTooLong = errors.New("upstream SSE line exceeds limit")

var errSSEEventTooLarge = errors.New("upstream SSE event exceeds limit")

// readBoundedSSELine 读取一行，内存占用不超过 limit（不含 bufio 自身缓冲）。
func readBoundedSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, errSSELineTooLong
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// -------------------------------------------------------------------------
// 增补部分：导出常量别名与非流式有界读取器（严格保留既有小写符号兼容性）
// -------------------------------------------------------------------------

const (
	MaxSSELineBytes      = maxSSELineBytes
	MaxSSEEventBytes     = maxSSEEventBytes
	MaxResponseBodyBytes = 16 << 20 // 16 MiB (非流式响应体默认上限)
)

var (
	ErrSSELineTooLong       = errSSELineTooLong
	ErrSSEEventTooLarge     = errSSEEventTooLarge
	ErrResponseBodyTooLarge = errors.New("response_body_too_large")
)

// ReadBoundedResponseBody 有界读取非流式响应体。
func ReadBoundedResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = MaxResponseBodyBytes
	}
	// 预留 +1 探测超限时需要 limit+1；MaxInt64 时加一会溢出为负，导致
	// io.LimitReader 立即 EOF 并静默返回空数据，因此先做 clamp。
	if limit > math.MaxInt64-1 {
		limit = math.MaxInt64 - 1
	}
	lr := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseBodyTooLarge
	}
	return data, nil
}
