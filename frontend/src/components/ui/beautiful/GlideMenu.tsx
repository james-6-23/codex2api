import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { cn } from '@/lib/utils'

interface GlideIndicator {
  top: number
  left: number
  width: number
  height: number
  visible: boolean
}

/**
 * Tactile gliding hover highlight inspired by beautifului.dev GlideMenu.
 * Mark interactive children with `data-glide-item="true"` and keep them `relative z-10`.
 */
export function GlideMenu({
  children,
  className,
  indicatorClassName,
  itemSelector = '[data-glide-item="true"]',
  as: Component = 'div',
  'aria-label': ariaLabel,
}: {
  children: ReactNode
  className?: string
  indicatorClassName?: string
  itemSelector?: string
  as?: 'div' | 'nav'
  'aria-label'?: string
}) {
  const containerRef = useRef<HTMLDivElement | null>(null)
  const activeElRef = useRef<HTMLElement | null>(null)
  const [indicator, setIndicator] = useState<GlideIndicator>({
    top: 0,
    left: 0,
    width: 0,
    height: 0,
    visible: false,
  })

  const syncTarget = useCallback((target: HTMLElement | null) => {
    const container = containerRef.current
    if (!container || !target || !container.contains(target)) {
      activeElRef.current = null
      setIndicator((prev) => (prev.visible ? { ...prev, visible: false } : prev))
      return
    }
    const containerRect = container.getBoundingClientRect()
    const targetRect = target.getBoundingClientRect()
    activeElRef.current = target
    setIndicator({
      top: targetRect.top - containerRect.top + container.scrollTop,
      left: targetRect.left - containerRect.left + container.scrollLeft,
      width: targetRect.width,
      height: targetRect.height,
      visible: true,
    })
  }, [])

  const handlePointerOver = useCallback(
    (event: React.PointerEvent<HTMLElement>) => {
      const target = (event.target as HTMLElement | null)?.closest<HTMLElement>(itemSelector) ?? null
      if (target) syncTarget(target)
    },
    [itemSelector, syncTarget],
  )

  const handleFocusCapture = useCallback(
    (event: React.FocusEvent<HTMLElement>) => {
      const target = (event.target as HTMLElement | null)?.closest<HTMLElement>(itemSelector) ?? null
      if (target) syncTarget(target)
    },
    [itemSelector, syncTarget],
  )

  const handleLeave = useCallback(() => {
    activeElRef.current = null
    setIndicator((prev) => (prev.visible ? { ...prev, visible: false } : prev))
  }, [])

  useEffect(() => {
    const container = containerRef.current
    if (!container || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => {
      if (activeElRef.current) syncTarget(activeElRef.current)
    })
    ro.observe(container)
    return () => ro.disconnect()
  }, [syncTarget])

  return (
    <Component
      ref={containerRef as React.RefObject<never>}
      aria-label={ariaLabel}
      onPointerOver={handlePointerOver}
      onPointerLeave={handleLeave}
      onFocusCapture={handleFocusCapture}
      onBlurCapture={(event) => {
        if (!containerRef.current?.contains(event.relatedTarget as Node | null)) {
          handleLeave()
        }
      }}
      className={cn('relative', className)}
    >
      <div
        aria-hidden="true"
        className={cn(
          'pointer-events-none absolute top-0 left-0 z-0 rounded-lg bg-muted/75 transition-[transform,width,height,opacity] duration-200 ease-[cubic-bezier(0.23,1,0.32,1)] dark:bg-white/[0.065]',
          indicatorClassName,
        )}
        style={{
          transform: `translate3d(${indicator.left}px, ${indicator.top}px, 0)`,
          width: `${indicator.width}px`,
          height: `${indicator.height}px`,
          opacity: indicator.visible ? 1 : 0,
        }}
      />
      {children}
    </Component>
  )
}
