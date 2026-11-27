import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { MouseEvent, ReactNode } from 'react'

/** Resolved appearance actually applied to the document. */
export type Theme = 'light' | 'dark'
/** User preference: fixed light/dark, or follow OS. */
export type ThemeMode = 'light' | 'dark' | 'system'

export type WebsiteThemeStyle = 'studio' | 'github' | 'vercel' | 'google' | 'apple' | 'notion' | 'stripe' | 'spotify' | 'slack' | 'glass'

export type ColorTheme =
  | WebsiteThemeStyle
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
  | 'ocean'
  | 'forest'
  | 'lavender'
  | 'amber'
  | 'espresso'
  | 'sakura'
  | 'mint'
  | 'midnight'

export type ThemeGroup = 'recommended' | 'website' | 'light' | 'dark' | 'editor'

export interface ThemePreviewSwatch {
  sidebar?: string
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
  uiStyle?: WebsiteThemeStyle
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
    id: 'studio',
    nameKey: 'common.theme.studio',
    descriptionKey: 'themeSettings.themeDesc.studio',
    group: 'website',
    uiStyle: 'studio',
    recommended: true,
    previewLight: {
      primary: 'oklch(0.585 0.205 254.947)',
      bg: 'oklch(0.966 0.002 252)',
      surface: 'oklch(1 0 0)',
      muted: 'oklch(0.946 0.003 252)',
      sidebar: 'hsl(240 20% 99%)',
    },
    previewDark: {
      primary: 'oklch(0.68 0.173 253.301)',
      bg: 'oklch(0.195 0.004 264.5)',
      surface: 'oklch(0.238 0.005 268)',
      muted: 'oklch(0.275 0.006 266)',
      sidebar: 'hsl(240 8% 12%)',
    },
  }),
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
    id: 'glass',
    nameKey: 'common.theme.glass',
    descriptionKey: 'themeSettings.themeDesc.glass',
    group: 'light',
    recommended: true,
    uiStyle: 'glass',
    previewLight: {
      primary: 'hsl(32 92% 48%)',
      bg: 'hsl(210 33% 96%)',
      surface: 'hsl(0 0% 100%)',
      muted: 'hsl(214 22% 90%)',
      sidebar: 'hsl(210 40% 98%)',
    },
    previewDark: {
      primary: 'hsl(43 96% 76%)',
      bg: 'hsl(222 47% 6%)',
      surface: 'hsl(217 33% 14%)',
      muted: 'hsl(215 19% 27%)',
      sidebar: 'hsl(222 41% 8%)',
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
    id: 'github',
    nameKey: 'common.theme.github',
    descriptionKey: 'themeSettings.themeDesc.github',
    group: 'website',
    uiStyle: 'github',
    recommended: true,
    previewLight: {
      primary: '#1f883d',
      bg: '#f6f8fa',
      surface: '#ffffff',
      muted: '#eaeef2',
      sidebar: '#f6f8fa',
    },
    previewDark: {
      primary: '#3fb950',
      bg: '#0d1117',
      surface: '#161b22',
      muted: '#21262d',
      sidebar: '#0d1117',
    },
  }),
  def({
    id: 'vercel',
    nameKey: 'common.theme.vercel',
    descriptionKey: 'themeSettings.themeDesc.vercel',
    group: 'website',
    uiStyle: 'vercel',
    recommended: true,
    previewLight: {
      primary: '#171717',
      bg: '#fafafa',
      surface: '#ffffff',
      muted: '#ededed',
      sidebar: '#ffffff',
    },
    previewDark: {
      primary: '#ededed',
      bg: '#000000',
      surface: '#0a0a0a',
      muted: '#1a1a1a',
      sidebar: '#000000',
    },
  }),
  def({
    id: 'google',
    nameKey: 'common.theme.google',
    descriptionKey: 'themeSettings.themeDesc.google',
    group: 'website',
    uiStyle: 'google',
    previewLight: {
      primary: '#0b57d0',
      bg: '#f8fafd',
      surface: '#ffffff',
      muted: '#e9eef6',
      sidebar: '#edf2fa',
    },
    previewDark: {
      primary: '#a8c7fa',
      bg: '#131314',
      surface: '#1e1f20',
      muted: '#282a2c',
      sidebar: '#1a1b1e',
    },
  }),
  def({
    id: 'apple',
    nameKey: 'common.theme.apple',
    descriptionKey: 'themeSettings.themeDesc.apple',
    group: 'website',
    uiStyle: 'apple',
    previewLight: {
      primary: '#0071e3',
      bg: '#f5f5f7',
      surface: '#ffffff',
      muted: '#eaeaec',
      sidebar: '#f5f5f7',
    },
    previewDark: {
      primary: '#2997ff',
      bg: '#000000',
      surface: '#1d1d1f',
      muted: '#2b2b2d',
      sidebar: '#111113',
    },
  }),
  def({
    id: 'notion',
    nameKey: 'common.theme.notion',
    descriptionKey: 'themeSettings.themeDesc.notion',
    group: 'website',
    uiStyle: 'notion',
    previewLight: {
      primary: '#37352f',
      bg: '#ffffff',
      surface: '#ffffff',
      muted: '#f1f1ef',
      sidebar: '#f7f7f5',
    },
    previewDark: {
      primary: '#e3e2de',
      bg: '#191919',
      surface: '#202020',
      muted: '#2c2c2c',
      sidebar: '#202020',
    },
  }),
  def({
    id: 'stripe',
    nameKey: 'common.theme.stripe',
    descriptionKey: 'themeSettings.themeDesc.stripe',
    group: 'website',
    uiStyle: 'stripe',
    previewLight: {
      primary: '#635bff',
      bg: '#f6f9fc',
      surface: '#ffffff',
      muted: '#e9eef5',
      sidebar: '#f3f6fb',
    },
    previewDark: {
      primary: '#a59bff',
      bg: '#0a1020',
      surface: '#141e32',
      muted: '#202c43',
      sidebar: '#10172a',
    },
  }),
  def({
    id: 'spotify',
    nameKey: 'common.theme.spotify',
    descriptionKey: 'themeSettings.themeDesc.spotify',
    group: 'website',
    uiStyle: 'spotify',
    previewLight: {
      primary: '#087a35',
      bg: '#f6f8f6',
      surface: '#ffffff',
      muted: '#e6eee8',
      sidebar: '#edf3ef',
    },
    previewDark: {
      primary: '#1ed760',
      bg: '#121212',
      surface: '#181818',
      muted: '#282828',
      sidebar: '#000000',
    },
  }),
  def({
    id: 'slack',
    nameKey: 'common.theme.slack',
    descriptionKey: 'themeSettings.themeDesc.slack',
    group: 'website',
    uiStyle: 'slack',
    previewLight: {
      primary: '#4a154b',
      bg: '#f8f7fa',
      surface: '#ffffff',
      muted: '#eeedf1',
      sidebar: '#4a154b',
    },
    previewDark: {
      primary: '#d6a5e2',
      bg: '#1a1d21',
      surface: '#222529',
      muted: '#2d3035',
      sidebar: '#350d36',
    },
  }),
  def({
    id: 'ocean',
    nameKey: 'common.theme.ocean',
    descriptionKey: 'themeSettings.themeDesc.ocean',
    group: 'light',
    recommended: true,
    previewLight: {
      primary: 'hsl(202 82% 35%)',
      bg: 'hsl(202 50% 96%)',
      surface: 'hsl(202 35% 99%)',
      muted: 'hsl(202 32% 91%)',
    },
    previewDark: {
      primary: 'hsl(195 78% 64%)',
      bg: 'hsl(207 44% 10%)',
      surface: 'hsl(207 38% 14%)',
      muted: 'hsl(207 28% 20%)',
    },
  }),
  def({
    id: 'forest',
    nameKey: 'common.theme.forest',
    descriptionKey: 'themeSettings.themeDesc.forest',
    group: 'light',
    recommended: true,
    previewLight: {
      primary: 'hsl(145 40% 30%)',
      bg: 'hsl(90 24% 95%)',
      surface: 'hsl(90 25% 98%)',
      muted: 'hsl(108 18% 89%)',
    },
    previewDark: {
      primary: 'hsl(139 35% 66%)',
      bg: 'hsl(145 22% 10%)',
      surface: 'hsl(145 19% 14%)',
      muted: 'hsl(143 16% 21%)',
    },
  }),
  def({
    id: 'lavender',
    nameKey: 'common.theme.lavender',
    descriptionKey: 'themeSettings.themeDesc.lavender',
    group: 'light',
    previewLight: {
      primary: 'hsl(267 42% 44%)',
      bg: 'hsl(266 40% 97%)',
      surface: 'hsl(270 40% 99%)',
      muted: 'hsl(269 28% 92%)',
    },
    previewDark: {
      primary: 'hsl(269 65% 77%)',
      bg: 'hsl(266 24% 12%)',
      surface: 'hsl(267 22% 17%)',
      muted: 'hsl(267 18% 24%)',
    },
  }),
  def({
    id: 'amber',
    nameKey: 'common.theme.amber',
    descriptionKey: 'themeSettings.themeDesc.amber',
    group: 'light',
    previewLight: {
      primary: 'hsl(35 85% 31%)',
      bg: 'hsl(43 48% 96%)',
      surface: 'hsl(42 50% 99%)',
      muted: 'hsl(42 31% 89%)',
    },
    previewDark: {
      primary: 'hsl(42 85% 63%)',
      bg: 'hsl(35 17% 11%)',
      surface: 'hsl(35 16% 15%)',
      muted: 'hsl(35 14% 22%)',
    },
  }),
  def({
    id: 'espresso',
    nameKey: 'common.theme.espresso',
    descriptionKey: 'themeSettings.themeDesc.espresso',
    group: 'dark',
    previewLight: {
      primary: 'hsl(22 38% 35%)',
      bg: 'hsl(26 26% 94%)',
      surface: 'hsl(30 27% 98%)',
      muted: 'hsl(28 20% 87%)',
    },
    previewDark: {
      primary: 'hsl(28 46% 70%)',
      bg: 'hsl(20 22% 10%)',
      surface: 'hsl(22 20% 14%)',
      muted: 'hsl(22 17% 21%)',
    },
  }),
  def({
    id: 'sakura',
    nameKey: 'common.theme.sakura',
    descriptionKey: 'themeSettings.themeDesc.sakura',
    group: 'light',
    previewLight: {
      primary: 'hsl(345 54% 43%)',
      bg: 'hsl(350 52% 97%)',
      surface: 'hsl(345 45% 99%)',
      muted: 'hsl(347 33% 92%)',
    },
    previewDark: {
      primary: 'hsl(346 68% 74%)',
      bg: 'hsl(340 23% 11%)',
      surface: 'hsl(340 22% 16%)',
      muted: 'hsl(339 18% 23%)',
    },
  }),
  def({
    id: 'mint',
    nameKey: 'common.theme.mint',
    descriptionKey: 'themeSettings.themeDesc.mint',
    group: 'light',
    previewLight: {
      primary: 'hsl(164 67% 27%)',
      bg: 'hsl(157 38% 96%)',
      surface: 'hsl(155 38% 99%)',
      muted: 'hsl(157 27% 90%)',
    },
    previewDark: {
      primary: 'hsl(158 52% 65%)',
      bg: 'hsl(166 33% 9%)',
      surface: 'hsl(164 28% 13%)',
      muted: 'hsl(164 23% 20%)',
    },
  }),
  def({
    id: 'midnight',
    nameKey: 'common.theme.midnight',
    descriptionKey: 'themeSettings.themeDesc.midnight',
    group: 'dark',
    previewLight: {
      primary: 'hsl(230 60% 45%)',
      bg: 'hsl(228 45% 96%)',
      surface: 'hsl(226 45% 99%)',
      muted: 'hsl(228 29% 91%)',
    },
    previewDark: {
      primary: 'hsl(218 84% 74%)',
      bg: 'hsl(228 45% 7%)',
      surface: 'hsl(228 37% 12%)',
      muted: 'hsl(228 30% 19%)',
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

export const THEME_GROUP_ORDER: ThemeGroup[] = ['recommended', 'website', 'light', 'dark', 'editor']

export function getThemePreviewSwatch(item: ColorThemeDef, mode: Theme): ThemePreviewSwatch {
  return mode === 'dark' ? item.previewDark : item.previewLight
}

const STORAGE_KEY = 'theme'
const COLOR_THEME_STORAGE_KEY = 'color-theme'
const DEFAULT_COLOR_THEME: ColorTheme = 'studio'
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
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (isThemeMode(stored)) return stored
    // Legacy: only light/dark was stored — treat as explicit choice.
    if (stored === 'light' || stored === 'dark') return stored
  } catch {
    /* localStorage unavailable (private mode / blocked) */
  }
  return DEFAULT_THEME_MODE
}

function getInitialColorTheme(): ColorTheme {
  try {
    const stored = localStorage.getItem(COLOR_THEME_STORAGE_KEY)
    if (isColorTheme(stored)) return stored
  } catch {
    /* localStorage unavailable (private mode / blocked) */
  }
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
  try {
    localStorage.setItem(STORAGE_KEY, mode)
  } catch {
    /* localStorage unavailable (private mode / blocked) */
  }
  applyResolvedTheme(resolveTheme(mode))
}

function persistColorTheme(nextColorTheme: ColorTheme) {
  const root = document.documentElement
  COLOR_THEMES.forEach((theme) => {
    root.classList.remove(`theme-${theme.id}`)
  })
  root.classList.add(`theme-${nextColorTheme}`)
  try {
    localStorage.setItem(COLOR_THEME_STORAGE_KEY, nextColorTheme)
  } catch {
    /* localStorage unavailable (private mode / blocked) */
  }
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
    // 冻结元素级颜色过渡：View Transition 的"新画面"是活文档,若放任全页
    // 几千个 200ms transition-colors 同时跑,扫过期间会逐帧重绘导致卡顿。
    root.classList.add('theme-switching')
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
          duration: 400,
          easing: 'ease-out',
          pseudoElement: '::view-transition-new(root)',
        },
      )
    })
    const cleanup = () => root.classList.remove('theme-switching')
    transition.finished.then(cleanup, cleanup)
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
