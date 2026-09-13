import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

import { buildWritableSettingsPayload } from './settingsPayload.ts'

test('WS context takeover is typed, default-off, and auto-saved in Codex transport settings', () => {
  const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
  assert.match(types, /codex_ws_context_takeover: boolean/)
  const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
  assert.match(settings, /codex_ws_context_takeover: false/)
  assert.match(settings, /codex_ws_context_takeover: cacheNormalized\.codex_ws_context_takeover \?\? false/)
  const cardStart = settings.indexOf("<SettingsCard title={t('settings.codexWebsocket')}")
  assert.ok(cardStart >= 0)
  const card = settings.slice(cardStart, settings.indexOf('</SettingsCard>', cardStart))
  const fieldStart = card.indexOf("<SettingField label={t('settings.codexWSContextTakeover')}")
  assert.ok(fieldStart >= 0)
  const field = card.slice(fieldStart, card.indexOf('</SettingField>', fieldStart))
  assert.match(field, /description=\{t\('settings\.codexWSContextTakeoverHint'\)\}/)
  assert.match(field, /channels=\{CHANNELS_CODEX_ONLY\}/)
  assert.match(field, /checked=\{settingsForm\.codex_ws_context_takeover\}/)
  assert.match(field, /autoSaveBooleanField\('codex_ws_context_takeover', checked\)/)
  assert.doesNotMatch(field, /disabled=/)
})

test('writable settings payload preserves both WS context takeover values without adding it to unrelated patches', () => {
  for (const enabled of [true, false]) {
    assert.deepEqual(buildWritableSettingsPayload({ codex_ws_context_takeover: enabled }), {
      codex_ws_context_takeover: enabled,
    })
  }
  assert.deepEqual(buildWritableSettingsPayload({ site_name: 'unchanged transport' }), {
    site_name: 'unchanged transport',
  })
})

test('WS context takeover translations explain negotiation, memory cost, and new connections only', () => {
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const { settings } = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    assert.ok(settings.codexWSContextTakeover, locale)
    const hint = settings.codexWSContextTakeoverHint
    for (const constraint of [
      /默认关闭|預設關閉|Off by default/,
      /permessage-deflate/,
      /协商类似官方|協商類似官方|negotiates official-like/,
      /服务端仍可拒绝压缩|伺服器仍可拒絕壓縮|server can decline compression/,
      /要求消息独立压缩|要求訊息獨立壓縮|require independent messages/,
      /每条连接占用更多内存|每條連線佔用更多記憶體|higher per-connection memory/,
      /仅对新建的上游 WebSocket 连接生效|僅對新建立的上游 WebSocket 連線生效|Only newly established upstream WebSocket connections/,
      /已有连接及续链保持不变|既有連線及續鏈保持不變|existing connections and continuation remain unchanged/,
      /直到自然替换|直到自然替換|until naturally replaced/,
      /不主动中断|不主動中斷|without interruption/,
      /独立于 HTTP zstd|獨立於 HTTP zstd|Independent of HTTP zstd/,
      /不修改聊天上下文或会话\/响应 ID|不修改聊天上下文或會話\/回應 ID|does not change chat context or session\/response IDs/,
    ]) {
      assert.match(hint, constraint, locale)
    }
    assert.doesNotMatch(hint, /client_max_window_bits/, locale)
  }
})
