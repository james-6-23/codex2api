package proxy

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// withCompressionSetting 临时改写系统设置里的压缩开关，测试结束自动还原。
func withCompressionSetting(t *testing.T, enabled bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexRequestCompression = enabled
	ApplyRuntimeSettings(next)
}

func decodeZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer decoder.Close()
	plain, err := decoder.DecodeAll(data, nil)
	if err != nil {
		t.Fatalf("上游解压失败: %v", err)
	}
	return plain
}

func TestCompressCodexRequestBodyDefaultsToEnabled(t *testing.T) {
	// 默认开启：真实 Codex CLI 对 ChatGPT 后端的 /responses 本来就压，
	// 默认关等于默认不像真实客户端。
	if !DefaultRuntimeSettings().CodexRequestCompression {
		t.Fatal("DefaultRuntimeSettings 未默认开启压缩")
	}

	withCompressionSetting(t, true)
	body := []byte(`{"model":"gpt-5.4","instructions":"","input":[{"role":"user","content":"hello"}]}`)

	got, encoding := CompressCodexRequestBody(body)
	if encoding != "zstd" {
		t.Fatalf("Content-Encoding = %q, want %q", encoding, "zstd")
	}
	if bytes.Equal(got, body) {
		t.Fatal("body 未被压缩")
	}
	if plain := decodeZstd(t, got); !bytes.Equal(plain, body) {
		t.Fatalf("解压结果 = %q, want %q", plain, body)
	}
}

func TestCompressCodexRequestBodyRespectsSystemSetting(t *testing.T) {
	// 后台关掉开关后必须立刻回到明文——这是出问题时的一键回退路径，不需要重启。
	withCompressionSetting(t, false)
	body := []byte(`{"model":"gpt-5.4"}`)

	got, encoding := CompressCodexRequestBody(body)
	if encoding != "" {
		t.Fatalf("系统设置已关闭，仍返回 Content-Encoding=%q", encoding)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body 被改写 = %q, want %q", got, body)
	}
}

func TestCompressCodexRequestBodyEnvOverridesSystemSetting(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4"}`)

	// env 是部署级逃生阀：DB 不可达、后台打不开时仍要能强制切换，因此优先于系统设置。
	t.Run("env 强制开启", func(t *testing.T) {
		withCompressionSetting(t, false)
		t.Setenv("CODEX_REQUEST_COMPRESSION", "zstd")
		if _, encoding := CompressCodexRequestBody(body); encoding != "zstd" {
			t.Fatalf("env=zstd 未覆盖系统设置，encoding=%q", encoding)
		}
	})

	t.Run("env 强制关闭", func(t *testing.T) {
		withCompressionSetting(t, true)
		t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
		if _, encoding := CompressCodexRequestBody(body); encoding != "" {
			t.Fatalf("env=off 未覆盖系统设置，encoding=%q", encoding)
		}
	})
}

func TestCodexRequestCompressionUnknownEnvFallsBackToSetting(t *testing.T) {
	// 无法识别的取值不该被当成"关闭"，否则一个拼写错误会静默推翻后台配置。
	for _, value := range []string{"", "gzip", "br", "nonsense"} {
		t.Run("env="+value, func(t *testing.T) {
			withCompressionSetting(t, true)
			t.Setenv("CODEX_REQUEST_COMPRESSION", value)
			if !codexRequestCompressionEnabled() {
				t.Fatalf("CODEX_REQUEST_COMPRESSION=%q 覆盖了系统设置的开启状态", value)
			}

			withCompressionSetting(t, false)
			if codexRequestCompressionEnabled() {
				t.Fatalf("CODEX_REQUEST_COMPRESSION=%q 覆盖了系统设置的关闭状态", value)
			}
		})
	}
}

func TestCompressCodexRequestBodyEmptyBodyStaysPlaintext(t *testing.T) {
	withCompressionSetting(t, true)
	got, encoding := CompressCodexRequestBody(nil)
	if encoding != "" || got != nil {
		t.Fatalf("空 body 被压缩: body=%v encoding=%q", got, encoding)
	}
}
