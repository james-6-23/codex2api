import { useTranslation } from 'react-i18next'
import { formatBeijingTime } from '../utils/time'

export default function AccountUsageBar({ label, pct, resetAt, detailText }: {
  label: string
  pct: number | null
  resetAt?: string
  detailText?: string
}) {
  const { t } = useTranslation()
  const known = pct !== null && Number.isFinite(pct)
  const resetTime = resetAt && Date.parse(resetAt) > Date.now()
    ? formatBeijingTime(resetAt, '') : ''
  const color = known && pct >= 90 ? 'bg-red-500'
    : known && pct >= 70 ? 'bg-amber-500' : 'bg-emerald-500'
  return <div className="account-usage-window">
    <div className="flex items-center gap-1.5">
      <span className="w-10 shrink-0 text-[11px] font-medium text-muted-foreground">{label}</span>
      <div className="account-usage-track h-1.5 w-[88px] shrink-0 overflow-hidden rounded-full bg-muted"
        role={known ? 'progressbar' : undefined} aria-label={label}
        aria-valuemin={known ? 0 : undefined} aria-valuemax={known ? 100 : undefined}
        aria-valuenow={known ? Math.max(0, Math.min(100, pct)) : undefined}
        aria-valuetext={known ? `${pct.toFixed(1)}%` : undefined}>
        {known && <div className={`h-full rounded-full transition-all ${color}`} style={{ width: `${Math.max(0, Math.min(100, pct))}%` }} />}
      </div>
      <span className="w-[42px] shrink-0 text-right text-[12px] font-semibold tabular-nums">{known ? `${pct.toFixed(1)}%` : t('keyUsageAccounts.unknown')}</span>
    </div>
    <div className="account-usage-window__details">
      {detailText && <div className="account-usage-meta mt-0.5 pl-[46px] text-[11px] font-medium text-muted-foreground">{detailText}</div>}
      {resetTime && <div className="account-usage-meta mt-0.5 pl-[46px] text-[11px] font-medium text-muted-foreground" title={resetTime}>⏱ {resetTime.slice(5)}</div>}
    </div>
  </div>
}
