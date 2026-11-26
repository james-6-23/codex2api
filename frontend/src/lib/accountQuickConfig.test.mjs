import assert from "node:assert/strict";
import test from "node:test";

import {
  accountHasQuickConfigDetails,
  buildQuickConfigSavePayload,
  canSaveQuickConfig,
  formStateFromAccount,
  isQuickConfigFormCurrent,
  normalizeCodexFingerprintMode,
} from "./accountQuickConfig.ts";

const listRow = {
  id: 42,
  detail_loaded: false,
  codex_fingerprint_mode: "full",
  tags: ["shared"],
  group_ids: [1],
};

const detailedRow = {
  id: 42,
  detail_loaded: true,
  codex_fingerprint_mode: "full",
  score_bias_override: null,
  base_concurrency_override: 4,
  scheduler_priority: 8,
  skip_warm_tier: true,
  proxy_url: "http://127.0.0.1:7890",
  custom_headers: { "Chatgpt-Account-Id": "team-workspace" },
  tags: ["shared"],
  group_ids: [1],
};

test("list rows without detail_loaded are not treated as a complete source", () => {
  assert.equal(accountHasQuickConfigDetails(listRow), false);
  assert.equal(accountHasQuickConfigDetails(detailedRow), true);
  assert.equal(accountHasQuickConfigDetails(null), false);
});

test("formStateFromAccount keeps full fingerprint mode instead of falling back to off", () => {
  const form = formStateFromAccount(detailedRow);
  assert.equal(form.accountId, 42);
  assert.equal(form.fingerprintMode, "full");
  assert.equal(form.concurrencyMode, "custom");
  assert.equal(form.concurrencyInput, "4");
  assert.match(form.customHeadersText, /team-workspace/);
});

test("missing fingerprint mode normalizes to off", () => {
  assert.equal(normalizeCodexFingerprintMode(undefined), "off");
  assert.equal(normalizeCodexFingerprintMode("converge"), "off");
  const form = formStateFromAccount({ id: 7 });
  assert.equal(form.fingerprintMode, "off");
  assert.equal(form.customHeadersText, "");
});

test("save is blocked until the form belongs to the current account", () => {
  const form = formStateFromAccount(detailedRow);
  assert.equal(
    canSaveQuickConfig({
      status: "loading",
      saving: false,
      form: null,
      accountId: 42,
    }),
    false,
  );
  assert.equal(
    canSaveQuickConfig({
      status: "error",
      saving: false,
      form: null,
      accountId: 42,
    }),
    false,
  );
  assert.equal(
    canSaveQuickConfig({
      status: "ready",
      saving: false,
      form,
      accountId: 99,
    }),
    false,
  );
  assert.equal(
    canSaveQuickConfig({
      status: "ready",
      saving: false,
      form,
      accountId: 42,
    }),
    true,
  );
  assert.equal(isQuickConfigFormCurrent(form, 42), true);
  assert.equal(isQuickConfigFormCurrent(form, 99), false);
});

test("payload is refused before details are ready so list defaults cannot clear headers", () => {
  const listForm = formStateFromAccount(listRow);
  const blocked = buildQuickConfigSavePayload(listForm, false);
  assert.equal(blocked.ok, false);
  if (!blocked.ok) assert.equal(blocked.error, "not_ready");

  const alsoBlocked = buildQuickConfigSavePayload(null, true);
  assert.equal(alsoBlocked.ok, false);
  if (!alsoBlocked.ok) assert.equal(alsoBlocked.error, "not_ready");
});

test("ready details round-trip custom headers and full fingerprint mode", () => {
  const form = formStateFromAccount(detailedRow);
  const result = buildQuickConfigSavePayload(form, true);
  assert.equal(result.ok, true);
  if (!result.ok) return;
  assert.equal(result.payload.codex_fingerprint_mode, "full");
  assert.deepEqual(result.payload.custom_headers, {
    "Chatgpt-Account-Id": "team-workspace",
  });
  assert.equal(result.payload.base_concurrency_override, 4);
  assert.equal(result.payload.scheduler_priority, 8);
  assert.equal(result.payload.skip_warm_tier, true);
  assert.equal(result.payload.proxy_url, "http://127.0.0.1:7890");
});

test("quick configuration sends groups only when the selection changes", () => {
  const form = formStateFromAccount({ ...detailedRow, group_ids: [1, 2] });
  for (const [groupIds, expected] of [[[2, 1], undefined], [[2, 3], [2, 3]], [[], []]]) {
    const result = buildQuickConfigSavePayload({ ...form, groupIds }, true);
    assert.equal(result.ok, true);
    assert.equal("group_ids" in result.payload, expected !== undefined);
    assert.deepEqual(result.payload.group_ids, expected);
  }
});

test("clearing the headers field after details loaded sends null, not an omitted key", () => {
  const form = {
    ...formStateFromAccount(detailedRow),
    customHeadersText: "   ",
  };
  const result = buildQuickConfigSavePayload(form, true);
  assert.equal(result.ok, true);
  if (!result.ok) return;
  assert.equal("custom_headers" in result.payload, true);
  assert.equal(result.payload.custom_headers, null);
});

test("invalid custom header JSON is rejected instead of being saved as empty", () => {
  const form = {
    ...formStateFromAccount(detailedRow),
    customHeadersText: "{not-json",
  };
  const result = buildQuickConfigSavePayload(form, true);
  assert.equal(result.ok, false);
  if (!result.ok) assert.equal(result.error, "invalid_headers");
});

for (const mode of ['single_machine_multi_window']) {
  test(`quick configuration preserves ${mode} on reopening`, () => {
    assert.equal(normalizeCodexFingerprintMode(mode), mode);
    assert.equal(formStateFromAccount({ ...detailedRow, codex_fingerprint_mode: mode }).fingerprintMode, mode);
  });
}
