import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  accountStateSurfaceClass,
  accountStateTableRowClass,
  disabledAccountSurfaceClass,
  disabledAccountTableRowClass,
  isDisabledAccountOverlayAccount,
  resolveAccountOverlayKind,
} from "./accountStateOverlay.ts";

function readSource(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8").replace(/\r\n/g, "\n");
}

function sourceSlice(source, startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  assert.notEqual(start, -1, `missing source marker: ${startMarker}`);
  const end = source.indexOf(endMarker, start + startMarker.length);
  assert.notEqual(end, -1, `missing source marker: ${endMarker}`);
  return source.slice(start, end);
}

function sourceTail(source, startMarker) {
  const start = source.indexOf(startMarker);
  assert.notEqual(start, -1, `missing source marker: ${startMarker}`);
  return source.slice(start);
}

function occurrenceCount(source, value) {
  return source.split(value).length - 1;
}

test("account state helpers preserve disabled precedence and disabled-only behavior", () => {
  for (const [account, kind] of [
    [{ enabled: false, status: "overload_paused" }, "disabled"],
    [{ enabled: false, status: "active" }, "disabled"],
    [{ enabled: true, status: "overload_paused" }, "overload"],
    [{ enabled: true, status: "active" }, null],
  ]) {
    const context = JSON.stringify(account);
    const disabled = kind === "disabled";
    const surface = kind ? " account-state-surface" : "";
    const row = kind ? ` account-state-table-row account-state-table-row--${kind}` : "";
    assert.equal(resolveAccountOverlayKind(account), kind, context);
    assert.equal(isDisabledAccountOverlayAccount(account), disabled, context);
    assert.equal(accountStateSurfaceClass(account), surface, context);
    assert.equal(accountStateTableRowClass(account), row, context);
    assert.equal(disabledAccountSurfaceClass(account), disabled ? surface : "", context);
    assert.equal(disabledAccountTableRowClass(account), disabled ? row : "", context);
  }
});

test("table markers replace status content without entering selection cells", () => {
  const accountsRow = sourceSlice(
    readSource("../pages/Accounts.tsx"),
    "const AccountTableRow = memo(function AccountTableRow(",
    "// AccountMobileCard",
  );
  const grokRow = sourceSlice(
    readSource("../pages/GrokAccounts.tsx"),
    "function GrokAccountTableRow(",
    "function grokFormatDollars",
  );

  const accountsSelectionCell = sourceSlice(
    accountsRow,
    "<TableCell>",
    "{visibleColumns.sequence",
  );
  const accountsStatusCell = sourceSlice(
    accountsRow,
    '<TableCell data-account-state-cell="status">',
    "{visibleColumns.today",
  );
  const grokSelectionCell = sourceSlice(
    grokRow,
    '<TableCell className="w-9">',
    '<TableCell className="font-mono',
  );
  const grokStatusCell = sourceSlice(
    grokRow,
    '<TableCell data-account-state-cell="status">',
    "<RequestCountPills account={account} compact />",
  );

  assert.match(accountsRow, /accountStateTableRowClass\(account\)/);
  assert.match(grokRow, /disabledAccountTableRowClass\(account\)/);
  assert.match(
    accountsRow,
    /const tableOverlay = renderAccountStateOverlay\(account, t, \{[\s\S]*?markerOnly: true,/,
  );
  assert.doesNotMatch(accountsRow, /\brenderDisabledAccountOverlay\(/);
  assert.match(
    grokRow,
    /const tableOverlay = renderDisabledAccountOverlay\(account, t, \{[\s\S]*?markerOnly: true,/,
  );
  assert.doesNotMatch(grokRow, /\brenderAccountStateOverlay\(/);

  for (const [row, selectionCell, statusCell] of [
    [accountsRow, accountsSelectionCell, accountsStatusCell],
    [grokRow, grokSelectionCell, grokStatusCell],
  ]) {
    assert.doesNotMatch(selectionCell, /\{tableOverlay\b/);
    assert.match(statusCell, /\{tableOverlay \?\? \(/);
    assert.match(statusCell, /<AccountHealthBar/);
    assert.equal(occurrenceCount(row, "{tableOverlay ?? ("), 1);
  }
  assert.match(accountsSelectionCell, /tableOverlayKind/);
  assert.match(accountsSelectionCell, /className="sr-only"/);
  assert.match(accountsSelectionCell, /actions\.resetStatus\(account\)/);
});

test("table styling has no positioned scrim on internal table boxes", () => {
  const cssSource = readSource("../index.css");
  const tableRules = Array.from(
    cssSource.matchAll(/[^{}]*\.account-state-table-row[^{}]*\{[^{}]*\}/g),
    (match) => match[0],
  );
  const tableCss = tableRules.join("\n");

  assert.ok(tableRules.length >= 6, "expected account table state rules");
  assert.match(tableCss, /background-image:/);
  assert.match(tableCss, /:not\(\.account-state-overlay--marker-only\)/);
  assert.doesNotMatch(tableCss, /::(?:before|after)/);
  assert.doesNotMatch(tableCss, /\bposition\s*:/);
  assert.doesNotMatch(tableCss, /\binset\s*:/);
});

test("marker-only rendering stays in normal flow and is passed through", () => {
  const componentSource = readSource("../components/AccountStateOverlay.tsx");
  const overlayComponent = sourceSlice(
    componentSource,
    "export function AccountStateOverlay(",
    "export function renderAccountStateOverlay(",
  );
  const accountRenderer = sourceSlice(
    componentSource,
    "export function renderAccountStateOverlay(",
    "export function renderDisabledAccountOverlay(",
  );
  const disabledRenderer = sourceTail(
    componentSource,
    "export function renderDisabledAccountOverlay(",
  );

  assert.match(
    overlayComponent,
    /markerOnly\s*\?\s*"account-state-overlay--marker-only w-full"\s*:\s*"absolute inset-0/,
  );
  assert.match(
    overlayComponent,
    /markerOnly\s*\?\s*"account-state-overlay__mark--inline[^\"]*"\s*:\s*"absolute inset-0/,
  );
  assert.match(
    overlayComponent,
    /aria-hidden=\{markerOnly \? undefined : false\}/,
  );
  assert.equal(occurrenceCount(accountRenderer, "markerOnly={options.markerOnly}"), 1);
  assert.equal(occurrenceCount(disabledRenderer, "markerOnly={options.markerOnly}"), 1);
  assert.match(
    disabledRenderer,
    /if \(!isDisabledAccountOverlayAccount\(account\)\) return null;/,
  );
});

test("pull request CI runs frontend regression tests", () => {
  const frontendJob = sourceSlice(
    readSource("../../../.github/workflows/pr-check.yml"),
    "  frontend:\n",
    "  golangci-lint:\n",
  );

  assert.match(
    frontendJob,
    /- name: Test frontend\s+working-directory: frontend\s+run: npm test/,
  );
});
