import type { Dispatch, ReactNode, SetStateAction, TextareaHTMLAttributes } from 'react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { NavLink, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { AlertTriangle, CheckCircle2, HelpCircle, Plus, RefreshCw, Save, Search, ShieldAlert, Trash2, Wand2, X } from 'lucide-react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import type { PromptFilterLog, PromptFilterMatch, PromptFilterRule, PromptFilterRulesResponse, PromptFilterVerdict, SystemSettings } from '../types'
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

const PROMPT_FILTER_VIEWS = ['overview', 'logs', 'rules'] as const
type PromptFilterView = typeof PROMPT_FILTER_VIEWS[number]

type PromptFilterForm = Pick<
  SystemSettings,
  | 'prompt_filter_enabled'
  | 'prompt_filter_mode'
  | 'prompt_filter_threshold'
  | 'prompt_filter_strict_threshold'
  | 'prompt_filter_log_matches'
  | 'prompt_filter_max_text_length'
  | 'prompt_filter_sensitive_words'
  | 'prompt_filter_custom_patterns'
  | 'prompt_filter_disabled_patterns'
>

type LogFilters = {
  action: string
  source: string
  endpoint: string
  model: string
  apiKeyId: string
  q: string
}

type CustomRuleDraft = {
  name: string
  pattern: string
  weight: string
  category: string
  strict: boolean
}

const defaultForm: PromptFilterForm = {
  prompt_filter_enabled: false,
  prompt_filter_mode: 'monitor',
  prompt_filter_threshold: 50,
  prompt_filter_strict_threshold: 90,
  prompt_filter_log_matches: true,
  prompt_filter_max_text_length: 81920,
  prompt_filter_sensitive_words: '',
  prompt_filter_custom_patterns: '[]',
  prompt_filter_disabled_patterns: '[]',
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

const normalizePromptFilterForm = (settings?: SystemSettings | null): PromptFilterForm => ({
  prompt_filter_enabled: Boolean(settings?.prompt_filter_enabled),
  prompt_filter_mode: settings?.prompt_filter_mode || 'monitor',
  prompt_filter_threshold: settings?.prompt_filter_threshold || 50,
  prompt_filter_strict_threshold: settings?.prompt_filter_strict_threshold || 90,
  prompt_filter_log_matches: settings?.prompt_filter_log_matches ?? true,
  prompt_filter_max_text_length: settings?.prompt_filter_max_text_length || 81920,
  prompt_filter_sensitive_words: settings?.prompt_filter_sensitive_words || '',
  prompt_filter_custom_patterns: settings?.prompt_filter_custom_patterns || '[]',
  prompt_filter_disabled_patterns: settings?.prompt_filter_disabled_patterns || '[]',
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
      const payload = partial ?? form
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
  ]
  const activeIndex = Math.max(0, tabs.findIndex((tab) => tab.view === activeView))
  return (
    <div className="mb-5 flex justify-center">
      <div className="relative grid w-full max-w-[560px] grid-cols-3 rounded-2xl border border-border bg-background/80 p-1 shadow-sm backdrop-blur-lg" role="tablist">
        <div
          className="pointer-events-none absolute left-1 top-1 h-[calc(100%-0.5rem)] rounded-xl border border-primary/15 bg-primary/8 transition-transform duration-300 ease-out"
          style={{ width: 'calc((100% - 0.5rem) / 3)', transform: `translateX(${activeIndex * 100}%)` }}
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
                <Input type="number" min={1} max={100} value={form.prompt_filter_threshold} onChange={(event) => setForm((current) => ({ ...current, prompt_filter_threshold: parseInt(event.target.value, 10) || 1 }))} />
              </Field>
              <Field label={t('promptFilter.strictThreshold')}>
                <Input type="number" min={1} max={100} value={form.prompt_filter_strict_threshold} onChange={(event) => setForm((current) => ({ ...current, prompt_filter_strict_threshold: parseInt(event.target.value, 10) || 1 }))} />
              </Field>
              <Field label={t('promptFilter.logMatches')}>
                <Select value={form.prompt_filter_log_matches ? 'true' : 'false'} onValueChange={(value) => setForm((current) => ({ ...current, prompt_filter_log_matches: value === 'true' }))} options={booleanOptions} />
              </Field>
              <Field label={t('promptFilter.maxTextLength')}>
                <Input type="number" min={1024} max={262144} value={form.prompt_filter_max_text_length} onChange={(event) => setForm((current) => ({ ...current, prompt_filter_max_text_length: parseInt(event.target.value, 10) || 81920 }))} />
              </Field>
            </div>
            <Field label={t('promptFilter.sensitiveWords')}>
              <Textarea rows={5} value={form.prompt_filter_sensitive_words} placeholder={t('promptFilter.sensitiveWordsPlaceholder')} onChange={(event) => setForm((current) => ({ ...current, prompt_filter_sensitive_words: event.target.value }))} />
            </Field>
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
  const [customDraft, setCustomDraft] = useState<CustomRuleDraft>(defaultCustomRuleDraft)
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

  const addCustomRule = async () => {
    const name = customDraft.name.trim()
    const pattern = customDraft.pattern.trim()
    const weight = parseInt(customDraft.weight, 10)
    if (!name || !pattern || !weight || weight <= 0) return
    const next = [
      ...customPatterns,
      {
        name,
        pattern,
        weight,
        category: customDraft.category.trim() || 'custom',
        strict: customDraft.strict,
        enabled: true,
      },
    ]
    await saveCustomPatterns(next)
    setCustomDraft(defaultCustomRuleDraft)
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
            <Button onClick={() => void addCustomRule()} disabled={savingRule !== '' || !customDraft.name.trim() || !customDraft.pattern.trim()}>
              <Plus className="size-4" />
              {t('promptFilter.addCustomRule')}
            </Button>
          </div>

          <div className="mb-4 grid gap-3 lg:grid-cols-[minmax(160px,0.8fr)_minmax(0,1.7fr)_120px_minmax(140px,0.8fr)_120px]">
            <Field label={t('promptFilter.ruleName')}>
              <Input value={customDraft.name} onChange={(event) => setCustomDraft((current) => ({ ...current, name: event.target.value }))} placeholder="custom_rule" />
            </Field>
            <Field label={t('promptFilter.rulePattern')}>
              <Input value={customDraft.pattern} onChange={(event) => setCustomDraft((current) => ({ ...current, pattern: event.target.value }))} placeholder="(?i)dangerous phrase" />
            </Field>
            <Field label={t('promptFilter.ruleWeight')}>
              <Input type="number" min={1} max={1000} value={customDraft.weight} onChange={(event) => setCustomDraft((current) => ({ ...current, weight: event.target.value }))} />
            </Field>
            <Field label={t('promptFilter.ruleCategory')}>
              <Input value={customDraft.category} onChange={(event) => setCustomDraft((current) => ({ ...current, category: event.target.value }))} />
            </Field>
            <Field label={t('promptFilter.ruleStrict')}>
              <Select value={customDraft.strict ? 'true' : 'false'} onValueChange={(value) => setCustomDraft((current) => ({ ...current, strict: value === 'true' }))} options={[{ label: t('common.enabled'), value: 'true' }, { label: t('common.disabled'), value: 'false' }]} />
            </Field>
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
                  <RuleRow key={`${rule.name}-${index}`} rule={{ ...rule, builtin: false, enabled: rule.enabled !== false }} onToggle={() => void toggleCustom(index)} onDelete={() => void deleteCustom(index)} busy={savingRule !== ''} />
                ))}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>

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

function RuleRow({ rule, selected, onSelect, onToggle, onDelete, busy }: { rule: PromptFilterRule; selected?: boolean; onSelect?: () => void; onToggle: () => void; onDelete?: () => void; busy?: boolean }) {
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
          <Button size="sm" variant="outline" onClick={onToggle} disabled={busy}>
            {enabled ? t('promptFilter.disableRule') : t('promptFilter.enableRule')}
          </Button>
          {onDelete ? (
            <Button size="sm" variant="ghost" onClick={onDelete} disabled={busy}>
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
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t('promptFilter.colTime')}</TableHead>
            <TableHead>{t('promptFilter.colAction')}</TableHead>
            <TableHead>{t('promptFilter.colEndpoint')}</TableHead>
            <TableHead>{t('promptFilter.colScore')}</TableHead>
            <TableHead>{t('promptFilter.colMatch')}</TableHead>
            <TableHead>{t('promptFilter.colApiKey')}</TableHead>
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

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block min-w-0 space-y-2">
      <span className="block text-sm font-semibold leading-none text-foreground">{label}</span>
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
      </div>
      {verdict.reason ? <p className="mt-3 text-sm text-muted-foreground">{verdict.reason}</p> : null}
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
        <pre className="mt-3 max-h-28 overflow-auto rounded-md bg-background p-2 text-xs leading-5 text-muted-foreground">{verdict.text_preview}</pre>
      ) : null}
    </div>
  )
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
  const matches = parseLogMatches(log.matched_patterns)
  return (
    <TableRow>
      <TableCell className="min-w-[150px]">
        <div className="font-medium text-foreground">{formatRelativeTime(log.created_at, { variant: 'compact' })}</div>
        {!compact ? <div className="text-xs text-muted-foreground">{formatBeijingTime(log.created_at)}</div> : null}
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <ActionBadge action={log.action} />
          {log.source === 'upstream_cyber_policy' ? <Badge variant="outline" className="text-[11px]">upstream</Badge> : null}
        </div>
      </TableCell>
      <TableCell>
        <div className="font-mono text-xs text-foreground">{log.endpoint || '-'}</div>
        <div className="font-mono text-xs text-muted-foreground">{log.model || '-'}</div>
      </TableCell>
      <TableCell>
        <span className="font-semibold">{log.score}</span>
        <span className="text-muted-foreground"> / {log.threshold}</span>
      </TableCell>
      <TableCell className="max-w-[220px]">
        {matches.length ? (
          <div className="flex flex-wrap gap-1">
            {matches.slice(0, 3).map((match, index) => <Badge key={`${match.name}-${index}`} variant="outline">{match.name}</Badge>)}
            {matches.length > 3 ? <Badge variant="secondary">+{matches.length - 3}</Badge> : null}
          </div>
        ) : <span className="text-muted-foreground">-</span>}
      </TableCell>
      <TableCell>
        <div className="max-w-[160px] truncate">{log.api_key_name || log.api_key_masked || '-'}</div>
        {!compact && log.client_ip ? <div className="text-xs text-muted-foreground">{log.client_ip}</div> : null}
      </TableCell>
      <TableCell className="max-w-[360px]">
        <div className="truncate text-muted-foreground" title={log.text_preview || log.error_code || ''}>{log.text_preview || log.error_code || '-'}</div>
      </TableCell>
    </TableRow>
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
