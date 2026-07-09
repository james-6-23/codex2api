import { Check, ChevronDown } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { cn } from '@/lib/utils'

export interface SelectOption {
  label: string
  value: string
  triggerLabel?: string
}

interface SelectProps {
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
const DROPDOWN_MAX_HEIGHT = 288
const VIEWPORT_PADDING = 8

export function Select({
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
  const selectedOption = options.find((option) => option.value === value)

  const computePosition = useCallback(() => {
    const trigger = triggerRef.current
    if (!trigger) return
    const rect = trigger.getBoundingClientRect()
    const viewportHeight = window.innerHeight
    const viewportWidth = window.innerWidth
    const spaceBelow = viewportHeight - rect.bottom - DROPDOWN_GAP - VIEWPORT_PADDING
    const spaceAbove = rect.top - DROPDOWN_GAP - VIEWPORT_PADDING
    const openUp = spaceBelow < Math.min(DROPDOWN_MAX_HEIGHT, 160) && spaceAbove > spaceBelow
    const maxHeight = Math.max(120, Math.min(DROPDOWN_MAX_HEIGHT, openUp ? spaceAbove : spaceBelow))
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

    const handleReposition = () => computePosition()

    document.addEventListener('pointerdown', handlePointerDown)
    document.addEventListener('keydown', handleEscape)
    window.addEventListener('resize', handleReposition)
    window.addEventListener('scroll', handleReposition, true)

    return () => {
      document.removeEventListener('pointerdown', handlePointerDown)
      document.removeEventListener('keydown', handleEscape)
      window.removeEventListener('resize', handleReposition)
      window.removeEventListener('scroll', handleReposition, true)
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
        <span className={cn('truncate', selectedOption ? 'text-foreground' : 'text-muted-foreground')}>
          {selectedOption?.triggerLabel ?? selectedOption?.label ?? placeholder}
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
              }}
              className={cn(
                'pointer-events-auto z-[1000] overflow-hidden rounded-md border border-border bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm'
              )}
            >
              <div
                className={cn('overflow-auto', compact ? 'p-1' : 'p-1.5')}
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
                        <span className="truncate">{option.label}</span>
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
