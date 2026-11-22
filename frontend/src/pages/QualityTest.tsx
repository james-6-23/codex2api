import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'
import { Check, Code2, Gauge, Copy, Download, Eye, FlaskConical, RefreshCw, Play, RotateCcw, Square, History, Clock3, ArrowUpRight, X, ExternalLink, Loader2, FileImage, BookmarkPlus, Pencil, Trash2, Plus, Lock, Wand2, CopyPlus, Filter, FilterX } from 'lucide-react'
import { api } from '../api'
import type { AccountRow, UpstreamChannel } from '../types'
import PageHeader from '../components/PageHeader'
import { SegmentedPillGroup } from '../components/ui/segmented-pill-group'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { Select } from '../components/ui/select'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '../components/ui/dialog'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { formatRelativeTime } from '../utils/time'
import { useToast } from '../hooks/useToast'
import { useVisibleChannels } from '../visibleChannels'
import { formatAccountName } from '../lib/connectionTestModels'
import { extractQualityTestHTML, PELICAN_PROMPT, isQualityTestActive, qualityTestPlanTone, qualityTestStatusKey, QUALITY_TEST_TIMEOUT_MINUTES, QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES, clampQualityTestFrameHeight, QUALITY_TEST_BUILTIN_PRESETS, EMPTY_QUALITY_TEST_FACETS, type QualityTestJob, type QualityTestJobsFilter, type QualityTestPrompt } from '../lib/qualityTest'
import { useQualityTestSVGExport, type QualityTestSVGExport } from '../hooks/useQualityTestSVGExport'
import { getErrorMessage } from '../utils/error'
import { useQualityTestJobs, useQualityTestDetail } from '../hooks/useQualityTestJobs'
import { useHighlightedHtml } from '../hooks/useHighlighter'
import { formatQualitySource } from '../lib/qualityTestFormat'
import { formatBeijingTime } from '../utils/time'
import Pagination from '../components/Pagination'
import './quality-test.css'

function PlanBadge({ plan }: { plan: string }) {
  return <span className={`quality-test-plan quality-test-plan--${qualityTestPlanTone(plan)}`}>{plan || '—'}</span>
}

// 记录里的预设来源:内置按 key 取当前语言名称,自定义用快照名称,手写提示词显示"自定义"。
function PresetLabel({ job }: { job: Pick<QualityTestJob, 'preset_kind' | 'preset_ref' | 'preset_name'> }) {
  const { t } = useTranslation()
  if (!job.preset_kind) return <span className="quality-test-preset-chip is-custom">{t('qualityTest.presets.handwritten')}</span>
  const name = job.preset_kind === 'builtin' ? t(`qualityTest.presets.builtins.${job.preset_ref}`, { defaultValue: job.preset_name || job.preset_ref }) : job.preset_name
  return <span className={`quality-test-preset-chip is-${job.preset_kind}`} title={name}>{job.preset_kind === 'builtin' ? <Lock className="size-3" /> : <BookmarkPlus className="size-3" />}{name}</span>
}

function AccountChoice({ account, compact = false }: { account: AccountRow; compact?: boolean }) {
  return <span className={`quality-test-account-option ${compact ? 'is-compact' : ''}`}>
    <span className="quality-test-account-identity"><span>{formatAccountName(account)}</span>{!compact ? <small>#{account.id}{account.name && account.email && account.name !== account.email ? ` · ${account.email}` : ''}</small> : null}</span>
    <PlanBadge plan={account.plan_type} />
  </span>
}

// 超过该体积的输出只显示纯文本,避免高亮阻塞主线程。
const HIGHLIGHT_LIMIT = 200 * 1024

function SourceView({ source }: { source: string }) {
  const { t } = useTranslation()
  // 模型输出通常不带缩进,先格式化再高亮;格式化失败或超限时原样显示。
  const [formatted, setFormatted] = useState(source)
  useEffect(() => {
    let cancelled = false
    setFormatted(source)
    void formatQualitySource(source).then((result) => { if (!cancelled) setFormatted(result) })
    return () => { cancelled = true }
  }, [source])
  const highlighted = useHighlightedHtml(formatted && formatted.length <= HIGHLIGHT_LIMIT ? formatted : '', 'html')
  if (highlighted) return <div className="quality-test-source shiki-wrapper" tabIndex={0} aria-label={t('qualityTest.source')} dangerouslySetInnerHTML={{ __html: highlighted }} />
  return <pre className="quality-test-source" tabIndex={0} aria-label={t('qualityTest.source')}><code>{formatted || t('qualityTest.sourceEmpty')}</code></pre>
}

// 一分钟内保留一位小数秒,更长的生成按"分 秒"显示,避免 441.0 s 这种难读的读数。
function useDurationFormatter() {
  const { t } = useTranslation()
  return (ms?: number) => {
    if (ms === undefined) return '—'
    if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
    const total = Math.round(ms / 1000)
    return t('qualityTest.durationMinutes', { m: Math.floor(total / 60), s: total % 60 })
  }
}

// 运行中的计时用 mm:ss 时钟读数,逐秒跳动时宽度稳定。
function formatClock(ms: number) {
  const total = Math.max(0, Math.floor(ms / 1000))
  return `${String(Math.floor(total / 60)).padStart(2, '0')}:${String(total % 60).padStart(2, '0')}`
}

function timeoutProgress(job: Pick<QualityTestJob, 'duration_ms' | 'timeout_ms'>) {
  return job.timeout_ms ? Math.min(100, (job.duration_ms / job.timeout_ms) * 100) : 0
}

// 只渲染输出尾部:运行中每秒刷新,全文可达 1 MiB,整段重绘会拖慢页面。
const LIVE_TAIL = 12000

// 生成过程实况:阶段、接收字数与速度、用时/上限进度条,以及自动跟随滚动的输出尾部。
function LiveOutput({ run }: { run: QualityTestJob }) {
  const { t } = useTranslation()
  const output = run.output ?? ''
  const writing = output.length > 0
  const truncated = output.length > LIVE_TAIL
  const tail = truncated ? output.slice(-LIVE_TAIL) : output
  const scrollRef = useRef<HTMLPreElement>(null)
  // 用户上翻查看时不抢滚动;回到底部附近后恢复跟随。
  const followRef = useRef(true)
  useLayoutEffect(() => {
    const element = scrollRef.current
    if (element && followRef.current) element.scrollTop = element.scrollHeight
  }, [tail])
  const writingMS = run.first_content_ms !== undefined ? run.duration_ms - run.first_content_ms : 0
  const rate = writingMS > 1000 ? Math.round(output.length / (writingMS / 1000)) : 0
  const progress = timeoutProgress(run)
  return <div className="quality-test-live">
    <div className="quality-test-live-head">
      <span className={`quality-test-live-phase ${writing ? 'is-writing' : ''}`}><span />{t(writing ? 'qualityTest.liveWriting' : 'qualityTest.liveThinking')}</span>
      {writing ? <span className="quality-test-live-stat">{t('qualityTest.liveReceived', { chars: output.length.toLocaleString() })}{rate ? ` · ${t('qualityTest.liveRate', { rate: rate.toLocaleString() })}` : ''}</span> : null}
      <span className="quality-test-live-clock" title={t('qualityTest.liveProgress')}>{formatClock(run.duration_ms)}{run.timeout_ms ? <small> / {formatClock(run.timeout_ms)}</small> : null}</span>
    </div>
    <div className={`quality-test-live-bar ${progress >= 80 ? 'is-late' : ''}`} role="progressbar" aria-label={t('qualityTest.liveProgress')} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(progress)}><span style={{ width: `${progress}%` }} /></div>
    {writing ? <pre ref={scrollRef} className="quality-test-live-output" tabIndex={0} aria-label={t('qualityTest.source')} onScroll={(event) => { const element = event.currentTarget; followRef.current = element.scrollHeight - element.scrollTop - element.clientHeight < 48 }}>
      {truncated ? <span className="quality-test-live-truncated">{t('qualityTest.liveTail', { chars: LIVE_TAIL.toLocaleString() })}</span> : null}{tail}<span className="quality-test-live-caret" aria-hidden="true" />
    </pre> : <div className="quality-test-live-waiting">
      <div className="quality-test-illustration"><Gauge strokeWidth={1.2} className="size-16" /><span>HTML</span></div>
      <h4>{t('qualityTest.generating')}</h4>
      <p>{t('qualityTest.liveWaitingHint')}</p>
      <div className="quality-test-progress"><span /></div>
    </div>}
  </div>
}

function downloadQualityFile(content: string, type: string, extension: 'html' | 'svg', id?: number) {
  const url = URL.createObjectURL(new Blob([content], { type }))
  const link = document.createElement('a')
  link.href = url
  link.download = `pelican-${id ?? Date.now()}.${extension}`
  link.style.display = 'none'
  document.body.append(link)
  link.click()
  link.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// 结果画布:工作台与检测记录弹窗共用,预览 iframe 走隔离页 + postMessage 注入。
function ResultCanvas({ run, view, html, svgExport, previewKey, narrow, loading }: { run: QualityTestJob | null; view: 'preview' | 'source'; html: string; svgExport: QualityTestSVGExport; previewKey: number; narrow: boolean; loading?: boolean }) {
  const { t } = useTranslation()
  const running = isQualityTestActive(run)
  const { frameRef, preview } = svgExport
  // 预览页回传内容高度,画布跟随内容尺寸;换任务或重放时清零回到默认高度。
  const [frameHeight, setFrameHeight] = useState<number>()
  const runID = run?.id
  useEffect(() => { setFrameHeight(undefined) }, [runID, previewKey])
  useEffect(() => {
    const onMessage = (event: MessageEvent) => {
      if (!frameRef.current || event.source !== frameRef.current.contentWindow || event.data?.type !== 'quality-test-preview-size') return
      const height = clampQualityTestFrameHeight(event.data.height)
      if (height !== undefined) setFrameHeight((current) => current === height ? current : height)
    }
    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
  }, [frameRef])
  const measured = view === 'preview' && Boolean(preview) && frameHeight !== undefined
  return <div className={`quality-test-canvas ${view === 'source' ? 'is-source' : ''} ${measured ? 'is-measured' : ''}`}>
    {loading && !run ? <div className="quality-test-empty"><Loader2 className="size-6 animate-spin text-muted-foreground" /></div> :
      running && run && view === 'source' ? <pre className="quality-test-source" tabIndex={0} aria-label={t('qualityTest.source')}><code>{run.output || t('qualityTest.sourceEmpty')}</code></pre> :
      running && run ? <LiveOutput run={run} /> :
      view === 'source' ? <SourceView source={html || run?.output || ''} /> : preview ?
      <iframe ref={frameRef} key={`${run?.id}-${previewKey}`} title={t('qualityTest.previewTitle')} src="/api/quality-test/preview" sandbox="allow-scripts" referrerPolicy="no-referrer" onLoad={(event) => event.currentTarget.contentWindow?.postMessage({ type: 'quality-test-preview', html: preview }, '*')} className={narrow ? 'is-narrow' : ''} style={frameHeight !== undefined ? { height: frameHeight } : undefined} /> :
      <div className="quality-test-empty">
        <div className="quality-test-illustration"><Gauge strokeWidth={1.2} className="size-20" /><span>HTML</span></div>
        <span className="quality-test-tag">{t('qualityTest.emptyTag')}</span>
        <h4>{t(run ? 'qualityTest.noHTML' : 'qualityTest.emptyTitle')}</h4>
        <p>{t(run ? 'qualityTest.noHTMLHint' : 'qualityTest.emptyHint')}</p>
      </div>}
  </div>
}

function PreviewActions({ run, html, view, narrow, svgExport, onToggleNarrow, onReplay, onCopy }: { run: QualityTestJob | null; html: string; view: 'preview' | 'source'; narrow: boolean; svgExport: QualityTestSVGExport; onToggleNarrow: () => void; onReplay: () => void; onCopy: () => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const title = t(svgExport.exporting ? 'qualityTest.svgExporting' : view !== 'preview' ? 'qualityTest.svgPreviewRequired' : !svgExport.ready ? 'qualityTest.svgPreparing' : !svgExport.hasSVG ? 'qualityTest.svgUnavailable' : 'qualityTest.downloadSVG')
  async function downloadSVG() {
    try {
      const svg = await svgExport.exportSVG()
      downloadQualityFile(svg, 'image/svg+xml;charset=utf-8', 'svg', run?.id)
    } catch (error) {
      const code = error instanceof Error ? error.message : 'failed'
      if (code === 'cancelled') return
      const key = ['timeout', 'noSVG', 'unsupported', 'tooLarge', 'notReady'].includes(code) ? code : 'failed'
      showToast(t(`qualityTest.svgErrors.${key}`), 'error')
    }
  }
  return <div>
    <Button size="sm" variant="ghost" disabled={!html || view !== 'preview'} aria-pressed={narrow} onClick={onToggleNarrow}>{narrow ? '100%' : '390px'}</Button>
    <Button size="icon-sm" variant="ghost" title={t('qualityTest.replay')} aria-label={t('qualityTest.replay')} disabled={!html} onClick={onReplay}><RotateCcw /></Button>
    <Button size="icon-sm" variant="ghost" title={t('qualityTest.copy')} aria-label={t('qualityTest.copy')} disabled={!run?.output} onClick={onCopy}><Copy /></Button>
    <Button size="icon-sm" variant="ghost" title={t('qualityTest.download')} aria-label={t('qualityTest.download')} disabled={!html} onClick={() => downloadQualityFile(html, 'text/html;charset=utf-8', 'html', run?.id)}><Download /></Button>
    <span title={title}><Button size="icon-sm" variant="ghost" aria-label={title} aria-busy={svgExport.exporting} disabled={!svgExport.hasSVG || svgExport.exporting} onClick={() => void downloadSVG()}>{svgExport.exporting ? <Loader2 className="animate-spin" /> : <FileImage />}</Button></span>
  </div>
}

// 运行结果提示:错误原文 + 中断时保留的部分输出 + 输出前自动重试次数。
function RunNotice({ run }: { run: QualityTestJob | null }) {
  const { t } = useTranslation()
  if (!run || (!run.error && !run.retries)) return null
  const partial = run.interrupted && run.output ? t('qualityTest.partialKept', { count: run.output.length }) : ''
  const retried = run.retries ? t('qualityTest.autoRetried', { count: run.retries }) : ''
  if (!run.error) return <div role="status" className="quality-test-error quality-test-result-error is-note">{retried}</div>
  return <div role="alert" className={`quality-test-error quality-test-result-error ${run.interrupted ? 'is-interrupted' : ''}`}>{run.error}{partial ? <small>{partial}</small> : null}{retried ? <small>{retried}</small> : null}</div>
}

function DetailMeta({ label, value, mono = true, hint }: { label: string; value: string; mono?: boolean; hint?: string }) {
  return <div className="quality-test-dialog-meta" title={hint}><dt>{label}</dt><dd className={mono ? 'is-mono' : ''}>{value}</dd></div>
}

// 检测记录的结果弹窗:全屏拟态窗口,左侧渲染动画,右侧列出账号/模型/耗时等详情。
function ResultDialog({ id, revision, onClose, onOpenStudio, onRerun, rerunDisabled }: { id: number | undefined; revision: number; onClose: () => void; onOpenStudio: (id: number) => void; onRerun: (job: QualityTestJob) => void; rerunDisabled: (job: QualityTestJob) => boolean }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const detail = useQualityTestDetail(id, revision)
  const run = detail.job
  const formatSeconds = useDurationFormatter()
  const [view, setView] = useState<'preview' | 'source'>('preview')
  const [previewKey, setPreviewKey] = useState(0)
  const [narrow, setNarrow] = useState(false)
  const html = useMemo(() => run && !isQualityTestActive(run) ? extractQualityTestHTML(run.output ?? '') : '', [run])
  const svgExport = useQualityTestSVGExport(html, `${id}-${previewKey}`, Boolean(id) && view === 'preview')
  useEffect(() => { setView('preview'); setNarrow(false); setPreviewKey(0) }, [id])
  const running = isQualityTestActive(run)

  async function copySource() {
    try {
      await navigator.clipboard.writeText(html || run?.output || '')
      showToast(t('qualityTest.copied'))
    } catch { showToast(t('qualityTest.copyFailed'), 'error') }
  }

  return <Dialog open={Boolean(id)} onOpenChange={(open) => { if (!open) onClose() }}>
    <DialogContent className="quality-test-dialog !flex !h-[calc(100dvh-1.5rem)] !w-[min(1480px,calc(100vw-1.5rem))] !max-w-none flex-col gap-0 overflow-hidden p-0 sm:p-0" showCloseButton={false}>
      <DialogHeader className="quality-test-dialog-header">
        <div className="quality-test-dialog-heading">
          <DialogTitle className="text-sm">{t('qualityTest.result')}{id ? <span className="quality-test-dialog-id">#{id}</span> : null}</DialogTitle>
          <DialogDescription className="truncate text-[11px]">{run ? `${run.account_name} · #${run.account_id} / ${run.model} / ${run.reasoning_effort || t('qualityTest.efforts.default')}` : t('qualityTest.loadingRecords')}</DialogDescription>
        </div>
        <SegmentedPillGroup value={view} onChange={setView} label={t('qualityTest.result')} options={[{ value: 'preview', label: t('qualityTest.preview'), icon: <Eye className="size-3.5" /> }, { value: 'source', label: t('qualityTest.source'), icon: <Code2 className="size-3.5" /> }]} />
      </DialogHeader>
      <Button type="button" size="icon-sm" variant="ghost" onClick={onClose} className="absolute right-3 top-3 z-10" aria-label={t('common.close')}><X className="size-4" /></Button>
      <div className="quality-test-dialog-layout">
        <div className="quality-test-dialog-stage">
          {detail.error ? <div role="alert" className="quality-test-error quality-test-result-error">{detail.error}</div> : null}
          <RunNotice run={run} />
          <ResultCanvas run={run} view={view} html={html} svgExport={svgExport} previewKey={previewKey} narrow={narrow} loading={detail.loading} />
        </div>
        <aside className="quality-test-dialog-info">
          <div className="quality-test-dialog-status">
            <span className={`quality-test-status ${run ? qualityTestStatusKey(run) : ''}`} role="status">{running ? <RefreshCw className="size-3.5 animate-spin" /> : run?.status === 'completed' ? <Check className="size-3.5" /> : <span className="quality-test-status-dot" />}{t(`qualityTest.status.${qualityTestStatusKey(run)}`)}</span>
            {run ? <PlanBadge plan={run.plan_type} /> : null}
          </div>
          <h3>{t('qualityTest.runDetails')}</h3>
          <dl className="quality-test-dialog-grid">
            <DetailMeta label={t('qualityTest.account')} value={run ? `${run.account_name} · #${run.account_id}` : '—'} mono={false} />
            <DetailMeta label={t('qualityTest.model')} value={run?.model ?? '—'} />
            <DetailMeta label={t('qualityTest.effort')} value={run ? run.reasoning_effort || t('qualityTest.efforts.default') : '—'} />
            <div className="quality-test-dialog-meta"><dt>{t('qualityTest.filters.preset')}</dt><dd>{run ? <PresetLabel job={run} /> : '—'}</dd></div>
            <DetailMeta label={t('qualityTest.testTime')} value={run ? formatBeijingTime(run.created_at) : '—'} />
            <DetailMeta label={t('qualityTest.duration')} value={run ? formatSeconds(run.duration_ms) : '—'} />
            <DetailMeta label={t('qualityTest.firstContent')} value={formatSeconds(run?.first_content_ms)} hint={t('qualityTest.firstContentHint')} />
            <DetailMeta label={t('qualityTest.timeout')} value={run?.timeout_ms ? t('qualityTest.timeoutOption', { count: Math.round(run.timeout_ms / 60000) }) : '—'} />
            <DetailMeta label={t('qualityTest.outputTokens')} value={run?.output_tokens?.toLocaleString() ?? '—'} />
            <DetailMeta label={t('qualityTest.reasoningTokens')} value={run?.reasoning_tokens?.toLocaleString() ?? '—'} />
          </dl>
          {run?.response_model ? <p className="quality-test-response-model !p-0 mt-3">{t('qualityTest.responseModel')}: {run.response_model}</p> : null}
          {run?.prompt ? <><div className="mt-6 flex items-center justify-between gap-2"><h3 className="!mb-0">{t('qualityTest.savedPrompt')}</h3></div><p className="quality-test-dialog-prompt">{run.prompt}</p>{run.completed_at ? <small className="quality-test-dialog-finished">{t('qualityTest.finishedAt')}: {formatBeijingTime(run.completed_at)}</small> : null}</> : null}
        </aside>
      </div>
      <div className="quality-test-dialog-footer">
        <div className="quality-test-preview-actions !border-0 !p-0">
          <span className="quality-test-hint">{t('qualityTest.isolatedPreview')}</span>
          <PreviewActions run={run} html={html} view={view} narrow={narrow} svgExport={svgExport} onToggleNarrow={() => setNarrow((value) => !value)} onReplay={() => setPreviewKey((key) => key + 1)} onCopy={() => void copySource()} />
        </div>
        <div className="flex items-center gap-2">
          {run && !running && run.prompt ? <Button size="sm" variant="outline" disabled={rerunDisabled(run)} onClick={() => onRerun(run)}><RotateCcw className="size-3.5" />{t('qualityTest.rerun')}</Button> : null}
          <Button size="sm" variant="outline" disabled={!id} onClick={() => id && onOpenStudio(id)}><ExternalLink className="size-3.5" />{t('qualityTest.openInStudio')}</Button>
        </div>
      </div>
    </DialogContent>
  </Dialog>
}

type PresetDraft = { id?: number; name: string; prompt: string }
const PROMPT_BYTE_LIMIT = 16000

// 预设编辑弹窗:新建/编辑共用,名称留空由服务端取提示词开头。
function PresetEditorDialog({ draft, saving, onChange, onClose, onSave }: { draft: PresetDraft | null; saving: boolean; onChange: (patch: Partial<PresetDraft>) => void; onClose: () => void; onSave: () => void }) {
  const { t } = useTranslation()
  const bytes = draft ? new TextEncoder().encode(draft.prompt).length : 0
  const invalid = !draft || !draft.prompt.trim() || bytes > PROMPT_BYTE_LIMIT
  return <Dialog open={Boolean(draft)} onOpenChange={(open) => { if (!open && !saving) onClose() }}>
    <DialogContent className="quality-test-preset-dialog sm:max-w-[640px]">
      <DialogHeader>
        <DialogTitle>{t(draft?.id ? 'qualityTest.presets.edit' : 'qualityTest.presets.new')}</DialogTitle>
        <DialogDescription>{t('qualityTest.presets.hint')}</DialogDescription>
      </DialogHeader>
      {draft ? <div className="quality-test-fields !my-0">
        <label htmlFor="quality-preset-name">{t('qualityTest.presets.name')}</label>
        <Input id="quality-preset-name" value={draft.name} maxLength={100} placeholder={t('qualityTest.presets.namePlaceholder')} onChange={(event) => onChange({ name: event.target.value })} />
        <div className="quality-test-prompt-label"><label htmlFor="quality-preset-prompt">{t('qualityTest.presets.prompt')}</label><span className={`quality-test-hint ${bytes > PROMPT_BYTE_LIMIT ? 'text-destructive' : ''}`}>{t('qualityTest.presets.bytes', { count: bytes })}</span></div>
        <textarea id="quality-preset-prompt" rows={8} value={draft.prompt} aria-invalid={bytes > PROMPT_BYTE_LIMIT} onChange={(event) => onChange({ prompt: event.target.value })} />
        <p className="quality-test-hint">{t('qualityTest.promptHint')}</p>
      </div> : null}
      <DialogFooter>
        <Button variant="outline" disabled={saving} onClick={onClose}>{t('common.cancel')}</Button>
        <Button disabled={saving || invalid} onClick={onSave}>{saving ? <RefreshCw className="size-3.5 animate-spin" /> : null}{t(saving ? 'common.saving' : 'common.save')}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
}

function PresetCard({ preset, onUse, onEdit, onDelete }: { preset: QualityTestPrompt; onUse: () => void; onEdit: () => void; onDelete: () => void }) {
  const { t } = useTranslation()
  return <article className="quality-test-preset-card">
    <button type="button" className="quality-test-preset-body" onClick={onEdit} title={t('qualityTest.presets.edit')}>
      <h3>{preset.name}</h3>
      <p>{preset.prompt}</p>
    </button>
    <div className="quality-test-preset-meta">
      <span>{t('qualityTest.presets.uses', { count: preset.usage_count })}</span>
      <span title={formatBeijingTime(preset.updated_at)}>{formatRelativeTime(preset.updated_at, { variant: 'compact' })}</span>
    </div>
    <div className="quality-test-preset-footer">
      <Button size="sm" variant="secondary" onClick={onUse}><Wand2 className="size-3.5" />{t('qualityTest.presets.use')}<ArrowUpRight className="size-3.5" /></Button>
      <Button size="icon-sm" variant="ghost" onClick={onEdit} aria-label={t('qualityTest.presets.edit')} title={t('qualityTest.presets.edit')}><Pencil className="size-3.5" /></Button>
      <Button size="icon-sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={onDelete} aria-label={t('qualityTest.presets.deleteTitle')} title={t('qualityTest.presets.deleteTitle')}><Trash2 className="size-3.5" /></Button>
    </div>
  </article>
}

type TestOptions = { models: string[]; reasoning_efforts: string[] }
const channelNames: Record<UpstreamChannel, string> = { codex: 'Codex / Responses', claude: 'Claude', grok: 'Grok', antigravity: 'Antigravity' }

export default function QualityTest() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const { channels } = useVisibleChannels()
  const [channel, setChannel] = useState<UpstreamChannel>('codex')
  const [search, setSearch] = useState('')
  const [accounts, setAccounts] = useState<AccountRow[]>([])
  const [account, setAccount] = useState<AccountRow | null>(null)
  const [accountLoading, setAccountLoading] = useState(true)
  const [total, setTotal] = useState(0)
  const [options, setOptions] = useState<TestOptions | null>(null)
  const [optionsLoading, setOptionsLoading] = useState(false)
  const [loadError, setLoadError] = useState('')
  const [reload, setReload] = useState(0)
  const [model, setModel] = useState('')
  const [effort, setEffort] = useState('high')
  const [timeoutMinutes, setTimeoutMinutes] = useState(QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES)
  const [prompt, setPrompt] = useState(PELICAN_PROMPT)
  const [presets, setPresets] = useState<QualityTestPrompt[]>([])
  const [presetsLoading, setPresetsLoading] = useState(true)
  const [presetRevision, setPresetRevision] = useState(0)
  const [presetDraft, setPresetDraft] = useState<PresetDraft | null>(null)
  const [presetSaving, setPresetSaving] = useState(false)
  const { confirm, confirmDialog } = useConfirmDialog()
  // 当前提示词与哪条预设一致由文本推导:改过就是"自定义",不额外维护选中态。
  const activePreset = presets.find((item) => item.prompt === prompt)
  const activeBuiltin = activePreset ? undefined : QUALITY_TEST_BUILTIN_PRESETS.find((item) => item.prompt === prompt)
  const presetValue = activePreset ? String(activePreset.id) : activeBuiltin ? `builtin:${activeBuiltin.key}` : 'custom'
  const [searchParams, setSearchParams] = useSearchParams()
  const requestedPane = searchParams.get('view')
  const pane: 'studio' | 'presets' | 'history' = requestedPane === 'history' ? 'history' : requestedPane === 'presets' ? 'presets' : 'studio'
  const [recordPage, setRecordPage] = useState(1)
  const [recordFilter, setRecordFilter] = useState<QualityTestJobsFilter>({})
  const filterActive = Boolean(recordFilter.plan || recordFilter.model || recordFilter.effort || recordFilter.account_id || recordFilter.preset)
  const updateFilter = (patch: QualityTestJobsFilter) => { setRecordFilter((current) => ({ ...current, ...patch })); setRecordPage(1) }
  const [modalID, setModalID] = useState<number>()
  const [revision, setRevision] = useState(0)
  const records = useQualityTestJobs(recordPage, revision, recordFilter)
  const facets = records.facets ?? EMPTY_QUALITY_TEST_FACETS
  const requestedID = Number(searchParams.get('job'))
  // 结果面板只跟随显式选中(URL job 参数)或正在运行的任务;不回退到历史第一条,
  // 否则每次打开页面都会先看到上一次生成的结果。
  const selectedID = requestedID > 0 ? requestedID : records.active_jobs[0]?.id
  const detail = useQualityTestDetail(pane === 'studio' ? selectedID : undefined, revision)
  const openRecord = (id: number) => setModalID(id)
  const run = detail.job
  const [submitting, setSubmitting] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const submittingRef = useRef(false)
  const [view, setView] = useState<'preview' | 'source'>('preview')
  const [previewKey, setPreviewKey] = useState(0)
  const [narrowPreview, setNarrowPreview] = useState(false)
  const mountedRef = useRef(true)
  const running = isQualityTestActive(run)
  const slotsFull = records.active_jobs.length >= records.concurrency_limit
  const accountBusy = records.active_jobs.some((job) => job.account_id === account?.id)
  const promptBytes = new TextEncoder().encode(prompt).length
  const html = useMemo(() => run && !isQualityTestActive(run) ? extractQualityTestHTML(run.output ?? '') : '', [run])
  const svgExport = useQualityTestSVGExport(html, `${selectedID}-${previewKey}`, pane === 'studio' && view === 'preview')
  const accountChoices = account && !accounts.some((row) => row.id === account.id) ? [account, ...accounts] : accounts
  const shownChannels = [...new Set<UpstreamChannel>(['codex', ...channels])]

  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    setPresetsLoading(true)
    api.getQualityTestPrompts(controller.signal)
      .then((result) => { if (!controller.signal.aborted) setPresets(result.prompts) })
      .catch((error) => { if (!controller.signal.aborted) showToast(getErrorMessage(error), 'error') })
      .finally(() => { if (!controller.signal.aborted) setPresetsLoading(false) })
    return () => controller.abort()
  }, [presetRevision, showToast])

  useEffect(() => {
    const controller = new AbortController()
    setAccountLoading(true)
    const timer = window.setTimeout(() => {
      api.getAccountsPage({ channel, page: 1, pageSize: 100, search }, controller.signal)
        .then((result) => {
          if (controller.signal.aborted) return
          setAccounts(result.accounts)
          setTotal(result.total)
          setLoadError('')
        })
        .catch((error) => { if (!controller.signal.aborted) setLoadError(getErrorMessage(error)) })
        .finally(() => { if (!controller.signal.aborted) setAccountLoading(false) })
    }, 250)
    return () => { window.clearTimeout(timer); controller.abort() }
  }, [channel, search, reload])

  const accountID = account?.id
  useEffect(() => {
    setOptions(null)
    setModel('')
    if (!accountID) { setOptionsLoading(false); return }
    const controller = new AbortController()
    setOptionsLoading(true)
    api.getQualityTestOptions(accountID, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return
        setOptions(result)
        setModel(result.models[0] ?? '')
        setEffort((current) => result.reasoning_efforts.includes(current) ? current : '')
        setLoadError('')
      })
      .catch((error) => { if (!controller.signal.aborted) setLoadError(getErrorMessage(error)) })
      .finally(() => { if (!controller.signal.aborted) setOptionsLoading(false) })
    return () => controller.abort()
  }, [accountID, reload])

  function goToPane(nextPane: 'studio' | 'presets' | 'history') {
    setSearchParams((previous) => { const next = new URLSearchParams(previous); next.set('view', nextPane); return next })
  }

  function applyPreset(value: string) {
    if (value.startsWith('builtin:')) {
      const builtin = QUALITY_TEST_BUILTIN_PRESETS.find((item) => `builtin:${item.key}` === value)
      if (builtin) setPrompt(builtin.prompt)
      return
    }
    const preset = presets.find((item) => String(item.id) === value)
    if (preset) setPrompt(preset.prompt)
  }

  async function savePreset() {
    if (!presetDraft || presetSaving) return
    setPresetSaving(true)
    try {
      const body = { name: presetDraft.name.trim(), prompt: presetDraft.prompt }
      const result = presetDraft.id ? await api.updateQualityTestPrompt(presetDraft.id, body) : await api.createQualityTestPrompt(body)
      if (!mountedRef.current) return
      // 编辑中的预设若正被工作台使用,同步更新工作台文本,保持"已选中"关系。
      if (presetDraft.id && activePreset?.id === presetDraft.id) setPrompt(result.prompt.prompt)
      setPresetDraft(null)
      setPresetRevision((value) => value + 1)
      showToast(t('qualityTest.presets.saved'))
    } catch (error) {
      if (mountedRef.current) showToast(getErrorMessage(error), 'error')
    } finally {
      if (mountedRef.current) setPresetSaving(false)
    }
  }

  async function deletePreset(preset: QualityTestPrompt) {
    const ok = await confirm({ title: t('qualityTest.presets.deleteTitle'), description: <><strong>{preset.name}</strong><br />{t('qualityTest.presets.deleteHint')}</>, confirmText: t('common.delete'), tone: 'destructive' })
    if (!ok) return
    try {
      await api.deleteQualityTestPrompt(preset.id)
      if (!mountedRef.current) return
      setPresetRevision((value) => value + 1)
      showToast(t('qualityTest.presets.deleted'))
    } catch (error) {
      if (mountedRef.current) showToast(getErrorMessage(error), 'error')
    }
  }

  function selectJob(id: number, nextPane = pane) {
    setSearchParams({ view: nextPane, job: String(id) })
    setView('preview')
    setPreviewKey((key) => key + 1)
  }

  function startTest() {
    if (!account || !model || !prompt.trim() || promptBytes > 16000 || slotsFull || accountBusy) return
    void submitTest(account.id, { model, reasoning_effort: effort, prompt, prompt_id: activePreset?.id, preset_key: activeBuiltin?.key, preset_name: activeBuiltin ? t(`qualityTest.presets.builtins.${activeBuiltin.key}`) : undefined, timeout_minutes: timeoutMinutes })
  }

  const rerunDisabled = (job: QualityTestJob) => submitting || slotsFull || records.active_jobs.some((item) => item.account_id === job.account_id)

  // 重新检测沿用原记录的账号/模型/强度/提示词/预设;超时取原记录与当前表单中较大者。
  function rerunJob(job: QualityTestJob) {
    if (!job.prompt || rerunDisabled(job)) return
    const previous = job.timeout_ms ? Math.round(job.timeout_ms / 60000) : 0
    const timeout = QUALITY_TEST_TIMEOUT_MINUTES.find((value) => value >= Math.max(previous, timeoutMinutes)) ?? QUALITY_TEST_TIMEOUT_MINUTES[QUALITY_TEST_TIMEOUT_MINUTES.length - 1]
    setModalID(undefined)
    void submitTest(job.account_id, { model: job.model, reasoning_effort: job.reasoning_effort, prompt: job.prompt, prompt_id: job.preset_kind === 'custom' ? Number(job.preset_ref) || undefined : undefined, preset_key: job.preset_kind === 'builtin' ? job.preset_ref : undefined, preset_name: job.preset_kind === 'builtin' ? job.preset_name : undefined, timeout_minutes: timeout })
  }

  async function submitTest(accountId: number, body: Parameters<typeof api.createQualityTest>[1]) {
    if (submittingRef.current) return
    submittingRef.current = true
    setSubmitting(true)
    try {
      const result = await api.createQualityTest(accountId, body)
      if (!mountedRef.current) return
      selectJob(result.job.id, 'studio')
      setRecordPage(1)
      setRevision((value) => value + 1)
      showToast(t('qualityTest.backgroundStarted'))
    } catch (error) {
      if (mountedRef.current) { showToast(getErrorMessage(error), 'error'); setRevision((value) => value + 1) }
    } finally {
      submittingRef.current = false
      if (mountedRef.current) setSubmitting(false)
    }
  }

  async function stopTest() {
    if (!run || !running || cancelling) return
    setCancelling(true)
    try {
      await api.cancelQualityTest(run.id)
      if (mountedRef.current) setRevision((value) => value + 1)
    } catch (error) { if (mountedRef.current) showToast(getErrorMessage(error), 'error') }
    finally { if (mountedRef.current) setCancelling(false) }
  }

  async function copySource() {
    try {
      await navigator.clipboard.writeText(html || run?.output || '')
      showToast(t('qualityTest.copied'))
    } catch { showToast(t('qualityTest.copyFailed'), 'error') }
  }

  const formatTime = useDurationFormatter()
  const startKeys = /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘ ↵' : 'Ctrl ↵'
  return (
    <div className="quality-test-page">
      <PageHeader title={t('qualityTest.title')} description={t('qualityTest.subtitle')}
        titleAdornment={<span className="quality-test-tag"><FlaskConical className="size-3.5" /> HTML / SVG</span>}
        actionMeta={<span className="quality-test-capacity"><span className={records.active_jobs.length ? 'is-active' : ''} />{t('qualityTest.capacity', { count: records.active_jobs.length, limit: records.concurrency_limit })}</span>}
        actions={<SegmentedPillGroup value={pane} label={t('qualityTest.title')} onChange={(value) => setSearchParams((previous) => { const next = new URLSearchParams(previous); next.set('view', value); return next })}
          options={[{ value: 'studio', label: t('qualityTest.studioTab'), icon: <FlaskConical className="size-4" /> }, { value: 'presets', label: t('qualityTest.presetsTab'), icon: <BookmarkPlus className="size-4" /> }, { value: 'history', label: t('qualityTest.recordsTab'), icon: <History className="size-4" /> }]} />} />
      {records.error || detail.error ? <div role="alert" className="quality-test-error">{records.error || detail.error}<Button size="sm" variant="outline" onClick={() => setRevision((value) => value + 1)}>{t('common.retry')}</Button></div> : null}
      {records.active_jobs.length > 0 ? <section className="quality-test-active" aria-label={t('qualityTest.activeTasks')}>
        <div className="quality-test-active-heading"><span><RefreshCw className="size-3.5 animate-spin" />{t('qualityTest.activeTasks')}</span><p>{t('qualityTest.backgroundHint')}</p></div>
        <div className="quality-test-active-list">{records.active_jobs.map((job) => <Button variant="outline" key={job.id} className="quality-test-active-card" aria-pressed={selectedID === job.id} onClick={() => selectJob(job.id, 'studio')}>
          <span className="quality-test-active-account"><span>{job.account_name}</span><PlanBadge plan={job.plan_type} /></span>
          <span className="quality-test-active-model">{job.model} · {job.reasoning_effort || t('qualityTest.efforts.default')}</span>
          <span className="quality-test-active-meta">#{job.id} · {t(`qualityTest.status.${job.status}`)}<span className="tabular-nums">{formatClock(job.duration_ms)}{job.timeout_ms ? ` / ${formatClock(job.timeout_ms)}` : ''}<ArrowUpRight className="size-3.5" /></span></span>
          <span className={`quality-test-active-progress ${timeoutProgress(job) >= 80 ? 'is-late' : ''}`} aria-hidden="true"><span style={{ width: `${timeoutProgress(job)}%` }} /></span>
        </Button>)}</div>
      </section> : null}
      {pane === 'presets' ? <section className="quality-test-records quality-test-presets" aria-labelledby="quality-presets-title">
        <div className="quality-test-records-heading">
          <div><h3 id="quality-presets-title">{t('qualityTest.presets.title')}</h3><p>{t('qualityTest.presets.hint')}</p></div>
          <div className="quality-test-records-tools">{presets.length > 0 ? <span className="quality-test-records-count">{presets.length}</span> : null}<Button size="sm" onClick={() => setPresetDraft({ name: '', prompt: '' })}><Plus className="size-3.5" />{t('qualityTest.presets.new')}</Button></div>
        </div>
        <div className="quality-test-preset-grid">
          {QUALITY_TEST_BUILTIN_PRESETS.map((builtin) => <article key={builtin.key} className="quality-test-preset-card is-builtin">
            <div className="quality-test-preset-body">
              <h3><Lock className="size-3.5" />{t(`qualityTest.presets.builtins.${builtin.key}`)}<span className="quality-test-tag">{t(builtin.key === 'pelican' ? 'qualityTest.presets.builtinDefault' : 'qualityTest.presets.builtin')}</span></h3>
              <p>{builtin.prompt}</p>
            </div>
            <div className="quality-test-preset-meta"><span>{t(builtin.key === 'pelican' ? 'qualityTest.presets.builtinHint' : 'qualityTest.presets.builtinOtherHint')}</span></div>
            <div className="quality-test-preset-footer">
              <Button size="sm" variant="secondary" onClick={() => { setPrompt(builtin.prompt); goToPane('studio') }}><Wand2 className="size-3.5" />{t('qualityTest.presets.use')}<ArrowUpRight className="size-3.5" /></Button>
              <Button size="icon-sm" variant="ghost" onClick={() => setPresetDraft({ name: t(`qualityTest.presets.builtins.${builtin.key}`), prompt: builtin.prompt })} aria-label={t('qualityTest.presets.clone')} title={t('qualityTest.presets.clone')}><CopyPlus className="size-3.5" /></Button>
            </div>
          </article>)}
          {presets.map((preset) => <PresetCard key={preset.id} preset={preset} onUse={() => { setPrompt(preset.prompt); goToPane('studio') }} onEdit={() => setPresetDraft({ id: preset.id, name: preset.name, prompt: preset.prompt })} onDelete={() => void deletePreset(preset)} />)}
          {!presetsLoading && presets.length === 0 ? <button type="button" className="quality-test-preset-add" onClick={() => setPresetDraft({ name: '', prompt: '' })}><span><Plus className="size-5" /></span><strong>{t('qualityTest.presets.empty')}</strong><small>{t('qualityTest.presets.emptyHint')}</small></button> : null}
        </div>
      </section> : pane === 'history' ? <section className="quality-test-records" aria-labelledby="quality-records-title">
        <div className="quality-test-records-heading">
          <div><h3 id="quality-records-title">{t('qualityTest.recordsTab')}</h3><p>{t('qualityTest.recordsHint')}</p></div>
          <div className="quality-test-records-tools">{records.total > 0 ? <span className="quality-test-records-count">{records.total}</span> : null}<Button variant="outline" size="sm" onClick={() => setRevision((value) => value + 1)}><RefreshCw className={records.loading ? 'animate-spin' : ''} />{t('common.refresh')}</Button></div>
        </div>
        <div className="quality-test-filters" role="group" aria-label={t('qualityTest.filters.title')}>
          <span className="quality-test-filters-label"><Filter className="size-3.5" />{t('qualityTest.filters.title')}</span>
          <Select aria-label={t('qualityTest.filters.plan')} compact value={recordFilter.plan ?? ''} onValueChange={(value) => updateFilter({ plan: value })} options={[{ value: '', label: t('qualityTest.filters.allPlans') }, ...facets.plans.map((plan) => ({ value: plan, label: plan || '—', content: <PlanBadge plan={plan} /> }))]} />
          <Select aria-label={t('qualityTest.filters.account')} compact value={recordFilter.account_id ? String(recordFilter.account_id) : ''} onValueChange={(value) => updateFilter({ account_id: Number(value) || undefined })} options={[{ value: '', label: t('qualityTest.filters.allAccounts') }, ...facets.accounts.map((item) => ({ value: String(item.id), label: `${item.name} · #${item.id}` }))]} />
          <Select aria-label={t('qualityTest.filters.model')} compact value={recordFilter.model ?? ''} onValueChange={(value) => updateFilter({ model: value })} options={[{ value: '', label: t('qualityTest.filters.allModels') }, ...facets.models.map((model) => ({ value: model, label: model }))]} />
          <Select aria-label={t('qualityTest.filters.effort')} compact value={recordFilter.effort ?? ''} onValueChange={(value) => updateFilter({ effort: value })} options={[{ value: '', label: t('qualityTest.filters.allEfforts') }, ...facets.efforts.map((effort) => ({ value: effort || 'default', label: effort ? t(`qualityTest.efforts.${effort}`, { defaultValue: effort }) : t('qualityTest.efforts.default') }))]} />
          <Select aria-label={t('qualityTest.filters.preset')} compact value={recordFilter.preset ?? ''} onValueChange={(value) => updateFilter({ preset: value })} options={[{ value: '', label: t('qualityTest.filters.allPresets') }, { value: 'none', label: t('qualityTest.presets.handwritten') }, ...facets.presets.map((item) => ({ value: `${item.kind}:${item.ref}`, label: item.kind === 'builtin' ? t(`qualityTest.presets.builtins.${item.ref}`, { defaultValue: item.name || item.ref }) : item.name, content: <PresetLabel job={{ preset_kind: item.kind, preset_ref: item.ref, preset_name: item.name }} /> }))]} />
          {filterActive ? <Button size="sm" variant="ghost" onClick={() => { setRecordFilter({}); setRecordPage(1) }}><FilterX className="size-3.5" />{t('qualityTest.filters.clear')}</Button> : null}
        </div>
        {records.jobs.length === 0 ? <div className="quality-test-records-empty"><History className="size-9" /><h4>{t(records.loading ? 'qualityTest.loadingRecords' : filterActive ? 'qualityTest.filters.noMatch' : 'qualityTest.emptyRecords')}</h4><p>{t(filterActive && !records.loading ? 'qualityTest.filters.noMatchHint' : 'qualityTest.emptyRecordsHint')}</p></div> : <div className="quality-test-records-scroll"><table>
          <thead><tr><th>{t('qualityTest.recordID')}</th><th>{t('qualityTest.account')}</th><th>{t('qualityTest.model')}</th><th>{t('qualityTest.effort')}</th><th>{t('qualityTest.filters.preset')}</th><th>{t('qualityTest.testTime')}</th><th>{t('qualityTest.recordStatus')}</th><th className="is-numeric">{t('qualityTest.duration')}</th><th className="is-numeric" title={t('qualityTest.firstContentHint')}>{t('qualityTest.firstContent')}</th><th className="is-numeric">{t('qualityTest.outputTokens')}</th><th><span className="sr-only">{t('qualityTest.viewResult')}</span></th></tr></thead>
          <tbody>{records.jobs.map((job) => <tr key={job.id} className={modalID === job.id ? 'is-selected' : ''} onClick={() => openRecord(job.id)}>
            <td className="quality-test-record-id">#{job.id}</td>
            <td><div className="quality-test-record-account"><span title={job.account_name}>{job.account_name}</span><small>#{job.account_id}<PlanBadge plan={job.plan_type} /></small></div></td>
            <td><span className="quality-test-record-model">{job.model}</span></td><td><span className="quality-test-effort-chip">{job.reasoning_effort || t('qualityTest.efforts.default')}</span></td>
            <td><PresetLabel job={job} /></td>
            <td className="quality-test-record-time">{formatBeijingTime(job.created_at)}</td>
            <td><span className={`quality-test-status quality-test-status-pill ${qualityTestStatusKey(job)}`}>{isQualityTestActive(job) ? <RefreshCw className="size-3 animate-spin" /> : <span className="quality-test-status-dot" />}{t(`qualityTest.status.${qualityTestStatusKey(job)}`)}</span></td>
            <td className="quality-test-record-time is-numeric">{formatTime(job.duration_ms)}</td>
            <td className="quality-test-record-time is-numeric">{formatTime(job.first_content_ms)}</td>
            <td className="quality-test-record-time is-numeric">{job.output_tokens?.toLocaleString() ?? '—'}</td>
            <td className="quality-test-record-action"><Button size="sm" variant="outline" onClick={(event) => { event.stopPropagation(); openRecord(job.id) }}><Eye className="size-3.5" />{t('qualityTest.viewResult')}</Button></td>
          </tr>)}</tbody>
        </table></div>}
        <Pagination page={recordPage} totalPages={Math.ceil(records.total / 20)} onPageChange={setRecordPage} totalItems={records.total} pageSize={20} />
      </section> : <>
      <div className="quality-test-workspace">
        <section className="quality-test-controls" aria-labelledby="quality-config-title">
          <div className="quality-test-section-heading"><span className="quality-test-step">01</span><h3 id="quality-config-title">{t('qualityTest.configuration')}</h3></div>
          {loadError ? <div role="alert" className="quality-test-error">{loadError}<Button size="sm" variant="outline" onClick={() => setReload((value) => value + 1)}>{t('common.retry')}</Button></div> : null}
          <fieldset disabled={submitting} className="quality-test-fields">
            <label htmlFor="quality-channel">{t('qualityTest.channel')}</label>
            <Select id="quality-channel" value={channel} disabled={submitting} onValueChange={(value) => { setChannel(value as UpstreamChannel); setAccount(null); setAccounts([]); setSearch(''); setLoadError('') }} options={shownChannels.map((item) => ({ value: item, label: channelNames[item] }))} />
            <label htmlFor="quality-account-search">{t('qualityTest.account')}</label>
            <Input id="quality-account-search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t('qualityTest.searchAccounts')} />
            <Select id="quality-account" aria-label={t('qualityTest.account')} value={String(account?.id ?? '')} disabled={accountLoading || submitting} placeholder={accountLoading ? t('qualityTest.loadingAccounts') : t('qualityTest.chooseAccount')} onValueChange={(value) => { setOptions(null); setModel(''); setAccount(accountChoices.find((row) => row.id === Number(value)) ?? null) }} options={accountChoices.map((row) => ({ value: String(row.id), label: `${formatAccountName(row)} · #${row.id} · ${row.plan_type || '—'}`, content: <AccountChoice account={row} />, triggerContent: <AccountChoice account={row} compact /> }))} />
            <p className="quality-test-hint">{!accountLoading && total === 0 ? t('qualityTest.noAccounts') : t('qualityTest.accountCount', { count: total })}</p>
            <label htmlFor="quality-model">{t('qualityTest.model')}</label>
            <Select id="quality-model" value={model} disabled={!options || optionsLoading || submitting} onValueChange={setModel} placeholder={optionsLoading ? t('qualityTest.loadingModels') : t('qualityTest.chooseModel')} options={(options?.models ?? []).map((item) => ({ value: item, label: item }))} />
            {options?.models.length === 0 ? <p className="quality-test-hint">{t('qualityTest.noModels')}</p> : null}
            <div className="quality-test-field-row">
              <div>
                <label htmlFor="quality-effort">{t('qualityTest.effort')}</label>
                <Select id="quality-effort" value={effort} disabled={!options || options.reasoning_efforts.length < 2 || submitting} onValueChange={setEffort} options={(options?.reasoning_efforts ?? ['high']).map((item) => ({ value: item, label: item ? t(`qualityTest.efforts.${item}`) : t('qualityTest.efforts.default') }))} />
              </div>
              <div>
                <label htmlFor="quality-timeout">{t('qualityTest.timeout')}</label>
                <Select id="quality-timeout" value={String(timeoutMinutes)} disabled={submitting} onValueChange={(value) => setTimeoutMinutes(Number(value))} options={QUALITY_TEST_TIMEOUT_MINUTES.map((value) => ({ value: String(value), label: t('qualityTest.timeoutOption', { count: value }) }))} />
              </div>
            </div>
            <p className="quality-test-hint">{t(options?.reasoning_efforts.length === 1 ? 'qualityTest.fixedEffort' : 'qualityTest.timeoutHint')}</p>
            <label htmlFor="quality-preset">{t('qualityTest.presets.select')}</label>
            <Select id="quality-preset" value={presetValue} disabled={submitting} onValueChange={applyPreset} options={[...QUALITY_TEST_BUILTIN_PRESETS.map((item) => ({ value: `builtin:${item.key}`, label: t(item.key === 'pelican' ? 'qualityTest.presets.selectDefault' : 'qualityTest.presets.builtinOption', { name: t(`qualityTest.presets.builtins.${item.key}`) }) })), ...presets.map((item) => ({ value: String(item.id), label: item.name })), ...(presetValue === 'custom' ? [{ value: 'custom', label: t('qualityTest.presets.selectCustom') }] : [])]} />
            <p className="quality-test-hint">{t(presets.length === 0 && !presetsLoading ? 'qualityTest.presets.selectEmptyHint' : 'qualityTest.presets.selectHint')}</p>
            <div className="quality-test-prompt-label"><label htmlFor="quality-prompt">{t('qualityTest.prompt')}</label><span><Button type="button" variant="ghost" size="xs" disabled={!prompt.trim() || promptBytes > 16000} onClick={() => setPresetDraft({ id: activePreset?.id, name: activePreset?.name ?? '', prompt })}><BookmarkPlus className="size-3" />{t(activePreset ? 'qualityTest.presets.updateCurrent' : 'qualityTest.presets.saveCurrent')}</Button><Button type="button" variant="ghost" size="xs" onClick={() => setPrompt(PELICAN_PROMPT)}>{t('qualityTest.resetPrompt')}</Button></span></div>
            <textarea id="quality-prompt" rows={5} value={prompt} onChange={(event) => setPrompt(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) { event.preventDefault(); startTest() } }} aria-invalid={promptBytes > 16000} />
            <p className="quality-test-hint quality-test-prompt-foot"><span>{t('qualityTest.promptHint')}</span><span className={`tabular-nums ${promptBytes > 16000 ? 'text-destructive' : ''}`}>{promptBytes.toLocaleString()} / 16,000</span></p>
            {promptBytes > 16000 ? <p role="alert" className="text-sm text-destructive">{t('qualityTest.promptTooLong')}</p> : null}
          </fieldset>
          <Button size="lg" className="w-full" onClick={startTest} disabled={submitting || !account || !model || optionsLoading || !prompt.trim() || promptBytes > 16000 || slotsFull || accountBusy}>
            {submitting ? <RefreshCw className="size-4 animate-spin" /> : <Play className="size-4" />}
            {t(submitting ? 'qualityTest.submitting' : accountBusy ? 'qualityTest.accountBusy' : slotsFull ? 'qualityTest.slotsFull' : 'qualityTest.start')}
            {!submitting && !accountBusy && !slotsFull ? <kbd className="quality-test-kbd" title={t('qualityTest.startShortcut', { keys: startKeys })}>{startKeys}</kbd> : null}
          </Button>
          <p className="quality-test-hint quality-test-footnote">{t('qualityTest.runHint')}</p>
        </section>

        <section className="quality-test-results" aria-labelledby="quality-result-title">
          <div className="quality-test-result-toolbar">
            <div className="quality-test-section-heading"><span className="quality-test-step">02</span><h3 id="quality-result-title">{t('qualityTest.result')}</h3></div>
            <SegmentedPillGroup value={view} onChange={setView} label={t('qualityTest.result')} options={[{ value: 'preview', label: t('qualityTest.preview'), icon: <Eye className="size-3.5" /> }, { value: 'source', label: t('qualityTest.source'), icon: <Code2 className="size-3.5" /> }]} />
          </div>
          <div className="quality-test-run-meta">
            <span className={`quality-test-status quality-test-status-pill ${run ? qualityTestStatusKey(run) : ''}`} role="status">{running ? <RefreshCw className="size-3.5 animate-spin" /> : run?.status === 'completed' ? <Check className="size-3.5" /> : <span className="quality-test-status-dot" />}{t(`qualityTest.status.${qualityTestStatusKey(run)}`)}</span>
            {run ? <>
              <span className="quality-test-run-identity" title={`${run.account_name} · #${run.account_id}`}><span>{run.account_name}</span><PlanBadge plan={run.plan_type} /></span>
              <span className="quality-test-record-model">{run.model}</span>
              <span className="quality-test-effort-chip">{run.reasoning_effort || t('qualityTest.efforts.default')}</span>
              <span className="quality-test-run-when"><Clock3 className="size-3.5" />{formatBeijingTime(run.created_at)}<span>#{run.id}</span></span>
              <span className="quality-test-run-actions">{running ? <Button size="sm" variant="outline" disabled={cancelling || run.status === 'cancelling'} onClick={() => void stopTest()}><Square className="size-3.5" />{t(run.status === 'cancelling' ? 'qualityTest.status.cancelling' : 'qualityTest.stop')}</Button> : run.prompt ? <Button size="sm" variant="outline" disabled={rerunDisabled(run)} onClick={() => rerunJob(run)}><RotateCcw className="size-3.5" />{t('qualityTest.rerun')}</Button> : null}</span>
            </> : <span className="quality-test-run-ready">{t('qualityTest.readyHint')}</span>}
          </div>
          <RunNotice run={run} />
          <ResultCanvas run={run} view={view} html={html} svgExport={svgExport} previewKey={previewKey} narrow={narrowPreview} loading={detail.loading} />
          <div className="quality-test-preview-actions">
            <span className="quality-test-hint">{t('qualityTest.isolatedPreview')}</span>
            <PreviewActions run={run} html={html} view={view} narrow={narrowPreview} svgExport={svgExport} onToggleNarrow={() => setNarrowPreview((value) => !value)} onReplay={() => setPreviewKey((key) => key + 1)} onCopy={() => void copySource()} />
          </div>
          <dl className="quality-test-metrics">
            {([[t('qualityTest.duration'), run ? formatTime(run.duration_ms) : '—'], [t('qualityTest.firstContent'), formatTime(run?.first_content_ms), t('qualityTest.firstContentHint')], [t('qualityTest.outputTokens'), run?.output_tokens?.toLocaleString() ?? '—'], [t('qualityTest.reasoningTokens'), run?.reasoning_tokens?.toLocaleString() ?? '—']] as [string, string, string?][]).map(([label, value, hint]) => <div key={label} title={hint}><dt>{label}</dt><dd>{value}</dd>{hint ? <small className="quality-test-metric-hint">{hint}</small> : null}</div>)}
          </dl>
          {run?.response_model ? <p className="quality-test-response-model">{t('qualityTest.responseModel')}: {run.response_model}</p> : null}
          {run?.prompt ? <details className="quality-test-saved-prompt"><summary>{t('qualityTest.savedPrompt')}</summary><p>{run.prompt}</p>{run.completed_at ? <small>{t('qualityTest.finishedAt')}: {formatBeijingTime(run.completed_at)}</small> : null}</details> : null}
        </section>
      </div>

      <section className="quality-test-review">
        <div><h3>{t('qualityTest.reviewTitle')}</h3><p>{t('qualityTest.reviewHint')}</p></div>
        <ul>{['shape', 'mechanics', 'motion', 'completion'].map((item, index) => <li key={item}><span>0{index + 1}</span>{t(`qualityTest.criteria.${item}`)}</li>)}</ul>
      </section>
      </>}
      <ResultDialog id={modalID} revision={revision} onClose={() => setModalID(undefined)} onOpenStudio={(id) => { setModalID(undefined); selectJob(id, 'studio') }} onRerun={rerunJob} rerunDisabled={rerunDisabled} />
      <PresetEditorDialog draft={presetDraft} saving={presetSaving} onChange={(patch) => setPresetDraft((current) => current ? { ...current, ...patch } : current)} onClose={() => setPresetDraft(null)} onSave={() => void savePreset()} />
      {confirmDialog}
    </div>
  )
}
