package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/attestation"
)

var codexAttestationService = attestation.NewService(nil)

type CodexAttestationInput struct {
	Headers http.Header
	Account *auth.Account
	// 非 nil 表示恢复会话快照，空字符串也必须保留为“不发送”。
	FrozenValue *string
}

// ApplyCodexAttestation 在最终身份装配后，仅使用本项目生成或冻结的证明。
func ApplyCodexAttestation(ctx context.Context, input CodexAttestationInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if input.Headers == nil {
		return nil
	}
	setCodexAttestation(input.Headers, "")
	if input.FrozenValue != nil {
		setCodexAttestation(input.Headers, *input.FrozenValue)
		return nil
	}
	if input.Account == nil {
		return nil
	}
	request := codexAttestationRequest(input.Headers.Get("User-Agent"))
	if request.Platform == "" {
		return nil
	}
	request.Signals = codexAttestationSignals(input.Account, time.Now())
	value, err := codexAttestationService.Generate(ctx, request)
	if err != nil {
		return err
	}
	setCodexAttestation(input.Headers, value)
	return nil
}

func codexAttestationRequest(userAgent string) attestation.Request {
	parts := codexPersonaPlatformPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(parts) != 3 {
		return attestation.Request{}
	}
	target := codexTargetPlatform(parts[1], parts[2])
	platform, arch, _ := strings.Cut(target, "-")
	if platform != attestation.PlatformWindows && platform != attestation.PlatformMacOS {
		return attestation.Request{}
	}
	return attestation.Request{Platform: platform, Arch: arch}
}

func setCodexAttestation(headers http.Header, value string) {
	// 同时清理非规范大小写的残留，避免同名头被重复发送。
	for name := range headers {
		if strings.EqualFold(name, attestation.HeaderName) {
			delete(headers, name)
		}
	}
	if value != "" {
		headers.Set(attestation.HeaderName, value)
	}
}
