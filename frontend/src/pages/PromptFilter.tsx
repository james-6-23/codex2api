import type { Dispatch, ReactNode, SetStateAction, TextareaHTMLAttributes } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { NavLink, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Activity, AlertTriangle, BookOpen, CheckCircle2, ChevronDown, ClipboardCheck, Copy, FileText, Gauge, GitBranch, HelpCircle, KeyRound, Layers, ListChecks, Network, Pencil, Plus, Power, PowerOff, RefreshCw, Save, Search, Shield, ShieldAlert, Sparkles, Trash2, Wand2, X } from 'lucide-react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import type { PromptFilterLog, PromptFilterMatch, PromptFilterRule, PromptFilterRulesResponse, PromptFilterVerdict, PromptGuardConfig, PromptGuardLayer, PromptGuardMode, PromptGuardProfile, PromptGuardProvider, PromptIntelligenceCandidate, PromptIntelligenceRun, SystemSettings } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { DraftNumberInput } from '@/components/ui/draft-number-input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

const PROMPT_FILTER_VIEWS = ['overview', 'logs', 'rules', 'intelligence', 'docs'] as const
const HIT_START_MARKER = '⟦PF_HIT⟧'
const HIT_END_MARKER = '⟦/PF_HIT⟧'
type PromptFilterView = typeof PROMPT_FILTER_VIEWS[number]

type PromptFilterForm = Pick<
  SystemSettings,
  | 'prompt_filter_enabled'
  | 'prompt_filter_mode'
  | 'prompt_filter_threshold'
  | 'prompt_filter_strict_threshold'
  | 'prompt_filter_strict_terminal_enabled'
  | 'prompt_filter_advanced_config'
  | 'prompt_filter_log_matches'
  | 'prompt_filter_max_text_length'
  | 'prompt_filter_sensitive_words'
  | 'prompt_filter_custom_patterns'
  | 'prompt_filter_disabled_patterns'
  | 'prompt_filter_review_enabled'
  | 'prompt_filter_review_api_key'
  | 'prompt_filter_review_api_key_configured'
  | 'prompt_filter_review_api_key_count'
  | 'prompt_filter_review_base_url'
  | 'prompt_filter_review_model'
  | 'prompt_filter_review_timeout_seconds'
  | 'prompt_filter_review_fail_closed'
>

type LogFilters = {
  action: string
  source: string
  endpoint: string
  model: string
  apiKeyId: string
  q: string
}

type RulePatternTestState = {
  text: string
  testing: boolean
  result: 'matched' | 'not_matched' | 'invalid' | null
  message: string
}

type CustomRuleDraft = {
  name: string
  pattern: string
  weight: string
  category: string
  strict: boolean
}

type AdvancedProtectionConfig = {
  guard: PromptGuardConfig
  enforcement: { terminal_categories: string[] }
  normalization: {
    enabled: boolean
    decode_url: boolean
    decode_html: boolean
    decode_base64: boolean
    decode_hex: boolean
    decode_rot13: boolean
    decode_escapes: boolean
    decode_compression: boolean
    max_decode_runs: number
    max_decoded_bytes: number
    max_encoded_blocks: number
  }
  context_discount: { enabled: boolean; intent_aware: boolean; max_discount: number; operational_max_discount: number }
  risk: { enabled: boolean; window_seconds: number; block_threshold: number; review_threshold: number; user_weight_percent: number; ip_weight_percent: number; session_weight_percent: number }
  sidecar: {
    enabled: boolean
    base_url: string
    timeout_seconds: number
    fail_closed: boolean
    min_score: number
    scan_clean_enabled: boolean
    sample_percent: number
    mode: 'shadow' | 'warn' | 'enforce'
    max_text_length: number
    cache_ttl_seconds: number
    max_concurrent: number
    circuit_breaker_failures: number
    circuit_breaker_seconds: number
  }
  session: {
    enabled: boolean
    window_seconds: number
    max_fragments: number
    max_text_length: number
    combine_short_fragments: boolean
    short_fragment_max_chars: number
    require_signed_identity: boolean
  }
  attachment: {
    enabled: boolean
    base_url: string
    timeout_seconds: number
    max_files: number
    max_bytes: number
    max_extracted_chars: number
    cache_ttl_seconds: number
    max_concurrent: number
    circuit_breaker_failures: number
    circuit_breaker_seconds: number
    allow_remote_urls: boolean
  }
  output: { enabled: boolean; buffer_bytes: number; overlap_bytes: number; strict_only: boolean }
  intelligence: { enabled: boolean; interval_hours: number; queries: string[]; max_search_results: number; model_enabled: boolean; model: string; max_model_calls: number; auto_add: boolean }
  newapi: { enabled: boolean; max_clock_skew_seconds: number; offense_window_seconds: number; ban_after: number }
}

const promptGuardModes: PromptGuardMode[] = ['inherit', 'off', 'shadow', 'warn', 'enforce']
const promptGuardProfiles: PromptGuardProfile[] = ['balanced', 'strict', 'research']
const promptGuardProviders: PromptGuardProvider[] = ['openai', 'anthropic', 'xai', 'unknown']
const promptGuardLayers: PromptGuardLayer[] = ['current_user', 'history', 'system', 'developer', 'instructions', 'tool_output', 'tool_arguments', 'attachment_refs', 'session_context', 'attachment_content']
const sidecarModes: AdvancedProtectionConfig['sidecar']['mode'][] = ['shadow', 'warn', 'enforce']
const inheritedPromptGuardProfile = '__default__'

const defaultPromptGuard: PromptGuardConfig = {
  mode: 'inherit',
  default_profile: 'balanced',
  allow_trusted_overrides: false,
  provider_profiles: {},
  layers: {
    current_user: { mode: 'enforce' },
    history: { mode: 'off' },
    system: { mode: 'off' },
    developer: { mode: 'off' },
    instructions: { mode: 'off' },
    tool_output: { mode: 'shadow' },
    tool_arguments: { mode: 'off' },
    attachment_refs: { mode: 'shadow' },
    session_context: { mode: 'shadow' },
    attachment_content: { mode: 'shadow' },
  },
}

const defaultAdvancedProtection: AdvancedProtectionConfig = {
  guard: defaultPromptGuard,
  enforcement: { terminal_categories: [] },
  normalization: {
    enabled: true,
    decode_url: true,
    decode_html: true,
    decode_base64: true,
    decode_hex: true,
    decode_rot13: true,
    decode_escapes: true,
    decode_compression: true,
    max_decode_runs: 2,
    max_decoded_bytes: 32768,
    max_encoded_blocks: 16,
  },
  context_discount: { enabled: true, intent_aware: true, max_discount: 90, operational_max_discount: 0 },
  risk: { enabled: false, window_seconds: 600, block_threshold: 100, review_threshold: 60, user_weight_percent: 60, ip_weight_percent: 20, session_weight_percent: 20 },
  sidecar: {
    enabled: false,
    base_url: '',
    timeout_seconds: 1,
    fail_closed: false,
    min_score: 30,
    scan_clean_enabled: false,
    sample_percent: 5,
    mode: 'shadow',
    max_text_length: 8192,
    cache_ttl_seconds: 60,
    max_concurrent: 16,
    circuit_breaker_failures: 3,
    circuit_breaker_seconds: 30,
  },
  session: {
    enabled: true,
    window_seconds: 300,
    max_fragments: 3,
    max_text_length: 4096,
    combine_short_fragments: false,
    short_fragment_max_chars: 24,
    require_signed_identity: true,
  },
  attachment: {
    enabled: false,
    base_url: '',
    timeout_seconds: 2,
    max_files: 4,
    max_bytes: 65536,
    max_extracted_chars: 8192,
    cache_ttl_seconds: 300,
    max_concurrent: 8,
    circuit_breaker_failures: 3,
    circuit_breaker_seconds: 30,
    allow_remote_urls: false,
  },
  output: { enabled: false, buffer_bytes: 4096, overlap_bytes: 512, strict_only: true },
  intelligence: { enabled: false, interval_hours: 24, queries: ['LLM jailbreak prompt injection', 'ChatGPT jailbreak prompt', 'Codex prompt injection jailbreak', '大模型 破限 提示词', 'GPT 破甲 提示词', 'AI 越狱 提示词', '中文 prompt injection 绕过'], max_search_results: 20, model_enabled: false, model: 'gpt-5.4', max_model_calls: 1, auto_add: false },
  newapi: { enabled: false, max_clock_skew_seconds: 120, offense_window_seconds: 86400, ban_after: 2 },
}

function parsePromptGuardMode(value: unknown, fallback: PromptGuardMode = 'inherit'): PromptGuardMode {
  return typeof value === 'string' && promptGuardModes.includes(value as PromptGuardMode) ? value as PromptGuardMode : fallback
}

function parsePromptGuardProfile(value: unknown, fallback: PromptGuardProfile = 'balanced'): PromptGuardProfile {
  return typeof value === 'string' && promptGuardProfiles.includes(value as PromptGuardProfile) ? value as PromptGuardProfile : fallback
}

function parsePromptGuard(value: unknown): PromptGuardConfig {
  const raw = value && typeof value === 'object' ? value as Record<string, unknown> : {}
  const rawProviders = raw.provider_profiles && typeof raw.provider_profiles === 'object'
    ? raw.provider_profiles as Record<string, unknown>
    : {}
  const rawLayers = raw.layers && typeof raw.layers === 'object'
    ? raw.layers as Record<string, unknown>
    : {}

  const providerProfiles: PromptGuardConfig['provider_profiles'] = {}
  for (const provider of promptGuardProviders) {
    const profile = rawProviders[provider]
    if (typeof profile === 'string' && promptGuardProfiles.includes(profile as PromptGuardProfile)) {
      providerProfiles[provider] = profile as PromptGuardProfile
    }
  }

  const layers = { ...defaultPromptGuard.layers }
  for (const layer of promptGuardLayers) {
    const rawLayer = rawLayers[layer] && typeof rawLayers[layer] === 'object'
      ? rawLayers[layer] as Record<string, unknown>
      : {}
    layers[layer] = { mode: parsePromptGuardMode(rawLayer.mode) }
  }

  return {
    mode: parsePromptGuardMode(raw.mode),
    default_profile: parsePromptGuardProfile(raw.default_profile),
    allow_trusted_overrides: raw.allow_trusted_overrides === true,
    provider_profiles: providerProfiles,
    layers,
  }
}

function parseAdvancedProtection(raw: string): AdvancedProtectionConfig {
  try {
    const value = JSON.parse(raw || '{}')
    const enforcement = { ...defaultAdvancedProtection.enforcement, ...(value.enforcement || {}) }
    const intelligence = { ...defaultAdvancedProtection.intelligence, ...(value.intelligence || {}) }
    const sidecar = { ...defaultAdvancedProtection.sidecar, ...(value.sidecar || {}) }
    return {
      guard: parsePromptGuard(value.guard),
      enforcement: {
        ...enforcement,
        terminal_categories: Array.isArray(enforcement.terminal_categories)
          ? enforcement.terminal_categories.filter((category: unknown): category is string => typeof category === 'string')
          : [],
      },
      normalization: { ...defaultAdvancedProtection.normalization, ...(value.normalization || {}) },
      context_discount: { ...defaultAdvancedProtection.context_discount, ...(value.context_discount || {}) },
      risk: { ...defaultAdvancedProtection.risk, ...(value.risk || {}) },
      sidecar: {
        ...sidecar,
        mode: sidecarModes.includes(sidecar.mode) ? sidecar.mode : defaultAdvancedProtection.sidecar.mode,
      },
      session: { ...defaultAdvancedProtection.session, ...(value.session || {}) },
      attachment: { ...defaultAdvancedProtection.attachment, ...(value.attachment || {}) },
      output: { ...defaultAdvancedProtection.output, ...(value.output || {}) },
      intelligence: {
        ...intelligence,
        queries: Array.isArray(intelligence.queries)
          ? intelligence.queries.filter((query: unknown): query is string => typeof query === 'string')
          : [...defaultAdvancedProtection.intelligence.queries],
      },
      newapi: { ...defaultAdvancedProtection.newapi, ...(value.newapi || {}) },
    }
  } catch { return defaultAdvancedProtection }
}

const defaultForm: PromptFilterForm = {
  prompt_filter_enabled: false,
  prompt_filter_mode: 'block',
  prompt_filter_threshold: 50,
  prompt_filter_strict_threshold: 90,
  prompt_filter_strict_terminal_enabled: true,
  prompt_filter_advanced_config: '{}',
  prompt_filter_log_matches: true,
  prompt_filter_max_text_length: 81920,
  prompt_filter_sensitive_words: '',
  prompt_filter_custom_patterns: '[]',
  prompt_filter_disabled_patterns: '[]',
  prompt_filter_review_enabled: false,
  prompt_filter_review_api_key: '',
  prompt_filter_review_api_key_configured: false,
  prompt_filter_review_api_key_count: 0,
  prompt_filter_review_base_url: 'https://api.openai.com',
  prompt_filter_review_model: 'omni-moderation-latest',
  prompt_filter_review_timeout_seconds: 10,
  prompt_filter_review_fail_closed: true,
}

const emptyFilters: LogFilters = {
  action: '',
  source: '',
  endpoint: '',
  model: '',
  apiKeyId: '',
  q: '',
}

const defaultCustomRuleDraft: CustomRuleDraft = {
  name: '',
  pattern: '',
  weight: '50',
  category: 'custom',
  strict: false,
}

const defaultRulePatternTestState: RulePatternTestState = {
  text: '',
  testing: false,
  result: null,
  message: '',
}

function parseRuleWeight(raw: string): number | null {
  const trimmed = raw.trim()
  if (!/^\d+$/.test(trimmed)) return null
  const weight = Number(trimmed)
  if (!Number.isSafeInteger(weight) || weight <= 0 || weight > 1000) return null
  return weight
}

function customRuleDraftFromRule(rule: PromptFilterRule): CustomRuleDraft {
  return {
    name: rule.name || '',
    pattern: rule.pattern || '',
    weight: String(rule.weight || 50),
    category: rule.category || 'custom',
    strict: Boolean(rule.strict),
  }
}

const normalizePromptFilterForm = (settings?: SystemSettings | null): PromptFilterForm => ({
  prompt_filter_enabled: Boolean(settings?.prompt_filter_enabled),
  prompt_filter_mode: settings?.prompt_filter_mode || 'block',
  prompt_filter_threshold: settings?.prompt_filter_threshold || 50,
  prompt_filter_strict_threshold: settings?.prompt_filter_strict_threshold || 90,
  prompt_filter_strict_terminal_enabled: settings?.prompt_filter_strict_terminal_enabled ?? true,
  prompt_filter_advanced_config: settings?.prompt_filter_advanced_config || '{}',
  prompt_filter_log_matches: settings?.prompt_filter_log_matches ?? true,
  prompt_filter_max_text_length: settings?.prompt_filter_max_text_length || 81920,
  prompt_filter_sensitive_words: settings?.prompt_filter_sensitive_words || '',
  prompt_filter_custom_patterns: settings?.prompt_filter_custom_patterns || '[]',
  prompt_filter_disabled_patterns: settings?.prompt_filter_disabled_patterns || '[]',
  prompt_filter_review_enabled: Boolean(settings?.prompt_filter_review_enabled),
  prompt_filter_review_api_key: '',
  prompt_filter_review_api_key_configured: Boolean(settings?.prompt_filter_review_api_key_configured),
  prompt_filter_review_api_key_count: settings?.prompt_filter_review_api_key_count || 0,
  prompt_filter_review_base_url: settings?.prompt_filter_review_base_url || 'https://api.openai.com',
  prompt_filter_review_model: settings?.prompt_filter_review_model || 'omni-moderation-latest',
  prompt_filter_review_timeout_seconds: settings?.prompt_filter_review_timeout_seconds || 10,
  prompt_filter_review_fail_closed: settings?.prompt_filter_review_fail_closed ?? true,
})

function normalizePromptFilterView(value?: string): PromptFilterView {
  return PROMPT_FILTER_VIEWS.includes(value as PromptFilterView) ? value as PromptFilterView : 'overview'
}

function parseJSONList<T>(raw: string, fallback: T[] = []): T[] {
  try {
    const parsed = JSON.parse(raw || '[]')
    return Array.isArray(parsed) ? parsed as T[] : fallback
  } catch {
    return fallback
  }
}

function promptFilterSavePayload(form: PromptFilterForm): Partial<SystemSettings> {
  const payload: Partial<SystemSettings> = { ...form }
  // 展示用字段，不参与写入。
  delete payload.prompt_filter_review_api_key_configured
  delete payload.prompt_filter_review_api_key_count
  if (!payload.prompt_filter_review_api_key?.trim()) {
    delete payload.prompt_filter_review_api_key
  }
  return payload
}

export default function PromptFilter() {
  const { t } = useTranslation()
  const { view } = useParams()
  const activeView = normalizePromptFilterView(view)
  const { toast, showToast } = useToast()
  const [form, setForm] = useState<PromptFilterForm>(defaultForm)
  const [saving, setSaving] = useState(false)
  const [clearing, setClearing] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testText, setTestText] = useState('')
  const [testEndpoint, setTestEndpoint] = useState('/v1/responses')
  const [testModel, setTestModel] = useState('gpt-5.5')
  const [testVerdict, setTestVerdict] = useState<PromptFilterVerdict | null>(null)

  const loadData = useCallback(async () => {
    const [settings, logsResp, rules] = await Promise.all([
      api.getSettings(),
      api.getPromptFilterLogs({ limit: 5 }),
      api.getPromptFilterRules(),
    ])
    return {
      settings,
      recentLogs: logsResp.logs ?? [],
      totalLogs: logsResp.total ?? logsResp.logs?.length ?? 0,
      rules,
    }
  }, [])

  const { data, loading, error, reload, setData } = useDataLoader<{
    settings: SystemSettings | null
    recentLogs: PromptFilterLog[]
    totalLogs: number
    rules: PromptFilterRulesResponse | null
  }>({
    initialData: {
      settings: null,
      recentLogs: [],
      totalLogs: 0,
      rules: null,
    },
    load: loadData,
  })

  useEffect(() => {
    if (data.settings) {
      setForm(normalizePromptFilterForm(data.settings))
    }
  }, [data.settings])

  const modeOptions = [
    { label: t('promptFilter.modeMonitor'), value: 'monitor' },
    { label: t('promptFilter.modeWarn'), value: 'warn' },
    { label: t('promptFilter.modeBlock'), value: 'block' },
  ]
  const booleanOptions = [
    { label: t('common.enabled'), value: 'true' },
    { label: t('common.disabled'), value: 'false' },
  ]
  const endpointOptions = [
    { label: '/v1/responses', value: '/v1/responses' },
    { label: '/v1/chat/completions', value: '/v1/chat/completions' },
    { label: '/v1/messages', value: '/v1/messages' },
    { label: '/v1/images/generations', value: '/v1/images/generations' },
  ]

  const saveSettings = async (partial?: Partial<SystemSettings>) => {
    setSaving(true)
    try {
      const payload = partial ?? promptFilterSavePayload(form)
      const updated = await api.updateSettings(payload)
      setForm(normalizePromptFilterForm(updated))
      const rules = await api.getPromptFilterRules()
      const logsResp = await api.getPromptFilterLogs({ limit: 5 })
      setData((current) => ({
        ...current,
        settings: updated,
        rules,
        recentLogs: logsResp.logs ?? [],
        totalLogs: logsResp.total ?? current.totalLogs,
      }))
      showToast(t('promptFilter.saveSuccess'))
    } catch (err) {
      showToast(`${t('promptFilter.saveFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setSaving(false)
    }
  }

  const runTest = async () => {
    const text = testText.trim()
    if (!text) {
      showToast(t('promptFilter.testEmpty'), 'error')
      return
    }
    setTesting(true)
    try {
      const result = await api.testPromptFilter({
        text,
        endpoint: testEndpoint,
        model: testModel,
      })
      setTestVerdict(result.verdict)
      showToast(t('promptFilter.testDone'))
    } catch (err) {
      showToast(`${t('promptFilter.testFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setTesting(false)
    }
  }

  const clearLogs = async () => {
    setClearing(true)
    try {
      await api.clearPromptFilterLogs()
      setData((current) => ({ ...current, recentLogs: [], totalLogs: 0 }))
      showToast(t('promptFilter.logsCleared'))
    } catch (err) {
      showToast(`${t('promptFilter.clearFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setClearing(false)
    }
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('promptFilter.loadingTitle')}
      loadingDescription={t('promptFilter.loadingDesc')}
      errorTitle={t('promptFilter.errorTitle')}
    >
      <>
        <PageHeader
          title={t('promptFilter.title')}
          description={t('promptFilter.description')}
          actions={
            activeView === 'overview' ? (
              <>
                <Button variant="outline" onClick={() => void reload()}>
                  <RefreshCw className="size-3.5" />
                  {t('common.refresh')}
                </Button>
                <Button onClick={() => void saveSettings()} disabled={saving}>
                  <Save className="size-4" />
                  {saving ? t('common.saving') : t('common.save')}
                </Button>
              </>
            ) : (
              <Button variant="outline" onClick={() => void reload()}>
                <RefreshCw className="size-3.5" />
                {t('common.refresh')}
              </Button>
            )
          }
        />

        <PromptFilterTabs activeView={activeView} />

        {activeView === 'overview' ? (
          <OverviewView
            form={form}
            setForm={setForm}
            saving={saving}
            modeOptions={modeOptions}
            booleanOptions={booleanOptions}
            endpointOptions={endpointOptions}
            recentLogs={data.recentLogs}
            totalLogs={data.totalLogs}
            testText={testText}
            setTestText={setTestText}
            testEndpoint={testEndpoint}
            setTestEndpoint={setTestEndpoint}
            testModel={testModel}
            setTestModel={setTestModel}
            testing={testing}
            testVerdict={testVerdict}
            runTest={runTest}
            clearLogs={clearLogs}
            clearing={clearing}
            onSave={() => void saveSettings()}
          />
        ) : null}

        {activeView === 'logs' ? (
          <LogsView clearLogs={clearLogs} clearing={clearing} />
        ) : null}

        {activeView === 'rules' ? (
          <RulesView
            form={form}
            rules={data.rules}
            saving={saving}
            onRulesUpdated={(rules, settings) => {
              if (settings) setForm(normalizePromptFilterForm(settings))
              setData((current) => ({ ...current, rules, settings: settings ?? current.settings }))
            }}
          />
        ) : null}

        {activeView === 'intelligence' ? <IntelligenceView /> : null}

        {activeView === 'docs' ? <DocsView /> : null}

      </>
    </StateShell>
  )
}

function PromptFilterTabs({ activeView }: { activeView: PromptFilterView }) {
  const { t } = useTranslation()
  const tabs = [
    { view: 'overview' as const, label: t('promptFilter.views.overview'), to: '/prompt-filter/overview' },
    { view: 'logs' as const, label: t('promptFilter.views.logs'), to: '/prompt-filter/logs' },
    { view: 'rules' as const, label: t('promptFilter.views.rules'), to: '/prompt-filter/rules' },
    { view: 'intelligence' as const, label: t('promptFilter.views.intelligence'), to: '/prompt-filter/intelligence' },
    { view: 'docs' as const, label: t('promptFilter.views.docs'), to: '/prompt-filter/docs' },
  ]
  const tabCount = tabs.length
  const activeIndex = Math.max(0, tabs.findIndex((tab) => tab.view === activeView))
  return (
    <div className="mb-5 flex justify-center">
      <div
        className="relative grid w-full max-w-[900px] rounded-2xl border border-border bg-background/80 p-1 shadow-sm backdrop-blur-lg"
        style={{ gridTemplateColumns: `repeat(${tabCount}, minmax(0, 1fr))` }}
        role="tablist"
      >
        <div
          className="pointer-events-none absolute left-1 top-1 h-[calc(100%-0.5rem)] rounded-xl border border-primary/15 bg-primary/8 transition-transform duration-300 ease-out"
          style={{ width: `calc((100% - 0.5rem) / ${tabCount})`, transform: `translateX(${activeIndex * 100}%)` }}
        />
        {tabs.map((tab) => (
          <NavLink
            key={tab.view}
            to={tab.to}
            role="tab"
            aria-selected={activeView === tab.view}
            className={`relative z-10 flex h-9 items-center justify-center rounded-xl px-2 text-sm font-semibold transition-colors sm:px-3 ${
              activeView === tab.view ? 'text-primary' : 'text-muted-foreground hover:text-foreground'
            }`}
          >
            {tab.label}
          </NavLink>
        ))}
      </div>
    </div>
  )
}

function AdvancedProtectionEditor({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const { t } = useTranslation()
  const [generatedNewAPISecret, setGeneratedNewAPISecret] = useState('')
  const [secretCopied, setSecretCopied] = useState(false)
  const [secretStatus, setSecretStatus] = useState<{ configured: boolean; source: string; masked: string }>({ configured: false, source: 'none', masked: '' })
  const [secretSaving, setSecretSaving] = useState(false)
  const [secretError, setSecretError] = useState('')
  const [secretRevealOpen, setSecretRevealOpen] = useState(false)
  const [secretCloseConfirmOpen, setSecretCloseConfirmOpen] = useState(false)
  const config = useMemo(() => parseAdvancedProtection(value), [value])
  const update = <K extends keyof AdvancedProtectionConfig>(section: K, patch: Partial<AdvancedProtectionConfig[K]>) => {
    onChange(JSON.stringify({ ...config, [section]: { ...config[section], ...patch } }))
  }
  const setBool = <K extends keyof AdvancedProtectionConfig>(section: K, key: string, next: boolean) => {
    update(section, { [key]: next } as never)
  }
  useEffect(() => { void api.getPromptFilterNewAPISecret().then(setSecretStatus).catch(() => undefined) }, [])
  const generateNewAPISecret = async () => {
    if (secretStatus.source === 'environment') return
    setSecretSaving(true); setSecretError('')
    try {
      const result = await api.generatePromptFilterNewAPISecret()
      setGeneratedNewAPISecret(result.secret); setSecretStatus(result); setSecretCopied(false); setSecretRevealOpen(true)
    } catch (error) { setSecretError(getErrorMessage(error)) } finally { setSecretSaving(false) }
  }
  const copyNewAPISecret = async () => {
    if (!generatedNewAPISecret) return
    await navigator.clipboard.writeText(generatedNewAPISecret)
    setSecretCopied(true)
  }
  const requestCloseSecretReveal = () => {
    if (!generatedNewAPISecret) { setSecretRevealOpen(false); return }
    setSecretCloseConfirmOpen(true)
  }
  const confirmCloseSecretReveal = () => {
    setSecretCloseConfirmOpen(false)
    setSecretRevealOpen(false)
    setGeneratedNewAPISecret('')
    setSecretCopied(false)
  }
  const terminalCategoriesText = config.enforcement.terminal_categories.join(', ')
  const queryCount = config.intelligence.queries.length
  const guardModeOptions = promptGuardModes.map((mode) => ({
    value: mode,
    label: t(`promptFilter.guard.modes.${mode}.label`),
  }))
  const guardProfileOptions = promptGuardProfiles.map((profile) => ({
    value: profile,
    label: t(`promptFilter.guard.profiles.${profile}.label`),
  }))
  const guardProviderProfileOptions = [
    { value: inheritedPromptGuardProfile, label: t('promptFilter.guard.inheritDefaultProfile') },
    ...guardProfileOptions,
  ]
  const sidecarModeOptions = sidecarModes.map((mode) => ({
    value: mode,
    label: t(`promptFilter.extensions.sidecar.modes.${mode}`),
  }))
  const updateGuard = (patch: Partial<PromptGuardConfig>) => update('guard', patch)
  const updateGuardProvider = (provider: PromptGuardProvider, profile: PromptGuardProfile | null) => {
    const providerProfiles = { ...config.guard.provider_profiles }
    if (profile) providerProfiles[provider] = profile
    else delete providerProfiles[provider]
    updateGuard({
      provider_profiles: providerProfiles,
    })
  }
  const updateGuardLayer = (layer: PromptGuardLayer, mode: PromptGuardMode) => {
    updateGuard({
      layers: { ...config.guard.layers, [layer]: { mode } },
    })
  }

  return (
    <div className="space-y-3">
      <SectionTitle title={t('promptFilter.advancedVisualTitle')} />

      <AdvancedPanel title={t('promptFilter.guard.title')} hint={t('promptFilter.guard.description')}>
        <div className="space-y-4">
          <div className="grid gap-3 lg:grid-cols-2 xl:grid-cols-4">
            <div className="rounded-lg border border-foreground/10 bg-muted/15 p-3 dark:border-foreground/15">
              <CompactField label={t('promptFilter.guard.globalMode')} hint={t('promptFilter.guard.globalModeHint')}>
                <Select
                  value={config.guard.mode}
                  onValueChange={(next) => updateGuard({ mode: next as PromptGuardMode })}
                  options={guardModeOptions}
                />
              </CompactField>
            </div>
            <div className="rounded-lg border border-foreground/10 bg-muted/15 p-3 dark:border-foreground/15">
              <CompactField label={t('promptFilter.guard.defaultProfile')} hint={t('promptFilter.guard.defaultProfileHint')}>
                <Select
                  value={config.guard.default_profile}
                  onValueChange={(next) => updateGuard({ default_profile: next as PromptGuardProfile })}
                  options={guardProfileOptions}
                />
              </CompactField>
            </div>
            <div className="rounded-lg border border-foreground/10 bg-muted/15 p-3 dark:border-foreground/15">
              <SwitchField
                label={t('promptFilter.guard.trustedOverrides')}
                hint={t('promptFilter.guard.trustedOverridesHint')}
                checked={config.guard.allow_trusted_overrides}
                onCheckedChange={(next) => updateGuard({ allow_trusted_overrides: next })}
              />
            </div>
            <div className="flex items-start gap-3 rounded-lg border border-sky-500/20 bg-sky-500/[0.06] p-3 text-sm dark:border-sky-400/20 dark:bg-sky-400/[0.07]">
              <Shield className="mt-0.5 size-4 shrink-0 text-sky-600 dark:text-sky-300" />
              <div className="min-w-0">
                <div className="font-medium text-foreground">{t('promptFilter.guard.compatibilityTitle')}</div>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.guard.compatibilityHint')}</p>
              </div>
            </div>
          </div>

          <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-5">
            {promptGuardModes.map((mode) => (
              <div
                key={mode}
                className={cn(
                  'rounded-lg border px-3 py-2.5 transition-colors',
                  config.guard.mode === mode
                    ? 'border-primary/35 bg-primary/[0.07]'
                    : 'border-foreground/10 bg-background dark:border-foreground/15',
                )}
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="text-xs font-semibold">{t(`promptFilter.guard.modes.${mode}.label`)}</span>
                  {config.guard.mode === mode ? <Badge className="h-5 px-1.5 text-[10px]">{t('promptFilter.guard.active')}</Badge> : null}
                </div>
                <p className="mt-1 text-[11px] leading-[1.45] text-muted-foreground">{t(`promptFilter.guard.modes.${mode}.description`)}</p>
              </div>
            ))}
          </div>

          <div className="grid gap-3 xl:grid-cols-2">
            <div className="rounded-lg border border-foreground/10 p-3 dark:border-foreground/15">
              <div className="mb-3 flex items-start gap-2.5">
                <Network className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                <div>
                  <h4 className="text-sm font-semibold">{t('promptFilter.guard.providerTitle')}</h4>
                  <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{t('promptFilter.guard.providerDescription')}</p>
                </div>
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                {promptGuardProviders.map((provider) => (
                  <CompactField
                    key={provider}
                    label={t(`promptFilter.guard.providers.${provider}.label`)}
                    hint={t(`promptFilter.guard.providers.${provider}.description`)}
                  >
                    <Select
                      value={config.guard.provider_profiles[provider] ?? inheritedPromptGuardProfile}
                      onValueChange={(next) => updateGuardProvider(
                        provider,
                        next === inheritedPromptGuardProfile ? null : next as PromptGuardProfile,
                      )}
                      options={guardProviderProfileOptions}
                    />
                  </CompactField>
                ))}
              </div>
            </div>

            <div className="rounded-lg border border-foreground/10 p-3 dark:border-foreground/15">
              <div className="mb-3 flex items-start gap-2.5">
                <Gauge className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                <div>
                  <h4 className="text-sm font-semibold">{t('promptFilter.guard.profileTitle')}</h4>
                  <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{t('promptFilter.guard.profileDescription')}</p>
                </div>
              </div>
              <div className="space-y-2">
                {promptGuardProfiles.map((profile) => (
                  <div
                    key={profile}
                    className={cn(
                      'rounded-md border px-3 py-2',
                      config.guard.default_profile === profile
                        ? 'border-primary/30 bg-primary/[0.06]'
                        : 'border-foreground/10 bg-muted/10 dark:border-foreground/15',
                    )}
                  >
                    <div className="text-xs font-semibold">{t(`promptFilter.guard.profiles.${profile}.label`)}</div>
                    <p className="mt-0.5 text-[11px] leading-[1.45] text-muted-foreground">{t(`promptFilter.guard.profiles.${profile}.description`)}</p>
                  </div>
                ))}
              </div>
            </div>
          </div>

          <div className="rounded-lg border border-foreground/10 p-3 dark:border-foreground/15">
            <div className="mb-3 flex items-start gap-2.5">
              <Layers className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
              <div>
                <h4 className="text-sm font-semibold">{t('promptFilter.guard.layersTitle')}</h4>
                <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{t('promptFilter.guard.layersDescription')}</p>
              </div>
            </div>
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              {promptGuardLayers.map((layer) => (
                <CompactField
                  key={layer}
                  label={t(`promptFilter.guard.layers.${layer}.label`)}
                  hint={t(`promptFilter.guard.layers.${layer}.description`)}
                >
                  <Select
                    value={config.guard.layers[layer].mode}
                    onValueChange={(next) => updateGuardLayer(layer, next as PromptGuardMode)}
                    options={guardModeOptions}
                  />
                </CompactField>
              ))}
            </div>
          </div>
        </div>
      </AdvancedPanel>

      {/* Core defense: bounded decoding and intent-aware scoring keep the default preset useful without widening penalties. */}
      <div className="grid gap-3 lg:grid-cols-2">
        <div className="lg:col-span-2">
          <AdvancedPanel title={t('promptFilter.normalizationTitle')} hint={t('promptFilter.help.normalizationPanel')}>
            <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-3 xl:grid-cols-6">
              <SwitchField
                label={t('promptFilter.enabled')}
                hint={t('promptFilter.help.normalizationEnabled')}
                checked={config.normalization.enabled}
                onCheckedChange={(next) => setBool('normalization', 'enabled', next)}
              />
              <CompactField label={t('promptFilter.decodeRuns')} hint={t('promptFilter.help.decodeRuns')}>
                <DraftNumberInput min={1} max={2} value={config.normalization.max_decode_runs} onValueChange={(v) => update('normalization', { max_decode_runs: v })} />
              </CompactField>
              <CompactField label={t('promptFilter.maxDecodedBytes')} hint={t('promptFilter.help.maxDecodedBytes')}>
                <DraftNumberInput min={1024} max={65536} value={config.normalization.max_decoded_bytes} onValueChange={(v) => update('normalization', { max_decoded_bytes: v })} />
              </CompactField>
              <CompactField label={t('promptFilter.maxEncodedBlocks')} hint={t('promptFilter.help.maxEncodedBlocks')}>
                <DraftNumberInput min={1} max={32} value={config.normalization.max_encoded_blocks} onValueChange={(v) => update('normalization', { max_encoded_blocks: v })} />
              </CompactField>
              <SwitchField
                label={t('promptFilter.decoders.url')}
                hint={t('promptFilter.help.decodeUrl')}
                checked={config.normalization.decode_url}
                onCheckedChange={(next) => setBool('normalization', 'decode_url', next)}
              />
              <SwitchField
                label={t('promptFilter.decoders.html')}
                hint={t('promptFilter.help.decodeHtml')}
                checked={config.normalization.decode_html}
                onCheckedChange={(next) => setBool('normalization', 'decode_html', next)}
              />
              <SwitchField
                label={t('promptFilter.decoders.base64')}
                hint={t('promptFilter.help.decodeBase64')}
                checked={config.normalization.decode_base64}
                onCheckedChange={(next) => setBool('normalization', 'decode_base64', next)}
              />
              <SwitchField
                label={t('promptFilter.decoders.hex')}
                hint={t('promptFilter.help.decodeHex')}
                checked={config.normalization.decode_hex}
                onCheckedChange={(next) => setBool('normalization', 'decode_hex', next)}
              />
              <SwitchField
                label={t('promptFilter.decoders.rot13')}
                hint={t('promptFilter.help.decodeRot13')}
                checked={config.normalization.decode_rot13}
                onCheckedChange={(next) => setBool('normalization', 'decode_rot13', next)}
              />
              <SwitchField
                label={t('promptFilter.decoders.escapes')}
                hint={t('promptFilter.help.decodeEscapes')}
                checked={config.normalization.decode_escapes}
                onCheckedChange={(next) => setBool('normalization', 'decode_escapes', next)}
              />
              <SwitchField
                label={t('promptFilter.decoders.compression')}
                hint={t('promptFilter.help.decodeCompression')}
                checked={config.normalization.decode_compression}
                onCheckedChange={(next) => setBool('normalization', 'decode_compression', next)}
              />
            </div>
          </AdvancedPanel>
        </div>

        <AdvancedPanel title={t('promptFilter.contextDiscount.title')} hint={t('promptFilter.contextDiscount.description')}>
          <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-4">
            <SwitchField
              label={t('promptFilter.contextDiscount.enabled')}
              hint={t('promptFilter.contextDiscount.enabledHint')}
              checked={config.context_discount.enabled}
              onCheckedChange={(next) => setBool('context_discount', 'enabled', next)}
            />
            <SwitchField
              label={t('promptFilter.contextDiscount.intentAware')}
              hint={t('promptFilter.contextDiscount.intentAwareHint')}
              checked={config.context_discount.intent_aware}
              onCheckedChange={(next) => setBool('context_discount', 'intent_aware', next)}
            />
            <CompactField label={t('promptFilter.contextDiscount.maxDiscount')} hint={t('promptFilter.contextDiscount.maxDiscountHint')}>
              <DraftNumberInput
                min={0}
                max={90}
                value={config.context_discount.max_discount}
                onValueChange={(v) => update('context_discount', {
                  max_discount: v,
                  operational_max_discount: Math.min(config.context_discount.operational_max_discount, v),
                })}
              />
            </CompactField>
            <CompactField label={t('promptFilter.contextDiscount.operationalMaxDiscount')} hint={t('promptFilter.contextDiscount.operationalMaxDiscountHint')}>
              <DraftNumberInput min={0} max={config.context_discount.max_discount} value={config.context_discount.operational_max_discount} onValueChange={(v) => update('context_discount', { operational_max_discount: v })} />
            </CompactField>
          </div>
        </AdvancedPanel>

        <AdvancedPanel title={t('promptFilter.terminalCategories')}>
          <CompactField label={t('promptFilter.terminalCategories')} hint={t('promptFilter.help.terminalCategories')}>
            <Input
              value={terminalCategoriesText}
              placeholder="malware, credential_attack"
              onChange={(e) => update('enforcement', {
                terminal_categories: e.target.value.split(',').map((item) => item.trim()).filter(Boolean),
              })}
            />
          </CompactField>
          <p className="text-[11px] leading-relaxed text-muted-foreground">{t('promptFilter.terminalCategoriesHint')}</p>
        </AdvancedPanel>

        <AdvancedPanel title={t('promptFilter.riskTitle')}>
          <div className="grid grid-cols-1 gap-x-3 gap-y-3 sm:grid-cols-2">
            <SwitchField
              label={t('promptFilter.enabled')}
              hint={t('promptFilter.help.riskEnabled')}
              checked={config.risk.enabled}
              onCheckedChange={(next) => setBool('risk', 'enabled', next)}
            />
            <CompactField label={t('promptFilter.riskWindow')} hint={t('promptFilter.help.riskWindow')}>
              <DraftNumberInput min={60} max={86400} value={config.risk.window_seconds} onValueChange={(v) => update('risk', { window_seconds: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.blockThreshold')} hint={t('promptFilter.help.blockThreshold')}>
              <DraftNumberInput min={1} max={1000} value={config.risk.block_threshold} onValueChange={(v) => update('risk', { block_threshold: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.reviewThreshold')} hint={t('promptFilter.help.reviewThreshold')}>
              <DraftNumberInput min={1} max={1000} value={config.risk.review_threshold} onValueChange={(v) => update('risk', { review_threshold: v })} />
            </CompactField>
          </div>
        </AdvancedPanel>

        <AdvancedPanel title={t('promptFilter.outputScanTitle')}>
          <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-4">
            <SwitchField
              label={t('promptFilter.enabled')}
              hint={t('promptFilter.help.outputEnabled')}
              checked={config.output.enabled}
              onCheckedChange={(next) => setBool('output', 'enabled', next)}
            />
            <SwitchField
              label={t('promptFilter.strictOnly')}
              hint={t('promptFilter.help.strictOnly')}
              checked={config.output.strict_only}
              onCheckedChange={(next) => setBool('output', 'strict_only', next)}
            />
            <CompactField label={t('promptFilter.bufferBytes')} hint={t('promptFilter.help.bufferBytes')}>
              <DraftNumberInput min={512} max={65536} value={config.output.buffer_bytes} onValueChange={(v) => update('output', { buffer_bytes: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.overlapBytes')} hint={t('promptFilter.help.overlapBytes')}>
              <DraftNumberInput min={64} max={16384} value={config.output.overlap_bytes} onValueChange={(v) => update('output', { overlap_bytes: v })} />
            </CompactField>
          </div>
        </AdvancedPanel>
      </div>

      <SectionTitle title={t('promptFilter.extensions.title')} />
      <div className="grid gap-3 xl:grid-cols-2">
        <AdvancedPanel
          title={t('promptFilter.extensions.sidecar.title')}
          hint={t('promptFilter.extensions.sidecar.description')}
          footer={(
            <details className="group rounded-lg border border-foreground/10 bg-muted/10 open:bg-muted/15 dark:border-foreground/15">
              <summary className="flex h-9 cursor-pointer list-none items-center justify-between gap-2 px-3 text-sm font-medium marker:content-none [&::-webkit-details-marker]:hidden">
                <span>{t('promptFilter.extensions.runtimeLimits')}</span>
                <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
              </summary>
              <div className="grid gap-3 border-t border-foreground/8 px-3 py-3 sm:grid-cols-2 lg:grid-cols-3">
                <CompactField label={t('promptFilter.extensions.sidecar.maxTextLength')} hint={t('promptFilter.extensions.sidecar.maxTextLengthHint')}>
                  <DraftNumberInput min={512} max={65536} value={config.sidecar.max_text_length} onValueChange={(v) => update('sidecar', { max_text_length: v })} />
                </CompactField>
                <CompactField label={t('promptFilter.extensions.cacheTTL')} hint={t('promptFilter.extensions.sidecar.cacheTTLHint')}>
                  <DraftNumberInput min={0} max={86400} value={config.sidecar.cache_ttl_seconds} onValueChange={(v) => update('sidecar', { cache_ttl_seconds: v })} />
                </CompactField>
                <CompactField label={t('promptFilter.extensions.maxConcurrent')} hint={t('promptFilter.extensions.sidecar.maxConcurrentHint')}>
                  <DraftNumberInput min={1} max={128} value={config.sidecar.max_concurrent} onValueChange={(v) => update('sidecar', { max_concurrent: v })} />
                </CompactField>
                <CompactField label={t('promptFilter.extensions.breakerFailures')} hint={t('promptFilter.extensions.sidecar.breakerFailuresHint')}>
                  <DraftNumberInput min={1} max={20} value={config.sidecar.circuit_breaker_failures} onValueChange={(v) => update('sidecar', { circuit_breaker_failures: v })} />
                </CompactField>
                <CompactField label={t('promptFilter.extensions.breakerSeconds')} hint={t('promptFilter.extensions.sidecar.breakerSecondsHint')}>
                  <DraftNumberInput min={1} max={3600} value={config.sidecar.circuit_breaker_seconds} onValueChange={(v) => update('sidecar', { circuit_breaker_seconds: v })} />
                </CompactField>
              </div>
            </details>
          )}
        >
          <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-3">
            <SwitchField
              label={t('promptFilter.extensions.sidecar.enabled')}
              hint={t('promptFilter.extensions.sidecar.enabledHint')}
              checked={config.sidecar.enabled}
              onCheckedChange={(next) => setBool('sidecar', 'enabled', next)}
            />
            <SwitchField
              label={t('promptFilter.extensions.sidecar.scanClean')}
              hint={t('promptFilter.extensions.sidecar.scanCleanHint')}
              checked={config.sidecar.scan_clean_enabled}
              onCheckedChange={(next) => setBool('sidecar', 'scan_clean_enabled', next)}
            />
            <CompactField label={t('promptFilter.extensions.sidecar.mode')} hint={t('promptFilter.extensions.sidecar.modeHint')}>
              <Select value={config.sidecar.mode} onValueChange={(next) => update('sidecar', { mode: next as AdvancedProtectionConfig['sidecar']['mode'] })} options={sidecarModeOptions} />
            </CompactField>
            <div className="col-span-2 sm:col-span-3">
              <CompactField label={t('promptFilter.extensions.serviceURL')} hint={t('promptFilter.extensions.sidecar.baseURLHint')}>
                <Input value={config.sidecar.base_url} placeholder="http://127.0.0.1:18110" onChange={(e) => update('sidecar', { base_url: e.target.value })} />
              </CompactField>
            </div>
            <CompactField label={t('promptFilter.extensions.timeout')} hint={t('promptFilter.extensions.sidecar.timeoutHint')}>
              <DraftNumberInput min={1} max={30} value={config.sidecar.timeout_seconds} onValueChange={(v) => update('sidecar', { timeout_seconds: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.extensions.sidecar.minScore')} hint={t('promptFilter.extensions.sidecar.minScoreHint')}>
              <DraftNumberInput min={0} max={100} value={config.sidecar.min_score} onValueChange={(v) => update('sidecar', { min_score: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.extensions.sidecar.samplePercent')} hint={t('promptFilter.extensions.sidecar.samplePercentHint')}>
              <DraftNumberInput min={0} max={100} value={config.sidecar.sample_percent} onValueChange={(v) => update('sidecar', { sample_percent: v })} />
            </CompactField>
            <SwitchField
              label={t('promptFilter.extensions.sidecar.failClosed')}
              hint={t('promptFilter.extensions.sidecar.failClosedHint')}
              checked={config.sidecar.fail_closed}
              onCheckedChange={(next) => setBool('sidecar', 'fail_closed', next)}
            />
          </div>
        </AdvancedPanel>

        <AdvancedPanel
          title={t('promptFilter.extensions.session.title')}
          hint={t('promptFilter.extensions.session.description')}
          footer={(
            <div className="rounded-lg border border-sky-500/20 bg-sky-500/[0.06] p-3 text-xs leading-5 text-muted-foreground dark:border-sky-400/20 dark:bg-sky-400/[0.07]">
              {t('promptFilter.extensions.session.recommendedHint')}
            </div>
          )}
        >
          <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-3">
            <SwitchField
              label={t('promptFilter.extensions.session.enabled')}
              hint={t('promptFilter.extensions.session.enabledHint')}
              checked={config.session.enabled}
              onCheckedChange={(next) => setBool('session', 'enabled', next)}
            />
            <SwitchField
              label={t('promptFilter.extensions.session.requireSignedIdentity')}
              hint={t('promptFilter.extensions.session.requireSignedIdentityHint')}
              checked={config.session.require_signed_identity}
              onCheckedChange={(next) => setBool('session', 'require_signed_identity', next)}
            />
            <SwitchField
              label={t('promptFilter.extensions.session.combineShortFragments')}
              hint={t('promptFilter.extensions.session.combineShortFragmentsHint')}
              checked={config.session.combine_short_fragments}
              onCheckedChange={(next) => setBool('session', 'combine_short_fragments', next)}
            />
            <CompactField label={t('promptFilter.extensions.session.window')} hint={t('promptFilter.extensions.session.windowHint')}>
              <DraftNumberInput min={30} max={3600} value={config.session.window_seconds} onValueChange={(v) => update('session', { window_seconds: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.extensions.session.maxFragments')} hint={t('promptFilter.extensions.session.maxFragmentsHint')}>
              <DraftNumberInput min={1} max={10} value={config.session.max_fragments} onValueChange={(v) => update('session', { max_fragments: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.extensions.session.maxTextLength')} hint={t('promptFilter.extensions.session.maxTextLengthHint')}>
              <DraftNumberInput min={512} max={16384} value={config.session.max_text_length} onValueChange={(v) => update('session', { max_text_length: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.extensions.session.shortFragmentMaxChars')} hint={t('promptFilter.extensions.session.shortFragmentMaxCharsHint')}>
              <DraftNumberInput min={1} max={256} value={config.session.short_fragment_max_chars} onValueChange={(v) => update('session', { short_fragment_max_chars: v })} />
            </CompactField>
          </div>
        </AdvancedPanel>

        <div className="xl:col-span-2">
          <AdvancedPanel
            title={t('promptFilter.extensions.attachment.title')}
            hint={t('promptFilter.extensions.attachment.description')}
            footer={(
              <details className="group rounded-lg border border-foreground/10 bg-muted/10 open:bg-muted/15 dark:border-foreground/15">
                <summary className="flex h-9 cursor-pointer list-none items-center justify-between gap-2 px-3 text-sm font-medium marker:content-none [&::-webkit-details-marker]:hidden">
                  <span>{t('promptFilter.extensions.runtimeLimits')}</span>
                  <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
                </summary>
                <div className="grid gap-3 border-t border-foreground/8 px-3 py-3 sm:grid-cols-2 lg:grid-cols-5">
                  <CompactField label={t('promptFilter.extensions.cacheTTL')} hint={t('promptFilter.extensions.attachment.cacheTTLHint')}>
                    <DraftNumberInput min={0} max={86400} value={config.attachment.cache_ttl_seconds} onValueChange={(v) => update('attachment', { cache_ttl_seconds: v })} />
                  </CompactField>
                  <CompactField label={t('promptFilter.extensions.maxConcurrent')} hint={t('promptFilter.extensions.attachment.maxConcurrentHint')}>
                    <DraftNumberInput min={1} max={64} value={config.attachment.max_concurrent} onValueChange={(v) => update('attachment', { max_concurrent: v })} />
                  </CompactField>
                  <CompactField label={t('promptFilter.extensions.breakerFailures')} hint={t('promptFilter.extensions.attachment.breakerFailuresHint')}>
                    <DraftNumberInput min={1} max={20} value={config.attachment.circuit_breaker_failures} onValueChange={(v) => update('attachment', { circuit_breaker_failures: v })} />
                  </CompactField>
                  <CompactField label={t('promptFilter.extensions.breakerSeconds')} hint={t('promptFilter.extensions.attachment.breakerSecondsHint')}>
                    <DraftNumberInput min={1} max={3600} value={config.attachment.circuit_breaker_seconds} onValueChange={(v) => update('attachment', { circuit_breaker_seconds: v })} />
                  </CompactField>
                </div>
              </details>
            )}
          >
            <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-3 lg:grid-cols-6">
              <SwitchField
                label={t('promptFilter.extensions.attachment.enabled')}
                hint={t('promptFilter.extensions.attachment.enabledHint')}
                checked={config.attachment.enabled}
                onCheckedChange={(next) => setBool('attachment', 'enabled', next)}
              />
              <SwitchField
                label={t('promptFilter.extensions.attachment.allowRemoteURLs')}
                hint={t('promptFilter.extensions.attachment.allowRemoteURLsHint')}
                checked={config.attachment.allow_remote_urls}
                onCheckedChange={(next) => setBool('attachment', 'allow_remote_urls', next)}
              />
              <div className="col-span-2 sm:col-span-3 lg:col-span-4">
                <CompactField label={t('promptFilter.extensions.serviceURL')} hint={t('promptFilter.extensions.attachment.baseURLHint')}>
                  <Input value={config.attachment.base_url} placeholder="http://127.0.0.1:18120" onChange={(e) => update('attachment', { base_url: e.target.value })} />
                </CompactField>
              </div>
              <CompactField label={t('promptFilter.extensions.timeout')} hint={t('promptFilter.extensions.attachment.timeoutHint')}>
                <DraftNumberInput min={1} max={30} value={config.attachment.timeout_seconds} onValueChange={(v) => update('attachment', { timeout_seconds: v })} />
              </CompactField>
              <CompactField label={t('promptFilter.extensions.attachment.maxFiles')} hint={t('promptFilter.extensions.attachment.maxFilesHint')}>
                <DraftNumberInput min={1} max={16} value={config.attachment.max_files} onValueChange={(v) => update('attachment', { max_files: v })} />
              </CompactField>
              <CompactField label={t('promptFilter.extensions.attachment.maxBytes')} hint={t('promptFilter.extensions.attachment.maxBytesHint')}>
                <DraftNumberInput min={1024} max={1048576} value={config.attachment.max_bytes} onValueChange={(v) => update('attachment', { max_bytes: v })} />
              </CompactField>
              <CompactField label={t('promptFilter.extensions.attachment.maxExtractedChars')} hint={t('promptFilter.extensions.attachment.maxExtractedCharsHint')}>
                <DraftNumberInput min={512} max={65536} value={config.attachment.max_extracted_chars} onValueChange={(v) => update('attachment', { max_extracted_chars: v })} />
              </CompactField>
            </div>
          </AdvancedPanel>
        </div>
      </div>

      {/* Integration row: NewAPI + Intelligence — matched structure & equal height */}
      <div className="grid gap-3 xl:grid-cols-2">
        <AdvancedPanel
          title={t('promptFilter.newapi.title')}
          hint={t('promptFilter.newapi.description')}
          footer={(
            <details className="group rounded-lg border border-foreground/10 bg-muted/10 open:bg-muted/15 dark:border-foreground/15">
              <summary className="flex h-9 cursor-pointer list-none items-center justify-between gap-2 px-3 text-sm font-medium marker:content-none [&::-webkit-details-marker]:hidden">
                <span>{t('promptFilter.newapi.protocolTitle')}</span>
                <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
              </summary>
              <div className="space-y-4 border-t border-foreground/8 px-3 py-3">
                <div className="grid gap-4 lg:grid-cols-2">
                  <div className="space-y-2">
                    <div className="text-xs font-semibold text-muted-foreground">{t('promptFilter.newapi.codexEnv')}</div>
                    <SoftCodeBlock>{t('promptFilter.newapi.codexSecretExample')}</SoftCodeBlock>
                    <p className="text-xs leading-relaxed text-muted-foreground">{t('promptFilter.newapi.secretStorageHint')}</p>
                    <div className="rounded-lg border border-foreground/10 bg-background/80 p-3">
                      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                        <div className="flex items-center gap-2 text-sm font-medium"><KeyRound className="size-4 text-muted-foreground" />{t('promptFilter.newapi.generator')}</div>
                        <Button type="button" size="sm" variant="outline" disabled={secretSaving || secretStatus.source === 'environment'} onClick={() => void generateNewAPISecret()}>
                          <RefreshCw className={`size-3.5 ${secretSaving ? 'animate-spin' : ''}`} />
                          {secretStatus.configured ? t('promptFilter.newapi.replaceSecret') : t('promptFilter.newapi.generateSecret')}
                        </Button>
                      </div>
                      {secretError ? <p className="text-xs text-destructive">{secretError}</p> : null}
                      <p className="text-xs text-muted-foreground">
                        {secretStatus.configured
                          ? t('promptFilter.newapi.secretConfigured', {
                              masked: secretStatus.masked,
                              source: secretStatus.source === 'environment' ? t('promptFilter.newapi.environment') : t('promptFilter.newapi.database'),
                            })
                          : t('promptFilter.newapi.secretUnconfigured')}
                      </p>
                    </div>
                  </div>
                  <div className="space-y-2">
                    <div className="text-xs font-semibold text-muted-foreground">{t('promptFilter.newapi.newapiEnv')}</div>
                    <SoftCodeBlock>{t('promptFilter.newapi.newapiEnvExample')}</SoftCodeBlock>
                  </div>
                  <div className="space-y-2 lg:col-span-2">
                    <div className="text-xs font-semibold text-muted-foreground">{t('promptFilter.newapi.headersTitle')}</div>
                    <SoftCodeBlock>{t('promptFilter.newapi.headersExample')}</SoftCodeBlock>
                    <p className="text-xs leading-relaxed text-muted-foreground">{t('promptFilter.newapi.signatureHint')}</p>
                  </div>
                </div>
              </div>
            </details>
          )}
        >
          <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-4">
            <SwitchField
              label={t('promptFilter.newapi.enabled')}
              hint={t('promptFilter.newapi.enabledHint')}
              checked={config.newapi.enabled}
              onCheckedChange={(next) => setBool('newapi', 'enabled', next)}
            />
            <CompactField label={t('promptFilter.newapi.clockSkew')} hint={t('promptFilter.newapi.clockSkewHint')}>
              <DraftNumberInput min={30} max={600} value={config.newapi.max_clock_skew_seconds} onValueChange={(v) => update('newapi', { max_clock_skew_seconds: v })} />
            </CompactField>
          </div>
        </AdvancedPanel>

        <AdvancedPanel
          title={t('promptFilter.intelligence.configTitle')}
          footer={(
            <details className="group rounded-md border border-foreground/15 bg-muted/15 open:bg-muted/25 dark:border-foreground/20">
              <summary className="flex h-9 cursor-pointer list-none items-center justify-between gap-2 px-3 text-sm font-medium marker:content-none [&::-webkit-details-marker]:hidden">
                <span className="flex items-center gap-2">
                  {t('promptFilter.intelligence.queries')}
                  <Badge variant="outline" className="h-5 font-normal">{queryCount}</Badge>
                </span>
                <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
              </summary>
              <div className="space-y-3 border-t px-3 py-3">
                <CompactField label={t('promptFilter.intelligence.queries')} hint={t('promptFilter.help.queries')}>
                  <Textarea
                    rows={3}
                    value={config.intelligence.queries.join('\n')}
                    placeholder="LLM jailbreak prompt injection"
                    onChange={(e) => update('intelligence', {
                      queries: e.target.value.split('\n').map((item) => item.trim()).filter(Boolean),
                    })}
                  />
                </CompactField>
                <div className="rounded-md bg-muted/50 p-2.5">
                  <div className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                    {t('promptFilter.intelligence.builtinQueries')}
                  </div>
                  <div className="flex flex-wrap gap-1.5">
                    {['LLM jailbreak prompt injection', 'ChatGPT jailbreak prompt', 'Codex prompt injection jailbreak', '大模型 破限 提示词', 'GPT 破甲 提示词', 'AI 越狱 提示词', '中文 prompt injection 绕过'].map((query) => (
                      <Badge key={query} variant="outline" className="text-[11px] font-normal">{query}</Badge>
                    ))}
                  </div>
                </div>
              </div>
            </details>
          )}
        >
          <div className="grid grid-cols-2 gap-x-3 gap-y-3 sm:grid-cols-4">
            <SwitchField
              label={t('promptFilter.intelligence.scheduleEnabled')}
              hint={t('promptFilter.help.scheduleEnabled')}
              checked={config.intelligence.enabled}
              onCheckedChange={(next) => setBool('intelligence', 'enabled', next)}
            />
            <CompactField label={t('promptFilter.intelligence.intervalHours')} hint={t('promptFilter.help.intervalHours')}>
              <DraftNumberInput min={1} max={720} value={config.intelligence.interval_hours} onValueChange={(v) => update('intelligence', { interval_hours: v })} />
            </CompactField>
            <CompactField label={t('promptFilter.intelligence.maxResults')} hint={t('promptFilter.help.maxResults')}>
              <DraftNumberInput min={1} max={100} value={config.intelligence.max_search_results} onValueChange={(v) => update('intelligence', { max_search_results: v })} />
            </CompactField>
            <SwitchField
              label={t('promptFilter.intelligence.modelEnabled')}
              hint={t('promptFilter.help.modelEnabled')}
              checked={config.intelligence.model_enabled}
              onCheckedChange={(next) => setBool('intelligence', 'model_enabled', next)}
            />
            <CompactField label={t('promptFilter.intelligence.model')} hint={t('promptFilter.help.model')}>
              <Input value={config.intelligence.model} onChange={(e) => update('intelligence', { model: e.target.value })} />
            </CompactField>
            <CompactField label={t('promptFilter.intelligence.maxModelCalls')} hint={t('promptFilter.help.maxModelCalls')}>
              <DraftNumberInput min={0} max={3} value={config.intelligence.max_model_calls} onValueChange={(v) => update('intelligence', { max_model_calls: v })} />
            </CompactField>
            <SwitchField
              label={t('promptFilter.intelligence.autoAdd')}
              hint={t('promptFilter.help.autoAdd')}
              checked={config.intelligence.auto_add}
              onCheckedChange={(next) => setBool('intelligence', 'auto_add', next)}
            />
          </div>
        </AdvancedPanel>
      </div>

      <Dialog open={secretRevealOpen} onOpenChange={(open) => { if (!open) requestCloseSecretReveal() }}>
        <DialogContent className="sm:max-w-2xl" onEscapeKeyDown={(event) => { event.preventDefault(); requestCloseSecretReveal() }} onPointerDownOutside={(event) => { event.preventDefault(); requestCloseSecretReveal() }}>
          <DialogHeader>
            <DialogTitle>{t('promptFilter.newapi.revealTitle')}</DialogTitle>
            <DialogDescription>{t('promptFilter.newapi.revealDescription')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="flex gap-2">
              <Input readOnly value={generatedNewAPISecret} className="font-mono text-xs" />
              <Button type="button" variant="outline" onClick={() => void copyNewAPISecret()}>
                <Copy className="size-4" />
                {secretCopied ? t('promptFilter.newapi.copied') : t('promptFilter.newapi.copySecret')}
              </Button>
            </div>
            <SoftCodeBlock>{`CODEX2API_POLICY_SECRET=${generatedNewAPISecret}`}</SoftCodeBlock>
            <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
              {t('promptFilter.newapi.revealWarning')}
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={requestCloseSecretReveal}>{t('promptFilter.newapi.close')}</Button>
            <Button type="button" onClick={() => void copyNewAPISecret()}>
              <Copy className="size-4" />
              {secretCopied ? t('promptFilter.newapi.copied') : t('promptFilter.newapi.copyAndConfigure')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <Dialog open={secretCloseConfirmOpen} onOpenChange={setSecretCloseConfirmOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('promptFilter.newapi.closeConfirmTitle')}</DialogTitle>
            <DialogDescription>{t('promptFilter.newapi.closeConfirmDescription')}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setSecretCloseConfirmOpen(false)}>{t('promptFilter.newapi.backToCopy')}</Button>
            <Button type="button" variant="destructive" onClick={confirmCloseSecretReveal}>{t('promptFilter.newapi.confirmClose')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function FieldHint({ label, hint }: { label: string; hint?: string }) {
  if (!hint) return null
  return (
    <TooltipProvider delayDuration={150}>
      <Tooltip>
        <TooltipTrigger asChild>
          <button type="button" aria-label={`${label} help`} className="shrink-0 text-muted-foreground hover:text-primary" onClick={(event) => event.preventDefault()}>
            <HelpCircle className="size-3" />
          </button>
        </TooltipTrigger>
        <TooltipContent className="max-w-[320px] whitespace-normal leading-relaxed">{hint}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}

function AdvancedPanel({
  title,
  hint,
  children,
  footer,
}: {
  title: string
  hint?: string
  children: ReactNode
  footer?: ReactNode
}) {
  return (
    <div className="flex h-full min-h-0 flex-col gap-3 rounded-lg border border-foreground/15 bg-background p-3.5 shadow-sm dark:border-foreground/20">
      <div className="flex h-5 items-center gap-1.5">
        <h3 className="text-sm font-semibold leading-none text-foreground">{title}</h3>
        <FieldHint label={title} hint={hint} />
      </div>
      <div className="min-w-0 flex-1">{children}</div>
      {footer ? <div className="mt-auto min-w-0">{footer}</div> : null}
    </div>
  )
}

function SoftCodeBlock({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <pre
      className={cn(
        'overflow-x-auto rounded-lg border border-foreground/10 bg-muted/40 p-3 font-mono text-xs leading-6 text-foreground/85 shadow-none',
        'dark:border-foreground/12 dark:bg-muted/30 dark:text-foreground/80',
        className,
      )}
    >
      <code className="whitespace-pre-wrap break-all">{children}</code>
    </pre>
  )
}

function CompactField({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="flex min-w-0 flex-col gap-1.5">
      <span className="flex h-4 items-center gap-1 truncate text-xs font-medium leading-none text-muted-foreground">
        <span className="truncate">{label}</span>
        <FieldHint label={label} hint={hint} />
      </span>
      <div className="min-w-0 [&_input]:h-9 [&_input]:border-foreground/15 [&_input]:shadow-none dark:[&_input]:border-foreground/20">
        {children}
      </div>
    </label>
  )
}

function SwitchField({
  label,
  hint,
  checked,
  onCheckedChange,
}: {
  label: string
  hint?: string
  checked: boolean
  onCheckedChange: (checked: boolean) => void
}) {
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <span className="flex h-4 items-center gap-1 truncate text-xs font-medium leading-none text-muted-foreground">
        <span className="truncate">{label}</span>
        <FieldHint label={label} hint={hint} />
      </span>
      <div className="flex h-9 items-center rounded-md border border-foreground/15 bg-transparent px-3 dark:border-foreground/20 dark:bg-input/30">
        <Switch checked={checked} onCheckedChange={onCheckedChange} />
      </div>
    </div>
  )
}

function IntelligenceView() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [running, setRunning] = useState(false)
  const [adding, setAdding] = useState('')
  const [result, setResult] = useState<PromptIntelligenceRun | null>(null)
  const [history, setHistory] = useState<PromptIntelligenceRun[]>([])
  const [historyLoading, setHistoryLoading] = useState(false)

  const loadHistory = useCallback(async () => {
    setHistoryLoading(true)
    try { setHistory((await api.getPromptIntelligenceHistory(1, 20)).runs) } catch (error) { showToast(getErrorMessage(error), 'error') } finally { setHistoryLoading(false) }
  }, [showToast])

  useEffect(() => { void loadHistory() }, [loadHistory])

  const run = async () => {
    setRunning(true)
    try {
      const value = await api.runPromptIntelligence()
      setResult(value)
      await loadHistory()
      showToast(t('promptFilter.intelligence.runSuccess', { count: value.candidates.length }))
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setRunning(false)
    }
  }

  const add = async (candidate: PromptIntelligenceCandidate) => {
    setAdding(candidate.name)
    try {
      const value = await api.addPromptIntelligenceRule(candidate)
      showToast(value.updated ? t('promptFilter.intelligence.updateSuccess') : value.added ? t('promptFilter.intelligence.addSuccess') : t('promptFilter.intelligence.alreadyExists'))
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setAdding('')
    }
  }

  return (
    <div className="space-y-5">
      <Card>
        <CardContent className="p-5">
          <div className="flex flex-wrap items-start justify-between gap-4">
            <div>
              <h2 className="text-base font-semibold">{t('promptFilter.intelligence.title')}</h2>
              <p className="mt-1 max-w-3xl text-sm text-muted-foreground">{t('promptFilter.intelligence.description')}</p>
            </div>
            <Button onClick={() => void run()} disabled={running}>
              <Search className="size-4" />
              {running ? t('promptFilter.intelligence.running') : t('promptFilter.intelligence.run')}
            </Button>
          </div>
          <div className="mt-4 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-muted-foreground">
            {t('promptFilter.intelligence.auditHint')}
          </div>
        </CardContent>
      </Card>

      {result ? (
        <Card>
          <CardContent className="p-5">
            <div className="mb-4 flex flex-wrap gap-3 text-sm text-muted-foreground">
              <span>{t('promptFilter.intelligence.sources')}: {result.sources.length}</span>
              <span>{t('promptFilter.intelligence.modelCalls')}: {result.model_calls}</span>
              <span>{t('promptFilter.intelligence.candidates')}: {result.candidates.length}</span>
              <span>{t('promptFilter.intelligence.autoAdded')}: {result.added}</span>
            </div>
            {result.errors.length ? <div className="mb-4 rounded-lg border border-destructive/30 p-3 text-sm text-destructive">{result.errors.join('；')}</div> : null}
            <Table>
              <TableHeader><TableRow><TableHead>{t('promptFilter.intelligence.rule')}</TableHead><TableHead>{t('promptFilter.intelligence.category')}</TableHead><TableHead>{t('promptFilter.intelligence.weight')}</TableHead><TableHead>{t('promptFilter.intelligence.reason')}</TableHead><TableHead className="text-right">{t('common.actions')}</TableHead></TableRow></TableHeader>
              <TableBody>
                {result.candidates.map((candidate) => (
                  <TableRow key={`${candidate.name}-${candidate.pattern}`}>
                    <TableCell><div className="flex items-center gap-2 font-medium">{candidate.name}<Badge variant="outline" className={candidate.status === 'update' ? 'border-amber-500/40 text-amber-600' : 'border-emerald-500/40 text-emerald-600'}>{candidate.status === 'update' ? t('promptFilter.intelligence.update') : t('promptFilter.intelligence.new')}</Badge></div><code className="mt-1 block max-w-md break-all text-xs text-muted-foreground">{candidate.pattern}</code></TableCell>
                    <TableCell>{candidate.category}</TableCell><TableCell>{candidate.weight}{candidate.strict ? ' / strict' : ''}</TableCell>
                    <TableCell className="max-w-sm text-sm text-muted-foreground">{candidate.rationale || '-'}</TableCell>
                    <TableCell className="text-right"><Button size="sm" variant="outline" disabled={adding === candidate.name} onClick={() => void add(candidate)}>{candidate.status === 'update' ? t('promptFilter.intelligence.updateRule') : t('promptFilter.intelligence.addRule')}</Button></TableCell>
                  </TableRow>
                ))}
                {!result.candidates.length ? <TableRow><TableCell colSpan={5} className="py-8 text-center text-muted-foreground">{t('promptFilter.intelligence.noCandidates')}</TableCell></TableRow> : null}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardContent className="p-5">
          <div className="mb-4 flex items-center justify-between"><div><h2 className="text-base font-semibold">{t('promptFilter.intelligence.historyTitle')}</h2><p className="mt-1 text-sm text-muted-foreground">{t('promptFilter.intelligence.historyDesc')}</p></div><Button variant="outline" size="sm" onClick={() => void loadHistory()} disabled={historyLoading}><RefreshCw className="size-4" />{t('common.refresh')}</Button></div>
          <div className="space-y-3">
            {history.map((run, index) => <div key={`${run.started_at}-${index}`} className="rounded-lg border p-4"><div className="flex flex-wrap items-center justify-between gap-2"><div className="font-medium">{formatBeijingTime(run.started_at)}</div><div className="flex gap-2"><Badge variant="outline">{t('promptFilter.intelligence.sources')} {run.sources.length}</Badge><Badge variant="outline">{t('promptFilter.intelligence.candidates')} {run.candidates.length}</Badge><Badge variant="outline">{t('promptFilter.intelligence.modelCalls')} {run.model_calls}</Badge></div></div><div className="mt-3 grid gap-2 md:grid-cols-2">{run.sources.map((source) => <a key={source.url} href={source.url} target="_blank" rel="noreferrer" className="rounded-md bg-muted/40 p-2 text-sm hover:text-primary"><div className="font-medium">{source.title}</div><div className="truncate text-xs text-muted-foreground">{source.url}</div></a>)}</div>{run.errors.length ? <div className="mt-3 text-sm text-destructive">{run.errors.join('；')}</div> : null}</div>)}
            {!historyLoading && !history.length ? <div className="py-8 text-center text-muted-foreground">{t('promptFilter.intelligence.noHistory')}</div> : null}
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

type DocsSectionKind = 'default' | 'pipeline' | 'pages' | 'modes' | 'features' | 'checklist'

type DocsSection = {
  id: string
  group: 'intro' | 'setup' | 'core' | 'ops'
  kind: DocsSectionKind
  title: string
  icon: ReactNode
  paragraphs?: string[]
  bullets?: string[]
  steps?: string[]
  callout?: string
  table?: { headers: string[]; rows: string[][] }
  cards?: { title: string; body: string; tone?: 'neutral' | 'warn' | 'danger' | 'success' }[]
}

const DOCS_GROUP_ORDER = ['intro', 'setup', 'core', 'ops'] as const

function DocsView() {
  const { t } = useTranslation()
  const [activeId, setActiveId] = useState('what')
  const activeLockRef = useRef(false)
  const activeLockTimerRef = useRef<number | null>(null)

  const sections = useMemo<DocsSection[]>(() => [
    {
      id: 'what',
      group: 'intro',
      kind: 'features',
      icon: <Shield className="size-4" />,
      title: t('promptFilter.docs.what.title'),
      paragraphs: [t('promptFilter.docs.what.p1'), t('promptFilter.docs.what.p2')],
      bullets: [
        t('promptFilter.docs.what.b1'),
        t('promptFilter.docs.what.b2'),
        t('promptFilter.docs.what.b3'),
        t('promptFilter.docs.what.b4'),
      ],
    },
    {
      id: 'pipeline',
      group: 'intro',
      kind: 'pipeline',
      icon: <GitBranch className="size-4" />,
      title: t('promptFilter.docs.pipeline.title'),
      paragraphs: [t('promptFilter.docs.pipeline.p1')],
      steps: [
        t('promptFilter.docs.pipeline.s1'),
        t('promptFilter.docs.pipeline.s2'),
        t('promptFilter.docs.pipeline.s3'),
        t('promptFilter.docs.pipeline.s4'),
        t('promptFilter.docs.pipeline.s5'),
        t('promptFilter.docs.pipeline.s6'),
        t('promptFilter.docs.pipeline.s7'),
      ],
    },
    {
      id: 'pages',
      group: 'intro',
      kind: 'pages',
      icon: <Layers className="size-4" />,
      title: t('promptFilter.docs.pages.title'),
      paragraphs: [t('promptFilter.docs.pages.p1')],
      table: {
        headers: [t('promptFilter.docs.pages.colPage'), t('promptFilter.docs.pages.colUse')],
        rows: [
          [t('promptFilter.views.overview'), t('promptFilter.docs.pages.overview')],
          [t('promptFilter.views.logs'), t('promptFilter.docs.pages.logs')],
          [t('promptFilter.views.rules'), t('promptFilter.docs.pages.rules')],
          [t('promptFilter.views.intelligence'), t('promptFilter.docs.pages.intelligence')],
          [t('promptFilter.views.docs'), t('promptFilter.docs.pages.docs')],
        ],
      },
    },
    {
      id: 'quickstart',
      group: 'setup',
      kind: 'checklist',
      icon: <Sparkles className="size-4" />,
      title: t('promptFilter.docs.quickstart.title'),
      paragraphs: [t('promptFilter.docs.quickstart.p1')],
      steps: [
        t('promptFilter.docs.quickstart.s1'),
        t('promptFilter.docs.quickstart.s2'),
        t('promptFilter.docs.quickstart.s3'),
        t('promptFilter.docs.quickstart.s4'),
        t('promptFilter.docs.quickstart.s5'),
        t('promptFilter.docs.quickstart.s6'),
      ],
      callout: t('promptFilter.docs.quickstart.callout'),
    },
    {
      id: 'modes',
      group: 'setup',
      kind: 'modes',
      icon: <Gauge className="size-4" />,
      title: t('promptFilter.docs.modes.title'),
      paragraphs: [t('promptFilter.docs.modes.p1')],
      cards: [
        { title: t('promptFilter.modeMonitor'), body: t('promptFilter.docs.modes.monitor'), tone: 'neutral' },
        { title: t('promptFilter.modeWarn'), body: t('promptFilter.docs.modes.warn'), tone: 'warn' },
        { title: t('promptFilter.modeBlock'), body: t('promptFilter.docs.modes.block'), tone: 'danger' },
      ],
      callout: t('promptFilter.docs.modes.callout'),
    },
    {
      id: 'scoring',
      group: 'core',
      kind: 'default',
      icon: <Activity className="size-4" />,
      title: t('promptFilter.docs.scoring.title'),
      paragraphs: [t('promptFilter.docs.scoring.p1'), t('promptFilter.docs.scoring.p2')],
      bullets: [
        t('promptFilter.docs.scoring.b1'),
        t('promptFilter.docs.scoring.b2'),
        t('promptFilter.docs.scoring.b3'),
        t('promptFilter.docs.scoring.b4'),
      ],
    },
    {
      id: 'advanced',
      group: 'core',
      kind: 'features',
      icon: <ShieldAlert className="size-4" />,
      title: t('promptFilter.docs.advanced.title'),
      paragraphs: [t('promptFilter.docs.advanced.p1')],
      bullets: [
        t('promptFilter.docs.advanced.b1'),
        t('promptFilter.docs.advanced.b2'),
        t('promptFilter.docs.advanced.b3'),
        t('promptFilter.docs.advanced.b4'),
        t('promptFilter.docs.advanced.b5'),
        t('promptFilter.docs.advanced.b6'),
      ],
      callout: t('promptFilter.docs.advanced.callout'),
    },
    {
      id: 'review',
      group: 'core',
      kind: 'default',
      icon: <ClipboardCheck className="size-4" />,
      title: t('promptFilter.docs.review.title'),
      paragraphs: [t('promptFilter.docs.review.p1'), t('promptFilter.docs.review.p2')],
      bullets: [
        t('promptFilter.docs.review.b1'),
        t('promptFilter.docs.review.b2'),
        t('promptFilter.docs.review.b3'),
      ],
    },
    {
      id: 'rules',
      group: 'ops',
      kind: 'default',
      icon: <FileText className="size-4" />,
      title: t('promptFilter.docs.rules.title'),
      paragraphs: [t('promptFilter.docs.rules.p1'), t('promptFilter.docs.rules.p2')],
      bullets: [
        t('promptFilter.docs.rules.b1'),
        t('promptFilter.docs.rules.b2'),
        t('promptFilter.docs.rules.b3'),
        t('promptFilter.docs.rules.b4'),
      ],
    },
    {
      id: 'logs',
      group: 'ops',
      kind: 'default',
      icon: <ListChecks className="size-4" />,
      title: t('promptFilter.docs.logs.title'),
      paragraphs: [t('promptFilter.docs.logs.p1')],
      bullets: [
        t('promptFilter.docs.logs.b1'),
        t('promptFilter.docs.logs.b2'),
        t('promptFilter.docs.logs.b3'),
        t('promptFilter.docs.logs.b4'),
      ],
    },
    {
      id: 'intelligence',
      group: 'ops',
      kind: 'default',
      icon: <Search className="size-4" />,
      title: t('promptFilter.docs.intelligence.title'),
      paragraphs: [t('promptFilter.docs.intelligence.p1')],
      bullets: [
        t('promptFilter.docs.intelligence.b1'),
        t('promptFilter.docs.intelligence.b2'),
        t('promptFilter.docs.intelligence.b3'),
      ],
      callout: t('promptFilter.docs.intelligence.callout'),
    },
    {
      id: 'newapi',
      group: 'ops',
      kind: 'default',
      icon: <Network className="size-4" />,
      title: t('promptFilter.docs.newapi.title'),
      paragraphs: [t('promptFilter.docs.newapi.p1'), t('promptFilter.docs.newapi.p2')],
      bullets: [
        t('promptFilter.docs.newapi.b1'),
        t('promptFilter.docs.newapi.b2'),
        t('promptFilter.docs.newapi.b3'),
      ],
    },
    {
      id: 'checklist',
      group: 'ops',
      kind: 'checklist',
      icon: <CheckCircle2 className="size-4" />,
      title: t('promptFilter.docs.checklist.title'),
      paragraphs: [t('promptFilter.docs.checklist.p1')],
      steps: [
        t('promptFilter.docs.checklist.s1'),
        t('promptFilter.docs.checklist.s2'),
        t('promptFilter.docs.checklist.s3'),
        t('promptFilter.docs.checklist.s4'),
        t('promptFilter.docs.checklist.s5'),
        t('promptFilter.docs.checklist.s6'),
      ],
    },
  ], [t])

  const groups = useMemo(() => {
    return DOCS_GROUP_ORDER.map((group) => ({
      id: group,
      label: t(`promptFilter.docs.groups.${group}`),
      items: sections.filter((section) => section.group === group),
    })).filter((group) => group.items.length > 0)
  }, [sections, t])

  const sectionIds = useMemo(() => sections.map((section) => section.id), [sections])

  useEffect(() => {
    const SPY_OFFSET = 120

    const resolveActiveSection = () => {
      if (activeLockRef.current) return

      let current = sectionIds[0] ?? 'what'
      for (const id of sectionIds) {
        const el = document.getElementById(`pf-docs-${id}`)
        if (!el) continue
        // Last section whose top has reached/passed the spy line is active.
        if (el.getBoundingClientRect().top - SPY_OFFSET <= 0) {
          current = id
        } else {
          break
        }
      }
      setActiveId((prev) => (prev === current ? prev : current))
    }

    let frame = 0
    const onScroll = () => {
      if (frame) return
      frame = window.requestAnimationFrame(() => {
        frame = 0
        resolveActiveSection()
      })
    }

    resolveActiveSection()
    window.addEventListener('scroll', onScroll, { passive: true })
    window.addEventListener('resize', onScroll)
    return () => {
      if (frame) window.cancelAnimationFrame(frame)
      window.removeEventListener('scroll', onScroll)
      window.removeEventListener('resize', onScroll)
      if (activeLockTimerRef.current != null) {
        window.clearTimeout(activeLockTimerRef.current)
        activeLockTimerRef.current = null
      }
    }
  }, [sectionIds])

  const scrollTo = (id: string) => {
    const el = document.getElementById(`pf-docs-${id}`)
    if (!el) return

    // Lock highlight during smooth scroll so the next section is not auto-selected mid-animation.
    activeLockRef.current = true
    setActiveId(id)
    if (activeLockTimerRef.current != null) {
      window.clearTimeout(activeLockTimerRef.current)
    }

    el.scrollIntoView({ behavior: 'smooth', block: 'start' })

    activeLockTimerRef.current = window.setTimeout(() => {
      activeLockRef.current = false
      activeLockTimerRef.current = null
      // Re-sync once after scroll settles (in case user scrolled past during lock).
      const SPY_OFFSET = 120
      let current = id
      for (const sectionId of sectionIds) {
        const node = document.getElementById(`pf-docs-${sectionId}`)
        if (!node) continue
        if (node.getBoundingClientRect().top - SPY_OFFSET <= 0) {
          current = sectionId
        } else {
          break
        }
      }
      setActiveId(current)
    }, 900)
  }

  return (
    <div className="grid gap-5 xl:grid-cols-[260px_minmax(0,1fr)]">
      {/* Sidebar TOC */}
      <aside className="h-fit xl:sticky xl:top-3">
        <div className="overflow-hidden rounded-xl border border-foreground/12 bg-card shadow-sm">
          <div className="border-b border-foreground/10 bg-muted/30 px-4 py-3">
            <div className="text-[11px] font-semibold uppercase tracking-[0.14em] text-muted-foreground">
              {t('promptFilter.docs.toc')}
            </div>
            <div className="mt-1 text-sm font-semibold text-foreground">{t('promptFilter.docs.tocHint')}</div>
          </div>
          <nav className="max-h-[min(70vh,720px)] space-y-4 overflow-y-auto p-3 [scrollbar-width:thin]">
            {groups.map((group) => (
              <div key={group.id}>
                <div className="mb-1.5 px-2 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground/80">
                  {group.label}
                </div>
                <div className="space-y-0.5">
                  {group.items.map((section) => {
                    const active = activeId === section.id
                    return (
                      <button
                        key={section.id}
                        type="button"
                        onClick={() => scrollTo(section.id)}
                        className={cn(
                          'flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-left text-sm transition-colors',
                          active
                            ? 'bg-primary/10 font-medium text-primary shadow-sm ring-1 ring-primary/15'
                            : 'text-muted-foreground hover:bg-muted/70 hover:text-foreground',
                        )}
                      >
                        <span className={cn(
                          'flex size-7 shrink-0 items-center justify-center rounded-md border',
                          active ? 'border-primary/20 bg-primary/10 text-primary' : 'border-foreground/10 bg-background text-muted-foreground',
                        )}>
                          {section.icon}
                        </span>
                        <span className="min-w-0 flex-1 truncate leading-snug">{section.title}</span>
                      </button>
                    )
                  })}
                </div>
              </div>
            ))}
          </nav>
        </div>
      </aside>

      {/* Document body */}
      <article className="overflow-hidden rounded-xl border border-foreground/12 bg-card shadow-sm">
        <header className="relative overflow-hidden border-b border-foreground/10 bg-gradient-to-br from-primary/[0.07] via-card to-card px-6 py-7 sm:px-8 sm:py-8">
          <div className="pointer-events-none absolute -right-16 -top-20 size-56 rounded-full bg-primary/10 blur-3xl" />
          <div className="pointer-events-none absolute -bottom-20 right-20 size-40 rounded-full bg-sky-400/10 blur-3xl" />
          <div className="relative flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
            <div className="max-w-3xl">
              <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-primary/20 bg-primary/8 px-2.5 py-1 text-[11px] font-semibold uppercase tracking-wide text-primary">
                <BookOpen className="size-3.5" />
                {t('promptFilter.docs.badge')}
              </div>
              <h1 className="text-2xl font-semibold tracking-tight text-foreground sm:text-[1.75rem]">
                {t('promptFilter.docs.title')}
              </h1>
              <p className="mt-2.5 text-sm leading-7 text-muted-foreground sm:text-[15px]">
                {t('promptFilter.docs.description')}
              </p>
            </div>
            <div className="flex shrink-0 flex-wrap gap-2">
              <Badge variant="outline" className="h-7 border-foreground/15 bg-background/80 font-normal">
                {t('promptFilter.docs.metaSections', { count: sections.length })}
              </Badge>
              <Badge variant="outline" className="h-7 border-foreground/15 bg-background/80 font-normal">
                {t('promptFilter.docs.metaAudience')}
              </Badge>
            </div>
          </div>
        </header>

        <div className="divide-y divide-foreground/10">
          {sections.map((section, index) => (
            <section
              key={section.id}
              id={`pf-docs-${section.id}`}
              className="scroll-mt-24 px-6 py-7 sm:px-8 sm:py-8"
            >
              <div className="mb-4 flex items-start gap-3">
                <div className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-lg border border-foreground/12 bg-muted/40 text-foreground">
                  {section.icon}
                </div>
                <div className="min-w-0">
                  <div className="mb-1 flex flex-wrap items-center gap-2">
                    <span className="font-mono text-[11px] font-medium tracking-wide text-muted-foreground">
                      {String(index + 1).padStart(2, '0')}
                    </span>
                    <span className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground/80">
                      {t(`promptFilter.docs.groups.${section.group}`)}
                    </span>
                  </div>
                  <h2 className="text-lg font-semibold tracking-tight text-foreground sm:text-xl">
                    {section.title}
                  </h2>
                </div>
              </div>

              <div className="space-y-4 pl-0 sm:pl-12">
                {section.paragraphs?.map((paragraph) => (
                  <p key={paragraph} className="text-sm leading-7 text-muted-foreground sm:text-[15px]">
                    {paragraph}
                  </p>
                ))}

                {section.kind === 'pipeline' && section.steps?.length ? (
                  <div className="relative space-y-0">
                    <div className="absolute bottom-3 left-[15px] top-3 w-px bg-border" />
                    {section.steps.map((step, stepIndex) => (
                      <div key={step} className="relative flex gap-3 py-2.5">
                        <div className="relative z-10 flex size-8 shrink-0 items-center justify-center rounded-full border border-foreground/15 bg-background text-xs font-semibold text-foreground shadow-sm">
                          {stepIndex + 1}
                        </div>
                        <div className="min-w-0 flex-1 rounded-lg border border-foreground/10 bg-muted/20 px-3.5 py-2.5 text-sm leading-6 text-foreground/90">
                          {step}
                        </div>
                      </div>
                    ))}
                  </div>
                ) : null}

                {section.kind === 'checklist' && section.steps?.length ? (
                  <ol className="space-y-2.5">
                    {section.steps.map((step, stepIndex) => (
                      <li
                        key={step}
                        className="flex gap-3 rounded-lg border border-foreground/10 bg-background px-3.5 py-3 text-sm leading-6 shadow-sm"
                      >
                        <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-primary/10 text-xs font-semibold text-primary">
                          {stepIndex + 1}
                        </span>
                        <span className="pt-0.5 text-foreground/90">{step}</span>
                      </li>
                    ))}
                  </ol>
                ) : null}

                {section.kind !== 'pipeline' && section.kind !== 'checklist' && section.steps?.length ? (
                  <ol className="space-y-2">
                    {section.steps.map((step, stepIndex) => (
                      <li key={step} className="flex gap-3 text-sm leading-6">
                        <span className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full border border-foreground/15 bg-muted/40 text-[11px] font-semibold">
                          {stepIndex + 1}
                        </span>
                        <span className="text-foreground/90">{step}</span>
                      </li>
                    ))}
                  </ol>
                ) : null}

                {section.kind === 'features' && section.bullets?.length ? (
                  <div className="grid gap-2.5 sm:grid-cols-2">
                    {section.bullets.map((bullet, bulletIndex) => (
                      <div
                        key={bullet}
                        className="rounded-lg border border-foreground/10 bg-muted/15 px-3.5 py-3 text-sm leading-6 text-foreground/90"
                      >
                        <div className="mb-1.5 font-mono text-[11px] font-medium text-muted-foreground">
                          {String(bulletIndex + 1).padStart(2, '0')}
                        </div>
                        {bullet}
                      </div>
                    ))}
                  </div>
                ) : null}

                {section.kind !== 'features' && section.bullets?.length ? (
                  <ul className="space-y-2.5">
                    {section.bullets.map((bullet) => (
                      <li key={bullet} className="flex gap-2.5 text-sm leading-6 text-foreground/90">
                        <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-primary/80" />
                        <span>{bullet}</span>
                      </li>
                    ))}
                  </ul>
                ) : null}

                {section.kind === 'modes' && section.cards?.length ? (
                  <div className="grid gap-3 md:grid-cols-3">
                    {section.cards.map((card) => (
                      <div
                        key={card.title}
                        className={cn(
                          'rounded-xl border p-4 shadow-sm',
                          card.tone === 'warn' && 'border-amber-500/25 bg-amber-500/[0.06]',
                          card.tone === 'danger' && 'border-rose-500/25 bg-rose-500/[0.06]',
                          card.tone === 'success' && 'border-emerald-500/25 bg-emerald-500/[0.06]',
                          (!card.tone || card.tone === 'neutral') && 'border-foreground/10 bg-muted/20',
                        )}
                      >
                        <div className="mb-2 text-sm font-semibold text-foreground">{card.title}</div>
                        <p className="text-sm leading-6 text-muted-foreground">{card.body}</p>
                      </div>
                    ))}
                  </div>
                ) : null}

                {section.table ? (
                  <div className="overflow-hidden rounded-xl border border-foreground/12 shadow-sm">
                    <Table>
                      <TableHeader>
                        <TableRow className="bg-muted/40 hover:bg-muted/40">
                          {section.table.headers.map((header) => (
                            <TableHead key={header} className="h-10 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                              {header}
                            </TableHead>
                          ))}
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {section.table.rows.map((row) => (
                          <TableRow key={row.join('|')} className="hover:bg-muted/20">
                            {row.map((cell, cellIndex) => (
                              <TableCell
                                key={`${cell}-${cellIndex}`}
                                className={cn(
                                  'align-top text-sm leading-6',
                                  cellIndex === 0 ? 'w-[140px] font-semibold text-foreground whitespace-nowrap' : 'text-muted-foreground',
                                )}
                              >
                                {cell}
                              </TableCell>
                            ))}
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </div>
                ) : null}

                {section.callout ? (
                  <div className="flex gap-3 rounded-xl border border-amber-500/25 bg-amber-500/[0.07] px-4 py-3.5">
                    <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
                    <p className="text-sm leading-6 text-foreground/85">{section.callout}</p>
                  </div>
                ) : null}
              </div>
            </section>
          ))}
        </div>
      </article>
    </div>
  )
}

function OverviewView({
  form,
  setForm,
  saving,
  modeOptions,
  booleanOptions,
  endpointOptions,
  recentLogs,
  totalLogs,
  testText,
  setTestText,
  testEndpoint,
  setTestEndpoint,
  testModel,
  setTestModel,
  testing,
  testVerdict,
  runTest,
  clearLogs,
  clearing,
  onSave,
}: {
  form: PromptFilterForm
  setForm: Dispatch<SetStateAction<PromptFilterForm>>
  saving: boolean
  modeOptions: { label: string; value: string }[]
  booleanOptions: { label: string; value: string }[]
  endpointOptions: { label: string; value: string }[]
  recentLogs: PromptFilterLog[]
  totalLogs: number
  testText: string
  setTestText: (value: string) => void
  testEndpoint: string
  setTestEndpoint: (value: string) => void
  testModel: string
  setTestModel: (value: string) => void
  testing: boolean
  testVerdict: PromptFilterVerdict | null
  runTest: () => void
  clearLogs: () => Promise<void>
  clearing: boolean
  onSave: () => void
}) {
  const { t } = useTranslation()
  const stats = useMemo(() => ({
    blocks: recentLogs.filter((log) => log.action === 'block').length,
    upstream: recentLogs.filter((log) => log.source === 'upstream_cyber_policy').length,
    latest: recentLogs[0]?.created_at,
  }), [recentLogs])

  return (
    <>
      <div className="mb-4 grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3">
        <MetricTile label={t('promptFilter.status')}>
          <Badge variant={form.prompt_filter_enabled ? 'default' : 'outline'}>
            {form.prompt_filter_enabled ? t('common.enabled') : t('common.disabled')}
          </Badge>
        </MetricTile>
        <MetricTile label={t('promptFilter.currentMode')}>
          {modeOptions.find((item) => item.value === form.prompt_filter_mode)?.label ?? form.prompt_filter_mode}
        </MetricTile>
        <MetricTile label={t('promptFilter.recentBlockedLogs')}>{stats.blocks}</MetricTile>
        <MetricTile label={t('promptFilter.totalLogs')}>{totalLogs}</MetricTile>
        <MetricTile label={t('promptFilter.latestLog')}>
          {stats.latest ? formatRelativeTime(stats.latest, { variant: 'compact' }) : '-'}
        </MetricTile>
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,0.9fr)_minmax(420px,1.1fr)]">
        <Card>
          <CardContent className="space-y-5">
            <SectionTitle title={t('promptFilter.rulesTitle')} />
            <div className="grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-4">
              <Field label={t('promptFilter.enabled')}>
                <Select
                  value={form.prompt_filter_enabled ? 'true' : 'false'}
                  onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_enabled: value === 'true' }))}
                  options={booleanOptions}
                />
              </Field>
              <Field label={t('promptFilter.mode')}>
                <Select
                  value={form.prompt_filter_mode}
                  onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_mode: value }))}
                  options={modeOptions}
                />
              </Field>
              <Field label={t('promptFilter.threshold')}>
                <DraftNumberInput min={1} max={100} value={form.prompt_filter_threshold} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_threshold: value }))} />
              </Field>
              <Field label={t('promptFilter.strictThreshold')}>
                <DraftNumberInput min={1} max={100} value={form.prompt_filter_strict_threshold} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_strict_threshold: value }))} />
              </Field>
              <Field label={t('promptFilter.strictTerminal')} hint={t('promptFilter.strictTerminalHint')}>
                <Select value={form.prompt_filter_strict_terminal_enabled ? 'true' : 'false'} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_strict_terminal_enabled: value === 'true' }))} options={booleanOptions} />
              </Field>
              <Field label={t('promptFilter.logMatches')}>
                <Select value={form.prompt_filter_log_matches ? 'true' : 'false'} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_log_matches: value === 'true' }))} options={booleanOptions} />
              </Field>
              <Field label={t('promptFilter.maxTextLength')}>
                <DraftNumberInput min={1024} max={262144} value={form.prompt_filter_max_text_length} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_max_text_length: value }))} />
              </Field>
            </div>
            <Field label={t('promptFilter.sensitiveWords')}>
              <Textarea rows={5} value={form.prompt_filter_sensitive_words} placeholder={t('promptFilter.sensitiveWordsPlaceholder')} onChange={(event) => setForm((current) => ({ ...current, prompt_filter_sensitive_words: event.target.value }))} />
              <span className="block text-xs leading-5 text-muted-foreground">{t('promptFilter.sensitiveWordsHint')}</span>
            </Field>
          </CardContent>
        </Card>

        <Card>
          <CardContent className="space-y-5">
            <SectionTitle title={t('promptFilter.testerTitle')} />
            <div className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
              <Field label={t('promptFilter.testEndpoint')}>
                <Select value={testEndpoint} onValueChange={setTestEndpoint} options={endpointOptions} />
              </Field>
              <Field label={t('promptFilter.testModel')}>
                <Input value={testModel} onChange={(event) => setTestModel(event.target.value)} />
              </Field>
            </div>
            <Field label={t('promptFilter.testText')}>
              <Textarea rows={10} value={testText} placeholder={t('promptFilter.testPlaceholder')} onChange={(event) => setTestText(event.target.value)} />
            </Field>
            <div className="flex flex-wrap items-center gap-2">
              <Button onClick={runTest} disabled={testing}>
                <Wand2 className="size-4" />
                {testing ? t('promptFilter.testing') : t('promptFilter.runTest')}
              </Button>
              {testVerdict ? <VerdictBadge verdict={testVerdict} /> : null}
            </div>
            {testVerdict ? <VerdictPanel verdict={testVerdict} /> : null}
          </CardContent>
        </Card>
      </div>

      <Card className="mt-4">
        <CardContent className="space-y-5 pt-5">
          <AdvancedProtectionEditor
            value={form.prompt_filter_advanced_config}
            onChange={(value) => setForm((current) => ({ ...current, prompt_filter_advanced_config: value }))}
          />

          <div className="space-y-4 rounded-lg border border-border bg-muted/20 p-4">
            <div>
              <SectionTitle title={t('promptFilter.reviewTitle')} />
              <p className="mt-1 text-sm text-muted-foreground">{t('promptFilter.reviewDesc')}</p>
            </div>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-4">
              <Field label={t('promptFilter.reviewEnabled')}>
                <Select
                  value={form.prompt_filter_review_enabled ? 'true' : 'false'}
                  onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_review_enabled: value === 'true' }))}
                  options={booleanOptions}
                />
              </Field>
              <Field label={t('promptFilter.reviewFailClosed')}>
                <Select
                  value={form.prompt_filter_review_fail_closed ? 'true' : 'false'}
                  onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_review_fail_closed: value === 'true' }))}
                  options={[
                    { label: t('promptFilter.reviewFailClosedBlock'), value: 'true' },
                    { label: t('promptFilter.reviewFailClosedAllow'), value: 'false' },
                  ]}
                />
              </Field>
              <Field label={t('promptFilter.reviewTimeout')}>
                <DraftNumberInput min={1} max={60} value={form.prompt_filter_review_timeout_seconds} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_review_timeout_seconds: value }))} />
              </Field>
            </div>
            <div className="grid gap-4 lg:grid-cols-[minmax(0,1.3fr)_minmax(180px,0.8fr)]">
              <Field label={t('promptFilter.reviewBaseUrl')}>
                <Input value={form.prompt_filter_review_base_url} placeholder="https://api.openai.com" onChange={(event) => setForm((current) => ({ ...current, prompt_filter_review_base_url: event.target.value }))} />
              </Field>
              <Field label={t('promptFilter.reviewModel')}>
                <Input value={form.prompt_filter_review_model} placeholder="omni-moderation-latest" onChange={(event) => setForm((current) => ({ ...current, prompt_filter_review_model: event.target.value }))} />
              </Field>
            </div>
            <Field label={t('promptFilter.reviewApiKey')}>
              <Textarea
                rows={3}
                className="font-mono"
                value={form.prompt_filter_review_api_key ?? ''}
                placeholder={
                  form.prompt_filter_review_api_key_configured
                    ? t('promptFilter.reviewApiKeyConfigured', { n: form.prompt_filter_review_api_key_count })
                    : t('promptFilter.reviewApiKeyPlaceholder')
                }
                onChange={(event) => setForm((current) => ({ ...current, prompt_filter_review_api_key: event.target.value }))}
              />
              <span className="block text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewApiKeyHint')}</span>
            </Field>
          </div>

          <Button onClick={onSave} disabled={saving}>
            <Save className="size-4" />
            {saving ? t('common.saving') : t('common.save')}
          </Button>
        </CardContent>
      </Card>

      <Card className="mt-4">
        <CardContent>
          <div className="mb-4 flex items-center justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
            <SectionTitle title={t('promptFilter.recentLogsTitle')} />
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" asChild>
                <NavLink to="/prompt-filter/logs">{t('promptFilter.viewAllLogs')}</NavLink>
              </Button>
              <Button variant="outline" onClick={() => void clearLogs()} disabled={clearing || recentLogs.length === 0}>
                <Trash2 className="size-3.5" />
                {clearing ? t('promptFilter.clearing') : t('promptFilter.clearLogs')}
              </Button>
            </div>
          </div>
          <PromptFilterLogsTable logs={recentLogs} compact />
        </CardContent>
      </Card>
    </>
  )
}

function LogsView({ clearLogs, clearing }: { clearLogs: () => Promise<void>; clearing: boolean }) {
  const { t } = useTranslation()
  const [draftFilters, setDraftFilters] = useState<LogFilters>(emptyFilters)
  const [filters, setFilters] = useState<LogFilters>(emptyFilters)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = usePersistedPageSize('prompt_filter_logs', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [logs, setLogs] = useState<PromptFilterLog[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const loadLogs = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await api.getPromptFilterLogs({
        page,
        pageSize,
        action: filters.action,
        source: filters.source,
        endpoint: filters.endpoint,
        model: filters.model,
        apiKeyId: filters.apiKeyId,
        q: filters.q,
      })
      setLogs(result.logs ?? [])
      setTotal(result.total ?? 0)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [filters, page, pageSize])

  useEffect(() => {
    void loadLogs()
  }, [loadLogs])

  const applyFilters = () => {
    setPage(1)
    setFilters(draftFilters)
  }

  const resetFilters = () => {
    setDraftFilters(emptyFilters)
    setFilters(emptyFilters)
    setPage(1)
  }

  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  return (
    <Card>
      <CardContent>
        <div className="mb-4 flex items-center justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
          <SectionTitle title={t('promptFilter.logsTitle')} />
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" onClick={() => void loadLogs()} disabled={loading}>
              <RefreshCw className="size-3.5" />
              {t('common.refresh')}
            </Button>
            <Button variant="outline" onClick={() => void clearLogs().then(loadLogs)} disabled={clearing || logs.length === 0}>
              <Trash2 className="size-3.5" />
              {clearing ? t('promptFilter.clearing') : t('promptFilter.clearLogs')}
            </Button>
          </div>
        </div>

        <div className="mb-4 grid grid-cols-[repeat(auto-fit,minmax(160px,1fr))] gap-3">
          <Field label={t('promptFilter.colAction')}>
            <Select value={draftFilters.action} onValueChange={(value) => setDraftFilters((current) => ({ ...current, action: value }))} options={[{ label: t('common.all'), value: '' }, { label: t('promptFilter.modeBlock'), value: 'block' }, { label: t('promptFilter.modeWarn'), value: 'warn' }, { label: t('promptFilter.actionAllow'), value: 'allow' }]} />
          </Field>
          <Field label={t('promptFilter.source')}>
            <Select value={draftFilters.source} onValueChange={(value) => setDraftFilters((current) => ({ ...current, source: value }))} options={[{ label: t('common.all'), value: '' }, { label: 'local_filter', value: 'local_filter' }, { label: 'upstream_cyber_policy', value: 'upstream_cyber_policy' }]} />
          </Field>
          <Field label={t('promptFilter.endpoint')}>
            <Input value={draftFilters.endpoint} onChange={(event) => setDraftFilters((current) => ({ ...current, endpoint: event.target.value }))} placeholder="/v1/responses" />
          </Field>
          <Field label={t('promptFilter.model')}>
            <Input value={draftFilters.model} onChange={(event) => setDraftFilters((current) => ({ ...current, model: event.target.value }))} placeholder="gpt-5.5" />
          </Field>
          <Field label={t('promptFilter.apiKeyId')}>
            <Input value={draftFilters.apiKeyId} onChange={(event) => setDraftFilters((current) => ({ ...current, apiKeyId: event.target.value }))} placeholder="ID" />
          </Field>
          <Field label={t('promptFilter.keyword')}>
            <Input value={draftFilters.q} onChange={(event) => setDraftFilters((current) => ({ ...current, q: event.target.value }))} placeholder={t('promptFilter.keywordPlaceholder')} />
          </Field>
        </div>

        <div className="mb-4 flex flex-wrap gap-2">
          <Button onClick={applyFilters}>
            <Search className="size-4" />
            {t('promptFilter.applyFilters')}
          </Button>
          <Button variant="outline" onClick={resetFilters}>
            <X className="size-4" />
            {t('promptFilter.resetFilters')}
          </Button>
          <span className="self-center text-xs text-muted-foreground">{loading ? t('common.loading') : t('promptFilter.recordsCount', { count: total })}</span>
        </div>

        <StateShell loading={loading} error={error} isEmpty={!loading && logs.length === 0} onRetry={() => void loadLogs()} emptyTitle={t('promptFilter.noLogs')}>
          <PromptFilterLogsTable logs={logs} />
          <Pagination page={page} totalPages={totalPages} totalItems={total} pageSize={pageSize} onPageChange={setPage} onPageSizeChange={(next) => { setPage(1); setPageSize(next) }} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} />
        </StateShell>
      </CardContent>
    </Card>
  )
}

function RulesView({
  form,
  rules,
  saving,
  onRulesUpdated,
}: {
  form: PromptFilterForm
  rules: PromptFilterRulesResponse | null
  saving: boolean
  onRulesUpdated: (rules: PromptFilterRulesResponse, settings?: SystemSettings) => void
}) {
  const { t } = useTranslation()
  const [infoOpen, setInfoOpen] = useState(false)
  const [previewRule, setPreviewRule] = useState<PromptFilterRule | null>(null)
  const [previewPatternCopied, setPreviewPatternCopied] = useState(false)
  const [customDialogMode, setCustomDialogMode] = useState<'create' | 'edit' | null>(null)
  const [editingCustomIndex, setEditingCustomIndex] = useState<number | null>(null)
  const [customDialogDraft, setCustomDialogDraft] = useState<CustomRuleDraft>(defaultCustomRuleDraft)
  const [savingRule, setSavingRule] = useState('')
  const [categoryFilter, setCategoryFilter] = useState<string>('')
  const [selectedRules, setSelectedRules] = useState<Set<string>>(new Set())
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(10)

  const openRulePreview = (rule: PromptFilterRule) => {
    setPreviewRule(rule)
    setPreviewPatternCopied(false)
  }

  const copyPreviewPattern = async () => {
    if (!previewRule?.pattern) return
    try {
      await navigator.clipboard.writeText(previewRule.pattern)
      setPreviewPatternCopied(true)
      window.setTimeout(() => setPreviewPatternCopied(false), 1500)
    } catch {
      // ignore clipboard failures
    }
  }

  const disabled = useMemo(() => parseJSONList<string>(form.prompt_filter_disabled_patterns), [form.prompt_filter_disabled_patterns])
  const customPatterns = rules?.custom_patterns ?? parseJSONList<PromptFilterRule>(form.prompt_filter_custom_patterns)

  const allCategories = useMemo(() => {
    const cats = new Set<string>()
    ;(rules?.builtin_patterns ?? []).forEach((rule) => rule.category && cats.add(rule.category))
    return Array.from(cats).sort()
  }, [rules?.builtin_patterns])

  const filteredBuiltinRules = useMemo(() => {
    const builtins = rules?.builtin_patterns ?? []
    if (!categoryFilter) return builtins
    return builtins.filter((rule) => rule.category === categoryFilter)
  }, [rules?.builtin_patterns, categoryFilter])

  const paginatedRules = useMemo(() => {
    const start = (page - 1) * pageSize
    return filteredBuiltinRules.slice(start, start + pageSize)
  }, [filteredBuiltinRules, page, pageSize])

  const totalPages = Math.max(1, Math.ceil(filteredBuiltinRules.length / pageSize))

  const toggleSelectAll = () => {
    if (selectedRules.size === paginatedRules.length) {
      setSelectedRules(new Set())
    } else {
      setSelectedRules(new Set(paginatedRules.map((rule) => rule.name)))
    }
  }

  const toggleSelectRule = (ruleName: string) => {
    const next = new Set(selectedRules)
    if (next.has(ruleName)) {
      next.delete(ruleName)
    } else {
      next.add(ruleName)
    }
    setSelectedRules(next)
  }

  const batchToggleRules = async (enable: boolean) => {
    if (selectedRules.size === 0) return
    const current = new Set(disabled.map((name) => name.toLowerCase()))
    selectedRules.forEach((ruleName) => {
      if (enable) {
        current.delete(ruleName.toLowerCase())
      } else {
        current.add(ruleName.toLowerCase())
      }
    })
    const names = (rules?.builtin_patterns ?? [])
      .map((item) => item.name)
      .filter((name) => current.has(name.toLowerCase()))
    await savePartialAndReload({ prompt_filter_disabled_patterns: JSON.stringify(names) })
    setSelectedRules(new Set())
  }

  const savePartialAndReload = async (partial: Partial<SystemSettings>) => {
    setSavingRule('rules')
    try {
      const updated = await api.updateSettings(partial)
      const nextRules = await api.getPromptFilterRules()
      onRulesUpdated(nextRules, updated)
    } finally {
      setSavingRule('')
    }
  }

  const toggleBuiltin = async (rule: PromptFilterRule) => {
    const current = new Set(disabled.map((name) => name.toLowerCase()))
    if (rule.enabled) {
      current.add(rule.name.toLowerCase())
    } else {
      current.delete(rule.name.toLowerCase())
    }
    const names = (rules?.builtin_patterns ?? [])
      .map((item) => item.name)
      .filter((name) => current.has(name.toLowerCase()))
    await savePartialAndReload({ prompt_filter_disabled_patterns: JSON.stringify(names) })
  }

  const saveCustomPatterns = async (next: PromptFilterRule[]) => {
    await savePartialAndReload({ prompt_filter_custom_patterns: JSON.stringify(next) })
  }

  const startCreateCustomRule = () => {
    setCustomDialogMode('create')
    setEditingCustomIndex(null)
    setCustomDialogDraft(defaultCustomRuleDraft)
  }

  const startEditCustomRule = (index: number) => {
    const rule = customPatterns[index]
    if (!rule) return
    setCustomDialogMode('edit')
    setEditingCustomIndex(index)
    setCustomDialogDraft(customRuleDraftFromRule(rule))
  }

  const closeCustomRuleDialog = () => {
    setCustomDialogMode(null)
    setEditingCustomIndex(null)
    setCustomDialogDraft(defaultCustomRuleDraft)
  }

  const saveCustomRuleDialog = async () => {
    const name = customDialogDraft.name.trim()
    const pattern = customDialogDraft.pattern
    const weight = parseRuleWeight(customDialogDraft.weight)
    if (!name || !pattern.trim() || weight === null) return

    if (customDialogMode === 'create') {
      await saveCustomPatterns([
        ...customPatterns,
        {
          name,
          pattern,
          weight,
          category: customDialogDraft.category.trim() || 'custom',
          strict: customDialogDraft.strict,
          enabled: true,
        },
      ])
      closeCustomRuleDialog()
      return
    }

    if (customDialogMode === 'edit' && editingCustomIndex !== null) {
      const existing = customPatterns[editingCustomIndex]
      if (!existing) {
        closeCustomRuleDialog()
        return
      }
      const next = customPatterns.map((rule, index) => index === editingCustomIndex ? {
        ...rule,
        name,
        pattern,
        weight,
        category: customDialogDraft.category.trim() || 'custom',
        strict: customDialogDraft.strict,
        enabled: rule.enabled !== false,
      } : rule)
      await saveCustomPatterns(next)
      closeCustomRuleDialog()
    }
  }

  const toggleCustom = async (index: number) => {
    const next = customPatterns.map((rule, i) => i === index ? { ...rule, enabled: rule.enabled === false } : rule)
    await saveCustomPatterns(next)
  }

  const deleteCustom = async (index: number) => {
    await saveCustomPatterns(customPatterns.filter((_, i) => i !== index))
  }

  return (
    <>
      <Card>
        <CardContent>
          <div className="mb-4 flex items-center justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
            <div>
              <SectionTitle title={t('promptFilter.rulesCatalogTitle')} />
              <p className="mt-1 text-sm text-muted-foreground">{t('promptFilter.rulesCatalogDesc')}</p>
            </div>
            <Button variant="outline" onClick={() => setInfoOpen(true)}>
              <HelpCircle className="size-4" />
              {t('promptFilter.ruleHelp')}
            </Button>
          </div>

          <div className="mb-4 flex flex-wrap items-center gap-3">
            <div className="min-w-[240px]">
              <Field label={t('promptFilter.filterByCategory')}>
                <Select
                  value={categoryFilter}
                  onValueChange={(value) => {
                    setCategoryFilter(value)
                    setPage(1)
                    setSelectedRules(new Set())
                  }}
                  options={[
                    { label: t('common.all'), value: '' },
                    ...allCategories.map((cat) => ({ label: cat, value: cat }))
                  ]}
                />
              </Field>
            </div>
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="outline" onClick={toggleSelectAll}>
                {selectedRules.size === paginatedRules.length && paginatedRules.length > 0 ? t('promptFilter.deselectAll') : t('promptFilter.selectAll')}
              </Button>
              <Button size="sm" variant="default" onClick={() => void batchToggleRules(true)} disabled={selectedRules.size === 0 || savingRule !== ''}>
                {t('promptFilter.batchEnable')} ({selectedRules.size})
              </Button>
              <Button size="sm" variant="destructive" onClick={() => void batchToggleRules(false)} disabled={selectedRules.size === 0 || savingRule !== ''}>
                {t('promptFilter.batchDisable')} ({selectedRules.size})
              </Button>
            </div>
          </div>

          <div className="rounded-lg border border-border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-12">
                    <input
                      type="checkbox"
                      checked={selectedRules.size === paginatedRules.length && paginatedRules.length > 0}
                      onChange={toggleSelectAll}
                      className="size-4 cursor-pointer"
                    />
                  </TableHead>
                  <TableHead>{t('promptFilter.ruleName')}</TableHead>
                  <TableHead>{t('promptFilter.ruleCategory')}</TableHead>
                  <TableHead>{t('promptFilter.ruleWeight')}</TableHead>
                  <TableHead>{t('promptFilter.rulePattern')}</TableHead>
                  <TableHead>{t('common.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {paginatedRules.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={6} className="h-20 text-center text-muted-foreground">{t('promptFilter.noRulesInCategory')}</TableCell>
                  </TableRow>
                ) : paginatedRules.map((rule) => (
                  <RuleRow
                    key={rule.name}
                    rule={rule}
                    selected={selectedRules.has(rule.name)}
                    onSelect={() => toggleSelectRule(rule.name)}
                    onPreview={() => openRulePreview(rule)}
                    onToggle={() => void toggleBuiltin(rule)}
                    busy={saving || savingRule !== ''}
                  />
                ))}
              </TableBody>
            </Table>
          </div>

          <Pagination
            page={page}
            totalPages={totalPages}
            totalItems={filteredBuiltinRules.length}
            pageSize={pageSize}
            onPageChange={setPage}
            onPageSizeChange={(next) => {
              setPage(1)
              setPageSize(next)
              setSelectedRules(new Set())
            }}
            pageSizeOptions={[10, 20, 50, 100]}
          />
        </CardContent>
      </Card>

      <Card className="mt-4">
        <CardContent>
          <div className="mb-4 flex items-center justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
            <div>
              <SectionTitle title={t('promptFilter.customRulesTitle')} />
              <p className="mt-1 text-sm text-muted-foreground">{t('promptFilter.customRulesDesc')}</p>
            </div>
            <Button onClick={startCreateCustomRule} disabled={savingRule !== ''}>
              <Plus className="size-4" />
              {t('promptFilter.addCustomRule')}
            </Button>
          </div>

          <div className="rounded-lg border border-border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('promptFilter.ruleName')}</TableHead>
                  <TableHead>{t('promptFilter.ruleCategory')}</TableHead>
                  <TableHead>{t('promptFilter.ruleWeight')}</TableHead>
                  <TableHead>{t('promptFilter.rulePattern')}</TableHead>
                  <TableHead>{t('common.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {customPatterns.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={5} className="h-20 text-center text-muted-foreground">{t('promptFilter.noCustomRules')}</TableCell>
                  </TableRow>
                ) : customPatterns.map((rule, index) => (
                  <RuleRow
                    key={`${rule.name}-${index}`}
                    rule={{ ...rule, builtin: false, enabled: rule.enabled !== false }}
                    onPreview={() => openRulePreview({ ...rule, builtin: false, enabled: rule.enabled !== false })}
                    onToggle={() => void toggleCustom(index)}
                    onEdit={() => startEditCustomRule(index)}
                    onDelete={() => void deleteCustom(index)}
                    iconActions
                    busy={savingRule !== ''}
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>

      <Dialog open={customDialogMode !== null} onOpenChange={(open) => { if (!open) closeCustomRuleDialog() }}>
        <DialogContent className="max-h-[calc(100vh-2rem)] max-w-2xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{customDialogMode === 'create' ? t('promptFilter.addCustomRule') : t('promptFilter.editCustomRule')}</DialogTitle>
            <DialogDescription>{customDialogMode === 'create' ? t('promptFilter.addCustomRuleDesc') : t('promptFilter.editCustomRuleDesc')}</DialogDescription>
          </DialogHeader>
          <div className="grid gap-3 sm:grid-cols-[minmax(160px,0.8fr)_minmax(0,1.2fr)]">
            <Field label={t('promptFilter.ruleName')}>
              <Input value={customDialogDraft.name} onChange={(event) => setCustomDialogDraft((current) => ({ ...current, name: event.target.value }))} placeholder="custom_rule" />
            </Field>
            <Field label={t('promptFilter.ruleCategory')}>
              <Input value={customDialogDraft.category} onChange={(event) => setCustomDialogDraft((current) => ({ ...current, category: event.target.value }))} />
            </Field>
          </div>
          <Field label={t('promptFilter.rulePattern')}>
            <Textarea rows={5} value={customDialogDraft.pattern} onChange={(event) => setCustomDialogDraft((current) => ({ ...current, pattern: event.target.value }))} placeholder="(?i)dangerous phrase" />
          </Field>
          <RulePatternTester pattern={customDialogDraft.pattern} />
          <div className="grid gap-3 sm:grid-cols-[minmax(120px,0.8fr)_minmax(140px,0.8fr)]">
            <Field label={t('promptFilter.ruleWeight')}>
              <Input type="number" min={1} max={1000} value={customDialogDraft.weight} onChange={(event) => setCustomDialogDraft((current) => ({ ...current, weight: event.target.value }))} />
            </Field>
            <Field label={t('promptFilter.ruleStrict')}>
              <Select value={customDialogDraft.strict ? 'true' : 'false'} onValueChange={(value) => setCustomDialogDraft((current) => ({ ...current, strict: value === 'true' }))} triggerClassName="h-9 rounded-md px-3 text-sm" options={[{ label: t('common.enabled'), value: 'true' }, { label: t('common.disabled'), value: 'false' }]} />
            </Field>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={closeCustomRuleDialog} disabled={savingRule !== ''}>{t('common.cancel')}</Button>
            <Button onClick={() => void saveCustomRuleDialog()} disabled={savingRule !== '' || !customDialogDraft.name.trim() || !customDialogDraft.pattern.trim() || parseRuleWeight(customDialogDraft.weight) === null}>
              <Save className="size-4" />
              {savingRule !== '' ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={infoOpen} onOpenChange={setInfoOpen}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t('promptFilter.ruleHelpTitle')}</DialogTitle>
            <DialogDescription>{t('promptFilter.ruleHelpDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3 text-sm leading-relaxed text-muted-foreground">
            <p>{t('promptFilter.ruleHelpBody1')}</p>
            <pre className="max-h-64 overflow-auto rounded-lg bg-muted/50 p-3 text-xs text-foreground">{`{
  "name": "custom_reverse_shell",
  "pattern": "(?i)reverse\\\\s+shell",
  "weight": 60,
  "category": "remote_access",
  "strict": true,
  "enabled": true
}`}</pre>
            <p>{t('promptFilter.ruleHelpBody2')}</p>
          </div>
          <DialogFooter>
            <Button onClick={() => setInfoOpen(false)}>{t('common.confirm')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={previewRule !== null}
        onOpenChange={(open) => {
          if (!open) {
            setPreviewRule(null)
            setPreviewPatternCopied(false)
          }
        }}
      >
        <DialogContent className="max-h-[calc(100vh-2rem)] max-w-3xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle className="font-mono text-base">{previewRule?.name}</DialogTitle>
            <DialogDescription>{t('promptFilter.rulePreviewDesc')}</DialogDescription>
          </DialogHeader>

          {previewRule ? (
            <div className="space-y-4">
              <div className="flex flex-wrap items-center gap-2">
                {previewRule.builtin ? (
                  <Badge variant="secondary">{t('promptFilter.builtinRule')}</Badge>
                ) : (
                  <Badge variant="outline">{t('promptFilter.customRule')}</Badge>
                )}
                {previewRule.strict ? <Badge variant="destructive">{t('promptFilter.ruleStrict')}</Badge> : null}
                <Badge variant={previewRule.enabled !== false ? 'default' : 'outline'}>
                  {previewRule.enabled !== false ? t('common.enabled') : t('common.disabled')}
                </Badge>
                <Badge variant="outline" className="font-mono">
                  {t('promptFilter.ruleWeight')}: {previewRule.weight}
                </Badge>
                <Badge variant="outline" className="font-mono">
                  {t('promptFilter.ruleCategory')}: {previewRule.category || '-'}
                </Badge>
              </div>

              <div className="space-y-2">
                <div className="flex items-center justify-between gap-2">
                  <span className="text-sm font-semibold text-foreground">{t('promptFilter.rulePattern')}</span>
                  <Button type="button" size="sm" variant="outline" onClick={() => void copyPreviewPattern()}>
                    <Copy className="size-3.5" />
                    {previewPatternCopied ? t('promptFilter.patternCopied') : t('promptFilter.copyPattern')}
                  </Button>
                </div>
                <pre className="max-h-[min(40vh,360px)] overflow-auto whitespace-pre-wrap break-all rounded-lg border border-foreground/12 bg-muted/40 p-3 font-mono text-xs leading-6 text-foreground">
                  {previewRule.pattern || '-'}
                </pre>
              </div>

              <RulePatternTester pattern={previewRule.pattern || ''} />
            </div>
          ) : null}

          <DialogFooter>
            <Button variant="outline" onClick={() => setPreviewRule(null)}>{t('common.close')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

function RulePatternTester({ pattern, className }: { pattern: string; className?: string }) {
  const { t } = useTranslation()
  const [state, setState] = useState<RulePatternTestState>(defaultRulePatternTestState)
  const requestIdRef = useRef(0)

  useEffect(() => {
    requestIdRef.current += 1
    setState((current) => ({ ...current, result: null, message: '' }))
  }, [pattern])

  const runPatternTest = async () => {
    const text = state.text
    if (!pattern.trim()) {
      setState((current) => ({ ...current, result: 'invalid', message: t('promptFilter.rulePatternRequired') }))
      return
    }
    if (!text.trim()) {
      setState((current) => ({ ...current, result: 'invalid', message: t('promptFilter.ruleTestTextRequired') }))
      return
    }
    const requestId = requestIdRef.current + 1
    requestIdRef.current = requestId
    setState((current) => ({ ...current, testing: true, result: null, message: '' }))
    try {
      const result = await api.testPromptFilterRulePattern({ pattern, text })
      if (requestIdRef.current !== requestId) return
      if (result.error) {
        setState((current) => ({ ...current, testing: false, result: 'invalid', message: result.error || t('promptFilter.rulePatternInvalid') }))
      } else if (result.matched) {
        setState((current) => ({ ...current, testing: false, result: 'matched', message: t('promptFilter.ruleTestMatched') }))
      } else {
        setState((current) => ({ ...current, testing: false, result: 'not_matched', message: t('promptFilter.ruleTestNotMatched') }))
      }
    } catch (err) {
      if (requestIdRef.current !== requestId) return
      setState((current) => ({ ...current, testing: false, result: 'invalid', message: getErrorMessage(err) }))
    }
  }

  const resultClass = state.result === 'matched'
    ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300'
    : state.result === 'not_matched'
      ? 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300'
      : 'border-destructive/30 bg-destructive/10 text-destructive'

  return (
    <div className={cn('rounded-lg border border-border bg-muted/20 p-3', className)}>
      <div className="mb-3 flex items-center justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
        <div>
          <div className="text-sm font-semibold text-foreground">{t('promptFilter.rulePatternTesterTitle')}</div>
          <p className="mt-1 text-xs text-muted-foreground">{t('promptFilter.rulePatternTesterDesc')}</p>
        </div>
        <Button size="sm" variant="outline" onClick={() => void runPatternTest()} disabled={state.testing || !pattern.trim() || !state.text.trim()}>
          <Search className="size-3.5" />
          {state.testing ? t('promptFilter.rulePatternTesting') : t('promptFilter.rulePatternTest')}
        </Button>
      </div>
      <Textarea
        rows={3}
        value={state.text}
        placeholder={t('promptFilter.ruleTestTextPlaceholder')}
        onChange={(event) => {
          requestIdRef.current += 1
          setState((current) => ({ ...current, text: event.target.value, result: null, message: '' }))
        }}
      />
      {state.result && state.message ? (
        <div className={cn('mt-3 rounded-md border px-3 py-2 text-sm font-medium', resultClass)}>{state.message}</div>
      ) : null}
    </div>
  )
}

function RuleRow({
  rule,
  selected,
  onSelect,
  onPreview,
  onToggle,
  onEdit,
  onDelete,
  iconActions = false,
  busy,
}: {
  rule: PromptFilterRule
  selected?: boolean
  onSelect?: () => void
  onPreview?: () => void
  onToggle: () => void
  onEdit?: () => void
  onDelete?: () => void
  iconActions?: boolean
  busy?: boolean
}) {
  const { t } = useTranslation()
  const enabled = rule.enabled !== false
  return (
    <TableRow
      className={onPreview ? 'cursor-pointer hover:bg-muted/30' : undefined}
      onClick={onPreview}
    >
      {onSelect !== undefined ? (
        <TableCell onClick={(event) => event.stopPropagation()}>
          <input
            type="checkbox"
            checked={selected}
            onChange={onSelect}
            className="size-4 cursor-pointer"
          />
        </TableCell>
      ) : null}
      <TableCell>
        <button
          type="button"
          className="text-left"
          onClick={(event) => {
            if (!onPreview) return
            event.stopPropagation()
            onPreview()
          }}
        >
          <div className="font-mono text-xs font-semibold text-foreground transition-colors hover:text-primary">
            {rule.name}
          </div>
        </button>
        <div className="mt-1 flex gap-1">
          {rule.builtin ? <Badge variant="secondary">{t('promptFilter.builtinRule')}</Badge> : <Badge variant="outline">{t('promptFilter.customRule')}</Badge>}
          {rule.strict ? <Badge variant="destructive">{t('promptFilter.ruleStrict')}</Badge> : null}
          <Badge variant={enabled ? 'default' : 'outline'}>{enabled ? t('common.enabled') : t('common.disabled')}</Badge>
        </div>
      </TableCell>
      <TableCell>{rule.category || '-'}</TableCell>
      <TableCell className="font-mono text-sm">{rule.weight}</TableCell>
      <TableCell className="max-w-[520px]">
        <code
          className={cn(
            'line-clamp-2 whitespace-normal break-all rounded bg-muted/60 px-2 py-1 text-xs text-muted-foreground',
            onPreview && 'transition-colors hover:bg-primary/10 hover:text-foreground',
          )}
          title={onPreview ? t('promptFilter.rulePreviewHint') : undefined}
        >
          {rule.pattern}
        </code>
      </TableCell>
      <TableCell onClick={(event) => event.stopPropagation()}>
        <div className="flex flex-wrap gap-2">
          {iconActions ? (
            <Button size="icon-sm" variant="ghost" onClick={onToggle} disabled={busy} aria-label={enabled ? t('promptFilter.disableRule') : t('promptFilter.enableRule')} title={enabled ? t('promptFilter.disableRule') : t('promptFilter.enableRule')}>
              {enabled ? <PowerOff className="size-3.5" /> : <Power className="size-3.5" />}
            </Button>
          ) : (
            <Button size="sm" variant="outline" onClick={onToggle} disabled={busy}>
              {enabled ? t('promptFilter.disableRule') : t('promptFilter.enableRule')}
            </Button>
          )}
          {onEdit ? (
            <Button size="icon-sm" variant="ghost" onClick={onEdit} disabled={busy} aria-label={t('promptFilter.editCustomRule')} title={t('promptFilter.editCustomRule')}>
              <Pencil className="size-3.5" />
            </Button>
          ) : null}
          {onDelete ? (
            <Button size="icon-sm" variant="ghost" onClick={onDelete} disabled={busy} aria-label={t('common.delete')} title={t('common.delete')}>
              <Trash2 className="size-3.5" />
            </Button>
          ) : null}
        </div>
      </TableCell>
    </TableRow>
  )
}

function PromptFilterLogsTable({ logs, compact = false }: { logs: PromptFilterLog[]; compact?: boolean }) {
  const { t } = useTranslation()
  return (
    <div className="rounded-lg border border-border">
      <Table className="table-fixed">
        <TableHeader>
          <TableRow>
            <TableHead className={compact ? 'w-[92px]' : 'w-[150px]'}>{t('promptFilter.colTime')}</TableHead>
            <TableHead className={compact ? 'w-[82px]' : 'w-[96px]'}>{t('promptFilter.colAction')}</TableHead>
            <TableHead className={compact ? 'w-[150px]' : 'w-[180px]'}>{t('promptFilter.colEndpoint')}</TableHead>
            <TableHead className={compact ? 'w-[72px]' : 'w-[88px]'}>{t('promptFilter.colScore')}</TableHead>
            <TableHead className={compact ? 'w-[150px]' : 'w-[220px]'}>{t('promptFilter.colMatch')}</TableHead>
            <TableHead className={compact ? 'w-[118px]' : 'w-[160px]'}>{t('promptFilter.colApiKey')}</TableHead>
            <TableHead>{t('promptFilter.colPreview')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {logs.length === 0 ? (
            <TableRow>
              <TableCell colSpan={7} className="h-24 text-center text-muted-foreground">{t('promptFilter.noLogs')}</TableCell>
            </TableRow>
          ) : logs.map((log) => <PromptFilterLogRow key={log.id} log={log} compact={compact} />)}
        </TableBody>
      </Table>
    </div>
  )
}

function MetricTile({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-h-[76px] flex-col justify-between gap-2 rounded-lg border border-border bg-card p-3 shadow-sm">
      <span className="text-[11px] font-bold uppercase text-muted-foreground">{label}</span>
      <div className="text-sm font-semibold text-foreground">{children}</div>
    </div>
  )
}

function SectionTitle({ title }: { title: string }) {
  return <h3 className="text-base font-semibold leading-tight text-foreground">{title}</h3>
}

function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="block min-w-0 space-y-2">
      <span className="flex items-center gap-1.5 text-sm font-semibold leading-none text-foreground">
        {label}
        {hint ? <TooltipProvider delayDuration={150}><Tooltip><TooltipTrigger asChild><button type="button" aria-label={`${label} help`} className="text-muted-foreground hover:text-primary" onClick={(event) => event.preventDefault()}><HelpCircle className="size-3.5" /></button></TooltipTrigger><TooltipContent className="max-w-[320px] whitespace-normal leading-relaxed">{hint}</TooltipContent></Tooltip></TooltipProvider> : null}
      </span>
      {children}
    </label>
  )
}

function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return (
    <textarea
      className={cn(
        'w-full min-w-0 resize-y rounded-md border border-input bg-transparent px-3 py-2 text-sm leading-5 shadow-xs outline-none transition-[color,box-shadow] placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:pointer-events-none disabled:opacity-50 dark:bg-input/30',
        className
      )}
      {...props}
    />
  )
}

function VerdictBadge({ verdict }: { verdict: PromptFilterVerdict }) {
  const { t } = useTranslation()
  const action = verdict.action
  if (action === 'block') {
    return (
      <Badge variant="destructive" className="gap-1.5">
        <ShieldAlert className="size-3" />
        {t('promptFilter.modeBlock')}
      </Badge>
    )
  }
  if (action === 'warn') {
    return (
      <Badge variant="outline" className="gap-1.5 border-amber-500/30 text-amber-700 dark:text-amber-300">
        <AlertTriangle className="size-3" />
        {t('promptFilter.modeWarn')}
      </Badge>
    )
  }
  return (
    <Badge variant="outline" className="gap-1.5 border-emerald-500/30 text-emerald-700 dark:text-emerald-300">
      <CheckCircle2 className="size-3" />
      {t('promptFilter.actionAllow')}
    </Badge>
  )
}

function VerdictPanel({ verdict }: { verdict: PromptFilterVerdict }) {
  const { t } = useTranslation()
  const localizedMode = verdict.mode === 'block'
    ? t('promptFilter.modeBlock')
    : verdict.mode === 'warn'
      ? t('promptFilter.modeWarn')
      : verdict.mode === 'monitor'
        ? t('promptFilter.modeMonitor')
        : (verdict.mode || '-')
  const localizedReview = verdict.reviewed
    ? (verdict.review_flagged ? t('promptFilter.testReviewFlagged') : t('promptFilter.testReviewCleared'))
    : t('promptFilter.testReviewSkipped')
  return (
    <div className="rounded-lg border border-border bg-muted/25 p-3">
      <div className="grid grid-cols-[repeat(auto-fit,minmax(120px,1fr))] gap-2 text-sm">
        <MiniStat label={t('promptFilter.testResultMode')} value={localizedMode} />
        <MiniStat label={t('promptFilter.testResultScore')} value={`${verdict.score} / ${verdict.threshold}`} />
        <MiniStat label={t('promptFilter.testResultMatches')} value={String(verdict.matched?.length ?? 0)} />
        <MiniStat label={t('promptFilter.testResultReview')} value={localizedReview} />
      </div>
      {verdict.reason ? <p className="mt-3 text-sm text-muted-foreground">{verdict.reason}</p> : null}
      {verdict.review_error ? <p className="mt-2 text-sm text-destructive">{verdict.review_error}</p> : null}
      {verdict.matched?.length ? (
        <div className="mt-3 flex flex-wrap gap-1.5">
          {verdict.matched.map((match, index) => (
            <Badge key={`${match.name}-${index}`} variant="outline">
              {match.name} · {match.weight}
            </Badge>
          ))}
        </div>
      ) : null}
      {verdict.text_preview ? (
        <pre className="mt-3 max-h-28 overflow-auto rounded-md bg-background p-2 text-xs leading-5 text-muted-foreground"><HighlightedPromptPreview text={verdict.text_preview} /></pre>
      ) : null}
    </div>
  )
}

function HighlightedPromptPreview({ text, className }: { text: string; className?: string }) {
  const parts = parseHitMarkedText(text)
  return <HighlightedParts parts={parts} className={className} />
}

function HighlightedPromptText({ text, terms, className }: { text: string; terms: string[]; className?: string }) {
  return <HighlightedParts parts={splitTextByHitTerms(text, terms)} className={className} />
}

function HighlightedParts({ parts, className }: { parts: Array<{ text: string; hit: boolean }>; className?: string }) {
  return (
    <span className={className}>
      {parts.map((part, index) => part.hit ? (
        <mark key={index} className="rounded bg-amber-200 px-1 py-0.5 font-medium text-amber-950 dark:bg-amber-400/25 dark:text-amber-100">
          {part.text}
        </mark>
      ) : <span key={index}>{part.text}</span>)}
    </span>
  )
}

function parseHitMarkedText(text: string): Array<{ text: string; hit: boolean }> {
  const parts: Array<{ text: string; hit: boolean }> = []
  let cursor = 0
  while (cursor < text.length) {
    const start = text.indexOf(HIT_START_MARKER, cursor)
    if (start < 0) {
      parts.push({ text: text.slice(cursor), hit: false })
      break
    }
    const end = text.indexOf(HIT_END_MARKER, start + HIT_START_MARKER.length)
    if (end < 0) {
      parts.push({ text: text.slice(cursor), hit: false })
      break
    }
    if (start > cursor) {
      parts.push({ text: text.slice(cursor, start), hit: false })
    }
    parts.push({ text: text.slice(start + HIT_START_MARKER.length, end), hit: true })
    cursor = end + HIT_END_MARKER.length
  }
  return parts.length ? parts : [{ text, hit: false }]
}

function extractHitTerms(text: string): string[] {
  const terms: string[] = []
  for (const part of parseHitMarkedText(text)) {
    const term = stripHitMarkers(part.text).trim()
    if (part.hit && term && !terms.some((existing) => existing.toLowerCase() === term.toLowerCase())) {
      terms.push(term)
    }
  }
  return terms
}

function splitTextByHitTerms(text: string, terms: string[]): Array<{ text: string; hit: boolean }> {
  const normalizedTerms = terms
    .map((term) => term.trim())
    .filter((term) => term.length > 0)
    .sort((a, b) => b.length - a.length)
  if (text === '' || normalizedTerms.length === 0) {
    return [{ text, hit: false }]
  }

  const lowerText = text.toLowerCase()
  const parts: Array<{ text: string; hit: boolean }> = []
  let cursor = 0
  while (cursor < text.length) {
    let bestIndex = -1
    let bestTerm = ''
    for (const term of normalizedTerms) {
      const index = lowerText.indexOf(term.toLowerCase(), cursor)
      if (index >= 0 && (bestIndex < 0 || index < bestIndex || (index === bestIndex && term.length > bestTerm.length))) {
        bestIndex = index
        bestTerm = term
      }
    }
    if (bestIndex < 0) {
      parts.push({ text: text.slice(cursor), hit: false })
      break
    }
    if (bestIndex > cursor) {
      parts.push({ text: text.slice(cursor, bestIndex), hit: false })
    }
    parts.push({ text: text.slice(bestIndex, bestIndex + bestTerm.length), hit: true })
    cursor = bestIndex + bestTerm.length
  }
  return parts.length ? parts : [{ text, hit: false }]
}

function stripHitMarkers(text: string): string {
  return text.split(HIT_START_MARKER).join('').split(HIT_END_MARKER).join('')
}

function MiniStat({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border border-border bg-background px-3 py-2">
      <div className="text-[11px] font-bold uppercase text-muted-foreground">{label}</div>
      <div className="mt-1 font-semibold text-foreground">{value}</div>
    </div>
  )
}

function PromptFilterLogRow({ log, compact }: { log: PromptFilterLog; compact?: boolean }) {
  const { t } = useTranslation()
  const matches = parseLogMatches(log.matched_patterns)
  const [expanded, setExpanded] = useState(false)
  const fullText = (log.full_text || '').trim()
  const hasFull = fullText.length > 0
  const matchContext = (log.match_context || '').trim()
  const userPrompt = (log.text_preview || '').trim()
  const primaryOriginLabel = log.primary_origin
    ? t(`promptFilter.origins.${log.primary_origin}`, { defaultValue: log.primary_origin })
    : t('promptFilter.origins.unknown')
  const hitTerms = extractHitTerms(matchContext || userPrompt)
  const fallbackPreview = log.error_code || log.review_error || ''
  const auxiliaryOrigin = Boolean(log.primary_origin && log.primary_origin !== 'current_user')
  const userPromptLabel = !matchContext && auxiliaryOrigin
    ? t('promptFilter.legacyRequestPreviewLabel')
    : t('promptFilter.userPromptLabel')
  const legacyMissingMatchContext = !matchContext && !userPrompt && !fullText &&
    auxiliaryOrigin
  return (
    <>
    <TableRow>
      <TableCell className={compact ? 'w-[92px] min-w-0' : 'w-[150px] min-w-0'}>
        <div className="font-medium text-foreground">{formatRelativeTime(log.created_at, { variant: 'compact' })}</div>
        {!compact ? <div className="text-xs text-muted-foreground">{formatBeijingTime(log.created_at)}</div> : null}
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <ActionBadge action={log.action} />
          {log.policy_profile ? <Badge variant="outline" className="text-[11px]">{log.policy_profile}</Badge> : null}
          {log.primary_origin ? (
            <Badge
              variant="secondary"
              className="text-[11px]"
              title={`${t('promptFilter.triggerOrigin')}: ${primaryOriginLabel}`}
            >
              {primaryOriginLabel}
            </Badge>
          ) : null}
          {log.strike_eligible ? <Badge variant="destructive" className="text-[11px]">strike</Badge> : null}
          {log.source === 'upstream_cyber_policy' ? <Badge variant="outline" className="text-[11px]">upstream</Badge> : null}
          {log.review_model ? <Badge variant="outline" className="text-[11px]">{log.review_flagged ? 'review flagged' : 'review cleared'}</Badge> : null}
        </div>
      </TableCell>
      <TableCell>
        <div className="truncate font-mono text-xs text-foreground">{log.endpoint || '-'}</div>
        <div className="truncate font-mono text-xs text-muted-foreground">{log.model || '-'}</div>
        {log.protocol || log.provider ? (
          <div className="mt-1 flex flex-wrap gap-1">
            {log.protocol ? <Badge variant="outline" className="text-[10px]">{log.protocol}</Badge> : null}
            {log.provider ? <Badge variant="secondary" className="text-[10px]">{log.provider}</Badge> : null}
          </div>
        ) : null}
      </TableCell>
      <TableCell>
        <div>
          <span className="font-semibold">{log.score}</span>
          <span className="text-muted-foreground"> / {log.threshold}</span>
        </div>
        {(log.audit_score ?? 0) > log.score ? (
          <div className="mt-0.5 text-[11px] text-muted-foreground">{t('promptFilter.auditScore')}: {log.audit_score}</div>
        ) : null}
      </TableCell>
      <TableCell className={compact ? 'w-[150px] min-w-0' : 'w-[220px] min-w-0'}>
        {matches.length ? (
          <div className="flex flex-wrap gap-1">
            {matches.slice(0, 3).map((match, index) => <Badge key={`${match.name}-${index}`} variant="outline">{match.name}</Badge>)}
            {matches.length > 3 ? <Badge variant="secondary">+{matches.length - 3}</Badge> : null}
          </div>
        ) : <span className="text-muted-foreground">-</span>}
      </TableCell>
      <TableCell>
        <div className={compact ? 'max-w-[110px] truncate' : 'max-w-[160px] truncate'}>{log.api_key_name || log.api_key_masked || '-'}</div>
        {!compact && log.client_ip ? <div className="text-xs text-muted-foreground">{log.client_ip}</div> : null}
      </TableCell>
      <TableCell className="min-w-0">
        <div className="space-y-1.5">
          {matchContext ? (
            <div className="min-w-0 rounded-md border border-amber-500/20 bg-amber-500/[0.06] px-2 py-1.5">
              <div className="mb-0.5 flex min-w-0 items-center gap-1 text-[10px] font-semibold text-amber-700 dark:text-amber-300">
                <span className="shrink-0">{t('promptFilter.matchContextLabel')}</span>
                <span aria-hidden="true" className="text-amber-600/60 dark:text-amber-300/60">·</span>
                <span className="truncate" title={`${t('promptFilter.triggerOrigin')}: ${primaryOriginLabel}`}>{primaryOriginLabel}</span>
              </div>
              <div
                className={cn('break-words text-xs leading-5 text-foreground', compact ? 'line-clamp-2' : 'line-clamp-3')}
                title={stripHitMarkers(matchContext)}
              >
                <HighlightedPromptPreview text={matchContext} />
              </div>
            </div>
          ) : null}
          {userPrompt ? (
            <div className="min-w-0 px-0.5">
              <div className="mb-0.5 text-[10px] font-semibold text-muted-foreground">{userPromptLabel}</div>
              <div className="truncate text-xs leading-5 text-muted-foreground" title={stripHitMarkers(userPrompt)}>
                <HighlightedPromptPreview text={userPrompt} />
              </div>
            </div>
          ) : null}
          {!matchContext && !userPrompt ? (
            legacyMissingMatchContext ? (
              <div className="rounded-md border border-dashed border-border bg-muted/30 px-2 py-1.5 text-xs leading-5 text-muted-foreground">
                {t('promptFilter.legacyMissingMatchContext')}
              </div>
            ) : (
              <div className="truncate text-muted-foreground" title={fallbackPreview}>{fallbackPreview || '-'}</div>
            )
          ) : null}
        </div>
        {hasFull ? (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="mt-1 inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
          >
            <ChevronDown className={`size-3 transition-transform ${expanded ? 'rotate-180' : ''}`} />
            {expanded ? t('promptFilter.collapseFullText') : t('promptFilter.viewFullText')}
          </button>
        ) : null}
        {!compact && log.review_model ? <div className="mt-1 truncate text-xs text-muted-foreground">{log.review_model}</div> : null}
      </TableCell>
    </TableRow>
    {expanded && hasFull ? (
      <TableRow>
        <TableCell colSpan={7} className="bg-muted/30">
          <div className="mb-1.5 flex items-center justify-between">
            <span className="text-xs font-semibold text-muted-foreground">{t('promptFilter.fullTextTitle')}</span>
            <button
              type="button"
              onClick={() => void navigator.clipboard?.writeText(fullText)}
              className="text-xs font-medium text-primary hover:underline"
            >
              {t('common.copy')}
            </button>
          </div>
          <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-background p-3 text-xs leading-relaxed text-foreground"><HighlightedPromptText text={fullText} terms={hitTerms} /></pre>
        </TableCell>
      </TableRow>
    ) : null}
    </>
  )
}

function ActionBadge({ action }: { action: string }) {
  const { t } = useTranslation()
  if (action === 'block') return <Badge variant="destructive">{t('promptFilter.modeBlock')}</Badge>
  if (action === 'warn') return <Badge variant="outline" className="border-amber-500/30 text-amber-700 dark:text-amber-300">{t('promptFilter.modeWarn')}</Badge>
  return <Badge variant="outline">{t('promptFilter.actionAllow')}</Badge>
}

function parseLogMatches(raw: string): PromptFilterMatch[] {
  if (!raw) return []
  try {
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed as PromptFilterMatch[] : []
  } catch {
    return []
  }
}
