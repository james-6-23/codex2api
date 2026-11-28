import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { motion } from 'framer-motion'
import { useTranslation } from 'react-i18next'
import { ArrowRight, CalendarDays, Clock, RotateCcw } from 'lucide-react'
import { Calendar, type DateRange } from '@/components/ui/calendar'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

export interface CustomDateRangeValue {
  start: string // RFC3339 with local timezone offset
  end: string
}

interface UsageDateRangePopoverProps {
  anchorRef: React.RefObject<HTMLButtonElement | null>
  initial: CustomDateRangeValue | null
  maxDays: number
  onApply: (range: CustomDateRangeValue) => void
  onCancel: () => void
}

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate())
}

function dateToLocalRFC3339(date: Date): string {
  const offset = date.getTimezoneOffset()
  const sign = offset <= 0 ? '+' : '-'
  const absOffset = Math.abs(offset)
  return `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}T${pad2(date.getHours())}:${pad2(date.getMinutes())}:${pad2(date.getSeconds())}${sign}${pad2(Math.floor(absOffset / 60))}:${pad2(absOffset % 60)}`
}

function formatShortDate(d?: Date): string {
  if (!d) return '--/--'
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
}

function clampTimePart(raw: string, max: number): number {
  const parsed = Number.parseInt(raw.replace(/\D/g, ''), 10)
  if (!Number.isFinite(parsed)) return 0
  return Math.max(0, Math.min(max, parsed))
}

function combineDateTime(day: Date, hour: number, minute: number, second = 0): Date {
  return new Date(day.getFullYear(), day.getMonth(), day.getDate(), hour, minute, second, 0)
}

interface TimeValue {
  hour: number
  minute: number
}

function TimeStepperField({
  label,
  value,
  onChange,
}: {
  label: string
  value: TimeValue
  onChange: (next: TimeValue) => void
}) {
  return (
    <div className="flex flex-1 items-center justify-between gap-2 rounded-xl border border-border/70 bg-muted/25 px-2.5 py-1.5">
      <span className="inline-flex items-center gap-1 text-[11px] font-medium text-muted-foreground">
        <Clock className="size-3 opacity-75" />
        {label}
      </span>
      <div className="inline-flex items-center gap-0.5 rounded-lg bg-background/90 px-1.5 py-0.5 ring-1 ring-border/65">
        <input
          type="text"
          inputMode="numeric"
          aria-label={`${label} hour`}
          value={pad2(value.hour)}
          onChange={(e) => onChange({ ...value, hour: clampTimePart(e.target.value.slice(-2), 23) })}
          onKeyDown={(e) => {
            if (e.key === 'ArrowUp') {
              e.preventDefault()
              onChange({ ...value, hour: (value.hour + 1) % 24 })
            } else if (e.key === 'ArrowDown') {
              e.preventDefault()
              onChange({ ...value, hour: (value.hour + 23) % 24 })
            }
          }}
          className="w-5 bg-transparent text-center font-sans text-[12px] font-medium text-foreground tabular-nums outline-none focus:text-primary"
        />
        <span className="text-[11px] text-muted-foreground/70">:</span>
        <input
          type="text"
          inputMode="numeric"
          aria-label={`${label} minute`}
          value={pad2(value.minute)}
          onChange={(e) => onChange({ ...value, minute: clampTimePart(e.target.value.slice(-2), 59) })}
          onKeyDown={(e) => {
            if (e.key === 'ArrowUp') {
              e.preventDefault()
              onChange({ ...value, minute: (value.minute + 5) % 60 })
            } else if (e.key === 'ArrowDown') {
              e.preventDefault()
              onChange({ ...value, minute: (value.minute + 55) % 60 })
            }
          }}
          className="w-5 bg-transparent text-center font-sans text-[12px] font-medium text-foreground tabular-nums outline-none focus:text-primary"
        />
      </div>
    </div>
  )
}

export function formatCustomRangeChipLabel(range: CustomDateRangeValue | null): string | null {
  if (!range) return null
  const start = new Date(range.start)
  const end = new Date(range.end)
  if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) return null
  const fmt = (d: Date) => `${pad2(d.getMonth() + 1)}/${pad2(d.getDate())}`
  const s = fmt(start)
  const e = fmt(end)
  return s === e ? `${s} ${pad2(start.getHours())}:${pad2(start.getMinutes())}` : `${s} – ${e}`
}

export default function UsageDateRangePopover({
  anchorRef,
  initial,
  maxDays,
  onApply,
  onCancel,
}: UsageDateRangePopoverProps) {
  const { t, i18n } = useTranslation()
  const now = useMemo(() => new Date(), [])
  const today = useMemo(() => startOfDay(now), [now])

  const initialStart = useMemo(() => {
    const parsed = initial ? new Date(initial.start) : null
    if (parsed && !Number.isNaN(parsed.getTime())) return parsed
    const fallback = new Date(now)
    fallback.setDate(fallback.getDate() - 6)
    fallback.setHours(0, 0, 0, 0)
    return fallback
  }, [initial, now])

  const initialEnd = useMemo(() => {
    const parsed = initial ? new Date(initial.end) : null
    if (parsed && !Number.isNaN(parsed.getTime())) return parsed
    return now
  }, [initial, now])

  const [selectedRange, setSelectedRange] = useState<DateRange | undefined>(() => ({
    from: startOfDay(initialStart),
    to: startOfDay(initialEnd),
  }))
  const [displayedMonth, setDisplayedMonth] = useState<Date>(() => startOfDay(initialEnd))
  const [startTime, setStartTime] = useState<TimeValue>(() => ({
    hour: initialStart.getHours(),
    minute: initialStart.getMinutes(),
  }))
  const [endTime, setEndTime] = useState<TimeValue>(() => ({
    hour: initialEnd.getHours(),
    minute: initialEnd.getMinutes(),
  }))
  const [activePreset, setActivePreset] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const popoverRef = useRef<HTMLDivElement>(null)
  const [position, setPosition] = useState<{ top: number; left: number; width: number } | null>(null)

  const recompute = useCallback(() => {
    const anchor = anchorRef.current
    if (!anchor) return
    const rect = anchor.getBoundingClientRect()
    const viewportW = window.innerWidth
    const viewportH = window.innerHeight
    const width = Math.min(492, Math.max(300, viewportW - 16))
    const estimatedHeight = viewportW < 640 ? 580 : 458
    const spaceBelow = viewportH - rect.bottom - 12
    const top = spaceBelow >= estimatedHeight || spaceBelow >= rect.top
      ? Math.min(viewportH - estimatedHeight - 8, rect.bottom + 8)
      : Math.max(8, rect.top - estimatedHeight - 8)
    const desiredLeft = rect.left
    const left = Math.max(8, Math.min(viewportW - width - 8, desiredLeft))
    setPosition({ top: Math.max(8, top), left, width })
  }, [anchorRef])

  useLayoutEffect(() => {
    recompute()
  }, [recompute])

  useEffect(() => {
    const handle = () => recompute()
    window.addEventListener('resize', handle)
    window.addEventListener('scroll', handle, true)
    return () => {
      window.removeEventListener('resize', handle)
      window.removeEventListener('scroll', handle, true)
    }
  }, [recompute])

  useEffect(() => {
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target as Node | null
      if (!target) return
      if (popoverRef.current?.contains(target)) return
      if (anchorRef.current?.contains(target)) return
      onCancel()
    }
    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onCancel()
    }
    document.addEventListener('pointerdown', handlePointerDown)
    document.addEventListener('keydown', handleEscape)
    return () => {
      document.removeEventListener('pointerdown', handlePointerDown)
      document.removeEventListener('keydown', handleEscape)
    }
  }, [anchorRef, onCancel])

  const presets = useMemo(() => {
    const mkDaysAgo = (days: number) => {
      const d = new Date(today)
      d.setDate(d.getDate() - days)
      return d
    }
    const yesterday = mkDaysAgo(1)
    const monthStart = new Date(today.getFullYear(), today.getMonth(), 1)
    return [
      {
        id: 'today',
        label: t('usage.customPresetToday'),
        from: today,
        to: today,
        start: { hour: 0, minute: 0 },
        end: { hour: now.getHours(), minute: now.getMinutes() },
      },
      {
        id: 'yesterday',
        label: t('usage.customPresetYesterday'),
        from: yesterday,
        to: yesterday,
        start: { hour: 0, minute: 0 },
        end: { hour: 23, minute: 59 },
      },
      {
        id: '3d',
        label: t('usage.customPresetLastDays', { count: 3 }),
        from: mkDaysAgo(2),
        to: today,
        start: { hour: 0, minute: 0 },
        end: { hour: now.getHours(), minute: now.getMinutes() },
      },
      {
        id: '7d',
        label: t('usage.customPresetLastDays', { count: 7 }),
        from: mkDaysAgo(6),
        to: today,
        start: { hour: 0, minute: 0 },
        end: { hour: now.getHours(), minute: now.getMinutes() },
      },
      {
        id: '14d',
        label: t('usage.customPresetLastDays', { count: 14 }),
        from: mkDaysAgo(13),
        to: today,
        start: { hour: 0, minute: 0 },
        end: { hour: now.getHours(), minute: now.getMinutes() },
      },
      {
        id: '30d',
        label: t('usage.customPresetLastDays', { count: 30 }),
        from: mkDaysAgo(29),
        to: today,
        start: { hour: 0, minute: 0 },
        end: { hour: now.getHours(), minute: now.getMinutes() },
      },
      {
        id: 'thisMonth',
        label: t('usage.customPresetThisMonth'),
        from: monthStart,
        to: today,
        start: { hour: 0, minute: 0 },
        end: { hour: now.getHours(), minute: now.getMinutes() },
      },
      {
        id: '90d',
        label: t('usage.customPresetLastDays', { count: Math.min(90, maxDays) }),
        from: mkDaysAgo(Math.min(90, maxDays) - 1),
        to: today,
        start: { hour: 0, minute: 0 },
        end: { hour: now.getHours(), minute: now.getMinutes() },
      },
    ]
  }, [today, now, maxDays, t])

  const effectiveFrom = selectedRange?.from
  const effectiveTo = selectedRange?.to ?? selectedRange?.from

  const spanDays = useMemo(() => {
    if (!effectiveFrom || !effectiveTo) return 0
    return Math.round((startOfDay(effectiveTo).getTime() - startOfDay(effectiveFrom).getTime()) / 86_400_000) + 1
  }, [effectiveFrom, effectiveTo])

  const applyFullDayTime = () => {
    setStartTime({ hour: 0, minute: 0 })
    setEndTime({ hour: 23, minute: 59 })
    setError(null)
  }

  const applyUntilNow = () => {
    const current = new Date()
    setStartTime({ hour: 0, minute: 0 })
    setEndTime({ hour: current.getHours(), minute: current.getMinutes() })
    setError(null)
  }

  const handleApply = () => {
    if (!effectiveFrom || !effectiveTo) {
      setError(t('usage.customRangeInvalid'))
      return
    }
    const startDate = combineDateTime(effectiveFrom, startTime.hour, startTime.minute, 0)
    const endDate = combineDateTime(effectiveTo, endTime.hour, endTime.minute, 59)
    if (endDate.getTime() <= startDate.getTime()) {
      setError(t('usage.customRangeEndBeforeStart'))
      return
    }
    const maxMs = maxDays * 24 * 60 * 60 * 1000
    if (endDate.getTime() - startDate.getTime() > maxMs) {
      setError(t('usage.customRangeTooLong', { days: maxDays }))
      return
    }
    setError(null)
    onApply({
      start: dateToLocalRFC3339(startDate),
      end: dateToLocalRFC3339(endDate),
    })
  }

  if (!position) return null

  return createPortal(
    <motion.div
      ref={popoverRef}
      initial={{ opacity: 0, y: -4, scale: 0.985 }}
      animate={{ opacity: 1, y: 0, scale: 1 }}
      transition={{ duration: 0.16, ease: [0.23, 1, 0.32, 1] }}
      style={{
        position: 'fixed',
        top: position.top,
        left: position.left,
        width: position.width,
      }}
      className="z-[1000] overflow-hidden rounded-2xl border border-border/80 bg-popover text-popover-foreground shadow-[0_22px_54px_rgba(15,23,42,0.18)] backdrop-blur-md dark:shadow-[0_24px_60px_rgba(0,0,0,0.48)]"
    >
      {/* Top Selected Range Summary Header */}
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/65 bg-muted/25 px-4 py-2.5">
        <div className="flex items-center gap-2 text-xs">
          <CalendarDays className="size-3.5 text-primary" />
          <span className="font-semibold text-foreground">{t('usage.customRangeTitle')}</span>
        </div>
        <div className="flex items-center gap-1.5 font-sans text-[11.5px] tabular-nums">
          <span className="rounded-md bg-background/90 px-2 py-0.5 font-medium text-foreground ring-1 ring-border/60">
            {formatShortDate(effectiveFrom)} {pad2(startTime.hour)}:{pad2(startTime.minute)}
          </span>
          <ArrowRight className="size-3 text-muted-foreground/70" />
          <span className="rounded-md bg-background/90 px-2 py-0.5 font-medium text-foreground ring-1 ring-border/60">
            {formatShortDate(effectiveTo)} {pad2(endTime.hour)}:{pad2(endTime.minute)}
          </span>
          {spanDays > 0 ? (
            <span className="rounded-md bg-primary/10 px-1.5 py-0.5 text-[10.5px] font-medium text-primary">
              {t('usage.customRangeDaysBadge', { count: spanDays })}
            </span>
          ) : null}
        </div>
      </div>

      {/* Main Split Body: Presets (left) + Calendar & Time Steppers (right) */}
      <div className="grid grid-cols-1 sm:grid-cols-[148px_minmax(0,1fr)]">
        {/* Left Preset Rail */}
        <div className="flex gap-1 overflow-x-auto border-b border-border/60 bg-muted/15 p-2.5 sm:flex-col sm:border-r sm:border-b-0">
          <span className="mb-1 hidden px-2 text-[10.5px] font-medium text-muted-foreground sm:block">
            {t('usage.customPresetsHeading')}
          </span>
          {presets.map((preset) => {
            const active = activePreset === preset.id
            return (
              <button
                key={preset.id}
                type="button"
                onClick={() => {
                  setActivePreset(preset.id)
                  setSelectedRange({ from: preset.from, to: preset.to })
                  setDisplayedMonth(preset.to)
                  setStartTime(preset.start)
                  setEndTime(preset.end)
                  setError(null)
                }}
                className={cn(
                  'shrink-0 rounded-lg px-2.5 py-1.5 text-left text-xs font-medium whitespace-nowrap transition-colors',
                  active
                    ? 'bg-primary/10 text-primary ring-1 ring-inset ring-primary/25'
                    : 'text-muted-foreground hover:bg-muted/70 hover:text-foreground',
                )}
              >
                {preset.label}
              </button>
            )
          })}
        </div>

        {/* Right Calendar + Time Inputs */}
        <div className="flex flex-col p-3.5">
          <Calendar
            mode="range"
            locale={i18n.language}
            weekStartsOn={1}
            month={displayedMonth}
            onMonthChange={setDisplayedMonth}
            selected={selectedRange}
            disabled={(date) => startOfDay(date).getTime() > today.getTime()}
            onSelect={(next) => {
              setSelectedRange(next)
              setActivePreset(null)
              setError(null)
            }}
            className="mx-auto max-w-none border-0 bg-transparent p-0"
          />

          {/* Custom Non-Native Time Steppers */}
          <div className="mt-3 space-y-2 border-t border-border/60 pt-3">
            <div className="flex items-center justify-between gap-2">
              <span className="text-[11px] font-medium text-muted-foreground">
                {t('usage.customTimePrecision')}
              </span>
              <div className="flex shrink-0 items-center gap-1">
                <button
                  type="button"
                  onClick={applyFullDayTime}
                  className="whitespace-nowrap rounded-md bg-muted/65 px-2 py-0.5 text-[10.5px] font-medium text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                  {t('usage.customTimeFullDay')}
                </button>
                <button
                  type="button"
                  onClick={applyUntilNow}
                  className="whitespace-nowrap rounded-md bg-muted/65 px-2 py-0.5 text-[10.5px] font-medium text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                  {t('usage.customTimeUntilNow')}
                </button>
              </div>
            </div>
            <div className="flex flex-col gap-2 sm:flex-row">
              <TimeStepperField
                label={t('usage.customRangeStartShort')}
                value={startTime}
                onChange={(v) => { setStartTime(v); setActivePreset(null); setError(null) }}
              />
              <TimeStepperField
                label={t('usage.customRangeEndShort')}
                value={endTime}
                onChange={(v) => { setEndTime(v); setActivePreset(null); setError(null) }}
              />
            </div>
          </div>

          {error ? (
            <div role="alert" className="mt-2.5 rounded-lg border border-destructive/25 bg-destructive/8 px-2.5 py-1.5 text-[11.5px] text-destructive">
              {error}
            </div>
          ) : null}
        </div>
      </div>

      {/* Footer Actions */}
      <div className="flex items-center justify-between gap-2 border-t border-border/65 bg-muted/20 px-4 py-2.5">
        <button
          type="button"
          onClick={() => {
            const fallbackStart = new Date(today)
            fallbackStart.setDate(fallbackStart.getDate() - 6)
            setSelectedRange({ from: fallbackStart, to: today })
            setDisplayedMonth(today)
            setStartTime({ hour: 0, minute: 0 })
            setEndTime({ hour: now.getHours(), minute: now.getMinutes() })
            setActivePreset('7d')
            setError(null)
          }}
          className="inline-flex items-center gap-1 rounded-lg px-2 py-1 text-xs font-medium text-muted-foreground transition-colors hover:bg-muted/70 hover:text-foreground"
        >
          <RotateCcw className="size-3" />
          {t('usage.customRangeReset')}
        </button>

        <div className="flex items-center gap-2">
          <Button variant="ghost" size="sm" className="h-8 rounded-lg" onClick={onCancel}>
            {t('common.cancel', { defaultValue: 'Cancel' })}
          </Button>
          <Button size="sm" className="h-8 rounded-lg px-3.5" onClick={handleApply}>
            {t('usage.customRangeApply')}
          </Button>
        </div>
      </div>
    </motion.div>,
    document.body,
  )
}
