import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { ChevronDown, ChevronRight, Download, ExternalLink, ListChecks, MessageSquareText, Send } from 'lucide-react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api } from '../api'
import {
  formatPromptRequestSections, formatPromptRequestChange, formatPromptRequestChangeSections,
  promptRequestContextCoversPart, promptRequestReadInstruction, type PromptRequestTextPart,
} from '../lib/promptRequestDisplay'
import { downloadPromptSkill, promptSkillDownloadFileName, promptSkillMarkdown, promptSkillMarkdownIsPartial } from '../lib/promptSkillDownload'
import type {
  PromptRequestChanges, PromptRequestContextSection, PromptRequestConversationPage, PromptRequestDetail, PromptRequestImage,
  PromptRequestKeyRecording, PromptRequestRecord, PromptRequestResult, PromptRequestTask, PromptRequestTaskDetail, PromptRequestTaskInternalEvent,
  PromptRequestTaskStep, PromptRequestToolResult,
} from '../types'
import { formatBeijingTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Select } from '@/components/ui/select'
import { Input } from '@/components/ui/input'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

const retentionChoices = [30, 90, 7, 1]
const replayPreviewLines = 5
const replayPreviewSteps = 5
const replayRefreshIntervalMs = 15_000

function compareReplayPosition(leftAt: string, leftID: number, rightAt: string, rightID: number) {
  return Date.parse(leftAt) - Date.parse(rightAt) || leftID - rightID
}

export default function PromptRequestRecords({ refresh }: { refresh: number }) {
  const { t } = useTranslation()
  const [searchParams, setSearchParams] = useSearchParams()
  const recordParam = searchParams.get('request')
  const directID = recordParam && /^[1-9]\d*$/.test(recordParam) ? Number(recordParam) : 0
  const showDirect = directID > 0 && Number.isSafeInteger(directID)
  const [cursors, setCursors] = useState([0])
  const cursor = cursors[cursors.length - 1]
  const [page, setPage] = useState<PromptRequestConversationPage>()
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [draftRetention, setDraftRetention] = useState<number>()
  const [savingRetention, setSavingRetention] = useState(false)
  const [retentionError, setRetentionError] = useState('')
  const [recordingSettingsRevision, setRecordingSettingsRevision] = useState(0)
  const [selectedID, setSelectedID] = useState<number>()

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setError('')
    void api.listPromptRequestConversations(cursor, controller.signal)
      .then((result) => { if (!controller.signal.aborted) setPage(result) })
      .catch((err: unknown) => { if (!controller.signal.aborted) setError(getErrorMessage(err)) })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [cursor, refresh, recordingSettingsRevision])

  async function saveRetention() {
    if (draftRetention === undefined || savingRetention) return
    setSavingRetention(true)
    setRetentionError('')
    try {
      const result = await api.updatePromptRequestRetention(draftRetention)
      setPage((current) => current && { ...current, retention_days: result.retention_days })
      setDraftRetention(undefined)
    } catch (err) {
      setRetentionError(getErrorMessage(err))
    } finally {
      setSavingRetention(false)
    }
  }

  function closeRequestDialog() {
    setSelectedID(undefined)
    if (showDirect) {
      const next = new URLSearchParams(searchParams)
      next.delete('request')
      setSearchParams(next, { replace: true })
    }
  }

  function reloadAfterExclusions() {
    closeRequestDialog()
    setLoading(true)
    setPage((current) => current && { ...current, groups: [], next_before: 0 })
    setCursors([0])
    setRecordingSettingsRevision((revision) => revision + 1)
  }

  return (
    <>
      <Card>
        <CardContent className="space-y-4 pt-5">
          {page && (
            <div className="flex flex-wrap items-center justify-between gap-3">
              <p className="text-sm text-muted-foreground">
                {page.enabled
                  ? t('promptFilter.requests.retention', { days: page.retention_days })
                  : t('promptFilter.requests.disabled')}
              </p>
              <div className="flex flex-wrap items-center gap-2">
                <label htmlFor="prompt-request-retention" className="text-sm font-medium">
                  {t('promptFilter.requests.retentionLabel')}
                </label>
                <Select
                  id="prompt-request-retention"
                  aria-label={t('promptFilter.requests.retentionLabel')}
                  className="w-28"
                  value={String(draftRetention ?? page.retention_days)}
                  options={retentionChoices.map((days) => ({
                    value: String(days), label: t('promptFilter.requests.retentionOption', { days }),
                  }))}
                  onValueChange={(value) => setDraftRetention(Number(value))}
                  disabled={savingRetention}
                />
                <Button variant="outline" size="sm" onClick={() => void saveRetention()}
                  disabled={loading || savingRetention || draftRetention === undefined || draftRetention === page.retention_days}>
                  {savingRetention ? t('common.saving') : t('common.save')}
                </Button>
                <PromptRequestKeyExclusions onSave={reloadAfterExclusions} />
              </div>
            </div>
          )}
          {retentionError ? <p role="alert" className="text-sm text-destructive">{retentionError}</p> : null}
          {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
          <div className="overflow-x-auto rounded-lg border">
            <Table aria-busy={loading}>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('promptFilter.requests.conversations.lastActivity')}</TableHead>
                  <TableHead>{t('promptFilter.requests.endpoint')}</TableHead>
                  <TableHead>{t('promptFilter.requests.model')}</TableHead>
                  <TableHead>API Key</TableHead>
                  <TableHead>{t('promptFilter.requests.replay.requestColumn')}</TableHead>
                  <TableHead><span className="sr-only">{t('promptFilter.requests.conversations.view')}</span></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {page?.groups.map((item) => (
                  <TableRow key={item.id}>
                    <TableCell className="whitespace-nowrap text-xs">{formatBeijingTime(item.created_at)}</TableCell>
                    <TableCell className="font-mono text-xs">
                      <div>{item.endpoint}</div>
                      <div className="mt-0.5 text-muted-foreground">{item.transport === 'websocket' ? 'WebSocket' : 'HTTP'}</div>
                    </TableCell>
                    <TableCell className="text-xs">{item.model || '—'}</TableCell>
                    <TableCell className="text-xs">{item.api_key_name || '—'}</TableCell>
                    <TableCell className="whitespace-nowrap text-sm tabular-nums">
                      {t('promptFilter.requests.replay.requestsCount', { count: item.rounds })}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button variant="outline" size="sm" onClick={() => setSelectedID(item.id)}>
                        {t('promptFilter.requests.conversations.view')}
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
                {!page?.groups.length && !error && (
                  <TableRow>
                    <TableCell colSpan={6} className="py-10 text-center text-sm text-muted-foreground">
                      {loading ? t('common.loading') : t('promptFilter.requests.empty')}
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
          <div className="flex justify-end gap-2">
            <Button variant="outline" size="sm" disabled={loading || cursors.length < 2}
              onClick={() => setCursors((items) => items.slice(0, -1))}>
              {t('promptFilter.requests.previous')}
            </Button>
            <Button variant="outline" size="sm" disabled={loading || !page?.next_before}
              onClick={() => { if (page?.next_before) setCursors((items) => [...items, page.next_before]) }}>
              {t('common.next')}
            </Button>
          </div>
        </CardContent>
      </Card>

      <Dialog open={showDirect || selectedID !== undefined} onOpenChange={(open) => { if (!open) closeRequestDialog() }}>
        <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>{showDirect
              ? `${t('promptFilter.requests.detail')} · #${directID}`
              : t('promptFilter.requests.replay.title')}</DialogTitle>
            <DialogDescription>{showDirect
              ? t('promptFilter.requests.detailDescription')
              : t('promptFilter.requests.replay.timeline')}</DialogDescription>
          </DialogHeader>
          {showDirect ? <RoundDetail key={directID} id={directID} />
            : selectedID !== undefined ? <ConversationTimeline key={selectedID} id={selectedID} /> : null}
        </DialogContent>
      </Dialog>
    </>
  )
}

function PromptRequestKeyExclusions({ onSave }: { onSave: () => void }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [keys, setKeys] = useState<PromptRequestKeyRecording[]>()
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [search, setSearch] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!open) return
    const controller = new AbortController()
    setKeys(undefined)
    setSearch('')
    setError('')
    void api.getPromptRequestKeyExclusions(controller.signal)
      .then(({ keys }) => {
        if (controller.signal.aborted) return
        setKeys(keys)
        setSelected(new Set(keys.filter((key) => key.excluded).map((key) => key.id)))
      })
      .catch((err: unknown) => { if (!controller.signal.aborted) setError(getErrorMessage(err)) })
    return () => controller.abort()
  }, [open])

  async function save() {
    setSaving(true)
    setError('')
    try {
      await api.updatePromptRequestKeyExclusions([...selected])
      setOpen(false)
      onSave()
    } catch (err: unknown) {
      setError(getErrorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  const query = search.trim().toLowerCase()
  const matches = keys?.filter((key) => key.name.toLowerCase().includes(query) || String(key.id).includes(query))
  const changed = keys?.some((key) => key.excluded !== selected.has(key.id))
  const label = t('promptFilter.requests.excludeKeys.label')

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!saving) setOpen(next) }}>
      <DialogTrigger asChild>
        <Button type="button" variant="outline" size="sm">{label}</Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{label}</DialogTitle>
          <DialogDescription>{t('promptFilter.requests.excludeKeys.description')}</DialogDescription>
        </DialogHeader>
        {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
        {keys ? <>
          <Input aria-label={t('promptFilter.requests.excludeKeys.search')}
            placeholder={t('promptFilter.requests.excludeKeys.search')} value={search}
            onChange={(event) => setSearch(event.target.value)} disabled={saving} />
          {matches?.length ? (
            <fieldset className="max-h-64 min-w-0 space-y-1 overflow-y-auto rounded-md border p-2" disabled={saving}>
              <legend className="sr-only">{label}</legend>
              {matches.map((key) => (
                <label key={key.id} className="flex min-w-0 cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-sm hover:bg-muted">
                  <input type="checkbox" className="size-4 shrink-0 accent-primary" checked={selected.has(key.id)}
                    onChange={(event) => {
                      const next = new Set(selected)
                      if (event.target.checked) next.add(key.id)
                      else next.delete(key.id)
                      setSelected(next)
                    }} />
                  <span className="break-all">{key.name || 'API Key'}</span>
                  <span className="shrink-0 text-xs text-muted-foreground">#{key.id}</span>
                </label>
              ))}
            </fieldset>
          ) : <p className="text-sm text-muted-foreground">{t('promptFilter.requests.excludeKeys.empty')}</p>}
        </> : !error ? <p role="status" className="text-sm text-muted-foreground">{t('common.loading')}</p> : null}
        <DialogFooter>
          <Button type="button" variant="outline" disabled={saving} onClick={() => setOpen(false)}>{t('common.cancel')}</Button>
          <Button type="button" disabled={saving || !changed} onClick={() => void save()}>
            {t(saving ? 'common.saving' : 'common.save')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ConversationTimeline({ id }: { id: number }) {
  const { t } = useTranslation()
  const [tasks, setTasks] = useState<PromptRequestTask[]>([])
  const [internalEvents, setInternalEvents] = useState<PromptRequestTaskInternalEvent[]>([])
  const [total, setTotal] = useState(0)
  const [cursor, setCursor] = useState(0)
  const [nextBefore, setNextBefore] = useState(0)
  const [retry, setRetry] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showRequests, setShowRequests] = useState(false)

  useEffect(() => {
    const controller = new AbortController()
    const live = cursor === 0
    let timer: number | undefined
    setLoading(true)
    setError('')

    async function loadPage() {
      try {
        const result = await api.listPromptRequestTasks(id, cursor, controller.signal, live)
        if (controller.signal.aborted) return
        setTasks((previous) => cursor ? [...result.tasks, ...previous] : result.tasks)
        setInternalEvents((previous) => cursor ? [...(result.internal_events ?? []), ...previous] : result.internal_events ?? [])
        setTotal(result.total_requests)
        setNextBefore(result.next_before)
        if (live && result.tasks.some((task) => task.status === 'pending')) {
          timer = window.setTimeout(() => void loadPage(), replayRefreshIntervalMs)
        }
      } catch (err) {
        if (!controller.signal.aborted) setError(getErrorMessage(err))
      } finally {
        if (!controller.signal.aborted) setLoading(false)
      }
    }

    void loadPage()
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [id, cursor, retry])

  const orderedEvents = [...internalEvents].sort((left, right) => compareReplayPosition(left.started_at, left.id, right.started_at, right.id))
  let eventIndex = 0
  const taskCards = tasks.map((task) => {
    const firstEvent = eventIndex
    while (eventIndex < orderedEvents.length) {
      const event = orderedEvents[eventIndex]
      if (compareReplayPosition(event.started_at, event.id, task.started_at, task.id) >= 0) break
      eventIndex += 1
    }
    return <TaskCard key={task.id} summary={task} events={orderedEvents.slice(firstEvent, eventIndex)} />
  })
  const remainingEvents = orderedEvents.slice(eventIndex)

  return (
    <div className="min-w-0 space-y-3">
      <div className="flex items-center justify-between gap-2">
        {total > 0 ? <p className="text-sm text-muted-foreground">{t('promptFilter.requests.replay.modelCallsCount', { count: total })}</p> : null}
        {nextBefore > 0 ? (
          <Button variant="outline" size="sm" disabled={loading || Boolean(error)} onClick={() => setCursor(nextBefore)}>
            {t('promptFilter.requests.replay.earlier')}
          </Button>
        ) : null}
      </div>
      {error ? <div className="flex flex-wrap items-center gap-2">
        <p role="alert" className="text-sm text-destructive">{error}</p>
        <Button size="sm" variant="outline" onClick={() => setRetry(retry + 1)}>{t('promptFilter.requests.replay.retry')}</Button>
      </div> : null}
      {loading && tasks.length === 0 && internalEvents.length === 0 ? <p className="text-sm text-muted-foreground">{t('common.loading')}</p> : null}
      {taskCards}
      {remainingEvents.length ? <details className="rounded-md border">
        <summary className="cursor-pointer px-3 py-2 text-sm font-medium">{t('promptFilter.requests.replay.execution')}</summary>
        <ol aria-label={t('promptFilter.requests.replay.executionSteps')} className="space-y-1.5 px-2 pb-2">
          {remainingEvents.map((event) => <li key={event.id}><TaskCompactionStep event={event} /></li>)}
        </ol>
      </details> : null}
      <details className="rounded-md border" onToggle={(event) => setShowRequests(event.currentTarget.open)}>
        <summary className="cursor-pointer px-3 py-2 text-sm font-medium">{t('promptFilter.requests.replay.rawRequests')}</summary>
        {showRequests ? <ConversationRequests id={id} /> : null}
      </details>
    </div>
  )
}

function TaskCompactionStep({ event, beforeModel }: { event: PromptRequestTaskInternalEvent; beforeModel?: boolean }) {
  const { t } = useTranslation()
  const contentID = useId()
  const [open, setOpen] = useState(false)

  return <div className="min-w-0 rounded-md border">
    <Button type="button" size="sm" variant="ghost" aria-controls={contentID} aria-expanded={open}
      className="flex h-auto w-full flex-wrap justify-start gap-x-2 gap-y-1 px-2 py-1.5 text-xs"
      onClick={() => setOpen(!open)}>
      <ChevronRight aria-hidden="true" className={`size-3 shrink-0 ${open ? 'rotate-90' : ''}`} />
      <span>{t('promptFilter.requests.replay.contextCompaction')} · #{event.id}</span>
      {beforeModel ? <span className="text-muted-foreground">{t('promptFilter.requests.replay.beforeModelCalls')}</span> : null}
      <time dateTime={event.started_at} className="ml-auto text-muted-foreground">{formatBeijingTime(event.started_at)}</time>
    </Button>
    <div id={contentID} hidden={!open}>
      {open ? <div className="px-2 pb-2"><RoundDetail id={event.id} /></div> : null}
    </div>
  </div>
}

function ConversationRequests({ id }: { id: number }) {
  const { t } = useTranslation()
  const [requests, setRequests] = useState<PromptRequestRecord[]>([])
  const [before, setBefore] = useState(0)
  const [nextBefore, setNextBefore] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [openID, setOpenID] = useState<number>()

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setError('')
    void api.listPromptRequestRounds(id, before, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return
        setRequests((previous) => before ? [...result.rounds, ...previous] : result.rounds)
        setNextBefore(result.next_before)
      })
      .catch((err: unknown) => { if (!controller.signal.aborted) setError(getErrorMessage(err)) })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [id, before])

  return <div className="space-y-1.5 px-2 pb-2">
    {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
    {loading && requests.length === 0 ? <p className="text-sm text-muted-foreground">{t('common.loading')}</p> : null}
    {nextBefore > 0 ? <Button type="button" size="sm" variant="outline" disabled={loading} onClick={() => setBefore(nextBefore)}>
      {t('promptFilter.requests.replay.earlierRequests')}
    </Button> : null}
    {requests.map((request) => (
      <div key={request.id} className="min-w-0 rounded-md border">
        <Button type="button" size="sm" variant="ghost" aria-expanded={openID === request.id}
          className="flex h-auto w-full flex-wrap justify-between gap-2 py-2"
          onClick={() => setOpenID(openID === request.id ? undefined : request.id)}>
          <span>#{request.id} · {formatBeijingTime(request.created_at)}</span>
          <span className="text-muted-foreground">{request.status || null}</span>
        </Button>
        {openID === request.id ? <RoundDetail id={request.id} /> : null}
      </div>
    ))}
  </div>
}

function TaskCard({ summary, events }: { summary: PromptRequestTask; events: PromptRequestTaskInternalEvent[] }) {
  const { t } = useTranslation()
  const headingID = useId()
  const userID = useId()
  const activityID = useId()
  const [activityOpen, setActivityOpen] = useState(false)
  const [load, setLoad] = useState(false)
  const [attempt, setAttempt] = useState(0)
  const [task, setTask] = useState<PromptRequestTaskDetail>()
  const [error, setError] = useState('')
  const awaitingReply = summary.status === 'pending'

  useEffect(() => {
    if (!load) return
    const controller = new AbortController()
    void api.getPromptRequestTask(summary.id, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return
        setTask(result)
        setError('')
      })
      .catch((err: unknown) => { if (!controller.signal.aborted) setError(getErrorMessage(err)) })
    return () => controller.abort()
  }, [summary.id, load, attempt, awaitingReply])

  function requestDetails() {
    if (error) {
      setError('')
      setAttempt((current) => current + 1)
    }
    setLoad(true)
  }

  const userText = task?.user?.text ?? summary.user_text ?? summary.user_preview
  const userImages = task?.user?.images ?? summary.user_images
  const userRequestID = task?.user?.request_id ?? summary.user_request_id ?? summary.id
  const needsFullUser = summary.user_text_truncated || (summary.user_text === undefined && Boolean(summary.user_preview))
  const loadingDetails = load && !error && (!task || (task.status === 'pending' && !awaitingReply))

  return (
    <article aria-labelledby={headingID} className="min-w-0 space-y-3 rounded-lg border p-3">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
        <h3 id={headingID} className="font-medium">{t('promptFilter.requests.replay.taskNumber', { id: summary.id })}</h3>
        <time dateTime={summary.started_at}>{formatBeijingTime(summary.started_at)}</time>
        <span>{t('promptFilter.requests.replay.modelCallsCount', { count: summary.request_count ?? summary.request_ids.length })}</span>
      </div>
      {summary.kind === 'context_resume' ? <p className="text-sm text-muted-foreground">
        {t('promptFilter.requests.replay.contextResume')}
      </p> : <section aria-labelledby={userID} className="flex min-w-0 flex-col items-end gap-2">
        <h4 id={userID} className="flex items-center gap-2 text-sm font-semibold"><Send className="size-4" aria-hidden="true" />{t('promptFilter.requests.replay.user')}</h4>
        {userText || needsFullUser || userImages?.length ? (
          <div className="w-fit max-w-full space-y-2 rounded-xl border bg-muted/25 px-3 py-2 sm:max-w-[85%]">
            {userImages?.length ? <div className="flex flex-wrap gap-2">
              {userImages.map((image) => <RequestImage key={`${userRequestID}:${image.index}`} id={userRequestID} image={image} inline />)}
            </div> : null}
            {userText || needsFullUser ? <ReplayTextPreview loadFull={needsFullUser && !task ? requestDetails : undefined} loading={loadingDetails}>
              <p className="whitespace-pre-wrap [overflow-wrap:anywhere]">{userText}</p>
            </ReplayTextPreview> : null}
            {needsFullUser && task && !task.user?.text ? <p className="text-xs text-muted-foreground">{t('promptFilter.requests.replay.fullTextUnavailable')}</p> : null}
          </div>
        ) : null}
        {!userText && !userImages?.length && (!needsFullUser || task) ? <p className="text-sm text-muted-foreground">{t('promptFilter.requests.replay.userMissing')}</p> : null}
      </section>}
      <Button type="button" variant="outline" size="sm" aria-controls={activityID} aria-expanded={activityOpen}
        onClick={() => {
          if (!activityOpen && !load) requestDetails()
          setActivityOpen(!activityOpen)
        }}>
        <ListChecks aria-hidden="true" />
        {t(activityOpen ? 'promptFilter.requests.replay.collapseExecution' : 'promptFilter.requests.replay.expandExecution')}
        <ChevronDown aria-hidden="true" className={activityOpen ? 'rotate-180' : undefined} />
      </Button>
      {loadingDetails ? <p role="status" className="text-xs text-muted-foreground">{t('common.loading')}</p> : null}
      {error ? <div className="flex flex-wrap items-center gap-2">
        <p role="alert" className="text-xs text-destructive">{error}</p>
        <Button type="button" size="xs" variant="outline" onClick={requestDetails}>{t('promptFilter.requests.replay.retry')}</Button>
      </div> : null}
      <div id={activityID} hidden={!activityOpen}>
        {activityOpen && task ? <section className="min-w-0 space-y-2">
          <h4 className="text-sm font-semibold">{t('promptFilter.requests.replay.activity')}</h4>
          <TaskActivity task={task} events={events} />
        </section> : null}
      </div>
      <section className="min-w-0 space-y-2">
        <h4 className="flex items-center gap-2 text-sm font-semibold"><MessageSquareText className="size-4" aria-hidden="true" />{t('promptFilter.requests.replay.reply')}</h4>
        <TaskReply summary={summary} task={task} loading={loadingDetails} loadFull={requestDetails} />
      </section>
    </article>
  )
}

function ReplayTextPreview({ children, loadFull, loading }: { children: ReactNode; loadFull?: () => void; loading?: boolean }) {
  const { t } = useTranslation()
  const id = useId()
  const contentRef = useRef<HTMLDivElement>(null)
  const [expanded, setExpanded] = useState(false)
  const [canExpand, setCanExpand] = useState(false)
  const showFull = expanded && !loadFull

  useLayoutEffect(() => {
    const content = contentRef.current
    if (!content) return

    function updateOverflow(element: HTMLDivElement) {
      const lineHeight = Number.parseFloat(window.getComputedStyle(element).lineHeight)
      const previewHeight = lineHeight * replayPreviewLines
      const { height, top } = element.getBoundingClientRect()
      setCanExpand(height > previewHeight + 0.5)
      for (const link of element.querySelectorAll<HTMLAnchorElement>('a[href]')) {
        link.inert = !showFull && link.getBoundingClientRect().top >= top + previewHeight
      }
    }

    updateOverflow(content)
    const observer = new ResizeObserver(() => updateOverflow(content))
    observer.observe(content)
    return () => observer.disconnect()
  }, [children, showFull])

  return (
    <div className="min-w-0 space-y-1">
      <div id={id} className="overflow-clip text-sm leading-relaxed"
        style={showFull ? undefined : { maxHeight: `${replayPreviewLines}lh` }}>
        <div ref={contentRef} className="flow-root">{children}</div>
      </div>
      {canExpand || loadFull ? (
        <Button type="button" variant="outline" size="xs" className="cursor-pointer" disabled={Boolean(loadFull) && loading}
          aria-controls={id} aria-expanded={showFull} onClick={() => {
            if (loadFull) {
              loadFull()
              setExpanded(true)
            } else {
              setExpanded(!expanded)
            }
          }}>
          <ChevronDown aria-hidden="true" className={showFull ? 'rotate-180' : undefined} />
          {t(showFull ? 'promptFilter.requests.replay.collapseText' : 'promptFilter.requests.replay.expandText')}
        </Button>
      ) : null}
    </div>
  )
}

function TaskSkillDialog({ name, sections, clientObserved }: {
  name: string
  sections: PromptRequestContextSection[]
  clientObserved: boolean
}) {
  const { t } = useTranslation()
  const downloadableSource = sections.findIndex((item) => {
    const markdown = promptSkillMarkdown(item)
    return Boolean(promptSkillDownloadFileName(item.name) && markdown && !promptSkillMarkdownIsPartial(item, markdown))
  })
  const [selected, setSelected] = useState(() => Math.max(0, downloadableSource))
  const section = sections[selected]
  const markdown = section ? promptSkillMarkdown(section) : undefined
  const partial = Boolean(section && markdown && promptSkillMarkdownIsPartial(section, markdown))
  const fileName = promptSkillDownloadFileName(section?.name)
  const message = !markdown ? 'skillNoMarkdown' : partial ? 'skillPartialMarkdown' : !fileName ? 'skillInvalidName' : undefined

  return (
    <Dialog>
      <DialogTrigger asChild>
        <Badge asChild variant="outline" className="cursor-pointer whitespace-normal break-all text-foreground hover:bg-accent">
          <button type="button">
            {name}
            {downloadableSource >= 0 ? <Download aria-hidden="true" className="shrink-0 text-muted-foreground" /> : null}
          </button>
        </Badge>
      </DialogTrigger>
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle className="break-all pr-6">{name}</DialogTitle>
          <DialogDescription>{t('promptFilter.requests.replay.skillDetails')}</DialogDescription>
        </DialogHeader>
        {sections.length > 1 ? (
          <Select
            aria-label={t('promptFilter.requests.replay.skillSourceChoice')}
            value={String(selected)}
            options={sections.map((item, index) => {
              const origin = item.source_request_id
                ? t('promptFilter.requests.roundContext.sourceRound', { id: item.source_request_id })
                : t(`promptFilter.requests.roundContext.sources.${item.source}`, { defaultValue: item.source ?? name })
              return { value: String(index), label: item.path ? `${origin} · ${item.path}` : origin }
            })}
            onValueChange={(value) => setSelected(Number(value))}
          />
        ) : null}
        <div className="min-w-0 space-y-2">
          {section ? <RoundPromptSection section={{ ...section, text: markdown ?? section.text }} multiple={false} /> : null}
          {clientObserved ? <Badge variant="outline">{t('promptFilter.requests.replay.source.client')}</Badge> : null}
          {message ? <p className="text-sm text-muted-foreground">
            {t(`promptFilter.requests.replay.${message}`)}
          </p> : null}
        </div>
        <DialogFooter>
          <Button type="button" disabled={Boolean(message)} onClick={() => {
            if (!message && section?.name && markdown) downloadPromptSkill(section.name, markdown)
          }}>
            <Download aria-hidden="true" />
            {t('promptFilter.requests.replay.skillDownload')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function TaskActivity({ task, events }: { task: PromptRequestTaskDetail; events: PromptRequestTaskInternalEvent[] }) {
  const { t } = useTranslation()
  const skills = task.skills ?? []
  const loaded = skills.filter((section) => section.kind === 'loaded_skill')
  const attempted = skills.filter((section) => section.kind === 'attempted_skill')
  const clientSteps = task.client_steps ?? []
  const clientSkills = [...new Set(clientSteps.filter((step) => step.kind === 'skill' && step.status === 'loaded').map((step) => step.name))]
  const unnamedSkill = t('promptFilter.requests.context.loadedSkill')
  const skillNames = [...new Set([...loaded.map((skill) => skill.name ?? unnamedSkill), ...clientSkills])]
  const steps = task.steps ?? []
  const usedSkillNames = [...new Set(steps.flatMap((step) => (step.skill_usage ?? []).map((skill) => skill.name)))]
  const observedTools = [...new Set([
    ...steps.flatMap((step) => (step.tools ?? []).flatMap((tool) => tool.name ? [tool.name] : [])),
    ...clientSteps.filter((step) => step.kind === 'tool').map((step) => step.name),
  ])]
  const gatewayRows = [
    ...steps.map((step) => ({ id: step.request_id, at: step.created_at, content: <TaskStep step={step} context={skills} /> })),
    ...events.map((event) => ({ id: event.id, at: event.started_at, content: <TaskCompactionStep event={event} beforeModel /> })),
  ].sort((left, right) => compareReplayPosition(left.at, left.id, right.at, right.id))
  const gatewaySteps = (
    <ReplayStepList label={t('promptFilter.requests.replay.executionSteps')} className="space-y-1.5">
      {gatewayRows.map((row) => <li key={row.id}>{row.content}</li>)}
    </ReplayStepList>
  )

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-1.5 text-xs">
        <span className="text-muted-foreground">{t('promptFilter.requests.replay.tools')}</span>
        {observedTools.length ? observedTools.map((name) => (
          <Badge key={name} variant="secondary" className="whitespace-normal break-all">
            {t(`promptFilter.requests.readable.tools.${name}`, { defaultValue: name })}
          </Badge>
        )) : <span className="text-muted-foreground">{t('promptFilter.requests.replay.toolsMissing')}</span>}
      </div>
      <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
        {skillNames.length ? <>
          <span>{t('promptFilter.requests.replay.loadedSkills')}</span>
          {skillNames.map((name) => (
            <TaskSkillDialog key={name} name={name} clientObserved={clientSkills.includes(name)}
              sections={loaded.filter((item) => (item.name ?? unnamedSkill) === name)} />
          ))}
        </> : <span>{t('promptFilter.requests.replay.loadedSkills')} · {t('promptFilter.requests.roundContext.noLoadedSkill')}</span>}
      </div>
      {skillNames.length || usedSkillNames.length ? (
        <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          {usedSkillNames.length ? <>
            <span>{t('promptFilter.requests.replay.usedSkills')}</span>
            {usedSkillNames.map((name) => (
              <Badge key={name} variant="secondary" className="whitespace-normal break-all">{name}</Badge>
            ))}
          </> : <span>{t('promptFilter.requests.replay.usedSkills')} · {t('promptFilter.requests.replay.useEvidenceMissing')}</span>}
        </div>
      ) : null}
      {attempted.length ? (
        <details className="rounded-md border">
          <summary className="cursor-pointer px-2 py-1.5 text-xs font-medium">{t('promptFilter.requests.context.attemptedSkill')}</summary>
          <div className="space-y-2 px-2 pb-2">{attempted.map((skill, index) => <RoundPromptSection key={index} section={skill} multiple />)}</div>
        </details>
      ) : null}
      {clientSteps.length ? <>
        <ReplayStepList label={t('promptFilter.requests.replay.clientExecutionSteps')} className="divide-y rounded-md border">
          {clientSteps.map((step, i) => {
            const confirmed = step.kind === 'skill' && step.status === 'loaded'
            return (
              <li key={i} className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 px-2 py-1.5 text-xs">
                <span className="font-medium break-all">
                  {confirmed ? t('promptFilter.requests.replay.readSkillStep') : t(`promptFilter.requests.readable.tools.${step.name}`, { defaultValue: step.name })}
                </span>
                {!confirmed ? <span className="text-muted-foreground">{t(`promptFilter.requests.replay.status.${step.status}`, { defaultValue: step.status })}</span> : null}
                {step.at ? <time className="text-muted-foreground" dateTime={step.at}>{formatBeijingTime(step.at)}</time> : null}
                {confirmed ? <Badge variant="outline" className="ml-auto whitespace-normal break-all">
                  {t('promptFilter.requests.replay.stepSkill', { name: step.name })}
                </Badge> : null}
              </li>
            )
          })}
        </ReplayStepList>
        {gatewayRows.length ? <details className="rounded-md border">
          <summary className="cursor-pointer px-2 py-1.5 text-xs font-medium">{t('promptFilter.requests.replay.gatewaySteps')}</summary>
          <div className="px-2 pb-2">{gatewaySteps}</div>
        </details> : null}
      </> : gatewaySteps}
      {task.steps_truncated || task.client_steps_truncated ? <p className="text-xs text-muted-foreground">{t('promptFilter.requests.replay.stepsTruncated')}</p> : null}
    </div>
  )
}

function ReplayStepList({ label, className, children }: { label: string; className: string; children: ReactNode[] }) {
  const { t } = useTranslation()
  const id = useId()
  const [expanded, setExpanded] = useState(false)
  const remaining = children.length - replayPreviewSteps

  if (children.length === 0) return null

  return (
    <div className="space-y-1">
      <ol id={id} aria-label={label} className={className}>
        {expanded ? children : children.slice(0, replayPreviewSteps)}
      </ol>
      {remaining > 0 ? (
        <Button type="button" variant="ghost" size="sm" className="h-auto px-2 py-1 text-xs"
          aria-controls={id} aria-expanded={expanded} onClick={() => setExpanded(!expanded)}>
          {expanded ? t('promptFilter.requests.replay.collapseSteps') : t('promptFilter.requests.replay.expandSteps', { count: remaining })}
        </Button>
      ) : null}
    </div>
  )
}

function TaskStep({ step, context }: { step: PromptRequestTaskStep; context: PromptRequestContextSection[] }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [showRequest, setShowRequest] = useState(false)
  const tools = step.tools ?? []
  const names = [...new Set(tools.flatMap((tool) => tool.name ? [tool.name] : []))]
  const loaded = step.skills ?? []
  const used = step.skill_usage ?? []
  const delegations = step.delegations ?? []
  const commentary = step.commentary?.trim()
  const showCommentary = commentary && !step.response?.text?.includes(commentary)

  const stepLabel = step.kind === 'context_compaction' ? t('promptFilter.requests.replay.contextCompaction')
    : step.kind === 'context_resume' ? t('promptFilter.requests.replay.contextResume')
    : names.length ? names.map((name) => t(`promptFilter.requests.readable.tools.${name}`, { defaultValue: name })).join(' · ')
    : t('promptFilter.requests.replay.requestStep')
  let emptyResponseLabel = t('promptFilter.requests.outcome.missingReply')
  if (step.response?.tool_only) emptyResponseLabel = t('promptFilter.requests.replay.stepToolOnly')
  else if (step.response) emptyResponseLabel = t('promptFilter.requests.outcome.emptyReply')

  return (
    <details className="min-w-0 rounded-md border" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-2 gap-y-1 px-2 py-1.5 text-xs font-medium [&::-webkit-details-marker]:hidden">
        <span className="flex min-w-0 flex-1 items-center gap-1">
          <ChevronRight aria-hidden="true" className={`size-3 shrink-0 ${open ? 'rotate-90' : ''}`} />
          <span className="break-words">
            {delegations.length ? `${t('promptFilter.requests.replay.delegationResult')} · ` : ''}
            {stepLabel}
            <span className="ml-2 whitespace-nowrap text-muted-foreground">#{step.request_id}{step.status ? ` · ${step.status}` : ''}</span>
          </span>
        </span>
        {loaded.length || used.length ? (
          <span className="flex min-w-0 max-w-full basis-full flex-wrap gap-1 pl-4 sm:ml-auto sm:basis-auto sm:justify-end sm:pl-0">
            {loaded.map((skill, index) => (
              <Badge key={`loaded-${index}`} variant="outline" className="whitespace-normal break-all"
                title={t('promptFilter.requests.roundContext.sourceRound', { id: skill.source_request_id })}>
                {t('promptFilter.requests.replay.stepSkill', { name: skill.name })}
                <span className="whitespace-nowrap text-muted-foreground">
                  {t(skill.source === 'tool_result' ? 'promptFilter.requests.replay.stepReceipt' : 'promptFilter.requests.replay.stepEvidence', { id: skill.source_request_id })}
                </span>
              </Badge>
            ))}
            {used.map((skill, index) => (
              <Badge key={`used-${index}`} variant="secondary" className="whitespace-normal break-all"
                title={t('promptFilter.requests.roundContext.sourceRound', { id: skill.source_request_id })}>
                {t('promptFilter.requests.replay.stepSkillUse', { name: skill.name })}
                <span className="whitespace-nowrap text-muted-foreground">
                  {t('promptFilter.requests.replay.stepReceipt', { id: skill.source_request_id })}
                </span>
              </Badge>
            ))}
          </span>
        ) : null}
      </summary>
      {open ? <div className="space-y-2 px-2 pb-2">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
          <time dateTime={step.created_at}>{formatBeijingTime(step.created_at)}</time>
          <span className="break-all font-mono">{step.endpoint}</span>
          {step.model ? <span className="break-all">{step.model}</span> : null}
          {step.action ? <span>{t(`promptFilter.requests.actions.${step.action}`, { defaultValue: step.action })}</span> : null}
        </div>
        {delegations.length ? <section className="min-w-0 space-y-2 rounded-md border bg-muted/20 p-2">
          <h5 className="text-xs font-semibold">{t('promptFilter.requests.replay.delegationResult')}</h5>
          {delegations.map((delegation, index) => (
            <div key={index} className="min-w-0 space-y-1">
              <p className="break-all text-xs text-muted-foreground">
                {t('promptFilter.requests.replay.delegationSource', { id: delegation.source_thread_id })}
              </p>
              <ReplayTextPreview><p className="whitespace-pre-wrap [overflow-wrap:anywhere]">{delegation.text}</p></ReplayTextPreview>
            </div>
          ))}
        </section> : null}
        <Button type="button" size="sm" variant="outline" aria-expanded={showRequest} onClick={() => setShowRequest(!showRequest)}>
          {t('promptFilter.requests.replay.requestDetails', { id: step.request_id })}
        </Button>
        {showRequest ? <RoundDetail id={step.request_id} showResults={step.response?.truncated === true} /> : null}
        <section className="min-w-0 space-y-1 rounded-md border bg-muted/20 p-2">
          <div className="flex flex-wrap items-center gap-2">
            <h5 className="text-xs font-semibold">{t('promptFilter.requests.replay.stepResponse')}</h5>
            {step.response ? <Badge variant={step.response.status === 'completed' ? 'outline' : 'destructive'}>
              {t(`promptFilter.requests.outcome.status.${step.response.status}`, { defaultValue: step.response.status })}
            </Badge> : null}
            {step.response?.truncated ? <Badge variant="outline">{t('promptFilter.requests.replay.stepResponsePreviewTruncated')}</Badge> : null}
          </div>
          {step.response?.text ? <ReplayTextPreview><TaskReplyMarkdown text={step.response.text} /></ReplayTextPreview>
            : <p className="text-xs text-muted-foreground">{emptyResponseLabel}</p>}
          {step.response?.error ? <div className="text-destructive"><ReadableText text={step.response.error} /></div> : null}
        </section>
        {showCommentary ? <div className="space-y-1 rounded-md bg-muted/25 p-2">
          <p className="text-xs font-medium">{t('promptFilter.requests.replay.commentary')}</p>
          <ReadableText text={step.commentary ?? ''} />
        </div> : null}
        {tools.map((tool, index) => (
          <div key={index} className="min-w-0 space-y-2 rounded-md border bg-muted/15 p-2">
            {tool.name || tool.arguments !== undefined ? <RoundToolCall name={tool.name} argumentsText={tool.arguments} /> : null}
            {tool.result !== undefined || tool.images?.length ? <RoundToolReceipt receipt={{
              call_id: tool.call_id ?? '', name: tool.name, text: tool.result ?? '',
              from_request_id: tool.from_request_id ?? step.request_id, images: tool.images,
            }} context={context} showSource={tool.from_request_id !== undefined} /> : null}
          </div>
        ))}
        <TaskStepSkillEvidence step={step} context={context} />
      </div> : null}
    </details>
  )
}

function TaskStepSkillEvidence({ step, context }: { step: PromptRequestTaskStep; context: PromptRequestContextSection[] }) {
  const { t } = useTranslation()
  const loaded = step.skills ?? []
  const used = step.skill_usage ?? []
  return <>
    {loaded.length ? <section className="space-y-1.5">
      <h5 className="text-xs font-semibold">{t('promptFilter.requests.replay.stepLoadEvidence')}</h5>
      {loaded.map((skill, index) => {
        const observed = context.find((item) => item.kind === 'loaded_skill' && item.name === skill.name &&
          item.path === skill.path && item.source === skill.source &&
          (item.source_request_id === skill.source_request_id || (!item.source_request_id && skill.source !== 'tool_result')))
        const evidence: PromptRequestContextSection = observed ?? {
          kind: 'loaded_skill', state: 'received', ...skill,
          evidence: skill.source === 'tool_result' ? 'successful_read' : undefined,
        }
        return <RoundPromptSection key={index} section={{ ...evidence, source_request_id: skill.source_request_id }} multiple />
      })}
    </section> : null}
    {used.length ? <section className="space-y-1.5">
      <h5 className="text-xs font-semibold">{t('promptFilter.requests.replay.stepUseEvidence')}</h5>
      {used.map((skill, index) => (
        <div key={index} className="min-w-0 space-y-1.5 rounded-md bg-muted/25 p-2">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <strong className="break-all">{skill.name}</strong>
            <span className="text-muted-foreground">{t('promptFilter.requests.roundContext.sourceRound', { id: skill.source_request_id })}</span>
          </div>
          <p className="text-xs">{t('promptFilter.requests.replay.skillHelperExecution')}</p>
          <p className="break-all font-mono text-xs text-muted-foreground">{skill.path}</p>
          <p className="text-xs text-muted-foreground">{t('promptFilter.requests.replay.skillUseCommand')}</p>
          <ReadableText text={skill.command} />
          <p className="text-xs text-muted-foreground">{t('promptFilter.requests.replay.skillUseReceipt')}</p>
          <ReadableText text={skill.evidence} />
        </div>
      ))}
    </section> : null}
  </>
}

function TaskReply({ summary, task, loadFull, loading }: {
  summary: PromptRequestTask
  task?: PromptRequestTaskDetail
  loadFull: () => void
  loading: boolean
}) {
  const { t } = useTranslation()
  const reply = task?.final
  const text = reply?.text ?? summary.final_preview
  const taskStatus = summary.status ?? task?.status
  const finalStatus = reply?.status ?? summary.final_status ?? taskStatus
  const status = taskStatus === 'interrupted' || taskStatus === 'failed' ? taskStatus : finalStatus
  const source = reply?.source ?? summary.final_source
  const duration = task?.duration_ms ?? summary.duration_ms
  const timeSource = task?.timing_source ?? summary.timing_source
  let durationText = ''
  if (typeof duration === 'number' && Number.isFinite(duration) && duration >= 0 && (timeSource === 'client' || timeSource === 'gateway_observed')) {
    const seconds = Math.floor(duration / 1000)
    const parts = []
    if (seconds >= 3600) parts.push(t('promptFilter.requests.replay.duration.hours', { count: Math.floor(seconds / 3600) }))
    if (seconds % 3600 >= 60) parts.push(t('promptFilter.requests.replay.duration.minutes', { count: Math.floor((seconds % 3600) / 60) }))
    parts.push(seconds ? t('promptFilter.requests.replay.duration.seconds', { count: seconds % 60 })
      : t('promptFilter.requests.replay.duration.milliseconds', { count: Math.floor(duration) }))
    durationText = t(`promptFilter.requests.replay.time.${timeSource === 'client' ? 'client' : 'gateway'}`, { duration: parts.join(' ') })
  }

  return <div className="min-w-0 space-y-2 rounded-md border bg-muted/20 px-3 py-2">
    <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground" aria-live="polite">
      {status !== 'pending' || durationText ? <span>{durationText || t('promptFilter.requests.replay.timeMissing')}</span> : null}
      {source ? <Badge variant="outline">{t(`promptFilter.requests.replay.source.${source === 'gateway' && finalStatus !== 'completed' ? 'gatewayResponse' : source}`)}</Badge> : null}
      {status === 'interrupted' || status === 'failed' || status === 'pending' ? (
        <Badge variant={status === 'pending' ? 'outline' : 'destructive'}>{t(`promptFilter.requests.replay.status.${status}`)}</Badge>
      ) : null}
    </div>
    {text && finalStatus !== 'completed' ? <p className="text-xs text-muted-foreground">{t('promptFilter.requests.replay.partialReply')}</p> : null}
    {text ? <ReplayTextPreview loadFull={summary.final_preview_truncated && (!task || loading) ? loadFull : undefined} loading={loading}>
      <TaskReplyMarkdown text={text} />
    </ReplayTextPreview> : status !== 'pending' ? <p className="text-sm text-muted-foreground">{t(finalStatus === 'completed' && source
      ? 'promptFilter.requests.outcome.emptyReply' : 'promptFilter.requests.replay.replyMissing')}</p> : null}
    {reply?.truncated || (summary.final_preview_truncated && task && !reply) ? <p className="text-xs text-muted-foreground">{t('promptFilter.requests.outcome.truncated')}</p> : null}
    {(reply?.redacted ?? summary.final_redacted) ? <p className="text-xs text-muted-foreground">{t('promptFilter.requests.replay.redacted')}</p> : null}
  </div>
}

function TaskReplyMarkdown({ text }: { text: string }) {
  const { t } = useTranslation()
  return <div className="min-w-0 space-y-2 break-words [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:pl-3 [&_code]:break-all [&_li]:ml-4 [&_li]:pl-0.5 [&_ol]:list-decimal [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:bg-background [&_pre]:p-2 [&_table]:block [&_table]:overflow-x-auto [&_td]:border [&_td]:p-1.5 [&_th]:border [&_th]:p-1.5 [&_ul]:list-disc">
    <Markdown skipHtml remarkPlugins={[remarkGfm]} urlTransform={(value) => requestTraceHref(value) ?? ''} components={{
      a: ({ href, children }) => href ? <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> : <span>{children}</span>,
      img: ({ alt }) => <span>{alt || t('promptFilter.requests.readable.image')}</span>,
    }}>{text}</Markdown>
  </div>
}

function RoundDetail({ id, showResults = true }: { id: number; showResults?: boolean }) {
  const { t } = useTranslation()
  const [detail, setDetail] = useState<PromptRequestDetail>()
  const [error, setError] = useState('')

  useEffect(() => {
    const controller = new AbortController()
    void api.getPromptRequest(id, controller.signal)
      .then((item) => { if (!controller.signal.aborted) setDetail(item) })
      .catch((err: unknown) => { if (!controller.signal.aborted) setError(getErrorMessage(err)) })
    return () => controller.abort()
  }, [id])

  return (
    <div className="min-w-0 border-t p-3">
      {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
      {detail ? <RequestDetails key={id} detail={detail} showResults={showResults} /> : null}
      {!detail && !error ? <p className="text-sm text-muted-foreground">{t('common.loading')}</p> : null}
    </div>
  )
}

function RequestDetails({ detail, showResults }: { detail: PromptRequestDetail; showResults: boolean }) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState(false)
  const changes = detail.changes
  const context = detail.prompt_context?.sections ?? []
  const changedContext = context.filter((section) => section.state !== 'inherited')
  const traceHref = requestTraceHref(detail.langfuse_trace_url)
  const indices = new Set(changes?.image_indices)
  const newImages = changes ? detail.images.filter((image) => indices.has(image.index)) : []
  const entries = changes?.entries.filter((entry) => !entry.result_request_id) ?? []
  const earlierResultRounds = new Set(changes?.entries.flatMap((entry) => entry.result_request_id ? [entry.result_request_id] : []))

  return (
    <div className="min-w-0 space-y-4">
      {detail.prompt_context || traceHref ? (
        <div className="min-w-0 space-y-2 rounded-lg border p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            {detail.prompt_context ? <h3 className="text-sm font-semibold">{t('promptFilter.requests.roundContext.heading')}</h3> : null}
            {traceHref ? (
              <Button size="sm" variant="outline" asChild>
                <a href={traceHref} target="_blank" rel="noopener noreferrer">
                  {t('promptFilter.requests.roundContext.langfuse')}<ExternalLink aria-hidden="true" />
                </a>
              </Button>
            ) : null}
          </div>
          {detail.prompt_context ? <RoundPromptContext sections={context} /> : null}
        </div>
      ) : null}
      {showResults ? <RoundResults result={detail.result} context={context} /> : null}
      {changes ? <section className="min-w-0 space-y-3">
        <div>
          <h3 className="text-sm font-semibold">{t('promptFilter.requests.changes.heading')}</h3>
          <p className="mt-1 text-xs text-muted-foreground">
            {t('promptFilter.requests.changes.comparison', {
              id: changes.previous_id, repeated: changes.repeated_items, newItems: changes.new_items,
            })}
          </p>
        </div>
        {changes.entries.length === 0 && newImages.length === 0 ? (
          <p className="rounded-lg border p-3 text-sm text-muted-foreground">{t('promptFilter.requests.changes.empty')}</p>
        ) : null}
        {entries.map((entry, index) => <RequestChange key={index} entry={entry} context={changedContext} />)}
        {[...earlierResultRounds].map((id) => (
          <p key={id} className="text-xs text-muted-foreground">{t('promptFilter.requests.outcome.resultInRound', { id })}</p>
        ))}
        <RequestImages id={detail.id} images={newImages} title={t('promptFilter.requests.changes.images')} />
      </section> : <section className="min-w-0 space-y-3">
        <h3 className="text-sm font-semibold">{t('promptFilter.requests.changes.received')}</h3>
        <CompleteRequest detail={detail} omitContext={context} />
      </section>}
      {changes || context.length > 0 ? <details className="rounded-lg border" onToggle={(event) => setExpanded(event.currentTarget.open)}>
        <summary className="cursor-pointer px-4 py-3 text-sm font-medium">
          {t('promptFilter.requests.changes.full')}
          {changes && changes.historical_images > 0 ? (
            <span className="ml-2 text-xs font-normal text-muted-foreground">
              {t('promptFilter.requests.changes.historicalImages', { count: changes.historical_images })}
            </span>
          ) : null}
        </summary>
        {expanded ? <div className="px-4 pb-4"><CompleteRequest detail={detail} /></div> : null}
      </details> : null}
    </div>
  )
}

function requestTraceHref(value?: string): string | undefined {
  if (!value) return undefined
  try {
    const url = new URL(value)
    if ((url.protocol === 'https:' || url.protocol === 'http:') && !url.username && !url.password) return url.href
  } catch {
    return undefined
  }
}

const roundContextKinds = [
  ['system_prompt', 'systemPrompt'],
  ['developer_prompt', 'developerPrompt'],
  ['agents_md', 'agents'],
  ['loaded_skill', 'loadedSkill'],
  ['attempted_skill', 'attemptedSkill'],
  ['available_skills', 'availableSkills'],
] as const

function RoundPromptContext({ sections }: { sections: PromptRequestContextSection[] }) {
  const { t } = useTranslation()
  return (
    <section aria-label={t('promptFilter.requests.roundContext.heading')} className="min-w-0 space-y-1.5">
      {roundContextKinds.map(([kind, label]) => {
        const group = sections.filter((section) => section.kind === kind)
        const title = label === 'agents' ? 'AGENTS.md' : t(`promptFilter.requests.context.${label}`)
        if (group.length === 0) {
          const empty = kind === 'system_prompt' ? 'noSystem' : kind === 'loaded_skill' ? 'noLoadedSkill' : undefined
          return empty ? (
            <p key={kind} className="flex flex-wrap items-center gap-2 px-3 py-1.5 text-sm">
              {title}<span className="text-xs text-muted-foreground">{t(`promptFilter.requests.roundContext.${empty}`)}</span>
            </p>
          ) : null
        }

        const states = [...new Set(group.map((section) => section.state))]
        const skillsCount = group.reduce((count, section) => count + (section.skills?.length ?? 0), 0)
        const countLabel = kind === 'available_skills'
          ? t('promptFilter.requests.context.catalogCount', { count: skillsCount })
          : group.length > 1 ? t('promptFilter.requests.roundContext.sectionCount', { count: group.length }) : undefined
        return (
          <details key={kind} className="min-w-0 rounded-md border bg-background">
            <summary className="cursor-pointer break-words px-3 py-2 text-sm font-medium">
              {title}
              {group.length === 1 && group[0].name ? <span className="ml-2 break-all font-mono text-xs">{group[0].name}</span> : null}
              {states.map((state) => (
                <Badge key={state} variant={state === 'new' ? 'secondary' : 'outline'} className="ml-2">
                  {t(`promptFilter.requests.roundContext.states.${state}`)}
                </Badge>
              ))}
              {countLabel ? <span className="ml-2 text-xs text-muted-foreground">{countLabel}</span> : null}
            </summary>
            <div className="space-y-3 px-3 pb-3">
              {group.map((section, index) => <RoundPromptSection key={index} section={section} multiple={group.length > 1} />)}
            </div>
          </details>
        )
      })}
    </section>
  )
}

function RoundPromptSection({ section, multiple }: { section: PromptRequestContextSection; multiple: boolean }) {
  const { t } = useTranslation()
  return (
    <div className="min-w-0 space-y-1.5 rounded-md bg-muted/25 p-2">
      {section.source || section.source_request_id || multiple ? (
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          {multiple && section.name ? <strong className="break-all text-foreground">{section.name}</strong> : null}
          {multiple ? <Badge variant="outline">{t(`promptFilter.requests.roundContext.states.${section.state}`)}</Badge> : null}
          {section.source ? <span>{t('promptFilter.requests.roundContext.source', {
            source: t(`promptFilter.requests.roundContext.sources.${section.source}`, { defaultValue: section.source }),
          })}</span> : null}
          {section.source_request_id ? <span>{t('promptFilter.requests.roundContext.sourceRound', { id: section.source_request_id })}</span> : null}
        </div>
      ) : null}
      {section.path ? <p className="break-all font-mono text-xs text-muted-foreground">{section.path}</p> : null}
      {section.evidence ? <p className="text-xs">{t('promptFilter.requests.roundContext.evidence', {
        evidence: t(`promptFilter.requests.roundContext.evidenceKinds.${section.evidence}`, { defaultValue: section.evidence }),
      })}</p> : null}
      {section.skills?.length ? (
        <ul className="space-y-1 text-xs">
          {section.skills.map((skill, index) => (
            <li key={index} className="flex min-w-0 flex-wrap gap-x-2">
              <span className="break-all font-medium">{skill.name}</span>
              {skill.path ? <span className="break-all text-muted-foreground">{skill.path}</span> : null}
            </li>
          ))}
        </ul>
      ) : null}
      {section.text ? <ReadableText text={section.text} /> : null}
    </div>
  )
}

function RequestChange({ entry, context }: {
  entry: PromptRequestChanges['entries'][number]
  context: PromptRequestContextSection[]
}) {
  const { t } = useTranslation()
  const readable = (key: string, fallback?: string) => t(`promptFilter.requests.readable.${key}`, { defaultValue: fallback ?? key })
  const parts = formatPromptRequestChangeSections(entry.text, entry.kind, readable, entry.label)
    .filter((part) => !promptRequestContextCoversPart(part, context))
  if (parts.length === 0) return null
  const contextOnly = parts.length > 0 && parts.every((part) => part.context)
  const labelGroup = entry.kind === 'tool_call' || entry.kind === 'tool_result' ? 'tools' : entry.kind === 'context' ? 'fields' : 'roles'

  return (
    <div className={contextOnly ? 'min-w-0 space-y-2' : 'min-w-0 space-y-1.5 rounded-lg border bg-muted/20 p-3'}>
      {contextOnly ? null : <h4 className="text-xs font-semibold">
        {t(`promptFilter.requests.changes.${entry.kind}`)}
        {entry.label ? <span className="ml-2 break-words font-normal text-muted-foreground">
          {readable(labelGroup + '.' + entry.label, entry.label)}
        </span> : null}
      </h4>}
      {parts.map((part, index) => part.context
        ? <RequestContextDetails key={index} part={part} />
        : part.text ? <ReadableText key={index} text={part.text} /> : null)}
    </div>
  )
}

function RequestContextDetails({ part }: { part: PromptRequestTextPart }) {
  const { t } = useTranslation()
  const context = part.context
  if (!context) return null
  const tag = context.kind === 'agents' ? 'AGENTS.md' : t(`promptFilter.requests.context.${context.kind}`)
  return (
    <details className="min-w-0 rounded-md border bg-background">
      <summary className="cursor-pointer px-3 py-2 text-sm">
        <Badge variant="secondary" className="ml-1">{tag}</Badge>
        {context.name ? <span className="ml-2 break-words font-medium">{context.name}</span> : null}
        {context.path ? <span className="ml-2 break-all text-xs text-muted-foreground">{context.path}</span> : null}
        {context.skills ? <span className="ml-2 text-xs text-muted-foreground">
          {t('promptFilter.requests.context.catalogCount', { count: context.skills.length })}
        </span> : null}
      </summary>
      <div className="min-w-0 space-y-2 px-3 pb-3">
        {context.skills?.length ? (
          <ul className="space-y-1 border-b pb-2 text-xs">
            {context.skills.map((skill, index) => (
              <li key={index} className="flex flex-wrap gap-x-2">
                <span className="break-words font-medium">{skill.name}</span>
                <span className="break-all text-muted-foreground">{skill.path}</span>
              </li>
            ))}
          </ul>
        ) : null}
        <ReadableText text={part.text} />
      </div>
    </details>
  )
}

function ReadableText({ text }: { text: string }) {
  return <pre className="max-h-[35vh] overflow-auto whitespace-pre-wrap break-words text-xs leading-relaxed">{text}</pre>
}

function RoundResults({ result, context }: { result?: PromptRequestResult; context: PromptRequestContextSection[] }) {
  const { t } = useTranslation()
  const response = result?.response
  const toolResults = result?.tool_results ?? []
  const calls = response?.tool_calls ?? []
  const callIDs = new Set(calls.map((call) => call.call_id).filter(Boolean))
  const resultsByCall = new Map<string, PromptRequestToolResult[]>()
  for (const receipt of toolResults) {
    if (callIDs.has(receipt.call_id)) {
      const previous = resultsByCall.get(receipt.call_id) ?? []
      resultsByCall.set(receipt.call_id, [...previous, receipt])
    }
  }
  const unpaired = toolResults.filter((receipt) => !callIDs.has(receipt.call_id))

  return (
    <section className="min-w-0 space-y-2 rounded-lg border bg-muted/20 p-3">
      <h3 className="text-sm font-semibold">{t('promptFilter.requests.outcome.heading')}</h3>
      {response ? <>
        <div className="flex flex-wrap items-center gap-2">
          <h4 className="text-xs font-semibold">{t('promptFilter.requests.outcome.reply')}</h4>
          <Badge variant={response.status === 'completed' ? 'outline' : 'destructive'}>
            {t(`promptFilter.requests.outcome.status.${response.status}`)}
          </Badge>
          {response.truncated ? <Badge variant="outline">{t('promptFilter.requests.outcome.truncated')}</Badge> : null}
        </div>
        {response.text
          ? <ReadableText text={response.text} />
          : <p className="text-xs text-muted-foreground">{t('promptFilter.requests.outcome.emptyReply')}</p>}
        {response.error ? <div className="rounded-md border p-2 text-destructive"><ReadableText text={response.error} /></div> : null}
      </> : <p className="text-xs text-muted-foreground">{t('promptFilter.requests.outcome.missingReply')}</p>}
      {calls.map((call, index) => (
        <div key={index} className="min-w-0 space-y-2 rounded-md border bg-background p-3">
          <RoundToolCall name={call.name} argumentsText={call.arguments} />
          {resultsByCall.get(call.call_id)?.map((receipt, receiptIndex) => (
            <RoundToolReceipt key={receiptIndex} receipt={receipt} name={call.name} argumentsText={call.arguments} context={context} />
          ))}
        </div>
      ))}
      {unpaired.map((receipt, index) => (
        <div key={index} className="min-w-0 space-y-2 rounded-md border bg-background p-3">
          {receipt.arguments !== undefined ? <RoundToolCall name={receipt.name} argumentsText={receipt.arguments} /> : null}
          <RoundToolReceipt receipt={receipt} context={context} />
        </div>
      ))}
    </section>
  )
}

function RoundToolCall({ name, argumentsText }: { name?: string; argumentsText?: string }) {
  const { t } = useTranslation()
  const readable = (key: string, fallback?: string) => t(`promptFilter.requests.readable.${key}`, { defaultValue: fallback ?? key })
  return (
    <div className="min-w-0 space-y-1">
      <h4 className="break-words text-xs font-semibold">
        {readable('toolCall')}{name ? ' · ' + readable('tools.' + name, name) : ''}
      </h4>
      {argumentsText ? <ReadableText text={formatPromptRequestChange(argumentsText, 'tool_call', readable, name)} /> : null}
    </div>
  )
}

function RoundToolReceipt({ receipt, name, argumentsText, context, showSource = true }: {
  receipt: PromptRequestToolResult
  name?: string
  argumentsText?: string
  context: PromptRequestContextSection[]
  showSource?: boolean
}) {
  const { t } = useTranslation()
  const readable = (key: string, fallback?: string) => t(`promptFilter.requests.readable.${key}`, { defaultValue: fallback ?? key })
  const toolName = receipt.name || name
  const candidate = promptRequestReadInstruction(toolName ?? '', receipt.arguments ?? argumentsText ?? '', receipt.text)
  const confirmedSkill = context.find((item) => item.kind === 'loaded_skill' && item.source === 'tool_result' &&
    item.source_request_id === receipt.from_request_id && item.path === candidate?.context?.path)
  const instruction = candidate?.context?.kind === 'loadedSkill' &&
    (!confirmedSkill || !promptRequestContextCoversPart(candidate, [confirmedSkill])) ? undefined : candidate
  const shownInContext = instruction && promptRequestContextCoversPart(instruction, context)
  return (
    <div className="min-w-0 space-y-1">
      <h4 className="break-words text-xs font-semibold">
        {readable('toolResult')}{toolName ? ' · ' + readable('tools.' + toolName, toolName) : ''}
      </h4>
      {showSource ? <p className="text-xs text-muted-foreground">{t('promptFilter.requests.outcome.resultSource', { id: receipt.from_request_id })}</p> : null}
      {instruction && !shownInContext ? <RequestContextDetails part={instruction} /> : null}
      {shownInContext ? <p className="text-xs text-muted-foreground">{t('promptFilter.requests.roundContext.bodyInContext')}</p> : null}
      {!instruction && receipt.text ? <ReadableText text={formatPromptRequestChange(receipt.text, 'tool_result', readable, toolName)} /> : null}
      <RequestImages id={receipt.from_request_id} images={receipt.images ?? []} title={t('promptFilter.requests.outcome.resultImages')} />
    </div>
  )
}

function CompleteRequest({ detail, omitContext }: { detail: PromptRequestDetail; omitContext?: PromptRequestContextSection[] }) {
  const { t } = useTranslation()
  const readable = (key: string, fallback?: string) => t(`promptFilter.requests.readable.${key}`, { defaultValue: fallback ?? key })
  const all = formatPromptRequestSections(detail.text, readable)
  const sections = omitContext?.length ? all.filter((part) => !promptRequestContextCoversPart(part, omitContext)) : all
  return (
    <div className="min-w-0 space-y-4">
      <section className="min-w-0 space-y-2">
        {omitContext?.length ? null : <h3 className="text-sm font-semibold">{t('promptFilter.requests.text')}</h3>}
        {sections.length ? sections.map((part, index) => part.context
          ? <RequestContextDetails key={index} part={part} />
          : <pre key={index} className="max-h-[45vh] overflow-auto whitespace-pre-wrap break-words rounded-lg border bg-muted/30 p-4 text-xs leading-relaxed">
            {part.text}
          </pre>
        ) : <p className="rounded-lg border bg-muted/30 p-4 text-xs text-muted-foreground">
          {t(all.length ? 'promptFilter.requests.roundContext.bodyInContext' : 'promptFilter.requests.emptyBody')}
        </p>}
      </section>
      <RequestImages id={detail.id} images={detail.images} title={t('promptFilter.requests.images')} />
    </div>
  )
}

function RequestImages({ id, images, title }: { id: number; images: PromptRequestImage[]; title: string }) {
  if (images.length === 0) return null
  return (
    <section>
      <h3 className="mb-2 text-sm font-semibold">{title}</h3>
      <div className="grid gap-3 sm:grid-cols-2">
        {images.map((item) => <RequestImage key={item.index} id={id} image={item} />)}
      </div>
    </section>
  )
}

function RequestImage({ id, image, inline = false }: { id: number; image: PromptRequestImage; inline?: boolean }) {
  const { t } = useTranslation()
  const container = useRef<HTMLElement>(null)
  const [visible, setVisible] = useState(false)
  const [source, setSource] = useState('')
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    if (image.source !== 'embedded' || !container.current) return
    if (typeof IntersectionObserver === 'undefined') {
      setVisible(true)
      return
    }
    const observer = new IntersectionObserver(([entry]) => {
      if (entry?.isIntersecting) {
        setVisible(true)
        observer.disconnect()
      }
    })
    observer.observe(container.current)
    return () => observer.disconnect()
  }, [image.source])

  useEffect(() => {
    if (!visible || image.source === 'reference') return
    const controller = new AbortController()
    let objectURL = ''
    void api.getPromptRequestImage(id, image.index, controller.signal)
      .then((blob) => {
        if (!controller.signal.aborted) {
          objectURL = URL.createObjectURL(blob)
          setSource(objectURL)
        }
      })
      .catch(() => { if (!controller.signal.aborted) setFailed(true) })
    return () => {
      controller.abort()
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
  }, [id, image.index, image.source, visible])

  const label = t('promptFilter.requests.imageNumber', { number: image.index + 1 })
  const previewLink = image.source === 'url' && !visible && image.reference ? (
    <Button variant="outline" size="sm" onClick={() => setVisible(true)}>
      {t('promptFilter.requests.previewUrl')}
    </Button>
  ) : null

  if (inline) {
    return (
      <figure ref={container} className="min-w-0 max-w-full">
        {source && !failed ? (
          <Dialog>
            <DialogTrigger asChild>
              <button type="button" className="flex min-h-10 min-w-10 max-w-full cursor-zoom-in items-center justify-center overflow-hidden rounded-md focus-visible:outline-2 focus-visible:outline-ring">
                <img src={source} alt={label} className="max-h-48 max-w-full object-contain sm:max-h-56"
                  referrerPolicy="no-referrer" onError={() => setFailed(true)} />
              </button>
            </DialogTrigger>
            <DialogContent aria-describedby={undefined} showCloseButton={false}
              className="block max-h-none w-fit max-w-none gap-0 overflow-visible rounded-none border-0 bg-transparent p-0 shadow-none sm:max-w-none sm:p-0">
              <DialogTitle className="sr-only">{label}</DialogTitle>
              <img src={source} alt={label} draggable={false} referrerPolicy="no-referrer"
                className="block h-auto max-h-[calc(100dvh-1.5rem)] w-auto max-w-[calc(100vw-1.5rem)] object-contain" />
            </DialogContent>
          </Dialog>
        ) : null}
        {previewLink}
        {(visible || image.source === 'embedded') && !source && !failed ? (
          <p className="min-h-8 min-w-24 text-xs text-muted-foreground">{t('common.loading')}</p>
        ) : null}
        {failed || image.source === 'reference' || (image.source === 'url' && !image.reference) ? (
          <p className="text-xs text-muted-foreground">{label} · {t('promptFilter.requests.unavailable')}</p>
        ) : null}
      </figure>
    )
  }

  return (
    <figure ref={container} className="min-w-0 space-y-2 rounded-lg border p-3">
      <figcaption className="text-sm font-medium">{label}</figcaption>
      <p className="break-all text-xs text-muted-foreground">{image.label}</p>
      {source && !failed ? (
        <img src={source} alt={label} className="max-h-80 w-full rounded-md bg-muted/30 object-contain"
          referrerPolicy="no-referrer" onError={() => setFailed(true)} />
      ) : null}
      {image.reference ? <p className="break-all text-xs text-muted-foreground">{image.reference}</p> : null}
      {previewLink}
      {visible && !source && !failed ? <p className="text-xs text-muted-foreground">{t('common.loading')}</p> : null}
      {failed || image.source === 'reference' ? (
        <p className="text-xs text-muted-foreground">{t('promptFilter.requests.unavailable')}</p>
      ) : null}
    </figure>
  )
}
