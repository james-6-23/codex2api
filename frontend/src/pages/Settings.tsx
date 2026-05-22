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
import { DEFAULT_SITE_LOGO, sanitizeBrandingLogo, useBranding } from '../branding'
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

import { ExternalLink, RefreshCw, Save, Trash2, Upload, X } from 'lucide-react'

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
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <div className="grid shrink-0 grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] gap-1.5 px-1 text-xs font-semibold text-muted-foreground">
        <span>{t('settings2.anthropicModel')}</span>
        <span>{t('settings2.codexModel')}</span>
        <span />
      </div>
      <div className="min-h-[180px] flex-1 space-y-1.5 overflow-y-auto pr-1">
        {mappings.map(([k, v], i) => (
          <div key={i} className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] items-center gap-1.5">
            <Input
              className="h-8 px-2 font-mono text-xs"
              placeholder="claude-opus-4-6"
              value={k}
              onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 0, e.target.value)}
            />
            <Input
              className="h-8 px-2 font-mono text-xs"
              placeholder="gpt-5.5"
              value={v}
              onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 1, e.target.value)}
            />
            <button
              type="button"
              onClick={() => handleRemove(i)}
              aria-label={t('common.delete')}
              className="flex size-8 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10"
            >
              <Trash2 className="size-3.5" />
            </button>
          </div>
        ))}
      </div>
      <Button type="button" variant="outline" size="sm" className="self-start" onClick={handleAdd}>
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
  contentClassName,
  footer,
}: {
  title: string
  description?: string
  children: ReactNode
  className?: string
  contentClassName?: string
  footer?: ReactNode
}) {
  return (
    <Card className={cn('py-0', className)}>
      <CardContent className={cn('p-5', contentClassName)}>
        <div className="mb-4 shrink-0">
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

const SITE_LOGO_MAX_BYTES = 600 * 1024
const SITE_LOGO_CANVAS_SIZE = 80

function formatBytesKB(bytes: number) {
  return Math.max(1, Math.round(bytes / 1024))
}

function getSiteLogoMimeType(file: File) {
  const type = file.type.toLowerCase()
  const name = file.name.toLowerCase()
  if (type === 'image/png' || name.endsWith('.png')) return 'image/png'
  if (type === 'image/jpeg' || name.endsWith('.jpg') || name.endsWith('.jpeg')) return 'image/jpeg'
  if (type === 'image/svg+xml' || name.endsWith('.svg')) return 'image/svg+xml'
  return ''
}

function dataURLByteLength(dataURL: string) {
  const commaIndex = dataURL.indexOf(',')
  if (commaIndex === -1) return new Blob([dataURL]).size
  const meta = dataURL.slice(0, commaIndex)
  const data = dataURL.slice(commaIndex + 1)
  if (meta.endsWith(';base64')) {
    const padding = data.endsWith('==') ? 2 : data.endsWith('=') ? 1 : 0
    return Math.floor((data.length * 3) / 4) - padding
  }
  return new Blob([decodeURIComponent(data)]).size
}

function readFileAsDataURL(file: File) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(typeof reader.result === 'string' ? reader.result : '')
    reader.onerror = reject
    reader.readAsDataURL(file)
  })
}

function textToBase64(value: string) {
  const bytes = new TextEncoder().encode(value)
  let binary = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.slice(i, i + 0x8000))
  }
  return btoa(binary)
}

function minifySVG(value: string) {
  return value
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/>\s+</g, '><')
    .replace(/\s{2,}/g, ' ')
    .trim()
}

function loadImage(src: string) {
  return new Promise<HTMLImageElement>((resolve, reject) => {
    const image = new Image()
    image.onload = () => resolve(image)
    image.onerror = reject
    image.src = src
  })
}

function canvasToBlob(canvas: HTMLCanvasElement, type: string, quality?: number) {
  return new Promise<Blob>((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob)
      else reject(new Error('canvas-to-blob-failed'))
    }, type, quality)
  })
}

async function blobToDataURL(blob: Blob) {
  return readFileAsDataURL(new File([blob], 'site-logo', { type: blob.type }))
}

async function compressImageSourceToDataURL(src: string, mimeType: string) {
  const image = await loadImage(src)
  const canvas = document.createElement('canvas')
  canvas.width = SITE_LOGO_CANVAS_SIZE
  canvas.height = SITE_LOGO_CANVAS_SIZE
  const ctx = canvas.getContext('2d')
  if (!ctx) throw new Error('canvas-context-unavailable')

  const outputType = mimeType === 'image/jpeg' ? 'image/jpeg' : 'image/png'
  if (outputType === 'image/jpeg') {
    ctx.fillStyle = '#ffffff'
    ctx.fillRect(0, 0, canvas.width, canvas.height)
  } else {
    ctx.clearRect(0, 0, canvas.width, canvas.height)
  }

  const sourceWidth = image.naturalWidth || image.width || SITE_LOGO_CANVAS_SIZE
  const sourceHeight = image.naturalHeight || image.height || SITE_LOGO_CANVAS_SIZE
  const scale = Math.min(canvas.width / sourceWidth, canvas.height / sourceHeight)
  const drawWidth = Math.max(1, Math.round(sourceWidth * scale))
  const drawHeight = Math.max(1, Math.round(sourceHeight * scale))
  const dx = Math.round((canvas.width - drawWidth) / 2)
  const dy = Math.round((canvas.height - drawHeight) / 2)
  ctx.drawImage(image, dx, dy, drawWidth, drawHeight)

  if (outputType === 'image/png') {
    const blob = await canvasToBlob(canvas, outputType)
    return blobToDataURL(blob)
  }

  const qualities = [0.86, 0.72, 0.6, 0.48, 0.36]
  let bestDataURL = ''
  for (const quality of qualities) {
    const blob = await canvasToBlob(canvas, outputType, quality)
    const dataURL = await blobToDataURL(blob)
    bestDataURL = dataURL
    if (dataURLByteLength(dataURL) <= SITE_LOGO_MAX_BYTES) return dataURL
  }
  return bestDataURL
}

async function compressSiteLogoFile(file: File, mimeType: string) {
  if (mimeType === 'image/svg+xml') {
    const minified = minifySVG(await file.text())
    const svgDataURL = `data:image/svg+xml;base64,${textToBase64(minified)}`
    if (dataURLByteLength(svgDataURL) <= SITE_LOGO_MAX_BYTES) return svgDataURL
    return compressImageSourceToDataURL(svgDataURL, mimeType)
  }

  const objectURL = URL.createObjectURL(file)
  try {
    return await compressImageSourceToDataURL(objectURL, mimeType)
  } finally {
    URL.revokeObjectURL(objectURL)
  }
}

export default function Settings() {
  const { t } = useTranslation()
  const { applyBranding } = useBranding()
  const booleanOptions = [
    { label: t('common.disabled'), value: 'false' },
    { label: t('common.enabled'), value: 'true' },
  ]
  const schedulerModeOptions = [
    { label: t('settings.schedulerModeRoundRobin'), value: 'round_robin' },
    { label: t('settings.schedulerModeRemainingQuota'), value: 'remaining_quota' },
  ]
  const clientCompatOptions = [
    { label: t('settings.clientCompatPreserve'), value: 'preserve' },
    { label: t('settings.clientCompatAuto'), value: 'auto' },
    { label: t('settings.clientCompatForce'), value: 'force' },
  ]
  const usageLogModeOptions = [
    { label: t('settings.usageLogFull'), value: 'full' },
    { label: t('settings.usageLogErrors'), value: 'errors' },
    { label: t('settings.usageLogOff'), value: 'off' },
  ]
  const streamFlushPolicyOptions = [
    { label: t('settings.streamFlushImmediate'), value: 'immediate' },
    { label: t('settings.streamFlushCoalesce'), value: 'coalesce' },
  ]
  const imageStorageBackendOptions = [
    { label: t('settings.imageStorageLocal'), value: 'local' },
    { label: t('settings.imageStorageS3'), value: 's3' },
  ]
  const [settingsForm, setSettingsForm] = useState<SystemSettings>({
    site_name: 'CodexProxy',
    site_logo: '',
    max_concurrency: 2,
    global_rpm: 0,
    test_model: '',
    test_concurrency: 50,
    background_refresh_interval_minutes: 2,
    usage_probe_max_age_minutes: 10,
    recovery_probe_interval_minutes: 30,
    lazy_mode: false,
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
    scheduler_mode: 'round_robin',
    max_retries: 2,
    max_rate_limit_retries: 1,
    allow_remote_migration: false,
    database_driver: 'postgres',
    database_label: 'PostgreSQL',
    cache_driver: 'redis',
    cache_label: 'Redis',
    model_mapping: '{}',
    resin_url: '',
    resin_platform_name: '',
    prompt_filter_enabled: false,
    prompt_filter_mode: 'monitor',
    prompt_filter_threshold: 50,
    prompt_filter_strict_threshold: 90,
    prompt_filter_log_matches: true,
    prompt_filter_max_text_length: 81920,
    prompt_filter_sensitive_words: '',
    prompt_filter_custom_patterns: '[]',
    prompt_filter_disabled_patterns: '[]',
    client_compat_mode: 'preserve',
    codex_min_cli_version: '0.118.0',
    usage_log_mode: 'full',
    usage_log_batch_size: 200,
    usage_log_flush_interval_seconds: 5,
    stream_flush_policy: 'immediate',
    stream_flush_interval_ms: 20,
    image_storage_backend: 'local',
    image_s3_endpoint: '',
    image_s3_region: '',
    image_s3_bucket: '',
    image_s3_access_key: '',
    image_s3_secret_key: '',
    image_s3_prefix: '',
    image_s3_force_path_style: false,
  })
  const [savingSettings, setSavingSettings] = useState(false)
  const [testingImageStorage, setTestingImageStorage] = useState(false)
  const [loadedAdminSecret, setLoadedAdminSecret] = useState('')
  const [modelList, setModelList] = useState<string[]>([])
  const [modelItems, setModelItems] = useState<ModelInfo[]>([])
  const [modelsLastSyncedAt, setModelsLastSyncedAt] = useState<string | undefined>()
  const [modelsSourceURL, setModelsSourceURL] = useState('')
  const [syncingModels, setSyncingModels] = useState(false)
  const logoFileInputRef = useRef<HTMLInputElement>(null)
  const { toast, showToast } = useToast()

  const loadSettingsData = useCallback(async () => {
    const [health, settings, modelsResp] = await Promise.all([api.getHealth(), api.getSettings(), api.getModels()])
    setSettingsForm(settings)
    applyBranding({ site_name: settings.site_name, site_logo: settings.site_logo })
    setLoadedAdminSecret(settings.admin_secret ?? '')
    setModelList(modelsResp.models ?? [])
    setModelItems(modelsResp.items ?? [])
    setModelsLastSyncedAt(modelsResp.last_synced_at)
    setModelsSourceURL(modelsResp.source_url ?? '')
    return {
      health,
    }
  }, [applyBranding])

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
      applyBranding({ site_name: updated.site_name, site_logo: updated.site_logo })
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

  const handleSiteLogoUpload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    const mimeType = getSiteLogoMimeType(file)
    if (!mimeType) {
      showToast(t('settings.siteLogoInvalidType'), 'error')
      return
    }

    try {
      const result = file.size <= SITE_LOGO_MAX_BYTES
        ? await readFileAsDataURL(file)
        : await compressSiteLogoFile(file, mimeType)
      if (dataURLByteLength(result) > SITE_LOGO_MAX_BYTES) {
        showToast(t('settings.siteLogoTooLarge'), 'error')
        return
      }
      setSettingsForm((f) => ({ ...f, site_logo: result }))
      if (file.size > SITE_LOGO_MAX_BYTES) {
        showToast(t('settings.siteLogoCompressed', { size: formatBytesKB(dataURLByteLength(result)) }))
      }
    } catch {
      showToast(t('settings.siteLogoCompressionFailed'), 'error')
    }
  }

  const handleTestImageStorage = async () => {
    setTestingImageStorage(true)
    try {
      const result = await api.testImageStorageConnection({
        endpoint: settingsForm.image_s3_endpoint,
        region: settingsForm.image_s3_region,
        bucket: settingsForm.image_s3_bucket,
        access_key: settingsForm.image_s3_access_key,
        secret_key: settingsForm.image_s3_secret_key,
        prefix: settingsForm.image_s3_prefix,
        force_path_style: settingsForm.image_s3_force_path_style,
      })
      showToast(t('settings.imageS3TestSuccess', { bucket: result.bucket }))
    } catch (error) {
      showToast(`${t('settings.imageS3TestFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setTestingImageStorage(false)
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
  const siteLogoPreview = sanitizeBrandingLogo(settingsForm.site_logo) || DEFAULT_SITE_LOGO
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
                <SettingField label={t('settings.maxRateLimitRetries')} description={t('settings.maxRateLimitRetriesRange')}>
                  <Input
                    type="number"
                    min={0}
                    max={10}
                    value={settingsForm.max_rate_limit_retries}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, max_rate_limit_retries: parseInt(e.target.value) || 0 }))}
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
                <SettingField label={t('settings.lazyMode')} description={t('settings.lazyModeDesc')}>
                  <Select
                    value={settingsForm.lazy_mode ? 'true' : 'false'}
                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, lazy_mode: value === 'true' }))}
                    options={booleanOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.fastSchedulerEnabled')} description={t('settings.fastSchedulerEnabledDesc')}>
                  <Select
                    value={settingsForm.fast_scheduler_enabled ? 'true' : 'false'}
                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, fast_scheduler_enabled: value === 'true' }))}
                    options={booleanOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.schedulerMode')} description={t('settings.schedulerModeDesc')}>
                  <Select
                    value={settingsForm.scheduler_mode}
                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, scheduler_mode: value }))}
                    options={schedulerModeOptions}
                  />
                </SettingField>
              </div>
            </SettingsCard>
          </div>

          <SettingsCard title={t('settings.runtimeOptimization')} description={t('settings.runtimeOptimizationDesc')}>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(230px,1fr))] gap-4">
              <SettingField label={t('settings.clientCompatMode')} description={t('settings.clientCompatModeDesc')}>
                <Select
                  value={settingsForm.client_compat_mode}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, client_compat_mode: value }))}
                  options={clientCompatOptions}
                />
              </SettingField>
              <SettingField label={t('settings.codexMinCliVersion')} description={t('settings.codexMinCliVersionDesc')}>
                <Input
                  value={settingsForm.codex_min_cli_version}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, codex_min_cli_version: e.target.value }))}
                />
              </SettingField>
              <SettingField label={t('settings.usageLogMode')} description={t('settings.usageLogModeDesc')}>
                <Select
                  value={settingsForm.usage_log_mode}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, usage_log_mode: value }))}
                  options={usageLogModeOptions}
                />
              </SettingField>
              <SettingField label={t('settings.usageLogBatchSize')} description={t('settings.usageLogBatchSizeDesc')}>
                <Input
                  type="number"
                  min={1}
                  max={10000}
                  value={settingsForm.usage_log_batch_size}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, usage_log_batch_size: parseInt(e.target.value) || 200 }))}
                />
              </SettingField>
              <SettingField label={t('settings.usageLogFlushInterval')} description={t('settings.usageLogFlushIntervalDesc')}>
                <Input
                  type="number"
                  min={1}
                  max={300}
                  value={settingsForm.usage_log_flush_interval_seconds}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, usage_log_flush_interval_seconds: parseInt(e.target.value) || 5 }))}
                />
              </SettingField>
              <SettingField label={t('settings.streamFlushPolicy')} description={t('settings.streamFlushPolicyDesc')}>
                <Select
                  value={settingsForm.stream_flush_policy}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, stream_flush_policy: value }))}
                  options={streamFlushPolicyOptions}
                />
              </SettingField>
              <SettingField label={t('settings.streamFlushInterval')} description={t('settings.streamFlushIntervalDesc')}>
                <Input
                  type="number"
                  min={1}
                  max={1000}
                  value={settingsForm.stream_flush_interval_ms}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, stream_flush_interval_ms: parseInt(e.target.value) || 20 }))}
                />
              </SettingField>
            </div>
          </SettingsCard>

          <SettingsCard title={t('settings.imageStorage')} description={t('settings.imageStorageDesc')}>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(240px,1fr))] gap-4">
              <SettingField label={t('settings.imageStorageBackend')} description={t('settings.imageStorageBackendDesc')}>
                <Select
                  value={settingsForm.image_storage_backend}
                  onValueChange={(value) => setSettingsForm((f) => ({ ...f, image_storage_backend: value }))}
                  options={imageStorageBackendOptions}
                />
              </SettingField>
              {settingsForm.image_storage_backend === 's3' ? (
                <>
                  <SettingField label={t('settings.imageS3Endpoint')} description={t('settings.imageS3EndpointDesc')}>
                    <Input
                      value={settingsForm.image_s3_endpoint}
                      placeholder="https://..."
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_endpoint: e.target.value }))}
                    />
                  </SettingField>
                  <SettingField label={t('settings.imageS3Region')} description={t('settings.imageS3RegionDesc')}>
                    <Input
                      value={settingsForm.image_s3_region}
                      placeholder="auto"
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_region: e.target.value }))}
                    />
                  </SettingField>
                  <SettingField label={t('settings.imageS3Bucket')} description={t('settings.imageS3BucketDesc')}>
                    <Input
                      value={settingsForm.image_s3_bucket}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_bucket: e.target.value }))}
                    />
                  </SettingField>
                  <SettingField label={t('settings.imageS3AccessKey')} description={t('settings.imageS3AccessKeyDesc')}>
                    <Input
                      value={settingsForm.image_s3_access_key}
                      autoComplete="off"
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_access_key: e.target.value }))}
                    />
                  </SettingField>
                  <SettingField label={t('settings.imageS3SecretKey')} description={t('settings.imageS3SecretKeyDesc')}>
                    <Input
                      type="password"
                      value={settingsForm.image_s3_secret_key}
                      autoComplete="new-password"
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_secret_key: e.target.value }))}
                    />
                  </SettingField>
                  <SettingField label={t('settings.imageS3Prefix')} description={t('settings.imageS3PrefixDesc')}>
                    <Input
                      value={settingsForm.image_s3_prefix}
                      placeholder="codex/images"
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_prefix: e.target.value }))}
                    />
                  </SettingField>
                  <SettingField label={t('settings.imageS3ForcePathStyle')} description={t('settings.imageS3ForcePathStyleDesc')}>
                    <Select
                      value={settingsForm.image_s3_force_path_style ? 'true' : 'false'}
                      onValueChange={(value) => setSettingsForm((f) => ({ ...f, image_s3_force_path_style: value === 'true' }))}
                      options={booleanOptions}
                    />
                  </SettingField>
                </>
              ) : null}
            </div>
            {settingsForm.image_storage_backend === 's3' ? (
              <div className="mt-4">
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => void handleTestImageStorage()}
                  disabled={testingImageStorage || !settingsForm.image_s3_bucket || !settingsForm.image_s3_access_key || !settingsForm.image_s3_secret_key}
                >
                  {testingImageStorage ? t('settings.imageS3Testing') : t('settings.imageS3Test')}
                </Button>
              </div>
            ) : null}
          </SettingsCard>

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
                <SettingField label={t('settings.promptFilterEnabled')} description={t('settings.promptFilterEnabledDesc')}>
                  <Select
                    value={settingsForm.prompt_filter_enabled ? 'true' : 'false'}
                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, prompt_filter_enabled: value === 'true' }))}
                    options={booleanOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.promptFilterMode')} description={t('settings.promptFilterModeDesc')}>
                  <Select
                    value={settingsForm.prompt_filter_mode}
                    onValueChange={(value) => setSettingsForm((f) => ({ ...f, prompt_filter_mode: value }))}
                    options={[
                      { label: t('promptFilter.modeMonitor'), value: 'monitor' },
                      { label: t('promptFilter.modeWarn'), value: 'warn' },
                      { label: t('promptFilter.modeBlock'), value: 'block' },
                    ]}
                  />
                </SettingField>
              </div>
            </SettingsCard>

            <SettingsCard title={t('settings.display')}>
              <div className="space-y-4">
                <SettingField label={t('settings.siteName')} description={t('settings.siteNameDesc')}>
                  <Input
                    value={settingsForm.site_name}
                    maxLength={80}
                    placeholder="CodexProxy"
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, site_name: e.target.value }))}
                  />
                </SettingField>
                <SettingField label={t('settings.siteLogo')} description={t('settings.siteLogoDesc')}>
                  <div className="flex items-start gap-3">
                    <div className="flex size-12 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-border bg-background shadow-sm">
                      <img src={siteLogoPreview} alt={settingsForm.site_name || 'CodexProxy'} className="size-full object-cover" />
                    </div>
                    <div className="min-w-0 flex-1 space-y-2">
                      <Input
                        value={settingsForm.site_logo}
                        placeholder="/favicon.png or https://..."
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, site_logo: e.target.value }))}
                      />
                      <div className="flex flex-wrap gap-2">
                        <Button type="button" variant="outline" size="sm" onClick={() => logoFileInputRef.current?.click()}>
                          <Upload className="size-4" />
                          {t('settings.siteLogoUpload')}
                        </Button>
                        <Button type="button" variant="ghost" size="sm" onClick={() => setSettingsForm(f => ({ ...f, site_logo: '' }))}>
                          <X className="size-4" />
                          {t('settings.siteLogoReset')}
                        </Button>
                      </div>
                      <input
                        ref={logoFileInputRef}
                        type="file"
                        accept="image/png,image/jpeg,image/svg+xml,.png,.jpg,.jpeg,.svg"
                        className="hidden"
                        onChange={handleSiteLogoUpload}
                      />
                    </div>
                  </div>
                </SettingField>
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
              </div>
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

          <div className="grid items-stretch gap-4 xl:grid-cols-2">
            <SettingsCard
              title={t('settings.modelRegistry')}
              description={t('settings.modelRegistryDesc')}
              className="h-full xl:h-[430px]"
              contentClassName="flex h-full min-h-0 flex-col"
            >
              <div className="flex min-h-0 flex-1 flex-col gap-4">
                <div className="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-3">
                  <StatusTile label={t('settings.modelsEnabled')}>
                    {enabledModelCount}
                  </StatusTile>
                  <StatusTile label={t('settings.modelsLastSynced')}>
                    <span className="text-xs font-semibold">{modelsLastSyncedLabel}</span>
                  </StatusTile>
                </div>
                <div className="flex shrink-0 flex-wrap items-center justify-between gap-2">
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
                <div className="flex min-h-0 flex-1 flex-wrap content-start gap-2 overflow-auto rounded-lg border border-border bg-muted/20 p-3">
                  {visibleModelItems.map((model) => (
                    <div key={model.id} className="flex h-fit flex-wrap items-center gap-1.5 rounded-md border border-border bg-background px-2.5 py-1.5">
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

            <SettingsCard
              title={t('settings2.modelMapping')}
              description={t('settings2.modelMappingDesc')}
              className="h-full xl:h-[430px]"
              contentClassName="flex h-full min-h-0 flex-col"
            >
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
