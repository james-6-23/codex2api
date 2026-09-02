package proxy

import (
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Codex 请求体压缩保真。
//
// 真实 Codex CLI 对 ChatGPT 后端的 /responses 请求体默认做 zstd 压缩：特性
// enable_request_compression 在 codex-rs/features/src/lib.rs 里是 Stage::Stable +
// default_enabled=true，codex-rs/core/src/client.rs 的 responses_request_compression
// 对「ChatGPT 后端 + OpenAI provider」一律返回 Compression::Zstd。编码点见
// codex-rs/http-client/src/request.rs：zstd 级别 3、Content-Encoding: zstd、
// 没有最小长度阈值（再小的 body 也压），Content-Type 仍是 application/json。
//
// 本网关出站一直发明文 JSON，于是「originator 报 Codex 系客户端、UA 带新版版本号，
// 请求体却 100% 明文」构成一个每请求都在、零噪声的区分特征：上游只要按
// content-encoding 分组统计就能把代理流量整体切出来，比任何单个标识字段都稳。
//
// 默认开启（系统设置 codex_request_compression，DB 默认 true）：真实客户端本来就压，
// 默认关等于默认不像真实客户端。开关在管理后台可热更新，出问题一键关、无需重启。
//
// 只作用于 HTTP 路径，与 codex_force_websocket 正交——WS 走 permessage-deflate
// （拨号器 EnableCompression 已开），两者可同时生效，不需要为了压缩去关 WS。
//
// 一处已知的不完全保真：真实客户端用 libzstd，本网关用 klauspost/compress。
// 两者产出的都是合法 zstd 帧且上游解压结果一致，但帧头（窗口描述符、
// 内容长度字段的编码宽度）未必逐字节相同。这比"根本不压缩"接近得多，
// 但不足以对抗针对压缩器实现的字节级指纹。

// codexRequestCompressionEnabled 判定本次请求是否压缩。
//
// 环境变量 CODEX_REQUEST_COMPRESSION 显式设置时优先于系统设置：它是部署级逃生阀，
// 在 DB 不可达、后台打不开、或需要整机强制回退时仍然有效。未设置（或取值无法识别）
// 时以系统设置为准，由管理后台控制。
func codexRequestCompressionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_REQUEST_COMPRESSION"))) {
	case "zstd", "on", "true", "1":
		return true
	case "off", "none", "false", "0", "plain":
		return false
	}
	return CurrentRuntimeSettings().CodexRequestCompression
}

var (
	codexZstdEncoderOnce sync.Once
	codexZstdEncoder     *zstd.Encoder
)

// codexRequestZstdEncoder 返回进程级共享编码器。zstd.Encoder 的 EncodeAll 是并发
// 安全的，因此不需要每请求新建（新建会分配整套窗口缓冲，在高 RPM 下代价显著）。
// SpeedDefault 对应 zstd 级别 3，与真实客户端 encode_all(body, 3) 一致。
func codexRequestZstdEncoder() *zstd.Encoder {
	codexZstdEncoderOnce.Do(func() {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			// 参数是常量，构造失败不可达；留 nil 让调用方降级到明文而不是 panic。
			return
		}
		codexZstdEncoder = encoder
	})
	return codexZstdEncoder
}

// CompressCodexRequestBody 按部署档位压缩 Codex 官方上游的请求体。
//
// 返回 (出站字节, Content-Encoding 取值)。第二个返回值为空串表示未压缩，调用方
// 不应设置 Content-Encoding 头。压缩失败一律降级为明文——请求发出去比形态完美重要。
//
// 只在 HTTP /responses 上调用。两条相邻路径都不该调：
//   - /responses/compact：真实客户端这条路走 CompactClient::compact，压根没有
//     compression 参数（codex-api/src/endpoint/compact.rs），发的是明文 JSON。
//     两条路径都压反而比都不压更不像真实客户端。
//   - WebSocket：载荷是 response.create 帧，压缩语义归 WS 协议层，与 HTTP 的
//     Content-Encoding 不是一回事。
func CompressCodexRequestBody(body []byte) ([]byte, string) {
	if len(body) == 0 || !codexRequestCompressionEnabled() {
		return body, ""
	}
	encoder := codexRequestZstdEncoder()
	if encoder == nil {
		return body, ""
	}
	compressed := encoder.EncodeAll(body, nil)
	if len(compressed) == 0 {
		return body, ""
	}
	return compressed, "zstd"
}
