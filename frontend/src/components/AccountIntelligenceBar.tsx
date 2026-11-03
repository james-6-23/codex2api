import { useEffect, useState } from 'react'
import { History, LoaderCircle, Play } from 'lucide-react'
import { api } from '../api'
import { refreshIntelligenceBars } from '../hooks/useIntelligenceBars'
import { useToast } from '../hooks/useToast'
import type { IntelligenceBar } from '../lib/intelligenceCheck'
import { intelligenceColor, intelligenceLabels, intelligenceWindow, intelligenceScheduleLabel } from '../lib/intelligenceCheck'
import { getErrorMessage } from '../utils/error'
import AccountStatusBar from './AccountStatusBar'
import AccountIntelligenceHistoryDialog from './AccountIntelligenceHistoryDialog'
import IntelligenceRunDialog from './IntelligenceRunDialog'
import { Button } from './ui/button'

type AccountIntelligenceProps = {
  bar?: IntelligenceBar
  accountID: number
  accountName?: string
  error?: boolean
}

export function AccountIntelligenceActions({ bar, accountID, accountName }: AccountIntelligenceProps) {
  const { showToast } = useToast()
  const [history, setHistory] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [pendingID, setPendingID] = useState<number | null>(null)
  useEffect(() => { setPendingID(null) }, [accountID])
  useEffect(() => {
    if (pendingID && (bar?.active_run_id === pendingID || bar?.records.some(r => r.id === pendingID))) setPendingID(null)
  }, [bar, pendingID])
  const pending = pendingID !== null && !bar?.records.some(r => r.id === pendingID)
  const active = submitting || !!bar?.active_run_id || pending
  const run = async () => {
    if (active) return
    setSubmitting(true)
    try {
      const result = await api.createIntelligenceRuns([accountID])
      setPendingID(result.runs.find(r => r.account_id === accountID && (r.status === 'queued' || r.status === 'running'))?.id ?? null)
      refreshIntelligenceBars()
      showToast('检测任务已提交')
    } catch (e) { showToast(getErrorMessage(e), 'error') }
    finally { setSubmitting(false) }
  }
  return <div role="group" aria-label="检测操作" className="inline-flex shrink-0 items-center gap-0.5" onClick={event => event.stopPropagation()}>
    <Button variant="ghost" size="icon-xs" className="size-4.5 rounded-sm text-muted-foreground hover:text-foreground" aria-label={active ? '检测中' : '立即检测'} title={active ? '检测中' : '立即检测'} disabled={active} aria-busy={active} onClick={() => void run()}>
      {active ? <LoaderCircle className="size-3 animate-spin" /> : <Play className="size-3" />}
    </Button>
    <Button variant="ghost" size="icon-xs" className="size-4.5 rounded-sm text-muted-foreground hover:text-foreground" aria-label="检测记录" title="检测记录" onClick={() => setHistory(true)}><History className="size-3" /></Button>
    {history && <AccountIntelligenceHistoryDialog accountID={accountID} accountName={accountName} onClose={() => setHistory(false)} />}
  </div>
}

export default function AccountIntelligenceBar({ bar, error }: AccountIntelligenceProps) {
  const [runID, setRunID] = useState<number | null>(null)
  if (!bar?.enabled) return null
  const { records, rate } = intelligenceWindow(bar?.records ?? [])
  const rateClass = error || rate === null ? 'bg-muted text-muted-foreground'
    : rate >= 90 ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400'
      : 'bg-red-100 text-red-600 dark:bg-red-950 dark:text-red-400'
  return <div className="mt-1.5 min-w-28" onClick={event => event.stopPropagation()}>
    {(error || (bar?.action && bar.action !== 'none')) && <div className="mb-1 flex items-center gap-1">
      {error && <span role="status" className="text-[10px] text-destructive">加载失败</span>}
      {bar?.action && bar.action !== 'none' && <span className="text-[10px] text-muted-foreground">{intelligenceLabels[bar.action]}</span>}
    </div>}
    <p className="mb-1 text-[10px] text-muted-foreground">{intelligenceScheduleLabel(bar)}</p>
    <AccountStatusBar label="最近 20 次降智检测，由旧到新" rate={error ? null : rate} rateClass={rateClass}
      blocks={[
        ...Array.from({ length: 20 - records.length }, () => ({ tooltip: <span className="text-muted-foreground">暂无检测记录</span> })),
        ...records.map(record => {
          const time = new Date(record.completed_at || record.created_at || 0).toLocaleString()
          const result = intelligenceLabels[record.status]
          const duration = `${((record.duration_ms ?? 0) / 1000).toFixed(2)} 秒`
          return { color: intelligenceColor(record.status), label: `${time} · ${record.question_name || '检测'} · ${result} · ${duration}`, onClick: () => setRunID(record.id),
            tooltip: <><span className="mb-0.5 block text-muted-foreground">{time}</span><span className="flex items-center gap-2"><span style={{ color: intelligenceColor(record.status) }}>{record.question_name || '检测'} · {result}</span><span className="text-muted-foreground">{duration}</span></span></> }
        }),
      ]} />
    {runID && <IntelligenceRunDialog id={runID} onClose={() => setRunID(null)} />}
  </div>
}
