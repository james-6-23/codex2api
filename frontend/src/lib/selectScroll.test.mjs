import assert from "node:assert/strict";
import test from "node:test";

import { applyWheelScroll, selectAutoScrollKey } from "./selectScroll.ts";

test("selected option auto-scroll key is stable across dropdown repositioning", () => {
  assert.equal(selectAutoScrollKey(false, false, "gpt-5.4"), null);
  assert.equal(selectAutoScrollKey(true, false, "gpt-5.4"), null);
  assert.equal(selectAutoScrollKey(true, true, "gpt-5.4"), "gpt-5.4");
  assert.equal(selectAutoScrollKey(true, true, "gpt-5.4"), "gpt-5.4");
});

test("selected option auto-scroll runs again after the selected value changes", () => {
  assert.equal(selectAutoScrollKey(true, true, "gpt-5.4"), "gpt-5.4");
  assert.equal(selectAutoScrollKey(true, true, "gpt-5.5"), "gpt-5.5");
});

for (const [mode, lineHeight, dx, dy, expectedX, expectedY] of [
  [0, "20px", -0.5, 2.5, -0.5, 2.5],
  [1, "18.5px", -1, 2, -18.5, 37],
  [1, "normal", 1, -2, 16, -32],
  [2, "20px", -1, 2, -120, 160],
]) {
  test(`wheel deltaMode ${mode}, line-height ${lineHeight}`, () => {
    const list = {
      scrollLeft: 200, scrollTop: 200, clientWidth: 120, clientHeight: 80,
      scrollWidth: 1000, scrollHeight: 1000,
      ownerDocument: { defaultView: { getComputedStyle: () => ({ lineHeight }) } },
    };
    assert.equal(applyWheelScroll(list, { deltaMode: mode, deltaX: dx, deltaY: dy }), true);
    assert.equal(list.scrollLeft, 200 + expectedX);
    assert.equal(list.scrollTop, 200 + expectedY);
  });
}
