package proxy

import (
	"regexp"
	"strings"
)

var codexPersonaPlatformPattern = regexp.MustCompile(`^.+/[^ ]+ \((.+) [^ ;]+; ([^;)]+)\)`)
var codexPersonaAppVersionPattern = regexp.MustCompile(`\([^();]+; ([^()]+)\)$`)

func codexPersonaVersionSource(persona CodexUserAgentPersona, cfg CodexUserAgentConfig) CodexUserAgentPersona {
	if app := codexPersonaAppVersionPattern.FindStringSubmatch(persona.UserAgent); len(app) == 2 {
		persona.AppVersion = app[1]
	}
	if platform := codexPersonaPlatformPattern.FindStringSubmatch(persona.UserAgent); len(platform) == 3 {
		persona.TargetPlatform = codexTargetPlatform(platform[1], platform[2])
	}
	if codexHasManualVersionOverride(cfg) {
		persona.Source, persona.Status = "custom", "custom"
		return persona
	}
	kind := inferCodexClientKind(codexUserAgentClientName(persona.UserAgent))
	if kind != CodexClientKindDesktop && kind != CodexClientKindVSCode {
		persona.Source, persona.Status = "official_github_release", "independent_cli"
		return persona
	}
	target := codexClientVersionTarget(string(kind), persona.TargetPlatform)
	for _, pair := range target.Pairs {
		if pair.CLIVersion == persona.Version && pair.AppVersion == persona.AppVersion {
			persona.Source, persona.Status = pair.Source, target.Status
			return persona
		}
	}
	persona.Source, persona.Status = "builtin_observed", "builtin"
	return persona
}

// CodexClientVersionCacheView 补全尚未同步的目标，展示其内置回退配对。
func CodexClientVersionCacheView() []CodexClientVersionTarget {
	var view []CodexClientVersionTarget
	for _, kind := range []CodexClientKind{CodexClientKindDesktop, CodexClientKindVSCode} {
		for _, platform := range codexVSCodeTargets {
			if kind == CodexClientKindDesktop && strings.HasPrefix(platform, "linux-") {
				continue
			}
			target := codexClientVersionTarget(string(kind), platform)
			target.ClientKind, target.TargetPlatform = string(kind), platform
			target.Pairs = append([]CodexClientVersionPair(nil), target.Pairs...)
			if len(target.Pairs) == 0 {
				target = codexClientBuiltinTarget(target)
			}
			view = append(view, target)
		}
	}
	return view
}

func codexClientBuiltinTarget(target CodexClientVersionTarget) CodexClientVersionTarget {
	if target.Status == "" {
		target.Status = "builtin"
	}
	spec, ok := codexUAKindSpecFor(CodexClientKind(target.ClientKind))
	if !ok {
		return target
	}
	pair, ok := selectCodexClientPair(spec, target.TargetPlatform, "")
	if ok {
		target.Pairs = []CodexClientVersionPair{{AppVersion: pair.AppVersion, CLIVersion: pair.CLIVersion, Source: "builtin_observed"}}
	}
	return target
}

func codexHasManualVersionOverride(cfg CodexUserAgentConfig) bool {
	return cfg.RawUserAgent != "" || cfg.ClientVersion != "" || cfg.AppVersion != ""
}
