import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { getBucketConfig, type TimeRangeKey } from '../lib/timeRange'
import type { ChartAggregation } from '../types'

// SystemHealthBar 是仪表盘的系统级「健康状态」条：把所选时间跨度内服务端聚合的
// 时间线（每个桶的请求数 / 4xx / 5xx）渲染成 Statuspage 风格竖条状态柱 + 总体成功率。
// 复用 Dashboard 已加载的 chartData，无需额外请求；时间跨度跟随顶部选择器。

interface Block {
  success: number
  failure: number
  rate: number // -1 表示该时段无请求（idle）
  startTime: number
  endTime: number
}

const COLOR_STOPS = [
  { r: 239, g: 68, b: 68 }, // #ef4444 红
  { r: 245, g: 158, b: 11 }, // #f59e0b 琥珀
  { r: 16, g: 185, b: 129 }, // #10b981 翠绿
] as const

function rateToColor(rate: number): string {
  const t = Math.max(0, Math.min(1, rate))
  const segment = t < 0.5 ? 0 : 1
  const localT = segment === 0 ? t * 2 : (t - 0.5) * 2
  const from = COLOR_STOPS[segment]
  const to = COLOR_STOPS[segment + 1]
  const r = Math.round(from.r + (to.r - from.r) * localT)
  const g = Math.round(from.g + (to.g - from.g) * localT)
  const b = Math.round(from.b + (to.b - from.b) * localT)
  return `rgb(${r}, ${g}, ${b})`
}

function formatDate(timestamp: number): string {
  const date = new Date(timestamp)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

function formatClock(timestamp: number, withDate: boolean): string {
  const date = new Date(timestamp)
  const pad = (n: number) => String(n).padStart(2, '0')
  const hm = `${pad(date.getHours())}:${pad(date.getMinutes())}`
  return withDate ? `${formatDate(timestamp)} ${hm}` : hm
}

function formatSuccessRate(rate: number): string {
  const rounded = rate.toFixed(1)
  return `${rounded.endsWith('.0') ? rounded.slice(0, -2) : rounded}%`
}

interface Props {
  chartData: ChartAggregation | null
  timeRange: TimeRangeKey
  loading?: boolean
}

export default function SystemHealthBar({ chartData, timeRange, loading = false }: Props) {
  const { t } = useTranslation()
  const [activeTooltip, setActiveTooltip] = useState<number | null>(null)
  const blocksRef = useRef<HTMLDivElement>(null)

  const { bucketMinutes, bucketCount } = getBucketConfig(timeRange)
  const bucketDurationMs = bucketMinutes * 60 * 1000
  const isDayBucket = bucketMinutes >= 1440
  const withDate = timeRange === '24h' || timeRange === '7d' || timeRange === '30d'

  const tzOffsetMs = new Date().getTimezoneOffset() * 60 * 1000
  const alignToBucket = (ms: number) =>
    Math.floor((ms - tzOffsetMs) / bucketDurationMs) * bucketDurationMs + tzOffsetMs
  const now = Date.now()
  const lastBucketStart = alignToBucket(now)
  const windowStart = lastBucketStart - (bucketCount - 1) * bucketDurationMs

  const blocks: Block[] = Array.from({ length: bucketCount }, (_, idx) => {
    const start = windowStart + idx * bucketDurationMs
    return { success: 0, failure: 0, rate: -1, startTime: start, endTime: start + bucketDurationMs }
  })

  for (const point of chartData?.timeline ?? []) {
    const ts = new Date(point.bucket).getTime()
    if (Number.isNaN(ts)) continue
    let idx = Math.floor((alignToBucket(ts) - windowStart) / bucketDurationMs)
    if (idx < 0) idx = 0
    if (idx >= bucketCount) idx = bucketCount - 1
    const failure = point.errors_4xx + point.errors_5xx
    const success = Math.max(0, point.requests - failure)
    blocks[idx].success += success
    blocks[idx].failure += failure
  }

  for (const block of blocks) {
    const total = block.success + block.failure
    block.rate = total > 0 ? block.success / total : -1
  }

  const totalSuccess = blocks.reduce((sum, b) => sum + b.success, 0)
  const totalFailure = blocks.reduce((sum, b) => sum + b.failure, 0)
  const grandTotal = totalSuccess + totalFailure
  const hasData = grandTotal > 0
  const successRate = hasData ? (totalSuccess / grandTotal) * 100 : 100

  const rateClass = !hasData
    ? 'border-border/70 bg-muted/70 text-muted-foreground'
    : successRate >= 95
      ? 'border-emerald-500/20 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300'
      : successRate >= 80
        ? 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300'
        : 'border-destructive/25 bg-destructive/10 text-destructive'

  useEffect(() => {
    if (activeTooltip === null) return
    const handler = (e: PointerEvent) => {
      if (blocksRef.current && !blocksRef.current.contains(e.target as Node)) {
        setActiveTooltip(null)
      }
    }
    document.addEventListener('pointerdown', handler)
    return () => document.removeEventListener('pointerdown', handler)
  }, [activeTooltip])

  const handlePointerEnter = useCallback((e: React.PointerEvent, idx: number) => {
    if (e.pointerType === 'mouse') setActiveTooltip(idx)
  }, [])
  const handlePointerLeave = useCallback((e: React.PointerEvent) => {
    if (e.pointerType === 'mouse') setActiveTooltip(null)
  }, [])
  const handlePointerDown = useCallback((e: React.PointerEvent, idx: number) => {
    if (e.pointerType === 'touch') {
      e.preventDefault()
      setActiveTooltip((prev) => (prev === idx ? null : idx))
    }
  }, [])

  const total = blocks.length
  const startLabel = isDayBucket ? formatDate(windowStart) : formatClock(windowStart, withDate)
  const endLabel = isDayBucket ? formatDate(lastBucketStart) : formatClock(lastBucketStart + bucketDurationMs, withDate)

  const renderTooltip = (block: Block, idx: number) => {
    const count = block.success + block.failure
    const posClass =
      idx <= 3
        ? 'left-0 translate-x-0'
        : idx >= total - 4
          ? 'right-0 left-auto translate-x-0'
          : 'left-1/2 -translate-x-1/2'
    const timeRangeLabel = isDayBucket
      ? formatDate(block.startTime)
      : `${formatClock(block.startTime, withDate)} – ${formatClock(block.endTime, false)}`
    return (
      <div
        className={`pointer-events-none absolute bottom-[calc(100%+10px)] z-30 whitespace-nowrap rounded-xl border border-border/80 bg-popover/95 px-3 py-2 text-[11px] leading-snug text-popover-foreground shadow-lg backdrop-blur-md ${posClass}`}
      >
        <span className="mb-1 block font-mono text-[10.5px] text-muted-foreground">{timeRangeLabel}</span>
        {count > 0 ? (
          <span className="flex items-center gap-2.5 font-mono tabular-nums">
            <span className="font-semibold text-emerald-600 dark:text-emerald-400">
              {t('dashboard.healthBarSuccessShort')} {block.success}
            </span>
            <span className="font-semibold text-rose-500">
              {t('dashboard.healthBarFailureShort')} {block.failure}
            </span>
            <span className="text-muted-foreground">({(block.rate * 100).toFixed(1)}%)</span>
          </span>
        ) : (
          <span className="text-muted-foreground">{t('dashboard.healthBarNoRequests')}</span>
        )}
      </div>
    )
  }

  return (
    <Card className="rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
      <CardContent className="p-4 sm:p-5">
        <div className="mb-3.5 flex flex-wrap items-center justify-between gap-3">
          <div className="flex items-center gap-2">
            <span className="flex size-7 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <Activity className="size-3.5" />
            </span>
            <div>
              <h3 className="text-sm font-semibold leading-tight text-foreground">
                {t('dashboard.systemHealthTitle')}
              </h3>
            </div>
          </div>

          <div className="flex items-center gap-3">
            <div className="hidden items-center gap-3 text-[11px] text-muted-foreground sm:flex">
              <span className="inline-flex items-center gap-1">
                <span className="size-2 rounded-xs bg-emerald-500" />
                {t('dashboard.healthBarSuccessShort')}
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="size-2 rounded-xs bg-amber-500" />
                {t('dashboard.rateLimited')}
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="size-2 rounded-xs bg-rose-500" />
                {t('dashboard.healthBarFailureShort')}
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="size-2 rounded-xs bg-muted-foreground/25" />
                {t('dashboard.healthBarNoRequests')}
              </span>
            </div>
            <span
              className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 font-sans text-xs font-semibold tabular-nums ${rateClass}`}
            >
              <span className={`size-1.5 rounded-full ${!hasData ? 'bg-muted-foreground/50' : successRate >= 95 ? 'bg-emerald-500' : successRate >= 80 ? 'bg-amber-500' : 'bg-rose-500'}`} />
              {hasData ? formatSuccessRate(successRate) : '--'}
            </span>
          </div>
        </div>

        {loading && !hasData ? (
          <div className="flex gap-[3px]">
            {Array.from({ length: bucketCount }).map((_, i) => (
              <div
                key={i}
                className="h-7 flex-1 animate-pulse rounded-[4px] bg-muted/60"
                style={{ animationDelay: `${i * 25}ms` }}
              />
            ))}
          </div>
        ) : (
          <div className="relative flex items-center gap-[3px]" ref={blocksRef}>
            {blocks.map((block, idx) => {
              const isIdle = block.rate === -1
              const isActive = activeTooltip === idx
              return (
                <div
                  key={idx}
                  className="group relative min-w-[3px] flex-1 cursor-pointer py-0.5"
                  onPointerEnter={(e) => handlePointerEnter(e, idx)}
                  onPointerLeave={handlePointerLeave}
                  onPointerDown={(e) => handlePointerDown(e, idx)}
                >
                  <div
                    className={`h-7 w-full rounded-[4px] transition-all duration-150 group-hover:scale-y-110 group-hover:brightness-105 ${
                      isIdle ? 'bg-muted/70 hover:bg-muted' : ''
                    } ${isActive ? 'scale-y-110 ring-2 ring-primary/40 ring-offset-1 ring-offset-card' : ''}`}
                    style={isIdle ? undefined : { backgroundColor: rateToColor(block.rate) }}
                  />
                  {isActive && renderTooltip(block, idx)}
                </div>
              )
            })}
          </div>
        )}

        <div className="mt-2 flex items-center justify-between font-sans text-[11px] text-muted-foreground/80 tabular-nums">
          <span>{startLabel}</span>
          <span>
            {hasData
              ? `${totalSuccess.toLocaleString()} ${t('dashboard.healthBarSuccessShort')} · ${totalFailure.toLocaleString()} ${t('dashboard.healthBarFailureShort')}`
              : t('dashboard.healthBarNoRequests')}
          </span>
          <span>{endLabel}</span>
        </div>
      </CardContent>
    </Card>
  )
}
