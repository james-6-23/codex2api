package proxy

import (
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// TestRustlsSpecStressNoPanic 直接压测上次崩溃的路径：反复 ApplyPreset(rustlsClientHelloSpec())
// 并带 session cache + OmitEmptyPsk（与生产 createConnection 完全一致的配置），
// 1000 次不得 panic，且 PSK 若存在必须最后。防止随机打乱重新引入间歇性崩溃。
func TestRustlsSpecStressNoPanic(t *testing.T) {
	const iterations = 1000
	for i := 0; i < iterations; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("第 %d 次 panic: %v", i, r)
				}
			}()
			// 用 net.Pipe 造一个 conn，只 BuildHandshakeState（不真正握手），
			// 触发 uTLS 的 PSK/session 校验逻辑——这正是上次 panic 的地方。
			c1, c2 := net.Pipe()
			defer c1.Close()
			defer c2.Close()
			uconn := utls.UClient(c1, &utls.Config{
				ServerName:         "chatgpt.com",
				InsecureSkipVerify: true,
				ClientSessionCache: utls.NewLRUClientSessionCache(4), // 复现 session resumption
				OmitEmptyPsk:       true,
			}, utls.HelloCustom)
			if err := uconn.ApplyPreset(rustlsClientHelloSpec()); err != nil {
				t.Fatalf("第 %d 次 ApplyPreset 失败: %v", i, err)
			}
			// BuildHandshakeState 触发 checkSessionExts（PSK 必须最后的硬校验）
			if err := uconn.BuildHandshakeState(); err != nil {
				t.Fatalf("第 %d 次 BuildHandshakeState 失败: %v", i, err)
			}
		}()
	}
}

// TestRustlsSpecPSKAlwaysLast 直接检查 spec 结构：末位扩展必须是 PSK。
func TestRustlsSpecPSKAlwaysLast(t *testing.T) {
	for i := 0; i < 200; i++ {
		spec := rustlsClientHelloSpec()
		if len(spec.Extensions) == 0 {
			t.Fatal("spec 无扩展")
		}
		last := spec.Extensions[len(spec.Extensions)-1]
		if _, ok := last.(*utls.UtlsPreSharedKeyExtension); !ok {
			t.Fatalf("第 %d 次末位扩展不是 PSK, 而是 %T", i, last)
		}
	}
}

var _ = time.Now // keep time import if unused elsewhere
