"use client"

import * as React from "react"
import { motion, AnimatePresence, useReducedMotion } from "framer-motion"
import { ChevronLeft, ChevronRight } from "lucide-react"
import { cn } from "@/lib/utils"

/* ─── Types ─────────────────────────────────────────────────── */

export type CalendarMode = "single" | "range" | "multiple"

export interface DateRange {
  from?: Date
  to?: Date
}

export interface CalendarEvent {
  date: Date
  label?: string
}

interface CalendarSharedProps {
  /** Month currently displayed (controlled). */
  month?: Date
  /** Initial displayed month (uncontrolled). Defaults to today's month. */
  defaultMonth?: Date
  onMonthChange?: (month: Date) => void
  /** 0 = Sunday, 1 = Monday. Default 0. */
  weekStartsOn?: 0 | 1
  /** BCP-47 locale string for localized month & weekday names (e.g. 'zh', 'en'). */
  locale?: string
  /** Return true to disable a date from selection. */
  disabled?: (date: Date) => boolean
  /** Dates that are unavailable/reserved, shown struck-through and disabled. */
  bookedDates?: Date[]
  /** Dates with a scheduled-event indicator dot. */
  events?: CalendarEvent[]
  /** Enables the fast month/year jump view when clicking the header label. Default true. */
  quickNav?: boolean
  className?: string
}

interface CalendarSingleProps extends CalendarSharedProps {
  mode?: "single"
  selected?: Date
  defaultSelected?: Date
  onSelect?: (date: Date | undefined) => void
}

interface CalendarMultipleProps extends CalendarSharedProps {
  mode: "multiple"
  selected?: Date[]
  defaultSelected?: Date[]
  onSelect?: (dates: Date[]) => void
}

interface CalendarRangeProps extends CalendarSharedProps {
  mode: "range"
  selected?: DateRange
  defaultSelected?: DateRange
  onSelect?: (range: DateRange | undefined) => void
}

export type CalendarProps = CalendarSingleProps | CalendarMultipleProps | CalendarRangeProps

/* ─── Easing ────────────────────────────────────────────────── */

const EASE_OUT: [number, number, number, number] = [0.23, 1, 0.32, 1]

/* ─── Date Helpers ──────────────────────────────────────────── */

const MONTH_NAMES_EN = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
]
const MONTH_SHORT_EN = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
]
const MONTH_NAMES_ZH = [
  "1月", "2月", "3月", "4月", "5月", "6月",
  "7月", "8月", "9月", "10月", "11月", "12月",
]
const WEEKDAY_SUN_EN = ["Su", "Mo", "Tu", "We", "Th", "Fr", "Sa"]
const WEEKDAY_MON_EN = ["Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"]
const WEEKDAY_SUN_ZH = ["日", "一", "二", "三", "四", "五", "六"]
const WEEKDAY_MON_ZH = ["一", "二", "三", "四", "五", "六", "日"]

function isZhLocale(locale?: string): boolean {
  return (locale ?? "").toLowerCase().startsWith("zh")
}

function isSameDay(a?: Date, b?: Date): boolean {
  if (!a || !b) return false
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate()
}

function isSameMonth(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth()
}

function startOfDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate())
}

function addMonths(date: Date, amount: number): Date {
  return new Date(date.getFullYear(), date.getMonth() + amount, 1)
}

function isBefore(a: Date, b: Date): boolean {
  return startOfDay(a).getTime() < startOfDay(b).getTime()
}

function isWithinRange(date: Date, from?: Date, to?: Date): boolean {
  if (!from || !to) return false
  const t = startOfDay(date).getTime()
  const a = startOfDay(from).getTime()
  const b = startOfDay(to).getTime()
  return t > Math.min(a, b) && t < Math.max(a, b)
}

function buildMonthGrid(month: Date, weekStartsOn: 0 | 1): Date[] {
  const firstOfMonth = new Date(month.getFullYear(), month.getMonth(), 1)
  const firstWeekday = firstOfMonth.getDay()
  const leading = weekStartsOn === 1 ? (firstWeekday + 6) % 7 : firstWeekday
  const gridStart = new Date(firstOfMonth)
  gridStart.setDate(gridStart.getDate() - leading)

  return Array.from({ length: 42 }, (_, i) => {
    const d = new Date(gridStart)
    d.setDate(gridStart.getDate() + i)
    return d
  })
}

/* ─── Quick-Nav Tooltip ─────────────────────────────────────── */

function QuickNavTooltip({
  label,
  disabled,
  children,
}: {
  label: string
  disabled?: boolean
  children: React.ReactElement
}) {
  if (disabled) return children

  return (
    <span className="group/tip relative inline-flex">
      {children}
      <span
        role="tooltip"
        aria-hidden="true"
        className={cn(
          "pointer-events-none absolute left-1/2 top-full z-20 mt-1.5 -translate-x-1/2 whitespace-nowrap",
          "rounded-md bg-foreground px-1.5 py-0.5 text-[10.5px] font-medium text-background shadow-sm",
          "origin-top scale-95 opacity-0 transition-[opacity,transform] delay-0 duration-150 ease-[cubic-bezier(0.23,1,0.32,1)]",
          "group-hover/tip:scale-100 group-hover/tip:opacity-100 group-hover/tip:delay-300",
          "group-active/tip:!opacity-0 group-active/tip:!delay-0 group-active/tip:!duration-0",
        )}
      >
        {label}
      </span>
    </span>
  )
}

/* ─── <Calendar> ────────────────────────────────────────────── */

export const Calendar = React.forwardRef<HTMLDivElement, CalendarProps>((props, ref) => {
  const {
    mode = "single",
    month: controlledMonth,
    defaultMonth,
    onMonthChange,
    weekStartsOn = 0,
    locale,
    disabled,
    bookedDates,
    events,
    quickNav = true,
    className,
    selected,
    defaultSelected,
    onSelect,
  } = props as CalendarSharedProps & {
    mode?: CalendarMode
    selected?: Date | Date[] | DateRange
    defaultSelected?: Date | Date[] | DateRange
    onSelect?: (value: Date | undefined | Date[] | DateRange | undefined) => void
  }

  const prefersReducedMotion = useReducedMotion()
  const zh = isZhLocale(locale)
  const monthNames = zh ? MONTH_NAMES_ZH : MONTH_NAMES_EN
  const monthShortNames = zh ? MONTH_NAMES_ZH : MONTH_SHORT_EN
  const today = React.useMemo(() => startOfDay(new Date()), [])

  const [internalMonth, setInternalMonth] = React.useState(() =>
    startOfDay(defaultMonth ?? controlledMonth ?? new Date()),
  )
  const isMonthControlled = controlledMonth !== undefined
  const displayedMonth = isMonthControlled ? controlledMonth : internalMonth

  const [navView, setNavView] = React.useState<"days" | "months" | "years">("days")
  const [direction, setDirection] = React.useState(1)
  const [hoveredDate, setHoveredDate] = React.useState<Date | null>(null)
  const [yearAnchor, setYearAnchor] = React.useState(() => displayedMonth.getFullYear())

  function openYearsView() {
    setYearAnchor(displayedMonth.getFullYear())
    setNavView("years")
  }

  const dayGridRef = React.useRef<HTMLDivElement | null>(null)
  const pendingFocusDateRef = React.useRef<Date | null>(null)

  React.useEffect(() => {
    if (!pendingFocusDateRef.current) return
    const iso = pendingFocusDateRef.current.toDateString()
    pendingFocusDateRef.current = null
    ;(dayGridRef.current?.querySelector(`[data-date-key="${iso}"]`) as HTMLElement | null)?.focus()
  }, [displayedMonth])

  const setMonth = React.useCallback(
    (next: Date, dir: number) => {
      setDirection(dir)
      if (!isMonthControlled) setInternalMonth(next)
      onMonthChange?.(next)
    },
    [isMonthControlled, onMonthChange],
  )

  const [internalSingle, setInternalSingle] = React.useState<Date | undefined>(
    mode === "single" ? (defaultSelected as Date | undefined) : undefined,
  )
  const [internalMultiple, setInternalMultiple] = React.useState<Date[]>(
    mode === "multiple" ? ((defaultSelected as Date[] | undefined) ?? []) : [],
  )
  const [internalRange, setInternalRange] = React.useState<DateRange | undefined>(
    mode === "range" ? (defaultSelected as DateRange | undefined) : undefined,
  )

  const isSelectedControlled = selected !== undefined

  const currentSingle = mode === "single" ? ((isSelectedControlled ? selected : internalSingle) as Date | undefined) : undefined
  const currentMultiple = mode === "multiple" ? ((isSelectedControlled ? selected : internalMultiple) as Date[]) : []
  const currentRange = mode === "range" ? ((isSelectedControlled ? selected : internalRange) as DateRange | undefined) : undefined

  function isDisabled(date: Date): boolean {
    if (bookedDates?.some((b) => isSameDay(b, date))) return true
    return disabled?.(date) ?? false
  }

  function isBooked(date: Date): boolean {
    return bookedDates?.some((b) => isSameDay(b, date)) ?? false
  }

  function eventFor(date: Date): CalendarEvent | undefined {
    return events?.find((e) => isSameDay(e.date, date))
  }

  function handleSelectDay(date: Date) {
    if (isDisabled(date)) return

    if (mode === "single") {
      const next = isSameDay(currentSingle, date) ? undefined : date
      if (!isSelectedControlled) setInternalSingle(next)
      ;(onSelect as CalendarSingleProps["onSelect"])?.(next)
      return
    }

    if (mode === "multiple") {
      const exists = currentMultiple.some((d) => isSameDay(d, date))
      const next = exists ? currentMultiple.filter((d) => !isSameDay(d, date)) : [...currentMultiple, date]
      if (!isSelectedControlled) setInternalMultiple(next)
      ;(onSelect as CalendarMultipleProps["onSelect"])?.(next)
      return
    }

    // range
    const from = currentRange?.from
    const to = currentRange?.to
    let next: DateRange
    if (!from || (from && to)) {
      next = { from: date, to: undefined }
    } else if (isBefore(date, from)) {
      next = { from: date, to: from }
    } else {
      next = { from, to: date }
    }
    if (!isSelectedControlled) setInternalRange(next)
    ;(onSelect as CalendarRangeProps["onSelect"])?.(next)
  }

  function goToMonth(offset: number) {
    setMonth(addMonths(displayedMonth, offset), offset >= 0 ? 1 : -1)
  }

  const weekdayLabels = zh
    ? (weekStartsOn === 1 ? WEEKDAY_MON_ZH : WEEKDAY_SUN_ZH)
    : (weekStartsOn === 1 ? WEEKDAY_MON_EN : WEEKDAY_SUN_EN)

  const grid = React.useMemo(
    () => buildMonthGrid(displayedMonth, weekStartsOn),
    [displayedMonth, weekStartsOn],
  )

  const focusableDate = React.useMemo(() => {
    if (mode === "single" && currentSingle && isSameMonth(currentSingle, displayedMonth)) return currentSingle
    if (mode === "range" && currentRange?.from && isSameMonth(currentRange.from, displayedMonth)) return currentRange.from
    if (isSameMonth(today, displayedMonth)) return today
    return new Date(displayedMonth.getFullYear(), displayedMonth.getMonth(), 1)
  }, [mode, currentSingle, currentRange?.from, displayedMonth, today])

  // Hover preview range when user has selected `from` and is picking `to`.
  const previewFrom = mode === "range" && currentRange?.from && !currentRange.to && hoveredDate
    ? (isBefore(hoveredDate, currentRange.from) ? hoveredDate : currentRange.from)
    : undefined
  const previewTo = mode === "range" && currentRange?.from && !currentRange.to && hoveredDate
    ? (isBefore(hoveredDate, currentRange.from) ? currentRange.from : hoveredDate)
    : undefined

  function cellState(date: Date) {
    const outsideMonth = !isSameMonth(date, displayedMonth)
    const isToday = isSameDay(date, today)
    const isBookedDay = isBooked(date)
    const event = eventFor(date)
    const isDayDisabled = isDisabled(date)

    let isSelected = false
    let isRangeStart = false
    let isRangeEnd = false
    let isInRange = false
    let isPreviewRange = false
    let isPreviewStart = false
    let isPreviewEnd = false

    if (mode === "single") {
      isSelected = isSameDay(currentSingle, date)
    } else if (mode === "multiple") {
      isSelected = currentMultiple.some((d) => isSameDay(d, date))
    } else if (mode === "range" && currentRange) {
      isRangeStart = isSameDay(currentRange.from, date)
      isRangeEnd = isSameDay(currentRange.to, date)
      isInRange = isWithinRange(date, currentRange.from, currentRange.to)
      isSelected = isRangeStart || isRangeEnd
      if (previewFrom && previewTo && !isSameDay(previewFrom, previewTo)) {
        isPreviewStart = isSameDay(previewFrom, date)
        isPreviewEnd = isSameDay(previewTo, date)
        isPreviewRange = isWithinRange(date, previewFrom, previewTo)
      }
    }

    return {
      outsideMonth,
      isToday,
      isBookedDay,
      event,
      isDayDisabled,
      isSelected,
      isRangeStart,
      isRangeEnd,
      isInRange,
      isPreviewRange,
      isPreviewStart,
      isPreviewEnd,
    }
  }

  const monthLabel = zh
    ? `${displayedMonth.getFullYear()}年 ${monthNames[displayedMonth.getMonth()]}`
    : `${monthNames[displayedMonth.getMonth()]} ${displayedMonth.getFullYear()}`

  const weeks = React.useMemo(() => {
    const rows: Date[][] = []
    for (let i = 0; i < grid.length; i += 7) {
      rows.push(grid.slice(i, i + 7))
    }
    return rows
  }, [grid])

  return (
    <div
      ref={ref}
      data-slot="calendar"
      data-mode={mode}
      className={cn(
        "w-full max-w-[300px] select-none rounded-2xl border border-border/75 bg-card p-3",
        className,
      )}
    >
      <div className="sr-only" aria-live="polite" aria-atomic="true">
        {monthLabel}
      </div>

      {/* Header */}
      <div className="mb-2.5 flex items-center justify-between gap-1">
        <button
          type="button"
          aria-label={zh ? "上个月" : "Previous month"}
          onClick={() => goToMonth(-1)}
          className={cn(
            "flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground",
            "outline-none transition-colors duration-150",
            "hover:bg-muted/75 hover:text-foreground",
            "focus-visible:ring-2 focus-visible:ring-ring",
          )}
        >
          <ChevronLeft className="size-4" />
        </button>

        <div className="flex items-center gap-0.5">
          {zh ? (
            <>
              <QuickNavTooltip label="切换年份" disabled={!quickNav}>
                <button
                  type="button"
                  disabled={!quickNav}
                  aria-label="选择年份"
                  onClick={openYearsView}
                  className={cn(
                    "rounded-lg px-1.5 py-1 font-sans text-[13px] font-semibold tracking-[-0.01em] text-foreground tabular-nums",
                    "outline-none transition-colors duration-150",
                    quickNav && "hover:bg-muted/70",
                    "focus-visible:ring-2 focus-visible:ring-ring",
                    navView === "years" && "bg-primary/10 text-primary",
                  )}
                >
                  {displayedMonth.getFullYear()}年
                </button>
              </QuickNavTooltip>
              <span className="text-xs text-muted-foreground/40" aria-hidden="true">·</span>
              <QuickNavTooltip label="切换月份" disabled={!quickNav}>
                <button
                  type="button"
                  disabled={!quickNav}
                  aria-label="选择月份"
                  onClick={() => setNavView(navView === "months" ? "days" : "months")}
                  className={cn(
                    "rounded-lg px-1.5 py-1 font-sans text-[13px] font-semibold tracking-[-0.01em] text-foreground",
                    "outline-none transition-colors duration-150",
                    quickNav && "hover:bg-muted/70",
                    "focus-visible:ring-2 focus-visible:ring-ring",
                    navView === "months" && "bg-primary/10 text-primary",
                  )}
                >
                  {monthNames[displayedMonth.getMonth()]}
                </button>
              </QuickNavTooltip>
            </>
          ) : (
            <>
              <QuickNavTooltip label="Jump to month" disabled={!quickNav}>
                <button
                  type="button"
                  disabled={!quickNav}
                  aria-label="Show month picker"
                  onClick={() => setNavView(navView === "months" ? "days" : "months")}
                  className={cn(
                    "rounded-lg px-1.5 py-1 font-sans text-[13px] font-semibold tracking-[-0.01em] text-foreground",
                    "outline-none transition-colors duration-150",
                    quickNav && "hover:bg-muted/70",
                    "focus-visible:ring-2 focus-visible:ring-ring",
                    navView === "months" && "bg-primary/10 text-primary",
                  )}
                >
                  {monthNames[displayedMonth.getMonth()]}
                </button>
              </QuickNavTooltip>
              <span className="text-xs text-muted-foreground/40" aria-hidden="true">·</span>
              <QuickNavTooltip label="Jump to year" disabled={!quickNav}>
                <button
                  type="button"
                  disabled={!quickNav}
                  aria-label="Show year picker"
                  onClick={openYearsView}
                  className={cn(
                    "rounded-lg px-1.5 py-1 font-sans text-[13px] font-semibold tracking-[-0.01em] text-foreground tabular-nums",
                    "outline-none transition-colors duration-150",
                    quickNav && "hover:bg-muted/70",
                    "focus-visible:ring-2 focus-visible:ring-ring",
                    navView === "years" && "bg-primary/10 text-primary",
                  )}
                >
                  {displayedMonth.getFullYear()}
                </button>
              </QuickNavTooltip>
            </>
          )}
        </div>

        <button
          type="button"
          aria-label={zh ? "下个月" : "Next month"}
          onClick={() => goToMonth(1)}
          className={cn(
            "flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground",
            "outline-none transition-colors duration-150",
            "hover:bg-muted/75 hover:text-foreground",
            "focus-visible:ring-2 focus-visible:ring-ring",
          )}
        >
          <ChevronRight className="size-4" />
        </button>
      </div>

      <div className="relative overflow-hidden">
        <AnimatePresence mode="popLayout" initial={false} custom={direction}>
          {navView === "days" && (
            <motion.div
              key={`${displayedMonth.getFullYear()}-${displayedMonth.getMonth()}`}
              custom={direction}
              initial={prefersReducedMotion ? { opacity: 0 } : { opacity: 0, x: direction * 12 }}
              animate={{ opacity: 1, x: 0 }}
              exit={prefersReducedMotion ? { opacity: 0 } : { opacity: 0, x: direction * -12 }}
              transition={{ duration: prefersReducedMotion ? 0.12 : 0.18, ease: EASE_OUT }}
            >
              {/* Weekday row */}
              <div className="mb-1 grid grid-cols-7">
                {weekdayLabels.map((label, i) => (
                  <div
                    key={`${label}-${i}`}
                    className="flex h-7 items-center justify-center text-[11px] font-medium text-muted-foreground/80"
                  >
                    {label}
                  </div>
                ))}
              </div>

              {/* Day grid */}
              <div
                ref={dayGridRef}
                role="grid"
                aria-label={monthLabel}
                className="grid grid-cols-7 gap-y-0.5"
                onMouseLeave={() => setHoveredDate(null)}
                onKeyDown={(e) => {
                  const deltas: Record<string, number> = {
                    ArrowLeft: -1,
                    ArrowRight: 1,
                    ArrowUp: -7,
                    ArrowDown: 7,
                  }
                  let targetDate: Date | undefined
                  if (e.key in deltas) {
                    const current = new Date((e.target as HTMLElement).dataset.date ?? "")
                    current.setDate(current.getDate() + deltas[e.key]!)
                    targetDate = current
                  } else if (e.key === "Home" || e.key === "End") {
                    const current = new Date((e.target as HTMLElement).dataset.date ?? "")
                    const weekday = weekStartsOn === 1 ? (current.getDay() + 6) % 7 : current.getDay()
                    current.setDate(current.getDate() + (e.key === "Home" ? -weekday : 6 - weekday))
                    targetDate = current
                  } else if (e.key === "PageUp" || e.key === "PageDown") {
                    e.preventDefault()
                    const current = new Date((e.target as HTMLElement).dataset.date ?? "")
                    targetDate = addMonths(current, e.key === "PageUp" ? -1 : 1)
                    const daysInTargetMonth = new Date(targetDate.getFullYear(), targetDate.getMonth() + 1, 0).getDate()
                    targetDate.setDate(Math.min(current.getDate(), daysInTargetMonth))
                  }
                  if (!targetDate) return
                  e.preventDefault()
                  if (!isSameMonth(targetDate, displayedMonth)) {
                    pendingFocusDateRef.current = targetDate
                    setMonth(new Date(targetDate.getFullYear(), targetDate.getMonth(), 1), isBefore(targetDate, displayedMonth) ? -1 : 1)
                    return
                  }
                  const iso = targetDate.toDateString()
                  ;(dayGridRef.current?.querySelector(`[data-date-key="${iso}"]`) as HTMLElement | null)?.focus()
                }}
              >
                {weeks.map((week, weekIndex) => (
                  <div key={weekIndex} role="row" className="contents">
                    {week.map((date) => {
                      const state = cellState(date)
                      const showCommittedBand =
                        mode === "range" &&
                        currentRange?.from &&
                        currentRange?.to &&
                        !isSameDay(currentRange.from, currentRange.to) &&
                        (state.isInRange || state.isRangeStart || state.isRangeEnd)
                      const showPreviewBand =
                        !showCommittedBand &&
                        mode === "range" &&
                        (state.isPreviewRange || state.isPreviewStart || state.isPreviewEnd)

                      return (
                        <div
                          key={date.toISOString()}
                          role="gridcell"
                          aria-selected={state.isSelected || undefined}
                          className="relative flex h-8 items-center justify-center"
                        >
                          {showCommittedBand && (
                            <div
                              className={cn(
                                "absolute inset-y-0.5 bg-primary/[0.11] dark:bg-primary/[0.18]",
                                state.isRangeStart ? "left-1/2 right-0" : state.isRangeEnd ? "left-0 right-1/2" : "inset-x-0",
                              )}
                            />
                          )}
                          {showPreviewBand && (
                            <div
                              className={cn(
                                "absolute inset-y-0.5 bg-primary/[0.06] dark:bg-primary/[0.10]",
                                state.isPreviewStart ? "left-1/2 right-0" : state.isPreviewEnd ? "left-0 right-1/2" : "inset-x-0",
                              )}
                            />
                          )}
                          <button
                            type="button"
                            disabled={state.isDayDisabled}
                            aria-label={date.toDateString()}
                            data-date={date.toISOString()}
                            data-date-key={date.toDateString()}
                            tabIndex={isSameDay(date, focusableDate) ? 0 : -1}
                            onMouseEnter={() => {
                              if (mode === "range" && !state.isDayDisabled) setHoveredDate(date)
                            }}
                            onClick={() => handleSelectDay(date)}
                            className={cn(
                              "relative z-10 flex size-8 items-center justify-center rounded-full font-sans text-[12.5px] font-medium tabular-nums",
                              "outline-none transition-all duration-150",
                              "focus-visible:ring-2 focus-visible:ring-ring",
                              state.outsideMonth ? "text-muted-foreground/35" : "text-foreground",
                              !state.isSelected &&
                                !state.isDayDisabled &&
                                "hover:bg-muted/80",
                              state.isSelected &&
                                "bg-primary text-primary-foreground shadow-xs hover:bg-primary/90",
                              state.isToday &&
                                !state.isSelected &&
                                "ring-1 ring-inset ring-primary/45 font-semibold text-primary",
                              state.isDayDisabled &&
                                !state.isBookedDay &&
                                "pointer-events-none text-muted-foreground opacity-30",
                              state.isBookedDay &&
                                "pointer-events-none text-muted-foreground/50 line-through",
                            )}
                          >
                            {date.getDate()}
                            {state.event && !state.isSelected && (
                              <span className="absolute bottom-0.5 size-1 rounded-full bg-sky-500" />
                            )}
                          </button>
                        </div>
                      )
                    })}
                  </div>
                ))}
              </div>
            </motion.div>
          )}

          {navView === "months" && (
            <motion.div
              key="months"
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              transition={{ duration: prefersReducedMotion ? 0.12 : 0.15, ease: EASE_OUT }}
              className="grid grid-cols-3 gap-1.5 py-1.5"
            >
              {monthShortNames.map((name, i) => (
                <button
                  key={name}
                  type="button"
                  onClick={() => {
                    setMonth(new Date(displayedMonth.getFullYear(), i, 1), 1)
                    setNavView("days")
                  }}
                  className={cn(
                    "flex h-9 items-center justify-center rounded-lg font-sans text-[12.5px] font-medium text-foreground",
                    "outline-none transition-colors duration-150",
                    "hover:bg-muted/80",
                    i === displayedMonth.getMonth() &&
                      "bg-primary text-primary-foreground shadow-xs hover:bg-primary/90",
                  )}
                >
                  {name}
                </button>
              ))}
            </motion.div>
          )}

          {navView === "years" && (
            <motion.div
              key="years"
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              transition={{ duration: prefersReducedMotion ? 0.12 : 0.15, ease: EASE_OUT }}
            >
              <div className="mb-1.5 flex items-center justify-between px-0.5">
                <button
                  type="button"
                  aria-label={zh ? "前 12 年" : "Previous years"}
                  onClick={() => setYearAnchor((y) => y - 12)}
                  className="flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground outline-none transition-colors hover:bg-muted/80 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
                >
                  <ChevronLeft className="size-4" />
                </button>
                <span className="font-sans text-[11.5px] font-medium text-muted-foreground tabular-nums">
                  {yearAnchor - 5} – {yearAnchor + 6}
                </span>
                <button
                  type="button"
                  aria-label={zh ? "后 12 年" : "Next years"}
                  onClick={() => setYearAnchor((y) => y + 12)}
                  className="flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground outline-none transition-colors hover:bg-muted/80 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
                >
                  <ChevronRight className="size-4" />
                </button>
              </div>

              <div className="grid grid-cols-3 gap-1.5 py-1">
                {Array.from({ length: 12 }, (_, i) => yearAnchor - 5 + i).map((year) => (
                  <button
                    key={year}
                    type="button"
                    onClick={() => {
                      setMonth(new Date(year, displayedMonth.getMonth(), 1), 1)
                      setNavView("months")
                    }}
                    className={cn(
                      "flex h-9 items-center justify-center rounded-lg font-sans text-[12.5px] font-medium tabular-nums text-foreground",
                      "outline-none transition-colors duration-150",
                      "hover:bg-muted/80",
                      year === displayedMonth.getFullYear() &&
                        "bg-primary text-primary-foreground shadow-xs hover:bg-primary/90",
                    )}
                  >
                    {year}
                  </button>
                ))}
              </div>
            </motion.div>
          )}
        </AnimatePresence>
      </div>
    </div>
  )
})
Calendar.displayName = "Calendar"
