package proxy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const ErrorCodeCodexClientVersionUnavailable = "codex_client_version_unavailable"

type codexVersionSelection struct {
	OSName       string
	Arch         string
	CLIOverride  string
	AppOverride  string
	VersionFloor string
}

func codexClientVersionUnavailable(kind, target, floor string) error {
	return &Error{Code: ErrorCodeCodexClientVersionUnavailable, Type: ErrorTypeServerError,
		Message:    fmt.Sprintf("没有满足最低 CLI 版本 %s 的已知 Codex 客户端配对（%s / %s）", floor, kind, target),
		HTTPStatus: http.StatusServiceUnavailable, Retryable: false}
}

func codexTargetPlatform(osName, arch string) string {
	platform := "linux"
	switch strings.ToLower(osName) {
	case "mac os", "macos", "darwin":
		platform = "darwin"
	case "windows":
		platform = "win32"
	}
	switch strings.ToLower(arch) {
	case "aarch64", "arm64":
		return platform + "-arm64"
	case "x86_64", "x64", "amd64":
		return platform + "-x64"
	default:
		return platform + "-" + arch
	}
}

// resolveCodexCurrentVersions 只选择完整配对；手填字段独立覆盖默认值。
func resolveCodexCurrentVersions(spec *codexUAKindSpec, choice codexVersionSelection) (string, string, error) {
	cli, app := normalizeCodexClientVersionText(choice.CLIOverride), strings.TrimSpace(choice.AppOverride)
	if cli != "" && app != "" {
		return cli, app, nil
	}
	if spec == nil || spec.AppFollowsCLI {
		return resolveCodexCLISelection(spec, choice)
	}
	floor := choice.VersionFloor
	if cli != "" {
		floor = ""
	}
	target := codexTargetPlatform(choice.OSName, choice.Arch)
	pair, ok := selectCodexClientPair(spec, target, floor)
	if !ok {
		return "", "", codexClientVersionUnavailable(string(spec.Kind), target, floor)
	}
	return firstNonEmptyString(cli, pair.CLIVersion), firstNonEmptyString(app, pair.AppVersion), nil
}

func resolveCodexCLISelection(spec *codexUAKindSpec, choice codexVersionSelection) (string, string, error) {
	cli := normalizeCodexClientVersionText(choice.CLIOverride)
	if cli == "" {
		cli = effectiveLatestCodexCLIVersion()
		if !codexVersionAtLeast(cli, choice.VersionFloor) {
			return "", "", codexClientVersionUnavailable("codex-cli", "", choice.VersionFloor)
		}
	}
	return cli, firstNonEmptyString(choice.AppOverride, cli), nil
}

func selectCodexClientPair(spec *codexUAKindSpec, target, floor string) (codexUAVersionPair, bool) {
	pairs := make([]codexUAVersionPair, 0)
	for _, cached := range codexClientVersionTarget(string(spec.Kind), target).Pairs {
		pairs = append(pairs, codexUAVersionPair{CLIVersion: cached.CLIVersion, AppVersion: cached.AppVersion})
	}
	if len(pairs) == 0 {
		pairs = append(pairs, spec.VersionPairs...)
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].AppVersion != pairs[j].AppVersion {
			return codexBuildIsNewer(pairs[i].AppVersion, pairs[j].AppVersion)
		}
		cmp, valid := compareCodexClientVersions(pairs[i].CLIVersion, pairs[j].CLIVersion)
		return valid && cmp > 0
	})
	for _, pair := range pairs {
		if codexVersionAtLeast(pair.CLIVersion, floor) {
			return pair, true
		}
	}
	return codexUAVersionPair{}, false
}
