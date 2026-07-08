import type { ChangeEvent, ReactNode } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, resetAdminAuthState, setAdminKey } from '../api'
import { formatBeijingTime, getTimezone, setTimezone } from '../utils/time'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { HealthResponse, ModelInfo, SiteBranding, SystemSettings } from '../types'
import { getErrorMessage } from '../utils/error'
import { DEFAULT_CLAUDE_MODEL_MAP } from '../lib/modelMapping'
import { DEFAULT_SITE_LOGO, isBrandingVideo, sanitizeBrandingImage, sanitizeBrandingLogo, useBranding } from '../branding'
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
const EMPTY_MODEL_MAPPING_ENTRIES: ModelMappingEntry[] = []
type ReasoningEffortModelEntry = {
  model: string
  effort: string
}
type AutoSaveStatus = 'idle' | 'saving' | 'saved' | 'error'
type CodexUserAgentConfig = {
  raw_user_agent?: string
  client_name?: string
  client_version?: string
  os_name?: string
  os_version?: string
  arch?: string
  terminal?: string
}

const EMPTY_REASONING_EFFORT_MODEL_ENTRIES: ReasoningEffortModelEntry[] = []
const REASONING_EFFORT_OPTIONS = ['low', 'medium', 'high', 'xhigh'].map((effort) => ({
  label: effort,
  value: effort,
}))
const AUTO_SAVE_STATUS_RESET_MS = 1800
const AUTO_SAVE_TOAST_MS = 2000
const DEFAULT_CODEX_UA_CONFIG: Required<CodexUserAgentConfig> = {
  raw_user_agent: '',
  client_name: 'codex-tui',
  client_version: '0.142.3',
  os_name: 'Mac OS',
  os_version: '15.5.0',
  arch: 'arm64',
  terminal: 'xterm-256color',
}

const getDefaultModelMappingEntries = (): ModelMappingEntry[] =>
  Object.entries(DEFAULT_CLAUDE_MODEL_MAP) as ModelMappingEntry[]

const parseModelMappingEntries = (value: string, fallbackEntries: ModelMappingEntry[] = []): ModelMappingEntry[] => {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return fallbackEntries
    }

    const entries = Object.entries(parsed).map(([key, model]) => [
      key,
      typeof model === 'string' ? model : String(model ?? ''),
    ]) as ModelMappingEntry[]

    // 如果数据库中为空，按调用方提供的默认值填充
    return entries.length > 0 ? entries : fallbackEntries
  } catch {
    return fallbackEntries
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

const normalizeReasoningEffortValue = (effort: string) => {
  const value = effort.trim().toLowerCase()
  if (value === 'max') return 'xhigh'
  return ['low', 'medium', 'high', 'xhigh'].includes(value) ? value : 'xhigh'
}

const normalizeBillingTierPolicyValue = (value?: string | null): 'actual' | 'requested' =>
  value === 'requested' ? 'requested' : 'actual'

const normalizeFirstTokenModeValue = (value?: string | null): 'strict' | 'loose' =>
  value === 'loose' ? 'loose' : 'strict'

const getSettingsPatchValues = (settings: SystemSettings, keys: Array<keyof SystemSettings>): Partial<SystemSettings> => {
  const patch: Record<string, unknown> = {}
  for (const key of keys) {
    patch[key] = settings[key]
  }
  return patch as Partial<SystemSettings>
}

const parseReasoningEffortModelEntries = (value: string): ReasoningEffortModelEntry[] => {
  try {
    const parsed = JSON.parse(value || '[]')
    if (!Array.isArray(parsed)) return EMPTY_REASONING_EFFORT_MODEL_ENTRIES
    return parsed
      .map((entry) => ({
        model: typeof entry?.model === 'string' ? entry.model : '',
        effort: normalizeReasoningEffortValue(typeof entry?.effort === 'string' ? entry.effort : 'xhigh'),
      }))
      .filter((entry) => entry.model.trim())
  } catch {
    return EMPTY_REASONING_EFFORT_MODEL_ENTRIES
  }
}

const serializeReasoningEffortModelEntries = (entries: ReasoningEffortModelEntry[]) => {
  const seen = new Set<string>()
  const normalized: ReasoningEffortModelEntry[] = []
  for (const entry of entries) {
    const model = entry.model.trim()
    const effort = normalizeReasoningEffortValue(entry.effort)
    if (!model) continue
    const key = `${model.toLowerCase()}(${effort})`
    if (seen.has(key)) continue
    seen.add(key)
    normalized.push({ model, effort })
  }
  return JSON.stringify(normalized)
}

const reasoningEffortAlias = (entry: ReasoningEffortModelEntry) => {
  const model = entry.model.trim()
  const effort = normalizeReasoningEffortValue(entry.effort)
  return model ? `${model}(${effort})` : ''
}

const parseCodexUserAgentConfig = (value?: string): CodexUserAgentConfig => {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
    return {
      raw_user_agent: typeof parsed.raw_user_agent === 'string' ? parsed.raw_user_agent : '',
      client_name: typeof parsed.client_name === 'string' ? parsed.client_name : '',
      client_version: typeof parsed.client_version === 'string' ? parsed.client_version : '',
      os_name: typeof parsed.os_name === 'string' ? parsed.os_name : '',
      os_version: typeof parsed.os_version === 'string' ? parsed.os_version : '',
      arch: typeof parsed.arch === 'string' ? parsed.arch : '',
      terminal: typeof parsed.terminal === 'string' ? parsed.terminal : '',
    }
  } catch {
    return {}
  }
}

const serializeCodexUserAgentConfig = (config: CodexUserAgentConfig) => {
  const normalized: CodexUserAgentConfig = {}
  for (const key of ['raw_user_agent', 'client_name', 'client_version', 'os_name', 'os_version', 'arch', 'terminal'] as const) {
    const value = (config[key] ?? '').trim()
    if (value) normalized[key] = key === 'client_version' ? normalizeVersionText(value) : value
  }
  return JSON.stringify(normalized)
}

type ParsedVersion = {
  core: [number, number, number]
  prerelease: string
}

const normalizeVersionText = (version?: string) => (version ?? '').trim().replace(/^v/i, '')

const parseVersion = (version?: string): ParsedVersion | null => {
  const match = normalizeVersionText(version).match(/^(\d+)\.(\d+)\.(\d+)(?:-([A-Za-z0-9][A-Za-z0-9.-]*))?$/)
  if (!match) return null
  return {
    core: [Number(match[1]), Number(match[2]), Number(match[3])],
    prerelease: match[4] ?? '',
  }
}

const isNumericVersionIdentifier = (value: string) => /^\d+$/.test(value)

const compareNumericVersionIdentifier = (a: string, b: string) => {
  const av = a.replace(/^0+/, '') || '0'
  const bv = b.replace(/^0+/, '') || '0'
  if (av.length !== bv.length) return av.length > bv.length ? 1 : -1
  if (av !== bv) return av > bv ? 1 : -1
  return 0
}

const comparePrerelease = (a: string, b: string) => {
  if (!a && !b) return 0
  if (!a) return 1
  if (!b) return -1
  const av = a.split('.')
  const bv = b.split('.')
  for (let i = 0; i < av.length && i < bv.length; i += 1) {
    const ai = av[i]
    const bi = bv[i]
    const an = isNumericVersionIdentifier(ai)
    const bn = isNumericVersionIdentifier(bi)
    if (an && bn) {
      const cmp = compareNumericVersionIdentifier(ai, bi)
      if (cmp !== 0) return cmp
    } else if (an) {
      return -1
    } else if (bn) {
      return 1
    } else if (ai !== bi) {
      return ai > bi ? 1 : -1
    }
  }
  if (av.length !== bv.length) return av.length > bv.length ? 1 : -1
  return 0
}

const compareVersions = (a?: string, b?: string) => {
  const av = parseVersion(a)
  const bv = parseVersion(b)
  if (!av || !bv) return 0
  for (let i = 0; i < 3; i += 1) {
    if (av.core[i] !== bv.core[i]) return av.core[i] > bv.core[i] ? 1 : -1
  }
  return comparePrerelease(av.prerelease, bv.prerelease)
}

const effectiveGeneratedCodexClientVersion = (version: string, minVersion: string, compatMode: string) => {
  const cleanVersion = normalizeVersionText(version) || DEFAULT_CODEX_UA_CONFIG.client_version
  const cleanMinVersion = normalizeVersionText(minVersion)
  if (compatMode === 'auto' && cleanMinVersion && compareVersions(cleanVersion, cleanMinVersion) < 0) {
    return cleanMinVersion
  }
  return cleanVersion
}

const buildCodexUserAgentPreview = (config: CodexUserAgentConfig, minVersion: string, compatMode: string) => {
  const raw = (config.raw_user_agent ?? '').trim()
  if (raw) return raw
  const clientName = (config.client_name ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.client_name
  const clientVersion = effectiveGeneratedCodexClientVersion(
    (config.client_version ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.client_version,
    minVersion,
    compatMode,
  )
  const osName = (config.os_name ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.os_name
  const osVersion = (config.os_version ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.os_version
  const arch = (config.arch ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.arch
  const terminal = (config.terminal ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.terminal
  return `${clientName}/${clientVersion} (${osName} ${osVersion}; ${arch}) ${terminal} (${clientName}; ${clientVersion})`
}

// 模型映射编辑器组件
function ModelMappingEditor({
  value,
  onChange,
  fallbackEntries = EMPTY_MODEL_MAPPING_ENTRIES,
  sourceOptions,
  targetOptions,
  sourceLabel,
  targetLabel,
  sourcePlaceholder,
  targetPlaceholder,
}: {
  value: string
  onChange: (v: string) => void
  fallbackEntries?: ModelMappingEntry[]
  sourceOptions?: Array<{ label: string; value: string }>
  targetOptions?: Array<{ label: string; value: string }>
  sourceLabel: string
  targetLabel: string
  sourcePlaceholder: string
  targetPlaceholder: string
}) {
  const { t } = useTranslation()
  const [mappings, setMappings] = useState<ModelMappingEntry[]>(() => parseModelMappingEntries(value, fallbackEntries))
  const lastEmittedValueRef = useRef<string | null>(null)
  const sourceSelectOptions = useMemo(() => {
    if (!sourceOptions) return []
    const byValue = new Map(sourceOptions.map((option) => [option.value, option]))
    for (const [source] of mappings) {
      const value = source.trim()
      if (value && !byValue.has(value)) {
        byValue.set(value, { label: value, value })
      }
    }
    return [...byValue.values()]
  }, [mappings, sourceOptions])
  const targetSelectOptions = useMemo(() => {
    if (!targetOptions) return []
    const byValue = new Map(targetOptions.map((option) => [option.value, option]))
    for (const [, target] of mappings) {
      const value = target.trim()
      if (value && !byValue.has(value)) {
        byValue.set(value, { label: value, value })
      }
    }
    return [...byValue.values()]
  }, [mappings, targetOptions])

  useEffect(() => {
    if (value === lastEmittedValueRef.current) return
    setMappings(parseModelMappingEntries(value, fallbackEntries))
  }, [fallbackEntries, value])

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
    const defaultSource = sourceOptions && targetOptions
      ? sourceOptions[1]?.value ?? sourceOptions[0]?.value ?? ''
      : sourceOptions?.[0]?.value ?? ''
    updateMappings([...mappings, [defaultSource, targetOptions?.[0]?.value ?? '']])
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <div className="grid shrink-0 grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] gap-1.5 px-1 text-xs font-semibold text-muted-foreground">
        <span>{sourceLabel}</span>
        <span>{targetLabel}</span>
        <span />
      </div>
      <div className="min-h-[180px] flex-1 space-y-1.5 overflow-y-auto pr-1">
        {mappings.map(([k, v], i) => (
          <div key={i} className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] items-center gap-1.5">
            {sourceOptions ? (
              <Select
                compact
                value={k.trim()}
                options={sourceSelectOptions}
                placeholder={sourcePlaceholder}
                disabled={sourceSelectOptions.length === 0}
                onValueChange={(next) => handleChange(i, 0, next)}
              />
            ) : (
              <Input
                className="h-8 px-2 font-mono text-xs"
                placeholder={sourcePlaceholder}
                value={k}
                onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 0, e.target.value)}
              />
            )}
            {targetOptions ? (
              <Select
                compact
                value={v.trim()}
                options={targetSelectOptions}
                placeholder={targetPlaceholder}
                disabled={targetSelectOptions.length === 0}
                onValueChange={(next) => handleChange(i, 1, next)}
              />
            ) : (
              <Input
                className="h-8 px-2 font-mono text-xs"
                placeholder={targetPlaceholder}
                value={v}
                onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 1, e.target.value)}
              />
            )}
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

function ReasoningEffortModelsEditor({
  value,
  onChange,
  baseModelOptions,
}: {
  value: string
  onChange: (v: string) => void
  baseModelOptions: Array<{ label: string; value: string }>
}) {
  const { t } = useTranslation()
  const [entries, setEntries] = useState<ReasoningEffortModelEntry[]>(() => parseReasoningEffortModelEntries(value))
  const lastEmittedValueRef = useRef<string | null>(null)
  const modelOptions = useMemo(() => {
    const byValue = new Map(baseModelOptions.map((option) => [option.value, option]))
    for (const entry of entries) {
      const model = entry.model.trim()
      if (model && !byValue.has(model)) {
        byValue.set(model, { label: model, value: model })
      }
    }
    return [...byValue.values()]
  }, [baseModelOptions, entries])

  useEffect(() => {
    if (value === lastEmittedValueRef.current) return
    setEntries(parseReasoningEffortModelEntries(value))
  }, [value])

  const updateEntries = (nextEntries: ReasoningEffortModelEntry[]) => {
    setEntries(nextEntries)
    const serialized = serializeReasoningEffortModelEntries(nextEntries)
    lastEmittedValueRef.current = serialized
    onChange(serialized)
  }

  const handleChange = (index: number, patch: Partial<ReasoningEffortModelEntry>) => {
    const next = entries.map((entry, i) => (i === index ? { ...entry, ...patch } : entry))
    updateEntries(next)
  }

  const handleRemove = (index: number) => {
    updateEntries(entries.filter((_, i) => i !== index))
  }

  const handleAdd = () => {
    updateEntries([...entries, { model: baseModelOptions[0]?.value ?? 'gpt-5.5', effort: 'xhigh' }])
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <div className="overflow-x-auto pb-1">
        <div className="w-fit min-w-[32rem]">
          <div className="grid shrink-0 grid-cols-[minmax(0,13rem)_8rem_max-content_2rem] gap-2 px-1 text-xs font-semibold text-muted-foreground">
            <span>{t('settings2.baseModel')}</span>
            <span>{t('settings2.reasoningEffort')}</span>
            <span>{t('settings2.generatedModel')}</span>
            <span />
          </div>
          <div className="mt-2 max-h-[220px] space-y-1.5 overflow-y-auto pr-1">
            {entries.map((entry, i) => (
              <div key={i} className="grid grid-cols-[minmax(0,13rem)_8rem_max-content_2rem] items-center gap-2">
                <Select
                  compact
                  value={entry.model.trim()}
                  options={modelOptions}
                  placeholder={t('settings2.selectBaseModel')}
                  disabled={modelOptions.length === 0}
                  onValueChange={(model) => handleChange(i, { model })}
                />
                <Select
                  compact
                  value={normalizeReasoningEffortValue(entry.effort)}
                  options={REASONING_EFFORT_OPTIONS}
                  onValueChange={(effort) => handleChange(i, { effort })}
                />
                <div className="flex min-w-0">
                  <Badge variant="secondary" className="max-w-full px-2 py-1 font-mono text-[11px]">
                    <span className="truncate">{reasoningEffortAlias(entry) || '-'}</span>
                  </Badge>
                </div>
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
        </div>
      </div>
      <Button type="button" variant="outline" size="sm" className="self-start" onClick={handleAdd}>
        + {t('settings2.addReasoningModel')}
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
    <div data-slot="status-tile" className="flex min-h-[76px] flex-col justify-between gap-2 rounded-lg border border-border bg-muted/25 p-3">
      <span className="text-[11px] font-bold uppercase text-muted-foreground">{label}</span>
      <div className="text-sm font-semibold text-foreground">{children}</div>
    </div>
  )
}

const SITE_LOGO_MAX_BYTES = 600 * 1024
const SITE_LOGO_CANVAS_SIZE = 80
const BACKGROUND_IMAGE_UPLOAD_MAX_BYTES = 20 * 1024 * 1024
const BACKGROUND_VIDEO_UPLOAD_MAX_BYTES = 40 * 1024 * 1024

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

function getBackgroundImageMimeType(file: File) {
  const type = file.type.toLowerCase()
  const name = file.name.toLowerCase()
  if (type === 'image/png' || name.endsWith('.png')) return 'image/png'
  if (type === 'image/jpeg' || name.endsWith('.jpg') || name.endsWith('.jpeg')) return 'image/jpeg'
  if (type === 'image/webp' || name.endsWith('.webp')) return 'image/webp'
  if (type === 'image/svg+xml' || name.endsWith('.svg')) return 'image/svg+xml'
  if (type === 'video/mp4' || name.endsWith('.mp4')) return 'video/mp4'
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
  // 循环剥离注释直到不动点：单次替换可能因相邻片段重新拼出 "<!--" 而残留
  let out = value
  for (let prev = ''; prev !== out; ) {
    prev = out
    out = out.replace(/<!--[\s\S]*?-->/g, '').replace(/<!--/g, '')
  }
  return out
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
  const defaultClaudeModelMappingEntries = useMemo(() => getDefaultModelMappingEntries(), [])
  const booleanOptions = [
    { label: t('common.disabled'), value: 'false' },
    { label: t('common.enabled'), value: 'true' },
  ]
  const schedulerModeOptions = [
    { label: t('settings.schedulerModeRoundRobin'), value: 'round_robin' },
    { label: t('settings.schedulerModeRemainingQuota'), value: 'remaining_quota' },
  ]
  const transportRetryPolicyOptions = [
    { label: t('settings.transportRetryPolicyRotate'), value: 'rotate' },
    { label: t('settings.transportRetryPolicySticky'), value: 'sticky' },
  ]
  const affinityModeOptions = [
    { label: t('settings.affinityModeBounded'), value: 'bounded' },
    { label: t('settings.affinityModeOff'), value: 'off' },
    { label: t('settings.affinityModeStrict'), value: 'strict' },
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
  const billingTierPolicyOptions = [
    { label: t('settings.billingTierPolicyActual'), value: 'actual' },
    { label: t('settings.billingTierPolicyRequested'), value: 'requested' },
  ]
  const streamFlushPolicyOptions = [
    { label: t('settings.streamFlushImmediate'), value: 'immediate' },
    { label: t('settings.streamFlushCoalesce'), value: 'coalesce' },
  ]
  const firstTokenModeOptions = [
    { label: t('settings.firstTokenModeStrict'), value: 'strict' },
    { label: t('settings.firstTokenModeLoose'), value: 'loose' },
  ]
  const imageStorageBackendOptions = [
    { label: t('settings.imageStorageLocal'), value: 'local' },
    { label: t('settings.imageStorageS3'), value: 's3' },
  ]
  const normalizeLazySettingsForm = useCallback((settings: SystemSettings): SystemSettings => {
    const normalized = {
      ...settings,
      billing_tier_policy: normalizeBillingTierPolicyValue(settings.billing_tier_policy),
      first_token_mode: normalizeFirstTokenModeValue(settings.first_token_mode),
    }
    if (!normalized.lazy_mode) {
      return normalized
    }
    return {
      ...normalized,
      auto_clean_full_usage: false,
    }
  }, [])
  const [settingsForm, setSettingsForm] = useState<SystemSettings>({
    site_name: 'CodexProxy',
    site_logo: '',
    background_image: '',
    background_opacity: 18,
    background_blur: 0,
    background_glass_opacity: 58,
    background_glass_blur: 5,
    max_concurrency: 2,
    global_rpm: 0,
    test_model: '',
    test_content: 'hi',
    test_concurrency: 50,
	    background_refresh_interval_minutes: 2,
	    usage_probe_max_age_minutes: 10,
	    usage_probe_concurrency: 16,
	    usage_probe_responses_fallback_enabled: true,
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
    codex_force_websocket: false,
    codex_ws_keepalive_enabled: false,
    codex_ws_keepalive_interval_sec: 60,
    codex_ws_hide_upstream_errors: true,
    codex_ws_silent_retry_enabled: true,
    codex_ws_silent_max_retries: 2,
    scheduler_mode: 'round_robin',
    affinity_mode: 'bounded',
    max_retries: 2,
    max_rate_limit_retries: 1,
    retry_interval_ms: 0,
    transport_retry_policy: 'rotate',
    allow_remote_migration: false,
    database_driver: 'postgres',
    database_label: 'PostgreSQL',
    cache_driver: 'redis',
    cache_label: 'Redis',
    model_mapping: '{}',
    codex_model_mapping: '{}',
    reasoning_effort_models: '[]',
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
    prompt_filter_review_enabled: false,
    prompt_filter_review_api_key: '',
    prompt_filter_review_api_key_configured: false,
    prompt_filter_review_base_url: 'https://api.openai.com',
    prompt_filter_review_model: 'omni-moderation-latest',
    prompt_filter_review_timeout_seconds: 10,
    prompt_filter_review_fail_closed: true,
    client_compat_mode: 'preserve',
    codex_min_cli_version: '0.118.0',
    codex_user_agent_config: '{}',
    usage_log_mode: 'full',
    usage_log_batch_size: 200,
    usage_log_flush_interval_seconds: 5,
    stream_flush_policy: 'immediate',
    stream_flush_interval_ms: 20,
    first_token_mode: 'strict',
    first_token_timeout_seconds: 0,
    billing_tier_policy: 'actual',
    show_full_usage_numbers: false,
    public_key_usage_page_enabled: true,
    image_storage_backend: 'local',
    image_s3_endpoint: '',
    image_s3_region: '',
    image_s3_bucket: '',
    image_s3_access_key: '',
    image_s3_secret_key: '',
    image_s3_prefix: '',
    image_s3_force_path_style: false,
    auto_pause_5h_threshold: 0,
    auto_pause_7d_threshold: 0,
    auto_pause_5h_guard_band_percent: 5,
    auto_pause_5h_guard_concurrency: 1,
    smart_pacing_enabled: false,
    smart_pacing_min_concurrency: 1,
    smart_pacing_windows: '5h,7d',
  })
  const lazyModeActive = settingsForm.lazy_mode
  const [savingSettings, setSavingSettings] = useState(false)
  const [autoSaveStatus, setAutoSaveStatus] = useState<AutoSaveStatus>('idle')
  const [autoSaveError, setAutoSaveError] = useState('')
  const [testingImageStorage, setTestingImageStorage] = useState(false)
  const [loadedAdminSecret, setLoadedAdminSecret] = useState('')
  const [modelList, setModelList] = useState<string[]>([])
  const [modelItems, setModelItems] = useState<ModelInfo[]>([])
  const [modelsLastSyncedAt, setModelsLastSyncedAt] = useState<string | undefined>()
  const [modelsSourceURL, setModelsSourceURL] = useState('')
  const [syncingModels, setSyncingModels] = useState(false)
  const logoFileInputRef = useRef<HTMLInputElement>(null)
  const backgroundFileInputRef = useRef<HTMLInputElement>(null)
  const persistedBrandingRef = useRef<Partial<SiteBranding> | null>(null)
  const settingsFormRef = useRef(settingsForm)
  const autoSavePendingCountRef = useRef(0)
  const autoSaveFieldVersionsRef = useRef<Record<string, number>>({})
  const autoSaveStatusTimerRef = useRef<ReturnType<typeof window.setTimeout> | null>(null)
  const { toast, showToast } = useToast()

  useEffect(() => {
    settingsFormRef.current = settingsForm
  }, [settingsForm])

  useEffect(() => {
    return () => {
      if (autoSaveStatusTimerRef.current) {
        window.clearTimeout(autoSaveStatusTimerRef.current)
      }
    }
  }, [])

  const commitSettingsForm = useCallback(
    (next: SystemSettings) => {
      const normalized = normalizeLazySettingsForm(next)
      settingsFormRef.current = normalized
      setSettingsForm(normalized)
      return normalized
    },
    [normalizeLazySettingsForm],
  )

  const scheduleAutoSaveStatusReset = useCallback(() => {
    if (autoSaveStatusTimerRef.current) {
      window.clearTimeout(autoSaveStatusTimerRef.current)
    }
    autoSaveStatusTimerRef.current = window.setTimeout(() => {
      setAutoSaveStatus((status) => (status === 'saved' ? 'idle' : status))
      autoSaveStatusTimerRef.current = null
    }, AUTO_SAVE_STATUS_RESET_MS)
  }, [])

  const finishAutoSaveRequest = useCallback((status: Exclude<AutoSaveStatus, 'idle' | 'saving'>) => {
    autoSavePendingCountRef.current = Math.max(0, autoSavePendingCountRef.current - 1)
    if (autoSavePendingCountRef.current > 0) {
      setAutoSaveStatus('saving')
      return
    }
    setAutoSaveStatus(status)
    if (status === 'saved') {
      scheduleAutoSaveStatusReset()
    }
  }, [scheduleAutoSaveStatusReset])

  const autoSaveSettingsPatch = useCallback(async (patch: Partial<SystemSettings>) => {
    const patchKeys = Object.keys(patch) as Array<keyof SystemSettings>
    if (patchKeys.length === 0) return

    const previous = settingsFormRef.current
    const optimistic = commitSettingsForm({
      ...previous,
      ...patch,
    })
    const rollbackPatch = getSettingsPatchValues(previous, patchKeys)
    const requestedVersions: Record<string, number> = {}

    for (const key of patchKeys) {
      const fieldKey = String(key)
      const nextVersion = (autoSaveFieldVersionsRef.current[fieldKey] ?? 0) + 1
      autoSaveFieldVersionsRef.current[fieldKey] = nextVersion
      requestedVersions[fieldKey] = nextVersion
    }

    autoSavePendingCountRef.current += 1
    if (autoSaveStatusTimerRef.current) {
      window.clearTimeout(autoSaveStatusTimerRef.current)
      autoSaveStatusTimerRef.current = null
    }
    setAutoSaveError('')
    setAutoSaveStatus('saving')

    try {
      const updated = await api.updateSettings(getSettingsPatchValues(optimistic, patchKeys))
      const mergeKeys = patchKeys.filter((key) => {
        const fieldKey = String(key)
        return autoSaveFieldVersionsRef.current[fieldKey] === requestedVersions[fieldKey]
      })
      if (mergeKeys.length > 0) {
        commitSettingsForm({
          ...settingsFormRef.current,
          ...getSettingsPatchValues(updated, mergeKeys),
        })
      }
      const autoSaveSuccessMessage = updated.expired_cleaned && updated.expired_cleaned > 0
        ? `${t('settings.autoSaved')} · ${t('settings.expiredCleanedResult', { count: updated.expired_cleaned })}`
        : t('settings.autoSaved')
      showToast(autoSaveSuccessMessage, 'success', AUTO_SAVE_TOAST_MS)
      finishAutoSaveRequest('saved')
    } catch (error) {
      const rollbackKeys = patchKeys.filter((key) => {
        const fieldKey = String(key)
        return autoSaveFieldVersionsRef.current[fieldKey] === requestedVersions[fieldKey]
      })
      if (rollbackKeys.length > 0) {
        commitSettingsForm({
          ...settingsFormRef.current,
          ...getSettingsPatchValues({ ...previous, ...rollbackPatch }, rollbackKeys),
        })
      }
      const message = getErrorMessage(error)
      setAutoSaveError(message)
      showToast(`${t('settings.autoSaveFailed')}: ${message}`, 'error')
      finishAutoSaveRequest('error')
    }
  }, [commitSettingsForm, finishAutoSaveRequest, showToast, t])

  const autoSaveBooleanField = useCallback((field: keyof SystemSettings, value: string, extraPatch: Partial<SystemSettings> = {}) => {
    void autoSaveSettingsPatch({
      ...extraPatch,
      [field]: value === 'true',
    } as Partial<SystemSettings>)
  }, [autoSaveSettingsPatch])

  const autoSaveStringField = useCallback((field: keyof SystemSettings, value: string, extraPatch: Partial<SystemSettings> = {}) => {
    void autoSaveSettingsPatch({
      ...extraPatch,
      [field]: value,
    } as Partial<SystemSettings>)
  }, [autoSaveSettingsPatch])

  const loadSettingsData = useCallback(async () => {
    const [health, settings, modelsResp] = await Promise.all([api.getHealth(), api.getSettings(), api.getModels()])
    commitSettingsForm(settings)
    const branding = {
      site_name: settings.site_name,
      site_logo: settings.site_logo,
      background_image: settings.background_image,
      background_opacity: settings.background_opacity,
      background_blur: settings.background_blur,
      background_glass_opacity: settings.background_glass_opacity,
      background_glass_blur: settings.background_glass_blur,
    }
    persistedBrandingRef.current = branding
    applyBranding(branding)
    setLoadedAdminSecret(settings.admin_secret ?? '')
    setModelList(modelsResp.models ?? [])
    setModelItems(modelsResp.items ?? [])
    setModelsLastSyncedAt(modelsResp.last_synced_at)
    setModelsSourceURL(modelsResp.source_url ?? '')
    return {
      health,
    }
  }, [applyBranding, commitSettingsForm])

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
      const updated = await api.updateSettings(normalizeLazySettingsForm(settingsForm))
      commitSettingsForm(updated)
      const branding = {
        site_name: updated.site_name,
        site_logo: updated.site_logo,
        background_image: updated.background_image,
        background_opacity: updated.background_opacity,
        background_blur: updated.background_blur,
        background_glass_opacity: updated.background_glass_opacity,
        background_glass_blur: updated.background_glass_blur,
      }
      persistedBrandingRef.current = branding
      applyBranding(branding)
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

  useEffect(() => {
    if (!persistedBrandingRef.current) return
    applyBranding({
      site_name: settingsForm.site_name,
      site_logo: settingsForm.site_logo,
      background_image: settingsForm.background_image,
      background_opacity: settingsForm.background_opacity,
      background_blur: settingsForm.background_blur,
      background_glass_opacity: settingsForm.background_glass_opacity,
      background_glass_blur: settingsForm.background_glass_blur,
    })
  }, [
    applyBranding,
    settingsForm.site_name,
    settingsForm.site_logo,
    settingsForm.background_image,
    settingsForm.background_opacity,
    settingsForm.background_blur,
    settingsForm.background_glass_opacity,
    settingsForm.background_glass_blur,
  ])

  useEffect(() => {
    return () => {
      if (persistedBrandingRef.current) {
        applyBranding(persistedBrandingRef.current)
      }
    }
  }, [applyBranding])

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

  const handleBackgroundImageUpload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    const mimeType = getBackgroundImageMimeType(file)
    if (!mimeType) {
      showToast(t('settings.backgroundImageInvalidType'), 'error')
      return
    }
    const maxBytes = mimeType === 'video/mp4' ? BACKGROUND_VIDEO_UPLOAD_MAX_BYTES : BACKGROUND_IMAGE_UPLOAD_MAX_BYTES
    if (file.size > maxBytes) {
      showToast(t(mimeType === 'video/mp4' ? 'settings.backgroundVideoTooLarge' : 'settings.backgroundImageTooLarge'), 'error')
      return
    }

    try {
      const uploaded = await api.uploadBackground(file)
      setSettingsForm((f) => ({
        ...f,
        background_image: uploaded.url,
        background_opacity: f.background_opacity || 18,
      }))
      showToast(t('settings.backgroundImageUploaded'))
    } catch (err) {
      showToast(getErrorMessage(err) || t('settings.backgroundImageUploadFailed'), 'error')
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
  const autoSaveStatusMeta = autoSaveStatus === 'idle' ? null : (
    <span
      className={cn(
        'inline-flex items-center gap-1 font-medium',
        autoSaveStatus === 'saving' && 'text-muted-foreground',
        autoSaveStatus === 'saved' && 'text-emerald-600 dark:text-emerald-400',
        autoSaveStatus === 'error' && 'text-destructive',
      )}
      title={autoSaveStatus === 'error' ? autoSaveError : undefined}
    >
      <span
        className={cn(
          'size-1.5 rounded-full',
          autoSaveStatus === 'saving' && 'animate-pulse bg-muted-foreground',
          autoSaveStatus === 'saved' && 'bg-emerald-500',
          autoSaveStatus === 'error' && 'bg-destructive',
        )}
      />
      {autoSaveStatus === 'saving'
        ? t('settings.autoSaving')
        : autoSaveStatus === 'saved'
          ? t('settings.autoSaved')
          : t('settings.autoSaveFailed')}
    </span>
  )
  const siteLogoPreview = sanitizeBrandingLogo(settingsForm.site_logo) || DEFAULT_SITE_LOGO
  const backgroundImagePreview = sanitizeBrandingImage(settingsForm.background_image)
  const backgroundIsVideo = isBrandingVideo(backgroundImagePreview)
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
  const codexModelOptions = visibleModelItems
    .filter((model) =>
      model.enabled &&
      !model.id.includes('(') &&
      !model.id.includes(')')
    )
    .map((model) => ({ label: model.id, value: model.id }))
  const textModelOptions = visibleModelItems
    .filter((model) =>
      model.enabled &&
      model.category !== 'image' &&
      !model.id.includes('image') &&
      !model.id.includes('(') &&
      !model.id.includes(')')
    )
    .map((model) => ({ label: model.id, value: model.id }))
  const enabledModelCount = visibleModelItems.filter((model) => model.enabled).length
  const modelsLastSyncedLabel = modelsLastSyncedAt ? formatBeijingTime(modelsLastSyncedAt) : t('settings.modelsNeverSynced')
  const modelsSourceLabel = modelsSourceURL || 'https://developers.openai.com/codex/models'
  const codexUserAgentConfig = useMemo(
    () => parseCodexUserAgentConfig(settingsForm.codex_user_agent_config),
    [settingsForm.codex_user_agent_config],
  )
  const codexUserAgentPreview = useMemo(
    () => buildCodexUserAgentPreview(codexUserAgentConfig, settingsForm.codex_min_cli_version, settingsForm.client_compat_mode),
    [codexUserAgentConfig, settingsForm.client_compat_mode, settingsForm.codex_min_cli_version],
  )
  const updateCodexUserAgentConfig = useCallback((patch: Partial<CodexUserAgentConfig>) => {
    setSettingsForm((form) => {
      const current = parseCodexUserAgentConfig(form.codex_user_agent_config)
      return {
        ...form,
        codex_user_agent_config: serializeCodexUserAgentConfig({ ...current, ...patch }),
      }
    })
  }, [])
  const saveCodexUserAgentConfig = useCallback(() => {
    void autoSaveSettingsPatch({ codex_user_agent_config: settingsForm.codex_user_agent_config })
  }, [autoSaveSettingsPatch, settingsForm.codex_user_agent_config])
  const renderSaveButton = (className?: string) => (
    <Button className={className} onClick={() => void handleSaveSettings()} disabled={savingSettings || autoSaveStatus === 'saving'}>
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
          actionMeta={autoSaveStatusMeta}
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

          <div className="grid gap-4 xl:grid-cols-3">
            <SettingsCard title={t('settings.trafficProtection')}>
              <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-4">
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
                <SettingField label={t('settings.retryIntervalMs')} description={t('settings.retryIntervalMsDesc')}>
                  <Input
                    type="number"
                    min={0}
                    max={30000}
                    step={100}
                    value={settingsForm.retry_interval_ms}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, retry_interval_ms: parseInt(e.target.value) || 0 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.transportRetryPolicy')} description={t('settings.transportRetryPolicyDesc')}>
                  <Select
                    value={settingsForm.transport_retry_policy || 'rotate'}
                    onValueChange={(value) => autoSaveStringField('transport_retry_policy', value)}
                    options={transportRetryPolicyOptions}
                  />
                </SettingField>
              </div>
            </SettingsCard>

            <SettingsCard title={t('settings.probeScheduling')}>
              <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-4">
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
                <SettingField label={t('settings.usageProbeConcurrency')} description={t('settings.usageProbeConcurrencyDesc')}>
                  <Input
                    type="number"
                    min={1}
                    max={128}
                    value={settingsForm.usage_probe_concurrency}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, usage_probe_concurrency: parseInt(e.target.value) || 1 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.usageProbeResponsesFallback')} description={t('settings.usageProbeResponsesFallbackDesc')}>
                  <Select
                    value={settingsForm.usage_probe_responses_fallback_enabled ? 'true' : 'false'}
                    onValueChange={(value) => autoSaveBooleanField('usage_probe_responses_fallback_enabled', value)}
                    options={booleanOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.recoveryProbeInterval')} description={t('settings.recoveryProbeIntervalDesc')}>
                  <Input
                    type={lazyModeActive ? 'text' : 'number'}
                    min={1}
                    max={10080}
                    value={lazyModeActive ? '∞' : settingsForm.recovery_probe_interval_minutes}
                    disabled={lazyModeActive}
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, recovery_probe_interval_minutes: parseInt(e.target.value) || 1 }))}
                  />
                </SettingField>
                <SettingField label={t('settings.lazyMode')} description={t('settings.lazyModeDesc')}>
                  <Select
                    value={settingsForm.lazy_mode ? 'true' : 'false'}
                    onValueChange={(value) => {
                      const enabled = value === 'true'
                      void autoSaveSettingsPatch({
                        lazy_mode: enabled,
                        auto_clean_full_usage: enabled ? false : settingsFormRef.current.auto_clean_full_usage,
                      })
                    }}
                    options={booleanOptions}
                  />
                </SettingField>
              </div>
            </SettingsCard>

            <SettingsCard title={t('settings.schedulingStrategy')}>
              <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-4">
                <SettingField label={t('settings.testModelLabel')} description={t('settings.testModelHint')}>
                  <Select
                    value={settingsForm.test_model}
                    onValueChange={(value) => autoSaveStringField('test_model', value)}
                    options={textModelOptions}
                  />
                </SettingField>
                <SettingField className="md:col-span-2" label={t('settings.testContent')} description={t('settings.testContentDesc')}>
                  <textarea
                    rows={3}
                    value={settingsForm.test_content}
                    placeholder={t('settings.testContentPlaceholder')}
                    onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setSettingsForm(f => ({ ...f, test_content: e.target.value }))}
                    onBlur={(e) => autoSaveStringField('test_content', e.currentTarget.value)}
                    className={cn(
                      'flex min-h-[92px] w-full resize-y rounded-lg border border-input bg-background px-3 py-2 text-sm text-foreground shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50',
                    )}
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
                <SettingField label={t('settings.fastSchedulerEnabled')} description={t('settings.fastSchedulerEnabledDesc')}>
                  <Select
                    value={settingsForm.fast_scheduler_enabled ? 'true' : 'false'}
                    onValueChange={(value) => autoSaveBooleanField('fast_scheduler_enabled', value)}
                    options={booleanOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.schedulerMode')} description={t('settings.schedulerModeDesc')}>
                  <Select
                    value={settingsForm.scheduler_mode}
                    onValueChange={(value) => autoSaveStringField('scheduler_mode', value)}
                    options={schedulerModeOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.affinityMode')} description={t('settings.affinityModeDesc')}>
                  <Select
                    value={settingsForm.affinity_mode || 'bounded'}
                    onValueChange={(value) => autoSaveStringField('affinity_mode', value)}
                    options={affinityModeOptions}
                  />
                </SettingField>
              </div>
            </SettingsCard>
          </div>

          <SettingsCard title={t('settings.globalAutoPauseTitle')} description={t('settings.globalAutoPauseDesc')}>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(230px,1fr))] gap-4">
              <SettingField label={t('settings.globalAutoPause5h')} description={t('settings.globalAutoPauseHint')}>
                <Input
                  type="number"
                  min={0}
                  max={100}
                  step={0.1}
                  inputMode="decimal"
                  placeholder={t('settings.globalAutoPausePlaceholder')}
                  value={settingsForm.auto_pause_5h_threshold > 0 ? (settingsForm.auto_pause_5h_threshold * 100).toFixed(1).replace(/\.0$/, '') : ''}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => {
                    const raw = e.target.value
                    const ratio = raw === '' ? 0 : Math.max(0, Math.min(1, parseFloat(raw) / 100))
                    setSettingsForm(f => ({ ...f, auto_pause_5h_threshold: isNaN(ratio) ? 0 : ratio }))
                  }}
                  onBlur={() => {
                    void autoSaveSettingsPatch({ auto_pause_5h_threshold: settingsForm.auto_pause_5h_threshold })
                  }}
                />
              </SettingField>
              <SettingField label={t('settings.globalAutoPause7d')} description={t('settings.globalAutoPauseHint')}>
                <Input
                  type="number"
                  min={0}
                  max={100}
                  step={0.1}
                  inputMode="decimal"
                  placeholder={t('settings.globalAutoPausePlaceholder')}
                  value={settingsForm.auto_pause_7d_threshold > 0 ? (settingsForm.auto_pause_7d_threshold * 100).toFixed(1).replace(/\.0$/, '') : ''}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => {
                    const raw = e.target.value
                    const ratio = raw === '' ? 0 : Math.max(0, Math.min(1, parseFloat(raw) / 100))
                    setSettingsForm(f => ({ ...f, auto_pause_7d_threshold: isNaN(ratio) ? 0 : ratio }))
                  }}
                  onBlur={() => {
                    void autoSaveSettingsPatch({ auto_pause_7d_threshold: settingsForm.auto_pause_7d_threshold })
                  }}
                />
              </SettingField>
              <SettingField label={t('settings.autoPause5hGuardBand')} description={t('settings.autoPause5hGuardBandHint')}>
                <Input
                  type="number"
                  min={0}
                  max={100}
                  step={0.1}
                  inputMode="decimal"
                  placeholder={t('settings.autoPause5hGuardBandPlaceholder')}
                  value={settingsForm.auto_pause_5h_guard_band_percent > 0 ? settingsForm.auto_pause_5h_guard_band_percent : ''}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => {
                    const raw = e.target.value
                    if (raw === '') {
                      setSettingsForm(f => ({ ...f, auto_pause_5h_guard_band_percent: 0 }))
                      return
                    }
                    const parsed = parseFloat(raw)
                    if (Number.isNaN(parsed)) return
                    const value = Math.max(0, Math.min(100, parsed))
                    setSettingsForm(f => ({ ...f, auto_pause_5h_guard_band_percent: value }))
                  }}
                  onBlur={() => {
                    void autoSaveSettingsPatch({ auto_pause_5h_guard_band_percent: settingsForm.auto_pause_5h_guard_band_percent })
                  }}
                />
              </SettingField>
              <SettingField label={t('settings.autoPause5hGuardConcurrency')} description={t('settings.autoPause5hGuardConcurrencyHint')}>
                <Input
                  type="number"
                  min={0}
                  max={1000}
                  step={1}
                  inputMode="numeric"
                  value={settingsForm.auto_pause_5h_guard_concurrency ?? 1}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => {
                    const raw = e.target.value
                    if (raw === '') {
                      setSettingsForm(f => ({ ...f, auto_pause_5h_guard_concurrency: 0 }))
                      return
                    }
                    const parsed = Number.parseInt(raw, 10)
                    if (Number.isNaN(parsed)) return
                    const value = Math.max(0, Math.min(1000, parsed))
                    setSettingsForm(f => ({ ...f, auto_pause_5h_guard_concurrency: value }))
                  }}
                  onBlur={() => {
                    void autoSaveSettingsPatch({ auto_pause_5h_guard_concurrency: settingsForm.auto_pause_5h_guard_concurrency })
                  }}
                />
              </SettingField>
              <SettingField label={t('settings.smartPacingEnabled')} description={t('settings.smartPacingEnabledHint')}>
                <Select
                  value={settingsForm.smart_pacing_enabled ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('smart_pacing_enabled', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.smartPacingWindows')} description={t('settings.smartPacingWindowsHint')}>
                <Select
                  value={settingsForm.smart_pacing_windows || '5h,7d'}
                  onValueChange={(value) => {
                    setSettingsForm(f => ({ ...f, smart_pacing_windows: value }))
                    void autoSaveSettingsPatch({ smart_pacing_windows: value })
                  }}
                  options={[
                    { value: '5h,7d', label: t('settings.smartPacingWindowsBoth') },
                    { value: '5h', label: t('settings.smartPacingWindows5h') },
                    { value: '7d', label: t('settings.smartPacingWindows7d') },
                  ]}
                />
              </SettingField>
              <SettingField label={t('settings.smartPacingMinConcurrency')} description={t('settings.smartPacingMinConcurrencyHint')}>
                <Input
                  type="number"
                  min={1}
                  max={1000}
                  step={1}
                  inputMode="numeric"
                  value={settingsForm.smart_pacing_min_concurrency ?? 1}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => {
                    const raw = e.target.value
                    if (raw === '') {
                      setSettingsForm(f => ({ ...f, smart_pacing_min_concurrency: 1 }))
                      return
                    }
                    const parsed = Number.parseInt(raw, 10)
                    if (Number.isNaN(parsed)) return
                    const value = Math.max(1, Math.min(1000, parsed))
                    setSettingsForm(f => ({ ...f, smart_pacing_min_concurrency: value }))
                  }}
                  onBlur={() => {
                    void autoSaveSettingsPatch({ smart_pacing_min_concurrency: settingsForm.smart_pacing_min_concurrency })
                  }}
                />
              </SettingField>
            </div>
          </SettingsCard>

          <SettingsCard title={t('settings.codexWebsocket')} description={t('settings.codexWebsocketDesc')}>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(230px,1fr))] gap-4">
              <SettingField label={t('settings.codexForceWebsocket')} description={t('settings.codexForceWebsocketDesc')}>
                <Select
                  value={settingsForm.codex_force_websocket ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('codex_force_websocket', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.codexWSKeepaliveEnabled')} description={t('settings.codexWSKeepaliveEnabledDesc')}>
                <Select
                  value={settingsForm.codex_ws_keepalive_enabled ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('codex_ws_keepalive_enabled', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.codexWSKeepaliveInterval')} description={t('settings.codexWSKeepaliveIntervalDesc')}>
                <Input
                  type="number"
                  min={10}
                  max={600}
                  value={settingsForm.codex_ws_keepalive_interval_sec}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, codex_ws_keepalive_interval_sec: parseInt(e.target.value) || 60 }))}
                />
              </SettingField>
              <SettingField label={t('settings.codexWSHideUpstreamErrors')} description={t('settings.codexWSHideUpstreamErrorsDesc')}>
                <Select
                  value={settingsForm.codex_ws_hide_upstream_errors ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('codex_ws_hide_upstream_errors', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.codexWSSilentRetryEnabled')} description={t('settings.codexWSSilentRetryEnabledDesc')}>
                <Select
                  value={settingsForm.codex_ws_silent_retry_enabled ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('codex_ws_silent_retry_enabled', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.codexWSSilentMaxRetries')} description={t('settings.codexWSSilentMaxRetriesDesc')}>
                <Input
                  type="number"
                  min={0}
                  max={10}
                  value={settingsForm.codex_ws_silent_max_retries}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, codex_ws_silent_max_retries: parseInt(e.target.value) || 0 }))}
                />
              </SettingField>
            </div>
          </SettingsCard>

          <SettingsCard title={t('settings.runtimeOptimization')} description={t('settings.runtimeOptimizationDesc')}>
            <div className="grid grid-cols-[repeat(auto-fit,minmax(230px,1fr))] gap-4">
              <SettingField label={t('settings.clientCompatMode')} description={t('settings.clientCompatModeDesc')}>
                <Select
                  value={settingsForm.client_compat_mode}
                  onValueChange={(value) => autoSaveStringField('client_compat_mode', value)}
                  options={clientCompatOptions}
                />
              </SettingField>
              <SettingField label={t('settings.codexMinCliVersion')} description={t('settings.codexMinCliVersionDesc')}>
                <Input
                  value={settingsForm.codex_min_cli_version}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, codex_min_cli_version: e.target.value }))}
                />
              </SettingField>
              <SettingField label={t('settings.codexUserAgentRaw')} description={t('settings.codexUserAgentRawDesc')}>
                <Input
                  className="font-mono text-xs"
                  value={codexUserAgentConfig.raw_user_agent ?? ''}
                  placeholder="codex-tui/0.142.3 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.142.3)"
                  onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ raw_user_agent: e.target.value })}
                  onBlur={saveCodexUserAgentConfig}
                />
              </SettingField>
              <SettingField label={t('settings.codexUAClientName')} description={t('settings.codexUAClientNameDesc')}>
                <Input
                  value={codexUserAgentConfig.client_name ?? ''}
                  placeholder={DEFAULT_CODEX_UA_CONFIG.client_name}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ client_name: e.target.value })}
                  onBlur={saveCodexUserAgentConfig}
                />
              </SettingField>
              <SettingField label={t('settings.codexUAClientVersion')} description={t('settings.codexUAClientVersionDesc')}>
                <Input
                  value={codexUserAgentConfig.client_version ?? ''}
                  placeholder={DEFAULT_CODEX_UA_CONFIG.client_version}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ client_version: e.target.value })}
                  onBlur={saveCodexUserAgentConfig}
                />
              </SettingField>
              <SettingField label={t('settings.codexUAOSName')} description={t('settings.codexUAOSNameDesc')}>
                <Input
                  value={codexUserAgentConfig.os_name ?? ''}
                  placeholder={DEFAULT_CODEX_UA_CONFIG.os_name}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ os_name: e.target.value })}
                  onBlur={saveCodexUserAgentConfig}
                />
              </SettingField>
              <SettingField label={t('settings.codexUAOSVersion')} description={t('settings.codexUAOSVersionDesc')}>
                <Input
                  value={codexUserAgentConfig.os_version ?? ''}
                  placeholder={DEFAULT_CODEX_UA_CONFIG.os_version}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ os_version: e.target.value })}
                  onBlur={saveCodexUserAgentConfig}
                />
              </SettingField>
              <SettingField label={t('settings.codexUAArch')} description={t('settings.codexUAArchDesc')}>
                <Input
                  value={codexUserAgentConfig.arch ?? ''}
                  placeholder={DEFAULT_CODEX_UA_CONFIG.arch}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ arch: e.target.value })}
                  onBlur={saveCodexUserAgentConfig}
                />
              </SettingField>
              <SettingField label={t('settings.codexUATerminal')} description={t('settings.codexUATerminalDesc')}>
                <Input
                  value={codexUserAgentConfig.terminal ?? ''}
                  placeholder={DEFAULT_CODEX_UA_CONFIG.terminal}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ terminal: e.target.value })}
                  onBlur={saveCodexUserAgentConfig}
                />
              </SettingField>
              <div className="min-w-0 rounded-md border border-border/70 bg-muted/30 p-3 text-xs text-muted-foreground sm:col-span-2 lg:col-span-3">
                <div className="mb-1 font-medium text-foreground">{t('settings.codexUAPreview')}</div>
                <div className="break-all font-mono text-[11px] leading-5 text-foreground">{codexUserAgentPreview}</div>
              </div>
              <SettingField label={t('settings.usageLogMode')} description={t('settings.usageLogModeDesc')}>
                <Select
                  value={settingsForm.usage_log_mode}
                  onValueChange={(value) => autoSaveStringField('usage_log_mode', value)}
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
              <SettingField label={t('settings.billingTierPolicy')} description={t('settings.billingTierPolicyDesc')}>
                <Select
                  value={settingsForm.billing_tier_policy}
                  onValueChange={(value) => autoSaveStringField('billing_tier_policy', value)}
                  options={billingTierPolicyOptions}
                />
              </SettingField>
              <SettingField label={t('settings.streamFlushPolicy')} description={t('settings.streamFlushPolicyDesc')}>
                <Select
                  value={settingsForm.stream_flush_policy}
                  onValueChange={(value) => autoSaveStringField('stream_flush_policy', value)}
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
              <SettingField label={t('settings.firstTokenMode')} description={t('settings.firstTokenModeDesc')}>
                <Select
                  value={settingsForm.first_token_mode}
                  onValueChange={(value) => autoSaveStringField('first_token_mode', value)}
                  options={firstTokenModeOptions}
                />
              </SettingField>
              <SettingField label={t('settings.firstTokenTimeout')} description={t('settings.firstTokenTimeoutDesc')}>
                <Input
                  type="number"
                  min={0}
                  max={600}
                  value={settingsForm.first_token_timeout_seconds}
                  onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, first_token_timeout_seconds: parseInt(e.target.value) || 0 }))}
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
                      onValueChange={(value) => autoSaveBooleanField('image_s3_force_path_style', value)}
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
                  onValueChange={(value) => autoSaveBooleanField('auto_clean_unauthorized', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanRateLimited')} description={t('settings.autoCleanRateLimitedDesc')}>
                <Select
                  value={settingsForm.auto_clean_rate_limited ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('auto_clean_rate_limited', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanFullUsage')} description={t('settings.autoCleanFullUsageDesc')}>
                <Select
                  value={lazyModeActive ? 'false' : settingsForm.auto_clean_full_usage ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('auto_clean_full_usage', value)}
                  disabled={lazyModeActive}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanError')} description={t('settings.autoCleanErrorDesc')}>
                <Select
                  value={settingsForm.auto_clean_error ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('auto_clean_error', value)}
                  options={booleanOptions}
                />
              </SettingField>
              <SettingField label={t('settings.autoCleanExpired')} description={t('settings.autoCleanExpiredDesc')}>
                <Select
                  value={settingsForm.auto_clean_expired ? 'true' : 'false'}
                  onValueChange={(value) => autoSaveBooleanField('auto_clean_expired', value)}
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
                    onValueChange={(value) => autoSaveBooleanField('allow_remote_migration', value)}
                    options={booleanOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.promptFilterEnabled')} description={t('settings.promptFilterEnabledDesc')}>
                  <Select
                    value={settingsForm.prompt_filter_enabled ? 'true' : 'false'}
                    onValueChange={(value) => autoSaveBooleanField('prompt_filter_enabled', value)}
                    options={booleanOptions}
                  />
                </SettingField>
                <SettingField label={t('settings.promptFilterMode')} description={t('settings.promptFilterModeDesc')}>
                  <Select
                    value={settingsForm.prompt_filter_mode}
                    onValueChange={(value) => autoSaveStringField('prompt_filter_mode', value)}
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
                <SettingField label={t('settings.showFullUsageNumbers')} description={t('settings.showFullUsageNumbersDesc')}>
                  <Select
                    value={settingsForm.show_full_usage_numbers ? 'true' : 'false'}
                    onValueChange={(value) => autoSaveBooleanField('show_full_usage_numbers', value)}
                    options={booleanOptions}
                  />
                </SettingField>
              </div>
            </SettingsCard>
          </div>

          <SettingsCard title={t('settings.backgroundImage')} description={t('settings.backgroundImageDesc')}>
            <div className="grid gap-5 xl:grid-cols-[minmax(0,1.35fr)_minmax(340px,0.65fr)]">
              <div className="relative aspect-[16/7] min-h-[220px] overflow-hidden rounded-lg border border-border bg-muted/40 shadow-sm max-sm:aspect-[4/3]">
                {backgroundImagePreview && backgroundIsVideo ? (
                  <video
                    src={backgroundImagePreview}
                    className="size-full object-cover"
                    style={{
                      opacity: Math.min(100, Math.max(0, settingsForm.background_opacity)) / 100,
                      filter: settingsForm.background_blur > 0 ? `blur(${settingsForm.background_blur}px)` : undefined,
                      transform: settingsForm.background_blur > 0 ? 'scale(1.04)' : undefined,
                    }}
                    autoPlay
                    muted
                    loop
                    playsInline
                  />
                ) : backgroundImagePreview ? (
                  <img
                    src={backgroundImagePreview}
                    alt=""
                    className="size-full object-cover"
                    style={{
                      opacity: Math.min(100, Math.max(0, settingsForm.background_opacity)) / 100,
                      filter: settingsForm.background_blur > 0 ? `blur(${settingsForm.background_blur}px)` : undefined,
                      transform: settingsForm.background_blur > 0 ? 'scale(1.04)' : undefined,
                    }}
                  />
                ) : (
                  <div className="flex size-full items-center justify-center text-xs font-medium text-muted-foreground">
                    {t('settings.backgroundImageEmpty')}
                  </div>
                )}
              </div>
              <div className="flex min-w-0 flex-col justify-between gap-5">
                <div className="min-w-0 space-y-3">
                  <Input
                    value={settingsForm.background_image}
                    placeholder="/wallpaper.jpg or https://..."
                    onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_image: e.target.value }))}
                  />
                  <div className="flex flex-wrap gap-2">
                    <Button type="button" variant="outline" size="sm" onClick={() => backgroundFileInputRef.current?.click()}>
                      <Upload className="size-4" />
                      {t('settings.backgroundImageUpload')}
                    </Button>
                    <Button type="button" variant="ghost" size="sm" onClick={() => setSettingsForm(f => ({ ...f, background_image: '' }))}>
                      <X className="size-4" />
                      {t('settings.backgroundImageReset')}
                    </Button>
                  </div>
                  <input
                    ref={backgroundFileInputRef}
                    type="file"
                    accept="image/png,image/jpeg,image/webp,image/svg+xml,video/mp4,.png,.jpg,.jpeg,.webp,.svg,.mp4"
                    className="hidden"
                    onChange={handleBackgroundImageUpload}
                  />
                </div>
                <div className="grid gap-4">
                  <div className="space-y-2">
                    <div className="flex items-center justify-between gap-3 text-xs font-semibold text-muted-foreground">
                      <span>{t('settings.backgroundOpacity')}</span>
                      <span>{settingsForm.background_opacity}%</span>
                    </div>
                    <input
                      type="range"
                      min={0}
                      max={100}
                      value={settingsForm.background_opacity}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_opacity: parseInt(e.target.value) || 0 }))}
                      className="w-full accent-primary"
                    />
                  </div>
                  <div className="space-y-2">
                    <div className="flex items-center justify-between gap-3 text-xs font-semibold text-muted-foreground">
                      <span>{t('settings.backgroundBlur')}</span>
                      <span>{settingsForm.background_blur}px</span>
                    </div>
                    <input
                      type="range"
                      min={0}
                      max={24}
                      value={settingsForm.background_blur}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_blur: parseInt(e.target.value) || 0 }))}
                      className="w-full accent-primary"
                    />
                  </div>
                  <div className="space-y-2">
                    <div className="flex items-center justify-between gap-3 text-xs font-semibold text-muted-foreground">
                      <span>{t('settings.backgroundGlassOpacity')}</span>
                      <span>{settingsForm.background_glass_opacity}%</span>
                    </div>
                    <input
                      type="range"
                      min={0}
                      max={100}
                      value={settingsForm.background_glass_opacity}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_glass_opacity: parseInt(e.target.value) || 0 }))}
                      className="w-full accent-primary"
                    />
                  </div>
                  <div className="space-y-2">
                    <div className="flex items-center justify-between gap-3 text-xs font-semibold text-muted-foreground">
                      <span>{t('settings.backgroundGlassBlur')}</span>
                      <span>{settingsForm.background_glass_blur}px</span>
                    </div>
                    <input
                      type="range"
                      min={0}
                      max={20}
                      value={settingsForm.background_glass_blur}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_glass_blur: parseInt(e.target.value) || 0 }))}
                      className="w-full accent-primary"
                    />
                  </div>
                </div>
              </div>
            </div>
          </SettingsCard>

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
                        {model.source === 'official_codex_docs'
                          ? t('settings.modelSourceOfficial')
                          : model.source === 'reasoning_effort'
                            ? t('settings.modelSourceReasoning')
                            : t('settings.modelSourceBuiltin')}
                      </Badge>
                      {model.pro_only ? <Badge variant="outline" className="text-[11px]">{t('settings.modelProOnly')}</Badge> : null}
                      {model.category === 'image' ? <Badge variant="outline" className="text-[11px]">{t('settings.modelImage')}</Badge> : null}
                    </div>
                  ))}
                </div>
              </div>
            </SettingsCard>

            <SettingsCard
              title={t('settings2.anthropicModelMapping')}
              description={t('settings2.anthropicModelMappingDesc')}
              className="h-full xl:h-[430px]"
              contentClassName="flex h-full min-h-0 flex-col"
            >
              <ModelMappingEditor
                value={settingsForm.model_mapping}
                onChange={(v) => setSettingsForm(f => ({ ...f, model_mapping: v }))}
                fallbackEntries={defaultClaudeModelMappingEntries}
                sourceLabel={t('settings2.anthropicModel')}
                targetLabel={t('settings2.codexModel')}
                sourcePlaceholder="claude-opus-4-6"
                targetPlaceholder="gpt-5.5"
              />
            </SettingsCard>

            <SettingsCard
              title={t('settings2.codexModelMapping')}
              description={t('settings2.codexModelMappingDesc')}
              className="h-full xl:h-[430px]"
              contentClassName="flex h-full min-h-0 flex-col"
            >
              <ModelMappingEditor
                value={settingsForm.codex_model_mapping}
                onChange={(v) => setSettingsForm(f => ({ ...f, codex_model_mapping: v }))}
                sourceOptions={codexModelOptions}
                targetOptions={codexModelOptions}
                sourceLabel={t('settings2.requestedModel')}
                targetLabel={t('settings2.targetModel')}
                sourcePlaceholder="gpt-5.2"
                targetPlaceholder="gpt-5.5"
              />
            </SettingsCard>

            <SettingsCard
              title={t('settings2.reasoningEffortModels')}
              description={t('settings2.reasoningEffortModelsDesc')}
              className="h-full xl:h-[430px]"
              contentClassName="flex h-full min-h-0 flex-col"
            >
              <ReasoningEffortModelsEditor
                value={settingsForm.reasoning_effort_models}
                onChange={(v) => setSettingsForm(f => ({ ...f, reasoning_effort_models: v }))}
                baseModelOptions={textModelOptions}
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

      </>
    </StateShell>
  )
}
