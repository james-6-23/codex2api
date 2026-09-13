package wsrelay

import (
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestContextTakeoverCompressionOptionsAffectWire(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	payload := append(compressionWindowPayload("settings-fixture", 64<<10), bytes.Repeat([]byte("repeated JSON request contents "), 2000)...)
	// Revisit levels after closing connections to exercise encoder pool reuse.
	for _, level := range []int{1, 6, 9, 1, 6} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("level=%d/disableFragmentation=%t", level, disabled), func(t *testing.T) {
				proxy.UpdateRuntimeSettings(func(s proxy.RuntimeSettings) proxy.RuntimeSettings {
					s.CodexWSCompressionLevel = level
					s.CodexWSDisableFragmentation = disabled
					return s
				})
				type observed struct {
					wire    []byte
					lengths []int
					err     error
				}
				result := make(chan observed, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					peer, buffered, err := w.(http.Hijacker).Hijack()
					if err != nil {
						result <- observed{err: err}
						return
					}
					defer peer.Close()
					_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
					if err = writeWindowHandshake(buffered, r, "permessage-deflate; client_max_window_bits=15"); err != nil {
						result <- observed{err: err}
						return
					}
					got := observed{}
					for {
						first, data, err := readCompressionFixtureFrame(buffered)
						if err != nil {
							got.err = err
							break
						}
						wantOpcode := byte(0)
						if len(got.lengths) == 0 {
							wantOpcode = 0x42
						}
						if first&79 != wantOpcode {
							got.err = fmt.Errorf("unexpected opcode/RSV1: %x", first)
							break
						}
						got.wire = append(got.wire, data...)
						got.lengths = append(got.lengths, len(data))
						if first&128 != 0 {
							break
						}
					}
					result <- got
				}))
				defer server.Close()
				connection, _, err := dialContextTakeover(context.Background(), &websocket.Dialer{HandshakeTimeout: time.Second}, server.URL, nil)
				require.NoError(t, err)
				defer connection.Close()
				// Changing settings must not alter an already established connection.
				proxy.UpdateRuntimeSettings(func(s proxy.RuntimeSettings) proxy.RuntimeSettings {
					s.CodexWSCompressionLevel = 10 - level
					s.CodexWSDisableFragmentation = !disabled
					return s
				})
				require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, payload))
				select {
				case got := <-result:
					require.NoError(t, got.err)
					var expected bytes.Buffer
					compressor, err := flate.NewWriter(&expected, level)
					require.NoError(t, err)
					_, err = compressor.Write(payload)
					require.NoError(t, err)
					require.NoError(t, compressor.Flush())
					want := bytes.Clone(expected.Bytes()[:expected.Len()-4])
					require.NoError(t, compressor.Close())
					require.Equal(t, want, got.wire, "selected compression level must control actual bytes")
					decoded, err := decodeWindowMessage(got.wire, nil)
					require.NoError(t, err)
					require.Equal(t, payload, decoded)
					if disabled {
						require.Len(t, got.lengths, 1)
						require.Greater(t, got.lengths[0], 16<<10)
					} else {
						require.Greater(t, len(got.lengths), 1)
						for _, size := range got.lengths {
							require.LessOrEqual(t, size, 16<<10)
						}
					}
				case <-time.After(4 * time.Second):
					t.Fatal("message capture timed out")
				}
			})
		}
	}
}
