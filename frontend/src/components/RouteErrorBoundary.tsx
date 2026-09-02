import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { AlertCircle } from 'lucide-react'

interface Props {
  children: ReactNode
  fallbackTitle?: string
  fallbackDescription?: string
  retryLabel?: string
}

interface State {
  error: Error | null
}

const CHUNK_RELOAD_FLAG = 'codex2api:chunk-reloaded'
const CHUNK_RELOAD_COOLDOWN_MS = 10_000

function reloadWithCacheBust() {
  const url = new URL(window.location.href)
  url.searchParams.set('_reload', Date.now().toString())
  window.location.replace(url.toString())
}

async function clearRuntimeCachesAndReload() {
  try {
    if ('caches' in window) {
      const cacheNames = await window.caches.keys()
      await Promise.all(cacheNames.map((cacheName) => window.caches.delete(cacheName)))
    }
    if ('serviceWorker' in navigator) {
      const registrations = await navigator.serviceWorker.getRegistrations()
      await Promise.all(registrations.map((registration) => registration.unregister()))
    }
  } catch {
    // Cache APIs may be unavailable; the cache-busted navigation is still useful.
  }
  reloadWithCacheBust()
}

function isChunkLoadError(error: unknown): boolean {
  if (!error) return false
  const message = error instanceof Error ? `${error.name} ${error.message}` : String(error)
  return /ChunkLoadError|Loading chunk \d+ failed|Failed to fetch dynamically imported module|Importing a module script failed/i.test(
    message,
  )
}

export default class RouteErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    if (isChunkLoadError(error)) {
      try {
        const lastReload = Number(sessionStorage.getItem(CHUNK_RELOAD_FLAG) || 0)
        if (!lastReload || Date.now() - lastReload > CHUNK_RELOAD_COOLDOWN_MS) {
          sessionStorage.setItem(CHUNK_RELOAD_FLAG, String(Date.now()))
          void clearRuntimeCachesAndReload()
          return
        }
      } catch {
        // sessionStorage may be unavailable; fall through to render fallback
      }
    }
    if (import.meta.env.DEV) {
      console.error('[RouteErrorBoundary]', error, info)
    }
  }

  componentDidMount() {
    try {
      const lastReload = Number(sessionStorage.getItem(CHUNK_RELOAD_FLAG) || 0)
      if (lastReload && Date.now() - lastReload > CHUNK_RELOAD_COOLDOWN_MS) {
        sessionStorage.removeItem(CHUNK_RELOAD_FLAG)
      }
    } catch {
      // ignore
    }
  }

  handleRetry = () => {
    try {
      sessionStorage.removeItem(CHUNK_RELOAD_FLAG)
    } catch {
      // ignore
    }
    void clearRuntimeCachesAndReload()
  }

  render() {
    const { error } = this.state
    if (!error) return this.props.children

    const chunkError = isChunkLoadError(error)
    const title =
      this.props.fallbackTitle ??
      (chunkError ? '页面资源已更新，请刷新' : '页面渲染失败')
    const description =
      this.props.fallbackDescription ??
      (chunkError
        ? '应用版本已更新，本地缓存的旧资源已不可用。点击下方按钮刷新页面以加载最新版本。'
        : error.message || '发生了未知错误，请刷新页面或稍后重试。')
    const retryLabel = this.props.retryLabel ?? '刷新页面'

    return (
      <div
        className="flex flex-col items-center justify-center gap-3 rounded-lg border border-border bg-card/80 p-8 text-center shadow-sm min-h-[320px]"
        role="alert"
      >
        <div className="size-14 flex items-center justify-center rounded-full bg-destructive/12 text-destructive">
          <AlertCircle className="size-6" />
        </div>
        <strong className="text-lg font-bold text-foreground">{title}</strong>
        <p className="max-w-[420px] text-sm leading-relaxed text-muted-foreground">{description}</p>
        <Button variant="outline" onClick={this.handleRetry}>
          {retryLabel}
        </Button>
      </div>
    )
  }
}
