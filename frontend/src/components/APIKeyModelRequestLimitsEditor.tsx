import { useTranslation } from 'react-i18next'
import { Plus, Trash2 } from 'lucide-react'
import { MAX_MODEL_REQUEST_RULES, newModelRequestLimitRow, type ModelRequestLimitFormState } from '../lib/apiKeyModelRequests'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'

export default function APIKeyModelRequestLimitsEditor({
  value,
  onChange,
}: {
  value: ModelRequestLimitFormState[]
  onChange: (next: ModelRequestLimitFormState[]) => void
}) {
  const { t } = useTranslation()
  const patch = (index: number, next: Partial<ModelRequestLimitFormState>) => {
    onChange(value.map((row, rowIndex) => rowIndex === index ? { ...row, ...next } : row))
  }
  const weekdays = Array.from({ length: 7 }, (_, index) => ({
    value: String(index + 1), label: t(`modelRequests.weekdays.${index + 1}`),
  }))

  return (
    <div className="space-y-3">
      <p className="text-xs leading-relaxed text-muted-foreground">{t('modelRequests.matchHint')}</p>
      <p className="text-xs leading-relaxed text-muted-foreground">{t('modelRequests.countHint')}</p>
      {value.length === 0 ? <p className="rounded-md bg-muted/30 p-3 text-xs text-muted-foreground">{t('modelRequests.empty')}</p> : null}
      {value.map((row, index) => (
        <div key={row.id ?? index} className="space-y-3 rounded-lg border border-border p-3">
          <div className="flex items-center justify-between gap-3">
            <span className="text-xs font-semibold">{t('modelRequests.rule', { number: index + 1 })}</span>
            <Button type="button" variant="ghost" size="icon" aria-label={t('modelRequests.remove')} onClick={() => onChange(value.filter((_, rowIndex) => rowIndex !== index))}>
              <Trash2 className="size-3.5" />
            </Button>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="space-y-1.5 text-xs font-medium">
              <span>{t('modelRequests.model')}</span>
              <Input value={row.model} maxLength={200} placeholder="gpt-6*" disabled={Boolean(row.id)} onChange={(event) => patch(index, { model: event.target.value })} />
            </label>
            <label className="space-y-1.5 text-xs font-medium">
              <span>{t('modelRequests.limit')}</span>
              <Input type="number" min="1" step="1" inputMode="numeric" value={row.maxRequests} placeholder="50" onChange={(event) => patch(index, { maxRequests: event.target.value })} />
            </label>
          </div>
          <div className="grid gap-3 sm:grid-cols-3">
            <label className="space-y-1.5 text-xs font-medium sm:col-span-3">
              <span>{t('modelRequests.timezone')}</span>
              <Input value={row.timezone} placeholder="Asia/Shanghai" disabled={Boolean(row.id)} onChange={(event) => patch(index, { timezone: event.target.value })} />
            </label>
            <label className="space-y-1.5 text-xs font-medium sm:col-span-2">
              <span>{t('modelRequests.weekday')}</span>
              <Select value={String(row.resetWeekday)} options={weekdays} disabled={Boolean(row.id)} onValueChange={(next) => patch(index, { resetWeekday: Number(next) })} />
            </label>
            <label className="space-y-1.5 text-xs font-medium">
              <span>{t('modelRequests.time')}</span>
              <Input type="time" step="60" value={row.resetTime} disabled={Boolean(row.id)} onChange={(event) => patch(index, { resetTime: event.target.value })} />
            </label>
          </div>
          {row.id ? <p className="text-xs leading-relaxed text-muted-foreground">{t('modelRequests.savedRuleHint')}</p> : null}
        </div>
      ))}
      <Button type="button" variant="outline" size="sm" disabled={value.length >= MAX_MODEL_REQUEST_RULES} onClick={() => onChange([...value, newModelRequestLimitRow()])}>
        <Plus className="size-3.5" />{t('modelRequests.add')}
      </Button>
      {value.length >= MAX_MODEL_REQUEST_RULES ? <p className="text-xs text-muted-foreground">{t('modelRequests.tooManyRules')}</p> : null}
    </div>
  )
}
