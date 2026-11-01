import { useEffect, useState } from 'react'
import { api } from '../api'
import APIKeyConcurrencyBadge from './APIKeyConcurrencyBadge'

const REFRESH_INTERVAL_MS = 5_000

export default function PublicAPIKeyConcurrencyBadge({ apiKey, limit }: { apiKey: string; limit?: number }) {
  const [current, setCurrent] = useState<number>()
  useEffect(() => {
    let cancelled = false
    let pending = false
    setCurrent(undefined)
    const refresh = async () => {
      if (pending || document.hidden) return
      pending = true
      try {
        const response = await api.getPublicAPIKeyConcurrency(apiKey)
        if (!cancelled) setCurrent(response.current)
      } catch {
        // 暂时读取失败时保留上次快照，下一次轮询重试。
      } finally {
        pending = false
      }
    }
    void refresh()
    const timer = window.setInterval(() => void refresh(), REFRESH_INTERVAL_MS)
    document.addEventListener('visibilitychange', refresh)
    return () => {
      cancelled = true
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', refresh)
    }
  }, [apiKey])
  return <APIKeyConcurrencyBadge current={current} limit={limit} showIdle />
}
