package database

// NormalizeCodexWSCompressionLevel preserves the existing fast encoder default
// for old, missing or invalid persisted settings. The admin API rejects invalid input.
func NormalizeCodexWSCompressionLevel(level int) int {
	if level < 1 || level > 9 {
		return 1
	}
	return level
}
