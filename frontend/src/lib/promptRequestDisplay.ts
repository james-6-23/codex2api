import type { PromptRequestContextSection } from '../types'

type Translate = (key: string, fallback?: string) => string
type ObjectValue = Record<string, unknown>

const inputFields = ['input', 'messages', 'contents']
const instructionFields = ['instructions', 'system', 'systemInstruction', 'system_instruction']
const opaqueFields = new Set(['encrypted_content', 'encryptedContent', 'internal_chat_message_metadata_passthrough'])

function objectValue(value: unknown): ObjectValue | undefined {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as ObjectValue : undefined
}

function structuredText(text: string): unknown {
  const trimmed = text.trim()
  if (!trimmed.startsWith('{') && !trimmed.startsWith('[')) return undefined
  try { return JSON.parse(trimmed) as unknown } catch { return undefined }
}

function indent(text: string) {
  return text.split('\n').map((line) => line ? '  ' + line : '').join('\n')
}

function field(t: Translate, name: string) {
  return t('fields.' + name, name.replace(/_/g, ' '))
}

function block(title: string, value: string) {
  return value ? title + '\n' + indent(value) : ''
}

function textValue(value: string, t: Translate) {
  return value.replace(/\r\n?/g, '\n').replace(/^\[Image (\d+)\]$/, (_, number: string) => t('image', 'Image') + ' ' + number)
}

// Only decode tool output envelopes. Plain text and commands retain literal
// backslashes, including sequences such as C:\new used in Windows paths.
function toolText(text: string, t: Translate, depth: number): string {
  const parsed = structuredText(text)
  if (parsed !== undefined) return valueText(parsed, t, depth + 1, true)
  return textValue(text, t).split('\n').map((line) => {
    const candidate = structuredText(line)
    return candidate === undefined ? line : valueText(candidate, t, depth + 1, true)
  }).join('\n')
}

// Decode static arguments in the exec tool's direct text(await tools.*(...))
// wrapper. Treat anything dynamic as plain code, and never execute it.
function readInvocationArgument(source: string): { value: unknown; rest: string } | undefined {
  let end = 0
  let depth = 0
  let json = ''
  if (!source.startsWith('{') && !source.startsWith('"')) return undefined
  while (end < source.length) {
    const char = source[end]
    if (char === '"') {
      const literal = source.slice(end).match(/^"(?:[^"\\]|\\[\s\S])*"/)
      if (!literal) return undefined
      json += literal[0]
      end += literal[0].length
      if (depth === 0) break
      continue
    }
    if (char === '{' || char === '[') depth++
    if (char === '}' || char === ']') depth--
    const field = (char === '{' || char === ',') && source.slice(end + 1).match(/^\s*([A-Za-z_$][\w$]*)\s*:/)
    if (field) {
      json += char + JSON.stringify(field[1]) + ':'
      end += 1 + field[0].length
      continue
    }
    json += char
    end++
    if (depth === 0) break
  }
  try { return { value: JSON.parse(json) as unknown, rest: source.slice(end) } } catch { return undefined }
}

function wrappedInvocations(source: string, t: Translate): string {
  const calls: string[] = []
  let rest = source.trim()
  while (rest) {
    const prefix = rest.match(/^text\(\s*await\s+tools\.([\w$]+)\(\s*/)
    if (!prefix) return ''
    const argument = readInvocationArgument(rest.slice(prefix[0].length))
    const ending = argument?.rest.match(/^\s*\)\s*\)\s*;?\s*/)
    if (!argument || !ending) return ''
    calls.push(block(t('tools.' + prefix[1], prefix[1]), valueText(argument.value, t, 0)))
    rest = argument.rest.slice(ending[0].length)
  }
  return calls.join('\n\n')
}

function toolCallValue(value: unknown, name: unknown, t: Translate) {
  if (name === 'exec' && typeof value === 'string') {
    const wrapped = wrappedInvocations(value, t)
    if (wrapped) return wrapped
  }
  return valueText(value, t, 0, true)
}

function valueText(value: unknown, t: Translate, depth: number, tool = false): string {
  if (value === null || value === undefined) return ''
  if (depth >= 25) return t('nested', 'Additional nested content')
  if (typeof value === 'string') return tool ? toolText(value, t, depth) : textValue(value, t)
  if (typeof value === 'boolean') return value ? t('yes', 'Yes') : t('no', 'No')
  if (typeof value !== 'object') return String(value)
  if (Array.isArray(value)) {
    if (value.every((item) => objectValue(item)?.type === 'text')) {
      return value.map((item) => valueText(objectValue(item)?.text, t, depth + 1, tool)).filter(Boolean).join('\n')
    }
    return value.map((item) => {
      const text = valueText(item, t, depth + 1, tool)
      if (!text) return ''
      const [first, ...rest] = text.split('\n')
      return '• ' + first + (rest.length ? '\n' + indent(rest.join('\n')) : '')
    }).filter(Boolean).join('\n')
  }

  const item = objectValue(value)!
  if (item.type === 'text' && typeof item.text === 'string') return valueText(item.text, t, depth + 1, tool)
  return Object.entries(item).filter(([key]) => !opaqueFields.has(key)).map(([key, child]) => {
    const rendered = valueText(child, t, depth + 1, tool)
    if (!rendered) return ''
    const label = field(t, key)
    return rendered.includes('\n') ? block(label, rendered) : label + '：' + rendered
  }).filter(Boolean).join('\n')
}

function contentText(value: unknown, t: Translate): string {
  if (Array.isArray(value)) return value.map((item) => contentText(item, t)).filter(Boolean).join('\n')
  const item = objectValue(value)
  if (!item) return valueText(value, t, 0)
  if (typeof item.text === 'string') return textValue(item.text, t)
  return formatItem(item, t)
}

function toolTitle(t: Translate, kind: 'toolCall' | 'toolResult', name: unknown) {
  return t(kind, kind === 'toolCall' ? 'Tool call' : 'Tool result') +
    (typeof name === 'string' && name ? ' · ' + t('tools.' + name, name) : '')
}

function messageToolCalls(item: ObjectValue, t: Translate): string {
  if (!Array.isArray(item.tool_calls)) return ''
  return item.tool_calls.map((call) => {
    const fn = objectValue(objectValue(call)?.function)
    return fn ? block(toolTitle(t, 'toolCall', fn.name), valueText(fn.arguments, t, 0, true)) : formatItem(call, t)
  }).filter(Boolean).join('\n')
}

function formatItem(value: unknown, t: Translate): string {
  const item = objectValue(value)
  if (!item) return contentText(value, t)
  const type = typeof item.type === 'string' ? item.type : ''
  const role = typeof item.role === 'string' ? item.role : ''
  if (type === 'reasoning' || type === 'compaction' || type === 'context_compaction' || type === 'encrypted_content') {
    const summary = contentText(item.summary, t)
    return block(t('types.' + type, type), summary || t('opaque', 'Encoded context'))
  }
  if (type === 'image' || type === 'image_url' || type === 'input_image' || type === 'computer_screenshot') {
    const target = item.image_url ?? item.source ?? item.file_id ?? item.url
    const content = valueText(target, t, 0)
    return content ? block(t('image', 'Image'), content) : t('image', 'Image')
  }
  if (type.endsWith('_output') || type === 'tool_result' || role === 'tool' || role === 'function' || item.functionResponse) {
    return block(toolTitle(t, 'toolResult', item.name),
      valueText(item.output ?? item.content ?? item.text ?? item.functionResponse, t, 0, true))
  }
  if (type.endsWith('_call') || type === 'tool_use' || item.functionCall) {
    return block(toolTitle(t, 'toolCall', item.name),
      toolCallValue(item.arguments ?? item.input ?? item.action ?? item.query ?? item.functionCall, item.name, t))
  }
  const content = item.content ?? item.parts ?? item.text
  if (role || content !== undefined) {
    const body = [contentText(content, t), messageToolCalls(item, t)].filter(Boolean).join('\n')
    return role ? block(t('roles.' + role, role), body) : body
  }
  return valueText(item, t, 0)
}

export function formatPromptRequestSections(text: string, t: Translate): PromptRequestTextPart[] {
  const value = structuredText(text)
  if (Array.isArray(value) && value.every((part) => typeof objectValue(part)?.name === 'string' && 'value' in objectValue(part)!)) {
    const formatted = value.map((part) => {
      const { name, filename, value } = objectValue(part)!
      const label = field(t, String(name)) + (typeof filename === 'string' && filename ? ' · ' + filename : '')
      return block(label, valueText(value, t, 0))
    }).filter(Boolean).join('\n\n')
    return formatted ? [{ text: formatted }] : []
  }
  const fields = objectValue(value)
  if (!fields) return instructionSections(value !== undefined ? valueText(value, t, 0) : textValue(text, t))
  const sections = [
    ...instructionFields.flatMap((name) => instructionFieldSections(fields[name], t)),
    ...inputFields.flatMap((name) => {
      const items = fields[name]
      return Array.isArray(items)
        ? items.flatMap((item) => formatRequestItemSections(item, t))
        : instructionSections(contentText(items, t), undefined, (value) => block(field(t, name), value))
    }),
  ]
  const other = Object.fromEntries(Object.entries(fields).filter(([key]) =>
    !instructionFields.includes(key) && !inputFields.includes(key)))
  const metadata = block(t('context', 'Other request fields'), valueText(other, t, 0))
  if (metadata) sections.push({ text: metadata })

  const merged: PromptRequestTextPart[] = []
  const plain: string[] = []
  for (const part of sections) {
    if (!part.context) {
      if (part.text) plain.push(part.text)
      continue
    }
    if (plain.length) merged.push({ text: plain.splice(0).join('\n\n') })
    merged.push(part)
  }
  if (plain.length) merged.push({ text: plain.join('\n\n') })
  return merged
}

export function formatPromptRequestChange(text: string, kind: string, t: Translate, name?: string): string {
  if (kind === 'tool_call') return toolCallValue(text, name, t)
  if (kind === 'tool_result') return toolText(text, t, 0)
  if (kind === 'context') return valueText(structuredText(text) ?? text, t, 0)
  return textValue(text, t)
}

type InstructionRoleKind = 'systemPrompt' | 'developerPrompt'

export interface PromptRequestContextLabel {
  kind: 'agents' | 'availableSkills' | 'loadedSkill' | InstructionRoleKind
  name?: string
  path?: string
  skills?: { name: string; path?: string }[]
}

export interface PromptRequestTextPart {
  text: string
  context?: PromptRequestContextLabel
}

const projectedKinds = {
  systemPrompt: 'system_prompt',
  developerPrompt: 'developer_prompt',
  agents: 'agents_md',
  availableSkills: 'available_skills',
  loadedSkill: 'loaded_skill',
} satisfies Record<PromptRequestContextLabel['kind'], PromptRequestContextSection['kind']>

export function promptRequestContextCoversPart(part: PromptRequestTextPart, sections: PromptRequestContextSection[]): boolean {
  if (!part.context) return false
  const kind = projectedKinds[part.context.kind]
  const original = part.text.replace(/\r\n?/g, '\n').trim()
  if (!original) return false
  return sections.some((section) => {
    if (section.kind !== kind || !section.text) return false
    const projected = section.text.replace(/\r\n?/g, '\n').trim()
    return projected === original ||
      (section.state !== 'inherited' && `\n${projected}\n`.includes(`\n${original}\n`))
  })
}

const instructionRoles = new Set(['', 'developer', 'system', 'user'])

function instructionRoleKind(role: string): InstructionRoleKind | undefined {
  if (role === 'system') return 'systemPrompt'
  if (role === 'developer') return 'developerPrompt'
}

function instructionSections(
  text: string,
  role?: InstructionRoleKind,
  formatPlain: (value: string) => string = (value) => value,
  standaloneUserSkills = false,
): PromptRequestTextPart[] {
  const trimmed = standaloneUserSkills ? text.replace(/\r\n?/g, '\n').trim() : ''
  return splitPromptRequestInstructions(text)
    .filter((part) => part.text.trim())
    .map((part) => {
      if (part.context && (!standaloneUserSkills || part.context.kind !== 'loadedSkill' || part.text.trim() === trimmed)) return part
      if (role) return { text: part.text, context: { kind: role } }
      return { text: formatPlain(part.text) }
    })
}

function instructionFieldSections(value: unknown, t: Translate): PromptRequestTextPart[] {
  const item = objectValue(value)
  const content = item?.content ?? item?.parts ?? item?.text ?? value
  return instructionSections(contentText(content, t), 'systemPrompt')
}

function formatRequestItemSections(value: unknown, t: Translate): PromptRequestTextPart[] {
  const item = objectValue(value)
  const role = typeof item?.role === 'string' ? item.role : ''
  if (!item || !instructionRoles.has(role) || (item.type && item.type !== 'message')) {
    return [{ text: formatItem(value, t) }]
  }
  const content = item.content ?? item.parts ?? item.text
  if (content === undefined) return [{ text: formatItem(value, t) }]
  const sections = instructionSections(contentText(content, t), instructionRoleKind(role), (value) =>
    role ? block(t('roles.' + role, role), value) : value, role === 'user' || role === '')
  const calls = messageToolCalls(item, t)
  if (calls) sections.push({ text: calls })
  return sections
}

function skillCatalog(text: string): { name: string; path?: string }[] {
  const heading = /^#{2,4}[ \t]+Available skills[ \t]*\n/im.exec(text)
  if (heading) {
    const rest = text.slice(heading.index + heading[0].length)
    const end = rest.search(/^#{1,4}[ \t]+/m)
    return [...(end === -1 ? rest : rest.slice(0, end)).matchAll(/^[ \t]*-[ \t]+([^:\n]+):[^\n]*?\((?:file:[ \t]*)?([^\n]+?[/\\]SKILL\.md)\)[ \t]*$/gm)]
      .map(([, name, path]) => ({ name: name.trim(), path: path.trim() }))
  }
  return [...text.matchAll(/<skill>[\s\S]*?<name>([^<>\n]+)<\/name>([\s\S]*?)<\/skill>/g)]
    .map(([, name, fields]) => ({ name: name.trim(), path: /<(?:path|location)>([^<>\n]+)<\/(?:path|location)>/.exec(fields)?.[1].trim() }))
}

export function splitPromptRequestInstructions(source: string): PromptRequestTextPart[] {
  const text = source.replace(/\r\n?/g, '\n')
  const spans: { start: number; end: number; context: PromptRequestContextLabel }[] = []

  for (const match of text.matchAll(/^# AGENTS\.md instructions for[ \t]+([^\n]+)\n[ \t\n]*<INSTRUCTIONS>[ \t]*\n([\s\S]*?)^<\/INSTRUCTIONS>[ \t]*$/gm)) {
    if (!match[2].trim()) continue
    const directory = match[1].trim()
    const path = /(?:^|[/\\])AGENTS\.md$/.test(directory) ? directory : directory.replace(/[/\\]$/, '') + '/AGENTS.md'
    spans.push({ start: match.index, end: match.index + match[0].length, context: { kind: 'agents', path } })
  }
  for (const match of text.matchAll(/^<(skills_instructions|available_skills)>[ \t]*\n([\s\S]*?)^<\/\1>[ \t]*$/gm)) {
    const skills = skillCatalog(match[2])
    if (skills.length) spans.push({ start: match.index, end: match.index + match[0].length, context: { kind: 'availableSkills', skills } })
  }
  for (const heading of text.matchAll(/^#{2,4}[ \t]+Available skills[ \t]*\n/gim)) {
    const start = heading.index
    const restStart = start + heading[0].length
    const nextSection = text.slice(restStart).search(/^(?:#{1,4}[ \t]+|<\/?[a-z_]+>[ \t]*$)/im)
    const end = nextSection === -1 ? text.length : restStart + nextSection
    const skills = skillCatalog(text.slice(start, end))
    if (skills.length) spans.push({ start, end, context: { kind: 'availableSkills', skills } })
  }
  for (const match of text.matchAll(/^<skill>[ \t]*\n[ \t]*<name>([^<>\n]+)<\/name>[ \t]*\n[ \t]*<path>([^<>\n]+)<\/path>[ \t]*\n([\s\S]*?)^<\/skill>[ \t]*$/gm)) {
    const path = match[2].trim()
    const content = match[3].replace(/<\/?content>/g, '').trim()
    if (!/(?:^|[/\\])SKILL\.md$/.test(path) || !content || /^<(?:description|location)>/.test(content)) continue
    spans.push({ start: match.index, end: match.index + match[0].length, context: { kind: 'loadedSkill', name: match[1].trim(), path } })
  }

  const parts: PromptRequestTextPart[] = []
  let offset = 0
  for (const { start, end, context } of spans.sort((a, b) => a.start - b.start || b.end - a.end)) {
    if (start < offset) continue
    if (start > offset) parts.push({ text: text.slice(offset, start) })
    parts.push({ text: text.slice(start, end), context })
    offset = end
  }
  if (offset < text.length) parts.push({ text: text.slice(offset) })
  return parts
}

export function formatPromptRequestChangeSections(text: string, kind: string, t: Translate, label = ''): PromptRequestTextPart[] {
  if (kind === 'text' && instructionRoles.has(label)) {
    return instructionSections(textValue(text, t), instructionRoleKind(label), undefined, label === 'user' || label === '')
  }
  if (kind === 'context' && instructionFields.includes(label)) {
    return instructionFieldSections(structuredText(text) ?? text, t)
  }
  return [{ text: formatPromptRequestChange(text, kind, t, label) }]
}

function readCommandPath(name: string, args: string): string | undefined {
  let command = objectValue(structuredText(args))?.cmd
  if (name === 'exec') {
    const prefix = /^text\(\s*await\s+tools\.exec_command\(\s*/.exec(args)
    const invocation = prefix && readInvocationArgument(args.slice(prefix[0].length))
    if (!invocation || !/^\s*\)\s*\)\s*;?\s*$/.test(invocation.rest)) return undefined
    command = objectValue(invocation.value)?.cmd
  }
  if (typeof command !== 'string') return undefined
  // An exact cat command is evidence only when its matching output confirms success.
  const read = /^cat[ \t]+(?:--[ \t]+)?(?:'([^'\r\n]+)'|"([^"$\x60\\\r\n]+)"|([/\w.-]+))[ \t]*$/.exec(command)
  return read?.[1] ?? read?.[2] ?? read?.[3]
}

function successfulCommandOutput(output: string): string | undefined {
  const envelope = structuredText(output)
  if (Array.isArray(envelope)) {
    if (envelope.length !== 1) return undefined
    const text = objectValue(envelope[0])?.text
    return typeof text === 'string' ? successfulCommandOutput(text) : undefined
  }
  const fields = objectValue(envelope)
  if (fields?.exit_code === 0 && typeof fields.output === 'string') return fields.output
  const header = /^Chunk ID:[^\n]*\nWall time:[^\n]*\nProcess exited with code 0\n(?:Original token count:[^\n]*\n)?(?:Final )?Output:\n/i.exec(output)
  return header ? output.slice(header[0].length) : undefined
}

export function promptRequestReadInstruction(name: string, args: string, output: string): PromptRequestTextPart | undefined {
  if (name !== 'exec' && name !== 'exec_command') return undefined
  const path = readCommandPath(name, args)
  if (!path || !/(?:^|[/\\])(?:AGENTS|SKILL)\.md$/.test(path)) return undefined
  const body = successfulCommandOutput(output.replace(/\r\n?/g, '\n'))
  if (!body?.trim()) return undefined
  if (/(?:^|[/\\])AGENTS\.md$/.test(path)) return { text: body, context: { kind: 'agents', path } }
  const frontMatter = /^---\n([\s\S]*?)\n---(?:\n|$)/.exec(body)
  const segments = path.split(/[/\\]/)
  const skill = frontMatter?.[1].match(/^name:[ \t]*["']?([^"'\n]+)["']?[ \t]*$/m)?.[1].trim() ??
    segments[segments.length - 2]
  if (skill) return { text: body, context: { kind: 'loadedSkill', name: skill, path } }
}
