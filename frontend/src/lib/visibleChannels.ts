import type { UpstreamChannel } from '../types'

// 管理台可见渠道：与后端 database.AllUpstreamChannels / FallbackVisibleChannel 保持一致。
export const ALL_VISIBLE_CHANNEL_OPTIONS: readonly UpstreamChannel[] = ['codex', 'claude', 'antigravity', 'grok']
export const FALLBACK_VISIBLE_CHANNEL: UpstreamChannel = 'codex'
export const VISIBLE_CHANNELS_STORAGE_KEY = 'codex2api:visible-channels'

const isUpstreamChannel = (value: unknown): value is UpstreamChannel =>
  typeof value === 'string' && (ALL_VISIBLE_CHANNEL_OPTIONS as readonly string[]).includes(value)

// 与后端规范化规则一致：null/undefined 视为全部显示；未知项丢弃；兜底渠道始终在列；固定顺序。
export function normalizeVisibleChannels(input: unknown): UpstreamChannel[] {
  if (input === null || input === undefined) return [...ALL_VISIBLE_CHANNEL_OPTIONS]
  if (!Array.isArray(input)) return [...ALL_VISIBLE_CHANNEL_OPTIONS]
  const wanted = new Set<UpstreamChannel>([FALLBACK_VISIBLE_CHANNEL])
  for (const item of input) {
    const name = typeof item === 'string' ? item.trim().toLowerCase() : ''
    if (isUpstreamChannel(name)) wanted.add(name)
  }
  return ALL_VISIBLE_CHANNEL_OPTIONS.filter((channel) => wanted.has(channel))
}

export function toggleVisibleChannel(current: readonly UpstreamChannel[], channel: UpstreamChannel): UpstreamChannel[] {
  if (channel === FALLBACK_VISIBLE_CHANNEL) return normalizeVisibleChannels(current)
  const next = current.includes(channel)
    ? current.filter((item) => item !== channel)
    : [...current, channel]
  return normalizeVisibleChannels(next)
}

export function readCachedVisibleChannels(): UpstreamChannel[] {
  try {
    const raw = window.localStorage.getItem(VISIBLE_CHANNELS_STORAGE_KEY)
    if (!raw) return normalizeVisibleChannels(null)
    return normalizeVisibleChannels(JSON.parse(raw))
  } catch {
    return normalizeVisibleChannels(null)
  }
}

export function writeCachedVisibleChannels(channels: readonly UpstreamChannel[]) {
  try {
    window.localStorage.setItem(VISIBLE_CHANNELS_STORAGE_KEY, JSON.stringify(channels))
  } catch {
    // 私密模式等写不进去时忽略：下次进入会重新向后端拉取
  }
}
