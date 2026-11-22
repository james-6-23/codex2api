package proxy

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// ==================== Codex 客户端形态目录 ====================
//
// 真实 Codex 客户端的 User-Agent 形状是
//   {originator}/{cli版本} ({os} {os版本}; {arch}) {terminal} ({app名}; {app版本})
// 末尾标记按形态各不相同:codex-tui / codex_exec 复用前缀与 CLI 版本;ChatGPT 桌面端固定
// "Codex Desktop" 加桌面端构建号;VS Code 插件写宿主 IDE 名(VS Code / Cursor / Windsurf)
// 加插件构建号。目录中的历史版本对仅用作未同步时的默认画像。
//
// 下面的权重来自 2026-09-07~08 约 6.5 万条去重请求的下游 UA 统计(按百分比取整),
// 用于:预设默认值、管理页的搭配候选和号池画像分布。

// CodexClientKind 是可模拟的 Codex 客户端形态。
type CodexClientKind string

const (
	CodexClientKindTUI     CodexClientKind = "codex-tui"
	CodexClientKindDesktop CodexClientKind = "codex-desktop"
	CodexClientKindVSCode  CodexClientKind = "codex-vscode"
	CodexClientKindExec    CodexClientKind = "codex-exec"
	CodexClientKindCustom  CodexClientKind = "custom"
)

const (
	CodexUserAgentModeSingle = "single"
	CodexUserAgentModePool   = "pool"
)

type codexUAWeighted struct {
	Value  string
	Weight int
}

type codexUAPlatform struct {
	OSName    string
	OSVersion string
	Arch      string
	Weight    int
}

// codexUAVersionPair 是一组真实出现过的 (CLI 版本, 末尾标记版本) 配对;Weight 为观测占比。
type codexUAVersionPair struct {
	CLIVersion string
	AppVersion string
	Weight     int
}

type codexUAKindSpec struct {
	Kind          CodexClientKind
	ClientName    string // UA 前缀,同时作为 Originator
	AppFollowsCLI bool   // 末尾标记 = (client_name; cli_version),无独立构建号
	// 默认值:单画像模式下字段留空时使用。TUI 沿用历史常量,保证既有部署出站字节不变。
	DefaultPlatform codexUAPlatform
	DefaultTerminal string
	AppNames        []codexUAWeighted // 末尾标记名候选,首个为默认
	Terminals       []codexUAWeighted
	// ReferenceTerminals 只作管理页预设与搭配校验,不参与号池抽样,因此不会改变既有账号画像。
	ReferenceTerminals []string
	Platforms          []codexUAPlatform
	// ReferencePlatforms 同 ReferenceTerminals:只作预设与搭配校验,不参与号池抽样。
	ReferencePlatforms []codexUAPlatform
	VersionPairs       []codexUAVersionPair // 未同步时的画像默认值;AppFollowsCLI 时为空
}

var codexUAKindOrder = []CodexClientKind{
	CodexClientKindTUI,
	CodexClientKindDesktop,
	CodexClientKindVSCode,
	CodexClientKindExec,
}

// codexUAReferenceTerminals 是流量统计里没出现、但真实终端会产生的 token,按 Codex
// terminal-detection 规则推导:有 TERM_PROGRAM 时取 "TERM_PROGRAM/TERM_PROGRAM_VERSION",
// 其次是 Konsole / VTE 等专属环境变量,最后回退 TERM。版本取 2026-09 各终端最新稳定版。
var codexUAReferenceTerminals = []string{
	"iTerm.app/3.7.3",
	"Apple_Terminal/470.2",
	"ghostty/1.3.1",
	"WarpTerminal/v0.2026.09.16.08.27.stable_02",
	"WezTerm/20240203-110809-5046fc22",
	"WezTerm/20260819-012343-33891b4a",
	"vscode/1.139.1",
	"vscode/3.19.19", // Cursor:TERM_PROGRAM=vscode,版本为 Cursor 自身版本
	"zed/1.21.0",
	"kitty",
	"Alacritty",
	"Konsole/260801",
	"gnome-terminal",
	"VTE/8401",
	"WindowsTerminal",
	"mintty/3.8.3",
	"xterm-256color",
	"tmux-256color",
	"screen-256color",
}

// codexUAReferencePlatforms 是流量统计之后发布、尚未观测到的平台。
var codexUAReferencePlatforms = []codexUAPlatform{
	{OSName: "Mac OS", OSVersion: "27.0.0", Arch: "arm64"},
}

// 默认号池配比(按真实流量占比取整):桌面端过半,VS Code 次之,TUI 再次。
var defaultCodexUAPoolMix = map[CodexClientKind]int{
	CodexClientKindDesktop: 50,
	CodexClientKindVSCode:  30,
	CodexClientKindTUI:     20,
}

var codexUACatalog = map[CodexClientKind]*codexUAKindSpec{
	CodexClientKindTUI: {
		Kind:            CodexClientKindTUI,
		ClientName:      latestCodexClientName,
		AppFollowsCLI:   true,
		DefaultPlatform: codexUAPlatform{OSName: defaultCodexUserAgentOSName, OSVersion: defaultCodexUserAgentOSVersion, Arch: defaultCodexUserAgentArch},
		DefaultTerminal: defaultCodexUserAgentTerminal,
		AppNames:        []codexUAWeighted{{latestCodexClientName, 100}},
		Terminals: []codexUAWeighted{
			{"unknown", 30}, {"WindowsTerminal", 21}, {"kitty", 7}, {"xterm-256color", 7},
			{"vscode/1.135.0", 3}, {"vscode/1.126.0", 3}, {"Apple_Terminal/470.2", 3}, {"Apple_Terminal/455.1", 2},
			{"gnome-terminal", 3}, {"iTerm.app/3.6.11", 2}, {"vscode/1.101.2", 2}, {"xterm", 2}, {"ghostty/1.3.1", 1},
		},
		ReferenceTerminals: codexUAReferenceTerminals,
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.26200", "x86_64", 47}, {"NixOS", "26.5.0", "x86_64", 8}, {"Ubuntu", "22.4.0", "x86_64", 7},
			{"Windows", "10.0.19045", "x86_64", 6}, {"Ubuntu", "24.4.0", "x86_64", 4}, {"Ubuntu", "20.4.0", "aarch64", 3},
			{"Mac OS", "14.6.1", "x86_64", 3}, {"Mac OS", "26.6.2", "arm64", 2}, {"Windows", "10.0.22631", "x86_64", 2},
			{"CentOS", "7.0.0", "x86_64", 1}, {"Windows", "10.0.26100", "x86_64", 1}, {"Mac OS", "15.7.7", "x86_64", 1},
			{"Mac OS", "15.7.3", "arm64", 1}, {"Mac OS", "15.5.0", "arm64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
	},
	CodexClientKindDesktop: {
		Kind:            CodexClientKindDesktop,
		ClientName:      "Codex Desktop",
		DefaultPlatform: codexUAPlatform{OSName: "Windows", OSVersion: "10.0.26200", Arch: "x86_64"},
		DefaultTerminal: "unknown",
		AppNames:        []codexUAWeighted{{"Codex Desktop", 100}},
		Terminals:       []codexUAWeighted{{"unknown", 999}, {"dumb", 1}},
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.26200", "x86_64", 47}, {"Mac OS", "26.5.2", "arm64", 8}, {"Windows", "10.0.19045", "x86_64", 7},
			{"Windows", "10.0.22631", "x86_64", 5}, {"Mac OS", "14.4.1", "arm64", 5}, {"Mac OS", "26.5.1", "arm64", 4},
			{"Mac OS", "26.6.2", "arm64", 3}, {"Windows", "10.0.26100", "x86_64", 3}, {"Mac OS", "26.5.0", "arm64", 2},
			{"Windows", "10.0.22621", "x86_64", 2}, {"Mac OS", "26.2.0", "arm64", 1}, {"Mac OS", "26.3.0", "arm64", 1},
			{"Windows", "10.0.26220", "x86_64", 1}, {"Mac OS", "26.4.1", "arm64", 1}, {"Mac OS", "15.7.3", "arm64", 1},
			{"Mac OS", "26.4.0", "arm64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
		// 只收录正式版配对;alpha 构建不进预设。0.160.0 配对取自官方 appcast,
		// 保证未同步的部署也能越过 gpt-6.1-sol 的上游版本门槛(实测 0.159.0)。
		VersionPairs: []codexUAVersionPair{
			{"0.160.0", "26.930.51102", 1},
			{"0.153.4", "26.901.51231", 63}, {"0.153.4", "26.901.41600", 7}, {"0.153.3", "26.901.41123", 2},
			{"0.153.1", "26.901.31953", 1}, {"0.153.0", "26.901.22334", 2}, {"0.152.1", "26.831.21537", 1},
			{"0.152.0", "26.831.20005", 3}, {"0.150.1", "26.901.31953", 1}, {"0.149.1", "26.901.51231", 1},
		},
	},
	CodexClientKindVSCode: {
		Kind:            CodexClientKindVSCode,
		ClientName:      "codex_vscode",
		DefaultPlatform: codexUAPlatform{OSName: "Ubuntu", OSVersion: "22.4.0", Arch: "x86_64"},
		DefaultTerminal: "unknown",
		AppNames:        []codexUAWeighted{{"VS Code", 96}, {"Cursor", 3}, {"Windsurf", 1}, {"Positron", 1}},
		Terminals:       []codexUAWeighted{{"unknown", 70}, {"xterm-256color", 29}, {"WindowsTerminal", 1}, {"gnome-terminal", 1}},
		Platforms: []codexUAPlatform{
			{"Ubuntu", "22.4.0", "x86_64", 36}, {"Windows", "10.0.26200", "x86_64", 20}, {"Windows", "10.0.19045", "x86_64", 11},
			{"Ubuntu", "24.4.0", "x86_64", 9}, {"Mac OS", "26.6.2", "arm64", 3}, {"Mac OS", "26.4.0", "arm64", 3},
			{"CentOS", "8.6.2205", "x86_64", 3}, {"Ubuntu", "20.4.0", "x86_64", 2}, {"Windows", "10.0.22631", "x86_64", 2},
			{"Windows", "10.0.26100", "x86_64", 2}, {"Mac OS", "15.7.9", "arm64", 2}, {"CentOS", "7.0.0", "x86_64", 2},
			{"Windows", "10.0.22631", "aarch64", 1}, {"Mac OS", "26.5.2", "arm64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
		// 插件主力仍是 0.153.0,与 CLI 最新版 0.153.4 不同步。0.160.0 配对取自官方
		// marketplace,作用同桌面端。
		VersionPairs: []codexUAVersionPair{
			{"0.160.0", "26.930.41038", 1},
			{"0.153.4", "26.901.22334", 1}, {"0.153.0", "26.901.22334", 96}, {"0.147.0", "26.519.32039", 1},
			{"0.144.5", "26.707.91948", 1}, {"0.142.5", "26.623.81905", 1},
		},
	},
	CodexClientKindExec: {
		Kind:               CodexClientKindExec,
		ClientName:         "codex_exec",
		AppFollowsCLI:      true,
		DefaultPlatform:    codexUAPlatform{OSName: "Windows", OSVersion: "10.0.19045", Arch: "x86_64"},
		DefaultTerminal:    "unknown",
		AppNames:           []codexUAWeighted{{"codex_exec", 100}},
		Terminals:          []codexUAWeighted{{"unknown", 56}, {"dumb", 43}, {"kitty", 1}},
		ReferenceTerminals: codexUAReferenceTerminals,
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.19045", "x86_64", 52}, {"Ubuntu", "24.4.0", "x86_64", 40}, {"Ubuntu", "22.4.0", "x86_64", 3},
			{"Windows", "10.0.26100", "x86_64", 2}, {"Windows", "10.0.26200", "x86_64", 1}, {"Mac OS", "26.6.2", "arm64", 1},
			{"NixOS", "26.5.0", "x86_64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
	},
}

func codexUAKindSpecFor(kind CodexClientKind) (*codexUAKindSpec, bool) {
	spec, ok := codexUACatalog[kind]
	return spec, ok
}

// normalizeCodexClientKind 规范化配置里的 client_kind;空串表示"未指定,按 client_name 推断"。
func normalizeCodexClientKind(value string) (CodexClientKind, bool) {
	kind := CodexClientKind(strings.ToLower(strings.TrimSpace(value)))
	switch kind {
	case "":
		return "", true
	case CodexClientKindTUI, CodexClientKindDesktop, CodexClientKindVSCode, CodexClientKindExec, CodexClientKindCustom:
		return kind, true
	default:
		return "", false
	}
}

// inferCodexClientKind 按客户端名推断形态:未指定 kind 的旧配置与手填 "Codex Desktop"
// 都能自动落到对应预设;认不出的名字视为 custom。
func inferCodexClientKind(clientName string) CodexClientKind {
	name := strings.ToLower(strings.TrimSpace(clientName))
	if name == "" {
		return CodexClientKindTUI
	}
	for _, kind := range codexUAKindOrder {
		if spec := codexUACatalog[kind]; strings.EqualFold(spec.ClientName, name) {
			return kind
		}
	}
	return CodexClientKindCustom
}

func effectiveCodexClientKind(cfg CodexUserAgentConfig) CodexClientKind {
	if kind, ok := normalizeCodexClientKind(cfg.ClientKind); ok && kind != "" {
		return kind
	}
	return inferCodexClientKind(cfg.ClientName)
}

func codexUAHash(seed string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("codex2api:ua-catalog:v1:" + seed))
	return h.Sum32()
}

func pickCodexUAWeighted(items []codexUAWeighted, seed string) string {
	total := 0
	for _, it := range items {
		if it.Weight > 0 {
			total += it.Weight
		}
	}
	if total <= 0 || len(items) == 0 {
		return ""
	}
	r := int(codexUAHash(seed) % uint32(total))
	for _, it := range items {
		if it.Weight <= 0 {
			continue
		}
		if r < it.Weight {
			return it.Value
		}
		r -= it.Weight
	}
	return items[len(items)-1].Value
}

func pickCodexUAPlatform(items []codexUAPlatform, seed string) codexUAPlatform {
	total := 0
	for _, it := range items {
		if it.Weight > 0 {
			total += it.Weight
		}
	}
	if total <= 0 || len(items) == 0 {
		return codexUAPlatform{}
	}
	r := int(codexUAHash(seed) % uint32(total))
	for _, it := range items {
		if it.Weight <= 0 {
			continue
		}
		if r < it.Weight {
			return it
		}
		r -= it.Weight
	}
	return items[len(items)-1]
}

func codexVersionAtLeast(version, floor string) bool {
	floor = normalizeCodexClientVersionText(floor)
	if floor == "" {
		return true
	}
	cmp, ok := compareCodexClientVersions(version, floor)
	return ok && cmp >= 0
}

// resolveCodexVersionPair 为历史调用者提供默认平台的配对选择。
func resolveCodexVersionPair(spec *codexUAKindSpec, cliVersion, appVersion, versionFloor string) (string, string) {
	choice := codexVersionSelection{CLIOverride: cliVersion, AppOverride: appVersion, VersionFloor: versionFloor}
	if spec != nil {
		choice.OSName, choice.Arch = spec.DefaultPlatform.OSName, spec.DefaultPlatform.Arch
	}
	cli, app, _ := resolveCodexCurrentVersions(spec, choice)
	return cli, app
}

// codexPoolMix 返回生效的号池配比;未配置时用默认配比。
func codexPoolMix(cfg CodexUserAgentConfig) []codexUAWeighted {
	mix := cfg.PoolMix
	if len(mix) == 0 {
		mix = map[string]int{}
		for kind, w := range defaultCodexUAPoolMix {
			mix[string(kind)] = w
		}
	}
	keys := make([]string, 0, len(mix))
	for k := range mix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]codexUAWeighted, 0, len(keys))
	for _, k := range keys {
		if mix[k] > 0 {
			out = append(out, codexUAWeighted{Value: k, Weight: mix[k]})
		}
	}
	return out
}

// codexPoolPersona 按账号确定性地从目录抽取一套完整画像:同一账号永远得到同一画像,
// 不同账号按配比与真实分布散开。
func codexPoolPersona(cfg CodexUserAgentConfig, accountID int64, versionFloor string) (userAgent, version string, ok bool, err error) {
	mix := codexPoolMix(cfg)
	if len(mix) == 0 {
		return "", "", false, nil
	}
	seed := fmt.Sprintf("%d", accountID)
	kind := CodexClientKind(pickCodexUAWeighted(mix, "kind:"+seed))
	spec, found := codexUAKindSpecFor(kind)
	if !found {
		return "", "", false, nil
	}
	platform := pickCodexUAPlatform(spec.Platforms, "platform:"+seed)
	terminal := pickCodexUAWeighted(spec.Terminals, "terminal:"+seed)
	appName := spec.ClientName
	if !spec.AppFollowsCLI {
		appName = pickCodexUAWeighted(spec.AppNames, "app:"+seed)
	}
	cliVersion, appVersion, err := resolveCodexCurrentVersions(spec, codexVersionSelection{OSName: platform.OSName, Arch: platform.Arch, CLIOverride: cfg.ClientVersion, AppOverride: cfg.AppVersion, VersionFloor: versionFloor})
	if err != nil {
		return "", "", false, err
	}
	return formatCodexUserAgentWithApp(spec.ClientName, cliVersion, platform.OSName, platform.OSVersion, platform.Arch, terminal, appName, appVersion), cliVersion, true, nil
}
