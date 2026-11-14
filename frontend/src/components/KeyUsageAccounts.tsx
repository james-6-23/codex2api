import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Clock3, Gauge, RefreshCw, RotateCcw, Search, Ticket } from 'lucide-react'
import { AdminAPIError, api, type PublicKeyUsageAccount } from '../api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { AccountCardSurface } from './AccountCardSurface'
import AccountUsageBar from './AccountUsageBar'
import PlanBadge from './AccountPlanBadge'
import StatusBadge from './StatusBadge'
import StateShell from './StateShell'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { createUUID } from '../lib/uuid'

export default function KeyUsageAccounts({ apiKey }: { apiKey: string }) {
  const { t } = useTranslation()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [accounts, setAccounts] = useState<PublicKeyUsageAccount[]>([])
  const [groups, setGroups] = useState<{ id: number; name: string }[]>([])
  const [group, setGroup] = useState('all')
  const [search, setSearch] = useState('')
  const [loading, setLoading] = useState(true)
  const [disabled, setDisabled] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState<number | null>(null)
  const [notice, setNotice] = useState('')
  const [generation, setGeneration] = useState(0)
  const actionPending = useRef(false)

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setError('')
    void api.getPublicKeyUsageAccounts(apiKey, { signal: controller.signal }).then(data => {
      if (controller.signal.aborted) return
      setAccounts(data.accounts)
      setGroups(data.groups)
      setDisabled(false)
      setGroup(current => current === 'all' || current === 'ungrouped' || data.groups.some(g => String(g.id) === current) ? current : 'all')
    }).catch(e => {
      if (controller.signal.aborted) return
      if (e instanceof AdminAPIError && e.status === 404) {
        setDisabled(true)
        setAccounts([])
        setGroups([])
      } else setError(getErrorMessage(e))
    }).finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [apiKey, generation])

  const redeem = async (account: PublicKeyUsageAccount) => {
    if (actionPending.current) return
    actionPending.current = true
    try {
      const accepted = await confirm({
        title: t('keyUsageAccounts.redeem'),
        description: <div className="space-y-3"><p className="font-medium">{account.name || account.email || `#${account.id}`}</p><p>{t('keyUsageAccounts.confirm')}</p></div>,
        confirmText: t('keyUsageAccounts.redeem'), tone: 'destructive',
      })
      if (!accepted) return
      setBusy(account.id)
      setError('')
      setNotice('')
      const result = await api.resetPublicKeyUsageCredits(apiKey, account.id, createUUID())
      setNotice(t(result.usage_refreshed ? 'keyUsageAccounts.done' : 'keyUsageAccounts.refreshPending'))
      setGeneration(value => value + 1)
    } catch (e) { setError(getErrorMessage(e)) }
    finally { setBusy(null); actionPending.current = false }
  }

  const visible = useMemo(() => accounts.filter(account => {
    const matchesGroup = group === 'all' || (group === 'ungrouped' ? account.group_ids.length === 0 : account.group_ids.includes(Number(group)))
    const needle = search.trim().toLowerCase()
    return matchesGroup && (!needle || `${account.name} ${account.email}`.toLowerCase().includes(needle))
  }), [accounts, group, search])

  return <section className="codex-account-list space-y-5" aria-label={t('keyUsageAccounts.title')}>
    {confirmDialog}
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div><h2 className="text-base font-semibold">{t('keyUsageAccounts.title')}</h2><p className="mt-1 text-sm text-muted-foreground">{t('keyUsageAccounts.accountScope')}</p></div>
      <Button variant="outline" size="sm" onClick={() => setGeneration(value => value + 1)} disabled={loading || busy !== null}><RefreshCw className={`size-3.5 ${loading ? 'animate-spin' : ''}`} />{t('keyUsageAccounts.refresh')}</Button>
    </div>
    {!disabled && <div className="flex flex-wrap items-center gap-3 rounded-2xl border border-border bg-card p-3 shadow-sm">
      <div className="relative min-w-0 flex-1"><Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" /><Input aria-label={t('keyUsageAccounts.search')} placeholder={t('keyUsageAccounts.search')} value={search} onChange={event => setSearch(event.target.value)} className="pl-9" /></div>
      <Select className="w-full sm:w-52" aria-label={t('keyUsageAccounts.group')} value={group} onValueChange={setGroup} options={[{ value: 'all', label: t('keyUsageAccounts.allGroups') }, ...groups.map(item => ({ value: String(item.id), label: item.name })), { value: 'ungrouped', label: t('keyUsageAccounts.ungrouped') }]} />
      <span className="text-xs text-muted-foreground">{t('keyUsageAccounts.accountCount', { count: visible.length })}</span>
    </div>}
    {notice && <div className="rounded-xl border border-border bg-card px-4 py-3 text-sm" role="status">{notice}</div>}
    {error && accounts.length > 0 && <div className="rounded-xl border border-destructive/30 bg-destructive/5 px-4 py-3 text-sm text-destructive" role="alert">{error}</div>}
    <StateShell variant="section" loading={loading && accounts.length === 0} error={accounts.length === 0 ? error : undefined}
      isEmpty={disabled || (!loading && visible.length === 0)} emptyTitle={t(disabled ? 'keyUsageAccounts.disabled' : 'keyUsageAccounts.empty')}
      emptyDescription={t(disabled ? 'keyUsageAccounts.disabledHint' : 'keyUsageAccounts.emptyHint')} onRetry={() => setGeneration(value => value + 1)}>
      <div className="codex-account-grid">{visible.map(account => {
        const displayName = account.email || account.name || `#${account.id}`
        const updated = [account.usage_updated_at_5h, account.usage_updated_at_7d].filter((value): value is string => !!value && Number.isFinite(Date.parse(value))).sort((a, b) => Date.parse(b) - Date.parse(a))[0]
        return <AccountCardSurface key={account.id} data-state={account.status} aria-label={account.name || displayName}>
          <header className="codex-account-card__header">
            <div className="codex-account-card__identity">
              <div className="codex-account-card__avatar" aria-hidden="true">{displayName[0].toUpperCase()}</div>
              <div className="min-w-0 flex-1"><div className="codex-account-card__name truncate" title={displayName}>{displayName}</div>{account.name && account.name !== displayName && <p className="mt-1 truncate text-xs text-muted-foreground" title={account.name}>{account.name}</p>}</div>
              <span className="codex-account-card__sequence">{t('keyUsageAccounts.idLabel', { id: account.id })}</span>
            </div>
            <div className="codex-account-card__topline"><PlanBadge planType={account.plan_type} /><StatusBadge status={account.status} /></div>
          </header>
          <div className="codex-account-card__body"><section className="codex-account-card__usage">
            <div className="codex-account-card__section-title"><span className="inline-flex items-center gap-1.5"><Gauge className="size-3.5 text-primary/80" />{t('accounts.cardUsageTitle')}</span></div>
            <div className="account-usage-cell"><div className="account-usage-windows"><AccountUsageBar label="5h" pct={account.usage_percent_5h} resetAt={account.reset_5h_at} /><AccountUsageBar label={account.long_window_seconds ? `${Math.round(account.long_window_seconds / 86400)}d` : '7d'} pct={account.usage_percent_7d} resetAt={account.reset_7d_at} /></div></div>
          </section></div>
          <div className="codex-account-card__metadata"><div className="codex-account-card__tags">{account.group_ids.length ? account.group_ids.map(id => <Badge key={id} variant="secondary">{groups.find(item => item.id === id)?.name || t('keyUsageAccounts.groupLabel', { id })}</Badge>) : <Badge variant="outline">{t('keyUsageAccounts.ungrouped')}</Badge>}</div></div>
          <dl className="codex-account-card__timestamps"><div><dt><Clock3 className="size-3 text-muted-foreground/75" />{t('keyUsageAccounts.updated')}</dt><dd><time dateTime={updated} title={updated ? formatBeijingTime(updated) : undefined}>{updated ? formatRelativeTime(updated) : t('keyUsageAccounts.unknown')}</time></dd></div></dl>
          <footer className="codex-account-card__actions codex-account-card__actions--portal">
            <span className="inline-flex items-center gap-1.5 px-2 text-xs text-muted-foreground"><Ticket className="size-3.5" />{t('keyUsageAccounts.cardCount', { value: account.reset_credits ?? t('keyUsageAccounts.unknown') })}</span>
            {account.reset_allowed && <Button variant="ghost" size="sm" onClick={() => void redeem(account)} disabled={busy !== null || account.reset_credits === 0}><RotateCcw className={`size-3.5 ${busy === account.id ? 'animate-spin' : ''}`} />{t(busy === account.id ? 'keyUsageAccounts.pending' : 'keyUsageAccounts.redeem')}</Button>}
          </footer>
        </AccountCardSurface>
      })}</div>
    </StateShell>
  </section>
}
