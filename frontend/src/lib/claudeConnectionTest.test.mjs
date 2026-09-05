import test from 'node:test'
import assert from 'node:assert/strict'
import { createClaudeTestEventParser, readClaudeTestEvents, claudeTestTokenMetrics } from './claudeConnectionTest.ts'

test('Claude test SSE handles split CRLF, comments, multiline data, and an unterminated final frame', () => {
  const events = []
  const parser = createClaudeTestEventParser((event) => events.push(event))
  const wire = ': heartbeat\r\n\r\ndata:{"type":"content",\r\ndata: "text":"你好"}\r\n\r\n' +
    'data: not-json\n\ndata: {"type":"unknown"}\n\ndata: {"type":"test_complete","success":true}'
  for (const char of wire) parser.feed(char)
  parser.finish()
  assert.deepEqual(events, [{ type: 'content', text: '你好' }, { type: 'test_complete', success: true }])
})

test('Claude test drains diagnostics after the terminal event and resolves only after stream closure', async () => {
  const encoder = new TextEncoder()
  let controller
  const stream = new ReadableStream({ start(value) { controller = value } })
  const events = []
  let settled = false
  const result = readClaudeTestEvents(stream, (event) => events.push(event)).then((terminal) => { settled = true; return terminal })
  const text = encoder.encode('data: {"type":"content","text":"你好"}\n\n')
  for (const byte of text) controller.enqueue(Uint8Array.of(byte))
  controller.enqueue(encoder.encode('data: {"type":"test_complete","success":true}\n\n'))
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(settled, false)
  assert.equal(events[0].text, '你好')
  assert.equal(events.at(-1).type, 'test_complete')
  controller.enqueue(encoder.encode('data: {"type":"diagnostics","diagnostics":{"model":"claude-test","duration_ms":15}}\n\n'))
  controller.close()
  assert.equal(await result, true)
  assert.equal(events.at(-1).diagnostics.duration_ms, 15)
})

test('Claude test distinguishes missing terminal events, upstream errors, and broken transports', async () => {
  const encoder = new TextEncoder()
  for (const [wire, expected] of [
    ['data: {"type":"content","text":"partial"}\n\n', false],
    ['data: {"type":"error","error":"rate limited"}\n\n', true],
  ]) {
    const stream = new ReadableStream({ start(controller) { controller.enqueue(encoder.encode(wire)); controller.close() } })
    assert.equal(await readClaudeTestEvents(stream, () => {}), expected)
  }
  const stream = new ReadableStream({ start(controller) { controller.error(new Error('connection lost')) } })
  await assert.rejects(readClaudeTestEvents(stream, () => {}), /connection lost/)
})

test('Claude test token metrics preserve unknown and zero without adding cache tokens to input', () => {
  const metrics = claudeTestTokenMetrics({ input_tokens: 0, output_tokens: 20, cache_read_input_tokens: 100 })
  assert.deepEqual(metrics.map(({ value }) => value), [0, 20, 100, null])
  assert.deepEqual(metrics.map(({ percent }) => percent), [0, 20, 100, 0])
  assert.ok(claudeTestTokenMetrics().every(({ value }) => value === null))
  assert.ok(claudeTestTokenMetrics({ input_tokens: -1, output_tokens: NaN }).every(({ value }) => value === null))
})
