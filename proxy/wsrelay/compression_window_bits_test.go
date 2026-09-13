package wsrelay

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func compressionWindowPayload(label string, size int) []byte {
	result := make([]byte, 0, size+32)
	for index := 0; len(result) < size; index++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", label, index)))
		result = append(result, digest[:]...)
	}
	return result[:size]
}

func writeWindowHandshake(buffered *bufio.ReadWriter, request *http.Request, extensions string) error {
	digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, err := fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Extensions: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]), extensions)
	if err != nil {
		return err
	}
	return buffered.Flush()
}

func readWindowCompressedMessage(reader io.Reader) ([]byte, error) {
	var wire []byte
	for index := 0; ; index++ {
		first, payload, err := readCompressionFixtureFrame(reader)
		if err != nil {
			return nil, err
		}
		if index == 0 && first&79 != 0x42 {
			return nil, fmt.Errorf("expected compressed binary frame, got %x", first)
		}
		if index > 0 && first&79 != 0 {
			return nil, fmt.Errorf("expected continuation, got %x", first)
		}
		if len(payload) > 16<<10 {
			return nil, fmt.Errorf("unbounded data frame: %d", len(payload))
		}
		wire = append(wire, payload...)
		if first&128 != 0 {
			return wire, nil
		}
	}
}

func decodeWindowMessage(wire, dictionary []byte) ([]byte, error) {
	// Finish the message's sync flush and append an empty final DEFLATE block.
	stream := append(append([]byte(nil), wire...), 0, 0, 255, 255, 1, 0, 0, 255, 255)
	reader := flate.NewReaderDict(bytes.NewReader(stream), dictionary)
	defer reader.Close()
	return io.ReadAll(reader)
}

func TestContextTakeoverNegotiatedClientWindowSizes(test *testing.T) {
	for _, scenario := range []struct {
		bits                int
		independent, quoted bool
	}{
		{15, false, false}, {9, false, false}, {10, false, false},
		{11, false, false}, {12, false, false}, {13, false, false}, {14, false, false},
		{0, false, false}, {9, true, false}, {12, true, false}, {15, true, false}, {9, false, true},
	} {
		test.Run(fmt.Sprintf("bits_%d_independent_%t_quoted_%t", scenario.bits, scenario.independent, scenario.quoted), func(test *testing.T) {
			bits := scenario.bits
			if bits == 0 {
				bits = 15
			}
			window := 1 << bits
			seed := compressionWindowPayload("old-history", 4096)
			gap := compressionWindowPayload("new-history", window+13)
			near := compressionWindowPayload("near-history", 256)
			messages := [][]byte{seed, gap, seed, near, near}
			completed := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				err := func() error {
					if request.Header.Get("Sec-WebSocket-Extensions") != "permessage-deflate; client_max_window_bits" {
						return fmt.Errorf("unexpected compression offer")
					}
					peer, buffered, err := response.(http.Hijacker).Hijack()
					if err != nil {
						return err
					}
					defer peer.Close()
					_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
					extensions := "permessage-deflate"
					if scenario.bits != 0 {
						value := fmt.Sprint(bits)
						if scenario.quoted {
							value = `"` + value + `"`
						}
						extensions += "; client_max_window_bits=" + value
					}
					if scenario.independent {
						extensions += "; client_no_context_takeover; server_no_context_takeover"
					}
					if err := writeWindowHandshake(buffered, request, extensions); err != nil {
						return err
					}
					var dictionary []byte
					var compressed bytes.Buffer
					encoder, err := flate.NewWriter(&compressed, flate.BestSpeed)
					if err != nil {
						return err
					}
					defer encoder.Close()
					for index, expected := range messages {
						wire, err := readWindowCompressedMessage(buffered)
						if err != nil {
							return err
						}
						actual, err := decodeWindowMessage(wire, dictionary)
						if err != nil {
							return fmt.Errorf("message %d exceeds the retained %d-byte history: %w", index, window, err)
						}
						if !bytes.Equal(expected, actual) {
							return fmt.Errorf("message %d data mismatch", index)
						}
						if !scenario.independent {
							dictionary = append(dictionary, actual...)
							if len(dictionary) > window {
								dictionary = dictionary[len(dictionary)-window:]
							}
						}
						compressed.Reset()
						if scenario.independent {
							encoder.Reset(&compressed)
						}
						if _, err := encoder.Write(actual); err != nil {
							return err
						}
						if err := encoder.Flush(); err != nil {
							return err
						}
						encoded := compressed.Bytes()[:compressed.Len()-4]
						split := len(encoded) / 2
						if err := writeCompressionFixtureFrame(buffered, 0x42, encoded[:split]); err != nil {
							return err
						}
						if err := writeCompressionFixtureFrame(buffered, 0x80, encoded[split:]); err != nil {
							return err
						}
						if err := buffered.Flush(); err != nil {
							return err
						}
					}
					return nil
				}()
				completed <- err
			}))
			defer server.Close()
			connection, _, err := dialContextTakeover(context.Background(), &websocket.Dialer{HandshakeTimeout: time.Second}, server.URL, nil)
			require.NoError(test, err)
			defer connection.Close()
			connection.SetReadLimit(1 << 20)
			for _, message := range messages {
				require.NoError(test, connection.WriteMessage(websocket.BinaryMessage, message))
				kind, reader, err := connection.NextReader()
				require.NoError(test, err)
				require.Equal(test, websocket.BinaryMessage, kind)
				actual, err := io.ReadAll(reader)
				require.NoError(test, err)
				require.Equal(test, message, actual)
			}
			require.NoError(test, <-completed)
		})
	}
}

func TestContextTakeoverWindowResponseValidation(test *testing.T) {
	for _, parameter := range []string{"client_max_window_bits", "client_max_window_bits=7", "client_max_window_bits=8", `client_max_window_bits="8"`, "server_max_window_bits=8", "client_max_window_bits=16", "client_max_window_bits=09", "client_max_window_bits=12; client_max_window_bits=13"} {
		test.Run(strings.ReplaceAll(parameter, ";", "_"), func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				peer, buffered, err := response.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer peer.Close()
				_ = writeWindowHandshake(buffered, request, "permessage-deflate; "+parameter)
			}))
			defer server.Close()
			connection, _, err := dialContextTakeover(context.Background(), &websocket.Dialer{HandshakeTimeout: time.Second}, server.URL, nil)
			require.Error(test, err)
			require.Nil(test, connection)
			require.Contains(test, err.Error(), "permessage-deflate")
		})
	}
}
