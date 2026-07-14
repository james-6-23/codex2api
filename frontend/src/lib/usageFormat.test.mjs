import assert from "node:assert/strict";
import test from "node:test";

import { needsUsageReload } from "./usageFormat.ts";

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
