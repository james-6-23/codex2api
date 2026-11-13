import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { AdminAPIError, api, type PublicKeyUsageAccount } from '../api'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/select'
import { getErrorMessage } from '../utils/error'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { createUUID } from '../lib/uuid'

export default function KeyUsageAccounts({ apiKey }: { apiKey: string }) {
  const { t } = useTranslation()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [accounts, setAccounts] = useState<PublicKeyUsageAccount[] | null>(null)
  const [groups, setGroups] = useState<{ id: number; name: string }[]>([])
  const [group, setGroup] = useState('all')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState<number | null>(null)
  const [notice, setNotice] = useState('')
  const [generation, setGeneration] = useState(0)

  useEffect(() => {
    let cancelled = false
    setAccounts(null)
    setError('')
    void api.getPublicKeyUsageAccounts(apiKey).then(data => { if (!cancelled) { setAccounts(data.accounts); setGroups(data.groups); setGroup(current => current === 'all' || current === 'ungrouped' || data.groups.some(g => String(g.id) === current) ? current : 'all') } }).catch(e => {
      if (!cancelled && !(e instanceof AdminAPIError && e.status === 404)) setError(getErrorMessage(e))
    })
    return () => { cancelled = true }
  }, [apiKey, generation])

  const redeem = async (id: number) => {
    if (busy !== null || !await confirm({ title: t('keyUsageAccounts.redeem'), description: t('keyUsageAccounts.confirm'), confirmText: t('keyUsageAccounts.redeem'), tone: 'destructive' })) return
    setBusy(id)
    setError('')
    setNotice('')
    try { await api.resetPublicKeyUsageCredits(apiKey, id, createUUID()); setNotice(t('keyUsageAccounts.done')); setGeneration(n => n + 1) }
    catch (e) { setError(getErrorMessage(e)) }
    finally { setBusy(null) }
  }
  if (!accounts && !error && !notice) return null
  const fmt = (s?: string) => s ? new Date(s).toLocaleString() : t('keyUsageAccounts.unknown')
  const visible = accounts?.filter(a => group === 'all' || (group === 'ungrouped' ? a.group_ids.length === 0 : a.group_ids.includes(Number(group))))
  return <section className="space-y-3 rounded-xl border border-border p-4">
    {confirmDialog}
    <div className="flex items-center justify-between gap-2"><h2 className="font-semibold">{t('keyUsageAccounts.title')}</h2><Button variant="outline" size="sm" onClick={() => setGeneration(n => n + 1)} disabled={busy !== null}>{t('keyUsageAccounts.refresh')}</Button></div>
    <p className="text-xs text-muted-foreground">{t('keyUsageAccounts.resetWarning')}</p>
    <Select aria-label={t('keyUsageAccounts.group')} value={group} onValueChange={setGroup} options={[{ value: 'all', label: t('keyUsageAccounts.allGroups') }, ...groups.map(g => ({ value: String(g.id), label: g.name })), { value: 'ungrouped', label: t('keyUsageAccounts.ungrouped') }]} />
    {error && <p className="text-sm text-destructive" role="alert">{error}</p>}
    {notice && <p className="text-sm" role="status">{notice}</p>}
    {visible?.length === 0 && <p className="text-sm text-muted-foreground">{t('keyUsageAccounts.empty')}</p>}
    <div className="grid gap-3 md:grid-cols-2">{visible?.map(account => <div key={account.id} className="space-y-2 rounded-lg border p-3 text-sm">
      <div className="font-medium">{account.name || `#${account.id}`} {account.email && <span className="text-muted-foreground">{account.email}</span>}</div>
      <div className="text-muted-foreground">{account.group_ids.length ? account.group_ids.map(id => groups.find(g => g.id === id)?.name || `#${id}`).join(' · ') : t('keyUsageAccounts.ungrouped')}</div>
      <div>{account.plan_type || '—'} · {account.status}</div>
      {([['5h', account.usage_percent_5h, account.reset_5h_at, account.usage_updated_at_5h], [account.long_window_seconds ? `${Math.round(account.long_window_seconds / 86400)}d` : '7d', account.usage_percent_7d, account.reset_7d_at, account.usage_updated_at_7d]] as const).map(([label, used, reset, updated]) => <div key={label}>{label}: {used == null ? t('keyUsageAccounts.unknown') : `${used}% ${t('keyUsageAccounts.used')}`} · {t('keyUsageAccounts.resetTime')}: {fmt(reset)} · {t('keyUsageAccounts.updated')}: {fmt(updated)}</div>)}
      <div>{t('keyUsageAccounts.cards')}: {account.reset_credits ?? t('keyUsageAccounts.unknown')}</div>
      {account.reset_uncertain && <p className="text-destructive">{t('keyUsageAccounts.uncertain')}</p>}
      {account.reset_allowed && <Button variant="outline" size="sm" onClick={() => void redeem(account.id)} disabled={busy !== null || account.reset_credits === 0}>{busy === account.id ? t('keyUsageAccounts.pending') : t('keyUsageAccounts.redeem')}</Button>}
    </div>)}</div>
  </section>
}
