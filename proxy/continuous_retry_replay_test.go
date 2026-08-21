package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestContinuousRetryReplayKeepsAttemptPrivateUntilCommit(t *testing.T) {
	replay := newContinuousRetryReplayWithLimit(1024)
	t.Cleanup(func() { _ = replay.Close() })

	if _, err := replay.Write([]byte("first")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var downstream bytes.Buffer
	if downstream.Len() != 0 {
		t.Fatalf("downstream changed before commit: %q", downstream.String())
	}
	if err := replay.CommitTo(&downstream, nil); err != nil {
		t.Fatalf("CommitTo: %v", err)
	}
	if got := downstream.String(); got != "first" {
		t.Fatalf("committed body = %q, want first", got)
	}
}

func TestContinuousRetryReplaySpillsAndRemovesTemporaryFile(t *testing.T) {
	replay := newContinuousRetryReplayWithLimit(3)
	if _, err := replay.Write([]byte("abcdef")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if replay.file == nil {
		t.Fatal("expected replay to spill to a temporary file")
	}
	fileName := replay.file.Name()
	if _, err := os.Stat(fileName); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary file directory entry still exists after spill")
	}
	var downstream bytes.Buffer
	if err := replay.CommitTo(&downstream, nil); err != nil {
		t.Fatalf("CommitTo: %v", err)
	}
	if got := downstream.String(); got != "abcdef" {
		t.Fatalf("committed body = %q, want abcdef", got)
	}
	if err := replay.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := replay.Write([]byte("late")); !errors.Is(err, errContinuousRetryReplayClosed) {
		t.Fatalf("write after close error = %v", err)
	}
}

func TestContinuousRetryReplayRejectsAttemptAboveTotalLimit(t *testing.T) {
	replay := newContinuousRetryReplayWithLimits(4, 6)
	t.Cleanup(func() { _ = replay.Close() })

	if _, err := replay.Write([]byte("four")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if n, err := replay.Write([]byte("too")); n != 0 || !errors.Is(err, errContinuousRetryReplayLimitExceeded) {
		t.Fatalf("oversized Write = (%d, %v), want (0, size limit error)", n, err)
	}
	if replay.size != 4 {
		t.Fatalf("replay size = %d, want 4", replay.size)
	}

	var downstream bytes.Buffer
	if err := replay.CommitTo(&downstream, nil); err != nil {
		t.Fatalf("CommitTo: %v", err)
	}
	if got := downstream.String(); got != "four" {
		t.Fatalf("committed body = %q, want four", got)
	}
}

func TestContinuousRetryReplayUses64MiBProductionLimit(t *testing.T) {
	replay := newContinuousRetryReplay()
	t.Cleanup(func() { _ = replay.Close() })

	if replay.totalLimit != 64<<20 {
		t.Fatalf("production total limit = %d, want %d", replay.totalLimit, int64(64<<20))
	}
}

func TestContinuousRetryReplayMapsFileErrorsWithoutPath(t *testing.T) {
	replay := newContinuousRetryReplayWithLimits(0, 1024)
	if _, err := replay.Write([]byte("first")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	fileName := replay.file.Name()
	if err := replay.file.Close(); err != nil {
		t.Fatal("failed to close replay file during test setup")
	}
	if _, err := replay.reader(); !errors.Is(err, errContinuousRetryReplayStorage) {
		t.Fatal("file seek error was not mapped to the stable storage error")
	}

	_, err := replay.Write([]byte("second"))
	if !errors.Is(err, errContinuousRetryReplayStorage) {
		t.Fatal("file write error was not mapped to the stable storage error")
	}
	if strings.Contains(err.Error(), fileName) {
		t.Fatal("mapped storage error contains the temporary file name")
	}
	if err := replay.Close(); !errors.Is(err, errContinuousRetryReplayStorage) {
		t.Fatal("file close error was not mapped to the stable storage error")
	}
}

func TestContinuousRetryWSReplayPreservesMessageBoundaries(t *testing.T) {
	replay := &continuousRetryWSReplay{replay: newContinuousRetryReplayWithLimit(5)}
	t.Cleanup(func() { _ = replay.Close() })
	for _, message := range [][]byte{[]byte("one"), []byte("two-two"), []byte("three")} {
		if err := replay.WriteMessage(message); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}
	var got []string
	if err := replay.Commit(func(payload []byte) error {
		got = append(got, string(payload))
		return nil
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	want := []string{"one", "two-two", "three"}
	if len(got) != len(want) {
		t.Fatalf("message count = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("message[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestContinuousRetryWSReplayForEachMessageIsReadOnly(t *testing.T) {
	replay := &continuousRetryWSReplay{replay: newContinuousRetryReplayWithLimits(5, 1024)}
	t.Cleanup(func() { _ = replay.Close() })
	for _, message := range [][]byte{[]byte("one"), []byte("two")} {
		if err := replay.WriteMessage(message); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	var firstPass []string
	if err := replay.ForEachMessage(func(payload []byte) error {
		firstPass = append(firstPass, string(payload))
		return nil
	}); err != nil {
		t.Fatalf("ForEachMessage: %v", err)
	}
	var secondPass []string
	if err := replay.ForEachMessage(func(payload []byte) error {
		secondPass = append(secondPass, string(payload))
		return nil
	}); err != nil {
		t.Fatalf("second ForEachMessage: %v", err)
	}
	if strings.Join(firstPass, ",") != "one,two" || strings.Join(secondPass, ",") != "one,two" {
		t.Fatalf("message passes = %v and %v, want [one two] twice", firstPass, secondPass)
	}
}

func TestContinuousRetryWSReplayRejectsInvalidLengthBeforeAllocation(t *testing.T) {
	buffer := newContinuousRetryReplayWithLimits(16, 16)
	t.Cleanup(func() { _ = buffer.Close() })
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], ^uint32(0))
	if _, err := buffer.Write(header[:]); err != nil {
		t.Fatalf("Write header: %v", err)
	}
	replay := &continuousRetryWSReplay{replay: buffer, count: 1}
	called := false
	err := replay.ForEachMessage(func([]byte) error {
		called = true
		return nil
	})
	if !errors.Is(err, errContinuousRetryWSReplayInvalid) {
		t.Fatalf("ForEachMessage error = %v, want invalid replay", err)
	}
	if called {
		t.Fatal("visitor called for invalid oversized message")
	}
}

func TestContinuousRetryWSReplaySizeCheckDoesNotWritePartialRecord(t *testing.T) {
	buffer := newContinuousRetryReplayWithLimits(8, 8)
	replay := &continuousRetryWSReplay{replay: buffer}
	t.Cleanup(func() { _ = replay.Close() })

	if err := replay.WriteMessage([]byte("12345")); !errors.Is(err, errContinuousRetryReplayLimitExceeded) {
		t.Fatalf("WriteMessage error = %v, want size limit error", err)
	}
	if buffer.size != 0 || replay.count != 0 {
		t.Fatalf("failed message changed replay: size=%d count=%d", buffer.size, replay.count)
	}
}

func TestContinuousRetryReplayCommitAfterCloseFails(t *testing.T) {
	attempt := newContinuousRetryStreamAttempt(true, &bytes.Buffer{}, nil)
	if err := attempt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := attempt.Commit(); !errors.Is(err, errContinuousRetryReplayClosed) {
		t.Fatalf("Commit after Close = %v, want closed error", err)
	}
}

func TestContinuousRetryWSReplayCommitAfterCloseFails(t *testing.T) {
	replay := newContinuousRetryWSReplay()
	if err := replay.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := replay.Commit(func([]byte) error { return nil }); !errors.Is(err, errContinuousRetryReplayClosed) {
		t.Fatalf("Commit after Close = %v, want closed error", err)
	}
}

func TestReplayResponsesWSSuccessWithoutOutputFilter(t *testing.T) {
	replay := newContinuousRetryWSReplay()
	t.Cleanup(func() { _ = replay.Close() })
	if err := replay.WriteMessage([]byte("response.completed")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var got [][]byte
	wroteAny, err := replayResponsesWSSuccess(replay, nil, func(payload []byte) error {
		got = append(got, append([]byte(nil), payload...))
		return nil
	})
	if err != nil {
		t.Fatalf("replayResponsesWSSuccess: %v", err)
	}
	if !wroteAny || len(got) != 1 || string(got[0]) != "response.completed" {
		t.Fatalf("replayed output = wrote:%v messages:%q", wroteAny, got)
	}
}
