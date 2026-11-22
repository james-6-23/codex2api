import assert from 'node:assert/strict'
import test from 'node:test'
import {
  createPromptSkillDownload,
  promptSkillDownloadFileName,
  promptSkillMarkdown,
  promptSkillMarkdownIsPartial,
} from './promptSkillDownload.ts'

const readSkill = { kind: 'loaded_skill', state: 'received', name: 'fixture', path: '/skills/fixture/SKILL.md', evidence: 'successful_read' }

test('preserves the recorded Markdown bytes and only unwraps matching injected evidence', () => {
  const document = '\ufeff---\r\nname: fixture\r\n---\r\n\r\n  Use the test fixture.\r\n'
  assert.equal(promptSkillMarkdown({ ...readSkill, text: document }), document)
  const header = '<skill>\r\n<name>fixture</name>\r\n<path>/skills/fixture/SKILL.md</path>\r\n'
  const text = header + document + '\r\n</skill>'
  const section = { ...readSkill, evidence: 'injected_body', text }
  assert.equal(promptSkillMarkdown(section), document)
  assert.equal(promptSkillMarkdown({ ...section, name: 'another' }), undefined)
  assert.equal(promptSkillMarkdown({ ...section, path: '/other/SKILL.md' }), undefined)
  assert.equal(promptSkillMarkdown({ ...section, text: 'Example\n' + text }), undefined)
  assert.equal(promptSkillMarkdown({ ...readSkill, text }), text)
  assert.equal(promptSkillMarkdown({ ...section, text: header + '<content>' + document + '</content>\r\n</skill>' }), document)
  assert.equal(promptSkillMarkdown({ ...readSkill, text: undefined }), undefined)
})

test('flags incomplete loaded evidence while retaining its observed text', () => {
  const headerOnly = '---\nname: fixture\ndescription: test\n---\n'
  const complete = headerOnly + '\nUse the test fixture.\n\n---\n'
  assert.equal(promptSkillMarkdownIsPartial(readSkill, headerOnly), true)
  assert.equal(promptSkillMarkdownIsPartial(readSkill, complete), false)
  assert.equal(promptSkillMarkdownIsPartial(readSkill, complete + '…17 chars truncated…'), true)
  const header = '<skill>\n<name>fixture</name>\n<path>/skills/fixture/SKILL.md</path>\n'
  const fragment = 'x'.repeat(65533 - header.length) + '…'
  const truncated = { ...readSkill, evidence: 'injected_body', text: header + fragment }
  assert.equal(promptSkillMarkdown(truncated), fragment)
  assert.equal(promptSkillMarkdownIsPartial(truncated, fragment), true)
  const clippedDocument = 'x'.repeat(65533) + '…'
  assert.equal(promptSkillMarkdownIsPartial({ ...readSkill, text: clippedDocument }, clippedDocument), true)
})

test('rejects unsafe or ambiguous folder names and empty content', () => {
  for (const invalid of [undefined, '', '.', '..', '../skill', 'skill/extra', 'skill\\extra', 'C:skill', 'NUL',
    'com1.log', 'lpt²', 'a\u0000b', 'a.', 'a ', '验'.repeat(70)]) {
    assert.equal(promptSkillDownloadFileName(invalid), undefined)
  }
  assert.equal(promptSkillDownloadFileName('验证-skill_1'), '验证-skill_1.zip')
  assert.equal(createPromptSkillDownload('../unsafe', 'fixture'), undefined)
  assert.equal(createPromptSkillDownload('fixture', ' '), undefined)
})

test('writes a UTF-8 archive containing one named SKILL.md with valid offsets and CRC', async () => {
  const result = createPromptSkillDownload('验证-skill', '123456789')
  assert.equal(result.fileName, '验证-skill.zip')
  assert.equal(result.blob.type, 'application/zip')
  const buffer = await result.blob.arrayBuffer()
  const bytes = new Uint8Array(buffer)
  const view = new DataView(buffer)
  const read16 = (offset) => view.getUint16(offset, true)
  const read32 = (offset) => view.getUint32(offset, true)
  const decoder = new TextDecoder()
  const end = bytes.length - 22
  const directory = read32(end + 16)
  assert.equal(read32(0), 0x04034b50)
  assert.equal(read16(6), 0x0800)
  assert.equal(read16(8), 0)
  assert.equal(read32(14), 0xcbf43926)
  assert.equal(read32(18), 9)
  assert.equal(read32(22), 9)
  const nameLength = read16(26)
  assert.equal(decoder.decode(bytes.subarray(30, 30 + nameLength)), '验证-skill/SKILL.md')
  assert.equal(decoder.decode(bytes.subarray(30 + nameLength, directory)), '123456789')
  assert.equal(read32(directory), 0x02014b50)
  assert.equal(read32(directory + 16), 0xcbf43926)
  assert.equal(read32(directory + 42), 0)
  assert.equal(decoder.decode(bytes.subarray(directory + 46, end)), '验证-skill/SKILL.md')
  assert.equal(read32(end), 0x06054b50)
  assert.equal(read16(end + 10), 1)
  assert.equal(read32(end + 12), end - directory)
})
