import { useEffect, useState } from 'react'
import { AdminAPIError, api } from '../api'
import APIKeyConcurrencyBadge from './APIKeyConcurrencyBadge'

const REFRESH_INTERVAL_MS = 5_000
const REFRESH_TIMEOUT_MS = 4_000

export default function PublicAPIKeyConcurrencyBadge({ apiKey, limit }: { apiKey: string; limit?: number }) {
  const [current, setCurrent] = useState<number>()
  useEffect(() => {
    let cancelled = false
    let pending = false
    setCurrent(undefined)
    const refresh = async () => {
      if (pending || document.hidden) return
      pending = true
      const controller = new AbortController()
      const timeout = window.setTimeout(() => controller.abort(), REFRESH_TIMEOUT_MS)
      try {
        const response = await api.getPublicAPIKeyConcurrency(apiKey, { signal: controller.signal })
        if (!cancelled) setCurrent(response.current)
      } catch (err) {
        // 密钥失效或功能关闭时清空快照；其他暂时性失败保留上次快照，下一次轮询重试。
        if (!cancelled && err instanceof AdminAPIError && (err.status === 401 || err.status === 404)) {
          setCurrent(undefined)
        }
      } finally {
        window.clearTimeout(timeout)
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
  return <APIKeyConcurrencyBadge current={current} limit={limit} />
}
