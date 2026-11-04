import assert from "node:assert/strict";
import test from "node:test";
import {
  autoPauseThresholdPercentToRatio,
  formatAutoPauseThresholdPercent,
  isAutoPauseThresholdPercentInvalid,
} from "./autoPauseThreshold.ts";

test("percent input converts to backend ratio (issue #843)", () => {
  assert.equal(autoPauseThresholdPercentToRatio("95"), 0.95);
  assert.equal(autoPauseThresholdPercentToRatio(" 90 "), 0.9);
  assert.equal(autoPauseThresholdPercentToRatio("100"), 1);
  assert.equal(autoPauseThresholdPercentToRatio("87.5"), 0.875);
  assert.equal(autoPauseThresholdPercentToRatio(""), null);
  assert.equal(autoPauseThresholdPercentToRatio("0"), null);
});

test("percent input validation accepts 0..100 and blank", () => {
  assert.equal(isAutoPauseThresholdPercentInvalid(""), false);
  assert.equal(isAutoPauseThresholdPercentInvalid("0"), false);
  assert.equal(isAutoPauseThresholdPercentInvalid("95"), false);
  assert.equal(isAutoPauseThresholdPercentInvalid("100"), false);
  assert.equal(isAutoPauseThresholdPercentInvalid("101"), true);
  assert.equal(isAutoPauseThresholdPercentInvalid("-1"), true);
  assert.equal(isAutoPauseThresholdPercentInvalid("abc"), true);
});

test("backend ratio renders as percent text", () => {
  assert.equal(formatAutoPauseThresholdPercent(0.95), "95");
  assert.equal(formatAutoPauseThresholdPercent(0.875), "87.5");
  assert.equal(formatAutoPauseThresholdPercent(0.333333), "33.33");
  assert.equal(formatAutoPauseThresholdPercent(0), "");
  assert.equal(formatAutoPauseThresholdPercent(null), "");
  assert.equal(formatAutoPauseThresholdPercent(undefined), "");
});
