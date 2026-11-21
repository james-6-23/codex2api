package proxy

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
)

const codexIdentityV2FromEnv = "CODEX_IDENTITY_V2_FROM_UNIX_MS"

var codexIdentityV2From atomic.Int64

// ConfigureCodexIdentityTimeFromEnv runs once at startup. The persisted cutoff
// must stay identical across restarts and replicas: changing it changes which
// existing source IDs use the legacy mapping. No request-time clock is involved.
func ConfigureCodexIdentityTimeFromEnv() error {
	raw := strings.TrimSpace(os.Getenv(codexIdentityV2FromEnv))
	var cutoff int64
	if raw != "" {
		var err error
		cutoff, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cutoff < 0 || cutoff >= 1<<48 {
			return fmt.Errorf("%s must be a Unix millisecond timestamp in [0, 2^48); empty or 0 preserves legacy mapping", codexIdentityV2FromEnv)
		}
	}
	codexIdentityV2From.Store(cutoff)
	return nil
}

// CodexIdentityV2FromUnixMilli exposes only the non-secret, effective cutoff.
func CodexIdentityV2FromUnixMilli() int64 { return codexIdentityV2From.Load() }

// deriveCodexIdentityUUIDv7 preserves the birth time of eligible source UUIDv7s
// while deriving their other bits in the caller's existing isolation namespace.
// Earlier or opaque IDs keep their exact legacy value. Do not use this for
// key-only cache fallbacks: they have no source creation time to preserve.
// This follows the time-preserving scoped-ID approach in codex-proxy-rs v3.48.0.
func deriveCodexIdentityUUIDv7(seed, original string) string {
	cutoff := codexIdentityV2From.Load()
	if cutoff > 0 {
		if source, err := uuid.Parse(original); err == nil && source.Version() == 7 && source.Variant() == uuid.RFC4122 {
			// UUIDv7 stores its Unix milliseconds in the first 48 bits.
			var timestamp [8]byte
			copy(timestamp[2:], source[:6])
			millis := int64(binary.BigEndian.Uint64(timestamp[:]))
			if millis >= cutoff {
				return deriveStableCodexUUIDv7("codex2api:identity-time:v2:"+seed, millis)
			}
		}
	}
	return DeriveStableSessionUUIDv7(seed)
}
