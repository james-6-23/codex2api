import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))
const en = JSON.parse(readFileSync(new URL('../locales/en.json', import.meta.url), 'utf8'))

const TABS = ['codex', 'claude', 'antigravity', 'grok', 'appearance', 'general']

test('settings page is split into one panel per tab driven by ?tab=', () => {
  assert.match(settings, /useSearchParams\(\)/)
  assert.match(settings, /searchParams\.get\('tab'\)/)
  assert.match(settings, /type SettingsTabKey = 'codex' \| 'claude' \| 'antigravity' \| 'grok' \| 'appearance' \| 'general'/)
  for (const tab of TABS) {
    assert.match(settings, new RegExp(`\\{ id: '${tab}', label: t\\('settings\\.nav\\.${tab}'\\)`), `tab pill ${tab}`)
    assert.match(settings, new RegExp(`\\{activeTab === '${tab}' \\? \\(`), `panel ${tab}`)
  }
  // 旧滚动定位导航已移除，避免回退成单页长滚动。
  assert.doesNotMatch(settings, /scrollToSection/)
  assert.doesNotMatch(settings, /settingsSections/)
  assert.match(settings, /role="tablist"/)
  assert.match(settings, /role="tab"/)
})

test('legacy #settings-* anchors map onto a tab', () => {
  for (const id of ['settings-overview', 'settings-traffic', 'settings-runtime', 'settings-models', 'settings-grok', 'settings-claude', 'settings-antigravity', 'settings-appearance']) {
    assert.match(settings, new RegExp(`'${id}': '(${TABS.join('|')})'`), id)
  }
})

test('channel-specific cards live in their channel tab, shared cards in general', () => {
  const panel = (tab) => {
    const start = settings.indexOf(`{activeTab === '${tab}' ? (`)
    const next = TABS.map((t) => settings.indexOf(`{activeTab === '${t}' ? (`)).filter((i) => i > start)
    const end = next.length ? Math.min(...next) : settings.length
    assert.ok(start > 0, `panel ${tab}`)
    return settings.slice(start, end)
  }
  const codex = panel('codex')
  for (const key of ['settings.probeScheduling', 'settings.globalAutoPauseTitle', 'settings.codexWebsocket', 'settings.codexCompatToggles', 'settings.codexOverloadPause', 'settings.responseCache.title', 'settings.codexClientTitle', 'settings2.codexModelMapping']) {
    assert.ok(codex.includes(key), `codex tab should contain ${key}`)
  }
  assert.ok(codex.includes('codex_user_agent_config') || codex.includes('codexUserAgentConfig'))
  assert.ok(!codex.includes('settings.usageLogMode'), 'usage log settings are shared, not Codex-only')
  assert.ok(panel('claude').includes('<ClaudeCodeSettingsCard />'))
  assert.ok(panel('antigravity').includes('settings.antigravityOAuth.title'))
  assert.ok(panel('grok').includes('settings.grokSettingsTitle'))
  const appearance = panel('appearance')
  assert.ok(appearance.includes('settings.display') && appearance.includes('settings.backgroundImage'))
  const general = panel('general')
  for (const key of ['settings.systemStatus', 'settings.visibleChannelsTitle', 'settings.trafficProtection', 'settings.modelCooldownTitle', 'settings.schedulingStrategy', 'settings.runtimeOptimization', 'settings.usageLogMode', 'settings.githubAccess', 'settings.imageStorage', 'settings.security', 'settings.apiEndpoints']) {
    assert.ok(general.includes(key), `general tab should contain ${key}`)
  }
  assert.ok(!general.includes('settings.codexUserAgentRaw'), 'Codex UA emulation must not stay in general runtime card')
})

test('tab and section labels exist in zh and en', () => {
  for (const locale of [zh, en]) {
    for (const key of ['codex', 'codexDesc', 'claude', 'antigravity', 'grok', 'appearance', 'general', 'generalDesc', 'codexQuota', 'codexQuotaDesc', 'codexTransport', 'codexTransportDesc', 'codexClient', 'codexClientDesc']) {
      assert.equal(typeof locale.settings?.nav?.[key], 'string', `settings.nav.${key}`)
    }
    assert.equal(typeof locale.settings?.codexClientTitle, 'string')
    assert.equal(typeof locale.settings?.codexClientDesc, 'string')
  }
})

test('codex tab points at the shared scheduling strategy in general', () => {
  const start = settings.indexOf("{activeTab === 'codex' ? (")
  const end = settings.indexOf("{activeTab === 'claude' ? (")
  const codex = settings.slice(start, end)
  assert.match(codex, /selectTab\('general', 'settings-traffic'\)/)
  assert.match(codex, /settings\.codexSchedulingHintAction/)
  assert.match(settings, /pendingSectionRef/)
  for (const locale of [zh, en]) {
    for (const key of ['codexSchedulingHintTitle', 'codexSchedulingHintDesc', 'codexSchedulingHintAction']) {
      assert.equal(typeof locale.settings?.[key], 'string', `settings.${key}`)
    }
    assert.match(locale.settings.schedulerModeDesc, /Grok/)
    assert.match(locale.settings.schedulerModeDesc, /Antigravity/)
  }
})

test('shared settings cards declare which upstream channels they apply to', () => {
  const badges = readFileSync(new URL('../components/ChannelScopeBadges.tsx', import.meta.url), 'utf8')
  assert.match(badges, /export const ALL_UPSTREAM_CHANNELS/)
  assert.match(badges, /data-channel-scope/)
  assert.match(settings, /channels\?: readonly UpstreamChannel\[\]/)
  // 通用 Tab 里每张跨渠道卡片都必须带 channels，避免再出现"看不出给谁用"的设置。
  for (const title of ['settings.trafficProtection', 'settings.schedulingStrategy', 'settings.runtimeOptimization', 'settings.autoCleanup']) {
    assert.match(settings, new RegExp(`title=\\{t\\('${title.replace('.', '\\.')}'\\)\\}[^\\n]*channels=\\{ALL_UPSTREAM_CHANNELS\\}`), title)
  }
  assert.match(settings, /title=\{t\('settings\.continuousRetryTitle'\)\}\n\s+channels=\{ALL_UPSTREAM_CHANNELS\}/)
  assert.match(settings, /title=\{t\('settings\.modelCooldownTitle'\)\}\n\s+channels=\{ALL_UPSTREAM_CHANNELS\}/)
  assert.match(settings, /settings\.imageStorage'\)\}[^\n]*channels=\{CHANNELS_CODEX_ONLY\}/)
  assert.match(settings, /settings\.globalAutoPauseTitle'\)\}[^\n]*channels=\{CHANNELS_CODEX_CLAUDE\}/)
  assert.match(settings, /settings\.billingTierPolicy'\)\}[^\n]*channels=\{CHANNELS_CODEX_ONLY\}/)
  assert.match(settings, /settings\.streamFlushPolicy'\)\}[^\n]*channels=\{CHANNELS_STREAMING\}/)
  assert.match(settings, /value: 'response_failed', channels: CHANNELS_CODEX_ONLY/)
  assert.match(settings, /<ChannelScopeBadges channels=\{CHANNELS_RELAY\} size="xs" \/>/)
  for (const locale of [zh, en]) {
    assert.match(locale.settings.channelScope, /\{\{channels\}\}/)
    assert.equal(typeof locale.settings.channelScopeAll, 'string')
  }
})

test('multi-section tabs render a section index that mirrors the rendered sections', () => {
  assert.match(settings, /const SETTINGS_TAB_SECTION_INDEX: Record<SettingsTabKey/)
  assert.match(settings, /function useActiveSettingsSection\(/)
  assert.match(settings, /<SettingsSectionIndex\n/)
  const start = settings.indexOf('const SETTINGS_TAB_SECTION_INDEX')
  const end = settings.indexOf('\n}\n', start)
  const index = settings.slice(start, end)
  for (const id of index.match(/id: '(settings-[a-z-]+)'/g).map((m) => m.slice(5, -1))) {
    assert.match(settings, new RegExp(`<SettingsSection id="${id}"`), `section index entry ${id} must point at a rendered section`)
  }
  for (const tab of TABS) {
    assert.match(index, new RegExp(`\\b${tab}: \\[`), `section index for ${tab}`)
  }
  // Tab 栏跟随页面流粘顶，不再 fixed 悬浮盖住内容。
  assert.doesNotMatch(settings, /fixed left-1\/2 top-\[max\(0\.625rem/)
  assert.match(settings, /sticky top-2[\s\S]{0,400}role="tablist"/)
})

test('manual-save fields are tracked against the persisted snapshot', () => {
  assert.match(settings, /const \[persistedSettings, setPersistedSettings\] = useState<SystemSettings \| null>/)
  assert.match(settings, /setPersistedSettings\(commitSettingsForm\(settings\)\)/, 'load must seed the snapshot')
  assert.match(settings, /setPersistedSettings\(commitSettingsForm\(updated\)\)/, 'manual save must refresh the snapshot')
  assert.match(settings, /markPersisted\(getSettingsPatchValues\(optimistic, patchKeys\)\)/, 'auto-save must merge only its own keys')
  assert.match(settings, /\{dirtyCount > 0 \? \(/, 'bottom save bar only renders with unsaved changes')
  assert.match(settings, /<SaveStatusPill autoSaveStatus=\{autoSaveStatus\} dirtyCount=\{dirtyCount\} \/>/)
  for (const locale of [zh, en]) {
    for (const key of ['saveStatusSaved', 'saveStatusUnsaved', 'saveStatusUnsavedHint', 'discardChanges', 'sectionIndex']) {
      assert.equal(typeof locale.settings?.[key], 'string', `settings.${key}`)
    }
    assert.match(locale.settings.saveStatusUnsaved, /\{\{n\}\}/)
  }
})

test('single-toggle compatibility settings are one row-list card, not three narrow cards', () => {
  const start = settings.indexOf("title={t('settings.codexCompatToggles')}")
  const end = settings.indexOf('</SettingsCard>', start)
  const card = settings.slice(start, end)
  assert.ok(start > 0)
  assert.match(card, /className=\{SETTINGS_ROW_LIST\}/)
  for (const key of ['overflowAutoCompact', 'compactViaResponses', 'codexPreflightSSEPassthrough']) {
    assert.match(card, new RegExp(`label=\\{t\\('settings\\.${key}'\\)\\}\\n\\s+description=\\{t\\('settings\\.${key}Desc'\\)\\}\\n\\s+help=\\{t\\('settings\\.${key}EnabledDesc'\\)\\}\\n\\s+layout="row"`), key)
  }
  // 双列开关栅格里只放一个开关会挤成半宽折行：逐个栅格数到同缩进的 </div> 为止。
  const lines = settings.split('\n')
  lines.forEach((line, i) => {
    if (!line.includes('className={SETTINGS_SWITCH_GRID}')) return
    const indent = line.length - line.trimStart().length
    let switches = 0
    let fields = 0
    for (let j = i + 1; j < lines.length; j++) {
      const l = lines[j]
      if (l.trim() === '</div>' && l.length - l.trimStart().length === indent) break
      if (l.includes('<SettingField')) fields++
      if (l.includes('layout="switch"')) switches++
    }
    assert.ok(fields > 1, `line ${i + 1}: a lone switch must use SETTINGS_SWITCH_ROW, not SETTINGS_SWITCH_GRID (${switches} switch, ${fields} field)`)
  })
  for (const locale of [zh, en]) {
    assert.equal(typeof locale.settings.codexCompatToggles, 'string')
    assert.equal(typeof locale.settings.codexCompatTogglesDesc, 'string')
  }
})

test('provider visibility picker lives in general and keeps the fallback channel locked', () => {
  assert.match(settings, /<VisibleChannelsPicker \/>/)
  assert.match(settings, /if \(channel === FALLBACK_VISIBLE_CHANNEL \|\| saving\) return/)
  for (const locale of [zh, en]) {
    for (const key of ['visibleChannelsTitle', 'visibleChannelsDesc', 'visibleChannelsFallbackHint', 'visibleChannelsSaveFailed']) {
      assert.equal(typeof locale.settings?.[key], 'string', `settings.${key}`)
    }
  }
})
