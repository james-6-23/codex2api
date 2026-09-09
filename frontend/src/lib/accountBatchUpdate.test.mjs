import assert from "node:assert/strict";
import test from "node:test";

import { buildBatchMetadataUpdate } from "./accountBatchUpdate.ts";

test("buildBatchMetadataUpdate includes enabled scheduler fields", () => {
  const payload = buildBatchMetadataUpdate({
    ids: [3, 7],
    updateTags: false,
    tags: ["ignored"],
    updateGroups: false,
    groupIds: [99],
    updateScoreBias: true,
    scoreBias: 25,
    updateBaseConcurrency: true,
    baseConcurrency: 4,
    updateSkipWarmTier: true,
    skipWarmTier: true,
    updateSchedulerPriority: true,
    schedulerPriority: 10,
  });

  assert.deepEqual(payload, {
    ids: [3, 7],
    score_bias_override: 25,
    base_concurrency_override: 4,
    skip_warm_tier: true,
    scheduler_priority: 10,
  });
});

test("buildBatchMetadataUpdate omits the fingerprint mode unless explicitly enabled", () => {
  const untouched = buildBatchMetadataUpdate({
    ids: [1],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateCodexFingerprintMode: false,
    codexFingerprintMode: "session",
  });

  assert.deepEqual(untouched, { ids: [1] });

  const applied = buildBatchMetadataUpdate({
    ids: [1],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateCodexFingerprintMode: true,
    codexFingerprintMode: "session",
  });

  assert.deepEqual(applied, { ids: [1], codex_fingerprint_mode: "session" });
});

test("buildBatchMetadataUpdate sends null only for enabled reset fields", () => {
  const payload = buildBatchMetadataUpdate({
    ids: [5],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: true,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: true,
    schedulerPriority: null,
  });

  assert.deepEqual(payload, {
    ids: [5],
    score_bias_override: null,
    scheduler_priority: null,
  });
});

test("buildBatchMetadataUpdate applies session capacity only when selected", () => {
  const untouched = buildBatchMetadataUpdate({
    ids: [2, 4],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateSessionCapacity: false,
    sessionCapacityEnabled: true,
    sessionCapacityMax: 8,
    sessionCapacityIdleTTLSeconds: 7200,
  });
  assert.deepEqual(untouched, { ids: [2, 4] });

  const applied = buildBatchMetadataUpdate({
    ids: [2, 4],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateSessionCapacity: true,
    sessionCapacityEnabled: true,
    sessionCapacityMax: 8,
    sessionCapacityIdleTTLSeconds: 7200,
  });
  assert.deepEqual(applied, {
    ids: [2, 4],
    session_capacity_enabled: true,
    session_capacity_max: 8,
    session_capacity_idle_ttl_seconds: 7200,
  });
});
