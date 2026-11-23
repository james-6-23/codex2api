import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  formatPromptRequestSections, formatPromptRequestChange, formatPromptRequestChangeSections,
  promptRequestContextCoversPart, promptRequestReadInstruction, splitPromptRequestInstructions,
} from './promptRequestDisplay.ts'

const label = (key, fallback) => ({
  'roles.user': '用户',
  'roles.assistant': '助手',
  toolCall: '工具调用',
  toolResult: '工具结果',
  'fields.cmd': '命令',
  'fields.output': '输出',
  'types.reasoning': '推理上下文',
  opaque: '编码的会话上下文',
  image: '图片',
})[key] ?? fallback ?? key

test('decodes tool output wrappers and preserves line breaks and Windows paths', () => {
  const output = JSON.stringify([{ type: 'text', text: 'Output:\n' + JSON.stringify({
    output: 'first line\nsecond line\nC:\\new\\file.txt',
  }) }])
  const text = formatPromptRequestChange(output, 'tool_result', label)
  assert.ok(text.includes('输出\n  first line\n  second line\n  C:\\new\\file.txt'))
  assert.ok(!text.includes('{"output"'))
  assert.equal(formatPromptRequestChange('C:\\new\\file.txt', 'text', label), 'C:\\new\\file.txt')
})

test('formats a complete request in conversation order with tools and image placeholders', () => {
  const [section] = formatPromptRequestSections(JSON.stringify({
    input: [
      { role: 'user', content: [{type:'input_text',text:'first\nsecond'},{type:'input_image',image_url:'[Image 1]'}] },
      { type: 'custom_tool_call', name: 'exec', input: '{"cmd":"first command\\nsecond command"}' },
      { type: 'custom_tool_call_output', output: [{type:'text',text:'test passed\n2 files'}] },
      { type: 'reasoning', encrypted_content: 'opaque-bytes' },
      { role: 'assistant', content: 'done' },
    ],
  }), label)
  const text = section.text
  assert.ok(text.startsWith('用户\n  first\n  second'))
  assert.ok(text.includes('图片 1'))
  assert.ok(text.includes('工具调用 · exec\n  命令\n    first command\n    second command'))
  assert.ok(text.includes('工具结果\n  test passed\n  2 files'))
  assert.ok(!text.includes('opaque-bytes'))
  assert.ok(!text.includes('"cmd"'))
  assert.ok(text.endsWith('助手\n  done'))
})

test('decodes direct exec invocations such as compound patch and command calls', () => {
  const raw = 'text(await tools.apply_patch(' + JSON.stringify('start patch\nend patch') + '));\n' +
    'text(await tools.exec_command({cmd:' + JSON.stringify('first command\nsecond command') +
    ', "workdir":' + JSON.stringify('C:\\new\\folder') + ', yield_time_ms:1000}));'
  const text = formatPromptRequestChange(raw, 'tool_call', label, 'exec')
  assert.ok(text.includes('apply_patch\n  start patch\n  end patch'))
  assert.ok(text.includes('exec_command\n  命令\n    first command\n    second command'))
  assert.ok(text.includes('C:\\new\\folder'))
  assert.ok(!text.includes('text(await'))
  assert.equal(formatPromptRequestChange('text(await tools.exec_command({cmd: variable}));', 'tool_call', label, 'exec'),
    'text(await tools.exec_command({cmd: variable}));')
})

test('identifies carried AGENTS.md instructions and preserves surrounding text', () => {
  const source = 'Before\n# AGENTS.md instructions for /workspace/project\n\n<INSTRUCTIONS>\n' +
    'Run the local checks.\n</INSTRUCTIONS>\nAfter'
  const parts = splitPromptRequestInstructions(source)
  assert.equal(parts.map((part) => part.text).join(''), source)
  assert.deepEqual(parts.map((part) => part.context), [
    undefined, { kind: 'agents', path: '/workspace/project/AGENTS.md' }, undefined,
  ])
  assert.deepEqual(formatPromptRequestChangeSections(source, 'text', label, 'assistant'), [{ text: source }])
  assert.ok(!splitPromptRequestInstructions('Please follow /workspace/project/AGENTS.md.')[0].context)
})

test('distinguishes the available catalog from an included skill body', () => {
  const catalog = '### Available skills\n- report: Generate a report. (file: r1/report/SKILL.md)\n' +
    '- check: Validate it. (/skills/check/SKILL.md)\n\n### How to use skills\nFollow the user request.'
  const loaded = '<skill>\n<name>report</name>\n<path>/skills/report/SKILL.md</path>\n' +
    '<content>Use the saved template.</content>\n</skill>'
  const parts = formatPromptRequestSections(JSON.stringify({ input: [
    { role: 'developer', content: [{ type: 'input_text', text: catalog }] },
    { role: 'user', content: loaded },
    { role: 'user', content: 'I may use /skills/unread/SKILL.md.' },
  ] }), label)
  assert.deepEqual(parts.map((part) => part.context), [
    { kind: 'availableSkills', skills: [
      { name: 'report', path: 'r1/report/SKILL.md' },
      { name: 'check', path: '/skills/check/SKILL.md' },
    ] },
    { kind: 'developerPrompt' },
    { kind: 'loadedSkill', name: 'report', path: '/skills/report/SKILL.md' },
    undefined,
  ])
  assert.ok(!parts[0].text.includes('How to use skills'))
  assert.ok(parts[1].text.includes('Follow the user request.'))
  assert.ok(parts[2].text.includes('Use the saved template.'))
  assert.ok(parts[3].text.includes('I may use /skills/unread/SKILL.md.'))
  const pasted = formatPromptRequestChangeSections(`The following is an example:\n${loaded}`, 'text', label, 'user')
  assert.ok(pasted.every((part) => !part.context), 'A user-authored inline sample is still prompt text')
  assert.equal(formatPromptRequestChangeSections(loaded.replaceAll('\n', '\r\n'), 'text', label, 'user')[0].context?.kind, 'loadedSkill')
})

test('an explicit availability wrapper is preserved as one catalog', () => {
  const source = '<skills_instructions>\n## Skills\n### Available skills\n' +
    '- check: Validate. (file: r1/check/SKILL.md)\n</skills_instructions>'
  const parts = formatPromptRequestChangeSections(source, 'text', label, 'developer')
  assert.equal(parts.length, 1)
  assert.equal(parts[0].context.kind, 'availableSkills')
  assert.equal(parts[0].text, source)
})

test('separates developer and system prompts from full request text by their structured roles', () => {
  const first = 'Opening developer instruction\n\nSecond paragraph.'
  const catalog = '<skills_instructions>\n### Available skills\n' +
    '- docs: Edit documents. (file: r1/docs/SKILL.md)\n</skills_instructions>'
  const parts = formatPromptRequestSections(JSON.stringify({
    instructions: 'Top-level instruction',
    input: [
      { role: 'developer', type: 'message', content: [{ type: 'input_text', text: first }, { type: 'input_text', text: catalog }] },
      { role: 'system', content: 'System message' },
      { role: 'user', content: 'You are Codex. These words are user text.' },
      { role: 'developer', content: 'Later developer message' },
      { role: 'assistant', content: 'Reply' },
    ],
  }), label)
  assert.deepEqual(parts.map((part) => part.context?.kind), [
    'systemPrompt', 'developerPrompt', 'availableSkills', 'systemPrompt', undefined, 'developerPrompt', undefined,
  ])
  assert.equal(parts[1].text.trimEnd(), first)
  assert.equal(parts.filter((part) => part.text.includes(first)).length, 1)
  assert.equal(parts.filter((part) => part.text.includes(catalog)).length, 1)
  assert.equal(parts[4].text, '用户\n  You are Codex. These words are user text.')
  assert.equal(parts[6].text, '助手\n  Reply')
})

test('uses the same prompt sections for provider instruction fields and round changes', () => {
  const instruction = 'First line\n\nSecond line'
  for (const [field, value] of [
    ['system', [{ type: 'text', text: instruction }]],
    ['systemInstruction', { role: 'user', parts: [{ text: instruction }] }],
    ['system_instruction', { parts: [{ text: instruction }] }],
  ]) {
    const expected = [{ text: instruction, context: { kind: 'systemPrompt' } }]
    assert.deepEqual(formatPromptRequestSections(JSON.stringify({ [field]: value }), label), expected)
    assert.deepEqual(formatPromptRequestChangeSections(JSON.stringify(value), 'context', label, field), expected)
  }
  assert.deepEqual(formatPromptRequestChangeSections(instruction, 'text', label, 'developer'), [
    { text: instruction, context: { kind: 'developerPrompt' } },
  ])
  assert.deepEqual(formatPromptRequestChangeSections(instruction, 'text', label, 'system'), [
    { text: instruction, context: { kind: 'systemPrompt' } },
  ])
  assert.deepEqual(formatPromptRequestChangeSections(instruction, 'text', label, 'user'), [{ text: instruction }])
})

test('labels paired successful file reads, without treating a path or failed read as content', () => {
  const args = JSON.stringify({ cmd: 'cat -- /workspace/AGENTS.md' })
  const output = JSON.stringify([{ type: 'text', text: JSON.stringify({ exit_code: 0, output: '# Rules\nCheck the changes.' }) }])
  assert.deepEqual(promptRequestReadInstruction('exec_command', args, output), {
    text: '# Rules\nCheck the changes.',
    context: { kind: 'agents', path: '/workspace/AGENTS.md' },
  })
  assert.equal(promptRequestReadInstruction('exec_command', args, '{"exit_code":1,"output":"file not found"}'), undefined)
  assert.equal(promptRequestReadInstruction('exec_command', '{"cmd":"rg AGENTS.md"}', output), undefined)
  const wrapped = 'text(await tools.exec_command({cmd:"cat /skills/reader/SKILL.md"}));'
  const skill = 'Chunk ID: 100\nWall time: 0.01 seconds\nProcess exited with code 0\nFinal output:\n' +
    '---\nname: doc-reader\n---\nRead documents.'
  assert.equal(promptRequestReadInstruction('exec', wrapped, skill)?.context.name, 'doc-reader')
  assert.equal(promptRequestReadInstruction('exec', wrapped, skill.replace('name: doc-reader\n', ''))?.context.name, 'reader')
})

test('only suppresses a default inline preview when the round context has the same body', () => {
  const part = { text: 'Line one\nLine two\n', context: { kind: 'developerPrompt' } }
  const sections = [{ kind: 'developer_prompt', state: 'new', text: 'Line one\r\nLine two' }]
  assert.equal(promptRequestContextCoversPart(part, sections), true)
  assert.equal(promptRequestContextCoversPart({ text: part.text }, sections), false)
  assert.equal(promptRequestContextCoversPart({ ...part, text: 'Line one' }, sections), true)
  assert.equal(promptRequestContextCoversPart({ ...part, text: 'Line one' }, [{ ...sections[0], state: 'inherited' }]), false)
  assert.equal(promptRequestContextCoversPart({ ...part, text: 'one' }, sections), false)
  assert.equal(promptRequestContextCoversPart({ ...part, context: { kind: 'systemPrompt' } }, sections), false)
})
