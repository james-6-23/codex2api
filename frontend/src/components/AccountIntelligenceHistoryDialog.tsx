import { useEffect, useRef, useState } from 'react'
import { api } from '../api'
import { refreshIntelligenceBars } from '../hooks/useIntelligenceBars'
import { intelligenceColor, intelligenceLabels, intelligenceScheduleLabel, type IntelligenceBar, type IntelligenceRun, type IntelligencePage } from '../lib/intelligenceCheck'
import { getErrorMessage } from '../utils/error'
import { Button } from './ui/button'
import { Switch } from './ui/switch'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from './ui/dialog'

function verdict(run: IntelligenceRun) {
  if (run.status === 'queued' || run.status === 'running') return intelligenceLabels[run.status]
  if (run.status === 'error') return '请求未成功，未参与答案与耗时判定'
  if (run.status === 'interrupted') return '检测已中断，未参与判定'
  const checks = []
  if (run.judge_mode !== 'latency') checks.push(run.answer_passed ? '答案匹配' : '答案不匹配')
  if (run.judge_mode !== 'answer') checks.push(run.latency_passed ? '耗时达标' : '耗时超出阈值')
  return checks.join('；')
}

export default function AccountIntelligenceHistoryDialog({ accountID, accountName, onClose }: {
  accountID: number | null
  accountName?: string
  onClose: () => void
}) {
  const [page, setPage] = useState(1)
  const [data, setData] = useState<IntelligencePage<IntelligenceRun> | null>(null)
  const [enabled, setEnabled] = useState<boolean | null>(null)
  const [error, setError] = useState('')
  const [schedule, setSchedule] = useState<IntelligenceBar | null>(null)
  const [saving, setSaving] = useState(false)
  const changing = useRef(false)
  const [refresh, setRefresh] = useState(0)
  useEffect(() => { setPage(1); setEnabled(null); setSchedule(null) }, [accountID])
  useEffect(() => {
    setData(null)
    setError('')
    if (!accountID) return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    const load = async () => {
      if (document.hidden) { timer = setTimeout(load, 4000); return }
      try {
        const [runs, bars] = await Promise.all([
          api.getIntelligenceRuns(page, { account_id: accountID }, controller.signal),
          api.getIntelligenceBars([accountID], controller.signal),
        ])
        if (controller.signal.aborted) return
        setData(runs)
        setSchedule(bars.bars[String(accountID)] ?? null)
        if (!changing.current) setEnabled(bars.bars[String(accountID)]?.enabled ?? false)
        setError('')
      } catch (e) {
        if (!controller.signal.aborted) setError(getErrorMessage(e))
      } finally {
        if (!controller.signal.aborted) timer = setTimeout(load, 4000)
      }
    }
    void load()
    return () => { controller.abort(); clearTimeout(timer) }
  }, [accountID, page, refresh])
  const toggle = async (value: boolean) => {
    if (!accountID) return
    changing.current = true
    setSaving(true)
    try {
      await api.setIntelligenceAccounts([accountID], value)
      setEnabled(value)
      setError('')
      refreshIntelligenceBars()
    } catch (e) { setError(getErrorMessage(e)) }
    finally { changing.current = false; setSaving(false) }
  }
  return <Dialog open={!!accountID} onOpenChange={open => { if (!open) onClose() }}>
    <DialogContent className="sm:max-w-3xl">
      <DialogHeader>
        <DialogTitle className="break-all pr-6">降智检测记录 · {accountName || data?.items[0]?.account_name || `#${accountID}`}</DialogTitle>
        <DialogDescription>最近记录在上；请求错误不计入通过率。</DialogDescription>
      </DialogHeader>
      <div className="flex flex-wrap items-center justify-between gap-3 border-b pb-4 text-sm">
        <label className="flex items-center gap-2">此账号自动检测
          <Switch aria-label="此账号自动检测" checked={enabled ?? false} disabled={enabled === null || saving} onCheckedChange={v => void toggle(v)} />
        </label>
        <Button variant="outline" size="sm" onClick={() => setRefresh(n => n + 1)}>刷新记录</Button>
      </div>
      {schedule && <p className="text-sm text-muted-foreground">{intelligenceScheduleLabel(schedule)}</p>}
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {!data && !error && <p className="py-8 text-center text-sm text-muted-foreground">正在加载…</p>}
      {data?.items.length === 0 && <p className="py-8 text-center text-sm text-muted-foreground">暂无检测记录，可在账号状态条上方点击立即检测。</p>}
      <div className="min-w-0 space-y-4">
        {data?.items.slice().sort((a, b) => b.id - a.id).map(run => <article key={run.id} className="min-w-0 space-y-3 rounded-lg border bg-card p-4 text-sm">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="inline-flex items-center gap-1.5 font-medium"><span className="size-2 rounded-full bg-muted" style={{ backgroundColor: intelligenceColor(run.status) }} />{intelligenceLabels[run.status]}</span>
            <span className="text-xs text-muted-foreground">{run.source === 'manual' ? '手动' : '自动'}</span>
            <time className="text-xs text-muted-foreground">{new Date(run.created_at || 0).toLocaleString()}</time>
            <span className="ml-auto text-xs tabular-nums text-muted-foreground">#{run.id}</span>
          </div>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
            <span className="break-all">题目：{run.question.name}</span><span className="break-all">模型：{run.model}{run.reasoning_effort ? ` · ${run.reasoning_effort}` : ''}</span>
            <span>耗时：{((run.duration_ms ?? 0) / 1000).toFixed(2)} 秒 / 阈值 {(run.question.max_duration_ms / 1000).toFixed(2)} 秒</span>
          </div>
          <p className="break-words">判定原因：{verdict(run)}</p>
          <p className="break-words">预期答案：<code>{run.question.expected_answer}</code></p>
          <div><p className="mb-1 text-xs text-muted-foreground">实际输出</p><pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-3 font-sans text-sm [overflow-wrap:anywhere]">{run.output || '暂无输出'}</pre></div>
          {run.error && <div><p className="mb-1 text-xs text-muted-foreground">错误</p><pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-3 font-sans text-sm text-red-800 dark:text-red-400 [overflow-wrap:anywhere]">{run.error}</pre></div>}
          {run.action_message && <p className="break-words text-xs text-muted-foreground">{run.action_message}</p>}
        </article>)}
      </div>
      {data && <div className="flex items-center justify-between gap-2 border-t pt-4 text-xs text-muted-foreground">
        <span>共 {data.total} 条 · 第 {page} 页</span>
        <div className="flex gap-2"><Button size="sm" variant="outline" disabled={page <= 1} onClick={() => setPage(p => p - 1)}>上一页</Button><Button size="sm" variant="outline" disabled={page * data.page_size >= data.total} onClick={() => setPage(p => p + 1)}>下一页</Button></div>
      </div>}
    </DialogContent>
  </Dialog>
}
