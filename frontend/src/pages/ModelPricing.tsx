import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  ArrowUpRight,
  Check,
  ChevronDown,
  CloudDownload,
  Link2,
  Loader2,
  RotateCcw,
  Save,
  Search,
  Sparkles,
  Wand2,
  X,
  ChevronsUpDown,
  AlertCircle,
  Undo2,
} from 'lucide-react'

import { api } from '@/api'
import ChannelLogo from '../components/ChannelLogo'
import ModelLogo from '../components/ModelLogo'
import Modal from '../components/Modal'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { StatTile } from '../components/StatTile'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { cn } from '@/lib/utils'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import type { ModelPricingOverride, OfficialPricingSyncConfig } from '@/types'
import {
  buildModelPricingPreview,
  type PricingPreviewRate,
} from '../lib/modelPricingPreview'

type Row = {
  model: string
  channel?: string
  source: string
  pricing: ModelPricingOverride
  canonical_model?: string
  is_alias?: boolean
}
type SourceFilter = 'all' | 'custom' | 'synced' | 'default' | 'unsaved'
type ChannelFilter = 'all' | 'codex' | 'grok' | 'antigravity' | 'claude'
const CHANNEL_ORDER: Array<Exclude<ChannelFilter, 'all'>> = ['codex', 'grok', 'antigravity', 'claude']
const CHANNEL_LABEL: Record<Exclude<ChannelFilter, 'all'>, string> = {
  codex: 'Codex',
  grok: 'Grok',
  antigravity: 'Antigravity',
  claude: 'Claude',
}
function rowChannel(r: Row): Exclude<ChannelFilter, 'all'> {
  const c = (r.channel || '').toLowerCase()
  if (c === 'grok' || c === 'antigravity' || c === 'claude') return c
  return 'codex'
}
// 已见过的模型集(localStorage):用于给新出现的模型打"新"标。首次加载会播种、不标新。
const SEEN_MODELS_KEY = 'model-pricing-seen-models-v1'
function readSeenModels(): Set<string> | null {
  if (typeof window === 'undefined') return new Set()
  const raw = window.localStorage.getItem(SEEN_MODELS_KEY)
  if (raw == null) return null
  try {
    return new Set((JSON.parse(raw) as string[]).map((m) => m.toLowerCase()))
  } catch {
    return new Set()
  }
}
function writeSeenModels(models: string[]) {
  if (typeof window === 'undefined') return
  try {
    window.localStorage.setItem(SEEN_MODELS_KEY, JSON.stringify(models.map((m) => m.toLowerCase())))
  } catch {
    // ignore
  }
}

type FieldDef = {
  key: keyof ModelPricingOverride
  labelKey: string
  shortKey: string
  tone: 'neutral' | 'accent'
}

const PRIMARY_FIELDS: FieldDef[] = [
  { key: 'input', labelKey: 'settings.pricing.input', shortKey: 'settings.pricing.shortInput', tone: 'neutral' },
  { key: 'cached_input', labelKey: 'settings.pricing.cached', shortKey: 'settings.pricing.shortCached', tone: 'neutral' },
  { key: 'cache_write_5m', labelKey: 'settings.pricing.cacheWrite5m', shortKey: 'settings.pricing.shortCacheWrite5m', tone: 'neutral' },
  { key: 'cache_write_1h', labelKey: 'settings.pricing.cacheWrite1h', shortKey: 'settings.pricing.shortCacheWrite1h', tone: 'neutral' },
  { key: 'output', labelKey: 'settings.pricing.output', shortKey: 'settings.pricing.shortOutput', tone: 'neutral' },
]

const ADVANCED_FIELDS: FieldDef[] = [
  { key: 'input_priority', labelKey: 'settings.pricing.inputPriority', shortKey: 'settings.pricing.shortInputPriority', tone: 'accent' },
	{ key: 'cached_input_priority', labelKey: 'settings.pricing.cachedInputPriority', shortKey: 'settings.pricing.shortCachedInputPriority', tone: 'accent' },
  { key: 'output_priority', labelKey: 'settings.pricing.outputPriority', shortKey: 'settings.pricing.shortOutputPriority', tone: 'accent' },
  { key: 'input_long', labelKey: 'settings.pricing.inputLong', shortKey: 'settings.pricing.shortInputLong', tone: 'accent' },
	{ key: 'cached_input_long', labelKey: 'settings.pricing.cachedInputLong', shortKey: 'settings.pricing.shortCachedInputLong', tone: 'accent' },
  { key: 'output_long', labelKey: 'settings.pricing.outputLong', shortKey: 'settings.pricing.shortOutputLong', tone: 'accent' },
	{ key: 'input_long_priority', labelKey: 'settings.pricing.inputLongPriority', shortKey: 'settings.pricing.shortInputLongPriority', tone: 'accent' },
	{ key: 'cached_input_long_priority', labelKey: 'settings.pricing.cachedInputLongPriority', shortKey: 'settings.pricing.shortCachedInputLongPriority', tone: 'accent' },
	{ key: 'output_long_priority', labelKey: 'settings.pricing.outputLongPriority', shortKey: 'settings.pricing.shortOutputLongPriority', tone: 'accent' },
]

const ALL_FIELDS = [...PRIMARY_FIELDS, ...ADVANCED_FIELDS]

const TONE_DOT: Record<FieldDef['tone'], string> = {
  neutral: 'bg-muted-foreground/40',
  accent: 'bg-primary',
}

function normalizePrice(value: unknown): number {
  const n = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(n) ? n : 0
}

function isDirty(draft: ModelPricingOverride | undefined, saved: ModelPricingOverride | undefined): boolean {
  for (const field of ALL_FIELDS) {
    if (normalizePrice(draft?.[field.key]) !== normalizePrice(saved?.[field.key])) return true
  }
  if (
    normalizePrice(draft?.long_context_threshold_tokens) !==
    normalizePrice(saved?.long_context_threshold_tokens)
  ) return true
  return false
}

function isAdvancedDirty(draft: ModelPricingOverride | undefined, saved: ModelPricingOverride | undefined): boolean {
  for (const field of ADVANCED_FIELDS) {
    if (normalizePrice(draft?.[field.key]) !== normalizePrice(saved?.[field.key])) return true
  }
  if (
    normalizePrice(draft?.long_context_threshold_tokens) !==
    normalizePrice(saved?.long_context_threshold_tokens)
  ) return true
  return false
}

function formatPriceDisplay(value: number): string {
  if (!Number.isFinite(value) || value === 0) return '0'
  if (Number.isInteger(value)) return String(value)
  return value.toFixed(4).replace(/\.?0+$/, '')
}

function getOutputMultiplier(input: number, output: number): string | null {
  if (input <= 0 || output <= 0) return null
  const ratio = output / input
  return `${ratio.toFixed(1).replace(/\.0$/, '')}x`
}

const PREFERRED_MODEL_ORDER = ['gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna'] as const

function modelPreferredRank(model: string): number {
  const lower = model.trim().toLowerCase()
  for (let i = 0; i < PREFERRED_MODEL_ORDER.length; i += 1) {
    const preferred = PREFERRED_MODEL_ORDER[i]
    if (lower === preferred || lower.startsWith(`${preferred}-`) || lower.startsWith(`${preferred}(`)) {
      return i
    }
  }
  return -1
}

function modelVersionParts(model: string): number[] {
  const matches = model.match(/\d+/g)
  if (!matches) return []
  return matches.map((m) => Number(m)).filter((n) => Number.isFinite(n))
}

function modelFamilyRank(model: string): number {
  return model.trim().toLowerCase().startsWith('grok') ? 1 : 0
}

function compareModelsNewestFirst(a: string, b: string): number {
  if (a === b) return 0
  const fa = modelFamilyRank(a)
  const fb = modelFamilyRank(b)
  if (fa !== fb) return fa - fb
  const ra = modelPreferredRank(a)
  const rb = modelPreferredRank(b)
  if (ra >= 0 || rb >= 0) {
    if (ra < 0) return 1
    if (rb < 0) return -1
    if (ra !== rb) return ra - rb
    return a.localeCompare(b)
  }
  const va = modelVersionParts(a)
  const vb = modelVersionParts(b)
  if (va.length === 0 && vb.length === 0) return a.localeCompare(b)
  if (va.length === 0) return 1
  if (vb.length === 0) return -1
  const n = Math.min(va.length, vb.length)
  for (let i = 0; i < n; i += 1) {
    if (va[i] !== vb[i]) return vb[i] - va[i]
  }
  if (va.length !== vb.length) return vb.length - va.length
  return a.localeCompare(b)
}

function sourceMeta(source: string): { labelKey: string; className: string; dot: string } {
  if (source === 'custom') {
    return {
      labelKey: 'settings.pricing.source.custom',
      className: 'bg-primary/10 text-primary ring-primary/15',
      dot: 'bg-primary',
    }
  }
  if (source === 'synced') {
    return {
      labelKey: 'settings.pricing.source.synced',
      className: 'bg-sky-500/10 text-sky-700 ring-sky-500/15 dark:text-sky-300',
      dot: 'bg-sky-500',
    }
  }
  return {
    labelKey: 'settings.pricing.source.default',
    className: 'bg-muted text-muted-foreground ring-border/60',
    dot: 'bg-muted-foreground/50',
  }
}

function PriceField({
  field,
  value,
  savedValue,
  changed,
  dense,
  onChange,
  onRevert,
}: {
  field: FieldDef
  value: number
  savedValue?: number
  changed: boolean
  dense?: boolean
  onChange: (next: string) => void
  onRevert?: () => void
}) {
  const { t } = useTranslation()

  return (
    <label
      className={cn(
        'group relative flex min-w-0 flex-col rounded-xl border bg-background/80 transition-all',
        dense ? 'gap-1.5 p-2.5 sm:p-3' : 'gap-2 p-3 sm:p-3.5',
        changed
          ? 'border-amber-500/40 bg-amber-500/5 ring-1 ring-amber-500/30'
          : 'border-border/80 hover:border-border hover:bg-card',
        'focus-within:border-primary/40 focus-within:ring-[3px] focus-within:ring-primary/15',
      )}
    >
      <div className="flex items-center justify-between gap-1.5">
        <div className="flex min-w-0 items-center gap-1.5">
          <span className={cn('size-1.5 shrink-0 rounded-full', TONE_DOT[field.tone])} aria-hidden />
          <span className="truncate text-[11px] font-semibold tracking-wide text-muted-foreground">
            {t(field.labelKey)}
          </span>
        </div>
        {changed && onRevert ? (
          <button
            type="button"
            onClick={(e) => {
              e.preventDefault()
              onRevert()
            }}
            title={savedValue !== undefined ? `还原为 $${savedValue}` : '还原'}
            className="flex size-5 items-center justify-center rounded-md text-amber-600 transition-colors hover:bg-amber-500/20 dark:text-amber-400"
          >
            <Undo2 className="size-3" />
          </button>
        ) : null}
      </div>
      <div className="relative">
        <span className="pointer-events-none absolute left-0 top-1/2 -translate-y-1/2 text-sm font-medium text-muted-foreground/70">
          $
        </span>
        <input
          type="number"
          step="0.01"
          min={0}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          className={cn(
            'w-full border-0 bg-transparent pl-4 font-mono font-semibold tabular-nums tracking-tight text-foreground outline-none',
            dense ? 'h-7 text-[15px]' : 'h-8 text-lg sm:text-[1.35rem]',
            '[appearance:textfield] [&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none',
          )}
        />
      </div>
      <span className="text-[10px] font-medium text-muted-foreground/70">/ 1M tok</span>
    </label>
  )
}

function ContextThresholdField({
  value,
  savedValue,
  changed,
  onChange,
  onRevert,
}: {
  value: number
  savedValue: number
  changed: boolean
  onChange: (next: string) => void
  onRevert: () => void
}) {
  const { t } = useTranslation()
  return (
    <label
      className={cn(
        'group relative flex min-w-0 flex-col gap-1.5 rounded-xl border bg-background/80 p-2.5 transition-all sm:p-3',
        changed
          ? 'border-amber-500/40 bg-amber-500/5 ring-1 ring-amber-500/30'
          : 'border-border/80 hover:bg-card',
        'focus-within:border-primary/40 focus-within:ring-[3px] focus-within:ring-primary/15',
      )}
    >
      <div className="flex items-center justify-between gap-1.5">
        <span className="truncate text-[11px] font-semibold tracking-wide text-muted-foreground">
          {t('settings.pricing.contextThreshold')}
        </span>
        {changed ? (
          <button
            type="button"
            onClick={(event) => {
              event.preventDefault()
              onRevert()
            }}
            title={t('settings.pricing.revertThreshold', { value: savedValue })}
            className="flex size-5 items-center justify-center rounded-md text-amber-600 transition-colors hover:bg-amber-500/20 dark:text-amber-400"
          >
            <Undo2 className="size-3" />
          </button>
        ) : null}
      </div>
      <input
        type="number"
        step={1}
        min={0}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        className="h-7 w-full border-0 bg-transparent font-mono text-[15px] font-semibold tabular-nums text-foreground outline-none [appearance:textfield] [&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none"
      />
      <span className="text-[10px] font-medium text-muted-foreground/70">tokens</span>
    </label>
  )
}

function formatPreviewRate(rate: PricingPreviewRate) {
  return `$${formatPriceDisplay(rate.input)} / $${formatPriceDisplay(rate.cached)} / $${formatPriceDisplay(rate.output)}`
}

function BillingRulePreview({ pricing }: { pricing: ModelPricingOverride }) {
  const { t } = useTranslation()
  const preview = buildModelPricingPreview(pricing)
  const badge =
    preview.mode === 'tiered'
      ? t('settings.pricing.tiered')
      : t('settings.pricing.singleTier')

  return (
    <section className='mt-4 rounded-xl border border-border/80 bg-muted/[0.18] p-3.5 sm:p-4'>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <div className='flex items-center gap-2'>
          <span className='size-1.5 rounded-full bg-primary' aria-hidden />
          <h5 className='text-[12px] font-semibold text-foreground'>
            {t('settings.pricing.billingPreview')}
          </h5>
          <span className='rounded-full bg-background px-2 py-0.5 text-[10px] font-bold text-muted-foreground ring-1 ring-inset ring-border/70'>
            {badge}
          </span>
        </div>
        {preview.long ? (
          <span className='text-[10px] font-medium text-muted-foreground'>
            {t('settings.pricing.thresholdSummary', {
              value: preview.threshold.toLocaleString(),
            })}
          </span>
        ) : null}
      </div>

      <div className='mt-3 grid gap-2 sm:grid-cols-2 xl:grid-cols-4'>
        <div className='rounded-lg border border-border/70 bg-background/70 px-3 py-2.5'>
          <div className='text-[10px] font-semibold uppercase tracking-wide text-muted-foreground'>
            {t('settings.pricing.standardRate')}
          </div>
          <div className='mt-1 font-mono text-xs font-semibold tabular-nums text-foreground'>
            {formatPreviewRate(preview.standard)}
          </div>
          <div className='mt-0.5 text-[10px] text-muted-foreground'>
            input / cache read / output · USD/M
          </div>
        </div>
        {preview.long ? (
          <div className='rounded-lg border border-primary/20 bg-primary/[0.04] px-3 py-2.5'>
            <div className='text-[10px] font-semibold uppercase tracking-wide text-primary'>
              {t('settings.pricing.longRate')}
            </div>
            <div className='mt-1 font-mono text-xs font-semibold tabular-nums text-foreground'>
              {formatPreviewRate(preview.long)}
            </div>
            <div className='mt-0.5 text-[10px] text-muted-foreground'>
              {t('settings.pricing.fromTokens', {
                value: preview.threshold.toLocaleString(),
              })}
            </div>
          </div>
        ) : null}
        {preview.priority ? (
          <div className='rounded-lg border border-amber-500/20 bg-amber-500/[0.04] px-3 py-2.5'>
            <div className='text-[10px] font-semibold uppercase tracking-wide text-amber-700 dark:text-amber-300'>
              {t('settings.pricing.priorityRate')}
            </div>
            <div className='mt-1 font-mono text-xs font-semibold tabular-nums text-foreground'>
              {formatPreviewRate(preview.priority)}
            </div>
            <div className='mt-0.5 text-[10px] text-muted-foreground'>
              {t('settings.pricing.priorityHint')}
            </div>
          </div>
        ) : null}
        {preview.flexMultiplier ? (
          <div className='rounded-lg border border-sky-500/20 bg-sky-500/[0.04] px-3 py-2.5'>
            <div className='text-[10px] font-semibold uppercase tracking-wide text-sky-700 dark:text-sky-300'>
              {t('settings.pricing.flexRate')}
            </div>
            <div className='mt-1 font-mono text-xs font-semibold tabular-nums text-foreground'>
              ×{preview.flexMultiplier}
            </div>
            <div className='mt-0.5 text-[10px] text-muted-foreground'>
              {t('settings.pricing.flexHint')}
            </div>
          </div>
        ) : null}
      </div>

      <div className='mt-3 rounded-lg border border-dashed border-border/80 bg-background/55 px-3 py-2.5'>
        <div className='text-[10px] font-semibold uppercase tracking-wide text-muted-foreground'>
          {t('settings.pricing.expressionPreview')}
        </div>
        <code className='mt-1 block break-all font-mono text-[10px] leading-relaxed text-muted-foreground'>
          {preview.expression}
        </code>
      </div>
    </section>
  )
}

// ModelCatalogModal 是"模型目录"弹窗:按 provider 分组、可搜索、点击某模型直接定位到
// 价格行;可刷新账号真实可用模型;新出现的模型标"新",便于快速锁定。
function ModelCatalogModal({
  open,
  onClose,
  rows,
  newModels,
  query,
  onQueryChange,
  onJump,
  onRefresh,
  refreshing,
  onAcknowledge,
}: {
  open: boolean
  onClose: () => void
  rows: Row[]
  newModels: Set<string>
  query: string
  onQueryChange: (v: string) => void
  onJump: (model: string) => void
  onRefresh: () => void
  refreshing: boolean
  onAcknowledge: () => void
}) {
  const { t } = useTranslation()
  const q = query.trim().toLowerCase()
  const groups = useMemo(() => {
    const map = new Map<string, Row[]>()
    for (const r of rows) {
      if (q && !r.model.toLowerCase().includes(q)) continue
      const c = rowChannel(r)
      const arr = map.get(c) || []
      arr.push(r)
      map.set(c, arr)
    }
    for (const arr of map.values()) arr.sort((a, b) => compareModelsNewestFirst(a.model, b.model))
    return CHANNEL_ORDER.filter((c) => map.has(c)).map((c) => ({ channel: c, rows: map.get(c)! }))
  }, [rows, q])

  return (
    <Modal
      show={open}
      onClose={onClose}
      title={t('settings.pricing.catalogTitle')}
      contentClassName="sm:max-w-[640px]"
      footer={
        <div className="flex w-full items-center justify-between gap-2">
          <span className="text-xs text-muted-foreground">
            {t('settings.pricing.catalogCount', { count: rows.length })}
          </span>
          <div className="flex items-center gap-2">
            {newModels.size > 0 ? (
              <Button variant="ghost" size="sm" onClick={onAcknowledge}>
                {t('settings.pricing.catalogMarkSeen')}
              </Button>
            ) : null}
            <Button variant="outline" size="sm" className="gap-1.5" onClick={onRefresh} disabled={refreshing}>
              {refreshing ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCcw className="size-3.5" />}
              {t('settings.pricing.catalogRefresh')}
            </Button>
          </div>
        </div>
      }
    >
      <div className="space-y-3">
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => onQueryChange(e.target.value)}
            placeholder={t('settings.pricing.catalogSearch')}
            className="pl-8"
          />
        </div>
        {groups.length === 0 ? (
          <p className="py-8 text-center text-sm text-muted-foreground">{t('settings.pricing.emptyFiltered')}</p>
        ) : (
          groups.map((group) => (
            <div key={group.channel} className="space-y-1">
              <div className="flex items-center gap-1.5 px-1 pt-1">
                <ChannelLogo channel={group.channel} size={14} />
                <span className="text-xs font-semibold text-foreground/80">{CHANNEL_LABEL[group.channel]}</span>
                <span className="text-[10px] text-muted-foreground">{group.rows.length}</span>
              </div>
              <div className="grid gap-1 sm:grid-cols-2">
                {group.rows.map((r) => {
                  const isNew = newModels.has(r.model.toLowerCase())
                  return (
                    <button
                      key={r.model}
                      type="button"
                      onClick={() => onJump(r.model)}
                      className="flex items-center justify-between gap-2 rounded-lg border border-border/70 bg-background/60 px-2.5 py-1.5 text-left transition-colors hover:border-primary/40 hover:bg-accent/50"
                    >
                      <span className="truncate font-mono text-[12px] text-foreground">{r.model}</span>
                      <span className="flex shrink-0 items-center gap-1">
                        {isNew ? (
                          <span className="rounded-full bg-rose-500/15 px-1.5 py-0.5 text-[9px] font-bold text-rose-600 ring-1 ring-inset ring-rose-500/25 dark:text-rose-300">
                            {t('settings.pricing.newBadge')}
                          </span>
                        ) : null}
                        <span className="tabular-nums text-[11px] text-muted-foreground">
                          ${formatPriceDisplay(normalizePrice(r.pricing.input))}/${formatPriceDisplay(normalizePrice(r.pricing.output))}
                        </span>
                      </span>
                    </button>
                  )
                })}
              </div>
            </div>
          ))
        )}
      </div>
    </Modal>
  )
}

export default function ModelPricing() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [rows, setRows] = useState<Row[]>([])
  const [drafts, setDrafts] = useState<Record<string, ModelPricingOverride>>({})
  const [syncUrl, setSyncUrl] = useState('')
  const [defaultUrl, setDefaultUrl] = useState('')
  const [modelsDevUrl, setModelsDevUrl] = useState('')
  const [officialOpenAIUrl, setOfficialOpenAIUrl] = useState('')
  const [officialXAIUrl, setOfficialXAIUrl] = useState('')
  const [officialClaudeUrl, setOfficialClaudeUrl] = useState('')
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [syncing, setSyncing] = useState(false)
  const [bulkSaving, setBulkSaving] = useState(false)
  const [officialSyncing, setOfficialSyncing] = useState(false)
  const [officialSaving, setOfficialSaving] = useState(false)
  const [officialConfig, setOfficialConfig] = useState<OfficialPricingSyncConfig>({
    enabled: false,
    interval_minutes: 1440,
    include_openai: true,
    include_grok: true,
    include_claude: true,
  })
  const [savingModel, setSavingModel] = useState('')
  const [query, setQuery] = useState('')
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>('all')
  const [channelFilter, setChannelFilter] = useState<ChannelFilter>('all')
  const [catalogOpen, setCatalogOpen] = useState(false)
  const [catalogQuery, setCatalogQuery] = useState('')
  const [jumpedModel, setJumpedModel] = useState('')
  const [refreshingModels, setRefreshingModels] = useState(false)
  const [seenBump, setSeenBump] = useState(0)
  const [syncOpen, setSyncOpen] = useState(false)
  const [expandedAdvanced, setExpandedAdvanced] = useState<Record<string, boolean>>({})

  const load = useCallback(async () => {
    setLoading(true)
    setLoadError(null)
    try {
      const res = await api.listModelPricing()
      setRows(res.models)
      setDefaultUrl(res.default_sync_url)
      setModelsDevUrl(res.models_dev_url)
      setOfficialOpenAIUrl(res.official_openai_url)
      setOfficialXAIUrl(res.official_xai_url)
      setOfficialClaudeUrl(res.official_claude_url)
      setSyncUrl(res.sync_url || '')
      setOfficialConfig(res.official_sync_config)
      const d: Record<string, ModelPricingOverride> = {}
      for (const r of res.models) d[r.model] = { ...r.pricing }
      setDrafts(d)
    } catch (error) {
      const msg = getErrorMessage(error)
      setLoadError(msg)
      showToast(msg, 'error')
    } finally {
      setLoading(false)
    }
  }, [showToast])

  useEffect(() => {
    void load()
  }, [load])

  const setField = (model: string, key: keyof ModelPricingOverride, value: string) => {
    const num = value.trim() === '' ? 0 : Number(value)
    setDrafts((prev) => ({
      ...prev,
      [model]: { ...prev[model], [key]: Number.isFinite(num) ? num : 0 },
    }))
  }

  const revertField = (model: string, key: keyof ModelPricingOverride) => {
    const row = rows.find((r) => r.model === model)
    if (!row) return
    const origVal = row.pricing[key] ?? 0
    setDrafts((prev) => ({
      ...prev,
      [model]: { ...prev[model], [key]: origVal },
    }))
  }

  const save = async (model: string) => {
    setSavingModel(model)
    try {
      await api.updateModelPricing({ model, pricing: drafts[model] })
      showToast(t('settings.pricing.saved', { model }))
      await load()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSavingModel('')
    }
  }

  const saveAllDirty = async () => {
    setBulkSaving(true)
    try {
      const dirtyModels = rows.filter((r) => isDirty(drafts[r.model], r.pricing))
      for (const r of dirtyModels) {
        await api.updateModelPricing({ model: r.model, pricing: drafts[r.model] })
      }
      showToast(t('settings.pricing.saved', { model: `${dirtyModels.length}` }))
      await load()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setBulkSaving(false)
    }
  }

  const discardAllChanges = () => {
    const resetDrafts: Record<string, ModelPricingOverride> = {}
    for (const r of rows) resetDrafts[r.model] = { ...r.pricing }
    setDrafts(resetDrafts)
  }

  const reset = async (model: string) => {
    setSavingModel(model)
    try {
      await api.updateModelPricing({ model, reset: true })
      showToast(t('settings.pricing.reset', { model }))
      await load()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSavingModel('')
    }
  }

  const sync = async () => {
    setSyncing(true)
    try {
      const res = await api.syncModelPricing(syncUrl)
      showToast(t('settings.pricing.syncDone', { applied: res.applied, skipped: res.skipped }))
      await load()
    } catch (error) {
      showToast(`${t('settings.pricing.syncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncing(false)
    }
  }

  const toggleAllAdvanced = () => {
    const allExpanded = rows.every((r) => expandedAdvanced[r.model])
    const nextState: Record<string, boolean> = {}
    for (const r of rows) nextState[r.model] = !allExpanded
    setExpandedAdvanced(nextState)
  }

  const saveOfficialConfig = async () => {
    setOfficialSaving(true)
    try {
      const saved = await api.updateOfficialPricingSyncConfig(officialConfig)
      setOfficialConfig(saved)
      showToast(t('settings.pricing.officialConfigSaved'))
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setOfficialSaving(false)
    }
  }

  const syncOfficial = async () => {
    setOfficialSyncing(true)
    try {
      const result = await api.syncOfficialModelPricing({
        include_openai: officialConfig.include_openai,
        include_grok: officialConfig.include_grok,
        include_claude: officialConfig.include_claude,
      })
      showToast(t('settings.pricing.officialSyncDone', { applied: result.applied, skipped: result.skipped }))
      await load()
    } catch (error) {
      showToast(`${t('settings.pricing.syncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setOfficialSyncing(false)
    }
  }

  const activePreset = useMemo(() => {
    const url = syncUrl.trim()
    if (url === '' || url === defaultUrl) return 'default'
    if (modelsDevUrl && url === modelsDevUrl) return 'modelsdev'
    return 'custom'
  }, [defaultUrl, modelsDevUrl, syncUrl])

  const counts = useMemo(() => {
    let custom = 0
    let synced = 0
    let defaults = 0
    let unsaved = 0
    for (const r of rows) {
      if (r.source === 'custom') custom += 1
      else if (r.source === 'synced') synced += 1
      else defaults += 1

      if (isDirty(drafts[r.model], r.pricing)) unsaved += 1
    }
    return { total: rows.length, custom, synced, defaults, unsaved }
  }, [drafts, rows])

  const dirtyCount = counts.unsaved

  // 各 provider(渠道)模型数量:仅当存在多于一个渠道时才显示渠道过滤条。
  const channelCounts = useMemo(() => {
    const m: Record<string, number> = { codex: 0, grok: 0, antigravity: 0, claude: 0 }
    for (const r of rows) m[rowChannel(r)] += 1
    return m
  }, [rows])
  const activeChannels = CHANNEL_ORDER.filter((c) => channelCounts[c] > 0)

  const filteredRows = useMemo(() => {
    const q = query.trim().toLowerCase()
    return rows
      .filter((r) => {
        if (channelFilter !== 'all' && rowChannel(r) !== channelFilter) return false
        if (sourceFilter === 'unsaved') {
          if (!isDirty(drafts[r.model], r.pricing)) return false
        } else if (sourceFilter !== 'all' && r.source !== sourceFilter) {
          return false
        }
        if (q && !r.model.toLowerCase().includes(q)) return false
        return true
      })
      .slice()
      .sort((a, b) => compareModelsNewestFirst(a.model, b.model))
  }, [drafts, query, rows, sourceFilter, channelFilter])

  // 当前视图下按 provider 分组(用于分组小标题)。
  const groupedRows = useMemo(() => {
    const groups = new Map<string, Row[]>()
    for (const r of filteredRows) {
      const c = rowChannel(r)
      const arr = groups.get(c) || []
      arr.push(r)
      groups.set(c, arr)
    }
    return CHANNEL_ORDER.filter((c) => groups.has(c)).map((c) => ({ channel: c, rows: groups.get(c)! }))
  }, [filteredRows])

  // 新模型集:localStorage 里没见过的模型。首次加载(localStorage 为空)时播种、不标新。
  const newModels = useMemo(() => {
    const set = new Set<string>()
    if (rows.length === 0) return set
    const seen = readSeenModels()
    if (seen === null) return set
    for (const r of rows) {
      if (!seen.has(r.model.toLowerCase())) set.add(r.model.toLowerCase())
    }
    return set
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows, seenBump])

  useEffect(() => {
    // 首次加载后播种"已见"集,使后续新出现的模型才被标"新"。
    if (rows.length > 0 && readSeenModels() === null) {
      writeSeenModels(rows.map((r) => r.model))
    }
  }, [rows])

  const jumpToModel = useCallback((model: string) => {
    setCatalogOpen(false)
    setChannelFilter('all')
    setSourceFilter('all')
    setQuery('')
    setJumpedModel(model.toLowerCase())
    requestAnimationFrame(() => {
      const el = document.getElementById(`pricing-row-${model.toLowerCase()}`)
      if (el) el.scrollIntoView({ behavior: 'smooth', block: 'center' })
      window.setTimeout(() => setJumpedModel(''), 2200)
    })
  }, [])

  const refreshCatalogModels = useCallback(async () => {
    setRefreshingModels(true)
    try {
      const res = await api.refreshAllClaudeModels()
      showToast(t('settings.pricing.catalogRefreshed', { count: res.model_count }))
      await load()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setRefreshingModels(false)
    }
  }, [load, showToast, t])

  const acknowledgeNewModels = useCallback(() => {
    writeSeenModels(rows.map((r) => r.model))
    setSeenBump((n) => n + 1)
  }, [rows])

  const sourceFilters: Array<{ id: SourceFilter; label: string; count: number }> = [
    { id: 'all', label: t('settings.pricing.filterAll'), count: counts.total },
    { id: 'custom', label: t('settings.pricing.source.custom'), count: counts.custom },
    { id: 'synced', label: t('settings.pricing.source.synced'), count: counts.synced },
    { id: 'default', label: t('settings.pricing.source.default'), count: counts.defaults },
    { id: 'unsaved', label: t('settings.pricing.filterUnsaved'), count: counts.unsaved },
  ]

  const isAllAdvancedExpanded = useMemo(() => {
    if (rows.length === 0) return false
    return rows.every((r) => expandedAdvanced[r.model])
  }, [expandedAdvanced, rows])

  return (
    <div className="relative pb-16 w-full min-w-0">
      <PageHeader
        title={t('settings.pricing.title')}
        description={t('settings.pricing.desc')}
        onRefresh={() => void load()}
        actions={
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              className="gap-1.5"
              onClick={() => { setCatalogQuery(''); setCatalogOpen(true) }}
            >
              <ChevronsUpDown className="size-3.5" />
              {t('settings.pricing.catalogTitle')}
              {newModels.size > 0 ? (
                <span className="ml-0.5 inline-flex min-w-4 items-center justify-center rounded-full bg-rose-500/90 px-1 text-[10px] font-bold text-white">
                  {newModels.size}
                </span>
              ) : null}
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="gap-1.5"
              onClick={() => setSyncOpen((v) => !v)}
            >
              <CloudDownload className="size-3.5" />
              {t('settings.pricing.syncTitle')}
              <ChevronDown className={cn('size-3.5 transition-transform', syncOpen && 'rotate-180')} />
            </Button>
          </div>
        }
      />
      <ModelCatalogModal
        open={catalogOpen}
        onClose={() => setCatalogOpen(false)}
        rows={rows}
        newModels={newModels}
        query={catalogQuery}
        onQueryChange={setCatalogQuery}
        onJump={jumpToModel}
        onRefresh={() => void refreshCatalogModels()}
        refreshing={refreshingModels}
        onAcknowledge={acknowledgeNewModels}
      />

      <StateShell
        variant="page"
        loading={loading && rows.length === 0}
        error={loadError && rows.length === 0 ? loadError : null}
        onRetry={() => void load()}
      >
        <div className="space-y-5 sm:space-y-6">
          {/* Source metrics */}
          <div className="grid grid-cols-2 gap-2.5 sm:gap-4 xl:grid-cols-4">
            <StatTile
              label={t('settings.pricing.statTotal')}
              value={counts.total}
              sub={t('settings.pricing.unitHint')}
              active={sourceFilter === 'all'}
              onClick={() => setSourceFilter('all')}
            />
            <StatTile
              label={t('settings.pricing.statCustom')}
              value={counts.custom}
              active={sourceFilter === 'custom'}
              onClick={() => setSourceFilter('custom')}
            />
            <StatTile
              label={t('settings.pricing.statSynced')}
              value={counts.synced}
              active={sourceFilter === 'synced'}
              onClick={() => setSourceFilter('synced')}
            />
            <StatTile
              label={t('settings.pricing.statDefault')}
              value={counts.defaults}
              active={sourceFilter === 'default'}
              onClick={() => setSourceFilter('default')}
            />
          </div>

          {/* Sync panel */}
          <div
            className={cn(
              'grid transition-[grid-template-rows,opacity] duration-300 ease-out',
              syncOpen ? 'grid-rows-[1fr] opacity-100' : 'grid-rows-[0fr] opacity-0',
            )}
          >
            <div className="min-h-0 overflow-hidden">
              <section className="rounded-xl border border-border/80 bg-card p-4 shadow-sm sm:p-5">
                <div className="flex flex-col gap-4 lg:flex-row lg:items-start">
                  <div className="flex min-w-0 flex-1 gap-3">
                    <div className="flex size-11 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary ring-1 ring-inset ring-primary/15">
                      <CloudDownload className="size-5" />
                    </div>
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-2">
                        <h3 className="text-base font-semibold tracking-tight text-foreground">
                          {t('settings.pricing.syncTitle')}
                        </h3>
                        <span className="rounded-full bg-muted px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
                          {activePreset === 'default'
                            ? t('settings.pricing.presetDefault')
                            : activePreset === 'modelsdev'
                              ? 'models.dev'
                              : t('settings.pricing.presetCustom')}
                        </span>
                      </div>
                      <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
                        {t('settings.pricing.syncSubtitle')}
                      </p>
                    </div>
                  </div>
                  <button
                    type="button"
                    className="self-start rounded-lg p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground lg:ml-auto"
                    onClick={() => setSyncOpen(false)}
                    aria-label={t('common.close')}
                  >
                    <X className="size-4" />
                  </button>
                </div>

                <div className="mt-5 space-y-3">
					<div className="rounded-xl border border-primary/20 bg-primary/[0.03] p-4">
						<div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
							<div>
								<div className="flex flex-wrap items-center gap-2">
									<h4 className="text-sm font-semibold text-foreground">{t('settings.pricing.officialTitle')}</h4>
									<span className="rounded-full bg-emerald-500/10 px-2 py-0.5 text-[10px] font-bold text-emerald-700 dark:text-emerald-300">{t('settings.pricing.authoritative')}</span>
								</div>
								<p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('settings.pricing.officialDesc')}</p>
								<div className="mt-2 flex flex-wrap gap-3 text-[11px] font-semibold">
									<a href={officialOpenAIUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">OpenAI <ArrowUpRight className="size-3" /></a>
									<a href={officialXAIUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">xAI <ArrowUpRight className="size-3" /></a>
									<a href={officialClaudeUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">Anthropic <ArrowUpRight className="size-3" /></a>
								</div>
							</div>
							<Button className="shrink-0" onClick={() => void syncOfficial()} disabled={officialSyncing || (!officialConfig.include_openai && !officialConfig.include_grok && !officialConfig.include_claude)}>
								{officialSyncing ? <Loader2 className="size-3.5 animate-spin" /> : <CloudDownload className="size-3.5" />}
								{officialSyncing ? t('settings.pricing.syncing') : t('settings.pricing.officialSyncNow')}
							</Button>
						</div>
						<div className="mt-4 grid gap-3 sm:grid-cols-2">
							<label className="flex items-center justify-between gap-3 rounded-lg border border-border bg-background/80 px-3 py-2.5">
								<span className="text-sm font-medium">OpenAI / Codex</span>
								<Switch checked={officialConfig.include_openai} onCheckedChange={(checked) => setOfficialConfig((cfg) => ({ ...cfg, include_openai: checked }))} />
							</label>
							<label className="flex items-center justify-between gap-3 rounded-lg border border-border bg-background/80 px-3 py-2.5">
								<span className="text-sm font-medium">xAI / Grok</span>
								<Switch checked={officialConfig.include_grok} onCheckedChange={(checked) => setOfficialConfig((cfg) => ({ ...cfg, include_grok: checked }))} />
							</label>
							<label className="flex items-center justify-between gap-3 rounded-lg border border-border bg-background/80 px-3 py-2.5">
								<span className="inline-flex items-center gap-1.5 text-sm font-medium"><ChannelLogo channel="claude" size={14} />Anthropic / Claude</span>
								<Switch checked={officialConfig.include_claude} onCheckedChange={(checked) => setOfficialConfig((cfg) => ({ ...cfg, include_claude: checked }))} />
							</label>
						</div>
						<div className="mt-3 flex flex-col gap-3 rounded-lg border border-border bg-background/80 p-3 sm:flex-row sm:items-center">
							<label className="flex flex-1 items-center justify-between gap-3">
								<span>
									<span className="block text-sm font-medium">{t('settings.pricing.autoOfficialSync')}</span>
									<span className="block text-[11px] text-muted-foreground">{t('settings.pricing.autoOfficialSyncHint')}</span>
								</span>
								<Switch checked={officialConfig.enabled} onCheckedChange={(enabled) => setOfficialConfig((cfg) => ({ ...cfg, enabled }))} />
							</label>
							<label className="flex items-center gap-2 text-xs text-muted-foreground">
								{t('settings.pricing.intervalMinutes')}
								<Input
									type="number"
									min={60}
									max={10080}
									className="h-9 w-28"
									value={officialConfig.interval_minutes}
									onChange={(event) => setOfficialConfig((cfg) => ({ ...cfg, interval_minutes: Number(event.target.value) }))}
								/>
							</label>
							<Button variant="outline" size="sm" onClick={() => void saveOfficialConfig()} disabled={officialSaving || (!officialConfig.include_openai && !officialConfig.include_grok && !officialConfig.include_claude)}>
								{officialSaving ? <Loader2 className="size-3.5 animate-spin" /> : <Save className="size-3.5" />}
								{t('common.save')}
							</Button>
						</div>
						{officialConfig.last_success_at ? (
							<p className="mt-2 text-[11px] text-muted-foreground">{t('settings.pricing.lastOfficialSuccess')}: {new Date(officialConfig.last_success_at).toLocaleString()}</p>
						) : null}
						{officialConfig.last_error ? <p className="mt-1 break-all text-[11px] text-destructive">{officialConfig.last_error}</p> : null}
						{officialConfig.last_warning ? <p className="mt-1 break-all text-[11px] text-amber-700 dark:text-amber-300">{t('settings.pricing.lastWarning')}: {officialConfig.last_warning}</p> : null}
					</div>

					<div className="pt-1 text-xs font-semibold text-muted-foreground">{t('settings.pricing.referenceTitle')}</div>
                  <div className="flex flex-col gap-2.5 sm:flex-row sm:items-center">
                    <div className="relative min-w-0 flex-1">
                      <Link2 className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                      <Input
                        className="h-11 rounded-xl border-border/80 bg-muted/20 pl-9 font-mono text-xs shadow-none"
                        value={syncUrl}
                        placeholder={defaultUrl}
                        onChange={(e) => setSyncUrl(e.target.value)}
                      />
                    </div>
                    <Button
                      className="h-11 shrink-0 rounded-xl px-5"
                      onClick={() => void sync()}
                      disabled={syncing}
                    >
                      {syncing ? (
                        <Loader2 className="size-3.5 animate-spin" />
                      ) : (
                        <ArrowUpRight className="size-3.5" />
                      )}
                      {syncing ? t('settings.pricing.syncing') : t('settings.pricing.syncNow')}
                    </Button>
                  </div>

                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
                      {t('settings.pricing.presets')}
                    </span>
                    <button
                      type="button"
                      onClick={() => setSyncUrl('')}
                      className={cn(
                        'inline-flex h-8 items-center gap-1.5 rounded-full border px-3 text-xs font-semibold transition-all',
                        activePreset === 'default'
                          ? 'border-primary/30 bg-primary text-primary-foreground shadow-sm'
                          : 'border-border bg-background text-muted-foreground hover:border-border hover:bg-muted/50 hover:text-foreground',
                      )}
                    >
                      <Sparkles className="size-3" />
                      {t('settings.pricing.presetDefault')}
                    </button>
                    <button
                      type="button"
                      onClick={() => setSyncUrl(modelsDevUrl)}
                      disabled={!modelsDevUrl}
                      className={cn(
                        'inline-flex h-8 items-center gap-1.5 rounded-full border px-3 text-xs font-semibold transition-all disabled:opacity-40',
                        activePreset === 'modelsdev'
                          ? 'border-primary/30 bg-primary text-primary-foreground shadow-sm'
                          : 'border-border bg-background text-muted-foreground hover:border-border hover:bg-muted/50 hover:text-foreground',
                      )}
                    >
                      <Wand2 className="size-3" />
                      models.dev
                    </button>
                  </div>

                  <p className="rounded-xl border border-dashed border-border/80 bg-muted/25 px-3.5 py-3 text-[12px] leading-relaxed text-muted-foreground">
                    {t('settings.pricing.hint')}
                  </p>
                </div>
              </section>
            </div>
          </div>

          {/* Sticky toolbar */}
          <div className="sticky top-2 z-20 -mx-1 px-1">
            <div className="flex flex-col gap-3 rounded-xl border border-border/80 bg-card/95 p-2.5 shadow-sm backdrop-blur-xl sm:flex-row sm:items-center sm:justify-between sm:p-2 sm:pl-3">
              <div className="flex min-w-0 flex-1 items-center gap-2">
                <div className="relative min-w-0 flex-1 sm:max-w-xs">
                  <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                  <Input
                    className="h-9 border-transparent bg-muted/40 pl-9 text-sm shadow-none focus-visible:bg-background"
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                    placeholder={t('settings.pricing.searchPlaceholder')}
                  />
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={toggleAllAdvanced}
                  className="h-9 shrink-0 gap-1.5 px-2.5 text-xs text-muted-foreground hover:text-foreground"
                  title={isAllAdvancedExpanded ? t('settings.pricing.collapseAllAdvanced') : t('settings.pricing.expandAllAdvanced')}
                >
                  <ChevronsUpDown className="size-3.5" />
                  <span className="hidden min-[540px]:inline">
                    {isAllAdvancedExpanded ? t('settings.pricing.collapseAllAdvanced') : t('settings.pricing.expandAllAdvanced')}
                  </span>
                </Button>
              </div>

              {activeChannels.length > 1 ? (
                <div
                  className="flex max-w-full gap-0.5 overflow-x-auto rounded-xl bg-muted/50 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
                  role="tablist"
                  aria-label="provider"
                >
                  <button
                    type="button"
                    role="tab"
                    aria-selected={channelFilter === 'all'}
                    onClick={() => setChannelFilter('all')}
                    className={cn(
                      'inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-2.5 text-xs font-semibold transition-all',
                      channelFilter === 'all' ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
                    )}
                  >
                    {t('settings.pricing.filterAll')}
                    <span className="tabular-nums rounded-md bg-background/60 px-1 py-px text-[10px] font-bold text-muted-foreground">
                      {counts.total}
                    </span>
                  </button>
                  {activeChannels.map((c) => {
                    const active = channelFilter === c
                    return (
                      <button
                        key={c}
                        type="button"
                        role="tab"
                        aria-selected={active}
                        onClick={() => setChannelFilter(c)}
                        className={cn(
                          'inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-2.5 text-xs font-semibold transition-all',
                          active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
                        )}
                      >
                        <ChannelLogo channel={c} size={14} />
                        {CHANNEL_LABEL[c]}
                        <span
                          className={cn(
                            'tabular-nums rounded-md px-1 py-px text-[10px] font-bold',
                            active ? 'bg-primary/10 text-primary' : 'bg-background/60 text-muted-foreground',
                          )}
                        >
                          {channelCounts[c]}
                        </span>
                      </button>
                    )
                  })}
                </div>
              ) : null}
              <div
                className="flex max-w-full gap-0.5 overflow-x-auto rounded-xl bg-muted/50 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
                role="tablist"
              >
                {sourceFilters.map((item) => {
                  const active = sourceFilter === item.id
                  return (
                    <button
                      key={item.id}
                      type="button"
                      role="tab"
                      aria-selected={active}
                      onClick={() => setSourceFilter(item.id)}
                      className={cn(
                        'inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-2.5 text-xs font-semibold transition-all',
                        active
                          ? 'bg-background text-foreground shadow-sm'
                          : 'text-muted-foreground hover:text-foreground',
                      )}
                    >
                      {item.label}
                      <span
                        className={cn(
                          'tabular-nums rounded-md px-1 py-px text-[10px] font-bold',
                          active
                            ? item.id === 'unsaved' && item.count > 0
                              ? 'bg-amber-500/20 text-amber-600 dark:text-amber-400'
                              : 'bg-primary/10 text-primary'
                            : item.id === 'unsaved' && item.count > 0
                              ? 'bg-amber-500/10 text-amber-600 dark:text-amber-400'
                              : 'bg-background/60 text-muted-foreground',
                        )}
                      >
                        {item.count}
                      </span>
                    </button>
                  )
                })}
              </div>
            </div>
          </div>

          {/* Model list */}
          {filteredRows.length === 0 ? (
            <StateShell
              isEmpty
              emptyTitle={t('settings.pricing.emptyTitle')}
              emptyDescription={
                query || sourceFilter !== 'all' || channelFilter !== 'all'
                  ? t('settings.pricing.emptyFiltered')
                  : t('settings.pricing.emptyDesc')
              }
            >
              {null}
            </StateShell>
          ) : (
            <div className="space-y-3.5">
              <div className="flex flex-wrap items-center justify-between gap-2 px-1">
                <p className="text-xs font-medium text-muted-foreground">
                  {t('settings.pricing.listCount', {
                    shown: filteredRows.length,
                    total: counts.total,
                  })}
                </p>
                {dirtyCount > 0 ? (
                  <span className="inline-flex items-center gap-1 rounded-full bg-amber-500/10 px-2 py-0.5 text-[10px] font-bold text-amber-700 ring-1 ring-inset ring-amber-500/20 dark:text-amber-300">
                    {t('settings.pricing.unsavedCount', { count: dirtyCount })}
                  </span>
                ) : null}
              </div>

              {groupedRows.map((group) => (
                <div key={group.channel} className="space-y-3.5">
                  {channelFilter === 'all' && activeChannels.length > 1 ? (
                    <div className="flex items-center gap-2 px-1 pt-1.5">
                      <ChannelLogo channel={group.channel} size={16} />
                      <span className="text-xs font-semibold text-foreground/80">{CHANNEL_LABEL[group.channel]}</span>
                      <span className="tabular-nums text-[10px] font-medium text-muted-foreground">{group.rows.length}</span>
                    </div>
                  ) : null}
                  {group.rows.map((r) => {
                const draft = drafts[r.model] ?? {}
                const dirty = isDirty(draft, r.pricing)
                const advDirty = isAdvancedDirty(draft, r.pricing)
                const busy = savingModel === r.model || bulkSaving
                const source = sourceMeta(r.source)
                const advancedOpen = expandedAdvanced[r.model] ?? false
                const inputVal = normalizePrice(draft.input)
                const outputVal = normalizePrice(draft.output)
                const multiplier = getOutputMultiplier(inputVal, outputVal)
                const hasLongContextPricing =
                  normalizePrice(draft.long_context_threshold_tokens) > 0 ||
                  normalizePrice(draft.input_long) > 0 ||
                  normalizePrice(draft.cached_input_long) > 0 ||
                  normalizePrice(draft.output_long) > 0

                return (
                  <article
                    key={r.model}
                    id={`pricing-row-${r.model.toLowerCase()}`}
                    className={cn(
                      'group/card relative overflow-hidden rounded-xl border bg-card shadow-sm transition-all hover:border-border scroll-mt-24',
                      dirty ? 'border-amber-500/30' : 'border-border/80',
                      jumpedModel === r.model.toLowerCase() && 'ring-2 ring-primary ring-offset-2 ring-offset-background',
                    )}
                  >
                    <div className="p-4 sm:p-5">
                      {/* Header */}
                      <div className="flex flex-col gap-3.5 sm:flex-row sm:items-start sm:justify-between">
                        <div className="flex min-w-0 items-start gap-3.5">
                          <ModelLogo model={r.model} size={44} variant="ring" className="rounded-xl" />
                          <div className="min-w-0">
                            <div className="flex flex-wrap items-center gap-2">
                              <h4 className="truncate font-mono text-[15px] font-semibold tracking-tight text-foreground sm:text-base">
                                {r.model}
                              </h4>
                              {newModels.has(r.model.toLowerCase()) ? (
                                <span className="inline-flex items-center rounded-full bg-rose-500/15 px-1.5 py-0.5 text-[10px] font-bold text-rose-600 ring-1 ring-inset ring-rose-500/25 dark:text-rose-300">
                                  {t('settings.pricing.newBadge')}
                                </span>
                              ) : null}
                              {r.is_alias && r.canonical_model ? (
                                <span className="inline-flex items-center gap-1 rounded-full bg-violet-500/10 px-2 py-0.5 text-[10px] font-bold text-violet-700 ring-1 ring-inset ring-violet-500/20 dark:text-violet-300">
                                  {t('settings.pricing.aliasOf', {
                                    model: r.canonical_model,
                                  })}
                                </span>
                              ) : null}
                              <span
                                className={cn(
                                  'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-bold ring-1 ring-inset',
                                  source.className,
                                )}
                              >
                                <span className={cn('size-1.5 rounded-full', source.dot)} />
                                {t(source.labelKey)}
                              </span>
                              {dirty ? (
                                <span className="inline-flex items-center gap-1 rounded-full bg-amber-500/10 px-2 py-0.5 text-[10px] font-bold text-amber-700 ring-1 ring-inset ring-amber-500/20 dark:text-amber-300">
                                  <span className="size-1.5 rounded-full bg-amber-500" />
                                  {t('settings.pricing.unsaved')}
                                </span>
                              ) : (
                                <span className="inline-flex items-center gap-1 rounded-full bg-emerald-500/10 px-2 py-0.5 text-[10px] font-bold text-emerald-700 ring-1 ring-inset ring-emerald-500/15 dark:text-emerald-300">
                                  <Check className="size-2.5" />
                                  {t('settings.pricing.syncedState')}
                                </span>
                              )}
                            </div>
                            <div className="mt-1 flex flex-wrap items-center gap-2 text-[12px] text-muted-foreground">
                              <span>
                                <span className="font-semibold tabular-nums text-foreground">
                                  ${formatPriceDisplay(inputVal)}
                                </span>
                                <span className="mx-1 text-border">→</span>
                                <span className="font-semibold tabular-nums text-foreground">
                                  ${formatPriceDisplay(outputVal)}
                                </span>
                                <span className="ml-1 text-muted-foreground/80">
                                  {t('settings.pricing.perMillion')}
                                </span>
                              </span>
                              {multiplier ? (
                                <span className="rounded bg-muted/60 px-1.5 py-0.5 font-mono text-[10px] font-semibold text-muted-foreground">
                                  {t('settings.pricing.multiplier', { ratio: multiplier })}
                                </span>
                              ) : null}
                            </div>
                          </div>
                        </div>

                        <div className="flex shrink-0 items-center gap-1.5 sm:pt-0.5">
                          {r.source !== 'default' ? (
                            <Button
                              size="sm"
                              variant="ghost"
                              className="h-9 rounded-xl text-muted-foreground"
                              disabled={busy}
                              onClick={() => void reset(r.model)}
                            >
                              <RotateCcw className={cn('size-3.5', busy && 'animate-spin')} />
                              <span className="max-sm:hidden">{t('settings.pricing.resetBtn')}</span>
                            </Button>
                          ) : null}
                          <Button
                            size="sm"
                            className="h-9 min-w-[96px] rounded-xl"
                            disabled={busy || !dirty}
                            onClick={() => void save(r.model)}
                          >
                            {busy ? (
                              <Loader2 className="size-3.5 animate-spin" />
                            ) : (
                              <Save className="size-3.5" />
                            )}
                            {busy ? t('common.saving') : t('common.save')}
                          </Button>
                        </div>
                      </div>

                      {/* Primary rates */}
                      <div className="mt-4 grid grid-cols-1 gap-2.5 min-[480px]:grid-cols-3">
                        {PRIMARY_FIELDS.map((field) => (
                          <PriceField
                            key={field.key}
                            field={field}
                            value={normalizePrice(draft[field.key])}
                            savedValue={normalizePrice(r.pricing[field.key])}
                            changed={
                              normalizePrice(draft[field.key]) !== normalizePrice(r.pricing[field.key])
                            }
                            onChange={(next) => setField(r.model, field.key, next)}
                            onRevert={() => revertField(r.model, field.key)}
                          />
                        ))}
                      </div>

                      <BillingRulePreview pricing={draft} />

                      {/* Advanced rates */}
                      <div className="mt-3">
                        <button
                          type="button"
                          onClick={() =>
                            setExpandedAdvanced((prev) => ({
                              ...prev,
                              [r.model]: !advancedOpen,
                            }))
                          }
                          className="flex w-full items-center justify-between gap-2 rounded-xl px-1 py-1.5 text-left transition-colors hover:bg-muted/40"
                        >
                          <span className="flex items-center gap-2 text-[12px] font-semibold text-muted-foreground">
                            <ChevronDown
                              className={cn(
                                'size-3.5 transition-transform duration-200',
                                advancedOpen && 'rotate-180',
                              )}
                            />
                            {t('settings.pricing.advancedRates')}
                            {!advancedOpen && advDirty ? (
                              <span className="inline-flex items-center gap-1 rounded-full bg-amber-500/10 px-2 py-0.5 text-[10px] font-bold text-amber-600 dark:text-amber-400">
                                <span className="size-1.5 animate-pulse rounded-full bg-amber-500" />
                                {t('settings.pricing.hasAdvancedDirty')}
                              </span>
                            ) : null}
                          </span>
                          <span className="text-[11px] text-muted-foreground/70">
                            {t('settings.pricing.advancedRatesHint')}
                          </span>
                        </button>

                        <div
                          className={cn(
                            'grid transition-[grid-template-rows,opacity] duration-300 ease-out',
                            advancedOpen ? 'grid-rows-[1fr] opacity-100' : 'grid-rows-[0fr] opacity-0',
                          )}
                        >
                          <div className="min-h-0 overflow-hidden">
                            <div className="grid grid-cols-1 gap-2.5 pt-2 min-[480px]:grid-cols-2 xl:grid-cols-4">
                              {ADVANCED_FIELDS.map((field) => (
                                <PriceField
                                  key={field.key}
                                  field={field}
                                  dense
                                  value={normalizePrice(draft[field.key])}
                                  savedValue={normalizePrice(r.pricing[field.key])}
                                  changed={
                                    normalizePrice(draft[field.key]) !==
                                    normalizePrice(r.pricing[field.key])
                                  }
                                  onChange={(next) => setField(r.model, field.key, next)}
                                  onRevert={() => revertField(r.model, field.key)}
                                />
                              ))}
                              {hasLongContextPricing ? (
                                <ContextThresholdField
                                  value={Math.round(normalizePrice(draft.long_context_threshold_tokens))}
                                  savedValue={Math.round(normalizePrice(r.pricing.long_context_threshold_tokens))}
                                  changed={
                                    normalizePrice(draft.long_context_threshold_tokens) !==
                                    normalizePrice(r.pricing.long_context_threshold_tokens)
                                  }
                                  onChange={(next) =>
                                    setField(r.model, 'long_context_threshold_tokens', next)
                                  }
                                  onRevert={() =>
                                    revertField(r.model, 'long_context_threshold_tokens')
                                  }
                                />
                              ) : null}
                            </div>
                          </div>
                        </div>
                      </div>
                    </div>
                  </article>
                )
                  })}
                </div>
              ))}
            </div>
          )}
        </div>
      </StateShell>

      {/* 底部未保存批量操作悬浮条 */}
      {dirtyCount > 0 ? (
        <div className="fixed bottom-6 left-1/2 z-50 -translate-x-1/2 px-4 max-w-lg w-full animate-in fade-in slide-in-from-bottom-4 duration-200">
          <div className="flex items-center justify-between gap-3 rounded-2xl border border-amber-500/30 bg-card/95 p-3 shadow-xl backdrop-blur-xl ring-1 ring-amber-500/20">
            <div className="flex items-center gap-2.5 pl-1 min-w-0">
              <span className="relative flex size-2.5 shrink-0">
                <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-amber-400 opacity-75" />
                <span className="relative inline-flex size-2.5 rounded-full bg-amber-500" />
              </span>
              <span className="truncate text-xs font-semibold text-foreground">
                {t('settings.pricing.unsavedFloatingBar', { count: dirtyCount })}
              </span>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              <Button
                variant="ghost"
                size="sm"
                className="h-8 rounded-xl px-2.5 text-xs text-muted-foreground hover:text-foreground"
                onClick={discardAllChanges}
                disabled={bulkSaving}
              >
                {t('settings.pricing.discardAll')}
              </Button>
              <Button
                size="sm"
                className="h-8 rounded-xl px-3.5 text-xs font-semibold"
                onClick={() => void saveAllDirty()}
                disabled={bulkSaving}
              >
                {bulkSaving ? (
                  <Loader2 className="size-3 animate-spin" />
                ) : (
                  <Save className="size-3" />
                )}
                {bulkSaving ? t('settings.pricing.savingAll') : t('settings.pricing.saveAll')}
              </Button>
            </div>
          </div>
        </div>
      ) : null}
    </div>
  )
}
