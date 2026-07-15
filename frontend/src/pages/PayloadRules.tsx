import type { ChangeEvent } from 'react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertTriangle,
  Braces,
  Check,
  Copy,
  FilePlus2,
  FileText,
  Filter,
  Gauge,
  ListPlus,
  Pencil,
  Plus,
  RefreshCw,
  Save,
  Sparkles,
  Trash2,
  Wand2,
  Zap,
} from 'lucide-react'

import { api } from '@/api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
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

type TemplateKey = 'appendPrompt' | 'overridePrompt' | 'serviceTier' | 'effortMap' | 'custom'

const TEMPLATES: Array<{ key: TemplateKey; icon: typeof Wand2; titleKey: string; descKey: string }> = [
  { key: 'appendPrompt', icon: ListPlus, titleKey: 'payloadRules.tplAppendTitle', descKey: 'payloadRules.tplAppendDesc' },
  { key: 'overridePrompt', icon: Wand2, titleKey: 'payloadRules.tplOverrideTitle', descKey: 'payloadRules.tplOverrideDesc' },
  { key: 'serviceTier', icon: Zap, titleKey: 'payloadRules.tplTierTitle', descKey: 'payloadRules.tplTierDesc' },
  { key: 'effortMap', icon: Gauge, titleKey: 'payloadRules.tplEffortTitle', descKey: 'payloadRules.tplEffortDesc' },
  { key: 'custom', icon: Braces, titleKey: 'payloadRules.tplCustomTitle', descKey: 'payloadRules.tplCustomDesc' },
]

const EFFORT_LEVELS = ['minimal', 'low', 'medium', 'high', 'xhigh'].map((v) => ({ label: v, value: v }))

interface KVRow {
  path: string
  value: string
}

interface RuleFormState {
  template: TemplateKey
  group: RuleGroup
  models: string
  promptText: string
  tierValue: string
  effortFrom: string
  effortTo: string
  paramRows: KVRow[]
  matchRows: KVRow[]
  filterPaths: string
}

function emptyFormState(template: TemplateKey): RuleFormState {
  return {
    template,
    group: template === 'appendPrompt' ? 'append' : 'override',
    models: '',
    promptText: '',
    tierValue: 'priority',
    effortFrom: 'medium',
    effortTo: 'high',
    paramRows: [{ path: '', value: '' }],
    matchRows: [],
    filterPaths: '',
  }
}

function formStateFromEntry(entry: RuleEntry): RuleFormState {
  return {
    template: 'custom',
    group: entry.group,
    models: entry.models.join(', '),
    promptText: '',
    tierValue: 'priority',
    effortFrom: 'medium',
    effortTo: 'high',
    paramRows: Array.isArray(entry.params)
      ? [{ path: '', value: '' }]
      : Object.entries(entry.params).map(([path, value]) => ({ path, value: formatParamValue(value) })),
    matchRows: Object.entries(entry.match).map(([path, value]) => ({ path, value: formatParamValue(value) })),
    filterPaths: Array.isArray(entry.params) ? entry.params.join(', ') : '',
  }
}

function splitList(text: string): string[] {
  return text
    .split(',')
    .map((item) => item.trim())
    .filter(Boolean)
}

function formStateToEntry(form: RuleFormState): RuleEntry | null {
  const models = splitList(form.models)
  const base: RuleEntry = { group: form.group, models, headers: {}, match: {}, notMatch: {}, exist: [], notExist: [], params: {} }
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

function StatTile({ label, value, hint, accent }: { label: string; value: string; hint?: string; accent?: boolean }) {
  return (
    <div className="rounded-xl border border-border/80 bg-card/85 px-4 py-3 shadow-sm">
      <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{label}</div>
      <div className={cn('mt-1 text-xl font-bold tabular-nums', accent ? 'text-primary' : 'text-foreground')}>{value}</div>
      {hint ? <div className="mt-0.5 truncate text-[11px] text-muted-foreground">{hint}</div> : null}
    </div>
  )
}

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

export default function PayloadRules() {
  const { t } = useTranslation()
  const { showToast } = useToast()

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
        onRefresh={() => void load()}
        actions={
          <Button
            size="sm"
            className="gap-1.5"
            disabled={saving || !dirty || Boolean(jsonError)}
            onClick={() => void saveRules()}
          >
            <Save className="size-3.5" />
            {saving ? t('common.saving') : t('common.save')}
          </Button>
        }
      />

      <StateShell variant="page" loading={loading} error={loadError} onRetry={() => void load()}>
        <div className="space-y-4">
          {/* 统计磁贴 */}
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <StatTile
              label={t('payloadRules.statStatus')}
              value={entries.length > 0 ? t('payloadRules.statusActive') : t('payloadRules.statusEmpty')}
              hint={dirty ? t('payloadRules.unsaved') : undefined}
              accent={entries.length > 0}
            />
            <StatTile label={t('payloadRules.statTotal')} value={String(entries.length)} />
            <StatTile label={t('payloadRules.statInstructions')} value={String(instructionsRuleCount)} hint={t('payloadRules.statInstructionsHint')} />
            <StatTile label={t('payloadRules.statConditional')} value={String(conditionalRuleCount)} hint={t('payloadRules.statConditionalHint')} />
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
                              <div className="flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100">
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
        <DialogContent className="sm:max-w-[560px]">
          <DialogHeader>
            <DialogTitle>{editingIndex !== null ? t('payloadRules.editRule') : t('payloadRules.addRule')}</DialogTitle>
            <DialogDescription>
              {templatePicked ? t('payloadRules.formDesc') : t('payloadRules.pickTemplate')}
            </DialogDescription>
          </DialogHeader>

          {!templatePicked ? (
            <div className="grid gap-2 sm:grid-cols-2">
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
                      'flex items-start gap-2.5 rounded-xl border border-border/80 bg-background/70 p-3 text-left transition-all hover:border-primary/40 hover:bg-muted/20 hover:shadow-sm',
                      tpl.key === 'custom' && 'sm:col-span-2',
                    )}
                  >
                    <div className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/15">
                      <Icon className="size-4" />
                    </div>
                    <div className="min-w-0">
                      <div className="text-[13px] font-semibold text-foreground">{t(tpl.titleKey)}</div>
                      <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">{t(tpl.descKey)}</p>
                    </div>
                  </button>
                )
              })}
            </div>
          ) : (
            <div className="space-y-3.5">
              {(form.template === 'appendPrompt' || form.template === 'overridePrompt') ? (
                <div className="space-y-1.5">
                  <label className="text-xs font-semibold text-foreground">{t('payloadRules.formText')}</label>
                  <textarea
                    rows={6}
                    value={form.promptText}
                    placeholder={t('payloadRules.formTextPlaceholder')}
                    onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setForm({ ...form, promptText: e.target.value })}
                    className="flex w-full resize-y rounded-md border border-input bg-background px-3 py-2 text-xs leading-relaxed text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
                  />
                </div>
              ) : null}

              {form.template === 'serviceTier' ? (
                <div className="space-y-1.5">
                  <label className="text-xs font-semibold text-foreground">{t('payloadRules.formTier')}</label>
                  <Input
                    value={form.tierValue}
                    placeholder="priority"
                    className="h-8 font-mono text-xs"
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setForm({ ...form, tierValue: e.target.value })}
                  />
                </div>
              ) : null}

              {form.template === 'effortMap' ? (
                <div className="grid grid-cols-2 gap-3">
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

              {form.template === 'custom' ? (
                <>
                  <div className="space-y-1.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formGroup')}</label>
                    <Select
                      compact
                      value={form.group}
                      options={GROUP_OPTIONS.map((option) => ({ label: t(option.labelKey), value: option.value }))}
                      onValueChange={(v) => setForm({ ...form, group: v as RuleGroup })}
                    />
                  </div>
                  {form.group === 'filter' ? (
                    <div className="space-y-1.5">
                      <label className="text-xs font-semibold text-foreground">{t('payloadRules.formFilterPaths')}</label>
                      <Input
                        value={form.filterPaths}
                        placeholder="metadata.debug, safety_identifier"
                        className="h-8 font-mono text-xs"
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setForm({ ...form, filterPaths: e.target.value })}
                      />
                    </div>
                  ) : (
                    <div className="space-y-1.5">
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
                  <div className="space-y-1.5">
                    <label className="text-xs font-semibold text-foreground">{t('payloadRules.formMatch')}</label>
                    <KVRowsEditor
                      rows={form.matchRows}
                      onChange={(matchRows) => setForm({ ...form, matchRows })}
                      pathPlaceholder="reasoning.effort"
                      valuePlaceholder="medium"
                      addLabel={t('payloadRules.formAddRow')}
                    />
                  </div>
                </>
              ) : null}

              <div className="space-y-1.5">
                <label className="text-xs font-semibold text-foreground">{t('payloadRules.formModels')}</label>
                <Input
                  value={form.models}
                  placeholder={t('payloadRules.formModelsPlaceholder')}
                  className="h-8 font-mono text-xs"
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setForm({ ...form, models: e.target.value })}
                />
              </div>
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
    </div>
  )
}
