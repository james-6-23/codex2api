import type { Dispatch, ReactNode, SetStateAction, TextareaHTMLAttributes } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { NavLink, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { AlertTriangle, CheckCircle2, ChevronDown, Copy, HelpCircle, KeyRound, Pencil, Plus, Power, PowerOff, RefreshCw, Save, Search, ShieldAlert, Trash2, Wand2, X } from 'lucide-react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import type { PromptFilterLog, PromptFilterMatch, PromptFilterRule, PromptFilterRulesResponse, PromptFilterVerdict, PromptIntelligenceCandidate, PromptIntelligenceRun, SystemSettings } from '../types'
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

const PROMPT_FILTER_VIEWS = ['overview', 'logs', 'rules', 'intelligence'] as const
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
  enforcement: { terminal_categories: string[] }
  normalization: { enabled: boolean; decode_url: boolean; decode_html: boolean; decode_base64: boolean; max_decode_runs: number }
  risk: { enabled: boolean; window_seconds: number; block_threshold: number; review_threshold: number; user_weight_percent: number; ip_weight_percent: number; session_weight_percent: number }
  sidecar: { enabled: boolean; base_url: string; timeout_seconds: number; fail_closed: boolean; min_score: number }
  output: { enabled: boolean; buffer_bytes: number; overlap_bytes: number; strict_only: boolean }
  intelligence: { enabled: boolean; interval_hours: number; queries: string[]; max_search_results: number; model_enabled: boolean; model: string; max_model_calls: number; auto_add: boolean }
  newapi: { enabled: boolean; max_clock_skew_seconds: number; offense_window_seconds: number; ban_after: number }
}

const defaultAdvancedProtection: AdvancedProtectionConfig = {
  enforcement: { terminal_categories: [] },
  normalization: { enabled: false, decode_url: false, decode_html: false, decode_base64: false, max_decode_runs: 1 },
  risk: { enabled: false, window_seconds: 600, block_threshold: 100, review_threshold: 60, user_weight_percent: 50, ip_weight_percent: 30, session_weight_percent: 20 },
  sidecar: { enabled: false, base_url: '', timeout_seconds: 3, fail_closed: true, min_score: 30 },
  output: { enabled: false, buffer_bytes: 4096, overlap_bytes: 512, strict_only: true },
  intelligence: { enabled: false, interval_hours: 24, queries: ['LLM jailbreak prompt injection', 'ChatGPT jailbreak prompt', 'Codex prompt injection jailbreak', '大模型 破限 提示词', 'GPT 破甲 提示词', 'AI 越狱 提示词', '中文 prompt injection 绕过'], max_search_results: 20, model_enabled: false, model: 'gpt-5.4', max_model_calls: 1, auto_add: false },
  newapi: { enabled: false, max_clock_skew_seconds: 120, offense_window_seconds: 86400, ban_after: 2 },
}

function parseAdvancedProtection(raw: string): AdvancedProtectionConfig {
  try {
    const value = JSON.parse(raw || '{}')
    return {
      enforcement: { ...defaultAdvancedProtection.enforcement, ...(value.enforcement || {}) },
      normalization: { ...defaultAdvancedProtection.normalization, ...(value.normalization || {}) },
      risk: { ...defaultAdvancedProtection.risk, ...(value.risk || {}) },
      sidecar: { ...defaultAdvancedProtection.sidecar, ...(value.sidecar || {}) },
      output: { ...defaultAdvancedProtection.output, ...(value.output || {}) },
      intelligence: { ...defaultAdvancedProtection.intelligence, ...(value.intelligence || {}) },
      newapi: { ...defaultAdvancedProtection.newapi, ...(value.newapi || {}) },
    }
  } catch { return defaultAdvancedProtection }
}

const defaultForm: PromptFilterForm = {
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
  prompt_filter_mode: settings?.prompt_filter_mode || 'monitor',
  prompt_filter_threshold: settings?.prompt_filter_threshold || 50,
  prompt_filter_strict_threshold: settings?.prompt_filter_strict_threshold || 90,
  prompt_filter_strict_terminal_enabled: Boolean(settings?.prompt_filter_strict_terminal_enabled),
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
  ]
  const activeIndex = Math.max(0, tabs.findIndex((tab) => tab.view === activeView))
  return (
    <div className="mb-5 flex justify-center">
      <div className="relative grid w-full max-w-[720px] grid-cols-4 rounded-2xl border border-border bg-background/80 p-1 shadow-sm backdrop-blur-lg" role="tablist">
        <div
          className="pointer-events-none absolute left-1 top-1 h-[calc(100%-0.5rem)] rounded-xl border border-primary/15 bg-primary/8 transition-transform duration-300 ease-out"
          style={{ width: 'calc((100% - 0.5rem) / 4)', transform: `translateX(${activeIndex * 100}%)` }}
        />
        {tabs.map((tab) => (
          <NavLink
            key={tab.view}
            to={tab.to}
            role="tab"
            aria-selected={activeView === tab.view}
            className={`relative z-10 flex h-9 items-center justify-center rounded-xl px-3 text-sm font-semibold transition-colors ${
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

function AdvancedProtectionEditor({ value, onChange, booleanOptions }: { value: string; onChange: (value: string) => void; booleanOptions: { label: string; value: string }[] }) {
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
  const bool = (section: keyof AdvancedProtectionConfig, key: string, current: boolean) => (
    <Select value={current ? 'true' : 'false'} onValueChange={(next) => update(section, { [key]: next === 'true' } as never)} options={booleanOptions} />
  )
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
  return (
    <div className="space-y-4 rounded-xl border border-border bg-muted/15 p-4">
      <SectionTitle title={t('promptFilter.advancedVisualTitle')} />
      <div className="grid gap-4 xl:grid-cols-2">
        <div className="space-y-3 rounded-lg border bg-background/70 p-4">
          <h3 className="font-semibold">{t('promptFilter.normalizationTitle')}</h3>
          <div className="grid gap-3 sm:grid-cols-2"><Field label={t('promptFilter.enabled')} hint={t('promptFilter.help.normalizationEnabled')}>{bool('normalization', 'enabled', config.normalization.enabled)}</Field><Field label={t('promptFilter.decodeRuns')} hint={t('promptFilter.help.decodeRuns')}><DraftNumberInput min={1} max={2} value={config.normalization.max_decode_runs} onValueChange={(v) => update('normalization', { max_decode_runs: v })} /></Field><Field label="URL Decode" hint={t('promptFilter.help.decodeUrl')}>{bool('normalization', 'decode_url', config.normalization.decode_url)}</Field><Field label="HTML Decode" hint={t('promptFilter.help.decodeHtml')}>{bool('normalization', 'decode_html', config.normalization.decode_html)}</Field><Field label="Base64 Decode" hint={t('promptFilter.help.decodeBase64')}>{bool('normalization', 'decode_base64', config.normalization.decode_base64)}</Field></div>
        </div>
        <div className="space-y-3 rounded-lg border bg-background/70 p-4">
          <h3 className="font-semibold">{t('promptFilter.terminalCategories')}</h3>
          <Field label={t('promptFilter.terminalCategories')} hint={t('promptFilter.help.terminalCategories')}><Input value={config.enforcement.terminal_categories.join(', ')} placeholder="reverse_engineering, malware" onChange={(e) => update('enforcement', { terminal_categories: e.target.value.split(',').map((item) => item.trim()).filter(Boolean) })} /></Field>
          <p className="text-xs text-muted-foreground">{t('promptFilter.terminalCategoriesHint')}</p>
        </div>
        <div className="space-y-3 rounded-lg border bg-background/70 p-4">
          <h3 className="font-semibold">{t('promptFilter.riskTitle')}</h3>
          <div className="grid gap-3 sm:grid-cols-2"><Field label={t('promptFilter.enabled')} hint={t('promptFilter.help.riskEnabled')}>{bool('risk', 'enabled', config.risk.enabled)}</Field><Field label={t('promptFilter.riskWindow')} hint={t('promptFilter.help.riskWindow')}><DraftNumberInput min={60} max={86400} value={config.risk.window_seconds} onValueChange={(v) => update('risk', { window_seconds: v })} /></Field><Field label={t('promptFilter.blockThreshold')} hint={t('promptFilter.help.blockThreshold')}><DraftNumberInput min={1} max={1000} value={config.risk.block_threshold} onValueChange={(v) => update('risk', { block_threshold: v })} /></Field><Field label={t('promptFilter.reviewThreshold')} hint={t('promptFilter.help.reviewThreshold')}><DraftNumberInput min={1} max={1000} value={config.risk.review_threshold} onValueChange={(v) => update('risk', { review_threshold: v })} /></Field></div>
        </div>
        <div className="space-y-3 rounded-lg border bg-background/70 p-4">
          <h3 className="font-semibold">{t('promptFilter.outputScanTitle')}</h3>
          <div className="grid gap-3 sm:grid-cols-2"><Field label={t('promptFilter.enabled')} hint={t('promptFilter.help.outputEnabled')}>{bool('output', 'enabled', config.output.enabled)}</Field><Field label={t('promptFilter.strictOnly')} hint={t('promptFilter.help.strictOnly')}>{bool('output', 'strict_only', config.output.strict_only)}</Field><Field label={t('promptFilter.bufferBytes')} hint={t('promptFilter.help.bufferBytes')}><DraftNumberInput min={512} max={65536} value={config.output.buffer_bytes} onValueChange={(v) => update('output', { buffer_bytes: v })} /></Field><Field label={t('promptFilter.overlapBytes')} hint={t('promptFilter.help.overlapBytes')}><DraftNumberInput min={64} max={16384} value={config.output.overlap_bytes} onValueChange={(v) => update('output', { overlap_bytes: v })} /></Field></div>
        </div>
        <div className="space-y-3 rounded-lg border bg-background/70 p-4 xl:col-span-2">
          <div>
            <h3 className="font-semibold">{t('promptFilter.newapi.title')}</h3>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('promptFilter.newapi.description')}</p>
          </div>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Field label={t('promptFilter.newapi.enabled')} hint={t('promptFilter.newapi.enabledHint')}>{bool('newapi', 'enabled', config.newapi.enabled)}</Field>
            <Field label={t('promptFilter.newapi.clockSkew')} hint={t('promptFilter.newapi.clockSkewHint')}><DraftNumberInput min={30} max={600} value={config.newapi.max_clock_skew_seconds} onValueChange={(v) => update('newapi', { max_clock_skew_seconds: v })} /></Field>
            <Field label={t('promptFilter.newapi.offenseWindow')} hint={t('promptFilter.newapi.offenseWindowHint')}><DraftNumberInput min={60} max={2592000} value={config.newapi.offense_window_seconds} onValueChange={(v) => update('newapi', { offense_window_seconds: v })} /></Field>
            <Field label={t('promptFilter.newapi.banAfter')} hint={t('promptFilter.newapi.banAfterHint')}><DraftNumberInput min={2} max={10} value={config.newapi.ban_after} onValueChange={(v) => update('newapi', { ban_after: v })} /></Field>
          </div>
          <details className="rounded-lg border bg-muted/20 p-3">
            <summary className="cursor-pointer text-sm font-semibold">{t('promptFilter.newapi.protocolTitle')}</summary>
            <div className="mt-3 grid gap-4 lg:grid-cols-2">
              <div className="space-y-2">
                <div className="text-xs font-semibold text-muted-foreground">{t('promptFilter.newapi.codexEnv')}</div>
                <pre className="overflow-x-auto rounded-md bg-slate-950 p-3 text-xs text-slate-100"><code>{t('promptFilter.newapi.codexSecretExample')}</code></pre>
                <p className="text-xs leading-relaxed text-muted-foreground">{t('promptFilter.newapi.secretStorageHint')}</p>
                <div className="rounded-md border bg-background p-3">
                  <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                    <div className="flex items-center gap-2 text-sm font-medium"><KeyRound className="size-4" />{t('promptFilter.newapi.generator')}</div>
                    <Button type="button" size="sm" variant="outline" disabled={secretSaving || secretStatus.source === 'environment'} onClick={() => void generateNewAPISecret()}><RefreshCw className={`size-3.5 ${secretSaving ? 'animate-spin' : ''}`} />{secretStatus.configured ? t('promptFilter.newapi.replaceSecret') : t('promptFilter.newapi.generateSecret')}</Button>
                  </div>
                  {secretError && <p className="text-xs text-destructive">{secretError}</p>}
                  <p className="text-xs text-muted-foreground">{secretStatus.configured ? t('promptFilter.newapi.secretConfigured', { masked: secretStatus.masked, source: secretStatus.source === 'environment' ? t('promptFilter.newapi.environment') : t('promptFilter.newapi.database') }) : t('promptFilter.newapi.secretUnconfigured')}</p>
                </div>
              </div>
              <div className="space-y-2">
                <div className="text-xs font-semibold text-muted-foreground">{t('promptFilter.newapi.newapiEnv')}</div>
                <pre className="overflow-x-auto rounded-md bg-slate-950 p-3 text-xs text-slate-100"><code>{t('promptFilter.newapi.newapiEnvExample')}</code></pre>
              </div>
              <div className="space-y-2 lg:col-span-2">
                <div className="text-xs font-semibold text-muted-foreground">{t('promptFilter.newapi.headersTitle')}</div>
                <pre className="overflow-x-auto rounded-md bg-slate-950 p-3 text-xs text-slate-100"><code>{t('promptFilter.newapi.headersExample')}</code></pre>
                <p className="text-xs leading-relaxed text-muted-foreground">{t('promptFilter.newapi.signatureHint')}</p>
              </div>
            </div>
          </details>
          <Dialog open={secretRevealOpen} onOpenChange={(open) => { if (!open) requestCloseSecretReveal() }}>
            <DialogContent className="sm:max-w-2xl" onEscapeKeyDown={(event) => { event.preventDefault(); requestCloseSecretReveal() }} onPointerDownOutside={(event) => { event.preventDefault(); requestCloseSecretReveal() }}>
              <DialogHeader>
                <DialogTitle>{t('promptFilter.newapi.revealTitle')}</DialogTitle>
                <DialogDescription>{t('promptFilter.newapi.revealDescription')}</DialogDescription>
              </DialogHeader>
              <div className="space-y-3">
                <div className="flex gap-2"><Input readOnly value={generatedNewAPISecret} className="font-mono text-xs" /><Button type="button" variant="outline" onClick={() => void copyNewAPISecret()}><Copy className="size-4" />{secretCopied ? t('promptFilter.newapi.copied') : t('promptFilter.newapi.copySecret')}</Button></div>
                <pre className="overflow-x-auto rounded-md bg-slate-950 p-3 text-xs text-slate-100"><code>{`CODEX2API_POLICY_SECRET=${generatedNewAPISecret}`}</code></pre>
                <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">{t('promptFilter.newapi.revealWarning')}</div>
              </div>
              <DialogFooter><Button type="button" variant="outline" onClick={requestCloseSecretReveal}>{t('promptFilter.newapi.close')}</Button><Button type="button" onClick={() => void copyNewAPISecret()}><Copy className="size-4" />{secretCopied ? t('promptFilter.newapi.copied') : t('promptFilter.newapi.copyAndConfigure')}</Button></DialogFooter>
            </DialogContent>
          </Dialog>
          <Dialog open={secretCloseConfirmOpen} onOpenChange={setSecretCloseConfirmOpen}>
            <DialogContent className="sm:max-w-md">
              <DialogHeader><DialogTitle>{t('promptFilter.newapi.closeConfirmTitle')}</DialogTitle><DialogDescription>{t('promptFilter.newapi.closeConfirmDescription')}</DialogDescription></DialogHeader>
              <DialogFooter><Button type="button" variant="outline" onClick={() => setSecretCloseConfirmOpen(false)}>{t('promptFilter.newapi.backToCopy')}</Button><Button type="button" variant="destructive" onClick={confirmCloseSecretReveal}>{t('promptFilter.newapi.confirmClose')}</Button></DialogFooter>
            </DialogContent>
          </Dialog>
        </div>
        <div className="space-y-3 rounded-lg border bg-background/70 p-4 xl:col-span-2">
          <h3 className="font-semibold">{t('promptFilter.intelligence.configTitle')}</h3>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4"><Field label={t('promptFilter.intelligence.scheduleEnabled')} hint={t('promptFilter.help.scheduleEnabled')}>{bool('intelligence', 'enabled', config.intelligence.enabled)}</Field><Field label={t('promptFilter.intelligence.intervalHours')} hint={t('promptFilter.help.intervalHours')}><DraftNumberInput min={1} max={720} value={config.intelligence.interval_hours} onValueChange={(v) => update('intelligence', { interval_hours: v })} /></Field><Field label={t('promptFilter.intelligence.maxResults')} hint={t('promptFilter.help.maxResults')}><DraftNumberInput min={1} max={100} value={config.intelligence.max_search_results} onValueChange={(v) => update('intelligence', { max_search_results: v })} /></Field><Field label={t('promptFilter.intelligence.modelEnabled')} hint={t('promptFilter.help.modelEnabled')}>{bool('intelligence', 'model_enabled', config.intelligence.model_enabled)}</Field><Field label={t('promptFilter.intelligence.model')} hint={t('promptFilter.help.model')}><Input value={config.intelligence.model} onChange={(e) => update('intelligence', { model: e.target.value })} /></Field><Field label={t('promptFilter.intelligence.maxModelCalls')} hint={t('promptFilter.help.maxModelCalls')}><DraftNumberInput min={0} max={3} value={config.intelligence.max_model_calls} onValueChange={(v) => update('intelligence', { max_model_calls: v })} /></Field><Field label={t('promptFilter.intelligence.autoAdd')} hint={t('promptFilter.help.autoAdd')}>{bool('intelligence', 'auto_add', config.intelligence.auto_add)}</Field></div>
          <Field label={t('promptFilter.intelligence.queries')} hint={t('promptFilter.help.queries')}><Textarea rows={4} value={config.intelligence.queries.join('\n')} placeholder="LLM jailbreak prompt injection" onChange={(e) => update('intelligence', { queries: e.target.value.split('\n').map((item) => item.trim()).filter(Boolean) })} /></Field>
          <div className="rounded-md bg-muted/50 p-3"><div className="mb-2 text-xs font-semibold text-muted-foreground">{t('promptFilter.intelligence.builtinQueries')}</div><div className="flex flex-wrap gap-2">{['LLM jailbreak prompt injection', 'ChatGPT jailbreak prompt', 'Codex prompt injection jailbreak', '大模型 破限 提示词', 'GPT 破甲 提示词', 'AI 越狱 提示词', '中文 prompt injection 绕过'].map((query) => <Badge key={query} variant="outline">{query}</Badge>)}</div></div>
        </div>
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
              <Field label={t('promptFilter.strictTerminal')}>
                <Select value={form.prompt_filter_strict_terminal_enabled ? 'true' : 'false'} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_strict_terminal_enabled: value === 'true' }))} options={booleanOptions} />
                <span className="block text-xs leading-5 text-muted-foreground">{t('promptFilter.strictTerminalHint')}</span>
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
            </Field>
            <AdvancedProtectionEditor value={form.prompt_filter_advanced_config} onChange={(value) => setForm((current) => ({ ...current, prompt_filter_advanced_config: value }))} booleanOptions={booleanOptions} />

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
                  rows={4}
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
            <Select value={draftFilters.action} onValueChange={(value) => setDraftFilters((current) => ({ ...current, action: value }))} options={[{ label: t('common.all'), value: '' }, { label: 'block', value: 'block' }, { label: 'warn', value: 'warn' }, { label: 'allow', value: 'allow' }]} />
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
  const [customDialogMode, setCustomDialogMode] = useState<'create' | 'edit' | null>(null)
  const [editingCustomIndex, setEditingCustomIndex] = useState<number | null>(null)
  const [customDialogDraft, setCustomDialogDraft] = useState<CustomRuleDraft>(defaultCustomRuleDraft)
  const [savingRule, setSavingRule] = useState('')
  const [categoryFilter, setCategoryFilter] = useState<string>('')
  const [selectedRules, setSelectedRules] = useState<Set<string>>(new Set())
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(10)

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
  onToggle,
  onEdit,
  onDelete,
  iconActions = false,
  busy,
}: {
  rule: PromptFilterRule
  selected?: boolean
  onSelect?: () => void
  onToggle: () => void
  onEdit?: () => void
  onDelete?: () => void
  iconActions?: boolean
  busy?: boolean
}) {
  const { t } = useTranslation()
  const enabled = rule.enabled !== false
  return (
    <TableRow>
      {onSelect !== undefined ? (
        <TableCell>
          <input
            type="checkbox"
            checked={selected}
            onChange={onSelect}
            className="size-4 cursor-pointer"
          />
        </TableCell>
      ) : null}
      <TableCell>
        <div className="font-mono text-xs font-semibold text-foreground">{rule.name}</div>
        <div className="mt-1 flex gap-1">
          {rule.builtin ? <Badge variant="secondary">{t('promptFilter.builtinRule')}</Badge> : <Badge variant="outline">{t('promptFilter.customRule')}</Badge>}
          {rule.strict ? <Badge variant="destructive">{t('promptFilter.ruleStrict')}</Badge> : null}
          <Badge variant={enabled ? 'default' : 'outline'}>{enabled ? t('common.enabled') : t('common.disabled')}</Badge>
        </div>
      </TableCell>
      <TableCell>{rule.category || '-'}</TableCell>
      <TableCell className="font-mono text-sm">{rule.weight}</TableCell>
      <TableCell className="max-w-[520px]">
        <code className="line-clamp-2 whitespace-normal break-all rounded bg-muted/60 px-2 py-1 text-xs text-muted-foreground">{rule.pattern}</code>
      </TableCell>
      <TableCell>
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
  const action = verdict.action
  if (action === 'block') {
    return (
      <Badge variant="destructive" className="gap-1.5">
        <ShieldAlert className="size-3" />
        Block
      </Badge>
    )
  }
  if (action === 'warn') {
    return (
      <Badge variant="outline" className="gap-1.5 border-amber-500/30 text-amber-700 dark:text-amber-300">
        <AlertTriangle className="size-3" />
        Warn
      </Badge>
    )
  }
  return (
    <Badge variant="outline" className="gap-1.5 border-emerald-500/30 text-emerald-700 dark:text-emerald-300">
      <CheckCircle2 className="size-3" />
      Allow
    </Badge>
  )
}

function VerdictPanel({ verdict }: { verdict: PromptFilterVerdict }) {
  return (
    <div className="rounded-lg border border-border bg-muted/25 p-3">
      <div className="grid grid-cols-[repeat(auto-fit,minmax(120px,1fr))] gap-2 text-sm">
        <MiniStat label="Mode" value={verdict.mode || '-'} />
        <MiniStat label="Score" value={`${verdict.score} / ${verdict.threshold}`} />
        <MiniStat label="Matches" value={String(verdict.matched?.length ?? 0)} />
        <MiniStat label="Review" value={verdict.reviewed ? (verdict.review_flagged ? 'Flagged' : 'Cleared') : '-'} />
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
  const hitTerms = extractHitTerms(log.text_preview || '')
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
          {log.source === 'upstream_cyber_policy' ? <Badge variant="outline" className="text-[11px]">upstream</Badge> : null}
          {log.review_model ? <Badge variant="outline" className="text-[11px]">{log.review_flagged ? 'review flagged' : 'review cleared'}</Badge> : null}
        </div>
      </TableCell>
      <TableCell>
        <div className="truncate font-mono text-xs text-foreground">{log.endpoint || '-'}</div>
        <div className="truncate font-mono text-xs text-muted-foreground">{log.model || '-'}</div>
      </TableCell>
      <TableCell>
        <span className="font-semibold">{log.score}</span>
        <span className="text-muted-foreground"> / {log.threshold}</span>
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
        <div className="truncate text-muted-foreground" title={stripHitMarkers(log.text_preview || log.error_code || log.review_error || '')}>{log.text_preview ? <HighlightedPromptPreview text={log.text_preview} /> : (log.error_code || log.review_error || '-')}</div>
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
  if (action === 'block') return <Badge variant="destructive">block</Badge>
  if (action === 'warn') return <Badge variant="outline" className="border-amber-500/30 text-amber-700 dark:text-amber-300">warn</Badge>
  return <Badge variant="outline">allow</Badge>
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
