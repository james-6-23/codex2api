//go:build unix

package admin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRestartProcessExecsProvidedPathAndKeepsEnvironment(t *testing.T) {
	if os.Getenv("CODEX2API_RESTART_EXEC_HELPER") == "1" {
		targetPath := os.Getenv("CODEX2API_RESTART_EXEC_TARGET")
		if err := defaultRestartProcess(targetPath); err != nil {
			t.Fatalf("defaultRestartProcess(%q) error: %v", targetPath, err)
		}
		t.Fatal("defaultRestartProcess returned after successful exec")
	}

	tempDir := t.TempDir()
	targetPath := filepath.Join(tempDir, "codex2api-new")
	targetScript := "#!/bin/sh\nprintf 'target=new env=%s\\n' \"$CODEX2API_RESTART_EXEC_VALUE\"\n"
	if err := os.WriteFile(targetPath, []byte(targetScript), 0755); err != nil {
		t.Fatalf("write target script: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestDefaultRestartProcessExecsProvidedPathAndKeepsEnvironment")
	cmd.Env = append(os.Environ(),
		"CODEX2API_RESTART_EXEC_HELPER=1",
		"CODEX2API_RESTART_EXEC_TARGET="+targetPath,
		"CODEX2API_RESTART_EXEC_VALUE=kept",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restart helper failed: %v\n%s", err, string(output))
	}
	if got, want := strings.TrimSpace(string(output)), "target=new env=kept"; got != want {
		t.Fatalf("restart helper output = %q, want %q", got, want)
	}
}
