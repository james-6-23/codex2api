package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

// The child runs the actual first-start path, including the SystemSettings
// insert. Testing only runtime defaults would miss zero values explicitly
// written over SQLite's column defaults by the first-start initializer.
func TestMainFirstStartPersistsWebSocketRetentionDefaults(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	root := t.TempDir()
	dbPath := filepath.Join(root, "startup.db")
	command := exec.Command(os.Args[0], "-test.run=^TestMainWebSocketDefaultsSubprocess$")
	command.Dir = root // Do not load the checkout's optional .env.
	command.Env = append(os.Environ(),
		"CODEX2API_TEST_WS_DEFAULTS_SUBPROCESS=1",
		"CODEX_BIND=127.0.0.1", "CODEX_PORT="+strconv.Itoa(port),
		"DATABASE_DRIVER=sqlite", "DATABASE_PATH="+dbPath, "CACHE_DRIVER=memory",
		"CODEX_MIGRATE_ONLY=false", "CODEX_ALLOW_ANONYMOUS=false",
		"CODEX_DISABLE_CLI_VERSION_SYNC=true", "CLAUDE_DISABLE_CLI_VERSION_SYNC=true", "CODEX_TELEMETRY_ENABLED=false",
		"ADMIN_SECRET=synthetic-startup-defaults-test", "LOG_DISABLED=true",
		"IMAGE_ASSET_DIR="+filepath.Join(root, "images"), "BACKGROUND_ASSET_DIR="+filepath.Join(root, "backgrounds"),
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		shutdownSignal := os.Interrupt
		if runtime.GOOS == "windows" {
			// Windows cannot send os.Interrupt; this test checks persisted startup settings.
			shutdownSignal = os.Kill
		}
		if err := command.Process.Signal(shutdownSignal); err != nil {
			t.Errorf("stop isolated first-start subprocess: %v", err)
		}
		select {
		case err := <-done:
			if err != nil && shutdownSignal != os.Kill {
				t.Errorf("isolated first-start subprocess failed during shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-done
			t.Error("isolated first-start subprocess did not stop gracefully")
		}
		stopped = true
	}
	defer stop()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(20 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("isolated first-start subprocess exited before health readiness: %v", err)
		default:
		}
		response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated first-start service did not become healthy")
	}
	stop()
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil || settings == nil {
		t.Fatalf("first-start settings unavailable: %v", err)
	}
	defaults := proxy.DefaultRuntimeSettings()
	if settings.CodexWSStatelessSlots != defaults.CodexWSStatelessSlots || settings.CodexWSDownstreamKeepaliveSlots != database.DefaultCodexWSDownstreamKeepaliveSlots {
		t.Fatalf("first-start WS retention = blank:%d/chat:%d, want blank:%d/chat:%d", settings.CodexWSStatelessSlots, settings.CodexWSDownstreamKeepaliveSlots, defaults.CodexWSStatelessSlots, database.DefaultCodexWSDownstreamKeepaliveSlots)
	}
	// Zero is an explicit opt-out, not a missing default. A subsequent database
	// round trip and runtime application must keep it unchanged.
	settings.CodexWSStatelessSlots = 0
	settings.CodexWSDownstreamKeepaliveSlots = 0
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(oldRuntime) })
	runtime := proxy.ApplyRuntimeSettingsFromSystem(stored)
	if stored.CodexWSStatelessSlots != 0 || stored.CodexWSDownstreamKeepaliveSlots != 0 || runtime.CodexWSStatelessSlots != 0 || runtime.CodexWSDownstreamKeepaliveSlots != 0 {
		t.Fatal("explicit zero WS retention was replaced with defaults")
	}
}

func TestMainWebSocketDefaultsSubprocess(t *testing.T) {
	if os.Getenv("CODEX2API_TEST_WS_DEFAULTS_SUBPROCESS") != "1" {
		return
	}
	main()
}
