import { useEffect, useRef, useState } from 'react'
import { ChevronRight } from 'lucide-react'

export type DocsTOCItem = {
  id: string
  label: string
  children?: { id: string; label: string; method?: string }[]
}

type DocsTOCProps = {
  items: DocsTOCItem[]
  title: string
}

const METHOD_COLOR: Record<string, string> = {
  GET: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400',
  POST: 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400',
  PATCH: 'bg-violet-100 text-violet-700 dark:bg-violet-900/30 dark:text-violet-300',
  PUT: 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400',
  DELETE: 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400',
}

export default function DocsTOC({ items, title }: DocsTOCProps) {
  const [activeId, setActiveId] = useState<string>(items[0]?.children?.[0]?.id ?? items[0]?.id ?? '')
  const [expandedParent, setExpandedParent] = useState<string>(items[0]?.id ?? '')
  const userToggledRef = useRef(false)

  useEffect(() => {
    const allIds: string[] = []
    items.forEach((parent) => {
      allIds.push(parent.id)
      parent.children?.forEach((c) => allIds.push(c.id))
    })

    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => (a.boundingClientRect.top - b.boundingClientRect.top))
        if (visible.length > 0) {
          const topId = visible[0].target.id
          setActiveId(topId)
          if (!userToggledRef.current) {
            const parent = items.find(
              (p) => p.id === topId || p.children?.some((c) => c.id === topId)
            )
            if (parent) setExpandedParent(parent.id)
          }
        }
      },
      { rootMargin: '-72px 0px -65% 0px', threshold: 0.1 }
    )

    for (const id of allIds) {
      const el = document.getElementById(id)
      if (el) observer.observe(el)
    }
    return () => observer.disconnect()
  }, [items])

  const scrollTo = (id: string) => {
    setActiveId(id)
    const el = document.getElementById(id)
    if (el) el.scrollIntoView({ behavior: 'smooth' })
  }

  const toggleParent = (parentId: string) => {
    userToggledRef.current = true
    setExpandedParent((current) => (current === parentId ? '' : parentId))
    window.setTimeout(() => { userToggledRef.current = false }, 600)
  }

  return (
    <div className="flex h-full max-h-full flex-col overflow-hidden rounded-lg border border-border bg-card/90 p-3 shadow-sm backdrop-blur-sm">
        <div className="mb-2 shrink-0 px-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          {title}
        </div>
        <nav className="min-h-0 flex-1 space-y-0.5 overflow-y-auto overscroll-contain pr-1 [-ms-overflow-style:none] [scrollbar-width:thin]">
          {items.map((parent) => {
            const isExpanded = expandedParent === parent.id
            const isParentActive = activeId === parent.id
            const hasActiveChild = parent.children?.some((c) => c.id === activeId)
            return (
              <div key={parent.id}>
                <button
                  onClick={() => {
                    if (parent.children && parent.children.length > 0) {
                      toggleParent(parent.id)
                      if (!isExpanded) scrollTo(parent.children[0].id)
                    } else {
                      scrollTo(parent.id)
                    }
                  }}
                  className={`flex w-full items-center gap-1.5 rounded-md border px-2.5 py-2 text-left text-[13px] font-semibold transition-all duration-200 hover:bg-muted/60 ${
                    isParentActive || (hasActiveChild && !isExpanded)
                      ? 'border-primary/25 bg-primary/10 text-primary'
                      : 'border-transparent text-foreground hover:text-foreground'
                  }`}
                >
                  {parent.children && parent.children.length > 0 ? (
                    <ChevronRight
                      className={`size-3.5 shrink-0 text-muted-foreground transition-transform duration-300 ${
                        isExpanded ? 'rotate-90' : 'rotate-0'
                      }`}
                    />
                  ) : (
                    <span className="size-3.5 shrink-0" />
                  )}
                  <span className="min-w-0 flex-1 truncate">{parent.label}</span>
                </button>

                {parent.children && parent.children.length > 0 && (
                  <div
                    className="grid transition-[grid-template-rows] duration-300 ease-in-out"
                    style={{ gridTemplateRows: isExpanded ? '1fr' : '0fr' }}
                  >
                    <div className="overflow-hidden">
                      <div className="ml-3 mt-0.5 space-y-0.5 border-l border-border pl-2 pb-1">
                        {parent.children.map((child) => {
                          const isActive = activeId === child.id
                          return (
                            <button
                              key={child.id}
                              onClick={() => scrollTo(child.id)}
                              className={`flex w-full items-center gap-1.5 rounded-md px-2 py-1.5 text-left text-[12px] font-medium transition-colors duration-200 hover:bg-muted/60 ${
                                isActive
                                  ? 'bg-primary/10 text-primary'
                                  : 'text-muted-foreground hover:text-foreground'
                              }`}
                            >
                              {child.method && (
                                <span
                                  className={`inline-flex shrink-0 items-center rounded px-1 py-px text-[9px] font-bold ${
                                    METHOD_COLOR[child.method] || 'bg-muted text-foreground'
                                  }`}
                                >
                                  {child.method}
                                </span>
                              )}
                              <span className="min-w-0 truncate">{child.label}</span>
                            </button>
                          )
                        })}
                      </div>
                    </div>
                  </div>
                )}
              </div>
            )
          })}
        </nav>
    </div>
  )
}
