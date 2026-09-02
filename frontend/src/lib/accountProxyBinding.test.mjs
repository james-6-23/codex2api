import assert from "node:assert/strict";
import test from "node:test";

import {
  buildProxyBindingContext,
  formatProxyDisplayURL,
  formatProxyEgressSummary,
  formatProxyShortName,
  isManagedProxyUnavailable,
  proxyIsUsable,
  resolveAccountProxyBinding,
} from "./accountProxyBinding.ts";

const healthy = {
  id: 1,
  url: "http://hk-01:8080",
  label: "HK-01",
  enabled: true,
  test_status: "success",
  test_ip: "1.2.3.4",
  test_location: "香港",
  test_latency_ms: 82,
};

const disabled = {
  id: 2,
  url: "http://jp-02:8080",
  label: "JP-02",
  enabled: false,
  test_status: "success",
};

const failed = {
  id: 3,
  url: "http://sg-03:8080",
  label: "SG-03",
  enabled: true,
  test_status: "error",
};

const groups = [
  { id: 10, name: "主力", proxy_urls: ["http://hk-01:8080"] },
  { id: 11, name: "无代理组", proxy_urls: [] },
  { id: 12, name: "全挂组", proxy_urls: ["http://sg-03:8080"] },
];

function ctxWith(overrides = {}) {
  return buildProxyBindingContext({
    proxies: [healthy, disabled, failed],
    groups,
    poolEnabled: true,
    globalProxy: "",
    ...overrides,
  });
}

test("pool membership mirrors ListEnabledProxies: enabled and not test-failed", () => {
  assert.equal(proxyIsUsable(healthy), true);
  assert.equal(proxyIsUsable(disabled), false);
  assert.equal(proxyIsUsable(failed), false);
  assert.equal(proxyIsUsable({ enabled: true }), true);

  const ctx = ctxWith();
  assert.equal(ctx.poolSize, 1);
  assert.deepEqual([...ctx.usable], ["http://hk-01:8080"]);
  assert.equal(ctx.managed.size, 3);
});

test("account pinned to a healthy pool entry is bound", () => {
  const binding = resolveAccountProxyBinding(
    { proxy_url: "http://hk-01:8080" },
    ctxWith(),
  );
  assert.equal(binding.kind, "bound");
  assert.equal(binding.url, "http://hk-01:8080");
  assert.equal(binding.proxy?.label, "HK-01");
  assert.equal(binding.usable, true);
});

test("account pinned to a disabled or test-failed managed proxy is fail-closed", () => {
  for (const url of ["http://jp-02:8080", "http://sg-03:8080"]) {
    const binding = resolveAccountProxyBinding({ proxy_url: url }, ctxWith());
    assert.equal(binding.kind, "bound_unusable");
    assert.equal(binding.usable, false);
    // 徽章仍要拿得到那一行，才能在提示里说清楚是哪条代理挂了。
    assert.equal(binding.proxy?.url, url);
  }
});

test("pool disabled keeps a pinned unavailable proxy working — no false alarm", () => {
  const ctx = ctxWith({ poolEnabled: false });
  const binding = resolveAccountProxyBinding(
    { proxy_url: "http://jp-02:8080" },
    ctx,
  );
  assert.equal(binding.kind, "bound");
  assert.equal(binding.usable, true);
  assert.equal(isManagedProxyUnavailable("http://jp-02:8080", ctx), false);
});

test("a pinned URL outside the proxies table is custom and never fail-closed", () => {
  const binding = resolveAccountProxyBinding(
    { proxy_url: "socks5://127.0.0.1:1080" },
    ctxWith(),
  );
  assert.equal(binding.kind, "bound_custom");
  assert.equal(binding.proxy, null);
  assert.equal(binding.usable, true);
});

test("account proxy is matched by trim only, never normalized", () => {
  const ctx = ctxWith();
  assert.equal(
    resolveAccountProxyBinding({ proxy_url: "  http://hk-01:8080  " }, ctx).kind,
    "bound",
  );
  // 末尾斜杠在后端是另一个 key，这里必须落到 custom 而不是被"修好"。
  assert.equal(
    resolveAccountProxyBinding({ proxy_url: "http://hk-01:8080/" }, ctx).kind,
    "bound_custom",
  );
});

test("unpinned account inherits the first group that still has a usable proxy", () => {
  const binding = resolveAccountProxyBinding(
    { proxy_url: "", group_ids: [11, 10] },
    ctxWith(),
  );
  assert.equal(binding.kind, "group");
  assert.equal(binding.groupName, "主力");
  // 继承态不报具体 URL：池内/组内选路是粘性散列的，前端复刻必漂移。
  assert.equal(binding.url, "");
});

test("a group whose proxies are all unavailable is skipped, not reported", () => {
  const binding = resolveAccountProxyBinding(
    { group_ids: [12, 10] },
    ctxWith(),
  );
  assert.equal(binding.kind, "group");
  assert.equal(binding.groupName, "主力");

  // 只有全挂的组时继续回落到代理池。
  const fallback = resolveAccountProxyBinding({ group_ids: [12] }, ctxWith());
  assert.equal(fallback.kind, "pool");
});

test("unknown group ids and missing group data fall through", () => {
  const binding = resolveAccountProxyBinding({ group_ids: [999] }, ctxWith());
  assert.equal(binding.kind, "pool");
});

test("fallback order is pool then global then direct", () => {
  assert.equal(resolveAccountProxyBinding({}, ctxWith()).kind, "pool");

  const noPool = ctxWith({ proxies: [], globalProxy: "http://global:9090" });
  assert.equal(resolveAccountProxyBinding({}, noPool).kind, "global");

  const poolOff = ctxWith({ poolEnabled: false, proxies: [healthy] });
  assert.equal(resolveAccountProxyBinding({}, poolOff).kind, "direct");
  assert.equal(resolveAccountProxyBinding({}, poolOff).usable, true);
});

test("pool enabled with nothing usable and no global proxy blocks direct egress", () => {
  const ctx = ctxWith({ proxies: [disabled, failed], globalProxy: "" });
  const binding = resolveAccountProxyBinding({}, ctx);
  assert.equal(binding.kind, "direct_blocked");
  assert.equal(binding.usable, false);
});

test("null account resolves without throwing", () => {
  assert.equal(resolveAccountProxyBinding(null, ctxWith()).kind, "pool");
  assert.equal(resolveAccountProxyBinding(undefined, ctxWith()).kind, "pool");
});

test("short name prefers label, then host, then the raw string", () => {
  assert.equal(formatProxyShortName(healthy, healthy.url), "HK-01");
  assert.equal(
    formatProxyShortName(null, "http://user:pass@hk-01:8080"),
    "hk-01:8080",
  );
  assert.equal(formatProxyShortName(null, "not a url"), "not a url");
  assert.equal(formatProxyShortName(null, ""), "");
});

test("tooltip URLs keep scheme/host but never leak credentials", () => {
  assert.equal(
    formatProxyDisplayURL("socks5h://user:pass@66.253.7.45:10000"),
    "socks5h://***:***@66.253.7.45:10000",
  );
  // 无凭据的原样返回，别把能直接认出来的 URL 也糊掉。
  assert.equal(
    formatProxyDisplayURL("http://hk-01:8080"),
    "http://hk-01:8080",
  );
  assert.equal(formatProxyDisplayURL("  http://hk-01:8080  "), "http://hk-01:8080");
  assert.equal(formatProxyDisplayURL("not a url"), "not a url");
  assert.equal(formatProxyDisplayURL(""), "");
});

test("egress summary only shows for a successful test", () => {
  assert.equal(formatProxyEgressSummary(healthy), "1.2.3.4 · 香港 · 82ms");
  assert.equal(formatProxyEgressSummary(failed), "");
  assert.equal(formatProxyEgressSummary(null), "");
});
