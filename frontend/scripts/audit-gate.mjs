#!/usr/bin/env node
// 前端依赖漏洞门禁:等价于 `npm audit --omit=dev --audit-level=high`,
// 但支持逐条 advisory 的例外白名单——npm audit 本身没有这个能力,
// 只能整体降低 --audit-level,那会把真实的 high 一起放过去。
//
// 白名单里的每一条都必须写明「为什么本项目不受影响」和复查时间。
// 白名单只对 high/critical 生效,其余 severity 本来就不拦。
// 拿不到审计结果(网络/解析失败)时一律失败,避免门禁静默失效。

import { spawnSync } from "node:child_process";

/**
 * 已评估的例外:GHSA id → 理由(含复查时间)。
 * 加入前先确认该漏洞的触发路径在本项目里不存在,而不是"暂时没人利用"。
 */
const ALLOWLIST = new Map([
  // GHSA-qwww-vcr4-c8h2(React Router RSC CSRF)已随 react-router 7.18.2 修复,
  // 条目于 2026-08-08 移除;新增放行前先确认触发路径在本项目里不存在。
]);

const BLOCKING_SEVERITIES = new Set(["high", "critical"]);

const npmCommand = process.platform === "win32" ? "npm.cmd" : "npm";
const result = spawnSync(npmCommand, ["audit", "--omit=dev", "--json"], {
  encoding: "utf8",
  maxBuffer: 32 * 1024 * 1024,
  // Windows 上 npm 是 .cmd，需要经 cmd.exe 启动。
  shell: process.platform === "win32",
});

// npm audit 检出漏洞时退出码非 0,这属正常输出而非执行失败,只有拿不到 stdout 才是真失败。
if (result.error || !result.stdout) {
  console.error("audit-gate: 无法获取 npm audit 结果");
  if (result.error) console.error(result.error.message);
  if (result.stderr) console.error(result.stderr);
  process.exit(1);
}

let report;
try {
  report = JSON.parse(result.stdout);
} catch (err) {
  console.error(`audit-gate: 解析 npm audit JSON 失败:${err.message}`);
  process.exit(1);
}

/** 从 advisory url 里取 GHSA id,取不到时退回 npm 的数字 source id。 */
function advisoryID(via) {
  const matched = /GHSA-[0-9a-z-]+/i.exec(via.url ?? "");
  return matched ? matched[0] : String(via.source ?? "unknown");
}

// vulnerabilities[pkg].via 里既有 advisory 对象,也有指向其它包名的字符串(传递依赖),
// 后者的 advisory 会在其根包上出现,这里只收对象形态并按 id 去重。
const advisories = new Map();
for (const vuln of Object.values(report.vulnerabilities ?? {})) {
  for (const via of vuln.via ?? []) {
    if (typeof via !== "object" || !BLOCKING_SEVERITIES.has(via.severity)) continue;
    const id = advisoryID(via);
    if (!advisories.has(id)) {
      advisories.set(id, {
        id,
        title: via.title ?? "(no title)",
        pkg: via.name ?? vuln.name ?? "(unknown)",
        range: via.range ?? "(unknown)",
        url: via.url ?? "",
      });
    }
  }
}

const blocking = [];
const allowed = [];
for (const advisory of advisories.values()) {
  (ALLOWLIST.has(advisory.id) ? allowed : blocking).push(advisory);
}

for (const advisory of allowed) {
  console.log(`已放行 ${advisory.id} [${advisory.pkg} ${advisory.range}] ${advisory.title}`);
  console.log(`         理由:${ALLOWLIST.get(advisory.id)}`);
}

// 白名单条目对应的漏洞已消失(依赖升级了/advisory 撤销了)→ 提醒删掉,防止白名单腐化。
const stale = [...ALLOWLIST.keys()].filter((id) => !advisories.has(id));
for (const id of stale) {
  console.log(`白名单条目 ${id} 已不再命中,可从 audit-gate.mjs 移除`);
}

if (blocking.length === 0) {
  console.log(`audit-gate: 通过(0 条待处理 high/critical,${allowed.length} 条已放行)`);
  process.exit(0);
}

console.error(`\naudit-gate: 发现 ${blocking.length} 条未放行的 high/critical 漏洞:`);
for (const advisory of blocking) {
  console.error(`  ${advisory.id} [${advisory.pkg} ${advisory.range}] ${advisory.title}`);
  if (advisory.url) console.error(`    ${advisory.url}`);
}
console.error(
  "\n先尝试 `npm audit fix` 升级;确认本项目不受影响时,再把 GHSA id 与理由加进 audit-gate.mjs 的 ALLOWLIST。",
);
process.exit(1);
