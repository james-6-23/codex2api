import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')
const page = read('../pages/KiroAccounts.tsx')
const settings = read('../pages/Settings.tsx')
const api = read('../api.ts')
const locales = ['zh', 'en', 'zh-TW'].map((name) => [name, JSON.parse(read(`../locales/${name}.json`))])

test('Kiro accounts page uses the shared controls and Kiro admin APIs', () => {
  for (const call of ['getAccountsPage', 'importKiroAccounts', 'exportKiroAccounts', 'startKiroSocialLogin', 'finishKiroSocialLogin', 'startKiroDeviceLogin', 'pollKiroDeviceLogin', 'syncKiroAccount', 'syncAllKiroAccounts']) {
    assert.match(page, new RegExp(`api\\.${call}\\(`), call)
    assert.match(api, new RegExp(`\\b${call}:`), `api.ts defines ${call}`)
  }
  assert.match(page, /channel: "kiro"/)
  assert.match(page, /<TestConnectionModal/)
  assert.match(page, /from "\.\.\/components\/ui\/select"/)
  assert.doesNotMatch(page, /<select/)
  assert.match(read('../pages/Accounts.tsx'), /<KiroAccounts headerSlot=\{providerSwitcher\} \/>/)
  assert.match(read('../App.tsx'), /path="\/accounts\/kiro"/)
})

test('Kiro settings card saves through the dedicated settings API', () => {
  assert.match(settings, /function KiroSettingsCard\(/)
  assert.match(settings, /api\.getKiroSettings\(\)/)
  assert.match(settings, /api\.updateKiroSettings\(patch\)/)
  assert.match(settings, /<SegmentedPillGroup[\s\S]*?save\(\{ api_domain: value \}\)/)
  assert.match(settings, /onCheckedChange=\{\(checked\) => void save\(\{ cache_simulation: checked \}\)\}/)
})

test('Kiro is a first-class channel in the shared enumerations', () => {
  assert.match(read('../types.ts'), /export type UpstreamChannel = .*'kiro'/)
  assert.match(read('../components/ChannelLogo.tsx'), /kiro-color\.svg/)
  assert.match(read('../components/ChannelFilter.tsx'), /key: "kiro"/)
  assert.match(read('./visibleChannels.ts'), /'kiro'\]/)
})

test('Kiro copy exists in every locale', () => {
  for (const [name, locale] of locales) {
    for (const key of ['pageTitle', 'importHint', 'socialHint', 'deviceHint', 'syncAll', 'exportKam', 'deleteDescription']) {
      assert.equal(typeof locale.kiro?.[key], 'string', `${name} kiro.${key}`)
    }
    for (const key of ['title', 'apiDomain', 'cacheSimulation', 'cacheSimulationDesc', 'saved']) {
      assert.equal(typeof locale.settings?.kiro?.[key], 'string', `${name} settings.kiro.${key}`)
    }
    assert.equal(typeof locale.accounts?.providerViewKiro, 'string', `${name} accounts.providerViewKiro`)
    assert.equal(typeof locale.apiKeys?.limits?.upstreamChannelKiro, 'string', `${name} apiKeys.limits.upstreamChannelKiro`)
  }
})

test('connection test modal uses the Kiro catalog instead of the Codex model list', () => {
  const modal = read('../components/TestConnectionModal.tsx')
  assert.match(modal, /const isKiroAccount = Boolean\(account\.kiro_api\)/)
  assert.match(modal, /channelSettings\.kiro\?\.test_model/)
  assert.match(modal, /!isAntigravityAccount && !isKiroAccount;/)
})

test('Kiro accounts page mirrors the shared account tooling', () => {
  for (const component of ['AccountDetailSheet', 'AccountUsageModal', 'AccountHealthBar', 'AccountIntelligenceBar', 'RequestCountPills', 'BatchAccountGroupModal', 'AccountGroupManagerModal', 'AccountProxyQuickEditor', 'APIKeyMultiSelect', 'ColumnSettingsMenu']) {
    assert.match(page, new RegExp(`<${component}\\b`), component)
  }
  for (const call of ['updateAccountScheduler', 'batchUpdateAccounts', 'batchDeleteAccounts', 'refreshAccount', 'refreshAccountUsage', 'toggleAccountLock', 'resetAccountStatus', 'getAccountPageStats', 'getAccountHealthBars']) {
    assert.match(page, new RegExp(`\\.${call}\\(`), call)
  }
  // Kiro 按积分计费，没有 5h/7d 窗口：不提供按窗口的自动暂停。
  assert.doesNotMatch(page, /auto_pause_(5h|7d)_threshold/)
  assert.match(page, /kiro_auth_kinds/)
  assert.match(page, /"kiro student"/)
  assert.match(page, /"kiro max"/)
})
