import assert from "node:assert/strict";
import test from "node:test";

import {
  getAccountStatusBadgeStatus,
  isOfficialCostHiddenAccount,
  isOfficialCostTooNew,
  isUnsampledQuotaAccount,
  needsOfficialCostReload,
  needsUsageReload,
  officialUsdFromDailyItems,
  officialUsdValue,
  supportsOfficialUsage,
} from "./usageFormat.ts";

test("usage reload accepts either optional usage window as sampled", () => {
  assert.equal(needsUsageReload({ status: "active" }), true);
  assert.equal(
    needsUsageReload({ status: "active", usage_percent_5h: 12 }),
    false,
  );
  assert.equal(
    needsUsageReload({ status: "ready", usage_percent_7d: 34 }),
    false,
  );
});

test("usage reload skips accounts that cannot be sampled", () => {
  assert.equal(needsUsageReload({ status: "unauthorized" }), false);
});

test("Claude usage probe without quota headers still counts as sampled", () => {
  const sampled = {
    status: "active",
    claude_api: true,
    claude_usage_probe_at: "2026-08-29T05:00:00Z",
    claude_usage_probe_error: "",
  };
  assert.equal(needsUsageReload(sampled), false);
  assert.equal(isUnsampledQuotaAccount(sampled), false);
  assert.equal(getAccountStatusBadgeStatus(sampled), "active");
});

test("Claude probe failures remain unsampled and are not eligible for OpenAI billing", () => {
  const failed = {
    status: "active",
    claude_api: true,
    claude_usage_probe_at: "2026-08-29T05:00:00Z",
    claude_usage_probe_error: "upstream timeout",
  };
  assert.equal(needsUsageReload(failed), true);
  assert.equal(isUnsampledQuotaAccount(failed), true);
  assert.equal(supportsOfficialUsage(failed), false);
  assert.equal(needsOfficialCostReload(failed), false);
});

test("unsampled quota accounts are not treated as available", () => {
  assert.equal(isUnsampledQuotaAccount({ status: "active" }), true);
  assert.equal(
    isUnsampledQuotaAccount({ status: "active", usage_percent_5h: 8 }),
    false,
  );
  assert.equal(
    isUnsampledQuotaAccount({ status: "unauthorized" }),
    false,
  );
  assert.equal(
    isUnsampledQuotaAccount({ status: "active", grok_api: true }),
    false,
  );
  assert.equal(
    isUnsampledQuotaAccount({ status: "active", openai_responses_api: true }),
    false,
  );
  assert.equal(getAccountStatusBadgeStatus({ status: "active" }), "unsampled");
  assert.equal(
    getAccountStatusBadgeStatus({ status: "active", usage_percent_7d: 12 }),
    "active",
  );
  assert.equal(
    getAccountStatusBadgeStatus({ status: "rate_limited" }),
    "rate_limited",
  );
  assert.equal(
    getAccountStatusBadgeStatus({ status: "overload_paused" }),
    "active",
  );
});

test("official cost reload only retries Codex accounts missing the snapshot", () => {
  assert.equal(needsOfficialCostReload({}), true);
  assert.equal(needsOfficialCostReload({ official_usd: 0 }), false);
  assert.equal(needsOfficialCostReload({ official_usd_7d: 0 }), false);
  assert.equal(needsOfficialCostReload({ official_usd_7d: 12.5 }), false);
  assert.equal(needsOfficialCostReload({ openai_responses_api: true }), false);
  assert.equal(needsOfficialCostReload({ grok_api: true }), false);
  assert.equal(needsOfficialCostReload({ claude_api: true }), false);
  assert.equal(
    needsOfficialCostReload({ access_token_type: "codex_at" }),
    false,
  );
  assert.equal(needsOfficialCostReload({ status: "error" }), false);
  assert.equal(needsOfficialCostReload({ status: "unauthorized" }), false);
  assert.equal(
    needsOfficialCostReload({
      created_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
    }),
    false,
  );
  assert.equal(
    needsOfficialCostReload({
      created_at: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString(),
    }),
    true,
  );
  assert.equal(supportsOfficialUsage({}), true);
  assert.equal(supportsOfficialUsage({ access_token_type: "codex_at" }), false);
  assert.equal(supportsOfficialUsage({ access_token_type: " CODEX_AT " }), false);
  assert.equal(supportsOfficialUsage({ openai_responses_api: true }), false);
  assert.equal(supportsOfficialUsage({ grok_api: true }), false);
  assert.equal(supportsOfficialUsage({ claude_api: true }), false);
  assert.equal(isOfficialCostHiddenAccount({ status: "error" }), true);
  assert.equal(isOfficialCostHiddenAccount({ status: "active" }), false);
  assert.equal(
    isOfficialCostTooNew({
      created_at: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
    }),
    true,
  );
});

test("official cost reload stops once the backend reports a completed sync", () => {
  // 同步成功但上游没有数据(官方统计滞后):继续重拉不会有结果,必须停。
  assert.equal(needsOfficialCostReload({ official_usage_synced: true }), false);
  assert.equal(
    needsOfficialCostReload({ official_usage_synced: false }),
    true,
  );
});

test("official billed badge prefers official_usd over the 7d alias", () => {
  assert.equal(officialUsdValue({}), null);
  assert.equal(officialUsdValue({ official_usd_7d: 1.5 }), 1.5);
  assert.equal(officialUsdValue({ official_usd: 9, official_usd_7d: 1.5 }), 9);
});

test("official billed badge sums all synced days unless a window is requested", () => {
  assert.equal(officialUsdFromDailyItems([]), null);
  assert.equal(
    officialUsdFromDailyItems([
      { day: "2026-08-20", usd: 10 },
      { day: "2026-08-25", usd: 1.25 },
      { day: "2026-08-26", usd: 2.5 },
      { day: "2026-08-27", usd: 3 },
    ]),
    16.75,
  );
  assert.equal(
    officialUsdFromDailyItems(
      [
        { day: "2026-08-20", usd: 10 },
        { day: "2026-08-26", usd: 2 },
        { day: "2026-08-27", usd: 3 },
      ],
      2,
    ),
    5,
  );
});
