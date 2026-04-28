package image

import (
	"testing"
)

func TestValidateUpscale(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"2k", Upscale2K},
		{"4k", Upscale4K},
		{"", UpscaleNone},
		{"invalid", UpscaleNone},
		{"2K", UpscaleNone}, // 大小写敏感
	}

	for _, tt := range tests {
		got := ValidateUpscale(tt.input)
		if got != tt.want {
			t.Errorf("ValidateUpscale(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDoUpscale(t *testing.T) {
	// 创建一个 1x1 的测试图片
	testPNG := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG 签名
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x00, 0x03, 0x00, 0x01, 0x00, 0x18, 0xdd,
		0x8d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
		0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}

	t.Run("空档位返回原图", func(t *testing.T) {
		result, ct, err := DoUpscale(testPNG, "")
		if err != nil {
			t.Fatalf("DoUpscale failed: %v", err)
		}
		if ct != "" {
			t.Errorf("expected empty content-type, got %q", ct)
		}
		if len(result) != len(testPNG) {
			t.Errorf("expected original bytes, got different length")
		}
	})

	t.Run("无效输入", func(t *testing.T) {
		_, _, err := DoUpscale([]byte("invalid"), "2k")
		if err == nil {
			t.Error("expected error for invalid input")
		}
	})
}

func TestUpscaleCache(t *testing.T) {
	cache := NewUpscaleCache(1024*1024, 2) // 1MB 缓存

	t.Run("基础存取", func(t *testing.T) {
		key := "test-key"
		data := []byte("test data")
		ct := "image/png"

		// 未命中
		if _, _, ok := cache.Get(key); ok {
			t.Error("expected cache miss")
		}

		// 写入
		cache.Put(key, data, ct)

		// 命中
		gotData, gotCT, ok := cache.Get(key)
		if !ok {
			t.Fatal("expected cache hit")
		}
		if string(gotData) != string(data) {
			t.Errorf("got data %q, want %q", gotData, data)
		}
		if gotCT != ct {
			t.Errorf("got ct %q, want %q", gotCT, ct)
		}
	})

	t.Run("并发控制", func(t *testing.T) {
		cache.Acquire()
		cache.Release()
	})
}

func TestClampThumbKB(t *testing.T) {
	tests := []struct {
		input int
		want  int
	}{
		{0, 0},
		{-1, 0},
		{32, 32},
		{64, 64},
		{100, MaxThumbKB},
	}

	for _, tt := range tests {
		got := ClampThumbKB(tt.input)
		if got != tt.want {
			t.Errorf("ClampThumbKB(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
