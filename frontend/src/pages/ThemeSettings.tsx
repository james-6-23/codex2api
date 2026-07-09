import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react'
import { useMemo, useState } from 'react'
import { Check, Monitor, Moon, Palette, RotateCcw, Sun } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import PageHeader from '../components/PageHeader'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  COLOR_THEMES,
  THEME_GROUP_ORDER,
  getThemePreviewSwatch,
  type ColorThemeDef,
  type Theme,
  type ThemeGroup,
  type ThemeMode,
  useTheme,
} from '../hooks/useTheme'
import { cn } from '@/lib/utils'

type ModeOption = {
  id: ThemeMode
  icon: ReactNode
  labelKey: string
  descriptionKey: string
}

const modeOptions: ModeOption[] = [
  {
    id: 'light',
    icon: <Sun className="size-4" />,
    labelKey: 'themeSettings.modeLight',
    descriptionKey: 'themeSettings.modeLightDesc',
  },
  {
    id: 'dark',
    icon: <Moon className="size-4" />,
    labelKey: 'themeSettings.modeDark',
    descriptionKey: 'themeSettings.modeDarkDesc',
  },
  {
    id: 'system',
    icon: <Monitor className="size-4" />,
    labelKey: 'themeSettings.modeSystem',
    descriptionKey: 'themeSettings.modeSystemDesc',
  },
]

const GROUP_FILTERS: Array<{ id: 'all' | ThemeGroup; labelKey: string }> = [
  { id: 'all', labelKey: 'themeSettings.filterAll' },
  { id: 'recommended', labelKey: 'themeSettings.filterRecommended' },
  { id: 'light', labelKey: 'themeSettings.filterLight' },
  { id: 'dark', labelKey: 'themeSettings.filterDark' },
  { id: 'editor', labelKey: 'themeSettings.filterEditor' },
]

function ThemePreviewCard({ compact = false }: { compact?: boolean }) {
  const { t } = useTranslation()

  return (
    <Card className="min-w-0 py-0">
      <CardContent className={cn('min-w-0', compact ? 'p-3.5' : 'p-5')}>
        <div className={cn(compact ? 'mb-3' : 'mb-4')}>
          <h3 className="text-base font-semibold leading-tight text-foreground">
            {t('themeSettings.previewTitle')}
          </h3>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {t('themeSettings.previewDesc')}
          </p>
        </div>
        <div className="overflow-hidden rounded-xl border border-border bg-background shadow-sm">
          <div className={cn('flex min-w-0', compact ? 'min-h-[140px]' : 'min-h-[220px]')}>
            <div
              className={cn(
                'shrink-0 border-r border-border bg-[hsl(var(--sidebar-background))] p-3',
                compact ? 'w-[72px]' : 'w-[132px] max-sm:w-[88px]',
              )}
            >
              <div className="mb-3 flex items-center gap-2">
                <span className="size-7 rounded-lg bg-primary/15 ring-1 ring-primary/20" />
                {!compact ? (
                  <span className="h-3 w-14 rounded-full bg-foreground/16 max-sm:hidden" />
                ) : null}
              </div>
              <div className="space-y-2">
                <span className="block h-7 rounded-lg bg-primary/12 ring-1 ring-primary/20" />
                <span className="block h-7 rounded-lg bg-muted/70" />
                {!compact ? <span className="block h-7 rounded-lg bg-muted/50" /> : null}
              </div>
            </div>
            <div className={cn('min-w-0 flex-1', compact ? 'p-3' : 'p-4')}>
              <div className="mb-3 flex items-start justify-between gap-3">
                <div className="min-w-0 flex-1">
                  <span className="mb-2 block h-4 w-28 max-w-full rounded-full bg-foreground/18 sm:w-36" />
                  <span className="block h-3 w-40 max-w-full rounded-full bg-muted sm:w-56" />
                </div>
                {!compact ? (
                  <Button size="sm" className="max-sm:hidden">
                    <Palette className="size-3.5" />
                    {t('themeSettings.previewAction')}
                  </Button>
                ) : null}
              </div>
              <div className={cn('grid gap-2', compact ? 'grid-cols-2' : 'grid-cols-2 sm:grid-cols-3')}>
                <div className="min-h-[64px] rounded-lg border border-border bg-card p-2.5 sm:min-h-[74px] sm:p-3">
                  <span className="mb-2 block h-2.5 w-14 rounded-full bg-muted" />
                  <span className="block h-4 w-16 rounded-full bg-primary/18" />
                </div>
                <div className="min-h-[64px] rounded-lg border border-border bg-card p-2.5 sm:min-h-[74px] sm:p-3">
                  <span className="mb-2 block h-2.5 w-16 rounded-full bg-muted" />
                  <span className="block h-4 w-12 rounded-full bg-emerald-500/18" />
                </div>
                {!compact ? (
                  <div className="min-h-[64px] rounded-lg border border-border bg-card p-2.5 max-sm:hidden sm:min-h-[74px] sm:p-3">
                    <span className="mb-2 block h-2.5 w-12 rounded-full bg-muted" />
                    <span className="block h-4 w-20 rounded-full bg-amber-500/18" />
                  </div>
                ) : null}
              </div>
              {!compact ? (
                <div className="mt-3 overflow-hidden rounded-lg border border-border bg-card max-sm:hidden">
                  {[0, 1, 2].map((row) => (
                    <div
                      key={row}
                      className="grid grid-cols-[1fr_72px_56px] items-center gap-3 border-b border-border px-3 py-2.5 last:border-b-0"
                    >
                      <span className="h-3 rounded-full bg-muted" />
                      <span className="h-3 rounded-full bg-muted/80" />
                      <span className={cn('h-5 rounded-full', row === 0 ? 'bg-primary/18' : 'bg-muted/70')} />
                    </div>
                  ))}
                </div>
              ) : null}
            </div>
          </div>
        </div>
      </CardContent>
    </Card>
  )
}

function ThemeStyleCard({
  item,
  active,
  resolvedMode,
  onSelect,
}: {
  item: ColorThemeDef
  active: boolean
  resolvedMode: Theme
  onSelect: () => void
}) {
  const { t } = useTranslation()
  const swatch = getThemePreviewSwatch(item, resolvedMode)

  return (
    <button
      type="button"
      role="option"
      aria-selected={active}
      aria-pressed={active}
      onClick={onSelect}
      className={cn(
        'group min-w-0 rounded-xl border bg-card p-3 text-left shadow-sm outline-none transition-all hover:-translate-y-0.5 hover:border-primary/40 hover:shadow-md focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/35',
        active ? 'border-primary/55 ring-2 ring-primary/20' : 'border-border',
      )}
    >
      <div
        className="relative aspect-[4/3] overflow-hidden rounded-lg border shadow-inner sm:aspect-[16/10]"
        style={{ backgroundColor: swatch.bg, borderColor: swatch.muted }}
      >
        <div
          className="absolute inset-y-0 left-0 w-[28%] border-r"
          style={{ backgroundColor: swatch.surface, borderColor: swatch.muted }}
        >
          <span className="mx-2 mt-2 block h-4 rounded-md" style={{ backgroundColor: swatch.primary }} />
          <span className="mx-2 mt-2 block h-2 rounded-full" style={{ backgroundColor: swatch.muted }} />
          <span className="mx-2 mt-1.5 block h-2 rounded-full" style={{ backgroundColor: swatch.muted }} />
        </div>
        <div className="ml-[28%] p-2.5">
          <span className="mb-2 block h-3 w-16 rounded-full sm:w-20" style={{ backgroundColor: swatch.primary }} />
          <div className="grid grid-cols-2 gap-1.5">
            <span className="h-7 rounded-md sm:h-8" style={{ backgroundColor: swatch.surface }} />
            <span className="h-7 rounded-md sm:h-8" style={{ backgroundColor: swatch.muted }} />
          </div>
          <span className="mt-2 block h-2 rounded-full" style={{ backgroundColor: swatch.muted }} />
          <span className="mt-1.5 block h-2 w-2/3 rounded-full" style={{ backgroundColor: swatch.muted }} />
        </div>
        {item.recommended ? (
          <span className="absolute left-2 top-2 rounded-md bg-background/90 px-1.5 py-0.5 text-[10px] font-bold text-foreground shadow-sm ring-1 ring-border">
            {t('themeSettings.recommendedBadge')}
          </span>
        ) : null}
        {active ? (
          <span className="absolute right-2 top-2 inline-flex size-6 items-center justify-center rounded-full bg-primary text-primary-foreground shadow-sm">
            <Check className="size-3.5" />
          </span>
        ) : null}
      </div>
      <div className="mt-3 flex min-w-0 items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold text-foreground" title={t(item.nameKey)}>
            {t(item.nameKey)}
          </div>
          <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-muted-foreground" title={t(item.descriptionKey)}>
            {t(item.descriptionKey)}
          </p>
        </div>
        <div className="flex shrink-0 items-center pt-0.5">
          <span
            className="size-3.5 rounded-full border border-black/5 shadow-inner"
            style={{ backgroundColor: swatch.primary }}
          />
          <span
            className="-ml-1.5 size-3.5 rounded-full border border-black/5 shadow-inner"
            style={{ backgroundColor: swatch.bg }}
          />
        </div>
      </div>
    </button>
  )
}

export default function ThemeSettings() {
  const { t } = useTranslation()
  const {
    theme,
    themeMode,
    setThemeMode,
    colorTheme,
    setColorTheme,
    resetTheme,
  } = useTheme()
  const [groupFilter, setGroupFilter] = useState<'all' | ThemeGroup>('all')

  const activeColorTheme = COLOR_THEMES.find((item) => item.id === colorTheme) ?? COLOR_THEMES[0]
  const modeLabelKey =
    themeMode === 'system'
      ? 'themeSettings.modeSystem'
      : themeMode === 'dark'
        ? 'themeSettings.modeDark'
        : 'themeSettings.modeLight'

  const filteredThemes = useMemo(() => {
    if (groupFilter === 'all') return COLOR_THEMES
    if (groupFilter === 'recommended') {
      return COLOR_THEMES.filter((item) => item.recommended)
    }
    return COLOR_THEMES.filter((item) => item.group === groupFilter)
  }, [groupFilter])

  const groupedSections = useMemo(() => {
    if (groupFilter !== 'all') {
      return [{ group: groupFilter as ThemeGroup, items: filteredThemes }]
    }
    // All: recommended first, then remaining by primary group (without duplicating recommended).
    const recommended = COLOR_THEMES.filter((item) => item.recommended)
    const rest = THEME_GROUP_ORDER.filter((g) => g !== 'recommended').map((group) => ({
      group,
      items: COLOR_THEMES.filter((item) => item.group === group && !item.recommended),
    })).filter((section) => section.items.length > 0)
    return [
      { group: 'recommended' as ThemeGroup, items: recommended },
      ...rest,
    ]
  }, [filteredThemes, groupFilter])

  const handleModeChange = (nextMode: ThemeMode, event: ReactMouseEvent<HTMLButtonElement>) => {
    setThemeMode(nextMode, event)
  }

  const handleReset = () => {
    resetTheme()
    setGroupFilter('all')
  }

  return (
    <div className="mx-auto max-w-7xl">
      <PageHeader
        title={t('themeSettings.title')}
        description={t('themeSettings.description')}
        actions={(
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="inline-flex min-h-9 items-center gap-2 rounded-lg border border-border bg-card px-3 text-sm font-semibold text-foreground shadow-sm">
              <Palette className="size-4 text-primary" />
              {t('themeSettings.currentThemeFull', {
                theme: t(activeColorTheme.nameKey),
                mode: t(modeLabelKey),
              })}
            </span>
            <Button variant="outline" size="sm" onClick={handleReset} title={t('themeSettings.resetDesc')}>
              <RotateCcw className="size-3.5" />
              {t('themeSettings.reset')}
            </Button>
          </div>
        )}
      />

      <div className="grid items-start gap-4 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
        <Card className="min-w-0 py-0">
          <CardContent className="min-w-0 p-4 sm:p-5">
            <div>
              <h3 className="text-base font-semibold leading-tight text-foreground">
                {t('themeSettings.modeTitle')}
              </h3>
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                {t('themeSettings.modeDesc')}
              </p>
            </div>
            <div className="mt-4 grid min-w-0 gap-2 sm:grid-cols-3">
              {modeOptions.map((item) => {
                const active = themeMode === item.id
                return (
                  <button
                    key={item.id}
                    type="button"
                    aria-pressed={active}
                    onClick={(event) => handleModeChange(item.id, event)}
                    className={cn(
                      'flex min-h-[96px] min-w-0 flex-col items-start gap-2 rounded-xl border p-3 text-left outline-none transition-all hover:border-primary/40 hover:bg-muted/35 focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/35',
                      active ? 'border-primary/55 bg-primary/10 text-primary' : 'border-border bg-background text-foreground',
                    )}
                  >
                    <span
                      className={cn(
                        'inline-flex size-9 shrink-0 items-center justify-center rounded-lg',
                        active ? 'bg-primary text-primary-foreground' : 'bg-muted text-muted-foreground',
                      )}
                    >
                      {item.icon}
                    </span>
                    <span className="min-w-0">
                      <span className="flex items-center gap-1.5 text-sm font-semibold">
                        {t(item.labelKey)}
                        {active ? <Check className="size-3.5" /> : null}
                      </span>
                      <span className="mt-1 block text-[11px] leading-relaxed text-muted-foreground sm:text-xs">
                        {t(item.descriptionKey)}
                        {item.id === 'system' ? (
                          <span className="mt-0.5 block text-[10px] font-medium text-muted-foreground/90">
                            {t('themeSettings.systemResolved', {
                              mode: t(theme === 'dark' ? 'themeSettings.modeDark' : 'themeSettings.modeLight'),
                            })}
                          </span>
                        ) : null}
                      </span>
                    </span>
                  </button>
                )
              })}
            </div>
          </CardContent>
        </Card>

        {/* Mobile compact preview */}
        <div className="lg:hidden">
          <ThemePreviewCard compact />
        </div>

        {/* Desktop sticky live preview */}
        <div className="hidden lg:block">
          <div className="sticky top-4">
            <ThemePreviewCard />
          </div>
        </div>
      </div>

      <section className="mt-6">
        <div className="mb-4 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
          <div>
            <h3 className="text-lg font-semibold leading-tight text-foreground">
              {t('themeSettings.stylesTitle')}
            </h3>
            <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
              {t('themeSettings.stylesDesc')}
            </p>
          </div>
          <div
            className="flex max-w-full gap-1 overflow-x-auto rounded-xl border border-border bg-muted/30 p-1 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
            role="tablist"
            aria-label={t('themeSettings.stylesTitle')}
          >
            {GROUP_FILTERS.map((filter) => {
              const active = groupFilter === filter.id
              return (
                <button
                  key={filter.id}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  onClick={() => setGroupFilter(filter.id)}
                  className={cn(
                    'shrink-0 rounded-lg px-2.5 py-1.5 text-[12px] font-semibold transition-colors',
                    active
                      ? 'bg-background text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  {t(filter.labelKey)}
                </button>
              )
            })}
          </div>
        </div>

        <div className="space-y-6" role="listbox" aria-label={t('themeSettings.stylesTitle')}>
          {groupedSections.map((section) => (
            <div key={section.group}>
              {groupFilter === 'all' ? (
                <div className="mb-2.5 flex items-center gap-2">
                  <h4 className="text-sm font-semibold text-foreground">
                    {t(`themeSettings.group.${section.group}`)}
                  </h4>
                  <span className="text-xs text-muted-foreground">{section.items.length}</span>
                </div>
              ) : null}
              <div className="grid gap-3 grid-cols-1 min-[420px]:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
                {section.items.map((item) => (
                  <ThemeStyleCard
                    key={item.id}
                    item={item}
                    active={item.id === colorTheme}
                    resolvedMode={theme}
                    onSelect={() => setColorTheme(item.id)}
                  />
                ))}
              </div>
            </div>
          ))}
          {filteredThemes.length === 0 ? (
            <div className="rounded-xl border border-dashed border-border bg-card/60 px-4 py-10 text-center text-sm text-muted-foreground">
              {t('themeSettings.emptyFilter')}
            </div>
          ) : null}
        </div>
      </section>
    </div>
  )
}
