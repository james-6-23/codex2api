import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Clock3, Plus, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'

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
  return (
    <div className="space-y-3 rounded-xl border border-border/60 bg-muted/20 p-3.5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-1.5 text-[13px] font-medium text-foreground">
          <Clock3 className="size-3.5 text-muted-foreground" aria-hidden="true" />
          <span>{t('settings.quietHours')}</span>
        </div>
        <span
          className={cn(
            'inline-flex items-center rounded-full px-2 py-0.5 font-sans text-[11px] font-medium tabular-nums',
            draft.length > 0
              ? 'bg-amber-500/10 text-amber-700 ring-1 ring-inset ring-amber-500/20 dark:bg-amber-500/15 dark:text-amber-300'
              : 'bg-emerald-500/10 text-emerald-700 ring-1 ring-inset ring-emerald-500/20 dark:bg-emerald-500/15 dark:text-emerald-300',
          )}
        >
          {draft.length > 0
            ? t('settings.quietHoursActiveCount', { count: draft.length })
            : t('settings.quietHoursAllDay')}
        </span>
      </div>

      <p className="text-xs leading-relaxed text-muted-foreground">{t('settings.quietHoursDesc')}</p>

      {draft.length === 0 ? (
        <p className="rounded-lg border border-dashed border-border/60 bg-background/50 px-3 py-2 text-xs text-muted-foreground">
          {t('settings.quietHoursEmpty')}
        </p>
      ) : (
        <div className="space-y-2">
          {draft.map((row, index) => (
            <div key={index} className="flex flex-wrap items-end gap-2 rounded-lg border border-border/55 bg-background/80 p-2.5">
              <label className="min-w-[110px] flex-1 space-y-1 text-[11.5px] font-medium text-muted-foreground">
                <span>{t('settings.quietHoursStart')}</span>
                <Input
                  aria-label={`${t('settings.quietHoursStart')} ${index + 1}`}
                  type="time"
                  step="60"
                  value={row.start}
                  disabled={saving}
                  onChange={event => change(index, 'start', event.target.value)}
                  className="h-8 rounded-lg font-sans text-xs tabular-nums"
                />
              </label>
              <label className="min-w-[110px] flex-1 space-y-1 text-[11.5px] font-medium text-muted-foreground">
                <span>{t('settings.quietHoursEnd')}</span>
                <Input
                  aria-label={`${t('settings.quietHoursEnd')} ${index + 1}`}
                  placeholder="07:00"
                  value={row.end}
                  maxLength={5}
                  disabled={saving}
                  onChange={event => change(index, 'end', event.target.value)}
                  className="h-8 rounded-lg font-sans text-xs tabular-nums"
                />
              </label>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-8 rounded-lg text-muted-foreground hover:bg-destructive/10 hover:text-destructive"
                disabled={saving}
                onClick={() => setDraft(rows => rows.filter((_, i) => i !== index))}
              >
                <Trash2 className="size-3.5" aria-hidden="true" />
                {t('settings.quietHoursRemove')}
              </Button>
            </div>
          ))}
        </div>
      )}

      {error ? (
        <p role="alert" className="rounded-lg border border-destructive/25 bg-destructive/8 px-2.5 py-1.5 text-xs text-destructive">
          {error}
        </p>
      ) : null}

      <div className="flex flex-wrap items-center gap-2 pt-0.5">
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="h-8 rounded-lg bg-background/80 text-xs"
          disabled={saving || draft.length >= 32}
          onClick={() => setDraft(rows => [...rows, { start: '00:00', end: '07:00' }])}
        >
          <Plus className="size-3.5" aria-hidden="true" />
          {t('settings.quietHoursAdd')}
        </Button>
        <Button
          type="button"
          variant={dirty ? 'default' : 'outline'}
          size="sm"
          className="h-8 rounded-lg text-xs"
          disabled={saving || !dirty}
          onClick={() => void save()}
        >
          {t('settings.quietHoursSave')}
        </Button>
      </div>
    </div>
  )
}
