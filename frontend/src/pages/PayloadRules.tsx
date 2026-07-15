import type { ChangeEvent } from 'react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { RefreshCw, Save } from 'lucide-react'

import { api } from '@/api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import type { ObservedInstructionsSample } from '../types'

export const PAYLOAD_RULE_GROUPS = ['default', 'default_raw', 'override', 'override_raw', 'append', 'filter'] as const

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

const PAYLOAD_RULES_PLACEHOLDER = `{
  "append":   [{"params": {"instructions": "追加到系统提示词末尾的文本"}}],
  "override": [{"models": ["gpt-*"], "params": {"service_tier": "priority"}},
               {"match": {"reasoning.effort": "medium"}, "params": {"reasoning.effort": "high"}}],
  "filter":   [{"params": ["metadata.debug"]}]
}`

function prettifyRulesJSON(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw || '{}'), null, 2)
  } catch {
    return raw
  }
}

export default function PayloadRules() {
  const { t } = useTranslation()
  const { showToast } = useToast()

  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saved, setSaved] = useState('{}')
  const [draft, setDraft] = useState('{}')
  const [saving, setSaving] = useState(false)

  const [samples, setSamples] = useState<ObservedInstructionsSample[]>([])
  const [samplesLoading, setSamplesLoading] = useState(false)
  const [expandedSample, setExpandedSample] = useState<number | null>(null)

  const dirty = draft !== saved
  const ruleCount = useMemo(() => countPayloadRules(draft), [draft])

  const jsonError = useMemo(() => {
    const trimmed = draft.trim()
    if (!trimmed) return null
    try {
      JSON.parse(trimmed)
      return null
    } catch (e) {
      return e instanceof Error ? e.message : String(e)
    }
  }, [draft])

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
      const pretty = prettifyRulesJSON(settings.payload_rules || '{}')
      setSaved(pretty)
      setDraft(pretty)
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

  const save = useCallback(async () => {
    if (jsonError) return
    setSaving(true)
    try {
      const updated = await api.updateSettings({ payload_rules: draft.trim() || '{}' })
      const pretty = prettifyRulesJSON(updated.payload_rules || '{}')
      setSaved(pretty)
      setDraft(pretty)
      showToast(t('payloadRules.saved'), 'success')
    } catch (error) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      setSaving(false)
    }
  }, [draft, jsonError, showToast, t])

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
            onClick={() => void save()}
          >
            <Save className="size-3.5" />
            {saving ? t('common.saving') : t('common.save')}
          </Button>
        }
      />

      <StateShell
        variant="page"
        loading={loading}
        error={loadError}
        onRetry={() => void load()}
      >
        <div className="space-y-4">
          <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs leading-relaxed text-amber-700 dark:text-amber-300">
            {t('settings2.payloadRulesWarning')}
          </div>

          <div className="rounded-xl border border-border bg-card/85 p-4 shadow-sm">
            <div className="mb-2 flex items-center justify-between gap-2">
              <div className="text-sm font-semibold text-foreground">{t('payloadRules.editorTitle')}</div>
              <div className="flex items-center gap-2">
                {dirty ? <Badge variant="outline" className="text-[11px]">{t('payloadRules.unsaved')}</Badge> : null}
                <Badge variant="secondary" className="text-[11px]">
                  {t('settings.nav.mappingCount', { count: ruleCount })}
                </Badge>
              </div>
            </div>
            <textarea
              rows={18}
              value={draft}
              spellCheck={false}
              placeholder={PAYLOAD_RULES_PLACEHOLDER}
              onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setDraft(e.target.value)}
              className="flex w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs leading-relaxed text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
            />
            {jsonError ? (
              <p className="mt-1.5 text-xs text-destructive">{t('settings2.payloadRulesJsonError')}: {jsonError}</p>
            ) : (
              <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">{t('settings2.payloadRulesHint')}</p>
            )}
          </div>

          <div className="rounded-xl border border-border bg-card/85 p-4 shadow-sm">
            <div className="mb-1 flex items-center justify-between gap-2">
              <div className="text-sm font-semibold text-foreground">{t('settings2.payloadRulesObserved')}</div>
              <Button size="sm" variant="outline" className="gap-1.5" onClick={() => void loadSamples()} disabled={samplesLoading}>
                <RefreshCw className={samplesLoading ? 'size-3.5 animate-spin' : 'size-3.5'} />
                {t('settings2.payloadRulesObservedLoad')}
              </Button>
            </div>
            <p className="mb-3 text-xs leading-relaxed text-muted-foreground">{t('settings2.payloadRulesObservedDesc')}</p>
            {samples.length === 0 ? (
              <p className="text-xs text-muted-foreground">{t('settings2.payloadRulesObservedEmpty')}</p>
            ) : (
              <div className="space-y-2">
                {samples.map((sample, i) => (
                  <div key={i} className="rounded-md border border-border bg-background p-2.5">
                    <button
                      type="button"
                      className="flex w-full items-center justify-between gap-2 text-left"
                      onClick={() => setExpandedSample(expandedSample === i ? null : i)}
                    >
                      <span className="min-w-0 truncate font-mono text-xs font-semibold text-foreground">
                        {sample.model || '-'}
                        <span className="ml-2 font-normal text-muted-foreground">{sample.originator}</span>
                      </span>
                      <span className="shrink-0 text-[11px] text-muted-foreground">
                        {sample.length.toLocaleString()} chars{sample.truncated ? ' (truncated)' : ''}
                      </span>
                    </button>
                    {expandedSample === i ? (
                      <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words rounded bg-muted/40 p-2 text-[11px] leading-relaxed text-foreground">
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
    </div>
  )
}
