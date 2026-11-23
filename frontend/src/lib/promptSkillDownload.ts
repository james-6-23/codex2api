import type { PromptRequestContextSection } from '../types'

const encoder = new TextEncoder()
const replayTextLimit = 64 << 10
const injectedHeader = /^<skill>[ \t]*\r?\n[ \t]*<name>([^<>\r\n]+)<\/name>[ \t]*\r?\n[ \t]*<path>([^<>\r\n]+)<\/path>[ \t]*\r?\n/
const injectedEnd = /\r?\n<\/skill>[ \t]*$/
const frontMatter = /^\ufeff?---[ \t]*\r?\n[\s\S]*?\r?\n---[ \t]*(?:\r?\n|$)/
const safeSkillName = /^[\p{L}\p{N}_-][\p{L}\p{M}\p{N}._-]*$/u
const reservedSkillName = /^(?:con|prn|aux|nul|com[1-9¹²³]|lpt[1-9¹²³])(?:\.|$)/i

function replayTextWasClipped(text: string): boolean {
  if (!text.endsWith('…')) return false
  const length = encoder.encode(text).length
  return length > replayTextLimit - 4 && length <= replayTextLimit
}

export function promptSkillMarkdown(section: PromptRequestContextSection): string | undefined {
  const text = section.text
  if (section.kind !== 'loaded_skill' || !text?.trim()) return undefined
  if (section.evidence !== 'injected_body') return text

  const envelope = text.trim()
  const header = injectedHeader.exec(envelope)
  if (!header || header[1].trim() !== section.name || header[2].trim() !== section.path) return undefined
  const end = injectedEnd.exec(envelope)
  if (!end && !replayTextWasClipped(text)) return undefined

  // Codex adds the line break before its closing tag. Keep any line breaks
  // belonging to the document itself, including a trailing newline.
  const body = envelope.slice(header[0].length, end?.index)
  const wrappedContent = /^[ \t\r\n]*<content>([\s\S]*)<\/content>[ \t\r\n]*$/.exec(body)
  const markdown = wrappedContent?.[1] ?? body
  return markdown.trim() ? markdown : undefined
}

export function promptSkillMarkdownIsPartial(section: PromptRequestContextSection, markdown: string): boolean {
  if (section.text && replayTextWasClipped(section.text)) return true
  if (section.evidence !== 'successful_read') return false
  if (/…[0-9]+ chars truncated…/.test(markdown)) return true
  // Batched tool receipts only keep the matched YAML header as evidence.
  const header = frontMatter.exec(markdown)
  return Boolean(header && !markdown.slice(header[0].length).trim())
}

export function promptSkillDownloadFileName(name?: string): string | undefined {
  if (typeof name !== 'string' || !safeSkillName.test(name) || name.endsWith('.') || reservedSkillName.test(name) ||
    encoder.encode(name).length > 200) return undefined
  return name + '.zip'
}

function crc32(bytes: Uint8Array): number {
  let crc = 0xffffffff
  for (const byte of bytes) {
    crc ^= byte
    for (let bit = 0; bit < 8; bit++) {
      crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0)
    }
  }
  return (crc ^ 0xffffffff) >>> 0
}

export function createPromptSkillDownload(name: string, markdown: string): { fileName: string; blob: Blob } | undefined {
  const fileName = promptSkillDownloadFileName(name)
  if (!fileName || !markdown.trim()) return undefined

  const entryName = encoder.encode(name + '/SKILL.md')
  const contents = encoder.encode(markdown)
  const checksum = crc32(contents)
  const directoryOffset = 30 + entryName.length + contents.length
  const directoryLength = 46 + entryName.length
  const archive = new Uint8Array(directoryOffset + directoryLength + 22)

  // A single uncompressed UTF-8 entry with a corresponding central directory.
  const local = new DataView(archive.buffer, 0, 30)
  local.setUint32(0, 0x04034b50, true)
  local.setUint16(4, 20, true)
  local.setUint16(6, 0x0800, true)
  local.setUint16(12, 0x21, true)
  local.setUint32(14, checksum, true)
  local.setUint32(18, contents.length, true)
  local.setUint32(22, contents.length, true)
  local.setUint16(26, entryName.length, true)
  archive.set(entryName, 30)
  archive.set(contents, 30 + entryName.length)

  const directory = new DataView(archive.buffer, directoryOffset, 46)
  directory.setUint32(0, 0x02014b50, true)
  directory.setUint16(4, 20, true)
  directory.setUint16(6, 20, true)
  directory.setUint16(8, 0x0800, true)
  directory.setUint16(14, 0x21, true)
  directory.setUint32(16, checksum, true)
  directory.setUint32(20, contents.length, true)
  directory.setUint32(24, contents.length, true)
  directory.setUint16(28, entryName.length, true)
  archive.set(entryName, directoryOffset + 46)

  const end = new DataView(archive.buffer, directoryOffset + directoryLength, 22)
  end.setUint32(0, 0x06054b50, true)
  end.setUint16(8, 1, true)
  end.setUint16(10, 1, true)
  end.setUint32(12, directoryLength, true)
  end.setUint32(16, directoryOffset, true)

  return { fileName, blob: new Blob([archive], { type: 'application/zip' }) }
}

export function downloadPromptSkill(name: string, markdown: string): void {
  const archive = createPromptSkillDownload(name, markdown)
  if (!archive) return
  const url = URL.createObjectURL(archive.blob)
  const link = document.createElement('a')
  link.href = url
  link.download = archive.fileName
  link.hidden = true
  document.body.append(link)
  try {
    link.click()
  } finally {
    link.remove()
    window.setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
}
