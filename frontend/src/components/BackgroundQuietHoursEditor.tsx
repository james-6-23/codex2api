import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

type Period = { start: string; end: string }

export default function BackgroundQuietHoursEditor({ value, onSave }: {
  value: Period[]
  onSave: (periods: Period[]) => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<Period[]>(value)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const serialized = JSON.stringify(value)
  useEffect(() => { setDraft(JSON.parse(serialized)); setError('') }, [serialized])
  const dirty = JSON.stringify(draft) !== serialized
  const change = (index: number, field: keyof Period, text: string) => {
    setDraft(rows => rows.map((row, i) => i === index ? { ...row, [field]: text } : row))
    setError('')
  }
  const save = async () => {
    if (draft.some(row => !/^(?:[01]\d|2[0-3]):[0-5]\d$/.test(row.start) ||
      !/^(?:(?:[01]\d|2[0-3]):[0-5]\d|24:00)$/.test(row.end) || row.start === row.end)) {
      setError(t('settings.quietHoursInvalid')); return
    }
    setSaving(true)
    try { await onSave(draft) } catch { setError(t('settings.quietHoursSaveFailed')) }
    finally { setSaving(false) }
  }
  return <div className="space-y-3 rounded-lg border p-3">
    <div className="text-sm font-medium">{t('settings.quietHours')}</div>
    <p className="text-xs text-muted-foreground">{t('settings.quietHoursDesc')}</p>
    {draft.length === 0 && <p className="text-xs text-muted-foreground">{t('settings.quietHoursEmpty')}</p>}
    {draft.map((row, index) => <div key={index} className="flex flex-wrap items-center gap-2">
      <label className="min-w-0 flex-1 text-xs">{t('settings.quietHoursStart')}
        <Input aria-label={`${t('settings.quietHoursStart')} ${index + 1}`} type="time" step="60" value={row.start} disabled={saving} onChange={event => change(index, 'start', event.target.value)} />
      </label>
      <label className="min-w-0 flex-1 text-xs">{t('settings.quietHoursEnd')}
        <Input aria-label={`${t('settings.quietHoursEnd')} ${index + 1}`} placeholder="07:00" value={row.end} maxLength={5} disabled={saving} onChange={event => change(index, 'end', event.target.value)} />
      </label>
      <Button type="button" variant="ghost" disabled={saving} onClick={() => setDraft(rows => rows.filter((_, i) => i !== index))}>{t('settings.quietHoursRemove')}</Button>
    </div>)}
    {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    <div className="flex flex-wrap gap-2">
      <Button type="button" variant="outline" disabled={saving || draft.length >= 32} onClick={() => setDraft(rows => [...rows, { start: '00:00', end: '07:00' }])}>{t('settings.quietHoursAdd')}</Button>
      <Button type="button" disabled={saving || !dirty} onClick={() => void save()}>{t('settings.quietHoursSave')}</Button>
    </div>
  </div>
}
