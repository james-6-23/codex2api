import assert from "node:assert/strict";
import test from "node:test";

import {
  mergeConfirmedSettingsValues,
  resolveAutoSaveRollbackValues,
} from "./settingsAutoSave.ts";

test("auto-save rollback restores the last server-confirmed value instead of the typed draft", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
  };
  // blur 提交前按下的草稿值已经同步进 settingsFormRef，但服务器从未确认过它。
  const typedDraft = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
    site_name: "CodexProxy",
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(confirmed, typedDraft, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
  );
});

test("auto-save rollback only touches the failing request's fields", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
    max_concurrency: 16,
  };
  const submitted = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
    site_name: "Renamed",
    max_concurrency: 16,
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(confirmed, submitted, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
    "unrelated form edits must not leak into the rollback patch",
  );
});

test("auto-save rollback falls back to the submitted form before the first confirmed snapshot", () => {
  const submitted = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(null, submitted, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 3 * 1024 * 1024 },
  );
});

test("confirmed snapshot takes over only the acknowledged request's fields", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
  };

  const merged = mergeConfirmedSettingsValues(confirmed, {
    output_guard_max_channel_bytes: 4 * 1024 * 1024,
  });

  assert.deepEqual(merged, {
    output_guard_max_channel_bytes: 4 * 1024 * 1024,
    site_name: "CodexProxy",
  });
  assert.deepEqual(
    confirmed,
    {
      output_guard_max_channel_bytes: 2 * 1024 * 1024,
      site_name: "CodexProxy",
    },
    "the previous snapshot must not be mutated",
  );
  assert.equal(
    mergeConfirmedSettingsValues(null, {
      output_guard_max_channel_bytes: 4 * 1024 * 1024,
    }),
    null,
    "no snapshot is created before the initial load confirms one",
  );
});
