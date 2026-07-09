import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { MouseEvent, ReactNode } from 'react'

/** Resolved appearance actually applied to the document. */
export type Theme = 'light' | 'dark'
/** User preference: fixed light/dark, or follow OS. */
export type ThemeMode = 'light' | 'dark' | 'system'

export type ColorTheme =
  | 'default'
  | 'claude'
  | 'chatgpt'
  | 'deepseek'
  | 'graphite'
  | 'aurora'
  | 'rose'
  | 'mono'
  | 'one-dark-pro'
  | 'github-dimmed'
  | 'tokyo-night'
  | 'dracula'
  | 'monokai-pro'
  | 'nord'
  | 'catppuccin'
  | 'gruvbox'
  | 'solarized-light'
  | 'quiet-light'
  | 'ayu-light'
  | 'noctis-lux'

export type ThemeGroup = 'recommended' | 'light' | 'dark' | 'editor'

export interface ThemePreviewSwatch {
  primary: string
  bg: string
  surface: string
  muted: string
}

export interface ColorThemeDef {
  id: ColorTheme
  nameKey: string
  descriptionKey: string
  group: ThemeGroup
  recommended?: boolean
  previewLight: ThemePreviewSwatch
  previewDark: ThemePreviewSwatch
  /** @deprecated use previewLight — kept for callers that still read flat fields */
  previewPrimary: string
  previewBg: string
  previewSurface: string
  previewMuted: string
}

function def(
  partial: Omit<ColorThemeDef, 'previewPrimary' | 'previewBg' | 'previewSurface' | 'previewMuted'> & {
    previewLight: ThemePreviewSwatch
    previewDark: ThemePreviewSwatch
  },
): ColorThemeDef {
  return {
    ...partial,
    previewPrimary: partial.previewLight.primary,
    previewBg: partial.previewLight.bg,
    previewSurface: partial.previewLight.surface,
    previewMuted: partial.previewLight.muted,
  }
}

export const COLOR_THEMES: ColorThemeDef[] = [
  def({
    id: 'default',
    nameKey: 'common.theme.default',
    descriptionKey: 'themeSettings.themeDesc.default',
    group: 'light',
    recommended: true,
    previewLight: {
      primary: 'hsl(214 84% 46%)',
      bg: 'hsl(220 24% 96%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(214 22% 93%)',
    },
    previewDark: {
      primary: 'hsl(205 62% 54%)',
      bg: 'hsl(222 16% 14%)',
      surface: 'hsl(222 14% 17%)',
      muted: 'hsl(222 11% 24%)',
    },
  }),
  def({
    id: 'graphite',
    nameKey: 'common.theme.graphite',
    descriptionKey: 'themeSettings.themeDesc.graphite',
    group: 'light',
    recommended: true,
    previewLight: {
      primary: 'hsl(194 72% 38%)',
      bg: 'hsl(216 18% 96%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(216 12% 90%)',
    },
    previewDark: {
      primary: 'hsl(190 72% 48%)',
      bg: 'hsl(220 13% 10%)',
      surface: 'hsl(220 12% 14%)',
      muted: 'hsl(220 10% 21%)',
    },
  }),
  def({
    id: 'tokyo-night',
    nameKey: 'common.theme.tokyoNight',
    descriptionKey: 'themeSettings.themeDesc.tokyoNight',
    group: 'dark',
    recommended: true,
    previewLight: {
      primary: 'hsl(214 82% 55%)',
      bg: 'hsl(229 11% 95%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(225 19% 88%)',
    },
    previewDark: {
      primary: 'hsl(217 92% 73%)',
      bg: 'hsl(230 24% 16%)',
      surface: 'hsl(229 24% 19%)',
      muted: 'hsl(229 17% 28%)',
    },
  }),
  def({
    id: 'mono',
    nameKey: 'common.theme.mono',
    descriptionKey: 'themeSettings.themeDesc.mono',
    group: 'light',
    recommended: true,
    previewLight: {
      primary: 'hsl(222 10% 18%)',
      bg: 'hsl(0 0% 98%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(0 0% 91%)',
    },
    previewDark: {
      primary: 'hsl(0 0% 92%)',
      bg: 'hsl(0 0% 8%)',
      surface: 'hsl(0 0% 12%)',
      muted: 'hsl(0 0% 19%)',
    },
  }),
  def({
    id: 'claude',
    nameKey: 'common.theme.claude',
    descriptionKey: 'themeSettings.themeDesc.claude',
    group: 'light',
    previewLight: {
      primary: 'hsl(16 76% 50%)',
      bg: 'hsl(30 20% 97%)',
      surface: 'hsl(30 17% 95%)',
      muted: 'hsl(30 12% 91%)',
    },
    previewDark: {
      primary: 'hsl(16 80% 55%)',
      bg: 'hsl(30 12% 10%)',
      surface: 'hsl(30 10% 13%)',
      muted: 'hsl(30 8% 18%)',
    },
  }),
  def({
    id: 'chatgpt',
    nameKey: 'common.theme.chatgpt',
    descriptionKey: 'themeSettings.themeDesc.chatgpt',
    group: 'light',
    previewLight: {
      primary: 'hsl(160 84% 33%)',
      bg: 'hsl(0 0% 100%)',
      surface: 'hsl(0 0% 97%)',
      muted: 'hsl(0 0% 93%)',
    },
    previewDark: {
      primary: 'hsl(160 84% 35%)',
      bg: 'hsl(0 0% 13%)',
      surface: 'hsl(0 0% 9%)',
      muted: 'hsl(0 0% 18%)',
    },
  }),
  def({
    id: 'deepseek',
    nameKey: 'common.theme.deepseek',
    descriptionKey: 'themeSettings.themeDesc.deepseek',
    group: 'light',
    previewLight: {
      primary: 'hsl(220 100% 50%)',
      bg: 'hsl(214 30% 97%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(214 20% 92%)',
    },
    previewDark: {
      primary: 'hsl(217 91% 60%)',
      bg: 'hsl(222 47% 7%)',
      surface: 'hsl(222 33% 13%)',
      muted: 'hsl(222 25% 20%)',
    },
  }),
  def({
    id: 'aurora',
    nameKey: 'common.theme.aurora',
    descriptionKey: 'themeSettings.themeDesc.aurora',
    group: 'light',
    previewLight: {
      primary: 'hsl(173 78% 32%)',
      bg: 'hsl(166 33% 96%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(192 35% 90%)',
    },
    previewDark: {
      primary: 'hsl(168 78% 46%)',
      bg: 'hsl(198 42% 8%)',
      surface: 'hsl(198 31% 12%)',
      muted: 'hsl(198 24% 18%)',
    },
  }),
  def({
    id: 'rose',
    nameKey: 'common.theme.rose',
    descriptionKey: 'themeSettings.themeDesc.rose',
    group: 'light',
    previewLight: {
      primary: 'hsl(347 70% 48%)',
      bg: 'hsl(350 35% 97%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(342 28% 91%)',
    },
    previewDark: {
      primary: 'hsl(346 74% 61%)',
      bg: 'hsl(345 20% 10%)',
      surface: 'hsl(345 18% 13%)',
      muted: 'hsl(345 15% 20%)',
    },
  }),
  def({
    id: 'solarized-light',
    nameKey: 'common.theme.solarizedLight',
    descriptionKey: 'themeSettings.themeDesc.solarizedLight',
    group: 'light',
    previewLight: {
      primary: 'hsl(205 69% 49%)',
      bg: 'hsl(44 87% 94%)',
      surface: 'hsl(46 42% 88%)',
      muted: 'hsl(180 7% 80%)',
    },
    previewDark: {
      primary: 'hsl(205 69% 55%)',
      bg: 'hsl(192 100% 11%)',
      surface: 'hsl(192 81% 14%)',
      muted: 'hsl(192 60% 18%)',
    },
  }),
  def({
    id: 'quiet-light',
    nameKey: 'common.theme.quietLight',
    descriptionKey: 'themeSettings.themeDesc.quietLight',
    group: 'light',
    previewLight: {
      primary: 'hsl(283 35% 47%)',
      bg: 'hsl(0 0% 96%)',
      surface: 'hsl(0 0% 98%)',
      muted: 'hsl(0 0% 92%)',
    },
    previewDark: {
      primary: 'hsl(283 45% 65%)',
      bg: 'hsl(220 14% 14%)',
      surface: 'hsl(220 12% 17%)',
      muted: 'hsl(220 10% 22%)',
    },
  }),
  def({
    id: 'ayu-light',
    nameKey: 'common.theme.ayuLight',
    descriptionKey: 'themeSettings.themeDesc.ayuLight',
    group: 'light',
    previewLight: {
      primary: 'hsl(28 100% 56%)',
      bg: 'hsl(0 0% 98%)',
      surface: 'hsl(0 0% 95%)',
      muted: 'hsl(210 9% 90%)',
    },
    previewDark: {
      primary: 'hsl(28 100% 63%)',
      bg: 'hsl(223 21% 16%)',
      surface: 'hsl(220 19% 18%)',
      muted: 'hsl(220 13% 24%)',
    },
  }),
  def({
    id: 'noctis-lux',
    nameKey: 'common.theme.noctisLux',
    descriptionKey: 'themeSettings.themeDesc.noctisLux',
    group: 'light',
    previewLight: {
      primary: 'hsl(34 92% 44%)',
      bg: 'hsl(36 64% 88%)',
      surface: 'hsl(50 84% 93%)',
      muted: 'hsl(45 30% 84%)',
    },
    previewDark: {
      primary: 'hsl(34 92% 54%)',
      bg: 'hsl(189 70% 9%)',
      surface: 'hsl(190 100% 12%)',
      muted: 'hsl(190 60% 16%)',
    },
  }),
  def({
    id: 'one-dark-pro',
    nameKey: 'common.theme.oneDarkPro',
    descriptionKey: 'themeSettings.themeDesc.oneDarkPro',
    group: 'editor',
    previewLight: {
      primary: 'hsl(221 87% 60%)',
      bg: 'hsl(0 0% 98%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(240 7% 92%)',
    },
    previewDark: {
      primary: 'hsl(207 82% 66%)',
      bg: 'hsl(220 13% 18%)',
      surface: 'hsl(220 12% 15%)',
      muted: 'hsl(220 11% 22%)',
    },
  }),
  def({
    id: 'github-dimmed',
    nameKey: 'common.theme.githubDimmed',
    descriptionKey: 'themeSettings.themeDesc.githubDimmed',
    group: 'editor',
    previewLight: {
      primary: 'hsl(212 92% 45%)',
      bg: 'hsl(0 0% 100%)',
      surface: 'hsl(210 29% 97%)',
      muted: 'hsl(210 24% 93%)',
    },
    previewDark: {
      primary: 'hsl(212 89% 64%)',
      bg: 'hsl(213 13% 16%)',
      surface: 'hsl(216 13% 20%)',
      muted: 'hsl(213 11% 25%)',
    },
  }),
  def({
    id: 'dracula',
    nameKey: 'common.theme.dracula',
    descriptionKey: 'themeSettings.themeDesc.dracula',
    group: 'editor',
    previewLight: {
      primary: 'hsl(265 60% 55%)',
      bg: 'hsl(60 30% 96%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(264 14% 92%)',
    },
    previewDark: {
      primary: 'hsl(265 89% 78%)',
      bg: 'hsl(231 15% 18%)',
      surface: 'hsl(232 14% 23%)',
      muted: 'hsl(232 14% 31%)',
    },
  }),
  def({
    id: 'monokai-pro',
    nameKey: 'common.theme.monokaiPro',
    descriptionKey: 'themeSettings.themeDesc.monokaiPro',
    group: 'editor',
    previewLight: {
      primary: 'hsl(341 75% 58%)',
      bg: 'hsl(13 33% 96%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(20 28% 92%)',
    },
    previewDark: {
      primary: 'hsl(349 100% 70%)',
      bg: 'hsl(290 6% 17%)',
      surface: 'hsl(285 4% 22%)',
      muted: 'hsl(285 3% 30%)',
    },
  }),
  def({
    id: 'nord',
    nameKey: 'common.theme.nord',
    descriptionKey: 'themeSettings.themeDesc.nord',
    group: 'dark',
    previewLight: {
      primary: 'hsl(213 32% 52%)',
      bg: 'hsl(218 27% 94%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(218 27% 88%)',
    },
    previewDark: {
      primary: 'hsl(193 43% 67%)',
      bg: 'hsl(220 16% 22%)',
      surface: 'hsl(222 16% 28%)',
      muted: 'hsl(222 14% 31%)',
    },
  }),
  def({
    id: 'catppuccin',
    nameKey: 'common.theme.catppuccin',
    descriptionKey: 'themeSettings.themeDesc.catppuccin',
    group: 'dark',
    previewLight: {
      primary: 'hsl(266 85% 58%)',
      bg: 'hsl(220 23% 95%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(223 16% 88%)',
    },
    previewDark: {
      primary: 'hsl(267 84% 81%)',
      bg: 'hsl(240 21% 15%)',
      surface: 'hsl(240 21% 20%)',
      muted: 'hsl(234 13% 31%)',
    },
  }),
  def({
    id: 'gruvbox',
    nameKey: 'common.theme.gruvbox',
    descriptionKey: 'themeSettings.themeDesc.gruvbox',
    group: 'dark',
    previewLight: {
      primary: 'hsl(35 80% 39%)',
      bg: 'hsl(48 87% 88%)',
      surface: 'hsl(53 73% 91%)',
      muted: 'hsl(43 59% 81%)',
    },
    previewDark: {
      primary: 'hsl(40 78% 50%)',
      bg: 'hsl(0 0% 16%)',
      surface: 'hsl(20 6% 22%)',
      muted: 'hsl(15 8% 30%)',
    },
  }),
]

export const THEME_GROUP_ORDER: ThemeGroup[] = ['recommended', 'light', 'dark', 'editor']

export function getThemePreviewSwatch(item: ColorThemeDef, mode: Theme): ThemePreviewSwatch {
  return mode === 'dark' ? item.previewDark : item.previewLight
}

const STORAGE_KEY = 'theme'
const COLOR_THEME_STORAGE_KEY = 'color-theme'
const DEFAULT_COLOR_THEME: ColorTheme = 'default'
const DEFAULT_THEME_MODE: ThemeMode = 'system'

function isColorTheme(value: string | null): value is ColorTheme {
  return COLOR_THEMES.some((theme) => theme.id === value)
}

function isThemeMode(value: string | null): value is ThemeMode {
  return value === 'light' || value === 'dark' || value === 'system'
}

function getSystemTheme(): Theme {
  if (typeof window === 'undefined') return 'light'
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

function resolveTheme(mode: ThemeMode): Theme {
  return mode === 'system' ? getSystemTheme() : mode
}

function getInitialThemeMode(): ThemeMode {
  const stored = localStorage.getItem(STORAGE_KEY)
  if (isThemeMode(stored)) return stored
  // Legacy: only light/dark was stored — treat as explicit choice.
  if (stored === 'light' || stored === 'dark') return stored
  return DEFAULT_THEME_MODE
}

function getInitialColorTheme(): ColorTheme {
  const stored = localStorage.getItem(COLOR_THEME_STORAGE_KEY)
  if (isColorTheme(stored)) return stored
  return DEFAULT_COLOR_THEME
}

function applyResolvedTheme(nextTheme: Theme) {
  const root = document.documentElement
  if (nextTheme === 'dark') {
    root.classList.add('dark')
  } else {
    root.classList.remove('dark')
  }
}

function persistThemeMode(mode: ThemeMode) {
  localStorage.setItem(STORAGE_KEY, mode)
  applyResolvedTheme(resolveTheme(mode))
}

function persistColorTheme(nextColorTheme: ColorTheme) {
  const root = document.documentElement
  COLOR_THEMES.forEach((theme) => {
    root.classList.remove(`theme-${theme.id}`)
  })
  root.classList.add(`theme-${nextColorTheme}`)
  localStorage.setItem(COLOR_THEME_STORAGE_KEY, nextColorTheme)
}

interface ThemeContextValue {
  /** Resolved light/dark currently applied. */
  theme: Theme
  /** User preference including system. */
  themeMode: ThemeMode
  setTheme: (next: Theme, e?: MouseEvent) => void
  setThemeMode: (next: ThemeMode, e?: MouseEvent) => void
  toggle: (e?: MouseEvent) => void
  colorTheme: ColorTheme
  setColorTheme: (next: ColorTheme) => void
  resetTheme: () => void
}

const ThemeContext = createContext<ThemeContextValue | null>(null)

function runThemeTransition(e: MouseEvent | undefined, apply: () => void) {
  const root = document.documentElement
  const x = e?.clientX ?? 40
  const y = e?.clientY ?? window.innerHeight - 40
  const endRadius = Math.hypot(
    Math.max(x, window.innerWidth - x),
    Math.max(y, window.innerHeight - y),
  )

  if (document.startViewTransition) {
    const transition = document.startViewTransition(() => {
      apply()
    })
    transition.ready.then(() => {
      root.animate(
        {
          clipPath: [
            `circle(0px at ${x}px ${y}px)`,
            `circle(${endRadius}px at ${x}px ${y}px)`,
          ],
        },
        {
          duration: 500,
          easing: 'ease-out',
          pseudoElement: '::view-transition-new(root)',
        },
      )
    })
  } else {
    apply()
  }
}

// Provider：把主题状态提升到全局，避免 Layout 与 ThemeSettings 各持一份导致 UI 不同步。
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [themeMode, setThemeModeState] = useState<ThemeMode>(getInitialThemeMode)
  const [theme, setThemeState] = useState<Theme>(() => resolveTheme(getInitialThemeMode()))
  const [colorTheme, setColorThemeState] = useState<ColorTheme>(getInitialColorTheme)

  useEffect(() => {
    persistThemeMode(themeMode)
    setThemeState(resolveTheme(themeMode))
  }, [themeMode])

  useEffect(() => {
    persistColorTheme(colorTheme)
  }, [colorTheme])

  // Follow OS when themeMode === 'system'
  useEffect(() => {
    if (themeMode !== 'system') return
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = () => {
      const next = getSystemTheme()
      applyResolvedTheme(next)
      setThemeState(next)
    }
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [themeMode])

  const setThemeMode = useCallback((nextMode: ThemeMode, e?: MouseEvent) => {
    if (nextMode === themeMode) {
      // Still re-resolve system in case OS flipped while we thought we matched.
      if (nextMode === 'system') {
        const resolved = resolveTheme('system')
        if (resolved !== theme) {
          runThemeTransition(e, () => {
            applyResolvedTheme(resolved)
            setThemeState(resolved)
          })
        }
      }
      return
    }

    const nextResolved = resolveTheme(nextMode)
    const currentResolved = theme
    if (nextResolved === currentResolved) {
      localStorage.setItem(STORAGE_KEY, nextMode)
      setThemeModeState(nextMode)
      return
    }

    runThemeTransition(e, () => {
      localStorage.setItem(STORAGE_KEY, nextMode)
      applyResolvedTheme(nextResolved)
      setThemeModeState(nextMode)
      setThemeState(nextResolved)
    })
  }, [theme, themeMode])

  const setTheme = useCallback((nextTheme: Theme, e?: MouseEvent) => {
    setThemeMode(nextTheme, e)
  }, [setThemeMode])

  const setColorTheme = useCallback((nextColorTheme: ColorTheme) => {
    persistColorTheme(nextColorTheme)
    setColorThemeState(nextColorTheme)
  }, [])

  const toggle = useCallback((e?: MouseEvent) => {
    const nextTheme: Theme = theme === 'dark' ? 'light' : 'dark'
    setThemeMode(nextTheme, e)
  }, [setThemeMode, theme])

  const resetTheme = useCallback(() => {
    setThemeMode(DEFAULT_THEME_MODE)
    setColorTheme(DEFAULT_COLOR_THEME)
  }, [setColorTheme, setThemeMode])

  const value = useMemo<ThemeContextValue>(
    () => ({
      theme,
      themeMode,
      setTheme,
      setThemeMode,
      toggle,
      colorTheme,
      setColorTheme,
      resetTheme,
    }),
    [theme, themeMode, setTheme, setThemeMode, toggle, colorTheme, setColorTheme, resetTheme],
  )

  return (
    <ThemeContext.Provider value={value}>
      {children}
    </ThemeContext.Provider>
  )
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext)
  if (!ctx) {
    throw new Error('useTheme must be used within <ThemeProvider>')
  }
  return ctx
}
