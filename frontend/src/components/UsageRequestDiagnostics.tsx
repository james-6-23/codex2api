import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import type { UsageLog } from '../types'
import { diagnosticClientInfo, diagnosticEntries, diagnosticJSONDisplay, diagnosticRecord, diagnosticValueText, splitOutboundIdentityDiagnostic, usageRequestTypeLabelKey, type UsageRequestDiagnosticDetail } from '../lib/usageRequestDiagnostics'
import { useToast } from '../hooks/useToast'
import Modal from './Modal'
import { Button } from './ui/button'

export function UsageRequestTypeButton({ log, onClick }: { log: UsageLog; onClick: () => void }) {
  const { t } = useTranslation()
  return <button type="button" onClick={onClick} title={t('usage.diagnostics.open')}
    className="rounded-md border border-border bg-muted/40 px-2 py-0.5 text-[11px] font-medium whitespace-nowrap hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
    {t(usageRequestTypeLabelKey(log.request_type))}
  </button>
}

function DiagnosticFields({ value, includeMissing = false }: { value: unknown; includeMissing?: boolean }) {
  const { t } = useTranslation()
  const entries = diagnosticEntries(value, includeMissing)
  if (!entries.length) return <p className="text-xs text-muted-foreground">{t('usage.diagnostics.missing')}</p>
  return <dl className="grid gap-x-4 gap-y-2 text-xs sm:grid-cols-[minmax(0,220px)_minmax(0,1fr)]">
    {entries.map(([key, item]) => {
      const text = diagnosticValueText(item)
      return <div key={key} className="contents">
        <dt className="break-all font-mono text-muted-foreground">{key}</dt>
        <dd className="break-all font-mono select-text">{text === null ? <span className="text-muted-foreground">{t('usage.diagnostics.missing')}</span> : text}</dd>
      </div>
    })}
  </dl>
}

export default function UsageRequestDiagnostics({ log, onClose }: { log: UsageLog | null; onClose: () => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [detail, setDetail] = useState<UsageRequestDiagnosticDetail | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [reload, setReload] = useState(0)
  const [decodeMetadata, setDecodeMetadata] = useState(false)
  const id = log?.id

  useEffect(() => {
    setDetail(null)
    setError('')
    if (!id) return
    const controller = new AbortController()
    setLoading(true)
    api.getUsageRequestDiagnostics(id, controller.signal).then((result) => {
      if (!controller.signal.aborted) setDetail(result)
    }).catch((reason: unknown) => {
      if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : String(reason))
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false)
    })
    return () => controller.abort()
  }, [id, reload])

  const data = detail?.diagnostics
  const { outbound_identity: outboundIdentity, ...upstream } = diagnosticRecord(data?.upstream)
  const accountMapping = diagnosticRecord(diagnosticRecord(outboundIdentity).account_mapping)
  const outbound = splitOutboundIdentityDiagnostic(outboundIdentity)
  const sections: [string, unknown][] = data ? [
    ['request', { request_type: detail.request_type, ...diagnosticRecord(data.request), started_at: data.started_at, completed_at: data.completed_at, correlation_id: data.correlation_id, newapi_request_id: data.newapi_request_id, attempt: data.attempt, capture_status: data.capture_status, responses_input: data.responses_input }],
    ['client', diagnosticClientInfo(data.incoming)],
    ['upstream', upstream],
    ['outbound', outbound.snapshot],
    ['outboundDiagnostics', outbound.local],
    ['resolved', data.resolved],
    ['continuity', { ...diagnosticRecord(data.session_continuity), account_failover: data.account_failover ?? diagnosticRecord(data.session_continuity).account_failover }],
    ['audit', data.audit],
    ['dispatch', data.dispatch],
    ['windows', { user_window: data.user_window, account_window: data.account_window, user_window_key_hash: data.user_window_key_hash }],
    ['routing', { root_account_lookup: data.root_account_lookup, root_account_id: data.root_account_id, selected_account_id: data.selected_account_id, selection: data.selection, candidate_rejections: data.candidate_rejections, background_account_match: data.background_account_match }],
    ['recent', data.recent_account],
  ] : []

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(JSON.stringify(diagnosticJSONDisplay(detail, decodeMetadata), null, 2))
      showToast(t('usage.diagnostics.copied'))
    } catch {
      showToast(t('usage.diagnostics.copyFailed'), 'error')
    }
  }

  return <Modal show={log !== null} onClose={onClose} title={`${t('usage.diagnostics.title')} #${id ?? ''}`}
    contentClassName="sm:max-w-[820px]" footer={<Button size="sm" disabled={!data || loading} onClick={() => void copy()}>{t('usage.diagnostics.copy')}</Button>}>
    <p className="mb-4 text-xs leading-5 text-muted-foreground">{t('usage.diagnostics.hint')}</p>
    <label className="mb-4 flex items-center gap-2 text-xs text-muted-foreground">
      <input type="checkbox" checked={decodeMetadata} onChange={(event) => setDecodeMetadata(event.target.checked)} />
      {t('usage.diagnostics.decodeMetadata')}
    </label>
    {loading ? <p role="status" className="text-sm text-muted-foreground">{t('common.loading')}</p> : error ? <div role="alert" className="space-y-3 text-sm text-destructive">
      <p>{error}</p><Button variant="outline" size="sm" onClick={() => setReload((value) => value + 1)}>{t('usage.diagnostics.retry')}</Button>
    </div> : !data ? <p className="text-sm text-muted-foreground">{t('usage.diagnostics.unavailable')}</p> : <div className="space-y-4">
      {data.classification_changed === true && <p role="alert" className="rounded-md bg-amber-500/10 p-3 text-sm text-amber-600">{t('usage.diagnostics.changed')}</p>}
      {data.truncated === true && <p className="text-xs text-amber-600">{t('usage.diagnostics.truncated')}</p>}
      {sections.map(([title, value]) => <section key={title} className="rounded-lg border p-3">
        <h3 className="mb-3 text-sm font-semibold">{title === 'continuity' ? t('sessionContinuity.title') : t(`usage.diagnostics.sections.${title}`)}</h3>
        {title === 'client' && <p className="mb-3 text-xs leading-5 text-muted-foreground">{t('usage.diagnostics.clientHint')}</p>}
        {title === 'outbound' || title === 'outboundDiagnostics' ? <>
          <p className="mb-3 text-xs leading-5 text-muted-foreground">{t(title === 'outboundDiagnostics' ? 'usage.diagnostics.outboundLocalHint' : diagnosticRecord(outboundIdentity).format_version === 2 ? 'usage.diagnostics.outboundJSONHint' : 'usage.diagnostics.outboundLegacyHint')}</p>
          {title === 'outboundDiagnostics' && String(accountMapping.status || '').startsWith('mapped') && <p className="mb-3 rounded-md bg-primary/10 p-2 text-xs">{t('usage.diagnostics.accountMapped')}</p>}
          {title === 'outboundDiagnostics' && String(accountMapping.status || '').startsWith('preserved') && <p className="mb-3 rounded-md bg-muted p-2 text-xs">{t('usage.diagnostics.accountMappingPreserved')}</p>}
          <pre className="max-h-[560px] overflow-auto rounded-md bg-muted/40 p-3 text-xs font-mono select-text" tabIndex={0}>{JSON.stringify(diagnosticJSONDisplay(value ?? null, decodeMetadata), null, 2)}</pre>
        </> : <DiagnosticFields value={value} />}
      </section>)}
      <section className="rounded-lg border p-3">
        <h3 className="mb-3 text-sm font-semibold">{t('usage.diagnostics.sections.incoming')}</h3>
        {[...new Set(['headers', 'turn_metadata_header', 'client_metadata', 'signed_newapi', ...Object.keys(diagnosticRecord(data.incoming))])].map((source) => <div key={source} className="mt-3 border-t pt-3 first:mt-0 first:border-0 first:pt-0">
          <h4 className="mb-2 font-mono text-xs font-semibold">{source}</h4>
          <DiagnosticFields value={diagnosticRecord(data.incoming)[source]} includeMissing={source !== 'headers'} />
        </div>)}
      </section>
    </div>}
  </Modal>
}
