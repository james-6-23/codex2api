// 回复渲染使用 react-markdown 的 skipHtml：未知 HTML 标签会被整体丢弃。
// 多 Agent 场景（Claude Code SendMessage / Codex multi-agent）里，子 Agent 的消息会被包成
//   <agent-message from="general-purpose"> ... </agent-message>
// 被 skipHtml 吞掉后，后台只剩空白或丢失正文。这里在交给 Markdown 之前，把该标签转成
// 带来源标注的引用块：标签语义可见、正文仍按 Markdown 渲染。
//
// - 逐行扫描并维护围栏（``` / ~~~）状态：围栏内的内容（包括其中的标签文本）原样保留；
//   位于标签正文中的围栏代码块属于该标签，会随正文一起进入引用块，不会把引用截断。
// - 未闭合的标签（流式 / 截断预览）视为一直延伸到文本末尾；新的开标签会结束上一个未闭合块。
// - 自闭合标签 <agent-message from="x"/> 生成只含标题的引用块，不吞后续文本。
// - 孤立的 </agent-message> 直接去掉。

const TAG = /<agent-message(\s[^>]*?)?(\/)?>|<\/agent-message\s*>/gi
const FROM_ATTRIBUTE = /\bfrom\s*=\s*(?:"([^"]*)"|'([^']*)')/i
const FENCE_OPEN = /^[ \t]{0,3}(`{3,}|~{3,})/

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
  const trimmed = line.trim()
  return trimmed.length >= fence.length && trimmed[0] === fence[0] && trimmed === trimmed[0].repeat(trimmed.length)
}

export function formatAgentMessageTags(text: string): string {
  if (!text || !/<\/?agent-message/i.test(text)) return text

  let out = ''
  let body = ''
  let attributes: string | undefined
  let collecting = false
  let fence: string | null = null

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

  const lines = text.split('\n')
  lines.forEach((line, index) => {
    const lastLine = index === lines.length - 1

    if (fence !== null) {
      append(line)
      if (closesFence(line, fence)) fence = null
    } else {
      const open = FENCE_OPEN.exec(line)
      if (open) {
        fence = open[1]
        append(line)
      } else {
        let cursor = 0
        TAG.lastIndex = 0
        for (let match = TAG.exec(line); match !== null; match = TAG.exec(line)) {
          append(line.slice(cursor, match.index))
          cursor = match.index + match[0].length
          const isClose = match[0].startsWith('</')
          if (isClose) {
            if (collecting) finish()
            continue
          }
          if (collecting) finish()
          if (match[2]) {
            out += renderBlock(match[1], '')
          } else {
            collecting = true
            attributes = match[1]
          }
        }
        append(line.slice(cursor))
      }
    }

    if (!lastLine) append('\n')
  })

  if (collecting) finish()
  return out
}
