import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { useNavigate } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import {
  Activity,
  BookOpen,
  Braces,
  CircleDollarSign,
  CornerDownLeft,
  FlaskConical,
  Globe,
  Image as ImageIcon,
  KeyRound,
  Languages,
  LayoutDashboard,
  Moon,
  Palette,
  RadioTower,
  Search,
  Server,
  Settings,
  ShieldAlert,
  Sparkles,
  Sun,
  Users,
  X,
} from 'lucide-react'
import { useTheme } from '../hooks/useTheme'
import { filterCommandItems, type CommandSearchItem } from '../lib/commandPaletteSearch'
import { cn } from '@/lib/utils'
import { GlideMenu } from '@/components/ui/beautiful'

interface CommandEntry extends CommandSearchItem {
  icon: ReactNode
  badge?: string
  onSelect: () => void
}

export default function CommandPalette({
  open,
  onClose,
  onToggleLang,
}: {
  open: boolean
  onClose: () => void
  onToggleLang: () => void
}) {
  const { t, i18n } = useTranslation()
  const navigate = useNavigate()
  const { theme, toggle, colorTheme, setColorTheme } = useTheme()
  const isZh = (i18n.language || '').toLowerCase().startsWith('zh')
  const [query, setQuery] = useState('')
  const [selectedIndex, setSelectedIndex] = useState(0)
  const inputRef = useRef<HTMLInputElement | null>(null)
  const listRef = useRef<HTMLDivElement | null>(null)

  const items = useMemo<CommandEntry[]>(() => {
    const go = (path: string) => () => {
      navigate(path)
      onClose()
    }
    const navGroup = isZh ? '页面导航' : 'Navigation'
    const channelGroup = isZh ? '账号渠道直达' : 'Account Channels'
    const actionGroup = isZh ? '快捷操作' : 'Quick Actions'

    return [
      {
        id: 'nav-dashboard',
        label: t('nav.dashboard'),
        group: navGroup,
        keywords: ['dashboard', 'home', 'overview', '仪表盘', '首页'],
        icon: <LayoutDashboard className="size-4" />,
        onSelect: go('/'),
      },
      {
        id: 'nav-accounts',
        label: t('nav.accounts'),
        group: navGroup,
        keywords: ['accounts', 'codex', 'pool', '账号'],
        icon: <Users className="size-4" />,
        onSelect: go('/accounts'),
      },
      {
        id: 'nav-api-keys',
        label: t('nav.apiKeys'),
        group: navGroup,
        keywords: ['api', 'keys', 'token', '密钥'],
        icon: <KeyRound className="size-4" />,
        onSelect: go('/api-keys'),
      },
      {
        id: 'nav-proxies',
        label: t('nav.proxies'),
        group: navGroup,
        keywords: ['proxy', 'socks5', 'http', '代理'],
        icon: <Globe className="size-4" />,
        onSelect: go('/proxies'),
      },
      {
        id: 'nav-usage',
        label: t('nav.usage'),
        group: navGroup,
        keywords: ['usage', 'logs', 'stats', '使用统计', '日志'],
        icon: <Activity className="size-4" />,
        onSelect: go('/usage'),
      },
      {
        id: 'nav-channel-monitor',
        label: t('nav.channelMonitor'),
        group: navGroup,
        keywords: ['monitor', 'health', '渠道监控'],
        icon: <RadioTower className="size-4" />,
        onSelect: go('/channel-monitor'),
      },
      {
        id: 'nav-ops',
        label: t('nav.ops'),
        group: navGroup,
        keywords: ['ops', 'operations', 'runtime', 'scheduler', '运维'],
        icon: <Server className="size-4" />,
        onSelect: go('/ops/overview'),
      },
      {
        id: 'nav-images',
        label: t('nav.images'),
        group: navGroup,
        keywords: ['image', 'studio', '生图'],
        icon: <ImageIcon className="size-4" />,
        onSelect: go('/images/studio'),
      },
      {
        id: 'nav-intelligence',
        label: t('nav.intelligenceCheck'),
        group: navGroup,
        keywords: ['intelligence', 'check', '降智检测'],
        icon: <FlaskConical className="size-4" />,
        onSelect: go('/intelligence-check'),
      },
      {
        id: 'nav-quality',
        label: t('nav.qualityTest'),
        group: navGroup,
        keywords: ['quality', 'test', 'benchmark', '质量测试'],
        icon: <FlaskConical className="size-4" />,
        onSelect: go('/quality-test'),
      },
      {
        id: 'nav-prompt-filter',
        label: t('nav.promptFilter'),
        group: navGroup,
        keywords: ['prompt', 'filter', 'security', '检查'],
        icon: <ShieldAlert className="size-4" />,
        onSelect: go('/prompt-filter/overview'),
      },
      {
        id: 'nav-model-pricing',
        label: t('nav.modelPricing'),
        group: navGroup,
        keywords: ['pricing', 'cost', 'model', '模型定价'],
        icon: <CircleDollarSign className="size-4" />,
        onSelect: go('/model-pricing'),
      },
      {
        id: 'nav-payload-rules',
        label: t('nav.payloadRules'),
        group: navGroup,
        keywords: ['payload', 'rules', 'rewrite', '请求体重写'],
        icon: <Braces className="size-4" />,
        onSelect: go('/payload-rules/editor'),
      },
      {
        id: 'nav-theme',
        label: t('nav.theme'),
        group: navGroup,
        keywords: ['theme', 'appearance', 'studio', 'dark', '主题'],
        icon: <Palette className="size-4" />,
        onSelect: go('/theme'),
      },
      {
        id: 'nav-settings',
        label: t('nav.settings'),
        group: navGroup,
        keywords: ['settings', 'config', '系统设置'],
        icon: <Settings className="size-4" />,
        onSelect: go('/settings'),
      },
      {
        id: 'nav-docs',
        label: t('nav2.docs'),
        group: navGroup,
        keywords: ['docs', 'api', 'guide', '文档'],
        icon: <BookOpen className="size-4" />,
        onSelect: go('/docs'),
      },
      // Channel shortcuts
      {
        id: 'channel-opencode',
        label: 'OpenCode',
        description: isZh ? 'OpenCode Go / Zen 账号池' : 'OpenCode Go / Zen pool',
        group: channelGroup,
        keywords: ['opencode', 'go', 'zen', 'sub2api'],
        icon: <Users className="size-4" />,
        badge: 'Go / Zen',
        onSelect: go('/accounts/opencode'),
      },
      {
        id: 'channel-claude',
        label: 'Claude',
        description: isZh ? 'Claude 账号池' : 'Claude account pool',
        group: channelGroup,
        keywords: ['claude', 'anthropic'],
        icon: <Users className="size-4" />,
        onSelect: go('/accounts/claude'),
      },
      {
        id: 'channel-grok',
        label: 'Grok',
        description: isZh ? 'Grok 账号池' : 'Grok account pool',
        group: channelGroup,
        keywords: ['grok', 'xai'],
        icon: <Users className="size-4" />,
        onSelect: go('/accounts/grok'),
      },
      {
        id: 'channel-antigravity',
        label: 'Antigravity',
        description: isZh ? 'Google Antigravity 账号池' : 'Google Antigravity pool',
        group: channelGroup,
        keywords: ['antigravity', 'google', 'gemini'],
        icon: <Users className="size-4" />,
        onSelect: go('/accounts/antigravity'),
      },
      {
        id: 'channel-kiro',
        label: 'Kiro',
        description: isZh ? 'Kiro 账号池' : 'Kiro account pool',
        group: channelGroup,
        keywords: ['kiro', 'aws'],
        icon: <Users className="size-4" />,
        onSelect: go('/accounts/kiro'),
      },
      // Quick Actions
      {
        id: 'action-toggle-mode',
        label: theme === 'dark' ? t('common.switchToLight') : t('common.switchToDark'),
        group: actionGroup,
        keywords: ['theme', 'dark', 'light', 'mode', '浅色', '深色', '暗色'],
        icon: theme === 'dark' ? <Sun className="size-4" /> : <Moon className="size-4" />,
        onSelect: () => {
          toggle()
          onClose()
        },
      },
      ...(colorTheme !== 'studio'
        ? [
            {
              id: 'action-studio-theme',
              label: isZh ? '切换为 Beautiful Studio 主题' : 'Switch to Beautiful Studio Theme',
              group: actionGroup,
              keywords: ['studio', 'beautiful', 'theme', '主题', '美化'],
              icon: <Sparkles className="size-4" />,
              badge: 'NEW',
              onSelect: () => {
                setColorTheme('studio')
                onClose()
              },
            } satisfies CommandEntry,
          ]
        : []),
      {
        id: 'action-toggle-lang',
        label: isZh ? 'Switch Language to English' : '切换界面语言为中文',
        group: actionGroup,
        keywords: ['language', 'english', 'chinese', 'i18n', '语言', '中文', '英文'],
        icon: <Languages className="size-4" />,
        onSelect: () => {
          onToggleLang()
          onClose()
        },
      },
    ]
  }, [colorTheme, isZh, navigate, onClose, onToggleLang, setColorTheme, t, theme, toggle])

  const filtered = useMemo(() => filterCommandItems(items, query), [items, query])

  const grouped = useMemo(() => {
    const map = new Map<string, CommandEntry[]>()
    for (const item of filtered) {
      const bucket = map.get(item.group) ?? []
      bucket.push(item)
      map.set(item.group, bucket)
    }
    return Array.from(map.entries())
  }, [filtered])

  useEffect(() => {
    if (!open) return
    setQuery('')
    setSelectedIndex(0)
    const id = window.requestAnimationFrame(() => inputRef.current?.focus())
    return () => window.cancelAnimationFrame(id)
  }, [open])

  useEffect(() => {
    setSelectedIndex(0)
  }, [query])

  useEffect(() => {
    if (!open) return
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onClose()
        return
      }
      if (event.key === 'ArrowDown') {
        event.preventDefault()
        setSelectedIndex((prev) => (filtered.length === 0 ? 0 : (prev + 1) % filtered.length))
        return
      }
      if (event.key === 'ArrowUp') {
        event.preventDefault()
        setSelectedIndex((prev) =>
          filtered.length === 0 ? 0 : (prev - 1 + filtered.length) % filtered.length,
        )
        return
      }
      if (event.key === 'Enter' && filtered[selectedIndex]) {
        event.preventDefault()
        filtered[selectedIndex].onSelect()
      }
    }
    document.addEventListener('keydown', handleKeyDown)
    return () => document.removeEventListener('keydown', handleKeyDown)
  }, [filtered, onClose, open, selectedIndex])

  useEffect(() => {
    if (!open) return
    const active = listRef.current?.querySelector<HTMLElement>('[data-selected="true"]')
    active?.scrollIntoView({ block: 'nearest' })
  }, [open, selectedIndex])

  if (!open) return null

  let flatIndex = -1

  return createPortal(
    <div
      className="fixed inset-0 z-[95] flex items-start justify-center px-4 pt-[min(14vh,110px)]"
      role="dialog"
      aria-modal="true"
      aria-label={isZh ? '快速命令搜索' : 'Command Search'}
    >
      <div
        className="fixed inset-0 bg-black/45 backdrop-blur-[2px] animate-in fade-in-0 duration-150"
        onClick={onClose}
      />
      <div
        className="relative z-10 flex w-full max-w-[560px] flex-col overflow-hidden rounded-2xl border border-border/80 bg-popover text-popover-foreground animate-in fade-in-0 zoom-in-95 duration-150 dark:border-white/12"
        style={{ boxShadow: 'var(--bui-shadow-overlay)' }}
      >
        <div className="flex items-center gap-2.5 border-b border-border/75 px-4 py-3">
          <Search className="size-4 shrink-0 text-muted-foreground" />
          <input
            ref={inputRef}
            type="text"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder={isZh ? '搜索页面、账号渠道或快捷操作…' : 'Search pages, channels, or actions…'}
            className="min-w-0 flex-1 bg-transparent text-sm text-foreground outline-none placeholder:text-muted-foreground/80"
          />
          {query ? (
            <button
              type="button"
              onClick={() => setQuery('')}
              className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              aria-label={t('common.close')}
            >
              <X className="size-3.5" />
            </button>
          ) : (
            <kbd className="hidden rounded-md border border-border bg-muted/60 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground sm:inline-block">
              ESC
            </kbd>
          )}
        </div>

        <div ref={listRef} className="max-h-[min(56vh,420px)] overflow-y-auto p-2">
          {grouped.length === 0 ? (
            <div className="flex flex-col items-center justify-center gap-1.5 py-10 text-center">
              <Search className="size-5 text-muted-foreground/50" />
              <p className="text-xs font-medium text-muted-foreground">
                {isZh ? '未找到匹配的页面或命令' : 'No matching pages or commands'}
              </p>
            </div>
          ) : (
            <GlideMenu className="space-y-3">
              {grouped.map(([groupName, entries]) => (
                <div key={groupName} className="space-y-0.5">
                  <div className="px-2.5 py-1 text-[10.5px] font-semibold uppercase tracking-wider text-muted-foreground/80">
                    {groupName}
                  </div>
                  {entries.map((entry) => {
                    flatIndex += 1
                    const idx = flatIndex
                    const isSelected = idx === selectedIndex
                    return (
                      <button
                        key={entry.id}
                        type="button"
                        data-glide-item="true"
                        data-selected={isSelected ? 'true' : undefined}
                        onMouseEnter={() => setSelectedIndex(idx)}
                        onClick={entry.onSelect}
                        className={cn(
                          'relative z-10 flex w-full items-center justify-between gap-3 rounded-lg px-2.5 py-2 text-left text-[13px] transition-colors',
                          isSelected
                            ? 'bg-primary/10 text-primary'
                            : 'text-foreground/90',
                        )}
                      >
                        <span className="flex min-w-0 items-center gap-2.5">
                          <span
                            className={cn(
                              'flex size-7 shrink-0 items-center justify-center rounded-md border text-muted-foreground transition-colors',
                              isSelected
                                ? 'border-primary/25 bg-primary/10 text-primary'
                                : 'border-border/70 bg-background/80',
                            )}
                          >
                            {entry.icon}
                          </span>
                          <span className="min-w-0">
                            <span className="block truncate font-medium">{entry.label}</span>
                            {entry.description ? (
                              <span className="block truncate text-[11px] text-muted-foreground">
                                {entry.description}
                              </span>
                            ) : null}
                          </span>
                        </span>
                        <span className="flex shrink-0 items-center gap-1.5">
                          {entry.badge ? (
                            <span className="rounded-md border border-border/70 bg-muted/60 px-1.5 py-0.5 font-mono text-[10px] font-medium text-muted-foreground">
                              {entry.badge}
                            </span>
                          ) : null}
                          {isSelected ? <CornerDownLeft className="size-3.5 text-primary/80" /> : null}
                        </span>
                      </button>
                    )
                  })}
                </div>
              ))}
            </GlideMenu>
          )}
        </div>

        <div className="flex items-center justify-between border-t border-border/75 bg-muted/25 px-3.5 py-2 text-[11px] text-muted-foreground">
          <div className="flex items-center gap-3">
            <span className="inline-flex items-center gap-1">
              <kbd className="rounded border border-border bg-background px-1 font-mono text-[10px]">↑↓</kbd>
              <span>{isZh ? '选择' : 'Navigate'}</span>
            </span>
            <span className="inline-flex items-center gap-1">
              <kbd className="rounded border border-border bg-background px-1 font-mono text-[10px]">↵</kbd>
              <span>{isZh ? '打开' : 'Open'}</span>
            </span>
          </div>
          <span className="font-mono text-[10px] opacity-75">⌘K / Ctrl+K</span>
        </div>
      </div>
    </div>,
    document.body,
  )
}
