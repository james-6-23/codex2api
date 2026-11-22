import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { runInNewContext } from "node:vm";
import ts from "typescript";

const apiKeys = readFileSync(
  new URL("../pages/APIKeys.tsx", import.meta.url),
  "utf8",
);
const accounts = readFileSync(
  new URL("../pages/Accounts.tsx", import.meta.url),
  "utf8",
);
const types = readFileSync(new URL("../types.ts", import.meta.url), "utf8");
// 测连弹窗已从 Accounts.tsx 抽成四渠道共用组件。
const testModal = readFileSync(
  new URL("../components/TestConnectionModal.tsx", import.meta.url),
  "utf8",
);
const detailSheet = readFileSync(
  new URL("../components/AccountDetailSheet.tsx", import.meta.url),
  "utf8",
);

test("Claude API key fallback models use the native provider aliases", () => {
  assert.match(
    apiKeys,
    /const DEFAULT_CLAUDE_MODEL_OPTIONS = \[\s*"claude-opus-4-5",\s*"claude-sonnet-4-5",\s*"claude-haiku-4-5",\s*\]/,
  );
});

test("Claude API key plan allowlist is isolated from Codex plans", () => {
  // Execute the provider selector without loading the page's React dependencies.
  const source = ts.createSourceFile(
    "APIKeys.tsx",
    apiKeys,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX,
  );
  const declarations = new Set([
    "CODEX_PLAN_FILTER_OPTIONS",
    "GROK_PLAN_FILTER_OPTIONS",
    "CLAUDE_PLAN_FILTER_OPTIONS",
    "ANTIGRAVITY_PLAN_FILTER_OPTIONS",
    "PLAN_FILTER_OPTIONS",
  ]);
  const statements = source.statements.filter((statement) =>
    ts.isVariableStatement(statement)
      ? statement.declarationList.declarations.some((declaration) =>
          declarations.has(declaration.name.getText(source)),
        )
      : ts.isFunctionDeclaration(statement) &&
        statement.name?.text === "planOptionsForChannel",
  );
  const { outputText } = ts.transpileModule(
    statements.map((statement) => statement.getText(source)).join("\n"),
    { compilerOptions: { target: ts.ScriptTarget.ES2020 } },
  );
  const selectPlans = runInNewContext(`${outputText}\nplanOptionsForChannel;`);
  const plans = (channel) => Array.from(selectPlans(channel));

  assert.deepEqual(plans("claude"), [
    "claude", "free", "pro", "max", "max-5x", "max-20x", "team",
    "enterprise", "business",
  ]);
  assert.deepEqual(plans("codex"), [
    "free", "plus", "pro", "promax", "prolite", "team", "k12", "go",
  ]);
  assert.deepEqual(plans("antigravity"), [
    "free", "pro", "ultra", "standard", "api",
  ]);

  const automatic = plans("auto");
  const providerUnion = new Set(
    ["codex", "grok", "claude", "antigravity"].flatMap(plans),
  );
  assert.equal(automatic.length, new Set(automatic).size);
  assert.deepEqual(new Set(automatic), providerUnion);
});

test("recycle-bin account projection preserves Claude provider identity", () => {
  assert.match(types, /export interface RecycleBinAccountRow[\s\S]*claude_api\?: boolean/);
  assert.match(
    accounts,
    /claude_api:\s*row\.claude_api/,
  );
});

test("shared connection test modal selects Claude native models", () => {
  assert.match(accounts, /<TestConnectionModal/);
  assert.match(testModal, /isClaudeAccount/);
  assert.match(testModal, /account\.claude_api/);
  assert.match(testModal, /claude-opus-4-5|claude-sonnet-4-5/);
});

test("shared account detail sheet keeps Claude out of Codex-only actions", () => {
  assert.match(detailSheet, /claude_api/);
  assert.match(detailSheet, /isClaude/);
});
