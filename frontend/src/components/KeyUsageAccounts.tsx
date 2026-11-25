import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { RefreshCw, Search } from 'lucide-react'
import { AdminAPIError, api, type PublicKeyUsageAccount } from '../api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import KeyUsageAccountCard from './KeyUsageAccountCard'
import StateShell from './StateShell'
import { getErrorMessage } from '../utils/error'
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
      if (e instanceof AdminAPIError && [401, 403, 404].includes(e.status)) {
        setDisabled(e.status === 404)
        if (e.status !== 404) setError(getErrorMessage(e))
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
      <div className="codex-account-grid">{visible.map((account, index) =>
        <KeyUsageAccountCard key={account.id} apiKey={apiKey} onRefresh={() => setGeneration(value => value + 1)} account={account} sequence={index + 1} groups={groups}
          busy={busy !== null} pending={busy === account.id} onRedeem={() => void redeem(account)} />
      )}</div>
    </StateShell>
  </section>
}
