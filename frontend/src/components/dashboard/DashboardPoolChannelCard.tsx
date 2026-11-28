import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { Layers, Sparkles } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import ChannelLogo from '../ChannelLogo'
import ModelLogo from '../ModelLogo'
import type { UsageChannel } from '../ChannelFilter'
import type { ChartModelPoint, StatsChannelCounts } from '../../types'
import { cn } from '@/lib/utils'

export interface ChannelBreakdownEntry {
  key: 'codex' | 'grok' | 'antigravity' | 'claude' | 'kiro' | 'opencode'
  label: string
  counts: StatsChannelCounts
}

interface DashboardPoolChannelCardProps {
  total: number
  available: number
  rateLimited: number
  errorCount: number
  activeChannel: UsageChannel
  channelBreakdown: ChannelBreakdownEntry[]
  models: ChartModelPoint[]
  onSelectChannel: (channel: UsageChannel) => void
}

function formatMoney(v?: number): string {
  if (!v || v <= 0) return ''
  if (v >= 10) return `$${v.toFixed(1)}`
  if (v >= 0.01) return `$${v.toFixed(2)}`
  return `$${v.toFixed(4)}`
}

export default function DashboardPoolChannelCard({
  total,
  available,
  rateLimited,
  errorCount,
  activeChannel,
  channelBreakdown,
  models,
  onSelectChannel,
}: DashboardPoolChannelCardProps) {
  const { t } = useTranslation()

  const healthyPct = total > 0 ? Math.round((available / total) * 100) : 0
  const otherCount = Math.max(0, total - available - rateLimited - errorCount)
  const segments = useMemo(() => {
    if (total <= 0) return []
    return [
      { key: 'available', count: available, pct: (available / total) * 100, cls: 'bg-emerald-500' },
      { key: 'rateLimited', count: rateLimited, pct: (rateLimited / total) * 100, cls: 'bg-amber-500' },
      { key: 'error', count: errorCount, pct: (errorCount / total) * 100, cls: 'bg-rose-500' },
      { key: 'other', count: otherCount, pct: (otherCount / total) * 100, cls: 'bg-muted-foreground/30' },
    ].filter((s) => s.count > 0)
  }, [available, errorCount, otherCount, rateLimited, total])

  const topModels = useMemo(() => {
    const sorted = models.slice().sort((a, b) => b.requests - a.requests).slice(0, 5)
    const maxReq = sorted[0]?.requests || 1
    return sorted.map((m) => ({
      ...m,
      pct: Math.max(6, Math.round((m.requests / maxReq) * 100)),
    }))
  }, [models])

  return (
    <Card className="flex h-full flex-col rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
      <CardContent className="flex flex-1 flex-col gap-5 p-5">
        {/* Section 1: Pool Status Distribution */}
        <div>
          <div className="flex items-start justify-between gap-3">
            <div>
              <h3 className="text-[15.5px] font-semibold tracking-tight text-foreground">
                {t('dashboard.poolHealthCardTitle')}
              </h3>
              <p className="mt-0.5 text-xs text-muted-foreground">
                {t('dashboard.poolHealthCardDesc')}
              </p>
            </div>
            <div className="text-right">
              <strong
                className={cn(
                  'font-sans text-[26px] leading-none font-semibold tracking-[-0.03em] tabular-nums',
                  total === 0
                    ? 'text-muted-foreground'
                    : healthyPct >= 80
                      ? 'text-emerald-600 dark:text-emerald-400'
                      : healthyPct >= 50
                        ? 'text-amber-600 dark:text-amber-400'
                        : 'text-rose-600 dark:text-rose-400',
                )}
              >
                {total > 0 ? `${healthyPct}%` : '--'}
              </strong>
              <span className="mt-0.5 block text-[11px] font-medium text-muted-foreground">
                {t('dashboard.poolHealthyRate')}
              </span>
            </div>
          </div>

          {/* Segmented gap progress track */}
          <div className="mt-3.5 flex h-2.5 w-full gap-1 overflow-hidden rounded-full bg-muted/60 p-[1px]">
            {segments.map((seg) => (
              <div
                key={seg.key}
                className={cn('h-full rounded-full transition-all duration-300', seg.cls)}
                style={{ width: `${seg.pct}%` }}
              />
            ))}
          </div>

          <div className="mt-3 grid grid-cols-3 gap-2">
            {[
              { label: t('dashboard.available'), count: available, dot: 'bg-emerald-500' },
              { label: t('dashboard.rateLimited'), count: rateLimited, dot: 'bg-amber-500' },
              { label: t('dashboard.error'), count: errorCount, dot: 'bg-rose-500' },
            ].map((item) => (
              <div key={item.label} className="flex items-center justify-between rounded-lg bg-muted/35 px-2.5 py-1.5 text-xs">
                <span className="inline-flex items-center gap-1.5 truncate text-muted-foreground">
                  <span className={cn('size-1.5 shrink-0 rounded-full', item.dot)} />
                  <span className="truncate">{item.label}</span>
                </span>
                <strong className="font-sans font-medium text-foreground tabular-nums">{item.count}</strong>
              </div>
            ))}
          </div>
        </div>

        {/* Section 2: 2-Column Multi-Channel Tiles */}
        {!activeChannel && channelBreakdown.length > 0 ? (
          <div className="border-t border-border/55 pt-4">
            <div className="mb-2.5 flex items-center justify-between gap-2">
              <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-foreground">
                <Layers className="size-3.5 text-primary" />
                {t('dashboard.channelMatrixTitle')}
              </span>
              <span className="text-[11px] text-muted-foreground">
                {t('dashboard.channelMatrixHint')}
              </span>
            </div>
            <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
              {channelBreakdown.map(({ key, label, counts }) => {
                const ratio = counts.total > 0 ? Math.round((counts.available / counts.total) * 100) : 0
                return (
                  <button
                    key={key}
                    type="button"
                    onClick={() => onSelectChannel(key)}
                    title={t('dashboard.heroChannelTitle', {
                      channel: label,
                      available: counts.available,
                      total: counts.total,
                      requests: counts.today_requests,
                    })}
                    className="group flex flex-col gap-2 rounded-xl border border-border/60 bg-muted/25 p-2.5 text-left transition-all hover:border-border hover:bg-muted/50 hover:shadow-2xs"
                  >
                    <div className="flex items-center justify-between gap-2">
                      <span className="flex min-w-0 items-center gap-1.5">
                        <ChannelLogo channel={key} size={15} />
                        <span className="truncate text-xs font-semibold text-foreground">{label}</span>
                      </span>
                      <span className="shrink-0 font-sans text-[12px] font-medium text-foreground tabular-nums">
                        {counts.available}<span className="text-muted-foreground/70">/{counts.total}</span>
                      </span>
                    </div>
                    <div className="flex items-center gap-2">
                      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
                        <div
                          className={cn(
                            'h-full rounded-full transition-all',
                            ratio >= 80 ? 'bg-emerald-500' : ratio >= 50 ? 'bg-amber-500' : 'bg-rose-500',
                          )}
                          style={{ width: `${ratio}%` }}
                        />
                      </div>
                      <span className="shrink-0 font-sans text-[11px] text-muted-foreground tabular-nums">
                        {counts.today_requests.toLocaleString()} {t('dashboard.reqUnit')}
                      </span>
                    </div>
                  </button>
                )
              })}
            </div>
          </div>
        ) : null}

        {/* Section 3: Top 5 Models */}
        <div className="mt-auto border-t border-border/55 pt-4">
          <div className="mb-3 flex items-center justify-between gap-2">
            <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-foreground">
              <Sparkles className="size-3.5 text-primary" />
              {t('dashboard.modelRanking')}
            </span>
            <span className="rounded-md bg-muted/60 px-1.5 py-0.5 font-mono text-[10.5px] font-medium text-muted-foreground">
              Top 5
            </span>
          </div>

          {topModels.length === 0 ? (
            <p className="py-3 text-center text-xs text-muted-foreground">
              {t('dashboard.chartsEmptyTitle')}
            </p>
          ) : (
            <div className="space-y-3">
              {topModels.map((item) => {
                const costText = formatMoney(item.user_billed)
                return (
                  <div key={item.model} className="space-y-1.5">
                    <div className="flex items-center justify-between gap-2 text-xs">
                      <div className="flex min-w-0 items-center gap-2">
                        <ModelLogo model={item.model} size={16} className="shrink-0" />
                        <span className="truncate font-medium text-foreground" title={item.model}>
                          {item.model}
                        </span>
                      </div>
                      <div className="flex shrink-0 items-center gap-2 font-sans text-[12px] tabular-nums">
                        {item.error_count && item.error_count > 0 ? (
                          <span className="rounded bg-amber-500/12 px-1.5 py-0.5 text-[10.5px] font-medium text-amber-700 dark:text-amber-300">
                            {item.error_count} err
                          </span>
                        ) : null}
                        {costText ? (
                          <span className="font-medium text-emerald-600 dark:text-emerald-400">{costText}</span>
                        ) : null}
                        <span className="min-w-[2.25rem] text-right font-semibold text-foreground">
                          {item.requests.toLocaleString()}
                        </span>
                      </div>
                    </div>
                    <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted/65">
                      <div
                        className="h-full rounded-full bg-gradient-to-r from-primary to-primary/55 transition-all duration-300"
                        style={{ width: `${item.pct}%` }}
                      />
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </CardContent>
    </Card>
  )
}
