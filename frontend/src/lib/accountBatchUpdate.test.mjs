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
    updateSchedulerPriority: true,
    schedulerPriority: 10,
  });

  assert.deepEqual(payload, {
    ids: [3, 7],
    score_bias_override: 25,
    base_concurrency_override: 4,
    scheduler_priority: 10,
  });
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
