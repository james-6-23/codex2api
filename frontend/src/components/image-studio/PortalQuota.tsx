import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { AlertTriangle, ChevronDown, KeyRound, RefreshCw, Sparkles, Wallet } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { formatImageStudioQuota } from '@/lib/imageStudioQuota'
import { formatBeijingTime } from '@/utils/time'
import type { ImageStudioQuota } from '@/types'

export function PortalQuota({ maskedKey, quota, loading, error, onRefresh }: {
  maskedKey: string
  quota: ImageStudioQuota | null
  loading: boolean
  error: string
  onRefresh: () => void
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const amount = quota?.quota_remaining === null
    ? t('imageStudioPortal.quota.unlimited')
    : formatImageStudioQuota(quota?.quota_remaining)
  const label = error ? t('imageStudioPortal.quota.unavailable') : quota
    ? quota.quota_remaining === null ? t('imageStudioPortal.quota.unlimitedLabel') : t('imageStudioPortal.quota.remainingAmount', { amount })
    : t(loading ? 'imageStudioPortal.quota.loading' : 'imageStudioPortal.quota.unavailable')
  const limited = quota != null && quota.quota_remaining !== null && quota.quota_limit > 0
  const remainingRatio = limited ? Math.max(0, Math.min(1, (quota.quota_remaining ?? 0) / quota.quota_limit)) : null
  const low = remainingRatio !== null && remainingRatio <= 0.1
  const warning = Boolean(error || low || quota?.status === 'expired' || quota?.status === 'quota_exhausted')

  return (
    <div className="portal-key-summary">
      <Button
        type="button"
        variant="outline"
        className="portal-quota-trigger"
        data-warning={warning}
        onClick={() => { setOpen(true); onRefresh() }}
      >
        <span className="portal-quota-icon" aria-hidden="true">
          <KeyRound className="size-3.5" />
        </span>
        <span className="portal-quota-key">{maskedKey}</span>
        <span className="portal-quota-balance">
          <span className="portal-quota-dot" aria-hidden="true" />
          {label}
        </span>
        {error ? (
          <AlertTriangle className="size-3.5 text-destructive" aria-label={t('imageStudioPortal.quota.refreshFailed')} />
        ) : (
          <ChevronDown className="size-3.5 text-muted-foreground" />
        )}
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="portal-quota-dialog sm:max-w-[440px]">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <span className="flex size-7 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/20">
                <Wallet className="size-4" />
              </span>
              {t('imageStudioPortal.quota.title')}
            </DialogTitle>
            <DialogDescription>{t('imageStudioPortal.quota.description')}</DialogDescription>
          </DialogHeader>
          <div className="portal-quota-amount" data-warning={warning} aria-live="polite">
            <div className="portal-quota-amount-head">
              <span>
                <Sparkles className="size-3.5" />
                {t(error && quota ? 'imageStudioPortal.quota.lastKnownRemaining' : 'imageStudioPortal.quota.remaining')}
              </span>
              <span className="portal-quota-key-pill">{maskedKey}</span>
            </div>
            <strong>{quota ? amount : '—'}</strong>
            {remainingRatio !== null ? (
              <div className="portal-quota-meter" aria-hidden="true">
                <div
                  className="portal-quota-meter-fill"
                  style={{ width: `${Math.max(remainingRatio * 100, remainingRatio > 0 ? 3 : 0)}%` }}
                />
              </div>
            ) : null}
            {quota?.status === 'expired' || quota?.status === 'quota_exhausted' ? (
              <p className="text-destructive">{t(`imageStudioPortal.quota.${quota.status}`)}</p>
            ) : null}
          </div>
          {quota ? (
            <dl className="portal-quota-details">
              <div>
                <dt>{t('imageStudioPortal.quota.limit')}</dt>
                <dd>{quota.quota_remaining === null ? t('imageStudioPortal.quota.unlimited') : formatImageStudioQuota(quota.quota_limit)}</dd>
              </div>
              <div>
                <dt>{t('imageStudioPortal.quota.used')}</dt>
                <dd>{formatImageStudioQuota(quota.quota_used)}</dd>
              </div>
              <div>
                <dt>{t('imageStudioPortal.quota.expiresAt')}</dt>
                <dd>{quota.expires_at ? formatBeijingTime(quota.expires_at) : t('imageStudioPortal.quota.neverExpires')}</dd>
              </div>
            </dl>
          ) : null}
          {error ? <p role="alert" className="portal-quota-error">{t('imageStudioPortal.quota.refreshFailed')} · {error}</p> : null}
          <p className="portal-quota-note">{t('imageStudioPortal.quota.settlementHint')}</p>
          <Button type="button" variant="outline" className="w-full" onClick={onRefresh} disabled={loading}>
            <RefreshCw className={loading ? 'size-4 animate-spin' : 'size-4'} />
            {t(loading ? 'imageStudioPortal.quota.refreshing' : 'imageStudioPortal.quota.refresh')}
          </Button>
        </DialogContent>
      </Dialog>
    </div>
  )
}
