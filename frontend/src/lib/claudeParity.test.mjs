import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const channelFilter = readFileSync(new URL('../components/ChannelFilter.tsx', import.meta.url), 'utf8')
const dashboard = readFileSync(new URL('../pages/Dashboard.tsx', import.meta.url), 'utf8')
const usage = readFileSync(new URL('../pages/Usage.tsx', import.meta.url), 'utf8')
const apiKeys = readFileSync(new URL('../pages/APIKeys.tsx', import.meta.url), 'utf8')
const proxies = readFileSync(new URL('../pages/Proxies.tsx', import.meta.url), 'utf8')
const accountsPage = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')
const scheduler = readFileSync(new URL('../pages/SchedulerBoard.tsx', import.meta.url), 'utf8')
const claude = readFileSync(new URL('../pages/ClaudeAccounts.tsx', import.meta.url), 'utf8')
const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const docs = readFileSync(new URL('../pages/Docs.tsx', import.meta.url), 'utf8')
const guide = readFileSync(new URL('../pages/Guide.tsx', import.meta.url), 'utf8')
const apiReference = readFileSync(new URL('../pages/ApiReference.tsx', import.meta.url), 'utf8')
const docsContent = readFileSync(new URL('../pages/docs/docsContent.ts', import.meta.url), 'utf8')
const quickStartTools = readFileSync(new URL('../pages/docs/quickStartTools.ts', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const styles = readFileSync(new URL('../index.css', import.meta.url), 'utf8')
const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))

test('shared usage channel filter exposes Claude and persists it', () => {
  assert.match(channelFilter, /UsageChannel = "" \| "codex" \| "grok" \| "antigravity" \| "claude"/)
  assert.match(channelFilter, /raw === "claude"/)
  assert.match(channelFilter, /key: "claude"/)
  assert.match(channelFilter, /channel="claude"/)
})

test('dashboard renders Claude channel counters', () => {
  assert.match(dashboard, /'claude'/)
  assert.match(dashboard, /key === 'claude'/)
  assert.match(dashboard, /channel: key === 'claude' \? 'Claude'/)
})

test('usage and management filters keep Claude provider identity', () => {
  assert.match(usage, /log\.channel === 'claude'/)
  assert.match(usage, /channel === 'claude'/)
  assert.match(apiKeys, /claudeModelOptions/)
  assert.match(apiKeys, /key: "claude"/)
  assert.match(proxies, /BindKindFilter = "all" \| "codex" \| "grok" \| "claude"/)
  assert.match(proxies, /bindKindClaude/)
  assert.match(scheduler, /channel.*claude|claude.*channel/)
  assert.match(scheduler, /selectedAvailable|summary\?\.active/)
  assert.match(proxies, /\["claude", t\("proxies\.bindKindClaude"\)\]/)
  assert.match(accountsPage, /\["codex", "grok", "antigravity", "claude"\]/)
})

test('Claude rows expose sampling state and provider copy', () => {
  assert.match(claude, /usage_probe|usageProbe|sampled|unsampled/)
  assert.match(claude, /last.*sample|采样|sample/i)
  assert.match(claude, /getAccountStatusBadgeStatus\(acc\)/)
  assert.match(types, /claude/)
  assert.equal(typeof zh.claude?.samplingState, 'object')
})

test('Claude account list refreshes after asynchronous sampling without stale overwrites', () => {
  assert.match(claude, /reloadAbortRef/)
  assert.match(claude, /samplingPoll|sample.*poll/i)
  assert.match(claude, /claude_usage_probe_at/)
  assert.match(claude, /getAccountLiveState/)
  assert.match(claude, /AccountDetailSheet/)
  assert.match(claude, /onOpenDetail/)
  assert.match(claude, /<AccountDetailSheet/)
  assert.match(claude, /<ClaudeTestModal/)
})

test('Claude model whitelist stays provider-scoped and uses optimistic detail validation', () => {
  assert.match(claude, /CLAUDE_MODEL_ID_RE = \/\^claude-/)
  assert.match(claude, /api\.syncAccountModelsUpstream\(account\.id\)/)
  assert.match(claude, /api\.updateAccountModels\(account\.id, requested\)/)
  assert.match(claude, /latest\.updated_at !== baseUpdatedAt/)
  assert.match(claude, /latest\.claude_api !== true/)
  assert.match(claude, /modelsWhitelistConflict/)
  assert.equal(typeof zh.claude?.modelsWhitelistTitle, 'string')
})

test('Claude default refresh keeps deterministic account order', () => {
  assert.match(claude, /default:\s*\{\s*sort:\s*undefined,\s*order:\s*['"]asc['"]\s*\}/)
})

test('Claude security limits default to upstream-compatible unlimited mode', () => {
  const start = settings.indexOf('function ClaudeCodeSettingsCard')
  const end = settings.indexOf('\nfunction SettingsCard', start)
  assert.ok(start >= 0 && end > start)
  const card = settings.slice(start, end)
  assert.match(card, /maxOutputTokens, setMaxOutputTokens\] = useState\('0'\)/)
  assert.match(card, /max_output_tokens: Number\.isFinite\(maxOutputValue\)/)
  assert.match(card, /claudeUnlimitedPlaceholder/)
})

test('API key rows expose Prompt scope separately from NewAPI identity binding', () => {
  assert.match(apiKeys, /getPromptFilterNewAPIBindings/)
  assert.match(apiKeys, /promptBindings/)
  assert.match(apiKeys, /promptFilterScope|newapiPolicyStatus/i)
})

test('Claude disabled rows use the shared account-state table treatment', () => {
  assert.match(styles, /\.account-state-table-row > \[data-slot="table-cell"\],\s*\.account-state-table-row > td/)
  assert.match(styles, /\.account-state-table-row > \[data-slot="table-cell"\] > :not\(\.account-state-overlay--marker-only\),\s*\.account-state-table-row > td > :not\(\.account-state-overlay--marker-only\)/)
})

test('Claude detail metadata exposes safe operational fields without credentials', () => {
  const start = claude.indexOf('providerSlot={')
  const end = claude.indexOf('onClose={closeDetail}', start)
  assert.ok(start >= 0 && end > start)
  const providerSlot = claude.slice(start, end)
  assert.match(providerSlot, /plan_type/)
  assert.match(providerSlot, /subscription_expires_at/)
  assert.match(providerSlot, /claude_fingerprint_mode/)
  assert.match(providerSlot, /claude_usage_probe_at/)
  assert.doesNotMatch(providerSlot, /access_token|refresh_token|custom_headers|api_key/i)
})

test('Claude quota display keeps an unknown value distinct from zero', () => {
  assert.match(claude, /v === null \|\| v === undefined/)
  assert.match(claude, /claudeUsagePct\(v: unknown\): number \| null/)
})

test('Claude connection test waits for the SSE stream to close before refreshing', () => {
  assert.match(claude, /receivedTerminalEvent/)
  assert.match(claude, /Refresh only once the SSE stream has closed/)
  assert.match(claude, /onSettledRef/)
})

test('Claude documentation uses the current alias and real provider catalog', () => {
  for (const source of [docs, guide, apiReference, docsContent, quickStartTools]) {
    assert.doesNotMatch(source, /claude-sonnet-4-5-20250514/)
  }
  assert.match(docs, /claude_models/)
  assert.match(guide, /claude_models/)
  assert.match(docsContent, /channel=codex\|grok\|antigravity\|claude/)
})

test('Claude admin API reference covers import, sampling, probing, and config controls', () => {
  for (const id of [
    'claude-management',
    'claude-import',
    'claude-refresh-usage',
    'claude-probe-models',
    'claude-update-models',
    'claude-update-config',
  ]) {
    assert.match(apiReference, new RegExp(`id=["']${id}["']`))
  }
  assert.match(apiReference, /X-Admin-Key/)
  assert.match(apiReference, /Claude \/ Anthropic 管理 API/)
  assert.match(apiReference, /Messages API/)
})
