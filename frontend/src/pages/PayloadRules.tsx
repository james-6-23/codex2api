import type { ChangeEvent, ReactNode } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import {
  AlertTriangle,
  BookOpen,
  Braces,
  Check,
  CheckCircle2,
  ClipboardList,
  Copy,
  FilePlus2,
  FileText,
  Filter,
  Gauge,
  GitBranch,
  Layers,
  ListPlus,
  Pencil,
  Plus,
  RefreshCw,
  Save,
  ShieldAlert,
  Sparkles,
  Trash2,
  Wand2,
  Zap,
} from 'lucide-react'

import { api } from '@/api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { StatTile } from '../components/StatTile'
import { SegmentedTabs } from '../components/SegmentedTabs'
import ChipInput from '../components/ChipInput'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { cn } from '@/lib/utils'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime } from '../utils/time'
import type { ObservedInstructionsSample } from '../types'

// ==================== 规则模型 ====================

export const PAYLOAD_RULE_GROUPS = ['default', 'default_raw', 'override', 'override_raw', 'append', 'filter'] as const
type RuleGroup = (typeof PAYLOAD_RULE_GROUPS)[number]

export function countPayloadRules(raw: string): number {
  try {
    const parsed = JSON.parse(raw || '{}') as Record<string, unknown>
    return PAYLOAD_RULE_GROUPS.reduce((sum, group) => {
      const rules = parsed[group]
      return sum + (Array.isArray(rules) ? rules.length : 0)
    }, 0)
  } catch {
    return 0
  }
}

interface RuleEntry {
  group: RuleGroup
  models: string[]
  headers: Record<string, string>
  match: Record<string, unknown>
  notMatch: Record<string, unknown>
  exist: string[]
  notExist: string[]
  /** 下游 API Key 身份匹配门（字符串数组，支持通配符 *，组内 OR） */
  apiKeyIds: string[]
  apiKeyNames: string[]
  /** 账号分组身份匹配门（字符串数组，支持通配符 *，组内 OR）——匹配 Key 允许的组 */
  groupIds: string[]
  groupNames: string[]
  /** 账号门——匹配本次实际调度选中账号的组/套餐（issue #410） */
  accountGroupIds: string[]
  accountGroupNames: string[]
  accountPlans: string[]
  /** filter 组为字段路径数组，其余组为 路径→值 */
  params: Record<string, unknown> | string[]
}

function parseRuleEntries(raw: string): RuleEntry[] | null {
  let parsed: Record<string, unknown>
  try {
    parsed = JSON.parse(raw || '{}') as Record<string, unknown>
  } catch {
    return null
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return null
  const entries: RuleEntry[] = []
  for (const group of PAYLOAD_RULE_GROUPS) {
    const rules = parsed[group]
    if (rules === undefined) continue
    if (!Array.isArray(rules)) return null
    for (const rule of rules) {
      if (rule === null || typeof rule !== 'object') return null
      const r = rule as Record<string, unknown>
      entries.push({
        group,
        models: Array.isArray(r.models) ? r.models.map(String) : [],
        headers: (r.headers && typeof r.headers === 'object' && !Array.isArray(r.headers) ? r.headers : {}) as Record<string, string>,
        match: (r.match && typeof r.match === 'object' && !Array.isArray(r.match) ? r.match : {}) as Record<string, unknown>,
        notMatch: (r.not_match && typeof r.not_match === 'object' && !Array.isArray(r.not_match) ? r.not_match : {}) as Record<string, unknown>,
        exist: Array.isArray(r.exist) ? r.exist.map(String) : [],
        notExist: Array.isArray(r.not_exist) ? r.not_exist.map(String) : [],
        apiKeyIds: Array.isArray(r.api_key_ids) ? r.api_key_ids.map(String) : [],
        apiKeyNames: Array.isArray(r.api_key_names) ? r.api_key_names.map(String) : [],
        groupIds: Array.isArray(r.group_ids) ? r.group_ids.map(String) : [],
        groupNames: Array.isArray(r.group_names) ? r.group_names.map(String) : [],
        accountGroupIds: Array.isArray(r.account_group_ids) ? r.account_group_ids.map(String) : [],
        accountGroupNames: Array.isArray(r.account_group_names) ? r.account_group_names.map(String) : [],
        accountPlans: Array.isArray(r.account_plans) ? r.account_plans.map(String) : [],
        params: group === 'filter'
          ? (Array.isArray(r.params) ? r.params.map(String) : [])
          : ((r.params && typeof r.params === 'object' && !Array.isArray(r.params) ? r.params : {}) as Record<string, unknown>),
      })
    }
  }
  return entries
}

function serializeRuleEntries(entries: RuleEntry[]): string {
  const out: Record<string, unknown[]> = {}
  for (const group of PAYLOAD_RULE_GROUPS) {
    const rules = entries
      .filter((entry) => entry.group === group)
      .map((entry) => {
        const rule: Record<string, unknown> = {}
        if (entry.models.length > 0) rule.models = entry.models
        if (Object.keys(entry.headers).length > 0) rule.headers = entry.headers
        if (Object.keys(entry.match).length > 0) rule.match = entry.match
        if (Object.keys(entry.notMatch).length > 0) rule.not_match = entry.notMatch
        if (entry.exist.length > 0) rule.exist = entry.exist
        if (entry.notExist.length > 0) rule.not_exist = entry.notExist
        if (entry.apiKeyIds.length > 0) rule.api_key_ids = entry.apiKeyIds
        if (entry.apiKeyNames.length > 0) rule.api_key_names = entry.apiKeyNames
        if (entry.groupIds.length > 0) rule.group_ids = entry.groupIds
        if (entry.groupNames.length > 0) rule.group_names = entry.groupNames
        if (entry.accountGroupIds.length > 0) rule.account_group_ids = entry.accountGroupIds
        if (entry.accountGroupNames.length > 0) rule.account_group_names = entry.accountGroupNames
        if (entry.accountPlans.length > 0) rule.account_plans = entry.accountPlans
        rule.params = entry.params
        return rule
      })
    if (rules.length > 0) out[group] = rules
  }
  return Object.keys(out).length === 0 ? '{}' : JSON.stringify(out, null, 2)
}

function formatParamValue(value: unknown): string {
  if (typeof value === 'string') return value
  return JSON.stringify(value)
}

/** 输入字符串还原为类型化值：合法 JSON 字面量按 JSON 解析，其余按字符串。 */
function coerceParamValue(text: string): unknown {
  const trimmed = text.trim()
  if (trimmed === '') return ''
  try {
    return JSON.parse(trimmed)
  } catch {
    return text
  }
}

// ==================== 规则组视觉元数据 ====================

const GROUP_META: Record<RuleGroup, { labelKey: string; badge: string; bar: string }> = {
  default: {
    labelKey: 'payloadRules.groupDefault',
    badge: 'bg-sky-500/12 text-sky-700 dark:text-sky-300 ring-1 ring-sky-500/25',
    bar: 'bg-sky-500',
  },
  default_raw: {
    labelKey: 'payloadRules.groupDefaultRaw',
    badge: 'bg-sky-500/12 text-sky-700 dark:text-sky-300 ring-1 ring-sky-500/25',
    bar: 'bg-sky-400',
  },
  override: {
    labelKey: 'payloadRules.groupOverride',
    badge: 'bg-amber-500/12 text-amber-700 dark:text-amber-300 ring-1 ring-amber-500/25',
    bar: 'bg-amber-500',
  },
  override_raw: {
    labelKey: 'payloadRules.groupOverrideRaw',
    badge: 'bg-amber-500/12 text-amber-700 dark:text-amber-300 ring-1 ring-amber-500/25',
    bar: 'bg-amber-400',
  },
  append: {
    labelKey: 'payloadRules.groupAppend',
    badge: 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300 ring-1 ring-emerald-500/25',
    bar: 'bg-emerald-500',
  },
  filter: {
    labelKey: 'payloadRules.groupFilter',
    badge: 'bg-rose-500/12 text-rose-700 dark:text-rose-300 ring-1 ring-rose-500/25',
    bar: 'bg-rose-500',
  },
}

const GROUP_OPTIONS = PAYLOAD_RULE_GROUPS.map((group) => ({ value: group, labelKey: GROUP_META[group].labelKey }))

// ==================== 添加/编辑规则表单 ====================

type TemplateKey = 'appendPrompt' | 'overridePrompt' | 'serviceTier' | 'effortMap' | 'fastGroup' | 'custom'

const TEMPLATES: Array<{ key: TemplateKey; icon: typeof Wand2; titleKey: string; descKey: string }> = [
  { key: 'appendPrompt', icon: ListPlus, titleKey: 'payloadRules.tplAppendTitle', descKey: 'payloadRules.tplAppendDesc' },
  { key: 'overridePrompt', icon: Wand2, titleKey: 'payloadRules.tplOverrideTitle', descKey: 'payloadRules.tplOverrideDesc' },
  { key: 'serviceTier', icon: Zap, titleKey: 'payloadRules.tplTierTitle', descKey: 'payloadRules.tplTierDesc' },
  { key: 'effortMap', icon: Gauge, titleKey: 'payloadRules.tplEffortTitle', descKey: 'payloadRules.tplEffortDesc' },
  { key: 'fastGroup', icon: Zap, titleKey: 'payloadRules.tplFastGroupTitle', descKey: 'payloadRules.tplFastGroupDesc' },
  { key: 'custom', icon: Braces, titleKey: 'payloadRules.tplCustomTitle', descKey: 'payloadRules.tplCustomDesc' },
]

// 与系统设置思考强度别名、后端 normalizeReasoningEffort 对齐；max 仅 gpt-5.6+ 上游接受，旧模型会被钳到 xhigh。
const EFFORT_LEVELS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'ultra', 'max'].map((v) => ({ label: v, value: v }))

// 上游实际只接受 priority（fast 会映射为 priority），其余值会在发往上游前被剔除
const SERVICE_TIER_OPTIONS = ['priority', 'fast', 'auto', 'default', 'flex', 'scale'].map((v) => ({ label: v, value: v }))

// 账号套餐门候选值,与后端 knownAPIKeyPlanFilters / Key 限额的套餐白名单选项一致
const ACCOUNT_PLAN_OPTIONS = ['free', 'plus', 'pro', 'prolite', 'team', 'k12', 'go']

interface KVRow {
  path: string
  value: string
}

interface RuleFormState {
  template: TemplateKey
  group: RuleGroup
  models: string[]
  promptText: string
  tierValue: string
  effortFrom: string
  effortTo: string
  paramRows: KVRow[]
  matchRows: KVRow[]
  filterPaths: string
  apiKeyIds: string[]
  apiKeyNames: string[]
  groupIds: string[]
  groupNames: string[]
  accountGroupIds: string[]
  accountGroupNames: string[]
  accountPlans: string[]
  /** 编辑时透传表单未暴露的条件字段，避免保存后静默丢失 */
  carry: Pick<RuleEntry, 'headers' | 'notMatch' | 'exist' | 'notExist'>
}

const EMPTY_CARRY: RuleFormState['carry'] = { headers: {}, notMatch: {}, exist: [], notExist: [] }

function emptyFormState(template: TemplateKey): RuleFormState {
  return {
    template,
    group: template === 'appendPrompt' ? 'append' : 'override',
    models: [],
    promptText: '',
    tierValue: 'priority',
    effortFrom: 'medium',
    effortTo: 'high',
    paramRows: [{ path: '', value: '' }],
    matchRows: [],
    filterPaths: '',
    apiKeyIds: [],
    apiKeyNames: [],
    groupIds: [],
    groupNames: [],
    accountGroupIds: [],
    // fastGroup 模板默认对实际调度到名为 fast 的分组账号生效（issue #410：
    // 按实际选中账号的组匹配，而非 Key 允许的组）
    accountGroupNames: template === 'fastGroup' ? ['fast'] : [],
    accountPlans: [],
    carry: EMPTY_CARRY,
  }
}

/** 编辑时反推规则最贴合的模板：无附加条件的单字段规则回到语义化表单，其余走 custom。 */
function detectTemplate(entry: RuleEntry): TemplateKey {
  if (Array.isArray(entry.params)) return 'custom'
  const params = entry.params
  const hasConditions =
    Object.keys(entry.match).length > 0 ||
    Object.keys(entry.notMatch).length > 0 ||
    Object.keys(entry.headers).length > 0 ||
    entry.exist.length > 0 ||
    entry.notExist.length > 0
  if (hasConditions) return 'custom'
  const keys = Object.keys(params)
  if (keys.length === 1 && keys[0] === 'instructions' && typeof params.instructions === 'string') {
    if (entry.group === 'append') return 'appendPrompt'
    if (entry.group === 'override') return 'overridePrompt'
  }
  if (
    keys.length === 1 &&
    keys[0] === 'service_tier' &&
    entry.group === 'override' &&
    SERVICE_TIER_OPTIONS.some((opt) => opt.value === params.service_tier)
  ) {
    return 'serviceTier'
  }
  return 'custom'
}

function formStateFromEntry(entry: RuleEntry): RuleFormState {
  const template = detectTemplate(entry)
  const params = Array.isArray(entry.params) ? {} : entry.params
  return {
    template,
    group: entry.group,
    models: entry.models,
    promptText: typeof params.instructions === 'string' ? params.instructions : '',
    tierValue: typeof params.service_tier === 'string' ? params.service_tier : 'priority',
    effortFrom: 'medium',
    effortTo: 'high',
    paramRows: Array.isArray(entry.params)
      ? [{ path: '', value: '' }]
      : Object.entries(entry.params).map(([path, value]) => ({ path, value: formatParamValue(value) })),
    matchRows: Object.entries(entry.match).map(([path, value]) => ({ path, value: formatParamValue(value) })),
    filterPaths: Array.isArray(entry.params) ? entry.params.join(', ') : '',
    apiKeyIds: entry.apiKeyIds,
    apiKeyNames: entry.apiKeyNames,
    groupIds: entry.groupIds,
    groupNames: entry.groupNames,
    accountGroupIds: entry.accountGroupIds,
    accountGroupNames: entry.accountGroupNames,
    accountPlans: entry.accountPlans,
    carry: { headers: entry.headers, notMatch: entry.notMatch, exist: entry.exist, notExist: entry.notExist },
  }
}

function splitList(text: string): string[] {
  return text
    .split(',')
    .map((item) => item.trim())
    .filter(Boolean)
}

function formStateToEntry(form: RuleFormState): RuleEntry | null {
  const models = form.models.map((model) => model.trim()).filter(Boolean)
  const apiKeyIds = form.apiKeyIds.map((v) => v.trim()).filter(Boolean)
  const apiKeyNames = form.apiKeyNames.map((v) => v.trim()).filter(Boolean)
  const groupIds = form.groupIds.map((v) => v.trim()).filter(Boolean)
  const groupNames = form.groupNames.map((v) => v.trim()).filter(Boolean)
  const accountGroupIds = form.accountGroupIds.map((v) => v.trim()).filter(Boolean)
  const accountGroupNames = form.accountGroupNames.map((v) => v.trim()).filter(Boolean)
  const accountPlans = form.accountPlans.map((v) => v.trim()).filter(Boolean)
  const base: RuleEntry = {
    group: form.group,
    models,
    headers: form.carry.headers,
    match: {},
    notMatch: form.carry.notMatch,
    exist: form.carry.exist,
    notExist: form.carry.notExist,
    apiKeyIds,
    apiKeyNames,
    groupIds,
    groupNames,
    accountGroupIds,
    accountGroupNames,
    accountPlans,
    params: {},
  }
  switch (form.template) {
    case 'appendPrompt': {
      if (!form.promptText.trim()) return null
      return { ...base, group: 'append', params: { instructions: form.promptText } }
    }
    case 'overridePrompt': {
      if (!form.promptText.trim()) return null
      return { ...base, group: 'override', params: { instructions: form.promptText } }
    }
    case 'serviceTier': {
      if (!form.tierValue.trim()) return null
      return { ...base, group: 'override', params: { service_tier: form.tierValue.trim() } }
    }
    case 'effortMap': {
      return {
        ...base,
        group: 'override',
        match: { 'reasoning.effort': form.effortFrom },
        params: { 'reasoning.effort': form.effortTo },
      }
    }
    case 'fastGroup': {
      // 按本次实际调度选中账号的组匹配（issue #410）：Key 允许多组时，
      // 只有真正路由到目标组的账号才覆写 tier。
      return {
        ...base,
        group: 'override',
        accountGroupNames: accountGroupNames.length > 0 ? accountGroupNames : ['fast'],
        params: { service_tier: form.tierValue.trim() || 'priority' },
      }
    }
    case 'custom': {
      const match: Record<string, unknown> = {}
      for (const row of form.matchRows) {
        if (row.path.trim()) match[row.path.trim()] = coerceParamValue(row.value)
      }
      if (form.group === 'filter') {
        const paths = splitList(form.filterPaths)
        if (paths.length === 0) return null
        return { ...base, match, params: paths }
      }
      const params: Record<string, unknown> = {}
      for (const row of form.paramRows) {
        if (row.path.trim()) params[row.path.trim()] = coerceParamValue(row.value)
      }
      if (Object.keys(params).length === 0) return null
      return { ...base, match, params }
    }
  }
}

// ==================== 子组件 ====================

function GateChips({ entry }: { entry: RuleEntry }) {
  const { t } = useTranslation()
  const chips: Array<{ key: string; label: string; mono?: boolean }> = []
  if (entry.models.length > 0) {
    chips.push({ key: 'models', label: `${t('payloadRules.chipModels')}: ${entry.models.join(', ')}`, mono: true })
  } else {
    chips.push({ key: 'all', label: t('payloadRules.chipAllModels') })
  }
  for (const [path, value] of Object.entries(entry.match)) {
    chips.push({ key: `m-${path}`, label: `${path} = ${formatParamValue(value)}`, mono: true })
  }
  for (const [path, value] of Object.entries(entry.notMatch)) {
    chips.push({ key: `nm-${path}`, label: `${path} ≠ ${formatParamValue(value)}`, mono: true })
  }
  for (const [name, value] of Object.entries(entry.headers)) {
    chips.push({ key: `h-${name}`, label: `${t('payloadRules.chipHeader')} ${name}: ${value}`, mono: true })
  }
  for (const path of entry.exist) chips.push({ key: `e-${path}`, label: `${t('payloadRules.chipExist')} ${path}`, mono: true })
  for (const path of entry.notExist) chips.push({ key: `ne-${path}`, label: `${t('payloadRules.chipNotExist')} ${path}`, mono: true })
  if (entry.apiKeyNames.length > 0) {
    chips.push({ key: 'akn', label: `${t('payloadRules.chipApiKeyName')}: ${entry.apiKeyNames.join(', ')}`, mono: true })
  }
  if (entry.apiKeyIds.length > 0) {
    chips.push({ key: 'aki', label: `${t('payloadRules.chipApiKeyId')}: ${entry.apiKeyIds.join(', ')}`, mono: true })
  }
  if (entry.groupNames.length > 0) {
    chips.push({ key: 'gn', label: `${t('payloadRules.chipGroupName')}: ${entry.groupNames.join(', ')}`, mono: true })
  }
  if (entry.groupIds.length > 0) {
    chips.push({ key: 'gi', label: `${t('payloadRules.chipGroupId')}: ${entry.groupIds.join(', ')}`, mono: true })
  }
  if (entry.accountGroupNames.length > 0) {
    chips.push({ key: 'agn', label: `${t('payloadRules.chipAccountGroupName')}: ${entry.accountGroupNames.join(', ')}`, mono: true })
  }
  if (entry.accountGroupIds.length > 0) {
    chips.push({ key: 'agi', label: `${t('payloadRules.chipAccountGroupId')}: ${entry.accountGroupIds.join(', ')}`, mono: true })
  }
  if (entry.accountPlans.length > 0) {
    chips.push({ key: 'ap', label: `${t('payloadRules.chipAccountPlan')}: ${entry.accountPlans.join(', ')}`, mono: true })
  }
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {chips.map((chip) => (
        <span
          key={chip.key}
          className={cn(
            'inline-flex max-w-full items-center truncate rounded-md bg-muted/60 px-1.5 py-0.5 text-[11px] text-muted-foreground',
            chip.mono && 'font-mono',
          )}
        >
          {chip.label}
        </span>
      ))}
    </div>
  )
}

function RuleParamLines({ entry }: { entry: RuleEntry }) {
  if (Array.isArray(entry.params)) {
    return (
      <div className="space-y-1">
        {entry.params.map((path) => (
          <div key={path} className="flex items-center gap-1.5 font-mono text-xs text-foreground">
            <Trash2 className="size-3 shrink-0 text-rose-500/70" />
            <span className="truncate">{path}</span>
          </div>
        ))}
      </div>
    )
  }
  const operator = entry.group === 'append' ? '+=' : '='
  return (
    <div className="space-y-1">
      {Object.entries(entry.params).map(([path, value]) => (
        <div key={path} className="flex min-w-0 items-baseline gap-1.5 font-mono text-xs">
          <span className="shrink-0 font-semibold text-foreground">{path}</span>
          <span className="shrink-0 text-muted-foreground">{operator}</span>
          <span className="truncate text-muted-foreground" title={formatParamValue(value)}>
            {formatParamValue(value)}
          </span>
        </div>
      ))}
    </div>
  )
}

function KVRowsEditor({
  rows,
  onChange,
  pathPlaceholder,
  valuePlaceholder,
  addLabel,
}: {
  rows: KVRow[]
  onChange: (rows: KVRow[]) => void
  pathPlaceholder: string
  valuePlaceholder: string
  addLabel: string
}) {
  return (
    <div className="space-y-2">
      {rows.map((row, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            value={row.path}
            placeholder={pathPlaceholder}
            className="h-8 flex-1 font-mono text-xs"
            onChange={(e: ChangeEvent<HTMLInputElement>) =>
              onChange(rows.map((r, j) => (j === i ? { ...r, path: e.target.value } : r)))
            }
          />
          <Input
            value={row.value}
            placeholder={valuePlaceholder}
            className="h-8 flex-1 font-mono text-xs"
            onChange={(e: ChangeEvent<HTMLInputElement>) =>
              onChange(rows.map((r, j) => (j === i ? { ...r, value: e.target.value } : r)))
            }
          />
          <button
            type="button"
            onClick={() => onChange(rows.filter((_, j) => j !== i))}
            className="flex size-8 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10"
          >
            <Trash2 className="size-3.5" />
          </button>
        </div>
      ))}
      <Button size="sm" variant="ghost" className="h-7 gap-1 px-2 text-xs" onClick={() => onChange([...rows, { path: '', value: '' }])}>
        <Plus className="size-3" />
        {addLabel}
      </Button>
    </div>
  )
}

// ==================== 主页面 ====================

const PAYLOAD_RULES_VIEWS = ['editor', 'docs'] as const
type PayloadRulesView = (typeof PAYLOAD_RULES_VIEWS)[number]

function normalizePayloadRulesView(value?: string): PayloadRulesView {
  return PAYLOAD_RULES_VIEWS.includes(value as PayloadRulesView) ? (value as PayloadRulesView) : 'editor'
}

export default function PayloadRules() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const { view } = useParams()
  const activeView = normalizePayloadRulesView(view)

  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [savedJSON, setSavedJSON] = useState('{}')
  const [entries, setEntries] = useState<RuleEntry[]>([])
  const [saving, setSaving] = useState(false)

  const [viewMode, setViewMode] = useState<'visual' | 'json'>('visual')
  const [jsonDraft, setJsonDraft] = useState('{}')

  const [dialogOpen, setDialogOpen] = useState(false)
  const [editingIndex, setEditingIndex] = useState<number | null>(null)
  const [form, setForm] = useState<RuleFormState>(() => emptyFormState('appendPrompt'))
  const [templatePicked, setTemplatePicked] = useState(false)

  const [samples, setSamples] = useState<ObservedInstructionsSample[]>([])
  const [samplesLoading, setSamplesLoading] = useState(false)
  const [expandedSample, setExpandedSample] = useState<number | null>(null)
  const [copiedSample, setCopiedSample] = useState<number | null>(null)
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [apiKeyNameOptions, setApiKeyNameOptions] = useState<string[]>([])
  const [apiKeyIdOptions, setApiKeyIdOptions] = useState<string[]>([])
  const [groupNameOptions, setGroupNameOptions] = useState<string[]>([])
  const [groupIdOptions, setGroupIdOptions] = useState<string[]>([])

  const visualJSON = useMemo(() => serializeRuleEntries(entries), [entries])
  const currentJSON = viewMode === 'json' ? jsonDraft : visualJSON
  const jsonError = useMemo(() => {
    if (viewMode !== 'json') return null
    return parseRuleEntries(jsonDraft) === null ? t('payloadRules.jsonInvalid') : null
  }, [viewMode, jsonDraft, t])

  const normalizeForCompare = (raw: string): string => {
    const parsed = parseRuleEntries(raw)
    return parsed === null ? raw : serializeRuleEntries(parsed)
  }
  const dirty = normalizeForCompare(currentJSON) !== normalizeForCompare(savedJSON)

  const instructionsRuleCount = useMemo(
    () =>
      entries.filter((entry) =>
        Array.isArray(entry.params) ? entry.params.includes('instructions') : 'instructions' in entry.params,
      ).length,
    [entries],
  )
  const conditionalRuleCount = useMemo(
    () =>
      entries.filter(
        (entry) =>
          Object.keys(entry.match).length > 0 ||
          Object.keys(entry.notMatch).length > 0 ||
          Object.keys(entry.headers).length > 0 ||
          entry.exist.length > 0 ||
          entry.notExist.length > 0,
      ).length,
    [entries],
  )

  const loadSamples = useCallback(async () => {
    setSamplesLoading(true)
    try {
      const resp = await api.getObservedInstructions()
      setSamples(resp.samples || [])
    } catch {
      setSamples([])
    } finally {
      setSamplesLoading(false)
    }
  }, [])

  const load = useCallback(async () => {
    setLoading(true)
    setLoadError(null)
    try {
      const settings = await api.getSettings()
      const raw = settings.payload_rules || '{}'
      const parsed = parseRuleEntries(raw)
      const pretty = parsed === null ? raw : serializeRuleEntries(parsed)
      setSavedJSON(pretty)
      setEntries(parsed ?? [])
      setJsonDraft(pretty)
    } catch (error) {
      setLoadError(getErrorMessage(error))
    } finally {
      setLoading(false)
    }
    void loadSamples()
  }, [loadSamples])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    void (async () => {
      try {
        const resp = await api.getModels()
        setModelOptions(resp.models || [])
      } catch {
        setModelOptions([])
      }
    })()
    void (async () => {
      try {
        const resp = await api.getAPIKeys()
        const keys = resp.keys || []
        setApiKeyNameOptions(keys.map((k) => k.name).filter(Boolean))
        setApiKeyIdOptions(keys.map((k) => String(k.id)))
      } catch {
        setApiKeyNameOptions([])
        setApiKeyIdOptions([])
      }
    })()
    void (async () => {
      try {
        const resp = await api.listAccountGroups()
        const groups = resp.groups || []
        setGroupNameOptions(groups.map((g) => g.name).filter(Boolean))
        setGroupIdOptions(groups.map((g) => String(g.id)))
      } catch {
        setGroupNameOptions([])
        setGroupIdOptions([])
      }
    })()
  }, [])

  const saveRules = useCallback(async () => {
    if (jsonError) return
    setSaving(true)
    try {
      const updated = await api.updateSettings({ payload_rules: currentJSON.trim() || '{}' })
      const raw = updated.payload_rules || '{}'
      const parsed = parseRuleEntries(raw)
      const pretty = parsed === null ? raw : serializeRuleEntries(parsed)
      setSavedJSON(pretty)
      setEntries(parsed ?? [])
      setJsonDraft(pretty)
      showToast(t('payloadRules.saved'), 'success')
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSaving(false)
    }
  }, [currentJSON, jsonError, showToast, t])

  const switchMode = (mode: 'visual' | 'json') => {
    if (mode === viewMode) return
    if (mode === 'json') {
      setJsonDraft(visualJSON)
      setViewMode('json')
      return
    }
    const parsed = parseRuleEntries(jsonDraft)
    if (parsed === null) {
      showToast(t('payloadRules.jsonInvalidSwitch'), 'error')
      return
    }
    setEntries(parsed)
    setViewMode('visual')
  }

  const openAddDialog = () => {
    setEditingIndex(null)
    setTemplatePicked(false)
    setForm(emptyFormState('appendPrompt'))
    setDialogOpen(true)
  }

  const openEditDialog = (index: number) => {
    setEditingIndex(index)
    setTemplatePicked(true)
    setForm(formStateFromEntry(entries[index]))
    setDialogOpen(true)
  }

  const submitDialog = () => {
    const entry = formStateToEntry(form)
    if (!entry) {
      showToast(t('payloadRules.formIncomplete'), 'error')
      return
    }
    if (editingIndex !== null) {
      setEntries(entries.map((existing, i) => (i === editingIndex ? entry : existing)))
    } else {
      setEntries([...entries, entry])
    }
    setDialogOpen(false)
  }

  const removeRule = (index: number) => {
    setEntries(entries.filter((_, i) => i !== index))
  }

  const copySample = async (index: number) => {
    try {
      await navigator.clipboard.writeText(samples[index].instructions)
      setCopiedSample(index)
      setTimeout(() => setCopiedSample(null), 1500)
    } catch {
      showToast(t('payloadRules.copyFailed'), 'error')
    }
  }

  const formValid = formStateToEntry(form) !== null

  return (
    <div className="w-full min-w-0">
      <PageHeader
        title={t('settings2.payloadRules')}
        description={t('settings2.payloadRulesDesc')}
        onRefresh={activeView === 'editor' ? () => void load() : undefined}
        actions={
          activeView === 'editor' ? (
            <Button
              size="sm"
              className="gap-1.5"
              disabled={saving || !dirty || Boolean(jsonError)}
              onClick={() => void saveRules()}
            >
              <Save className="size-3.5" />
              {saving ? t('common.saving') : t('common.save')}
            </Button>
          ) : null
        }
      />

      <PayloadRulesTabs activeView={activeView} />

      {activeView === 'docs' ? <PayloadRulesDocsView /> : null}

      {activeView === 'editor' ? (
        <>
      <StateShell variant="page" loading={loading} error={loadError} onRetry={() => void load()}>
        <div className="space-y-4">
          {/* 统计磁贴 */}
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <StatTile
              label={t('payloadRules.statStatus')}
              value={entries.length > 0 ? t('payloadRules.statusActive') : t('payloadRules.statusEmpty')}
              sub={dirty ? t('payloadRules.unsaved') : undefined}
              tone={entries.length > 0 ? 'success' : 'neutral'}
              toneSurface={entries.length > 0}
            />
            <StatTile label={t('payloadRules.statTotal')} value={String(entries.length)} />
            <StatTile label={t('payloadRules.statInstructions')} value={String(instructionsRuleCount)} sub={t('payloadRules.statInstructionsHint')} />
            <StatTile label={t('payloadRules.statConditional')} value={String(conditionalRuleCount)} sub={t('payloadRules.statConditionalHint')} />
          </div>

          {/* 风险提示 */}
          <div className="flex items-start gap-2.5 rounded-xl border border-amber-500/30 bg-amber-500/[0.07] px-3.5 py-2.5">
            <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />
            <p className="text-xs leading-relaxed text-amber-700 dark:text-amber-300">{t('settings2.payloadRulesWarning')}</p>
          </div>

          {/* 规则编辑主卡 */}
          <div className="overflow-hidden rounded-xl border border-border bg-card/85 shadow-sm">
            <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/70 px-4 py-3">
              <div className="flex items-center gap-1 rounded-lg bg-muted/50 p-0.5">
                {(['visual', 'json'] as const).map((mode) => (
                  <button
                    key={mode}
                    type="button"
                    onClick={() => switchMode(mode)}
                    className={cn(
                      'flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-semibold transition-colors',
                      viewMode === mode
                        ? 'bg-background text-foreground shadow-sm'
                        : 'text-muted-foreground hover:text-foreground',
                    )}
                  >
                    {mode === 'visual' ? <Sparkles className="size-3.5" /> : <Braces className="size-3.5" />}
                    {mode === 'visual' ? t('payloadRules.tabVisual') : t('payloadRules.tabJson')}
                  </button>
                ))}
              </div>
              {viewMode === 'visual' ? (
                <Button size="sm" variant="outline" className="gap-1.5" onClick={openAddDialog}>
                  <FilePlus2 className="size-3.5" />
                  {t('payloadRules.addRule')}
                </Button>
              ) : null}
            </div>

            {viewMode === 'visual' ? (
              <div className="p-4">
                {entries.length === 0 ? (
                  <div className="flex flex-col items-center justify-center gap-3 rounded-xl border border-dashed border-border/80 bg-muted/20 px-6 py-12 text-center">
                    <div className="flex size-12 items-center justify-center rounded-2xl bg-primary/10 text-primary ring-1 ring-primary/15">
                      <Filter className="size-5" />
                    </div>
                    <div>
                      <div className="text-sm font-semibold text-foreground">{t('payloadRules.emptyTitle')}</div>
                      <p className="mx-auto mt-1 max-w-md text-xs leading-relaxed text-muted-foreground">{t('payloadRules.emptyDesc')}</p>
                    </div>
                    <Button size="sm" className="gap-1.5" onClick={openAddDialog}>
                      <FilePlus2 className="size-3.5" />
                      {t('payloadRules.addRule')}
                    </Button>
                  </div>
                ) : (
                  <div className="space-y-2.5">
                    {entries.map((entry, index) => {
                      const meta = GROUP_META[entry.group]
                      return (
                        <div
                          key={index}
                          className="group relative flex items-stretch overflow-hidden rounded-xl border border-border/80 bg-background/70 shadow-sm transition-all hover:border-border hover:shadow-md"
                        >
                          <div className={cn('w-1 shrink-0', meta.bar)} />
                          <div className="flex min-w-0 flex-1 flex-col gap-2 p-3.5">
                            <div className="flex items-center justify-between gap-2">
                              <span className={cn('inline-flex items-center rounded-md px-2 py-0.5 text-[11px] font-semibold', meta.badge)}>
                                {t(meta.labelKey)}
                              </span>
                              <div className="flex items-center gap-1 transition-opacity focus-within:opacity-100 [@media(hover:hover)]:opacity-0 [@media(hover:hover)]:group-hover:opacity-100">
                                <button
                                  type="button"
                                  onClick={() => openEditDialog(index)}
                                  aria-label={t('payloadRules.editRule')}
                                  className="flex size-7 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                                >
                                  <Pencil className="size-3.5" />
                                </button>
                                <button
                                  type="button"
                                  onClick={() => removeRule(index)}
                                  aria-label={t('payloadRules.deleteRule')}
                                  className="flex size-7 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10"
                                >
                                  <Trash2 className="size-3.5" />
                                </button>
                              </div>
                            </div>
                            <RuleParamLines entry={entry} />
                            <GateChips entry={entry} />
                          </div>
                        </div>
                      )
                    })}
                  </div>
                )}
              </div>
            ) : (
              <div className="p-4">
                <textarea
                  rows={18}
                  value={jsonDraft}
                  spellCheck={false}
                  onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setJsonDraft(e.target.value)}
                  className="flex w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs leading-relaxed text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
                />
                {jsonError ? (
                  <p className="mt-1.5 text-xs text-destructive">{jsonError}</p>
                ) : (
                  <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">{t('settings2.payloadRulesHint')}</p>
                )}
              </div>
            )}
          </div>

          {/* 透传样本 */}
          <div className="rounded-xl border border-border bg-card/85 p-4 shadow-sm">
            <div className="mb-1 flex items-center justify-between gap-2">
              <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
                <FileText className="size-4 text-primary" />
                {t('settings2.payloadRulesObserved')}
              </div>
              <Button size="sm" variant="outline" className="gap-1.5" onClick={() => void loadSamples()} disabled={samplesLoading}>
                <RefreshCw className={cn('size-3.5', samplesLoading && 'animate-spin')} />
                {t('settings2.payloadRulesObservedLoad')}
              </Button>
            </div>
            <p className="mb-3 text-xs leading-relaxed text-muted-foreground">{t('settings2.payloadRulesObservedDesc')}</p>
            {samples.length === 0 ? (
              <div className="rounded-lg border border-dashed border-border/80 bg-muted/20 px-4 py-6 text-center text-xs text-muted-foreground">
                {t('settings2.payloadRulesObservedEmpty')}
              </div>
            ) : (
              <div className="space-y-2">
                {samples.map((sample, i) => (
                  <div key={i} className="rounded-lg border border-border bg-background/70 transition-colors hover:border-border">
                    <div className="flex items-center gap-2 px-3 py-2">
                      <button
                        type="button"
                        className="flex min-w-0 flex-1 items-center gap-2 text-left"
                        onClick={() => setExpandedSample(expandedSample === i ? null : i)}
                      >
                        <Badge variant="secondary" className="shrink-0 font-mono text-[11px]">{sample.model || '-'}</Badge>
                        <span className="min-w-0 truncate text-xs text-muted-foreground">{sample.originator}</span>
                      </button>
                      <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">
                        {sample.length.toLocaleString()} {t('payloadRules.chars')}
                        {sample.truncated ? ` · ${t('payloadRules.truncated')}` : ''}
                      </span>
                      <span className="hidden shrink-0 text-[11px] text-muted-foreground sm:inline">
                        {formatBeijingTime(sample.observed_at)}
                      </span>
                      <button
                        type="button"
                        onClick={() => void copySample(i)}
                        aria-label={t('payloadRules.samplesCopy')}
                        className="flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                      >
                        {copiedSample === i ? <Check className="size-3.5 text-emerald-500" /> : <Copy className="size-3.5" />}
                      </button>
                    </div>
                    {expandedSample === i ? (
                      <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words border-t border-border/70 bg-muted/30 px-3 py-2.5 text-[11px] leading-relaxed text-foreground">
                        {sample.instructions}
                      </pre>
                    ) : null}
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      </StateShell>

      {/* 添加/编辑规则对话框 */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-[760px]">
          <DialogHeader>
            <DialogTitle className="flex flex-wrap items-center gap-2">
              {editingIndex !== null ? t('payloadRules.editRule') : t('payloadRules.addRule')}
              {templatePicked ? (
                <Badge variant="secondary" className="text-[11px] font-semibold">
                  {t(TEMPLATES.find((tpl) => tpl.key === form.template)?.titleKey ?? 'payloadRules.tplCustomTitle')}
                </Badge>
              ) : null}
            </DialogTitle>
            <DialogDescription>
              {templatePicked ? t('payloadRules.formDesc') : t('payloadRules.pickTemplate')}
            </DialogDescription>
          </DialogHeader>

          {!templatePicked ? (
            <div className="grid gap-2.5 sm:grid-cols-2">
              {TEMPLATES.map((tpl) => {
                const Icon = tpl.icon
                return (
                  <button
                    key={tpl.key}
                    type="button"
                    onClick={() => {
                      setForm(emptyFormState(tpl.key))
                      setTemplatePicked(true)
                    }}
                    className={cn(
                      'flex items-start gap-3 rounded-xl border border-border/80 bg-background/70 p-3.5 text-left transition-all hover:border-primary/40 hover:bg-muted/20 hover:shadow-sm',
                      tpl.key === 'custom' && 'sm:col-span-2',
                    )}
                  >
                    <div className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/15">
                      <Icon className="size-4" />
                    </div>
                    <div className="min-w-0">
                      <div className="text-sm font-semibold text-foreground">{t(tpl.titleKey)}</div>
                      <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">{t(tpl.descKey)}</p>
                    </div>
                  </button>
                )
              })}
            </div>
          ) : (
            <div className="max-h-[65dvh] space-y-4 overflow-y-auto pr-1">
              {(form.template === 'appendPrompt' || form.template === 'overridePrompt') ? (
                <div className="space-y-1.5">
                  <div className="flex items-center justify-between gap-2">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formText')}</label>
                    <span className="text-[11px] tabular-nums text-muted-foreground">
                      {form.promptText.length.toLocaleString()} {t('payloadRules.chars')}
                    </span>
                  </div>
                  <textarea
                    rows={9}
                    value={form.promptText}
                    placeholder={t('payloadRules.formTextPlaceholder')}
                    onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setForm({ ...form, promptText: e.target.value })}
                    className="flex min-h-[180px] w-full resize-y rounded-xl border border-input bg-background px-3 py-2.5 text-[13px] leading-relaxed text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
                  />
                </div>
              ) : null}

              {form.template === 'serviceTier' ? (
                <div className="space-y-1.5">
                  <label className="text-xs font-semibold text-foreground">{t('payloadRules.formTier')}</label>
                  <div className="max-w-[220px]">
                    <Select
                      compact
                      value={form.tierValue}
                      options={SERVICE_TIER_OPTIONS}
                      onValueChange={(v) => setForm({ ...form, tierValue: v })}
                    />
                  </div>
                  <p className="text-[11px] leading-relaxed text-muted-foreground">{t('payloadRules.formTierHint')}</p>
                </div>
              ) : null}

              {form.template === 'effortMap' ? (
                <div className="grid gap-4 sm:grid-cols-2">
                  <div className="space-y-1.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formEffortFrom')}</label>
                    <Select compact value={form.effortFrom} options={EFFORT_LEVELS} onValueChange={(v) => setForm({ ...form, effortFrom: v })} />
                  </div>
                  <div className="space-y-1.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formEffortTo')}</label>
                    <Select compact value={form.effortTo} options={EFFORT_LEVELS} onValueChange={(v) => setForm({ ...form, effortTo: v })} />
                  </div>
                </div>
              ) : null}

              {form.template === 'fastGroup' ? (
                <div className="space-y-4">
                  <div className="space-y-1.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.accountGroupNamesLabel')}</label>
                    <ChipInput
                      value={form.accountGroupNames}
                      onChange={(accountGroupNames) => setForm({ ...form, accountGroupNames })}
                      options={groupNameOptions}
                      placeholder={t('payloadRules.groupNamesPlaceholder')}
                    />
                    <p className="text-[11px] leading-relaxed text-muted-foreground">{t('payloadRules.tplFastGroupHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formTier')}</label>
                    <div className="max-w-[220px]">
                      <Select
                        compact
                        value={form.tierValue}
                        options={SERVICE_TIER_OPTIONS}
                        onValueChange={(v) => setForm({ ...form, tierValue: v })}
                      />
                    </div>
                    <p className="text-[11px] leading-relaxed text-muted-foreground">{t('payloadRules.formTierHint')}</p>
                  </div>
                </div>
              ) : null}

              {form.template !== 'custom' && form.template !== 'fastGroup' ? (
                <div className="space-y-3 rounded-xl border border-border/70 bg-muted/20 p-3.5">
                  <div className="space-y-0.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.identityGatesLabel')}</label>
                    <p className="text-[11px] leading-relaxed text-muted-foreground">{t('payloadRules.identityGatesHint')}</p>
                  </div>
                  <div className="grid gap-3 sm:grid-cols-2">
                    <div className="space-y-1.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.groupNamesLabel')}</label>
                      <ChipInput
                        value={form.groupNames}
                        onChange={(groupNames) => setForm({ ...form, groupNames })}
                        options={groupNameOptions}
                        placeholder={t('payloadRules.groupNamesPlaceholder')}
                      />
                    </div>
                    <div className="space-y-1.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.apiKeyNamesLabel')}</label>
                      <ChipInput
                        value={form.apiKeyNames}
                        onChange={(apiKeyNames) => setForm({ ...form, apiKeyNames })}
                        options={apiKeyNameOptions}
                        placeholder={t('payloadRules.apiKeyNamesPlaceholder')}
                      />
                    </div>
                    <div className="space-y-1.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.accountGroupNamesLabel')}</label>
                      <ChipInput
                        value={form.accountGroupNames}
                        onChange={(accountGroupNames) => setForm({ ...form, accountGroupNames })}
                        options={groupNameOptions}
                        placeholder={t('payloadRules.groupNamesPlaceholder')}
                      />
                      <p className="text-[11px] leading-relaxed text-muted-foreground">{t('payloadRules.accountGroupNamesHint')}</p>
                    </div>
                    <div className="space-y-1.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.accountPlansLabel')}</label>
                      <ChipInput
                        value={form.accountPlans}
                        onChange={(accountPlans) => setForm({ ...form, accountPlans })}
                        options={ACCOUNT_PLAN_OPTIONS}
                        placeholder={t('payloadRules.accountPlansPlaceholder')}
                      />
                    </div>
                  </div>
                </div>
              ) : null}

              {form.template === 'custom' ? (
                <>
                  <div className="grid gap-4 sm:grid-cols-2">
                    <div className="space-y-1.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.formGroup')}</label>
                      <Select
                        compact
                        value={form.group}
                        options={GROUP_OPTIONS.map((option) => ({ label: t(option.labelKey), value: option.value }))}
                        onValueChange={(v) => setForm({ ...form, group: v as RuleGroup })}
                      />
                    </div>
                    <div className="space-y-1.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.formModels')}</label>
                      <ChipInput
                        value={form.models}
                        onChange={(models) => setForm({ ...form, models })}
                        options={modelOptions}
                        placeholder={t('payloadRules.formModelsPlaceholder')}
                      />
                    </div>
                  </div>
                  {form.group === 'filter' ? (
                    <div className="space-y-1.5 rounded-xl border border-border/70 bg-muted/20 p-3.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.formFilterPaths')}</label>
                      <Input
                        value={form.filterPaths}
                        placeholder="metadata.debug, safety_identifier"
                        className="h-9 font-mono text-xs"
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setForm({ ...form, filterPaths: e.target.value })}
                      />
                    </div>
                  ) : (
                    <div className="space-y-2 rounded-xl border border-border/70 bg-muted/20 p-3.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.formParams')}</label>
                      <KVRowsEditor
                        rows={form.paramRows}
                        onChange={(paramRows) => setForm({ ...form, paramRows })}
                        pathPlaceholder={t('payloadRules.formParamPath')}
                        valuePlaceholder={t('payloadRules.formParamValue')}
                        addLabel={t('payloadRules.formAddRow')}
                      />
                    </div>
                  )}
                  <div className="space-y-2 rounded-xl border border-border/70 bg-muted/20 p-3.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formMatch')}</label>
                    <KVRowsEditor
                      rows={form.matchRows}
                      onChange={(matchRows) => setForm({ ...form, matchRows })}
                      pathPlaceholder="reasoning.effort"
                      valuePlaceholder="medium"
                      addLabel={t('payloadRules.formAddRow')}
                    />
                  </div>
                  <div className="space-y-3 rounded-xl border border-border/70 bg-muted/20 p-3.5">
                    <div className="space-y-0.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.identityGatesLabel')}</label>
                      <p className="text-[11px] leading-relaxed text-muted-foreground">{t('payloadRules.identityGatesHint')}</p>
                    </div>
                    <div className="grid gap-3 sm:grid-cols-2">
                      <div className="space-y-1.5">
                        <label className="text-xs font-semibold text-foreground">{t('payloadRules.apiKeyNamesLabel')}</label>
                        <ChipInput
                          value={form.apiKeyNames}
                          onChange={(apiKeyNames) => setForm({ ...form, apiKeyNames })}
                          options={apiKeyNameOptions}
                          placeholder={t('payloadRules.apiKeyNamesPlaceholder')}
                        />
                      </div>
                      <div className="space-y-1.5">
                        <label className="text-xs font-semibold text-foreground">{t('payloadRules.apiKeyIdsLabel')}</label>
                        <ChipInput
                          value={form.apiKeyIds}
                          onChange={(apiKeyIds) => setForm({ ...form, apiKeyIds })}
                          options={apiKeyIdOptions}
                          placeholder={t('payloadRules.apiKeyIdsPlaceholder')}
                        />
                      </div>
                      <div className="space-y-1.5">
                        <label className="text-xs font-semibold text-foreground">{t('payloadRules.groupNamesLabel')}</label>
                        <ChipInput
                          value={form.groupNames}
                          onChange={(groupNames) => setForm({ ...form, groupNames })}
                          options={groupNameOptions}
                          placeholder={t('payloadRules.groupNamesPlaceholder')}
                        />
                      </div>
                      <div className="space-y-1.5">
                        <label className="text-xs font-semibold text-foreground">{t('payloadRules.groupIdsLabel')}</label>
                        <ChipInput
                          value={form.groupIds}
                          onChange={(groupIds) => setForm({ ...form, groupIds })}
                          options={groupIdOptions}
                          placeholder={t('payloadRules.groupIdsPlaceholder')}
                        />
                      </div>
                      <div className="space-y-1.5">
                        <label className="text-xs font-semibold text-foreground">{t('payloadRules.accountGroupNamesLabel')}</label>
                        <ChipInput
                          value={form.accountGroupNames}
                          onChange={(accountGroupNames) => setForm({ ...form, accountGroupNames })}
                          options={groupNameOptions}
                          placeholder={t('payloadRules.groupNamesPlaceholder')}
                        />
                        <p className="text-[11px] leading-relaxed text-muted-foreground">{t('payloadRules.accountGroupNamesHint')}</p>
                      </div>
                      <div className="space-y-1.5">
                        <label className="text-xs font-semibold text-foreground">{t('payloadRules.accountGroupIdsLabel')}</label>
                        <ChipInput
                          value={form.accountGroupIds}
                          onChange={(accountGroupIds) => setForm({ ...form, accountGroupIds })}
                          options={groupIdOptions}
                          placeholder={t('payloadRules.groupIdsPlaceholder')}
                        />
                      </div>
                      <div className="space-y-1.5">
                        <label className="text-xs font-semibold text-foreground">{t('payloadRules.accountPlansLabel')}</label>
                        <ChipInput
                          value={form.accountPlans}
                          onChange={(accountPlans) => setForm({ ...form, accountPlans })}
                          options={ACCOUNT_PLAN_OPTIONS}
                          placeholder={t('payloadRules.accountPlansPlaceholder')}
                        />
                      </div>
                    </div>
                  </div>
                </>
              ) : null}

              {form.template !== 'custom' ? (
                <div className="space-y-1.5 rounded-xl border border-border/70 bg-muted/15 p-3">
                  <div className="flex items-center justify-between gap-2">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formModels')}</label>
                    <span className="text-[11px] text-muted-foreground">
                      {form.models.length > 0
                        ? t('payloadRules.formModelsSelected', { count: form.models.length })
                        : t('payloadRules.formModelsAll')}
                    </span>
                  </div>
                  <ChipInput
                    value={form.models}
                    onChange={(models) => setForm({ ...form, models })}
                    options={modelOptions}
                    placeholder={t('payloadRules.formModelsPlaceholder')}
                  />
                  <p className="text-[11px] leading-relaxed text-muted-foreground">
                    {t('payloadRules.formModelsHint')}
                  </p>
                </div>
              ) : null}
            </div>
          )}

          {templatePicked ? (
            <DialogFooter>
              {editingIndex === null ? (
                <Button variant="ghost" size="sm" onClick={() => setTemplatePicked(false)}>
                  {t('payloadRules.backToTemplates')}
                </Button>
              ) : null}
              <Button variant="outline" size="sm" onClick={() => setDialogOpen(false)}>
                {t('common.cancel')}
              </Button>
              <Button size="sm" disabled={!formValid} onClick={submitDialog}>
                {editingIndex !== null ? t('common.save') : t('payloadRules.create')}
              </Button>
            </DialogFooter>
          ) : null}
        </DialogContent>
      </Dialog>
        </>
      ) : null}
    </div>
  )
}

function PayloadRulesTabs({ activeView }: { activeView: PayloadRulesView }) {
  const { t } = useTranslation()
  return (
    <div className="mb-5 flex justify-center">
      <SegmentedTabs
        className="w-full max-w-[420px] backdrop-blur-lg"
        tabs={[
          { value: 'editor', label: t('payloadRules.views.editor'), to: '/payload-rules/editor' },
          { value: 'docs', label: t('payloadRules.views.docs'), to: '/payload-rules/docs' },
        ]}
        value={activeView}
      />
    </div>
  )
}

type PayloadDocsSectionKind = 'default' | 'pipeline' | 'modes' | 'features' | 'checklist'

type PayloadDocsSection = {
  id: string
  group: 'intro' | 'setup' | 'core' | 'ops'
  kind: PayloadDocsSectionKind
  title: string
  icon: ReactNode
  paragraphs?: string[]
  bullets?: string[]
  steps?: string[]
  callout?: string
  table?: { headers: string[]; rows: string[][] }
  cards?: { title: string; body: string; tone?: 'neutral' | 'warn' | 'danger' | 'success' }[]
}

const PAYLOAD_DOCS_GROUPS = ['intro', 'setup', 'core', 'ops'] as const

function PayloadRulesDocsView() {
  const { t } = useTranslation()
  const [activeId, setActiveId] = useState('what')
  const activeLockRef = useRef(false)
  const activeLockTimerRef = useRef<number | null>(null)

  const sections = useMemo<PayloadDocsSection[]>(() => [
    {
      id: 'what',
      group: 'intro',
      kind: 'features',
      icon: <Braces className="size-4" />,
      title: t('payloadRules.docs.what.title'),
      paragraphs: [t('payloadRules.docs.what.p1'), t('payloadRules.docs.what.p2')],
      bullets: [
        t('payloadRules.docs.what.b1'),
        t('payloadRules.docs.what.b2'),
        t('payloadRules.docs.what.b3'),
        t('payloadRules.docs.what.b4'),
      ],
    },
    {
      id: 'pipeline',
      group: 'intro',
      kind: 'pipeline',
      icon: <GitBranch className="size-4" />,
      title: t('payloadRules.docs.pipeline.title'),
      paragraphs: [t('payloadRules.docs.pipeline.p1')],
      steps: [
        t('payloadRules.docs.pipeline.s1'),
        t('payloadRules.docs.pipeline.s2'),
        t('payloadRules.docs.pipeline.s3'),
        t('payloadRules.docs.pipeline.s4'),
        t('payloadRules.docs.pipeline.s5'),
      ],
    },
    {
      id: 'groups',
      group: 'core',
      kind: 'modes',
      icon: <Layers className="size-4" />,
      title: t('payloadRules.docs.groups.title'),
      paragraphs: [t('payloadRules.docs.groups.p1')],
      cards: [
        { title: t('payloadRules.groupDefault'), body: t('payloadRules.docs.groups.default'), tone: 'neutral' },
        { title: t('payloadRules.groupOverride'), body: t('payloadRules.docs.groups.override'), tone: 'warn' },
        { title: t('payloadRules.groupAppend'), body: t('payloadRules.docs.groups.append'), tone: 'success' },
        { title: t('payloadRules.groupFilter'), body: t('payloadRules.docs.groups.filter'), tone: 'danger' },
        { title: t('payloadRules.groupDefaultRaw'), body: t('payloadRules.docs.groups.raw'), tone: 'neutral' },
        { title: t('payloadRules.groupOverrideRaw'), body: t('payloadRules.docs.groups.raw'), tone: 'warn' },
      ],
      callout: t('payloadRules.docs.groups.callout'),
    },
    {
      id: 'gates',
      group: 'core',
      kind: 'default',
      icon: <Filter className="size-4" />,
      title: t('payloadRules.docs.gates.title'),
      paragraphs: [t('payloadRules.docs.gates.p1')],
      bullets: [
        t('payloadRules.docs.gates.b1'),
        t('payloadRules.docs.gates.b2'),
        t('payloadRules.docs.gates.b3'),
        t('payloadRules.docs.gates.b4'),
        t('payloadRules.docs.gates.b5'),
      ],
    },
    {
      id: 'identity',
      group: 'core',
      kind: 'default',
      icon: <Zap className="size-4" />,
      title: t('payloadRules.docs.identity.title'),
      paragraphs: [t('payloadRules.docs.identity.p1'), t('payloadRules.docs.identity.p2')],
      bullets: [
        t('payloadRules.docs.identity.b1'),
        t('payloadRules.docs.identity.b2'),
        t('payloadRules.docs.identity.b3'),
        t('payloadRules.docs.identity.b4'),
        t('payloadRules.docs.identity.b5'),
      ],
      callout: t('payloadRules.docs.identity.callout'),
    },
    {
      id: 'templates',
      group: 'setup',
      kind: 'features',
      icon: <Sparkles className="size-4" />,
      title: t('payloadRules.docs.templates.title'),
      paragraphs: [t('payloadRules.docs.templates.p1')],
      bullets: [
        t('payloadRules.docs.templates.b1'),
        t('payloadRules.docs.templates.b2'),
        t('payloadRules.docs.templates.b3'),
        t('payloadRules.docs.templates.b4'),
        t('payloadRules.docs.templates.b6'),
        t('payloadRules.docs.templates.b5'),
      ],
      callout: t('payloadRules.docs.templates.callout'),
    },
    {
      id: 'quickstart',
      group: 'setup',
      kind: 'checklist',
      icon: <Wand2 className="size-4" />,
      title: t('payloadRules.docs.quickstart.title'),
      paragraphs: [t('payloadRules.docs.quickstart.p1')],
      steps: [
        t('payloadRules.docs.quickstart.s1'),
        t('payloadRules.docs.quickstart.s2'),
        t('payloadRules.docs.quickstart.s3'),
        t('payloadRules.docs.quickstart.s4'),
        t('payloadRules.docs.quickstart.s5'),
      ],
    },
    {
      id: 'protected',
      group: 'core',
      kind: 'default',
      icon: <ShieldAlert className="size-4" />,
      title: t('payloadRules.docs.protected.title'),
      paragraphs: [t('payloadRules.docs.protected.p1')],
      bullets: [
        t('payloadRules.docs.protected.b1'),
        t('payloadRules.docs.protected.b2'),
        t('payloadRules.docs.protected.b3'),
      ],
      callout: t('payloadRules.docs.protected.callout'),
    },
    {
      id: 'samples',
      group: 'ops',
      kind: 'default',
      icon: <FileText className="size-4" />,
      title: t('payloadRules.docs.samples.title'),
      paragraphs: [t('payloadRules.docs.samples.p1')],
      bullets: [
        t('payloadRules.docs.samples.b1'),
        t('payloadRules.docs.samples.b2'),
        t('payloadRules.docs.samples.b3'),
      ],
    },
    {
      id: 'json',
      group: 'ops',
      kind: 'default',
      icon: <ClipboardList className="size-4" />,
      title: t('payloadRules.docs.json.title'),
      paragraphs: [t('payloadRules.docs.json.p1'), t('payloadRules.docs.json.p2')],
      bullets: [
        t('payloadRules.docs.json.b1'),
        t('payloadRules.docs.json.b2'),
        t('payloadRules.docs.json.b3'),
      ],
    },
    {
      id: 'checklist',
      group: 'ops',
      kind: 'checklist',
      icon: <CheckCircle2 className="size-4" />,
      title: t('payloadRules.docs.checklist.title'),
      paragraphs: [t('payloadRules.docs.checklist.p1')],
      steps: [
        t('payloadRules.docs.checklist.s1'),
        t('payloadRules.docs.checklist.s2'),
        t('payloadRules.docs.checklist.s3'),
        t('payloadRules.docs.checklist.s4'),
        t('payloadRules.docs.checklist.s5'),
      ],
    },
  ], [t])

  const groups = useMemo(
    () =>
      PAYLOAD_DOCS_GROUPS.map((group) => ({
        id: group,
        label: t(`payloadRules.docs.groupLabels.${group}`),
        items: sections.filter((section) => section.group === group),
      })).filter((group) => group.items.length > 0),
    [sections, t],
  )

  const sectionIds = useMemo(() => sections.map((section) => section.id), [sections])

  useEffect(() => {
    const SPY_OFFSET = 120
    const resolveActiveSection = () => {
      if (activeLockRef.current) return
      let current = sectionIds[0] ?? 'what'
      for (const id of sectionIds) {
        const el = document.getElementById(`pr-docs-${id}`)
        if (!el) continue
        if (el.getBoundingClientRect().top - SPY_OFFSET <= 0) current = id
        else break
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
    const el = document.getElementById(`pr-docs-${id}`)
    if (!el) return
    activeLockRef.current = true
    setActiveId(id)
    if (activeLockTimerRef.current != null) window.clearTimeout(activeLockTimerRef.current)
    el.scrollIntoView({ behavior: 'smooth', block: 'start' })
    activeLockTimerRef.current = window.setTimeout(() => {
      activeLockRef.current = false
      activeLockTimerRef.current = null
    }, 900)
  }

  return (
    <div className="grid gap-5 xl:grid-cols-[260px_minmax(0,1fr)]">
      <aside className="h-fit xl:sticky xl:top-3">
        <div className="overflow-hidden rounded-xl border border-foreground/12 bg-card shadow-sm">
          <div className="border-b border-foreground/10 bg-muted/30 px-4 py-3">
            <div className="text-[11px] font-semibold uppercase tracking-[0.14em] text-muted-foreground">
              {t('payloadRules.docs.toc')}
            </div>
            <div className="mt-1 text-sm font-semibold text-foreground">{t('payloadRules.docs.tocHint')}</div>
          </div>
          <nav className="max-h-[180px] xl:max-h-[min(70vh,720px)] space-y-4 overflow-y-auto p-3 [scrollbar-width:thin]">
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

      <article className="overflow-hidden rounded-xl border border-foreground/12 bg-card shadow-sm">
        <header className="relative overflow-hidden border-b border-foreground/10 bg-gradient-to-br from-primary/[0.07] via-card to-card px-6 py-7 sm:px-8 sm:py-8">
          <div className="pointer-events-none absolute -right-16 -top-20 size-56 rounded-full bg-primary/10 blur-3xl" />
          <div className="relative max-w-3xl">
            <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-primary/20 bg-primary/8 px-2.5 py-1 text-[11px] font-semibold uppercase tracking-wide text-primary">
              <BookOpen className="size-3.5" />
              {t('payloadRules.docs.badge')}
            </div>
            <h1 className="text-2xl font-semibold tracking-tight text-foreground sm:text-[1.75rem]">
              {t('payloadRules.docs.title')}
            </h1>
            <p className="mt-2.5 text-sm leading-7 text-muted-foreground sm:text-[15px]">
              {t('payloadRules.docs.description')}
            </p>
          </div>
        </header>

        <div className="divide-y divide-foreground/10">
          {sections.map((section, index) => (
            <section key={section.id} id={`pr-docs-${section.id}`} className="scroll-mt-24 px-6 py-7 sm:px-8 sm:py-8">
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
                      {t(`payloadRules.docs.groupLabels.${section.group}`)}
                    </span>
                  </div>
                  <h2 className="text-lg font-semibold tracking-tight text-foreground sm:text-xl">{section.title}</h2>
                </div>
              </div>

              <div className="space-y-4 pl-0 sm:pl-12">
                {section.paragraphs?.map((paragraph) => (
                  <p key={paragraph} className="text-sm leading-7 text-muted-foreground sm:text-[15px]">{paragraph}</p>
                ))}

                {section.kind === 'pipeline' && section.steps?.length ? (
                  <div className="relative space-y-0">
                    <div className="absolute bottom-3 left-[15px] top-3 w-px bg-border" />
                    {section.steps.map((step, stepIndex) => (
                      <div key={step} className="relative flex gap-3 py-2.5">
                        <div className="relative z-10 flex size-8 shrink-0 items-center justify-center rounded-full border border-foreground/15 bg-background text-xs font-semibold shadow-sm">
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
                      <li key={step} className="flex gap-3 rounded-lg border border-foreground/10 bg-background px-3.5 py-3 text-sm leading-6 shadow-sm">
                        <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-primary/10 text-xs font-semibold text-primary">
                          {stepIndex + 1}
                        </span>
                        <span className="pt-0.5 text-foreground/90">{step}</span>
                      </li>
                    ))}
                  </ol>
                ) : null}

                {section.kind === 'features' && section.bullets?.length ? (
                  <div className="grid gap-2.5 sm:grid-cols-2">
                    {section.bullets.map((bullet, bulletIndex) => (
                      <div key={bullet} className="rounded-lg border border-foreground/10 bg-muted/15 px-3.5 py-3 text-sm leading-6 text-foreground/90">
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
                  <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
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
