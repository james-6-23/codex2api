import assert from "node:assert/strict";
import { test } from "node:test";
import {
  clampCodexTestPercent,
  codexTestTokenMetrics,
  codexTestWindowKind,
  formatCodexTestMS,
  formatCodexTestReset,
  isFinalCodexTestDiagnostics,
} from "./codexConnectionTest.ts";

test("window kind follows the backend minute thresholds", () => {
  assert.equal(codexTestWindowKind({ window_minutes: 300 }), "5h");
  assert.equal(codexTestWindowKind({ window_minutes: 10080 }), "7d");
  assert.equal(codexTestWindowKind({ window_minutes: 1440 }), "7d");
  assert.equal(codexTestWindowKind({ window_minutes: 30 }), "short");
  assert.equal(codexTestWindowKind({ used_percent: 5 }), "unknown");
  assert.equal(codexTestWindowKind(undefined), "unknown");
});

test("reset countdown formats coarse units and rejects garbage", () => {
  assert.equal(formatCodexTestReset(45), "45s");
  assert.equal(formatCodexTestReset(90), "1m");
  assert.equal(formatCodexTestReset(3600), "1h");
  assert.equal(formatCodexTestReset(4980), "1h 23m");
  assert.equal(formatCodexTestReset(90000), "1d 1h");
  assert.equal(formatCodexTestReset(-1), "");
  assert.equal(formatCodexTestReset(undefined), "");
  assert.equal(formatCodexTestReset(Number.NaN), "");
});

test("percent clamp keeps the bar inside its track", () => {
  assert.equal(clampCodexTestPercent(12.5), 12.5);
  assert.equal(clampCodexTestPercent(240), 100);
  assert.equal(clampCodexTestPercent(-3), null);
  assert.equal(clampCodexTestPercent(undefined), null);
});

test("token metrics scale bars against the largest observed count", () => {
  const metrics = codexTestTokenMetrics({ input_tokens: 40, output_tokens: 10, cached_input_tokens: 20 });
  assert.deepEqual(metrics.map((m) => m.key), ["input_tokens", "output_tokens", "cached_input_tokens", "reasoning_output_tokens"]);
  assert.equal(metrics[0].percent, 100);
  assert.equal(metrics[1].percent, 25);
  assert.equal(metrics[2].percent, 50);
  assert.equal(metrics[3].value, null);
  assert.equal(metrics[3].percent, 0);
  assert.equal(codexTestTokenMetrics(undefined).every((m) => m.value === null), true);
});

test("final diagnostics are recognised by duration_ms only", () => {
  assert.equal(isFinalCodexTestDiagnostics({ model: "gpt-5.4", http_status: 200 }), false);
  assert.equal(isFinalCodexTestDiagnostics({ model: "gpt-5.4", duration_ms: 0 }), true);
  assert.equal(isFinalCodexTestDiagnostics(null), false);
  assert.equal(formatCodexTestMS(1234), "1,234 ms");
  assert.equal(formatCodexTestMS(undefined), "—");
});
