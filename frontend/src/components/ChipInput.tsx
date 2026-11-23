import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ChangeEvent,
} from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { Check, ChevronDown, Search, X } from 'lucide-react'
import { cn } from '@/lib/utils'

export interface ChipInputProps {
  value: string[]
  onChange: (next: string[]) => void
  /** Pre-defined options for select-from-list mode */
  options?: string[]
  placeholder?: string
  disabled?: boolean
  /**
   * Max chips shown in the trigger before "+N".
   * Default shows all chips so each model can be removed via X.
   */
  maxVisible?: number
  className?: string
  /**
   * Prefer opening the options dropdown above the input.
   * When omitted, direction is chosen automatically from available viewport space.
   */
  dropUp?: boolean
}

interface DropdownPosition {
  top: number
  left: number
  width: number
  maxHeight: number
  openUp: boolean
}

const DROPDOWN_GAP = 6
const DROPDOWN_MAX_HEIGHT = 280
const VIEWPORT_PADDING = 8

// Radix Dialog 的 react-remove-scroll 会在 document capture 阶段对
// 弹层外（含 createPortal 到 body 的下拉）的 wheel/touchmove 调 preventDefault，
// 导致 overflow 容器无法原生滚动。手动改 scrollTop，兼容 Dialog 内外。
function applyManualScroll(el: HTMLElement, deltaX: number, deltaY: number): boolean {
  const canScrollY = el.scrollHeight > el.clientHeight + 1
  const canScrollX = el.scrollWidth > el.clientWidth + 1
  if (!canScrollY && !canScrollX) return false

  let scrolled = false
  if (canScrollY && deltaY !== 0) {
    const maxTop = el.scrollHeight - el.clientHeight
    const next = Math.min(maxTop, Math.max(0, el.scrollTop + deltaY))
    if (next !== el.scrollTop) {
      el.scrollTop = next
      scrolled = true
    }
  }
  if (canScrollX && deltaX !== 0) {
    const maxLeft = el.scrollWidth - el.clientWidth
    const next = Math.min(maxLeft, Math.max(0, el.scrollLeft + deltaX))
    if (next !== el.scrollLeft) {
      el.scrollLeft = next
      scrolled = true
    }
  }
  return scrolled
}

/**
 * Multi-select chip input:
 * - Free-text tag entry (Enter / comma)
 * - Select-from-list dropdown (portaled, Dialog-safe)
 * - Each chip has an X to remove
 * - Multi-select keeps the menu open after each pick
 */
export default function ChipInput({
  value,
  onChange,
  options,
  placeholder = '',
  disabled = false,
  maxVisible,
  className = '',
  dropUp,
}: ChipInputProps) {
  const { i18n } = useTranslation()
  const isZh = (i18n.language || '').toLowerCase().startsWith('zh')
  const [draft, setDraft] = useState('')
  const [showDropdown, setShowDropdown] = useState(false)
  const [position, setPosition] = useState<DropdownPosition | null>(null)
  const [highlightIndex, setHighlightIndex] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const triggerRef = useRef<HTMLDivElement>(null)
  const dropdownRef = useRef<HTMLDivElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const touchStartRef = useRef<{ x: number; y: number } | null>(null)
  // Latest value for event handlers that may close over stale props.
  const valueRef = useRef(value)
  valueRef.current = value
  const onChangeRef = useRef(onChange)
  onChangeRef.current = onChange

  const hasOptions = Array.isArray(options) && options.length > 0
  const chipLimit = typeof maxVisible === 'number' && maxVisible > 0 ? maxVisible : Number.POSITIVE_INFINITY

  const availableOptions = useMemo(() => {
    if (!hasOptions) return []
    const selected = new Set(value.map((v) => v.toLowerCase()))
    const query = draft.trim().toLowerCase()
    return options!
      .filter((opt) => !selected.has(opt.toLowerCase()))
      .filter((opt) => (query ? opt.toLowerCase().includes(query) : true))
  }, [hasOptions, options, value, draft])

  const computePosition = useCallback(() => {
    const trigger = triggerRef.current
    if (!trigger) return
    const rect = trigger.getBoundingClientRect()
    const viewportHeight = window.innerHeight
    const viewportWidth = window.innerWidth
    const spaceBelow = viewportHeight - rect.bottom - DROPDOWN_GAP - VIEWPORT_PADDING
    const spaceAbove = rect.top - DROPDOWN_GAP - VIEWPORT_PADDING
    const preferUp =
      typeof dropUp === 'boolean'
        ? dropUp
        : spaceBelow < Math.min(DROPDOWN_MAX_HEIGHT, 160) && spaceAbove > spaceBelow
    const maxHeight = Math.max(120, Math.min(DROPDOWN_MAX_HEIGHT, preferUp ? spaceAbove : spaceBelow))
    const width = Math.min(Math.max(rect.width, 220), viewportWidth - VIEWPORT_PADDING * 2)
    const maxLeft = viewportWidth - width - VIEWPORT_PADDING
    const left = Math.min(Math.max(VIEWPORT_PADDING, rect.left), Math.max(VIEWPORT_PADDING, maxLeft))
    setPosition({
      top: preferUp ? rect.top - DROPDOWN_GAP : rect.bottom + DROPDOWN_GAP,
      left,
      width,
      maxHeight,
      openUp: preferUp,
    })
  }, [dropUp])

  useLayoutEffect(() => {
    if (!showDropdown) {
      setPosition(null)
      return
    }
    computePosition()
  }, [showDropdown, computePosition, availableOptions.length, value.length, draft])

  useEffect(() => {
    if (!showDropdown) return

    // The portaled options count as inside, even though they are outside the trigger.
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target as Node | null
      if (!target) return
      if (triggerRef.current?.contains(target)) return
      if (dropdownRef.current?.contains(target)) return
      setShowDropdown(false)
    }

    const handleEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.stopPropagation()
        setShowDropdown(false)
      }
    }

    const handleReposition = () => computePosition()

    const handleWheel = (event: WheelEvent) => {
      const list = listRef.current
      if (!list) return
      const target = event.target as Node | null
      if (!target || !list.contains(target)) return
      if (applyManualScroll(list, event.deltaX, event.deltaY)) {
        event.preventDefault()
      }
    }

    const handleTouchStart = (event: TouchEvent) => {
      const list = listRef.current
      if (!list) return
      const target = event.target as Node | null
      if (!target || !list.contains(target)) return
      const touch = event.touches[0]
      if (!touch) return
      touchStartRef.current = { x: touch.clientX, y: touch.clientY }
    }

    const handleTouchMove = (event: TouchEvent) => {
      const list = listRef.current
      const start = touchStartRef.current
      if (!list || !start) return
      const target = event.target as Node | null
      if (!target || !list.contains(target)) return
      const touch = event.touches[0]
      if (!touch) return
      const deltaX = start.x - touch.clientX
      const deltaY = start.y - touch.clientY
      touchStartRef.current = { x: touch.clientX, y: touch.clientY }
      if (applyManualScroll(list, deltaX, deltaY)) {
        event.preventDefault()
      }
    }

    document.addEventListener('pointerdown', handlePointerDown)
    document.addEventListener('keydown', handleEscape)
    window.addEventListener('resize', handleReposition)
    window.addEventListener('scroll', handleReposition, true)
    document.addEventListener('wheel', handleWheel, { passive: false, capture: true })
    document.addEventListener('touchstart', handleTouchStart, { passive: true, capture: true })
    document.addEventListener('touchmove', handleTouchMove, { passive: false, capture: true })

    return () => {
      document.removeEventListener('pointerdown', handlePointerDown)
      document.removeEventListener('keydown', handleEscape)
      window.removeEventListener('resize', handleReposition)
      window.removeEventListener('scroll', handleReposition, true)
      document.removeEventListener('wheel', handleWheel, true)
      document.removeEventListener('touchstart', handleTouchStart, true)
      document.removeEventListener('touchmove', handleTouchMove, true)
      touchStartRef.current = null
    }
  }, [showDropdown, computePosition])

  useEffect(() => {
    setHighlightIndex(0)
  }, [draft, showDropdown, availableOptions.length])

  const addChip = useCallback((tag: string) => {
    const trimmed = tag.trim()
    if (!trimmed) return
    const current = valueRef.current
    const lower = trimmed.toLowerCase()
    if (current.some((v) => v.toLowerCase() === lower)) return
    onChangeRef.current([...current, trimmed])
    setDraft('')
    // Keep menu open for multi-select.
    requestAnimationFrame(() => {
      inputRef.current?.focus()
      computePosition()
    })
  }, [computePosition])

  const removeChip = useCallback((index: number) => {
    const current = valueRef.current
    if (index < 0 || index >= current.length) return
    const next = current.filter((_, i) => i !== index)
    onChangeRef.current(next)
  }, [])

  const selectOption = useCallback(
    (opt: string) => {
      addChip(opt)
    },
    [addChip],
  )

  const handleKeyDown = useCallback(
    (e: KeyboardEvent<HTMLInputElement>) => {
      if (disabled) return

      if (showDropdown && availableOptions.length > 0) {
        if (e.key === 'ArrowDown') {
          e.preventDefault()
          setHighlightIndex((i) => {
            const next = (i + 1) % availableOptions.length
            requestAnimationFrame(() => {
              listRef.current
                ?.querySelectorAll<HTMLElement>('[role="option"]')
                [next]?.scrollIntoView({ block: 'nearest' })
            })
            return next
          })
          return
        }
        if (e.key === 'ArrowUp') {
          e.preventDefault()
          setHighlightIndex((i) => {
            const next = (i - 1 + availableOptions.length) % availableOptions.length
            requestAnimationFrame(() => {
              listRef.current
                ?.querySelectorAll<HTMLElement>('[role="option"]')
                [next]?.scrollIntoView({ block: 'nearest' })
            })
            return next
          })
          return
        }
        if (e.key === 'Enter') {
          e.preventDefault()
          const pick = availableOptions[highlightIndex]
          if (pick) selectOption(pick)
          else if (draft.trim()) addChip(draft)
          return
        }
      }

      if (e.key === 'Enter' || e.key === ',') {
        e.preventDefault()
        if (draft.trim()) addChip(draft)
      } else if (e.key === 'Backspace' && !draft && value.length > 0) {
        removeChip(value.length - 1)
      } else if (e.key === 'Escape' && showDropdown) {
        e.preventDefault()
        setShowDropdown(false)
      }
    },
    [disabled, draft, addChip, removeChip, value.length, showDropdown, availableOptions, highlightIndex, selectOption],
  )

  const handleChange = useCallback(
    (e: ChangeEvent<HTMLInputElement>) => {
      const v = e.target.value
      if (v.includes(',')) {
        const parts = v.split(',')
        const current = valueRef.current
        const existing = new Set(current.map((item) => item.toLowerCase()))
        const toAdd: string[] = []
        for (let i = 0; i < parts.length - 1; i++) {
          const trimmed = parts[i].trim()
          if (!trimmed) continue
          const lowered = trimmed.toLowerCase()
          if (existing.has(lowered)) continue
          existing.add(lowered)
          toAdd.push(trimmed)
        }
        if (toAdd.length > 0) onChangeRef.current([...current, ...toAdd])
        setDraft(parts[parts.length - 1])
      } else {
        setDraft(v)
      }
      if (hasOptions) setShowDropdown(true)
    },
    [hasOptions],
  )

  const visibleChips = value.slice(0, Number.isFinite(chipLimit) ? chipLimit : value.length)
  const overflowCount = Number.isFinite(chipLimit) ? Math.max(0, value.length - chipLimit) : 0

  const dropdown =
    hasOptions && showDropdown && position
      ? createPortal(
          <div
            ref={dropdownRef}
            data-select-dropdown="true"
            className="pointer-events-auto fixed z-[1000] flex flex-col overflow-hidden rounded-xl border border-border/80 bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.16)] backdrop-blur-sm"
            style={
              position.openUp
                ? {
                    left: position.left,
                    width: position.width,
                    bottom: window.innerHeight - position.top,
                    maxHeight: position.maxHeight,
                  }
                : {
                    left: position.left,
                    width: position.width,
                    top: position.top,
                    maxHeight: position.maxHeight,
                  }
            }
          >
            <div className="flex shrink-0 items-center gap-2 border-b border-border/70 bg-muted/30 px-3 py-2">
              <Search className="size-3.5 shrink-0 text-muted-foreground" />
              <span className="min-w-0 truncate text-[11px] font-medium text-muted-foreground">
                {draft.trim()
                  ? isZh
                    ? `筛选 “${draft.trim()}” · ${availableOptions.length} 项`
                    : `Filter “${draft.trim()}” · ${availableOptions.length}`
                  : value.length > 0
                    ? isZh
                      ? `已选 ${value.length} · 还可选 ${availableOptions.length}（可多选）`
                      : `${value.length} selected · ${availableOptions.length} left · multi-select`
                    : isZh
                      ? `可选 ${availableOptions.length} 项 · 可多选 · 留空 = 全部`
                      : `${availableOptions.length} options · multi-select · empty = all`}
              </span>
            </div>
            <div
              ref={listRef}
              className={cn(
                'min-h-0 flex-1 overscroll-contain overflow-x-hidden overflow-y-auto p-1.5',
                '[scrollbar-gutter:stable] [scrollbar-width:thin]',
                '[&::-webkit-scrollbar]:w-1.5',
                '[&::-webkit-scrollbar-track]:bg-transparent',
                '[&::-webkit-scrollbar-thumb]:rounded-full',
                '[&::-webkit-scrollbar-thumb]:bg-border',
                '[&::-webkit-scrollbar-thumb:hover]:bg-muted-foreground/40',
              )}
              style={{ maxHeight: Math.max(80, position.maxHeight - 42) }}
              role="listbox"
              aria-multiselectable="true"
            >
              {availableOptions.length === 0 ? (
                <div className="px-3 py-6 text-center text-xs text-muted-foreground">
                  {draft.trim()
                    ? isZh
                      ? '无匹配项，可直接回车添加自定义值'
                      : 'No match — press Enter to add custom value'
                    : isZh
                      ? '没有更多可选项'
                      : 'No more options'}
                </div>
              ) : (
                <div className="space-y-0.5">
                  {availableOptions.map((opt, index) => {
                    const active = index === highlightIndex
                    return (
                      <button
                        key={opt}
                        type="button"
                        role="option"
                        aria-selected={active}
                        className={cn(
                          'flex w-full items-center gap-2 rounded-lg px-2.5 py-2 text-left text-sm transition-colors',
                          active
                            ? 'bg-accent text-accent-foreground'
                            : 'text-foreground hover:bg-accent/70 hover:text-accent-foreground',
                        )}
                        onMouseEnter={() => setHighlightIndex(index)}
                        // Keep input focus on mouse clicks without selecting during a swipe.
                        onMouseDown={(event) => event.preventDefault()}
                        onClick={(event) => {
                          event.preventDefault()
                          event.stopPropagation()
                          selectOption(opt)
                        }}
                      >
                        <span className="min-w-0 flex-1 truncate font-mono text-[12.5px] tracking-tight">
                          {opt}
                        </span>
                        {active ? <Check className="size-3.5 shrink-0 text-primary" /> : null}
                      </button>
                    )
                  })}
                </div>
              )}
            </div>
            {draft.trim() && !availableOptions.some((o) => o.toLowerCase() === draft.trim().toLowerCase()) ? (
              <div className="shrink-0 border-t border-border/70 px-2.5 py-2">
                <button
                  type="button"
                  className="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-xs font-medium text-primary transition-colors hover:bg-primary/10"
                  onMouseDown={(event) => event.preventDefault()}
                  onClick={(event) => {
                    event.preventDefault()
                    event.stopPropagation()
                    addChip(draft)
                  }}
                >
                  <span className="truncate">
                    {isZh ? `添加 “${draft.trim()}”` : `Add “${draft.trim()}”`}
                  </span>
                </button>
              </div>
            ) : null}
          </div>,
          document.body,
        )
      : null

  return (
    <div className={cn('relative w-full', className)}>
      <div
        ref={triggerRef}
        className={cn(
          'flex min-h-10 w-full flex-wrap items-center gap-1.5 rounded-xl border border-input bg-background px-2.5 py-1.5 text-sm shadow-xs transition-[border-color,box-shadow]',
          disabled ? 'cursor-not-allowed opacity-50' : 'cursor-text',
          showDropdown
            ? 'border-ring ring-[3px] ring-ring/50'
            : 'focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50',
        )}
        onClick={() => {
          if (disabled) return
          inputRef.current?.focus()
          if (hasOptions) setShowDropdown(true)
        }}
      >
        {visibleChips.map((chip, i) => (
          <span
            key={`${chip}-${i}`}
            className="inline-flex max-w-full items-center gap-0.5 rounded-md border border-primary/15 bg-primary/10 py-0.5 pl-2 pr-0.5 text-xs font-medium text-primary"
          >
            <span className="max-w-[12rem] truncate font-mono text-[11.5px] leading-none">{chip}</span>
            {!disabled && (
              <button
                type="button"
                onClick={(e) => {
                  e.preventDefault()
                  e.stopPropagation()
                  removeChip(i)
                }}
                onPointerDown={(e) => {
                  // Don't steal focus / don't bubble to trigger open handlers.
                  e.preventDefault()
                  e.stopPropagation()
                }}
                className="inline-flex size-5 shrink-0 items-center justify-center rounded-md text-primary/80 transition-colors hover:bg-primary/20 hover:text-primary"
                aria-label={isZh ? `删除 ${chip}` : `Remove ${chip}`}
                title={isZh ? '删除' : 'Remove'}
              >
                <X className="size-3.5" strokeWidth={2.25} />
              </button>
            )}
          </span>
        ))}
        {overflowCount > 0 && (
          <button
            type="button"
            className="inline-flex items-center rounded-md bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground hover:bg-muted/80"
            onClick={(e) => {
              e.stopPropagation()
              // Expand: temporarily show all by focusing — parent can pass higher maxVisible.
              // Fallback: toast-like title with full list.
            }}
            title={value.slice(chipLimit).join(', ')}
          >
            +{overflowCount}
          </button>
        )}
        <input
          ref={inputRef}
          type="text"
          value={draft}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onFocus={() => {
            if (hasOptions) setShowDropdown(true)
          }}
          placeholder={value.length === 0 ? placeholder : isZh ? '继续添加…' : 'Add more…'}
          disabled={disabled}
          className="min-w-[6rem] flex-1 bg-transparent py-0.5 text-sm outline-none placeholder:text-muted-foreground disabled:cursor-not-allowed"
        />
        {hasOptions && (
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation()
              if (disabled) return
              setShowDropdown((open) => !open)
              inputRef.current?.focus()
            }}
            className="shrink-0 rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            tabIndex={-1}
            aria-label="Toggle options"
          >
            <ChevronDown className={cn('size-4 transition-transform', showDropdown && 'rotate-180')} />
          </button>
        )}
      </div>
      {dropdown}
    </div>
  )
}
