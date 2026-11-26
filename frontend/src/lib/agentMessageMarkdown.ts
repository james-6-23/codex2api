// 回复渲染使用 react-markdown 的 skipHtml：未知 HTML 标签会被整体丢弃。
// 多 Agent 场景（Claude Code SendMessage / Codex multi-agent）里，子 Agent 的消息会被包成
//   <agent-message from="general-purpose"> ... </agent-message>
// 被 skipHtml 吞掉后，后台只剩空白或丢失正文。这里在交给 Markdown 之前，把该标签转成
// 带来源标注的引用块：标签语义可见、正文仍按 Markdown 渲染。
//
// - 围栏代码块（``` / ~~~）内的内容原样保留，不做转换。
// - 未闭合的标签（流式 / 截断预览）视为一直延伸到所在非代码段落末尾。
// - 孤立的 </agent-message> 直接去掉，避免被 skipHtml 吞成空行以外的副作用。

const AGENT_MESSAGE_BLOCK = /<agent-message(\s[^>]*)?>([\s\S]*?)(?:<\/agent-message\s*>|$)/gi
const AGENT_MESSAGE_CLOSE = /<\/agent-message\s*>/gi
const FROM_ATTRIBUTE = /\bfrom\s*=\s*(?:"([^"]*)"|'([^']*)')/i
const FENCE_OPEN = /^[ \t]{0,3}(`{3,}|~{3,})/

interface Segment {
  text: string
  code: boolean
}

function splitByFence(text: string): Segment[] {
  const lines = text.split('\n')
  const segments: Segment[] = []
  let buffer: string[] = []
  let fence: string | null = null

  const flush = (code: boolean) => {
    if (buffer.length === 0) return
    segments.push({ text: buffer.join('\n'), code })
    buffer = []
  }

  for (const line of lines) {
    if (fence === null) {
      const open = FENCE_OPEN.exec(line)
      if (open) {
        flush(false)
        fence = open[1]
        buffer.push(line)
      } else {
        buffer.push(line)
      }
      continue
    }
    buffer.push(line)
    const trimmed = line.trim()
    if (trimmed.length >= fence.length && trimmed[0] === fence[0] && trimmed === trimmed[0].repeat(trimmed.length)) {
      flush(true)
      fence = null
    }
  }
  flush(fence !== null)
  return segments
}

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

export function formatAgentMessageTags(text: string): string {
  if (!text || !/<\/?agent-message/i.test(text)) return text
  return splitByFence(text).map((segment) => {
    if (segment.code) return segment.text
    return segment.text
      .replace(AGENT_MESSAGE_BLOCK, (_match, attributes: string | undefined, body: string) => renderBlock(attributes, body))
      .replace(AGENT_MESSAGE_CLOSE, '')
  }).join('\n')
}
