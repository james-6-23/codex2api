import assert from "node:assert/strict";
import test from "node:test";

import {
  applyProxyTestResult,
  chunkProxyTestIDs,
  getProxyStatusBadgeKind,
  parseProxyBatchTestSSELine,
  readProxyBatchTestSSE,
} from "./proxyTestState.ts";

const healthyProxy = {
  enabled: true,
  test_status: "success",
  test_ip: "1.2.3.4",
  test_location: "US",
  test_timezone: "America/Los_Angeles",
  timezone_override: "Asia/Tokyo",
  test_latency_ms: 100,
};

test("untested proxies have a distinct status badge", () => {
  assert.equal(
    getProxyStatusBadgeKind({
      enabled: true,
      test_status: "untested",
    }),
    "untested",
  );
});

test("inconclusive probe failures preserve the previous proxy state", () => {
  assert.deepEqual(
    applyProxyTestResult(healthyProxy, {
      success: false,
      conclusive: false,
    }),
    healthyProxy,
  );
});

test("conclusive probe failures mark the proxy as error", () => {
  assert.deepEqual(
    applyProxyTestResult(healthyProxy, {
      success: false,
      conclusive: true,
    }),
    {
      ...healthyProxy,
      test_status: "error",
      test_ip: "",
      test_location: "",
      test_latency_ms: 0,
    },
  );
});

test("proxy timezone probes preserve overrides and discard inference for a changed exit", () => {
  for (const [result, expected] of [
    [{ success: true, ip: "1.2.3.4", timezone: "Europe/London" }, "Europe/London"],
    [{ success: true, ip: "1.2.3.4" }, "America/Los_Angeles"],
    [{ success: true, ip: "5.6.7.8" }, ""],
    [{ success: false, conclusive: true }, "America/Los_Angeles"],
  ]) {
    const updated = applyProxyTestResult(healthyProxy, result);
    assert.equal(updated.test_timezone, expected);
    assert.equal(updated.timezone_override, "Asia/Tokyo");
  }
});

test("proxy batch SSE lines decode progress events", () => {
  assert.deepEqual(
    parseProxyBatchTestSSELine(
      'data: {"type":"progress","proxy_id":7,"result":{"success":true,"conclusive":true}}',
    ),
    {
      type: "progress",
      proxy_id: 7,
      result: {
        success: true,
        conclusive: true,
      },
    },
  );
  assert.equal(parseProxyBatchTestSSELine(": keepalive"), null);
  assert.equal(parseProxyBatchTestSSELine("data: not-json"), null);
});

test("proxy batch SSE reader preserves events split across chunks", async () => {
  const encoder = new TextEncoder();
  const response = new Response(
    new ReadableStream({
      start(controller) {
        controller.enqueue(
          encoder.encode('data: {"type":"start","total":2}\n\ndata: {"type":"pro'),
        );
        controller.enqueue(
          encoder.encode('gress","proxy_id":1,"result":{"success":false,"conclusive":false}}\n\n'),
        );
        controller.enqueue(
          encoder.encode('data: {"type":"complete","current":2,"total":2}\n\n'),
        );
        controller.close();
      },
    }),
  );
  const events = [];

  const completeEvent = await readProxyBatchTestSSE(response, (event) =>
    events.push(event),
  );

  assert.deepEqual(
    events.map((event) => event.type),
    ["start", "progress", "complete"],
  );
  assert.equal(events[1].proxy_id, 1);
  assert.equal(events[1].result.conclusive, false);
  assert.deepEqual(completeEvent, {
    type: "complete",
    current: 2,
    total: 2,
  });
});

test("proxy batch SSE reader reports a stream that ends without complete", async () => {
  const encoder = new TextEncoder();
  const response = new Response(
    new ReadableStream({
      start(controller) {
        controller.enqueue(
          encoder.encode('data: {"type":"start","total":1}\n\n'),
        );
        controller.close();
      },
    }),
  );

  const completeEvent = await readProxyBatchTestSSE(response, () => {});

  assert.equal(completeEvent, null);
});

test("proxy test IDs are split to the server batch limit", () => {
  const ids = Array.from({ length: 205 }, (_, index) => index + 1);

  assert.deepEqual(
    chunkProxyTestIDs(ids).map((batch) => batch.length),
    [100, 100, 5],
  );
  assert.deepEqual(chunkProxyTestIDs([]), []);
});
