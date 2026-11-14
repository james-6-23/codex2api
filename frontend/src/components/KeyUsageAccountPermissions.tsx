import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Eye, RotateCcw, Loader2 } from 'lucide-react'
import { api } from '../api'
import { Switch } from '@/components/ui/switch'
import { getErrorMessage } from '../utils/error'

type Policy = { view_enabled: boolean; reset_enabled: boolean }

export default function KeyUsageAccountPermissions({ keyID = 0 }: { keyID?: number }) {
  const { t } = useTranslation()
  const [policy, setPolicy] = useState<Policy | null>(null)
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const global = keyID === 0

  useEffect(() => {
    let cancelled = false
    setPolicy(null)
    setError('')
    setSaved(false)
    void api.getKeyUsageAccountPermissions(keyID).then(value => { if (!cancelled) setPolicy(value) }).catch(e => { if (!cancelled) setError(getErrorMessage(e)) })
    return () => { cancelled = true }
  }, [keyID])

  const update = async (patch: Partial<Policy>) => {
    if (!policy || saving) return
    setSaving(true)
    setSaved(false)
    setError('')
    try {
      const value = await api.setKeyUsageAccountPermissions(keyID, { ...policy, ...patch })
      setPolicy(value)
      setSaved(true)
    } catch (e) { setError(getErrorMessage(e)) }
    finally { setSaving(false) }
  }

  return <section className="rounded-xl border border-border/80 bg-muted/20" aria-label={t(global ? 'keyUsageAccounts.globalTitle' : 'keyUsageAccounts.keyTitle')}>
    <header className="border-b border-border/70 p-3">
      <div className="flex items-center justify-between gap-2"><h3 className="text-sm font-semibold">{t(global ? 'keyUsageAccounts.globalTitle' : 'keyUsageAccounts.keyTitle')}</h3>{saving && <Loader2 className="size-3.5 animate-spin text-muted-foreground" aria-label={t('common.saving')} />}</div>
      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t(global ? 'keyUsageAccounts.globalDesc' : 'keyUsageAccounts.keyDesc')}</p>
    </header>
    <div className="divide-y divide-border/60 px-3">
      {([
        ['view_enabled', Eye, global ? 'keyUsageAccounts.view' : 'keyUsageAccounts.inheritView', 'keyUsageAccounts.viewHint'],
        ['reset_enabled', RotateCcw, global ? 'keyUsageAccounts.reset' : 'keyUsageAccounts.inheritReset', 'keyUsageAccounts.resetHint'],
      ] as const).map(([field, Icon, label, hint]) => <div key={field} className="flex items-center justify-between gap-4 py-3">
        <div className="min-w-0"><label htmlFor={`key-usage-${keyID}-${field}`} className="inline-flex items-center gap-2 text-sm font-medium"><Icon className="size-3.5 text-muted-foreground" />{t(label)}</label><p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t(hint)}</p></div>
        <Switch id={`key-usage-${keyID}-${field}`} checked={policy?.[field] ?? false} onCheckedChange={value => void update({ [field]: value })} disabled={!policy || saving} />
      </div>)}
    </div>
    <footer className="border-t border-border/70 px-3 py-2 text-xs text-muted-foreground" role={error ? 'alert' : 'status'}>
      {error ? <span className="text-destructive">{error}</span> : t(saved ? 'keyUsageAccounts.saved' : 'keyUsageAccounts.immediateSave')}
    </footer>
  </section>
}
