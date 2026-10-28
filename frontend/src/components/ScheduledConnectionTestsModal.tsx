import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CalendarClock, Loader2, Plus, Save, X } from 'lucide-react'
import { api } from '../api'
import type {
  AccountRow,
  ScheduledConnectionTestConfig,
  ScheduledConnectionTestRecord,
} from '../types'
import { formatAccountName } from '../lib/connectionTestModels'
import { useToast } from '../hooks/useToast'
import Modal from './Modal'
import { Button } from './ui/button'
import { Select } from './ui/select'
import { Switch } from './ui/switch'

export default function ScheduledConnectionTestsModal({
  account,
  models,
  defaultModel,
  defaultContent,
  onClose,
}: {
  account: AccountRow
  models: string[]
  defaultModel: string
  defaultContent: string
  onClose: () => void
}) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [config, setConfig] = useState<ScheduledConnectionTestConfig>({
    enabled: false,
    model: defaultModel,
    content: defaultContent,
    weekday_times: [],
    weekend_times: [],
  })
  const [records, setRecords] = useState<ScheduledConnectionTestRecord[]>([])
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [loaded, setLoaded] = useState(false)
  const [draftTimes, setDraftTimes] = useState({
    weekday_times: '09:00',
    weekend_times: '09:00',
  })
  const [dirty, setDirty] = useState(false)

  useEffect(() => {
    let active = true
    void api
      .getScheduledConnectionTests(account.id)
      .then((result) => {
        if (!active) return
        setConfig({
          ...result.config,
          model: result.config.model || defaultModel,
          content: result.config.model ? result.config.content : defaultContent,
          weekday_times: result.config.weekday_times || [],
          weekend_times: result.config.weekend_times || [],
        })
        setRecords(result.records)
        setLoaded(true)
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        if (active) setLoading(false)
      })
    return () => {
      active = false
    }
  }, [account.id, defaultModel, defaultContent])

  const update = (patch: Partial<ScheduledConnectionTestConfig>) => {
    setConfig((previous) => ({ ...previous, ...patch }))
    setDirty(true)
  }
  const addTime = (key: 'weekday_times' | 'weekend_times') => {
    const value = draftTimes[key]
    if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(value) || config[key].includes(value))
      return
    update({ [key]: [...config[key], value].sort() })
  }
  const deleteTime = (
    key: 'weekday_times' | 'weekend_times',
    value: string,
  ) => {
    const next = config[key].filter((item) => item !== value)
    const other =
      key === 'weekday_times' ? config.weekend_times : config.weekday_times
    update({
      [key]: next,
      ...(next.length + other.length === 0 ? { enabled: false } : {}),
    })
  }
  const save = async () => {
    if (saving || !loaded) return
    setSaving(true)
    setError('')
    try {
      const saved = await api.updateScheduledConnectionTests(account.id, config)
      setConfig(saved)
      setDirty(false)
      showToast(t('accounts.activationSaved'))
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }
  const blocked = loading || saving || !loaded
  const hasTimes = config.weekday_times.length + config.weekend_times.length > 0
  const modelOptions = [
    ...new Set([...models, config.model].filter(Boolean)),
  ].map((value) => ({ value, label: value }))
  const formatTime = (value?: string) =>
    value
      ? new Date(value).toLocaleString('zh-CN', {
          timeZone: 'Asia/Shanghai',
          hour12: false,
        })
      : '—'
  return (
    <Modal
      show
      title={
        <span className="flex items-center gap-3">
          <span className="rounded-xl bg-primary/10 p-2 text-primary">
            <CalendarClock className="size-5" />
          </span>
          <span>
            {t('accounts.activationTitle')}
            <span className="mt-1 block text-xs font-normal text-muted-foreground">
              {formatAccountName(account)}
            </span>
          </span>
        </span>
      }
      onClose={() => {
        if (!saving) onClose()
      }}
      contentClassName="sm:max-w-[720px]"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="text-xs text-muted-foreground">
            {dirty
              ? t('accounts.activationUnsaved')
              : t('accounts.activationSaveHint')}
          </span>
          <div className="flex gap-2">
            <Button variant="outline" onClick={onClose} disabled={saving}>
              {t('common.close')}
            </Button>
            <Button
              onClick={() => void save()}
              disabled={
                blocked ||
                !config.model ||
                !config.content.trim() ||
                (config.enabled && !hasTimes)
              }
            >
              {saving ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <Save className="size-4" />
              )}
              {t('common.save')}
            </Button>
          </div>
        </div>
      }
    >
      <div className="space-y-4">
        {loading && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            {t('common.loading')}
          </div>
        )}
        {error && (
          <div
            role="alert"
            className="rounded-xl border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
          >
            {error}
          </div>
        )}
        <section className="space-y-4 rounded-2xl border bg-muted/20 p-4">
          <div className="flex items-center justify-between gap-4">
            <div>
              <label
                htmlFor="scheduled-test-enabled"
                className="text-sm font-semibold"
              >
                {t('accounts.activationEnabled')}
              </label>
              <p className="mt-1 text-xs text-muted-foreground">
                {t('accounts.activationTimezone')}
              </p>
            </div>
            <Switch
              id="scheduled-test-enabled"
              checked={config.enabled}
              onCheckedChange={(enabled) => update({ enabled })}
              disabled={blocked}
            />
          </div>
          <div className="space-y-2">
            <label
              className="text-sm font-medium"
              id="scheduled-test-model-label"
            >
              {t('settings.testModel')}
            </label>
            <Select
              value={config.model}
              options={modelOptions}
              onValueChange={(model) => update({ model })}
              aria-label={t('settings.testModel')}
              disabled={blocked}
            />
          </div>
          <div className="space-y-2">
            <label
              htmlFor="scheduled-test-content"
              className="text-sm font-medium"
            >
              {t('settings.testContent')}
            </label>
            <textarea
              id="scheduled-test-content"
              rows={3}
              maxLength={8192}
              className="w-full resize-y rounded-xl border border-input bg-background p-3 text-sm focus:outline-none focus:ring-2 focus:ring-ring disabled:opacity-50"
              value={config.content}
              onChange={(event) => update({ content: event.target.value })}
              disabled={blocked}
            />
          </div>
        </section>
        <div className="grid gap-3 sm:grid-cols-2">
          {(['weekday_times', 'weekend_times'] as const).map((key) => (
            <section key={key} className="space-y-3 rounded-2xl border p-4">
              <h3 className="text-sm font-semibold">
                {t(
                  key === 'weekday_times'
                    ? 'accounts.activationWeekdays'
                    : 'accounts.activationWeekends',
                )}
              </h3>
              <div className="flex items-center gap-2">
                <input
                  type="time"
                  step="60"
                  aria-label={t(
                    key === 'weekday_times'
                      ? 'accounts.activationWeekdays'
                      : 'accounts.activationWeekends',
                  )}
                  className="min-w-0 flex-1 rounded-lg border border-input bg-background px-3 py-2 text-sm"
                  value={draftTimes[key]}
                  onChange={(event) =>
                    setDraftTimes((previous) => ({
                      ...previous,
                      [key]: event.target.value,
                    }))
                  }
                  disabled={blocked}
                />
                <Button
                  variant="outline"
                  size="sm"
                  disabled={
                    blocked ||
                    !draftTimes[key] ||
                    config[key].includes(draftTimes[key])
                  }
                  onClick={() => addTime(key)}
                >
                  <Plus className="size-3.5" />
                  {t('accounts.activationAdd')}
                </Button>
              </div>
              <div className="flex min-h-9 flex-wrap items-start gap-2">
                {config[key].length ? (
                  config[key].map((value) => (
                    <span
                      key={value}
                      className="inline-flex items-center gap-2 rounded-lg border bg-muted/40 px-2.5 py-1 text-sm tabular-nums"
                    >
                      {value}
                      <button
                        type="button"
                        className="rounded text-muted-foreground hover:text-destructive focus-visible:outline-ring disabled:opacity-50"
                        aria-label={t('accounts.activationRemove', {
                          time: value,
                        })}
                        disabled={blocked}
                        onClick={() => deleteTime(key, value)}
                      >
                        <X className="size-3.5" />
                      </button>
                    </span>
                  ))
                ) : (
                  <span className="text-xs text-muted-foreground">
                    {t('accounts.activationNoTimes')}
                  </span>
                )}
              </div>
            </section>
          ))}
        </div>
        <p className="rounded-xl bg-primary/5 p-3 text-xs leading-relaxed text-muted-foreground">
          {t('accounts.activationExecutionHint')}
        </p>
        <section className="space-y-2">
          <h3 className="text-sm font-semibold">
            {t('accounts.activationRecords')}
          </h3>
          {records.length === 0 ? (
            <p className="rounded-xl border border-dashed p-4 text-center text-xs text-muted-foreground">
              {t('accounts.activationNoRecords')}
            </p>
          ) : (
            <div className="max-h-52 space-y-2 overflow-y-auto">
              {records.map((record) => (
                <div
                  key={record.scheduled_at}
                  className="rounded-xl border p-3 text-xs"
                >
                  <div className="flex justify-between gap-2">
                    <span className="tabular-nums">
                      {formatTime(record.attempted_at || record.scheduled_at)}
                    </span>
                    <span
                      className={
                        record.success
                          ? 'text-emerald-500'
                          : 'text-muted-foreground'
                      }
                    >
                      {t(`accounts.activationStatus_${record.status}`)}
                    </span>
                  </div>
                  <p className="mt-1 text-muted-foreground">
                    {record.model || config.model} ·{' '}
                    {t('accounts.activationReset')}:{' '}
                    {formatTime(record.next_reset_5h_at)}
                  </p>
                  {record.error && (
                    <p className="mt-1 break-words text-destructive">
                      {record.error}
                    </p>
                  )}
                </div>
              ))}
            </div>
          )}
        </section>
      </div>
    </Modal>
  )
}
