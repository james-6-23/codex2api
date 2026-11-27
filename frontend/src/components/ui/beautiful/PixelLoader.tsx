import { useEffect, useState } from 'react'
import { cn } from '@/lib/utils'
import { ShimmerText } from './ShimmerText'

export type PixelLoaderMode = 'drive' | 'orbit' | 'dots'

// Delay steps (in ms) for a 3x3 pixel grid inspired by beautifului.dev #01 Loading State.
const MODE_DELAYS: Record<PixelLoaderMode, readonly number[]> = {
  drive: [0, 90, 180, 90, 180, 270, 180, 270, 360],
  orbit: [0, 110, 220, 770, 999, 330, 660, 550, 440],
  dots: [0, 220, 440, 160, 380, 600, 320, 540, 760],
}

const SIZE_MAP = {
  sm: { grid: 'gap-[2px]', cell: 'size-[3.5px] rounded-[1px]', text: 'text-xs' },
  md: { grid: 'gap-[2.5px]', cell: 'size-[4.5px] rounded-[1.5px]', text: 'text-sm' },
  lg: { grid: 'gap-[3px]', cell: 'size-[6px] rounded-[2px]', text: 'text-base' },
} as const

export function PixelLoader({
  mode = 'drive',
  size = 'md',
  label,
  showElapsed = false,
  className,
  gridClassName,
}: {
  mode?: PixelLoaderMode
  size?: keyof typeof SIZE_MAP
  label?: string
  showElapsed?: boolean
  className?: string
  gridClassName?: string
}) {
  const [elapsedMs, setElapsedMs] = useState(0)

  useEffect(() => {
    if (!showElapsed) return
    const startedAt = performance.now()
    setElapsedMs(0)
    const timer = window.setInterval(() => {
      setElapsedMs(performance.now() - startedAt)
    }, 100)
    return () => window.clearInterval(timer)
  }, [showElapsed])

  const spec = SIZE_MAP[size]
  const delays = MODE_DELAYS[mode]

  return (
    <span
      className={cn('inline-flex items-center gap-2.5 text-primary', className)}
      role="status"
      aria-live="polite"
    >
      <span
        aria-hidden="true"
        className={cn('inline-grid shrink-0 grid-cols-3', spec.grid, gridClassName)}
      >
        {delays.map((delay, idx) => {
          const isCenterOrbit = mode === 'orbit' && idx === 4
          return (
            <span
              key={idx}
              className={cn(
                spec.cell,
                'bg-current',
                isCenterOrbit ? 'opacity-20' : 'bui-pixel-cell',
              )}
              style={isCenterOrbit ? undefined : { animationDelay: `${delay}ms` }}
            />
          )
        })}
      </span>
      {label ? (
        <ShimmerText className={cn('font-medium', spec.text)}>{label}</ShimmerText>
      ) : null}
      {showElapsed ? (
        <span className="font-mono text-[11px] tabular-nums text-muted-foreground">
          {(elapsedMs / 1000).toFixed(1)}s
        </span>
      ) : null}
    </span>
  )
}
