package wsrelay

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// The peer closes normally while a compressed message is still fragmented.
// This is a normal connection lifecycle, not a malformed-frame fixture.
func TestContextTakeoverFragmentInterruptedByNormalClose(test *testing.T) {
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	require.NoError(test, err)
	_, err = writer.Write(bytes.Repeat([]byte("response data "), 20))
	require.NoError(test, err)
	require.NoError(test, writer.Flush())
	fragment := append([]byte(nil), compressed.Bytes()...)
	require.NoError(test, writer.Close())
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		peer, buffered, err := response.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer peer.Close()
		digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		_ = writeCompressionFixtureFrame(buffered, 0x41, fragment)
		_ = writeCompressionFixtureFrame(buffered, 0x88, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "finished"))
		_ = buffered.Flush()
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(io.Discard, peer)
	}))
	defer server.Close()
	connection, _, err := dialContextTakeover(context.Background(), &websocket.Dialer{HandshakeTimeout: time.Second}, server.URL, nil)
	require.NoError(test, err)
	defer connection.Close()
	_, reader, err := connection.NextReader()
	require.NoError(test, err)
	require.NotPanics(test, func() { _, err = io.ReadAll(reader) })
	require.True(test, websocket.IsCloseError(err, websocket.CloseNormalClosure), "%v", err)
}

func writeCompressionFixtureFrame(writer io.Writer, first byte, payload []byte) error {
	header := []byte{first}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = binary.BigEndian.AppendUint16(append(header, 126), uint16(len(payload)))
	default:
		header = binary.BigEndian.AppendUint64(append(header, 127), uint64(len(payload)))
	}
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}
