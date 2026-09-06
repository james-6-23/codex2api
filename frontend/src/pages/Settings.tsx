import type { ChangeEvent, FocusEvent, ReactNode } from 'react'
import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, resetAdminAuthState, setAdminKey } from '../api'
import { formatBeijingTime, getTimezone, setTimezone } from '../utils/time'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { AntigravityOAuthClientSetting, AntigravitySettingsResponse, ChannelTestSettings, HealthResponse, ModelInfo, SiteBranding, SystemSettings, UpstreamChannel } from '../types'
import { ANTIGRAVITY_DEFAULT_MODELS } from '../lib/antigravityModels'
import { countPayloadRules } from './PayloadRules'
import { getErrorMessage } from '../utils/error'
import { DEFAULT_CLAUDE_MODEL_MAP } from '../lib/modelMapping'
import {
  CLAUDE_TIMEZONE_CUSTOM,
  CLAUDE_TIMEZONE_OPTIONS,
  claudeTimezoneLabel,
  findClaudeTimezoneOption,
} from '../lib/claudeAccountOptions'
import { buildWritableSettingsPayload } from '../lib/settingsPayload'
import {
  buildContinuousRetryCatchAllPatch,
  buildContinuousRetryEnabledPatch,
  createContinuousRetrySaveQueue,
  parseContinuousRetryErrorCodes,
  parseContinuousRetryMaxDurationSeconds,
  parseContinuousRetryStatusCodes,
} from '../lib/continuousRetrySettings'
import {
  MIB,
  buildResponseCacheBudgetPatch,
  bytesToMiB,
  mergeResponseCacheGeneration,
  mibToBytes,
  validateResponseCacheBudget,
  type ResponseCacheBudgetMiB,
  type ResponseCacheBudgetValidationError,
} from '../lib/responseCacheMetrics'
import { DEFAULT_SITE_LOGO, isBrandingVideo, sanitizeBrandingImage, sanitizeBrandingLogo, useBranding } from '../branding'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DraftNumberInput } from '@/components/ui/draft-number-input'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { cn } from '@/lib/utils'

import { Switch } from '@/components/ui/switch'
import {
  Sheet,
  SheetBody,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import {
  Activity,
  Brain,
  Check,
  ChevronDown,
  ChevronRight,
  CircleAlert,
  CircleHelp,
  Cloud,
  Database,
  ExternalLink,
  Eye,
  Gauge,
  Globe,
  Image as ImageIcon,
  Layers,
  Link2,
  Loader2,
  Palette,
  RefreshCw,
  RotateCcw,
  Save,
  Server,
  Shield,
  ShieldAlert,
  SlidersHorizontal,
  Terminal,
  Trash2,
  Timer,
  Upload,
  Users,
  Shuffle,
  Wifi,
  Wrench,
  X,
} from 'lucide-react'
import { Link, useLocation, useNavigate, useSearchParams } from 'react-router-dom'
import ChannelLogo from '../components/ChannelLogo'
import ChannelScopeBadges, { ALL_UPSTREAM_CHANNELS } from '../components/ChannelScopeBadges'
import { useVisibleChannels } from '../visibleChannels'
import { ALL_VISIBLE_CHANNEL_OPTIONS, FALLBACK_VISIBLE_CHANNEL, toggleVisibleChannel } from '../lib/visibleChannels'

type ModelPanelKey = 'registry' | 'anthropic' | 'codex' | 'reasoning'

type ModelMappingEntry = [string, string]
const EMPTY_MODEL_MAPPING_ENTRIES: ModelMappingEntry[] = []
type ReasoningEffortModelEntry = {
  model: string
  effort: string
}
type AutoSaveStatus = 'idle' | 'saving' | 'saved' | 'error'
type CodexUserAgentConfig = {
  raw_user_agent?: string
  client_name?: string
  client_version?: string
  os_name?: string
  os_version?: string
  arch?: string
  terminal?: string
}

const EMPTY_REASONING_EFFORT_MODEL_ENTRIES: ReasoningEffortModelEntry[] = []
const REASONING_EFFORT_OPTIONS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'ultra', 'max'].map((effort) => ({
  label: effort,
  value: effort,
}))
const AUTO_SAVE_STATUS_RESET_MS = 1800
const AUTO_SAVE_TOAST_MS = 2000
const DEFAULT_RESPONSE_CACHE_TOTAL_BYTES = 64 * MIB
const DEFAULT_RESPONSE_CACHE_ENTRY_BYTES = 8 * MIB
const DEFAULT_RESPONSE_CACHE_RECONSTRUCT_BYTES = 64 * MIB
const DEFAULT_MODELS_LIST_READ_MAX_BYTES = 8 * MIB
const RESPONSE_CACHE_BUDGET_KEYS = [
  'response_cache_local_max_bytes',
  'response_cache_local_max_entry_bytes',
  'response_cache_reconstruct_max_bytes',
  'response_cache_write_policy',
] as const satisfies ReadonlyArray<keyof SystemSettings>
const DEFAULT_CODEX_UA_CONFIG: Required<CodexUserAgentConfig> = {
  raw_user_agent: '',
  client_name: 'codex-tui',
  client_version: '0.153.3',
  os_name: 'Mac OS',
  os_version: '15.5.0',
  arch: 'arm64',
  terminal: 'xterm-256color',
}

type SettingsTabKey = 'codex' | 'claude' | 'antigravity' | 'grok' | 'appearance' | 'general'
// 设置项适用渠道（按后端消费点核对）：
//   CODEX_ONLY   仅 Codex（Responses/WS、生图存储、Tier 计费、模型清单）
//   CODEX_CLAUDE 依赖 5h/7d 用量窗口的逻辑，Codex 与 Claude 都会写该窗口
//   STREAMING    经 handleStreamResponse 的 Chat 流式路径（Claude 原生 Messages 不经过）
//   RELAY        中转/API Key 型账号（Grok 强制走 OAuth 策略，不在其中）
const CHANNELS_CODEX_ONLY: readonly UpstreamChannel[] = ['codex']
const CHANNELS_CODEX_CLAUDE: readonly UpstreamChannel[] = ['codex', 'claude']
const CHANNELS_STREAMING: readonly UpstreamChannel[] = ['codex', 'grok', 'antigravity']
const CHANNELS_RELAY: readonly UpstreamChannel[] = ['codex', 'antigravity']
const SETTINGS_TAB_KEYS: readonly SettingsTabKey[] = ['codex', 'claude', 'antigravity', 'grok', 'appearance', 'general']
const DEFAULT_SETTINGS_TAB: SettingsTabKey = 'codex'
const isSettingsTabKey = (value: string | null): value is SettingsTabKey =>
  value !== null && (SETTINGS_TAB_KEYS as readonly string[]).includes(value)
// 旧版单页锚点 → Tab 映射，保证外部深链不失效。
const LEGACY_SECTION_TABS: Record<string, SettingsTabKey> = {
  'settings-overview': 'general',
  'settings-traffic': 'general',
  'settings-runtime': 'general',
  'settings-storage': 'general',
  'settings-security': 'general',
  'settings-reference': 'general',
  'settings-models': 'codex',
  'settings-codex-quota': 'codex',
  'settings-codex-transport': 'codex',
  'settings-codex-client': 'codex',
  'settings-grok': 'grok',
  'settings-claude': 'claude',
  'settings-antigravity': 'antigravity',
  'settings-appearance': 'appearance',
}
// 每个 Tab 内的分区目录：多于一个分区的 Tab 渲染侧边目录并按滚动位置高亮。
// icon 与对应 SettingsSection 的图标保持一致，目录项和分区标题才能互相对上。
const SETTINGS_TAB_SECTION_INDEX: Record<SettingsTabKey, ReadonlyArray<{ id: string; labelKey: string; icon: ReactNode }>> = {
  codex: [
    { id: 'settings-codex-quota', labelKey: 'settings.nav.codexQuota', icon: <Gauge /> },
    { id: 'settings-codex-transport', labelKey: 'settings.nav.codexTransport', icon: <Wifi /> },
    { id: 'settings-codex-client', labelKey: 'settings.nav.codexClient', icon: <Terminal /> },
    { id: 'settings-models', labelKey: 'settings.nav.models', icon: <Layers /> },
  ],
  claude: [{ id: 'settings-claude', labelKey: 'settings.nav.claude', icon: <ChannelLogo channel="claude" size={16} /> }],
  antigravity: [{ id: 'settings-antigravity', labelKey: 'settings.nav.antigravity', icon: <ChannelLogo channel="antigravity" size={16} /> }],
  grok: [{ id: 'settings-grok', labelKey: 'settings.nav.grok', icon: <ChannelLogo channel="grok" size={16} /> }],
  appearance: [{ id: 'settings-appearance', labelKey: 'settings.nav.appearance', icon: <Palette /> }],
  general: [
    { id: 'settings-overview', labelKey: 'settings.nav.overview', icon: <Activity /> },
    { id: 'settings-traffic', labelKey: 'settings.nav.traffic', icon: <Gauge /> },
    { id: 'settings-runtime', labelKey: 'settings.nav.runtime', icon: <Wrench /> },
    { id: 'settings-storage', labelKey: 'settings.nav.storage', icon: <ImageIcon /> },
    { id: 'settings-security', labelKey: 'settings.nav.security', icon: <Shield /> },
    { id: 'settings-reference', labelKey: 'settings.nav.reference', icon: <Link2 /> },
  ],
}
// 分区滚动高亮的判定线：分区顶部越过视口该高度即视为当前分区（要盖过粘性 Tab 栏）。
const SETTINGS_SECTION_SPY_OFFSET_PX = 140
// 手动保存字段的脏检查里跳过的键：生成号是服务端只读，自定义 Prompt 规则由规则页单独保存。
const SETTINGS_DIRTY_IGNORED_KEYS: ReadonlySet<string> = new Set(['response_cache_config_generation', 'prompt_filter_custom_patterns'])

const getDefaultModelMappingEntries = (): ModelMappingEntry[] =>
  Object.entries(DEFAULT_CLAUDE_MODEL_MAP) as ModelMappingEntry[]

const parseModelMappingEntries = (value: string, fallbackEntries: ModelMappingEntry[] = []): ModelMappingEntry[] => {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return fallbackEntries
    }

    const entries = Object.entries(parsed).map(([key, model]) => [
      key,
      typeof model === 'string' ? model : String(model ?? ''),
    ]) as ModelMappingEntry[]

    // 如果数据库中为空，按调用方提供的默认值填充
    return entries.length > 0 ? entries : fallbackEntries
  } catch {
    return fallbackEntries
  }
}

const serializeModelMappingEntries = (entries: ModelMappingEntry[]) => {
  const obj: Record<string, string> = {}
  for (const [key, model] of entries) {
    const trimmedKey = key.trim()
    const trimmedModel = model.trim()
    if (trimmedKey && trimmedModel) obj[trimmedKey] = trimmedModel
  }
  return JSON.stringify(obj)
}

const normalizeReasoningEffortValue = (effort: string) => {
  const value = effort.trim().toLowerCase()
  // max 仅 gpt-5.6+ 上游支持,后端会按模型钳位,前端保留原值让用户可配
  return ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'ultra', 'max'].includes(value) ? value : 'xhigh'
}

const normalizeBillingTierPolicyValue = (value?: string | null): 'actual' | 'requested' =>
  value === 'requested' ? 'requested' : 'actual'

const normalizeFirstTokenModeValue = (value?: string | null): 'strict' | 'loose' =>
  value === 'loose' ? 'loose' : 'strict'

const getSettingsPatchValues = (settings: SystemSettings, keys: Array<keyof SystemSettings>): Partial<SystemSettings> => {
  const patch: Record<string, unknown> = {}
  for (const key of keys) {
    patch[key] = settings[key]
  }
  return patch as Partial<SystemSettings>
}

// 脏检查用的宽松相等：null/undefined 同义，数组与对象按 JSON 结构比较。
const settingsValueEquals = (a: unknown, b: unknown) => {
  if (a === b) return true
  if (a == null && b == null) return true
  if (a == null || b == null) return false
  if (typeof a === 'object' || typeof b === 'object') return JSON.stringify(a) === JSON.stringify(b)
  return false
}
const normalizeResponseCacheSettings = (settings: SystemSettings): SystemSettings => ({
  ...settings,
  response_cache_local_max_bytes: Number.isFinite(settings.response_cache_local_max_bytes)
    ? settings.response_cache_local_max_bytes
    : DEFAULT_RESPONSE_CACHE_TOTAL_BYTES,
  response_cache_local_max_entry_bytes: Number.isFinite(settings.response_cache_local_max_entry_bytes)
    ? settings.response_cache_local_max_entry_bytes
    : DEFAULT_RESPONSE_CACHE_ENTRY_BYTES,
  response_cache_reconstruct_max_bytes: Number.isFinite(settings.response_cache_reconstruct_max_bytes)
    ? settings.response_cache_reconstruct_max_bytes
    : DEFAULT_RESPONSE_CACHE_RECONSTRUCT_BYTES,
  response_cache_write_policy: settings.response_cache_write_policy === 'on_demand' ? 'on_demand' : 'always',
  response_cache_config_generation: Number.isFinite(settings.response_cache_config_generation)
    ? settings.response_cache_config_generation
    : 0,
})

const responseCacheBudgetFromSettings = (settings: SystemSettings): ResponseCacheBudgetMiB => ({
  totalMiB: bytesToMiB(settings.response_cache_local_max_bytes),
  entryMiB: bytesToMiB(settings.response_cache_local_max_entry_bytes),
  reconstructMiB: bytesToMiB(settings.response_cache_reconstruct_max_bytes),
})

const isResponseCacheBudgetKey = (key: keyof SystemSettings) =>
  RESPONSE_CACHE_BUDGET_KEYS.some((candidate) => candidate === key)

const responseCacheBudgetFieldPatch = (
  field: keyof ResponseCacheBudgetMiB,
  value: number,
): Partial<SystemSettings> => {
  switch (field) {
    case 'totalMiB':
      return { response_cache_local_max_bytes: mibToBytes(value) }
    case 'entryMiB':
      return { response_cache_local_max_entry_bytes: mibToBytes(value) }
    case 'reconstructMiB':
      return { response_cache_reconstruct_max_bytes: mibToBytes(value) }
  }
}

const parseReasoningEffortModelEntries = (value: string): ReasoningEffortModelEntry[] => {
  try {
    const parsed = JSON.parse(value || '[]')
    if (!Array.isArray(parsed)) return EMPTY_REASONING_EFFORT_MODEL_ENTRIES
    return parsed
      .map((entry) => ({
        model: typeof entry?.model === 'string' ? entry.model : '',
        effort: normalizeReasoningEffortValue(typeof entry?.effort === 'string' ? entry.effort : 'xhigh'),
      }))
      .filter((entry) => entry.model.trim())
  } catch {
    return EMPTY_REASONING_EFFORT_MODEL_ENTRIES
  }
}

const serializeReasoningEffortModelEntries = (entries: ReasoningEffortModelEntry[]) => {
  const seen = new Set<string>()
  const normalized: ReasoningEffortModelEntry[] = []
  for (const entry of entries) {
    const model = entry.model.trim()
    const effort = normalizeReasoningEffortValue(entry.effort)
    if (!model) continue
    const key = `${model.toLowerCase()}(${effort})`
    if (seen.has(key)) continue
    seen.add(key)
    normalized.push({ model, effort })
  }
  return JSON.stringify(normalized)
}

const reasoningEffortAlias = (entry: ReasoningEffortModelEntry) => {
  const model = entry.model.trim()
  const effort = normalizeReasoningEffortValue(entry.effort)
  return model ? `${model}(${effort})` : ''
}

const parseCodexUserAgentConfig = (value?: string): CodexUserAgentConfig => {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
    return {
      raw_user_agent: typeof parsed.raw_user_agent === 'string' ? parsed.raw_user_agent : '',
      client_name: typeof parsed.client_name === 'string' ? parsed.client_name : '',
      client_version: typeof parsed.client_version === 'string' ? parsed.client_version : '',
      os_name: typeof parsed.os_name === 'string' ? parsed.os_name : '',
      os_version: typeof parsed.os_version === 'string' ? parsed.os_version : '',
      arch: typeof parsed.arch === 'string' ? parsed.arch : '',
      terminal: typeof parsed.terminal === 'string' ? parsed.terminal : '',
    }
  } catch {
    return {}
  }
}

const serializeCodexUserAgentConfig = (config: CodexUserAgentConfig) => {
  const normalized: CodexUserAgentConfig = {}
  for (const key of ['raw_user_agent', 'client_name', 'client_version', 'os_name', 'os_version', 'arch', 'terminal'] as const) {
    const value = (config[key] ?? '').trim()
    if (value) normalized[key] = key === 'client_version' ? normalizeVersionText(value) : value
  }
  return JSON.stringify(normalized)
}

type ParsedVersion = {
  core: [number, number, number]
  prerelease: string
}

const normalizeVersionText = (version?: string) => (version ?? '').trim().replace(/^v/i, '')

const parseVersion = (version?: string): ParsedVersion | null => {
  const match = normalizeVersionText(version).match(/^(\d+)\.(\d+)\.(\d+)(?:-([A-Za-z0-9][A-Za-z0-9.-]*))?$/)
  if (!match) return null
  return {
    core: [Number(match[1]), Number(match[2]), Number(match[3])],
    prerelease: match[4] ?? '',
  }
}

const isNumericVersionIdentifier = (value: string) => /^\d+$/.test(value)

const compareNumericVersionIdentifier = (a: string, b: string) => {
  const av = a.replace(/^0+/, '') || '0'
  const bv = b.replace(/^0+/, '') || '0'
  if (av.length !== bv.length) return av.length > bv.length ? 1 : -1
  if (av !== bv) return av > bv ? 1 : -1
  return 0
}

const comparePrerelease = (a: string, b: string) => {
  if (!a && !b) return 0
  if (!a) return 1
  if (!b) return -1
  const av = a.split('.')
  const bv = b.split('.')
  for (let i = 0; i < av.length && i < bv.length; i += 1) {
    const ai = av[i]
    const bi = bv[i]
    const an = isNumericVersionIdentifier(ai)
    const bn = isNumericVersionIdentifier(bi)
    if (an && bn) {
      const cmp = compareNumericVersionIdentifier(ai, bi)
      if (cmp !== 0) return cmp
    } else if (an) {
      return -1
    } else if (bn) {
      return 1
    } else if (ai !== bi) {
      return ai > bi ? 1 : -1
    }
  }
  if (av.length !== bv.length) return av.length > bv.length ? 1 : -1
  return 0
}

const compareVersions = (a?: string, b?: string) => {
  const av = parseVersion(a)
  const bv = parseVersion(b)
  if (!av || !bv) return 0
  for (let i = 0; i < 3; i += 1) {
    if (av.core[i] !== bv.core[i]) return av.core[i] > bv.core[i] ? 1 : -1
  }
  return comparePrerelease(av.prerelease, bv.prerelease)
}

const effectiveGeneratedCodexClientVersion = (version: string, minVersion: string, compatMode: string) => {
  const cleanVersion = normalizeVersionText(version) || DEFAULT_CODEX_UA_CONFIG.client_version
  const cleanMinVersion = normalizeVersionText(minVersion)
  if (compatMode === 'auto' && cleanMinVersion && compareVersions(cleanVersion, cleanMinVersion) < 0) {
    return cleanMinVersion
  }
  return cleanVersion
}

const buildCodexUserAgentPreview = (config: CodexUserAgentConfig, minVersion: string, compatMode: string) => {
  const raw = (config.raw_user_agent ?? '').trim()
  if (raw) return raw
  const clientName = (config.client_name ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.client_name
  const clientVersion = effectiveGeneratedCodexClientVersion(
    (config.client_version ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.client_version,
    minVersion,
    compatMode,
  )
  const osName = (config.os_name ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.os_name
  const osVersion = (config.os_version ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.os_version
  const arch = (config.arch ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.arch
  const terminal = (config.terminal ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.terminal
  return `${clientName}/${clientVersion} (${osName} ${osVersion}; ${arch}) ${terminal} (${clientName}; ${clientVersion})`
}

// 模型映射编辑器组件
function ModelMappingEditor({
  value,
  onChange,
  fallbackEntries = EMPTY_MODEL_MAPPING_ENTRIES,
  sourceOptions,
  targetOptions,
  sourceLabel,
  targetLabel,
  sourcePlaceholder,
  targetPlaceholder,
}: {
  value: string
  onChange: (v: string) => void
  fallbackEntries?: ModelMappingEntry[]
  sourceOptions?: Array<{ label: string; value: string }>
  targetOptions?: Array<{ label: string; value: string }>
  sourceLabel: string
  targetLabel: string
  sourcePlaceholder: string
  targetPlaceholder: string
}) {
  const { t } = useTranslation()
  const [mappings, setMappings] = useState<ModelMappingEntry[]>(() => parseModelMappingEntries(value, fallbackEntries))
  const lastEmittedValueRef = useRef<string | null>(null)
  const sourceListId = useId()
  const targetListId = useId()
  const sourceSuggestions = useMemo(() => {
    if (!sourceOptions) return []
    const byValue = new Map(sourceOptions.map((option) => [option.value, option]))
    for (const [source] of mappings) {
      const value = source.trim()
      if (value && !byValue.has(value)) {
        byValue.set(value, { label: value, value })
      }
    }
    return [...byValue.values()]
  }, [mappings, sourceOptions])
  const targetSuggestions = useMemo(() => {
    if (!targetOptions) return []
    const byValue = new Map(targetOptions.map((option) => [option.value, option]))
    for (const [, target] of mappings) {
      const value = target.trim()
      if (value && !byValue.has(value)) {
        byValue.set(value, { label: value, value })
      }
    }
    return [...byValue.values()]
  }, [mappings, targetOptions])

  useEffect(() => {
    if (value === lastEmittedValueRef.current) return
    setMappings(parseModelMappingEntries(value, fallbackEntries))
  }, [fallbackEntries, value])

  const updateMappings = (entries: ModelMappingEntry[]) => {
    setMappings(entries)
    const serialized = serializeModelMappingEntries(entries)
    lastEmittedValueRef.current = serialized
    onChange(serialized)
  }

  const handleChange = (index: number, field: 0 | 1, val: string) => {
    const next = [...mappings]
    next[index] = [...next[index]] as ModelMappingEntry
    next[index][field] = val
    updateMappings(next)
  }

  const handleRemove = (index: number) => {
    const next = mappings.filter((_, i) => i !== index)
    updateMappings(next)
  }

  const handleAdd = () => {
    const defaultSource = sourceOptions && targetOptions
      ? sourceOptions[1]?.value ?? sourceOptions[0]?.value ?? ''
      : sourceOptions?.[0]?.value ?? ''
    updateMappings([...mappings, [defaultSource, targetOptions?.[0]?.value ?? '']])
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <div className="hidden shrink-0 grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] gap-1.5 px-1 text-xs font-semibold text-muted-foreground sm:grid">
        <span>{sourceLabel}</span>
        <span>{targetLabel}</span>
        <span />
      </div>
      <div className="min-h-[180px] flex-1 space-y-2 overflow-y-auto pr-0.5 sm:space-y-1.5 sm:pr-1">
        {mappings.map(([k, v], i) => (
          <div
            key={i}
            className="grid grid-cols-1 gap-2 rounded-xl border border-border bg-background/70 p-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] sm:items-center sm:gap-1.5 sm:rounded-none sm:border-0 sm:bg-transparent sm:p-0"
          >
            <div className="min-w-0 space-y-1 sm:space-y-0">
              <span className="text-[11px] font-semibold text-muted-foreground sm:hidden">
                {sourceLabel}
              </span>
              <Input
                className="h-8 px-2 font-mono text-xs"
                list={sourceOptions ? sourceListId : undefined}
                placeholder={sourcePlaceholder}
                value={k}
                onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 0, e.target.value)}
              />
            </div>
            <div className="min-w-0 space-y-1 sm:space-y-0">
              <span className="text-[11px] font-semibold text-muted-foreground sm:hidden">
                {targetLabel}
              </span>
              <Input
                className="h-8 px-2 font-mono text-xs"
                list={targetOptions ? targetListId : undefined}
                placeholder={targetPlaceholder}
                value={v}
                onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 1, e.target.value)}
              />
            </div>
            <button
              type="button"
              onClick={() => handleRemove(i)}
              aria-label={t('common.delete')}
              className="flex size-8 items-center justify-center justify-self-end rounded-md text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10 sm:justify-self-auto"
            >
              <Trash2 className="size-3.5" />
            </button>
          </div>
        ))}
      </div>
      {sourceOptions ? (
        <datalist id={sourceListId}>
          {sourceSuggestions.map((option) => (
            <option key={option.value} value={option.value} label={option.label} />
          ))}
        </datalist>
      ) : null}
      {targetOptions ? (
        <datalist id={targetListId}>
          {targetSuggestions.map((option) => (
            <option key={option.value} value={option.value} label={option.label} />
          ))}
        </datalist>
      ) : null}
      <Button type="button" variant="outline" size="sm" className="self-start" onClick={handleAdd}>
        + {t('settings2.addMapping')}
      </Button>
    </div>
  )
}

function ReasoningEffortModelsEditor({
  value,
  onChange,
  baseModelOptions,
}: {
  value: string
  onChange: (v: string) => void
  baseModelOptions: Array<{ label: string; value: string }>
}) {
  const { t } = useTranslation()
  const [entries, setEntries] = useState<ReasoningEffortModelEntry[]>(() => parseReasoningEffortModelEntries(value))
  const lastEmittedValueRef = useRef<string | null>(null)
  const modelOptions = useMemo(() => {
    const byValue = new Map(baseModelOptions.map((option) => [option.value, option]))
    for (const entry of entries) {
      const model = entry.model.trim()
      if (model && !byValue.has(model)) {
        byValue.set(model, { label: model, value: model })
      }
    }
    return [...byValue.values()]
  }, [baseModelOptions, entries])

  useEffect(() => {
    if (value === lastEmittedValueRef.current) return
    setEntries(parseReasoningEffortModelEntries(value))
  }, [value])

  const updateEntries = (nextEntries: ReasoningEffortModelEntry[]) => {
    setEntries(nextEntries)
    const serialized = serializeReasoningEffortModelEntries(nextEntries)
    lastEmittedValueRef.current = serialized
    onChange(serialized)
  }

  const handleChange = (index: number, patch: Partial<ReasoningEffortModelEntry>) => {
    const next = entries.map((entry, i) => (i === index ? { ...entry, ...patch } : entry))
    updateEntries(next)
  }

  const handleRemove = (index: number) => {
    updateEntries(entries.filter((_, i) => i !== index))
  }

  const handleAdd = () => {
    updateEntries([...entries, { model: baseModelOptions[0]?.value ?? 'gpt-5.5', effort: 'xhigh' }])
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      {/* Mobile: stacked cards */}
      <div className="max-h-[320px] space-y-2 overflow-y-auto pr-0.5 sm:hidden">
        {entries.map((entry, i) => (
          <div
            key={i}
            className="rounded-xl border border-border bg-background/70 p-3 space-y-2"
          >
            <div className="flex items-center justify-between gap-2">
              <span className="text-[11px] font-semibold text-muted-foreground">
                {t('settings2.baseModel')}
              </span>
              <button
                type="button"
                onClick={() => handleRemove(i)}
                aria-label={t('common.delete')}
                className="flex size-8 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10"
              >
                <Trash2 className="size-3.5" />
              </button>
            </div>
            <Select
              compact
              value={entry.model.trim()}
              options={modelOptions}
              placeholder={t('settings2.selectBaseModel')}
              disabled={modelOptions.length === 0}
              onValueChange={(model) => handleChange(i, { model })}
            />
            <div className="grid grid-cols-2 gap-2">
              <div className="min-w-0 space-y-1">
                <span className="text-[11px] font-semibold text-muted-foreground">
                  {t('settings2.reasoningEffort')}
                </span>
                <Select
                  compact
                  value={normalizeReasoningEffortValue(entry.effort)}
                  options={REASONING_EFFORT_OPTIONS}
                  onValueChange={(effort) => handleChange(i, { effort })}
                />
              </div>
              <div className="min-w-0 space-y-1">
                <span className="text-[11px] font-semibold text-muted-foreground">
                  {t('settings2.generatedModel')}
                </span>
                <Badge variant="secondary" className="max-w-full px-2 py-1.5 font-mono text-[11px]">
                  <span className="truncate">{reasoningEffortAlias(entry) || '-'}</span>
                </Badge>
              </div>
            </div>
          </div>
        ))}
      </div>

      {/* Desktop: compact grid */}
      <div className="hidden min-h-0 flex-1 flex-col gap-2 sm:flex">
        <div className="grid shrink-0 grid-cols-[minmax(0,1.2fr)_minmax(0,0.8fr)_minmax(0,1fr)_2rem] gap-2 px-1 text-xs font-semibold text-muted-foreground">
          <span>{t('settings2.baseModel')}</span>
          <span>{t('settings2.reasoningEffort')}</span>
          <span>{t('settings2.generatedModel')}</span>
          <span />
        </div>
        <div className="max-h-[220px] space-y-1.5 overflow-y-auto pr-1">
          {entries.map((entry, i) => (
            <div
              key={i}
              className="grid grid-cols-[minmax(0,1.2fr)_minmax(0,0.8fr)_minmax(0,1fr)_2rem] items-center gap-2"
            >
              <Select
                compact
                value={entry.model.trim()}
                options={modelOptions}
                placeholder={t('settings2.selectBaseModel')}
                disabled={modelOptions.length === 0}
                onValueChange={(model) => handleChange(i, { model })}
              />
              <Select
                compact
                value={normalizeReasoningEffortValue(entry.effort)}
                options={REASONING_EFFORT_OPTIONS}
                onValueChange={(effort) => handleChange(i, { effort })}
              />
              <div className="flex min-w-0">
                <Badge variant="secondary" className="max-w-full px-2 py-1 font-mono text-[11px]">
                  <span className="truncate">{reasoningEffortAlias(entry) || '-'}</span>
                </Badge>
              </div>
              <button
                type="button"
                onClick={() => handleRemove(i)}
                aria-label={t('common.delete')}
                className="flex size-8 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10"
              >
                <Trash2 className="size-3.5" />
              </button>
            </div>
          ))}
        </div>
      </div>
      <Button type="button" variant="outline" size="sm" className="self-start" onClick={handleAdd}>
        + {t('settings2.addReasoningModel')}
      </Button>
    </div>
  )
}

/** Shared form grids — explicit columns so col-span / alignment stay predictable. */
const SETTINGS_FIELD_GRID = 'grid grid-cols-1 gap-x-4 gap-y-4 sm:grid-cols-2'
const SETTINGS_FIELD_GRID_3 = 'grid grid-cols-1 gap-x-4 gap-y-4 sm:grid-cols-2 xl:grid-cols-3'
const SETTINGS_SWITCH_GRID = 'grid grid-cols-1 gap-3 sm:grid-cols-2'
// 卡片里只有一个开关时用整行，放进双列栅格会挤成半宽、标签折行。
const SETTINGS_SWITCH_ROW = 'grid grid-cols-1 gap-3'
// 一组只含开关的相关设置合并成一张卡，用 SettingField layout="row" 逐行排列，说明文字直接外显。
const SETTINGS_ROW_LIST = 'divide-y divide-border/60'
// 卡片级双列栅格：卡片高度不一，必须顶对齐，否则矮卡被拉高留下大片空白。
const SETTINGS_CARD_GRID_2 = 'grid gap-4 lg:grid-cols-2 lg:items-stretch'

// ClaudeCodeSettingsCard 是 ClaudeCode 全局配置卡片(独立读写 /settings/claude-config)。
// 全体 Claude 账号默认遵守;个体账号可在「账号管理 → 编辑账号」里覆盖。
// Claude / Antigravity 渠道的连通性测试卡片:独立于全局 test_model/test_content(那是
// Codex 语义),按渠道保存默认探测模型与测活内容;留空模型 = 按账号目录自动选。
const CLAUDE_TEST_MODEL_CHOICES = ['claude-opus-4-6', 'claude-sonnet-4-6', 'claude-haiku-4-5', 'claude-opus-4-5', 'claude-sonnet-4-5']

function ChannelConnectivityTestCard({ channel }: { channel: 'antigravity' | 'claude' }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [settings, setSettings] = useState<ChannelTestSettings | null>(null)
  const [defaultContent, setDefaultContent] = useState('')
  const [defaultConcurrency, setDefaultConcurrency] = useState(0)
  const [contentDraft, setContentDraft] = useState('')
  const [catalogChoices, setCatalogChoices] = useState<string[]>([])
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let active = true
    void api.getChannelTestSettings().then((response) => {
      if (!active) return
      setSettings(response[channel])
      setContentDraft(response[channel].test_content)
      setDefaultContent(response.default_test_content)
      setDefaultConcurrency(response.default_test_concurrency)
      setCatalogChoices(response.model_choices?.[channel] ?? [])
    }).catch((error) => {
      if (!active) return
      showToast(getErrorMessage(error), 'error')
    })
    return () => {
      active = false
    }
  }, [channel, showToast])

  const save = useCallback(async (patch: Partial<ChannelTestSettings>) => {
    setSaving(true)
    try {
      const response = await api.updateChannelTestSettings({ [channel]: patch })
      setSettings(response[channel])
      setContentDraft(response[channel].test_content)
      setDefaultContent(response.default_test_content)
      setDefaultConcurrency(response.default_test_concurrency)
      setCatalogChoices(response.model_choices?.[channel] ?? [])
      showToast(t('settings.channelTest.saved'), 'success')
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSaving(false)
    }
  }, [channel, showToast, t])

  // 候选优先取后端汇总的号池目录并集(Claude 多为带日期的具体 ID),读不到再回落常量。
  const modelChoices = useMemo(() => {
    const base = catalogChoices.length > 0
      ? catalogChoices
      : channel === 'antigravity' ? [...ANTIGRAVITY_DEFAULT_MODELS] : CLAUDE_TEST_MODEL_CHOICES
    const current = settings?.test_model?.trim() ?? ''
    const list = current && !base.includes(current) ? [current, ...base] : base
    return [
      { value: '', label: t('settings.channelTest.autoModel') },
      ...list.map((model) => ({ value: model, label: model })),
    ]
  }, [catalogChoices, channel, settings?.test_model, t])

  return (
    <SettingsCard
      title={t('settings.connectivityTest')}
      description={t(channel === 'antigravity' ? 'settings.channelTest.antigravityDesc' : 'settings.channelTest.claudeDesc')}
      icon={<Wifi className="size-4" />}
    >
      <div className="space-y-4">
        <div className={SETTINGS_FIELD_GRID}>
          <SettingField label={t('settings.testModelLabel')} description={t('settings.channelTest.modelHint')}>
            <Select
              value={settings?.test_model ?? ''}
              onValueChange={(value) => void save({ test_model: value })}
              options={modelChoices}
              disabled={settings === null || saving}
            />
          </SettingField>
          <SettingField
            label={t('settings.testConcurrency')}
            description={t('settings.channelTest.concurrencyHint', { value: defaultConcurrency || 1 })}
          >
            <DraftNumberInput
              min={0}
              max={200}
              value={settings?.test_concurrency ?? 0}
              placeholder={String(defaultConcurrency || 1)}
              disabled={settings === null || saving}
              onValueChange={(value) => {
                if (value !== (settings?.test_concurrency ?? 0)) void save({ test_concurrency: value })
              }}
            />
          </SettingField>
        </div>
        <SettingField label={t('settings.testContent')} description={t('settings.channelTest.contentHint')}>
          <textarea
            rows={3}
            value={contentDraft}
            placeholder={defaultContent || t('settings.testContentPlaceholder')}
            disabled={settings === null || saving}
            onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setContentDraft(e.target.value)}
            onBlur={(e) => {
              const next = e.currentTarget.value.trim()
              if (next !== (settings?.test_content ?? '')) void save({ test_content: next })
            }}
            className={cn(
              'flex min-h-[88px] w-full resize-y rounded-md border border-input bg-background px-3 py-2 text-sm text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50',
            )}
          />
        </SettingField>
      </div>
    </SettingsCard>
  )
}

// Antigravity 模型重定向:下游请求不带思考强度后缀的逻辑模型(gemini-3.8-flash)时,
// 自动落到配置的固定档位(gemini-3.8-flash-high)。候选档位由后端按模型目录给出。
function AntigravityModelRedirectCard() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [settings, setSettings] = useState<AntigravitySettingsResponse | null>(null)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let active = true
    void api.getAntigravitySettings().then((response) => {
      if (active) setSettings(response)
    }).catch((error) => {
      if (active) showToast(getErrorMessage(error), 'error')
    })
    return () => {
      active = false
    }
  }, [showToast])

  const save = useCallback(async (patch: { model_redirects?: Record<string, string>; redirect_overrides_effort?: boolean }) => {
    setSaving(true)
    try {
      setSettings(await api.updateAntigravitySettings(patch))
      showToast(t('settings.antigravityRedirect.saved'), 'success')
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSaving(false)
    }
  }, [showToast, t])

  const setRedirect = (model: string, target: string) => {
    if (!settings) return
    const next = { ...settings.model_redirects }
    if (target) next[model] = target
    else delete next[model]
    void save({ model_redirects: next })
  }

  return (
    <SettingsCard
      title={t('settings.antigravityRedirect.title')}
      description={t('settings.antigravityRedirect.description')}
      icon={<Shuffle className="size-4" />}
    >
      <div className="space-y-4">
        <div className={SETTINGS_FIELD_GRID}>
          {(settings?.choices ?? []).map((choice) => (
            <SettingField
              key={choice.model}
              label={choice.model}
              description={t('settings.antigravityRedirect.rowHint', { level: choice.default_level })}
            >
              <Select
                value={settings?.model_redirects[choice.model] ?? ''}
                onValueChange={(value) => setRedirect(choice.model, value)}
                disabled={settings === null || saving}
                options={[
                  { value: '', label: t('settings.antigravityRedirect.noRedirect', { level: choice.default_level }) },
                  ...choice.tiers.map((tier) => ({ value: tier, label: tier })),
                ]}
              />
            </SettingField>
          ))}
        </div>
        <div className="flex items-center justify-between gap-3 rounded-lg border border-border/60 px-3 py-2.5">
          <div className="min-w-0">
            <div className="text-sm font-medium">{t('settings.antigravityRedirect.overrideLabel')}</div>
            <p className="mt-0.5 text-xs text-muted-foreground">{t('settings.antigravityRedirect.overrideHint')}</p>
          </div>
          <Switch
            checked={settings?.redirect_overrides_effort ?? false}
            disabled={settings === null || saving}
            onCheckedChange={(checked) => void save({ redirect_overrides_effort: checked })}
          />
        </div>
      </div>
    </SettingsCard>
  )
}

function ClaudeCodeSettingsCard() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [fingerprintMode, setFingerprintMode] = useState<'preserve' | 'force' | ''>('')
  const [clientPlatform, setClientPlatform] = useState<'any' | 'claude_code_cli_only'>('any')
  const [versionPolicy, setVersionPolicy] = useState<'passthrough' | 'fixed' | 'minimum'>('passthrough')
  const [clientVersion, setClientVersion] = useState('')
  const [timezone, setTimezone] = useState('')
  const [timezoneCustom, setTimezoneCustom] = useState(false)
  const [sessionWindow, setSessionWindow] = useState('')
  const [allowServiceTier, setAllowServiceTier] = useState(false)
  const [allowInferenceGeo, setAllowInferenceGeo] = useState(false)
  const [allowSpeed, setAllowSpeed] = useState(false)
  const [allowSafetyIdentifier, setAllowSafetyIdentifier] = useState(false)
  const [allowedBetaHeaders, setAllowedBetaHeaders] = useState('')
  const [maxOutputTokens, setMaxOutputTokens] = useState('0')
  const [maxToolCount, setMaxToolCount] = useState('0')
  const [maxToolSchemaBytes, setMaxToolSchemaBytes] = useState('0')
  const [cliVersionSyncEnabled, setCliVersionSyncEnabled] = useState(true)
  const [cliVersionSyncIntervalHours, setCliVersionSyncIntervalHours] = useState(12)
  const [firstTokenTimeoutSeconds, setFirstTokenTimeoutSeconds] = useState(120)
  const [streamKeepaliveEnabled, setStreamKeepaliveEnabled] = useState(true)
  const [syncedCliVersion, setSyncedCliVersion] = useState('')
  const [effectiveCliVersion, setEffectiveCliVersion] = useState('')
  const [syncingCliVersion, setSyncingCliVersion] = useState(false)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let cancelled = false
    void api
      .getClaudeConfig()
      .then((cfg) => {
        if (cancelled) return
        setFingerprintMode((cfg.fingerprint_mode as 'preserve' | 'force' | '') ?? '')
        setClientPlatform(cfg.client_platform ?? 'any')
        setVersionPolicy(cfg.version_policy ?? 'passthrough')
        setClientVersion(cfg.client_version ?? '')
        setTimezone(cfg.default_timezone ?? '')
        setTimezoneCustom(Boolean(cfg.default_timezone && !findClaudeTimezoneOption(cfg.default_timezone)))
        setSessionWindow(cfg.session_window_limit ? String(cfg.session_window_limit) : '')
        setAllowServiceTier(Boolean(cfg.allow_service_tier))
        setAllowInferenceGeo(Boolean(cfg.allow_inference_geo))
        setAllowSpeed(Boolean(cfg.allow_speed))
        setAllowSafetyIdentifier(Boolean(cfg.allow_safety_identifier))
        setAllowedBetaHeaders((cfg.allowed_beta_headers ?? []).join(', '))
        setMaxOutputTokens(String(cfg.max_output_tokens ?? 0))
        setMaxToolCount(String(cfg.max_tool_count ?? 0))
        setMaxToolSchemaBytes(String(cfg.max_tool_schema_bytes ?? 0))
        setCliVersionSyncEnabled(cfg.cli_version_sync_enabled ?? true)
        setCliVersionSyncIntervalHours(cfg.cli_version_sync_interval_hours || 12)
        setFirstTokenTimeoutSeconds(cfg.first_token_timeout_seconds ?? 120)
        setStreamKeepaliveEnabled(cfg.stream_keepalive_enabled ?? true)
        setSyncedCliVersion(cfg.synced_cli_version ?? '')
        setEffectiveCliVersion(cfg.effective_cli_version ?? cfg.builtin_cli_version ?? '')
      })
      .catch(() => {
        /* 读取失败保持默认空 */
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  const save = useCallback(async () => {
    setSaving(true)
    try {
      const n = Number(sessionWindow.trim())
      const maxOutputValue = Number(maxOutputTokens.trim())
      const maxToolValue = Number(maxToolCount.trim())
      const maxToolSchemaValue = Number(maxToolSchemaBytes.trim())
      await api.updateClaudeConfig({
        fingerprint_mode: fingerprintMode,
        client_platform: clientPlatform,
        version_policy: versionPolicy,
        client_version: versionPolicy === 'passthrough' ? '' : clientVersion.trim(),
        default_timezone: timezone.trim(),
        session_window_limit: Number.isFinite(n) && n > 0 ? Math.floor(n) : 0,
        allow_service_tier: allowServiceTier,
        allow_inference_geo: allowInferenceGeo,
        allow_speed: allowSpeed,
        allow_safety_identifier: allowSafetyIdentifier,
        allowed_beta_headers: allowedBetaHeaders.split(',').map((item) => item.trim()).filter(Boolean),
        max_output_tokens: Number.isFinite(maxOutputValue) && maxOutputValue >= 0 ? Math.floor(maxOutputValue) : 0,
        max_tool_count: Number.isFinite(maxToolValue) && maxToolValue >= 0 ? Math.floor(maxToolValue) : 0,
        max_tool_schema_bytes: Number.isFinite(maxToolSchemaValue) && maxToolSchemaValue >= 0 ? Math.floor(maxToolSchemaValue) : 0,
        cli_version_sync_enabled: cliVersionSyncEnabled,
        cli_version_sync_interval_hours: cliVersionSyncIntervalHours,
        first_token_timeout_seconds: firstTokenTimeoutSeconds,
        stream_keepalive_enabled: streamKeepaliveEnabled,
      })
      showToast(t('settings.claudeSaved'), 'success')
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSaving(false)
    }
  }, [allowInferenceGeo, allowSafetyIdentifier, allowServiceTier, allowSpeed, allowedBetaHeaders, cliVersionSyncEnabled, cliVersionSyncIntervalHours, clientPlatform, clientVersion, fingerprintMode, firstTokenTimeoutSeconds, maxOutputTokens, maxToolCount, maxToolSchemaBytes, sessionWindow, showToast, streamKeepaliveEnabled, t, timezone, versionPolicy])

  const handleSyncClaudeCliVersion = useCallback(async () => {
    setSyncingCliVersion(true)
    try {
      const result = await api.syncClaudeCLIVersion()
      if (result.updated) setSyncedCliVersion(result.fetched_version)
      setEffectiveCliVersion(result.effective_version)
      showToast(t('settings.claudeCliVersionSyncSuccess', { version: result.effective_version, accounts: result.accounts_refreshed }), 'success')
      if (result.warning) {
        showToast(t('settings.claudeCliVersionSyncWarning', { version: result.effective_version, message: result.warning }), 'warning')
      }
    } catch (error) {
      showToast(`${t('settings.claudeCliVersionSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingCliVersion(false)
    }
  }, [showToast, t])

  return (
    <SettingsCard
      title={t('settings.claudeSettingsTitle')}
      description={t('settings.claudeSettingsDesc')}
      icon={<ChannelLogo channel="claude" size={16} />}
      footer={
        <div className="flex justify-end">
          <Button onClick={() => void save()} disabled={loading || saving}>
            {t('common.save')}
          </Button>
        </div>
      }
    >
      <div className={SETTINGS_FIELD_GRID_3}>
        <SettingField label={t('settings.claudeSessionWindow')} description={t('settings.claudeSessionWindowDesc')}>
          <Input
            value={sessionWindow}
            onChange={(e) => setSessionWindow(e.target.value)}
            placeholder={t('settings.claudeFollowGlobal')}
            inputMode="numeric"
          />
        </SettingField>
        <SettingField label={t('settings.claudeFingerprintMode')} description={t('settings.claudeFingerprintModeDesc')}>
          <Select
            value={fingerprintMode}
            onValueChange={(value) => setFingerprintMode(value as 'preserve' | 'force' | '')}
            options={[
              { value: '', label: t('settings.claudeFpPreserve') },
              { value: 'preserve', label: t('settings.claudeFpPreserveExplicit') },
              { value: 'force', label: t('settings.claudeFpForce') },
            ]}
          />
        </SettingField>
        <SettingField label={t('settings.claudeClientPlatform')} description={t('settings.claudeClientPlatformDesc')}>
          <Select
            value={clientPlatform}
            onValueChange={(value) => setClientPlatform(value as 'any' | 'claude_code_cli_only')}
            options={[
              { value: 'any', label: t('settings.claudeClientPlatformAny') },
              { value: 'claude_code_cli_only', label: t('settings.claudeClientPlatformCLIOnly') },
            ]}
          />
        </SettingField>
        <SettingField label={t('settings.claudeVersionPolicy')} description={t('settings.claudeVersionPolicyDesc')}>
          <div className="space-y-1.5">
            <Select
              value={versionPolicy}
              onValueChange={(value) => setVersionPolicy(value as 'passthrough' | 'fixed' | 'minimum')}
              options={[
                { value: 'passthrough', label: t('settings.claudeVersionPolicyPassthrough') },
                { value: 'fixed', label: t('settings.claudeVersionPolicyFixed') },
                { value: 'minimum', label: t('settings.claudeVersionPolicyMinimum') },
              ]}
            />
            {versionPolicy !== 'passthrough' ? <Input value={clientVersion} onChange={(e) => setClientVersion(e.target.value)} placeholder="2.1.251" /> : null}
          </div>
        </SettingField>
        <SettingField label={t('settings.claudeDefaultTimezone')} description={t('settings.claudeDefaultTimezoneDesc')}>
          <div className="space-y-1.5">
            <Select
              value={timezoneCustom ? CLAUDE_TIMEZONE_CUSTOM : (findClaudeTimezoneOption(timezone)?.value ?? (timezone ? CLAUDE_TIMEZONE_CUSTOM : ''))}
              onValueChange={(value) => {
                if (value === CLAUDE_TIMEZONE_CUSTOM) {
                  setTimezoneCustom(true)
                  if (findClaudeTimezoneOption(timezone)) setTimezone('')
                  return
                }
                setTimezoneCustom(false)
                setTimezone(value)
              }}
              options={[
                { value: '', label: t('settings.claudeTimezoneUnset') },
                ...CLAUDE_TIMEZONE_OPTIONS,
                { value: CLAUDE_TIMEZONE_CUSTOM, label: t('settings.claudeTimezoneCustom') },
              ]}
            />
            {timezoneCustom ? <Input value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder="Asia/Shanghai" /> : null}
            {timezone ? <p className="text-[10px] text-muted-foreground">{claudeTimezoneLabel(timezone)}</p> : null}
          </div>
        </SettingField>
        <SettingField label={t('settings.claudeCliVersionSync')} description={t('settings.claudeCliVersionSyncDesc')}>
          <div className="flex items-center gap-2">
            <Button size="sm" variant="outline" onClick={() => void handleSyncClaudeCliVersion()} disabled={syncingCliVersion}>
              <RefreshCw className={cn('size-3.5', syncingCliVersion && 'animate-spin')} />
              {syncingCliVersion ? t('settings.claudeCliVersionSyncing') : t('settings.claudeCliVersionSyncNow')}
            </Button>
            {effectiveCliVersion ? (
              <span className="font-mono text-xs text-muted-foreground">
                {effectiveCliVersion}
                {!syncedCliVersion ? ` · ${t('settings.claudeCliVersionBuiltin')}` : ''}
              </span>
            ) : null}
          </div>
        </SettingField>
        {/* 自动同步开关 + 间隔成对横排，与 Codex 运行时优化保持同一布局 */}
        <div className="sm:col-span-2 grid gap-0 overflow-hidden rounded-lg border border-border/60 bg-muted/15 sm:grid-cols-2 sm:divide-x sm:divide-border/60">
          <div className="flex min-h-[48px] items-center justify-between gap-3 px-3 py-2.5">
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">{t('settings.claudeCliVersionAutoSync')}</span>
              <SettingHelp text={t('settings.claudeCliVersionAutoSyncDesc')} />
            </div>
            <Switch checked={cliVersionSyncEnabled} onCheckedChange={setCliVersionSyncEnabled} />
          </div>
          <div className={cn('flex min-h-[48px] items-center justify-between gap-3 border-t border-border/60 px-3 py-2.5 sm:border-t-0', !cliVersionSyncEnabled && 'opacity-60')}>
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">{t('settings.claudeCliVersionSyncInterval')}</span>
              <SettingHelp text={t('settings.claudeCliVersionSyncIntervalDesc')} />
            </div>
            <div className="relative w-[7.25rem] shrink-0">
              <DraftNumberInput
                min={1}
                max={720}
                className="h-9 pr-10 tabular-nums"
                disabled={!cliVersionSyncEnabled}
                value={cliVersionSyncIntervalHours}
                onValueChange={setCliVersionSyncIntervalHours}
              />
              <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-muted-foreground">h</span>
            </div>
          </div>
        </div>
        {/* 首字超时 + 首字前保活成对横排：两者都只作用于 Claude OAuth 路径 */}
        <div className="sm:col-span-2 grid gap-0 overflow-hidden rounded-lg border border-border/60 bg-muted/15 sm:grid-cols-2 sm:divide-x sm:divide-border/60">
          <div className="flex min-h-[48px] items-center justify-between gap-3 px-3 py-2.5">
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">{t('settings.claudeFirstTokenTimeout')}</span>
              <SettingHelp text={t('settings.claudeFirstTokenTimeoutDesc')} />
            </div>
            <div className="relative w-[7.25rem] shrink-0">
              <DraftNumberInput
                min={0}
                max={600}
                className="h-9 pr-10 tabular-nums"
                value={firstTokenTimeoutSeconds}
                onValueChange={setFirstTokenTimeoutSeconds}
              />
              <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-muted-foreground">s</span>
            </div>
          </div>
          <div className="flex min-h-[48px] items-center justify-between gap-3 border-t border-border/60 px-3 py-2.5 sm:border-t-0">
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">{t('settings.claudeStreamKeepalive')}</span>
              <SettingHelp text={t('settings.claudeStreamKeepaliveDesc')} />
            </div>
            <Switch checked={streamKeepaliveEnabled} onCheckedChange={setStreamKeepaliveEnabled} />
          </div>
        </div>
      </div>
      <details className="mt-4 rounded-lg border border-primary/20 bg-primary/5">
        <summary className="cursor-pointer px-3 py-2.5 text-sm font-semibold text-foreground">
          {t('settings.claudeSecurityTitle')}
        </summary>
        <div className="space-y-4 border-t border-primary/10 px-3 pb-3 pt-3">
          <p className="text-xs leading-relaxed text-muted-foreground">{t('settings.claudeSecurityDesc')}</p>
          <div className={SETTINGS_SWITCH_GRID}>
            <SettingField label={t('settings.claudeAllowServiceTier')} description={t('settings.claudeAllowServiceTierDesc')} layout="switch">
              <Switch checked={allowServiceTier} onCheckedChange={setAllowServiceTier} />
            </SettingField>
            <SettingField label={t('settings.claudeAllowInferenceGeo')} description={t('settings.claudeAllowInferenceGeoDesc')} layout="switch">
              <Switch checked={allowInferenceGeo} onCheckedChange={setAllowInferenceGeo} />
            </SettingField>
            <SettingField label={t('settings.claudeAllowSpeed')} description={t('settings.claudeAllowSpeedDesc')} layout="switch">
              <Switch checked={allowSpeed} onCheckedChange={setAllowSpeed} />
            </SettingField>
            <SettingField label={t('settings.claudeAllowSafetyIdentifier')} description={t('settings.claudeAllowSafetyIdentifierDesc')} layout="switch">
              <Switch checked={allowSafetyIdentifier} onCheckedChange={setAllowSafetyIdentifier} />
            </SettingField>
          </div>
          <div className={SETTINGS_FIELD_GRID}>
            <SettingField label={t('settings.claudeAllowedBetaHeaders')} description={t('settings.claudeAllowedBetaHeadersDesc')}>
              <Input value={allowedBetaHeaders} onChange={(event) => setAllowedBetaHeaders(event.target.value)} placeholder="token-efficient-tools-2025-02-19" />
            </SettingField>
            <SettingField label={t('settings.claudeMaxOutputTokens')} description={t('settings.claudeMaxOutputTokensDesc')}>
              <Input value={maxOutputTokens} onChange={(event) => setMaxOutputTokens(event.target.value)} inputMode="numeric" min={0} type="number" placeholder={t('settings.claudeUnlimitedPlaceholder')} />
            </SettingField>
            <SettingField label={t('settings.claudeMaxToolCount')} description={t('settings.claudeMaxToolCountDesc')}>
              <Input value={maxToolCount} onChange={(event) => setMaxToolCount(event.target.value)} inputMode="numeric" min={0} type="number" />
            </SettingField>
            <SettingField label={t('settings.claudeMaxToolSchemaBytes')} description={t('settings.claudeMaxToolSchemaBytesDesc')}>
              <Input value={maxToolSchemaBytes} onChange={(event) => setMaxToolSchemaBytes(event.target.value)} inputMode="numeric" min={0} type="number" />
            </SettingField>
          </div>
        </div>
      </details>
    </SettingsCard>
  )
}

function SettingsCard({
  title,
  description,
  children,
  className,
  contentClassName,
  footer,
  icon,
  badge,
  channels,
  tone = 'default',
}: {
  title: string
  description?: string
  children: ReactNode
  className?: string
  contentClassName?: string
  footer?: ReactNode
  icon?: ReactNode
  badge?: ReactNode
  channels?: readonly UpstreamChannel[]
  tone?: 'default' | 'danger'
}) {
  return (
    <Card
      className={cn(
        'gap-0 py-0 border-border/60 bg-card shadow-2xs',
        tone === 'danger' && 'border-destructive/30 bg-destructive/[0.02]',
        className,
      )}
    >
      <CardContent className={cn('p-4.5 sm:p-5.5', contentClassName)}>
        <div className="mb-4.5 flex shrink-0 items-start gap-3">
          {icon ? (
            <div
              className={cn(
                'flex size-8 shrink-0 items-center justify-center rounded-lg ring-1 ring-inset',
                tone === 'danger'
                  ? 'bg-destructive/10 text-destructive ring-destructive/20'
                  : 'bg-muted/70 text-muted-foreground ring-border/60',
              )}
              aria-hidden="true"
            >
              <span className="[&_svg]:size-4">{icon}</span>
            </div>
          ) : null}
          <div className="min-w-0 flex-1 pt-0.5">
            <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
              <h3 className="text-sm font-semibold leading-snug tracking-tight text-foreground sm:text-[15px]">
                {title}
              </h3>
              {badge}
              {channels && channels.length > 0 ? <ChannelScopeBadges channels={channels} /> : null}
            </div>
            {description ? (
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground/90">{description}</p>
            ) : null}
          </div>
        </div>
        {children}
        {footer ? <div className="mt-4.5 border-t border-border/60 pt-4 sm:mt-5">{footer}</div> : null}
      </CardContent>
    </Card>
  )
}

function SettingsCollapsibleNote({
  title,
  children,
}: {
  title: string
  children: ReactNode
}) {
  return (
    <details
      role="note"
      className="group overflow-hidden rounded-lg border border-primary/20 bg-primary/5"
    >
      <summary className="flex cursor-pointer list-none items-center gap-2.5 px-3 py-2.5 marker:content-none transition-colors hover:bg-primary/[0.06] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 [&::-webkit-details-marker]:hidden">
        <CircleHelp className="size-4 shrink-0 text-primary" aria-hidden="true" />
        <span className="min-w-0 flex-1 text-xs font-semibold text-foreground">
          {title}
        </span>
        <ChevronDown
          className="size-3.5 shrink-0 text-muted-foreground transition-transform group-open:rotate-180"
          aria-hidden="true"
        />
      </summary>
      <div className="border-t border-primary/10 px-3 pb-2.5 pt-2">
        <p className="text-[11px] leading-relaxed text-muted-foreground sm:text-xs">
          {children}
        </p>
      </div>
    </details>
  )
}

function SettingHelp({ text }: { text: string }) {
  return (
    <TooltipProvider delayDuration={200}>
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            className="inline-flex size-4 shrink-0 items-center justify-center rounded-full text-muted-foreground/80 transition-colors hover:bg-muted hover:text-foreground"
            aria-label={text}
          >
            <CircleHelp className="size-3.5" />
          </button>
        </TooltipTrigger>
        <TooltipContent
          side="top"
          sideOffset={6}
          className="max-w-[280px] bg-popover px-3 py-2 text-left text-xs leading-relaxed text-popover-foreground shadow-md"
        >
          {text}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}

function SettingField({
  label,
  description,
  help,
  warning,
  children,
  className,
  layout = 'stack',
  suffix,
  channels,
  stretch = false,
}: {
  label: string
  description?: string
  // row 布局下 description 直接外显，help 才进问号 tooltip；其他布局 help 与 description 合并进 tooltip。
  help?: string
  warning?: string
  // stretch:stack 布局下让控件撑满剩余高度(等高卡片里的 textarea)。
  stretch?: boolean
  children: ReactNode
  className?: string
  layout?: 'stack' | 'switch' | 'row'
  suffix?: string
  channels?: readonly UpstreamChannel[]
}) {
  const scope = channels && channels.length > 0 ? <ChannelScopeBadges channels={channels} size="xs" /> : null
  const control = suffix ? (
    <div className="relative min-w-0">
      <div className="[&_[data-slot=input]]:pr-11 [&_[data-slot=select-trigger]]:pr-11 [&_input]:pr-11">
        {children}
      </div>
      <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium tabular-nums text-muted-foreground">
        {suffix}
      </span>
    </div>
  ) : (
    children
  )

  if (layout === 'row') {
    return (
      <div className={cn('flex min-w-0 items-start justify-between gap-4 py-4 first:pt-0 last:pb-0', className)}>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <label className="text-[13px] font-semibold leading-snug text-foreground sm:text-sm">{label}</label>
            {help ? <SettingHelp text={help} /> : null}
            {scope}
          </div>
          {description ? (
            <p className="max-w-3xl text-xs leading-relaxed text-muted-foreground">{description}</p>
          ) : null}
          {warning ? (
            <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">{warning}</p>
          ) : null}
        </div>
        <div className="flex shrink-0 items-center pt-0.5">{control}</div>
      </div>
    )
  }

  const tooltip = [description, help].filter(Boolean).join(' ')

  if (layout === 'switch') {
    return (
      <div
        className={cn(
          'flex min-h-[52px] min-w-0 items-center justify-between gap-3 rounded-xl border border-border/70 bg-card p-3.5 shadow-2xs transition-colors hover:border-border/90',
          className,
        )}
      >
        <div className="min-w-0 flex-1 space-y-0.5">
          <div className="flex items-center gap-1.5">
            <label className="block text-[13px] font-semibold leading-snug text-foreground sm:text-sm">
              {label}
            </label>
            {tooltip ? <SettingHelp text={tooltip} /> : null}
            {scope}
          </div>
          {warning ? (
            <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">
              {warning}
            </p>
          ) : null}
        </div>
        <div className="flex shrink-0 items-center self-center">{control}</div>
      </div>
    )
  }

  return (
    <div className={cn('flex min-w-0 flex-col gap-1.5', stretch && 'flex-1', className)}>
      <div className="flex min-h-5 items-center gap-1.5">
        <label className="block text-[13px] font-semibold leading-none text-foreground sm:text-sm">
          {label}
        </label>
        {tooltip ? <SettingHelp text={tooltip} /> : null}
        {scope}
      </div>
      <div className={cn('min-w-0', stretch && 'flex flex-1 flex-col [&>*]:flex-1')}>{control}</div>
      {warning ? (
        <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">
          {warning}
        </p>
      ) : null}
    </div>
  )
}

function SettingsSkeleton() {
  return (
    <div className="space-y-6" aria-busy="true" aria-live="polite">
      <div className="space-y-2">
        <div className="h-8 w-40 animate-pulse rounded-lg bg-muted" />
        <div className="h-4 w-72 max-w-full animate-pulse rounded-md bg-muted/70" />
      </div>
      <div className="h-11 w-full animate-pulse rounded-full bg-muted" />
      <div className="grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="h-[72px] animate-pulse rounded-lg border border-border bg-muted/40" />
        ))}
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        {[0, 1].map((i) => (
          <Card key={i} className="gap-0 py-0">
            <CardContent className="space-y-3 p-5">
              <div className="h-4 w-28 animate-pulse rounded bg-muted" />
              <div className="grid grid-cols-2 gap-3">
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/70" />
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/60" />
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/50" />
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/40" />
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  )
}

function ModelSummaryCard({
  title,
  description,
  meta,
  onOpen,
  openLabel,
}: {
  title: string
  description: string
  meta: string
  onOpen: () => void
  openLabel: string
}) {
  return (
    <button
      type="button"
      onClick={onOpen}
      className="group flex w-full items-start gap-3.5 rounded-xl border border-border/70 bg-card p-4 text-left shadow-2xs transition-all hover:border-primary/40 hover:bg-muted/10 hover:shadow-xs"
    >
      <div className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted/70 text-muted-foreground ring-1 ring-border/60 transition-colors group-hover:bg-primary/10 group-hover:text-primary group-hover:ring-primary/20">
        <Layers className="size-4" />
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex items-start justify-between gap-2">
          <div className="min-w-0">
            <div className="text-sm font-semibold leading-snug text-foreground">{title}</div>
            <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-muted-foreground">
              {description}
            </p>
          </div>
          <ChevronRight className="mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5 group-hover:text-primary" />
        </div>
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <Badge variant="secondary" className="text-xs font-semibold tabular-nums">
            {meta}
          </Badge>
          <span className="text-xs font-semibold text-primary group-hover:underline">{openLabel}</span>
        </div>
      </div>
    </button>
  )
}

function StatusTile({
  label,
  children,
  icon,
}: {
  label: string
  children: ReactNode
  icon?: ReactNode
}) {
  return (
    <div
      data-slot="status-tile"
      className="flex min-h-[80px] flex-col justify-between gap-2.5 rounded-xl border border-border/70 bg-gradient-to-br from-card via-card to-muted/20 p-3.5 shadow-2xs transition-all hover:border-border"
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-[11px] font-semibold uppercase tracking-wider text-muted-foreground/90">
          {label}
        </span>
        {icon ? <span className="text-muted-foreground/70 [&_svg]:size-4">{icon}</span> : null}
      </div>
      <div className="flex min-h-6 items-center text-sm font-bold tabular-nums text-foreground">
        {children}
      </div>
    </div>
  )
}

function SegmentedPillGroup<T extends string>({
  value,
  onChange,
  options,
  disabled = false,
  className,
}: {
  value: T
  onChange: (value: T) => void
  options: Array<{ label: string; value: T; icon?: ReactNode }>
  disabled?: boolean
  className?: string
}) {
  const activeIndex = options.findIndex((opt) => opt.value === value)
  const count = options.length

  return (
    <div
      className={cn(
        'relative flex items-center rounded-xl border border-border/70 bg-muted/35 p-1 shadow-2xs select-none',
        className,
      )}
    >
      {/* 平滑滑块背景 indicator */}
      {activeIndex >= 0 && count > 0 ? (
        <div
          className="absolute inset-y-1 rounded-lg bg-background shadow-xs ring-1 ring-border/60 transition-all duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
          style={{
            width: `calc((100% - 0.5rem) / ${count})`,
            left: `calc(0.25rem + ${activeIndex} * ((100% - 0.5rem) / ${count}))`,
          }}
        />
      ) : null}

      {/* 选项按钮 */}
      <div className="relative z-10 flex w-full items-center">
        {options.map((opt) => {
          const active = opt.value === value
          return (
            <button
              key={opt.value}
              type="button"
              disabled={disabled}
              onClick={() => onChange(opt.value)}
              className={cn(
                'flex flex-1 items-center justify-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-semibold transition-colors duration-200 active:scale-98',
                active
                  ? 'text-foreground font-bold'
                  : 'text-muted-foreground hover:text-foreground',
                disabled && 'opacity-50 cursor-not-allowed',
              )}
            >
              {opt.icon ? <span className="[&_svg]:size-3.5">{opt.icon}</span> : null}
              <span className="whitespace-nowrap">{opt.label}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}

function SettingsSection({
  id,
  title,
  description,
  icon,
  children,
}: {
  id: string
  title: string
  description?: string
  icon?: ReactNode
  children: ReactNode
}) {
  return (
    <section id={id} data-settings-section={id} className="scroll-mt-32 space-y-4">
      <div className="space-y-1 px-0.5">
        <div className="flex items-center gap-2.5">
          {icon ? (
            <span className="shrink-0 text-muted-foreground [&_svg]:size-4" aria-hidden="true">
              {icon}
            </span>
          ) : null}
          <h2 className="text-[15px] font-semibold tracking-tight text-foreground sm:text-base">{title}</h2>
          <div className="ml-1 h-px flex-1 bg-border/60" />
        </div>
        {description ? (
          <p className="max-w-3xl text-xs leading-relaxed text-muted-foreground">{description}</p>
        ) : null}
      </div>
      <div className="space-y-4">{children}</div>
    </section>
  )
}

type SettingsSectionIndexItem = { id: string; label: string; icon: ReactNode }

// 按滚动位置算出当前分区：最后一个顶部越过判定线的分区即当前分区。
// 点击目录后先锁定所选分区，直到用户手动滚动（滚轮/触摸/键盘）才恢复按位置判定——
// 页尾几个分区挤在同一屏时，滚动位置分不出用户点的是哪一个。
// 滚到底的兜底只在末尾分区真正占据视口下半部分时才把高亮给它，否则会抢走倒数第二个分区。
function useActiveSettingsSection(sectionIds: readonly string[]) {
  const [activeId, setActiveId] = useState<string | null>(sectionIds[0] ?? null)
  const pinnedRef = useRef<string | null>(null)
  const pinSection = useCallback((id: string) => {
    pinnedRef.current = id
    setActiveId(id)
  }, [])
  useEffect(() => {
    pinnedRef.current = null
    setActiveId(sectionIds[0] ?? null)
    if (sectionIds.length < 2) return
    let frame = 0
    const update = () => {
      frame = 0
      if (pinnedRef.current) return
      let current = sectionIds[0]
      for (const id of sectionIds) {
        const el = document.getElementById(id)
        if (el && el.getBoundingClientRect().top <= SETTINGS_SECTION_SPY_OFFSET_PX) current = id
      }
      const doc = document.documentElement
      const atBottom = window.innerHeight + window.scrollY >= doc.scrollHeight - 2
      if (atBottom) {
        const lastId = sectionIds[sectionIds.length - 1]
        const last = document.getElementById(lastId)
        if (last && last.getBoundingClientRect().top <= window.innerHeight / 2) current = lastId
      }
      setActiveId(current)
    }
    const schedule = () => {
      if (!frame) frame = window.requestAnimationFrame(update)
    }
    const unpin = () => {
      if (!pinnedRef.current) return
      pinnedRef.current = null
      schedule()
    }
    update()
    window.addEventListener('scroll', schedule, { passive: true })
    window.addEventListener('resize', schedule)
    window.addEventListener('wheel', unpin, { passive: true })
    window.addEventListener('touchstart', unpin, { passive: true })
    window.addEventListener('keydown', unpin)
    return () => {
      if (frame) window.cancelAnimationFrame(frame)
      window.removeEventListener('scroll', schedule)
      window.removeEventListener('resize', schedule)
      window.removeEventListener('wheel', unpin)
      window.removeEventListener('touchstart', unpin)
      window.removeEventListener('keydown', unpin)
    }
  }, [sectionIds])
  return { activeId, pinSection }
}

// Tab 内分区目录：Tab 栏下方居中的磨砂玻璃胶囊条，随 Tab 栏一起粘顶，按滚动位置高亮当前分区。
function SettingsSectionIndex({
  items,
  activeId,
  label,
  onSelect,
}: {
  items: readonly SettingsSectionIndexItem[]
  activeId: string | null
  label: string
  onSelect: (id: string) => void
}) {
  return (
    <nav aria-label={label} className="flex justify-center">
      <div
        className={cn(
          'flex max-w-full items-center gap-1 overflow-x-auto rounded-full border border-white/50 bg-card/55 p-1 backdrop-blur-xl backdrop-saturate-150 dark:border-white/10 dark:bg-card/45',
          'shadow-[0_8px_30px_rgb(0_0_0/0.06)] ring-1 ring-black/[0.04] dark:ring-white/[0.05]',
          '[-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
        )}
      >
        {items.map((item) => {
          const active = item.id === activeId
          return (
            <button
              key={item.id}
              type="button"
              onClick={() => onSelect(item.id)}
              aria-current={active ? 'location' : undefined}
              className={cn(
                'inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full px-3 py-1.5 text-xs font-semibold transition-colors duration-200 [&_svg]:size-3.5',
                active
                  ? 'bg-primary/12 text-primary shadow-2xs ring-1 ring-primary/15'
                  : 'text-muted-foreground hover:bg-background/70 hover:text-foreground',
              )}
            >
              <span className={cn('shrink-0', active ? 'opacity-100' : 'opacity-75')} aria-hidden="true">
                {item.icon}
              </span>
              {item.label}
            </button>
          )
        })}
      </div>
    </nav>
  )
}

const VISIBLE_CHANNEL_LABELS: Record<UpstreamChannel, string> = {
  codex: 'Codex',
  claude: 'Claude',
  antigravity: 'Antigravity',
  grok: 'Grok',
}

// 供应商显示选择器：一排可多选的胶囊，点一下即保存；兜底渠道锁定在选中态。
function VisibleChannelsPicker() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const { channels, saveChannels } = useVisibleChannels()
  const [saving, setSaving] = useState(false)
  const toggle = async (channel: UpstreamChannel) => {
    if (channel === FALLBACK_VISIBLE_CHANNEL || saving) return
    setSaving(true)
    try {
      await saveChannels(toggleVisibleChannel(channels, channel))
      showToast(t('settings.autoSaved'), 'success', AUTO_SAVE_TOAST_MS)
    } catch (error) {
      showToast(`${t('settings.visibleChannelsSaveFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSaving(false)
    }
  }
  return (
    <div className="space-y-2.5">
      <div
        role="group"
        aria-label={t('settings.visibleChannelsTitle')}
        className="flex flex-wrap items-center gap-1.5 rounded-xl border border-border/70 bg-muted/35 p-1.5"
      >
        {ALL_VISIBLE_CHANNEL_OPTIONS.map((channel) => {
          const selected = channels.includes(channel)
          const locked = channel === FALLBACK_VISIBLE_CHANNEL
          return (
            <button
              key={channel}
              type="button"
              aria-pressed={selected}
              aria-disabled={locked || undefined}
              disabled={saving}
              title={locked ? t('settings.visibleChannelsFallbackHint') : undefined}
              onClick={() => void toggle(channel)}
              className={cn(
                'inline-flex items-center gap-2 rounded-lg px-3.5 py-2 text-sm font-semibold transition-colors duration-200',
                selected
                  ? 'bg-primary text-primary-foreground shadow-2xs'
                  : 'text-muted-foreground hover:bg-muted/70 hover:text-foreground',
                locked && 'cursor-default',
                saving && 'opacity-70',
              )}
            >
              <span className={cn('inline-flex shrink-0', !selected && 'opacity-75 grayscale')}>
                <ChannelLogo channel={channel} size={16} />
              </span>
              {VISIBLE_CHANNEL_LABELS[channel]}
            </button>
          )
        })}
      </div>
      <p className="text-xs leading-relaxed text-muted-foreground">{t('settings.visibleChannelsFallbackHint')}</p>
    </div>
  )
}

// 页头保存状态：自动保存进行中 > 手动字段未保存 > 自动保存失败 > 已保存。
function SaveStatusPill({
  autoSaveStatus,
  dirtyCount,
}: {
  autoSaveStatus: AutoSaveStatus
  dirtyCount: number
}) {
  const { t } = useTranslation()
  let tone = 'text-muted-foreground'
  let icon: ReactNode = <Check className="size-3.5" />
  let text = t('settings.saveStatusSaved')
  let title: string | undefined
  if (autoSaveStatus === 'saving') {
    icon = <Loader2 className="size-3.5 animate-spin" />
    text = t('settings.autoSaving')
  } else if (dirtyCount > 0) {
    tone = 'text-amber-700 dark:text-amber-300'
    icon = <span className="size-1.5 rounded-full bg-amber-500" aria-hidden="true" />
    text = t('settings.saveStatusUnsaved', { n: dirtyCount })
    title = t('settings.saveStatusUnsavedHint')
  } else if (autoSaveStatus === 'error') {
    tone = 'text-destructive'
    icon = <CircleAlert className="size-3.5" />
    text = t('settings.autoSaveFailed')
  } else if (autoSaveStatus === 'saved') {
    tone = 'text-emerald-700 dark:text-emerald-300'
    text = t('settings.autoSaved')
  }
  return (
    <span
      data-slot="save-status"
      title={title}
      aria-live="polite"
      className={cn('inline-flex h-8 items-center gap-1.5 whitespace-nowrap px-1 text-xs font-medium tabular-nums', tone)}
    >
      {icon}
      {text}
    </span>
  )
}

const SITE_LOGO_MAX_BYTES = 600 * 1024
const SITE_LOGO_CANVAS_SIZE = 80
const BACKGROUND_IMAGE_UPLOAD_MAX_BYTES = 20 * 1024 * 1024
const BACKGROUND_VIDEO_UPLOAD_MAX_BYTES = 40 * 1024 * 1024

function formatBytesKB(bytes: number) {
  return Math.max(1, Math.round(bytes / 1024))
}

function getSiteLogoMimeType(file: File) {
  const type = file.type.toLowerCase()
  const name = file.name.toLowerCase()
  if (type === 'image/png' || name.endsWith('.png')) return 'image/png'
  if (type === 'image/jpeg' || name.endsWith('.jpg') || name.endsWith('.jpeg')) return 'image/jpeg'
  if (type === 'image/svg+xml' || name.endsWith('.svg')) return 'image/svg+xml'
  return ''
}

function getBackgroundImageMimeType(file: File) {
  const type = file.type.toLowerCase()
  const name = file.name.toLowerCase()
  if (type === 'image/png' || name.endsWith('.png')) return 'image/png'
  if (type === 'image/jpeg' || name.endsWith('.jpg') || name.endsWith('.jpeg')) return 'image/jpeg'
  if (type === 'image/webp' || name.endsWith('.webp')) return 'image/webp'
  if (type === 'image/svg+xml' || name.endsWith('.svg')) return 'image/svg+xml'
  if (type === 'video/mp4' || name.endsWith('.mp4')) return 'video/mp4'
  return ''
}

function dataURLByteLength(dataURL: string) {
  const commaIndex = dataURL.indexOf(',')
  if (commaIndex === -1) return new Blob([dataURL]).size
  const meta = dataURL.slice(0, commaIndex)
  const data = dataURL.slice(commaIndex + 1)
  if (meta.endsWith(';base64')) {
    const padding = data.endsWith('==') ? 2 : data.endsWith('=') ? 1 : 0
    return Math.floor((data.length * 3) / 4) - padding
  }
  return new Blob([decodeURIComponent(data)]).size
}

function readFileAsDataURL(file: File) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(typeof reader.result === 'string' ? reader.result : '')
    reader.onerror = reject
    reader.readAsDataURL(file)
  })
}

function textToBase64(value: string) {
  const bytes = new TextEncoder().encode(value)
  let binary = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.slice(i, i + 0x8000))
  }
  return btoa(binary)
}

function minifySVG(value: string) {
  // 循环剥离注释直到不动点：单次替换可能因相邻片段重新拼出 "<!--" 而残留
  let out = value
  for (let prev = ''; prev !== out; ) {
    prev = out
    out = out.replace(/<!--[\s\S]*?-->/g, '').replace(/<!--/g, '')
  }
  return out
    .replace(/>\s+</g, '><')
    .replace(/\s{2,}/g, ' ')
    .trim()
}

function loadImage(src: string) {
  return new Promise<HTMLImageElement>((resolve, reject) => {
    const image = new Image()
    image.onload = () => resolve(image)
    image.onerror = reject
    image.src = src
  })
}

function canvasToBlob(canvas: HTMLCanvasElement, type: string, quality?: number) {
  return new Promise<Blob>((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob)
      else reject(new Error('canvas-to-blob-failed'))
    }, type, quality)
  })
}

async function blobToDataURL(blob: Blob) {
  return readFileAsDataURL(new File([blob], 'site-logo', { type: blob.type }))
}

async function compressImageSourceToDataURL(src: string, mimeType: string) {
  const image = await loadImage(src)
  const canvas = document.createElement('canvas')
  canvas.width = SITE_LOGO_CANVAS_SIZE
  canvas.height = SITE_LOGO_CANVAS_SIZE
  const ctx = canvas.getContext('2d')
  if (!ctx) throw new Error('canvas-context-unavailable')

  const outputType = mimeType === 'image/jpeg' ? 'image/jpeg' : 'image/png'
  if (outputType === 'image/jpeg') {
    ctx.fillStyle = '#ffffff'
    ctx.fillRect(0, 0, canvas.width, canvas.height)
  } else {
    ctx.clearRect(0, 0, canvas.width, canvas.height)
  }

  const sourceWidth = image.naturalWidth || image.width || SITE_LOGO_CANVAS_SIZE
  const sourceHeight = image.naturalHeight || image.height || SITE_LOGO_CANVAS_SIZE
  const scale = Math.min(canvas.width / sourceWidth, canvas.height / sourceHeight)
  const drawWidth = Math.max(1, Math.round(sourceWidth * scale))
  const drawHeight = Math.max(1, Math.round(sourceHeight * scale))
  const dx = Math.round((canvas.width - drawWidth) / 2)
  const dy = Math.round((canvas.height - drawHeight) / 2)
  ctx.drawImage(image, dx, dy, drawWidth, drawHeight)

  if (outputType === 'image/png') {
    const blob = await canvasToBlob(canvas, outputType)
    return blobToDataURL(blob)
  }

  const qualities = [0.86, 0.72, 0.6, 0.48, 0.36]
  let bestDataURL = ''
  for (const quality of qualities) {
    const blob = await canvasToBlob(canvas, outputType, quality)
    const dataURL = await blobToDataURL(blob)
    bestDataURL = dataURL
    if (dataURLByteLength(dataURL) <= SITE_LOGO_MAX_BYTES) return dataURL
  }
  return bestDataURL
}

async function compressSiteLogoFile(file: File, mimeType: string) {
  if (mimeType === 'image/svg+xml') {
    const minified = minifySVG(await file.text())
    const svgDataURL = `data:image/svg+xml;base64,${textToBase64(minified)}`
    if (dataURLByteLength(svgDataURL) <= SITE_LOGO_MAX_BYTES) return svgDataURL
    return compressImageSourceToDataURL(svgDataURL, mimeType)
  }

  const objectURL = URL.createObjectURL(file)
  try {
    return await compressImageSourceToDataURL(objectURL, mimeType)
  } finally {
    URL.revokeObjectURL(objectURL)
  }
}

export default function Settings() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { applyBranding } = useBranding()
  const defaultClaudeModelMappingEntries = useMemo(() => getDefaultModelMappingEntries(), [])
  const schedulerModeOptions = [
    { label: t('settings.schedulerModeRoundRobin'), value: 'round_robin' },
    { label: t('settings.schedulerModeRemainingQuota'), value: 'remaining_quota' },
    { label: t('settings.schedulerModeFillFirst'), value: 'fill_first' },
  ]
  const schedulerEngineOptions = [
    { label: t('settings.schedulerEngineLegacy'), value: 'legacy' },
    { label: t('settings.schedulerEngineShadow'), value: 'shadow' },
    { label: t('settings.schedulerEngineIndexed'), value: 'indexed' },
  ]
  const schedulerEngineExplanations = [
    {
      label: t('settings.schedulerEngineLegacy'),
      value: 'legacy',
      description: t('settings.schedulerEngineLegacyDesc'),
    },
    {
      label: t('settings.schedulerEngineShadow'),
      value: 'shadow',
      description: t('settings.schedulerEngineShadowDesc'),
    },
    {
      label: t('settings.schedulerEngineIndexed'),
      value: 'indexed',
      description: t('settings.schedulerEngineIndexedDesc'),
    },
  ]
  const transportRetryPolicyOptions = [
    { label: t('settings.transportRetryPolicyRotate'), value: 'rotate' },
    { label: t('settings.transportRetryPolicySticky'), value: 'sticky' },
  ]
  const continuousRetryCategoryOptions = [
    { label: t('settings.continuousRetryCategoryTransport'), value: 'transport' },
    { label: t('settings.continuousRetryCategory429'), value: 'http_429' },
    { label: t('settings.continuousRetryCategory4xx'), value: 'http_4xx' },
    { label: t('settings.continuousRetryCategory5xx'), value: 'http_5xx' },
    { label: t('settings.continuousRetryCategoryStream'), value: 'stream_error' },
    { label: t('settings.continuousRetryCategoryResponseFailed'), value: 'response_failed', channels: CHANNELS_CODEX_ONLY },
    { label: t('settings.continuousRetryCategoryContext'), value: 'context_error' },
  ]
  const codexFingerprintDefaultModeOptions = [
    { label: t('accounts.codexFingerprintModeOff'), value: 'off' },
    { label: t('accounts.codexFingerprintModeDevice'), value: 'device' },
    { label: t('accounts.codexFingerprintModeSession'), value: 'session' },
    { label: t('accounts.codexFingerprintModeFull'), value: 'full' },
  ]
  const modelCooldownModeOptions = [
    { label: t('settings.modelCooldownModeOff'), value: 'off' },
    { label: t('settings.modelCooldownModeFixed'), value: 'fixed' },
    { label: t('settings.modelCooldownModeAdaptive'), value: 'adaptive' },
  ]
  const responseCacheWritePolicyOptions = [
    { label: t('settings.responseCache.writePolicyAlways'), value: 'always' },
    { label: t('settings.responseCache.writePolicyOnDemand'), value: 'on_demand' },
  ]
  const affinityModeOptions = [
    { label: t('settings.affinityModeBounded'), value: 'bounded' },
    { label: t('settings.affinityModeOff'), value: 'off' },
    { label: t('settings.affinityModeStrict'), value: 'strict' },
  ]
  const grokAffinityModeOptions = [
    { label: t('settings.grokAffinityModeStrict'), value: 'strict' },
    { label: t('settings.grokAffinityModeFollow'), value: 'follow' },
    { label: t('settings.affinityModeBounded'), value: 'bounded' },
    { label: t('settings.affinityModeOff'), value: 'off' },
  ]
  const grokFollowUpEffortOptions = [
    { label: t('settings.grokFollowUpEffortLow'), value: 'low' },
    { label: t('settings.grokFollowUpEffortMedium'), value: 'medium' },
    { label: t('settings.grokFollowUpEffortHigh'), value: 'high' },
  ]
  const grokQualityGuardOnExhaustedOptions = [
    { label: t('settings.grokQualityGuardFailClosed'), value: 'fail_closed' },
    { label: t('settings.grokQualityGuardFailOpen'), value: 'fail_open' },
  ]
  const clientCompatOptions = [
    { label: t('settings.clientCompatPreserve'), value: 'preserve' },
    { label: t('settings.clientCompatAuto'), value: 'auto' },
    { label: t('settings.clientCompatForce'), value: 'force' },
  ]
  const usageLogModeOptions = [
    { label: t('settings.usageLogFull'), value: 'full' },
    { label: t('settings.usageLogErrors'), value: 'errors' },
    { label: t('settings.usageLogOff'), value: 'off' },
  ]
  const billingTierPolicyOptions = [
    { label: t('settings.billingTierPolicyActual'), value: 'actual' },
    { label: t('settings.billingTierPolicyRequested'), value: 'requested' },
  ]
  const streamFlushPolicyOptions = [
    { label: t('settings.streamFlushImmediate'), value: 'immediate' },
    { label: t('settings.streamFlushCoalesce'), value: 'coalesce' },
  ]
  const firstTokenModeOptions = [
    { label: t('settings.firstTokenModeStrict'), value: 'strict' },
    { label: t('settings.firstTokenModeLoose'), value: 'loose' },
  ]
  const imageStorageBackendOptions = [
    { label: t('settings.imageStorageLocal'), value: 'local' },
    { label: t('settings.imageStorageS3'), value: 's3' },
  ]
  const normalizeLazySettingsForm = useCallback((settings: SystemSettings): SystemSettings => {
    const cacheNormalized = normalizeResponseCacheSettings(settings)
    const normalized = {
      ...cacheNormalized,
      billing_tier_policy: normalizeBillingTierPolicyValue(cacheNormalized.billing_tier_policy),
      first_token_mode: normalizeFirstTokenModeValue(cacheNormalized.first_token_mode),
      models_list_read_max_bytes:
        Number.isFinite(cacheNormalized.models_list_read_max_bytes) && cacheNormalized.models_list_read_max_bytes >= MIB
          ? cacheNormalized.models_list_read_max_bytes
          : DEFAULT_MODELS_LIST_READ_MAX_BYTES,
    }
    if (!normalized.lazy_mode) {
      return normalized
    }
    return {
      ...normalized,
      auto_clean_full_usage: false,
    }
  }, [])
  const [settingsForm, setSettingsForm] = useState<SystemSettings>({
    site_name: 'CodexProxy',
    site_logo: '',
    background_image: '',
    background_opacity: 18,
    background_blur: 0,
    background_glass_opacity: 58,
    background_glass_blur: 5,
    max_concurrency: 2,
    global_rpm: 0,
    test_model: '',
    test_content: 'hi',
    test_concurrency: 50,
	    background_refresh_interval_minutes: 2,
	    usage_probe_max_age_minutes: 10,
	    usage_probe_concurrency: 16,
	    usage_probe_responses_fallback_enabled: true,
	    recovery_probe_interval_minutes: 30,
    lazy_mode: false,
    pg_max_conns: 50,
    redis_pool_size: 30,
    auto_clean_unauthorized: false,
    auto_clean_rate_limited: false,
    auto_clean_error: false,
    auto_clean_expired: false,
    admin_secret: '',
    admin_auth_source: 'disabled',
    auto_clean_full_usage: false,
    proxy_pool_enabled: false,
    fast_scheduler_enabled: false,
    scheduler_engine: 'legacy',
    auto_reset_credits_enabled: false,
    auto_reset_credits_before_expiry_min: 60,
    auto_activate_5h_window_enabled: false,
    codex_force_websocket: false,
    codex_request_compression: true,
    codex_ws_weak_network_mode: false,
    codex_ws_keepalive_enabled: false,
    codex_ws_keepalive_interval_sec: 60,
    codex_ws_hide_upstream_errors: true,
    codex_ws_silent_retry_enabled: true,
    codex_ws_silent_max_retries: 2,
    codex_ws_size_router_enabled: true,
    codex_ws_busy_acquire_max_wait_sec: 30,
    codex_ws_busy_overflow_enabled: false,
    codex_ws_busy_patience_sec: 2,
    codex_ws_stateless_slots: 8,
    github_token_configured: false,
    github_proxy_url: '',
    codex_overload_pause_enabled: false,
    codex_overload_threshold_percent: 20,
    codex_overload_pause_minutes: 30,
    codex_overload_window_minutes: 5,
    codex_continue_thinking_enabled: false,
    overflow_auto_compact_enabled: false,
    compact_via_responses_enabled: false,
    codex_preflight_sse_passthrough_enabled: false,
    codex_continue_max_rounds: 8,
    utls_shutdown_timeout_minutes: 30,
    scheduler_mode: 'round_robin',
    affinity_mode: 'bounded',
    session_affinity_spread: false,
    session_slot_buffer_enabled: false,
    session_slot_buffer_seconds: 10,
    grok_affinity_mode: 'strict',
    grok_probe_enabled: false,
    grok_probe_interval_minutes: 30,
    grok_max_rate_limit_retries: 0,
    grok_follow_up_effort_enabled: false,
    grok_follow_up_tool_effort: 'medium',
    grok_follow_up_small_effort: 'low',
    grok_quality_guard_enabled: false,
    grok_quality_guard_max_attempts: 6,
    grok_quality_guard_hold_timeout_sec: 30,
    grok_quality_guard_on_exhausted: 'fail_closed',
    grok_quality_guard_account_cooldown_hours: 12,
    grok_oauth_client_id: '',
    max_retries: 2,
    max_rate_limit_retries: 1,
    retry_interval_ms: 0,
    transport_retry_policy: 'rotate',
    continuous_retry_enabled: false,
    continuous_retry_catch_all: false,
    continuous_retry_categories: ['transport', 'http_429', 'http_5xx', 'stream_error'],
    continuous_retry_status_codes: [],
    continuous_retry_error_codes: [],
    continuous_retry_max_duration_seconds: 600,
    codex_fingerprint_default_mode: 'off',
    allow_remote_migration: false,
    database_driver: 'postgres',
    database_label: 'PostgreSQL',
    cache_driver: 'redis',
    cache_label: 'Redis',
    response_cache_local_max_bytes: DEFAULT_RESPONSE_CACHE_TOTAL_BYTES,
    response_cache_local_max_entry_bytes: DEFAULT_RESPONSE_CACHE_ENTRY_BYTES,
    response_cache_reconstruct_max_bytes: DEFAULT_RESPONSE_CACHE_RECONSTRUCT_BYTES,
    response_cache_write_policy: 'always',
    response_cache_config_generation: 0,
    relay_model_cooldown_mode: 'off',
    relay_model_cooldown_seconds: 2,
    relay_model_cooldown_backoff_enabled: false,
    oauth_model_cooldown_mode: 'adaptive',
    oauth_model_cooldown_seconds: 300,
    oauth_model_cooldown_backoff_enabled: true,
    model_mapping: '{}',
    codex_model_mapping: '{}',
    payload_rules: '{}',
    reasoning_effort_models: '[]',
    resin_url: '',
    resin_platform_name: '',
    prompt_filter_enabled: false,
    prompt_filter_mode: 'monitor',
    prompt_filter_threshold: 50,
    prompt_filter_strict_threshold: 90,
    prompt_filter_strict_terminal_enabled: false,
    prompt_filter_advanced_config: '{}',
    prompt_filter_log_matches: true,
    prompt_filter_max_text_length: 81920,
    prompt_filter_sensitive_words: '',
    prompt_filter_custom_patterns: '[]',
    prompt_filter_disabled_patterns: '[]',
    prompt_filter_review_enabled: false,
    prompt_filter_review_api_key: '',
    prompt_filter_review_api_key_configured: false,
    prompt_filter_review_base_url: 'https://api.openai.com',
    prompt_filter_review_model: 'omni-moderation-latest',
    prompt_filter_review_timeout_seconds: 10,
    prompt_filter_review_fail_closed: true,
    client_compat_mode: 'preserve',
    codex_min_cli_version: '0.153.3',
    codex_cli_version_sync_enabled: true,
    codex_cli_version_sync_interval_hours: 12,
    codex_user_agent_config: '{}',
    usage_log_mode: 'full',
    usage_log_batch_size: 200,
    usage_log_flush_interval_seconds: 5,
    stream_flush_policy: 'immediate',
    stream_flush_interval_ms: 20,
    first_token_mode: 'strict',
    first_token_timeout_seconds: 0,
    first_token_excludes_ws_acquire: false,
    billing_tier_policy: 'actual',
    models_list_read_max_bytes: DEFAULT_MODELS_LIST_READ_MAX_BYTES,
    show_full_usage_numbers: false,
    public_key_usage_page_enabled: true,
    public_image_studio_page_enabled: true,
    public_account_portal_page_enabled: false,
    image_storage_backend: 'local',
    image_s3_endpoint: '',
    image_s3_region: '',
    image_s3_bucket: '',
    image_s3_access_key: '',
    image_s3_secret_key: '',
    image_s3_prefix: '',
    image_s3_force_path_style: false,
    auto_pause_5h_threshold: 0,
    auto_pause_7d_threshold: 0,
    auto_pause_5h_guard_band_percent: 5,
    auto_pause_5h_guard_concurrency: 1,
    smart_pacing_enabled: false,
    smart_pacing_min_concurrency: 1,
    smart_pacing_windows: '5h,7d',
    ignore_usage_limit_status: false,
  })
  const continuousRetryStatusCodesText = (settingsForm.continuous_retry_status_codes ?? []).join(',')
  const continuousRetryErrorCodesText = (settingsForm.continuous_retry_error_codes ?? []).join(',')
  const continuousRetryFineControlsDisabled = !settingsForm.continuous_retry_enabled || settingsForm.continuous_retry_catch_all
  const [continuousRetryStatusCodesDraft, setContinuousRetryStatusCodesDraft] = useState(continuousRetryStatusCodesText)
  const [continuousRetryErrorCodesDraft, setContinuousRetryErrorCodesDraft] = useState(continuousRetryErrorCodesText)
  const lazyModeActive = settingsForm.lazy_mode
  const responseCacheBudget = responseCacheBudgetFromSettings(settingsForm)
  const [savingSettings, setSavingSettings] = useState(false)
  const [autoSaveStatus, setAutoSaveStatus] = useState<AutoSaveStatus>('idle')
  // 服务端已确认的设置快照，用来算"手动保存字段还有几项没存"。自动保存路径按 key 局部合并，
  // 不能整份覆盖，否则一次开关自动保存会把其他还没点保存的文本改动一起标成已保存。
  const [persistedSettings, setPersistedSettings] = useState<SystemSettings | null>(null)
  const markPersisted = useCallback((patch: Partial<SystemSettings>) => {
    setPersistedSettings((current) => (current ? { ...current, ...patch } : current))
  }, [])
  const [responseCacheValidationError, setResponseCacheValidationError] = useState<ResponseCacheBudgetValidationError | null>(null)
  const responseCacheValidationMessage = responseCacheValidationError
    ? t(`settings.responseCache.validation.${responseCacheValidationError}`)
    : ''
  const [testingImageStorage, setTestingImageStorage] = useState(false)
  const [loadedAdminSecret, setLoadedAdminSecret] = useState('')
  const [modelList, setModelList] = useState<string[]>([])
  const [modelItems, setModelItems] = useState<ModelInfo[]>([])
  const [modelsLastSyncedAt, setModelsLastSyncedAt] = useState<string | undefined>()
  const [modelsSourceURL, setModelsSourceURL] = useState('')
  const [syncingModels, setSyncingModels] = useState(false)
  const [syncingCliVersion, setSyncingCliVersion] = useState(false)
  // GitHub token 只写不回显：草稿态独立于 settingsForm，提交后清空（issue #522）
  const [githubTokenDraft, setGithubTokenDraft] = useState('')
  const [syncedCliVersion, setSyncedCliVersion] = useState('')
  // 实际用于出站 UA 的版本(内置与同步取大);「设为同步版本」按钮以它为准,同步值过期/为空时不会把门槛设低
  const [effectiveCliVersion, setEffectiveCliVersion] = useState('')
  const logoFileInputRef = useRef<HTMLInputElement>(null)
  const backgroundFileInputRef = useRef<HTMLInputElement>(null)
  const persistedBrandingRef = useRef<Partial<SiteBranding> | null>(null)
  const settingsFormRef = useRef(settingsForm)
  const autoSavePendingCountRef = useRef(0)
  const autoSaveFieldVersionsRef = useRef<Record<string, number>>({})
  const autoSaveStatusTimerRef = useRef<number | null>(null)
  const continuousRetrySaveQueueRef = useRef(createContinuousRetrySaveQueue())
  const { toast, showToast } = useToast()
  // 邀请引导开关走独立端点(system_settings.invite_guide_config),不进主设置
  // 表单——那条 UPSERT 的占位符已经排到 $119,每加一列都要整体顺移。
  // null 表示还没加载出来,此时开关不渲染,避免先闪一个错误的默认态。
  const [inviteGuideEnabled, setInviteGuideEnabled] = useState<boolean | null>(null)

  useEffect(() => {
    let cancelled = false
    void api.getInviteGuideSettings()
      .then((res) => { if (!cancelled) setInviteGuideEnabled(res.enabled) })
      .catch(() => undefined)
    return () => { cancelled = true }
  }, [])

  // 乐观更新 + 失败回滚：开关是本地独立状态，写失败必须回到真实值，
  // 否则界面显示开着、后端其实是关的。
  const saveInviteGuideEnabled = async (next: boolean) => {
    const previous = inviteGuideEnabled
    setInviteGuideEnabled(next)
    try {
      await api.updateInviteGuideSettings(next)
      showToast(t('settings.autoSaved'), 'success', AUTO_SAVE_TOAST_MS)
    } catch (error) {
      setInviteGuideEnabled(previous)
      showToast(getErrorMessage(error), 'error')
    }
  }

  useEffect(() => {
    settingsFormRef.current = settingsForm
  }, [settingsForm])

  useEffect(() => {
    setContinuousRetryStatusCodesDraft(continuousRetryStatusCodesText)
  }, [continuousRetryStatusCodesText])

  useEffect(() => {
    setContinuousRetryErrorCodesDraft(continuousRetryErrorCodesText)
  }, [continuousRetryErrorCodesText])

  useEffect(() => {
    return () => {
      if (autoSaveStatusTimerRef.current) {
        window.clearTimeout(autoSaveStatusTimerRef.current)
      }
    }
  }, [])

  const commitSettingsForm = useCallback(
    (next: SystemSettings) => {
      const normalized = normalizeLazySettingsForm(next)
      settingsFormRef.current = normalized
      setSettingsForm(normalized)
      return normalized
    },
    [normalizeLazySettingsForm],
  )

  const scheduleAutoSaveStatusReset = useCallback(() => {
    if (autoSaveStatusTimerRef.current) {
      window.clearTimeout(autoSaveStatusTimerRef.current)
    }
    autoSaveStatusTimerRef.current = window.setTimeout(() => {
      setAutoSaveStatus((status) => (status === 'saved' ? 'idle' : status))
      autoSaveStatusTimerRef.current = null
    }, AUTO_SAVE_STATUS_RESET_MS)
  }, [])

  const finishAutoSaveRequest = useCallback((status: Exclude<AutoSaveStatus, 'idle' | 'saving'>) => {
    autoSavePendingCountRef.current = Math.max(0, autoSavePendingCountRef.current - 1)
    if (autoSavePendingCountRef.current > 0) {
      setAutoSaveStatus('saving')
      return
    }
    setAutoSaveStatus(status)
    if (status === 'saved') {
      scheduleAutoSaveStatusReset()
    }
  }, [scheduleAutoSaveStatusReset])

  const autoSaveSettingsPatch = useCallback(async (patch: Partial<SystemSettings>) => {
    const patchKeys = Object.keys(patch) as Array<keyof SystemSettings>
    if (patchKeys.length === 0) return

    const previous = settingsFormRef.current
    const optimistic = commitSettingsForm({
      ...previous,
      ...patch,
    })
    const rollbackPatch = getSettingsPatchValues(previous, patchKeys)
    markPersisted(getSettingsPatchValues(optimistic, patchKeys))
    const requestedVersions: Record<string, number> = {}

    for (const key of patchKeys) {
      const fieldKey = String(key)
      const nextVersion = (autoSaveFieldVersionsRef.current[fieldKey] ?? 0) + 1
      autoSaveFieldVersionsRef.current[fieldKey] = nextVersion
      requestedVersions[fieldKey] = nextVersion
    }

    autoSavePendingCountRef.current += 1
    if (autoSaveStatusTimerRef.current) {
      window.clearTimeout(autoSaveStatusTimerRef.current)
      autoSaveStatusTimerRef.current = null
    }
    setAutoSaveStatus('saving')

    try {
      const updated = await api.updateSettings(getSettingsPatchValues(optimistic, patchKeys))
      const mergeKeys = patchKeys.filter((key) => {
        const fieldKey = String(key)
        return autoSaveFieldVersionsRef.current[fieldKey] === requestedVersions[fieldKey]
      })
      const currentSettings = settingsFormRef.current
      const responseCacheRequest = patchKeys.some(isResponseCacheBudgetKey)
      const mergedResponseCacheGeneration = responseCacheRequest
        ? mergeResponseCacheGeneration(
            currentSettings.response_cache_config_generation,
            updated.response_cache_config_generation,
          )
        : currentSettings.response_cache_config_generation
      if (
        mergeKeys.length > 0
        || mergedResponseCacheGeneration !== currentSettings.response_cache_config_generation
      ) {
        commitSettingsForm({
          ...currentSettings,
          ...getSettingsPatchValues(updated, mergeKeys),
          ...(responseCacheRequest
            ? { response_cache_config_generation: mergedResponseCacheGeneration }
            : {}),
        })
        markPersisted(getSettingsPatchValues(updated, mergeKeys))
      }
      const autoSaveSuccessMessage = updated.expired_cleaned && updated.expired_cleaned > 0
        ? `${t('settings.autoSaved')} · ${t('settings.expiredCleanedResult', { count: updated.expired_cleaned })}`
        : t('settings.autoSaved')
      showToast(autoSaveSuccessMessage, 'success', AUTO_SAVE_TOAST_MS)
      finishAutoSaveRequest('saved')
    } catch (error) {
      const rollbackKeys = patchKeys.filter((key) => {
        const fieldKey = String(key)
        return autoSaveFieldVersionsRef.current[fieldKey] === requestedVersions[fieldKey]
      })
      if (rollbackKeys.length > 0) {
        commitSettingsForm({
          ...settingsFormRef.current,
          ...getSettingsPatchValues({ ...previous, ...rollbackPatch }, rollbackKeys),
        })
        markPersisted(getSettingsPatchValues({ ...previous, ...rollbackPatch }, rollbackKeys))
      }
      const message = getErrorMessage(error)
      showToast(`${t('settings.autoSaveFailed')}: ${message}`, 'error')
      finishAutoSaveRequest('error')
    }
  }, [commitSettingsForm, finishAutoSaveRequest, showToast, t])

  const autoSaveContinuousRetryPatch = useCallback((patch: Partial<SystemSettings>) => {
    return continuousRetrySaveQueueRef.current(() => autoSaveSettingsPatch(patch))
  }, [autoSaveSettingsPatch])

  const autoSaveBooleanField = useCallback((field: keyof SystemSettings, value: boolean, extraPatch: Partial<SystemSettings> = {}) => {
    void autoSaveSettingsPatch({
      ...extraPatch,
      [field]: value,
    } as Partial<SystemSettings>)
  }, [autoSaveSettingsPatch])

  const autoSaveStringField = useCallback((field: keyof SystemSettings, value: string, extraPatch: Partial<SystemSettings> = {}) => {
    void autoSaveSettingsPatch({
      ...extraPatch,
      [field]: value,
    } as Partial<SystemSettings>)
  }, [autoSaveSettingsPatch])

  // ===== Antigravity OAuth client 配置(草稿态 + 显式保存;secret 不回显,留空 = 沿用已保存值) =====
  const [agOAuthDraft, setAgOAuthDraft] = useState<{ rows: AntigravityOAuthClientSetting[]; activeKey: string } | null>(null)
  const [agOAuthSaving, setAgOAuthSaving] = useState(false)
  const agOAuthServer = useMemo(() => ({
    rows: (settingsForm.antigravity_oauth_clients ?? []).map(client => ({ ...client, client_secret: '' })),
    activeKey: settingsForm.antigravity_oauth_client_key ?? '',
  }), [settingsForm.antigravity_oauth_clients, settingsForm.antigravity_oauth_client_key])
  const agOAuth = agOAuthDraft ?? agOAuthServer
  const agOAuthDirty = agOAuthDraft !== null
  const updateAgOAuthRow = (index: number, patch: Partial<AntigravityOAuthClientSetting>) => {
    setAgOAuthDraft({
      ...agOAuth,
      rows: agOAuth.rows.map((row, i) => (i === index ? { ...row, ...patch } : row)),
    })
  }
  const removeAgOAuthRow = (index: number) => {
    setAgOAuthDraft({ ...agOAuth, rows: agOAuth.rows.filter((_, i) => i !== index) })
  }
  const addAgOAuthRow = () => {
    setAgOAuthDraft({
      ...agOAuth,
      rows: [...agOAuth.rows, { key: '', client_id: '', client_secret: '', has_secret: false }],
    })
  }
  const saveAgOAuth = async () => {
    setAgOAuthSaving(true)
    try {
      const activeKey = agOAuth.activeKey.trim().toLowerCase()
      const updated = await api.updateSettings({
        antigravity_oauth_clients: agOAuth.rows.map(row => ({
          key: row.key.trim().toLowerCase(),
          client_id: row.client_id.trim(),
          client_secret: (row.client_secret ?? '').trim(),
        })),
        // 活跃 key 指向的条目被删掉时自动回落「第一个」,避免整次保存被后端校验拒绝。
        antigravity_oauth_client_key: agOAuth.rows.some(row => row.key.trim().toLowerCase() === activeKey) ? activeKey : '',
      })
      const agOAuthPatch: Partial<SystemSettings> = {
        antigravity_oauth_clients: updated.antigravity_oauth_clients,
        antigravity_oauth_client_key: updated.antigravity_oauth_client_key,
        antigravity_oauth_env_clients: updated.antigravity_oauth_env_clients,
        antigravity_oauth_client_key_env_override: updated.antigravity_oauth_client_key_env_override,
        antigravity_oauth_active_key_effective: updated.antigravity_oauth_active_key_effective,
        antigravity_oauth_using_builtin: updated.antigravity_oauth_using_builtin,
        antigravity_oauth_builtin_client: updated.antigravity_oauth_builtin_client,
      }
      commitSettingsForm({ ...settingsFormRef.current, ...agOAuthPatch })
      markPersisted(agOAuthPatch)
      setAgOAuthDraft(null)
      showToast(t('settings.antigravityOAuth.saved'), 'success')
    } catch (error) {
      showToast(`${t('settings.antigravityOAuth.saveFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setAgOAuthSaving(false)
    }
  }

  const updateResponseCacheBudget = (
    field: keyof ResponseCacheBudgetMiB,
    value: number,
  ) => {
    const next = {
      ...responseCacheBudgetFromSettings(settingsFormRef.current),
      [field]: value,
    }
    setResponseCacheValidationError(validateResponseCacheBudget(next))
    commitSettingsForm({
      ...settingsFormRef.current,
      ...responseCacheBudgetFieldPatch(field, value),
    })
  }

  const commitResponseCacheBudget = (
    field: keyof ResponseCacheBudgetMiB,
    value: number,
  ) => {
    const next = {
      ...responseCacheBudgetFromSettings(settingsFormRef.current),
      [field]: value,
    }
    const validationError = validateResponseCacheBudget(next)
    setResponseCacheValidationError(validationError)
    if (validationError) {
      showToast(t(`settings.responseCache.validation.${validationError}`), 'error')
      return
    }
    void autoSaveSettingsPatch(buildResponseCacheBudgetPatch(next))
  }

  const loadSettingsData = useCallback(async () => {
    const [health, settings, modelsResp] = await Promise.all([api.getHealth(), api.getSettings(), api.getModels()])
    setPersistedSettings(commitSettingsForm(settings))
    const branding = {
      site_name: settings.site_name,
      site_logo: settings.site_logo,
      background_image: settings.background_image,
      background_opacity: settings.background_opacity,
      background_blur: settings.background_blur,
      background_glass_opacity: settings.background_glass_opacity,
      background_glass_blur: settings.background_glass_blur,
    }
    persistedBrandingRef.current = branding
    applyBranding(branding)
    setLoadedAdminSecret(settings.admin_secret ?? '')
    setSyncedCliVersion(settings.codex_synced_cli_version ?? '')
    setEffectiveCliVersion(settings.codex_effective_cli_version ?? '')
    setModelList(modelsResp.models ?? [])
    setModelItems(modelsResp.items ?? [])
    setModelsLastSyncedAt(modelsResp.last_synced_at)
    setModelsSourceURL(modelsResp.source_url ?? '')
    return {
      health,
    }
  }, [applyBranding, commitSettingsForm])

  const { data, loading, error, reload } = useDataLoader<{
    health: HealthResponse | null
  }>({
    initialData: {
      health: null,
    },
    load: loadSettingsData,
  })

  const handleSaveSettings = async () => {
    const normalized = normalizeLazySettingsForm(settingsForm)
    const validationError = validateResponseCacheBudget(responseCacheBudgetFromSettings(normalized))
    setResponseCacheValidationError(validationError)
    if (validationError) {
      showToast(t(`settings.responseCache.validation.${validationError}`), 'error')
      return
    }
    setSavingSettings(true)
    try {
      const adminSecretChanged = settingsForm.admin_auth_source !== 'env' && settingsForm.admin_secret !== loadedAdminSecret
	      const payload: Partial<SystemSettings> = buildWritableSettingsPayload(normalized)
      // 自定义 Prompt 规则由规则页单独保存，避免普通设置提交覆盖并发发布结果。
      delete payload.prompt_filter_custom_patterns
      const updated = await api.updateSettings(payload)
      setPersistedSettings(commitSettingsForm(updated))
      const branding = {
        site_name: updated.site_name,
        site_logo: updated.site_logo,
        background_image: updated.background_image,
        background_opacity: updated.background_opacity,
        background_blur: updated.background_blur,
        background_glass_opacity: updated.background_glass_opacity,
        background_glass_blur: updated.background_glass_blur,
      }
      persistedBrandingRef.current = branding
      applyBranding(branding)
      setLoadedAdminSecret(updated.admin_secret ?? '')
      if (updated.admin_auth_source !== 'env') {
        setAdminKey(updated.admin_secret ?? '')
      }
      if (adminSecretChanged) {
        resetAdminAuthState()
        return
      }
      if (updated.expired_cleaned && updated.expired_cleaned > 0) {
        showToast(t('settings.expiredCleanedResult', { count: updated.expired_cleaned }))
      } else {
        showToast(t('settings.saveSuccess'))
      }
    } catch (error) {
      showToast(`${t('settings.saveFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSavingSettings(false)
    }
  }

  useEffect(() => {
    if (!persistedBrandingRef.current) return
    applyBranding({
      site_name: settingsForm.site_name,
      site_logo: settingsForm.site_logo,
      background_image: settingsForm.background_image,
      background_opacity: settingsForm.background_opacity,
      background_blur: settingsForm.background_blur,
      background_glass_opacity: settingsForm.background_glass_opacity,
      background_glass_blur: settingsForm.background_glass_blur,
    })
  }, [
    applyBranding,
    settingsForm.site_name,
    settingsForm.site_logo,
    settingsForm.background_image,
    settingsForm.background_opacity,
    settingsForm.background_blur,
    settingsForm.background_glass_opacity,
    settingsForm.background_glass_blur,
  ])

  useEffect(() => {
    return () => {
      if (persistedBrandingRef.current) {
        applyBranding(persistedBrandingRef.current)
      }
    }
  }, [applyBranding])

  const handleSiteLogoUpload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    const mimeType = getSiteLogoMimeType(file)
    if (!mimeType) {
      showToast(t('settings.siteLogoInvalidType'), 'error')
      return
    }

    try {
      const result = file.size <= SITE_LOGO_MAX_BYTES
        ? await readFileAsDataURL(file)
        : await compressSiteLogoFile(file, mimeType)
      if (dataURLByteLength(result) > SITE_LOGO_MAX_BYTES) {
        showToast(t('settings.siteLogoTooLarge'), 'error')
        return
      }
      setSettingsForm((f) => ({ ...f, site_logo: result }))
      if (file.size > SITE_LOGO_MAX_BYTES) {
        showToast(t('settings.siteLogoCompressed', { size: formatBytesKB(dataURLByteLength(result)) }))
      }
    } catch {
      showToast(t('settings.siteLogoCompressionFailed'), 'error')
    }
  }

  const handleBackgroundImageUpload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    const mimeType = getBackgroundImageMimeType(file)
    if (!mimeType) {
      showToast(t('settings.backgroundImageInvalidType'), 'error')
      return
    }
    const maxBytes = mimeType === 'video/mp4' ? BACKGROUND_VIDEO_UPLOAD_MAX_BYTES : BACKGROUND_IMAGE_UPLOAD_MAX_BYTES
    if (file.size > maxBytes) {
      showToast(t(mimeType === 'video/mp4' ? 'settings.backgroundVideoTooLarge' : 'settings.backgroundImageTooLarge'), 'error')
      return
    }

    try {
      const uploaded = await api.uploadBackground(file)
      setSettingsForm((f) => ({
        ...f,
        background_image: uploaded.url,
        background_opacity: f.background_opacity || 18,
      }))
      showToast(t('settings.backgroundImageUploaded'))
    } catch (err) {
      showToast(getErrorMessage(err) || t('settings.backgroundImageUploadFailed'), 'error')
    }
  }

  const handleTestImageStorage = async () => {
    setTestingImageStorage(true)
    try {
      const result = await api.testImageStorageConnection({
        endpoint: settingsForm.image_s3_endpoint,
        region: settingsForm.image_s3_region,
        bucket: settingsForm.image_s3_bucket,
        access_key: settingsForm.image_s3_access_key,
        secret_key: settingsForm.image_s3_secret_key,
        prefix: settingsForm.image_s3_prefix,
        force_path_style: settingsForm.image_s3_force_path_style,
      })
      showToast(t('settings.imageS3TestSuccess', { bucket: result.bucket }))
    } catch (error) {
      showToast(`${t('settings.imageS3TestFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setTestingImageStorage(false)
    }
  }

  const handleSyncCliVersion = async () => {
    setSyncingCliVersion(true)
    try {
      const result = await api.syncCodexCLIVersion()
      setSyncedCliVersion(result.effective_version)
      setEffectiveCliVersion(result.effective_version)
      showToast(t('settings.cliVersionSyncSuccess', {
        version: result.effective_version,
        fetched: result.fetched_version || '-',
      }))
    } catch (error) {
      showToast(`${t('settings.cliVersionSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingCliVersion(false)
    }
  }

  const handleSyncModels = async () => {
    setSyncingModels(true)
    try {
      const result = await api.syncModels()
      setModelList(result.models ?? [])
      setModelItems(result.items ?? [])
      setModelsLastSyncedAt(result.last_synced_at)
      setModelsSourceURL(result.source_url ?? '')
      showToast(t('settings.modelsSyncSuccess', {
        added: result.added,
        updated: result.updated,
        skipped: result.skipped?.length ?? 0,
        removed: result.removed?.length ?? 0,
      }))
    } catch (error) {
      showToast(`${t('settings.modelsSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingModels(false)
    }
  }

  const { health } = data
  const isExternalDatabase = settingsForm.database_driver === 'postgres'
  const isExternalCache = settingsForm.cache_driver === 'redis'
  const showConnectionPool = isExternalDatabase || isExternalCache
  const canConfigureRemoteMigration = settingsForm.admin_auth_source === 'env' || settingsForm.admin_secret.trim() !== ''
  const saveButtonLabel = savingSettings ? t('common.saving') : t('settings.saveSettings')
  const siteLogoPreview = sanitizeBrandingLogo(settingsForm.site_logo) || DEFAULT_SITE_LOGO
  const backgroundImagePreview = sanitizeBrandingImage(settingsForm.background_image)
  const backgroundIsVideo = isBrandingVideo(backgroundImagePreview)
  const visibleModelItems = useMemo(() => {
    if (modelItems.length > 0) {
      return modelItems
    }
    return modelList.map((id) => ({
      id,
      enabled: true,
      category: id.includes('image') ? 'image' : 'codex',
      source: 'builtin',
      pro_only: id === 'gpt-5.3-codex-spark',
      api_key_auth_available: !['gpt-5.5', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-6-astra'].includes(id),
    }))
  }, [modelItems, modelList])
  const codexModelOptions = visibleModelItems
    .filter((model) =>
      model.enabled &&
      !model.id.includes('(') &&
      !model.id.includes(')')
    )
    .map((model) => ({ label: model.id, value: model.id }))
  const textModelOptions = visibleModelItems
    .filter((model) =>
      model.enabled &&
      model.category !== 'image' &&
      !model.id.includes('image') &&
      !model.id.includes('(') &&
      !model.id.includes(')')
    )
    .map((model) => ({ label: model.id, value: model.id }))
  const enabledModelCount = visibleModelItems.filter((model) => model.enabled).length
  const modelsLastSyncedLabel = modelsLastSyncedAt ? formatBeijingTime(modelsLastSyncedAt) : t('settings.modelsNeverSynced')
  const modelsSourceLabel = modelsSourceURL || 'https://developers.openai.com/codex/models'
  const anthropicMappingCount = useMemo(
    () => parseModelMappingEntries(settingsForm.model_mapping, defaultClaudeModelMappingEntries).length,
    [defaultClaudeModelMappingEntries, settingsForm.model_mapping],
  )
  const codexMappingCount = useMemo(
    () => parseModelMappingEntries(settingsForm.codex_model_mapping).length,
    [settingsForm.codex_model_mapping],
  )
  const reasoningEffortCount = useMemo(
    () => parseReasoningEffortModelEntries(settingsForm.reasoning_effort_models).length,
    [settingsForm.reasoning_effort_models],
  )
  const payloadRuleCount = useMemo(
    () => countPayloadRules(settingsForm.payload_rules),
    [settingsForm.payload_rules],
  )
  const showInitialSkeleton = loading && !health
  const codexUserAgentConfig = useMemo(
    () => parseCodexUserAgentConfig(settingsForm.codex_user_agent_config),
    [settingsForm.codex_user_agent_config],
  )
  const codexUserAgentPreview = useMemo(
    () => buildCodexUserAgentPreview(codexUserAgentConfig, settingsForm.codex_min_cli_version, settingsForm.client_compat_mode),
    [codexUserAgentConfig, settingsForm.client_compat_mode, settingsForm.codex_min_cli_version],
  )
  const updateCodexUserAgentConfig = useCallback((patch: Partial<CodexUserAgentConfig>) => {
    setSettingsForm((form) => {
      const current = parseCodexUserAgentConfig(form.codex_user_agent_config)
      return {
        ...form,
        codex_user_agent_config: serializeCodexUserAgentConfig({ ...current, ...patch }),
      }
    })
  }, [])
  const saveCodexUserAgentConfig = useCallback(() => {
    void autoSaveSettingsPatch({ codex_user_agent_config: settingsForm.codex_user_agent_config })
  }, [autoSaveSettingsPatch, settingsForm.codex_user_agent_config])
  const dirtyKeys = useMemo(() => {
    if (!persistedSettings) return [] as string[]
    const current = normalizeLazySettingsForm(settingsForm) as unknown as Record<string, unknown>
    const base = persistedSettings as unknown as Record<string, unknown>
    const keys = new Set([...Object.keys(current), ...Object.keys(base)])
    const changed: string[] = []
    for (const key of keys) {
      if (SETTINGS_DIRTY_IGNORED_KEYS.has(key)) continue
      if (!settingsValueEquals(current[key], base[key])) changed.push(key)
    }
    return changed
  }, [normalizeLazySettingsForm, persistedSettings, settingsForm])
  const dirtyCount = dirtyKeys.length
  const discardChanges = useCallback(() => {
    if (!persistedSettings) return
    commitSettingsForm(persistedSettings)
    setResponseCacheValidationError(null)
  }, [commitSettingsForm, persistedSettings])
  // 有未保存改动时保存按钮才是主色；没改动也保留可点，脏检查漏判时用户仍能强制保存。
  const renderSaveButton = (className?: string) => (
    <Button
      className={className}
      variant={dirtyCount > 0 ? 'default' : 'outline'}
      onClick={() => void handleSaveSettings()}
      disabled={savingSettings || autoSaveStatus === 'saving'}
    >
      <Save className="size-4" />
      {saveButtonLabel}
    </Button>
  )

  const settingsTabs = useMemo(
    () =>
      [
        { id: 'codex', label: t('settings.nav.codex'), icon: <ChannelLogo channel="codex" size={16} /> },
        { id: 'claude', label: t('settings.nav.claude'), icon: <ChannelLogo channel="claude" size={16} /> },
        { id: 'antigravity', label: t('settings.nav.antigravity'), icon: <ChannelLogo channel="antigravity" size={16} /> },
        { id: 'grok', label: t('settings.nav.grok'), icon: <ChannelLogo channel="grok" size={16} /> },
        { id: 'appearance', label: t('settings.nav.appearance'), icon: <Palette className="size-4" /> },
        { id: 'general', label: t('settings.nav.general'), icon: <SlidersHorizontal className="size-4" /> },
      ] as const satisfies ReadonlyArray<{ id: SettingsTabKey; label: string; icon: ReactNode }>,
    [t],
  )
  const [searchParams, setSearchParams] = useSearchParams()
  const location = useLocation()
  const tabParam = searchParams.get('tab')
  const activeTab: SettingsTabKey = isSettingsTabKey(tabParam) ? tabParam : DEFAULT_SETTINGS_TAB
  const [modelPanel, setModelPanel] = useState<ModelPanelKey | null>(null)
  const settingsNavRef = useRef<HTMLElement | null>(null)

  // 切换 Tab 后要定位的 section id：面板内容在下一次渲染才挂载，滚动动作放到 effect 里。
  const pendingSectionRef = useRef<string | null>(null)
  const selectTab = useCallback(
    (tab: SettingsTabKey, sectionId?: string) => {
      pendingSectionRef.current = sectionId ?? null
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.set('tab', tab)
          return next
        },
        { replace: true },
      )
      if (!sectionId) window.scrollTo({ top: 0 })
    },
    [setSearchParams],
  )
  useEffect(() => {
    const sectionId = pendingSectionRef.current
    if (!sectionId) return
    pendingSectionRef.current = null
    document.getElementById(sectionId)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }, [activeTab])

  // 兼容旧的 #settings-xxx 锚点深链：首次进入时把锚点映射到对应 Tab。
  useEffect(() => {
    if (tabParam) return
    const legacy = LEGACY_SECTION_TABS[location.hash.replace(/^#/, '')]
    if (legacy) selectTab(legacy)
  }, [location.hash, selectTab, tabParam])

  // 当前 Tab 变化时，把对应 pill 滚进顶栏可视区（窄屏横向滚动导航）。
  useEffect(() => {
    const nav = settingsNavRef.current
    if (!nav) return
    const btn = nav.querySelector<HTMLElement>(`[data-tab-id="${activeTab}"]`)
    btn?.scrollIntoView({ behavior: 'smooth', inline: 'center', block: 'nearest' })
  }, [activeTab])

  const sectionIndexItems = useMemo(
    () => SETTINGS_TAB_SECTION_INDEX[activeTab].map((item) => ({ id: item.id, label: t(item.labelKey), icon: item.icon })),
    [activeTab, t],
  )
  const sectionIds = useMemo(() => SETTINGS_TAB_SECTION_INDEX[activeTab].map((item) => item.id), [activeTab])
  const { activeId: activeSectionId, pinSection } = useActiveSettingsSection(sectionIds)
  const hasSectionIndex = sectionIndexItems.length > 1
  const jumpToSection = useCallback((sectionId: string) => {
    pinSection(sectionId)
    document.getElementById(sectionId)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }, [pinSection])

  if (showInitialSkeleton) {
    return <SettingsSkeleton />
  }

  return (
    <StateShell
      variant="page"
      loading={false}
      error={error && !health ? error : null}
      onRetry={() => void reload()}
      loadingTitle={t('settings.loadingTitle')}
      loadingDescription={t('settings.loadingDesc')}
      errorTitle={t('settings.errorTitle')}
    >
      <>
        <PageHeader
          title={t('settings.title')}
          description={t('settings.description')}
          actions={
            <>
              <SaveStatusPill autoSaveStatus={autoSaveStatus} dirtyCount={dirtyCount} />
              {renderSaveButton('shrink-0')}
            </>
          }
        />

        {/* Tab 栏 + 分区目录一起跟随页面流、滚动时粘在顶部，不再用 fixed 悬浮盖住内容 */}
        <div className="sticky top-2 z-30 mb-5 space-y-2.5 lg:top-3">
          <nav
            ref={settingsNavRef}
            role="tablist"
            aria-label={t('settings.navLabel')}
            className={cn(
              'flex min-w-0 items-center gap-1 overflow-x-auto rounded-full border border-border/70 bg-card/95 p-1 shadow-sm backdrop-blur-xl',
              '[-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
            )}
          >
            {settingsTabs.map((tab) => {
              const active = activeTab === tab.id
              return (
                <button
                  key={tab.id}
                  type="button"
                  role="tab"
                  data-tab-id={tab.id}
                  aria-selected={active}
                  aria-current={active ? 'true' : undefined}
                  onClick={() => selectTab(tab.id)}
                  className={cn(
                    'inline-flex shrink-0 items-center justify-center gap-2 rounded-full px-3.5 py-1.5 text-[13px] font-semibold tracking-tight transition-colors duration-200 sm:flex-1 sm:basis-0 sm:px-4 sm:py-1.5 sm:text-xs',
                    active
                      ? 'bg-primary text-primary-foreground shadow-2xs'
                      : 'text-muted-foreground hover:bg-muted/70 hover:text-foreground',
                  )}
                >
                  <span
                    className={cn(
                      'shrink-0 [&_svg]:size-4 sm:[&_svg]:size-[1.05rem]',
                      active ? 'opacity-100' : 'opacity-75',
                    )}
                  >
                    {tab.icon}
                  </span>
                  <span className="whitespace-nowrap">{tab.label}</span>
                </button>
              )
            })}
          </nav>
          {hasSectionIndex ? (
            <SettingsSectionIndex
              items={sectionIndexItems}
              activeId={activeSectionId}
              label={t('settings.sectionIndex')}
              onSelect={jumpToSection}
            />
          ) : null}
        </div>

        <div key={activeTab} className="pb-4">
          <div className="min-w-0 space-y-7">
          {activeTab === 'codex' ? (
            <>
              <SettingsSection id="settings-codex-quota" title={t('settings.nav.codexQuota')} description={t('settings.nav.codexQuotaDesc')} icon={<Gauge className="size-4" />}>
              <div className={SETTINGS_CARD_GRID_2}>
                <SettingsCard title={t('settings.probeScheduling')} icon={<RefreshCw className="size-4" />}>
                  <div className="space-y-4">
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.backgroundRefreshInterval')} description={t('settings.backgroundRefreshIntervalDesc')} suffix={t('settings.unit.min')}>
                        <DraftNumberInput
                          min={1}
                          max={1440}
                          value={settingsForm.background_refresh_interval_minutes}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, background_refresh_interval_minutes: value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.usageProbeMaxAge')} description={t('settings.usageProbeMaxAgeDesc')} suffix={t('settings.unit.min')}>
                        <DraftNumberInput
                          min={1}
                          max={10080}
                          value={settingsForm.usage_probe_max_age_minutes}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_probe_max_age_minutes: value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.usageProbeConcurrency')} description={t('settings.usageProbeConcurrencyDesc')} suffix={t('settings.unit.concurrency')}>
                        <DraftNumberInput
                          min={1}
                          max={128}
                          value={settingsForm.usage_probe_concurrency}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_probe_concurrency: value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.recoveryProbeInterval')} description={t('settings.recoveryProbeIntervalDesc')}>
                        {lazyModeActive ? (
                          <Input value="∞" disabled />
                        ) : (
                          <DraftNumberInput
                            min={1}
                            max={10080}
                            value={settingsForm.recovery_probe_interval_minutes}
                            onValueChange={(value) => setSettingsForm(f => ({ ...f, recovery_probe_interval_minutes: value }))}
                          />
                        )}
                      </SettingField>
                    </div>
                    <div className={SETTINGS_SWITCH_GRID}>
                      <SettingField label={t('settings.usageProbeResponsesFallback')} description={t('settings.usageProbeResponsesFallbackDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.usage_probe_responses_fallback_enabled}
                          onCheckedChange={(checked) => autoSaveBooleanField('usage_probe_responses_fallback_enabled', checked)}
                        />
                      </SettingField>
                      <SettingField label={t('settings.lazyMode')} description={t('settings.lazyModeDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.lazy_mode}
                          onCheckedChange={(enabled) => {
                            void autoSaveSettingsPatch({
                              lazy_mode: enabled,
                              auto_clean_full_usage: enabled ? false : settingsFormRef.current.auto_clean_full_usage,
                            })
                          }}
                        />
                      </SettingField>
                      {inviteGuideEnabled !== null && (
                        <SettingField
                          label={t('settings.inviteGuide')}
                          description={t('settings.inviteGuideDesc')}
                          layout="switch"
                        >
                          <Switch
                            aria-label={t('settings.inviteGuide')}
                            checked={inviteGuideEnabled}
                            onCheckedChange={(checked) => void saveInviteGuideEnabled(checked)}
                          />
                        </SettingField>
                      )}
                    </div>
                  </div>
                </SettingsCard>
              <SettingsCard
                title={t('settings.connectivityTest')}
                description={t('settings.connectivityTestDesc')}
                icon={<Wifi className="size-4" />}
                className="h-full"
                contentClassName="flex h-full flex-col"
              >
                <div className="flex flex-1 flex-col gap-4">
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField label={t('settings.testModelLabel')} description={t('settings.testModelHint')}>
                      <Select
                        value={settingsForm.test_model}
                        onValueChange={(value) => autoSaveStringField('test_model', value)}
                        options={textModelOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.testConcurrency')} description={t('settings.testConcurrencyRange')}>
                      <DraftNumberInput
                        min={1}
                        max={200}
                        value={settingsForm.test_concurrency}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, test_concurrency: value }))}
                      />
                    </SettingField>
                  </div>
                  <SettingField label={t('settings.testContent')} description={t('settings.testContentDesc')} stretch>
                    <textarea
                      rows={3}
                      value={settingsForm.test_content}
                      placeholder={t('settings.testContentPlaceholder')}
                      onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setSettingsForm(f => ({ ...f, test_content: e.target.value }))}
                      onBlur={(e) => autoSaveStringField('test_content', e.currentTarget.value)}
                      className={cn(
                        'flex min-h-[88px] w-full resize-y rounded-md border border-input bg-background px-3 py-2 text-sm text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50',
                      )}
                    />
                  </SettingField>
                </div>
              </SettingsCard>
              </div>

              <SettingsCard
                title={t('settings.autoResetCreditsTitle')}
                description={t('settings.autoResetCreditsDesc')}
                icon={<RefreshCw className="size-4" />}
              >
                <div className={cn(SETTINGS_SWITCH_GRID, 'items-stretch')}>
                  <SettingField
                    label={t('settings.autoResetCreditsEnabled')}
                    description={t('settings.autoResetCreditsEnabledDesc')}
                    layout="switch"
                    className="h-full"
                  >
                    <Switch
                      checked={settingsForm.auto_reset_credits_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField(
                        'auto_reset_credits_enabled',
                        checked,
                        checked
                          ? { auto_reset_credits_before_expiry_min: settingsFormRef.current.auto_reset_credits_before_expiry_min }
                          : {},
                      )}
                    />
                  </SettingField>
                  <div className="flex min-h-[48px] min-w-0 items-center justify-between gap-3 rounded-lg border border-border/60 bg-muted/20 px-3 py-2.5">
                    <div className="min-w-0 flex-1 space-y-0.5">
                      <div className="flex items-center gap-1.5">
                        <label className="block text-[13px] font-medium leading-snug text-foreground sm:text-sm">
                          {t('settings.autoResetCreditsBeforeExpiry')}
                        </label>
                        <SettingHelp text={t('settings.autoResetCreditsBeforeExpiryDesc')} />
                      </div>
                    </div>
                    <div className="relative w-[7.5rem] shrink-0 sm:w-[8.5rem]">
                      <DraftNumberInput
                        min={10}
                        max={10080}
                        step={10}
                        className="pr-11"
                        value={settingsForm.auto_reset_credits_before_expiry_min}
                        onValueChange={(value) => {
                          commitSettingsForm({
                            ...settingsFormRef.current,
                            auto_reset_credits_before_expiry_min: value,
                          })
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({
                            auto_reset_credits_before_expiry_min: value,
                          })
                        }}
                      />
                      <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium tabular-nums text-muted-foreground">
                        {t('settings.unit.min')}
                      </span>
                    </div>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard
                title={t('settings.autoActivate5hTitle')}
                description={t('settings.autoActivate5hDesc')}
                icon={<Timer className="size-4" />}
              >
                <SettingField
                  label={t('settings.autoActivate5hEnabled')}
                  description={t('settings.autoActivate5hEnabledDesc')}
                  layout="switch"
                >
                  <Switch
                    checked={Boolean(settingsForm.auto_activate_5h_window_enabled)}
                    onCheckedChange={(checked) => autoSaveBooleanField('auto_activate_5h_window_enabled', checked)}
                  />
                </SettingField>
              </SettingsCard>

              <SettingsCard title={t('settings.globalAutoPauseTitle')} description={t('settings.globalAutoPauseDesc')} icon={<Activity className="size-4" />} channels={CHANNELS_CODEX_CLAUDE}>
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField label={t('settings.globalAutoPause5h')} description={t('settings.globalAutoPauseHint')}>
                      <DraftNumberInput
                        min={0}
                        max={100}
                        step={0.1}
                        inputMode="decimal"
                        placeholder={t('settings.globalAutoPausePlaceholder')}
                        integer={false}
                        emptyValue={0}
                        value={settingsForm.auto_pause_5h_threshold * 100}
                        formatValue={(value) => value > 0 ? value.toFixed(1).replace(/\.0$/, '') : ''}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_5h_threshold: value / 100 }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_5h_threshold: value / 100 })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.globalAutoPause7d')} description={t('settings.globalAutoPauseHint')}>
                      <DraftNumberInput
                        min={0}
                        max={100}
                        step={0.1}
                        inputMode="decimal"
                        placeholder={t('settings.globalAutoPausePlaceholder')}
                        integer={false}
                        emptyValue={0}
                        value={settingsForm.auto_pause_7d_threshold * 100}
                        formatValue={(value) => value > 0 ? value.toFixed(1).replace(/\.0$/, '') : ''}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_7d_threshold: value / 100 }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_7d_threshold: value / 100 })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.autoPause5hGuardBand')} description={t('settings.autoPause5hGuardBandHint')}>
                      <DraftNumberInput
                        min={0}
                        max={100}
                        step={0.1}
                        inputMode="decimal"
                        placeholder={t('settings.autoPause5hGuardBandPlaceholder')}
                        integer={false}
                        emptyValue={0}
                        value={settingsForm.auto_pause_5h_guard_band_percent}
                        formatValue={(value) => value > 0 ? String(value) : ''}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_5h_guard_band_percent: value }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_5h_guard_band_percent: value })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.autoPause5hGuardConcurrency')} description={t('settings.autoPause5hGuardConcurrencyHint')}>
                      <DraftNumberInput
                        min={0}
                        max={1000}
                        step={1}
                        inputMode="numeric"
                        value={settingsForm.auto_pause_5h_guard_concurrency ?? 1}
                        emptyValue={0}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_5h_guard_concurrency: value }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_5h_guard_concurrency: value })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.smartPacingWindows')} description={t('settings.smartPacingWindowsHint')}>
                      <Select
                        value={settingsForm.smart_pacing_windows || '5h,7d'}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, smart_pacing_windows: value }))
                          void autoSaveSettingsPatch({ smart_pacing_windows: value })
                        }}
                        options={[
                          { value: '5h,7d', label: t('settings.smartPacingWindowsBoth') },
                          { value: '5h', label: t('settings.smartPacingWindows5h') },
                          { value: '7d', label: t('settings.smartPacingWindows7d') },
                        ]}
                      />
                    </SettingField>
                    <SettingField label={t('settings.smartPacingMinConcurrency')} description={t('settings.smartPacingMinConcurrencyHint')}>
                      <DraftNumberInput
                        min={1}
                        max={1000}
                        step={1}
                        inputMode="numeric"
                        value={settingsForm.smart_pacing_min_concurrency ?? 1}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, smart_pacing_min_concurrency: value }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ smart_pacing_min_concurrency: value })
                        }}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_SWITCH_GRID}>
                    <SettingField
                      label={t('settings.ignoreUsageLimitStatus')}
                      description={t('settings.ignoreUsageLimitStatusHint')}
                      layout="switch"
                    >
                      <Switch
                        checked={settingsForm.ignore_usage_limit_status}
                        onCheckedChange={(checked) => autoSaveBooleanField('ignore_usage_limit_status', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.smartPacingEnabled')} description={t('settings.smartPacingEnabledHint')} layout="switch">
                      <Switch
                        checked={settingsForm.smart_pacing_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('smart_pacing_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>
              {/* 调度策略跨渠道共用，归在通用设置；这里只做导流，避免用户在 Codex 页找不到。 */}
              <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-dashed border-border bg-muted/20 px-4 py-3">
                <div className="flex min-w-0 items-center gap-3">
                  <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-muted/70 text-muted-foreground ring-1 ring-border/60">
                    <Layers className="size-4" />
                  </div>
                  <div className="min-w-0">
                    <div className="text-sm font-semibold text-foreground">{t('settings.codexSchedulingHintTitle')}</div>
                    <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">{t('settings.codexSchedulingHintDesc')}</p>
                  </div>
                </div>
                <Button size="sm" variant="outline" onClick={() => selectTab('general', 'settings-traffic')}>
                  <ChevronRight className="size-3.5" />
                  {t('settings.codexSchedulingHintAction')}
                </Button>
              </div>
              </SettingsSection>

              <SettingsSection id="settings-codex-transport" title={t('settings.nav.codexTransport')} description={t('settings.nav.codexTransportDesc')} icon={<Wifi className="size-4" />}>
              <SettingsCard title={t('settings.codexWebsocket')} description={t('settings.codexWebsocketDesc')} icon={<Wifi className="size-4" />}>
                <div className="space-y-4">
                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
                    <SettingField label={t('settings.codexForceWebsocket')} description={t('settings.codexForceWebsocketDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_force_websocket}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_force_websocket', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexRequestCompression')} description={t('settings.codexRequestCompressionDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_request_compression}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_request_compression', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSWeakNetworkMode')} description={t('settings.codexWSWeakNetworkModeDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_weak_network_mode}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_weak_network_mode', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSKeepaliveEnabled')} description={t('settings.codexWSKeepaliveEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_keepalive_enabled}
                        disabled={settingsForm.codex_ws_weak_network_mode}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_keepalive_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSHideUpstreamErrors')} description={t('settings.codexWSHideUpstreamErrorsDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_hide_upstream_errors}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_hide_upstream_errors', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSSilentRetryEnabled')} description={t('settings.codexWSSilentRetryEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_silent_retry_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_silent_retry_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSSizeRouterEnabled')} description={t('settings.codexWSSizeRouterEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_size_router_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_size_router_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSBusyOverflowEnabled')} description={t('settings.codexWSBusyOverflowEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_busy_overflow_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_busy_overflow_enabled', checked)}
                      />
                    </SettingField>
                  </div>

                  <div className={cn(SETTINGS_FIELD_GRID, 'border-t border-border/80 pt-4')}>
                    <SettingField
                      label={t('settings.codexWSKeepaliveInterval')}
                      description={t('settings.codexWSKeepaliveIntervalDesc')}
                      suffix={t('settings.unit.sec')}
                      className={cn((!settingsForm.codex_ws_keepalive_enabled || settingsForm.codex_ws_weak_network_mode) && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={10}
                        max={600}
                        disabled={!settingsForm.codex_ws_keepalive_enabled || settingsForm.codex_ws_weak_network_mode}
                        value={settingsForm.codex_ws_keepalive_interval_sec}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_keepalive_interval_sec: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_ws_keepalive_enabled || settingsForm.codex_ws_weak_network_mode) return
                          void autoSaveSettingsPatch({
                            codex_ws_keepalive_interval_sec: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSSilentMaxRetries')}
                      description={t('settings.codexWSSilentMaxRetriesDesc')}
                      suffix={t('settings.unit.times')}
                      className={cn(!settingsForm.codex_ws_silent_retry_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={10}
                        disabled={!settingsForm.codex_ws_silent_retry_enabled}
                        value={settingsForm.codex_ws_silent_max_retries}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_silent_max_retries: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_ws_silent_retry_enabled) return
                          void autoSaveSettingsPatch({
                            codex_ws_silent_max_retries: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSBusyAcquireMaxWait')}
                      description={t('settings.codexWSBusyAcquireMaxWaitDesc')}
                      suffix={t('settings.unit.sec')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={300}
                        value={settingsForm.codex_ws_busy_acquire_max_wait_sec}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_busy_acquire_max_wait_sec: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({
                            codex_ws_busy_acquire_max_wait_sec: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSBusyPatience')}
                      description={t('settings.codexWSBusyPatienceDesc')}
                      suffix={t('settings.unit.sec')}
                      className={cn(!settingsForm.codex_ws_busy_overflow_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={300}
                        disabled={!settingsForm.codex_ws_busy_overflow_enabled}
                        value={settingsForm.codex_ws_busy_patience_sec}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_busy_patience_sec: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_ws_busy_overflow_enabled) return
                          void autoSaveSettingsPatch({
                            codex_ws_busy_patience_sec: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSStatelessSlots')}
                      description={t('settings.codexWSStatelessSlotsDesc')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={32}
                        value={settingsForm.codex_ws_stateless_slots}
                        emptyValue={8}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_stateless_slots: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({
                            codex_ws_stateless_slots: value,
                          })
                        }}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.codexContinueThinking')} description={t('settings.codexContinueThinkingDesc')} icon={<Brain className="size-4" />}>
                <div className="space-y-4">
                  <div className={SETTINGS_SWITCH_ROW}>
                    <SettingField label={t('settings.codexContinueThinking')} description={t('settings.codexContinueThinkingDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_continue_thinking_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_continue_thinking_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField
                      label={t('settings.codexContinueMaxRounds')}
                      description={t('settings.codexContinueMaxRoundsDesc')}
                      className={cn(!settingsForm.codex_continue_thinking_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={32}
                        disabled={!settingsForm.codex_continue_thinking_enabled}
                        value={settingsForm.codex_continue_max_rounds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_continue_max_rounds: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_continue_thinking_enabled) return
                          void autoSaveSettingsPatch({
                            codex_continue_max_rounds: value,
                          })
                        }}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              {/* 三个只有一个开关的兼容项合并成一张卡逐行排列，说明外显；拆成三张窄卡时开关会被挤成半宽折行。 */}
              <SettingsCard title={t('settings.codexCompatToggles')} description={t('settings.codexCompatTogglesDesc')} icon={<Layers className="size-4" />}>
                <div className={SETTINGS_ROW_LIST}>
                  <SettingField
                    label={t('settings.overflowAutoCompact')}
                    description={t('settings.overflowAutoCompactDesc')}
                    help={t('settings.overflowAutoCompactEnabledDesc')}
                    layout="row"
                  >
                    <Switch
                      aria-label={t('settings.overflowAutoCompactEnabled')}
                      checked={settingsForm.overflow_auto_compact_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField('overflow_auto_compact_enabled', checked)}
                    />
                  </SettingField>
                  <SettingField
                    label={t('settings.compactViaResponses')}
                    description={t('settings.compactViaResponsesDesc')}
                    help={t('settings.compactViaResponsesEnabledDesc')}
                    layout="row"
                  >
                    <Switch
                      aria-label={t('settings.compactViaResponsesEnabled')}
                      checked={settingsForm.compact_via_responses_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField('compact_via_responses_enabled', checked)}
                    />
                  </SettingField>
                  <SettingField
                    label={t('settings.codexPreflightSSEPassthrough')}
                    description={t('settings.codexPreflightSSEPassthroughDesc')}
                    help={t('settings.codexPreflightSSEPassthroughEnabledDesc')}
                    layout="row"
                  >
                    <Switch
                      aria-label={t('settings.codexPreflightSSEPassthroughEnabled')}
                      checked={settingsForm.codex_preflight_sse_passthrough_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField('codex_preflight_sse_passthrough_enabled', checked)}
                    />
                  </SettingField>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.codexOverloadPause')} description={t('settings.codexOverloadPauseDesc')} icon={<ShieldAlert className="size-4" />}>
                <div className="space-y-4">
                  <div className={SETTINGS_SWITCH_ROW}>
                    <SettingField label={t('settings.codexOverloadPauseEnabled')} description={t('settings.codexOverloadPauseEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_overload_pause_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_overload_pause_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                  <div className={cn(SETTINGS_FIELD_GRID_3, !settingsForm.codex_overload_pause_enabled && 'opacity-60')}>
                    <SettingField
                      label={t('settings.codexOverloadThreshold')}
                      description={t('settings.codexOverloadThresholdDesc')}
                      suffix="%"
                    >
                      <DraftNumberInput
                        min={1}
                        max={100}
                        disabled={!settingsForm.codex_overload_pause_enabled}
                        value={settingsForm.codex_overload_threshold_percent}
                        emptyValue={20}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_overload_threshold_percent: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ codex_overload_threshold_percent: value })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexOverloadPauseMinutes')}
                      description={t('settings.codexOverloadPauseMinutesDesc')}
                      suffix={t('settings.unit.min')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={1440}
                        disabled={!settingsForm.codex_overload_pause_enabled}
                        value={settingsForm.codex_overload_pause_minutes}
                        emptyValue={30}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_overload_pause_minutes: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ codex_overload_pause_minutes: value })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexOverloadWindow')}
                      description={t('settings.codexOverloadWindowDesc')}
                      suffix={t('settings.unit.min')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={120}
                        disabled={!settingsForm.codex_overload_pause_enabled}
                        value={settingsForm.codex_overload_window_minutes}
                        emptyValue={5}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_overload_window_minutes: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ codex_overload_window_minutes: value })
                        }}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard
                title={t('settings.responseCache.title')}
                description={t('settings.responseCache.description')}
                icon={<Database className="size-4" />}
                badge={
                  <Badge variant="outline" className="text-[11px] tabular-nums">
                    {settingsForm.response_cache_config_generation > 0
                      ? t('settings.responseCache.generation', { value: settingsForm.response_cache_config_generation })
                      : t('settings.responseCache.generationPending')}
                  </Badge>
                }
              >
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField
                      label={t('settings.responseCache.total')}
                      description={t('settings.responseCache.totalDesc')}
                      suffix="MiB"
                    >
                      <DraftNumberInput
                        step={1}
                        integer={true}
                        value={responseCacheBudget.totalMiB}
                        aria-invalid={Boolean(responseCacheValidationError)}
                        onValueChange={(value) => updateResponseCacheBudget('totalMiB', value)}
                        onValueCommit={(value) => commitResponseCacheBudget('totalMiB', value)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.responseCache.entry')}
                      description={t('settings.responseCache.entryDesc')}
                      suffix="MiB"
                    >
                      <DraftNumberInput
                        step={1}
                        integer={true}
                        value={responseCacheBudget.entryMiB}
                        aria-invalid={Boolean(responseCacheValidationError)}
                        onValueChange={(value) => updateResponseCacheBudget('entryMiB', value)}
                        onValueCommit={(value) => commitResponseCacheBudget('entryMiB', value)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.responseCache.reconstruct')}
                      description={t('settings.responseCache.reconstructDesc')}
                      suffix="MiB"
                    >
                      <DraftNumberInput
                        step={1}
                        integer={true}
                        value={responseCacheBudget.reconstructMiB}
                        aria-invalid={Boolean(responseCacheValidationError)}
                        onValueChange={(value) => updateResponseCacheBudget('reconstructMiB', value)}
                        onValueCommit={(value) => commitResponseCacheBudget('reconstructMiB', value)}
                      />
                    </SettingField>
                  </div>
                  {responseCacheValidationMessage ? (
                    <p role="alert" className="text-xs font-medium text-destructive">
                      {responseCacheValidationMessage}
                    </p>
                  ) : null}

                  <SettingField
                    label={t('settings.responseCache.writePolicy')}
                    description={t('settings.responseCache.writePolicyDesc')}
                  >
                    <SegmentedPillGroup
                      value={settingsForm.response_cache_write_policy}
                      onChange={(value) => autoSaveStringField('response_cache_write_policy', value)}
                      options={responseCacheWritePolicyOptions}
                    />
                  </SettingField>

                  {/* 可视化预算分配比例条 (Memory Allocation Bar) */}
                  <div className="rounded-xl border border-border/70 bg-muted/20 p-3.5 space-y-2.5">
                    <div className="flex items-center justify-between text-xs font-semibold">
                      <span className="text-foreground">内存缓存分配预览 (Memory Allocation)</span>
                      <span className="font-mono text-primary font-bold">{responseCacheBudget.totalMiB} MiB</span>
                    </div>
                    <div className="flex h-2.5 w-full overflow-hidden rounded-full bg-muted shadow-inner">
                      <div
                        className="bg-primary transition-all duration-300"
                        style={{ width: `${Math.min(100, Math.round((responseCacheBudget.entryMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100))}%` }}
                      />
                      <div
                        className="bg-amber-500 transition-all duration-300"
                        style={{ width: `${Math.min(100 - Math.round((responseCacheBudget.entryMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100), Math.round((responseCacheBudget.reconstructMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100))}%` }}
                      />
                      <div
                        className="bg-emerald-500/40 flex-1 transition-all duration-300"
                      />
                    </div>
                    <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[11px] text-muted-foreground">
                      <div className="flex items-center gap-1.5">
                        <span className="size-2 rounded-full bg-primary" />
                        <span>{t('settings.responseCache.entry')}: <strong className="text-foreground font-mono">{responseCacheBudget.entryMiB} MiB</strong> ({Math.round((responseCacheBudget.entryMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100)}%)</span>
                      </div>
                      <div className="flex items-center gap-1.5">
                        <span className="size-2 rounded-full bg-amber-500" />
                        <span>{t('settings.responseCache.reconstruct')}: <strong className="text-foreground font-mono">{responseCacheBudget.reconstructMiB} MiB</strong> ({Math.round((responseCacheBudget.reconstructMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100)}%)</span>
                      </div>
                      <div className="flex items-center gap-1.5">
                        <span className="size-2 rounded-full bg-emerald-500/60" />
                        <span>常驻热点池: <strong className="text-foreground font-mono">{Math.max(0, responseCacheBudget.totalMiB - responseCacheBudget.entryMiB - responseCacheBudget.reconstructMiB)} MiB</strong></span>
                      </div>
                    </div>
                  </div>

                  <div className="rounded-lg border border-primary/15 bg-primary/5 px-3.5 py-3 text-xs leading-relaxed text-muted-foreground">
                    <p>{t('settings.responseCache.l1Note')}</p>
                    <p className="mt-1.5">{t('settings.responseCache.memoryNote')}</p>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-codex-client" title={t('settings.nav.codexClient')} description={t('settings.nav.codexClientDesc')} icon={<Terminal className="size-4" />}>
              <SettingsCard title={t('settings.codexClientTitle')} description={t('settings.codexClientDesc')} icon={<Terminal className="size-4" />}>
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField label={t('settings.clientCompatMode')} description={t('settings.clientCompatModeDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.client_compat_mode}
                        onChange={(value) => autoSaveStringField('client_compat_mode', value)}
                        options={clientCompatOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexMinCliVersion')} description={t('settings.codexMinCliVersionDesc')}>
                      <div className="flex items-center gap-2">
                        <Input
                          className="min-w-0 flex-1"
                          value={settingsForm.codex_min_cli_version}
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, codex_min_cli_version: e.target.value }))}
                        />
                        {/* 一键把阈值对齐到当前同步到的 CLI 版本；只改表单值,随「保存设置」落库 */}
                        <Button
                          size="sm"
                          variant="outline"
                          className="shrink-0"
                          disabled={!effectiveCliVersion || settingsForm.codex_min_cli_version.trim() === effectiveCliVersion}
                          title={effectiveCliVersion ? t('settings.codexMinCliVersionUseSyncedDesc', { version: effectiveCliVersion }) : t('settings.codexMinCliVersionNoSynced')}
                          onClick={() => setSettingsForm(f => ({ ...f, codex_min_cli_version: effectiveCliVersion }))}
                        >
                          {t('settings.codexMinCliVersionUseSynced')}
                        </Button>
                      </div>
                    </SettingField>
                    <SettingField label={t('settings.codexCliVersionSync')} description={t('settings.codexCliVersionSyncDesc')}>
                      <div className="flex items-center gap-2">
                        <Button size="sm" variant="outline" onClick={() => void handleSyncCliVersion()} disabled={syncingCliVersion}>
                          <RefreshCw className={cn('size-3.5', syncingCliVersion && 'animate-spin')} />
                          {syncingCliVersion ? t('settings.cliVersionSyncing') : t('settings.cliVersionSyncNow')}
                        </Button>
                        {syncedCliVersion && (
                          <span className="font-mono text-xs text-muted-foreground">{syncedCliVersion}</span>
                        )}
                      </div>
                    </SettingField>
                    {/* CLI 版本自动同步：开关 + 间隔成对横排，行高一致 */}
                    <div className="sm:col-span-2 grid gap-0 overflow-hidden rounded-lg border border-border/60 bg-muted/15 sm:grid-cols-2 sm:divide-x sm:divide-border/60">
                      <div className="flex min-h-[48px] items-center justify-between gap-3 px-3 py-2.5">
                        <div className="flex min-w-0 items-center gap-1.5">
                          <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">
                            {t('settings.codexCliVersionAutoSync')}
                          </span>
                          <SettingHelp text={t('settings.codexCliVersionAutoSyncDesc')} />
                        </div>
                        <Switch
                          checked={settingsForm.codex_cli_version_sync_enabled}
                          onCheckedChange={(checked) => autoSaveBooleanField('codex_cli_version_sync_enabled', checked)}
                        />
                      </div>
                      <div
                        className={cn(
                          'flex min-h-[48px] items-center justify-between gap-3 border-t border-border/60 px-3 py-2.5 sm:border-t-0',
                          !settingsForm.codex_cli_version_sync_enabled && 'opacity-60',
                        )}
                      >
                        <div className="flex min-w-0 items-center gap-1.5">
                          <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">
                            {t('settings.codexCliVersionSyncInterval')}
                          </span>
                          <SettingHelp text={t('settings.codexCliVersionSyncIntervalDesc')} />
                        </div>
                        <div className="relative w-[7.25rem] shrink-0">
                          <DraftNumberInput
                            min={1}
                            max={720}
                            className="h-9 pr-10 tabular-nums"
                            disabled={!settingsForm.codex_cli_version_sync_enabled}
                            value={settingsForm.codex_cli_version_sync_interval_hours}
                            onValueChange={(value) =>
                              setSettingsForm((f) => ({
                                ...f,
                                codex_cli_version_sync_interval_hours: value,
                              }))
                            }
                            onValueCommit={(value) => {
                              if (!settingsForm.codex_cli_version_sync_enabled) return
                              void autoSaveSettingsPatch({
                                codex_cli_version_sync_interval_hours: value,
                              })
                            }}
                          />
                          <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium text-muted-foreground">
                            {t('settings.unit.hour')}
                          </span>
                        </div>
                      </div>
                    </div>
                    <SettingField label={t('settings.utlsShutdownTimeout')} description={t('settings.utlsShutdownTimeoutDesc')}>
                      <div className="relative">
                        <DraftNumberInput
                          min={1}
                          max={240}
                          className="pr-12 tabular-nums"
                          value={settingsForm.utls_shutdown_timeout_minutes}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, utls_shutdown_timeout_minutes: value }))}
                          onValueCommit={(value) => {
                            void autoSaveSettingsPatch({
                              utls_shutdown_timeout_minutes: value,
                            })
                          }}
                        />
                        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium text-muted-foreground">
                          {t('settings.unit.min')}
                        </span>
                      </div>
                    </SettingField>
                    <SettingField label={t('settings.codexFingerprintDefaultMode')} description={t('settings.codexFingerprintDefaultModeDesc')}>
                      <Select
                        value={settingsForm.codex_fingerprint_default_mode || 'off'}
                        onValueChange={(value) => autoSaveStringField('codex_fingerprint_default_mode', value)}
                        options={codexFingerprintDefaultModeOptions}
                      />
                    </SettingField>
                    <SettingField className="sm:col-span-2 xl:col-span-3" label={t('settings.codexUserAgentRaw')} description={t('settings.codexUserAgentRawDesc')}>
                      <Input
                        className="font-mono text-xs"
                        value={codexUserAgentConfig.raw_user_agent ?? ''}
                        placeholder="codex-tui/0.153.3 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.153.3)"
                        onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ raw_user_agent: e.target.value })}
                        onBlur={saveCodexUserAgentConfig}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexUAClientName')} description={t('settings.codexUAClientNameDesc')}>
                      <Input
                        value={codexUserAgentConfig.client_name ?? ''}
                        placeholder={DEFAULT_CODEX_UA_CONFIG.client_name}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ client_name: e.target.value })}
                        onBlur={saveCodexUserAgentConfig}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexUAClientVersion')} description={t('settings.codexUAClientVersionDesc')}>
                      <Input
                        value={codexUserAgentConfig.client_version ?? ''}
                        placeholder={DEFAULT_CODEX_UA_CONFIG.client_version}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ client_version: e.target.value })}
                        onBlur={saveCodexUserAgentConfig}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexUAOSName')} description={t('settings.codexUAOSNameDesc')}>
                      <Input
                        value={codexUserAgentConfig.os_name ?? ''}
                        placeholder={DEFAULT_CODEX_UA_CONFIG.os_name}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ os_name: e.target.value })}
                        onBlur={saveCodexUserAgentConfig}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexUAOSVersion')} description={t('settings.codexUAOSVersionDesc')}>
                      <Input
                        value={codexUserAgentConfig.os_version ?? ''}
                        placeholder={DEFAULT_CODEX_UA_CONFIG.os_version}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ os_version: e.target.value })}
                        onBlur={saveCodexUserAgentConfig}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexUAArch')} description={t('settings.codexUAArchDesc')}>
                      <Input
                        value={codexUserAgentConfig.arch ?? ''}
                        placeholder={DEFAULT_CODEX_UA_CONFIG.arch}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ arch: e.target.value })}
                        onBlur={saveCodexUserAgentConfig}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexUATerminal')} description={t('settings.codexUATerminalDesc')}>
                      <Input
                        value={codexUserAgentConfig.terminal ?? ''}
                        placeholder={DEFAULT_CODEX_UA_CONFIG.terminal}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ terminal: e.target.value })}
                        onBlur={saveCodexUserAgentConfig}
                      />
                    </SettingField>
                    <div className="min-w-0 rounded-lg border border-border/70 bg-muted/25 p-3 sm:col-span-2 xl:col-span-3">
                      <div className="mb-1.5 text-[13px] font-medium text-foreground">{t('settings.codexUAPreview')}</div>
                      <div className="break-all font-mono text-[11px] leading-5 text-muted-foreground">{codexUserAgentPreview}</div>
                    </div>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-models" title={t('settings.nav.models')} description={t('settings.nav.modelsDesc')} icon={<Layers className="size-4" />}>
                <div className="flex flex-wrap items-center justify-between gap-2.5 rounded-lg border border-border/80 bg-card/80 px-3.5 py-2.5 shadow-sm">
                  <div className="flex min-w-0 flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    <Badge variant="secondary" className="tabular-nums">
                      {t('settings.modelsEnabled')}: {enabledModelCount}
                    </Badge>
                    <span className="hidden sm:inline text-border">·</span>
                    <span className="truncate">
                      {t('settings.modelsLastSynced')}: {modelsLastSyncedLabel}
                    </span>
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    <a
                      href={modelsSourceLabel}
                      target="_blank"
                      rel="noreferrer"
                      className="inline-flex items-center gap-1 text-xs font-semibold text-primary hover:underline"
                    >
                      <ExternalLink className="size-3.5" />
                      {t('settings.nav.openSource')}
                    </a>
                    <Button size="sm" variant="outline" onClick={() => void handleSyncModels()} disabled={syncingModels}>
                      <RefreshCw className={cn('size-3.5', syncingModels && 'animate-spin')} />
                      {syncingModels ? t('settings.modelsSyncing') : t('settings.syncUpstreamModels')}
                    </Button>
                  </div>
                </div>
                <div className="grid gap-3 sm:grid-cols-2">
                  <ModelSummaryCard
                    title={t('settings.modelRegistry')}
                    description={t('settings.modelRegistryDesc')}
                    meta={t('settings.nav.modelCount', { count: enabledModelCount })}
                    openLabel={t('settings.nav.manage')}
                    onOpen={() => setModelPanel('registry')}
                  />
                  <ModelSummaryCard
                    title={t('settings2.anthropicModelMapping')}
                    description={t('settings2.anthropicModelMappingDesc')}
                    meta={t('settings.nav.mappingCount', { count: anthropicMappingCount })}
                    openLabel={t('settings.nav.manage')}
                    onOpen={() => setModelPanel('anthropic')}
                  />
                  <ModelSummaryCard
                    title={t('settings2.codexModelMapping')}
                    description={t('settings2.codexModelMappingDesc')}
                    meta={t('settings.nav.mappingCount', { count: codexMappingCount })}
                    openLabel={t('settings.nav.manage')}
                    onOpen={() => setModelPanel('codex')}
                  />
                  <ModelSummaryCard
                    title={t('settings2.reasoningEffortModels')}
                    description={t('settings2.reasoningEffortModelsDesc')}
                    meta={t('settings.nav.mappingCount', { count: reasoningEffortCount })}
                    openLabel={t('settings.nav.manage')}
                    onOpen={() => setModelPanel('reasoning')}
                  />
                  <ModelSummaryCard
                    title={t('settings2.payloadRules')}
                    description={t('settings2.payloadRulesDesc')}
                    meta={t('settings.nav.mappingCount', { count: payloadRuleCount })}
                    openLabel={t('settings.nav.manage')}
                    onOpen={() => navigate('/payload-rules')}
                  />
                </div>

                <Sheet open={modelPanel !== null} onOpenChange={(open) => { if (!open) setModelPanel(null) }}>
                  <SheetContent
                    side="right"
                    className="sm:w-[min(calc(100%-2rem),720px)] sm:max-w-[min(calc(100%-2rem),720px)]"
                  >
                    <SheetHeader>
                      <SheetTitle>
                        {modelPanel === 'registry'
                          ? t('settings.modelRegistry')
                          : modelPanel === 'anthropic'
                            ? t('settings2.anthropicModelMapping')
                            : modelPanel === 'codex'
                              ? t('settings2.codexModelMapping')
                              : t('settings2.reasoningEffortModels')}
                      </SheetTitle>
                      <SheetDescription>
                        {modelPanel === 'registry'
                          ? t('settings.modelRegistryDesc')
                          : modelPanel === 'anthropic'
                            ? t('settings2.anthropicModelMappingDesc')
                            : modelPanel === 'codex'
                              ? t('settings2.codexModelMappingDesc')
                              : t('settings2.reasoningEffortModelsDesc')}
                      </SheetDescription>
                    </SheetHeader>
                    <SheetBody className="space-y-4">
                      {modelPanel === 'registry' ? (
                        <div className="space-y-3">
                          <div className="grid grid-cols-2 gap-3">
                            <StatusTile label={t('settings.modelsEnabled')}>{enabledModelCount}</StatusTile>
                            <StatusTile label={t('settings.modelsLastSynced')}>
                              <span className="text-xs font-semibold">{modelsLastSyncedLabel}</span>
                            </StatusTile>
                          </div>
                          <div className="flex max-h-[min(60dvh,520px)] flex-wrap content-start gap-2 overflow-auto rounded-xl border border-border bg-muted/20 p-3">
                            {visibleModelItems.map((model) => (
                              <div
                                key={model.id}
                                className="flex h-fit flex-wrap items-center gap-1.5 rounded-md border border-border bg-background px-2.5 py-1.5"
                              >
                                <span className="font-mono text-xs font-semibold text-foreground">{model.id}</span>
                                <Badge
                                  variant={model.source === 'official_codex_docs' ? 'default' : 'secondary'}
                                  className="text-[11px]"
                                >
                                  {model.source === 'official_codex_docs'
                                    ? t('settings.modelSourceOfficial')
                                    : model.source === 'reasoning_effort'
                                      ? t('settings.modelSourceReasoning')
                                      : t('settings.modelSourceBuiltin')}
                                </Badge>
                                {model.pro_only ? (
                                  <Badge variant="outline" className="text-[11px]">{t('settings.modelProOnly')}</Badge>
                                ) : null}
                                {model.category === 'image' ? (
                                  <Badge variant="outline" className="text-[11px]">{t('settings.modelImage')}</Badge>
                                ) : null}
                              </div>
                            ))}
                          </div>
                        </div>
                      ) : null}
                      {modelPanel === 'anthropic' ? (
                        <ModelMappingEditor
                          value={settingsForm.model_mapping}
                          onChange={(v) => setSettingsForm((f) => ({ ...f, model_mapping: v }))}
                          fallbackEntries={defaultClaudeModelMappingEntries}
                          sourceLabel={t('settings2.anthropicModel')}
                          targetLabel={t('settings2.codexModel')}
                          sourcePlaceholder="claude-opus-4-6"
                          targetPlaceholder="gpt-5.5"
                        />
                      ) : null}
                      {modelPanel === 'codex' ? (
                        <ModelMappingEditor
                          value={settingsForm.codex_model_mapping}
                          onChange={(v) => setSettingsForm((f) => ({ ...f, codex_model_mapping: v }))}
                          sourceOptions={codexModelOptions}
                          targetOptions={codexModelOptions}
                          sourceLabel={t('settings2.requestedModel')}
                          targetLabel={t('settings2.targetModel')}
                          sourcePlaceholder="gpt-5.2"
                          targetPlaceholder="gpt-5.5"
                        />
                      ) : null}
                      {modelPanel === 'reasoning' ? (
                        <ReasoningEffortModelsEditor
                          value={settingsForm.reasoning_effort_models}
                          onChange={(v) => setSettingsForm((f) => ({ ...f, reasoning_effort_models: v }))}
                          baseModelOptions={textModelOptions}
                        />
                      ) : null}
                    </SheetBody>
                  </SheetContent>
                </Sheet>
              </SettingsSection>
            </>
          ) : null}

          {activeTab === 'claude' ? (
            <>
              <SettingsSection id="settings-claude" title={t('settings.nav.claude')} description={t('settings.nav.claudeDesc')} icon={<ChannelLogo channel="claude" size={16} />}>
                <ChannelConnectivityTestCard channel="claude" />
                <ClaudeCodeSettingsCard />
              </SettingsSection>
            </>
          ) : null}

          {activeTab === 'antigravity' ? (
            <>
              <SettingsSection id="settings-antigravity" title={t('settings.nav.antigravity')} description={t('settings.nav.antigravityDesc')} icon={<ChannelLogo channel="antigravity" size={16} />}>
              <ChannelConnectivityTestCard channel="antigravity" />
              <AntigravityModelRedirectCard />
              <SettingsCard
                title={t('settings.antigravityOAuth.title')}
                description={t('settings.antigravityOAuth.description')}
                icon={<Shield className="size-4" />}
              >
                <div className="space-y-4">
                  {(settingsForm.antigravity_oauth_env_clients?.length ?? 0) > 0 && (
                    <div className="space-y-1.5 rounded-md border border-dashed border-border/70 p-3 text-xs text-muted-foreground">
                      <div>{t('settings.antigravityOAuth.envClientsHint')}</div>
                      {settingsForm.antigravity_oauth_env_clients?.map(client => (
                        <div key={client.key} className="flex items-center gap-2 font-mono">
                          <Badge variant="outline" className="text-[11px]">{client.key}</Badge>
                          <span className="truncate">{client.client_id}</span>
                        </div>
                      ))}
                    </div>
                  )}
                  {settingsForm.antigravity_oauth_using_builtin && settingsForm.antigravity_oauth_builtin_client && (
                    <div className="space-y-1.5 rounded-md border border-dashed border-border/70 p-3 text-xs text-muted-foreground">
                      <div>{t('settings.antigravityOAuth.builtinHint')}</div>
                      <div className="flex items-center gap-2 font-mono">
                        <Badge variant="outline" className="text-[11px]">{settingsForm.antigravity_oauth_builtin_client.key}</Badge>
                        <span className="truncate">{settingsForm.antigravity_oauth_builtin_client.client_id}</span>
                      </div>
                    </div>
                  )}
                  {agOAuth.rows.length === 0 ? (
                    <div className="text-sm text-muted-foreground">{t('settings.antigravityOAuth.empty')}</div>
                  ) : (
                    <div className="space-y-2">
                      <div className="hidden gap-2 text-xs text-muted-foreground sm:grid sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_minmax(0,2fr)_2rem]">
                        <span>{t('settings.antigravityOAuth.key')}</span>
                        <span>{t('settings.antigravityOAuth.clientId')}</span>
                        <span>{t('settings.antigravityOAuth.clientSecret')}</span>
                        <span />
                      </div>
                      {agOAuth.rows.map((row, index) => (
                        <div key={index} className="grid items-center gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_minmax(0,2fr)_2rem]">
                          <Input
                            value={row.key}
                            placeholder={t('settings.antigravityOAuth.keyPlaceholder')}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => updateAgOAuthRow(index, { key: e.target.value })}
                          />
                          <Input
                            value={row.client_id}
                            placeholder={t('settings.antigravityOAuth.clientIdPlaceholder')}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => updateAgOAuthRow(index, { client_id: e.target.value })}
                          />
                          <Input
                            type="password"
                            autoComplete="new-password"
                            value={row.client_secret ?? ''}
                            placeholder={row.has_secret ? t('settings.antigravityOAuth.secretKeepPlaceholder') : t('settings.antigravityOAuth.secretRequiredPlaceholder')}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => updateAgOAuthRow(index, { client_secret: e.target.value })}
                          />
                          <Button
                            variant="ghost"
                            size="icon"
                            aria-label={t('common.delete')}
                            onClick={() => removeAgOAuthRow(index)}
                          >
                            <Trash2 className="size-4" />
                          </Button>
                        </div>
                      ))}
                    </div>
                  )}
                  <Button variant="outline" size="sm" onClick={addAgOAuthRow}>
                    {t('settings.antigravityOAuth.addClient')}
                  </Button>
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField
                      label={t('settings.antigravityOAuth.activeKey')}
                      description={
                        settingsForm.antigravity_oauth_client_key_env_override
                          ? t('settings.antigravityOAuth.activeKeyEnvOverride', {
                              value: settingsForm.antigravity_oauth_active_key_effective || '',
                            })
                          : t('settings.antigravityOAuth.activeKeyDesc')
                      }
                    >
                      <Select
                        value={agOAuth.activeKey}
                        disabled={settingsForm.antigravity_oauth_client_key_env_override}
                        onValueChange={(value: string) => setAgOAuthDraft({ ...agOAuth, activeKey: value })}
                        options={[
                          { label: t('settings.antigravityOAuth.activeKeyAuto'), value: '' },
                          ...agOAuth.rows
                            .map(row => row.key.trim().toLowerCase())
                            .filter(key => key !== '')
                            .map(key => ({ label: key, value: key })),
                        ]}
                      />
                    </SettingField>
                  </div>
                  <div className="flex items-center gap-2">
                    <Button size="sm" onClick={() => void saveAgOAuth()} disabled={!agOAuthDirty || agOAuthSaving}>
                      {agOAuthSaving ? t('common.saving') : t('common.save')}
                    </Button>
                    {agOAuthDirty && !agOAuthSaving && (
                      <Button variant="ghost" size="sm" onClick={() => setAgOAuthDraft(null)}>
                        {t('common.cancel')}
                      </Button>
                    )}
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>
            </>
          ) : null}

          {activeTab === 'grok' ? (
            <>
              <SettingsSection id="settings-grok" title={t('settings.nav.grok')} description={t('settings.nav.grokDesc')} icon={<ChannelLogo channel="grok" size={16} />}>
              <SettingsCard title={t('settings.grokSettingsTitle')} description={t('settings.grokSettingsDesc')} icon={<ChannelLogo channel="grok" size={16} />}>
                {/* 与「探测调度」一致：表单控件同宽网格，开关单独一行，避免 switch 卡片与 input 混排导致高低宽不一致。 */}
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField label={t('settings.grokAffinityMode')} description={t('settings.grokAffinityModeDesc')}>
                      <Select
                        value={settingsForm.grok_affinity_mode || 'strict'}
                        onValueChange={(value) => autoSaveStringField('grok_affinity_mode', value)}
                        options={grokAffinityModeOptions}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.grokProbeInterval')}
                      description={t('settings.grokProbeIntervalDesc')}
                      suffix={t('settings.unit.min')}
                    >
                      <DraftNumberInput
                        min={5}
                        max={1440}
                        step={5}
                        integer
                        emptyValue={30}
                        disabled={!settingsForm.grok_probe_enabled}
                        value={settingsForm.grok_probe_interval_minutes ?? 30}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, grok_probe_interval_minutes: value }))
                        }}
                        onValueCommit={(value) => {
                          const v = value < 5 ? 5 : value
                          void autoSaveSettingsPatch({ grok_probe_interval_minutes: v })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.grokMaxRateLimitRetries')}
                      description={t('settings.grokMaxRateLimitRetriesDesc')}
                      suffix={t('settings.unit.times')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={20}
                        step={1}
                        integer
                        emptyValue={0}
                        value={settingsForm.grok_max_rate_limit_retries ?? 0}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, grok_max_rate_limit_retries: value }))
                        }}
                        onValueCommit={(value) => {
                          const v = value < 0 ? 0 : value
                          void autoSaveSettingsPatch({ grok_max_rate_limit_retries: v })
                        }}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_SWITCH_GRID}>
                    <SettingField label={t('settings.grokProbeEnabled')} description={t('settings.grokProbeEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.grok_probe_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('grok_probe_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.grokFollowUpEffortEnabled')} description={t('settings.grokFollowUpEffortEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.grok_follow_up_effort_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('grok_follow_up_effort_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField label={t('settings.grokFollowUpToolEffort')} description={t('settings.grokFollowUpToolEffortDesc')}>
                      <Select
                        value={settingsForm.grok_follow_up_tool_effort || 'medium'}
                        disabled={!settingsForm.grok_follow_up_effort_enabled}
                        onValueChange={(value) => autoSaveStringField('grok_follow_up_tool_effort', value)}
                        options={grokFollowUpEffortOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.grokFollowUpSmallEffort')} description={t('settings.grokFollowUpSmallEffortDesc')}>
                      <Select
                        value={settingsForm.grok_follow_up_small_effort || 'low'}
                        disabled={!settingsForm.grok_follow_up_effort_enabled}
                        onValueChange={(value) => autoSaveStringField('grok_follow_up_small_effort', value)}
                        options={grokFollowUpEffortOptions}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_SWITCH_ROW}>
                    <SettingField label={t('settings.grokQualityGuardEnabled')} description={t('settings.grokQualityGuardEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.grok_quality_guard_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('grok_quality_guard_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField
                      label={t('settings.grokQualityGuardMaxAttempts')}
                      description={t('settings.grokQualityGuardMaxAttemptsDesc')}
                      suffix={t('settings.unit.times')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={20}
                        step={1}
                        integer
                        emptyValue={6}
                        disabled={!settingsForm.grok_quality_guard_enabled}
                        value={settingsForm.grok_quality_guard_max_attempts ?? 6}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, grok_quality_guard_max_attempts: value }))
                        }}
                        onValueCommit={(value) => {
                          const v = value < 1 ? 1 : value
                          void autoSaveSettingsPatch({ grok_quality_guard_max_attempts: v })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.grokQualityGuardHoldTimeout')}
                      description={t('settings.grokQualityGuardHoldTimeoutDesc')}
                      suffix={t('settings.unit.sec')}
                    >
                      <DraftNumberInput
                        min={5}
                        max={300}
                        step={5}
                        integer
                        emptyValue={30}
                        disabled={!settingsForm.grok_quality_guard_enabled}
                        value={settingsForm.grok_quality_guard_hold_timeout_sec ?? 30}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, grok_quality_guard_hold_timeout_sec: value }))
                        }}
                        onValueCommit={(value) => {
                          const v = value < 5 ? 5 : value
                          void autoSaveSettingsPatch({ grok_quality_guard_hold_timeout_sec: v })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.grokQualityGuardCooldownHours')}
                      description={t('settings.grokQualityGuardCooldownHoursDesc')}
                      suffix={t('settings.unit.hour')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={168}
                        step={1}
                        integer
                        emptyValue={12}
                        disabled={!settingsForm.grok_quality_guard_enabled}
                        value={settingsForm.grok_quality_guard_account_cooldown_hours ?? 12}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, grok_quality_guard_account_cooldown_hours: value }))
                        }}
                        onValueCommit={(value) => {
                          const v = value < 1 ? 1 : value
                          void autoSaveSettingsPatch({ grok_quality_guard_account_cooldown_hours: v })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.grokQualityGuardOnExhausted')} description={t('settings.grokQualityGuardOnExhaustedDesc')}>
                      <Select
                        value={settingsForm.grok_quality_guard_on_exhausted || 'fail_closed'}
                        disabled={!settingsForm.grok_quality_guard_enabled}
                        onValueChange={(value) => autoSaveStringField('grok_quality_guard_on_exhausted', value)}
                        options={grokQualityGuardOnExhaustedOptions}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_FIELD_GRID_3}>
                    {/* client_id 同时可由环境变量 GROK_OAUTH_CLIENT_ID 指定，且环境变量优先级更高；
                        被覆盖时这里禁用输入并说明当前生效值，避免用户以为改了却不起作用。 */}
                    <SettingField
                      className="sm:col-span-2 xl:col-span-3"
                      label={t('settings.grokOAuthClientId')}
                      description={
                        settingsForm.grok_oauth_client_id_env_override
                          ? t('settings.grokOAuthClientIdEnvOverride', {
                              value: settingsForm.grok_oauth_client_id_effective || '',
                            })
                          : t('settings.grokOAuthClientIdDesc')
                      }
                    >
                      <Input
                        value={settingsForm.grok_oauth_client_id ?? ''}
                        disabled={settingsForm.grok_oauth_client_id_env_override}
                        placeholder={t('settings.grokOAuthClientIdPlaceholder')}
                        onChange={(e: ChangeEvent<HTMLInputElement>) =>
                          setSettingsForm(f => ({ ...f, grok_oauth_client_id: e.target.value }))
                        }
                        onBlur={(e) => autoSaveStringField('grok_oauth_client_id', e.currentTarget.value.trim())}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>
            </>
          ) : null}

          {activeTab === 'appearance' ? (
            <>
              <SettingsSection id="settings-appearance" title={t('settings.nav.appearance')} description={t('settings.nav.appearanceDesc')} icon={<Palette className="size-4" />}>
                <SettingsCard title={t('settings.display')} icon={<Palette className="size-4" />}>
                  <div className="space-y-4">
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.siteName')} description={t('settings.siteNameDesc')}>
                        <Input
                          value={settingsForm.site_name}
                          maxLength={80}
                          placeholder="CodexProxy"
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, site_name: e.target.value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.timezone')} description={t('settings.timezoneDesc')}>
                        <Select
                          value={getTimezone()}
                          onValueChange={(value) => {
                            setTimezone(value)
                            window.location.reload()
                          }}
                          options={[
                            { label: t('settings.timezoneAuto'), value: Intl.DateTimeFormat().resolvedOptions().timeZone },
                            { label: '(UTC) UTC', value: 'UTC' },
                            { label: '(GMT+08:00) Asia/Shanghai', value: 'Asia/Shanghai' },
                            { label: '(GMT+09:00) Asia/Tokyo', value: 'Asia/Tokyo' },
                            { label: '(GMT+09:00) Asia/Seoul', value: 'Asia/Seoul' },
                            { label: '(GMT+08:00) Asia/Singapore', value: 'Asia/Singapore' },
                            { label: '(GMT+08:00) Asia/Hong_Kong', value: 'Asia/Hong_Kong' },
                            { label: '(GMT+08:00) Asia/Taipei', value: 'Asia/Taipei' },
                            { label: '(GMT+07:00) Asia/Bangkok', value: 'Asia/Bangkok' },
                            { label: '(GMT+04:00) Asia/Dubai', value: 'Asia/Dubai' },
                            { label: '(GMT+05:30) Asia/Kolkata', value: 'Asia/Kolkata' },
                            { label: '(GMT+01:00) Europe/London', value: 'Europe/London' },
                            { label: '(GMT+02:00) Europe/Paris', value: 'Europe/Paris' },
                            { label: '(GMT+02:00) Europe/Berlin', value: 'Europe/Berlin' },
                            { label: '(GMT+03:00) Europe/Moscow', value: 'Europe/Moscow' },
                            { label: '(GMT+02:00) Europe/Amsterdam', value: 'Europe/Amsterdam' },
                            { label: '(GMT+02:00) Europe/Rome', value: 'Europe/Rome' },
                            { label: '(GMT-04:00) America/New_York', value: 'America/New_York' },
                            { label: '(GMT-07:00) America/Los_Angeles', value: 'America/Los_Angeles' },
                            { label: '(GMT-05:00) America/Chicago', value: 'America/Chicago' },
                            { label: '(GMT-03:00) America/Sao_Paulo', value: 'America/Sao_Paulo' },
                            { label: '(GMT+10:00) Australia/Sydney', value: 'Australia/Sydney' },
                            { label: '(GMT+12:00) Pacific/Auckland', value: 'Pacific/Auckland' },
                          ]}
                        />
                      </SettingField>
                    </div>
                    <SettingField label={t('settings.siteLogo')} description={t('settings.siteLogoDesc')}>
                      <div className="flex items-center gap-3">
                        <div className="flex size-11 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-border bg-background shadow-sm">
                          <img src={siteLogoPreview} alt={settingsForm.site_name || 'CodexProxy'} className="size-full object-cover" />
                        </div>
                        <div className="min-w-0 flex-1 space-y-2">
                          <Input
                            value={settingsForm.site_logo}
                            placeholder="/favicon.png or https://..."
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, site_logo: e.target.value }))}
                          />
                          <div className="flex flex-wrap gap-2">
                            <Button type="button" variant="outline" size="sm" onClick={() => logoFileInputRef.current?.click()}>
                              <Upload className="size-3.5" />
                              {t('settings.siteLogoUpload')}
                            </Button>
                            <Button type="button" variant="ghost" size="sm" onClick={() => setSettingsForm(f => ({ ...f, site_logo: '' }))}>
                              <X className="size-3.5" />
                              {t('settings.siteLogoReset')}
                            </Button>
                          </div>
                          <input
                            ref={logoFileInputRef}
                            type="file"
                            accept="image/png,image/jpeg,image/svg+xml,.png,.jpg,.jpeg,.svg"
                            className="hidden"
                            onChange={handleSiteLogoUpload}
                          />
                        </div>
                      </div>
                    </SettingField>
                    <div className={SETTINGS_SWITCH_ROW}>
                      <SettingField label={t('settings.showFullUsageNumbers')} description={t('settings.showFullUsageNumbersDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.show_full_usage_numbers}
                          onCheckedChange={(checked) => autoSaveBooleanField('show_full_usage_numbers', checked)}
                        />
                      </SettingField>
                    </div>
                  </div>
                </SettingsCard>

              <SettingsCard title={t('settings.backgroundImage')} description={t('settings.backgroundImageDesc')} icon={<ImageIcon className="size-4" />}>
                <div className="grid gap-5 xl:grid-cols-[minmax(0,1.4fr)_minmax(280px,0.6fr)] xl:items-start">
                  <div className="relative aspect-[16/7] min-h-[180px] overflow-hidden rounded-xl border border-border/80 bg-muted/40 shadow-inner max-sm:aspect-[4/3] sm:min-h-[220px]">
                    {backgroundImagePreview && backgroundIsVideo ? (
                      <video
                        src={backgroundImagePreview}
                        className="size-full object-cover"
                        style={{
                          opacity: Math.min(100, Math.max(0, settingsForm.background_opacity)) / 100,
                          filter: settingsForm.background_blur > 0 ? `blur(${settingsForm.background_blur}px)` : undefined,
                          transform: settingsForm.background_blur > 0 ? 'scale(1.04)' : undefined,
                        }}
                        autoPlay
                        muted
                        loop
                        playsInline
                      />
                    ) : backgroundImagePreview ? (
                      <img
                        src={backgroundImagePreview}
                        alt=""
                        className="size-full object-cover"
                        style={{
                          opacity: Math.min(100, Math.max(0, settingsForm.background_opacity)) / 100,
                          filter: settingsForm.background_blur > 0 ? `blur(${settingsForm.background_blur}px)` : undefined,
                          transform: settingsForm.background_blur > 0 ? 'scale(1.04)' : undefined,
                        }}
                      />
                    ) : (
                      <div className="flex size-full items-center justify-center text-xs font-medium text-muted-foreground bg-gradient-to-br from-primary/10 via-muted/30 to-background">
                        {t('settings.backgroundImageEmpty')}
                      </div>
                    )}
                    {/* 毛玻璃效果实时预览卡片 (Glassmorphism Live Preview Overlay) */}
                    <div
                      className="absolute inset-x-4 bottom-4 flex items-center justify-between rounded-xl border border-white/20 bg-background/60 p-3 shadow-lg backdrop-blur-md dark:border-white/10 dark:bg-black/50"
                      style={{
                        backgroundColor: `rgba(var(--bg-card-rgb, 255, 255, 255), ${Math.min(1, Math.max(0, settingsForm.background_glass_opacity / 100))})`,
                        backdropFilter: `blur(${settingsForm.background_glass_blur}px)`,
                        WebkitBackdropFilter: `blur(${settingsForm.background_glass_blur}px)`,
                      }}
                    >
                      <div className="flex items-center gap-2.5">
                        {siteLogoPreview ? (
                          <img src={siteLogoPreview} alt="" className="size-6 rounded object-contain shadow-2xs" />
                        ) : (
                          <div className="size-6 rounded bg-primary/20 flex items-center justify-center text-[10px] font-bold text-primary">
                            CP
                          </div>
                        )}
                        <div className="space-y-0.5">
                          <div className="text-xs font-bold leading-none text-foreground">{settingsForm.site_name || 'CodexProxy'}</div>
                          <div className="text-[10px] text-muted-foreground">毛玻璃预览 Glass Effect</div>
                        </div>
                      </div>
                      <Badge variant="outline" className="text-[10px] border-primary/30 bg-primary/10 text-primary font-mono">
                        {settingsForm.background_glass_opacity}% glass
                      </Badge>
                    </div>
                  </div>
                  <div className="flex min-w-0 flex-col gap-4">
                    <div className="min-w-0 space-y-2.5">
                      <Input
                        value={settingsForm.background_image}
                        placeholder="/wallpaper.jpg or https://..."
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_image: e.target.value }))}
                      />
                      <div className="flex flex-wrap gap-2">
                        <Button type="button" variant="outline" size="sm" onClick={() => backgroundFileInputRef.current?.click()}>
                          <Upload className="size-3.5" />
                          {t('settings.backgroundImageUpload')}
                        </Button>
                        <Button type="button" variant="ghost" size="sm" onClick={() => setSettingsForm(f => ({ ...f, background_image: '' }))}>
                          <X className="size-3.5" />
                          {t('settings.backgroundImageReset')}
                        </Button>
                      </div>
                      <input
                        ref={backgroundFileInputRef}
                        type="file"
                        accept="image/png,image/jpeg,image/webp,image/svg+xml,video/mp4,.png,.jpg,.jpeg,.webp,.svg,.mp4"
                        className="hidden"
                        onChange={handleBackgroundImageUpload}
                      />
                    </div>
                    <div className="grid gap-3.5 rounded-lg border border-border/60 bg-muted/15 p-3.5">
                      {([
                        {
                          label: t('settings.backgroundOpacity'),
                          value: settingsForm.background_opacity,
                          unit: '%',
                          min: 0,
                          max: 100,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_opacity: v })),
                        },
                        {
                          label: t('settings.backgroundBlur'),
                          value: settingsForm.background_blur,
                          unit: 'px',
                          min: 0,
                          max: 24,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_blur: v })),
                        },
                        {
                          label: t('settings.backgroundGlassOpacity'),
                          value: settingsForm.background_glass_opacity,
                          unit: '%',
                          min: 0,
                          max: 100,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_glass_opacity: v })),
                        },
                        {
                          label: t('settings.backgroundGlassBlur'),
                          value: settingsForm.background_glass_blur,
                          unit: 'px',
                          min: 0,
                          max: 20,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_glass_blur: v })),
                        },
                      ] as const).map((slider) => (
                        <div key={slider.label} className="space-y-1.5">
                          <div className="flex items-center justify-between gap-3 text-xs">
                            <span className="font-medium text-muted-foreground">{slider.label}</span>
                            <span className="min-w-[3rem] text-right font-semibold tabular-nums text-foreground">
                              {slider.value}{slider.unit}
                            </span>
                          </div>
                          <input
                            type="range"
                            min={slider.min}
                            max={slider.max}
                            value={slider.value}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => slider.onChange(parseInt(e.target.value) || 0)}
                            className="h-1.5 w-full cursor-pointer accent-primary"
                          />
                        </div>
                      ))}
                    </div>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>
            </>
          ) : null}

          {activeTab === 'general' ? (
            <>
              <SettingsSection id="settings-overview" title={t('settings.nav.overview')} description={t('settings.nav.overviewDesc')} icon={<Activity className="size-4" />}>
              <SettingsCard
                title={t('settings.systemStatus')}
                icon={<Activity className="size-4" />}
                badge={
                  <Badge variant="secondary" className="text-[11px]">
                    {t('settings.nav.live')}
                  </Badge>
                }
              >
                <div className="grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-4">
                  <StatusTile label={t('settings.service')} icon={<Activity className="size-4" />}>
                    <Badge variant={health?.status === 'ok' ? 'default' : 'destructive'} className="gap-1.5">
                      <span className={`size-1.5 rounded-full ${health?.status === 'ok' ? 'bg-emerald-500' : 'bg-red-400'}`} />
                      {health?.status === 'ok' ? t('common.running') : t('common.error')}
                    </Badge>
                  </StatusTile>
                  <StatusTile label={t('settings.accountsLabel')} icon={<Users className="size-4" />}>
                    {health?.available ?? 0} / {health?.total ?? 0}
                  </StatusTile>
                  <StatusTile label={settingsForm.database_label} icon={<Database className="size-4" />}>
                    <Badge variant="default" className="gap-1.5">
                      <span className="size-1.5 rounded-full bg-emerald-500" />
                      {isExternalDatabase ? t('common.connected') : t('common.running')}
                    </Badge>
                  </StatusTile>
                  <StatusTile label={settingsForm.cache_label} icon={<Layers className="size-4" />}>
                    <Badge variant="default" className="gap-1.5">
                      <span className="size-1.5 rounded-full bg-emerald-500" />
                      {isExternalCache ? t('common.connected') : t('common.running')}
                    </Badge>
                  </StatusTile>
                </div>
              </SettingsCard>
              <SettingsCard title={t('settings.visibleChannelsTitle')} description={t('settings.visibleChannelsDesc')} icon={<Eye className="size-4" />}>
                <VisibleChannelsPicker />
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-traffic" title={t('settings.nav.traffic')} description={t('settings.nav.trafficDesc')} icon={<Gauge className="size-4" />}>
                <SettingsCard title={t('settings.trafficProtection')} icon={<Gauge className="size-4" />} channels={ALL_UPSTREAM_CHANNELS}>
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField label={t('settings.maxConcurrency')} description={t('settings.maxConcurrencyRange')} suffix={t('settings.unit.concurrency')}>
                      <DraftNumberInput
                        min={1}
                        value={settingsForm.max_concurrency}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, max_concurrency: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.globalRpm')} description={t('settings.globalRpmRange')} suffix={t('settings.unit.rpm')}>
                      <DraftNumberInput
                        min={0}
                        value={settingsForm.global_rpm}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, global_rpm: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.maxRetries')} description={t('settings.maxRetriesRange')} suffix={t('settings.unit.times')}>
                      <DraftNumberInput
                        min={0}
                        max={10}
                        value={settingsForm.max_retries}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, max_retries: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.maxRateLimitRetries')} description={t('settings.maxRateLimitRetriesRange')} suffix={t('settings.unit.times')}>
                      <DraftNumberInput
                        min={0}
                        max={10}
                        value={settingsForm.max_rate_limit_retries}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, max_rate_limit_retries: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.retryIntervalMs')} description={t('settings.retryIntervalMsDesc')} suffix="ms">
                      <DraftNumberInput
                        min={0}
                        max={30000}
                        step={100}
                        value={settingsForm.retry_interval_ms}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, retry_interval_ms: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.transportRetryPolicy')} description={t('settings.transportRetryPolicyDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.transport_retry_policy || 'rotate'}
                        onChange={(value) => autoSaveStringField('transport_retry_policy', value)}
                        options={transportRetryPolicyOptions}
                      />
                    </SettingField>
                  </div>
                </SettingsCard>

                  <SettingsCard
                    title={t('settings.continuousRetryTitle')}
                    channels={ALL_UPSTREAM_CHANNELS}
                    description={t('settings.continuousRetryDesc')}
                    icon={<RefreshCw className="size-4" />}
                  >
                    <div className="space-y-4">
                      <SettingField
                        label={t('settings.continuousRetryEnabled')}
                        description={t('settings.continuousRetryEnabledDesc')}
                        layout="switch"
                      >
                        <Switch
                          aria-label={t('settings.continuousRetryEnabled')}
                          checked={settingsForm.continuous_retry_enabled}
                          onCheckedChange={(checked) => void autoSaveContinuousRetryPatch(buildContinuousRetryEnabledPatch(checked))}
                        />
                      </SettingField>
                      <SettingField
                        label={t('settings.continuousRetryCatchAll')}
                        description={t('settings.continuousRetryCatchAllDesc')}
                        warning={t('settings.continuousRetryCatchAllWarning')}
                        layout="switch"
                        className={cn(
                          'rounded-lg',
                          settingsForm.continuous_retry_catch_all && 'border-amber-500/50 bg-amber-500/10 hover:border-amber-500/60',
                        )}
                      >
                        <Switch
                          aria-label={t('settings.continuousRetryCatchAll')}
                          checked={settingsForm.continuous_retry_catch_all}
                          onCheckedChange={(checked) => void autoSaveContinuousRetryPatch(buildContinuousRetryCatchAllPatch(checked))}
                        />
                      </SettingField>
                      <SettingField
                        label={t('settings.continuousRetryMaxDuration')}
                        description={t('settings.continuousRetryMaxDurationDesc')}
                      >
                        <Input
                          aria-label={t('settings.continuousRetryMaxDuration')}
                          type="number"
                          min={1}
                          max={900}
                          step={1}
                          value={settingsForm.continuous_retry_max_duration_seconds}
                          disabled={!settingsForm.continuous_retry_enabled}
                          onChange={(event) => {
                            const value = Number(event.target.value)
                            setSettingsForm((current) => ({
                              ...current,
                              continuous_retry_max_duration_seconds: Number.isFinite(value) ? value : 600,
                            }))
                          }}
                          onBlur={(event) => {
                            const value = parseContinuousRetryMaxDurationSeconds(event.target.value)
                            setSettingsForm((current) => ({ ...current, continuous_retry_max_duration_seconds: value }))
                            void autoSaveContinuousRetryPatch({ continuous_retry_max_duration_seconds: value })
                          }}
                        />
                      </SettingField>
                      <div className={cn('grid gap-3 sm:grid-cols-2 lg:grid-cols-4', continuousRetryFineControlsDisabled && 'opacity-60')}>
                        {continuousRetryCategoryOptions.map((option) => (
                          <SettingField
                            key={option.value}
                            label={option.label}
                            layout="switch"
                            className="rounded-lg border border-border/60 px-3 py-2"
                            channels={option.channels}
                          >
                            <Switch
                              aria-label={option.label}
                              checked={(settingsForm.continuous_retry_categories ?? []).includes(option.value)}
                              disabled={continuousRetryFineControlsDisabled}
                              onCheckedChange={(checked) => {
                                const current = settingsFormRef.current.continuous_retry_categories ?? []
                                const next = checked
                                  ? Array.from(new Set([...current, option.value]))
                                  : current.filter((value) => value !== option.value)
                                void autoSaveContinuousRetryPatch({ continuous_retry_categories: next })
                              }}
                            />
                          </SettingField>
                        ))}
                      </div>
                      <div className="grid gap-4 lg:grid-cols-2">
                        <SettingField
                          label={t('settings.continuousRetryStatusCodes')}
                          description={t('settings.continuousRetryStatusCodesDesc')}
                        >
                          <Input
                            aria-label={t('settings.continuousRetryStatusCodes')}
                            value={continuousRetryStatusCodesDraft}
                            disabled={continuousRetryFineControlsDisabled}
                            placeholder="403,404,429,500,501,502,503,504"
                            onChange={(event) => setContinuousRetryStatusCodesDraft(event.target.value)}
                            onBlur={(event) => {
                              const values = parseContinuousRetryStatusCodes(event.target.value)
                              setContinuousRetryStatusCodesDraft(values.join(','))
                              void autoSaveContinuousRetryPatch({ continuous_retry_status_codes: values })
                            }}
                          />
                        </SettingField>
                        <SettingField
                          label={t('settings.continuousRetryErrorCodes')}
                          description={t('settings.continuousRetryErrorCodesDesc')}
                        >
                          <Input
                            aria-label={t('settings.continuousRetryErrorCodes')}
                            value={continuousRetryErrorCodesDraft}
                            disabled={continuousRetryFineControlsDisabled}
                            placeholder="rate_limited,context_length_exceeded"
                            onChange={(event) => setContinuousRetryErrorCodesDraft(event.target.value)}
                            onBlur={(event) => {
                              const values = parseContinuousRetryErrorCodes(event.target.value)
                              setContinuousRetryErrorCodesDraft(values.join(','))
                              void autoSaveContinuousRetryPatch({ continuous_retry_error_codes: values })
                            }}
                          />
                        </SettingField>
                      </div>
                      <p className="text-xs leading-relaxed text-amber-600 dark:text-amber-400">
                        {t('settings.continuousRetryWarning')}
                      </p>
                    </div>
                  </SettingsCard>

              <SettingsCard
                title={t('settings.modelCooldownTitle')}
                channels={ALL_UPSTREAM_CHANNELS}
                description={t('settings.modelCooldownDesc')}
                icon={<Timer className="size-4" />}
              >
                <div className="grid gap-4 lg:grid-cols-2">
                  <div className="space-y-3 rounded-xl border border-emerald-500/20 bg-emerald-500/5 p-3.5">
                    <div>
                      <div className="flex flex-wrap items-center gap-2">
                        <h3 className="text-sm font-semibold">{t('settings.relayModelCooldownTitle')}</h3>
                        <ChannelScopeBadges channels={CHANNELS_RELAY} size="xs" />
                      </div>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                        {t('settings.relayModelCooldownDesc')}
                      </p>
                    </div>
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.modelCooldownMode')} description={t('settings.modelCooldownModeDesc')}>
                        <SegmentedPillGroup
                          value={settingsForm.relay_model_cooldown_mode}
                          onChange={(value) => autoSaveStringField('relay_model_cooldown_mode', value)}
                          options={modelCooldownModeOptions}
                        />
                      </SettingField>
                      <SettingField
                        label={t('settings.modelCooldownSeconds')}
                        description={t('settings.modelCooldownSecondsDesc')}
                        suffix={t('settings.unit.sec')}
                        className={cn(settingsForm.relay_model_cooldown_mode === 'off' && 'opacity-60')}
                      >
                        <DraftNumberInput
                          min={1}
                          max={1800}
                          disabled={settingsForm.relay_model_cooldown_mode === 'off'}
                          value={settingsForm.relay_model_cooldown_seconds}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, relay_model_cooldown_seconds: value }))}
                          onValueCommit={(value) => void autoSaveSettingsPatch({ relay_model_cooldown_seconds: value })}
                        />
                      </SettingField>
                    </div>
                    <SettingField
                      label={t('settings.modelCooldownBackoff')}
                      description={t('settings.modelCooldownBackoffDesc')}
                      layout="switch"
                      className={cn(settingsForm.relay_model_cooldown_mode !== 'adaptive' && 'opacity-60')}
                    >
                      <Switch
                        checked={settingsForm.relay_model_cooldown_backoff_enabled}
                        disabled={settingsForm.relay_model_cooldown_mode !== 'adaptive'}
                        onCheckedChange={(checked) => autoSaveBooleanField('relay_model_cooldown_backoff_enabled', checked)}
                      />
                    </SettingField>
                  </div>

                  <div className="space-y-3 rounded-xl border border-amber-500/20 bg-amber-500/5 p-3.5">
                    <div>
                      <div className="flex flex-wrap items-center gap-2">
                        <h3 className="text-sm font-semibold">{t('settings.oauthModelCooldownTitle')}</h3>
                        <ChannelScopeBadges channels={ALL_UPSTREAM_CHANNELS} size="xs" />
                      </div>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                        {t('settings.oauthModelCooldownDesc')}
                      </p>
                    </div>
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.modelCooldownMode')} description={t('settings.modelCooldownModeDesc')}>
                        <SegmentedPillGroup
                          value={settingsForm.oauth_model_cooldown_mode}
                          onChange={(value) => autoSaveStringField('oauth_model_cooldown_mode', value)}
                          options={modelCooldownModeOptions}
                        />
                      </SettingField>
                      <SettingField
                        label={t('settings.modelCooldownSeconds')}
                        description={t('settings.modelCooldownSecondsDesc')}
                        suffix={t('settings.unit.sec')}
                        className={cn(settingsForm.oauth_model_cooldown_mode === 'off' && 'opacity-60')}
                      >
                        <DraftNumberInput
                          min={1}
                          max={1800}
                          disabled={settingsForm.oauth_model_cooldown_mode === 'off'}
                          value={settingsForm.oauth_model_cooldown_seconds}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, oauth_model_cooldown_seconds: value }))}
                          onValueCommit={(value) => void autoSaveSettingsPatch({ oauth_model_cooldown_seconds: value })}
                        />
                      </SettingField>
                    </div>
                    <SettingField
                      label={t('settings.modelCooldownBackoff')}
                      description={t('settings.modelCooldownBackoffDesc')}
                      layout="switch"
                      className={cn(settingsForm.oauth_model_cooldown_mode !== 'adaptive' && 'opacity-60')}
                    >
                      <Switch
                        checked={settingsForm.oauth_model_cooldown_backoff_enabled}
                        disabled={settingsForm.oauth_model_cooldown_mode !== 'adaptive'}
                        onCheckedChange={(checked) => autoSaveBooleanField('oauth_model_cooldown_backoff_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.schedulingStrategy')} icon={<Layers className="size-4" />} channels={ALL_UPSTREAM_CHANNELS}>
                <div className="grid auto-rows-min items-start gap-4 lg:grid-cols-2">
                  <div className="h-fit space-y-3 rounded-xl border border-border/60 bg-muted/10 p-3.5">
                    <div>
                      <h3 className="text-sm font-semibold">{t('settings.schedulingAccountGroup')}</h3>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                        {t('settings.schedulingAccountGroupDesc')}
                      </p>
                    </div>
                    <SettingsCollapsibleNote title={t('settings.schedulerEngineCompatibilityTitle')}>
                      {t('settings.schedulerEngineCompatibilityNote')}
                    </SettingsCollapsibleNote>
                    <SettingField label={t('settings.schedulerEngine')} description={t('settings.schedulerEngineDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.scheduler_engine}
                        onChange={(value) => autoSaveStringField('scheduler_engine', value)}
                        options={schedulerEngineOptions}
                      />
                    </SettingField>
                    <div className="grid gap-2" role="list" aria-label={t('settings.schedulerEngine')}>
                      {schedulerEngineExplanations.map((option) => {
                        const active = option.value === settingsForm.scheduler_engine
                        return (
                          <div
                            key={option.value}
                            role="listitem"
                            aria-current={active ? 'true' : undefined}
                            className={cn(
                              'rounded-lg border px-3 py-2.5 transition-colors',
                              active
                                ? 'border-primary/35 bg-primary/5'
                                : 'border-border/50 bg-background/45',
                            )}
                          >
                            <div className="flex items-start gap-2.5">
                              <span
                                className={cn(
                                  'mt-1.5 size-1.5 shrink-0 rounded-full',
                                  active ? 'bg-primary' : 'bg-muted-foreground/40',
                                )}
                                aria-hidden="true"
                              />
                              <div className="min-w-0">
                                <div className={cn('text-xs font-semibold', active ? 'text-primary' : 'text-foreground')}>
                                  {option.label}
                                </div>
                                <p className="mt-0.5 text-[11px] leading-relaxed text-muted-foreground sm:text-xs">
                                  {option.description}
                                </p>
                              </div>
                            </div>
                          </div>
                        )
                      })}
                    </div>
                    <SettingField
                      label={t('settings.schedulerMode')}
                      description={t('settings.schedulerModeDesc')}
                      warning={settingsForm.scheduler_engine !== 'legacy' ? undefined : t('settings.schedulerModeRequiresFast')}
                      className={cn(settingsForm.scheduler_engine === 'legacy' && 'opacity-60')}
                    >
                      <SegmentedPillGroup
                        value={settingsForm.scheduler_mode}
                        onChange={(value) => autoSaveStringField('scheduler_mode', value)}
                        options={schedulerModeOptions}
                      />
                    </SettingField>
                  </div>

                  <div className="h-fit space-y-3 rounded-xl border border-border/60 bg-muted/10 p-3.5">
                    <div>
                      <h3 className="text-sm font-semibold">{t('settings.schedulingAffinityGroup')}</h3>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                        {t('settings.schedulingAffinityGroupDesc')}
                      </p>
                    </div>
                    <SettingField label={t('settings.affinityMode')} description={t('settings.affinityModeDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.affinity_mode || 'bounded'}
                        onChange={(value) => autoSaveStringField('affinity_mode', value)}
                        options={affinityModeOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.sessionAffinitySpread')} description={t('settings.sessionAffinitySpreadDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.session_affinity_spread}
                        onCheckedChange={(checked) => autoSaveBooleanField('session_affinity_spread', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.sessionSlotBuffer')} description={t('settings.sessionSlotBufferDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.session_slot_buffer_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('session_slot_buffer_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.sessionSlotBufferSeconds')}
                      description={t('settings.sessionSlotBufferSecondsDesc')}
                      className={cn(!settingsForm.session_slot_buffer_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={60}
                        disabled={!settingsForm.session_slot_buffer_enabled}
                        value={settingsForm.session_slot_buffer_seconds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, session_slot_buffer_seconds: value }))}
                        onValueCommit={(value) => void autoSaveSettingsPatch({ session_slot_buffer_seconds: value })}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-runtime" title={t('settings.nav.runtime')} description={t('settings.nav.runtimeDesc')} icon={<Wrench className="size-4" />}>
              <SettingsCard title={t('settings.runtimeOptimization')} description={t('settings.runtimeOptimizationDesc')} icon={<Wrench className="size-4" />} channels={ALL_UPSTREAM_CHANNELS}>
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField label={t('settings.usageLogMode')} description={t('settings.usageLogModeDesc')}>
                      <Select
                        value={settingsForm.usage_log_mode}
                        onValueChange={(value) => autoSaveStringField('usage_log_mode', value)}
                        options={usageLogModeOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.usageLogBatchSize')} description={t('settings.usageLogBatchSizeDesc')}>
                      <DraftNumberInput
                        min={1}
                        max={1000}
                        value={settingsForm.usage_log_batch_size}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_log_batch_size: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.usageLogFlushInterval')} description={t('settings.usageLogFlushIntervalDesc')}>
                      <DraftNumberInput
                        min={1}
                        max={300}
                        value={settingsForm.usage_log_flush_interval_seconds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_log_flush_interval_seconds: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.billingTierPolicy')} description={t('settings.billingTierPolicyDesc')} channels={CHANNELS_CODEX_ONLY}>
                      <SegmentedPillGroup
                        value={settingsForm.billing_tier_policy}
                        onChange={(value) => autoSaveStringField('billing_tier_policy', value)}
                        options={billingTierPolicyOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.modelsListReadMaxBytes')} description={t('settings.modelsListReadMaxBytesDesc')} channels={CHANNELS_CODEX_ONLY}>
                      <div className="relative">
                        <DraftNumberInput
                          min={1}
                          max={256}
                          className="pr-12 tabular-nums"
                          value={bytesToMiB(settingsForm.models_list_read_max_bytes)}
                          onValueChange={(value) =>
                            setSettingsForm((form) => ({
                              ...form,
                              models_list_read_max_bytes: mibToBytes(value),
                            }))
                          }
                          onValueCommit={(value) =>
                            void autoSaveSettingsPatch({
                              models_list_read_max_bytes: mibToBytes(value),
                            })
                          }
                        />
                        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium text-muted-foreground">
                          MiB
                        </span>
                      </div>
                    </SettingField>
                    <SettingField label={t('settings.streamFlushPolicy')} description={t('settings.streamFlushPolicyDesc')} channels={CHANNELS_STREAMING}>
                      <SegmentedPillGroup
                        value={settingsForm.stream_flush_policy}
                        onChange={(value) => autoSaveStringField('stream_flush_policy', value)}
                        options={streamFlushPolicyOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.streamFlushInterval')} description={t('settings.streamFlushIntervalDesc')} channels={CHANNELS_STREAMING}>
                      <DraftNumberInput
                        min={1}
                        max={1000}
                        value={settingsForm.stream_flush_interval_ms}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, stream_flush_interval_ms: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.firstTokenMode')} description={t('settings.firstTokenModeDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.first_token_mode}
                        onChange={(value) => autoSaveStringField('first_token_mode', value)}
                        options={firstTokenModeOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.firstTokenTimeout')} description={t('settings.firstTokenTimeoutDesc')}>
                      <DraftNumberInput
                        min={0}
                        max={600}
                        value={settingsForm.first_token_timeout_seconds}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, first_token_timeout_seconds: value }))}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_SWITCH_ROW}>
                    <SettingField label={t('settings.firstTokenExcludesWsAcquire')} description={t('settings.firstTokenExcludesWsAcquireDesc')} layout="switch" channels={CHANNELS_CODEX_ONLY}>
                      <Switch
                        checked={settingsForm.first_token_excludes_ws_acquire}
                        onCheckedChange={(checked) => autoSaveBooleanField('first_token_excludes_ws_acquire', checked)}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.githubAccess')} description={t('settings.githubAccessDesc')} icon={<Globe className="size-4" />}>
                <div className={SETTINGS_FIELD_GRID}>
                  <SettingField label={t('settings.githubToken')} description={t('settings.githubTokenDesc')}>
                    <div className="flex items-center gap-2">
                      <Input
                        type="password"
                        autoComplete="off"
                        placeholder={settingsForm.github_token_configured ? t('settings.githubTokenConfiguredPlaceholder') : t('settings.githubTokenPlaceholder')}
                        value={githubTokenDraft}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setGithubTokenDraft(e.target.value)}
                        onBlur={() => {
                          const value = githubTokenDraft.trim()
                          if (!value) return
                          void autoSaveSettingsPatch({ github_token: value, github_token_configured: true })
                          setGithubTokenDraft('')
                        }}
                      />
                      {settingsForm.github_token_configured && (
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => {
                            void autoSaveSettingsPatch({ github_token: '', github_token_configured: false })
                          }}
                        >
                          {t('settings.githubTokenClear')}
                        </Button>
                      )}
                    </div>
                  </SettingField>
                  <SettingField label={t('settings.githubProxy')} description={t('settings.githubProxyDesc')}>
                    <Input
                      value={settingsForm.github_proxy_url}
                      placeholder="http://host:port / socks5://host:port"
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, github_proxy_url: e.target.value }))}
                      onBlur={(e: FocusEvent<HTMLInputElement>) => autoSaveStringField('github_proxy_url', e.target.value.trim())}
                    />
                  </SettingField>
                </div>
              </SettingsCard>

              <SettingsCard
                title={showConnectionPool ? t('settings.connectionPool') : t('settings.resinTitle')}
                description={showConnectionPool ? t('settings.nav.poolRestartHint') : t('settings.resinDesc')}
                icon={<Database className="size-4" />}
                badge={
                  showConnectionPool ? (
                    <Badge variant="outline" className="text-[11px]">
                      {t('settings.nav.restartRequired')}
                    </Badge>
                  ) : null
                }
              >
                <div className="space-y-4">
                  {showConnectionPool ? (
                    <div className={SETTINGS_FIELD_GRID}>
                      {isExternalDatabase ? (
                        <SettingField label={t('settings.pgMaxConns')} description={t('settings.pgMaxConnsRange')}>
                          <DraftNumberInput
                            min={5}
                            max={5000}
                            value={settingsForm.pg_max_conns}
                            onValueChange={(value) => setSettingsForm(f => ({ ...f, pg_max_conns: value }))}
                          />
                        </SettingField>
                      ) : null}
                      {isExternalCache ? (
                        <SettingField label={t('settings.redisPoolSize')} description={t('settings.redisPoolSizeRange')}>
                          <DraftNumberInput
                            min={5}
                            max={5000}
                            value={settingsForm.redis_pool_size}
                            onValueChange={(value) => setSettingsForm(f => ({ ...f, redis_pool_size: value }))}
                          />
                        </SettingField>
                      ) : null}
                    </div>
                  ) : null}
                  {showConnectionPool ? (
                    <div className="border-t border-border/80 pt-4">
                      <h4 className="text-[13px] font-semibold text-foreground sm:text-sm">{t('settings.resinTitle')}</h4>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('settings.resinDesc')}</p>
                    </div>
                  ) : null}
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField label={t('settings.resinUrl')} description={t('settings.resinUrlDesc')}>
                      <Input
                        placeholder="http://127.0.0.1:2260/your-token"
                        value={settingsForm.resin_url}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, resin_url: e.target.value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.resinPlatformName')} description={t('settings.resinPlatformNameDesc')}>
                      <Input
                        placeholder="codex2api"
                        value={settingsForm.resin_platform_name}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, resin_platform_name: e.target.value }))}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-storage" title={t('settings.nav.storage')} description={t('settings.nav.storageDesc')} icon={<ImageIcon className="size-4" />}>
              <SettingsCard title={t('settings.imageStorage')} description={t('settings.imageStorageDesc')} icon={<ImageIcon className="size-4" />} channels={CHANNELS_CODEX_ONLY}>
                <div className="space-y-4">
                  <SettingField label={t('settings.imageStorageBackend')} description={t('settings.imageStorageBackendDesc')}>
                    <SegmentedPillGroup
                      value={settingsForm.image_storage_backend}
                      onChange={(value) => setSettingsForm((f) => ({ ...f, image_storage_backend: value }))}
                      options={imageStorageBackendOptions}
                    />
                  </SettingField>

                  {settingsForm.image_storage_backend === 's3' ? (
                    <div className="rounded-xl border border-primary/20 bg-primary/5 p-4 space-y-4">
                      <div className="flex items-center gap-2 font-semibold text-foreground text-sm border-b border-primary/10 pb-2.5">
                        <Cloud className="size-4 text-primary" />
                        <span>对象存储凭证 (S3 Compatible Storage)</span>
                      </div>
                      <div className={SETTINGS_FIELD_GRID_3}>
                        <SettingField label={t('settings.imageS3Endpoint')} description={t('settings.imageS3EndpointDesc')}>
                          <Input
                            value={settingsForm.image_s3_endpoint}
                            placeholder="https://..."
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_endpoint: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3Region')} description={t('settings.imageS3RegionDesc')}>
                          <Input
                            value={settingsForm.image_s3_region}
                            placeholder="auto"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_region: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3Bucket')} description={t('settings.imageS3BucketDesc')}>
                          <Input
                            value={settingsForm.image_s3_bucket}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_bucket: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3AccessKey')} description={t('settings.imageS3AccessKeyDesc')}>
                          <Input
                            value={settingsForm.image_s3_access_key}
                            autoComplete="off"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_access_key: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3SecretKey')} description={t('settings.imageS3SecretKeyDesc')}>
                          <Input
                            type="password"
                            value={settingsForm.image_s3_secret_key}
                            autoComplete="new-password"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_secret_key: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3Prefix')} description={t('settings.imageS3PrefixDesc')}>
                          <Input
                            value={settingsForm.image_s3_prefix}
                            placeholder="codex/images"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_prefix: e.target.value }))}
                          />
                        </SettingField>
                      </div>
                      <SettingField label={t('settings.imageS3ForcePathStyle')} description={t('settings.imageS3ForcePathStyleDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.image_s3_force_path_style}
                          onCheckedChange={(checked) => autoSaveBooleanField('image_s3_force_path_style', checked)}
                        />
                      </SettingField>
                    </div>
                  ) : null}
                </div>
                {settingsForm.image_storage_backend === 's3' ? (
                  <div className="mt-4">
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => void handleTestImageStorage()}
                      disabled={testingImageStorage || !settingsForm.image_s3_bucket || !settingsForm.image_s3_access_key || !settingsForm.image_s3_secret_key}
                    >
                      {testingImageStorage ? t('settings.imageS3Testing') : t('settings.imageS3Test')}
                    </Button>
                  </div>
                ) : null}
              </SettingsCard>

              <SettingsCard title={t('settings.autoCleanup')} icon={<Trash2 className="size-4" />} tone="danger" channels={ALL_UPSTREAM_CHANNELS}>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
                  <SettingField label={t('settings.autoCleanUnauthorized')} description={t('settings.autoCleanUnauthorizedDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_unauthorized}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_unauthorized', checked)}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanRateLimited')} description={t('settings.autoCleanRateLimitedDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_rate_limited}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_rate_limited', checked)}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanFullUsage')} description={t('settings.autoCleanFullUsageDesc')} layout="switch">
                    <Switch
                      checked={lazyModeActive ? false : settingsForm.auto_clean_full_usage}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_full_usage', checked)}
                      disabled={lazyModeActive}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanError')} description={t('settings.autoCleanErrorDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_error}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_error', checked)}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanExpired')} description={t('settings.autoCleanExpiredDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_expired}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_expired', checked)}
                    />
                  </SettingField>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-security" title={t('settings.nav.security')} description={t('settings.nav.securityDesc')} icon={<Shield className="size-4" />}>
                <SettingsCard
                  title={t('settings.security')}
                  icon={<Shield className="size-4" />}
                  tone="danger"
                  badge={
                    <Badge variant="outline" className="border-destructive/30 text-[11px] text-destructive">
                      {t('settings.nav.sensitive')}
                    </Badge>
                  }
                >
                  <div className="space-y-4">
                    <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-destructive/20 bg-destructive/5 p-3.5 text-xs">
                      <div className="flex items-center gap-2 font-medium text-foreground">
                        <ShieldAlert className="size-4 text-destructive" />
                        <span>系统防护与审计 (Security Guard)</span>
                      </div>
                      <div className="flex items-center gap-2">
                        <Badge variant={settingsForm.prompt_filter_enabled ? 'default' : 'outline'} className="text-[11px] gap-1">
                          <span className={`size-1.5 rounded-full ${settingsForm.prompt_filter_enabled ? 'bg-emerald-400' : 'bg-amber-400'}`} />
                          Prompt 风控: {settingsForm.prompt_filter_enabled ? '已开启' : '未开启'}
                        </Badge>
                        <Badge variant="secondary" className="text-[11px] uppercase font-mono">
                          模式: {settingsForm.prompt_filter_mode}
                        </Badge>
                      </div>
                    </div>

                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField
                        label={t('settings.adminSecret')}
                        description={t('settings.adminSecretDesc')}
                        warning={settingsForm.admin_auth_source === 'env' ? t('settings.adminSecretEnvOverride') : undefined}
                      >
                        <Input
                          type="text"
                          placeholder={t('settings.adminSecretPlaceholder')}
                          value={settingsForm.admin_secret}
                          disabled={settingsForm.admin_auth_source === 'env'}
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => {
                            const nextSecret = e.target.value
                            return {
                              ...f,
                              admin_secret: nextSecret,
                              allow_remote_migration: nextSecret.trim() === '' ? false : f.allow_remote_migration,
                            }
                          })}
                        />
                      </SettingField>
                      <SettingField label={t('settings.promptFilterMode')} description={t('settings.promptFilterModeDesc')}>
                        <Select
                          value={settingsForm.prompt_filter_mode}
                          onValueChange={(value) => autoSaveStringField('prompt_filter_mode', value)}
                          options={[
                            { label: t('promptFilter.modeMonitor'), value: 'monitor' },
                            { label: t('promptFilter.modeWarn'), value: 'warn' },
                            { label: t('promptFilter.modeBlock'), value: 'block' },
                          ]}
                        />
                      </SettingField>
                    </div>
                    <div className={SETTINGS_SWITCH_GRID}>
                      <SettingField
                        label={t('settings.allowRemoteMigration')}
                        description={t('settings.allowRemoteMigrationDesc')}
                        warning={
                          !canConfigureRemoteMigration
                            ? t('settings.allowRemoteMigrationRequiresSecret')
                            : undefined
                        }
                        layout="switch"
                      >
                        <Switch
                          checked={settingsForm.allow_remote_migration}
                          disabled={!canConfigureRemoteMigration}
                          onCheckedChange={(checked) => autoSaveBooleanField('allow_remote_migration', checked)}
                        />
                      </SettingField>
                      <SettingField label={t('settings.promptFilterEnabled')} description={t('settings.promptFilterEnabledDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.prompt_filter_enabled}
                          onCheckedChange={(checked) => autoSaveBooleanField('prompt_filter_enabled', checked)}
                        />
                      </SettingField>
                    </div>
                  </div>
                </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-reference" title={t('settings.nav.reference')} description={t('settings.nav.referenceDesc')} icon={<Link2 className="size-4" />}>
                {/* 只读参考表常驻展开：折叠起来用户找不到端点列表。 */}
                <SettingsCard title={t('settings.apiEndpoints')} description={t('settings.nav.endpointsHint')} icon={<Link2 className="size-4" />}>
                    <div className="space-y-3">
                      <div className="flex flex-wrap items-center justify-between gap-2">
                        <p className="text-xs text-muted-foreground">
                          {t('settings.nav.endpointsReadonly')}
                        </p>
                        <Link
                          to="/docs#model-api"
                          className="inline-flex items-center gap-1 text-xs font-semibold text-primary hover:underline"
                        >
                          <ExternalLink className="size-3.5" />
                          {t('settings.nav.openDocs')}
                        </Link>
                      </div>
                      <div className="grid gap-2 sm:hidden">
                        {([
                          { method: 'POST', path: '/v1/chat/completions', desc: t('settings.openaiCompat'), tone: 'default' as const },
                          { method: 'POST', path: '/v1/responses', desc: t('settings.responsesApi'), tone: 'outline' as const },
                          { method: 'POST', path: '/v1/messages', desc: t('settings2.messagesEndpoint'), tone: 'outline' as const },
                          { method: 'POST', path: '/v1/images/generations', desc: t('settings.imageGenerationApi'), tone: 'outline' as const },
                          { method: 'POST', path: '/v1/images/edits', desc: t('settings.imageEditApi'), tone: 'outline' as const },
                          { method: 'GET', path: '/v1/models', desc: t('settings.modelList'), tone: 'secondary' as const },
                        ]).map((item) => (
                          <div
                            key={item.path}
                            className="rounded-xl border border-border bg-background/70 px-3 py-2.5"
                          >
                            <div className="flex items-center gap-2">
                              <Badge variant={item.tone} className="shrink-0 text-[11px]">
                                {item.method}
                              </Badge>
                              <code className="min-w-0 flex-1 truncate font-mono text-[12px] font-semibold text-foreground">
                                {item.path}
                              </code>
                            </div>
                            <p className="mt-1.5 text-[12px] leading-relaxed text-muted-foreground">
                              {item.desc}
                            </p>
                          </div>
                        ))}
                      </div>
                      <div className="data-table-shell hidden sm:block">
                        <Table>
                          <TableHeader>
                            <TableRow>
                              <TableHead className="text-[12px] font-semibold">{t('settings.method')}</TableHead>
                              <TableHead className="text-[12px] font-semibold">{t('settings.path')}</TableHead>
                              <TableHead className="text-[12px] font-semibold">{t('settings.endpointDesc')}</TableHead>
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            <TableRow>
                              <TableCell><Badge variant="default" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/chat/completions</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.openaiCompat')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/responses</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.responsesApi')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/messages</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings2.messagesEndpoint')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/images/generations</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.imageGenerationApi')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/images/edits</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.imageEditApi')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="secondary" className="text-[12px]">GET</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/models</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.modelList')}</TableCell>
                            </TableRow>
                          </TableBody>
                        </Table>
                      </div>
                    </div>
                </SettingsCard>
              </SettingsSection>
            </>
          ) : null}
          </div>
        </div>

        {/* 只有手动保存字段有改动时才出现的底部操作条；开关/下拉类已自动保存，不需要它。 */}
        {dirtyCount > 0 ? (
          <div
            role="status"
            className="sticky bottom-3 z-30 mt-2 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-amber-500/30 bg-card/95 px-4 py-2.5 shadow-lg backdrop-blur-md max-lg:bottom-[calc(5.5rem+env(safe-area-inset-bottom,0px))]"
          >
            <div className="flex min-w-0 items-center gap-2.5">
              <span className="size-2 shrink-0 rounded-full bg-amber-500" aria-hidden="true" />
              <div className="min-w-0">
                <div className="text-sm font-semibold text-foreground">{t('settings.saveStatusUnsaved', { n: dirtyCount })}</div>
                <p className="text-[11px] leading-relaxed text-muted-foreground sm:text-xs">{t('settings.saveStatusUnsavedHint')}</p>
              </div>
            </div>
            <div className="flex shrink-0 items-center gap-2 max-sm:w-full">
              <Button variant="ghost" size="sm" onClick={discardChanges} disabled={savingSettings} className="max-sm:flex-1">
                <RotateCcw className="size-3.5" />
                {t('settings.discardChanges')}
              </Button>
              {renderSaveButton('max-sm:flex-1')}
            </div>
          </div>
        ) : null}
      </>
    </StateShell>
  )
}
