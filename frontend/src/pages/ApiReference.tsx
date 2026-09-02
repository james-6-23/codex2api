import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Copy, Check, Download, Image as ImageIcon, Play, Loader2 } from 'lucide-react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/select'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { CodeBlock } from './docs/EndpointDoc'

// 状态码 Tab 切换
function StatusTabs({ tabs, active, onChange }: { tabs: { code: number; label?: string }[]; active: number; onChange: (c: number) => void }) {
  return (
    <div className="flex items-center gap-0.5 border-b border-border mb-0">
      {tabs.map(tab => {
        const isActive = active === tab.code
        const codeColor = tab.code < 300 ? 'text-emerald-600 dark:text-emerald-400'
          : tab.code < 400 ? 'text-amber-600 dark:text-amber-400'
          : 'text-red-500 dark:text-red-400'
        return (
          <button
            key={tab.code}
            onClick={() => onChange(tab.code)}
            className={`px-3 py-2 text-sm font-semibold border-b-2 transition-colors ${
              isActive
                ? `border-foreground ${codeColor}`
                : 'border-transparent text-muted-foreground/60 hover:text-muted-foreground'
            }`}
          >
            {tab.code}
          </button>
        )
      })}
    </div>
  )
}

// 方法颜色
function MethodBadge({ method, sm }: { method: string; sm?: boolean }) {
  const colors: Record<string, string> = {
    GET: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400 border-emerald-200 dark:border-emerald-800',
    POST: 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400 border-blue-200 dark:border-blue-800',
    PUT: 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400 border-amber-200 dark:border-amber-800',
    DELETE: 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400 border-red-200 dark:border-red-800',
  }
  const size = sm ? 'px-1.5 py-0.5 rounded text-[10px]' : 'px-2.5 py-1 rounded-lg text-xs'
  return (
    <span className={`inline-flex items-center font-bold border ${size} ${colors[method] || 'bg-muted text-foreground border-border'}`}>
      {method}
    </span>
  )
}

type ImagePreview = {
  src: string
  format: string
  filename: string
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function parseJSON(value: string): unknown | null {
  try {
    return JSON.parse(value)
  } catch {
    return null
  }
}

function firstString(...values: unknown[]): string | null {
  for (const value of values) {
    if (typeof value === 'string' && value.trim()) return value.trim()
  }
  return null
}

function normalizeImageFormat(value: unknown): string {
  const raw = typeof value === 'string' ? value.trim().toLowerCase() : ''
  if (!raw) return 'png'
  const withoutMime = raw.startsWith('image/') ? raw.slice('image/'.length) : raw
  const withoutDot = withoutMime.startsWith('.') ? withoutMime.slice(1) : withoutMime
  if (withoutDot === 'jpg') return 'jpeg'
  if (['png', 'jpeg', 'webp', 'gif'].includes(withoutDot)) return withoutDot
  return 'png'
}

function imageMime(format: string): string {
  const normalized = normalizeImageFormat(format)
  return normalized === 'jpeg' ? 'image/jpeg' : `image/${normalized}`
}

function imageExtension(format: string): string {
  return normalizeImageFormat(format) === 'jpeg' ? 'jpg' : normalizeImageFormat(format)
}

function inferDataUrlFormat(value: string): string | null {
  const match = value.match(/^data:image\/([^;,]+)[;,]/i)
  return match?.[1] ? normalizeImageFormat(match[1]) : null
}

function isLikelyImageBase64(value: string): boolean {
  const raw = value.trim()
  if (raw.startsWith('data:image/')) return true
  const normalized = raw.replace(/\s+/g, '')
  return normalized.length > 64 && /^[A-Za-z0-9+/_-]+={0,2}$/.test(normalized)
}

function imageFormatFromBody(body: unknown): string | null {
  if (!isRecord(body)) return null
  const direct = firstString(body.output_format, body.format)
  if (direct) return normalizeImageFormat(direct)
  if (Array.isArray(body.tools)) {
    for (const tool of body.tools) {
      if (!isRecord(tool)) continue
      const toolFormat = firstString(tool.output_format, tool.format)
      if (toolFormat) return normalizeImageFormat(toolFormat)
    }
  }
  return null
}

function extractImagePreviews(responseText: string, requestBody: string): ImagePreview[] {
  const responseJSON = parseJSON(responseText)
  if (!isRecord(responseJSON)) return []

  const requestFormat = imageFormatFromBody(parseJSON(requestBody))
  const rootFormat = firstString(responseJSON.output_format, responseJSON.format, requestFormat) ?? 'png'
  const previews: ImagePreview[] = []
  const seen = new Set<string>()

  const addPreview = (src: string, format: unknown) => {
    const normalizedFormat = inferDataUrlFormat(src) ?? normalizeImageFormat(format)
    const key = `${src.length}:${src.slice(0, 128)}`
    if (seen.has(key)) return
    seen.add(key)
    previews.push({
      src,
      format: normalizedFormat,
      filename: `image-${previews.length + 1}.${imageExtension(normalizedFormat)}`,
    })
  }

  const addBase64 = (raw: unknown, format: unknown) => {
    if (typeof raw !== 'string' || !isLikelyImageBase64(raw)) return
    const trimmed = raw.trim()
    if (trimmed.startsWith('data:image/')) {
      addPreview(trimmed, format)
      return
    }
    const normalized = trimmed.replace(/\s+/g, '')
    const nextFormat = normalizeImageFormat(format)
    addPreview(`data:${imageMime(nextFormat)};base64,${normalized}`, nextFormat)
  }

  const addURL = (raw: unknown, format: unknown) => {
    if (typeof raw !== 'string') return
    const url = raw.trim()
    if (!/^(https?:|blob:|data:image\/)/i.test(url)) return
    addPreview(url, format)
  }

  const addFromItem = (item: unknown) => {
    if (!isRecord(item)) return
    const format = firstString(item.output_format, item.format, item.mime_type, rootFormat)
    addBase64(item.b64_json, format)
    addBase64(item.result, format)
    addURL(item.url, format)
    addURL(item.image_url, format)
    if (isRecord(item.image_url)) {
      addURL(item.image_url.url, format)
    }
  }

  if (Array.isArray(responseJSON.data)) {
    responseJSON.data.forEach(addFromItem)
  } else {
    addFromItem(responseJSON.data)
  }

  if (Array.isArray(responseJSON.output)) {
    responseJSON.output.forEach(addFromItem)
  }

  return previews
}

function downloadImagePreview(preview: ImagePreview) {
  const a = document.createElement('a')
  a.href = preview.src
  a.download = preview.filename
  a.rel = 'noreferrer'
  document.body.appendChild(a)
  a.click()
  a.remove()
}

// Try It 测试弹窗
function TryItDialog({ open, onClose, method, path, defaultBody, apiKey, baseUrl, allKeys }: {
  open: boolean
  onClose: () => void
  method: string
  path: string
  defaultBody: string
  apiKey: string
  baseUrl: string
  allKeys: { name: string; key: string }[]
}) {
  const { t } = useTranslation()
  const [body, setBody] = useState(defaultBody)
  const [token, setToken] = useState(apiKey)
  const [response, setResponse] = useState('')
  const [status, setStatus] = useState<number | null>(null)
  const [loading, setLoading] = useState(false)
  const [duration, setDuration] = useState<number | null>(null)
  const imagePreviews = useMemo(() => extractImagePreviews(response, body), [response, body])

  useEffect(() => {
    if (open) {
      setBody(defaultBody)
      setToken(apiKey)
      setResponse('')
      setStatus(null)
      setDuration(null)
    }
  }, [open, defaultBody, apiKey])

  const handleSend = async () => {
    setLoading(true)
    setResponse('')
    setStatus(null)
    setDuration(null)
    const start = performance.now()
    try {
      const isAdmin = path.startsWith('/api/admin')
      const headers: Record<string, string> = { 'Content-Type': 'application/json' }
      if (isAdmin) {
        headers['X-Admin-Key'] = token
      } else if (path === '/v1/messages') {
        headers['x-api-key'] = token
        headers['anthropic-version'] = '2023-06-01'
      } else {
        headers['Authorization'] = `Bearer ${token}`
      }

      const isGet = method === 'GET'
      const url = baseUrl + path
      const res = await fetch(url, {
        method,
        headers: isGet ? { 'Authorization': `Bearer ${token}`, 'X-Admin-Key': token } : headers,
        body: isGet ? undefined : body.trim() || undefined,
      })
      setStatus(res.status)
      setDuration(Math.round(performance.now() - start))
      const text = await res.text()
      try {
        setResponse(JSON.stringify(JSON.parse(text), null, 2))
      } catch {
        setResponse(text)
      }
    } catch (e) {
      setDuration(Math.round(performance.now() - start))
      setResponse(`Error: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setLoading(false)
    }
  }

  const statusColor = status === null ? '' : status < 300 ? 'text-emerald-600' : status < 400 ? 'text-amber-600' : 'text-red-500'
  const statusBg = status === null ? '' : status < 300 ? 'bg-emerald-50 dark:bg-emerald-900/20' : status < 400 ? 'bg-amber-50 dark:bg-amber-900/20' : 'bg-red-50 dark:bg-red-900/20'

  return (
    <Dialog open={open} onOpenChange={(v) => { if (!v) onClose() }}>
      <DialogContent className="sm:max-w-3xl max-h-[90vh] overflow-visible flex flex-col gap-0 p-0 sm:p-0" showCloseButton={false}>
        {/* 顶部端点栏 + Send */}
        <div className="flex items-center gap-3 px-5 py-3.5 border-b border-border bg-muted/30">
          <div className="flex items-center gap-2.5 flex-1 px-3 py-2 rounded-xl border border-border bg-background">
            <MethodBadge method={method} />
            <code className="font-mono text-sm font-medium text-foreground">{path}</code>
          </div>
          <Button
            onClick={() => void handleSend()}
            disabled={loading}
            className="gap-2 bg-emerald-600 hover:bg-emerald-700 text-white px-5 shrink-0"
          >
            {loading ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
            {loading ? t('apiRef.tryIt.sending') : t('apiRef.tryIt.send')}
          </Button>
        </div>

        {/* 内容区：左右分栏 */}
        <div className="flex flex-1 min-h-0 overflow-hidden">
          {/* 左侧：参数 */}
          <div className="flex-1 overflow-visible p-5 space-y-4 border-r border-border">
            {/* Authorization */}
            <div className="rounded-xl border border-border overflow-visible">
              <div className="px-4 py-2.5 bg-muted/30 border-b border-border">
                <span className="text-sm font-semibold text-foreground">Authorization</span>
              </div>
              <div className="p-4 space-y-3">
                <div className="flex items-center justify-between gap-3">
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-semibold text-foreground">
                        {path === '/v1/messages' ? 'x-api-key' : path.startsWith('/api/admin') ? 'X-Admin-Key' : 'Authorization'}
                      </span>
                      <span className="text-[11px] text-muted-foreground font-mono">string</span>
                    </div>
                    <Badge variant="destructive" className="mt-1 text-[10px] px-1.5 py-0">required</Badge>
                  </div>
                  <input
                    className="w-52 px-3 py-1.5 rounded-lg border border-border bg-background text-sm font-mono focus:outline-none focus:ring-2 focus:ring-primary/30"
                    placeholder="enter token"
                    value={token}
                    onChange={e => setToken(e.target.value)}
                  />
                </div>
                {allKeys.length > 0 && (
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground shrink-0">{t('apiRef.tryIt.selectKey')}</span>
                    <Select
                      value={token}
                      onValueChange={v => setToken(v)}
                      options={allKeys.map(k => ({
                        label: `${k.name} — ${k.key.length > 20 ? k.key.slice(0, 8) + '...' + k.key.slice(-4) : k.key}`,
                        value: k.key,
                      }))}
                    />
                  </div>
                )}
              </div>
            </div>

            {/* Request Body */}
            {method !== 'GET' && method !== 'DELETE' && (
              <div className="rounded-xl border border-border overflow-hidden">
                <div className="px-4 py-2.5 bg-muted/30 border-b border-border">
                  <span className="text-sm font-semibold text-foreground">Request Body</span>
                </div>
                <textarea
                  className="w-full h-56 p-4 bg-background font-mono text-[20px] leading-relaxed resize-none focus:outline-none border-0"
                  value={body}
                  onChange={e => setBody(e.target.value)}
                  spellCheck={false}
                />
              </div>
            )}
          </div>

          {/* 右侧：响应 */}
          <div className="flex-1 overflow-auto p-5">
            <div className="rounded-xl border border-border overflow-hidden h-full flex flex-col">
              <div className="px-4 py-2.5 bg-muted/30 border-b border-border flex items-center justify-between">
                <span className="text-sm font-semibold text-foreground">{t('apiRef.tryIt.responseTitle')}</span>
                {status !== null && (
                  <div className="flex items-center gap-2.5">
                    <span className={`px-2 py-0.5 rounded-md text-xs font-bold ${statusColor} ${statusBg}`}>{status}</span>
                    {duration !== null && <span className="text-xs text-muted-foreground">{duration}ms</span>}
                  </div>
                )}
              </div>
              <div className="flex-1 overflow-auto">
                {response ? (
                  <div className="min-h-full">
                    {imagePreviews.length > 0 && (
                      <div className="border-b border-border bg-muted/10 p-4 space-y-3">
                        <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
                          <ImageIcon className="size-4 text-muted-foreground" />
                          <span>{t('apiRef.tryIt.imagePreview')}</span>
                        </div>
                        <div className={`grid gap-3 ${imagePreviews.length > 1 ? 'sm:grid-cols-2' : 'grid-cols-1'}`}>
                          {imagePreviews.map((preview, index) => (
                            <div key={`${preview.filename}-${index}`} className="overflow-hidden rounded-lg border border-border bg-background">
                              <div className="flex items-center justify-between gap-3 border-b border-border bg-muted/20 px-3 py-2">
                                <span className="truncate text-xs font-medium text-muted-foreground">
                                  #{index + 1} · {preview.format.toUpperCase()}
                                </span>
                                <button
                                  type="button"
                                  onClick={() => downloadImagePreview(preview)}
                                  className="inline-flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                                  title={t('apiRef.tryIt.downloadImage')}
                                  aria-label={t('apiRef.tryIt.downloadImage')}
                                >
                                  <Download className="size-3.5" />
                                </button>
                              </div>
                              <div className="flex min-h-[220px] items-center justify-center bg-zinc-100 p-2 dark:bg-zinc-950">
                                <img
                                  src={preview.src}
                                  alt={`${t('apiRef.tryIt.imagePreview')} ${index + 1}`}
                                  className="max-h-[360px] w-full object-contain"
                                />
                              </div>
                            </div>
                          ))}
                        </div>
                      </div>
                    )}
                    <pre className="p-4 font-mono text-[20px] text-foreground leading-relaxed whitespace-pre-wrap">
                      <code>{response}</code>
                    </pre>
                  </div>
                ) : (
                  <div className="flex items-center justify-center h-full min-h-[200px] text-sm text-muted-foreground">
                    {loading ? (
                      <div className="flex items-center gap-2">
                        <Loader2 className="size-4 animate-spin" />
                        <span>{t('apiRef.tryIt.sending')}</span>
                      </div>
                    ) : (
                      <span>{t('apiRef.tryIt.placeholder')}</span>
                    )}
                  </div>
                )}
              </div>
            </div>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// 单个端点文档
function EndpointDoc({ id, method, path, title, description, curlExample, responseExamples, defaultBody, apiKey, baseUrl, allKeys }: {
  id?: string
  method: string
  path: string
  title: string
  description: string
  curlExample: string
  responseExamples: { code: number; body: string }[]
  defaultBody?: string
  apiKey?: string
  baseUrl?: string
  allKeys?: { name: string; key: string }[]
}) {
  const { t } = useTranslation()
  const [activeStatus, setActiveStatus] = useState(responseExamples[0]?.code ?? 200)
  const activeBody = responseExamples.find(r => r.code === activeStatus)?.body ?? ''
  const [tryOpen, setTryOpen] = useState(false)
  // A path parameter is documentation syntax, not a requestable URL. Keep
  // the example visible while preventing Try it from sending a literal
  // ":id" request that can never reach the intended account.
  const supportsTryIt = !path.includes(':')

  return (
    <Card id={id} className="mb-6 scroll-mt-20">
      <CardContent className="p-6">
        {/* 标题 */}
        <h3 className="text-xl font-bold text-foreground mb-1">{title}</h3>
        <p className="text-sm text-muted-foreground mb-4">{description}</p>

        {/* 端点路径栏 + Try it */}
        <div className="flex items-center gap-3 p-3 rounded-xl border border-border bg-muted/30 mb-5">
          <MethodBadge method={method} />
          <code className="font-mono text-sm font-semibold text-foreground flex-1">{path}</code>
          <Button
            size="sm"
            onClick={() => setTryOpen(true)}
            disabled={!supportsTryIt}
            className="gap-1.5 bg-emerald-600 hover:bg-emerald-700 text-white shrink-0"
          >
            <Play className="size-3.5" />
            {t('apiRef.tryIt.button')}
          </Button>
        </div>

        {supportsTryIt && (
          <TryItDialog
            open={tryOpen}
            onClose={() => setTryOpen(false)}
            method={method}
            path={path}
            defaultBody={defaultBody || ''}
            apiKey={apiKey || ''}
            baseUrl={baseUrl || ''}
            allKeys={allKeys || []}
          />
        )}

        {/* cURL 示例 */}
        <div className="mb-5">
          <CodeBlock label="cURL" content={curlExample} />
        </div>

        {/* 响应示例 */}
        <div className="border border-border rounded-xl overflow-hidden">
          <div className="px-4 pt-1.5 bg-muted/30">
            <StatusTabs
              tabs={responseExamples.map(r => ({ code: r.code }))}
              active={activeStatus}
              onChange={setActiveStatus}
            />
          </div>
          <pre className="p-4 font-mono text-[15px] text-muted-foreground overflow-x-auto leading-[1.8] bg-muted/5 max-h-[400px]">
            <code>{activeBody}</code>
          </pre>
        </div>
      </CardContent>
    </Card>
  )
}

export default function ApiReference() {
  const { t, i18n } = useTranslation()
  const baseUrl = useMemo(() => window.location.origin, [])
  const [firstKey, setFirstKey] = useState('')
  const [allKeys, setAllKeys] = useState<{ name: string; key: string }[]>([])
  const copy = (zh: string, en: string) => i18n.language.toLowerCase().startsWith('en') ? en : zh

  // 加载 API Key 列表
  useEffect(() => {
    api.getAPIKeys().then(res => {
      const keys = (res.keys ?? []).map(k => ({ name: k.name, key: k.raw_key || k.key }))
      setAllKeys(keys)
      if (keys.length > 0) setFirstKey(keys[0].key)
    }).catch(() => {})
  }, [])

  const navItems = [
    { id: 'auth', label: t('apiRef.authSection'), method: '' },
    { id: 'responses', label: '/v1/responses', method: 'POST' },
    { id: 'chat', label: '/v1/chat/completions', method: 'POST' },
    { id: 'images-generations', label: '/v1/images/generations', method: 'POST' },
    { id: 'images-edits', label: '/v1/images/edits', method: 'POST' },
    { id: 'messages', label: '/v1/messages', method: 'POST' },
    { id: 'models', label: '/v1/models', method: 'GET' },
    { id: 'health', label: '/health', method: 'GET' },
    { id: 'add-account', label: t('apiRef.addAccount.title'), method: 'POST' },
    { id: 'add-account-at', label: t('apiRef.addATAccount.title'), method: 'POST' },
    { id: 'import-accounts', label: t('apiRef.importAccounts.title'), method: 'POST' },
    { id: 'delete-account', label: '/accounts/:id', method: 'DELETE' },
    { id: 'list-accounts', label: '/accounts', method: 'GET' },
    { id: 'claude-management', label: t('claude.providerTitle'), method: '' },
    { id: 'claude-list', label: '/accounts?channel=claude', method: 'GET' },
    { id: 'claude-auth-url', label: '/claude/oauth/auth-url', method: 'POST' },
    { id: 'claude-exchange-code', label: '/claude/oauth/exchange-code', method: 'POST' },
    { id: 'claude-import', label: '/claude/import', method: 'POST' },
    { id: 'claude-refresh-token', label: '/accounts/:id/refresh', method: 'POST' },
    { id: 'claude-refresh-models', label: '/accounts/:id/claude/models', method: 'POST' },
    { id: 'claude-refresh-all-models', label: '/claude/models/refresh', method: 'POST' },
    { id: 'claude-sync-models', label: '/accounts/:id/models/sync-upstream', method: 'POST' },
    { id: 'claude-update-models', label: '/accounts/:id/models', method: 'PATCH' },
    { id: 'claude-refresh-usage', label: '/accounts/:id/usage/refresh', method: 'POST' },
    { id: 'claude-probe-models', label: '/accounts/:id/models/probe', method: 'POST' },
    { id: 'claude-test-connection', label: '/accounts/:id/test', method: 'GET' },
    { id: 'claude-get-config', label: '/settings/claude-config', method: 'GET' },
    { id: 'claude-update-config', label: '/settings/claude-config', method: 'PUT' },
  ]

  const [activeNav, setActiveNav] = useState(navItems[0].id)
  const navRefs = useRef<Record<string, HTMLButtonElement | null>>({})
  const [indicator, setIndicator] = useState({ left: 0, width: 0 })

  const updateIndicator = useCallback((id: string) => {
    const el = navRefs.current[id]
    if (!el) return
    setIndicator({
      left: el.offsetLeft,
      width: el.offsetWidth,
    })
    // 将激活的导航项滚动到可见区域
    el.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' })
  }, [])

  useEffect(() => {
    updateIndicator(activeNav)
  }, [activeNav, updateIndicator])

  // 滚动时自动高亮当前可见的端点
  useEffect(() => {
    const ids = navItems.map(n => n.id)
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            setActiveNav(entry.target.id)
            break
          }
        }
      },
      { rootMargin: '-80px 0px -60% 0px', threshold: 0.1 }
    )
    for (const id of ids) {
      const el = document.getElementById(id)
      if (el) observer.observe(el)
    }
    return () => observer.disconnect()
  }, [])

  const scrollTo = (id: string) => {
    setActiveNav(id)
    document.getElementById(id)?.scrollIntoView({ behavior: 'smooth' })
  }

  return (
    <>
      <PageHeader
        title={t('apiRef.title')}
        description={t('apiRef.description')}
      />

      {/* 悬浮导航栏 */}
      <div className="sticky top-2 z-30 mb-4">
        <div className="relative flex items-center gap-x-0.5 px-3 py-2 rounded-2xl border border-border bg-background/80 backdrop-blur-lg shadow-sm overflow-x-auto [&::-webkit-scrollbar]:hidden [-ms-overflow-style:none] [scrollbar-width:none]">
          {/* 滑动指示器 */}
          <div
            className="absolute top-2 h-[calc(100%-16px)] rounded-xl bg-primary/8 border border-primary/15 transition-all duration-300 ease-out pointer-events-none"
            style={{ left: indicator.left, width: indicator.width }}
          />
          {navItems.map(item => (
            <button
              key={item.id}
              ref={el => { navRefs.current[item.id] = el }}
              onClick={() => scrollTo(item.id)}
              className={`relative flex items-center gap-1 px-2 py-1.5 rounded-xl text-[11px] font-medium whitespace-nowrap transition-colors ${
                activeNav === item.id
                  ? 'text-primary'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              {item.method && <MethodBadge method={item.method} sm />}
              <span>{item.label}</span>
            </button>
          ))}
        </div>
      </div>

      {/* 认证说明 */}
      <Card id="auth" className="mb-6 scroll-mt-20">
        <CardContent className="p-6">
          <h3 className="text-base font-semibold text-foreground mb-2">{t('apiRef.authSection')}</h3>
          <p className="text-sm text-muted-foreground mb-4">{t('apiRef.authDesc')}</p>
          <div className="space-y-2.5">
            <div className="flex items-center gap-2.5 px-3.5 py-2.5 rounded-xl bg-muted/40 border border-border">
              <Badge variant="outline" className="text-[10px] font-bold shrink-0">Header</Badge>
              <code className="font-mono text-sm font-medium text-foreground/80">Authorization: Bearer <span className="text-muted-foreground italic">&lt;key&gt;</span></code>
            </div>
            <div className="flex items-center gap-2.5 px-3.5 py-2.5 rounded-xl bg-muted/40 border border-border">
              <Badge variant="outline" className="text-[10px] font-bold shrink-0">Header</Badge>
              <code className="font-mono text-sm font-medium text-foreground/80">x-api-key: <span className="text-muted-foreground italic">&lt;key&gt;</span></code>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* POST /v1/responses */}
      <EndpointDoc
        id="responses"
        method="POST"
        path="/v1/responses"
        title={t('apiRef.responses.title')}
        description={t('apiRef.responses.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "model": "gpt-5.5",
  "input": [{"role": "user", "content": [{"type": "input_text", "text": "Hello"}]}],
  "stream": false
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/v1/responses \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.5",
  "input": [
    {
      "role": "user",
      "content": [
        {"type": "input_text", "text": "Hello, what can you do?"}
      ]
    }
  ],
  "stream": true,
  "reasoning": {"effort": "high"}
}'`}
        responseExamples={[
          { code: 200, body: `{
  "id": "resp_abc123",
  "object": "response",
  "model": "gpt-5.5",
  "status": "completed",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {
          "type": "output_text",
          "text": "Hello! I'm an AI assistant..."
        }
      ]
    }
  ],
  "usage": {
    "input_tokens": 12,
    "output_tokens": 45,
    "total_tokens": 57
  }
}` },
          { code: 400, body: `{
  "error": {
    "code": "invalid_request",
    "message": "model is required",
    "type": "invalid_request_error"
  }
}` },
          { code: 401, body: `{
  "error": {
    "code": "invalid_api_key",
    "message": "Invalid API key provided",
    "type": "authentication_error"
  }
}` },
          { code: 503, body: `{
  "error": {
    "message": "无可用账号，请稍后重试",
    "type": "server_error",
    "code": "no_available_account"
  }
}` },
          { code: 429, body: `{
  "error": {
    "message": "Rate limit exceeded",
    "type": "server_error",
    "code": "account_pool_usage_limit_reached",
    "resets_in_seconds": 18000
  }
}` },
        ]}
      />

      {/* POST /v1/chat/completions */}
      <EndpointDoc
        id="chat"
        method="POST"
        path="/v1/chat/completions"
        title={t('apiRef.chat.title')}
        description={t('apiRef.chat.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "model": "gpt-5.5",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": false
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/v1/chat/completions \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.5",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": true,
  "reasoning_effort": "high"
}'`}
        responseExamples={[
          { code: 200, body: `{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "model": "gpt-5.5",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 18,
    "completion_tokens": 9,
    "total_tokens": 27
  }
}` },
          { code: 400, body: `{
  "error": {
    "code": "invalid_request",
    "message": "Request validation failed",
    "type": "invalid_request_error"
  }
}` },
          { code: 401, body: `{
  "error": {
    "code": "invalid_api_key",
    "message": "Invalid API key provided",
    "type": "authentication_error"
  }
}` },
        ]}
      />

      {/* POST /v1/images/generations */}
      <EndpointDoc
        id="images-generations"
        method="POST"
        path="/v1/images/generations"
        title="Create image"
        description="OpenAI Images compatible endpoint backed by Codex Responses image_generation."
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "size": "1024x1024",
  "quality": "high"
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/v1/images/generations \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "response_format": "b64_json"
}'`}
        responseExamples={[
          { code: 200, body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [
    {
      "b64_json": "..."
    }
  ],
  "usage": {
    "images": 1
  }
}` },
        ]}
      />

      {/* POST /v1/images/edits */}
      <EndpointDoc
        id="images-edits"
        method="POST"
        path="/v1/images/edits"
        title="Edit image"
        description="OpenAI Images compatible edit endpoint. JSON image_url and multipart image uploads are supported."
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [
    {"image_url": "https://example.com/source.png"}
  ],
  "output_format": "png"
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/v1/images/edits \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [{"image_url": "https://example.com/source.png"}]
}'`}
        responseExamples={[
          { code: 200, body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [
    {
      "b64_json": "..."
    }
  ]
}` },
        ]}
      />

      {/* POST /v1/messages */}
      <EndpointDoc
        id="messages"
        method="POST"
        path="/v1/messages"
        title={t('apiRef.messages.title')}
        description={t('apiRef.messages.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "model": "claude-sonnet-4-5",
  "max_tokens": 1024,
  "messages": [{"role": "user", "content": "Hello"}]
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/v1/messages \\
  --header 'x-api-key: <token>' \\
  --header 'Content-Type: application/json' \\
  --header 'anthropic-version: 2023-06-01' \\
  --data '{
  "model": "claude-sonnet-4-5",
  "max_tokens": 1024,
  "messages": [
    {"role": "user", "content": "Hello, Claude!"}
  ]
}'`}
        responseExamples={[
          { code: 200, body: `{
  "id": "msg_abc123",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5",
  "content": [
    {
      "type": "text",
      "text": "Hello! How can I assist you today?"
    }
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 10,
    "output_tokens": 12,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  }
}` },
          { code: 400, body: `{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "model is required"
  }
}` },
          { code: 401, body: `{
  "type": "error",
  "error": {
    "type": "authentication_error",
    "message": "Invalid API key"
  }
}` },
          { code: 429, body: `{
  "type": "error",
  "error": {
    "type": "rate_limit_error",
    "message": "All accounts rate limited"
  }
}` },
        ]}
      />

      {/* GET /v1/models */}
      <EndpointDoc
        id="models"
        method="GET"
        path="/v1/models"
        title={t('apiRef.models.title')}
        description={t('apiRef.models.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request GET \\
  --url ${baseUrl}/v1/models \\
  --header 'Authorization: Bearer <token>'`}
        responseExamples={[
          { code: 200, body: `{
  "object": "list",
  "data": [
    {"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.4-mini", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.3-codex", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.3-codex-spark", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.2", "object": "model", "owned_by": "openai"},
    {"id": "gpt-image-2", "object": "model", "owned_by": "openai"}
  ]
}` },
          { code: 401, body: `{
  "error": {
    "code": "invalid_api_key",
    "message": "Invalid API key provided",
    "type": "authentication_error"
  }
}` },
        ]}
      />

      {/* GET /health */}
      <EndpointDoc
        id="health"
        method="GET"
        path="/health"
        title={t('apiRef.health.title')}
        description={t('apiRef.health.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request GET \\
  --url ${baseUrl}/health`}
        responseExamples={[
          { code: 200, body: `{
  "status": "ok",
  "available": 5,
  "total": 8
}` },
        ]}
      />

      {/* 账号管理分隔 */}
      <div className="mt-10 mb-6">
        <h2 className="text-lg font-bold text-foreground mb-1">{t('apiRef.accountSection')}</h2>
        <p className="text-sm text-muted-foreground">{t('apiRef.accountSectionDesc')}</p>
      </div>

      {/* POST /api/admin/accounts — RT 导入 */}
      <EndpointDoc
        id="add-account"
        method="POST"
        path="/api/admin/accounts"
        title={t('apiRef.addAccount.title')}
        description={t('apiRef.addAccount.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "name": "my-account",
  "refresh_token": "rt_XPqsKO3Ld...\\nrt_H2qdhY",
  "proxy_url": ""
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "my-account",
  "refresh_token": "rt_XPqsKO3Ld...\\nrt_H2qdhY",
  "proxy_url": ""
}'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "成功添加 1 个账号",
  "success": 1,
  "failed": 0
}` },
          { code: 400, body: `{
  "error": "refresh_token 是必填字段"
}` },
          { code: 401, body: `{
  "error": "Unauthorized"
}` },
        ]}
      />

      {/* POST /api/admin/accounts — 批量 RT */}
      <EndpointDoc
        id="add-account-batch"
        method="POST"
        path="/api/admin/accounts"
        title={t('apiRef.addAccountBatch.title')}
        description={t('apiRef.addAccountBatch.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "name": "batch",
  "refresh_token": "token_1\\ntoken_2\\ntoken_3",
  "proxy_url": ""
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "batch",
  "refresh_token": "token_1\\ntoken_2\\ntoken_3",
  "proxy_url": ""
}'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "成功添加 3 个账号",
  "success": 3,
  "failed": 0
}` },
        ]}
      />

      {/* POST /api/admin/accounts/at — AT 导入 */}
      <EndpointDoc
        id="add-account-at"
        method="POST"
        path="/api/admin/accounts/at"
        title={t('apiRef.addATAccount.title')}
        description={t('apiRef.addATAccount.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "name": "at-account",
  "access_token": "eyJhbGciOi...",
  "proxy_url": ""
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/at \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "at-account",
  "access_token": "eyJhbGciOi...",
  "proxy_url": ""
}'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "成功添加 1 个 AT-only 账号",
  "success": 1,
  "failed": 0
}` },
          { code: 400, body: `{
  "error": "access_token 是必填字段"
}` },
        ]}
      />

      {/* POST /api/admin/accounts/import — 文件导入 */}
      <EndpointDoc
        id="import-accounts"
        method="POST"
        path="/api/admin/accounts/import"
        title={t('apiRef.importAccounts.title')}
        description={t('apiRef.importAccounts.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`# TXT 格式（每行一个 Refresh Token）
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@tokens.txt' \\
  --form 'format=txt' \\
  --form 'proxy_url='

# JSON 格式（兼容 CLIProxyAPI 凭证导出）
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@credentials.json' \\
  --form 'format=json' \\
  --form 'proxy_url='

# AT TXT 格式（每行一个 Access Token）
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@access_tokens.txt' \\
  --form 'format=at_txt' \\
  --form 'proxy_url='`}
        responseExamples={[
          { code: 200, body: `{
  "message": "导入完成：成功 5，失败 0，重复 2",
  "total": 7,
  "success": 5,
  "failed": 0,
  "duplicate": 2
}` },
          { code: 400, body: `{
  "error": "请上传文件（字段名: file）"
}` },
        ]}
      />

      {/* DELETE /api/admin/accounts/:id */}
      <EndpointDoc
        id="delete-account"
        method="DELETE"
        path="/api/admin/accounts/:id"
        title={t('apiRef.deleteAccount.title')}
        description={t('apiRef.deleteAccount.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request DELETE \\
  --url ${baseUrl}/api/admin/accounts/1 \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "账号已删除"
}` },
          { code: 404, body: `{
  "error": "账号不存在"
}` },
        ]}
      />

      {/* GET /api/admin/accounts */}
      <EndpointDoc
        id="list-accounts"
        method="GET"
        path="/api/admin/accounts"
        title={t('apiRef.listAccounts.title')}
        description={t('apiRef.listAccounts.desc')}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request GET \\
  --url ${baseUrl}/api/admin/accounts \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "accounts": [
    {
      "id": 1,
      "name": "my-account",
      "email": "user@example.com",
      "plan_type": "team",
      "status": "active",
      "proxy_url": "",
      "created_at": "2025-01-01T00:00:00Z",
      "total_requests": 128,
      "success_requests": 125
    }
  ]
}` },
        ]}
      />

      {/* Claude / Anthropic 管理 API */}
      <div id="claude-management" className="mt-10 mb-6 scroll-mt-20">
        <div className="flex flex-wrap items-center gap-2 mb-1">
          <h2 className="text-lg font-bold text-foreground">{t('claude.providerTitle')}</h2>
          <Badge variant="outline">OAuth · Messages API</Badge>
        </div>
        <p className="text-sm text-muted-foreground">
          {copy(
            '以下接口用于导入、维护和验证 Claude OAuth 账号。所有接口均需要 X-Admin-Key；示例中的 Token、code、state 与账号 ID 都是占位符。',
            'Use these endpoints to import, maintain, and verify Claude OAuth accounts. Every endpoint requires X-Admin-Key; all tokens, codes, states, and account IDs below are placeholders.',
          )}
        </p>
      </div>

      <EndpointDoc
        id="claude-list"
        method="GET"
        path="/api/admin/accounts?channel=claude"
        title={copy('列出 Claude 账号', 'List Claude accounts')}
        description={copy(
          '只返回 Claude 渠道账号及其模型、用量采样、状态和调度字段。分页视图可追加 view=page、page 与 page_size。',
          'Return only Claude accounts with model, usage sampling, status, and scheduling fields. Add view=page, page, and page_size for the paged projection.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request GET \\
  --url '${baseUrl}/api/admin/accounts?channel=claude' \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "accounts": [
    {
      "id": 42,
      "name": "claude-team",
      "email": "user@example.com",
      "claude_api": true,
      "plan_type": "team",
      "status": "active",
      "models": ["claude-haiku-4-5", "claude-sonnet-4-5"],
      "claude_user_agent": "claude-cli/<version> (external, cli)",
      "claude_usage_probe_at": "2026-08-30T01:23:45Z",
      "usage_percent_5h": 12.5,
      "usage_percent_7d": 8.2
    }
  ]
}` },
          { code: 401, body: `{"error":"Unauthorized"}` },
        ]}
      />

      <EndpointDoc
        id="claude-auth-url"
        method="POST"
        path="/api/admin/accounts/claude/oauth/auth-url"
        title={copy('生成 Claude OAuth 授权链接', 'Generate a Claude OAuth authorization URL')}
        description={copy(
          '创建一次 15 分钟有效的 OAuth 登录会话，返回授权 URL 与一次性 state。服务端只暂存 PKCE verifier，不返回或记录账号 Token。',
          'Create a 15-minute OAuth login session and return an authorization URL plus one-time state. The server retains only the PKCE verifier and does not return or log account tokens.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody="{}"
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/claude/oauth/auth-url \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{}'`}
        responseExamples={[
          { code: 200, body: `{
  "auth_url": "https://claude.ai/oauth/authorize?...&state=<state>",
  "state": "<state>"
}` },
          { code: 401, body: `{"error":"Unauthorized"}` },
        ]}
      />

      <EndpointDoc
        id="claude-exchange-code"
        method="POST"
        path="/api/admin/accounts/claude/oauth/exchange-code"
        title={copy('完成 Claude OAuth 并入库', 'Exchange the Claude OAuth code and add the account')}
        description={copy(
          '提交上一步的 state 和回调 code。proxy_url 优先于 use_proxy_pool；timezone 使用 IANA 名称并参与账号指纹一致性。state 只能使用一次。',
          'Submit the state and callback code from the previous step. proxy_url takes precedence over use_proxy_pool; timezone must be an IANA name and contributes to stable account identity. A state can be consumed only once.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "state": "<state>",
  "code": "<oauth-code>",
  "name": "claude-team",
  "proxy_url": "",
  "use_proxy_pool": true,
  "timezone": "Asia/Shanghai"
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/claude/oauth/exchange-code \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "state": "<state>",
  "code": "<oauth-code>",
  "name": "claude-team",
  "use_proxy_pool": true,
  "timezone": "Asia/Shanghai"
}'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "成功添加 Claude 账号",
  "id": 42,
  "email": "user@example.com"
}` },
          { code: 400, body: `{"error":"登录会话已过期或不存在，请重新获取授权 URL"}` },
          { code: 409, body: `{"error":"Claude 账号已存在 (id=42)"}` },
        ]}
      />

      <EndpointDoc
        id="claude-import"
        method="POST"
        path="/api/admin/accounts/claude/import"
        title={copy('导入 Claude Token JSON', 'Import Claude token JSON')}
        description={copy(
          '导入 cmd/claude_login 或 Claude 专用导出端点生成的凭据。支持单对象、数组和 accounts bundle；会恢复标签、分组名称映射、时区与稳定指纹。access_token 与 refresh_token 必填。',
          'Import credentials produced by cmd/claude_login or the Claude export endpoint. Single objects, arrays, and accounts bundles restore tags, name-based group mappings, timezone, and the stable fingerprint. access_token and refresh_token are required.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "access_token": "<access-token>",
  "refresh_token": "<refresh-token>",
  "email": "user@example.com",
  "account_id": "<anthropic-account-id>",
  "expires_at": "2026-08-30T02:00:00Z",
  "name": "claude-imported",
  "proxy_url": "",
  "use_proxy_pool": true,
  "timezone": "Asia/Shanghai"
}`}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/claude/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "access_token": "<access-token>",
  "refresh_token": "<refresh-token>",
  "account_id": "<anthropic-account-id>",
  "name": "claude-imported",
  "use_proxy_pool": true,
  "timezone": "Asia/Shanghai"
}'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "成功添加 Claude 账号",
  "id": 43,
  "email": "user@example.com"
}` },
          { code: 400, body: `{"error":"access_token 与 refresh_token 均为必填"}` },
          { code: 409, body: `{"error":"Claude 账号已存在 (id=42)"}` },
        ]}
      />

      <EndpointDoc
        id="claude-export"
        method="GET"
        path="/api/admin/accounts/claude/export"
        title={copy('导出 Claude 完整凭据', 'Export complete Claude credentials')}
        description={copy(
          '管理员专用高敏下载。ids 可精确选择账号，filter=healthy 只导出健康账号；format=auto|json|zip 控制 JSON/ZIP 输出。仅携带 Claude 身份头，不导出 Authorization、Cookie、API Key 或实例内分组 ID。',
          'Admin-only secret download. ids selects exact accounts, filter=healthy limits the export to healthy accounts, and format=auto|json|zip selects JSON or ZIP output. Only Claude identity headers are included—never Authorization, Cookie, API keys, or instance-local group IDs.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request GET \\
  --url '${baseUrl}/api/admin/accounts/claude/export?ids=42&filter=all&format=json' \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --output claude-account.json`}
        responseExamples={[
          { code: 200, body: `{
  "type": "claude",
  "version": 1,
  "auth_kind": "oauth",
  "email": "user@example.com",
  "access_token": "<access-token>",
  "refresh_token": "<refresh-token>",
  "account_id": "<anthropic-account-id>",
  "timezone": "Asia/Shanghai",
  "claude_fingerprint_mode": "force",
  "fingerprint_headers": {
    "User-Agent": "claude-cli/<version> (external, cli)"
  },
  "tags": ["production"],
  "group_refs": [{"name":"Claude production","channel":"claude"}],
  "enabled": true
}` },
          { code: 404, body: `{"error":"no exportable Claude accounts"}` },
        ]}
      />

      <EndpointDoc
        id="claude-refresh-token"
        method="POST"
        path="/api/admin/accounts/:id/refresh"
        title={copy('刷新 Claude OAuth Token', 'Refresh a Claude OAuth token')}
        description={copy(
          '刷新指定账号的 Access Token，并按账号当前凭据重新执行受控的用量探测。将 :id 替换为真实账号 ID；该操作不会改变账号分组或 API Key 权限。',
          'Refresh the selected account Access Token and run the bounded usage probe using its current credentials. Replace :id with a real account ID; groups and API-key permissions are unchanged.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/42/refresh \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{"message":"账号刷新成功"}` },
          { code: 404, body: `{"error":"账号不存在"}` },
          { code: 500, body: `{"error":"刷新失败: upstream error"}` },
        ]}
      />

      <EndpointDoc
        id="claude-refresh-models"
        method="POST"
        path="/api/admin/accounts/:id/claude/models"
        title={copy('刷新单个 Claude 账号模型', 'Refresh models for one Claude account')}
        description={copy(
          '调用 Anthropic /v1/models，保存该账号真实可用的模型目录并立即刷新运行时缓存。仅接受 Claude 账号和有效 Access Token。',
          'Call Anthropic /v1/models, persist the account’s actual model catalog, and invalidate the runtime catalog cache immediately. The account must be Claude OAuth with a valid Access Token.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/42/claude/models \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "已更新可用模型",
  "models": ["claude-haiku-4-5", "claude-sonnet-4-5"],
  "count": 2
}` },
          { code: 400, body: `{"error":"账号缺少 access_token,请先刷新或重新导入"}` },
          { code: 502, body: `{"error":"拉取可用模型失败: upstream error"}` },
        ]}
      />

      <EndpointDoc
        id="claude-refresh-all-models"
        method="POST"
        path="/api/admin/accounts/claude/models/refresh"
        title={copy('刷新全部 Claude 模型目录', 'Refresh all Claude model catalogs')}
        description={copy(
          '顺序刷新所有启用的 Claude 账号模型目录，并汇总成功、失败和去重后的模型数量。单个账号失败不会回滚其它成功结果。',
          'Refresh enabled Claude account catalogs and return aggregate success, failure, and deduplicated model counts. One account failure does not roll back other successful updates.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody="{}"
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/claude/models/refresh \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{}'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "已刷新 Claude 账号可用模型",
  "refreshed": 3,
  "failed": 1,
  "model_count": 5
}` },
          { code: 500, body: `{"error":"failed to list Claude accounts"}` },
        ]}
      />

      <EndpointDoc
        id="claude-sync-models"
        method="POST"
        path="/api/admin/accounts/:id/models/sync-upstream"
        title={copy('预览 Claude 上游模型目录', 'Preview one Claude upstream model catalog')}
        description={copy(
          '使用账号凭据拉取 Anthropic 模型清单，只读返回，不覆盖账号模型白名单。确认后可通过模型更新接口保存白名单。',
          'Fetch the Anthropic model list with the account credentials and return it without changing the account allowlist. Save a reviewed allowlist with the model update endpoint.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/42/models/sync-upstream \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "models": ["claude-haiku-4-5", "claude-sonnet-4-5"]
}` },
          { code: 502, body: `{"error":"拉取 Claude 上游模型清单失败: upstream error"}` },
        ]}
      />

      <EndpointDoc
        id="claude-update-models"
        method="PATCH"
        path="/api/admin/accounts/:id/models"
        title={copy('设置 Claude 模型白名单', 'Set a Claude model allowlist')}
        description={copy(
          '保存账号级 Claude 原生模型白名单。传空数组清除覆盖并恢复全量模型；非空列表只能包含 claude-* 模型。',
          'Save an account-level native Claude model allowlist. Send an empty array to clear the override and allow the full catalog; non-empty entries must use the claude-* namespace.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "models": ["claude-haiku-4-5", "claude-sonnet-4-5"]
}`}
        curlExample={`curl --request PATCH \\
  --url ${baseUrl}/api/admin/accounts/42/models \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{"models":["claude-haiku-4-5","claude-sonnet-4-5"]}'`}
        responseExamples={[
          { code: 200, body: `{"models":["claude-haiku-4-5","claude-sonnet-4-5"]}` },
          { code: 400, body: `{"error":"Claude 账号模型必须使用 claude-* 原生模型: gpt-5.5"}` },
          { code: 404, body: `{"error":"账号不在运行时池中"}` },
        ]}
      />

      <EndpointDoc
        id="claude-refresh-usage"
        method="POST"
        path="/api/admin/accounts/:id/usage/refresh"
        title={copy('刷新 Claude 用量', 'Refresh Claude usage')}
        description={copy(
          '对指定账号执行一次受控原生 Messages 用量探测，并返回最新 5 小时/7 天窗口、重置时间和采样元数据。失败时不会伪造 0% 用量。',
          'Run one bounded native Messages usage probe and return the latest 5-hour/7-day windows, reset times, and sampling metadata. A failed probe never fabricates a 0% usage value.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/42/usage/refresh \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "refreshed": true,
  "usage_percent_5h": 12.5,
  "usage_percent_7d": 8.2,
  "reset_5h_at": "2026-08-30T05:00:00Z",
  "reset_7d_at": "2026-09-05T00:00:00Z",
  "claude_usage_probe_at": "2026-08-30T01:23:45Z"
}` },
          { code: 502, body: `{"error":"刷新用量失败: upstream timeout"}` },
        ]}
      />

      <EndpointDoc
        id="claude-usage-detail"
        method="GET"
        path="/api/admin/accounts/:id/usage?days=30"
        title={copy('查看 Claude 账号用量明细', 'Read Claude account usage details')}
        description={copy(
          '读取指定账号的历史请求和 Token 用量。days 范围为 0-3650，传 0 表示不限制历史天数；该查询不会触发上游请求。',
          'Read historical request and token usage for one account. days accepts 0-3650; 0 means no day limit. This endpoint is read-only and does not call the upstream provider.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request GET \\
  --url '${baseUrl}/api/admin/accounts/42/usage?days=30' \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "account_id": 42,
  "days": 30,
  "total_requests": 128,
  "success_requests": 125,
  "error_requests": 3,
  "input_tokens": 12000,
  "output_tokens": 4500
}` },
          { code: 400, body: `{"error":"days 参数无效，需要 0-3650 的整数"}` },
        ]}
      />

      <EndpointDoc
        id="claude-probe-models"
        method="POST"
        path="/api/admin/accounts/:id/models/probe"
        title={copy('探测 Claude 模型能力', 'Probe Claude model capabilities')}
        description={copy(
          '用账号凭据并发探测文本模型，返回 available 和每个模型的 outcome。探测只读，不写入账号冷却、错误或调度状态；追加 ?stream=true 可逐模型接收 SSE 进度。',
          'Probe text models concurrently with the account credentials and return available plus per-model outcomes. Probing is read-only and does not change cooldown, error, or scheduling state; append ?stream=true for per-model SSE progress.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`# JSON result
curl --request POST \\
  --url '${baseUrl}/api/admin/accounts/42/models/probe?stream=false' \\
  --header 'X-Admin-Key: <admin_secret>'

# Optional SSE progress
curl --request POST \\
  --url '${baseUrl}/api/admin/accounts/42/models/probe?stream=true' \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "available": ["claude-haiku-4-5"],
  "results": [
    {"model":"claude-haiku-4-5","outcome":"available","detail":"模型响应正常"},
    {"model":"claude-opus-4-5","outcome":"throttled","detail":"上游返回 429 限流"}
  ]
}` },
          { code: 200, body: `data: {"type":"start","total":2,"models":["claude-haiku-4-5","claude-opus-4-5"]}

data: {"type":"result","model":"claude-haiku-4-5","outcome":"available"}

data: {"type":"done","available":["claude-haiku-4-5"]}` },
        ]}
      />

      <EndpointDoc
        id="claude-test-connection"
        method="GET"
        path="/api/admin/accounts/:id/test?model=claude-haiku-4-5"
        title={copy('测试 Claude 账号连接', 'Test a Claude account connection')}
        description={copy(
          '执行一次最小原生 Messages SSE 测试，返回 test_start、content、error 或 test_complete 事件。与模型探测不同，手动测连会同步真实账号状态和限流信息。',
          'Run a minimal native Messages SSE request and emit test_start, content, error, or test_complete events. Unlike capability probing, a manual connection test synchronizes the real account status and rate-limit information.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --no-buffer --request GET \\
  --url '${baseUrl}/api/admin/accounts/42/test?model=claude-haiku-4-5' \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `data: {"type":"test_start","model":"claude-haiku-4-5"}

data: {"type":"content","text":"OK"}

data: {"type":"test_complete","success":true}` },
          { code: 200, body: `data: {"type":"test_start","model":"claude-haiku-4-5"}

data: {"type":"error","error":"Claude 上游返回了有效响应，但账号仍处于配额/限流状态"}` },
        ]}
      />

      <EndpointDoc
        id="claude-get-config"
        method="GET"
        path="/api/admin/settings/claude-config"
        title={copy('读取 Claude 全局配置', 'Read Claude global settings')}
        description={copy(
          '读取 ClaudeCode 全局指纹模式、默认时区、并发会话窗口和出口安全边界。资源限制字段为 0 时表示不设网关上限；个体账号可在账号调度设置中覆盖其它默认值。',
          'Read the ClaudeCode global fingerprint mode, default timezone, session window, and egress security boundary. A resource-limit field of 0 means no gateway cap; individual accounts may override other defaults in account scheduling settings.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        curlExample={`curl --request GET \\
  --url ${baseUrl}/api/admin/settings/claude-config \\
  --header 'X-Admin-Key: <admin_secret>'`}
        responseExamples={[
          { code: 200, body: `{
  "fingerprint_mode": "preserve",
  "default_timezone": "Asia/Shanghai",
  "session_window_limit": 0,
  "allow_service_tier": false,
  "allow_inference_geo": false,
  "allow_speed": false,
  "allow_safety_identifier": false,
  "allowed_beta_headers": [],
  "max_output_tokens": 0,
  "max_tool_count": 0,
  "max_tool_schema_bytes": 0
}` },
        ]}
      />

      <EndpointDoc
        id="claude-update-config"
        method="PUT"
        path="/api/admin/settings/claude-config"
        title={copy('更新 Claude 全局配置', 'Update Claude global settings')}
        description={copy(
          '保存 ClaudeCode 的默认指纹、时区、并发窗口和出口安全策略，并立即热更新运行时 Store。安全字段默认过滤；fingerprint_mode 仅支持 preserve 或 force；资源限制字段 0 表示不设网关上限，仍受请求体和上游模型限制。',
          'Save ClaudeCode fingerprint, timezone, session window, and egress security defaults and apply them immediately. Sensitive fields are filtered by default; fingerprint_mode accepts preserve or force; resource-limit fields set to 0 mean no gateway cap and still respect request-body and upstream model limits.',
        )}
        apiKey={firstKey}
        baseUrl={baseUrl}
        allKeys={allKeys}
        defaultBody={`{
  "fingerprint_mode": "preserve",
  "default_timezone": "Asia/Shanghai",
  "session_window_limit": 0,
  "allow_service_tier": false,
  "allow_inference_geo": false,
  "allow_speed": false,
  "allow_safety_identifier": false,
  "allowed_beta_headers": [],
  "max_output_tokens": 0,
  "max_tool_count": 0,
  "max_tool_schema_bytes": 0
}`}
        curlExample={`curl --request PUT \\
  --url ${baseUrl}/api/admin/settings/claude-config \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "fingerprint_mode": "preserve",
  "default_timezone": "Asia/Shanghai",
  "session_window_limit": 0,
  "allow_service_tier": false,
  "allow_inference_geo": false,
  "allow_speed": false,
  "allow_safety_identifier": false,
  "allowed_beta_headers": [],
  "max_output_tokens": 0,
  "max_tool_count": 0,
  "max_tool_schema_bytes": 0
}'`}
        responseExamples={[
          { code: 200, body: `{
  "message": "已保存 ClaudeCode 全局配置",
  "fingerprint_mode": "preserve",
  "default_timezone": "Asia/Shanghai",
  "session_window_limit": 0,
  "allow_service_tier": false,
  "allow_inference_geo": false,
  "allow_speed": false,
  "allow_safety_identifier": false,
  "allowed_beta_headers": [],
  "max_output_tokens": 0,
  "max_tool_count": 0,
  "max_tool_schema_bytes": 0
}` },
          { code: 400, body: `{"error":"fingerprint_mode must be one of: preserve, force"}` },
        ]}
      />
    </>
  )
}
