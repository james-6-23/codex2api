import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))
const zhTW = JSON.parse(readFileSync(new URL('../locales/zh-TW.json', import.meta.url), 'utf8'))
const operations = readFileSync(new URL('../pages/Operations.tsx', import.meta.url), 'utf8')
const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')

test('scheduler engine choices use localized Chinese labels', () => {
  assert.deepEqual(
    [zh.settings.schedulerEngineLegacy, zh.settings.schedulerEngineShadow, zh.settings.schedulerEngineIndexed],
    ['旧版扫描', '影子校验', '索引调度'],
  )
  assert.deepEqual(
    [zhTW.settings.schedulerEngineLegacy, zhTW.settings.schedulerEngineShadow, zhTW.settings.schedulerEngineIndexed],
    ['舊版掃描', '影子校驗', '索引調度'],
  )

  for (const locale of [zh.settings, zhTW.settings]) {
    assert.ok(locale.schedulerEngineLegacyDesc.length > 20)
    assert.match(locale.schedulerEngineShadowDesc, /64/)
    assert.ok(locale.schedulerEngineIndexedDesc.length > 20)
  }
  assert.match(zh.settings.schedulerEngineCompatibilityNote, /快速调度器/)
  assert.match(zh.settings.schedulerEngineCompatibilityNote, /关闭对应“旧版扫描”/)
  assert.match(zh.settings.schedulerEngineCompatibilityNote, /开启对应“索引调度”/)
  assert.match(settings, /schedulerEngineExplanations\.map/)
  assert.match(settings, /settings\.schedulerEngineCompatibilityNote/)
  assert.match(settings, /function SettingsCollapsibleNote/)
  assert.match(settings, /role="note"/)
  assert.match(settings, /aria-current=\{active \? 'true' : undefined\}/)
})

test('operations overview localizes the scheduler engine value', () => {
  assert.match(operations, /value=\{formatSchedulerEngine\(overview\.scheduler\.engine, t\)\}/)
  assert.match(operations, /case 'legacy':[\s\S]*settings\.schedulerEngineLegacy/)
  assert.match(operations, /case 'shadow':[\s\S]*settings\.schedulerEngineShadow/)
  assert.match(operations, /case 'indexed':[\s\S]*settings\.schedulerEngineIndexed/)
})

test('scheduling cards keep their own content height on desktop', () => {
  assert.match(settings, /className="grid auto-rows-min items-start gap-4 lg:grid-cols-2"/)
  assert.equal(
    settings.match(/h-fit space-y-3 rounded-xl border border-border\/60 bg-muted\/10 p-3.5/g)?.length,
    2,
  )
})
