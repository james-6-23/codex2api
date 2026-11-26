// 回复渲染使用 react-markdown 的 skipHtml：未知 HTML 标签会被整体丢弃。
// 多 Agent 场景（Claude Code SendMessage / Codex multi-agent）里，子 Agent 的消息会被包成
//   <agent-message from="general-purpose"> ... </agent-message>
// 被 skipHtml 吞掉后，后台只剩空白或丢失正文。这里在交给 Markdown 之前，把该标签转成
// 带来源标注的引用块：标签语义可见、正文仍按 Markdown 渲染。
//
// - 逐行扫描并维护围栏（``` / ~~~）状态：围栏内的内容（包括其中的标签文本）原样保留；
//   位于标签正文中的围栏代码块属于该标签，会随正文一起进入引用块，不会把引用截断。
//   围栏的开/闭缩进遵循 CommonMark：最多 3 个空格，缩进 4 个及以上不算围栏；
//   反引号围栏的信息串含反引号时不是围栏；围栏紧跟在开标签同一行时同样识别。
// - 行内代码（`...`）中的标签保持字面显示，不转换。
// - 开标签允许跨多行书写（属性中含换行）；超过上限仍未出现 ">" 时按普通文本处理。
// - 未闭合的标签（流式 / 截断预览）视为一直延伸到文本末尾；新的开标签会结束上一个未闭合块。
// - 自闭合标签 <agent-message from="x"/> 生成只含标题的引用块，不吞后续文本。
// - 孤立的 </agent-message> 直接去掉。

const TAG = /<agent-message(\s[^>]*?)?(\/)?>|<\/agent-message\s*>/gi
// 行尾处于“开标签尚未写完”的状态：<agent-message 或 <agent-message ...（没有 ">"）
const INCOMPLETE_OPEN_TAG = /<agent-message(?:\s[^>]*)?$/i
const FROM_ATTRIBUTE = /\bfrom\s*=\s*(?:"([^"]*)"|'([^']*)')/i
// CommonMark 围栏开头：反引号围栏的信息串（info string）不能再含反引号，波浪线围栏不受此限制。
const FENCE_OPEN = /^ {0,3}(?:(`{3,})[^`]*|(~{3,})[^\n]*)$/
const MAX_OPEN_TAG_LINES = 8

function fenceMarker(line: string): string | null {
  const match = FENCE_OPEN.exec(line)
  return match ? (match[1] ?? match[2]) : null
}

type Range = [start: number, end: number]

function quote(body: string): string {
  return body.split('\n').map((line) => (line.trim() === '' ? '>' : `> ${line}`)).join('\n')
}

function renderBlock(attributes: string | undefined, body: string): string {
  const from = attributes ? FROM_ATTRIBUTE.exec(attributes) : null
  const sender = (from?.[1] ?? from?.[2] ?? '').replace(/[`\r\n]/g, ' ').trim()
  const header = sender ? `**agent-message** · from \`${sender}\`` : '**agent-message**'
  const content = body.replace(/^\s*\n/, '').trimEnd()
  const lines = content.trim() === '' ? header : `${header}\n\n${content}`
  return `\n\n${quote(lines)}\n\n`
}

function closesFence(line: string, fence: string): boolean {
  // CommonMark：闭合围栏最多缩进 3 个空格（制表符视为 4 个空格），因此必须先看原始缩进。
  const indent = /^[ \t]*/.exec(line)?.[0] ?? ''
  if (indent.includes('\t') || indent.length > 3) return false
  const marker = line.trim()
  return marker.length >= fence.length && marker[0] === fence[0] && marker === marker[0].repeat(marker.length)
}

// 返回一行（或跨行拼接文本）中所有行内代码的 [start, end) 区间。
// 规则：N 个反引号开头，到下一个恰好 N 个反引号的串结束；找不到结束串则这些反引号是字面量。
function codeSpanRanges(text: string): Range[] {
  const ranges: Range[] = []
  let i = 0
  while (i < text.length) {
    const ch = text[i]
    if (ch === '\\') {
      i += 2
      continue
    }
    if (ch !== '`') {
      i += 1
      continue
    }
    let runEnd = i
    while (text[runEnd] === '`') runEnd += 1
    const runLength = runEnd - i
    let cursor = runEnd
    let spanEnd = -1
    while (cursor < text.length) {
      if (text[cursor] !== '`') {
        cursor += 1
        continue
      }
      let closeEnd = cursor
      while (text[closeEnd] === '`') closeEnd += 1
      if (closeEnd - cursor === runLength) {
        spanEnd = closeEnd
        break
      }
      cursor = closeEnd
    }
    if (spanEnd < 0) {
      i = runEnd
      continue
    }
    ranges.push([i, spanEnd])
    i = spanEnd
  }
  return ranges
}

function insideRange(ranges: Range[], index: number): boolean {
  return ranges.some(([start, end]) => index >= start && index < end)
}

export function formatAgentMessageTags(text: string): string {
  if (!text || !/<\/?agent-message/i.test(text)) return text

  let out = ''
  let body = ''
  let attributes: string | undefined
  let collecting = false
  let fence: string | null = null
  let pending: string | null = null // 尚未写完的开标签（可能跨多行）

  const append = (value: string) => {
    if (collecting) body += value
    else out += value
  }
  const finish = () => {
    out += renderBlock(attributes, body)
    collecting = false
    body = ''
    attributes = undefined
  }

  // 处理一段不含围栏的正文：把标签转成引用块，行内代码中的标签保持原样。
  const processProse = (segment: string) => {
    const spans = codeSpanRanges(segment)
    let cursor = 0
    TAG.lastIndex = 0
    for (let match = TAG.exec(segment); match !== null; match = TAG.exec(segment)) {
      if (insideRange(spans, match.index)) continue
      append(segment.slice(cursor, match.index))
      cursor = match.index + match[0].length
      if (match[0].startsWith('</')) {
        if (collecting) finish()
        continue
      }
      if (collecting) finish()
      if (match[2]) {
        out += renderBlock(match[1], '')
      } else {
        collecting = true
        attributes = match[1]
        // 围栏紧跟在开标签之后（同一行）：生成引用块后它位于正文行首，是合法围栏，
        // 其内容（包括其中的标签文本）必须保持字面。
        const rest = segment.slice(cursor)
        const marker = fenceMarker(rest)
        if (marker) {
          fence = marker
          append(rest)
          return
        }
      }
    }
    append(segment.slice(cursor))
  }

  const lines = text.split('\n')
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    const lastLine = index === lines.length - 1

    if (fence !== null) {
      append(line)
      if (closesFence(line, fence)) fence = null
      if (!lastLine) append('\n')
      continue
    }

    const marker = fenceMarker(line)
    if (marker) {
      if (pending !== null) {
        append(`${pending}\n`)
        pending = null
      }
      fence = marker
      append(line)
      if (!lastLine) append('\n')
      continue
    }

    const work: string = pending !== null ? `${pending}\n${line}` : line
    pending = null
    const partial = INCOMPLETE_OPEN_TAG.exec(work)
    if (partial && !insideRange(codeSpanRanges(work), partial.index) && work.split('\n').length <= MAX_OPEN_TAG_LINES) {
      processProse(work.slice(0, partial.index))
      pending = work.slice(partial.index)
      continue
    }

    processProse(work)
    if (!lastLine) append('\n')
  }

  if (pending !== null) append(pending)
  if (collecting) finish()
  return out
}
