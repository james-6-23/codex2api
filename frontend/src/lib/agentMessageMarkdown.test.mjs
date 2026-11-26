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

test('fenced code inside the body stays inside the same blockquote', () => {
  const out = formatAgentMessageTags('<agent-message from="a">intro\n```js\nlet x=1\n```\noutro</agent-message>\ntail')
  assert.match(out, /> intro\n> ```js\n> let x=1\n> ```\n> outro/)
  assert.match(out, /outro\n\n\ntail$/)
  assert.doesNotMatch(out, /\n```js/)
})

test('closing tag text inside a fence within the body does not end the block', () => {
  const out = formatAgentMessageTags('<agent-message from="a">x\n```\n</agent-message>\n```\ny</agent-message>')
  assert.match(out, /> ```\n> <\/agent-message>\n> ```\n> y/)
})

test('self-closing tag yields a header-only quote and does not swallow later text', () => {
  const out = formatAgentMessageTags('<agent-message from="a"/>\nafter text')
  assert.match(out, /> \*\*agent-message\*\* · from `a`\n\n/)
  assert.match(out, /\nafter text$/)
  assert.doesNotMatch(out, /> after text/)
})

test('a new opening tag ends a previous unclosed block', () => {
  const out = formatAgentMessageTags('<agent-message from="a">one\n<agent-message from="b">two')
  assert.match(out, /from `a`\n>\n> one/)
  assert.match(out, /from `b`\n>\n> two/)
})

test('four-space indented fence marker does not close a fence', () => {
  const text = '```xml\n    ```\n<agent-message>literal</agent-message>\n```'
  assert.equal(formatAgentMessageTags(text), text)
})

test('fence marker indented up to three spaces still closes the fence', () => {
  const out = formatAgentMessageTags('```\nx\n   ```\n<agent-message from="a">real</agent-message>')
  assert.match(out, /> \*\*agent-message\*\* · from `a`\n>\n> real/)
  const tab = '```\n\t```\n<agent-message>literal</agent-message>\n```'
  assert.equal(formatAgentMessageTags(tab), tab)
})

test('opening tag split across lines is recognised and sender newlines are flattened', () => {
  const out = formatAgentMessageTags('<agent-message\nfrom="a\nb">body</agent-message>\nafter')
  assert.match(out, /> \*\*agent-message\*\* · from `a b`\n>\n> body/)
  assert.match(out, /\nafter$/)
  assert.doesNotMatch(out, /<agent-message/)
  const closeOnNextLine = formatAgentMessageTags('before <agent-message from="x"\n>hi</agent-message>')
  assert.match(closeOnNextLine, /^before \n\n> \*\*agent-message\*\* · from `x`\n>\n> hi/)
})

test('an unfinished opening tag stays literal when no closing bracket arrives', () => {
  assert.equal(formatAgentMessageTags('text <agent-message'), 'text <agent-message')
  const withFence = '<agent-message\n```\ncode\n```'
  assert.equal(formatAgentMessageTags(withFence), withFence)
  const long = '<agent-message\n' + 'x\n'.repeat(12) + 'y'
  assert.equal(formatAgentMessageTags(long), long)
})

test('tags inside inline code spans stay literal', () => {
  const single = 'Use `<agent-message from="x">literal</agent-message>` here.'
  assert.equal(formatAgentMessageTags(single), single)
  const double = 'a ``<agent-message>`x</agent-message>`` b'
  assert.equal(formatAgentMessageTags(double), double)
})

test('real tags outside code spans are still converted next to code spans', () => {
  const out = formatAgentMessageTags('`<agent-message>` and <agent-message from="y">real</agent-message>')
  assert.match(out, /^`<agent-message>` and \n\n> \*\*agent-message\*\* · from `y`\n>\n> real/)
  const inBody = formatAgentMessageTags('<agent-message from="a">use `</agent-message>` literally</agent-message>')
  assert.match(inBody, /> use `<\/agent-message>` literally/)
})
