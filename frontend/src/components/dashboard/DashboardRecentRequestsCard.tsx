import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { ArrowRight, Clock } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import ChannelLogo from '../ChannelLogo'
import ModelLogo from '../ModelLogo'
import type { UpstreamChannel, UsageLog } from '../../types'
import { getTimezone } from '../../utils/time'
import { cn } from '@/lib/utils'

function toUpstreamChannel(raw?: string): UpstreamChannel {
  switch (raw) {
    case 'codex':
    case 'grok':
    case 'antigravity':
    case 'claude':
    case 'kiro':
    case 'opencode':
      return raw
    default:
      return 'codex'
  }
}

interface DashboardRecentRequestsCardProps {
  logs: UsageLog[]
  loading?: boolean
}

function formatClockTime(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return '-'
  return date.toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    timeZone: getTimezone(),
  })
}

function formatDuration(ms?: number): string {
  if (!ms || ms <= 0) return '-'
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`
  return `${Math.round(ms)}ms`
}

function formatCompactTokens(tokens?: number): string {
  if (!tokens || tokens <= 0) return '0'
  if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(2)}M`
  if (tokens >= 1_000) return `${(tokens / 1_000).toFixed(1)}K`
  return String(tokens)
}

function formatUsd(v?: number): string {
  if (!v || v <= 0) return '-'
  if (v >= 1) return `$${v.toFixed(2)}`
  return `$${v.toFixed(4)}`
}

export default function DashboardRecentRequestsCard({ logs, loading = false }: DashboardRecentRequestsCardProps) {
  const { t } = useTranslation()

  return (
    <Card className="overflow-hidden rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
      <CardContent className="p-0">
        <div className="flex flex-wrap items-center justify-between gap-3 px-5 py-4">
          <div>
            <h3 className="text-[15.5px] font-semibold tracking-tight text-foreground">
              {t('dashboard.recentRequestsTitle')}
            </h3>
            <p className="mt-0.5 text-xs text-muted-foreground">
              {t('dashboard.recentRequestsDesc')}
            </p>
          </div>
          <Button asChild variant="outline" size="sm" className="rounded-lg">
            <Link to="/usage">
              {t('dashboard.viewAllUsage')}
              <ArrowRight className="size-3.5" />
            </Link>
          </Button>
        </div>

        {loading && logs.length === 0 ? (
          <div className="space-y-2 px-5 pb-5">
            {[0, 1, 2, 3].map((i) => (
              <div key={i} className="h-10 w-full animate-pulse rounded-lg bg-muted/55" />
            ))}
          </div>
        ) : logs.length === 0 ? (
          <div className="mx-5 mb-5 flex flex-col items-center justify-center gap-1.5 rounded-xl border border-dashed border-border/75 bg-muted/20 py-9 text-center text-xs text-muted-foreground">
            <Clock className="size-5 opacity-50" />
            <span>{t('dashboard.recentRequestsEmpty')}</span>
          </div>
        ) : (
          <div className="overflow-x-auto border-t border-border/60">
            <table className="w-full min-w-[660px] border-collapse text-left text-xs">
              <thead>
                <tr className="border-b border-border/60 bg-muted/30 text-[11px] font-medium text-muted-foreground">
                  <th className="py-2.5 pl-5 pr-3 font-medium">{t('usage.tableTime')}</th>
                  <th className="px-3 py-2.5 font-medium">{t('usage.tableModel')}</th>
                  <th className="px-3 py-2.5 font-medium">{t('usage.tableAccount')}</th>
                  <th className="px-3 py-2.5 font-medium">{t('usage.tableStatus')}</th>
                  <th className="px-3 py-2.5 text-right font-medium">{t('dashboard.kpiAvgTtft')} / {t('usage.tableDuration')}</th>
                  <th className="px-3 py-2.5 text-right font-medium">Tokens</th>
                  <th className="py-2.5 pl-3 pr-5 text-right font-medium">{t('usage.tableCost')}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border/45">
                {logs.map((log) => {
                  const modelName = log.effective_model || log.model || '-'
                  const statusOk = log.status_code >= 200 && log.status_code < 300
                  const statusWarn = log.status_code >= 400 && log.status_code < 500
                  const accountLabel = log.account_email || log.account_name || (log.account_id > 0 ? `#${log.account_id}` : '-')
                  const channelKey = toUpstreamChannel(log.channel)
                  return (
                    <tr key={log.id} className="transition-colors hover:bg-muted/30">
                      <td className="py-3 pl-5 pr-3 font-sans text-xs text-muted-foreground tabular-nums">
                        {formatClockTime(log.created_at)}
                      </td>
                      <td className="px-3 py-3">
                        <div className="flex max-w-[240px] items-center gap-2">
                          <ChannelLogo channel={channelKey} size={14} />
                          <ModelLogo model={modelName} size={15} className="shrink-0" />
                          <div className="min-w-0">
                            <span className="block truncate font-medium text-foreground" title={modelName}>
                              {modelName}
                            </span>
                            {log.endpoint ? (
                              <span className="block truncate font-mono text-[10.5px] text-muted-foreground/80">
                                {log.endpoint}
                              </span>
                            ) : null}
                          </div>
                        </div>
                      </td>
                      <td className="max-w-[170px] truncate px-3 py-3 text-muted-foreground" title={accountLabel}>
                        {accountLabel}
                      </td>
                      <td className="px-3 py-3">
                        <span
                          className={cn(
                            'inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 font-sans text-[11.5px] font-medium tabular-nums',
                            statusOk
                              ? 'border-emerald-500/20 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300'
                              : statusWarn
                                ? 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300'
                                : 'border-destructive/25 bg-destructive/10 text-destructive',
                          )}
                        >
                          <span className={cn('size-1.5 rounded-full', statusOk ? 'bg-emerald-500' : statusWarn ? 'bg-amber-500' : 'bg-rose-500')} />
                          {log.status_code}
                        </span>
                      </td>
                      <td className="px-3 py-3 text-right font-sans text-xs text-muted-foreground tabular-nums">
                        <span className="font-medium text-foreground">{formatDuration(log.first_token_ms)}</span>
                        <span className="mx-1 opacity-40">/</span>
                        <span>{formatDuration(log.duration_ms)}</span>
                      </td>
                      <td className="px-3 py-3 text-right font-sans text-xs font-medium text-foreground tabular-nums">
                        {formatCompactTokens(log.total_tokens)}
                      </td>
                      <td className="py-3 pl-3 pr-5 text-right font-sans text-xs font-medium text-emerald-600 tabular-nums dark:text-emerald-400">
                        {formatUsd(log.user_billed)}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  )
}
