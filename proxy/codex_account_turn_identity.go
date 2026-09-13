package proxy

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type codexTurnIdentityInput struct {
	Turn bool
	Root bool
}

type codexTurnIdentityPlan struct {
	claims     []database.CodexIdentityAliasClaim
	epochs     map[string]database.CodexIdentityEpoch
	references map[string]database.CodexIdentityEpoch
}

func codexAccountTurnIdentityInputs(headers http.Header, body []byte) map[string]codexTurnIdentityInput {
	inputs := make(map[string]codexTurnIdentityInput)
	metadata := gjson.GetBytes(body, "client_metadata")
	for _, source := range []gjson.Result{metadata, diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")), gjson.Parse(headers.Get(codexTurnMetadataHeader))} {
		for _, field := range []string{"turn_id", "root_turn_id"} {
			value := source.Get(field)
			original := strings.ToLower(strings.TrimSpace(value.String()))
			if value.Type != gjson.String || original == "" {
				continue
			}
			if parsed, err := uuid.Parse(original); err == nil {
				original = parsed.String()
			}
			input := inputs[original]
			input.Turn = input.Turn || field == "turn_id"
			input.Root = input.Root || field == "root_turn_id"
			inputs[original] = input
		}
	}
	return inputs
}

func (fingerprint *CodexFingerprint) prepareAccountTurnIdentity(ctx context.Context, store CodexIdentityStore, mapping *codexAccountIdentity, rootKey string, currentEpoch database.CodexIdentityEpoch, diagnostic *codexAccountIdentityDiagnostic) (*codexTurnIdentityPlan, error) {
	plan := &codexTurnIdentityPlan{epochs: make(map[string]database.CodexIdentityEpoch), references: make(map[string]database.CodexIdentityEpoch)}
	if len(fingerprint.accountTurnIdentityInputs) > 32 {
		return nil, codexAccountIdentityError("出站轮次身份数量无效，请检查客户端元数据。")
	}
	ordered := make([]string, 0, len(fingerprint.accountTurnIdentityInputs))
	for original := range fingerprint.accountTurnIdentityInputs {
		ordered = append(ordered, original)
	}
	sort.Strings(ordered)
	mapping.turnAliases = make(map[string]string, len(ordered))
	for _, original := range ordered {
		input := fingerprint.accountTurnIdentityInputs[original]
		if mapping.preserveRoot {
			diagnostic.PreservedIDs = append(diagnostic.PreservedIDs, original)
			continue
		}
		parsed, err := uuid.Parse(original)
		if err != nil || parsed.Version() != 7 || parsed.Variant() != uuid.RFC4122 {
			return nil, codexAccountIdentityError("账号级出站轮次映射仅支持 UUIDv7，请检查 turn_id 与 root_turn_id。")
		}
		if len(mapping.secret) != 32 {
			return nil, codexAccountIdentityError("出站轮次映射密钥不可用，请核实会话身份策略。")
		}
		original = parsed.String()
		identityKey := codexIdentityDigest("codex-account-turn-epoch-v1", mapping.owner, mapping.account, original)
		referenceKey := codexIdentityDigest("codex-account-turn-reference-v1", rootKey, original)
		turnEpoch, found, bound, err := store.ReadCodexIdentityReference(ctx, referenceKey, identityKey)
		if err != nil {
			return nil, codexAccountIdentityError("暂时无法核实出站轮次映射，请稍后重试。")
		}
		if !found || !bound && turnEpoch.RootKey == currentEpoch.RootKey && turnEpoch.Generation < currentEpoch.Generation {
			turnEpoch = currentEpoch
		}
		turnMapping := *mapping
		turnMapping.epoch = turnEpoch.Segment
		turnMapping.mode = "account-suffix-v1"
		if turnEpoch.MappingVersion == database.CodexIdentityMappingUUIDv7 {
			turnMapping.mode = turnEpoch.MappingVersion
		}
		outbound, err := turnMapping.mapUUID(ctx, store, "turn", original)
		if err != nil {
			return nil, err
		}
		mapping.turnAliases[original] = outbound
		fields := make([]string, 0, 2)
		if input.Turn {
			fields = append(fields, "turn_id")
		}
		if input.Root {
			fields = append(fields, "root_turn_id")
		}
		diagnostic.Changes = append(diagnostic.Changes, turnMapping.identityChange(original, outbound, fields...))
		plan.claims = append(plan.claims, database.CodexIdentityAliasClaim{
			AliasKey:  codexIdentityDigest("codex-account-alias-v1", outbound),
			SourceKey: codexIdentityDigest("codex-account-turn-source-v1", mapping.owner, mapping.account, turnMapping.epoch, original),
		})
		plan.references[referenceKey] = turnEpoch
		if !bound {
			plan.epochs[identityKey] = turnEpoch
		}
	}
	return plan, nil
}

func (mapping *codexAccountIdentity) rewriteTurnValue(original string) string {
	if parsed, err := uuid.Parse(strings.TrimSpace(original)); err == nil {
		if alias := mapping.turnAliases[parsed.String()]; alias != "" {
			return alias
		}
	}
	return original
}
