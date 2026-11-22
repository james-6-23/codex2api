import { Check, ChevronDown } from 'lucide-react'
import { type ReactNode, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { cn } from '@/lib/utils'
import { selectAutoScrollKey } from '@/lib/selectScroll'

export interface SelectOption {
  label: string
  value: string
  triggerLabel?: string
  content?: ReactNode
  triggerContent?: ReactNode
}

interface SelectProps {
  id?: string
  'aria-label'?: string
  value: string
  onValueChange: (value: string) => void
  options: SelectOption[]
  placeholder?: string
  disabled?: boolean
  className?: string
  compact?: boolean
  triggerClassName?: string
}

interface DropdownPosition {
  top: number
  left: number
  width: number
  maxHeight: number
  openUp: boolean
}

const DROPDOWN_GAP = 8
const DROPDOWN_MAX_HEIGHT = 320
const VIEWPORT_PADDING = 8

// Radix Dialog 的 react-remove-scroll 会在 document capture 阶段对
// 弹层外（含 createPortal 到 body 的下拉）的 wheel/touchmove 调 preventDefault，
// 导致 overflow 容器无法原生滚动。这里手动改 scrollTop，兼容 Dialog 内外。
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

export function Select({
  id,
  'aria-label': ariaLabel,
  value,
  onValueChange,
  options,
  placeholder = '请选择',
  disabled = false,
  className,
  compact = false,
  triggerClassName,
}: SelectProps) {
  const [open, setOpen] = useState(false)
  const [position, setPosition] = useState<DropdownPosition | null>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const dropdownRef = useRef<HTMLDivElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const touchStartRef = useRef<{ x: number; y: number } | null>(null)
  const selectedOption = options.find((option) => option.value === value)
  const autoScrollKey = selectAutoScrollKey(open, position !== null, value)

  const computePosition = useCallback(() => {
    const trigger = triggerRef.current
    if (!trigger) return
    const rect = trigger.getBoundingClientRect()
    const viewportHeight = window.innerHeight
    const viewportWidth = window.innerWidth
    const spaceBelow = viewportHeight - rect.bottom - DROPDOWN_GAP - VIEWPORT_PADDING
    const spaceAbove = rect.top - DROPDOWN_GAP - VIEWPORT_PADDING
    const openUp = spaceBelow < Math.min(DROPDOWN_MAX_HEIGHT, 160) && spaceAbove > spaceBelow
    const maxHeight = Math.max(140, Math.min(DROPDOWN_MAX_HEIGHT, openUp ? spaceAbove : spaceBelow))
    // Keep dropdown fully inside the viewport on small screens.
    const width = Math.min(rect.width, viewportWidth - VIEWPORT_PADDING * 2)
    const maxLeft = viewportWidth - width - VIEWPORT_PADDING
    const left = Math.min(Math.max(VIEWPORT_PADDING, rect.left), Math.max(VIEWPORT_PADDING, maxLeft))
    setPosition({
      top: openUp ? rect.top - DROPDOWN_GAP : rect.bottom + DROPDOWN_GAP,
      left,
      width,
      maxHeight,
      openUp,
    })
  }, [])

  useLayoutEffect(() => {
    if (!open) return
    computePosition()
  }, [open, computePosition, options.length])

  // 打开后把当前选中项滚进可视区。
  useLayoutEffect(() => {
    if (autoScrollKey === null) return
    const list = listRef.current
    if (!list) return
    const selected = list.querySelector<HTMLElement>('[aria-selected="true"]')
    selected?.scrollIntoView({ block: 'nearest' })
  }, [autoScrollKey])

  useEffect(() => {
    if (!open) return

    // 关闭仅在「点击 trigger 与 dropdown 之外」触发。注意 dropdown 通过 createPortal
    // 渲染在 document.body 下，与 trigger 不在同一 DOM 子树，必须按 ref 直接判断。
    // 用 pointerdown 而非 mousedown，能同时覆盖鼠标 / 触屏 / 笔，且对路径上的 React
    // 合成事件 stopPropagation 不敏感（native 监听拿到的总是真实 target）。
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target as Node | null
      if (!target) return
      if (triggerRef.current?.contains(target)) return
      if (dropdownRef.current?.contains(target)) return
      setOpen(false)
    }

    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setOpen(false)
      }
    }

    const handleReposition = (event: Event) => {
      const target = event.target
      // list.scrollTop 的变化也会被 window capture 阶段收到；此时触发重定位
      // 只会制造额外 render，并可能把用户重新拉回当前选中项。
      if (target instanceof Node && dropdownRef.current?.contains(target)) return
      computePosition()
    }

    // 在 document capture 阶段手动滚动：晚于 React 挂载、与 remove-scroll 同阶段，
    // 即使其 preventDefault 了默认滚动，我们仍可通过改 scrollTop 完成滚动。
    // 命中判断放宽到整个 dropdown（含滚动条 gutter、列表内边距等 list 子树之外的
    // 区域），滚动仍作用于内部可滚动列表，避免鼠标停在边缘时滚不动。
    const handleWheel = (event: WheelEvent) => {
      const list = listRef.current
      if (!list) return
      const target = event.target as Node | null
      if (!target || !dropdownRef.current?.contains(target)) return
      if (applyManualScroll(list, event.deltaX, event.deltaY)) {
        event.preventDefault()
      }
    }

    const handleTouchStart = (event: TouchEvent) => {
      const list = listRef.current
      if (!list) return
      const target = event.target as Node | null
      if (!target || !dropdownRef.current?.contains(target)) return
      const touch = event.touches[0]
      if (!touch) return
      touchStartRef.current = { x: touch.clientX, y: touch.clientY }
    }

    const handleTouchMove = (event: TouchEvent) => {
      const list = listRef.current
      const start = touchStartRef.current
      if (!list || !start) return
      const target = event.target as Node | null
      if (!target || !dropdownRef.current?.contains(target)) return
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
  }, [open, computePosition])

  const handleSelect = useCallback(
    (next: string) => {
      onValueChange(next)
      setOpen(false)
    },
    [onValueChange]
  )

  return (
    <div className={cn('relative w-full', className)}>
      <button
        id={id}
        aria-label={ariaLabel}
        ref={triggerRef}
        data-slot="select-trigger"
        type="button"
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        className={cn(
          'flex w-full items-center justify-between gap-2 border border-input bg-background text-left shadow-xs transition-[border-color,box-shadow,transform] outline-none',
          // Match Input (h-9) so form grids stay vertically aligned.
          compact ? 'h-8 rounded-md px-2.5 text-[13px]' : 'h-9 rounded-md px-3 text-sm',
          'hover:border-primary/30 hover:bg-accent/50',
          'focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/20',
          'disabled:cursor-not-allowed disabled:opacity-60',
          open && 'border-primary/35 ring-[3px] ring-primary/10',
          triggerClassName
        )}
        onClick={() => {
          if (!disabled) {
            setOpen((current) => !current)
          }
        }}
      >
        <span className={cn('min-w-0 flex-1', selectedOption?.triggerContent ? 'block' : 'truncate', selectedOption ? 'text-foreground' : 'text-muted-foreground')}>
          {selectedOption?.triggerContent ?? selectedOption?.triggerLabel ?? selectedOption?.label ?? placeholder}
        </span>
        <ChevronDown className={cn('size-4 shrink-0 text-muted-foreground transition-transform', open && 'rotate-180')} />
      </button>

      {open && position
        ? createPortal(
            <div
              ref={dropdownRef}
              data-select-dropdown="true"
              style={{
                position: 'fixed',
                top: position.openUp ? undefined : position.top,
                bottom: position.openUp ? window.innerHeight - position.top : undefined,
                left: position.left,
                width: position.width,
                // 高度约束交给内层可滚动列表单点控制，避免双重 maxHeight + flex 布局
                // 让内层 clientHeight/scrollHeight 计算出现歧义、导致 wheel 判空滚不动。
              }}
              className={cn(
                'pointer-events-auto z-[1000] overflow-hidden rounded-md border border-border bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm'
              )}
            >
              <div
                ref={listRef}
                className={cn(
                  'overscroll-contain overflow-x-hidden overflow-y-auto',
                  // 始终显示细滚动条，避免 macOS 叠加滚动条「看不见、以为不能滚」。
                  '[scrollbar-gutter:stable] [scrollbar-width:thin]',
                  '[&::-webkit-scrollbar]:w-1.5',
                  '[&::-webkit-scrollbar-track]:bg-transparent',
                  '[&::-webkit-scrollbar-thumb]:rounded-full',
                  '[&::-webkit-scrollbar-thumb]:bg-border',
                  '[&::-webkit-scrollbar-thumb:hover]:bg-muted-foreground/40',
                  compact ? 'p-1' : 'p-1.5',
                )}
                style={{ maxHeight: position.maxHeight }}
              >
                <div role="listbox" aria-activedescendant={value || undefined} className="space-y-0.5">
                  {options.map((option) => {
                    const isSelected = option.value === value
                    return (
                      <button
                        key={option.value}
                        id={option.value}
                        type="button"
                        role="option"
                        aria-label={option.label}
                        aria-selected={isSelected}
                        className={cn(
                          'flex w-full items-center justify-between gap-2 text-left transition-colors',
                          compact ? 'rounded-md px-2 py-1.5 text-[13px]' : 'rounded-md px-2.5 py-2 text-sm',
                          isSelected
                            ? 'bg-primary/10 text-primary'
                            : 'text-foreground hover:bg-accent/70 hover:text-accent-foreground'
                        )}
                        // 用 onPointerDown 在 target 阶段直接 commit 选择：
                        //  1. 早于 document 的 outside-pointerdown handler，避免 portal 边界
                        //     场景下 dropdown 被先关掉、click 永远收不到的竞态；
                        //  2. preventDefault 阻止 button 的默认 focus 转移，下拉关闭时焦点自然
                        //     回到 trigger，不会跳到无关元素。
                        onPointerDown={(event) => {
                          event.preventDefault()
                          handleSelect(option.value)
                        }}
                        // onClick 兜底：键盘 Enter / Space 触发的合成 click 没有 pointerdown。
                        onClick={() => handleSelect(option.value)}
                      >
                        <span className={cn('min-w-0 flex-1', option.content ? 'block' : 'truncate')}>{option.content ?? option.label}</span>
                        <Check className={cn('size-4 shrink-0', isSelected ? 'opacity-100' : 'opacity-0')} />
                      </button>
                    )
                  })}
                </div>
              </div>
            </div>,
            document.body
          )
        : null}
    </div>
  )
}
