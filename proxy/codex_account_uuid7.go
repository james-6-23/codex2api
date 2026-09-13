package proxy

import (
	"context"
	"time"

	"github.com/codex2api/database"
	"github.com/google/uuid"
)

func (mapping *codexAccountIdentity) mapUUID(ctx context.Context, store CodexIdentityStore, domain, original string) (string, error) {
	entropy := mapping.digest(domain, original)
	outbound := original[:len(original)-9] + entropy[:9]
	if mapping.mode == database.CodexIdentityMappingUUIDv7 {
		key := codexIdentityDigest(database.CodexIdentityMappingUUIDv7, domain, mapping.owner, mapping.account, mapping.epoch, original)
		var err error
		outbound, err = store.ResolveCodexIdentityUUIDv7(ctx, key, entropy)
		if err != nil {
			return "", codexAccountIdentityError("暂时无法读取或持久化 UUIDv7 出站映射，已停止发送，请核实数据库。")
		}
	}
	if outbound == original {
		return "", codexAccountIdentityError("出站会话标识发生冲突，已停止请求，请联系管理员。")
	}
	return outbound, nil
}

func (mapping *codexAccountIdentity) identityChange(original, outbound string, fields ...string) codexAccountIdentityChange {
	change := codexAccountIdentityChange{Original: original, Outbound: outbound, Fields: fields, Version: mapping.mode}
	if mapping.mode == database.CodexIdentityMappingUUIDv7 {
		parsed, err := uuid.Parse(outbound)
		if err == nil && parsed.Version() == 7 {
			seconds, nanoseconds := parsed.Time().UnixTime()
			created := time.Unix(seconds, nanoseconds).UTC()
			change.MappedAt = &created
		}
	}
	return change
}
