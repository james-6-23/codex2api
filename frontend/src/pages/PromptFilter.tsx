import type { Dispatch, ReactNode, SetStateAction, TextareaHTMLAttributes } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { NavLink, useParams, useSearchParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Activity, AlertTriangle, BookOpen, CheckCircle2, ChevronDown, ClipboardCheck, Copy, FileText, Gauge, GitBranch, HelpCircle, Layers, ListChecks, Loader2, Network, Pencil, Plus, Power, PowerOff, RefreshCw, Save, Search, Shield, ShieldAlert, Sparkles, Trash2, Users, Wand2, X } from 'lucide-react'
import { AdminAPIError, api } from '../api'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import PromptFilterNewAPIBindings from '../components/PromptFilterNewAPIBindings'
import StateShell from '../components/StateShell'
import { StatTile } from '../components/StatTile'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import { getPromptFilterScoreBand, normalizePromptFilterScore } from '../lib/promptFilterScore'
import { parseAdvancedConfigDocument, patchAdvancedConfigDocument, readAdvancedConfigPath } from '../types'
import type { AdvancedConfigObject, AdvancedConfigPatch, PromptFilterLog, PromptFilterMatch, PromptFilterRule, PromptFilterRulesResponse, PromptFilterTestResponse, PromptGuardConfig, PromptGuardLayer, PromptGuardMode, PromptGuardProfile, PromptGuardProvider, PromptIdentityUpdateMode, PromptIntelligenceAIAnalysisResponse, PromptIntelligenceAIProvider, PromptIntelligenceCandidate, PromptIntelligenceEvidenceResponse, PromptIntelligenceGatewayKey, PromptIntelligenceRun, PromptPolicyAuditHealth, PromptPolicyIncident, PromptPolicyIncidentDetailResponse, PromptReviewAPIKeyDescriptor, PromptReviewKeyTestResult, PromptReviewProfile, PromptReviewTestResponse, PromptRiskProfile, PromptRiskProfileDetailResponse, SystemSettings, PromptLogRetention, PromptRiskIncidentSubject, PromptIntelligenceDraftSuggestion } from '../types'
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

const PROMPT_FILTER_VIEWS = ['overview', 'logs', 'profiles', 'rules', 'intelligence', 'docs'] as const
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
  reviewResult: string
}

type RiskProfileFilters = {
  subjectType: string
  riskLevel: string
  platform: string
  apiKeyId: string
  accountId: string
  minScore: string
	q: string
	lockedOnly: boolean
	cyOnly: boolean
	activityState: '' | 'active' | 'identity_only'
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

type ReviewAdapterFormConfig = {
  request_mode: 'moderations' | 'chat_completions'
  scope: 'all_requests' | 'local_candidates' | 'local_blocks'
  system_prompt: string
  user_prompt_template: string
  payload_template: string
  confidence_threshold: number
  moderation_thresholds: Record<string, number>
  max_concurrent: number
  max_text_length: number
  circuit_breaker_failures: number
  circuit_breaker_seconds: number
}

type AdaptiveReviewFormConfig = {
  enabled: boolean
  min_clean_reviews: number
  min_observation_hours: number
  sample_percent: number
  force_review_interval_minutes: number
}

type RecommendedProtectionStrength = 'monitor' | 'block'

const defaultReviewAdapter: ReviewAdapterFormConfig = {
  request_mode: 'moderations',
  scope: 'all_requests',
  system_prompt: '',
  user_prompt_template: '',
  payload_template: '',
  confidence_threshold: 0.7,
  moderation_thresholds: {
    harassment: 0.98,
    'harassment/threatening': 0.90,
    hate: 0.65,
    'hate/threatening': 0.65,
    illicit: 0.95,
    'illicit/violent': 0.95,
    'self-harm': 0.65,
    'self-harm/intent': 0.85,
    'self-harm/instructions': 0.65,
    sexual: 0.65,
    'sexual/minors': 0.65,
    violence: 0.95,
    'violence/graphic': 0.95,
  },
  max_concurrent: 32,
  max_text_length: 32768,
  circuit_breaker_failures: 3,
  circuit_breaker_seconds: 30,
}

const moderationThresholdCategories = Object.keys(defaultReviewAdapter.moderation_thresholds)

function parseReviewAdapter(value: AdvancedConfigObject): ReviewAdapterFormConfig {
  const raw = value.review_adapter && typeof value.review_adapter === 'object'
    ? value.review_adapter as Record<string, unknown>
    : {}
  const rawThresholds = raw.moderation_thresholds && typeof raw.moderation_thresholds === 'object'
    ? raw.moderation_thresholds as Record<string, unknown>
    : {}
  const moderationThresholds = Object.fromEntries(moderationThresholdCategories.map((category) => {
    const configured = rawThresholds[category]
    const threshold = typeof configured === 'number' && Number.isFinite(configured)
      ? Math.min(1, Math.max(0, configured))
      : defaultReviewAdapter.moderation_thresholds[category]
    return [category, threshold]
  }))
  return {
    request_mode: raw.request_mode === 'chat_completions' ? 'chat_completions' : 'moderations',
    scope: raw.scope === 'local_candidates' || raw.scope === 'local_blocks' ? raw.scope : 'all_requests',
    system_prompt: typeof raw.system_prompt === 'string' && raw.system_prompt.trim() ? raw.system_prompt : defaultReviewAdapter.system_prompt,
    user_prompt_template: typeof raw.user_prompt_template === 'string' && raw.user_prompt_template.trim() ? raw.user_prompt_template : defaultReviewAdapter.user_prompt_template,
    payload_template: typeof raw.payload_template === 'string' ? raw.payload_template : '',
    confidence_threshold: typeof raw.confidence_threshold === 'number' && raw.confidence_threshold > 0 && raw.confidence_threshold <= 1 ? raw.confidence_threshold : defaultReviewAdapter.confidence_threshold,
    moderation_thresholds: moderationThresholds,
    max_concurrent: typeof raw.max_concurrent === 'number' && raw.max_concurrent > 0 ? raw.max_concurrent : defaultReviewAdapter.max_concurrent,
    max_text_length: typeof raw.max_text_length === 'number' && raw.max_text_length > 0 ? raw.max_text_length : defaultReviewAdapter.max_text_length,
    circuit_breaker_failures: typeof raw.circuit_breaker_failures === 'number' && raw.circuit_breaker_failures > 0 ? raw.circuit_breaker_failures : defaultReviewAdapter.circuit_breaker_failures,
    circuit_breaker_seconds: typeof raw.circuit_breaker_seconds === 'number' && raw.circuit_breaker_seconds > 0 ? raw.circuit_breaker_seconds : defaultReviewAdapter.circuit_breaker_seconds,
  }
}

function parseAdaptiveReview(value: AdvancedConfigObject): AdaptiveReviewFormConfig {
  const raw = value.adaptive_review && typeof value.adaptive_review === 'object'
    ? value.adaptive_review as Record<string, unknown>
    : {}
  return {
    enabled: raw.enabled === true,
    min_clean_reviews: typeof raw.min_clean_reviews === 'number' && raw.min_clean_reviews > 0 ? raw.min_clean_reviews : 10,
    min_observation_hours: typeof raw.min_observation_hours === 'number' && raw.min_observation_hours > 0 ? raw.min_observation_hours : 24,
    sample_percent: typeof raw.sample_percent === 'number' && raw.sample_percent >= 0 ? raw.sample_percent : 5,
    force_review_interval_minutes: typeof raw.force_review_interval_minutes === 'number' && raw.force_review_interval_minutes > 0 ? raw.force_review_interval_minutes : 360,
  }
}

type PromptGuardEditorConfig = Omit<PromptGuardConfig, 'performance'>

type AdvancedProtectionConfig = {
  guard: PromptGuardEditorConfig
  enforcement: { terminal_categories: string[]; terminal_bypass_models: string[]; local_block_message: string; conversation_lock_enabled: boolean; conversation_lock_ttl_hours: number; user_cyber_cooldown_minutes: number; cyb_strike_enabled: boolean; local_severe_strike_enabled: boolean; authorized_pentest_allowed: boolean }
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
    fail_closed: boolean
    mode: 'shadow' | 'warn' | 'enforce'
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
    allow_remote_urls: boolean
  }
  output: { enabled: boolean; strict_only: boolean }
  intelligence: { enabled: boolean; interval_hours: number; queries: string[]; max_search_results: number; model_enabled: boolean; model: string; max_model_calls: number }
}

const promptGuardModes: PromptGuardMode[] = ['inherit', 'off', 'shadow', 'warn', 'enforce']
const promptGuardAuxiliaryModes: PromptGuardMode[] = ['off', 'shadow']
const promptGuardProfiles: PromptGuardProfile[] = ['balanced', 'strict', 'research']
const promptGuardProviders: PromptGuardProvider[] = ['openai', 'anthropic', 'xai', 'unknown']
const promptGuardLayers: PromptGuardLayer[] = ['current_user', 'history', 'system', 'developer', 'instructions', 'tool_output', 'tool_arguments', 'attachment_refs', 'session_context', 'attachment_content']
const sidecarModes: AdvancedProtectionConfig['sidecar']['mode'][] = ['shadow', 'warn', 'enforce']
const inheritedPromptGuardProfile = '__default__'

type LocalizedSelectOption = { value: string; label: string }

function selectWithPreservedUnknown(
  rawValue: unknown,
  fallback: string,
  options: LocalizedSelectOption[],
  unknownLabel: string,
): { value: string; options: LocalizedSelectOption[]; unknown: boolean } {
  if (typeof rawValue === 'string' && !options.some((option) => option.value === rawValue)) {
    return {
      value: rawValue,
      options: [{ value: rawValue, label: unknownLabel }, ...options],
      unknown: true,
    }
  }
  return {
    value: typeof rawValue === 'string' ? rawValue : fallback,
    options,
    unknown: false,
  }
}

const defaultPromptGuard: PromptGuardEditorConfig = {
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
  enforcement: { terminal_categories: [], terminal_bypass_models: ['codex-auto-review'], local_block_message: '', conversation_lock_enabled: true, conversation_lock_ttl_hours: 168, user_cyber_cooldown_minutes: 30, cyb_strike_enabled: false, local_severe_strike_enabled: true, authorized_pentest_allowed: false },
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
    fail_closed: false,
    mode: 'shadow',
  },
  session: {
    enabled: false,
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
    allow_remote_urls: false,
  },
  output: { enabled: false, strict_only: true },
  intelligence: { enabled: false, interval_hours: 24, queries: ['LLM jailbreak prompt injection', 'ChatGPT jailbreak prompt', 'Codex prompt injection jailbreak', '大模型 破限 提示词', 'GPT 破甲 提示词', 'AI 越狱 提示词', '中文 prompt injection 绕过'], max_search_results: 20, model_enabled: false, model: 'gpt-5.4', max_model_calls: 1 },
}

function parsePromptGuardMode(value: unknown, fallback: PromptGuardMode = 'inherit'): PromptGuardMode {
  return typeof value === 'string' && promptGuardModes.includes(value as PromptGuardMode) ? value as PromptGuardMode : fallback
}

function parsePromptGuardProfile(value: unknown, fallback: PromptGuardProfile = 'balanced'): PromptGuardProfile {
  return typeof value === 'string' && promptGuardProfiles.includes(value as PromptGuardProfile) ? value as PromptGuardProfile : fallback
}

function parsePromptGuard(value: unknown): PromptGuardEditorConfig {
  const raw = value && typeof value === 'object' ? value as Record<string, unknown> : {}
  const rawProviders = raw.provider_profiles && typeof raw.provider_profiles === 'object'
    ? raw.provider_profiles as Record<string, unknown>
    : {}
  const rawLayers = raw.layers && typeof raw.layers === 'object'
    ? raw.layers as Record<string, unknown>
    : {}

  const providerProfiles: PromptGuardEditorConfig['provider_profiles'] = {}
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

function parseAdvancedProtection(value: AdvancedConfigObject): AdvancedProtectionConfig {
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
      terminal_bypass_models: Array.isArray(enforcement.terminal_bypass_models)
        ? enforcement.terminal_bypass_models.filter((model: unknown): model is string => typeof model === 'string')
        : [...defaultAdvancedProtection.enforcement.terminal_bypass_models],
      local_block_message: typeof enforcement.local_block_message === 'string'
        ? enforcement.local_block_message
        : defaultAdvancedProtection.enforcement.local_block_message,
      conversation_lock_enabled: typeof enforcement.conversation_lock_enabled === 'boolean'
        ? enforcement.conversation_lock_enabled
        : defaultAdvancedProtection.enforcement.conversation_lock_enabled,
      conversation_lock_ttl_hours: typeof enforcement.conversation_lock_ttl_hours === 'number' && enforcement.conversation_lock_ttl_hours > 0
        ? enforcement.conversation_lock_ttl_hours
        : defaultAdvancedProtection.enforcement.conversation_lock_ttl_hours,
      user_cyber_cooldown_minutes: typeof enforcement.user_cyber_cooldown_minutes === 'number' && enforcement.user_cyber_cooldown_minutes > 0
        ? enforcement.user_cyber_cooldown_minutes
        : defaultAdvancedProtection.enforcement.user_cyber_cooldown_minutes,
      cyb_strike_enabled: typeof enforcement.cyb_strike_enabled === 'boolean'
        ? enforcement.cyb_strike_enabled
        : defaultAdvancedProtection.enforcement.cyb_strike_enabled,
      local_severe_strike_enabled: typeof enforcement.local_severe_strike_enabled === 'boolean'
        ? enforcement.local_severe_strike_enabled
        : defaultAdvancedProtection.enforcement.local_severe_strike_enabled,
      authorized_pentest_allowed: typeof enforcement.authorized_pentest_allowed === 'boolean'
        ? enforcement.authorized_pentest_allowed
        : defaultAdvancedProtection.enforcement.authorized_pentest_allowed,
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
  }
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
  prompt_filter_review_base_url: 'https://api.deepseek.com',
  prompt_filter_review_model: 'deepseek-v4-flash',
  prompt_filter_review_timeout_seconds: 10,
  prompt_filter_review_fail_closed: true,
}

// 必须与后端 security/promptfilter/review.go 的 parseReviewAPIKeys 保持同一
// 切分语义(空白/逗号/分号 + 去重保序):测试结果的 key_index 按后端解析顺序
// 编号,前端用它反查草稿位置,两侧分词一旦分歧就会定位到错误的 Key。
// U+0085(NEL)属 Go 的 unicode.IsSpace 但不在 JS 的 \s 内,需显式补上。
function parsePromptReviewAPIKeyInput(raw: string): string[] {
  const seen = new Set<string>()
  return raw
    .split(/[\s\u0085,;]+/)
    .map((key) => key.trim())
    .filter((key) => {
      if (!key || seen.has(key)) return false
      seen.add(key)
      return true
    })
}

const emptyFilters: LogFilters = {
  action: '',
  source: '',
  endpoint: '',
  model: '',
  apiKeyId: '',
  q: '',
  reviewResult: '',
}

const defaultLocalLogFilters: LogFilters = {
  ...emptyFilters,
  source: 'local_filter',
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

function customRuleIdentity(rule: PromptFilterRule): string {
  return JSON.stringify(rule)
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
  prompt_filter_review_base_url: settings?.prompt_filter_review_base_url || 'https://api.deepseek.com',
  prompt_filter_review_model: settings?.prompt_filter_review_model || 'deepseek-v4-flash',
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
  // 自定义规则使用独立的并发保护写入流程；普通设置保存不得回写旧快照。
  delete payload.prompt_filter_custom_patterns
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
  const [settingsSaveRevision, setSettingsSaveRevision] = useState(0)
  const advancedConfigError = useMemo(
    () => parseAdvancedConfigDocument(form.prompt_filter_advanced_config).error,
    [form.prompt_filter_advanced_config],
  )
  const [testing, setTesting] = useState(false)
  const [testText, setTestText] = useState('')
  const [testEndpoint, setTestEndpoint] = useState('/v1/responses')
  const [testModel, setTestModel] = useState('gpt-5.5')
  const [testResult, setTestResult] = useState<PromptFilterTestResponse | null>(null)

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
    if (advancedConfigError) {
      showToast(t('promptFilter.advancedConfigInvalidSave'), 'error')
      return
    }
    setSaving(true)
    let updated: SystemSettings
    try {
      const payload = partial ?? promptFilterSavePayload(form)
      updated = await api.updateSettings(payload)
    } catch (err) {
      showToast(`${t('promptFilter.saveFailed')}: ${getErrorMessage(err)}`, 'error')
      setSaving(false)
      return
    }

    setForm(normalizePromptFilterForm(updated))
    setData((current) => ({ ...current, settings: updated }))
    setSettingsSaveRevision((revision) => revision + 1)
    setSaving(false)
    const quarantines = updated.prompt_filter_pattern_quarantines ?? []
    if (quarantines.length > 0) {
      showToast(t('promptFilter.saveQuarantined', { count: quarantines.length }), 'warning')
    } else {
      showToast(t('promptFilter.saveSuccess'))
    }

    const [rulesResult, logsResult] = await Promise.allSettled([
      api.getPromptFilterRules(),
      api.getPromptFilterLogs({ limit: 5 }),
    ])
    setData((current) => ({
      ...current,
      rules: rulesResult.status === 'fulfilled' ? rulesResult.value : current.rules,
      recentLogs: logsResult.status === 'fulfilled' ? (logsResult.value.logs ?? []) : current.recentLogs,
      totalLogs: logsResult.status === 'fulfilled'
        ? (logsResult.value.total ?? current.totalLogs)
        : current.totalLogs,
    }))
    if (rulesResult.status === 'rejected' || logsResult.status === 'rejected') {
      showToast(t('promptFilter.saveRefreshFailed'), 'warning')
    }
  }

  const runTest = async () => {
    const text = testText.trim()
    if (!text) {
      showToast(t('promptFilter.testEmpty'), 'error')
      return
    }
    setTestResult(null)
    setTesting(true)
    try {
      const result = await api.testPromptFilter({
        text,
        endpoint: testEndpoint,
        model: testModel,
      })
      setTestResult(result)
      showToast(t('promptFilter.testDone'))
    } catch (err) {
      showToast(`${t('promptFilter.testFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setTesting(false)
    }
  }

  const refreshRecentLogs = async () => {
    const result = await api.getPromptFilterLogs({ limit: 5 })
    setData((current) => ({
      ...current,
      recentLogs: result.logs ?? [],
      totalLogs: result.total ?? 0,
    }))
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
                <Button onClick={() => void saveSettings()} disabled={saving || Boolean(advancedConfigError)}>
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
            setTestText={(value) => {
              setTestText(value)
              setTestResult(null)
            }}
            testEndpoint={testEndpoint}
            setTestEndpoint={(value) => {
              setTestEndpoint(value)
              setTestResult(null)
            }}
            testModel={testModel}
            setTestModel={(value) => {
              setTestModel(value)
              setTestResult(null)
            }}
            testing={testing}
            testResult={testResult}
            runTest={runTest}
            advancedConfigError={advancedConfigError}
            settingsSaveRevision={settingsSaveRevision}
            onSettingsChanged={(settings) => {
              setForm(normalizePromptFilterForm(settings))
              setData((current) => ({ ...current, settings }))
            }}
            onSave={() => void saveSettings()}
          />
        ) : null}

        {activeView === 'logs' ? <LogsView onPromptLogsChanged={refreshRecentLogs} /> : null}

        {activeView === 'profiles' ? <RiskProfilesView /> : null}

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
    { view: 'profiles' as const, label: t('promptFilter.views.profiles'), to: '/prompt-filter/profiles' },
    { view: 'rules' as const, label: t('promptFilter.views.rules'), to: '/prompt-filter/rules' },
    { view: 'intelligence' as const, label: t('promptFilter.views.intelligence'), to: '/prompt-filter/intelligence' },
    { view: 'docs' as const, label: t('promptFilter.views.docs'), to: '/prompt-filter/docs' },
  ]
  const tabCount = tabs.length
  const activeIndex = Math.max(0, tabs.findIndex((tab) => tab.view === activeView))
  return (
    <div className="mb-5 flex justify-center">
      <div
        className="relative flex max-w-[900px] w-full items-center gap-1 overflow-x-auto rounded-2xl border border-border bg-background/80 p-1 shadow-sm backdrop-blur-lg [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden sm:grid"
        style={{ gridTemplateColumns: `repeat(${tabCount}, minmax(0, 1fr))` }}
        role="tablist"
      >
        <div
          className="pointer-events-none absolute left-1 top-1 hidden h-[calc(100%-0.5rem)] rounded-xl border border-primary/15 bg-primary/8 transition-transform duration-300 ease-out sm:block"
          style={{ width: `calc((100% - 0.5rem) / ${tabCount})`, transform: `translateX(${activeIndex * 100}%)` }}
        />
        {tabs.map((tab) => {
          const active = activeView === tab.view
          return (
            <NavLink
              key={tab.view}
              to={tab.to}
              role="tab"
              aria-selected={active}
              className={`relative z-10 flex h-9 shrink-0 items-center justify-center rounded-xl px-3 text-sm font-semibold transition-colors whitespace-nowrap ${
                active ? 'text-primary max-sm:bg-primary/10 max-sm:border max-sm:border-primary/20' : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              {tab.label}
            </NavLink>
          )
        })}
      </div>
    </div>
  )
}

function AdvancedProtectionEditor({
  value,
  onChange,
}: {
  value: string
  onChange: (value: string) => void
}) {
  const { t } = useTranslation()
  const document = useMemo(() => parseAdvancedConfigDocument(value), [value])
  const config = useMemo(
    () => parseAdvancedProtection(document.value ?? {}),
    [document.value],
  )
  const applyPatches = (patches: readonly AdvancedConfigPatch[]) => {
    const result = patchAdvancedConfigDocument(value, [
      ...patches,
      { path: ['newapi', 'enabled'], remove: true },
      { path: ['newapi', 'secret'], remove: true },
      { path: ['newapi', 'offense_window_seconds'], remove: true },
      { path: ['newapi', 'ban_after'], remove: true },
      { path: ['intelligence', 'auto_add'], remove: true },
    ])
    if (!result.ok) return
    onChange(result.serialized)
  }
  const update = <K extends keyof AdvancedProtectionConfig>(section: K, patch: Partial<AdvancedProtectionConfig[K]>) => {
    applyPatches(Object.entries(patch).map(([key, next]) => ({ path: [section, key], value: next })))
  }
  const setBool = <K extends keyof AdvancedProtectionConfig>(section: K, key: string, next: boolean) => {
    update(section, { [key]: next } as never)
  }
  const terminalCategoriesText = config.enforcement.terminal_categories.join(', ')
  const terminalBypassModelsText = config.enforcement.terminal_bypass_models.join(', ')
  const queryCount = config.intelligence.queries.length
  const enabledExtensionCount = [config.sidecar.enabled, config.session.enabled, config.attachment.enabled, config.intelligence.enabled].filter(Boolean).length
  const guardModeOptions = promptGuardModes.map((mode) => ({
    value: mode,
    label: t(`promptFilter.guard.modes.${mode}.label`),
  }))
  const guardAuxiliaryModeOptions = promptGuardAuxiliaryModes.map((mode) => ({
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
  const unknownEnumLabel = t('promptFilter.guard.unknownEnumPreserved')
  const guardModeSelection = selectWithPreservedUnknown(
    readAdvancedConfigPath(document.value, ['guard', 'mode']),
    config.guard.mode,
    guardModeOptions,
    unknownEnumLabel,
  )
  const guardProfileSelection = selectWithPreservedUnknown(
    readAdvancedConfigPath(document.value, ['guard', 'default_profile']),
    config.guard.default_profile,
    guardProfileOptions,
    unknownEnumLabel,
  )
  const sidecarModeSelection = selectWithPreservedUnknown(
    readAdvancedConfigPath(document.value, ['sidecar', 'mode']),
    config.sidecar.mode,
    sidecarModeOptions,
    unknownEnumLabel,
  )
  const updateGuard = (patch: Partial<PromptGuardEditorConfig>) => update('guard', patch)
  const updateGuardProvider = (provider: PromptGuardProvider, profile: PromptGuardProfile | null) => {
    applyPatches([{
      path: ['guard', 'provider_profiles', provider],
      value: profile ?? undefined,
      remove: profile === null,
    }])
  }
  const updateGuardLayer = (layer: PromptGuardLayer, mode: PromptGuardMode) => {
    applyPatches([{ path: ['guard', 'layers', layer, 'mode'], value: mode }])
  }

  if (!document.ok) {
    return (
      <div role="alert" className="rounded-lg border border-destructive/30 bg-destructive/[0.06] p-4">
        <div className="flex items-start gap-3">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-destructive" />
          <div className="min-w-0">
            <div className="font-semibold text-foreground">{t('promptFilter.advancedConfigInvalidTitle')}</div>
            <p className="mt-1 text-sm leading-6 text-muted-foreground">{t('promptFilter.advancedConfigInvalidDescription')}</p>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-3">
      <SectionTitle title={t('promptFilter.advancedVisualTitle')} />

      <details className="group overflow-hidden rounded-lg border border-foreground/15 bg-background shadow-sm dark:border-foreground/20">
        <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3.5 marker:content-none [&::-webkit-details-marker]:hidden">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-semibold">{t('promptFilter.guard.title')}</span>
              <Badge variant="secondary">{t(`promptFilter.guard.profiles.${guardProfileSelection.value}.label`, { defaultValue: guardProfileSelection.value })}</Badge>
            </div>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.guard.simplifiedSummary')}</p>
          </div>
          <div className="flex shrink-0 items-center gap-2 text-xs font-medium text-primary">
            <span>{t('promptFilter.guard.configureRouting')}</span>
            <ChevronDown className="size-4 transition-transform group-open:rotate-180" />
          </div>
        </summary>
        <div className="space-y-4 border-t p-4">
          <div className="grid gap-3 lg:grid-cols-2 xl:grid-cols-4">
            <div className="rounded-lg border border-foreground/10 bg-muted/15 p-3 dark:border-foreground/15">
              <CompactField label={t('promptFilter.guard.globalMode')} hint={t('promptFilter.guard.globalModeHint')}>
                <Select
                  value={guardModeSelection.value}
                  onValueChange={(next) => updateGuard({ mode: next as PromptGuardMode })}
                  options={guardModeSelection.options}
                />
              </CompactField>
            </div>
            <div className="rounded-lg border border-foreground/10 bg-muted/15 p-3 dark:border-foreground/15">
              <CompactField label={t('promptFilter.guard.defaultProfile')} hint={t('promptFilter.guard.defaultProfileHint')}>
                <Select
                  value={guardProfileSelection.value}
                  onValueChange={(next) => updateGuard({ default_profile: next as PromptGuardProfile })}
                  options={guardProfileSelection.options}
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
            <div className="flex items-start gap-3 rounded-lg border border-[hsl(var(--info))]/25 bg-[hsl(var(--info-bg))] p-3 text-sm">
              <Shield className="mt-0.5 size-4 shrink-0 text-[hsl(var(--info))]" />
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
                  guardModeSelection.value === mode
                    ? 'border-primary/35 bg-primary/[0.07]'
                    : 'border-foreground/10 bg-background dark:border-foreground/15',
                )}
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="text-xs font-semibold">{t(`promptFilter.guard.modes.${mode}.label`)}</span>
                  {guardModeSelection.value === mode ? <Badge className="h-5 px-1.5 text-[10px]">{t('promptFilter.guard.active')}</Badge> : null}
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
                {promptGuardProviders.map((provider) => {
                  const selection = selectWithPreservedUnknown(
                    readAdvancedConfigPath(document.value, ['guard', 'provider_profiles', provider]),
                    config.guard.provider_profiles[provider] ?? inheritedPromptGuardProfile,
                    guardProviderProfileOptions,
                    unknownEnumLabel,
                  )
                  return (
                    <CompactField
                      key={provider}
                      label={t(`promptFilter.guard.providers.${provider}.label`)}
                      hint={t(`promptFilter.guard.providers.${provider}.description`)}
                    >
                      <Select
                        value={selection.value}
                        onValueChange={(next) => updateGuardProvider(
                          provider,
                          next === inheritedPromptGuardProfile ? null : next as PromptGuardProfile,
                        )}
                        options={selection.options}
                      />
                    </CompactField>
                  )
                })}
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
                      guardProfileSelection.value === profile
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
              {promptGuardLayers.map((layer) => {
                const layerModeOptions = layer === 'current_user' ? guardModeOptions : guardAuxiliaryModeOptions
                const selection = selectWithPreservedUnknown(
                  readAdvancedConfigPath(document.value, ['guard', 'layers', layer, 'mode']),
                  config.guard.layers[layer].mode,
                  layerModeOptions,
                  unknownEnumLabel,
                )
                return (
                  <CompactField
                    key={layer}
                    label={t(`promptFilter.guard.layers.${layer}.label`)}
                    hint={t(`promptFilter.guard.layers.${layer}.description`)}
                  >
                    <Select
                      value={selection.value}
                      onValueChange={(next) => updateGuardLayer(layer, next as PromptGuardMode)}
                      options={selection.options}
                    />
                  </CompactField>
                )
              })}
            </div>
          </div>

        </div>
      </details>

      {/* Core defense: bounded decoding and intent-aware scoring keep the default preset useful without widening penalties. */}
      <div className="grid gap-3 lg:grid-cols-2">
        <div className="lg:col-span-2">
          <AdvancedPanel title={t('promptFilter.normalizationTitle')} hint={t('promptFilter.help.normalizationPanel')}>
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div className="flex items-center gap-3">
                <Switch checked={config.normalization.enabled} onCheckedChange={(next) => setBool('normalization', 'enabled', next)} />
                <div>
                  <div className="text-sm font-medium">{config.normalization.enabled ? t('common.enabled') : t('common.disabled')}</div>
                  <p className="text-xs text-muted-foreground">{t('promptFilter.normalizationSimplifiedDesc')}</p>
                </div>
              </div>
              <Badge variant="secondary">{t('promptFilter.normalizationDecoderCount', { count: [config.normalization.decode_url, config.normalization.decode_html, config.normalization.decode_base64, config.normalization.decode_hex, config.normalization.decode_rot13, config.normalization.decode_escapes, config.normalization.decode_compression].filter(Boolean).length })}</Badge>
            </div>
            <details className={cn('group mt-3 rounded-md border border-foreground/10 bg-muted/10', !config.normalization.enabled && 'pointer-events-none opacity-60')}>
              <summary className="flex cursor-pointer list-none items-center justify-between px-3 py-2 text-xs font-medium marker:content-none [&::-webkit-details-marker]:hidden">
                <span>{t('promptFilter.normalizationTune')}</span>
                <ChevronDown className="size-4 text-muted-foreground transition-transform group-open:rotate-180" />
              </summary>
              <div className="grid grid-cols-2 gap-x-3 gap-y-3 border-t p-3 sm:grid-cols-3 xl:grid-cols-5">
                <CompactField label={t('promptFilter.decodeRuns')} hint={t('promptFilter.help.decodeRuns')}><DraftNumberInput min={1} max={2} value={config.normalization.max_decode_runs} onValueChange={(v) => update('normalization', { max_decode_runs: v })} /></CompactField>
                <CompactField label={t('promptFilter.maxDecodedBytes')} hint={t('promptFilter.help.maxDecodedBytes')}><DraftNumberInput min={1024} max={65536} value={config.normalization.max_decoded_bytes} onValueChange={(v) => update('normalization', { max_decoded_bytes: v })} /></CompactField>
                <CompactField label={t('promptFilter.maxEncodedBlocks')} hint={t('promptFilter.help.maxEncodedBlocks')}><DraftNumberInput min={1} max={32} value={config.normalization.max_encoded_blocks} onValueChange={(v) => update('normalization', { max_encoded_blocks: v })} /></CompactField>
                {([
                  ['decode_url', 'url', 'decodeUrl'],
                  ['decode_html', 'html', 'decodeHtml'],
                  ['decode_base64', 'base64', 'decodeBase64'],
                  ['decode_hex', 'hex', 'decodeHex'],
                  ['decode_rot13', 'rot13', 'decodeRot13'],
                  ['decode_escapes', 'escapes', 'decodeEscapes'],
                  ['decode_compression', 'compression', 'decodeCompression'],
                ] as const).map(([key, labelKey, hintKey]) => (
                  <SwitchField key={key} label={t(`promptFilter.decoders.${labelKey}`)} hint={t(`promptFilter.help.${hintKey}`)} checked={config.normalization[key]} onCheckedChange={(next) => setBool('normalization', key, next)} />
                ))}
              </div>
            </details>
          </AdvancedPanel>
        </div>

        <AdvancedPanel title={t('promptFilter.contextDiscount.title')} hint={t('promptFilter.contextDiscount.description')}>
          <div className="flex items-center gap-3">
            <Switch checked={config.context_discount.enabled} onCheckedChange={(next) => setBool('context_discount', 'enabled', next)} />
            <span className="text-sm text-muted-foreground">{t('promptFilter.contextDiscount.simplifiedDesc')}</span>
          </div>
          <details className={cn('group mt-3 rounded-md border border-foreground/10 bg-muted/10', !config.context_discount.enabled && 'pointer-events-none opacity-60')}>
            <summary className="flex cursor-pointer list-none items-center justify-between px-3 py-2 text-xs font-medium marker:content-none [&::-webkit-details-marker]:hidden"><span>{t('promptFilter.tuneParameters')}</span><ChevronDown className="size-4 text-muted-foreground transition-transform group-open:rotate-180" /></summary>
            <div className="grid grid-cols-2 gap-x-3 gap-y-3 border-t p-3 sm:grid-cols-3">
              <SwitchField label={t('promptFilter.contextDiscount.intentAware')} hint={t('promptFilter.contextDiscount.intentAwareHint')} checked={config.context_discount.intent_aware} onCheckedChange={(next) => setBool('context_discount', 'intent_aware', next)} />
              <CompactField label={t('promptFilter.contextDiscount.maxDiscount')} hint={t('promptFilter.contextDiscount.maxDiscountHint')}><DraftNumberInput min={0} max={90} value={config.context_discount.max_discount} onValueChange={(v) => update('context_discount', { max_discount: v, operational_max_discount: Math.min(config.context_discount.operational_max_discount, v) })} /></CompactField>
              <CompactField label={t('promptFilter.contextDiscount.operationalMaxDiscount')} hint={t('promptFilter.contextDiscount.operationalMaxDiscountHint')}><DraftNumberInput min={0} max={config.context_discount.max_discount} value={config.context_discount.operational_max_discount} onValueChange={(v) => update('context_discount', { operational_max_discount: v })} /></CompactField>
            </div>
          </details>
        </AdvancedPanel>

        <details className="group rounded-lg border border-foreground/15 bg-background shadow-sm dark:border-foreground/20">
          <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3.5 marker:content-none [&::-webkit-details-marker]:hidden"><div><div className="text-sm font-semibold">{t('promptFilter.terminalPolicyTitle')}</div><p className="mt-1 text-xs text-muted-foreground">{t('promptFilter.terminalCategoriesCollapsedDesc')}</p></div><ChevronDown className="size-4 text-muted-foreground transition-transform group-open:rotate-180" /></summary>
          <div className="space-y-2 border-t p-4">
            <CompactField label={t('promptFilter.terminalCategories')} hint={t('promptFilter.help.terminalCategories')}><Input value={terminalCategoriesText} placeholder="malware, credential_attack" onChange={(e) => update('enforcement', { terminal_categories: e.target.value.split(',').map((item) => item.trim()).filter(Boolean) })} /></CompactField>
            <p className="text-[11px] leading-relaxed text-muted-foreground">{t('promptFilter.terminalCategoriesHint')}</p>
            <CompactField label={t('promptFilter.terminalBypassModels')} hint={t('promptFilter.help.terminalBypassModels')}><Input value={terminalBypassModelsText} placeholder="codex-auto-review" onChange={(e) => update('enforcement', { terminal_bypass_models: e.target.value.split(',').map((item) => item.trim()).filter(Boolean) })} /></CompactField>
            <p className="text-[11px] leading-relaxed text-muted-foreground">{t('promptFilter.terminalBypassModelsHint')}</p>
            <CompactField label={t('promptFilter.localBlockMessage')} hint={t('promptFilter.help.localBlockMessage')}>
              <Textarea
                rows={3}
                value={config.enforcement.local_block_message}
                placeholder={t('promptFilter.localBlockMessagePlaceholder')}
                onChange={(event) => update('enforcement', { local_block_message: Array.from(event.target.value).slice(0, 2000).join('') })}
              />
            </CompactField>
            <SwitchField label={t('promptFilter.conversationLockEnabled')} hint={t('promptFilter.help.conversationLockEnabled')} checked={config.enforcement.conversation_lock_enabled} onCheckedChange={(next) => update('enforcement', { conversation_lock_enabled: next })} />
            {config.enforcement.conversation_lock_enabled ? <div className="grid gap-3 sm:grid-cols-2">
              <CompactField label={t('promptFilter.conversationLockTTL')} hint={t('promptFilter.help.conversationLockTTL')}><DraftNumberInput min={1} max={720} value={config.enforcement.conversation_lock_ttl_hours} onValueChange={(next) => update('enforcement', { conversation_lock_ttl_hours: next })} /></CompactField>
              <CompactField label={t('promptFilter.userCyberCooldownMinutes')} hint={t('promptFilter.help.userCyberCooldownMinutes')}><DraftNumberInput min={1} max={1440} value={config.enforcement.user_cyber_cooldown_minutes} onValueChange={(next) => update('enforcement', { user_cyber_cooldown_minutes: next })} /></CompactField>
            </div> : null}
            <SwitchField label={t('promptFilter.cybStrikeEnabled')} hint={t('promptFilter.help.cybStrikeEnabled')} checked={config.enforcement.cyb_strike_enabled} onCheckedChange={(next) => update('enforcement', { cyb_strike_enabled: next })} />
            <SwitchField label={t('promptFilter.localSevereStrikeEnabled')} hint={t('promptFilter.help.localSevereStrikeEnabled')} checked={config.enforcement.local_severe_strike_enabled} onCheckedChange={(next) => update('enforcement', { local_severe_strike_enabled: next })} />
            <SwitchField label={t('promptFilter.authorizedPentestAllowed')} hint={t('promptFilter.help.authorizedPentestAllowed')} checked={config.enforcement.authorized_pentest_allowed} onCheckedChange={(next) => update('enforcement', { authorized_pentest_allowed: next })} />
          </div>
        </details>

        <AdvancedPanel title={t('promptFilter.riskTitle')}>
          <div className="flex items-center gap-3">
            <Switch checked={config.risk.enabled} onCheckedChange={(next) => setBool('risk', 'enabled', next)} />
            <span className="text-sm text-muted-foreground">{t('promptFilter.riskSimplifiedDesc')}</span>
          </div>
          <details className="group mt-3 rounded-md border border-foreground/10 bg-muted/10">
            <summary className="flex cursor-pointer list-none items-center justify-between px-3 py-2 text-xs font-medium marker:content-none [&::-webkit-details-marker]:hidden"><span>{t('promptFilter.tuneParameters')}</span><ChevronDown className="size-4 text-muted-foreground transition-transform group-open:rotate-180" /></summary>
            <div className="grid grid-cols-1 gap-x-3 gap-y-3 border-t p-3 sm:grid-cols-3">
              <CompactField label={t('promptFilter.riskWindow')} hint={t('promptFilter.help.riskWindow')}><DraftNumberInput min={60} max={86400} value={config.risk.window_seconds} onValueChange={(v) => update('risk', { window_seconds: v })} /></CompactField>
              <CompactField label={t('promptFilter.blockThreshold')} hint={t('promptFilter.help.blockThreshold')}><DraftNumberInput min={1} max={1000} value={config.risk.block_threshold} onValueChange={(v) => update('risk', { block_threshold: v })} /></CompactField>
              <CompactField label={t('promptFilter.reviewThreshold')} hint={t('promptFilter.help.reviewThreshold')}><DraftNumberInput min={1} max={1000} value={config.risk.review_threshold} onValueChange={(v) => update('risk', { review_threshold: v })} /></CompactField>
            </div>
          </details>
        </AdvancedPanel>

        <AdvancedPanel title={t('promptFilter.outputScanTitle')}>
          <div className="flex items-center gap-3">
            <Switch checked={config.output.enabled} onCheckedChange={(next) => setBool('output', 'enabled', next)} />
            <span className="text-sm text-muted-foreground">{t('promptFilter.outputSimplifiedDesc')}</span>
          </div>
          {config.output.enabled ? <div className="mt-3"><SwitchField label={t('promptFilter.strictOnly')} hint={t('promptFilter.help.strictOnly')} checked={config.output.strict_only} onCheckedChange={(next) => setBool('output', 'strict_only', next)} /></div> : null}
        </AdvancedPanel>
      </div>

      <details className="group overflow-hidden rounded-lg border border-foreground/15 bg-background shadow-sm dark:border-foreground/20">
        <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3.5 marker:content-none [&::-webkit-details-marker]:hidden">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm font-semibold">{t('promptFilter.extensions.title')}</span>
              <Badge variant="outline">{t('promptFilter.extensions.enabledCount', { count: enabledExtensionCount })}</Badge>
            </div>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.extensions.collapsedDesc')}</p>
          </div>
          <div className="flex shrink-0 items-center gap-2 text-xs font-medium text-primary"><span>{t('promptFilter.extensions.configure')}</span><ChevronDown className="size-4 transition-transform group-open:rotate-180" /></div>
        </summary>
        <div className="space-y-4 border-t p-4">
      <div className="grid gap-3 xl:grid-cols-2">
        <AdvancedPanel
          title={t('promptFilter.extensions.sidecar.title')}
          hint={t('promptFilter.extensions.sidecar.description')}
        >
          <div className="grid grid-cols-1 gap-x-3 gap-y-3 sm:grid-cols-3">
            <SwitchField
              label={t('promptFilter.extensions.sidecar.enabled')}
              hint={t('promptFilter.extensions.sidecar.enabledHint')}
              checked={config.sidecar.enabled}
              onCheckedChange={(next) => setBool('sidecar', 'enabled', next)}
            />
            <CompactField label={t('promptFilter.extensions.sidecar.mode')} hint={t('promptFilter.extensions.sidecar.modeHint')}>
              <Select value={sidecarModeSelection.value} onValueChange={(next) => update('sidecar', { mode: next as AdvancedProtectionConfig['sidecar']['mode'] })} options={sidecarModeSelection.options} />
            </CompactField>
            <SwitchField
              label={t('promptFilter.extensions.sidecar.failClosed')}
              hint={t('promptFilter.extensions.sidecar.failClosedHint')}
              checked={config.sidecar.fail_closed}
              onCheckedChange={(next) => setBool('sidecar', 'fail_closed', next)}
            />
            <div className="sm:col-span-3">
              <CompactField label={t('promptFilter.extensions.serviceURL')} hint={t('promptFilter.extensions.sidecar.baseURLHint')}>
                <Input value={config.sidecar.base_url} placeholder="http://127.0.0.1:18110" onChange={(e) => update('sidecar', { base_url: e.target.value })} />
              </CompactField>
            </div>
          </div>
        </AdvancedPanel>

        <AdvancedPanel
          title={t('promptFilter.extensions.session.title')}
          hint={t('promptFilter.extensions.session.description')}
          footer={(
            <div className="rounded-lg border border-[hsl(var(--info))]/25 bg-[hsl(var(--info-bg))] p-3 text-xs leading-5 text-muted-foreground">
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
          >
            <div className="grid grid-cols-1 gap-x-3 gap-y-3 sm:grid-cols-3">
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
              <div className="sm:col-span-3">
                <CompactField label={t('promptFilter.extensions.serviceURL')} hint={t('promptFilter.extensions.attachment.baseURLHint')}>
                  <Input value={config.attachment.base_url} placeholder="http://127.0.0.1:18120" onChange={(e) => update('attachment', { base_url: e.target.value })} />
                </CompactField>
              </div>
            </div>
          </AdvancedPanel>
        </div>
      </div>

      {/* Optional intelligence service. NewAPI policy passthrough is managed beside the penalty preset. */}
      <div className="grid gap-3">
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
          </div>
        </AdvancedPanel>
      </div>

        </div>
      </details>

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
    <div className="flex h-full min-h-0 flex-col gap-3 rounded-lg border border-border/60 bg-background p-3.5 shadow-sm">
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

function CompactField({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  return (
    <label className="flex min-w-0 flex-col gap-1.5">
      <span className="flex h-4 items-center gap-1 truncate text-xs font-medium leading-none text-muted-foreground">
        <span className="truncate">{label}</span>
        <FieldHint label={label} hint={hint} />
      </span>
      <div className="min-w-0 [&_input]:h-9 [&_input]:border-border/60 [&_input]:shadow-none">
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
  const [result, setResult] = useState<PromptIntelligenceRun | null>(null)
  const [history, setHistory] = useState<PromptIntelligenceRun[]>([])
  const [historyTotal, setHistoryTotal] = useState(0)
  const [historyPage, setHistoryPage] = useState(1)
  const [historyPageSize, setHistoryPageSize] = useState(10)
  const [historyLoading, setHistoryLoading] = useState(false)
  const [candidates, setCandidates] = useState<PromptIntelligenceCandidate[]>([])
  const [candidateTotal, setCandidateTotal] = useState(0)
  const [candidateLoading, setCandidateLoading] = useState(false)
  const [candidateStatus, setCandidateStatus] = useState('pending')
  const [candidateQuery, setCandidateQuery] = useState('')
  const [candidateQueryDraft, setCandidateQueryDraft] = useState('')
  const [candidatePage, setCandidatePage] = useState(1)
  const [candidatePageSize, setCandidatePageSize] = useState(20)
  const [candidateAction, setCandidateAction] = useState<number | null>(null)
  const [publishTarget, setPublishTarget] = useState<PromptIntelligenceCandidate | null>(null)
  const [draftTarget, setDraftTarget] = useState<PromptIntelligenceCandidate | null>(null)
  const [draftForm, setDraftForm] = useState({ name: '', pattern: '', weight: 35, category: 'cyber_abuse', strict: true, rationale: '' })
  const [draftSuggesting, setDraftSuggesting] = useState(false)
  // 草案生成的提供方独立于 AI 归因：审核用的 Review Key 和生成用的账号池 Key 不是一回事，
  // 默认走账号池，上次选择记在本地。
  const [draftProvider, setDraftProvider] = useState<PromptIntelligenceAIProvider>(() => readDraftAIPreference().provider)
  const [draftModel, setDraftModel] = useState(() => readDraftAIPreference().model)
  const [draftAPIKeyID, setDraftAPIKeyID] = useState(() => readDraftAIPreference().apiKeyId)
  const [draftSuggestion, setDraftSuggestion] = useState<PromptIntelligenceDraftSuggestion | null>(null)
  const [evidenceLoading, setEvidenceLoading] = useState<number | null>(null)
  const [evidenceDialog, setEvidenceDialog] = useState<PromptIntelligenceEvidenceResponse | null>(null)
  const [dismissTarget, setDismissTarget] = useState<PromptIntelligenceCandidate | null>(null)
  const [aiTarget, setAITarget] = useState<PromptIntelligenceCandidate | null>(null)
  const [aiProvider, setAIProvider] = useState<PromptIntelligenceAIProvider>('review')
  const [aiModel, setAIModel] = useState('')
  const [aiAPIKeyID, setAIAPIKeyID] = useState('0')
  const [identityUpdateMode, setIdentityUpdateMode] = useState<PromptIdentityUpdateMode>('suggest')
  const [aiLoading, setAILoading] = useState(false)
  const [aiResult, setAIResult] = useState<PromptIntelligenceAIAnalysisResponse | null>(null)
  const [gatewayKeys, setGatewayKeys] = useState<PromptIntelligenceGatewayKey[]>([])
  const candidateLoadSequence = useRef(0)

  const loadHistory = useCallback(async (page = historyPage) => {
    setHistoryLoading(true)
    try {
      const value = await api.getPromptIntelligenceHistory(page, historyPageSize)
      setHistory(value.runs)
      setHistoryTotal(value.total)
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setHistoryLoading(false)
    }
  }, [historyPage, historyPageSize, showToast])

  const loadCandidates = useCallback(async () => {
    const sequence = candidateLoadSequence.current + 1
    candidateLoadSequence.current = sequence
    setCandidateLoading(true)
    try {
      const value = await api.getPromptIntelligenceCandidates({ page: candidatePage, pageSize: candidatePageSize, status: candidateStatus, q: candidateQuery.trim() })
      if (candidateLoadSequence.current !== sequence) return
      setCandidates(value.candidates)
      setCandidateTotal(value.total)
    } catch (error) {
      if (candidateLoadSequence.current !== sequence) return
      showToast(getErrorMessage(error), 'error')
    } finally {
      if (candidateLoadSequence.current === sequence) setCandidateLoading(false)
    }
  }, [candidatePage, candidatePageSize, candidateQuery, candidateStatus, showToast])

  useEffect(() => { void loadHistory() }, [loadHistory])
  useEffect(() => { void loadCandidates() }, [loadCandidates])

  const run = async () => {
    setRunning(true)
    try {
      const value = await api.runPromptIntelligence()
      setResult(value)
      setHistoryPage(1)
      await Promise.all([loadHistory(1), loadCandidates()])
      showToast(t('promptFilter.intelligence.runSuccess', { count: value.candidates.length }))
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setRunning(false)
    }
  }

  const publish = async (candidate: PromptIntelligenceCandidate) => {
    setCandidateAction(candidate.id)
    try {
      const value = await api.publishPromptIntelligenceCandidate(candidate.id)
      showToast(value.updated ? t('promptFilter.intelligence.updateSuccess') : value.added ? t('promptFilter.intelligence.addSuccess') : t('promptFilter.intelligence.alreadyExists'))
      setPublishTarget(null)
      await loadCandidates()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setCandidateAction(null)
    }
  }

  const dismiss = async () => {
    if (!dismissTarget) return
    setCandidateAction(dismissTarget.id)
    try {
      await api.dismissPromptIntelligenceCandidate(dismissTarget.id)
      showToast(t('promptFilter.intelligence.dismissSuccess'))
      setDismissTarget(null)
      await loadCandidates()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setCandidateAction(null)
    }
  }

  const openDraft = async (candidate: PromptIntelligenceCandidate) => {
    setDraftTarget(candidate)
    setDraftSuggestion(null)
    setDraftForm({
      name: '', pattern: '', weight: 35, category: 'cyber_abuse', strict: true,
      rationale: candidate.sample_preview ? t('promptFilter.intelligence.draftRationaleFromEvidence') : '',
    })
    if (!gatewayKeys.length) {
      try {
        const response = await api.getPromptIntelligenceAIProviders()
        setGatewayKeys(response.gateway_keys.filter((key) => key.status === 'active'))
      } catch {
        // Key 列表加载失败不影响手工填写草案。
      }
    }
  }

  // 让模型基于候选的 CY 证据先写出草案，预填表单；校验结果只提示，人审核后再保存。
  const suggestDraft = async () => {
    if (!draftTarget) return
    setDraftSuggesting(true)
    try {
      writeDraftAIPreference({ provider: draftProvider, model: draftModel, apiKeyId: draftAPIKeyID })
      const value = await api.suggestPromptIntelligenceCandidateDraft(draftTarget.id, {
        provider: draftProvider,
        model: draftModel.trim() || undefined,
        api_key_id: draftProvider === 'account_pool' ? Number(draftAPIKeyID) || undefined : undefined,
      })
      setDraftSuggestion(value)
      setDraftForm({
        name: value.rule.name,
        pattern: value.rule.pattern,
        weight: value.rule.weight,
        category: value.rule.category,
        strict: value.rule.strict,
        rationale: value.rule.rationale,
      })
      showToast(value.validation_error ? t('promptFilter.intelligence.draftSuggestedWithWarning') : t('promptFilter.intelligence.draftSuggested'), value.validation_error ? 'warning' : undefined)
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setDraftSuggesting(false)
    }
  }

  const createDraft = async () => {
    if (!draftTarget) return
    setCandidateAction(draftTarget.id)
    try {
      const result = await api.createPromptIntelligenceCandidateDraft(draftTarget.id, draftForm)
      showToast(t('promptFilter.intelligence.draftCreated', { name: result.candidate.name }))
      setDraftTarget(null)
      await loadCandidates()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setCandidateAction(null)
    }
  }

  const viewEvidence = async (candidate: PromptIntelligenceCandidate) => {
    setEvidenceLoading(candidate.id)
    try {
      setEvidenceDialog(await api.getPromptIntelligenceCandidateEvidence(candidate.id))
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setEvidenceLoading(null)
    }
  }

  const openAIAnalysis = async (candidate: PromptIntelligenceCandidate) => {
    setAITarget(candidate)
    const persisted = candidate.latest_ai_analysis ?? null
    setAIProvider(persisted?.provider ?? 'review')
    setAIModel(persisted?.model ?? '')
    setAIAPIKeyID('0')
    setIdentityUpdateMode(persisted?.identity_update.mode === 'guarded_auto' ? 'guarded_auto' : 'suggest')
    setAIResult(persisted)
    if (!gatewayKeys.length) {
      try {
        const response = await api.getPromptIntelligenceAIProviders()
        setGatewayKeys(response.gateway_keys.filter((key) => key.status === 'active'))
      } catch {
        // DS analysis remains available even when the optional Key list fails.
      }
    }
  }

  const runAIAnalysis = async () => {
    if (!aiTarget) return
    setAILoading(true)
    try {
      const value = await api.analyzePromptIntelligenceCandidate(aiTarget.id, {
        provider: aiProvider,
        model: aiModel.trim() || undefined,
        api_key_id: aiProvider === 'account_pool' ? Number(aiAPIKeyID) || undefined : undefined,
        identity_update_mode: identityUpdateMode,
      })
      setAIResult(value)
      showToast(t('promptFilter.intelligence.aiAnalysisSuccess'))
      await loadCandidates()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setAILoading(false)
    }
  }

  const applyAIIdentity = async () => {
    if (!aiTarget || !aiResult) return
    setAILoading(true)
    try {
      const value = await api.applyPromptIntelligenceIdentityUpdate(aiTarget.id, aiResult.analysis_evidence_id)
      setAIResult((current) => current ? { ...current, identity_update: value.identity_update } : current)
      setAITarget((current) => current ? { ...current, lifecycle_status: 'published' } : current)
      showToast(t('promptFilter.intelligence.identityApplied'))
      await loadCandidates()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setAILoading(false)
    }
  }

  const rollbackAIIdentity = async (candidateID: number, revisionEvidenceID: number) => {
    setAILoading(true)
    try {
      const value = await api.rollbackPromptIntelligenceIdentityUpdate(candidateID, revisionEvidenceID)
      setAIResult((current) => current ? { ...current, identity_update: value.identity_update } : current)
      setAITarget((current) => current ? { ...current, lifecycle_status: 'pending' } : current)
      if (evidenceDialog?.candidate.id === candidateID) {
        setEvidenceDialog(await api.getPromptIntelligenceCandidateEvidence(candidateID))
      }
      showToast(t('promptFilter.intelligence.identityRolledBack'))
      await loadCandidates()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setAILoading(false)
    }
  }

  const lifecycleLabel = (status: string) => t(`promptFilter.intelligence.lifecycle.${status}`, { defaultValue: status || '-' })
  const sourceLabel = (source?: string) => t(`promptFilter.intelligence.source.${source || 'unknown'}`, { defaultValue: source || '-' })
  // 标题统一带上候选 ID（#417）：列表、证据 / 归因 / 草案 / 发布弹窗都用它，
  // 与上游 CY 事件详情里的「候选 · #ID」一一对应。
  const candidateTitle = (candidate: PromptIntelligenceCandidate) => `#${candidate.id} · ${candidate.kind === 'evidence'
    ? t(candidate.lifecycle_status === 'published' ? 'promptFilter.intelligence.attributedEvidence' : 'promptFilter.intelligence.awaitingAttribution')
    : candidate.name || t('promptFilter.intelligence.unnamedRule')}`

  const candidateLifecycleLabel = (candidate: PromptIntelligenceCandidate) => candidate.kind === 'evidence' && candidate.lifecycle_status === 'published'
    ? t('promptFilter.intelligence.attributed')
    : lifecycleLabel(candidate.lifecycle_status)

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
          <div className="mt-4 rounded-lg border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-sm text-muted-foreground">
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
              <span>{t('promptFilter.intelligence.staged')}: {result.staged}</span>
            </div>
            {result.errors.length ? <div className="mb-4 rounded-lg border border-destructive/30 p-3 text-sm text-destructive">{result.errors.join('；')}</div> : null}
            <p className="text-sm text-muted-foreground">{t('promptFilter.intelligence.resultHint')}</p>
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardContent className="p-5">
          <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 className="text-base font-semibold">{t('promptFilter.intelligence.reviewTitle')}</h2>
              <p className="mt-1 text-sm text-muted-foreground">{t('promptFilter.intelligence.reviewDesc')}</p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Select
                value={candidateStatus}
                onValueChange={(value) => { setCandidateStatus(value); setCandidatePage(1) }}
                options={['pending', 'published', 'dismissed', 'all'].map((status) => ({ value: status, label: lifecycleLabel(status) }))}
              />
              <Input
                className="w-56"
                placeholder={t('promptFilter.intelligence.searchPlaceholder')}
                value={candidateQueryDraft}
                onChange={(event) => setCandidateQueryDraft(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter') {
                    setCandidatePage(1)
                    setCandidateQuery(candidateQueryDraft)
                  }
                }}
              />
              <Button size="sm" onClick={() => { setCandidatePage(1); setCandidateQuery(candidateQueryDraft) }}>
                <Search className="size-4" />
                {t('promptFilter.intelligence.search')}
              </Button>
              <Button variant="outline" size="sm" onClick={() => void loadCandidates()} disabled={candidateLoading}>
                <RefreshCw className="size-4" />
                {t('common.refresh')}
              </Button>
            </div>
          </div>
          <div className="mb-3 text-xs text-muted-foreground">{t('promptFilter.intelligence.reviewCount', { count: candidateTotal })}</div>
          <div className="overflow-x-auto rounded-lg border border-border bg-card">
            {/* 固定列宽：规则正则和说明只能在第一列内换行，不能横向压到来源 / 状态列上。 */}
            <Table className="min-w-[1280px] table-fixed">
            <TableHeader>
              <TableRow>
                <TableHead>{t('promptFilter.intelligence.ruleOrEvidence')}</TableHead>
                <TableHead className="w-[150px]">{t('promptFilter.intelligence.sourceLabel')}</TableHead>
                <TableHead className="w-[100px]">{t('promptFilter.intelligence.statusLabel')}</TableHead>
                <TableHead className="w-[70px]">{t('promptFilter.intelligence.evidenceCount')}</TableHead>
                <TableHead className="w-[160px]">{t('promptFilter.intelligence.lastSeen')}</TableHead>
                <TableHead className="w-[430px] text-right">{t('common.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {candidates.map((candidate) => (
                <TableRow key={candidate.id}>
                  <TableCell className="min-w-0 whitespace-normal align-top">
                    <div className="flex flex-wrap items-center gap-2 font-medium">
                      {candidateTitle(candidate)}
                      <Badge variant="outline">{candidate.kind === 'evidence' ? t('promptFilter.intelligence.evidenceOnly') : candidate.change_type === 'update' ? t('promptFilter.intelligence.update') : t('promptFilter.intelligence.new')}</Badge>
                      {candidate.ai_analyzed ? (
                        <Badge className="bg-sky-500/12 text-sky-600 dark:bg-sky-500/20 dark:text-sky-400">
                          {t('promptFilter.intelligence.aiLearned')}
                          {candidate.ai_analysis_count && candidate.ai_analysis_count > 1 ? ` ×${candidate.ai_analysis_count}` : ''}
                        </Badge>
                      ) : null}
                    </div>
                    {candidate.pattern ? <code className="mt-1 block whitespace-pre-wrap break-all text-xs text-muted-foreground">{candidate.pattern}</code> : null}
                    {candidate.kind === 'pattern' ? (
                      <div className="mt-2 flex flex-wrap gap-1.5">
                        <Badge variant="outline">{t('promptFilter.intelligence.category')}: {candidate.category || '-'}</Badge>
                        <Badge variant="outline">{t('promptFilter.intelligence.weight')}: {candidate.weight}</Badge>
                        {candidate.strict ? <Badge variant="outline" className="border-destructive/40 text-destructive">strict</Badge> : null}
                      </div>
                    ) : null}
                    {candidate.sample_preview ? <p className="mt-1 line-clamp-2 break-words text-xs text-muted-foreground">{candidate.sample_preview}</p> : null}
                    {candidate.rationale ? <p className="mt-1 break-words text-xs text-muted-foreground">{candidate.rationale}</p> : null}
                  </TableCell>
                  <TableCell className="align-top"><Badge variant="outline">{sourceLabel(candidate.source)}</Badge></TableCell>
                  <TableCell className="align-top"><Badge variant="outline">{candidateLifecycleLabel(candidate)}</Badge></TableCell>
                  <TableCell className="align-top">{candidate.evidence_count}</TableCell>
                  <TableCell className="whitespace-nowrap align-top text-sm text-muted-foreground">{candidate.last_seen_at ? formatBeijingTime(candidate.last_seen_at) : '-'}</TableCell>
                  <TableCell className="whitespace-normal align-top">
                    <div className="flex flex-wrap justify-end gap-2">
                      <Button size="sm" variant="outline" disabled={evidenceLoading === candidate.id} onClick={() => void viewEvidence(candidate)}>
                        <FileText className="size-4" />
                        {t('promptFilter.intelligence.viewEvidence')}
                      </Button>
                      {candidate.lifecycle_status === 'pending' && candidate.kind === 'pattern' ? (
                        <Button size="sm" disabled={candidateAction === candidate.id} onClick={() => setPublishTarget(candidate)}>
                          {candidate.change_type === 'update' ? t('promptFilter.intelligence.updateRule') : t('promptFilter.intelligence.addRule')}
                        </Button>
                      ) : null}
                      {candidate.kind === 'evidence' && (candidate.lifecycle_status === 'pending' || candidate.ai_analyzed) ? (
                        <>
                          <Button size="sm" variant="outline" disabled={candidateAction === candidate.id} onClick={() => void openAIAnalysis(candidate)}>
                            <Sparkles className="size-4" />
                            {candidate.ai_analyzed ? t('promptFilter.intelligence.aiViewResult') : t('promptFilter.intelligence.aiAnalyze')}
                          </Button>
                          {candidate.lifecycle_status === 'pending' ? (
                            <Button size="sm" disabled={candidateAction === candidate.id} onClick={() => void openDraft(candidate)}>
                              <Pencil className="size-4" />
                              {t('promptFilter.intelligence.createDraft')}
                            </Button>
                          ) : null}
                        </>
                      ) : null}
                      {candidate.lifecycle_status === 'pending' ? (
                        <Button size="sm" variant="outline" disabled={candidateAction === candidate.id} onClick={() => setDismissTarget(candidate)}>
                          {t('promptFilter.intelligence.dismiss')}
                        </Button>
                      ) : null}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
              {!candidateLoading && !candidates.length ? <TableRow><TableCell colSpan={6} className="py-8 text-center text-muted-foreground">{t('promptFilter.intelligence.noReviewCandidates')}</TableCell></TableRow> : null}
            </TableBody>
          </Table>
          </div>
          <Pagination
            page={candidatePage}
            totalPages={Math.max(1, Math.ceil(candidateTotal / candidatePageSize))}
            totalItems={candidateTotal}
            pageSize={candidatePageSize}
            onPageChange={setCandidatePage}
            onPageSizeChange={(next) => { setCandidatePage(1); setCandidatePageSize(next) }}
            pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
          />
        </CardContent>
      </Card>

      <Card>
        <CardContent className="p-5">
          <div className="mb-4 flex items-center justify-between"><div><h2 className="text-base font-semibold">{t('promptFilter.intelligence.historyTitle')}</h2><p className="mt-1 text-sm text-muted-foreground">{t('promptFilter.intelligence.historyDesc')}</p></div><Button variant="outline" size="sm" onClick={() => void loadHistory()} disabled={historyLoading}><RefreshCw className="size-4" />{t('common.refresh')}</Button></div>
          <div className="space-y-3">
            {history.map((run, index) => (
              <div key={`${run.started_at}-${index}`} className="rounded-lg border p-4">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="font-medium">{formatBeijingTime(run.started_at)}</div>
                  <div className="flex flex-wrap gap-2">
                    <Badge variant="outline">{t('promptFilter.intelligence.sources')} {run.sources.length}</Badge>
                    <Badge variant="outline">{t('promptFilter.intelligence.candidates')} {run.candidates.length}</Badge>
                    <Badge variant="outline">{t('promptFilter.intelligence.staged')} {run.staged ?? run.added ?? 0}</Badge>
                    <Badge variant="outline">{t('promptFilter.intelligence.modelCalls')} {run.model_calls}</Badge>
                  </div>
                </div>
                <div className="mt-3 grid gap-2 md:grid-cols-2">
                  {run.sources.map((source) => (
                    <a key={source.url} href={source.url} target="_blank" rel="noreferrer" className="rounded-md bg-muted/40 p-2 text-sm hover:text-primary">
                      <div className="font-medium">{source.title}</div>
                      <div className="truncate text-xs text-muted-foreground">{source.url}</div>
                    </a>
                  ))}
                </div>
                {run.candidates.length ? (
                  <details className="group mt-3 rounded-md border bg-muted/10">
                    <summary className="flex cursor-pointer list-none items-center justify-between px-3 py-2 text-sm font-medium marker:content-none [&::-webkit-details-marker]:hidden">
                      {t('promptFilter.intelligence.historyCandidates')}
                      <ChevronDown className="size-4 text-muted-foreground transition-transform group-open:rotate-180" />
                    </summary>
                    <div className="space-y-2 border-t p-3">
                      {run.candidates.map((candidate, candidateIndex) => (
                        <div key={`${candidate.name}-${candidate.pattern}-${candidateIndex}`} className="rounded-md bg-background p-3 text-sm">
                          <div className="flex flex-wrap items-center gap-2 font-medium">
                            {candidate.name || t('promptFilter.intelligence.unnamedRule')}
                            <Badge variant="outline">{(candidate.change_type || candidate.status) === 'update' ? t('promptFilter.intelligence.update') : t('promptFilter.intelligence.new')}</Badge>
                          </div>
                          {candidate.pattern ? <code className="mt-1 block break-all text-xs text-muted-foreground">{candidate.pattern}</code> : null}
                        </div>
                      ))}
                    </div>
                  </details>
                ) : null}
                {run.errors.length ? <div className="mt-3 text-sm text-destructive">{run.errors.join('；')}</div> : null}
              </div>
            ))}
            {!historyLoading && !history.length ? <div className="py-8 text-center text-muted-foreground">{t('promptFilter.intelligence.noHistory')}</div> : null}
          </div>
          <Pagination
            page={historyPage}
            totalPages={Math.max(1, Math.ceil(historyTotal / historyPageSize))}
            totalItems={historyTotal}
            pageSize={historyPageSize}
            onPageChange={setHistoryPage}
            onPageSizeChange={(next) => { setHistoryPage(1); setHistoryPageSize(next) }}
            pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
          />
        </CardContent>
      </Card>

      <Dialog open={Boolean(aiTarget)} onOpenChange={(open) => { if (!open && !aiLoading) { setAITarget(null); setAIResult(null) } }}>
        <DialogContent className="max-h-[90vh] max-w-4xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{t('promptFilter.intelligence.aiAnalysisTitle')}</DialogTitle>
            <DialogDescription>{t('promptFilter.intelligence.aiAnalysisDesc')}</DialogDescription>
          </DialogHeader>
          {aiTarget?.sample_preview ? (
            <div className="rounded-lg border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-sm whitespace-pre-wrap break-words">
              {aiTarget.sample_preview}
            </div>
          ) : null}
          <div className="grid gap-4 md:grid-cols-2">
            <Field label={t('promptFilter.intelligence.aiProvider')}>
              <Select
                value={aiProvider}
                onValueChange={(value) => setAIProvider(value as PromptIntelligenceAIProvider)}
                options={[
                  { value: 'review', label: t('promptFilter.intelligence.aiProviderReview') },
                  { value: 'account_pool', label: t('promptFilter.intelligence.aiProviderPool') },
                ]}
              />
            </Field>
            <Field label={t('promptFilter.intelligence.aiModel')} hint={t('promptFilter.intelligence.aiModelHint')}>
              <Input value={aiModel} onChange={(event) => setAIModel(event.target.value)} placeholder={t('promptFilter.intelligence.aiModelDefault')} />
            </Field>
            {aiProvider === 'account_pool' ? (
              <Field label={t('promptFilter.intelligence.aiGatewayKey')} hint={t('promptFilter.intelligence.aiGatewayKeyHint')}>
                <Select
                  value={aiAPIKeyID}
                  onValueChange={setAIAPIKeyID}
                  options={[
                    { value: '0', label: t('promptFilter.intelligence.aiGatewayKeyAuto') },
                    ...gatewayKeys.map((key) => ({ value: String(key.id), label: `${key.name || `#${key.id}`} · ${key.masked}` })),
                  ]}
                />
              </Field>
            ) : null}
            <Field label={t('promptFilter.intelligence.identityUpdateMode')} hint={t('promptFilter.intelligence.identityUpdateModeHint')}>
              <Select
                value={identityUpdateMode}
                onValueChange={(value) => setIdentityUpdateMode(value as PromptIdentityUpdateMode)}
                options={[
                  { value: 'suggest', label: t('promptFilter.intelligence.identitySuggest') },
                  { value: 'guarded_auto', label: t('promptFilter.intelligence.identityGuardedAuto') },
                ]}
              />
            </Field>
          </div>
          <div className="rounded-lg border bg-muted/20 p-3 text-sm text-muted-foreground">
            {t('promptFilter.intelligence.identitySafetyHint')}
          </div>

          {aiResult ? (
            <div className="space-y-4 rounded-xl border p-4">
              <div className="flex flex-wrap items-center gap-2">
                <Badge>{t(`promptFilter.intelligence.aiDecision.${aiResult.decision.decision}`, { defaultValue: aiResult.decision.decision })}</Badge>
                <Badge className="bg-sky-500/12 text-sky-600 dark:bg-sky-500/20 dark:text-sky-400">{t('promptFilter.intelligence.aiLearned')}</Badge>
                <Badge variant="outline">{t('promptFilter.intelligence.aiConfidence')}: {(aiResult.decision.confidence * 100).toFixed(0)}%</Badge>
                <Badge variant="outline">{aiResult.provider} · {aiResult.model}</Badge>
                {aiResult.evidence_basis === 'context_only' ? <Badge variant="secondary">{t('promptFilter.intelligence.aiContextOnlyBasis')}</Badge> : null}
              </div>
              <p className="text-sm">{aiResult.decision.reason || t('promptFilter.intelligence.aiNoReason')}</p>
              {aiResult.decision.rule ? (
                <div className="rounded-lg border p-3">
                  <div className="font-medium">{t('promptFilter.intelligence.aiRuleSuggestion')}</div>
                  <div className="mt-2 flex flex-wrap gap-2 text-xs">
                    <Badge variant="outline">{aiResult.decision.rule.name}</Badge>
                    <Badge variant="outline">{aiResult.decision.rule.category}</Badge>
                    <Badge variant="outline">{t('promptFilter.intelligence.weight')}: {aiResult.decision.rule.weight}</Badge>
                  </div>
                  <code className="mt-2 block break-all rounded bg-muted/40 p-2 text-xs">{aiResult.decision.rule.pattern}</code>
                  {aiResult.rule_candidate ? <p className="mt-2 text-xs text-[hsl(var(--success))]">{t('promptFilter.intelligence.aiRuleStaged')}</p> : null}
                  {aiResult.rule_error ? <p className="mt-2 text-xs text-destructive">{aiResult.rule_error}</p> : null}
                </div>
              ) : null}
              {aiResult.decision.identity_patch ? (
                <div className="rounded-lg border p-3">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="font-medium">{t('promptFilter.intelligence.aiIdentitySuggestion')}</div>
                    {aiResult.identity_update.applied ? (
                      <Badge className="bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]">{t('promptFilter.intelligence.identityAppliedBadge')}</Badge>
                    ) : aiResult.identity_update.rolled_back ? (
                      <Badge variant="outline">{t('promptFilter.intelligence.identityRolledBackBadge')}</Badge>
                    ) : aiResult.identity_update.block_reason ? (
                      <Badge variant="outline">{t('promptFilter.intelligence.identityBlockedBadge')}</Badge>
                    ) : (
                      <Badge variant="outline">{t('promptFilter.intelligence.identityPendingBadge')}</Badge>
                    )}
                  </div>
                  <ul className="mt-2 list-disc space-y-1 pl-5 text-sm">
                    {aiResult.decision.identity_patch.clauses.map((clause, index) => <li key={`${clause}-${index}`}>{clause}</li>)}
                  </ul>
                  {aiResult.identity_update.block_reason ? <p className="mt-2 text-xs text-[hsl(var(--warning))]">{aiResult.identity_update.block_reason}</p> : null}
                  <div className="mt-3 flex flex-wrap gap-2">
                    {!aiResult.identity_update.applied && aiTarget?.lifecycle_status === 'pending' ? (
                      <Button size="sm" disabled={aiLoading || Boolean(aiResult.identity_update.block_reason)} onClick={() => void applyAIIdentity()}>
                        <Save className="size-4" />
                        {t('promptFilter.intelligence.applyIdentityPatch')}
                      </Button>
                    ) : null}
                    {aiResult.identity_update.revision_evidence_id ? (
                      <Button size="sm" variant="outline" disabled={aiLoading} onClick={() => void rollbackAIIdentity(aiTarget?.id || 0, aiResult.identity_update.revision_evidence_id || 0)}>
                        {t('promptFilter.intelligence.rollbackIdentityPatch')}
                      </Button>
                    ) : null}
                  </div>
                </div>
              ) : null}
            </div>
          ) : null}
          <DialogFooter>
            <Button variant="outline" disabled={aiLoading} onClick={() => { setAITarget(null); setAIResult(null) }}>{t('common.close')}</Button>
            <Button disabled={aiLoading || !aiTarget || aiTarget.lifecycle_status !== 'pending' || Boolean(aiResult?.identity_update.applied)} onClick={() => void runAIAnalysis()}>
              <Sparkles className="size-4" />
              {aiLoading ? t('promptFilter.intelligence.aiAnalyzing') : aiResult ? t('promptFilter.intelligence.aiRunAgain') : t('promptFilter.intelligence.aiRunAnalysis')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(evidenceDialog)} onOpenChange={(open) => { if (!open) setEvidenceDialog(null) }}>
        <DialogContent className="max-h-[85vh] max-w-4xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{t('promptFilter.intelligence.evidenceTitle')}</DialogTitle>
            <DialogDescription>{evidenceDialog ? candidateTitle(evidenceDialog.candidate) : ''}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            {evidenceDialog?.evidence.map((evidence) => (
              <div key={evidence.id} className="rounded-lg border p-4">
                <div className="mb-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                  <Badge variant="outline">{sourceLabel(evidence.source_kind)}</Badge>
                  {evidence.protocol ? <span>{evidence.protocol}</span> : null}
                  {evidence.provider ? <span>{evidence.provider}</span> : null}
                  {evidence.model ? <span>{evidence.model}</span> : null}
                  {evidence.api_key_name ? <span>{evidence.api_key_name}</span> : null}
                  <span>{formatBeijingTime(evidence.observed_at)}</span>
                </div>
                {evidence.incident_id ? (
                  <div className="mb-3">
                    <div className="mb-1 text-xs font-semibold text-muted-foreground">{t('promptFilter.cyberRiskSubjects')} · <span className="font-mono font-normal">{evidence.incident_id.slice(0, 8)}</span></div>
                    <PromptRiskSubjectList subjects={evidence.risk_subjects ?? []} compact />
                  </div>
                ) : null}
                {evidence.sample_preview ? <p className="whitespace-pre-wrap break-words text-sm">{evidence.sample_preview}</p> : null}
                {evidence.source_ref ? <p className="mt-2 break-all text-xs text-muted-foreground">{t('promptFilter.intelligence.sourceReference')}: {evidence.source_ref}</p> : null}
                {Object.keys(evidence.metadata || {}).length ? <SoftCodeBlock className="mt-3">{JSON.stringify(evidence.metadata, null, 2)}</SoftCodeBlock> : null}
                {evidence.source_kind === 'ai_identity_update' && evidenceDialog ? (
                  <Button className="mt-3" size="sm" variant="outline" disabled={aiLoading} onClick={() => void rollbackAIIdentity(evidenceDialog.candidate.id, evidence.id)}>
                    {t('promptFilter.intelligence.rollbackIdentityPatch')}
                  </Button>
                ) : null}
              </div>
            ))}
            {evidenceDialog && !evidenceDialog.evidence.length ? <div className="py-8 text-center text-muted-foreground">{t('promptFilter.intelligence.noEvidence')}</div> : null}
          </div>
          <DialogFooter><Button variant="outline" onClick={() => setEvidenceDialog(null)}>{t('common.close')}</Button></DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(dismissTarget)} onOpenChange={(open) => { if (!open && candidateAction === null) setDismissTarget(null) }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t('promptFilter.intelligence.dismissTitle')}</DialogTitle>
            <DialogDescription>{t('promptFilter.intelligence.dismissDesc', { name: dismissTarget ? candidateTitle(dismissTarget) : '' })}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={candidateAction !== null} onClick={() => setDismissTarget(null)}>{t('common.cancel')}</Button>
            <Button variant="destructive" disabled={candidateAction !== null} onClick={() => void dismiss()}>{t('promptFilter.intelligence.dismiss')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(draftTarget)} onOpenChange={(open) => { if (!open && candidateAction === null) setDraftTarget(null) }}>
        <DialogContent className="max-h-[90vh] max-w-3xl overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{t('promptFilter.intelligence.createDraftTitle')}</DialogTitle>
            <DialogDescription>{t('promptFilter.intelligence.createDraftDesc')}</DialogDescription>
          </DialogHeader>
          {draftTarget?.sample_preview ? (
            <div className="rounded-lg border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-sm whitespace-pre-wrap break-words">{draftTarget.sample_preview}</div>
          ) : null}
          <div className="space-y-3 rounded-lg border border-border bg-muted/20 p-3">
            <div className="text-xs text-muted-foreground">
              <div className="font-semibold text-foreground">{t('promptFilter.intelligence.draftSuggestTitle')}</div>
              <div className="mt-0.5">{t('promptFilter.intelligence.draftSuggestHint')}</div>
            </div>
            <div className="grid gap-3 md:grid-cols-3">
              <Field label={t('promptFilter.intelligence.aiProvider')}>
                <Select
                  value={draftProvider}
                  onValueChange={(value) => setDraftProvider(value as PromptIntelligenceAIProvider)}
                  options={[
                    { value: 'account_pool', label: t('promptFilter.intelligence.aiProviderPool') },
                    { value: 'review', label: t('promptFilter.intelligence.aiProviderReview') },
                  ]}
                />
              </Field>
              <Field label={t('promptFilter.intelligence.aiModel')} hint={t('promptFilter.intelligence.aiModelHint')}>
                <Input value={draftModel} onChange={(event) => setDraftModel(event.target.value)} placeholder={t('promptFilter.intelligence.aiModelDefault')} />
              </Field>
              {draftProvider === 'account_pool' ? (
                <Field label={t('promptFilter.intelligence.aiGatewayKey')} hint={t('promptFilter.intelligence.aiGatewayKeyHint')}>
                  <Select
                    value={draftAPIKeyID}
                    onValueChange={setDraftAPIKeyID}
                    options={[
                      { value: '0', label: t('promptFilter.intelligence.aiGatewayKeyAuto') },
                      ...gatewayKeys.map((key) => ({ value: String(key.id), label: `${key.name || `#${key.id}`} · ${key.masked}` })),
                    ]}
                  />
                </Field>
              ) : null}
            </div>
            <div className="flex justify-end">
              <Button size="sm" variant="outline" disabled={draftSuggesting || candidateAction !== null} onClick={() => void suggestDraft()}>
                {draftSuggesting ? <Loader2 className="size-3.5 animate-spin" /> : <Sparkles className="size-3.5" />}
                {draftSuggesting ? t('promptFilter.intelligence.draftSuggesting') : t('promptFilter.intelligence.draftSuggest')}
              </Button>
            </div>
          </div>
          {draftSuggestion ? (
            <div className={cn('rounded-lg border p-3 text-xs', draftSuggestion.validation_error ? 'border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))]' : 'border-[hsl(var(--success))]/30 bg-[hsl(var(--success))]/5')}>
              <div className="flex flex-wrap items-center gap-1.5">
                <Badge variant="outline">{draftSuggestion.provider} · {draftSuggestion.model}</Badge>
                <Badge variant="outline">{t('promptFilter.intelligence.aiConfidence')}: {(draftSuggestion.confidence * 100).toFixed(0)}%</Badge>
                {draftSuggestion.evidence_basis === 'context_only' ? <Badge variant="secondary">{t('promptFilter.intelligence.aiContextOnlyBasis')}</Badge> : null}
                <Badge variant={draftSuggestion.evidence_matched > 0 ? 'outline' : 'destructive'}>{t('promptFilter.intelligence.draftSuggestMatches', { matched: draftSuggestion.evidence_matched, total: draftSuggestion.evidence_total })}</Badge>
              </div>
              {draftSuggestion.reason ? <p className="mt-2 text-sm">{draftSuggestion.reason}</p> : null}
              {draftSuggestion.validation_error ? <p className="mt-2 font-medium text-[hsl(var(--warning))]">{t('promptFilter.intelligence.draftSuggestValidation')}: {draftSuggestion.validation_error}</p> : <p className="mt-2 text-[hsl(var(--success))]">{t('promptFilter.intelligence.draftSuggestValid')}</p>}
            </div>
          ) : null}
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t('promptFilter.intelligence.draftName')}><Input value={draftForm.name} onChange={(event) => setDraftForm((current) => ({ ...current, name: event.target.value }))} /></Field>
            <Field label={t('promptFilter.intelligence.category')}><Input value={draftForm.category} onChange={(event) => setDraftForm((current) => ({ ...current, category: event.target.value }))} /></Field>
            <Field label={t('promptFilter.intelligence.weight')}><DraftNumberInput min={1} max={100} value={draftForm.weight} onValueChange={(value) => setDraftForm((current) => ({ ...current, weight: value }))} /></Field>
            <Field label={t('promptFilter.intelligence.strictLabel')}><Select value={draftForm.strict ? 'true' : 'false'} onValueChange={(value) => setDraftForm((current) => ({ ...current, strict: value === 'true' }))} options={[{ label: t('promptFilter.intelligence.strictYes'), value: 'true' }, { label: t('promptFilter.intelligence.strictNo'), value: 'false' }]} /></Field>
          </div>
          <Field label={t('promptFilter.intelligence.draftPattern')} hint={t('promptFilter.intelligence.draftPatternHint')}><Textarea rows={5} className="font-mono" value={draftForm.pattern} onChange={(event) => setDraftForm((current) => ({ ...current, pattern: event.target.value }))} /></Field>
          <Field label={t('promptFilter.intelligence.draftRationale')}><Textarea rows={3} value={draftForm.rationale} onChange={(event) => setDraftForm((current) => ({ ...current, rationale: event.target.value }))} /></Field>
          <DialogFooter>
            <Button variant="outline" disabled={candidateAction !== null} onClick={() => setDraftTarget(null)}>{t('common.cancel')}</Button>
            <Button disabled={candidateAction !== null || !draftForm.name.trim() || !draftForm.pattern.trim()} onClick={() => void createDraft()}>{t('promptFilter.intelligence.saveDraft')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(publishTarget)} onOpenChange={(open) => { if (!open && candidateAction === null) setPublishTarget(null) }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t('promptFilter.intelligence.publishTitle')}</DialogTitle>
            <DialogDescription>{t('promptFilter.intelligence.publishDesc')}</DialogDescription>
          </DialogHeader>
          {publishTarget ? (
            <div className="space-y-3 rounded-lg border p-4">
              <div className="font-medium">{candidateTitle(publishTarget)}</div>
              <code className="block break-all text-xs text-muted-foreground">{publishTarget.pattern}</code>
              <div className="flex flex-wrap gap-2">
                <Badge variant="outline">{t('promptFilter.intelligence.category')}: {publishTarget.category || '-'}</Badge>
                <Badge variant="outline">{t('promptFilter.intelligence.weight')}: {publishTarget.weight}</Badge>
                <Badge variant="outline">strict: {publishTarget.strict ? t('promptFilter.intelligence.strictYes') : t('promptFilter.intelligence.strictNo')}</Badge>
                <Badge variant="outline">{t('promptFilter.intelligence.evidenceCount')}: {publishTarget.evidence_count}</Badge>
              </div>
            </div>
          ) : null}
          <DialogFooter>
            <Button variant="outline" disabled={candidateAction !== null} onClick={() => setPublishTarget(null)}>{t('common.cancel')}</Button>
            <Button disabled={candidateAction !== null || !publishTarget} onClick={() => { if (publishTarget) void publish(publishTarget) }}>{t('promptFilter.intelligence.confirmPublish')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
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
          <div className="pointer-events-none absolute -bottom-20 right-20 size-40 rounded-full bg-[hsl(var(--info))]/10 blur-3xl" />
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
                          card.tone === 'warn' && 'border-[hsl(var(--warning))]/25 bg-[hsl(var(--warning-bg))]',
                          card.tone === 'danger' && 'border-destructive/30 bg-destructive/10',
                          card.tone === 'success' && 'border-[hsl(var(--success))]/25 bg-[hsl(var(--success-bg))]',
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
                  <div className="flex gap-3 rounded-xl border border-[hsl(var(--warning))]/25 bg-[hsl(var(--warning-bg))] px-4 py-3.5">
                    <AlertTriangle className="mt-0.5 size-4 shrink-0 text-[hsl(var(--warning))]" />
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
  testResult,
  runTest,
  advancedConfigError,
  settingsSaveRevision,
  onSettingsChanged,
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
  testResult: PromptFilterTestResponse | null
  runTest: () => void
  advancedConfigError: string | null
  settingsSaveRevision: number
  onSettingsChanged: (settings: SystemSettings) => void
  onSave: () => void
}) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const stats = useMemo(() => ({
    blocks: recentLogs.filter((log) => log.action === 'block').length,
    latest: recentLogs[0]?.created_at,
  }), [recentLogs])
  const advancedDocument = useMemo(
    () => parseAdvancedConfigDocument(form.prompt_filter_advanced_config),
    [form.prompt_filter_advanced_config],
  )
  const reviewAdapter = useMemo(
    () => parseReviewAdapter(advancedDocument.value ?? {}),
    [advancedDocument.value],
  )
  const adaptiveReview = useMemo(
    () => parseAdaptiveReview(advancedDocument.value ?? {}),
    [advancedDocument.value],
  )
  const [reviewTestText, setReviewTestText] = useState('请帮我整理今天的会议纪要。')
  const [reviewTesting, setReviewTesting] = useState(false)
  const [reviewModelsLoading, setReviewModelsLoading] = useState(false)
  const [reviewModelOptions, setReviewModelOptions] = useState<string[]>([])
  const [reviewTestResult, setReviewTestResult] = useState<PromptReviewTestResponse | null>(null)
  const [configuredReviewKeys, setConfiguredReviewKeys] = useState<PromptReviewAPIKeyDescriptor[]>([])
  const [reviewKeysLoading, setReviewKeysLoading] = useState(false)
  const [reviewKeysRefreshTick, setReviewKeysRefreshTick] = useState(0)
  const [deletingReviewKeyID, setDeletingReviewKeyID] = useState<string | null>(null)
  const [reviewProfiles, setReviewProfiles] = useState<PromptReviewProfile[]>([])
  const [reviewProfilesLoading, setReviewProfilesLoading] = useState(false)
  const [reviewProfileActionID, setReviewProfileActionID] = useState<string | null>(null)
  const { confirm, confirmDialog } = useConfirmDialog()
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [reviewSettingsOpen, setReviewSettingsOpen] = useState(false)
  const [newAPISettingsOpen, setNewAPISettingsOpen] = useState(false)
  const [expertSettingsOpen, setExpertSettingsOpen] = useState(false)
  const [recommendedStrength, setRecommendedStrength] = useState<RecommendedProtectionStrength>('block')
  const advancedProtection = useMemo(
    () => parseAdvancedProtection(advancedDocument.value ?? {}),
    [advancedDocument.value],
  )
  const protectionStrategy = form.prompt_filter_enabled ? form.prompt_filter_mode : 'off'
  const reviewStrategy = !form.prompt_filter_review_enabled
    ? 'off'
    : form.prompt_filter_review_fail_closed ? 'fail_closed' : 'fail_open'
  const draftReviewKeys = useMemo(
    () => parsePromptReviewAPIKeyInput(form.prompt_filter_review_api_key ?? ''),
    [form.prompt_filter_review_api_key],
  )
  const reviewKeyCount = draftReviewKeys.length || form.prompt_filter_review_api_key_count
  const enabledAdvancedFeatures = [
    advancedProtection.normalization.enabled ? t('promptFilter.enabledFeatures.normalization') : null,
    advancedProtection.context_discount.enabled ? t('promptFilter.enabledFeatures.contextDiscount') : null,
    advancedProtection.risk.enabled ? t('promptFilter.enabledFeatures.risk') : null,
    advancedProtection.sidecar.enabled ? t('promptFilter.enabledFeatures.sidecar') : null,
    advancedProtection.session.enabled ? t('promptFilter.enabledFeatures.session') : null,
    advancedProtection.attachment.enabled ? t('promptFilter.enabledFeatures.attachment') : null,
    advancedProtection.output.enabled ? t('promptFilter.enabledFeatures.output') : null,
    advancedProtection.intelligence.enabled ? t('promptFilter.enabledFeatures.intelligence') : null,
    adaptiveReview.enabled ? t('promptFilter.enabledFeatures.adaptiveReview') : null,
  ].filter((label): label is string => Boolean(label))
  const updateProtectionStrategy = (value: string) => {
    setForm((current) => value === 'off'
      ? { ...current, prompt_filter_enabled: false }
      : { ...current, prompt_filter_enabled: true, prompt_filter_mode: value })
  }
  const updateReviewStrategy = (value: string) => {
    setReviewTestResult(null)
    setForm((current) => value === 'off'
      ? { ...current, prompt_filter_review_enabled: false }
      : {
          ...current,
          prompt_filter_review_enabled: true,
          prompt_filter_review_fail_closed: value === 'fail_closed',
        })
  }
  const updateEnforcementSetting = (key: 'conversation_lock_enabled' | 'user_cyber_cooldown_minutes', value: boolean | number) => {
    const patched = patchAdvancedConfigDocument(form.prompt_filter_advanced_config, [{ path: ['enforcement', key], value }])
    if (!patched.ok) {
      showToast(t('promptFilter.advancedConfigInvalidSave'), 'error')
      return
    }
    setForm((current) => ({ ...current, prompt_filter_advanced_config: patched.serialized }))
  }
  const updateReviewAdapter = <K extends keyof ReviewAdapterFormConfig>(key: K, value: ReviewAdapterFormConfig[K]) => {
    const patched = patchAdvancedConfigDocument(form.prompt_filter_advanced_config, [{ path: ['review_adapter', key], value }])
    if (!patched.ok) {
      showToast(t('promptFilter.advancedConfigInvalidSave'), 'error')
      return
    }
    setReviewTestResult(null)
    setForm((current) => ({ ...current, prompt_filter_advanced_config: patched.serialized }))
  }
  const updateModerationThreshold = (category: string, percent: number) => {
    updateReviewAdapter('moderation_thresholds', {
      ...reviewAdapter.moderation_thresholds,
      [category]: Math.min(1, Math.max(0, percent / 100)),
    })
  }
  const resetModerationThresholds = () => {
    updateReviewAdapter('moderation_thresholds', { ...defaultReviewAdapter.moderation_thresholds })
  }
  const updateAdaptiveReview = (enabled: boolean) => {
    const patches = enabled
      ? [
          { path: ['adaptive_review', 'enabled'], value: true },
          { path: ['adaptive_review', 'min_clean_reviews'], value: 3 },
          { path: ['adaptive_review', 'min_observation_hours'], value: 1 },
          { path: ['adaptive_review', 'sample_percent'], value: 5 },
          { path: ['adaptive_review', 'force_review_interval_minutes'], value: 360 },
          { path: ['adaptive_review', 'trust_duration_hours'], value: 168 },
          { path: ['adaptive_review', 'reactivation_clean_reviews'], value: 3 },
          { path: ['adaptive_review', 'reactivation_cooldown_hours'], value: 24 },
        ]
      : [{ path: ['adaptive_review', 'enabled'], value: false }]
    const patched = patchAdvancedConfigDocument(form.prompt_filter_advanced_config, patches)
    if (!patched.ok) {
      showToast(t('promptFilter.advancedConfigInvalidSave'), 'error')
      return
    }
    setForm((current) => ({ ...current, prompt_filter_advanced_config: patched.serialized }))
  }
  const fetchReviewModels = async () => {
    setReviewModelsLoading(true)
    try {
      const result = await api.listPromptReviewModels({
        base_url: form.prompt_filter_review_base_url?.trim() || undefined,
        api_key: form.prompt_filter_review_api_key?.trim() || undefined,
        timeout_seconds: form.prompt_filter_review_timeout_seconds || undefined,
      })
      setReviewModelOptions(result.models)
      if (!result.models.length) {
        showToast(t('promptFilter.reviewModelsEmpty'), 'error')
      }
    } catch (err) {
      showToast(`${t('promptFilter.reviewModelsFetchFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setReviewModelsLoading(false)
    }
  }
  const runReviewConnectionTest = async () => {
    const text = reviewTestText.trim()
    if (!text) {
      showToast(t('promptFilter.testEmpty'), 'error')
      return
    }
    setReviewTesting(true)
    setReviewTestResult(null)
    try {
      const result = await api.testPromptReview({
        text,
        api_key: form.prompt_filter_review_api_key?.trim() || undefined,
        base_url: form.prompt_filter_review_base_url,
        model: form.prompt_filter_review_model,
        request_mode: reviewAdapter.request_mode,
        system_prompt: reviewAdapter.system_prompt,
        user_prompt_template: reviewAdapter.user_prompt_template,
        payload_template: reviewAdapter.payload_template,
        confidence_threshold: reviewAdapter.confidence_threshold,
        moderation_thresholds: reviewAdapter.moderation_thresholds,
        timeout_seconds: form.prompt_filter_review_timeout_seconds,
        max_concurrent: reviewAdapter.max_concurrent,
        max_text_length: reviewAdapter.max_text_length,
        test_all_keys: true,
      })
      setReviewTestResult(result)
      showToast(t('promptFilter.reviewTestSuccess'))
    } catch (err) {
      showToast(`${t('promptFilter.reviewTestFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setReviewTesting(false)
    }
  }
  const refreshReviewProfiles = async () => {
    setReviewProfilesLoading(true)
    try {
      const result = await api.listPromptReviewProfiles()
      setReviewProfiles(result.profiles ?? [])
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setReviewProfilesLoading(false)
    }
  }
  const saveReviewProfile = async () => {
    const name = window.prompt(t('promptFilter.reviewProfileNamePrompt'))?.trim()
    if (!name) return
    setReviewProfileActionID('new')
    try {
      const result = await api.savePromptReviewProfile({
        name,
        base_url: form.prompt_filter_review_base_url,
        model: form.prompt_filter_review_model,
        request_mode: reviewAdapter.request_mode,
        api_key: form.prompt_filter_review_api_key?.trim() || undefined,
        adapter_json: JSON.stringify(reviewAdapter),
        timeout_seconds: form.prompt_filter_review_timeout_seconds,
      })
      setReviewProfiles((current) => [result, ...current.filter((profile) => profile.id !== result.id)])
      showToast(t('promptFilter.reviewProfileSaved'))
    } catch (err) {
      showToast(`${t('promptFilter.reviewProfileSaveFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setReviewProfileActionID(null)
    }
  }
  const activateReviewProfile = async (profile: PromptReviewProfile) => {
    setReviewProfileActionID(profile.id)
    try {
      await api.activatePromptReviewProfile(profile.id)
      const settings = await api.getSettings()
      setForm(normalizePromptFilterForm(settings))
      onSettingsChanged(settings)
      await refreshReviewProfiles()
      showToast(t('promptFilter.reviewProfileActivated'))
    } catch (err) {
      showToast(`${t('promptFilter.reviewProfileActivateFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setReviewProfileActionID(null)
    }
  }
  const deleteReviewProfile = async (profile: PromptReviewProfile) => {
    if (!window.confirm(t('promptFilter.reviewProfileDeleteConfirm', { name: profile.name }))) return
    setReviewProfileActionID(profile.id)
    try {
      await api.deletePromptReviewProfile(profile.id)
      setReviewProfiles((current) => current.filter((item) => item.id !== profile.id))
      showToast(t('promptFilter.reviewProfileDeleted'))
    } catch (err) {
      showToast(`${t('promptFilter.reviewProfileDeleteFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setReviewProfileActionID(null)
    }
  }
  useEffect(() => {
    if (!reviewSettingsOpen) return
    let cancelled = false
    setReviewKeysLoading(true)
    void api.getPromptReviewAPIKeys()
      .then((result) => {
        if (!cancelled) setConfiguredReviewKeys(result.items)
      })
      .catch((err) => {
        if (!cancelled) showToast(getErrorMessage(err), 'error')
      })
      .finally(() => {
        if (!cancelled) setReviewKeysLoading(false)
      })
    return () => { cancelled = true }
  }, [reviewSettingsOpen, settingsSaveRevision, reviewKeysRefreshTick, showToast])
  useEffect(() => {
    if (reviewSettingsOpen) void refreshReviewProfiles()
  }, [reviewSettingsOpen])
  const deleteReviewKey = async (keyID: string, masked: string) => {
    const approved = await confirm({
      title: t('promptFilter.reviewKeyDeleteTitle'),
      description: t('promptFilter.reviewKeyDeleteConfirm', { key: masked }),
      confirmText: t('common.delete'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!approved) return
    setDeletingReviewKeyID(keyID)
    try {
      const result = await api.deletePromptReviewAPIKey(keyID)
      setConfiguredReviewKeys(result.items)
      // 保存触发的列表拉取可能仍在途,其删除前快照晚到会把已删 Key“复活”;
      // 换代重拉一次,顺带让旧请求走 cancelled 丢弃。
      setReviewKeysRefreshTick((tick) => tick + 1)
      setForm((current) => ({
        ...current,
        prompt_filter_review_api_key_configured: result.count > 0,
        prompt_filter_review_api_key_count: result.count,
      }))
      setReviewTestResult((current) => current ? {
        ...current,
        key_count: result.count,
        results: current.results?.filter((item) => item.key_id !== keyID),
      } : null)
      showToast(t('promptFilter.reviewKeyDeleted', { key: masked }))
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setDeletingReviewKeyID(null)
    }
  }
  const removeFailedReviewTestKey = async (item: PromptReviewKeyTestResult) => {
    if (!item.key_id) return
    const masked = item.key_masked || `Key #${item.key_index}`
    if (draftReviewKeys.length > 0) {
      const draftIndex = item.key_index - 1
      if (draftIndex < 0 || draftIndex >= draftReviewKeys.length) return
      const remaining = draftReviewKeys.filter((_, index) => index !== draftIndex)
      setForm((current) => ({ ...current, prompt_filter_review_api_key: remaining.join('\n') }))
      // key_index 是位置映射,移除后剩余卡片的下标即失效;整组测试结果作废,
      // 保留会让下一次点击删错 Key。
      setReviewTestResult(null)
      showToast(t(remaining.length === 0 ? 'promptFilter.reviewKeyRemovedFromDraftEmpty' : 'promptFilter.reviewKeyRemovedFromDraft', { key: masked }))
      return
    }
    if (configuredReviewKeys.some((key) => key.id === item.key_id)) {
      await deleteReviewKey(item.key_id, masked)
    }
  }
  const applyRecommendedProtection = () => {
    setForm((current) => {
      const patched = patchAdvancedConfigDocument(current.prompt_filter_advanced_config, [
        { path: ['adaptive_review', 'enabled'], value: true },
        { path: ['adaptive_review', 'min_clean_reviews'], value: 3 },
        { path: ['adaptive_review', 'min_observation_hours'], value: 1 },
        { path: ['adaptive_review', 'sample_percent'], value: 5 },
        { path: ['adaptive_review', 'force_review_interval_minutes'], value: 360 },
        { path: ['adaptive_review', 'trust_duration_hours'], value: 168 },
        { path: ['adaptive_review', 'reactivation_clean_reviews'], value: 3 },
        { path: ['adaptive_review', 'reactivation_cooldown_hours'], value: 24 },
        { path: ['review_adapter', 'circuit_breaker_failures'], value: 3 },
        { path: ['review_adapter', 'circuit_breaker_seconds'], value: 30 },
      ])
      return {
        ...current,
        prompt_filter_enabled: true,
        prompt_filter_mode: recommendedStrength === 'monitor' ? 'monitor' : 'block',
        prompt_filter_strict_terminal_enabled: recommendedStrength !== 'monitor',
        prompt_filter_log_matches: true,
        prompt_filter_review_timeout_seconds: Math.min(current.prompt_filter_review_timeout_seconds || 8, 8),
        prompt_filter_advanced_config: patched.ok ? patched.serialized : current.prompt_filter_advanced_config,
      }
    })
    showToast(t('promptFilter.recommendedAppliedWithStrength', { strength: t(`promptFilter.recommendedStrength.${recommendedStrength}.label`) }))
  }

  return (
    <>
      <div className="mb-4 grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3">
        <StatTile
          label={t('promptFilter.status')}
          value={(
            <Badge variant={form.prompt_filter_enabled ? 'default' : 'outline'}>
              {form.prompt_filter_enabled ? t('common.enabled') : t('common.disabled')}
            </Badge>
          )}
        />
        <StatTile
          label={t('promptFilter.currentMode')}
          value={modeOptions.find((item) => item.value === form.prompt_filter_mode)?.label ?? t('promptFilter.unknownMode')}
        />
        <StatTile label={t('promptFilter.recentBlockedLogs')} value={stats.blocks} />
        <StatTile label={t('promptFilter.totalLogs')} value={totalLogs} />
        <StatTile
          label={t('promptFilter.latestLog')}
          value={stats.latest ? formatRelativeTime(stats.latest, { variant: 'compact' }) : '-'}
        />
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,0.9fr)_minmax(420px,1.1fr)]">
        <Card className="border-primary/20 bg-primary/[0.025]">
          <CardContent className="space-y-5">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <div className="flex items-center gap-2 font-semibold"><Shield className="size-4 text-primary" />{t('promptFilter.protectionSummaryTitle')}</div>
                <p className="mt-1 text-sm leading-6 text-muted-foreground">{t('promptFilter.protectionSummaryDesc')}</p>
              </div>
              <Button onClick={() => setAdvancedOpen(true)}>
                <Pencil className="size-4" />
                {t('promptFilter.manageProtection')}
              </Button>
            </div>

            <div className="grid gap-3 sm:grid-cols-2">
              <div className="rounded-lg border bg-background/80 p-3">
                <div className="text-xs text-muted-foreground">{t('promptFilter.protectionStrategy')}</div>
                <div className="mt-1 flex items-center gap-2 text-sm font-semibold">
                  <Badge variant={form.prompt_filter_enabled ? 'default' : 'outline'}>
                    {protectionStrategy === 'off'
                      ? t('promptFilter.strategyOff')
                      : modeOptions.find((item) => item.value === protectionStrategy)?.label}
                  </Badge>
                </div>
              </div>
              <div className="rounded-lg border bg-background/80 p-3">
                <div className="text-xs text-muted-foreground">{t('promptFilter.reviewStrategy')}</div>
                <div className="mt-1 text-sm font-semibold">
                  {reviewStrategy === 'off'
                    ? t('promptFilter.reviewStrategyOff')
                    : reviewStrategy === 'fail_closed'
                      ? t('promptFilter.reviewStrategyFailClosed')
                      : t('promptFilter.reviewStrategyFailOpen')}
                </div>
                <div className="mt-1 truncate text-xs text-muted-foreground">{form.prompt_filter_review_model || '-'}</div>
                {adaptiveReview.enabled ? <Badge className="mt-2" variant="secondary">{t('promptFilter.adaptiveReview.activeBadge')}</Badge> : null}
              </div>
              <div className="rounded-lg border bg-background/80 p-3 sm:col-span-2">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="text-xs text-muted-foreground">{t('promptFilter.enabledFeaturesTitle')}</div>
                  <Badge variant="outline">{t('promptFilter.reviewKeyCount', { count: reviewKeyCount })}</Badge>
                </div>
                <div className="mt-2 flex flex-wrap gap-1.5">
                  {enabledAdvancedFeatures.length ? enabledAdvancedFeatures.map((label) => (
                    <Badge key={label} variant="secondary">{label}</Badge>
                  )) : (
                    <span className="text-sm text-muted-foreground">{t('promptFilter.enabledFeaturesEmpty')}</span>
                  )}
                </div>
              </div>
            </div>

            <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-4">
              <div className="flex flex-wrap gap-1.5">
                <Badge variant="outline">Responses / Chat / Messages / Images</Badge>
                <Badge variant="outline">HTTP / SSE / WebSocket</Badge>
                <Badge variant="outline">{t('promptFilter.coverageEveryRequest')}</Badge>
              </div>
              <Button variant="ghost" asChild>
                <NavLink to="/prompt-filter/intelligence"><ClipboardCheck className="size-4" />{t('promptFilter.openLearningReview')}</NavLink>
              </Button>
            </div>
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
              {testResult ? <TestDecisionBadge result={testResult} /> : null}
            </div>
            {testResult ? <PromptFilterTestResultPanel result={testResult} /> : null}
          </CardContent>
        </Card>
      </div>

      <Dialog open={advancedOpen} onOpenChange={setAdvancedOpen}>
        <DialogContent className="flex max-h-[92vh] w-[calc(100vw-2rem)] max-w-none flex-col gap-0 overflow-hidden p-0 sm:max-w-6xl sm:p-0">
          <DialogHeader className="border-b px-5 pb-4 pt-5 pr-12 sm:px-6 sm:pt-6 sm:pr-12"><DialogTitle>{t('promptFilter.advancedTitle')}</DialogTitle><DialogDescription>{t('promptFilter.advancedDescription')}</DialogDescription></DialogHeader>
          <div className="thin-scrollbar min-h-0 flex-1 space-y-4 overflow-y-auto px-5 py-4 sm:px-6">
            <div className="rounded-xl border bg-muted/20 p-4">
              <div className="mb-4">
                <h3 className="text-sm font-semibold">{t('promptFilter.dailyPolicyTitle')}</h3>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.dailyPolicyDesc')}</p>
              </div>
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
                <Field label={t('promptFilter.protectionStrategy')}>
                  <Select
                    value={protectionStrategy}
                    onValueChange={updateProtectionStrategy}
                    options={[
                      { label: t('promptFilter.strategyOff'), value: 'off' },
                      ...modeOptions,
                    ]}
                  />
                </Field>
                <Field label={t('promptFilter.reviewStrategy')}>
                  <Select
                    value={reviewStrategy}
                    onValueChange={updateReviewStrategy}
                    options={[
                      { label: t('promptFilter.reviewStrategyOff'), value: 'off' },
                      { label: t('promptFilter.reviewStrategyFailOpen'), value: 'fail_open' },
                      { label: t('promptFilter.reviewStrategyFailClosed'), value: 'fail_closed' },
                    ]}
                  />
                </Field>
                <Field label={t('promptFilter.conversationLockEnabled')} hint={t('promptFilter.help.conversationLockEnabled')}>
                  <Select
                    value={advancedProtection.enforcement.conversation_lock_enabled ? 'true' : 'false'}
                    onValueChange={(value) => updateEnforcementSetting('conversation_lock_enabled', value === 'true')}
                    options={booleanOptions}
                  />
                </Field>
                <Field label={t('promptFilter.userCyberCooldownMinutes')} hint={t('promptFilter.help.userCyberCooldownMinutes')}>
                  <DraftNumberInput
                    min={1}
                    max={1440}
                    disabled={!advancedProtection.enforcement.conversation_lock_enabled}
                    value={advancedProtection.enforcement.user_cyber_cooldown_minutes}
                    onValueChange={(value) => updateEnforcementSetting('user_cyber_cooldown_minutes', value)}
                  />
                </Field>
              </div>
              <p className="mt-3 rounded-md border border-primary/15 bg-primary/[0.04] px-3 py-2 text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewScopeHint')}</p>
            </div>

            <div className="rounded-xl border p-4">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 text-sm font-semibold"><Activity className="size-4 text-primary" />{t('promptFilter.reviewServiceSummary')}</div>
                  <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewServiceDesc', { model: form.prompt_filter_review_model || '-', count: reviewKeyCount })}</p>
                </div>
                <Button type="button" variant="outline" onClick={() => setReviewSettingsOpen((open) => !open)}>
                  {reviewSettingsOpen ? t('promptFilter.collapseReviewService') : t('promptFilter.configureReviewService')}
                  <ChevronDown className={cn('size-4 transition-transform', reviewSettingsOpen && 'rotate-180')} />
                </Button>
              </div>

              {reviewSettingsOpen ? (
                <div className="mt-4 space-y-4 border-t pt-4">
                  <div className="flex items-start justify-between gap-4 rounded-lg border border-primary/15 bg-primary/[0.04] p-4">
                    <div className="min-w-0">
                      <div className="text-sm font-semibold">{t('promptFilter.adaptiveReview.title')}</div>
                      <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.adaptiveReview.description')}</p>
                      <p className="mt-1 text-[11px] leading-5 text-muted-foreground">{t('promptFilter.adaptiveReview.defaults', { minClean: adaptiveReview.min_clean_reviews, hours: adaptiveReview.min_observation_hours, sample: adaptiveReview.sample_percent, forceHours: Math.ceil(adaptiveReview.force_review_interval_minutes / 60) })}</p>
                    </div>
                    <Switch checked={adaptiveReview.enabled} disabled={!form.prompt_filter_review_enabled} onCheckedChange={updateAdaptiveReview} />
                  </div>
                  <div className="grid gap-4 sm:grid-cols-3">
                    <Field label={t('promptFilter.reviewBaseUrl')}>
                      <Input value={form.prompt_filter_review_base_url} placeholder="https://api.deepseek.com" onChange={(event) => setForm((current) => ({ ...current, prompt_filter_review_base_url: event.target.value }))} />
                    </Field>
                    <Field label={t('promptFilter.reviewModel')}>
                      <div className="flex gap-2">
                        <Input className="min-w-0 flex-1" value={form.prompt_filter_review_model} placeholder="deepseek-v4-flash" onChange={(event) => setForm((current) => ({ ...current, prompt_filter_review_model: event.target.value }))} />
                        <Button
                          type="button"
                          variant="outline"
                          className="shrink-0"
                          disabled={reviewModelsLoading}
                          title={t('promptFilter.reviewModelsFetchHint')}
                          onClick={() => void fetchReviewModels()}
                        >
                          {reviewModelsLoading ? <RefreshCw className="size-4 animate-spin" /> : t('promptFilter.reviewModelsFetch')}
                        </Button>
                      </div>
                      {reviewModelOptions.length > 0 ? (
                        <Select
                          className="mt-2"
                          value={reviewModelOptions.includes(form.prompt_filter_review_model) ? form.prompt_filter_review_model : ''}
                          placeholder={t('promptFilter.reviewModelsPick', { count: reviewModelOptions.length })}
                          options={reviewModelOptions.map((model) => ({ value: model, label: model }))}
                          onValueChange={(model) => setForm((current) => ({ ...current, prompt_filter_review_model: model }))}
                        />
                      ) : null}
                    </Field>
                    <Field label={t('promptFilter.reviewTimeout')}>
                      <DraftNumberInput min={1} max={60} value={form.prompt_filter_review_timeout_seconds} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_review_timeout_seconds: value }))} />
                    </Field>
                  </div>
                  <div className="space-y-3 rounded-lg border border-primary/15 bg-primary/[0.04] p-4">
                    <div className="flex flex-wrap items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold">{t('promptFilter.reviewProfilesTitle')}</div>
                        <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewProfilesHint')}</p>
                      </div>
                      <Button type="button" variant="outline" size="sm" onClick={() => void saveReviewProfile()} disabled={reviewProfileActionID !== null}>
                        <Save className="size-4" /> {t('promptFilter.reviewProfileSave')}
                      </Button>
                    </div>
                    {reviewProfilesLoading ? <div className="text-xs text-muted-foreground">{t('common.loading')}</div> : null}
                    {reviewProfiles.length > 0 ? (
                      <div className="grid gap-2 sm:grid-cols-2">
                        {reviewProfiles.map((profile) => (
                          <div key={profile.id} className="flex items-center justify-between gap-3 rounded-md border bg-background px-3 py-2">
                            <div className="min-w-0">
                              <div className="flex items-center gap-2">
                                <span className="truncate text-xs font-semibold">{profile.name}</span>
                                {profile.active ? <Badge variant="default">{t('promptFilter.reviewProfileActive')}</Badge> : null}
                              </div>
                              <div className="mt-1 truncate text-[11px] text-muted-foreground">{profile.base_url} · {profile.model} · {profile.key_count} keys</div>
                            </div>
                            <div className="flex shrink-0 items-center gap-1">
                              {!profile.active ? <Button type="button" size="sm" variant="outline" disabled={reviewProfileActionID !== null} onClick={() => void activateReviewProfile(profile)}>{t('promptFilter.reviewProfileActivate')}</Button> : null}
                              <Button type="button" size="icon" variant="ghost" className="text-destructive hover:text-destructive" disabled={reviewProfileActionID !== null} onClick={() => void deleteReviewProfile(profile)} aria-label={t('promptFilter.reviewProfileDelete')}>
                                <Trash2 className="size-4" />
                              </Button>
                            </div>
                          </div>
                        ))}
                      </div>
                    ) : <div className="text-xs text-muted-foreground">{t('promptFilter.reviewProfilesEmpty')}</div>}
                  </div>
                  <Field label={t('promptFilter.reviewApiKey')}>
                    <Textarea
                      rows={3}
                      className="font-mono"
                      value={form.prompt_filter_review_api_key ?? ''}
                      placeholder={form.prompt_filter_review_api_key_configured ? t('promptFilter.reviewApiKeyConfigured', { n: form.prompt_filter_review_api_key_count }) : t('promptFilter.reviewApiKeyPlaceholder')}
                      onChange={(event) => {
                        setForm((current) => ({ ...current, prompt_filter_review_api_key: event.target.value }))
                        // 草稿变更后旧测试结果的 key_index 映射即失效,保留会误删。
                        setReviewTestResult(null)
                      }}
                    />
                    <span className="block text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewApiKeyHint')}</span>
                  </Field>
                  {configuredReviewKeys.length > 0 ? (
                    <div className="space-y-2 rounded-lg border bg-muted/15 p-3">
                      <div className="flex items-center justify-between gap-3">
                        <div>
                          <div className="text-xs font-semibold">{t('promptFilter.reviewKeyListTitle')}</div>
                          <p className="mt-0.5 text-[11px] text-muted-foreground">{t('promptFilter.reviewKeyListHint')}</p>
                        </div>
                        <Badge variant="outline">{reviewKeysLoading ? '…' : configuredReviewKeys.length}</Badge>
                      </div>
                      <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
                        {configuredReviewKeys.map((key) => {
                          const tested = reviewTestResult?.results?.find((item) => item.key_id ? item.key_id === key.id : item.key_index === key.index)
                          return (
                            <div key={key.id} className="flex items-center justify-between gap-2 rounded-md border bg-background px-3 py-2">
                              <div className="min-w-0">
                                <div className="flex items-center gap-2">
                                  <span className="text-[11px] font-medium text-muted-foreground">#{key.index}</span>
                                  <span className="truncate font-mono text-xs">{key.masked}</span>
                                  {tested ? <Badge variant={tested.ok ? 'default' : 'destructive'}>{tested.ok ? t('common.success') : t('common.failed')}</Badge> : null}
                                </div>
                                {tested ? <div className="mt-0.5 truncate text-[10px] text-muted-foreground">{tested.latency_ms} ms · {tested.ok ? tested.confidence.toFixed(2) : tested.error || '-'}</div> : null}
                              </div>
                              <Button type="button" size="icon" variant="ghost" className="shrink-0 text-destructive hover:text-destructive" disabled={deletingReviewKeyID !== null} onClick={() => void deleteReviewKey(key.id, key.masked)} aria-label={t('promptFilter.reviewKeyDeleteAria', { key: key.masked })}>
                                {deletingReviewKeyID === key.id ? <RefreshCw className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
                              </Button>
                            </div>
                          )
                        })}
                      </div>
                    </div>
                  ) : null}
                  <div className="grid gap-4 sm:grid-cols-3">
                    <Field label={t('promptFilter.reviewRequestMode')}><Select value={reviewAdapter.request_mode} onValueChange={(value) => updateReviewAdapter('request_mode', value as ReviewAdapterFormConfig['request_mode'])} options={[{ label: t('promptFilter.reviewModeChat'), value: 'chat_completions' }, { label: t('promptFilter.reviewModeModerations'), value: 'moderations' }]} /></Field>
                    <Field label={t('promptFilter.reviewScope')}><Select value={reviewAdapter.scope} onValueChange={(value) => updateReviewAdapter('scope', value as ReviewAdapterFormConfig['scope'])} options={(['all_requests', 'local_candidates', 'local_blocks'] as ReviewAdapterFormConfig['scope'][]).map((scope) => ({ label: t(`promptFilter.reviewScopeOptions.${scope}`), value: scope }))} /></Field>
                    {reviewAdapter.request_mode === 'chat_completions' ? <Field label={t('promptFilter.reviewConfidenceThreshold')}><DraftNumberInput integer={false} step="0.01" min={0.01} max={1} value={reviewAdapter.confidence_threshold} onValueChange={(value) => updateReviewAdapter('confidence_threshold', value)} /></Field> : null}
                  </div>
                  {reviewAdapter.request_mode === 'moderations' ? (
                    <div className="space-y-3 rounded-lg border bg-muted/20 p-4">
                      <div className="flex flex-wrap items-start justify-between gap-3">
                        <div>
                          <div className="text-sm font-semibold">{t('promptFilter.moderationThresholds')}</div>
                          <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.moderationThresholdsHint')}</p>
                        </div>
                        <Button type="button" variant="outline" size="sm" onClick={resetModerationThresholds}>{t('promptFilter.moderationThresholdsReset')}</Button>
                      </div>
                      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                        {moderationThresholdCategories.map((category) => (
                          <Field
                            key={category}
                            label={`${t(`promptFilter.moderationCategories.${category}`, { defaultValue: category })} (${category})`}
                            hint={t('promptFilter.moderationThresholdDefault', { percent: defaultReviewAdapter.moderation_thresholds[category] * 100 })}
                          >
                            <div className="flex items-center gap-2">
                              <DraftNumberInput integer={false} step="0.1" min={0} max={100} value={reviewAdapter.moderation_thresholds[category] * 100} onValueChange={(value) => updateModerationThreshold(category, value)} />
                              <span className="text-sm text-muted-foreground">%</span>
                            </div>
                          </Field>
                        ))}
                      </div>
                    </div>
                  ) : null}
                  <details className="group rounded-lg border border-foreground/10 bg-muted/10 open:bg-muted/20">
                    <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 marker:content-none [&::-webkit-details-marker]:hidden">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold">{t('promptFilter.reviewResilienceTitle')}</div>
                        <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewResilienceDesc')}</p>
                      </div>
                      <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
                    </summary>
                    <div className="grid gap-4 border-t px-4 py-4 sm:grid-cols-2 lg:grid-cols-4">
                      <Field label={t('promptFilter.reviewMaxConcurrent')}><DraftNumberInput min={1} max={256} value={reviewAdapter.max_concurrent} onValueChange={(value) => updateReviewAdapter('max_concurrent', value)} /></Field>
                      <Field label={t('promptFilter.reviewMaxTextLength')}><DraftNumberInput min={1024} max={262144} value={reviewAdapter.max_text_length} onValueChange={(value) => updateReviewAdapter('max_text_length', value)} /></Field>
                      <Field label={t('promptFilter.reviewCircuitBreakerFailures')}><DraftNumberInput min={1} max={20} value={reviewAdapter.circuit_breaker_failures} onValueChange={(value) => updateReviewAdapter('circuit_breaker_failures', value)} /></Field>
                      <Field label={t('promptFilter.reviewCircuitBreakerSeconds')}><DraftNumberInput min={1} max={3600} value={reviewAdapter.circuit_breaker_seconds} onValueChange={(value) => updateReviewAdapter('circuit_breaker_seconds', value)} /></Field>
                    </div>
                  </details>
                  <details className="group rounded-lg border border-foreground/10 bg-muted/10 open:bg-muted/20">
                    <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 marker:content-none [&::-webkit-details-marker]:hidden">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold">{t('promptFilter.reviewTemplatesTitle')}</div>
                        <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewTemplatesDesc')}</p>
                      </div>
                      <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180" />
                    </summary>
                    <div className="space-y-4 border-t px-4 py-4">
                      <Field label={t('promptFilter.reviewSystemPrompt')} hint={t('promptFilter.reviewSystemPromptHint')}><Textarea rows={16} className="font-mono text-xs leading-5" value={reviewAdapter.system_prompt} placeholder={t('promptFilter.reviewSystemPromptPlaceholder')} onChange={(event) => updateReviewAdapter('system_prompt', event.target.value)} /></Field>
                      <Field label={t('promptFilter.reviewUserPromptTemplate')} hint={t('promptFilter.reviewUserPromptTemplateHint')}><Textarea rows={9} className="font-mono text-xs leading-5" value={reviewAdapter.user_prompt_template} placeholder={t('promptFilter.reviewUserPromptTemplatePlaceholder')} onChange={(event) => updateReviewAdapter('user_prompt_template', event.target.value)} /></Field>
                      <Field label={t('promptFilter.reviewPayloadTemplate')} hint={t('promptFilter.reviewPayloadTemplateHint')}><Textarea rows={10} className="font-mono text-xs leading-5" value={reviewAdapter.payload_template} placeholder={'{\n  "model": "{{model}}",\n  "messages": [\n    {"role": "system", "content": "{{system_prompt}}"},\n    {"role": "user", "content": "{{user_prompt}}"}\n  ],\n  "temperature": 0\n}'} onChange={(event) => updateReviewAdapter('payload_template', event.target.value)} /></Field>
                    </div>
                  </details>
                  <div className="space-y-3 rounded-lg bg-muted/40 p-4">
                    <div>
                      <h3 className="text-sm font-semibold">{t('promptFilter.reviewConnectionTestTitle')}</h3>
                      <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewConnectionTestDesc')}</p>
                    </div>
                    <Textarea rows={4} value={reviewTestText} onChange={(event) => { setReviewTestText(event.target.value); setReviewTestResult(null) }} />
                    <div className="flex flex-wrap items-center gap-2">
                      <Button type="button" variant="outline" onClick={() => void runReviewConnectionTest()} disabled={reviewTesting}>
                        <Activity className="size-4" />
                        {reviewTesting ? t('promptFilter.reviewTesting') : t('promptFilter.reviewRunTest')}
                      </Button>
                      {reviewTestResult ? <Badge variant={reviewTestResult.flagged ? 'destructive' : 'default'}>{reviewTestResult.flagged ? t('promptFilter.testReviewFlagged') : t('promptFilter.testReviewCleared')}</Badge> : null}
                    </div>
                    {reviewTestResult ? (
                      <div className="grid gap-2 rounded-md bg-background p-3 text-xs sm:grid-cols-2">
                        <div>{t('promptFilter.reviewTestEndpoint')}: <span className="font-mono break-all">{reviewTestResult.endpoint}</span></div>
                        <div>{t('promptFilter.reviewTestLatency')}: {reviewTestResult.latency_ms} ms</div>
                        <div>
                          {reviewAdapter.request_mode === 'moderations' ? t('promptFilter.reviewTestDecision') : t('promptFilter.reviewTestConfidence')}:{' '}
                          {reviewAdapter.request_mode === 'moderations' ? (
                            <><span className="font-mono">{reviewTestResult.decision_category || '-'}</span>{' '}{(reviewTestResult.decision_score ?? reviewTestResult.confidence).toFixed(2)} / {typeof reviewTestResult.decision_threshold === 'number' ? reviewTestResult.decision_threshold.toFixed(2) : '-'}</>
                          ) : `${reviewTestResult.confidence.toFixed(2)} / ${reviewTestResult.confidence_threshold.toFixed(2)}`}
                        </div>
                        <div>{t('promptFilter.reviewModel')}: <span className="font-mono">{reviewTestResult.model}</span></div>
                        {reviewTestResult.highest_category ? <div>{t('promptFilter.reviewTestHighestCategory')}: <span className="font-mono">{reviewTestResult.highest_category}</span></div> : null}
                        {reviewTestResult.reason ? <div className="sm:col-span-2">{t('promptFilter.reviewTestReason')}: {reviewTestResult.reason}</div> : null}
                        {reviewTestResult.results?.length ? (
                          <div className="sm:col-span-2 mt-1 grid gap-2 md:grid-cols-3">
                            {reviewTestResult.results.map((item) => {
                              const canRemoveFailedKey = !item.ok && Boolean(item.key_id) && (
                                draftReviewKeys.length > 0 || configuredReviewKeys.some((key) => key.id === item.key_id)
                              )
                              const masked = item.key_masked || `Key #${item.key_index}`
                              return (
                                <div key={item.key_id || item.key_index} className="rounded border bg-background p-2">
                                  <div className="flex items-center justify-between gap-2">
                                    <span className="font-medium">{masked}</span>
                                    <div className="flex items-center gap-1">
                                      <Badge variant={item.ok ? 'default' : 'destructive'}>{item.ok ? t('common.success') : t('common.failed')}</Badge>
                                      {canRemoveFailedKey ? (
                                        <Button
                                          type="button"
                                          size="icon"
                                          variant="ghost"
                                          className="size-7 text-destructive hover:text-destructive"
                                          disabled={deletingReviewKeyID !== null}
                                          onClick={() => void removeFailedReviewTestKey(item)}
                                          aria-label={t(draftReviewKeys.length > 0 ? 'promptFilter.reviewKeyRemoveDraftAria' : 'promptFilter.reviewKeyDeleteAria', { key: masked })}
                                          title={t(draftReviewKeys.length > 0 ? 'promptFilter.reviewKeyRemoveDraftAria' : 'promptFilter.reviewKeyDeleteAria', { key: masked })}
                                        >
                                          {deletingReviewKeyID === item.key_id ? <RefreshCw className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
                                        </Button>
                                      ) : null}
                                    </div>
                                  </div>
                                  <div className="mt-1 text-muted-foreground">{item.latency_ms} ms · {item.ok ? item.confidence.toFixed(2) : item.error || '-'}</div>
                                </div>
                              )
                            })}
                          </div>
                        ) : null}
                      </div>
                    ) : null}
                  </div>
                </div>
              ) : null}
            </div>

            <div className="rounded-xl border p-4">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 text-sm font-semibold"><Network className="size-4 text-primary" />{t('promptFilter.newapiAdapterSummary')}</div>
                  <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.newapiAdapterDesc')}</p>
                </div>
                <Button type="button" variant="outline" onClick={() => setNewAPISettingsOpen((open) => !open)}>
                  {newAPISettingsOpen ? t('promptFilter.collapseNewAPISettings') : t('promptFilter.configureNewAPISettings')}
                  <ChevronDown className={cn('size-4 transition-transform', newAPISettingsOpen && 'rotate-180')} />
                </Button>
              </div>
              {newAPISettingsOpen ? (
                <div className="mt-4 border-t pt-4">
                  <PromptFilterNewAPIBindings />
                </div>
              ) : null}
            </div>

            <div className="rounded-xl border p-4">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 text-sm font-semibold"><Layers className="size-4 text-primary" />{t('promptFilter.expertSettingsSummary')}</div>
                  <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.expertSettingsDesc')}</p>
                </div>
                <Button type="button" variant="outline" onClick={() => setExpertSettingsOpen((open) => !open)}>
                  {expertSettingsOpen ? t('promptFilter.collapseExpertSettings') : t('promptFilter.openExpertSettings')}
                  <ChevronDown className={cn('size-4 transition-transform', expertSettingsOpen && 'rotate-180')} />
                </Button>
              </div>

              {expertSettingsOpen ? (
                <div className="mt-4 space-y-5 border-t pt-4">
                  <p className="rounded-md border border-[hsl(var(--warning))]/20 bg-[hsl(var(--warning-bg))] px-3 py-2 text-xs leading-5 text-muted-foreground">{t('promptFilter.expertSettingsWarning')}</p>
                  <div className="grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-4 rounded-lg border p-4">
                    <Field label={t('promptFilter.threshold')}><DraftNumberInput min={1} max={100} value={form.prompt_filter_threshold} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_threshold: value }))} /></Field>
                    <Field label={t('promptFilter.strictThreshold')}><DraftNumberInput min={1} max={100} value={form.prompt_filter_strict_threshold} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_strict_threshold: value }))} /></Field>
                    <Field label={t('promptFilter.strictTerminal')} hint={t('promptFilter.strictTerminalHint')}><Select value={form.prompt_filter_strict_terminal_enabled ? 'true' : 'false'} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_strict_terminal_enabled: value === 'true' }))} options={booleanOptions} /></Field>
                    <Field label={t('promptFilter.logMatches')}><Select value={form.prompt_filter_log_matches ? 'true' : 'false'} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_log_matches: value === 'true' }))} options={booleanOptions} /></Field>
                    <Field label={t('promptFilter.maxTextLength')}><DraftNumberInput min={1024} max={262144} value={form.prompt_filter_max_text_length} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_max_text_length: value }))} /></Field>
                  </div>
                  <Field label={t('promptFilter.sensitiveWords')}><Textarea rows={5} value={form.prompt_filter_sensitive_words} placeholder={t('promptFilter.sensitiveWordsPlaceholder')} onChange={(event) => setForm((current) => ({ ...current, prompt_filter_sensitive_words: event.target.value }))} /><span className="block text-xs leading-5 text-muted-foreground">{t('promptFilter.sensitiveWordsHint')}</span></Field>
                  <AdvancedProtectionEditor value={form.prompt_filter_advanced_config} onChange={(value) => setForm((current) => ({ ...current, prompt_filter_advanced_config: value }))} />
                </div>
              ) : null}
            </div>
          </div>
          <DialogFooter className="flex-wrap border-t px-5 py-4 sm:justify-between sm:px-6">
            <div className="flex flex-wrap items-end gap-2">
              <div className="min-w-[210px] space-y-1.5">
                <div className="text-xs font-semibold text-muted-foreground">{t('promptFilter.recommendedStrengthTitle')}</div>
                <Select
                  value={recommendedStrength}
                  onValueChange={(value) => setRecommendedStrength(value as RecommendedProtectionStrength)}
                  options={(['monitor', 'block'] as RecommendedProtectionStrength[]).map((strength) => ({
                    value: strength,
                    label: t(`promptFilter.recommendedStrength.${strength}.label`),
                  }))}
                />
              </div>
              <Button variant="ghost" onClick={applyRecommendedProtection}><Shield className="size-4" />{t('promptFilter.applyRecommended')}</Button>
            </div>
            <div className="flex gap-2">
              <Button variant="outline" onClick={() => setAdvancedOpen(false)}>{t('common.close')}</Button>
              <Button onClick={() => { onSave(); setAdvancedOpen(false) }} disabled={saving || Boolean(advancedConfigError)}><Save className="size-4" />{saving ? t('common.saving') : t('common.save')}</Button>
            </div>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Card className="mt-4">
        <CardContent>
          <div className="mb-4 flex items-center justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
            <SectionTitle title={t('promptFilter.recentLogsTitle')} />
            <Button variant="outline" asChild>
              <NavLink to="/prompt-filter/logs">{t('promptFilter.viewAllLogs')}</NavLink>
            </Button>
          </div>
          <PromptFilterLogsTable logs={recentLogs} compact />
        </CardContent>
      </Card>
      {confirmDialog}
    </>
  )
}

function PromptLogFilterControls({
  draftFilters,
  setDraftFilters,
  onApply,
  onReset,
  loading,
  showAction = false,
  showSource = false,
  showReviewResult = false,
}: {
  draftFilters: LogFilters
  setDraftFilters: Dispatch<SetStateAction<LogFilters>>
  onApply: () => void
  onReset: () => void
  loading: boolean
  showAction?: boolean
  showSource?: boolean
  showReviewResult?: boolean
}) {
  const { t } = useTranslation()

  return (
    <>
      <div className="mb-3 grid grid-cols-[repeat(auto-fit,minmax(160px,1fr))] gap-3">
        {showReviewResult ? (
          <Field label={t('promptFilter.reviewResultFilter')}>
            <Select
              value={draftFilters.reviewResult}
              onValueChange={(value) => setDraftFilters((current) => ({ ...current, reviewResult: value }))}
              options={[
                { label: t('common.all'), value: '' },
                { label: t('promptFilter.labels.reviewFlagged'), value: 'flagged' },
                { label: t('promptFilter.labels.reviewCleared'), value: 'cleared' },
                { label: t('promptFilter.reviewResultError'), value: 'error' },
              ]}
            />
          </Field>
        ) : null}
        {showAction ? (
          <Field label={t('promptFilter.reviewFinalAction')}>
            <Select
              value={draftFilters.action}
              onValueChange={(value) => setDraftFilters((current) => ({ ...current, action: value }))}
              options={[
                { label: t('common.all'), value: '' },
                { label: t('promptFilter.modeBlock'), value: 'block' },
                { label: t('promptFilter.modeWarn'), value: 'warn' },
                { label: t('promptFilter.actionAllow'), value: 'allow' },
              ]}
            />
          </Field>
        ) : null}
        {showSource ? (
          <Field label={t('promptFilter.source')}>
            <Select
              value={draftFilters.source}
              onValueChange={(value) => setDraftFilters((current) => ({ ...current, source: value }))}
              options={[
                { label: t('common.all'), value: '' },
                { label: t('promptFilter.sources.local_filter'), value: 'local_filter' },
                { label: t('promptFilter.sources.upstream_cyber_policy'), value: 'upstream_cyber_policy' },
              ]}
            />
          </Field>
        ) : null}
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
        <Button onClick={onApply} disabled={loading}>
          <Search className="size-4" />
          {t('promptFilter.applyFilters')}
        </Button>
        <Button variant="outline" onClick={onReset} disabled={loading}>
          <X className="size-4" />
          {t('promptFilter.resetFilters')}
        </Button>
      </div>
    </>
  )
}

type PromptLogClearSection = 'incidents' | 'review' | 'local'

function LogsView({ onPromptLogsChanged }: { onPromptLogsChanged: () => Promise<void> }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [searchParams] = useSearchParams()
  const auditReference = searchParams.get('audit')?.trim() || ''
  const initialLogFilters = () => ({ ...emptyFilters, q: auditReference })
  const initialLocalLogFilters = () => ({ ...defaultLocalLogFilters, q: auditReference })
  const [incidentDraftFilters, setIncidentDraftFilters] = useState<LogFilters>(initialLogFilters)
  const [incidentFilters, setIncidentFilters] = useState<LogFilters>(initialLogFilters)
  const [reviewDraftFilters, setReviewDraftFilters] = useState<LogFilters>(initialLogFilters)
  const [reviewFilters, setReviewFilters] = useState<LogFilters>(initialLogFilters)
  const [localDraftFilters, setLocalDraftFilters] = useState<LogFilters>(initialLocalLogFilters)
  const [localFilters, setLocalFilters] = useState<LogFilters>(initialLocalLogFilters)
  const [logPage, setLogPage] = useState(1)
  const [logPageSize, setLogPageSize] = usePersistedPageSize('prompt_filter_logs', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [reviewPage, setReviewPage] = useState(1)
  const [reviewPageSize, setReviewPageSize] = usePersistedPageSize('prompt_review_logs', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [incidentPage, setIncidentPage] = useState(1)
  const [incidentPageSize, setIncidentPageSize] = usePersistedPageSize('prompt_policy_incidents', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [logs, setLogs] = useState<PromptFilterLog[]>([])
  const [total, setTotal] = useState(0)
  const [reviewLogs, setReviewLogs] = useState<PromptFilterLog[]>([])
  const [reviewTotal, setReviewTotal] = useState(0)
  const [incidents, setIncidents] = useState<PromptPolicyIncident[]>([])
  const [incidentTotal, setIncidentTotal] = useState(0)
  const [localLoading, setLocalLoading] = useState(false)
  const [reviewLoading, setReviewLoading] = useState(false)
  const [incidentLoading, setIncidentLoading] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)
  const [reviewError, setReviewError] = useState<string | null>(null)
  const [incidentError, setIncidentError] = useState<string | null>(null)
  const [clearingSection, setClearingSection] = useState<PromptLogClearSection | null>(null)
  const [retention, setRetention] = useState<PromptLogRetention | null>(null)
  const [retentionDraft, setRetentionDraft] = useState<number>(7)
  const [retentionSaving, setRetentionSaving] = useState(false)
  const [retentionRunning, setRetentionRunning] = useState(false)
  const loadRetention = useCallback(async () => {
    try {
      const next = await api.getPromptLogRetention()
      setRetention(next)
      setRetentionDraft(next.retention_days)
      setRetentionRunning(next.running)
    } catch {
      /* 保留设置读取失败不影响日志页其它功能 */
    }
  }, [])
  useEffect(() => {
    void loadRetention()
  }, [loadRetention])
  const saveRetention = async () => {
    setRetentionSaving(true)
    try {
      const next = await api.updatePromptLogRetention(retentionDraft)
      setRetention(next)
      showToast(t('promptFilter.retention.saved', { days: next.retention_days }))
    } catch (err) {
      showToast(`${t('promptFilter.retention.saveFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setRetentionSaving(false)
    }
  }
  const runRetentionNow = async () => {
    try {
      await api.runPromptLogRetention()
      setRetentionRunning(true)
      showToast(t('promptFilter.retention.started'))
    } catch (err) {
      showToast(`${t('promptFilter.retention.runFailed')}: ${getErrorMessage(err)}`, 'error')
    }
  }
  const [auditHealth, setAuditHealth] = useState<PromptPolicyAuditHealth | null>(null)
  const [auditHealthOpen, setAuditHealthOpen] = useState(false)
  const [auditHealthLoading, setAuditHealthLoading] = useState(false)
  const [auditHealthError, setAuditHealthError] = useState<string | null>(null)

  const loadLocalLogs = useCallback(async (pageOverride?: number) => {
    setLocalLoading(true)
    setLocalError(null)
    try {
      const result = await api.getPromptFilterLogs({
        page: pageOverride ?? logPage,
        pageSize: logPageSize,
        action: localFilters.action,
        source: localFilters.source,
        endpoint: localFilters.endpoint,
        model: localFilters.model,
        apiKeyId: localFilters.apiKeyId,
        q: localFilters.q,
      })
      setLogs(result.logs ?? [])
      setTotal(result.total ?? 0)
    } catch (err) {
      setLocalError(getErrorMessage(err))
    } finally {
      setLocalLoading(false)
    }
  }, [localFilters, logPage, logPageSize])

  const loadReviewLogs = useCallback(async (pageOverride?: number) => {
    setReviewLoading(true)
    setReviewError(null)
    try {
      const result = await api.getPromptFilterLogs({
        page: pageOverride ?? reviewPage,
        pageSize: reviewPageSize,
        action: reviewFilters.action,
        endpoint: reviewFilters.endpoint,
        model: reviewFilters.model,
        apiKeyId: reviewFilters.apiKeyId,
        q: reviewFilters.q,
        reviewed: true,
        reviewResult: reviewFilters.reviewResult,
      })
      setReviewLogs(result.logs ?? [])
      setReviewTotal(result.total ?? 0)
    } catch (err) {
      setReviewError(getErrorMessage(err))
    } finally {
      setReviewLoading(false)
    }
  }, [reviewFilters, reviewPage, reviewPageSize])

  // 后台清理进行中时轮询状态，跑完后刷新日志计数。
  useEffect(() => {
    if (!retentionRunning) return
    const timer = window.setInterval(async () => {
      try {
        const next = await api.getPromptLogRetention()
        setRetention(next)
        if (!next.running) {
          setRetentionRunning(false)
          setLogPage(1)
          setReviewPage(1)
          await Promise.all([loadReviewLogs(1), loadLocalLogs(1)])
        }
      } catch {
        /* ignore */
      }
    }, 2000)
    return () => window.clearInterval(timer)
  }, [retentionRunning, loadReviewLogs, loadLocalLogs])


  const loadIncidents = useCallback(async () => {
    setIncidentLoading(true)
    setIncidentError(null)
    try {
      const result = await api.getPromptPolicyIncidents({
        page: incidentPage,
        pageSize: incidentPageSize,
        endpoint: incidentFilters.endpoint,
        model: incidentFilters.model,
        apiKeyId: incidentFilters.apiKeyId,
        q: incidentFilters.q,
      })
      setIncidents(result.incidents ?? [])
      setIncidentTotal(result.total ?? 0)
    } catch (err) {
      setIncidentError(getErrorMessage(err))
    } finally {
      setIncidentLoading(false)
    }
  }, [incidentFilters, incidentPage, incidentPageSize])

  useEffect(() => {
    void loadLocalLogs()
  }, [loadLocalLogs])

  useEffect(() => {
    void loadReviewLogs()
  }, [loadReviewLogs])

  useEffect(() => {
    void loadIncidents()
  }, [loadIncidents])

  const refreshAll = useCallback(async () => {
    await Promise.all([loadIncidents(), loadReviewLogs(), loadLocalLogs()])
  }, [loadIncidents, loadLocalLogs, loadReviewLogs])

  const showAuditHealth = async () => {
    setAuditHealthOpen(true)
    setAuditHealthLoading(true)
    setAuditHealthError(null)
    try {
      setAuditHealth(await api.getPromptPolicyAuditHealth())
    } catch (err) {
      setAuditHealthError(getErrorMessage(err))
    } finally {
      setAuditHealthLoading(false)
    }
  }

  const clearLogSection = async (section: PromptLogClearSection) => {
    setClearingSection(section)
    try {
      if (section === 'incidents') {
        await api.clearPromptPolicyIncidents()
        setIncidents([])
        setIncidentTotal(0)
        setIncidentPage(1)
        showToast(t('promptFilter.cyberIncidentsCleared'))
        return
      }

      await api.clearPromptFilterLogs(section === 'review' ? { reviewed: true } : { source: 'local_filter' })
      // 清空现已改为后台分批执行：先刷新一次，再轮询到清理结束后自动再刷新。
      setRetentionRunning(true)
      // The two panels are projections of the same persisted rows. A reviewed
      // local-filter row appears in both, so either cleanup must refresh both
      // projections instead of leaving the other panel with stale records.
      setLogPage(1)
      setReviewPage(1)
      await Promise.all([loadReviewLogs(1), loadLocalLogs(1)])
      if (section === 'review') {
        showToast(t('promptFilter.reviewLogsCleared'))
      } else {
        showToast(t('promptFilter.localLogsCleared'))
      }

      try {
        await onPromptLogsChanged()
      } catch {
        showToast(t('promptFilter.logSummaryRefreshFailed'), 'warning')
      }
    } catch (err) {
      showToast(`${t('promptFilter.clearFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setClearingSection(null)
    }
  }

  const anyLoading = localLoading || reviewLoading || incidentLoading || clearingSection !== null
  const logTotalPages = Math.max(1, Math.ceil(total / logPageSize))
  const reviewTotalPages = Math.max(1, Math.ceil(reviewTotal / reviewPageSize))
  const incidentTotalPages = Math.max(1, Math.ceil(incidentTotal / incidentPageSize))

  return (
    <Card>
      <CardContent>
        <div className="mb-4 flex items-center justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
          <div>
            <SectionTitle title={t('promptFilter.logsTitle')} />
            <p className="mt-1 text-xs text-muted-foreground">{t('promptFilter.auditRecordsCount', { incidents: incidentTotal, reviews: reviewTotal, logs: total })}</p>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" onClick={() => void refreshAll()} disabled={anyLoading}>
              <RefreshCw className="size-3.5" />
              {t('promptFilter.refreshAllLogs')}
            </Button>
          </div>
        </div>

        <div className="space-y-5">
          <section className="rounded-xl border p-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="text-sm font-semibold">{t('promptFilter.retention.title')}</div>
                <p className="mt-1 text-xs text-muted-foreground">{t('promptFilter.retention.description')}</p>
                <p className="mt-1 text-xs text-muted-foreground">
                  {retention?.last_run_at
                    ? t('promptFilter.retention.lastRun', {
                        time: new Date(retention.last_run_at).toLocaleString(),
                        logs: retention.last_deleted_logs,
                        events: retention.last_deleted_events,
                        sources: retention.last_deleted_sources,
                        seconds: (retention.last_duration_ms / 1000).toFixed(1),
                      })
                    : t('promptFilter.retention.neverRun')}
                  {retention?.last_error ? ` · ${t('promptFilter.retention.lastError', { error: retention.last_error })}` : ''}
                </p>
              </div>
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-xs text-muted-foreground">{t('promptFilter.retention.daysLabel')}</span>
                <div className="w-24">
                  <DraftNumberInput min={0} max={365} value={retentionDraft} onValueChange={(v) => setRetentionDraft(v)} />
                </div>
                <Button size="sm" variant="outline" onClick={() => void saveRetention()} disabled={retentionSaving || retention?.retention_days === retentionDraft}>
                  {retentionSaving ? t('promptFilter.retention.saving') : t('common.save')}
                </Button>
                <Button size="sm" variant="outline" onClick={() => void runRetentionNow()} disabled={retentionRunning || clearingSection !== null || (retention?.retention_days ?? 0) <= 0}>
                  {retentionRunning ? <Loader2 className="size-3.5 animate-spin" /> : <Trash2 className="size-3.5" />}
                  {retentionRunning ? t('promptFilter.retention.running') : t('promptFilter.retention.runNow')}
                </Button>
              </div>
            </div>
          </section>

          <section className="rounded-xl border p-4">
            <div className="mb-3 flex items-start justify-between gap-3">
              <div>
                <div className="text-sm font-semibold">{t('promptFilter.cyberIncidentsTitle')} · {incidentTotal}</div>
                <p className="mt-1 text-xs text-muted-foreground">{t('promptFilter.sectionRefreshHint')}</p>
              </div>
              <div className="flex flex-wrap gap-2">
                <Button size="sm" variant="outline" onClick={() => void showAuditHealth()} disabled={auditHealthLoading}>
                  <Activity className="size-3.5" />
                  {t('promptFilter.auditHealth.action')}
                </Button>
                <Button size="sm" variant="outline" onClick={() => void loadIncidents()} disabled={incidentLoading || clearingSection !== null}>
                  <RefreshCw className="size-3.5" />
                  {t('common.refresh')}
                </Button>
                <Button size="sm" variant="outline" className="text-destructive hover:text-destructive" onClick={() => void clearLogSection('incidents')} disabled={clearingSection !== null}>
                  <Trash2 className="size-3.5" />
                  {clearingSection === 'incidents' ? t('promptFilter.clearing') : t('promptFilter.clearCyberIncidents')}
                </Button>
              </div>
            </div>
            <PromptLogFilterControls
              draftFilters={incidentDraftFilters}
              setDraftFilters={setIncidentDraftFilters}
              onApply={() => { setIncidentPage(1); setIncidentFilters(incidentDraftFilters) }}
              onReset={() => { setIncidentDraftFilters(emptyFilters); setIncidentFilters(emptyFilters); setIncidentPage(1) }}
              loading={incidentLoading}
            />
            <StateShell loading={incidentLoading} error={incidentError} isEmpty={!incidentLoading && incidents.length === 0} onRetry={() => void loadIncidents()} emptyTitle={t('promptFilter.noCyberIncidents')}>
              <PromptPolicyIncidentsTable incidents={incidents} onDeleted={async () => { await loadIncidents(); setIncidentPage((current) => Math.min(current, Math.max(1, Math.ceil(Math.max(0, incidentTotal - 1) / incidentPageSize)))) }} />
              <Pagination page={incidentPage} totalPages={incidentTotalPages} totalItems={incidentTotal} pageSize={incidentPageSize} onPageChange={setIncidentPage} onPageSizeChange={(next) => { setIncidentPage(1); setIncidentPageSize(next) }} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} />
            </StateShell>
          </section>

          <section className="rounded-xl border p-4">
            <div className="mb-3 flex items-start justify-between gap-3">
              <div>
                <div className="text-sm font-semibold">{t('promptFilter.reviewHistoryTitle')} · {reviewTotal}</div>
                <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.reviewHistoryDesc')}</p>
              </div>
              <div className="flex flex-wrap gap-2">
                <Button size="sm" variant="outline" onClick={() => void loadReviewLogs()} disabled={reviewLoading || clearingSection !== null}>
                  <RefreshCw className="size-3.5" />
                  {t('common.refresh')}
                </Button>
                <Button size="sm" variant="outline" className="text-destructive hover:text-destructive" onClick={() => void clearLogSection('review')} disabled={clearingSection !== null}>
                  <Trash2 className="size-3.5" />
                  {clearingSection === 'review' ? t('promptFilter.clearing') : t('promptFilter.clearReviewLogs')}
                </Button>
              </div>
            </div>
            <PromptLogFilterControls
              draftFilters={reviewDraftFilters}
              setDraftFilters={setReviewDraftFilters}
              onApply={() => { setReviewPage(1); setReviewFilters(reviewDraftFilters) }}
              onReset={() => { setReviewDraftFilters(emptyFilters); setReviewFilters(emptyFilters); setReviewPage(1) }}
              loading={reviewLoading}
              showAction
              showReviewResult
            />
            <StateShell loading={reviewLoading} error={reviewError} isEmpty={!reviewLoading && reviewLogs.length === 0} onRetry={() => void loadReviewLogs()} emptyTitle={t('promptFilter.reviewHistoryEmpty')}>
              <PromptReviewLogsTable logs={reviewLogs} />
              <Pagination page={reviewPage} totalPages={reviewTotalPages} totalItems={reviewTotal} pageSize={reviewPageSize} onPageChange={setReviewPage} onPageSizeChange={(next) => { setReviewPage(1); setReviewPageSize(next) }} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} />
            </StateShell>
          </section>

          <section className="rounded-xl border p-4">
            <div className="mb-3 flex items-start justify-between gap-3">
              <div>
                <div className="text-sm font-semibold">{t('promptFilter.localAuditLogsTitle')} · {total}</div>
                <p className="mt-1 text-xs text-muted-foreground">{t('promptFilter.sectionRefreshHint')}</p>
              </div>
              <div className="flex flex-wrap gap-2">
                <Button size="sm" variant="outline" onClick={() => void loadLocalLogs()} disabled={localLoading || clearingSection !== null}>
                  <RefreshCw className="size-3.5" />
                  {t('common.refresh')}
                </Button>
                <Button size="sm" variant="outline" className="text-destructive hover:text-destructive" onClick={() => void clearLogSection('local')} disabled={clearingSection !== null}>
                  <Trash2 className="size-3.5" />
                  {clearingSection === 'local' ? t('promptFilter.clearing') : t('promptFilter.clearLocalLogs')}
                </Button>
              </div>
            </div>
            <PromptLogFilterControls
              draftFilters={localDraftFilters}
              setDraftFilters={setLocalDraftFilters}
              onApply={() => { setLogPage(1); setLocalFilters(localDraftFilters) }}
              onReset={() => { setLocalDraftFilters(defaultLocalLogFilters); setLocalFilters(defaultLocalLogFilters); setLogPage(1) }}
              loading={localLoading}
              showAction
              showSource
            />
            <StateShell loading={localLoading} error={localError} isEmpty={!localLoading && logs.length === 0} onRetry={() => void loadLocalLogs()} emptyTitle={t('promptFilter.noLogs')}>
              <PromptFilterLogsTable logs={logs} />
              <Pagination page={logPage} totalPages={logTotalPages} totalItems={total} pageSize={logPageSize} onPageChange={setLogPage} onPageSizeChange={(next) => { setLogPage(1); setLogPageSize(next) }} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} />
            </StateShell>
          </section>
        </div>
      </CardContent>
      <Dialog open={auditHealthOpen} onOpenChange={setAuditHealthOpen}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t('promptFilter.auditHealth.title')}</DialogTitle>
            <DialogDescription>{t('promptFilter.auditHealth.description')}</DialogDescription>
          </DialogHeader>
          {auditHealthLoading ? (
            <div className="py-8 text-center text-sm text-muted-foreground">{t('common.loading')}</div>
          ) : auditHealthError ? (
            <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">{auditHealthError}</div>
          ) : auditHealth ? (
            <div className="space-y-4">
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant={auditHealth.ok ? 'default' : 'destructive'}>
                  {auditHealth.ok ? t('promptFilter.auditHealth.healthy') : t('promptFilter.auditHealth.degraded')}
                </Badge>
                <span className="text-sm text-muted-foreground">{t('promptFilter.auditHealth.incidentCount', { count: auditHealth.incident_count })}</span>
              </div>
              <div className="grid gap-3 sm:grid-cols-3">
                <AuditHealthFlag label={t('promptFilter.auditHealth.storage')} enabled={auditHealth.storage_ready} />
                <AuditHealthFlag label={t('promptFilter.auditHealth.promptFilter')} enabled={auditHealth.prompt_filter_enabled} />
                <AuditHealthFlag label={t('promptFilter.auditHealth.modelReview')} enabled={auditHealth.review_enabled} />
                <AuditHealthFlag label={t('promptFilter.auditHealth.conversationLock')} enabled={auditHealth.conversation_lock_enabled} />
                <AuditHealthMetric label={t('promptFilter.auditHealth.reviewKeys')} value={`${auditHealth.review_pool.available} / ${auditHealth.review_pool.configured}`} danger={auditHealth.review_pool.available === 0} />
                <AuditHealthMetric label={t('promptFilter.auditHealth.queuePending')} value={auditHealth.queue.pending_high + auditHealth.queue.pending_low} />
                <AuditHealthMetric label={t('promptFilter.auditHealth.queueFailures')} value={auditHealth.queue.failed + auditHealth.queue.dropped_high} danger={auditHealth.queue.failed + auditHealth.queue.dropped_high > 0} />
              </div>
              <div className={cn('rounded-lg border p-3 text-sm', auditHealth.review_pool.available === 0 && 'border-[hsl(var(--warning))]/40 bg-[hsl(var(--warning-bg))]')}>
                <div className="font-medium">{auditHealth.review_fail_closed ? t('promptFilter.auditHealth.fallbackBlock') : t('promptFilter.auditHealth.fallbackLocal')}</div>
                <div className="mt-1 text-xs text-muted-foreground">
                  {t('promptFilter.auditHealth.reviewPoolDetail', { cooling: auditHealth.review_pool.cooling_down, probing: auditHealth.review_pool.probing })}
                  {auditHealth.review_pool.next_retry_at ? ` · ${t('promptFilter.auditHealth.nextRetry', { time: formatBeijingTime(auditHealth.review_pool.next_retry_at) })}` : ''}
                </div>
              </div>
              {auditHealth.latest_incident_id ? (
                <div className="rounded-lg border p-3 text-sm">
                  <div className="text-xs text-muted-foreground">{t('promptFilter.auditHealth.latestIncident')}</div>
                  <div className="mt-1 break-all font-mono text-xs">{auditHealth.latest_incident_id}</div>
                  {auditHealth.latest_incident_at ? <div className="mt-1 text-xs text-muted-foreground">{formatBeijingTime(auditHealth.latest_incident_at)}</div> : null}
                </div>
              ) : (
                <div className="rounded-lg border border-dashed p-3 text-sm text-muted-foreground">{t('promptFilter.auditHealth.noIncident')}</div>
              )}
            </div>
          ) : null}
          <DialogFooter>
            <Button variant="outline" onClick={() => setAuditHealthOpen(false)}>{t('common.close')}</Button>
            <Button onClick={() => void showAuditHealth()} disabled={auditHealthLoading}>
              <RefreshCw className="size-4" />
              {t('common.refresh')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  )
}

function AuditHealthFlag({ label, enabled }: { label: string; enabled: boolean }) {
  const { t } = useTranslation()
  return (
    <div className="rounded-lg border p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={cn('mt-1 text-sm font-medium', enabled ? 'text-[hsl(var(--success))]' : 'text-destructive')}>
        {enabled ? t('common.enabled') : t('common.disabled')}
      </div>
    </div>
  )
}

function AuditHealthMetric({ label, value, danger = false }: { label: string; value: ReactNode; danger?: boolean }) {
  return (
    <div className="rounded-lg border p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={cn('mt-1 text-sm font-semibold', danger && 'text-destructive')}>{value}</div>
    </div>
  )
}

const emptyRiskProfileFilters: RiskProfileFilters = {
  subjectType: 'newapi_user',
  riskLevel: '',
  platform: '',
  apiKeyId: '',
  accountId: '',
  minScore: '',
	q: '',
	lockedOnly: false,
	cyOnly: false,
	activityState: '',
}

function RiskProfilesView() {
  const { t } = useTranslation()
  const [draftFilters, setDraftFilters] = useState<RiskProfileFilters>(emptyRiskProfileFilters)
  const [filters, setFilters] = useState<RiskProfileFilters>(emptyRiskProfileFilters)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = usePersistedPageSize('prompt_risk_profiles', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [profiles, setProfiles] = useState<PromptRiskProfile[]>([])
  const [total, setTotal] = useState(0)
  const [scoringVersion, setScoringVersion] = useState('')
  const [guardrail, setGuardrail] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const loadProfiles = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await api.getPromptRiskProfiles({
        page,
        pageSize,
        subjectType: filters.subjectType,
        riskLevel: filters.riskLevel,
        platform: filters.platform,
        apiKeyId: filters.apiKeyId,
        accountId: filters.accountId,
        minScore: filters.minScore,
        q: filters.q,
        lockedOnly: filters.lockedOnly,
		cyOnly: filters.cyOnly,
		activityState: filters.activityState,
      })
      setProfiles(result.profiles ?? [])
      setTotal(result.total ?? 0)
      setScoringVersion(result.scoring_version ?? '')
      setGuardrail(result.guardrail ?? '')
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [filters, page, pageSize])

  useEffect(() => {
    void loadProfiles()
  }, [loadProfiles])

  const applyFilters = () => {
    setPage(1)
    setFilters(draftFilters)
  }
  const resetFilters = () => {
    setDraftFilters(emptyRiskProfileFilters)
    setFilters(emptyRiskProfileFilters)
    setPage(1)
  }
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const pageSummary = useMemo(() => {
    const summary = { frozen: 0, cy: 0, critical: 0, high: 0 }
    for (const profile of profiles) {
      if (promptRiskFreezeClass(profile) !== 'none') summary.frozen += 1
      if (profile.upstream_cy_count > 0) summary.cy += 1
      if (profile.risk_level === 'critical') summary.critical += 1
      if (profile.risk_level === 'high') summary.high += 1
    }
    return summary
  }, [profiles])

  return (
    <Card>
      <CardContent>
        <div className="mb-4 flex items-start justify-between gap-3 max-sm:flex-col max-sm:items-stretch">
          <div>
            <SectionTitle title={t('promptFilter.risk.title')} />
            <p className="mt-1 text-sm text-muted-foreground">{t('promptFilter.risk.description')}</p>
          </div>
          <Button variant="outline" onClick={() => void loadProfiles()} disabled={loading}>
            <RefreshCw className="size-3.5" />
            {t('common.refresh')}
          </Button>
        </div>

        <div className="mb-4 rounded-lg border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-sm text-[hsl(var(--warning))]">
          <div className="flex items-start gap-2"><ShieldAlert className="mt-0.5 size-4 shrink-0" /><span>{guardrail || t('promptFilter.risk.guardrail')}</span></div>
          {scoringVersion ? <div className="mt-1 pl-6 font-mono text-xs opacity-75">{scoringVersion}</div> : null}
        </div>

        <div className="mb-4 grid gap-2 sm:grid-cols-4">
          {[
            [t('promptFilter.risk.summary.total'), total],
            [t('promptFilter.risk.summary.frozen'), pageSummary.frozen],
            [t('promptFilter.risk.summary.cy'), pageSummary.cy],
            [t('promptFilter.risk.summary.highCritical'), pageSummary.high + pageSummary.critical],
          ].map(([label, value]) => <div key={String(label)} className="rounded-lg border bg-muted/20 px-3 py-2"><div className="text-xs text-muted-foreground">{label}</div><div className="mt-1 font-mono text-lg font-semibold">{value}</div></div>)}
        </div>

        <div className="mb-4 flex flex-wrap items-center gap-2 rounded-lg border bg-muted/20 p-3">
          <Button size="sm" variant={!draftFilters.lockedOnly && !draftFilters.cyOnly && draftFilters.subjectType === 'newapi_user' ? 'default' : 'outline'} onClick={() => { setDraftFilters((current) => ({ ...current, subjectType: 'newapi_user', lockedOnly: false, cyOnly: false })); setFilters((current) => ({ ...current, subjectType: 'newapi_user', lockedOnly: false, cyOnly: false })); setPage(1) }}>
            <Users className="size-4" />{t('promptFilter.risk.peopleProfiles')}
          </Button>
          <Button size="sm" variant={!draftFilters.lockedOnly && !draftFilters.cyOnly && draftFilters.subjectType === '' ? 'default' : 'outline'} onClick={() => { setDraftFilters((current) => ({ ...current, subjectType: '', lockedOnly: false, cyOnly: false })); setFilters((current) => ({ ...current, subjectType: '', lockedOnly: false, cyOnly: false })); setPage(1) }}>
            <Network className="size-4" />{t('promptFilter.risk.allObjects')}
          </Button>
          <Button size="sm" variant={draftFilters.lockedOnly ? 'destructive' : 'outline'} onClick={() => { setDraftFilters((current) => ({ ...current, subjectType: '', lockedOnly: true, cyOnly: false })); setFilters((current) => ({ ...current, subjectType: '', lockedOnly: true, cyOnly: false })); setPage(1) }}>
            <ShieldAlert className="size-4" />{t('promptFilter.risk.lockedProfiles')}
          </Button>
          <Button size="sm" variant={draftFilters.cyOnly ? 'default' : 'outline'} onClick={() => { setDraftFilters((current) => ({ ...current, subjectType: '', lockedOnly: false, cyOnly: true })); setFilters((current) => ({ ...current, subjectType: '', lockedOnly: false, cyOnly: true })); setPage(1) }}>
            <ShieldAlert className="size-4" />{t('promptFilter.risk.upstreamCYProfiles')}
          </Button>
          <Button size="sm" variant={!draftFilters.cyOnly && draftFilters.subjectType === 'api_key' ? 'default' : 'outline'} onClick={() => { setDraftFilters((current) => ({ ...current, subjectType: 'api_key', lockedOnly: false, cyOnly: false })); setFilters((current) => ({ ...current, subjectType: 'api_key', lockedOnly: false, cyOnly: false })); setPage(1) }}>
            {t('promptFilter.risk.apiKeyProfiles')}
          </Button>
          <Button size="sm" variant={!draftFilters.cyOnly && draftFilters.subjectType === 'upstream_account' ? 'default' : 'outline'} onClick={() => { setDraftFilters((current) => ({ ...current, subjectType: 'upstream_account', lockedOnly: false, cyOnly: false })); setFilters((current) => ({ ...current, subjectType: 'upstream_account', lockedOnly: false, cyOnly: false })); setPage(1) }}>
            {t('promptFilter.risk.upstreamAccountProfiles')}
          </Button>
          <span className="text-xs leading-5 text-muted-foreground">{draftFilters.lockedOnly ? t('promptFilter.risk.lockedProfilesHint') : draftFilters.cyOnly ? t('promptFilter.risk.upstreamCYProfilesHint') : draftFilters.subjectType === 'newapi_user' ? t('promptFilter.risk.peopleProfilesHint') : t('promptFilter.risk.nonPersonHint')}</span>
        </div>

        <div className="mb-4 grid grid-cols-[repeat(auto-fit,minmax(155px,1fr))] gap-3">
          <Field label={t('promptFilter.risk.subjectType')}>
            <Select value={draftFilters.subjectType} onValueChange={(value) => setDraftFilters((current) => ({ ...current, subjectType: value }))} options={[
              { label: t('common.all'), value: '' },
              ...['newapi_user', 'session', 'api_key', 'client_ip', 'upstream_account'].map((value) => ({ label: t(`promptFilter.risk.subjects.${value}`), value })),
            ]} />
          </Field>
          <Field label={t('promptFilter.risk.level')}>
            <Select value={draftFilters.riskLevel} onValueChange={(value) => setDraftFilters((current) => ({ ...current, riskLevel: value }))} options={[
              { label: t('common.all'), value: '' },
              ...['low', 'observed', 'elevated', 'high', 'critical'].map((value) => ({ label: t(`promptFilter.risk.levels.${value}`), value })),
            ]} />
          </Field>
          <Field label={t('promptFilter.risk.platform')}><Input value={draftFilters.platform} onChange={(event) => setDraftFilters((current) => ({ ...current, platform: event.target.value }))} placeholder="newapi" /></Field>
          <Field label={t('promptFilter.apiKeyId')}><Input value={draftFilters.apiKeyId} onChange={(event) => setDraftFilters((current) => ({ ...current, apiKeyId: event.target.value }))} placeholder="ID" /></Field>
          <Field label={t('promptFilter.risk.accountId')}><Input value={draftFilters.accountId} onChange={(event) => setDraftFilters((current) => ({ ...current, accountId: event.target.value }))} placeholder="ID" /></Field>
          <Field label={t('promptFilter.risk.minScore')}><Input type="number" min={0} max={100} value={draftFilters.minScore} onChange={(event) => setDraftFilters((current) => ({ ...current, minScore: event.target.value }))} placeholder="0" /></Field>
          <Field label={t('promptFilter.risk.activityState')}><Select value={draftFilters.activityState} onValueChange={(value) => setDraftFilters((current) => ({ ...current, activityState: value as RiskProfileFilters['activityState'] }))} options={[
            { label: t('common.all'), value: '' },
            { label: t('promptFilter.risk.activityStates.active'), value: 'active' },
            { label: t('promptFilter.risk.activityStates.identityOnly'), value: 'identity_only' },
          ]} /></Field>
          <Field label={t('promptFilter.keyword')}><Input value={draftFilters.q} onChange={(event) => setDraftFilters((current) => ({ ...current, q: event.target.value }))} placeholder={t('promptFilter.risk.keywordPlaceholder')} /></Field>
        </div>
        <div className="mb-4 flex flex-wrap gap-2">
          <Button onClick={applyFilters}><Search className="size-4" />{t('promptFilter.applyFilters')}</Button>
          <Button variant="outline" onClick={resetFilters}><X className="size-4" />{t('promptFilter.resetFilters')}</Button>
          <span className="self-center text-xs text-muted-foreground">{loading ? t('common.loading') : t('promptFilter.risk.recordsCount', { total })}</span>
        </div>

        <StateShell loading={loading} error={error} isEmpty={!loading && profiles.length === 0} onRetry={() => void loadProfiles()} emptyTitle={t('promptFilter.risk.empty')}>
          <RiskProfilesTable profiles={profiles} />
          <Pagination page={page} totalPages={totalPages} totalItems={total} pageSize={pageSize} onPageChange={setPage} onPageSizeChange={(next) => { setPage(1); setPageSize(next) }} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} />
        </StateShell>
      </CardContent>
    </Card>
  )
}

function promptRiskIdentityPrimary(profile: Pick<PromptRiskProfile, 'subject_display' | 'newapi_user_name' | 'newapi_user_email'>) {
  return profile.newapi_user_name || profile.newapi_user_email || profile.subject_display || '-'
}

type PromptRiskFreezeClass = 'none' | 'conversation' | 'user_cooldown' | 'fingerprint_replay'

function promptRiskFreezeClass(profile: PromptRiskProfile): PromptRiskFreezeClass {
  const lock = profile.conversation_lock
  if (!lock || lock.status !== 'active') return 'none'
  if (lock.restriction_scope === 'user_cooldown') return 'user_cooldown'
  if (lock.restriction_scope === 'fingerprint_replay' || lock.identity_kind === 'fingerprint_replay') return 'fingerprint_replay'
  return 'conversation'
}

function promptRiskIdentityKind(profile: PromptRiskProfile) {
  if (profile.subject_type === 'newapi_user') return profile.is_person ? 'newapi_user' : 'unverified_user'
  return profile.subject_type
}

function formatPromptRestrictionRemaining(seconds?: number) {
  const total = Math.max(0, Math.ceil(Number(seconds) || 0))
  if (total <= 0) return '-'
  const minutes = Math.ceil(total / 60)
  if (minutes < 60) return `${minutes}m`
  const hours = Math.floor(minutes / 60)
  const remainder = minutes % 60
  return remainder > 0 ? `${hours}h ${remainder}m` : `${hours}h`
}

function RiskProfilesTable({ profiles }: { profiles: PromptRiskProfile[] }) {
  const { t } = useTranslation()
  return (
    <div className="overflow-x-auto rounded-lg border border-border">
      <Table>
        <TableHeader><TableRow>
          <TableHead>{t('promptFilter.risk.identity')}</TableHead>
          <TableHead>{t('promptFilter.risk.freezeStatus')}</TableHead>
          <TableHead>{t('promptFilter.risk.score')}</TableHead>
          <TableHead>{t('promptFilter.risk.recent')}</TableHead>
          <TableHead>{t('promptFilter.risk.evidence')}</TableHead>
          <TableHead>{t('promptFilter.risk.scope')}</TableHead>
          <TableHead>{t('promptFilter.risk.recommendation')}</TableHead>
          <TableHead className="text-right">{t('promptFilter.cyberDetail')}</TableHead>
        </TableRow></TableHeader>
        <TableBody>{profiles.map((profile) => (
          <TableRow key={`${profile.subject_type}:${profile.subject_key}`}>
            <TableCell>
              <div className="flex items-center gap-2"><Users className="size-4 text-muted-foreground" /><span className="font-medium">{promptRiskIdentityPrimary(profile)}</span></div>
              {profile.newapi_user_id || profile.newapi_user_email ? <div className="mt-1 text-xs text-muted-foreground">{profile.newapi_user_id ? `${t('promptFilter.risk.userId')} #${profile.newapi_user_id}` : ''}{profile.newapi_user_id && profile.newapi_user_email ? ' · ' : ''}{profile.newapi_user_email || ''}</div> : null}
              <div className="mt-1 flex flex-wrap gap-1"><Badge variant={profile.is_person ? 'default' : 'outline'}>{profile.is_person ? t('promptFilter.risk.person') : t('promptFilter.risk.nonPerson')}</Badge><Badge variant="outline">{t(`promptFilter.risk.subjects.${profile.subject_type}`)}</Badge><Badge variant="outline">{t('promptFilter.risk.identitySource')}: {t(`promptFilter.risk.identityKinds.${promptRiskIdentityKind(profile)}`)}</Badge></div>
              <div className="mt-1 flex flex-wrap gap-1 text-[11px] text-muted-foreground">{profile.platform ? <span>{profile.platform}</span> : null}{profile.newapi_user_group ? <span>{t('promptFilter.risk.userGroup')}: {profile.newapi_user_group}</span> : null}<span className="font-mono">{profile.subject_key.slice(0, 18)}</span></div>
            </TableCell>
            <TableCell>
              <div className="flex flex-wrap items-center gap-1.5"><Badge variant={promptRiskFreezeClass(profile) === 'none' ? 'outline' : 'destructive'}>{t(`promptFilter.risk.freezeStates.${promptRiskFreezeClass(profile)}`)}</Badge>{profile.conversation_lock?.status === 'active' ? <span className="font-mono text-xs text-muted-foreground">{formatPromptRestrictionRemaining(profile.conversation_lock.remaining_seconds)}</span> : null}</div>
              {profile.conversation_lock?.status === 'active' ? <div className="mt-1 max-w-[180px] truncate text-xs text-muted-foreground" title={profile.conversation_lock.reason_code}>{profile.conversation_lock.reason_code}</div> : <div className="mt-1 text-xs text-muted-foreground">{t('promptFilter.risk.freezeHint')}</div>}
            </TableCell>
            <TableCell><div className="flex items-center gap-2"><span className="font-mono text-lg font-semibold">{profile.risk_score}</span><Badge className={promptRiskBadgeClass(profile.risk_level)}>{t(`promptFilter.risk.levels.${profile.risk_level}`)}</Badge></div><div className="text-xs text-muted-foreground">{t('promptFilter.risk.identityConfidence')} {profile.identity_confidence}%</div></TableCell>
            <TableCell className="font-mono text-xs">{profile.has_activity ? <><div>10m {profile.events_10m} · 24h {profile.events_24h}</div><div className="mt-1 text-muted-foreground">7d {profile.events_7d} · 30d {profile.events_30d}</div></> : <><div className="font-sans text-muted-foreground">{t('promptFilter.risk.noAttributedRequests')}</div>{profile.identity_updated_at ? <div className="mt-1 text-muted-foreground">{formatBeijingTime(profile.identity_updated_at)}</div> : null}</>}</TableCell>
            <TableCell className="text-xs"><div>CY {profile.upstream_cy_count} · {t('promptFilter.risk.miss')} {profile.confirmed_miss_count}</div><div className="mt-1 text-muted-foreground">{t('promptFilter.risk.block')} {profile.local_block_count} · {t('promptFilter.risk.repeat')} {profile.repeated_fingerprints}</div></TableCell>
            <TableCell className="text-xs"><div>{profile.api_key_name || profile.api_key_masked || (profile.api_key_id ? `Key #${profile.api_key_id}` : '-')}</div><div className="mt-1 max-w-[180px] truncate text-muted-foreground" title={profile.account_name}>{profile.account_name || (profile.account_id ? `Account #${profile.account_id}` : '-')}</div></TableCell>
            <TableCell><div className="flex max-w-[240px] flex-wrap gap-1">{profile.recommended_actions.map((action) => <Badge key={action} variant="outline">{t(`promptFilter.risk.actions.${action}`)}</Badge>)}</div></TableCell>
            <TableCell className="text-right"><PromptRiskProfileDetailButton profile={profile} /></TableCell>
          </TableRow>
        ))}</TableBody>
      </Table>
    </div>
  )
}

function PromptRiskProfileDetailButton({ profile }: { profile: PromptRiskProfile }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [open, setOpen] = useState(false)
  const [trustOpen, setTrustOpen] = useState(false)
  const [trustSaving, setTrustSaving] = useState(false)
  const [unlockingConversation, setUnlockingConversation] = useState(false)
  const [trustDraft, setTrustDraft] = useState({ durationHours: 24, riskThreshold: 35, reason: '' })
  const [detail, setDetail] = useState<PromptRiskProfileDetailResponse | null>(null)
  const [eventPage, setEventPage] = useState(1)
  const [eventPageSize, setEventPageSize] = usePersistedPageSize('prompt_risk_profile_events', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [trustEventPage, setTrustEventPage] = useState(1)
  const [trustEventPageSize, setTrustEventPageSize] = usePersistedPageSize('prompt_risk_profile_trust_events', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const loadDetail = useCallback(async () => {
    if (!open) return
    setLoading(true)
    setError(null)
    try {
      setDetail(await api.getPromptRiskProfile(profile.subject_type, profile.subject_key, eventPage, eventPageSize, trustEventPage, trustEventPageSize))
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [eventPage, eventPageSize, open, profile.subject_key, profile.subject_type, trustEventPage, trustEventPageSize])

  useEffect(() => { void loadDetail() }, [loadDetail])
  const item = detail?.profile ?? profile
  const totalPages = Math.max(1, Math.ceil((detail?.event_total ?? 0) / eventPageSize))
  const trustEventTotalPages = Math.max(1, Math.ceil((detail?.trust_event_total ?? 0) / trustEventPageSize))
  const saveTrust = async () => {
    setTrustSaving(true)
    try {
      await api.upsertPromptRiskTrust(item.subject_type, item.subject_key, {
        duration_hours: trustDraft.durationHours,
        risk_threshold: trustDraft.riskThreshold,
        reason: trustDraft.reason.trim(),
      })
      setTrustOpen(false)
      showToast(t('promptFilter.risk.trust.saved'))
      await loadDetail()
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setTrustSaving(false)
    }
  }
  const revokeTrust = async () => {
    setTrustSaving(true)
    try {
      await api.revokePromptRiskTrust(item.subject_type, item.subject_key)
      showToast(t('promptFilter.risk.trust.revoked'))
      await loadDetail()
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setTrustSaving(false)
    }
  }
  const unlockConversation = async () => {
    const lock = item.conversation_lock
    const userCooldown = lock?.restriction_scope === 'user_cooldown' || item.subject_type === 'newapi_user'
    const scope = userCooldown ? 'user_cooldown' : 'conversation'
    if (!lock || !window.confirm(t(userCooldown ? 'promptFilter.risk.conversationLock.confirmUserCooldown' : 'promptFilter.risk.conversationLock.confirm'))) return
    const reason = window.prompt(t('promptFilter.risk.conversationLock.unlockReasonPrompt'), t(isFingerprintReplay ? 'promptFilter.risk.conversationLock.fingerprintReplayUnlockReason' : userCooldown ? 'promptFilter.risk.conversationLock.userCooldownUnlockReason' : 'promptFilter.risk.conversationLock.conversationUnlockReason'))
    if (reason === null || !reason.trim()) return
    setUnlockingConversation(true)
    try {
      const result = await api.unlockPromptConversation(lock.lock_key, reason.trim(), scope)
      showToast(t(userCooldown ? 'promptFilter.risk.conversationLock.userCooldownUnlocked' : 'promptFilter.risk.conversationLock.unlocked', { count: result.unlocked_count }))
      await loadDetail()
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setUnlockingConversation(false)
    }
  }
  const activeRestriction = item.conversation_lock
  const isUserCooldown = activeRestriction?.restriction_scope === 'user_cooldown' || item.subject_type === 'newapi_user'
  const isFingerprintReplay = activeRestriction?.restriction_scope === 'fingerprint_replay'
  const isLocalRestriction = !isUserCooldown && !isFingerprintReplay && activeRestriction?.reason_code !== 'upstream_cyber_policy'
  const auditReference = activeRestriction?.incident_id || activeRestriction?.request_id || activeRestriction?.decision_id?.replace(/^local-block:/, '') || ''
  return <>
    <Button size="sm" variant="outline" onClick={() => { setEventPage(1); setTrustEventPage(1); setOpen(true) }}>{t('promptFilter.cyberDetail')}</Button>
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent className="flex max-h-[90vh] flex-col overflow-hidden sm:max-w-6xl">
        <DialogHeader className="shrink-0 pr-8"><DialogTitle>{t('promptFilter.risk.detailTitle')}</DialogTitle><DialogDescription>{promptRiskIdentityPrimary(item)} · {t(`promptFilter.risk.subjects.${item.subject_type}`)}</DialogDescription></DialogHeader>
        <div className="min-h-0 flex-1 overflow-y-auto pr-1">
        {loading && !detail ? <div className="py-8 text-center text-sm text-muted-foreground">{t('common.loading')}</div> : error ? <div className="text-sm text-destructive">{error}</div> : <div className="space-y-4">
          <div className="rounded-lg border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-sm text-[hsl(var(--warning))]">{detail?.guardrail || t('promptFilter.risk.guardrail')}</div>
          {item.conversation_lock?.status === 'active' ? <div className="rounded-lg border border-destructive/35 bg-destructive/5 p-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div><div className="flex items-center gap-2 font-semibold text-destructive"><ShieldAlert className="size-4" />{t(isUserCooldown ? 'promptFilter.risk.conversationLock.userCooldownTitle' : isFingerprintReplay ? 'promptFilter.risk.conversationLock.fingerprintReplayTitle' : 'promptFilter.risk.conversationLock.title')}<Badge variant="destructive">{t(isUserCooldown ? 'promptFilter.risk.conversationLock.userCooldownActive' : isFingerprintReplay ? 'promptFilter.risk.conversationLock.fingerprintReplayActive' : 'promptFilter.risk.conversationLock.active')}</Badge></div><p className="mt-1 text-xs leading-5 text-muted-foreground">{t(isUserCooldown ? 'promptFilter.risk.conversationLock.userCooldownDescription' : isFingerprintReplay ? 'promptFilter.risk.conversationLock.fingerprintReplayDescription' : isLocalRestriction ? 'promptFilter.risk.conversationLock.localDescription' : 'promptFilter.risk.conversationLock.description')}</p></div>
              <Button size="sm" variant="destructive" disabled={unlockingConversation} onClick={() => void unlockConversation()}>{t(isUserCooldown ? 'promptFilter.risk.conversationLock.unlockUserCooldown' : isFingerprintReplay ? 'promptFilter.risk.conversationLock.fingerprintReplayUnlock' : 'promptFilter.risk.conversationLock.unlock')}</Button>
            </div>
            <div className="mt-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-4"><PromptPolicyDetailField label={t('promptFilter.risk.conversationLock.lockedAt')} value={formatBeijingTime(item.conversation_lock.locked_at)} /><PromptPolicyDetailField label={t('promptFilter.risk.conversationLock.expiresAt')} value={item.conversation_lock.expires_at ? formatBeijingTime(item.conversation_lock.expires_at) : '-'} /><PromptPolicyDetailField label={t('promptFilter.risk.conversationLock.remaining')} value={formatPromptRestrictionRemaining(item.conversation_lock.remaining_seconds)} /><PromptPolicyDetailField label={t('promptFilter.risk.conversationLock.reason')} value={isUserCooldown ? 'user_cyber_cooldown' : item.conversation_lock.reason_code || 'conversation_cyber_locked'} /><PromptPolicyDetailField label={t('promptFilter.colEndpoint')} value={item.conversation_lock.endpoint || '-'} /><PromptPolicyDetailField label={t('promptFilter.reviewModel')} value={item.conversation_lock.model || '-'} /><PromptPolicyDetailField label={t('promptFilter.risk.conversationLock.auditReference')} value={auditReference || '-'} /><PromptPolicyDetailField label={t('promptFilter.risk.conversationLock.decisionId')} value={item.conversation_lock.decision_id || '-'} /></div>
            {auditReference ? <div className="mt-3 flex flex-wrap items-center gap-2"><Button size="sm" variant="outline" asChild><NavLink to={`/prompt-filter/logs?audit=${encodeURIComponent(auditReference)}`}><Search className="size-3.5" />{t('promptFilter.risk.conversationLock.openAudit')}</NavLink></Button><span className="font-mono text-xs text-muted-foreground">{auditReference}</span></div> : null}
          </div> : null}
          <div className="rounded-lg border bg-muted/20 p-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <div className="flex flex-wrap items-center gap-2 font-semibold"><Shield className="size-4" />{t('promptFilter.risk.trust.title')}{item.trust_policy ? <Badge variant={item.trust_policy.status === 'active' ? 'default' : 'outline'}>{t(`promptFilter.risk.trust.status.${item.trust_policy.status}`, { defaultValue: item.trust_policy.status })}</Badge> : null}</div>
                <p className="mt-1 max-w-3xl text-xs leading-5 text-muted-foreground">{t('promptFilter.risk.trust.description')}</p>
              </div>
              {item.is_person ? <div className="flex flex-wrap gap-2"><Button size="sm" variant="outline" onClick={() => { setTrustDraft({ durationHours: 24, riskThreshold: item.trust_policy?.risk_threshold ?? 35, reason: item.trust_policy?.reason ?? '' }); setTrustOpen(true) }}>{item.trust_policy?.status === 'active' ? t('promptFilter.risk.trust.adjust') : t('promptFilter.risk.trust.enable')}</Button>{item.trust_policy?.status === 'active' ? <Button size="sm" variant="destructive" disabled={trustSaving} onClick={() => void revokeTrust()}>{t('promptFilter.risk.trust.revoke')}</Button> : null}</div> : null}
            </div>
            {item.trust_policy ? <div className="mt-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-4"><PromptPolicyDetailField label={t('promptFilter.risk.trust.source')} value={t(`promptFilter.risk.trust.sources.${item.trust_policy.source || 'manual'}`)} /><PromptPolicyDetailField label={t('promptFilter.risk.trust.validUntil')} value={formatBeijingTime(item.trust_policy.valid_until)} /><PromptPolicyDetailField label={t('promptFilter.risk.trust.threshold')} value={String(item.trust_policy.risk_threshold)} /><PromptPolicyDetailField label={t('promptFilter.risk.trust.bypassCount')} value={String(item.trust_policy.bypass_count)} /><PromptPolicyDetailField label={t('promptFilter.risk.trust.modelReviewCount')} value={String(item.trust_policy.model_review_count ?? 0)} /><PromptPolicyDetailField label={t('promptFilter.risk.trust.lastModelReview')} value={item.trust_policy.last_model_review_at ? formatBeijingTime(item.trust_policy.last_model_review_at) : '-'} /><PromptPolicyDetailField label={t('promptFilter.risk.trust.lastEvaluation')} value={item.trust_policy.last_evaluated_at ? `${item.trust_policy.last_risk_score} · ${formatBeijingTime(item.trust_policy.last_evaluated_at)}` : '-'} /></div> : <p className="mt-3 text-xs text-muted-foreground">{item.is_person ? t('promptFilter.risk.trust.notEnabled') : t('promptFilter.risk.trust.personOnly')}</p>}
          </div>
          {detail?.adaptive_review_basis ? <div className="rounded-lg border border-[hsl(var(--info))]/25 bg-[hsl(var(--info-bg))] p-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div><div className="font-semibold">{t('promptFilter.risk.trust.basisTitle')}</div><p className="mt-1 text-xs leading-5 text-muted-foreground">{t('promptFilter.risk.trust.basisDescription')}</p></div>
              <Badge variant={detail.adaptive_review_basis.decision === 'adaptive_active' ? 'default' : 'outline'}>{t(`promptFilter.risk.trust.decisions.${detail.adaptive_review_basis.decision}`, { defaultValue: detail.adaptive_review_basis.decision })}</Badge>
            </div>
            <div className="mt-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.cleanReviews')} value={`${detail.adaptive_review_basis.clean_review_count} / ${detail.adaptive_review_basis.min_clean_reviews}`} />
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.observationPeriod')} value={`${detail.adaptive_review_basis.observation_hours}h / ${detail.adaptive_review_basis.min_observation_hours}h`} />
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.positiveEvidence')} value={String(detail.adaptive_review_basis.positive_evidence_count)} />
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.riskBoundary')} value={`${item.risk_score} / ${detail.adaptive_review_basis.risk_threshold}`} />
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.sampleRate')} value={`${detail.adaptive_review_basis.sample_percent}%`} />
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.forceReviewInterval')} value={`${detail.adaptive_review_basis.force_review_interval_minutes} min`} />
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.lastCleanReview')} value={detail.adaptive_review_basis.last_clean_at ? formatBeijingTime(detail.adaptive_review_basis.last_clean_at) : '-'} />
              <PromptPolicyDetailField label={t('promptFilter.risk.trust.nextForcedReview')} value={detail.adaptive_review_basis.force_review_due ? t('promptFilter.risk.trust.reviewDueNow') : detail.adaptive_review_basis.next_forced_review_at ? formatBeijingTime(detail.adaptive_review_basis.next_forced_review_at) : '-'} />
            </div>
            <div className="mt-3 rounded-md border bg-background/70 px-3 py-2 text-xs leading-5 text-muted-foreground">{item.trust_policy?.reason || t('promptFilter.risk.trust.basisFallbackReason')}</div>
          </div> : null}
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <StatTile label={t('promptFilter.risk.totalScore')} value={item.risk_score} sub={<Badge className={promptRiskBadgeClass(item.risk_level)}>{t(`promptFilter.risk.levels.${item.risk_level}`)}</Badge>} />
            <StatTile label={t('promptFilter.risk.localSignal')} value={item.score_breakdown.local_signal} />
            <StatTile label={t('promptFilter.risk.upstreamSignal')} value={item.score_breakdown.upstream_signal} />
            <StatTile label={t('promptFilter.risk.recurrence')} value={item.score_breakdown.recurrence} />
          </div>
          <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
            <PromptPolicyDetailField label={t('promptFilter.risk.identity')} value={`${promptRiskIdentityPrimary(item)} · ${item.is_person ? t('promptFilter.risk.person') : t('promptFilter.risk.nonPerson')}`} />
            <PromptPolicyDetailField label={t('promptFilter.risk.identityConfidence')} value={`${item.identity_confidence}%`} />
            <PromptPolicyDetailField label={t('promptFilter.cyberSourceKey')} value={item.api_key_name || item.api_key_masked || (item.api_key_id ? `#${item.api_key_id}` : '-')} />
            <PromptPolicyDetailField label={t('promptFilter.cyberAccount')} value={item.account_name || (item.account_id ? `#${item.account_id}` : '-')} />
            <PromptPolicyDetailField label={t('promptFilter.risk.userId')} value={item.newapi_user_id ? `#${item.newapi_user_id}` : '-'} />
            <PromptPolicyDetailField label={t('promptFilter.risk.userName')} value={item.newapi_user_name || '-'} />
            <PromptPolicyDetailField label={t('promptFilter.risk.userEmail')} value={item.newapi_user_email || '-'} />
            <PromptPolicyDetailField label={t('promptFilter.risk.userGroup')} value={item.newapi_user_group || '-'} />
            <PromptPolicyDetailField label={t('promptFilter.risk.profileState')} value={t(item.has_activity ? 'promptFilter.risk.activeProfile' : 'promptFilter.risk.identityOnly')} />
            <PromptPolicyDetailField label={t('promptFilter.risk.identitySource')} value={item.identity_source || '-'} />
          </div>
          <div><div className="mb-2 text-sm font-semibold">{t('promptFilter.risk.eventHistory')} · {detail?.event_total ?? 0}</div>
            <div className="overflow-x-auto rounded-lg border border-border"><Table><TableHeader><TableRow><TableHead>{t('promptFilter.colTime')}</TableHead><TableHead>{t('promptFilter.risk.eventKind')}</TableHead><TableHead>{t('promptFilter.risk.requestEvidence')}</TableHead><TableHead>{t('promptFilter.colEndpoint')}</TableHead><TableHead>{t('promptFilter.risk.scope')}</TableHead></TableRow></TableHeader><TableBody>
              {(detail?.events ?? []).map((event) => <TableRow key={event.id}><TableCell className="whitespace-nowrap text-xs">{formatBeijingTime(event.created_at)}</TableCell><TableCell><Badge variant="outline">{t(`promptFilter.risk.events.${event.event_kind}`)}</Badge>{event.action || event.local_outcome ? <div className="mt-1 text-[11px] text-muted-foreground">{event.action || '-'} · {event.local_outcome || '-'}</div> : null}</TableCell><TableCell className="text-xs"><div className="font-mono">{event.request_risk_score} × {event.evidence_confidence}%</div>{event.reason_code ? <div className="mt-1 text-muted-foreground">{event.reason_code}</div> : null}{event.prompt_preview ? <div className="mt-1 max-w-[360px] line-clamp-2" title={event.prompt_preview}>{event.prompt_preview}</div> : null}{event.incident_id || event.prompt_filter_log_id || event.request_correlation_id ? <div className="mt-1 font-mono text-[10px] text-muted-foreground" title={event.incident_id || event.request_correlation_id}>{event.incident_id ? `incident ${event.incident_id}` : event.prompt_filter_log_id ? `log #${event.prompt_filter_log_id}` : `request ${event.request_correlation_id}`}</div> : null}</TableCell><TableCell><div className="font-mono text-xs">{event.endpoint || '-'}</div><div className="text-xs text-muted-foreground">{event.model || '-'}</div></TableCell><TableCell className="text-xs"><div>{event.newapi_user_name || (event.newapi_user_id ? `${t('promptFilter.risk.userId')} #${event.newapi_user_id}` : event.api_key_name || event.api_key_masked || '-')}</div><div className="text-muted-foreground">{event.newapi_user_group ? `${event.newapi_user_group} · ` : ''}{event.account_name || '-'}</div></TableCell></TableRow>)}
            </TableBody></Table></div>
            <Pagination page={eventPage} totalPages={totalPages} totalItems={detail?.event_total ?? 0} pageSize={eventPageSize} onPageChange={setEventPage} onPageSizeChange={(next) => { setEventPage(1); setEventPageSize(next) }} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} />
          </div>
          <div><div className="mb-2 text-sm font-semibold">{t('promptFilter.risk.trust.history')} · {detail?.trust_event_total ?? 0}</div><div className="overflow-x-auto rounded-lg border"><Table><TableHeader><TableRow><TableHead>{t('promptFilter.colTime')}</TableHead><TableHead>{t('promptFilter.risk.trust.operation')}</TableHead><TableHead>{t('promptFilter.risk.score')}</TableHead><TableHead>{t('promptFilter.risk.trust.requestAudit')}</TableHead><TableHead>{t('promptFilter.risk.trust.reason')}</TableHead></TableRow></TableHeader><TableBody>{detail?.trust_events.map((event) => <TableRow key={event.id}><TableCell className="whitespace-nowrap text-xs">{formatBeijingTime(event.created_at)}</TableCell><TableCell><Badge variant="outline">{t(`promptFilter.risk.trust.events.${event.event_type}`, { defaultValue: event.event_type })}</Badge></TableCell><TableCell className="font-mono text-xs">{event.risk_score}{event.risk_level ? ` · ${t(`promptFilter.risk.levels.${event.risk_level}`, { defaultValue: event.risk_level })}` : ''}</TableCell><TableCell className="font-mono text-[10px] text-muted-foreground" title={event.request_id_hash}>{event.request_id_hash ? event.request_id_hash.slice(0, 20) : '-'}</TableCell><TableCell className="text-xs">{event.reason || '-'}</TableCell></TableRow>)}{!detail?.trust_events.length ? <TableRow><TableCell colSpan={5} className="py-6 text-center text-muted-foreground">{t('promptFilter.risk.trust.noHistory')}</TableCell></TableRow> : null}</TableBody></Table></div><Pagination page={trustEventPage} totalPages={trustEventTotalPages} totalItems={detail?.trust_event_total ?? 0} pageSize={trustEventPageSize} onPageChange={setTrustEventPage} onPageSizeChange={(next) => { setTrustEventPage(1); setTrustEventPageSize(next) }} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} /></div>
        </div>}
        </div>
      </DialogContent>
    </Dialog>
    <Dialog open={trustOpen} onOpenChange={(value) => { if (!trustSaving) setTrustOpen(value) }}>
      <DialogContent>
        <DialogHeader><DialogTitle>{t('promptFilter.risk.trust.dialogTitle')}</DialogTitle><DialogDescription>{t('promptFilter.risk.trust.dialogDescription')}</DialogDescription></DialogHeader>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t('promptFilter.risk.trust.duration')}><Select value={String(trustDraft.durationHours)} onValueChange={(value) => setTrustDraft((current) => ({ ...current, durationHours: Number(value) }))} options={[{ label: '24h', value: '24' }, { label: '72h', value: '72' }, { label: '7d', value: '168' }, { label: '30d', value: '720' }]} /></Field>
          <Field label={t('promptFilter.risk.trust.threshold')}><DraftNumberInput min={15} max={79} value={trustDraft.riskThreshold} onValueChange={(value) => setTrustDraft((current) => ({ ...current, riskThreshold: value }))} /></Field>
        </div>
        <Field label={t('promptFilter.risk.trust.reason')} hint={t('promptFilter.risk.trust.reasonHint')}><Textarea rows={4} value={trustDraft.reason} onChange={(event) => setTrustDraft((current) => ({ ...current, reason: event.target.value }))} /></Field>
        <div className="rounded-lg border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-xs leading-5 text-muted-foreground">{t('promptFilter.risk.trust.safetyHint')}</div>
        <DialogFooter><Button variant="outline" disabled={trustSaving} onClick={() => setTrustOpen(false)}>{t('common.cancel')}</Button><Button disabled={trustSaving || !trustDraft.reason.trim()} onClick={() => void saveTrust()}>{trustSaving ? t('common.saving') : t('common.save')}</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  </>
}

function promptRiskBadgeClass(level: PromptRiskProfile['risk_level']) {
  switch (level) {
    case 'critical': return 'border-red-500/30 bg-red-500/12 text-red-600 dark:bg-red-500/20 dark:text-red-400'
    case 'high': return 'border-orange-500/30 bg-orange-500/12 text-orange-600 dark:bg-orange-500/20 dark:text-orange-400'
    case 'elevated': return 'border-amber-500/30 bg-amber-500/12 text-amber-600 dark:bg-amber-500/20 dark:text-amber-400'
    case 'observed': return 'border-blue-500/30 bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400'
    default: return 'border-emerald-500/30 bg-emerald-500/12 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-400'
  }
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
  const { showToast } = useToast()
  const [infoOpen, setInfoOpen] = useState(false)
  const [previewRule, setPreviewRule] = useState<PromptFilterRule | null>(null)
  const [previewPatternCopied, setPreviewPatternCopied] = useState(false)
  const [customDialogMode, setCustomDialogMode] = useState<'create' | 'edit' | null>(null)
  const [editingCustomOriginalFingerprint, setEditingCustomOriginalFingerprint] = useState<string | null>(null)
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
  const customPatterns = useMemo(
    () => parseJSONList<PromptFilterRule>(form.prompt_filter_custom_patterns),
    [form.prompt_filter_custom_patterns],
  )

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
    if (await savePartialAndReload({ prompt_filter_disabled_patterns: JSON.stringify(names) })) {
      setSelectedRules(new Set())
    }
  }

  const savePartialAndReload = async (partial: Partial<SystemSettings>): Promise<boolean> => {
    setSavingRule('rules')
    try {
      const updated = await api.updateSettings(partial)
      const nextRules = await api.getPromptFilterRules()
      onRulesUpdated(nextRules, updated)
      return true
    } catch (error) {
      if (error instanceof AdminAPIError && error.status === 409) {
        try {
          const [latestSettings, latestRules] = await Promise.all([
            api.getSettings(),
            api.getPromptFilterRules(),
          ])
          onRulesUpdated(latestRules, latestSettings)
          showToast(t('promptFilter.ruleSaveConflict'), 'warning')
        } catch (refreshError) {
          showToast(`${t('promptFilter.ruleSaveConflictRefreshFailed')}: ${getErrorMessage(refreshError)}`, 'error')
        }
        return false
      }
      showToast(`${t('promptFilter.saveFailed')}: ${getErrorMessage(error)}`, 'error')
      return false
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

  const saveCustomPatterns = async (next: PromptFilterRule[]): Promise<boolean> => {
    return savePartialAndReload({
      prompt_filter_custom_patterns: JSON.stringify(next),
      prompt_filter_custom_patterns_expected: form.prompt_filter_custom_patterns || '[]',
    })
  }

  const startCreateCustomRule = () => {
    setCustomDialogMode('create')
    setEditingCustomOriginalFingerprint(null)
    setCustomDialogDraft(defaultCustomRuleDraft)
  }

  const startEditCustomRule = (index: number) => {
    const rule = customPatterns[index]
    if (!rule) return
    setCustomDialogMode('edit')
    setEditingCustomOriginalFingerprint(customRuleIdentity(rule))
    setCustomDialogDraft(customRuleDraftFromRule(rule))
  }

  const closeCustomRuleDialog = () => {
    setCustomDialogMode(null)
    setEditingCustomOriginalFingerprint(null)
    setCustomDialogDraft(defaultCustomRuleDraft)
  }

  const saveCustomRuleDialog = async () => {
    const name = customDialogDraft.name.trim()
    const pattern = customDialogDraft.pattern
    const weight = parseRuleWeight(customDialogDraft.weight)
    if (!name || !pattern.trim() || weight === null) return

    if (customDialogMode === 'create') {
      const saved = await saveCustomPatterns([
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
      if (saved) closeCustomRuleDialog()
      return
    }

    if (customDialogMode === 'edit' && editingCustomOriginalFingerprint !== null) {
      const editingCustomIndex = customPatterns.findIndex((rule) => customRuleIdentity(rule) === editingCustomOriginalFingerprint)
      const existing = customPatterns[editingCustomIndex]
      if (editingCustomIndex < 0 || !existing) {
        showToast(t('promptFilter.ruleEditTargetMissing'), 'warning')
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
      if (await saveCustomPatterns(next)) closeCustomRuleDialog()
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

          <div className="overflow-x-auto rounded-lg border border-border">
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

          <div className="overflow-x-auto rounded-lg border border-border">
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
                    key={`${customRuleIdentity(rule)}-${index}`}
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
    ? 'border-[hsl(var(--success))]/30 bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]'
    : state.result === 'not_matched'
      ? 'border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] text-[hsl(var(--warning))]'
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

function PromptPolicyIncidentsTable({ incidents, onDeleted }: { incidents: PromptPolicyIncident[]; onDeleted: () => Promise<void> }) {
  const { t } = useTranslation()
  return (
    <div className="overflow-x-auto rounded-lg border border-border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t('promptFilter.colTime')}</TableHead>
            <TableHead>{t('promptFilter.cyberUpstream')}</TableHead>
            <TableHead>{t('promptFilter.cyberLocalResult')}</TableHead>
            <TableHead>{t('promptFilter.cyberSourceKey')}</TableHead>
            <TableHead>{t('promptFilter.cyberAccount')}</TableHead>
            <TableHead>{t('promptFilter.colScore')}</TableHead>
            <TableHead>{t('promptFilter.colEndpoint')}</TableHead>
            <TableHead>{t('promptFilter.cyberAttempt')}</TableHead>
            <TableHead className="text-right">{t('promptFilter.cyberDetail')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {incidents.length === 0 ? (
            <TableRow><TableCell colSpan={9} className="py-8 text-center text-sm text-muted-foreground">{t('promptFilter.noCyberIncidents')}</TableCell></TableRow>
          ) : incidents.map((incident) => (
            <TableRow key={incident.incident_id}>
              <TableCell className="whitespace-nowrap text-xs">{formatBeijingTime(incident.created_at)}</TableCell>
              <TableCell><Badge variant="destructive">{incident.upstream_error_code || 'cyber_policy'} · {incident.status_code || '-'}</Badge></TableCell>
              <TableCell>
                <div className="flex flex-wrap items-center gap-1">
                  <Badge variant="outline">{t(`promptFilter.cyberState.${incident.local_evaluation_state}`)}</Badge>
                  <Badge variant="secondary">{t(`promptFilter.cyberOutcome.${incident.local_outcome}`)}</Badge>
                  <Badge variant={incident.local_comparison === 'confirmed_miss' ? 'destructive' : 'outline'}>{t(`promptFilter.cyberComparisonStatus.${incident.local_comparison}`)}</Badge>
                </div>
              </TableCell>
              <TableCell>
                <div className="max-w-[160px] truncate text-sm" title={incident.api_key_name}>{incident.api_key_name || incident.api_key_masked || '-'}</div>
                <div className="font-mono text-xs text-muted-foreground">#{incident.api_key_id || '-'}{incident.api_key_masked ? ` · ${incident.api_key_masked}` : ''}</div>
              </TableCell>
              <TableCell>
                <div className="max-w-[180px] truncate text-sm" title={incident.account_name}>{incident.account_name || `#${incident.account_id || '-'}`}</div>
                <div className="max-w-[200px] truncate text-xs text-muted-foreground" title={incident.account_group_names?.join(', ')}>{incident.account_group_names?.join(', ') || '-'}</div>
				{incident.routing_snapshot_state === 'current_inferred' ? <div className="text-[11px] text-[hsl(var(--warning))]">{t('promptFilter.cyberRoutingState.current_inferred')}</div> : null}
              </TableCell>
              <TableCell className="font-mono text-xs">{formatPromptPolicyScore(incident.local_score, t('promptFilter.cyberUnscored'))} / {formatPromptPolicyScore(incident.local_audit_score, t('promptFilter.cyberUnscored'))}</TableCell>
              <TableCell><div className="font-mono text-xs">{incident.endpoint || '-'}</div><div className="text-xs text-muted-foreground">{incident.model || '-'}</div></TableCell>
              <TableCell className="font-mono text-xs">{incident.transport || '-'} · #{incident.attempt_index || '-'}</TableCell>
              <TableCell className="text-right"><PromptPolicyIncidentDetailButton incident={incident} onDeleted={onDeleted} /></TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function PromptPolicyIncidentDetailButton({ incident, onDeleted }: { incident: PromptPolicyIncident; onDeleted: () => Promise<void> }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [detail, setDetail] = useState<PromptPolicyIncidentDetailResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [deleting, setDeleting] = useState(false)
  const show = async () => {
    setOpen(true)
    if (detail || loading) return
    setLoading(true)
    setError(null)
    try {
      setDetail(await api.getPromptPolicyIncident(incident.incident_id))
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }
  const item = detail?.incident ?? incident
  const content = (item.prompt_text || item.prompt_preview || '').trim()
  const deleteIncident = async () => {
    if (!window.confirm(t('promptFilter.deleteCyberIncidentConfirm'))) return
    setDeleting(true)
    try {
      await api.deletePromptPolicyIncident(item.incident_id)
      setOpen(false)
      showToast(t('promptFilter.cyberIncidentDeleted'))
      await onDeleted()
    } catch (err) {
      showToast(`${t('promptFilter.clearFailed')}: ${getErrorMessage(err)}`, 'error')
    } finally {
      setDeleting(false)
    }
  }
  return (
    <>
      <Button size="sm" variant="outline" onClick={() => void show()}>{t('promptFilter.cyberDetail')}</Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="flex max-h-[90vh] flex-col overflow-hidden sm:max-w-4xl">
          <DialogHeader className="shrink-0 pr-8">
            <DialogTitle>{t('promptFilter.cyberDetailTitle')}</DialogTitle>
            <DialogDescription className="break-all font-mono">{item.incident_id}</DialogDescription>
          </DialogHeader>
          <div className="min-h-0 flex-1 overflow-y-auto pr-1">
          {loading ? <div className="py-8 text-center text-sm text-muted-foreground">{t('common.loading')}</div> : error ? <div className="text-sm text-destructive">{error}</div> : (
            <div className="space-y-4 text-sm">
              {item.local_evaluation_state === 'legacy_unknown' ? <div className="rounded-md border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-[hsl(var(--warning))]">{t('promptFilter.cyberLegacyUnknown')}</div> : null}
              <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
                <PromptPolicyDetailField label={t('promptFilter.cyberUpstream')} value={`${item.status_code || '-'} · ${item.upstream_error_code || 'cyber_policy'}`} />
                <PromptPolicyDetailField label={t('promptFilter.cyberLocalResult')} value={`${t(`promptFilter.cyberState.${item.local_evaluation_state}`)} · ${t(`promptFilter.cyberOutcome.${item.local_outcome}`)}`} />
                <PromptPolicyDetailField label={t('promptFilter.cyberComparison')} value={t(`promptFilter.cyberComparisonStatus.${item.local_comparison}`)} />
                <PromptPolicyDetailField label={t('promptFilter.executionScore')} value={formatPromptPolicyScore(item.local_score, t('promptFilter.cyberUnscored'))} />
                <PromptPolicyDetailField label={t('promptFilter.auditScore')} value={formatPromptPolicyScore(item.local_audit_score, t('promptFilter.cyberUnscored'))} />
                <PromptPolicyDetailField label={t('promptFilter.cyberProtocolTransport')} value={`${item.protocol || '-'} · ${item.transport || '-'}`} />
                <PromptPolicyDetailField label={t('promptFilter.model')} value={item.model || '-'} />
                <PromptPolicyDetailField label={t('promptFilter.cyberSourceKey')} value={`${item.api_key_name || item.api_key_masked || '-'} · #${item.api_key_id || '-'}`} />
                <PromptPolicyDetailField label={t('promptFilter.cyberAccountAttempt')} value={`${item.account_name || '-'} · #${item.account_id || '-'} · ${t('promptFilter.cyberAttempt')} #${item.attempt_index || '-'}`} />
                <PromptPolicyDetailField label={t('promptFilter.cyberAccountPlatform')} value={item.account_platform || item.platform || '-'} />
				<PromptPolicyDetailField label={t('promptFilter.cyberRoutingSource')} value={t(`promptFilter.cyberRoutingState.${item.routing_snapshot_state || 'unavailable'}`)} />
                <PromptPolicyDetailField label={t('promptFilter.risk.newapiIdentity')} value={item.newapi_user_id ? `${item.newapi_platform || '-'} · ${item.newapi_user_id} · ${item.newapi_policy_status || '-'}` : '-'} />
                <PromptPolicyDetailField label={t('promptFilter.cyberGroups')} value={item.account_group_names?.join(', ') || item.account_group_ids?.join(', ') || '-'} />
                <PromptPolicyDetailField label={t('promptFilter.cyberKeyAllowedGroups')} value={item.api_key_allowed_group_names?.join(', ') || item.api_key_allowed_group_ids?.join(', ') || '-'} />
                <PromptPolicyDetailField label={t('promptFilter.cyberPromptAvailable')} value={item.prompt_available ? t('promptFilter.testResultYes') : t('promptFilter.testResultNo')} />
                <PromptPolicyDetailField label={t('promptFilter.cyberCandidate')} value={detail?.candidate ? `${detail.candidate.status} · #${detail.candidate.id}` : '-'} />
              </div>
              {(item.local_reason || item.local_reason_code) ? <PromptPolicyDetailField label={t('promptFilter.cyberReason')} value={item.local_reason || item.local_reason_code} /> : null}
              {detail && detail.matches.length > 0 ? <div><div className="mb-2 font-semibold">{t('promptFilter.testResultMatches')}</div><div className="flex flex-wrap gap-1.5">{detail.matches.map((match, index) => <Badge key={`${match.name}-${index}`} variant="secondary">{match.name} · {match.weight}</Badge>)}</div></div> : null}
              {detail ? (
                <div>
                  <div className="mb-2 font-semibold">{t('promptFilter.cyberRiskSubjects')}</div>
                  <PromptRiskSubjectList subjects={detail.risk_subjects ?? []} />
                </div>
              ) : null}
              {content ? <div><div className="mb-2 font-semibold">{t('promptFilter.userPromptLabel')}</div><pre className="max-h-[45vh] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/30 p-3 text-xs">{content}</pre></div> : null}
              <div className="flex justify-end border-t pt-3"><Button size="sm" variant="outline" className="text-destructive hover:text-destructive" onClick={() => void deleteIncident()} disabled={deleting}>{deleting ? t('promptFilter.clearing') : t('promptFilter.deleteCyberIncident')}</Button></div>
            </div>
          )}
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

// CY 关联的画像主体列表：上游 CY 事件详情与 CY 学习审核的证据详情共用，
// 人员主体排在最前，每个主体可直接打开画像详情。
function PromptRiskSubjectList({ subjects, compact = false }: { subjects: PromptRiskIncidentSubject[]; compact?: boolean }) {
  const { t } = useTranslation()
  if (subjects.length === 0) {
    return <p className="text-xs text-muted-foreground">{t('promptFilter.cyberRiskSubjectsEmpty')}</p>
  }
  return (
    <div className={compact ? 'space-y-1' : 'space-y-1.5'}>
      {subjects.map((subject) => (
        <div key={`${subject.subject_type}:${subject.subject_key}`} className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-muted/20 px-2.5 py-2">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-1.5">
              <Badge variant={subject.subject_type === 'newapi_user' ? 'default' : 'outline'}>{t(`promptFilter.risk.subjects.${subject.subject_type}`, { defaultValue: subject.subject_type })}</Badge>
              <span className="font-medium">{subject.subject_display || subject.subject_key}</span>
              {subject.is_person ? <Badge variant="secondary">{t('promptFilter.risk.personVerified')}</Badge> : null}
            </div>
            <div className="mt-1 flex flex-wrap gap-x-2 text-[11px] text-muted-foreground">
              {subject.newapi_user_id ? <span>{t('promptFilter.risk.userId')} #{subject.newapi_user_id}</span> : null}
              {subject.newapi_user_email ? <span>{subject.newapi_user_email}</span> : null}
              {subject.newapi_user_name ? <span>{subject.newapi_user_name}</span> : null}
              {subject.newapi_user_group ? <span>{t('promptFilter.risk.userGroup')}: {subject.newapi_user_group}</span> : null}
              {subject.platform ? <span>{subject.platform}</span> : null}
              <span className="font-mono">{subject.subject_key.slice(0, 18)}</span>
              <span>{t('promptFilter.cyberRiskSubjectEvents', { count: subject.event_count })}</span>
            </div>
          </div>
          <PromptRiskProfileDetailButton profile={riskSubjectToProfileStub(subject)} />
        </div>
      ))}
    </div>
  )
}

const DRAFT_AI_PREFERENCE_KEY = 'prompt_intel_draft_ai'
type DraftAIPreference = { provider: PromptIntelligenceAIProvider; model: string; apiKeyId: string }
function readDraftAIPreference(): DraftAIPreference {
  const fallback: DraftAIPreference = { provider: 'account_pool', model: '', apiKeyId: '0' }
  try {
    const raw = window.localStorage.getItem(DRAFT_AI_PREFERENCE_KEY)
    if (!raw) return fallback
    const parsed = JSON.parse(raw) as Partial<DraftAIPreference>
    return {
      provider: parsed.provider === 'review' ? 'review' : 'account_pool',
      model: typeof parsed.model === 'string' ? parsed.model : '',
      apiKeyId: typeof parsed.apiKeyId === 'string' ? parsed.apiKeyId : '0',
    }
  } catch {
    return fallback
  }
}
function writeDraftAIPreference(value: DraftAIPreference) {
  try {
    window.localStorage.setItem(DRAFT_AI_PREFERENCE_KEY, JSON.stringify(value))
  } catch {
    /* 本地偏好写入失败不影响生成 */
  }
}

// CY 关联主体只有主体键和身份信息；画像详情按钮会用主体键拉取完整画像，这里只需一个占位对象。
function riskSubjectToProfileStub(subject: PromptRiskIncidentSubject): PromptRiskProfile {
  return {
    subject_type: subject.subject_type,
    subject_key: subject.subject_key,
    subject_display: subject.subject_display || subject.subject_key,
    platform: subject.platform,
    newapi_user_id: subject.newapi_user_id,
    newapi_user_name: subject.newapi_user_name,
    newapi_user_email: subject.newapi_user_email,
    newapi_user_group: subject.newapi_user_group,
    is_person: subject.is_person,
    identity_confidence: subject.identity_confidence,
    risk_score: 0,
    risk_level: 'low',
    recommended_actions: [],
    score_breakdown: { local_signal: 0, upstream_signal: 0, recurrence: 0, identity_confidence: subject.identity_confidence },
    has_activity: subject.event_count > 0,
    latest_at: new Date(0).toISOString(),
    event_count: subject.event_count,
    events_10m: 0,
    events_24h: 0,
    events_7d: 0,
  } as unknown as PromptRiskProfile
}

function PromptPolicyDetailField({ label, value }: { label: string; value: string }) {
  return <div className="rounded-md border border-border bg-muted/20 p-2.5"><div className="text-xs font-semibold text-muted-foreground">{label}</div><div className="mt-1 break-words">{value}</div></div>
}

function formatPromptPolicyScore(value: number | null | undefined, unscored: string) {
  return value === null || value === undefined ? unscored : String(value)
}

function promptFilterDecisionSource(log: PromptFilterLog): 'model' | 'local' | 'combined' | 'conversation' | null {
  if (log.action !== 'block' && log.action !== 'warn') return null
  if (log.reason_code === 'conversation_cyber_locked') return 'conversation'
  const model = Boolean(log.reviewed && log.review_flagged)
  const local = log.score > 0 || parseLogMatches(log.matched_patterns).length > 0
  if (model && local) return 'combined'
  if (model) return 'model'
  if (local) return 'local'
  return null
}

function PromptReviewLogsTable({ logs }: { logs: PromptFilterLog[] }) {
  const { t } = useTranslation()
  return (
    <div className="overflow-x-auto rounded-lg border border-border">
      <Table className="min-w-[980px] table-fixed">
        <TableHeader>
          <TableRow>
            <TableHead className="w-[150px]">{t('promptFilter.colTime')}</TableHead>
            <TableHead className="w-[330px]">{t('promptFilter.reviewRequest')}</TableHead>
            <TableHead className="w-[300px]">{t('promptFilter.reviewResponse')}</TableHead>
            <TableHead className="w-[110px]">{t('promptFilter.reviewFinalAction')}</TableHead>
	            <TableHead>{t('promptFilter.reviewHistoryScope')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {logs.length === 0 ? (
            <TableRow><TableCell colSpan={5} className="h-24 text-center text-muted-foreground">{t('promptFilter.reviewHistoryEmpty')}</TableCell></TableRow>
          ) : logs.map((log) => {
            const reviewFailed = Boolean(log.review_error)
            const confidence = typeof log.review_confidence === 'number' ? log.review_confidence.toFixed(2) : t('promptFilter.notScored')
            const threshold = typeof log.review_threshold === 'number' ? log.review_threshold.toFixed(2) : '-'
            const moderationCategory = log.review_request_mode === 'moderations'
              ? log.review_reason?.match(/^moderation decision:\s+(\S+)/)?.[1]
              : undefined
            return (
              <TableRow key={`review-${log.id}`}>
                <TableCell className="align-top">
                  <div className="font-medium">{formatRelativeTime(log.created_at, { variant: 'compact' })}</div>
                  <div className="mt-1 text-xs text-muted-foreground">{formatBeijingTime(log.created_at)}</div>
                  {typeof log.review_latency_ms === 'number' ? <div className="mt-1 font-mono text-xs text-muted-foreground">{log.review_latency_ms} ms</div> : null}
                </TableCell>
                <TableCell className="align-top">
                  <div className="flex flex-wrap gap-1.5">
                    <Badge variant="outline">{log.review_model || '-'}</Badge>
                    {log.review_request_mode ? <Badge variant="secondary">{log.review_request_mode}</Badge> : null}
                  </div>
                  <div className="mt-2 font-mono text-xs">{log.endpoint || '-'}</div>
                  <div className="font-mono text-xs text-muted-foreground">{log.model || '-'}</div>
                  {log.review_endpoint ? <div className="mt-1 break-all text-[11px] text-muted-foreground">→ {log.review_endpoint}</div> : null}
                  <div className="mt-2 line-clamp-3 break-words rounded-md bg-muted/40 px-2 py-1.5 text-xs leading-5" title={stripHitMarkers(log.text_preview || '')}>
                    {log.text_preview ? <HighlightedPromptPreview text={log.text_preview} /> : t('promptFilter.reviewRequestUnavailable')}
                  </div>
                </TableCell>
                <TableCell className="align-top">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <Badge variant={reviewFailed || log.review_flagged ? 'destructive' : 'default'}>
                      {reviewFailed ? t('promptFilter.reviewResultError') : log.review_flagged ? t('promptFilter.labels.reviewFlagged') : t('promptFilter.labels.reviewCleared')}
                    </Badge>
                    <span className="font-mono text-xs">{moderationCategory ? `${moderationCategory} ` : ''}{confidence} / {threshold}</span>
                  </div>
                  {log.review_reason ? <p className="mt-2 text-xs leading-5">{log.review_reason}</p> : null}
                  {log.review_error ? <p className="mt-2 break-words text-xs leading-5 text-destructive">{log.review_error}</p> : null}
                </TableCell>
                <TableCell className="align-top">
                  <ActionBadge action={log.action} />
                  {promptFilterDecisionSource(log) ? <div className="mt-1 text-[11px] font-medium text-muted-foreground">{t(`promptFilter.decisionSource.${promptFilterDecisionSource(log)}`)}</div> : null}
                </TableCell>
                <TableCell className="align-top text-xs">
                  <div>{log.api_key_name || log.api_key_masked || (log.api_key_id ? `#${log.api_key_id}` : '-')}</div>
                  {log.newapi_policy_status === 'unbound' ? <div className="mt-1 text-[11px] text-amber-700 dark:text-amber-300">{t('apiKeys.promptFilterIdentityUnbound')}</div> : null}
                  {log.newapi_user_id ? <div className="mt-1 truncate text-muted-foreground" title={log.newapi_user_id}>{t('promptFilter.newapiUser')} {log.newapi_user_id}</div> : null}
                  {log.request_correlation_id ? <div className="mt-1 truncate font-mono text-[11px] text-muted-foreground" title={log.request_correlation_id}>{log.request_correlation_id}</div> : null}
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </div>
  )
}

function PromptFilterLogsTable({ logs, compact = false }: { logs: PromptFilterLog[]; compact?: boolean }) {
  const { t } = useTranslation()
  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <Table className={cn('table-fixed', compact ? 'min-w-[1180px]' : 'min-w-[1420px]')}>
        <TableHeader>
          <TableRow>
            <TableHead className={compact ? 'w-[96px]' : 'w-[150px]'}>{t('promptFilter.colTime')}</TableHead>
            <TableHead className={compact ? 'w-[166px]' : 'w-[180px]'}>{t('promptFilter.colAction')}</TableHead>
            <TableHead className={compact ? 'w-[150px]' : 'w-[180px]'}>{t('promptFilter.colEndpoint')}</TableHead>
            <TableHead className={compact ? 'w-[144px]' : 'w-[156px]'}>{t('promptFilter.colScore')}</TableHead>
            <TableHead className={compact ? 'w-[190px]' : 'w-[230px]'}>{t('promptFilter.colMatch')}</TableHead>
            <TableHead className={compact ? 'w-[132px]' : 'w-[170px]'}>{t('promptFilter.colApiKey')}</TableHead>
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

function TestDecisionBadge({ result }: { result: PromptFilterTestResponse }) {
  const { t } = useTranslation()
  const action = result.decision?.action || result.verdict.action
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
      <Badge variant="outline" className="gap-1.5 border-[hsl(var(--warning))]/30 text-[hsl(var(--warning))]">
        <AlertTriangle className="size-3" />
        {t('promptFilter.modeWarn')}
      </Badge>
    )
  }
  return (
    <Badge variant="outline" className="gap-1.5 border-[hsl(var(--success))]/30 text-[hsl(var(--success))]">
      <CheckCircle2 className="size-3" />
      {t('promptFilter.actionAllow')}
    </Badge>
  )
}

function PromptFilterTestResultPanel({ result }: { result: PromptFilterTestResponse }) {
  const { t } = useTranslation()
  const { verdict, decision } = result
  const mode = decision?.mode || verdict.mode
  const action = decision?.action || verdict.action
  const localizedAction = action === 'block'
    ? t('promptFilter.modeBlock')
    : action === 'warn'
      ? t('promptFilter.modeWarn')
      : t('promptFilter.actionAllow')
  const localizedMode = mode === 'block'
    ? t('promptFilter.modeBlock')
    : mode === 'warn'
      ? t('promptFilter.modeWarn')
      : mode === 'monitor'
        ? t('promptFilter.modeMonitor')
        : promptGuardModes.includes(mode as PromptGuardMode)
          ? t(`promptFilter.guard.modes.${mode}.label`)
          : t('promptFilter.unknownMode')
  const localizedProfile = decision?.profile && promptGuardProfiles.includes(decision.profile as PromptGuardProfile)
    ? t(`promptFilter.guard.profiles.${decision.profile}.label`)
    : t('promptFilter.guard.unknownProfile')
  const localizedOrigin = decision?.primary_origin
    ? t(`promptFilter.origins.${decision.primary_origin}`, { defaultValue: decision.primary_origin })
    : '-'
  const localizedProvider = result.provider
    ? t(`promptFilter.guard.providers.${result.provider}.label`, { defaultValue: result.provider })
    : '-'
  const decisionReason = decision?.reason?.trim() || ''
  const verdictReason = verdict.reason?.trim() || ''
  const localizedReview = verdict.reviewed
    ? (verdict.review_flagged ? t('promptFilter.testReviewFlagged') : t('promptFilter.testReviewCleared'))
    : t('promptFilter.testReviewSkipped')
  return (
    <div className="rounded-lg border border-border bg-muted/25 p-3">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div>
          <div className="text-sm font-semibold text-foreground">{t('promptFilter.testResultPipelineTitle')}</div>
          <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
            {decision ? t('promptFilter.testResultPipelineHint') : t('promptFilter.testResultLegacyFallbackHint')}
          </p>
        </div>
        <TestDecisionBadge result={result} />
      </div>
      <div className="grid grid-cols-[repeat(auto-fit,minmax(135px,1fr))] gap-2 text-sm">
        <MiniStat label={t('promptFilter.testResultFinalAction')} value={localizedAction} />
        <MiniStat label={t('promptFilter.testResultMode')} value={localizedMode} />
        <MiniStat label={t('promptFilter.testResultProfile')} value={decision ? localizedProfile : '-'} />
        <MiniStat
          label={t('promptFilter.testResultProtocolProvider')}
          value={[result.protocol || '-', localizedProvider].join(' · ')}
        />
        <MiniStat
          label={t('promptFilter.testResultEndpointModel')}
          value={[result.endpoint || '-', result.model || '-'].join(' · ')}
        />
        <MiniStat label={t('promptFilter.testResultExecutionScore')} value={`${decision?.score ?? verdict.score} / ${verdict.threshold}`} />
        <MiniStat label={t('promptFilter.testResultAuditScore')} value={String(decision?.audit_score ?? 0)} />
        <MiniStat label={t('promptFilter.testResultOrigin')} value={localizedOrigin} />
        <MiniStat label={t('promptFilter.testResultReasonCode')} value={decision?.reason_code || '-'} mono />
        <MiniStat
          label={t('promptFilter.testResultStrikeEligible')}
          value={decision?.strike_eligible ? t('promptFilter.testResultYes') : t('promptFilter.testResultNo')}
        />
        <MiniStat label={t('promptFilter.testResultMatches')} value={String(verdict.matched?.length ?? 0)} />
        <MiniStat label={t('promptFilter.testResultReview')} value={localizedReview} />
      </div>
      {decision?.primary_detector ? (
        <div className="mt-3 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <span>{t('promptFilter.testResultPrimaryDetector')}</span>
          <Badge variant="outline" className="font-mono text-[11px]">{decision.primary_detector}</Badge>
        </div>
      ) : null}
      {decisionReason ? <p className="mt-3 text-sm text-muted-foreground">{decisionReason}</p> : null}
      {verdictReason && verdictReason !== decisionReason ? <p className="mt-3 text-sm text-muted-foreground">{verdictReason}</p> : null}
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
        <mark key={index} className="rounded bg-[hsl(var(--warning))]/25 px-1 py-0.5 font-medium text-foreground dark:bg-[hsl(var(--warning))]/30">
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

function MiniStat({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="rounded-md border border-border bg-background px-3 py-2">
      <div className="text-[11px] font-bold uppercase text-muted-foreground">{label}</div>
      <div className={cn('mt-1 break-words font-semibold text-foreground', mono && 'font-mono text-xs')}>{value}</div>
    </div>
  )
}

function PromptFilterLogRow({ log, compact }: { log: PromptFilterLog; compact?: boolean }) {
  const { t } = useTranslation()
  const matches = parseLogMatches(log.matched_patterns)
  const [expanded, setExpanded] = useState(false)
  const [detailOpen, setDetailOpen] = useState(false)
  const fullText = (log.full_text || '').trim()
  const hasFull = fullText.length > 0
  const matchContext = (log.match_context || '').trim()
  const userPrompt = (log.text_preview || '').trim()
  const primaryOriginLabel = log.primary_origin
    ? t(`promptFilter.origins.${log.primary_origin}`, { defaultValue: log.primary_origin })
    : t('promptFilter.origins.unknown')
  const policyProfileLabel = log.policy_profile && promptGuardProfiles.includes(log.policy_profile as PromptGuardProfile)
    ? t(`promptFilter.guard.profiles.${log.policy_profile}.label`)
    : t('promptFilter.guard.unknownProfile')
  const hitTerms = extractHitTerms(matchContext || userPrompt)
  const fallbackPreview = log.error_code || log.review_error || ''
  const auxiliaryOrigin = Boolean(log.primary_origin && log.primary_origin !== 'current_user')
  const userPromptLabel = !matchContext && auxiliaryOrigin
    ? t('promptFilter.legacyRequestPreviewLabel')
    : t('promptFilter.userPromptLabel')
  const legacyMissingMatchContext = !matchContext && !userPrompt && !fullText &&
    auxiliaryOrigin
  const auditScore = typeof log.audit_score === 'number' ? log.audit_score : undefined
  const apiKeyLabel = log.api_key_name || log.api_key_masked || '-'
  const decisionSource = promptFilterDecisionSource(log)
  const hasPreviewDetail = Boolean(matchContext || userPrompt || hasFull)
  return (
    <>
    <TableRow>
      <TableCell className={compact ? 'w-[92px] min-w-0' : 'w-[150px] min-w-0'}>
        <div className="font-medium text-foreground">{formatRelativeTime(log.created_at, { variant: 'compact' })}</div>
        {!compact ? <div className="text-xs text-muted-foreground">{formatBeijingTime(log.created_at)}</div> : null}
      </TableCell>
      <TableCell className="min-w-0 align-top whitespace-normal">
        <div className="min-w-0 rounded-lg border border-border/70 bg-muted/20 p-2">
          <div className="flex min-w-0 flex-wrap items-center gap-1.5">
            <ActionBadge action={log.action} />
            {decisionSource ? <Badge variant="outline" className="max-w-full text-[10px]" title={t(`promptFilter.decisionSource.${decisionSource}`)}>{t(`promptFilter.decisionSource.${decisionSource}`)}</Badge> : null}
            {log.policy_profile ? (
              <span className="min-w-0 truncate text-[11px] font-semibold text-muted-foreground" title={policyProfileLabel}>
                {policyProfileLabel}
              </span>
            ) : null}
            {log.newapi_policy_status === 'unbound' ? (
              <Badge variant="secondary" className="w-fit max-w-full truncate text-[10px]">
                {t('apiKeys.promptFilterScopeGlobal')}
              </Badge>
            ) : null}
          </div>
          <div className="mt-2 space-y-1 text-[11px] leading-4 text-muted-foreground">
            {log.primary_origin ? (
              <div className="flex min-w-0 items-center gap-1.5" title={`${t('promptFilter.triggerOrigin')}: ${primaryOriginLabel}`}>
                <FileText className="size-3 shrink-0 text-[hsl(var(--info))]" />
                <span className="min-w-0 truncate">{primaryOriginLabel}</span>
              </div>
            ) : null}
            {log.review_model ? (
              <div className="flex min-w-0 items-center gap-1.5">
                <Sparkles className={cn('size-3 shrink-0', log.review_flagged ? 'text-destructive' : 'text-[hsl(var(--success))]')} />
                <span className="min-w-0 truncate">{log.review_flagged ? t('promptFilter.labels.reviewFlagged') : t('promptFilter.labels.reviewCleared')}</span>
              </div>
            ) : null}
            {log.newapi_policy_status ? (
              <div className="flex min-w-0 items-center gap-1.5" title={log.newapi_decision_id || undefined}>
                <Network className={cn('size-3 shrink-0', log.newapi_policy_status === 'verification_failed' ? 'text-destructive' : 'text-muted-foreground')} />
                <span className="min-w-0 truncate">{t(`promptFilter.newapiPolicyStatus.${log.newapi_policy_status}`)}</span>
              </div>
            ) : null}
          </div>
          {log.strike_eligible || log.source === 'upstream_cyber_policy' ? (
            <div className="mt-2 flex flex-wrap gap-1">
              {log.strike_eligible ? <Badge variant="destructive" className="text-[10px]">{t('promptFilter.labels.strike')}</Badge> : null}
              {log.source === 'upstream_cyber_policy' ? <Badge variant="outline" className="text-[10px]">{t('promptFilter.labels.upstream')}</Badge> : null}
            </div>
          ) : null}
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
        <div className="space-y-2">
          <LogScoreMeter
            label={t('promptFilter.executionScore')}
            score={log.score}
            suffix={` / ${log.threshold}`}
            tone="execution"
            description={t('promptFilter.executionScoreHint')}
          />
          {auditScore !== undefined ? (
            <LogScoreMeter
              label={t('promptFilter.shadowAuditScore')}
              score={auditScore}
              tone="audit"
              description={t('promptFilter.shadowAuditScoreHint')}
            />
          ) : null}
        </div>
      </TableCell>
      <TableCell className={cn('min-w-0 align-top whitespace-normal', compact ? 'w-[190px]' : 'w-[230px]')}>
        {matches.length ? (
          <div className="min-w-0 space-y-1.5">
            {matches.slice(0, 3).map((match, index) => (
              <div
                key={`${match.name}-${index}`}
                className="min-w-0 overflow-hidden rounded-md border border-border/70 bg-muted/25 px-2 py-1.5"
                title={`${match.name} · ${match.weight}`}
              >
                <div className="flex min-w-0 items-start gap-1.5">
                  <span className={cn('mt-1 size-1.5 shrink-0 rounded-full', match.strict ? 'bg-destructive' : 'bg-[hsl(var(--warning))]')} />
                  <span className="min-w-0 break-words font-mono text-[11px] leading-4 text-foreground">{match.name.split('_').join('_\u200b')}</span>
                </div>
                <div className="mt-1 pl-3 font-mono text-[10px] text-muted-foreground">{t('promptFilter.ruleWeight')} {match.weight}</div>
              </div>
            ))}
            {matches.length > 3 ? <Badge variant="secondary" className="text-[10px]">+{matches.length - 3}</Badge> : null}
          </div>
        ) : <span className="text-muted-foreground">-</span>}
      </TableCell>
      <TableCell className="align-top">
        <div className="whitespace-normal break-all font-mono text-[11px] leading-4 text-foreground" title={apiKeyLabel}>{apiKeyLabel}</div>
        {!compact && log.client_ip ? <div className="text-xs text-muted-foreground">{log.client_ip}</div> : null}
        {!compact && log.newapi_platform ? <div className="truncate text-xs text-muted-foreground">NewAPI: {log.newapi_platform}</div> : null}
        {!compact && log.newapi_user_id ? <div className="truncate font-mono text-[11px] text-muted-foreground" title={log.newapi_user_id}>{t('promptFilter.newapiUser')} {log.newapi_user_id}</div> : null}
        {!compact && log.newapi_request_id ? <div className="truncate font-mono text-[11px] text-muted-foreground" title={log.newapi_request_id}>{t('promptFilter.newapiRequest')} {log.newapi_request_id}</div> : null}
      </TableCell>
      <TableCell className="min-w-0">
        <div
          className={cn('space-y-1.5', hasPreviewDetail && 'cursor-pointer rounded-md transition-colors hover:bg-muted/40')}
          role={hasPreviewDetail ? 'button' : undefined}
          tabIndex={hasPreviewDetail ? 0 : undefined}
          onClick={hasPreviewDetail ? () => setDetailOpen(true) : undefined}
          onKeyDown={hasPreviewDetail ? (event) => {
            if (event.key === 'Enter' || event.key === ' ') {
              event.preventDefault()
              setDetailOpen(true)
            }
          } : undefined}
        >
          {matchContext ? (
            <div className="min-w-0 rounded-md border border-[hsl(var(--warning))]/20 bg-[hsl(var(--warning-bg))] px-2 py-1.5">
              <div className="mb-0.5 flex min-w-0 items-center gap-1 text-[10px] font-semibold text-[hsl(var(--warning))]">
                <span className="shrink-0">{t('promptFilter.matchContextLabel')}</span>
                <span aria-hidden="true" className="text-[hsl(var(--warning))]/60">·</span>
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
        {hasPreviewDetail ? (
          <Dialog open={detailOpen} onOpenChange={setDetailOpen}>
            <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
              <DialogHeader>
                <DialogTitle>{t('promptFilter.previewDetailTitle')}</DialogTitle>
                <DialogDescription className="break-all font-mono text-xs">
                  {formatBeijingTime(log.created_at)} · {log.endpoint || '-'} · {log.model || '-'}
                </DialogDescription>
              </DialogHeader>
              <div className="space-y-4 text-sm">
                {matchContext ? (
                  <div>
                    <div className="mb-2 flex flex-wrap items-baseline gap-1.5">
                      <span className="font-semibold">{t('promptFilter.matchContextLabel')}</span>
                      <span className="text-xs text-muted-foreground">{t('promptFilter.triggerOrigin')}: {primaryOriginLabel}</span>
                    </div>
                    <pre className="max-h-[30vh] overflow-auto whitespace-pre-wrap break-words rounded-md border border-[hsl(var(--warning))]/30 bg-[hsl(var(--warning-bg))] p-3 text-xs leading-relaxed text-foreground"><HighlightedPromptPreview text={matchContext} /></pre>
                  </div>
                ) : null}
                {userPrompt ? (
                  <div>
                    <div className="mb-2 font-semibold">{userPromptLabel}</div>
                    <pre className="max-h-[30vh] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/30 p-3 text-xs leading-relaxed text-foreground"><HighlightedPromptPreview text={userPrompt} /></pre>
                  </div>
                ) : null}
                {hasFull ? (
                  <div>
                    <div className="mb-2 flex items-center justify-between gap-2">
                      <span className="font-semibold">{t('promptFilter.fullTextTitle')}</span>
                      <button
                        type="button"
                        onClick={() => void navigator.clipboard?.writeText(fullText)}
                        className="text-xs font-medium text-primary hover:underline"
                      >
                        {t('common.copy')}
                      </button>
                    </div>
                    <pre className="max-h-[40vh] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/30 p-3 text-xs leading-relaxed text-foreground"><HighlightedPromptText text={fullText} terms={hitTerms} /></pre>
                  </div>
                ) : null}
              </div>
            </DialogContent>
          </Dialog>
        ) : null}
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

function LogScoreMeter({
  label,
  score,
  suffix = '',
  tone,
  description,
}: {
  label: string
  score: number
  suffix?: string
  tone: 'execution' | 'audit'
  description: string
}) {
  const normalizedScore = normalizePromptFilterScore(score)
  const scoreBand = getPromptFilterScoreBand(score)
  const meterClass = tone === 'audit'
    ? scoreBand === 'high'
      ? 'bg-violet-500'
      : scoreBand === 'medium'
        ? 'bg-indigo-500'
        : 'bg-sky-500'
    : scoreBand === 'high'
      ? 'bg-destructive'
      : scoreBand === 'medium'
        ? 'bg-[hsl(var(--warning))]'
        : 'bg-[hsl(var(--success))]'

  return (
    <div className="min-w-0" title={description}>
      <div className="flex items-baseline justify-between gap-1 text-[11px]">
        <span className="truncate font-medium text-muted-foreground">{label}</span>
        <span className="shrink-0 font-semibold text-foreground">
          {score}
          <span className="font-normal text-muted-foreground">{suffix}</span>
        </span>
      </div>
      <div
        className="mt-1 h-1.5 overflow-hidden rounded-full bg-muted"
        role="progressbar"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={normalizedScore}
        aria-valuetext={String(score)}
      >
        <div className={cn('h-full rounded-full transition-[width]', meterClass)} style={{ width: `${normalizedScore}%` }} />
      </div>
      {tone === 'audit' ? (
        <div className="mt-1 text-[10px] leading-4 text-muted-foreground">{description}</div>
      ) : null}
    </div>
  )
}

function ActionBadge({ action }: { action: string }) {
  const { t } = useTranslation()
  if (action === 'block') return <Badge variant="destructive">{t('promptFilter.modeBlock')}</Badge>
  if (action === 'warn') return <Badge variant="outline" className="border-[hsl(var(--warning))]/30 text-[hsl(var(--warning))]">{t('promptFilter.modeWarn')}</Badge>
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
