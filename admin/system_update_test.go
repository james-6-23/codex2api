package admin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeSystemReleaseClient struct {
	release   *systemGitHubRelease
	files     map[string][]byte
	fetchErr  error
	downloads int
}

func (c *fakeSystemReleaseClient) FetchLatestRelease(context.Context) (*systemGitHubRelease, error) {
	if c.fetchErr != nil {
		return nil, c.fetchErr
	}
	return c.release, nil
}

func (c *fakeSystemReleaseClient) DownloadFile(_ context.Context, rawURL, dest string, _ int64) error {
	data, ok := c.files[rawURL]
	if !ok {
		return os.ErrNotExist
	}
	c.downloads++
	return os.WriteFile(dest, data, 0644)
}

func (c *fakeSystemReleaseClient) FetchText(_ context.Context, rawURL string, _ int64) ([]byte, error) {
	data, ok := c.files[rawURL]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func TestCompareSystemVersions(t *testing.T) {
	tests := []struct {
		a    string
		b    string
		want int
	}{
		{a: "v2.4.3", b: "2.4.4", want: -1},
		{a: "2.10.0", b: "2.9.9", want: 1},
		{a: "2.4.3", b: "v2.4.3", want: 0},
		{a: "2.4", b: "2.4.1", want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := compareSystemVersions(tt.a, tt.b)
			if got < 0 {
				got = -1
			} else if got > 0 {
				got = 1
			}
			if got != tt.want {
				t.Fatalf("compareSystemVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestValidateSystemUpdateURL(t *testing.T) {
	allowed := []string{
		"https://github.com/james-6-23/codex2api/releases/download/v1/codex2api.tar.gz",
		"https://release-assets.githubusercontent.com/github-production-release-asset/file",
		"https://objects.githubusercontent.com/github-production-release-asset/file",
	}
	for _, rawURL := range allowed {
		if err := validateSystemUpdateURL(rawURL); err != nil {
			t.Fatalf("validateSystemUpdateURL(%q) unexpected error: %v", rawURL, err)
		}
	}

	blocked := []string{
		"http://github.com/james-6-23/codex2api/releases/download/v1/codex2api.tar.gz",
		"https://example.com/codex2api.tar.gz",
	}
	for _, rawURL := range blocked {
		if err := validateSystemUpdateURL(rawURL); err == nil {
			t.Fatalf("validateSystemUpdateURL(%q) expected error", rawURL)
		}
	}
}

func TestSystemUpdaterCheckFindsMatchingAsset(t *testing.T) {
	client := &fakeSystemReleaseClient{release: &systemGitHubRelease{
		TagName: "v2.4.4",
		HTMLURL: "https://github.com/james-6-23/codex2api/releases/tag/v2.4.4",
		Assets: []systemGitHubAsset{
			{Name: "codex2api_2.4.4_linux_arm64.tar.gz", BrowserDownloadURL: "https://github.com/arm64"},
			{Name: "codex2api_2.4.4_linux_amd64.tar.gz", BrowserDownloadURL: "https://github.com/amd64"},
			{Name: "SHA256SUMS.txt", BrowserDownloadURL: "https://github.com/sums"},
		},
	}}
	updater := &systemUpdater{
		currentVersion: "v2.4.3",
		client:         client,
		goos:           "linux",
		goarch:         "amd64",
	}

	info, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if !info.HasUpdate {
		t.Fatal("HasUpdate = false, want true")
	}
	if !info.Supported {
		t.Fatalf("Supported = false: %s", info.UnsupportedReason)
	}
	if info.AssetName != "codex2api_2.4.4_linux_amd64.tar.gz" {
		t.Fatalf("AssetName = %q", info.AssetName)
	}
}

func TestSystemUpdaterRejectsDevBuild(t *testing.T) {
	client := &fakeSystemReleaseClient{release: &systemGitHubRelease{
		TagName: "v2.4.4",
		Assets:  []systemGitHubAsset{{Name: "codex2api_2.4.4_linux_amd64.tar.gz"}},
	}}
	updater := &systemUpdater{
		currentVersion: "dev",
		client:         client,
		goos:           "linux",
		goarch:         "amd64",
	}

	info, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if info.Supported {
		t.Fatal("Supported = true, want false")
	}
}

func TestSystemUpdaterRejectsNonSemverBuild(t *testing.T) {
	client := &fakeSystemReleaseClient{release: &systemGitHubRelease{
		TagName: "v2.4.4",
		Assets:  []systemGitHubAsset{{Name: "codex2api_2.4.4_linux_amd64.tar.gz"}},
	}}
	updater := &systemUpdater{
		currentVersion: "main",
		client:         client,
		goos:           "linux",
		goarch:         "amd64",
	}

	info, err := updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if info.Supported {
		t.Fatal("Supported = true, want false")
	}
}

func TestSystemUpdaterPerformUpdateReplacesBinaryAndKeepsBackup(t *testing.T) {
	tempDir := t.TempDir()
	currentPath := filepath.Join(tempDir, "codex2api")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("write current binary: %v", err)
	}

	archive := buildSystemUpdateTarball(t, "codex2api", []byte("new-binary"))
	archiveHash := sha256.Sum256(archive)
	archiveURL := "https://github.com/james-6-23/codex2api/releases/download/v2.4.4/codex2api_2.4.4_linux_amd64.tar.gz"
	restarted := make(chan struct{}, 1)
	client := &fakeSystemReleaseClient{
		release: &systemGitHubRelease{
			TagName: "v2.4.4",
			HTMLURL: "https://github.com/james-6-23/codex2api/releases/tag/v2.4.4",
			Assets: []systemGitHubAsset{{
				Name:               "codex2api_2.4.4_linux_amd64.tar.gz",
				BrowserDownloadURL: archiveURL,
				Digest:             "sha256:" + hex.EncodeToString(archiveHash[:]),
			}},
		},
		files: map[string][]byte{archiveURL: archive},
	}
	updater := &systemUpdater{
		currentVersion: "v2.4.3",
		client:         client,
		goos:           "linux",
		goarch:         "amd64",
		executablePath: func() (string, error) { return currentPath, nil },
		restartProcess: func() error {
			restarted <- struct{}{}
			return nil
		},
		restartDelay: 0,
	}

	result, err := updater.PerformUpdate(context.Background())
	if err != nil {
		t.Fatalf("PerformUpdate() error: %v", err)
	}
	if !result.Restarting || !result.NeedRestart {
		t.Fatalf("restart flags = restarting:%v need:%v, want true/true", result.Restarting, result.NeedRestart)
	}
	if got := string(mustReadFile(t, currentPath)); got != "new-binary" {
		t.Fatalf("current binary = %q, want new-binary", got)
	}
	if got := string(mustReadFile(t, currentPath+".backup")); got != "old-binary" {
		t.Fatalf("backup binary = %q, want old-binary", got)
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not scheduled")
	}
}

func TestSystemUpdaterPerformUpdateRejectsChecksumMismatch(t *testing.T) {
	tempDir := t.TempDir()
	currentPath := filepath.Join(tempDir, "codex2api")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("write current binary: %v", err)
	}

	archive := buildSystemUpdateTarball(t, "codex2api", []byte("new-binary"))
	archiveURL := "https://github.com/james-6-23/codex2api/releases/download/v2.4.4/codex2api_2.4.4_linux_amd64.tar.gz"
	client := &fakeSystemReleaseClient{
		release: &systemGitHubRelease{
			TagName: "v2.4.4",
			Assets: []systemGitHubAsset{{
				Name:               "codex2api_2.4.4_linux_amd64.tar.gz",
				BrowserDownloadURL: archiveURL,
				Digest:             "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
		},
		files: map[string][]byte{archiveURL: archive},
	}
	updater := &systemUpdater{
		currentVersion: "v2.4.3",
		client:         client,
		goos:           "linux",
		goarch:         "amd64",
		executablePath: func() (string, error) { return currentPath, nil },
		restartProcess: func() error { t.Fatal("restart should not be called"); return nil },
	}

	_, err := updater.PerformUpdate(context.Background())
	if err == nil {
		t.Fatal("PerformUpdate() expected checksum error")
	}
	if got := string(mustReadFile(t, currentPath)); got != "old-binary" {
		t.Fatalf("current binary changed after failed update: %q", got)
	}
}

func TestSystemUpdaterBusy(t *testing.T) {
	updater := &systemUpdater{}
	updater.mu.Lock()
	defer updater.mu.Unlock()

	_, err := updater.PerformUpdate(context.Background())
	if !errors.Is(err, errSystemUpdateBusy) {
		t.Fatalf("PerformUpdate() err = %v, want errSystemUpdateBusy", err)
	}
}

func buildSystemUpdateTarball(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0755,
		Size: int64(len(data)),
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("write tar data: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
