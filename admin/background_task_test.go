package admin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestStartDBBackgroundTaskWithParentCancelsAndDrains(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-background.sqlite"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	h := &Handler{db: db}
	parent, cancelParent := context.WithCancel(context.Background())
	started := make(chan struct{})
	stopped := make(chan struct{})
	if !h.startDBBackgroundTaskWithParent(parent, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}) {
		t.Fatal("startDBBackgroundTaskWithParent() = false, want true")
	}
	<-started
	cancelParent()

	if graceful := db.DrainBackgroundTasks(time.Second); !graceful {
		t.Fatal("DrainBackgroundTasks() = false, want true after parent cancellation")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("database drain returned before the parent-canceled task exited")
	}
}
