import assert from "node:assert/strict"
import test from "node:test"
import { shouldDeferAccountListFetch } from "./accountListFetch.ts"

test("status change off the first page defers the list fetch", () => {
  assert.equal(shouldDeferAccountListFetch(3, "error", "all"), true)
})

test("status change on the first page fetches immediately", () => {
  assert.equal(shouldDeferAccountListFetch(1, "error", "all"), false)
})

test("pagination with the same filter fetches immediately", () => {
  assert.equal(shouldDeferAccountListFetch(2, "all", "all"), false)
})
