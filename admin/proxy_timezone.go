package admin

import "github.com/codex2api/internal/timezone"

func validProxyTimezoneOverride(name string) bool {
	_, err := timezone.Load(name)
	return err == nil
}
