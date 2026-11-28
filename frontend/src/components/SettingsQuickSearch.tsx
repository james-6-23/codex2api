import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CornerDownLeft, Search, X } from 'lucide-react'
import { cn } from '@/lib/utils'

export interface SettingsQuickJumpEntry<TTab extends string = string> {
  tab: TTab
  tabLabel: string
  sectionId: string
  title: string
  keywords: string
}

interface SettingsQuickSearchProps<TTab extends string> {
  entries: ReadonlyArray<SettingsQuickJumpEntry<TTab>>
  onSelect: (tab: TTab, sectionId: string) => void
  align?: 'left' | 'right'
  className?: string
}

export default function SettingsQuickSearch<TTab extends string>({
  entries,
  onSelect,
  align = 'left',
  className,
}: SettingsQuickSearchProps<TTab>) {
  const { t } = useTranslation()
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(0)
  const containerRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const trimmed = query.trim().toLowerCase()
  const matches = useMemo(() => {
    if (!trimmed) return entries.slice(0, 8)
    const tokens = trimmed.split(/\s+/).filter(Boolean)
    return entries.filter((item) => {
      const haystack = `${item.tabLabel} ${item.title} ${item.keywords}`.toLowerCase()
      return tokens.every((tok) => haystack.includes(tok))
    }).slice(0, 10)
  }, [entries, trimmed])

  useEffect(() => {
    setActiveIndex(0)
  }, [trimmed])

  // 按 `/` 快速聚焦搜索框（仅在非输入控件聚焦时触发）
  useEffect(() => {
    const handleSlash = (event: KeyboardEvent) => {
      if (event.key !== '/' || event.metaKey || event.ctrlKey || event.altKey) return
      const target = event.target as HTMLElement | null
      if (
        target &&
        (target.tagName === 'INPUT' ||
          target.tagName === 'TEXTAREA' ||
          target.tagName === 'SELECT' ||
          target.isContentEditable)
      ) {
        return
      }
      event.preventDefault()
      inputRef.current?.focus()
      setOpen(true)
    }
    window.addEventListener('keydown', handleSlash)
    return () => window.removeEventListener('keydown', handleSlash)
  }, [])

  useEffect(() => {
    if (!open) return
    const handleOutside = (event: PointerEvent) => {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setOpen(false)
      }
    }
    document.addEventListener('pointerdown', handleOutside)
    return () => document.removeEventListener('pointerdown', handleOutside)
  }, [open])

  const commit = (item: SettingsQuickJumpEntry<TTab>) => {
    onSelect(item.tab, item.sectionId)
    setOpen(false)
    setQuery('')
  }

  return (
    <div ref={containerRef} className={cn('relative w-full', className)}>
      <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground/75" aria-hidden="true" />
      <input
        ref={inputRef}
        type="text"
        value={query}
        onFocus={() => setOpen(true)}
        onChange={(e) => {
          setQuery(e.target.value)
          setOpen(true)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            setOpen(false)
            inputRef.current?.blur()
            return
          }
          if (!open || matches.length === 0) return
          if (e.key === 'ArrowDown') {
            e.preventDefault()
            setActiveIndex((idx) => (idx + 1) % matches.length)
          } else if (e.key === 'ArrowUp') {
            e.preventDefault()
            setActiveIndex((idx) => (idx - 1 + matches.length) % matches.length)
          } else if (e.key === 'Enter') {
            e.preventDefault()
            const target = matches[activeIndex] ?? matches[0]
            if (target) commit(target)
          }
        }}
        placeholder={t('settings.quickSearchPlaceholder')}
        aria-label={t('settings.quickSearchPlaceholder')}
        aria-expanded={open}
        className="h-9 w-full rounded-xl border border-border/70 bg-muted/35 pl-8 pr-8 text-xs text-foreground outline-none transition-colors placeholder:text-muted-foreground/70 focus:border-primary/45 focus:bg-background focus:ring-2 focus:ring-primary/15"
      />
      {query ? (
        <button
          type="button"
          onClick={() => {
            setQuery('')
            inputRef.current?.focus()
          }}
          aria-label={t('common.clear', { defaultValue: 'Clear' })}
          className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-0.5 text-muted-foreground hover:text-foreground"
        >
          <X className="size-3" />
        </button>
      ) : !open ? (
        <kbd
          aria-hidden="true"
          className="pointer-events-none absolute right-2 top-1/2 hidden h-5 min-w-5 -translate-y-1/2 items-center justify-center rounded-md border border-border/75 bg-background/85 px-1 font-mono text-[10px] font-medium text-muted-foreground/80 shadow-2xs sm:inline-flex"
        >
          /
        </kbd>
      ) : null}

      {open ? (
        <div
          className={cn(
            'absolute top-full z-50 mt-2 w-full min-w-[300px] overflow-hidden rounded-xl border border-border/80 bg-popover p-1.5 text-popover-foreground shadow-xl backdrop-blur-md',
            align === 'right' ? 'right-0' : 'left-0',
          )}
        >
          {matches.length === 0 ? (
            <div className="px-3 py-4 text-center text-xs text-muted-foreground">
              {t('settings.quickSearchEmpty')}
            </div>
          ) : (
            <div className="max-h-72 space-y-0.5 overflow-y-auto">
              {matches.map((item, idx) => (
                <button
                  key={`${item.tab}:${item.sectionId}:${item.title}`}
                  type="button"
                  onMouseEnter={() => setActiveIndex(idx)}
                  onClick={() => commit(item)}
                  className={cn(
                    'flex w-full items-center justify-between gap-2 rounded-lg px-2.5 py-2 text-left text-xs transition-colors',
                    idx === activeIndex
                      ? 'bg-primary/10 text-foreground'
                      : 'text-foreground/90 hover:bg-muted/60',
                  )}
                >
                  <div className="min-w-0">
                    <div className="truncate font-medium">{item.title}</div>
                    <div className="truncate text-[10.5px] text-muted-foreground">{item.tabLabel}</div>
                  </div>
                  <CornerDownLeft
                    className={cn(
                      'size-3 shrink-0 transition-opacity',
                      idx === activeIndex ? 'text-primary opacity-100' : 'opacity-0',
                    )}
                    aria-hidden="true"
                  />
                </button>
              ))}
            </div>
          )}
          <div className="mt-1 border-t border-border/50 px-2.5 pt-1.5 pb-0.5 text-[10.5px] text-muted-foreground/80">
            {t('settings.quickSearchHint')}
          </div>
        </div>
      ) : null}
    </div>
  )
}
