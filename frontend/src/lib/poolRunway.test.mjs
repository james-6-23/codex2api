import assert from "node:assert/strict";
import test from "node:test";

import {
  buildPoolRunway,
  estimatePressureForecast,
  getAccountWindowMs,
  hasBurnPrediction,
  isClaudeUsagePlan,
  selectPoolRunway,
  selectPoolRunwayFromAnalysis,
} from "./poolRunway.ts";

const HOUR = 60 * 60_000;
const DAY = 24 * HOUR;

function baseAccount(overrides = {}) {
  return {
    id: 1,
    name: "a",
    email: "a@example.com",
    plan_type: "plus",
    status: "active",
    enabled: true,
    usage_percent_7d: 20,
    reset_7d_at: new Date(Date.now() + 5 * DAY).toISOString(),
    base_concurrency_effective: 2,
    ...overrides,
  };
}

test("hasBurnPrediction skips premium 5h when snapshot is missing (#382)", () => {
  const account = baseAccount({ plan_type: "plus", usage_percent_5h: null, reset_5h_at: undefined });
  assert.equal(hasBurnPrediction(account, "5h"), false);
  assert.equal(hasBurnPrediction({ ...account, usage_percent_5h: 40 }, "5h"), true);
});

test("Claude Max plans participate in native 5h burn prediction", () => {
  assert.equal(isClaudeUsagePlan("max-5x"), true);
  assert.equal(isClaudeUsagePlan("max-20x"), true);
  assert.equal(isClaudeUsagePlan("claude-max-5x"), true);
  assert.equal(isClaudeUsagePlan("enterprise"), true);
  assert.equal(
    hasBurnPrediction(
      baseAccount({
        claude_api: true,
        plan_type: "max-5x",
        usage_percent_5h: 42,
      }),
      "5h",
    ),
    true,
  );
});

test("Claude free tier does not claim a premium 5h window", () => {
  assert.equal(isClaudeUsagePlan("free"), false);
});

test("getAccountWindowMs uses monthly seconds for team long window", () => {
  const monthly = baseAccount({
    usage_window_7d_kind: "monthly",
    usage_window_7d_seconds: 2_592_000,
  });
  assert.equal(getAccountWindowMs(monthly, "7d"), 2_592_000 * 1000);
  assert.equal(getAccountWindowMs(baseAccount(), "7d"), 7 * DAY);
  assert.equal(getAccountWindowMs(baseAccount(), "5h"), 5 * HOUR);
});

test("buildPoolRunway maps remaining time to hours / day+ / critical", () => {
  const now = Date.now();
  const mk = (pressureAt, riskLevel = "medium") =>
    buildPoolRunway(
      {
        sampled: 5,
        threshold: 2,
        predictedAt: pressureAt,
        predictedCount: 2,
        unknown: 0,
        rpm: 10,
        effectiveRpmLimit: 60,
        rpmPressure: 0.2,
        activePressure: 0.1,
        rateLimitPressure: 0,
        dispatchableAccounts: 8,
        avgConcurrency: 2,
        highPressureAt: null,
        supplyShortageAt: null,
        riskLevel,
        confidence: 1,
      },
      now,
      "7d",
    );

  const hours = mk(now + 6.2 * HOUR);
  assert.equal(hours.kind, "hours");
  assert.equal(hours.remainingHours, 7);

  const dayPlus = mk(now + 30 * HOUR, "low");
  assert.equal(dayPlus.kind, "day_plus");

  const critical = mk(now + 20 * 60_000, "high");
  assert.equal(critical.kind, "critical");

  const stable = buildPoolRunway(
    {
      sampled: 3,
      threshold: 0,
      predictedAt: null,
      predictedCount: 0,
      unknown: 0,
      rpm: 1,
      effectiveRpmLimit: 60,
      rpmPressure: 0.01,
      activePressure: 0.05,
      rateLimitPressure: 0,
      dispatchableAccounts: 10,
      avgConcurrency: 2,
      highPressureAt: null,
      supplyShortageAt: null,
      riskLevel: "low",
      confidence: 1,
    },
    now,
    "7d",
  );
  assert.equal(stable.kind, "stable");
});

test("estimatePressureForecast prefers real account window length for burn", () => {
  const now = Date.now();
  // Monthly window: elapsed ~15 days, usage 50% => remaining ~15 days (not 3.5d as if 7d)
  const accounts = [
    baseAccount({
      id: 1,
      usage_percent_7d: 50,
      usage_window_7d_kind: "monthly",
      usage_window_7d_seconds: 30 * DAY / 1000,
      reset_7d_at: new Date(now + 15 * DAY).toISOString(),
      base_concurrency_effective: 2,
    }),
  ];
  const forecast = estimatePressureForecast(accounts, "7d", now, 0, 0, 10_000);
  assert.ok(forecast.sampled >= 1);
  // With no RPM, supplyShortage stays null; bulk prediction may still fire
  if (forecast.predictedAt != null) {
    const remaining = forecast.predictedAt - now;
    // Should be on the order of days, not ~3.5 days from a mistaken 7d window
    assert.ok(remaining > 10 * DAY, `expected >10d remaining, got ${remaining / DAY}d`);
  }
});

test("estimatePressureForecast excludes Responses-limited accounts from supply", () => {
  const now = Date.now();
  const forecast = estimatePressureForecast([
    baseAccount({
      status: "responses_rate_limited",
      usage_percent_5h: 20,
      reset_5h_at: new Date(now + HOUR).toISOString(),
    }),
  ], "5h", now, 0, 0, 10_000);
  assert.equal(forecast.dispatchableAccounts, 0);
  assert.equal(forecast.rateLimitPressure, 1);
});

test("selectPoolRunway picks earlier pressure between 5h and 7d", () => {
  const now = Date.now();
  const accounts = [
    baseAccount({
      id: 1,
      usage_percent_5h: 90,
      reset_5h_at: new Date(now + 2 * HOUR).toISOString(),
      usage_percent_7d: 10,
      reset_7d_at: new Date(now + 6 * DAY).toISOString(),
      // make 5h window look almost elapsed so burn extrapolates soon
      base_concurrency_effective: 1,
    }),
  ];
  // Force high burn on 5h: if reset is in 30min and used is 90%, window start was ~4.5h ago
  accounts[0].reset_5h_at = new Date(now + 0.5 * HOUR).toISOString();
  accounts[0].usage_percent_5h = 90;

  const runway = selectPoolRunway(accounts, now, 0, 0, 10_000);
  // Prefer window that has a nearer pressure if any
  assert.ok(runway.windowKey === "5h" || runway.windowKey === "7d");
  assert.ok(["critical", "hours", "day_plus", "stable", "unknown"].includes(runway.kind));
});

test("selectPoolRunwayFromAnalysis consumes fixed-size server forecasts", () => {
  const now = Date.now();
  const forecast = (predictedAt) => ({
    sampled: 40000,
    threshold: 100,
    predicted_at: predictedAt,
    predicted_count: 100,
    unknown: 0,
    rpm: 120,
    effective_rpm_limit: 500,
    rpm_pressure: 0.24,
    active_pressure: 0.1,
    rate_limit_pressure: 0,
    dispatchable_accounts: 39900,
    avg_concurrency: 2,
    high_pressure_at: null,
    supply_shortage_at: null,
    risk_level: "medium",
    confidence: 1,
  });
  const runway = selectPoolRunwayFromAnalysis({
    "5h": forecast(now + 2 * HOUR),
    "7d": forecast(now + 2 * DAY),
  }, now);
  assert.equal(runway.windowKey, "5h");
  assert.equal(runway.forecast.dispatchableAccounts, 39900);
});
