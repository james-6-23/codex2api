import { useTranslation } from 'react-i18next'
import { CalendarClock, Loader2 } from 'lucide-react'
import type { APIKeyModelRequestUsage } from '../types'
import { Card, CardContent } from '@/components/ui/card'

export default function APIKeyModelRequestUsageCard({
  items,
  loading = false,
  error = '',
}: {
  items: APIKeyModelRequestUsage[]
  loading?: boolean
  error?: string
}) {
  const { t, i18n } = useTranslation()
  if (!items.length && !loading && !error) return null

  const formatReset = (value: string, timezone: string) => {
    try {
      return new Intl.DateTimeFormat(i18n.language, {
        timeZone: timezone,
        year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', hour12: false,
      }).format(new Date(value))
    } catch {
      return value
    }
  }

  return (
    <Card>
      <CardContent className="space-y-3 p-4">
        <h3 className="flex items-center gap-2 text-sm font-semibold">
          <CalendarClock className="size-4 text-primary" />
          {t('modelRequests.usageTitle')}
        </h3>
        <p className="text-xs leading-relaxed text-muted-foreground">{t('modelRequests.usageHint')}</p>
        {loading ? <div className="flex items-center gap-2 text-xs text-muted-foreground"><Loader2 className="size-3.5 animate-spin" />{t('common.loading')}</div> : null}
        {error ? <p role="alert" className="text-xs text-destructive">{error}</p> : null}
        {items.map((item) => (
          <div key={item.rule_id} className="space-y-2 rounded-lg border border-border bg-muted/20 p-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <code className="break-all text-sm font-semibold">{item.model}</code>
              <span className={`text-xs font-medium ${item.remaining <= 0 ? 'text-destructive' : 'text-muted-foreground'}`}>
                {t('modelRequests.remaining', { count: item.remaining })}
              </span>
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-muted" aria-hidden="true">
              <div
                className={`h-full rounded-full ${item.remaining <= 0 ? 'bg-destructive' : 'bg-primary'}`}
                style={{ width: `${item.limit > 0 ? Math.min(100, Math.max(0, item.used / item.limit * 100)) : 0}%` }}
              />
            </div>
            <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
              <span>{t('modelRequests.used', { used: item.used, limit: item.limit })}</span>
              <span>{t('modelRequests.resetAt', { time: formatReset(item.reset_at, item.timezone), timezone: item.timezone })}</span>
            </div>
          </div>
        ))}
      </CardContent>
    </Card>
  )
}
