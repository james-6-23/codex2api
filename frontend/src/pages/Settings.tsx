import type { ChangeEvent, ReactNode } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, resetAdminAuthState, setAdminKey } from '../api'
import { formatBeijingTime, getTimezone, setTimezone } from '../utils/time'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import ToastNotice from '../components/ToastNotice'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { HealthResponse, ModelInfo, SystemSettings } from '../types'
import { getErrorMessage } from '../utils/error'
import { DEFAULT_CLAUDE_MODEL_MAP } from '../lib/modelMapping'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
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

import { ExternalLink, RefreshCw, Save, Trash2 } from 'lucide-react'

type ModelMappingEntry = [string, string]

const getDefaultModelMappingEntries = (): ModelMappingEntry[] =>
  Object.entries(DEFAULT_CLAUDE_MODEL_MAP) as ModelMappingEntry[]

const parseModelMappingEntries = (value: string): ModelMappingEntry[] => {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return getDefaultModelMappingEntries()
    }

    const entries = Object.entries(parsed).map(([key, model]) => [
      key,
      typeof model === 'string' ? model : String(model ?? ''),
    ]) as ModelMappingEntry[]

    // 如果数据库中为空，用默认值填充
    return entries.length > 0 ? entries : getDefaultModelMappingEntries()
  } catch {
    return getDefaultModelMappingEntries()
  }
}

const serializeModelMappingEntries = (entries: ModelMappingEntry[]) => {
  const obj: Record<string, string> = {}
  for (const [key, model] of entries) {
    const trimmedKey = key.trim()
    const trimmedModel = model.trim()
    if (trimmedKey && trimmedModel) obj[trimmedKey] = trimmedModel
  }
  return JSON.stringify(obj)
}

// 模型映射编辑器组件
function ModelMappingEditor({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const { t } = useTranslation()
  const [mappings, setMappings] = useState<ModelMappingEntry[]>(() => parseModelMappingEntries(value))
  const lastEmittedValueRef = useRef<string | null>(null)

  useEffect(() => {
    if (value === lastEmittedValueRef.current) return
    setMappings(parseModelMappingEntries(value))
  }, [value])

  const updateMappings = (entries: ModelMappingEntry[]) => {
    setMappings(entries)
    const serialized = serializeModelMappingEntries(entries)
    lastEmittedValueRef.current = serialized
    onChange(serialized)
  }

  const handleChange = (index: number, field: 0 | 1, val: string) => {
    const next = [...mappings]
    next[index] = [...next[index]] as ModelMappingEntry
    next[index][field] = val
    updateMappings(next)
  }

  const handleRemove = (index: number) => {
    const next = mappings.filter((_, i) => i !== index)
    updateMappings(next)
  }

  const handleAdd = () => {
    updateMappings([...mappings, ['', '']])
  }

  return (
    <div className="space-y-2">
      <div className="grid grid-cols-[1fr_1fr_40px] gap-2 text-xs font-semibold text-muted-foreground">
        <span>{t('settings2.anthropicModel')}</span>
        <span>{t('settings2.codexModel')}</span>
        <span />
      </div>
      {mappings.map(([k, v], i) => (
        <div key={i} className="grid grid-cols-[1fr_1fr_40px] gap-2 items-center">
          <Input
            className="font-mono text-[13px]"
            placeholder="claude-opus-4-6"
            value={k}
            onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 0, e.target.value)}
          />
          <Input
            className="font-mono text-[13px]"
            placeholder="gpt-5.4"
            value={v}
            onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 1, e.target.value)}
          />
          <button
            type="button"
            onClick={() => handleRemove(i)}
            className="flex items-center justify-center size-9 rounded-lg text-muted-foreground hover:text-red-500 hover:bg-red-50 dark:hover:bg-red-500/10 transition-colors"
          >
            <Trash2 className="size-4" />
          </button>
        </div>
      ))}
      <Button type="button" variant="outline" size="sm" onClick={handleAdd}>
        + {t('settings2.addMapping')}
      </Button>
    </div>
  )
}

function SettingsCard({
  title,
  description,
  children,
  className,
  footer,
}: {
  title: string
  description?: string
  children: ReactNode
  className?: string
  footer?: ReactNode
}) {
  return (
    <Card className={cn('py-0', className)}>
      <CardContent className="p-5">
        <div className="mb-4">
          <h3 className="text-base font-semibold leading-tight text-foreground">{title}</h3>
          {description ? (
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{description}</p>
          ) : null}
        </div>
        {children}
        {footer ? <div className="mt-5 border-t border-border pt-4">{footer}</div> : null}
      </CardContent>
    </Card>
  )
}

function SettingField({
  label,
  description,
  warning,
  children,
  className,
}: {
  label: string
  description?: string
  warning?: string
  children: ReactNode
  className?: string
}) {
  return (
    <div className={cn('min-w-0 space-y-2', className)}>
      <label className="block text-sm font-semibold leading-none text-foreground">{label}</label>
      {children}
      {description ? <p className="text-xs leading-relaxed text-muted-foreground">{description}</p> : null}
      {warning ? <p className="text-xs leading-relaxed text-amber-600">{warning}</p> : null}
    </div>
  )
}

function StatusTile({
  label,
  children,
}: {
  label: string
  children: ReactNode
}) {
  return (
    <div className="flex min-h-[76px] flex-col justify-between gap-2 rounded-lg border border-border bg-muted/25 p-3">
      <span className="text-[11px] font-bold uppercase text-muted-foreground">{label}</span>
      <div className="text-sm font-semibold text-foreground">{children}</div>
    </div>
  )
}

export default function Settings() {
  const { t } = useTranslation()
  const booleanOptions = [
    { label: t('common.disabled'), value: 'false' },
    { label: t('common.enabled'), value: 'true' },
  ]
  const [settingsForm, setSettingsForm] = useState<SystemSettings>({
    max_concurrency: 2,
    global_rpm: 0,
    test_model: '',
    test_concurrency: 50,
    background_refresh_interval_minutes: 2,
    usage_probe_max_age_minutes: 10,
    recovery_probe_interval_minutes: 30,
    pg_max_conns: 50,
    redis_pool_size: 30,
    auto_clean_unauthorized: false,
    auto_clean_rate_limited: false,
    auto_clean_error: false,
    auto_clean_expired: false,
    admin_secret: '',
    admin_auth_source: 'disabled',
    auto_clean_full_usage: false,
    proxy_pool_enabled: false,
    fast_scheduler_enabled: false,
    max_retries: 2,
    allow_remote_migration: false,
    database_driver: 'postgres',
    database_label: 'PostgreSQL',
    cache_driver: 'redis',
    cache_label: 'Redis',
    model_mapping: '{}',
    resin_url: '',
    resin_platform_name: '',
  })
  const [savingSettings, setSavingSettings] = useState(false)
  const [loadedAdminSecret, setLoadedAdminSecret] = useState('')
  const [modelList, setModelList] = useState<string[]>([])
  const [modelItems, setModelItems] = useState<ModelInfo[]>([])
  const [modelsLastSyncedAt, setModelsLastSyncedAt] = useState<string | undefined>()
  const [modelsSourceURL, setModelsSourceURL] = useState('')
  const [syncingModels, setSyncingModels] = useState(false)
  const { toast, showToast } = useToast()

  const loadSettingsData = useCallback(async () => {
    const [health, settings, modelsResp] = await Promise.all([api.getHealth(), api.getSettings(), api.getModels()])
    setSettingsForm(settings)
    setLoadedAdminSecret(settings.admin_secret ?? '')
    setModelList(modelsResp.models ?? [])
    setModelItems(modelsResp.items ?? [])
    setModelsLastSyncedAt(modelsResp.last_synced_at)
    setModelsSourceURL(modelsResp.source_url ?? '')
    return {
      health,
    }
  }, [])

  const { data, loading, error, reload } = useDataLoader<{
    health: HealthResponse | null
  }>({
    initialData: {
      health: null,
    },
    load: loadSettingsData,
  })

  const handleSaveSettings = async () => {
    setSavingSettings(true)
    try {
      const adminSecretChanged = settingsForm.admin_auth_source !== 'env' && settingsForm.admin_secret !== loadedAdminSecret
      const updated = await api.updateSettings(settingsForm)
      setSettingsForm(updated)
      setLoadedAdminSecret(updated.admin_secret ?? '')
      if (updated.admin_auth_source !== 'env') {
        setAdminKey(updated.admin_secret ?? '')
      }
      if (adminSecretChanged) {
        resetAdminAuthState()
        return
      }
      if (updated.expired_cleaned && updated.expired_cleaned > 0) {
        showToast(t('settings.expiredCleanedResult', { count: updated.expired_cleaned }))
      } else {
        showToast(t('settings.saveSuccess'))
      }
    } catch (error) {
      showToast(`${t('settings.saveFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSavingSettings(false)
    }
  }

  const handleSyncModels = async () => {
    setSyncingModels(true)
    try {
      const result = await api.syncModels()
      setModelList(result.models ?? [])
      setModelItems(result.items ?? [])
      setModelsLastSyncedAt(result.last_synced_at)
      setModelsSourceURL(result.source_url ?? '')
      showToast(t('settings.modelsSyncSuccess', {
        added: result.added,
        updated: result.updated,
        skipped: result.skipped?.length ?? 0,
      }))
    } catch (error) {
      showToast(`${t('settings.modelsSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingModels(false)
    }
  }

  const { health } = data
  const isExternalDatabase = settingsForm.database_driver === 'postgres'
  const isExternalCache = settingsForm.cache_driver === 'redis'
  const showConnectionPool = isExternalDatabase || isExternalCache
  const canConfigureRemoteMigration = settingsForm.admin_auth_source === 'env' || settingsForm.admin_secret.trim() !== ''
  const saveButtonLabel = savingSettings ? t('common.saving') : t('settings.saveSettings')
  const visibleModelItems = useMemo(() => {
    if (modelItems.length > 0) {
      return modelItems
    }
    return modelList.map((id) => ({
      id,
      enabled: true,
      category: id.includes('image') ? 'image' : 'codex',
      source: 'builtin',
      pro_only: id === 'gpt-5.3-codex-spark',
      api_key_auth_available: id !== 'gpt-5.5',
    }))
  }, [modelItems, modelList])
  const textModelOptions = visibleModelItems
    .filter((model) => model.enabled && model.category !== 'image' && !model.id.includes('image'))
    .map((model) => ({ label: model.id, value: model.id }))
  const enabledModelCount = visibleModelItems.filter((model) => model.enabled).length
  const modelsLastSyncedLabel = modelsLastSyncedAt ? formatBeijingTime(modelsLastSyncedAt) : t('settings.modelsNeverSynced')
  const modelsSourceLabel = modelsSourceURL || 'https://developers.openai.com/codex/models'
  const renderSaveButton = (className?: string) => (
    <Button className={className} onClick={() => void handleSaveSettings()} disabled={savingSettings}>
      <Save className="size-4" />
      {saveButtonLabel}
    </Button>
  )

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('settings.loadingTitle')}
      loadingDescription={t('settings.loadingDesc')}
      errorTitle={t('settings.errorTitle')}
    >
      <>
        <PageHeader
          title={t('settings.title')}
          description={t('settings.description')}
          actions={renderSaveButton('max-sm:w-full')}
        />

        <div className="space-y-4">
          <SettingsCard title={t('settings.systemStatus')}>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3">
              <StatusTile label={t('settings.service')}>
                <Badge variant={health?.status === 'ok' ? 'default' : 'destructive'} className="gap-1.5">
                  <span className={`size-1.5 rounded-full ${health?.status === 'ok' ? 'bg-emerald-500' : 'bg-red-400'}`} />
                  {health?.status === 'ok' ? t('common.running') : t('common.error')}
                </Badge>
              </StatusTile>
              <StatusTile label={t('settings.accountsLabel')}>
                {health?.available ?? 0} / {health?.total ?? 0}
              </StatusTile>
              <StatusTile label={settingsForm.database_label}>
                <Badge variant="default" className="gap-1.5">
                  <span className="size-1.5 rounded-full bg-emerald-500" />
                  {isExternalDatabase ? t('common.connected') : t('common.running')}
                </Badge>
              </StatusTile>
              <StatusTile label={settingsForm.cache_label}>
                <Badge variant="default" className="gap-1.5">
                  <span className="size-1.5 rounded-full bg-emerald-500" />
                  {isExternalCache ? t('common.connected') : t('common.running')}
                </Badge>
              </StatusTile>
            </div>
          </SettingsCard>

          <div className="grid gap-4 xl:grid-cols-[minmax(0,0.95fr)_minmax(360px,1.05fr)]">
            <SettingsCard title={t('settings.trafficProtection')}>
              <div className="grid grid-cols-[repeat(auto-fit,minmax(210px,1fr))] gap-4">
                <SettingField label={t('settings.maxConcurrency')} description={t('settings.maxConcurrencyRange')}>
                  <Input
                    type="number"
                    min={1}
                    max={50}
                    value={settingsForm.max_concurrency}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, max_concurrency: parseInt(e.target.value) || 1 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.globalRpm')} description={t('settings.globalRpmRange')}>
                  <Input
                    type="number"
                    min={0}
                    value={settingsForm.global_rpm}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, global_rpm: parseInt(e.target.value) || 0 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.maxRetries')} description={t('settings.maxRetriesRange')}>
                  <Input
                    type="number"
                    min={0}
                    max={10}
                    value={settingsForm.max_retries}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, max_retries: parseInt(e.target.value) || 0 }))}
                  />
                </SettingField>
              </div>
            </SettingsCard>

            <SettingsCard title={t('settings.scheduler')}>
              <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4">
                <SettingField label={t('settings.testModelLabel')} description={t('settings.testModelHint')}>
	                  <Select
	                    value={settingsForm.test_model}
	                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, test_model: value }))}
	                    options={textModelOptions}
	                  />
                </SettingField>
                <SettingField label={t('settings.testConcurrency')} description={t('settings.testConcurrencyRange')}>
                  <Input
                    type="number"
                    min={1}
                    max={200}
                    value={settingsForm.test_concurrency}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, test_concurrency: parseInt(e.target.value) || 1 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.backgroundRefreshInterval')} description={t('settings.backgroundRefreshIntervalDesc')}>
                  <Input
                    type="number"
                    min={1}
                    max={1440}
                    value={settingsForm.background_refresh_interval_minutes}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_refresh_interval_minutes: parseInt(e.target.value) || 1 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.usageProbeMaxAge')} description={t('settings.usageProbeMaxAgeDesc')}>
                  <Input
                    type="number"
                    min={1}
                    max={10080}
                    value={settingsForm.usage_probe_max_age_minutes}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, usage_probe_max_age_minutes: parseInt(e.target.value) || 1 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.recoveryProbeInterval')} description={t('settings.recoveryProbeIntervalDesc')}>
                  <Input
                    type="number"
                    min={1}
                    max={10080}
                    value={settingsForm.recovery_probe_interval_minutes}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, recovery_probe_interval_minutes: parseInt(e.target.value) || 1 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.fastSchedulerEnabled')} description={t('settings.fastSchedulerEnabledDesc')}>
                  <Select
                    value={settingsForm.fast_scheduler_enabled ? 'true' : 'false'}
                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, fast_scheduler_enabled: value === 'true' }))}
                    options={booleanOptions}
                  />
                </SettingField>
              </div>
            </SettingsCard>
          </div>

          <SettingsCard title={t('settings.autoCleanup')}>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(240px,1fr))] gap-4">
              <SettingField label={t('settings.autoCleanUnauthorized')} description={t('settings.autoCleanUnauthorizedDesc')}>
                <Select
                  value={settingsForm.auto_clean_unauthorized ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_unauthorized: value === 'true' }))}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanRateLimited')} description={t('settings.autoCleanRateLimitedDesc')}>
                <Select
                  value={settingsForm.auto_clean_rate_limited ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_rate_limited: value === 'true' }))}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanFullUsage')} description={t('settings.autoCleanFullUsageDesc')}>
                <Select
                  value={settingsForm.auto_clean_full_usage ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_full_usage: value === 'true' }))}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanError')} description={t('settings.autoCleanErrorDesc')}>
                <Select
                  value={settingsForm.auto_clean_error ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_error: value === 'true' }))}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanExpired')} description={t('settings.autoCleanExpiredDesc')}>
                <Select
                  value={settingsForm.auto_clean_expired ? 'true' : 'false'}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, auto_clean_expired: value === 'true' }))}
                  options={booleanOptions}
                />
              </SettingField>
            </div>
          </SettingsCard>

          <div className="grid gap-4 xl:grid-cols-2">
            <SettingsCard title={t('settings.security')}>
              <div className="grid grid-cols-[repeat(auto-fit,minmax(260px,1fr))] gap-4">
                <SettingField
                  label={t('settings.adminSecret')}
                  description={t('settings.adminSecretDesc')}
                  warning={settingsForm.admin_auth_source === 'env' ? t('settings.adminSecretEnvOverride') : undefined}
                >
                  <Input
                    type="text"
                    placeholder={t('settings.adminSecretPlaceholder')}
                    value={settingsForm.admin_secret}
                    disabled={settingsForm.admin_auth_source === 'env'}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => {
                      const nextSecret = e.target.value
                      return {
                        ...f,
                        admin_secret: nextSecret,
                        allow_remote_migration: nextSecret.trim() === '' ? false : f.allow_remote_migration,
                      }
                    })}
                  />
                </SettingField>
                <SettingField
                  label={t('settings.allowRemoteMigration')}
                  description={t('settings.allowRemoteMigrationDesc')}
                  warning={!canConfigureRemoteMigration ? t('settings.allowRemoteMigrationRequiresSecret') : undefined}
                >
                  <Select
                    value={settingsForm.allow_remote_migration ? 'true' : 'false'}
                    disabled={!canConfigureRemoteMigration}
                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, allow_remote_migration: value === 'true' }))}
                    options={booleanOptions}
                  />
                </SettingField>
              </div>
            </SettingsCard>

            <SettingsCard title={t('settings.display')}>
              <SettingField label={t('settings.timezone')} description={t('settings.timezoneDesc')}>
                <Select
                  value={getTimezone()}
                  onValueChange={(value) => {
                    setTimezone(value)
                    window.location.reload()
                  }}
                  options={[
                    { label: t('settings.timezoneAuto'), value: Intl.DateTimeFormat().resolvedOptions().timeZone },
                    { label: '(UTC) UTC', value: 'UTC' },
                    { label: '(GMT+08:00) Asia/Shanghai', value: 'Asia/Shanghai' },
                    { label: '(GMT+09:00) Asia/Tokyo', value: 'Asia/Tokyo' },
                    { label: '(GMT+09:00) Asia/Seoul', value: 'Asia/Seoul' },
                    { label: '(GMT+08:00) Asia/Singapore', value: 'Asia/Singapore' },
                    { label: '(GMT+08:00) Asia/Hong_Kong', value: 'Asia/Hong_Kong' },
                    { label: '(GMT+08:00) Asia/Taipei', value: 'Asia/Taipei' },
                    { label: '(GMT+07:00) Asia/Bangkok', value: 'Asia/Bangkok' },
                    { label: '(GMT+04:00) Asia/Dubai', value: 'Asia/Dubai' },
                    { label: '(GMT+05:30) Asia/Kolkata', value: 'Asia/Kolkata' },
                    { label: '(GMT+01:00) Europe/London', value: 'Europe/London' },
                    { label: '(GMT+02:00) Europe/Paris', value: 'Europe/Paris' },
                    { label: '(GMT+02:00) Europe/Berlin', value: 'Europe/Berlin' },
                    { label: '(GMT+03:00) Europe/Moscow', value: 'Europe/Moscow' },
                    { label: '(GMT+02:00) Europe/Amsterdam', value: 'Europe/Amsterdam' },
                    { label: '(GMT+02:00) Europe/Rome', value: 'Europe/Rome' },
                    { label: '(GMT-04:00) America/New_York', value: 'America/New_York' },
                    { label: '(GMT-07:00) America/Los_Angeles', value: 'America/Los_Angeles' },
                    { label: '(GMT-05:00) America/Chicago', value: 'America/Chicago' },
                    { label: '(GMT-03:00) America/Sao_Paulo', value: 'America/Sao_Paulo' },
                    { label: '(GMT+10:00) Australia/Sydney', value: 'Australia/Sydney' },
                    { label: '(GMT+12:00) Pacific/Auckland', value: 'Pacific/Auckland' },
                  ]}
                />
              </SettingField>
            </SettingsCard>
          </div>

          <SettingsCard title={showConnectionPool ? t('settings.connectionPool') : t('settings.resinTitle')} description={showConnectionPool ? undefined : t('settings.resinDesc')}>
            <div className="space-y-5">
              {showConnectionPool ? (
                <div className="grid grid-cols-[repeat(auto-fit,minmax(240px,1fr))] gap-4">
                  {isExternalDatabase ? (
                    <SettingField label={t('settings.pgMaxConns')} description={t('settings.pgMaxConnsRange')}>
                      <Input
                        type="number"
                        min={5}
                        max={500}
                        value={settingsForm.pg_max_conns}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, pg_max_conns: parseInt(e.target.value) || 50 }))}
                      />
                    </SettingField>
                  ) : null}
                  {isExternalCache ? (
                    <SettingField label={t('settings.redisPoolSize')} description={t('settings.redisPoolSizeRange')}>
                      <Input
                        type="number"
                        min={5}
                        max={500}
                        value={settingsForm.redis_pool_size}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, redis_pool_size: parseInt(e.target.value) || 30 }))}
                      />
                    </SettingField>
                  ) : null}
                </div>
              ) : null}
              {showConnectionPool ? (
                <div className="border-t border-border pt-4">
                  <h4 className="text-sm font-semibold text-foreground">{t('settings.resinTitle')}</h4>
                  <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('settings.resinDesc')}</p>
                </div>
              ) : null}
              <div className="grid grid-cols-[repeat(auto-fit,minmax(260px,1fr))] gap-4">
                <SettingField label={t('settings.resinUrl')} description={t('settings.resinUrlDesc')}>
                  <Input
                    placeholder="http://127.0.0.1:2260/your-token"
                    value={settingsForm.resin_url}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, resin_url: e.target.value }))}
                  />
                </SettingField>
                <SettingField label={t('settings.resinPlatformName')} description={t('settings.resinPlatformNameDesc')}>
                  <Input
                    placeholder="codex2api"
                    value={settingsForm.resin_platform_name}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, resin_platform_name: e.target.value }))}
                  />
                </SettingField>
              </div>
            </div>
          </SettingsCard>

	          <div className="grid gap-4 xl:grid-cols-2">
	            <SettingsCard title={t('settings.modelRegistry')} description={t('settings.modelRegistryDesc')}>
	              <div className="space-y-4">
	                <div className="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-3">
	                  <StatusTile label={t('settings.modelsEnabled')}>
	                    {enabledModelCount}
	                  </StatusTile>
	                  <StatusTile label={t('settings.modelsLastSynced')}>
	                    <span className="text-xs font-semibold">{modelsLastSyncedLabel}</span>
	                  </StatusTile>
	                </div>
	                <div className="flex flex-wrap items-center justify-between gap-2">
	                  <a
	                    href={modelsSourceLabel}
	                    target="_blank"
	                    rel="noreferrer"
	                    className="inline-flex min-w-0 items-center gap-1.5 text-xs font-medium text-primary hover:underline"
	                  >
	                    <ExternalLink className="size-3.5 shrink-0" />
	                    <span className="truncate">{modelsSourceLabel}</span>
	                  </a>
	                  <Button size="sm" variant="outline" onClick={() => void handleSyncModels()} disabled={syncingModels}>
	                    <RefreshCw className={cn('size-4', syncingModels && 'animate-spin')} />
	                    {syncingModels ? t('settings.modelsSyncing') : t('settings.syncUpstreamModels')}
	                  </Button>
	                </div>
	                <div className="flex max-h-[170px] flex-wrap gap-2 overflow-auto rounded-lg border border-border bg-muted/20 p-3">
	                  {visibleModelItems.map((model) => (
	                    <div key={model.id} className="flex flex-wrap items-center gap-1.5 rounded-md border border-border bg-background px-2.5 py-1.5">
	                      <span className="font-mono text-xs font-semibold text-foreground">{model.id}</span>
	                      <Badge variant={model.source === 'official_codex_docs' ? 'default' : 'secondary'} className="text-[11px]">
	                        {model.source === 'official_codex_docs' ? t('settings.modelSourceOfficial') : t('settings.modelSourceBuiltin')}
	                      </Badge>
	                      {model.pro_only ? <Badge variant="outline" className="text-[11px]">{t('settings.modelProOnly')}</Badge> : null}
	                      {model.category === 'image' ? <Badge variant="outline" className="text-[11px]">{t('settings.modelImage')}</Badge> : null}
	                    </div>
	                  ))}
	                </div>
	              </div>
	            </SettingsCard>

	            <SettingsCard title={t('settings2.modelMapping')} description={t('settings2.modelMappingDesc')}>
	              <ModelMappingEditor
	                value={settingsForm.model_mapping}
	                onChange={(v) => setSettingsForm(f => ({ ...f, model_mapping: v }))}
	              />
	            </SettingsCard>
	          </div>

	          <div className="grid gap-4">
	            <SettingsCard title={t('settings.apiEndpoints')}>
	              <div className="data-table-shell">
	                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="text-[12px] font-semibold">{t('settings.method')}</TableHead>
                      <TableHead className="text-[12px] font-semibold">{t('settings.path')}</TableHead>
                      <TableHead className="text-[12px] font-semibold">{t('settings.endpointDesc')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    <TableRow>
                      <TableCell><Badge variant="default" className="text-[12px]">POST</Badge></TableCell>
                      <TableCell className="font-mono text-[13px]">/v1/chat/completions</TableCell>
                      <TableCell className="text-[13px] text-muted-foreground">{t('settings.openaiCompat')}</TableCell>
                    </TableRow>
                    <TableRow>
                      <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                      <TableCell className="font-mono text-[13px]">/v1/responses</TableCell>
                      <TableCell className="text-[13px] text-muted-foreground">{t('settings.responsesApi')}</TableCell>
                    </TableRow>
                    <TableRow>
                      <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                      <TableCell className="font-mono text-[13px]">/v1/messages</TableCell>
                      <TableCell className="text-[13px] text-muted-foreground">{t('settings2.messagesEndpoint')}</TableCell>
                    </TableRow>
                    <TableRow>
                      <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                      <TableCell className="font-mono text-[13px]">/v1/images/generations</TableCell>
                      <TableCell className="text-[13px] text-muted-foreground">{t('settings.imageGenerationApi')}</TableCell>
                    </TableRow>
                    <TableRow>
                      <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                      <TableCell className="font-mono text-[13px]">/v1/images/edits</TableCell>
                      <TableCell className="text-[13px] text-muted-foreground">{t('settings.imageEditApi')}</TableCell>
                    </TableRow>
                    <TableRow>
                      <TableCell><Badge variant="secondary" className="text-[12px]">GET</Badge></TableCell>
                      <TableCell className="font-mono text-[13px]">/v1/models</TableCell>
                      <TableCell className="text-[13px] text-muted-foreground">{t('settings.modelList')}</TableCell>
                    </TableRow>
                  </TableBody>
                </Table>
              </div>
            </SettingsCard>
          </div>

          <div className="flex justify-end">
            {renderSaveButton('max-sm:w-full')}
          </div>
        </div>

        <ToastNotice toast={toast} />
      </>
    </StateShell>
  )
}
