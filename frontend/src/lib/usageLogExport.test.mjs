import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { api } from '../api.ts'
import { confirmedUsageLogDownload, downloadUsageLogPages, saveUsageLogExport } from './usageLogExport.ts'

test('canceling confirmation does not request or save an export', async () => {
  const calls = []
  const result = await confirmedUsageLogDownload(
    async () => { calls.push('confirm'); return false },
    async () => { calls.push('download'); return new Blob() },
    () => calls.push('save'),
  )
  assert.equal(result, false)
  assert.deepEqual(calls, ['confirm'])
})

test('confirmed export saves only after successful download', async () => {
  const calls = []
  const blob = new Blob(['{"logs":[],"complete":true}'])
  const result = await confirmedUsageLogDownload(
    async () => { calls.push('confirm'); return true },
    async () => { calls.push('download'); return blob },
    actual => { assert.equal(actual, blob); calls.push('save') },
  )
  assert.equal(result, true)
  assert.deepEqual(calls, ['confirm', 'download', 'save'])
  await assert.rejects(confirmedUsageLogDownload(
    async () => true,
    async () => { throw new Error('export interrupted') },
    () => assert.fail('a failed request must not save a partial file'),
  ), /export interrupted/)
})

test('filtered exports preserve query filters and all exports discard every filter', async context => {
  const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: { getItem: () => 'test-admin-key' } })
  context.after(() => {
    if (storage) Object.defineProperty(globalThis, 'localStorage', storage)
    else delete globalThis.localStorage
  })
  const calls = []
  context.mock.method(globalThis, 'fetch', async (url, options) => {
    calls.push({ url: new URL(url, 'http://localhost'), options })
    return new Response('{"logs":[],"total":0,"complete":true}', { headers: { 'Content-Type': 'application/json' } })
  })
  const filters = { start: '2026-09-11T00:00:00Z', end: '2026-09-12T00:00:00Z', q: '01a09012', requestType: 'compaction', channel: 'codex', status: '500', viaWebsocket: 'true', retry: 'false', accountId: '42', page: 99, pageSize: 1 }
  const signal = new AbortController().signal
  await api.downloadUsageLogs('filtered', filters, signal)
  const first = calls[0]
  assert.equal(first.url.pathname, '/api/admin/usage/logs/export')
  assert.equal(first.options.method, 'POST')
  assert.equal(first.options.signal, signal)
  assert.equal(first.options.headers.get('X-Admin-Key'), 'test-admin-key')
  assert.deepEqual(Object.fromEntries(first.url.searchParams), {
    start: filters.start, end: filters.end, q: filters.q, request_type: 'compaction', channel: 'codex', status: '500', via_websocket: 'true', retry: 'false', account_id: '42', scope: 'filtered', confirmed: 'true',
  })
  const blob = await api.downloadUsageLogs('all', filters, signal)
  assert.deepEqual(Object.fromEntries(calls[1].url.searchParams), { scope: 'all', confirmed: 'true' })
  assert.equal(JSON.parse(await blob.text()).complete, true)
})

test('usage JSON saves clean up the temporary download link', context => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const actions = []
  const anchor = { click: () => actions.push('click'), remove: () => actions.push('remove') }
  Object.defineProperty(globalThis, 'document', { configurable: true, value: {
    createElement: name => { assert.equal(name, 'a'); return anchor },
    body: { appendChild: value => { assert.equal(value, anchor); actions.push('append') } },
  } })
  context.after(() => {
    if (descriptor) Object.defineProperty(globalThis, 'document', descriptor)
    else delete globalThis.document
  })
  context.mock.method(URL, 'createObjectURL', () => 'blob:test-export')
  context.mock.method(URL, 'revokeObjectURL', url => { assert.equal(url, 'blob:test-export'); actions.push('revoke') })
  context.mock.method(globalThis, 'setTimeout', callback => { callback(); return 0 })
  saveUsageLogExport(new Blob(['{}']), 'filtered')
  assert.match(anchor.download, /^usage-logs-filtered-.*\.json$/)
  assert.deepEqual(actions, ['append', 'click', 'remove', 'revoke'])
})

test('usage page exposes two confirmed download actions and a privacy warning', () => {
  const source = readFileSync(new URL('../pages/Usage.tsx', import.meta.url), 'utf8')
  assert.match(source, /downloadLogs\('filtered'\)/)
  assert.match(source, /downloadLogs\('all'\)/)
  assert.match(source, /confirmedUsageLogDownload\(/)
  assert.match(source, /usage\.exportPrivacy/)
  for (const locale of ['zh', 'zh-TW', 'en']) {
    const messages = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8')).usage
    for (const key of ['exportFiltered', 'exportAll', 'exportFilteredTitle', 'exportAllTitle', 'exportFilteredDesc', 'exportAllDesc', 'exportPrivacy', 'exportConfirm', 'exportCancel', 'exportSuccess', 'exportFailed']) {
      assert.equal(typeof messages[key], 'string')
    }
  }
})

const pageMetadata = '{"version":1,"scope":"all","generated_at":"2026-09-12T00:00:00Z","filters":{}}'
const exportPage = (records, next = '') => ({ version: 1, metadata: pageMetadata, records, next_cursor: next, complete: !next })

test('paged downloads preserve large integers and avoid repeated records when retrying a page', async context => {
  context.mock.method(globalThis, 'setTimeout', callback => { queueMicrotask(callback); return 1 })
  const cursors = []
  const saved = []
  let attempts = 0
  const result = await downloadUsageLogPages(async cursor => {
    cursors.push(cursor)
    if (!cursor) return exportPage(['{"id":1,"counter":9007199254740993}'], 'page-2')
    if (attempts++ === 0) throw new TypeError('Failed to fetch')
    return exportPage(['{"id":2}'])
  }, blob => saved.push(blob), new AbortController().signal, () => {})
  assert.deepEqual(cursors, ['', 'page-2', 'page-2'])
  assert.equal(result.records, 2)
  assert.equal(saved.length, 1)
  const raw = await saved[0].text()
  assert.match(raw, /9007199254740993/)
  assert.equal(JSON.parse(raw).logs.length, 2)
  assert.equal(JSON.parse(raw).export_complete, true)
})

test('large exports save bounded valid JSON parts and mark only the last part as the completed export', async () => {
  const saved = []
  const result = await downloadUsageLogPages(async () => exportPage(['{"id":1}', '{"id":2}', '{"id":3}']), (blob, part) => saved.push({ blob, part }), new AbortController().signal, () => {}, 10)
  assert.equal(result.parts, 3)
  const decoded = await Promise.all(saved.map(async savedPart => JSON.parse(await savedPart.blob.text())))
  assert.deepEqual(decoded.map(part => part.logs[0].id), [1, 2, 3])
  assert.deepEqual(decoded.map(part => part.export_complete), [false, false, true])
  assert.deepEqual(decoded.map(part => part.part), [1, 2, 3])
  assert.equal(decoded[2].exported_records, 3)
})

test('failed, canceled, or inconsistent batches never save the unfinished part', async () => {
  for (const failure of ['failure', 'canceled', 'cursor', 'snapshot', 'record']) {
    const controller = new AbortController()
    await assert.rejects(downloadUsageLogPages(async cursor => {
      if (!cursor) return exportPage(['{"id":1}'], 'next')
      if (failure === 'failure') throw new Error('interrupted')
      if (failure === 'canceled') { controller.abort(); return exportPage(['{"id":2}']) }
      if (failure === 'cursor') return exportPage(['{"id":2}'], 'next')
      if (failure === 'record') return exportPage(['{"broken":'])
      return { ...exportPage(['{"id":2}']), metadata: '{"scope":"filtered"}' }
    }, () => assert.fail('must not save a partial or mismatched export'), controller.signal, () => {}))
  }
})

test('paged export API preserves scope, authentication, cancellation, and the opaque cursor', async context => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: { getItem: () => 'admin-test' } })
  context.after(() => { if (descriptor) Object.defineProperty(globalThis, 'localStorage', descriptor); else delete globalThis.localStorage })
  context.mock.method(globalThis, 'fetch', async (url, options) => {
    const query = new URL(url, 'http://localhost').searchParams
    assert.equal(query.get('scope'), 'all')
    assert.equal(query.get('paged'), 'true')
    assert.equal(query.get('confirmed'), 'true')
    assert.equal(query.get('cursor'), 'opaque-cursor')
    assert.equal(query.has('q'), false)
    assert.equal(options.headers.get('X-Admin-Key'), 'admin-test')
    assert.equal(options.method, 'POST')
    return new Response(JSON.stringify(exportPage([])), { headers: { 'Content-Type': 'application/json' } })
  })
  const page = await api.getUsageLogExportPage('all', { q: 'ignored', start: 'ignored', end: 'ignored' }, 'opaque-cursor', new AbortController().signal)
  assert.equal(page.complete, true)
})
