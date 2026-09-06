package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestEncryptedMemoryIsolationAndFreshContent(t *testing.T) {
	previous := rejectedEncryptedContent
	rejectedEncryptedContent = &encryptedContentMemory{now: time.Now}
	t.Cleanup(func() { rejectedEncryptedContent = previous })
	account := &auth.Account{DBID: 51, CredentialGeneration: 1}
	headers := http.Header{"Authorization": {"Bearer owner-a"}}
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"old"},{"type":"message","role":"user","content":"keep"}]}`)
	_, attempt := prepareEncryptedContentAttempt(context.Background(), account, body, "s1", headers)
	resp := &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_encrypted_content"}}`))}
	attempt.observeResponse(resp, body)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	next := []byte(`{"input":[{"type":"reasoning","encrypted_content":"old"},{"type":"reasoning","encrypted_content":"new"},{"type":"message","role":"user","content":"keep"},{"type":"function_call_output","call_id":"c","output":{"encrypted_content":"old"}}]}`)
	clean, _ := prepareEncryptedContentAttempt(context.Background(), account, next, "s1", headers)
	if len(gjson.GetBytes(clean, "input").Array()) != 3 || gjson.GetBytes(clean, "input.0.encrypted_content").String() != "new" || gjson.GetBytes(clean, "input.2.output.encrypted_content").String() != "old" {
		t.Fatalf("incorrect filtering: %s", clean)
	}
	if !strings.Contains(string(next), `"encrypted_content":"old"`) {
		t.Fatal("caller body was mutated")
	}
	for _, tc := range []struct {
		account        *auth.Account
		session, owner string
	}{
		{account, "s2", "owner-a"},
		{account, "s1", "owner-b"},
		{&auth.Account{DBID: 52, CredentialGeneration: 1}, "s1", "owner-a"},
		{&auth.Account{DBID: 51, CredentialGeneration: 2}, "s1", "owner-a"},
	} {
		got, _ := prepareEncryptedContentAttempt(context.Background(), tc.account, next, tc.session, http.Header{"Authorization": {"Bearer " + tc.owner}})
		if string(got) != string(next) {
			t.Errorf("history leaked into another namespace: %+v", tc)
		}
	}
}

func TestEncryptedErrorObserverStreamingAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		want         int
	}{
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_encrypted_content\"}}}\r\n\r\n", 1},
		{"multiline", "event: error\ndata: {\"code\":\ndata: \"invalid_encrypted_content\"}\n\n", 1},
		{"eof", "data: {\"type\":\"error\",\"code\":\"invalid_encrypted_content\"}\n", 1},
		{"content echo", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"invalid_encrypted_content\"}\n\n", 0},
		{"oversize", "data: " + strings.Repeat("x", encryptedErrorMaxBytes+1) + "\n\ndata: {\"type\":\"error\",\"code\":\"invalid_encrypted_content\"}\n\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			r := &encryptedErrorObserver{ReadCloser: io.NopCloser(&maxChunkReader{reader: strings.NewReader(tc.stream), size: 7}), stream: true, record: func(payload []byte) {
				if isRejectedEncryptedContentFailure(responseFailedErrorBody(payload)) {
					count++
				}
			}}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != tc.stream || count != tc.want {
				t.Fatalf("read error=%v records=%d want=%d", err, count, tc.want)
			}
			if len(r.pending) > encryptedErrorMaxBytes || len(r.event) > encryptedErrorMaxBytes {
				t.Fatal("observer exceeded cap")
			}
		})
	}
	for _, body := range []string{`{"input":{"code":"invalid_encrypted_content"},"error":{"message":"bad request"}}`, `{"error":{"code":"missing_required_parameter","message":"input.encrypted_content is missing"}}`} {
		if isRejectedEncryptedContentFailure([]byte(body)) {
			t.Fatalf("unrelated failure marked ciphertext: %s", body)
		}
	}
}

func TestEncryptedMemoryTTLAndCapacity(t *testing.T) {
	now := time.Now()
	m := &encryptedContentMemory{now: func() time.Time { return now }}
	key, digest := encryptedScopeKey{session: sha256.Sum256([]byte("session"))}, sha256.Sum256([]byte("ciphertext"))
	m.mark(key, []encryptedDigest{digest})
	now = now.Add(encryptedMemoryTTL)
	if len(m.get(key)) != 0 {
		t.Fatal("expired rejection retained")
	}
	for i := 0; i < encryptedMemoryMaxSessions+10; i++ {
		m.mark(encryptedScopeKey{session: sha256.Sum256([]byte(fmt.Sprint(i)))}, []encryptedDigest{digest})
	}
	if len(m.entries) != encryptedMemoryMaxSessions {
		t.Fatalf("session cap: %d", len(m.entries))
	}
	var digests []encryptedDigest
	for i := 0; i < encryptedMemoryMaxDigests+10; i++ {
		digests = append(digests, sha256.Sum256([]byte(fmt.Sprint(i))))
	}
	m.mark(key, digests)
	if len(m.get(key)) != encryptedMemoryMaxDigests {
		t.Fatal("digest cap was not enforced")
	}
}

func TestEncryptedMemoryConcurrentAccess(t *testing.T) {
	m := &encryptedContentMemory{now: time.Now}
	key := encryptedScopeKey{session: sha256.Sum256([]byte("s"))}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				m.mark(key, []encryptedDigest{sha256.Sum256([]byte(fmt.Sprint(i, j)))})
				_ = m.get(key)
			}
		}(i)
	}
	wg.Wait()
}

func TestEncryptedObserverConcurrentClose(t *testing.T) {
	for i := 0; i < 30; i++ {
		reader, writer := io.Pipe()
		observer := &encryptedErrorObserver{ReadCloser: reader, record: func([]byte) {}}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = io.Copy(io.Discard, observer) }()
		go func() {
			defer wg.Done()
			_, _ = io.WriteString(writer, `{"error":{"code":"invalid_encrypted_content"}}`)
			_ = writer.Close()
		}()
		_ = observer.Close()
		wg.Wait()
	}
}

func TestEncryptedMemoryAcrossWebsocketExecutorTurns(t *testing.T) {
	previousMemory, previousWS := rejectedEncryptedContent, WebsocketExecuteFunc
	rejectedEncryptedContent = &encryptedContentMemory{now: time.Now}
	t.Cleanup(func() { rejectedEncryptedContent, WebsocketExecuteFunc = previousMemory, previousWS })
	var sent [][]byte
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, device *DeviceProfileConfig, headers http.Header, route string) (*http.Response, error) {
		sent = append(sent, append([]byte(nil), body...))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_encrypted_content\"}}}\n\n"))}, nil
	}
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"reasoning","encrypted_content":"gAAAAold"},{"type":"message","role":"user","content":"continue"}]}`)
	for i := 0; i < 2; i++ {
		resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 51, AccessToken: "test"}, body, "same-session", "", "local", nil, http.Header{}, true)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if len(encryptedPayloadDigests(sent[0])) != 1 || len(encryptedPayloadDigests(sent[1])) != 0 || !strings.Contains(string(sent[1]), "continue") {
		t.Fatalf("cross-turn recovery failed: %q", sent)
	}
}
