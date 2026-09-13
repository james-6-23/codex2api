import assert from "node:assert/strict";
import test from "node:test";

import { buildUsageLogSearchParams } from "../api.ts";

test("usage log search params include diagnostic and traffic filters", () => {
  const params = buildUsageLogSearchParams({
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
    q: "rate limit",
    searchScope: "error",
    requestType: "related_internal",
    model: "gpt-5.6-sol",
    endpoint: "/v1/responses",
    apiKeyId: "12",
    accountId: "34",
    fast: "true",
    stream: "true",
    compact: "true",
    hasCompactionHistory: "true",
    channel: "codex",
    status: "5xx",
    errorOnly: "true",
    errorKind: "server",
    retry: "true",
    viaWebsocket: "false",
    includeCanceled: "true",
  });

  assert.deepEqual(Object.fromEntries(params), {
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
    q: "rate limit",
    search_scope: "error",
    request_type: "related_internal",
    model: "gpt-5.6-sol",
    endpoint: "/v1/responses",
    api_key_id: "12",
    account_id: "34",
    fast: "true",
    stream: "true",
    compact: "true",
    has_compaction_history: "true",
    channel: "codex",
    status: "5xx",
    error_only: "true",
    error_kind: "server",
    retry: "true",
    via_websocket: "false",
    include_canceled: "true",
  });
});

test("usage log search params omit empty optional filters", () => {
  const params = buildUsageLogSearchParams({
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
    q: "",
    searchScope: "session",
    status: "",
    retry: "",
    requestType: "",
  });

  assert.deepEqual(Object.fromEntries(params), {
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
  });
});

test("session prefix search uses the shared server-side log filters", () => {
  const params = buildUsageLogSearchParams({ q: "01a09012", accountId: "1706", model: "gpt-5.6-sol", status: "5xx", requestType: "compaction" });
  assert.equal(params.get("q"), "01a09012");
  assert.equal(params.get("account_id"), "1706");
  assert.equal(params.get("model"), "gpt-5.6-sol");
  assert.equal(params.get("status"), "5xx");
  assert.equal(params.get("request_type"), "compaction");
});
