import { useTranslation } from 'react-i18next'
import { Plus, RotateCcw, Trash2 } from 'lucide-react'
import { Button } from './ui/button'
import { DraftNumberInput } from './ui/draft-number-input'
import { Select } from './ui/select'
import { defaultSessionCreationCooldown } from '../lib/sessionCreationCooldown'
import type { SessionCreationCooldownConfig } from '../lib/sessionCreationCooldown'

export default function SessionCreationCooldownControls({ value, onChange }: {
  value: SessionCreationCooldownConfig
  onChange: (value: SessionCreationCooldownConfig) => void
}) {
  const { t } = useTranslation()
  const label = (key: string) => t(`promptFilter.creationCooldown.${key}`)
  const update = (patch: Partial<SessionCreationCooldownConfig>) => onChange({ ...value, ...patch })
  const fields = [
    ['frequency_window_seconds', 'frequencyWindow', 1, 1440, 60],
    ['free_creations', 'freeCreations', 1, 1000, 1],
    ['history_days', 'historyDays', 1, 90, 1],
    ['min_samples', 'minSamples', 1, 200, 1],
    ['max_samples', 'maxSamples', 1, 200, 1],
    ['max_interval_seconds', 'maxInterval', 0, 1440, 60],
  ] as const
  return <section className="mt-4 space-y-3 rounded-lg border border-primary/20 bg-primary/[0.03] p-3">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="text-sm font-semibold">{label('title')}</div>
      <Select value={value.mode} onValueChange={(mode) => update({ mode: mode as SessionCreationCooldownConfig['mode'] })}
        options={['off', 'observe', 'enforce'].map((mode) => ({ value: mode, label: label(mode) }))} />
    </div>
    <p className="text-xs leading-relaxed text-muted-foreground">{label('description')}</p>
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {fields.map(([field, text, min, max, divisor]) => <label key={field} className="space-y-1 text-xs text-muted-foreground">
        <span>{label(text)}</span>
        <DraftNumberInput aria-label={label(text)} min={min} max={max} value={value[field] / divisor}
          onValueChange={(next) => update({ [field]: next * divisor })} />
      </label>)}
    </div>
    <p className="text-xs text-muted-foreground">{label('tierHint')}</p>
    <div className="space-y-2">
      {value.tiers.map((tier, index) => <div key={index} className="grid grid-cols-[1fr_1fr_auto] items-end gap-2">
        <label className="space-y-1 text-xs text-muted-foreground"><span>{label('lowerBound')}</span>
          <DraftNumberInput aria-label={`${label('lowerBound')} ${index + 1}`} min={0} max={43200} value={tier.min_average_seconds / 60}
            onValueChange={(next) => update({ tiers: value.tiers.map((item, position) => position === index ? { ...item, min_average_seconds: next * 60 } : item) })} />
        </label>
        <label className="space-y-1 text-xs text-muted-foreground"><span>{label('interval')}</span>
          <DraftNumberInput aria-label={`${label('interval')} ${index + 1}`} min={0} max={1440} value={tier.interval_seconds / 60}
            onValueChange={(next) => update({ tiers: value.tiers.map((item, position) => position === index ? { ...item, interval_seconds: next * 60 } : item) })} />
        </label>
        <Button type="button" variant="ghost" size="icon" aria-label={label('removeTier')} disabled={value.tiers.length <= 1}
          onClick={() => update({ tiers: value.tiers.filter((_, position) => position !== index) })}><Trash2 className="size-4" /></Button>
      </div>)}
    </div>
    <div className="flex flex-wrap gap-2">
      <Button type="button" variant="outline" size="sm" disabled={value.tiers.length >= 12 || value.tiers.some((tier) => tier.min_average_seconds >= 2592000)} onClick={() => {
        const nextBound = value.tiers.length ? Math.max(...value.tiers.map((tier) => tier.min_average_seconds)) + 60 : 0
        update({ tiers: [...value.tiers, { min_average_seconds: nextBound, interval_seconds: 0 }] })
      }}><Plus className="size-3.5" />{label('addTier')}</Button>
      <Button type="button" variant="outline" size="sm" onClick={() => onChange({ ...defaultSessionCreationCooldown(), mode: value.mode })}>
        <RotateCcw className="size-3.5" />{label('reset')}</Button>
    </div>
    <p className="text-xs leading-relaxed text-muted-foreground">{label('protectionHint')}</p>
  </section>
}
