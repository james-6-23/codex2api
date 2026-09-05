import { createContext, type PropsWithChildren, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import { api } from './api'
import type { UpstreamChannel } from './types'
import {
  FALLBACK_VISIBLE_CHANNEL,
  normalizeVisibleChannels,
  readCachedVisibleChannels,
  writeCachedVisibleChannels,
} from './lib/visibleChannels'

// 管理台可见渠道：仪表盘与账号管理只展示这里勾选的上游渠道。
// 初始值取 localStorage 缓存避免首屏闪一下再隐藏，随后向后端拉取权威值。

type VisibleChannelsContextValue = {
  channels: UpstreamChannel[]
  isChannelVisible: (channel: UpstreamChannel) => boolean
  saveChannels: (next: readonly UpstreamChannel[]) => Promise<UpstreamChannel[]>
  refresh: () => Promise<void>
}

const VisibleChannelsContext = createContext<VisibleChannelsContextValue | null>(null)

export function VisibleChannelsProvider({ children }: PropsWithChildren) {
  const [channels, setChannels] = useState<UpstreamChannel[]>(() => readCachedVisibleChannels())

  const apply = useCallback((next: unknown) => {
    const normalized = normalizeVisibleChannels(next)
    setChannels(normalized)
    writeCachedVisibleChannels(normalized)
    return normalized
  }, [])

  const refresh = useCallback(async () => {
    try {
      const res = await api.getVisibleChannels()
      apply(res.channels)
    } catch {
      // 读不到就沿用缓存/默认全部显示：可见性只是展示偏好，不该挡住管理台
    }
  }, [apply])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const saveChannels = useCallback(async (next: readonly UpstreamChannel[]) => {
    const res = await api.updateVisibleChannels(normalizeVisibleChannels(next))
    return apply(res.channels)
  }, [apply])

  const value = useMemo<VisibleChannelsContextValue>(() => ({
    channels,
    isChannelVisible: (channel) => channel === FALLBACK_VISIBLE_CHANNEL || channels.includes(channel),
    saveChannels,
    refresh,
  }), [channels, refresh, saveChannels])

  return <VisibleChannelsContext.Provider value={value}>{children}</VisibleChannelsContext.Provider>
}

export function useVisibleChannels() {
  const context = useContext(VisibleChannelsContext)
  if (!context) {
    throw new Error('useVisibleChannels must be used inside VisibleChannelsProvider')
  }
  return context
}
