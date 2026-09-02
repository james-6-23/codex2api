package proxy

import (
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// readUpstreamRequestBody 读取 mock 上游收到的请求体，按 Content-Encoding 解压。
//
// 请求体 zstd 压缩默认开启（对齐真实 Codex CLI，见 codex_request_compression.go），
// 所以测试里的假上游必须和真实上游一样会解压，否则断言的是压缩帧而不是 JSON。
//
// 用它而不是在测试里把压缩关掉：关掉等于让整套 HTTP 路径测试绕开默认配置，
// 那条路径上的改写逻辑就再也没有被真实形态覆盖过。
//
// 函数是幂等的：没有 Content-Encoding 时原样返回，因此对不压缩的链路
// （grok / admin / WS 等）替换后语义不变。
func readUpstreamRequestBody(r *http.Request) []byte {
	if r == nil || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) == 0 {
		return raw
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "zstd") {
		return raw
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return raw
	}
	defer decoder.Close()
	plain, err := decoder.DecodeAll(raw, nil)
	if err != nil {
		return raw
	}
	return plain
}
