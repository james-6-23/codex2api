package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
)

const (
	continuousRetryReplayMemoryLimit int64 = 8 << 20
	continuousRetryReplayTotalLimit  int64 = 64 << 20
)

var (
	errContinuousRetryReplayClosed        = errors.New("continuous retry replay buffer is closed")
	errContinuousRetryReplayLimitExceeded = errors.New("continuous retry replay size limit exceeded")
	errContinuousRetryReplayStorage       = errors.New("continuous retry replay storage failure")
	errContinuousRetryWSReplayInvalid     = errors.New("continuous retry websocket replay is invalid")
	errContinuousRetryWSMessageTooLarge   = errors.New("continuous retry websocket message is too large")
)

// continuousRetryReplay keeps one upstream attempt private until the protocol
// reports a successful terminal event. Small responses stay in memory; larger
// responses spill to a mode-0600 temporary file so catch-all mode cannot turn a
// long generation into unbounded heap growth.
type continuousRetryReplay struct {
	memory      bytes.Buffer
	file        *os.File
	size        int64
	memoryLimit int64
	totalLimit  int64
	closed      bool
	// beforeReadForTest forces a file-backed commit failure after private writes;
	// production constructors leave it nil.
	// beforeReadForTest 在私有写入完成后注入文件回放提交失败；生产构造器保持 nil。
	beforeReadForTest func(*continuousRetryReplay)
}

func newContinuousRetryReplay() *continuousRetryReplay {
	return newContinuousRetryReplayWithLimits(continuousRetryReplayMemoryLimit, continuousRetryReplayTotalLimit)
}

func newContinuousRetryReplayWithLimit(memoryLimit int64) *continuousRetryReplay {
	return newContinuousRetryReplayWithLimits(memoryLimit, continuousRetryReplayTotalLimit)
}

func newContinuousRetryReplayWithLimits(memoryLimit, totalLimit int64) *continuousRetryReplay {
	if totalLimit < 0 {
		totalLimit = 0
	}
	if memoryLimit < 0 {
		memoryLimit = 0
	}
	if memoryLimit > totalLimit {
		memoryLimit = totalLimit
	}
	return &continuousRetryReplay{memoryLimit: memoryLimit, totalLimit: totalLimit}
}

func (r *continuousRetryReplay) checkWriteSize(size int64) error {
	if r == nil || r.closed {
		return errContinuousRetryReplayClosed
	}
	if size < 0 || size > r.totalLimit-r.size {
		return errContinuousRetryReplayLimitExceeded
	}
	return nil
}

func (r *continuousRetryReplay) Write(data []byte) (int, error) {
	if err := r.checkWriteSize(int64(len(data))); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	if r.file == nil && r.size+int64(len(data)) <= r.memoryLimit {
		n, err := r.memory.Write(data)
		r.size += int64(n)
		return n, err
	}
	if r.file == nil {
		file, err := os.CreateTemp("", "codex2api-continuous-retry-*")
		if err != nil {
			return 0, errContinuousRetryReplayStorage
		}
		// The open descriptor is sufficient for replay. Removing the directory
		// entry immediately prevents abandoned attempts from leaving files behind.
		if err := os.Remove(file.Name()); err != nil {
			_ = file.Close()
			return 0, errContinuousRetryReplayStorage
		}
		r.file = file
		if r.memory.Len() > 0 {
			if err := writeAll(r.file, r.memory.Bytes()); err != nil {
				_ = r.Close()
				return 0, errContinuousRetryReplayStorage
			}
			r.memory.Reset()
		}
	}
	n, err := r.file.Write(data)
	r.size += int64(n)
	if err != nil || n != len(data) {
		return n, errContinuousRetryReplayStorage
	}
	return n, err
}

// Flush implements http.Flusher for streamFlushWriter. The attempt remains
// private, so a transport flush intentionally does nothing until CommitTo.
func (r *continuousRetryReplay) Flush() {}

func (r *continuousRetryReplay) reader() (io.Reader, error) {
	if r == nil || r.closed {
		return nil, errContinuousRetryReplayClosed
	}
	if hook := r.beforeReadForTest; hook != nil {
		r.beforeReadForTest = nil
		hook(r)
	}
	if r.file == nil {
		return bytes.NewReader(r.memory.Bytes()), nil
	}
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return nil, errContinuousRetryReplayStorage
	}
	return io.LimitReader(continuousRetryStorageReader{reader: r.file}, r.size), nil
}

func (r *continuousRetryReplay) CommitTo(writer io.Writer, flusher http.Flusher) error {
	if writer == nil {
		return errors.New("nil continuous retry downstream writer")
	}
	reader, err := r.reader()
	if err != nil {
		return err
	}
	written, err := io.Copy(writer, reader)
	if err != nil {
		return err
	}
	if written != r.size {
		return errContinuousRetryReplayStorage
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func (r *continuousRetryReplay) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	r.memory.Reset()
	var closeErr error
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			closeErr = errContinuousRetryReplayStorage
		}
		r.file = nil
	}
	return closeErr
}

type continuousRetryStorageReader struct {
	reader io.Reader
}

func (r continuousRetryStorageReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, errContinuousRetryReplayStorage
	}
	return n, err
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// continuousRetryStreamAttempt exposes the replay as an io.Writer/http.Flusher
// while retaining the real downstream for a single success-only commit.
type continuousRetryStreamAttempt struct {
	replay     *continuousRetryReplay
	downstream io.Writer
	flusher    http.Flusher
	closed     bool
}

func newContinuousRetryStreamAttempt(enabled bool, downstream io.Writer, flusher http.Flusher) *continuousRetryStreamAttempt {
	if !enabled {
		return nil
	}
	return newContinuousRetryStreamAttemptWithReplay(true, downstream, flusher, newContinuousRetryReplay())
}

func newContinuousRetryStreamAttemptWithReplay(enabled bool, downstream io.Writer, flusher http.Flusher, replay *continuousRetryReplay) *continuousRetryStreamAttempt {
	if !enabled {
		return nil
	}
	if replay == nil {
		replay = newContinuousRetryReplay()
	}
	return &continuousRetryStreamAttempt{
		replay:     replay,
		downstream: downstream,
		flusher:    flusher,
	}
}

func (h *Handler) newContinuousRetryReplay() *continuousRetryReplay {
	if h != nil && h.continuousRetryReplayFactory != nil {
		if replay := h.continuousRetryReplayFactory(); replay != nil {
			return replay
		}
	}
	return newContinuousRetryReplay()
}

func (h *Handler) newContinuousRetryStreamAttempt(enabled bool, downstream io.Writer, flusher http.Flusher) *continuousRetryStreamAttempt {
	if !enabled {
		return nil
	}
	return newContinuousRetryStreamAttemptWithReplay(true, downstream, flusher, h.newContinuousRetryReplay())
}

func (a *continuousRetryStreamAttempt) writerOr(downstream io.Writer) io.Writer {
	if a == nil || a.replay == nil {
		return downstream
	}
	return a.replay
}

func (a *continuousRetryStreamAttempt) flusherOr(flusher http.Flusher) http.Flusher {
	if a == nil || a.replay == nil {
		return flusher
	}
	return a.replay
}

func (a *continuousRetryStreamAttempt) downstreamWrote(attemptWrote bool) bool {
	return attemptWrote && a == nil
}

func (a *continuousRetryStreamAttempt) Commit() error {
	if a == nil {
		return nil
	}
	if a.closed || a.replay == nil {
		return errContinuousRetryReplayClosed
	}
	return a.replay.CommitTo(a.downstream, a.flusher)
}

func (a *continuousRetryStreamAttempt) Close() error {
	if a == nil {
		return nil
	}
	if a.closed || a.replay == nil {
		a.closed = true
		return nil
	}
	a.closed = true
	err := a.replay.Close()
	a.replay = nil
	return err
}

type continuousRetryWSReplay struct {
	replay *continuousRetryReplay
	count  int
	closed bool
}

func newContinuousRetryWSReplay() *continuousRetryWSReplay {
	return &continuousRetryWSReplay{replay: newContinuousRetryReplay()}
}

func (h *Handler) newContinuousRetryWSReplay() *continuousRetryWSReplay {
	return &continuousRetryWSReplay{replay: h.newContinuousRetryReplay()}
}

func (r *continuousRetryWSReplay) WriteMessage(payload []byte) error {
	if r == nil || r.replay == nil {
		return errContinuousRetryReplayClosed
	}
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return errContinuousRetryWSMessageTooLarge
	}
	if err := r.replay.checkWriteSize(int64(len(payload)) + 4); err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(r.replay, header[:]); err != nil {
		return err
	}
	if err := writeAll(r.replay, payload); err != nil {
		return err
	}
	r.count++
	return nil
}

// ForEachMessage visits each buffered message without changing or closing the
// replay, so a successful attempt can be inspected before it is committed.
func (r *continuousRetryWSReplay) ForEachMessage(visit func([]byte) error) error {
	if r == nil {
		return nil
	}
	if r.closed || r.replay == nil {
		return errContinuousRetryReplayClosed
	}
	if visit == nil {
		return errors.New("nil continuous retry websocket writer")
	}
	reader, err := r.replay.reader()
	if err != nil {
		return err
	}
	remaining := r.replay.size
	for index := 0; index < r.count; index++ {
		if remaining < 4 {
			return errContinuousRetryWSReplayInvalid
		}
		var header [4]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if errors.Is(err, errContinuousRetryReplayStorage) {
				return err
			}
			return errContinuousRetryWSReplayInvalid
		}
		remaining -= 4
		payloadSize := int64(binary.BigEndian.Uint32(header[:]))
		if payloadSize > remaining || payloadSize > continuousRetryReplayTotalLimit {
			return errContinuousRetryWSReplayInvalid
		}
		payload := make([]byte, int(payloadSize))
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, errContinuousRetryReplayStorage) {
				return err
			}
			return errContinuousRetryWSReplayInvalid
		}
		remaining -= payloadSize
		if err := visit(payload); err != nil {
			return err
		}
	}
	if remaining != 0 {
		return errContinuousRetryWSReplayInvalid
	}
	return nil
}

func (r *continuousRetryWSReplay) Commit(writeMessage func([]byte) error) error {
	return r.ForEachMessage(writeMessage)
}

func (r *continuousRetryWSReplay) Close() error {
	if r == nil {
		return nil
	}
	if r.closed || r.replay == nil {
		r.closed = true
		return nil
	}
	r.closed = true
	err := r.replay.Close()
	r.replay = nil
	return err
}
