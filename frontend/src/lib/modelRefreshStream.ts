import type { ChannelModelRefreshResult, RefreshAllModelsResponse } from '../types'

// 「刷新账号模型」SSE 事件：start（渠道待探测分组数）/ progress（每组抽样账号的结果）/ complete（汇总）。
export interface ModelRefreshProgressEvent {
  type: 'start' | 'progress'
  channel: string
  groups?: number
  current?: number
  total?: number
  plan?: string
  members?: number
  account_id?: number
  account_email?: string
  status?: 'ok' | 'failed'
  message?: string
  error?: string
  model_count?: number
  added?: string[]
}

export type ModelRefreshStreamEvent = ModelRefreshProgressEvent | RefreshAllModelsResponse

export interface ModelRefreshChannelProgress {
  channel: string
  groups: number
  current: number
  total: number
  lastPlan: string
  lastAccount: string
  lastStatus: 'ok' | 'failed' | ''
  lastError: string
  added: string[]
  failed: number
  done: boolean
  error: string
}

export type ModelRefreshProgress = Record<string, ModelRefreshChannelProgress>

function emptyChannelProgress(channel: string): ModelRefreshChannelProgress {
  return { channel, groups: 0, current: 0, total: 0, lastPlan: '', lastAccount: '', lastStatus: '', lastError: '', added: [], failed: 0, done: false, error: '' }
}

// 把一条流事件合并进进度状态；返回新对象以便 React 触发渲染。
export function applyModelRefreshEvent(prev: ModelRefreshProgress, event: ModelRefreshStreamEvent): ModelRefreshProgress {
  const next: ModelRefreshProgress = { ...prev }
  if (event.type === 'complete') {
    for (const ch of (event as RefreshAllModelsResponse).channels as ChannelModelRefreshResult[]) {
      const cur = next[ch.channel] ?? emptyChannelProgress(ch.channel)
      next[ch.channel] = { ...cur, done: true, error: ch.error ?? '', failed: ch.failed, added: mergeAdded(cur.added, ch.added), groups: ch.groups ?? cur.groups }
    }
    return next
  }
  const cur = next[event.channel] ?? emptyChannelProgress(event.channel)
  if (event.type === 'start') {
    next[event.channel] = { ...cur, groups: event.groups ?? 0, total: event.groups ?? 0 }
    return next
  }
  next[event.channel] = {
    ...cur,
    current: event.current ?? cur.current,
    total: event.total ?? cur.total,
    lastPlan: event.plan ?? cur.lastPlan,
    lastAccount: event.account_email ?? '',
    lastStatus: event.status ?? '',
    lastError: event.error ?? '',
    failed: cur.failed + (event.status === 'failed' ? 1 : 0),
    added: mergeAdded(cur.added, event.added ?? []),
  }
  return next
}

function mergeAdded(prev: string[], incoming: string[]): string[] {
  const seen = new Set(prev.map((m) => m.toLowerCase()))
  const out = [...prev]
  for (const m of incoming) {
    const key = m.toLowerCase()
    if (seen.has(key)) continue
    seen.add(key)
    out.push(m)
  }
  return out
}

function parseSSELine(line: string): ModelRefreshStreamEvent | null {
  const trimmed = line.trim()
  if (!trimmed.startsWith('data:')) return null
  const payload = trimmed.slice(5).trim()
  if (!payload) return null
  try {
    const parsed = JSON.parse(payload) as ModelRefreshStreamEvent
    return parsed && typeof parsed === 'object' && typeof parsed.type === 'string' ? parsed : null
  } catch {
    return null
  }
}

// 读取 SSE 响应体；每解析到一条事件就回调，返回最终的 complete 汇总（没有则 null）。
export async function readModelRefreshSSE(
  response: Response,
  onEvent: (event: ModelRefreshStreamEvent) => void,
): Promise<RefreshAllModelsResponse | null> {
  const reader = response.body?.getReader()
  if (!reader) return null
  const decoder = new TextDecoder()
  let buffer = ''
  let complete: RefreshAllModelsResponse | null = null
  const consume = (line: string) => {
    const event = parseSSELine(line)
    if (!event) return
    if (event.type === 'complete') complete = event as RefreshAllModelsResponse
    onEvent(event)
  }
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() ?? ''
    for (const line of lines) consume(line)
  }
  buffer += decoder.decode()
  if (buffer) consume(buffer)
  return complete
}
