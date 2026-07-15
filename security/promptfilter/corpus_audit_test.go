package promptfilter

import (
	"archive/zip"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// PROMPT_FILTER_CORPUS enables an opt-in local corpus audit without embedding
// private or offensive corpus text in the repository or test output.
func TestOptionalLocalCorpusAudit(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("PROMPT_FILTER_CORPUS"))
	if root == "" {
		t.Skip("PROMPT_FILTER_CORPUS is not set")
	}
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Mode = ModeBlock
	cfg.StrictTerminalEnabled = true
	cfg.Advanced.Normalization = NormalizationConfig{Enabled: true, DecodeURL: true, DecodeHTML: true, DecodeBase64: true, MaxDecodeRuns: 1}
	extensions := map[string]bool{".md": true, ".txt": true, ".py": true, ".json": true, ".yaml": true, ".yml": true, ".sh": true, ".ps1": true, ".js": true, ".bat": true, ".docx": true}
	files, blocked, terminal, errors := 0, 0, 0, 0
	injectionCandidates, injectionMisses := 0, 0
	injectionMarker := regexp.MustCompile(`(?i)jailbreak|unrestricted|ignore.{0,30}(previous|system|instruction)|system.?prompt|developer.?mode|破限|破甲|越狱|忽略.{0,20}(指令|规则|限制)|无视.{0,20}(规则|限制)|解除.{0,20}(安全|限制)`)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			errors++
			return nil
		}
		if entry.IsDir() || !extensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, readErr := readCorpusText(path, 16<<20)
		if readErr != nil {
			errors++
			return nil
		}
		files++
		v := InspectText(string(data), cfg)
		candidate := injectionMarker.Match(data)
		if candidate {
			injectionCandidates++
		}
		if v.Action == ActionBlock {
			blocked++
		} else if candidate {
			injectionMisses++
			if os.Getenv("PROMPT_FILTER_CORPUS_REPORT_MISSES") == "1" {
				if rel, err := filepath.Rel(root, path); err == nil {
					t.Logf("injection candidate allowed: %s", rel)
				}
			}
		}
		if v.TerminalStrictHit {
			terminal++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus audit: files=%d blocked=%d terminal=%d injection_candidates=%d injection_misses=%d read_errors=%d", files, blocked, terminal, injectionCandidates, injectionMisses, errors)
}

func TestReadCorpusTextRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCorpusText(path, 5); err == nil {
		t.Fatal("oversized corpus file was accepted")
	}
}

func readCorpusText(path string, maxCorpusBytes int64) ([]byte, error) {
	if maxCorpusBytes <= 0 {
		return nil, io.ErrShortBuffer
	}
	if !strings.EqualFold(filepath.Ext(path), ".docx") {
		stream, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		data, err := io.ReadAll(io.LimitReader(stream, maxCorpusBytes+1))
		if err == nil && int64(len(data)) > maxCorpusBytes {
			return nil, io.ErrShortBuffer
		}
		return data, err
	}
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		if file.UncompressedSize64 > uint64(maxCorpusBytes) {
			return nil, io.ErrShortBuffer
		}
		stream, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(stream, maxCorpusBytes+1))
		_ = stream.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxCorpusBytes {
			return nil, io.ErrShortBuffer
		}
		text := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(string(data), " ")
		return []byte(html.UnescapeString(text)), nil
	}
	return nil, os.ErrNotExist
}
