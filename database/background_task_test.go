package database

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newBackgroundTaskTestDB() *DB {
	ctx, cancel := context.WithCancel(context.Background())
	return &DB{backgroundTaskCtx: ctx, backgroundTaskCancel: cancel}
}

func TestDrainBackgroundTasksWaitsForRunningTask(t *testing.T) {
	db := newBackgroundTaskTestDB()
	started := make(chan struct{})
	release := make(chan struct{})
	if !db.RunBackgroundTask(func(context.Context) {
		close(started)
		<-release
	}) {
		t.Fatal("RunBackgroundTask() = false, want true")
	}
	<-started

	drained := make(chan bool, 1)
	go func() {
		drained <- db.DrainBackgroundTasks(5 * time.Second)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		db.backgroundTaskMu.Lock()
		closing := db.backgroundTaskClosing
		db.backgroundTaskMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("DrainBackgroundTasks did not enter draining state")
		}
		runtime.Gosched()
	}
	select {
	case <-drained:
		t.Fatal("DrainBackgroundTasks returned before the running task completed")
	default:
	}
	close(release)
	select {
	case graceful := <-drained:
		if !graceful {
			t.Fatal("DrainBackgroundTasks() = false, want true for a task completed in grace period")
		}
	case <-time.After(time.Second):
		t.Fatal("DrainBackgroundTasks did not return after the task completed")
	}
}

func TestDrainBackgroundTasksCancelsAfterGracePeriodAndRejectsNewWork(t *testing.T) {
	db := newBackgroundTaskTestDB()
	started := make(chan struct{})
	canceled := make(chan struct{})
	if !db.RunBackgroundTask(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
	}) {
		t.Fatal("RunBackgroundTask() = false, want true")
	}
	<-started

	if graceful := db.DrainBackgroundTasks(10 * time.Millisecond); graceful {
		t.Fatal("DrainBackgroundTasks() = true, want false after grace-period cancellation")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("background task did not observe cancellation")
	}
	if db.RunBackgroundTask(func(context.Context) {}) {
		t.Fatal("RunBackgroundTask() = true after drain, want false")
	}
}

func TestDrainBackgroundTasksWaitsForCanceledTaskExit(t *testing.T) {
	db := newBackgroundTaskTestDB()
	started := make(chan struct{})
	exited := make(chan struct{})
	if !db.RunBackgroundTask(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(25 * time.Millisecond)
		close(exited)
	}) {
		t.Fatal("RunBackgroundTask() = false, want true")
	}
	<-started

	if graceful := db.DrainBackgroundTasks(time.Millisecond); graceful {
		t.Fatal("DrainBackgroundTasks() = true, want false after cancellation")
	}
	select {
	case <-exited:
	default:
		t.Fatal("DrainBackgroundTasks returned before the canceled task exited")
	}
}

func TestSQLiteCloseDrainsAsyncAccountEvents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "background-events.sqlite")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}

	ctx := context.Background()
	ids := make([]int64, 3)
	for i := range ids {
		ids[i], err = db.InsertAccount(ctx, fmt.Sprintf("account-%d", i), fmt.Sprintf("rt-%d", i), "")
		if err != nil {
			t.Fatalf("InsertAccount(%d): %v", i, err)
		}
	}
	db.InsertAccountEventAsync(ids[0], "added", "async-single")
	db.BatchInsertAccountEventsAsync(ids, "updated", "async-batch")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen New(sqlite): %v", err)
	}
	defer db.Close()

	var singleCount, batchCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_events WHERE source = 'async-single'`).Scan(&singleCount); err != nil {
		t.Fatalf("query single event count: %v", err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_events WHERE source = 'async-batch'`).Scan(&batchCount); err != nil {
		t.Fatalf("query batch event count: %v", err)
	}
	if singleCount != 1 || batchCount != len(ids) {
		t.Fatalf("event counts = single:%d batch:%d, want single:1 batch:%d", singleCount, batchCount, len(ids))
	}
}
