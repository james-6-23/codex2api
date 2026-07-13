import assert from "node:assert/strict";
import test from "node:test";

import {
  commitDraftNumber,
  updateDraftNumber,
} from "./numberInputDraft.ts";

test("empty draft stays blank without replacing the current value", () => {
  assert.deepEqual(updateDraftNumber("", 1, { integer: true, min: 1 }), {
    draft: "",
    value: 1,
    changed: false,
  });

  assert.deepEqual(updateDraftNumber("25", 1, { integer: true, min: 1 }), {
    draft: "25",
    value: 25,
    changed: true,
  });
});

test("blur restores required values and commits optional empty values", () => {
  assert.equal(commitDraftNumber("", 25, { integer: true, min: 1 }), 25);
  assert.equal(
    commitDraftNumber("", 25, { integer: true, min: 0, emptyValue: 0 }),
    0,
  );
});

test("committed values are clamped to their configured range", () => {
  assert.equal(
    commitDraftNumber("125", 2, { integer: true, min: 1, max: 50 }),
    50,
  );
});
