import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

import { buildWritableSettingsPayload } from './settingsPayload.ts'

test('session failover toggle is default-off and auto-saved near global auto-pause', () => {
  const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
  assert.match(settings, /codex_session_failover_enabled: false/)
  assert.match(settings, /codex_session_failover_enabled: cacheNormalized\.codex_session_failover_enabled \?\? false/)
  const cardStart = settings.indexOf("<SettingsCard title={t('settings.globalAutoPauseTitle')}")
  assert.ok(cardStart >= 0)
  const card = settings.slice(cardStart, settings.indexOf('</SettingsCard>', cardStart))
  assert.match(card, /label=\{t\('settings\.codexSessionFailoverEnabled'\)\}/)
  assert.match(card, /description=\{t\('settings\.codexSessionFailoverEnabledHint'\)\}/)
  assert.match(card, /channels=\{CHANNELS_CODEX_ONLY\}/)
  assert.match(card, /checked=\{settingsForm\.codex_session_failover_enabled\}/)
  assert.match(card, /autoSaveBooleanField\('codex_session_failover_enabled', checked\)/)
})

test('writable settings payload preserves both session failover boolean values', () => {
  for (const enabled of [true, false]) {
    assert.deepEqual(buildWritableSettingsPayload({ codex_session_failover_enabled: enabled }), {
      codex_session_failover_enabled: enabled,
    })
  }
})

test('session failover translations include identity and non-migration constraints', () => {
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const { settings } = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    assert.ok(settings.codexSessionFailoverEnabled, locale)
    if (locale === 'zh') {
      assert.equal(settings.codexSessionFailoverEnabled, '账号不可用或会话容量不足时允许换号')
    }
    const hint = settings.codexSessionFailoverEnabledHint
    assert.match(hint, /有损重开|有損重開|lossy restart/, locale)
    assert.match(hint, /出站副本|outbound copy/, locale)
    assert.match(hint, /没有可用输入|沒有可用輸入|no usable input/, locale)
    for (const constraint of [/previous_response_id/, /500/, /busy/, /默认关闭|預設關閉|Off by default/, /出站身份段|出站身分段|outbound identity segment/, /后续.*请求|後續.*請求|Later requests/, /完整明文上下文|full plaintext context/, /加密上下文|encrypted context/, /加密推理|encrypted reasoning/, /加密压缩|加密壓縮|compaction/, /未知的 previous_response_id|unknown previous_response_id/, /新建会话|建立新會話|new conversation/, /握手|handshake/, /黑名单|黑名單|blacklist/, /过期|到期|expiry/, /窗口|window/, /分组集合必须完全一致|分組集合必須完全一致|Group membership must match exactly/, /47→0/, /48→1/]) {
      assert.match(hint, constraint, locale)
    }
  }
})
