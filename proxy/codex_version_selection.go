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
	cachedPairs := make([]codexUAVersionPair, 0)
	for _, cached := range codexClientVersionTarget(string(spec.Kind), target).Pairs {
		cachedPairs = append(cachedPairs, codexUAVersionPair{CLIVersion: cached.CLIVersion, AppVersion: cached.AppVersion})
	}
	// 缓存优先；缓存里没有满足 floor 的配对时，回退内置 spec.VersionPairs，
	// 保证未同步的部署在 floor 提高后仍能拿到内置的更高版本配对，而不是直接
	// 返回 codex_client_version_unavailable（HTTP 503、不可重试）。
	if pair, ok := firstCodexPairMeetingFloor(cachedPairs, floor); ok {
		return pair, true
	}
	if pair, ok := firstCodexPairMeetingFloor(spec.VersionPairs, floor); ok {
		return pair, true
	}
	return codexUAVersionPair{}, false
}

// firstCodexPairMeetingFloor 从给定配对里挑出满足最低版本的最新配对（按应用构建号
// 新→旧排序）。返回 false 表示没有满足 floor 的配对。
func firstCodexPairMeetingFloor(pairs []codexUAVersionPair, floor string) (codexUAVersionPair, bool) {
	if len(pairs) == 0 {
		return codexUAVersionPair{}, false
	}
	sorted := append([]codexUAVersionPair(nil), pairs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].AppVersion != sorted[j].AppVersion {
			return codexBuildIsNewer(sorted[i].AppVersion, sorted[j].AppVersion)
		}
		cmp, valid := compareCodexClientVersions(sorted[i].CLIVersion, sorted[j].CLIVersion)
		return valid && cmp > 0
	})
	for _, pair := range sorted {
		if codexVersionAtLeast(pair.CLIVersion, floor) {
			return pair, true
		}
	}
	return codexUAVersionPair{}, false
}
