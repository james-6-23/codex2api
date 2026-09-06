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

test("account overlay kind keeps disabled precedence over overload", () => {
  assert.equal(
    resolveAccountOverlayKind({ enabled: false, status: "overload_paused" }),
    "disabled",
  );
  assert.equal(
    resolveAccountOverlayKind({ enabled: true, status: "overload_paused" }),
    "overload",
  );
  assert.equal(resolveAccountOverlayKind({ enabled: true, status: "active" }), null);
});

test("disabled-only helpers reject active and overload accounts", () => {
  const disabled = { enabled: false, status: "active" };
  const overload = { enabled: true, status: "overload_paused" };
  const active = { enabled: true, status: "active" };

  assert.equal(isDisabledAccountOverlayAccount(disabled), true);
  assert.equal(isDisabledAccountOverlayAccount(overload), false);
  assert.equal(isDisabledAccountOverlayAccount(active), false);

  assert.equal(accountStateSurfaceClass(disabled), " account-state-surface");
  assert.equal(disabledAccountSurfaceClass(disabled), " account-state-surface");
  assert.equal(disabledAccountSurfaceClass(overload), "");
  assert.equal(disabledAccountSurfaceClass(active), "");

  assert.equal(
    accountStateTableRowClass(disabled),
    " account-state-table-row account-state-table-row--disabled",
  );
  assert.equal(
    accountStateTableRowClass(overload),
    " account-state-table-row account-state-table-row--overload",
  );
  assert.equal(
    disabledAccountTableRowClass(disabled),
    " account-state-table-row account-state-table-row--disabled",
  );
  assert.equal(disabledAccountTableRowClass(overload), "");
  assert.equal(disabledAccountTableRowClass(active), "");
});

test("table markers replace status content without entering selection cells", () => {
  const accountsSource = readFileSync(
    new URL("../pages/Accounts.tsx", import.meta.url),
    "utf8",
  );
  const grokSource = readFileSync(
    new URL("../pages/GrokAccounts.tsx", import.meta.url),
    "utf8",
  );

  const accountsRow = sourceSlice(
    accountsSource,
    "const AccountTableRow = memo(function AccountTableRow(",
    "// AccountMobileCard",
  );
  const grokRow = sourceSlice(
    grokSource,
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

  assert.doesNotMatch(accountsSelectionCell, /\{tableOverlay\b/);
  assert.doesNotMatch(grokSelectionCell, /\{tableOverlay\b/);
  assert.match(accountsSelectionCell, /tableOverlayKind/);
  assert.match(accountsSelectionCell, /className="sr-only"/);
  assert.match(accountsSelectionCell, /actions\.resetStatus\(account\)/);
  assert.match(accountsStatusCell, /\{tableOverlay \?\? \(/);
  assert.match(grokStatusCell, /\{tableOverlay \?\? \(/);
  assert.match(accountsStatusCell, /<AccountHealthBar/);
  assert.match(grokStatusCell, /<AccountHealthBar/);
  assert.equal(occurrenceCount(accountsRow, "{tableOverlay ?? ("), 1);
  assert.equal(occurrenceCount(grokRow, "{tableOverlay ?? ("), 1);
});

test("table styling has no positioned scrim on internal table boxes", () => {
  const cssSource = readFileSync(new URL("../index.css", import.meta.url), "utf8");
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
  const componentSource = readFileSync(
    new URL("../components/AccountStateOverlay.tsx", import.meta.url),
    "utf8",
  );
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
  const workflowSource = readFileSync(
    new URL("../../../.github/workflows/pr-check.yml", import.meta.url),
    "utf8",
  );
  const frontendJob = sourceSlice(
    workflowSource.replace(/\r\n/g, "\n"),
    "  frontend:\n",
    "  golangci-lint:\n",
  );

  assert.match(
    frontendJob,
    /- name: Test frontend\s+working-directory: frontend\s+run: npm test/,
  );
});
