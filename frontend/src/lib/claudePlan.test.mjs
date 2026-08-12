import assert from "node:assert/strict";
import test from "node:test";

import {
  claudePlanFilterCategory,
  resolveAccountClaudePlan,
  resolveClaudePlan,
} from "./claudePlan.ts";

test("Claude plan keys resolve to display metadata", () => {
  const cases = [
    ["free", { key: "free", display: "Free", paid: false }],
    ["pro", { key: "pro", display: "Pro", paid: true }],
    ["max5", { key: "max5", display: "Max 5×", paid: true }],
    ["max20", { key: "max20", display: "Max 20×", paid: true }],
    ["team", { key: "team", display: "Team", paid: true }],
    ["enterprise", { key: "enterprise", display: "Enterprise", paid: true }],
  ];

  for (const [key, expected] of cases) {
    assert.deepEqual(resolveClaudePlan(key), expected);
  }
});

test("raw rate limit tiers map like the backend does, longest match first", () => {
  // default_claude_max_20x also contains "max"; a coarse-first match would
  // report Max 5× for a Max 20× organization.
  assert.equal(resolveClaudePlan("default_claude_max_20x")?.key, "max20");
  assert.equal(resolveClaudePlan("default_claude_max_5x")?.key, "max5");
  assert.equal(resolveClaudePlan("default_claude_pro")?.key, "pro");
  assert.equal(resolveClaudePlan("DEFAULT_CLAUDE_TEAM")?.key, "team");
  assert.equal(resolveClaudePlan("claude_enterprise_tier")?.key, "enterprise");
  assert.equal(resolveClaudePlan("default_free")?.key, "free");

  for (const invalid of ["", "   ", "unknown_tier", null, undefined, 20]) {
    assert.equal(resolveClaudePlan(invalid), null);
  }
});

test("account resolution falls back to the raw tier when plan_type is blank", () => {
  assert.equal(
    resolveAccountClaudePlan({
      plan_type: "",
      claude_rate_limit_tier: "default_claude_max_20x",
    })?.key,
    "max20",
  );
  // plan_type wins when both are present.
  assert.equal(
    resolveAccountClaudePlan({
      plan_type: "pro",
      claude_rate_limit_tier: "default_claude_max_20x",
    })?.key,
    "pro",
  );
  assert.equal(
    resolveAccountClaudePlan({ plan_type: "", claude_rate_limit_tier: "" }),
    null,
  );
});

test("unknown plans land in the other filter bucket", () => {
  assert.equal(claudePlanFilterCategory({ plan_type: "max5" }), "max5");
  assert.equal(claudePlanFilterCategory({ plan_type: "api" }), "other");
  assert.equal(
    claudePlanFilterCategory({ plan_type: "", claude_rate_limit_tier: "" }),
    "other",
  );
});
