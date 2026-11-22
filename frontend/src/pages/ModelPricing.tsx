import { isImage25Model } from '../lib/imageStudioModels'
import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertTriangle,
  ArrowUpRight,
  Check,
  ChevronDown,
  CloudDownload,
  Link2,
  Loader2,
  RotateCcw,
  Save,
  Search,
  X,
  ChevronsUpDown,
  Activity,
  CircleDollarSign,
  Clock3,
  Database,
  Layers,
  ListFilter,
  Pencil,
  SlidersHorizontal,
  Undo2,
} from 'lucide-react'

import { api } from '@/api'
import ChannelLogo from '../components/ChannelLogo'
import ModelLogo from '../components/ModelLogo'
import Modal from '../components/Modal'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import PricingSyncPanel from '../components/model-pricing/PricingSyncPanel'
import './model-pricing.css'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { SegmentedPillGroup } from '@/components/ui/segmented-pill-group'
import { DraftNumberInput } from '@/components/ui/draft-number-input'
import { mediaBillingUnit, supportsImageBilling, type MediaBillingUnit } from '../lib/imageBilling'
import { cn } from '@/lib/utils'
import { useToast } from '../hooks/useToast'
import { postAdminSSE } from '../hooks/useOperationProgress'
import { applyModelRefreshEvent, readModelRefreshSSE, type ModelRefreshProgress } from '../lib/modelRefreshStream'
import { getErrorMessage } from '../utils/error'
import type { ModelPricingOverride, OfficialPricingSyncConfig, UserBillingMode } from '@/types'
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
  /** Grok Imagine media rows are priced per image / per generated second, not per token. */
  media_unit?: MediaBillingUnit
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
  { key: 'output', labelKey: 'settings.pricing.output', shortKey: 'settings.pricing.shortOutput', tone: 'neutral' },
  { key: 'cache_write_5m', labelKey: 'settings.pricing.cacheWrite5m', shortKey: 'settings.pricing.shortCacheWrite5m', tone: 'neutral' },
  { key: 'cache_write_1h', labelKey: 'settings.pricing.cacheWrite1h', shortKey: 'settings.pricing.shortCacheWrite1h', tone: 'neutral' },
]

const IMAGE_FIELDS: FieldDef[] = [
  { key: 'image_input', labelKey: 'settings.pricing.imageInput', shortKey: 'settings.pricing.imageInput', tone: 'neutral' },
  { key: 'cached_image_input', labelKey: 'settings.pricing.cachedImageInput', shortKey: 'settings.pricing.cachedImageInput', tone: 'neutral' },
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

const ALL_FIELDS = [...PRIMARY_FIELDS, ...IMAGE_FIELDS, ...ADVANCED_FIELDS]

const TONE_DOT: Record<FieldDef['tone'], string> = {
  neutral: 'bg-muted-foreground/40',
  accent: 'bg-primary',
}

function normalizePrice(value: unknown): number {
  const n = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(n) ? n : 0
}

function isDirty(draft: ModelPricingOverride | undefined, saved: ModelPricingOverride | undefined): boolean {
  if ((draft?.user_billing_mode || 'token') !== (saved?.user_billing_mode || 'token') || normalizePrice(draft?.image_unit_price) !== normalizePrice(saved?.image_unit_price)) return true
  if (normalizePrice(draft?.media_unit_cost) !== normalizePrice(saved?.media_unit_cost)) return true
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
  return ratio.toFixed(1).replace(/\.0$/, '')
}

const PREFERRED_MODEL_ORDER = ['gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna'] as const

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

function PriceField({ field, value, savedValue, changed, dense, onChange, onRevert }: {
  field: FieldDef
  value: number
  savedValue?: number
  changed: boolean
  dense?: boolean
  onChange: (next: string) => void
  onRevert?: () => void
}) {
  const { t } = useTranslation()
  const inputId = useId()
  return (
    <div className={cn('pricing-field', changed && 'is-changed', dense && 'is-dense')}>
      <div className="pricing-field-heading">
        <label htmlFor={inputId}><span className={cn('pricing-field-dot', TONE_DOT[field.tone])} aria-hidden="true" />{t(field.labelKey)}</label>
        {changed && onRevert && (
          <button type="button" onClick={onRevert} aria-label={t('settings.pricing.revertPrice', { field: t(field.labelKey), value: savedValue ?? 0 })} title={t('settings.pricing.revertPrice', { field: t(field.labelKey), value: savedValue ?? 0 })}>
            <Undo2 size={12} aria-hidden="true" />
          </button>
        )}
      </div>
      <div className="pricing-field-value"><span aria-hidden="true">$</span>
        <DraftNumberInput id={inputId} value={value} integer={false} emptyValue={0} step="0.01" min={0} onValueChange={(next) => onChange(String(next))} />
      </div>
      <span className="pricing-field-unit">{t('settings.pricing.perMillion')}</span>
    </div>
  )
}

function ContextThresholdField({ value, savedValue, changed, onChange, onRevert }: {
  value: number
  savedValue: number
  changed: boolean
  onChange: (next: string) => void
  onRevert: () => void
}) {
  const { t } = useTranslation()
  const inputId = useId()
  return (
    <div className={cn('pricing-field pricing-threshold', changed && 'is-changed')}>
      <div className="pricing-field-heading">
        <label htmlFor={inputId}>{t('settings.pricing.contextThreshold')}</label>
        {changed && <button type="button" onClick={onRevert} aria-label={t('settings.pricing.revertThreshold', { value: savedValue })}><Undo2 size={12} aria-hidden="true" /></button>}
      </div>
      <div className="pricing-field-value"><DraftNumberInput id={inputId} value={value} min={0} emptyValue={0} step={1} onValueChange={(next) => onChange(String(next))} /></div>
      <span className="pricing-field-unit">tokens</span>
    </div>
  )
}

function formatPreviewRate(rate: PricingPreviewRate) {
  return `$${formatPriceDisplay(rate.input)} / $${formatPriceDisplay(rate.cached)} / $${formatPriceDisplay(rate.output)}`
}

function BillingRulePreview({ pricing }: { pricing: ModelPricingOverride }) {
  const { t } = useTranslation()
  const preview = buildModelPricingPreview(pricing)
  const rates = [
    { key: 'standard', label: 'settings.pricing.standardRate', rate: preview.standard },
    ...(preview.image ? [{ key: 'image', label: 'settings.pricing.imageRate', rate: preview.image }] : []),
    ...(preview.long ? [{ key: 'long', label: 'settings.pricing.longRate', rate: preview.long }] : []),
    ...(preview.priority ? [{ key: 'priority', label: 'settings.pricing.priorityRate', rate: preview.priority }] : []),
  ]
  return (
    <section className="pricing-rule-preview" aria-label={t('settings.pricing.billingPreview')}>
      <div className="pricing-preview-heading"><span><Activity size={15} aria-hidden="true" />{t(pricing.user_billing_mode === 'per_image' ? 'settings.pricing.imageBilling.upstreamRates' : 'settings.pricing.billingPreview')}</span><span>{t(preview.mode === 'tiered' ? 'settings.pricing.tiered' : 'settings.pricing.singleTier')}</span></div>
      <p className="pricing-preview-legend">{t('settings.pricing.previewLegend')}</p>
      <div className="pricing-preview-rates">
        {rates.map(({ key, label, rate }) => <div key={key} className={cn('pricing-preview-rate', key !== 'standard' && 'is-accent')}><span>{t(label)}</span><strong>{formatPreviewRate(rate)}</strong></div>)}
        {preview.flexMultiplier ? <div className="pricing-preview-rate"><span>{t('settings.pricing.flexRate')}</span><strong>×{preview.flexMultiplier}</strong><small>{t('settings.pricing.flexHint')}</small></div> : null}
      </div>
      {preview.long && <p className="pricing-preview-threshold"><Layers size={13} aria-hidden="true" />{t('settings.pricing.thresholdSummary', { value: preview.threshold.toLocaleString() })}</p>}
      <details className="pricing-expression"><summary>{t('settings.pricing.expressionPreview')}<ChevronDown size={12} aria-hidden="true" /></summary><code>{preview.expression}</code></details>
    </section>
  )
}

// ModelCatalogModal 是"模型目录"弹窗:按 provider 分组、可搜索、点击某模型直接定位到
// 价格行;可刷新账号真实可用模型;新出现的模型标"新",便于快速锁定。
// 刷新进度面板：每渠道一行，显示已探测的套餐分组数、当前抽样账号，以及刷出来的新模型。
function ModelRefreshProgressPanel({ progress, running }: { progress: ModelRefreshProgress; running: boolean }) {
  const { t } = useTranslation()
  const channels = CHANNEL_ORDER.filter((c) => progress[c]).map((c) => progress[c])
  if (channels.length === 0) {
    return (
      <div className="flex items-center gap-2 rounded-xl border border-sky-500/20 bg-sky-500/5 p-3 text-xs text-sky-700 dark:text-sky-300">
        <Loader2 className="size-3.5 animate-spin" />
        {t('settings.pricing.refreshStarting')}
      </div>
    )
  }
  return (
    <div className="space-y-1.5 rounded-xl border border-sky-500/20 bg-sky-500/5 p-3">
      {channels.map((ch) => {
        const label = CHANNEL_LABEL[ch.channel as Exclude<ChannelFilter, 'all'>] ?? ch.channel
        const finished = ch.done || (!running && ch.total > 0 && ch.current >= ch.total)
        const failed = Boolean(ch.error) || ch.failed > 0
        return (
          <div key={ch.channel} className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
            <span className="flex w-24 shrink-0 items-center gap-1.5 font-semibold text-foreground/80">
              {finished ? (
                failed ? <AlertTriangle className="size-3.5 text-amber-500" /> : <Check className="size-3.5 text-emerald-500" />
              ) : (
                <Loader2 className="size-3.5 animate-spin text-sky-500" />
              )}
              <ChannelLogo channel={ch.channel as Exclude<ChannelFilter, 'all'>} size={14} />
              {label}
            </span>
            <span className="text-muted-foreground">
              {ch.total > 0
                ? t('settings.pricing.refreshProbing', { current: ch.current, total: ch.total })
                : finished
                  ? t('settings.pricing.refreshNoAccounts')
                  : t('settings.pricing.refreshStarting')}
            </span>
            {ch.lastPlan ? (
              <span className="truncate font-mono text-[11px] text-muted-foreground">
                {ch.lastPlan}
                {ch.lastAccount ? ` · ${ch.lastAccount}` : ''}
                {ch.lastStatus === 'failed' ? ` · ${t('settings.pricing.catalogRefreshChannelFailed')}${ch.lastError ? `: ${ch.lastError}` : ''}` : ''}
              </span>
            ) : null}
            {ch.error ? <span className="text-[11px] text-amber-600 dark:text-amber-400">{ch.error}</span> : null}
            {ch.added.map((m) => (
              <span key={m} className="rounded-full bg-rose-500/15 px-1.5 py-0.5 font-mono text-[10px] font-bold text-rose-600 ring-1 ring-inset ring-rose-500/25 dark:text-rose-300">
                +{m}
              </span>
            ))}
          </div>
        )
      })}
    </div>
  )
}

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
  refreshProgress,
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
  refreshProgress: ModelRefreshProgress | null
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
        {refreshProgress ? <ModelRefreshProgressPanel progress={refreshProgress} running={refreshing} /> : null}
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            aria-label={t('settings.pricing.catalogSearch')}
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
                          {r.media_unit
                            ? normalizePrice(r.pricing.media_unit_cost) > 0
                              ? `$${formatPriceDisplay(normalizePrice(r.pricing.media_unit_cost))}${t(mediaCostUnitKey(r.media_unit))}`
                              : t('settings.pricing.mediaBilling.unpriced')
                            : `$${formatPriceDisplay(normalizePrice(r.pricing.input))}/$${formatPriceDisplay(normalizePrice(r.pricing.output))}`}
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

const USER_BILLING_MODES: UserBillingMode[] = ['token', 'per_image', 'per_video', 'per_second']

function parseUserBillingMode(value: string): UserBillingMode {
  return USER_BILLING_MODES.find(mode => mode === value) ?? 'token'
}

const MEDIA_MODE_LABEL: Record<UserBillingMode, string> = {
  token: 'settings.pricing.mediaBilling.followUpstream',
  per_image: 'settings.pricing.imageBilling.perImage',
  per_video: 'settings.pricing.mediaBilling.perVideo',
  per_second: 'settings.pricing.mediaBilling.perSecond',
}

const MEDIA_MODE_UNIT: Record<UserBillingMode, string> = {
  token: '',
  per_image: 'settings.pricing.perImageUnit',
  per_video: 'settings.pricing.perVideoUnit',
  per_second: 'settings.pricing.perSecondUnit',
}

function mediaCostUnitKey(unit: MediaBillingUnit): string {
  return unit === 'second' ? 'settings.pricing.perSecondUnit' : 'settings.pricing.perImageUnit'
}

// MediaPricingSummary 在折叠行上展示媒体模型的上游单位成本与用户计费方式;
// 单位成本为 0 时明确标"未定价",不让管理员误以为按 token 或免费结算。
function MediaPricingSummary({ unit, draft }: { unit: MediaBillingUnit; draft: ModelPricingOverride }) {
  const { t } = useTranslation()
  const cost = normalizePrice(draft.media_unit_cost)
  const mode = draft.user_billing_mode || 'token'
  return (
    <span className="pricing-image-summary">
      {cost > 0
        ? <strong>${formatPriceDisplay(cost)}<small>{t(mediaCostUnitKey(unit))}</small></strong>
        : <strong className="pricing-media-unpriced"><AlertTriangle size={13} aria-hidden="true" />{t('settings.pricing.mediaBilling.unpriced')}</strong>}
      <span>{mode === 'token' ? t(MEDIA_MODE_LABEL.token) : `${t(MEDIA_MODE_LABEL[mode])} · $${formatPriceDisplay(normalizePrice(draft.image_unit_price))}${t(MEDIA_MODE_UNIT[mode])}`}</span>
    </span>
  )
}

function MediaPricingEditor({ unit, draft, saved, busy, onFieldChange, onRevertField }: {
  unit: MediaBillingUnit
  draft: ModelPricingOverride
  saved: ModelPricingOverride
  busy: boolean
  onFieldChange: (key: keyof ModelPricingOverride, value: string) => void
  onRevertField: (key: keyof ModelPricingOverride) => void
}) {
  const { t } = useTranslation()
  const costId = useId()
  const mode = draft.user_billing_mode || 'token'
  const modes: UserBillingMode[] = unit === 'second' ? ['token', 'per_video', 'per_second'] : ['token', 'per_image']
  const cost = normalizePrice(draft.media_unit_cost)
  const costChanged = cost !== normalizePrice(saved.media_unit_cost)
  const unitName = t(unit === 'second' ? 'settings.pricing.mediaBilling.unitSecond' : 'settings.pricing.mediaBilling.unitImage')
  const priceLabel = mode === 'per_second' ? 'settings.pricing.mediaBilling.secondUnitPrice' : mode === 'per_video' ? 'settings.pricing.mediaBilling.videoUnitPrice' : 'settings.pricing.imageBilling.unitPrice'
  return (
    <div className="pricing-media-editor">
      <fieldset className="pricing-image-billing" disabled={busy} aria-label={t('settings.pricing.mediaBilling.title')}>
        <div><h5>{t('settings.pricing.mediaBilling.title')}</h5><SegmentedPillGroup label={t('settings.pricing.mediaBilling.title')} value={mode} options={modes.map(value => ({ value, label: t(MEDIA_MODE_LABEL[value]) }))} onChange={next => onFieldChange('user_billing_mode', next)} /></div>
        {mode !== 'token'
          ? <div className="pricing-image-price"><label><span>{t(priceLabel)}</span><DraftNumberInput aria-label={t(priceLabel)} value={draft.image_unit_price || 0} integer={false} min={0} step="0.001" onValueChange={value => onFieldChange('image_unit_price', String(value))} /></label><p>{t(unit === 'second' ? 'settings.pricing.mediaBilling.videoHint' : 'settings.pricing.mediaBilling.imageHint')}</p></div>
          : <p>{t('settings.pricing.mediaBilling.followUpstreamHint', { unit: unitName })}</p>}
      </fieldset>
      <fieldset className="pricing-image-billing" disabled={busy} aria-label={t('settings.pricing.mediaBilling.upstreamCost')}>
        <div><h5>{t('settings.pricing.mediaBilling.upstreamCost')}</h5>{cost <= 0 && <span className="pricing-media-unpriced"><AlertTriangle size={13} aria-hidden="true" />{t('settings.pricing.mediaBilling.unpriced')}</span>}</div>
        <div className="pricing-image-price">
          <label htmlFor={costId}>
            <span>{t('settings.pricing.mediaBilling.unitCost', { unit: unitName })}{costChanged && <button type="button" className="pricing-media-revert" onClick={() => onRevertField('media_unit_cost')} aria-label={t('settings.pricing.revertPrice', { field: t('settings.pricing.mediaBilling.upstreamCost'), value: normalizePrice(saved.media_unit_cost) })}><Undo2 size={12} aria-hidden="true" /></button>}</span>
            <DraftNumberInput id={costId} value={cost} integer={false} min={0} emptyValue={0} step="0.001" onValueChange={value => onFieldChange('media_unit_cost', String(value))} />
          </label>
          <p>{t(cost > 0 ? 'settings.pricing.mediaBilling.upstreamCostHint' : 'settings.pricing.mediaBilling.unpricedHint', { unit: unitName })}</p>
        </div>
      </fieldset>
    </div>
  )
}

function PricingModelRow({ row: r, draft, expanded, advancedOpen, busy, isNew, highlighted, onToggle, onToggleAdvanced, onFieldChange, onRevertField, onSave, onReset, onDiscard }: {
  row: Row
  draft: ModelPricingOverride
  expanded: boolean
  advancedOpen: boolean
  busy: boolean
  isNew: boolean
  highlighted: boolean
  onToggle: () => void
  onToggleAdvanced: () => void
  onFieldChange: (key: keyof ModelPricingOverride, value: string) => void
  onRevertField: (key: keyof ModelPricingOverride) => void
  onSave: () => void
  onReset: () => void
  onDiscard: () => void
}) {
  const { t } = useTranslation()
  const dirty = isDirty(draft, r.pricing)
  const advDirty = isAdvancedDirty(draft, r.pricing)
  const source = sourceMeta(r.source)
  const inputVal = normalizePrice(draft.input)
  const outputVal = normalizePrice(draft.output)
  const multiplier = getOutputMultiplier(inputVal, outputVal)
  const pricingModel = (r.canonical_model?.trim() || r.model.trim()).toLowerCase()
  const mediaUnit = r.media_unit || mediaBillingUnit(pricingModel)
  const imageModel = supportsImageBilling(pricingModel)
  const perImage = imageModel && draft.user_billing_mode === 'per_image'
  const primaryFields = imageModel ? [...PRIMARY_FIELDS.filter(field => !field.key.startsWith('cache_write')), ...(isImage25Model(pricingModel) ? IMAGE_FIELDS : [])] : PRIMARY_FIELDS
  const supportsLongContextPricing = !imageModel
  const advancedFields = imageModel ? [] : supportsLongContextPricing ? ADVANCED_FIELDS : ADVANCED_FIELDS.filter(field => !field.key.includes('_long'))
  const hasLongContextPricing = supportsLongContextPricing && (normalizePrice(draft.long_context_threshold_tokens) > 0 || normalizePrice(draft.input_long) > 0 || normalizePrice(draft.cached_input_long) > 0 || normalizePrice(draft.output_long) > 0)
  const advancedGroups = [
    { id: 'priority', label: 'settings.pricing.groupPriority', fields: advancedFields.filter(field => !field.key.includes('_long')) },
    { id: 'long', label: 'settings.pricing.groupLong', fields: advancedFields.filter(field => field.key.endsWith('_long')) },
    { id: 'long-priority', label: 'settings.pricing.groupLongPriority', fields: advancedFields.filter(field => field.key.includes('_long_priority')) },
  ]
  const editorId = `pricing-editor-${encodeURIComponent(r.model)}`
  const advancedId = `${editorId}-advanced`
  const priceInput = (field: FieldDef, dense = false) => <PriceField key={field.key} field={field} dense={dense} value={normalizePrice(draft[field.key])} savedValue={normalizePrice(r.pricing[field.key])} changed={normalizePrice(draft[field.key]) !== normalizePrice(r.pricing[field.key])} onChange={next => onFieldChange(field.key, next)} onRevert={() => onRevertField(field.key)} />

  return (
    <article id={`pricing-row-${r.model.toLowerCase()}`} data-model={r.model} aria-label={r.model} className={cn('pricing-model-row', expanded && 'is-expanded', dirty && 'is-dirty', highlighted && 'is-highlighted')}>
      <button type="button" className="pricing-row-summary" onClick={onToggle} aria-expanded={expanded} aria-controls={editorId} aria-describedby={`${editorId}-summary`} aria-label={t('settings.pricing.editModel', { model: r.model })}>
        <span className="pricing-model-identity">
          <ModelLogo model={r.model} size={34} variant="ring" />
          <span className="pricing-model-copy">
            <span className="pricing-model-name">{r.model}{isNew && <span className="pricing-new-badge">{t('settings.pricing.newBadge')}</span>}</span>
            <span className="pricing-model-meta">
              <span className={cn('pricing-source-badge', source.className)}><i className={source.dot} />{t(source.labelKey)}</span>
              {r.is_alias && r.canonical_model && <span className="pricing-alias" title={t('settings.pricing.aliasOf', { model: r.canonical_model })}><Link2 size={11} aria-hidden="true" />{r.canonical_model}</span>}
              {dirty && <span className="pricing-dirty-label"><span />{t('settings.pricing.unsaved')}</span>}
            </span>
          </span>
        </span>
        {mediaUnit ? (
          <MediaPricingSummary unit={mediaUnit} draft={draft} />
        ) : perImage ? (
          <span className="pricing-image-summary"><strong>${formatPriceDisplay(normalizePrice(draft.image_unit_price))}<small>{t('settings.pricing.perImageUnit')}</small></strong><span>{t('settings.pricing.imageBilling.perImage')}</span></span>
        ) : (
          <span className="pricing-row-prices">
            {([{ key: 'input', value: inputVal }, { key: 'cached', value: normalizePrice(draft.cached_input) }, { key: 'output', value: outputVal }] as const).map(({ key, value }) => <span key={key} className={cn('pricing-summary-price', key === 'output' && 'is-output')}><small>{t(`settings.pricing.${key}`)}</small><strong><span>$</span>{formatPriceDisplay(value)}</strong></span>)}
          </span>
        )}
        <span className="pricing-row-edit"><span>{t(expanded ? 'settings.pricing.closeEditor' : 'settings.pricing.editPrices')}</span><ChevronDown size={16} className={expanded ? 'is-open' : ''} aria-hidden="true" /></span>
        <span id={`${editorId}-summary`} className="sr-only">{mediaUnit ? `${t('settings.pricing.mediaBilling.upstreamCost')}: ${normalizePrice(draft.media_unit_cost) > 0 ? `$${formatPriceDisplay(normalizePrice(draft.media_unit_cost))}${t(mediaCostUnitKey(mediaUnit))}` : t('settings.pricing.mediaBilling.unpriced')}` : perImage ? `${t('settings.pricing.imageBilling.unitPrice')}: $${formatPriceDisplay(normalizePrice(draft.image_unit_price))}` : t('settings.pricing.summaryPrices', { input: formatPriceDisplay(inputVal), cached: formatPriceDisplay(normalizePrice(draft.cached_input)), output: formatPriceDisplay(outputVal) })}. {t(source.labelKey)}. {dirty ? t('settings.pricing.unsaved') : ''} {isNew ? t('settings.pricing.newBadge') : ''} {r.is_alias && r.canonical_model ? t('settings.pricing.aliasOf', { model: r.canonical_model }) : ''}</span>
      </button>
      <div id={editorId} hidden={!expanded}>
        {expanded && (
          <div className="pricing-editor">
            <div className="pricing-editor-heading"><span><SlidersHorizontal size={15} aria-hidden="true" />{t('settings.pricing.editPrices')}</span><span>{t('settings.pricing.editHint')}</span><span className="pricing-unit">{t('settings.pricing.unitHint')}</span></div>
            {mediaUnit && <MediaPricingEditor unit={mediaUnit} draft={draft} saved={r.pricing} busy={busy} onFieldChange={onFieldChange} onRevertField={onRevertField} />}
            {!mediaUnit && imageModel && (
              <fieldset className="pricing-image-billing" disabled={busy} aria-label={t('settings.pricing.imageBilling.title')}>
                <div><h5>{t('settings.pricing.imageBilling.title')}</h5><SegmentedPillGroup label={t('settings.pricing.imageBilling.title')} value={draft.user_billing_mode || 'token'} options={[{ value: 'token', label: t('settings.pricing.imageBilling.token') }, { value: 'per_image', label: t('settings.pricing.imageBilling.perImage') }]} onChange={mode => onFieldChange('user_billing_mode', mode)} /></div>
                {perImage ? <div className="pricing-image-price"><label><span>{t('settings.pricing.imageBilling.unitPrice')}</span><DraftNumberInput aria-label={t('settings.pricing.imageBilling.unitPrice')} value={draft.image_unit_price || 0} integer={false} min={0} step="0.001" onValueChange={value => onFieldChange('image_unit_price', String(value))} /></label><p>{t('settings.pricing.imageBilling.hint')}</p></div> : <p>{t('settings.pricing.imageBilling.tokenHint')}</p>}
              </fieldset>
            )}
            {!mediaUnit && <div className="pricing-editor-layout">
              <fieldset className="pricing-editor-fields" disabled={busy}>
                <legend>{t(perImage ? 'settings.pricing.imageBilling.upstreamRates' : 'settings.pricing.groupStandard')}</legend>
                <div className="pricing-base-fields">{primaryFields.map(field => priceInput(field))}</div>
                {!imageModel && (
                  <div className="pricing-advanced">
                    <button type="button" onClick={onToggleAdvanced} aria-expanded={advancedOpen} aria-controls={advancedId} className="pricing-advanced-toggle">
                      <span><ChevronDown size={14} className={advancedOpen ? 'is-open' : ''} aria-hidden="true" />{t('settings.pricing.advancedRates')}{advDirty && <><span className="pricing-dirty-dot" aria-hidden="true" /><span className="sr-only">{t('settings.pricing.hasAdvancedDirty')}</span></>}</span><span>{t(supportsLongContextPricing ? 'settings.pricing.advancedRatesHint' : 'settings.pricing.groupPriorityHint')}</span>
                    </button>
                    <div id={advancedId} hidden={!advancedOpen}>
                      {advancedOpen && advancedGroups.filter(group => group.fields.length > 0).map(group => (
                        <section key={group.id} className="pricing-advanced-group"><h5>{t(group.label)}</h5><div className="pricing-advanced-fields">{group.fields.map(field => priceInput(field, true))}</div>
                          {group.id === 'long' && hasLongContextPricing && <ContextThresholdField value={Math.round(normalizePrice(draft.long_context_threshold_tokens))} savedValue={Math.round(normalizePrice(r.pricing.long_context_threshold_tokens))} changed={normalizePrice(draft.long_context_threshold_tokens) !== normalizePrice(r.pricing.long_context_threshold_tokens)} onChange={next => onFieldChange('long_context_threshold_tokens', next)} onRevert={() => onRevertField('long_context_threshold_tokens')} />}
                        </section>
                      ))}
                    </div>
                  </div>
                )}
              </fieldset>
              <BillingRulePreview pricing={draft} />
            </div>}
            <div className="pricing-editor-footer">
              <div className="pricing-editor-notes">{r.is_alias && r.canonical_model ? <span><Link2 size={13} aria-hidden="true" />{t('settings.pricing.aliasOf', { model: r.canonical_model })}</span> : <span><Check size={13} aria-hidden="true" />{t(source.labelKey)}</span>}{multiplier && <span>{t('settings.pricing.outputRatio', { ratio: multiplier })}</span>}</div>
              <div className="pricing-editor-actions">
                {r.source !== 'default' && <Button size="sm" variant="ghost" disabled={busy} onClick={onReset}><RotateCcw className="size-3.5" />{t('settings.pricing.resetBtn')}</Button>}
                {dirty && <Button size="sm" variant="outline" disabled={busy} onClick={onDiscard}><Undo2 className="size-3.5" />{t('settings.pricing.discardModel')}</Button>}
                <Button size="sm" disabled={busy || !dirty} onClick={onSave}>{busy ? <Loader2 className="size-3.5 animate-spin" /> : <Save className="size-3.5" />}{t(busy ? 'common.saving' : 'common.save')}</Button>
              </div>
            </div>
          </div>
        )}
      </div>
    </article>
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
  const [litellmUrl, setLitellmUrl] = useState('')
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
  const [refreshProgress, setRefreshProgress] = useState<ModelRefreshProgress | null>(null)
  const refreshProgressHideTimer = useRef<number | null>(null)
  const [seenBump, setSeenBump] = useState(0)
  const [syncOpen, setSyncOpen] = useState(false)
  const [expandedAdvanced, setExpandedAdvanced] = useState<Record<string, boolean>>({})
  const [expandedModels, setExpandedModels] = useState<Record<string, boolean>>({})

  const load = useCallback(async () => {
    setLoading(true)
    setLoadError(null)
    try {
      const res = await api.listModelPricing()
      setRows(res.models)
      setDefaultUrl(res.default_sync_url)
      setModelsDevUrl(res.models_dev_url)
      setLitellmUrl(res.litellm_url)
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
    if (!allExpanded) setExpandedModels(Object.fromEntries(rows.map(row => [row.model, true])))
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
      showToast([t('settings.pricing.officialSyncDone', { applied: result.applied, skipped: result.skipped }), ...(result.notices ?? [])].join('；'))
      await load()
    } catch (error) {
      showToast(`${t('settings.pricing.syncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setOfficialSyncing(false)
    }
  }

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
    setExpandedModels(prev => ({ ...prev, [model]: true }))
    requestAnimationFrame(() => {
      const el = document.getElementById(`pricing-row-${model.toLowerCase()}`)
      if (el) el.scrollIntoView({ behavior: 'smooth', block: 'center' })
      window.setTimeout(() => setJumpedModel(''), 2200)
    })
  }, [])

  const refreshCatalogModels = useCallback(async () => {
    setRefreshingModels(true)
    if (refreshProgressHideTimer.current !== null) {
      window.clearTimeout(refreshProgressHideTimer.current)
      refreshProgressHideTimer.current = null
    }
    setRefreshProgress({})
    try {
      // 统一刷新所有渠道：按套餐分组抽样探测，SSE 逐组推送进度，刷出新模型立刻显示。
      const response = await postAdminSSE('/models/refresh-all?stream=1')
      const res = await readModelRefreshSSE(response, (event) => {
        setRefreshProgress((prev) => applyModelRefreshEvent(prev ?? {}, event))
      })
      if (!res) throw new Error(t('settings.pricing.refreshNoSummary'))
      const detail = res.channels
        .map((ch) => {
          const label = CHANNEL_LABEL[ch.channel as Exclude<ChannelFilter, 'all'>] ?? ch.channel
          const status = ch.error
            ? t('settings.pricing.catalogRefreshChannelFailed')
            : t('settings.pricing.catalogRefreshChannelGroups', { count: ch.groups ?? 0 })
          const added = ch.added.length ? ` +${ch.added.join(', ')}` : ''
          return `${label} ${status}${added}`
        })
        .join(' · ')
      const failed = res.channels.some((ch) => ch.error)
      showToast(t('settings.pricing.catalogRefreshed', { count: res.model_count, detail }), failed ? 'error' : undefined)
      await load()
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setRefreshingModels(false)
      refreshProgressHideTimer.current = window.setTimeout(() => {
        setRefreshProgress(null)
        refreshProgressHideTimer.current = null
      }, 4000)
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

  const clearFilters = () => {
    setQuery('')
    setSourceFilter('all')
    setChannelFilter('all')
  }
  const metricItems = [
    { id: 'all' as const, label: 'statTotal', sub: 'statTotalSub', value: counts.total, icon: Layers },
    { id: 'custom' as const, label: 'statCustom', sub: 'statCustomSub', value: counts.custom, icon: Pencil },
    { id: 'synced' as const, label: 'statSynced', sub: 'statSyncedSub', value: counts.synced, icon: CloudDownload },
    { id: 'default' as const, label: 'statDefault', sub: 'statDefaultSub', value: counts.defaults, icon: Database },
  ]

  return (
    <div className="model-pricing-page">
      <PageHeader
        title={t('settings.pricing.title')}
        description={t('settings.pricing.desc')}
        titleAdornment={<span className="pricing-currency-badge"><CircleDollarSign size={13} aria-hidden="true" />USD</span>}
        onRefresh={() => void load()}
        actions={<>
          <Button variant="outline" size="sm" onClick={() => { setCatalogQuery(''); setCatalogOpen(true) }}><Layers className="size-3.5" />{t('settings.pricing.catalogTitle')}{newModels.size > 0 && <span className="pricing-new-count">{newModels.size}</span>}</Button>
          <Button variant="outline" size="sm" onClick={() => setSyncOpen(true)} aria-haspopup="dialog"><CloudDownload className="size-3.5" />{t('settings.pricing.syncTitle')}</Button>
        </>}
      />
      <ModelCatalogModal open={catalogOpen} onClose={() => setCatalogOpen(false)} rows={rows} newModels={newModels} query={catalogQuery} onQueryChange={setCatalogQuery} onJump={jumpToModel} onRefresh={() => void refreshCatalogModels()} refreshing={refreshingModels} refreshProgress={refreshProgress} onAcknowledge={acknowledgeNewModels} />
      <PricingSyncPanel open={syncOpen} onClose={() => setSyncOpen(false)} syncUrl={syncUrl} onSyncUrlChange={setSyncUrl} urls={{ default: defaultUrl, litellm: litellmUrl, modelsDev: modelsDevUrl, openAI: officialOpenAIUrl, xAI: officialXAIUrl, claude: officialClaudeUrl }} config={officialConfig} onConfigChange={setOfficialConfig} syncing={syncing} officialSyncing={officialSyncing} officialSaving={officialSaving} onSync={() => void sync()} onSyncOfficial={() => void syncOfficial()} onSaveOfficial={() => void saveOfficialConfig()} />
      <StateShell variant="page" loading={loading && rows.length === 0} error={loadError && rows.length === 0 ? loadError : null} onRetry={() => void load()}>
        <div className="pricing-overview">
          <div className="pricing-metrics" role="group" aria-label={t('settings.pricing.sourceFilterLabel')}>
            {metricItems.map(({ id, label, sub, value, icon: Icon }) => (
              <button key={id} type="button" className="pricing-metric" data-source={id} aria-pressed={sourceFilter === id} onClick={() => setSourceFilter(id)}>
                <span className="pricing-metric-icon"><Icon size={18} strokeWidth={1.6} aria-hidden="true" /></span>
                <span className="pricing-metric-content"><span>{t(`settings.pricing.${label}`)}</span><strong>{value}<small>{t('settings.pricing.modelsUnit')}</small></strong><span className="pricing-metric-caption">{t(`settings.pricing.${sub}`)}</span></span>
                <span className="pricing-metric-bar" aria-hidden="true"><i style={{ width: `${counts.total ? value / counts.total * 100 : 0}%` }} /></span>
              </button>
            ))}
          </div>
          <div className="pricing-status-strip"><span><Clock3 size={13} aria-hidden="true" />{t(officialConfig.enabled ? 'settings.pricing.autoSyncEnabled' : 'settings.pricing.manualSync')}</span>{officialConfig.last_success_at && <span>{t('settings.pricing.lastOfficialSuccess')} · {new Date(officialConfig.last_success_at).toLocaleString()}</span>}<button type="button" onClick={() => setSyncOpen(true)}>{t('settings.pricing.manageSources')}<ArrowUpRight size={12} aria-hidden="true" /></button></div>
        </div>

        <section className="pricing-toolbar" aria-label={t('settings.pricing.filtersTitle')}>
          <div className="pricing-toolbar-top">
            <div className="pricing-list-title"><h3>{t('settings.pricing.priceList')}</h3><span role="status">{t('settings.pricing.listCount', { shown: filteredRows.length, total: counts.total })}</span></div>
            <div className="pricing-search"><Search size={16} aria-hidden="true" /><Input value={query} onChange={event => setQuery(event.target.value)} placeholder={t('settings.pricing.searchPlaceholder')} aria-label={t('settings.pricing.searchPlaceholder')} />{query && <button type="button" onClick={() => setQuery('')} aria-label={t('settings.pricing.clearSearch')}><X size={14} /></button>}</div>
            <Button variant="ghost" size="sm" className="pricing-expand-all" onClick={toggleAllAdvanced} aria-label={t(isAllAdvancedExpanded ? 'settings.pricing.collapseAllAdvanced' : 'settings.pricing.expandAllAdvanced')}><ChevronsUpDown className="size-3.5" /><span>{t(isAllAdvancedExpanded ? 'settings.pricing.collapseAllAdvanced' : 'settings.pricing.expandAllAdvanced')}</span></Button>
          </div>
          <div className="pricing-filter-layout">
            {activeChannels.length > 1 && <div className="pricing-filter-section"><span>{t('settings.pricing.channelFilterLabel')}</span><div className="pricing-filter-options" role="group" aria-label={t('settings.pricing.channelFilterLabel')}><button type="button" aria-pressed={channelFilter === 'all'} onClick={() => setChannelFilter('all')}>{t('settings.pricing.filterAll')}<span>{counts.total}</span></button>{activeChannels.map(channel => <button key={channel} type="button" aria-pressed={channelFilter === channel} onClick={() => setChannelFilter(channel)}><ChannelLogo channel={channel} size={14} />{CHANNEL_LABEL[channel]}<span>{channelCounts[channel]}</span></button>)}</div></div>}
            <div className="pricing-filter-section"><span>{t('settings.pricing.sourceFilterLabel')}</span><div className="pricing-filter-options" role="group" aria-label={t('settings.pricing.sourceFilterLabel')}>{sourceFilters.map(item => <button key={item.id} type="button" aria-pressed={sourceFilter === item.id} data-source={item.id} onClick={() => setSourceFilter(item.id)}>{item.id === 'unsaved' && dirtyCount > 0 && <i className="pricing-dirty-dot" />}{item.label}<span>{item.count}</span></button>)}</div></div>
          </div>
          {(query || sourceFilter !== 'all' || channelFilter !== 'all') && <div className="pricing-active-filters"><ListFilter size={12} aria-hidden="true" /><span>{t('settings.pricing.filteredView')}</span><button type="button" onClick={clearFilters}>{t('settings.pricing.clearFilters')}<X size={12} aria-hidden="true" /></button></div>}
        </section>

        {filteredRows.length === 0 ? (
          <div className="pricing-empty"><Search size={28} strokeWidth={1.5} aria-hidden="true" /><h3>{t('settings.pricing.emptyTitle')}</h3><p>{t(query || sourceFilter !== 'all' || channelFilter !== 'all' ? 'settings.pricing.emptyFiltered' : 'settings.pricing.emptyDesc')}</p>{(query || sourceFilter !== 'all' || channelFilter !== 'all') && <Button variant="outline" size="sm" onClick={clearFilters}>{t('settings.pricing.clearFilters')}</Button>}</div>
        ) : (
          <div className="pricing-model-groups">
            {groupedRows.map(group => (
              <section key={group.channel} className="pricing-channel-group" aria-label={CHANNEL_LABEL[group.channel]}>
                <div className="pricing-channel-heading"><ChannelLogo channel={group.channel} size={18} /><h3>{CHANNEL_LABEL[group.channel]}</h3><span>{group.rows.length}</span><small>{t('settings.pricing.unitHint')}</small></div>
                <div className="pricing-column-head" aria-hidden="true"><span>{t('settings.pricing.modelColumn')}</span><span className="pricing-column-rates"><span>{t('settings.pricing.input')}</span><span>{t('settings.pricing.cached')}</span><span>{t('settings.pricing.output')}</span></span><span /></div>
                {group.rows.map(r => <PricingModelRow key={r.model} row={r} draft={drafts[r.model] ?? {}} expanded={expandedModels[r.model] ?? false} advancedOpen={expandedAdvanced[r.model] ?? false} busy={savingModel === r.model || bulkSaving} isNew={newModels.has(r.model.toLowerCase())} highlighted={jumpedModel === r.model.toLowerCase()} onToggle={() => setExpandedModels(prev => ({ ...prev, [r.model]: !prev[r.model] }))} onToggleAdvanced={() => setExpandedAdvanced(prev => ({ ...prev, [r.model]: !prev[r.model] }))} onFieldChange={(key, value) => {
                  if (key === 'user_billing_mode') setDrafts(prev => ({ ...prev, [r.model]: { ...prev[r.model], user_billing_mode: parseUserBillingMode(value) } }))
                  else setField(r.model, key, value)
                }} onRevertField={key => revertField(r.model, key)} onSave={() => void save(r.model)} onReset={() => void reset(r.model)} onDiscard={() => setDrafts(prev => ({ ...prev, [r.model]: { ...r.pricing } }))} />)}
              </section>
            ))}
            <p className="pricing-list-note"><SlidersHorizontal size={13} aria-hidden="true" />{t('settings.pricing.listHint')}</p>
          </div>
        )}
      </StateShell>
      {dirtyCount > 0 && (
        <div className="pricing-bulk-bar"><div className="pricing-bulk-content"><div className="pricing-bulk-status" role="status"><span className="pricing-dirty-dot" /><span><strong>{t('settings.pricing.unsavedCount', { count: dirtyCount })}</strong><small>{t('settings.pricing.bulkScope')}</small></span></div><div className="pricing-bulk-actions"><Button variant="ghost" size="sm" onClick={discardAllChanges} disabled={bulkSaving}>{t('settings.pricing.discardAll')}</Button><Button size="sm" onClick={() => void saveAllDirty()} disabled={bulkSaving}>{bulkSaving ? <Loader2 className="size-3.5 animate-spin" /> : <Save className="size-3.5" />}{t(bulkSaving ? 'settings.pricing.savingAll' : 'settings.pricing.saveAll')}</Button></div></div></div>
      )}
    </div>
  )
}
