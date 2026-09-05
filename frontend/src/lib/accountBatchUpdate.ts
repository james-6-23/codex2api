import type { BatchUpdateAccountsRequest, CodexFingerprintMode } from "../types";

export interface BuildBatchMetadataUpdateOptions {
  ids: number[];
  updateTags: boolean;
  tags: string[];
  updateGroups: boolean;
  groupIds: number[];
  updateScoreBias: boolean;
  scoreBias: number | null;
  updateBaseConcurrency: boolean;
  baseConcurrency: number | null;
  updateSkipWarmTier?: boolean;
  skipWarmTier?: boolean;
  updateSchedulerPriority: boolean;
  schedulerPriority: number | null;
  updateCodexFingerprintMode?: boolean;
  codexFingerprintMode?: CodexFingerprintMode;
  updateSessionCapacity?: boolean;
  sessionCapacityEnabled?: boolean;
  sessionCapacityMax?: number;
  sessionCapacityIdleTTLSeconds?: number;
}

export function buildBatchMetadataUpdate({
  ids,
  updateTags,
  tags,
  updateGroups,
  groupIds,
  updateScoreBias,
  scoreBias,
  updateBaseConcurrency,
  baseConcurrency,
  updateSkipWarmTier,
  skipWarmTier,
  updateSchedulerPriority,
  schedulerPriority,
  updateCodexFingerprintMode,
  codexFingerprintMode,
  updateSessionCapacity,
  sessionCapacityEnabled,
  sessionCapacityMax,
  sessionCapacityIdleTTLSeconds,
}: BuildBatchMetadataUpdateOptions): BatchUpdateAccountsRequest {
  const payload: BatchUpdateAccountsRequest = { ids: [...ids] };
  if (updateTags) payload.tags = [...tags];
  if (updateGroups) payload.group_ids = [...groupIds];
  if (updateScoreBias) payload.score_bias_override = scoreBias;
  if (updateBaseConcurrency)
    payload.base_concurrency_override = baseConcurrency;
  if (updateSkipWarmTier) payload.skip_warm_tier = skipWarmTier ?? false;
  if (updateSchedulerPriority) payload.scheduler_priority = schedulerPriority;
  if (updateCodexFingerprintMode)
    payload.codex_fingerprint_mode = codexFingerprintMode ?? "off";
  if (updateSessionCapacity) {
    payload.session_capacity_enabled = sessionCapacityEnabled ?? false;
    payload.session_capacity_max = sessionCapacityMax ?? 5;
    payload.session_capacity_idle_ttl_seconds =
      sessionCapacityIdleTTLSeconds ?? 3600;
  }
  return payload;
}
