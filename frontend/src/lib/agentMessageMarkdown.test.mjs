import assert from 'node:assert/strict'
import test from 'node:test'

import { formatAgentMessageTags } from './agentMessageMarkdown.ts'

test('text without agent-message tags is returned unchanged', () => {
  const text = 'plain **markdown**\n\n```\n<div>x</div>\n```'
  assert.equal(formatAgentMessageTags(text), text)
  assert.equal(formatAgentMessageTags(''), '')
})

test('agent-message block becomes a labelled blockquote that keeps sender and body', () => {
  const out = formatAgentMessageTags('<agent-message from="general-purpose">\n已确认本仓助手存在。\n第二行\n</agent-message>')
  assert.match(out, /> \*\*agent-message\*\* · from `general-purpose`/)
  assert.match(out, /> 已确认本仓助手存在。\n> 第二行/)
  assert.doesNotMatch(out, /<\/?agent-message/)
})

test('consecutive agent-messages are separated into independent blockquotes', () => {
  const out = formatAgentMessageTags('<agent-message from="a">one</agent-message>\n<agent-message from="b">two</agent-message>')
  assert.equal((out.match(/\*\*agent-message\*\*/g) ?? []).length, 2)
  assert.match(out, /from `a`\n>\n> one\n\n/)
  assert.match(out, /from `b`\n>\n> two/)
})

test('single quoted and missing from attributes are handled', () => {
  assert.match(formatAgentMessageTags("<agent-message from='x y'>hi</agent-message>"), /from `x y`/)
  const bare = formatAgentMessageTags('<agent-message>hi</agent-message>')
  assert.match(bare, /> \*\*agent-message\*\*\n>\n> hi/)
})

test('unclosed tag from a truncated preview extends to the end of the text', () => {
  const out = formatAgentMessageTags('before\n<agent-message from="w">partial body')
  assert.match(out, /before/)
  assert.match(out, /> partial body/)
})

test('stray closing tag is removed and blank body keeps only the header', () => {
  assert.equal(formatAgentMessageTags('oops</agent-message> done'), 'oops done')
  const empty = formatAgentMessageTags('<agent-message from="e"></agent-message>')
  assert.match(empty, /> \*\*agent-message\*\* · from `e`/)
  assert.doesNotMatch(empty, />\n>/)
})

test('blank lines inside the body stay inside the quote', () => {
  const out = formatAgentMessageTags('<agent-message from="p">a\n\nb</agent-message>')
  assert.match(out, /> a\n>\n> b/)
})

test('fenced code blocks are left untouched', () => {
  const text = 'see:\n```xml\n<agent-message from="x">literal</agent-message>\n```\nafter'
  assert.equal(formatAgentMessageTags(text), text)
  const mixed = formatAgentMessageTags('<agent-message from="m">hi</agent-message>\n~~~\n<agent-message>keep</agent-message>\n~~~')
  assert.match(mixed, /> hi/)
  assert.match(mixed, /~~~\n<agent-message>keep<\/agent-message>\n~~~/)
})

test('sender backticks cannot break out of the code span', () => {
  const out = formatAgentMessageTags('<agent-message from="a`b">x</agent-message>')
  assert.match(out, /from `a b`/)
})
