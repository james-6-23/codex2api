import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { Select } from '@/components/ui/select'
import { Input } from '@/components/ui/input'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { getErrorMessage } from '../utils/error'

type Policy = { view_enabled: boolean; reset_enabled: boolean }

export default function KeyUsageAccountPermissions({ keys }: { keys: { id: number; name: string }[] }) {
  const { t } = useTranslation()
  const [id, setId] = useState(0)
  const [policy, setPolicy] = useState<Policy | null>(null)
  const [message, setMessage] = useState('')
  const [saving, setSaving] = useState(false)
  const [accountID, setAccountID] = useState('')
  const { confirm, confirmDialog } = useConfirmDialog()

  useEffect(() => {
    let cancelled = false
    setPolicy(null)
    setMessage('')
    void api.getKeyUsageAccountPermissions(id).then(p => { if (!cancelled) setPolicy(p) }).catch(e => { if (!cancelled) setMessage(getErrorMessage(e)) })
    return () => { cancelled = true }
  }, [id])

  const save = async () => {
    if (!policy) return
    setSaving(true)
    setMessage('')
    try { await api.setKeyUsageAccountPermissions(id, policy); setMessage(t('keyUsageAccounts.saved')) }
    catch (e) { setMessage(getErrorMessage(e)) }
    finally { setSaving(false) }
  }

  const reconcile = async () => {
    const account = Number(accountID)
    if (!Number.isSafeInteger(account) || account <= 0 || !await confirm({ title: t('keyUsageAccounts.reconcile'), description: t('keyUsageAccounts.reconcileConfirm'), tone: 'destructive' })) return
    setSaving(true); setMessage('')
    try { await api.clearKeyUsageResetUncertain(account); setMessage(t('keyUsageAccounts.saved')) }
    catch (e) { setMessage(getErrorMessage(e)) }
    finally { setSaving(false) }
  }

  return <div className="space-y-3 rounded-xl border border-border/80 bg-muted/20 p-3">
    {confirmDialog}
    <div className="text-sm font-semibold">{t('keyUsageAccounts.title')}</div>
    <p className="text-xs text-muted-foreground">{t('keyUsageAccounts.intro')}</p>
    <div className="space-y-1 text-sm">{t('keyUsageAccounts.specific')}
      <Select aria-label={t('keyUsageAccounts.specific')} value={String(id)} onValueChange={value => setId(Number(value))} disabled={saving}
        options={[{ value: '0', label: t('keyUsageAccounts.global') }, ...keys.map(key => ({ value: String(key.id), label: `${key.name} (#${key.id})` }))]} />
    </div>
    {id !== 0 && <p className="text-xs text-muted-foreground">{t('keyUsageAccounts.globalOff')}</p>}
    {policy && <>
      <label className="flex items-center justify-between gap-4 text-sm">{t('keyUsageAccounts.view')}<Switch checked={policy.view_enabled} onCheckedChange={v => setPolicy({ ...policy, view_enabled: v })} disabled={saving} /></label>
      <label className="flex items-center justify-between gap-4 text-sm">{t('keyUsageAccounts.reset')}<Switch checked={policy.reset_enabled} onCheckedChange={v => setPolicy({ ...policy, reset_enabled: v })} disabled={saving} /></label>
      <Button size="sm" onClick={() => void save()} disabled={saving}>{t('keyUsageAccounts.save')}</Button>
    </>}
    {message && <p className="text-xs" role="status">{message}</p>}
    <div className="space-y-2 border-t pt-3">
      <p className="text-xs text-muted-foreground">{t('keyUsageAccounts.reconcileHint')}</p>
      <div className="flex gap-2"><Input type="number" min="1" aria-label={t('keyUsageAccounts.accountID')} placeholder={t('keyUsageAccounts.accountID')} value={accountID} onChange={e => setAccountID(e.target.value)} disabled={saving} /><Button variant="outline" size="sm" disabled={saving || !accountID} onClick={() => void reconcile()}>{t('keyUsageAccounts.reconcile')}</Button></div>
    </div>
  </div>
}
