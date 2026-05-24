import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { NavLink, useNavigate, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import { formatBeijingTime } from '../utils/time'
import type { APIKeyRow, CreateImageJobPayload, ImageAsset, ImageGenerationJob, ImagePromptTemplate, ImagePromptTemplatePayload } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { Copy, Download, Eye, Image as ImageIcon, Loader2, Pencil, Play, Plus, RefreshCcw, Save, Search, Sparkles, Star, Trash2, Upload, X } from 'lucide-react'

const IMAGE_VIEWS = ['studio', 'prompts', 'gallery', 'history'] as const
type ImageView = typeof IMAGE_VIEWS[number]
const IMAGE_ASSET_PAGE_SIZE = 16
const IMAGE_JOB_HISTORY_PAGE_SIZE = 20
const IMAGE_JOB_STATUSES = ['queued', 'running', 'succeeded', 'failed'] as const
type ImageJobStatusFilter = 'all' | typeof IMAGE_JOB_STATUSES[number]
const IMAGE_ASSET_CACHE_DB = 'codex2api-image-assets'
const IMAGE_ASSET_CACHE_STORE = 'assets'
const IMAGE_ASSET_CACHE_VERSION = 1
const IMAGE_MODEL_2K_ALIAS = 'gpt-image-2-2k'
const IMAGE_MODEL_4K_ALIAS = 'gpt-image-2-4k'
const IMAGE_NOTICE_KEYS = [
  'images.notices.pngFallback',
  'images.notices.transparent',
  'images.notices.highQuality',
  'images.notices.accountRouting',
]

type TemplateEditorDraft = {
  id: number | null
  name: string
  tags: string
  prompt: string
  model: string
  size: string
  quality: string
  outputFormat: string
  background: string
  style: string
}

const IMAGE_MODELS = [
  { label: 'gpt-image-2', value: 'gpt-image-2' },
  { label: IMAGE_MODEL_2K_ALIAS, value: IMAGE_MODEL_2K_ALIAS },
  { label: IMAGE_MODEL_4K_ALIAS, value: IMAGE_MODEL_4K_ALIAS },
]

const SIZE_OPTIONS = [
  { label: 'Auto', value: 'auto' },
  { label: '1024x1024', value: '1024x1024' },
  { label: '1536x864', value: '1536x864' },
  { label: '864x1536', value: '864x1536' },
  { label: '2048x2048', value: '2048x2048' },
  { label: '2560x1440', value: '2560x1440' },
  { label: '1440x2560', value: '1440x2560' },
  { label: '3840x2160', value: '3840x2160' },
  { label: '2160x3840', value: '2160x3840' },
  { label: '2880x2880', value: '2880x2880' },
]

const SIZE_2K_VALUES = new Set(['auto', '2048x2048', '2560x1440', '1440x2560'])
const SIZE_4K_VALUES = new Set(['auto', '3840x2160', '2160x3840', '2880x2880'])

const QUALITY_OPTIONS = [
  { label: 'Auto', value: 'auto' },
  { label: 'High', value: 'high' },
  { label: 'Medium', value: 'medium' },
  { label: 'Low', value: 'low' },
]

const FORMAT_OPTIONS = [
  { label: 'PNG', value: 'png' },
  { label: 'WebP', value: 'webp' },
  { label: 'JPEG', value: 'jpeg' },
]

const UPSCALE_VALUES = ['', '2k', '4k'] as const

const STYLE_PRESETS = [
  {
    id: 'cinematic',
    value: 'Cinematic realistic photography, natural light, subtle film grain, rich but controlled color grading, soft shadows, professional composition, high detail.',
  },
  {
    id: 'commerce',
    value: 'Clean commercial product photography, premium studio lighting, crisp edges, realistic materials, neutral background, catalog-ready composition, high detail.',
  },
  {
    id: 'sticker',
    value: 'Cute sticker illustration, bold clean outline, simple readable shapes, vibrant colors, playful expression, isolated subject, transparent-background friendly.',
  },
  {
    id: 'toy',
    value: 'Premium 3D designer toy style, soft rounded forms, glossy vinyl material, studio render lighting, collectible figure presentation, charming details.',
  },
  {
    id: 'icon',
    value: 'Modern flat vector icon style, geometric shapes, simple silhouette, balanced negative space, clean edges, limited color palette, app-icon ready.',
  },
  {
    id: 'poster',
    value: 'Vintage editorial poster style, bold typography space, textured print grain, strong focal composition, retro color palette, dramatic visual hierarchy.',
  },
  {
    id: 'anime',
    value: 'Polished anime illustration style, expressive character design, clean line art, soft cel shading, luminous color accents, detailed atmosphere.',
  },
  {
    id: 'wallpaper',
    value: 'Minimal premium wallpaper style, spacious composition, refined lighting, elegant color contrast, calm background depth, suitable for desktop or mobile wallpaper.',
  },
] as const

function normalizeImageView(value?: string): ImageView {
  return IMAGE_VIEWS.includes(value as ImageView) ? value as ImageView : 'studio'
}

function formatBytes(bytes?: number): string {
  if (!bytes || bytes <= 0) return '-'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

function formatDuration(ms?: number): string {
  if (!ms || ms <= 0) return '-'
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

function parseTags(value: string): string[] {
  return value.split(/[,，\s]+/).map(tag => tag.trim()).filter(Boolean).slice(0, 12)
}

function tagsToText(tags?: string[]): string {
  return (tags ?? []).join(', ')
}

function sizeOptionsForModel(model: string) {
  switch (model) {
    case IMAGE_MODEL_2K_ALIAS:
      return SIZE_OPTIONS.filter(option => SIZE_2K_VALUES.has(option.value))
    case IMAGE_MODEL_4K_ALIAS:
      return SIZE_OPTIONS.filter(option => SIZE_4K_VALUES.has(option.value))
    default:
      return SIZE_OPTIONS
  }
}

function normalizeImageSizeForModel(model: string, size: string): string {
  const value = stringsTrimOrAuto(size)
  return sizeOptionsForModel(model).some(option => option.value === value) ? value : 'auto'
}

function stringsTrimOrAuto(value: string): string {
  return value.trim() || 'auto'
}

function emptyTemplateDraft(): TemplateEditorDraft {
  return {
    id: null,
    name: '',
    tags: '',
    prompt: '',
    model: 'gpt-image-2',
    size: 'auto',
    quality: 'auto',
    outputFormat: 'png',
    background: 'auto',
    style: '',
  }
}

function templateDraftFromTemplate(template: ImagePromptTemplate): TemplateEditorDraft {
  const model = template.model || 'gpt-image-2'
  return {
    id: template.id,
    name: template.name,
    tags: tagsToText(template.tags),
    prompt: template.prompt,
    model,
    size: normalizeImageSizeForModel(model, template.size || 'auto'),
    quality: template.quality || 'auto',
    outputFormat: template.output_format || 'png',
    background: template.background || 'auto',
    style: template.style || '',
  }
}

function assetResolution(asset: ImageAsset): string {
  return asset.actual_size || (asset.width > 0 && asset.height > 0 ? `${asset.width}x${asset.height}` : asset.requested_size || '-')
}

function imageAssetFormat(asset: ImageAsset): string {
  const outputFormat = asset.output_format?.trim()
  if (outputFormat) return outputFormat.toUpperCase()
  const mimeType = asset.mime_type?.trim()
  if (mimeType) return mimeType.replace(/^image\//i, '').toUpperCase()
  return '-'
}

function jobParams(job?: ImageGenerationJob | null): Partial<CreateImageJobPayload> {
  if (!job?.params_json) return {}
  try {
    return JSON.parse(job.params_json) as Partial<CreateImageJobPayload>
  } catch {
    return {}
  }
}

function jobStatusClass(status: string): string {
  switch (status) {
    case 'succeeded':
      return 'border-transparent bg-emerald-500/14 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300'
    case 'failed':
      return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
    case 'running':
      return 'border-transparent bg-blue-500/14 text-blue-600 dark:bg-blue-500/20 dark:text-blue-300'
    default:
      return 'border-transparent bg-slate-500/14 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300'
  }
}

function isImageJobBusy(job: ImageGenerationJob): boolean {
  return job.status === 'queued' || job.status === 'running'
}

function jobModel(job: ImageGenerationJob): string {
  const params = jobParams(job)
  return params.model || job.assets?.[0]?.model || '-'
}

function jobRequestedSize(job: ImageGenerationJob): string {
  const params = jobParams(job)
  return params.size || job.assets?.[0]?.requested_size || job.assets?.[0]?.actual_size || 'Auto'
}

function assetDisplayFrameClass(asset: ImageAsset, compact: boolean, gallery: boolean): string {
  if (gallery) return 'h-40 sm:h-44 xl:h-48'
  if (!compact) return 'aspect-[4/3]'

  const ratio = asset.width > 0 && asset.height > 0 ? asset.width / asset.height : 0
  if (ratio >= 1.45) return 'aspect-video'
  if (ratio >= 1.12) return 'aspect-[4/3]'
  if (ratio > 0.88) return 'aspect-square'
  if (ratio > 0.68) return 'aspect-[4/5]'
  return 'aspect-[3/4]'
}

function normalizeUpscale(value?: string): string {
  const normalized = (value || '').trim().toLowerCase()
  return UPSCALE_VALUES.includes(normalized as typeof UPSCALE_VALUES[number]) ? normalized : ''
}

function assetThumbnailURL(asset: ImageAsset, imageURLs: Record<number, string>): string | undefined {
  return asset.thumbnail_url || asset.proxy_url || imageURLs[asset.id]
}

function assetPreviewURL(asset: ImageAsset, imageURLs: Record<number, string>): string | undefined {
  return asset.proxy_url || imageURLs[asset.id] || asset.thumbnail_url
}

function hasServerImageURL(asset: ImageAsset): boolean {
  return Boolean(asset.thumbnail_url || asset.proxy_url)
}

type CachedImageAsset = {
  id: number
  blob: Blob
  mimeType: string
  bytes: number
  updatedAt: number
}

let imageAssetCacheDBPromise: Promise<IDBDatabase | null> | null = null

function openImageAssetCacheDB(): Promise<IDBDatabase | null> {
  if (typeof window === 'undefined' || !window.indexedDB) return Promise.resolve(null)
  if (imageAssetCacheDBPromise) return imageAssetCacheDBPromise
  imageAssetCacheDBPromise = new Promise(resolve => {
    const request = window.indexedDB.open(IMAGE_ASSET_CACHE_DB, IMAGE_ASSET_CACHE_VERSION)
    request.onupgradeneeded = () => {
      const db = request.result
      if (!db.objectStoreNames.contains(IMAGE_ASSET_CACHE_STORE)) {
        db.createObjectStore(IMAGE_ASSET_CACHE_STORE, { keyPath: 'id' })
      }
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => resolve(null)
    request.onblocked = () => resolve(null)
  })
  return imageAssetCacheDBPromise
}

async function readCachedImageAsset(id: number): Promise<Blob | null> {
  const db = await openImageAssetCacheDB()
  if (!db) return null
  return new Promise(resolve => {
    const tx = db.transaction(IMAGE_ASSET_CACHE_STORE, 'readwrite')
    const store = tx.objectStore(IMAGE_ASSET_CACHE_STORE)
    const request = store.get(id)
    request.onsuccess = () => {
      const record = request.result as CachedImageAsset | undefined
      if (!record?.blob) {
        resolve(null)
        return
      }
      try {
        store.put({ ...record, updatedAt: Date.now() })
      } catch {
        // Cache metadata refresh is best-effort.
      }
      resolve(record.blob)
    }
    request.onerror = () => resolve(null)
  })
}

async function writeCachedImageAsset(asset: ImageAsset, blob: Blob): Promise<void> {
  const db = await openImageAssetCacheDB()
  if (!db) return
  await new Promise<void>(resolve => {
    const tx = db.transaction(IMAGE_ASSET_CACHE_STORE, 'readwrite')
    const store = tx.objectStore(IMAGE_ASSET_CACHE_STORE)
    const record: CachedImageAsset = {
      id: asset.id,
      blob,
      mimeType: blob.type || asset.mime_type || 'application/octet-stream',
      bytes: blob.size || asset.bytes || 0,
      updatedAt: Date.now(),
    }
    const request = store.put(record)
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    tx.onerror = () => resolve()
  })
}

function blobFromInlineImageAsset(asset: ImageAsset): Blob | null {
  const raw = asset.cache_b64_json?.trim()
  if (!raw) return null
  try {
    const normalized = raw.replace(/\s+/g, '')
    const binary = window.atob(normalized)
    const chunkSize = 8192
    const chunks: BlobPart[] = []
    for (let offset = 0; offset < binary.length; offset += chunkSize) {
      const slice = binary.slice(offset, offset + chunkSize)
      const bytes = new Uint8Array(slice.length)
      for (let idx = 0; idx < slice.length; idx += 1) {
        bytes[idx] = slice.charCodeAt(idx)
      }
      chunks.push(bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer)
    }
    return new Blob(chunks, { type: asset.mime_type || 'application/octet-stream' })
  } catch {
    return null
  }
}

async function deleteCachedImageAsset(id: number): Promise<void> {
  const db = await openImageAssetCacheDB()
  if (!db) return
  await new Promise<void>(resolve => {
    const tx = db.transaction(IMAGE_ASSET_CACHE_STORE, 'readwrite')
    const request = tx.objectStore(IMAGE_ASSET_CACHE_STORE).delete(id)
    request.onsuccess = () => resolve()
    request.onerror = () => resolve()
    tx.onerror = () => resolve()
  })
}

export default function ImageStudio() {
  const { t } = useTranslation()
  const { view } = useParams()
  const navigate = useNavigate()
  const activeView = normalizeImageView(view)
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [templates, setTemplates] = useState<ImagePromptTemplate[]>([])
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([])
  const [jobs, setJobs] = useState<ImageGenerationJob[]>([])
  const [historyJobs, setHistoryJobs] = useState<ImageGenerationJob[]>([])
  const [historyTotal, setHistoryTotal] = useState(0)
  const [historyPage, setHistoryPage] = useState(1)
  const [historyStatusFilter, setHistoryStatusFilter] = useState<ImageJobStatusFilter>('all')
  const [historyLoading, setHistoryLoading] = useState(false)
  const [assets, setAssets] = useState<ImageAsset[]>([])
  const [assetTotal, setAssetTotal] = useState(0)
  const [assetPage, setAssetPage] = useState(1)
  const [assetURLs, setAssetURLs] = useState<Record<number, string>>({})
  const assetURLsRef = useRef<Record<number, string>>({})
  const activeAssetIDsRef = useRef<Set<number>>(new Set())
  const assetURLRequestsRef = useRef<Set<number>>(new Set())
  const [previewAsset, setPreviewAsset] = useState<ImageAsset | null>(null)
  const [currentJob, setCurrentJob] = useState<ImageGenerationJob | null>(null)
  const [loading, setLoading] = useState(true)
  const [submitting, setSubmitting] = useState(false)
  const [templateSearch, setTemplateSearch] = useState('')
  const [selectedTag, setSelectedTag] = useState('')
  const [selectedTemplateId, setSelectedTemplateId] = useState<number | null>(null)
  const [templateDialogOpen, setTemplateDialogOpen] = useState(false)
  const [templateDialogDraft, setTemplateDialogDraft] = useState<TemplateEditorDraft>(() => emptyTemplateDraft())
  const [templateDialogSaving, setTemplateDialogSaving] = useState(false)

  const [prompt, setPrompt] = useState('')
  const [model, setModel] = useState('gpt-image-2')
  const [size, setSize] = useState('auto')
  const [quality, setQuality] = useState('auto')
  const [outputFormat, setOutputFormat] = useState('png')
  const [background, setBackground] = useState('auto')
  const [upscale, setUpscale] = useState('')
  const [style, setStyle] = useState('')
  const [apiKeyID, setAPIKeyID] = useState('')
  const [templateName, setTemplateName] = useState('')
  const [templateTags, setTemplateTags] = useState('')
  const [imageToImageMode, setImageToImageMode] = useState(false)
  const [inputImageDataURLs, setInputImageDataURLs] = useState<string[]>([])

  const handleImageFileChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files
    if (!files || files.length === 0) return
    const MAX_INPUT_IMAGES = 10
    const currentCount = inputImageDataURLs.length
    if (currentCount >= MAX_INPUT_IMAGES) {
      showToast(t('images.maxInputImages', { max: MAX_INPUT_IMAGES }), 'error')
      e.target.value = ''
      return
    }
    const remaining = MAX_INPUT_IMAGES - currentCount
    const filesToRead = Array.from(files).slice(0, remaining)
    const reads: Promise<string>[] = []
    for (let i = 0; i < filesToRead.length; i++) {
      reads.push(new Promise<string>((resolve, reject) => {
        const reader = new FileReader()
        reader.onload = () => resolve(reader.result as string)
        reader.onerror = () => reject(new Error('Failed to read file'))
        reader.readAsDataURL(filesToRead[i])
      }))
    }
    Promise.allSettled(reads).then(results => {
      const dataURLs: string[] = []
      for (const r of results) {
        if (r.status === 'fulfilled') dataURLs.push(r.value)
      }
      if (dataURLs.length > 0) {
        setInputImageDataURLs(prev => [...prev, ...dataURLs].slice(0, MAX_INPUT_IMAGES))
      }
      if (dataURLs.length < results.length) {
        showToast(t('images.loadFailed'), 'error')
      }
    })
    e.target.value = ''
  }, [showToast, t, inputImageDataURLs])

  useEffect(() => {
    if (view && !IMAGE_VIEWS.includes(view as ImageView)) {
      navigate('/images/studio', { replace: true })
    }
  }, [navigate, view])

  const visibleAssets = useMemo(() => {
    const historyAssets = activeView === 'history' ? historyJobs.flatMap(job => job.assets ?? []) : []
    const merged = [...(currentJob?.assets ?? []), ...assets, ...historyAssets]
    const seen = new Set<number>()
    return merged.filter(asset => {
      if (seen.has(asset.id)) return false
      seen.add(asset.id)
      return true
    })
  }, [activeView, assets, currentJob, historyJobs])

  const allTags = useMemo(() => {
    const tags = new Set<string>()
    templates.forEach(template => template.tags.forEach(tag => tags.add(tag)))
    return Array.from(tags).sort((a, b) => a.localeCompare(b))
  }, [templates])
  const loadTemplates = useCallback(async () => {
    const res = await api.getImagePromptTemplates({ q: templateSearch || undefined, tag: selectedTag || undefined })
    setTemplates(res.templates ?? [])
  }, [selectedTag, templateSearch])

  const loadJobs = useCallback(async () => {
    const res = await api.getImageJobs({ page: 1, pageSize: 3 })
    setJobs(res.jobs ?? [])
  }, [])

  const loadHistoryJobs = useCallback(async () => {
    setHistoryLoading(true)
    try {
      const res = await api.getImageJobs({ page: historyPage, pageSize: IMAGE_JOB_HISTORY_PAGE_SIZE })
      setHistoryJobs(res.jobs ?? [])
      setHistoryTotal(res.total ?? 0)
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.loadFailed'), 'error')
    } finally {
      setHistoryLoading(false)
    }
  }, [historyPage, showToast, t])

  const loadAssets = useCallback(async () => {
    const res = await api.getImageAssets({ page: assetPage, pageSize: IMAGE_ASSET_PAGE_SIZE })
    setAssets(res.assets ?? [])
    setAssetTotal(res.total ?? 0)
  }, [assetPage])

  const loadInitial = useCallback(async () => {
    setLoading(true)
    try {
      const [keysRes] = await Promise.all([
        api.getAPIKeys(),
        loadTemplates(),
        loadJobs(),
        loadAssets(),
      ])
      setAPIKeys(keysRes.keys ?? [])
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.loadFailed'), 'error')
    } finally {
      setLoading(false)
    }
  }, [loadAssets, loadJobs, loadTemplates, showToast, t])

  useEffect(() => {
    void loadInitial()
  }, [loadInitial])

  useEffect(() => {
    if (activeView === 'history') {
      void loadHistoryJobs()
    }
  }, [activeView, loadHistoryJobs])

  useEffect(() => {
    assetURLsRef.current = assetURLs
  }, [assetURLs])

  useEffect(() => {
    return () => {
      Object.values(assetURLsRef.current).forEach(url => URL.revokeObjectURL(url))
      assetURLsRef.current = {}
      activeAssetIDsRef.current.clear()
      assetURLRequestsRef.current.clear()
    }
  }, [])

  useEffect(() => {
    const activeIDs = new Set(visibleAssets.map(asset => asset.id))
    const serverURLIDs = new Set(visibleAssets.filter(hasServerImageURL).map(asset => asset.id))
    activeAssetIDsRef.current = activeIDs

    setAssetURLs(prev => {
      let changed = false
      const next = { ...prev }
      for (const [id, url] of Object.entries(prev)) {
        const assetID = Number(id)
        if (!activeIDs.has(assetID) || serverURLIDs.has(assetID)) {
          URL.revokeObjectURL(url)
          delete next[assetID]
          assetURLRequestsRef.current.delete(assetID)
          changed = true
        }
      }
      if (changed) {
        assetURLsRef.current = next
      }
      return changed ? next : prev
    })

    visibleAssets.forEach(asset => {
      if (hasServerImageURL(asset)) return
      if (assetURLsRef.current[asset.id] || assetURLRequestsRef.current.has(asset.id)) return
      assetURLRequestsRef.current.add(asset.id)
      void (async () => {
        let blob = blobFromInlineImageAsset(asset)
        if (blob) {
          await writeCachedImageAsset(asset, blob)
        }
        if (!blob) {
          blob = await readCachedImageAsset(asset.id)
        }
        if (!blob) {
          try {
            blob = await api.getImageAssetFile(asset.id)
            await writeCachedImageAsset(asset, blob)
          } catch {
            blob = null
          }
        }
        if (!blob || !activeAssetIDsRef.current.has(asset.id)) return
        const url = URL.createObjectURL(blob)
        setAssetURLs(prev => {
          if (prev[asset.id]) {
            URL.revokeObjectURL(url)
            return prev
          }
          const next = { ...prev, [asset.id]: url }
          assetURLsRef.current = next
          return next
        })
      })().finally(() => {
        assetURLRequestsRef.current.delete(asset.id)
      })
    })
  }, [visibleAssets])

  useEffect(() => {
    if (!currentJob || !['queued', 'running'].includes(currentJob.status)) return
    const timer = window.setInterval(async () => {
      try {
        const res = await api.getImageJob(currentJob.id, { includeCache: true })
        setCurrentJob(res.job)
        if (!['queued', 'running'].includes(res.job.status)) {
          await Promise.all([loadJobs(), loadAssets(), loadTemplates(), loadHistoryJobs()])
        }
      } catch {
        // keep polling quiet; the visible job state is enough context
      }
    }, 2500)
    return () => window.clearInterval(timer)
  }, [currentJob, loadAssets, loadHistoryJobs, loadJobs, loadTemplates])

  const promptForAsset = useCallback((asset: ImageAsset) => {
    const job = [...jobs, ...historyJobs].find(item => item.id === asset.job_id)
    if (job) return job.prompt
    if (currentJob?.id === asset.job_id) return currentJob.prompt
    return asset.revised_prompt || ''
  }, [currentJob, historyJobs, jobs])

  const fillTemplate = (template: ImagePromptTemplate) => {
    const nextModel = template.model || 'gpt-image-2'
    setSelectedTemplateId(template.id)
    setPrompt(template.prompt)
    setModel(nextModel)
    setSize(normalizeImageSizeForModel(nextModel, template.size || 'auto'))
    setQuality(template.quality || 'auto')
    setOutputFormat(template.output_format || 'png')
    setBackground(template.background || 'auto')
    setStyle(template.style || '')
    setTemplateName(template.name)
    setTemplateTags(tagsToText(template.tags))
  }

  const applyTemplate = (template: ImagePromptTemplate) => {
    fillTemplate(template)
    navigate('/images/studio')
  }

  const selectTemplateForGeneration = (value: string) => {
    if (!value) {
      setSelectedTemplateId(null)
      return
    }
    const template = templates.find(item => item.id === Number(value))
    if (!template) return
    fillTemplate(template)
  }

  const openNewTemplateDialog = () => {
    setTemplateDialogDraft(emptyTemplateDraft())
    setTemplateDialogOpen(true)
  }

  const openEditTemplateDialog = (template: ImagePromptTemplate) => {
    setTemplateDialogDraft(templateDraftFromTemplate(template))
    setTemplateDialogOpen(true)
  }

  const updateTemplateDialogDraft = (patch: Partial<TemplateEditorDraft>) => {
    setTemplateDialogDraft(prev => ({ ...prev, ...patch }))
  }

  const saveCurrentPromptAsTemplate = async () => {
    if (!prompt.trim()) {
      showToast(t('images.promptRequired'), 'error')
      return
    }
    const payload: ImagePromptTemplatePayload = {
      name: templateName.trim() || prompt.trim().slice(0, 24) || t('images.untitledTemplate'),
      prompt,
      model,
      size: size === 'auto' ? '' : size,
      quality: quality === 'auto' ? '' : quality,
      output_format: outputFormat,
      background: background === 'auto' ? '' : background,
      style,
      tags: parseTags(templateTags),
    }
    try {
      await api.createImagePromptTemplate(payload)
      showToast(t('images.templateSaved'), 'success')
      setTemplateName('')
      setTemplateTags('')
      await loadTemplates()
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.saveFailed'), 'error')
    }
  }

  const saveTemplateDialog = async () => {
    if (!templateDialogDraft.prompt.trim()) {
      showToast(t('images.promptRequired'), 'error')
      return
    }
    const payload: ImagePromptTemplatePayload = {
      name: templateDialogDraft.name.trim() || templateDialogDraft.prompt.trim().slice(0, 24) || t('images.untitledTemplate'),
      prompt: templateDialogDraft.prompt,
      model: templateDialogDraft.model,
      size: templateDialogDraft.size === 'auto' ? '' : templateDialogDraft.size,
      quality: templateDialogDraft.quality === 'auto' ? '' : templateDialogDraft.quality,
      output_format: templateDialogDraft.outputFormat,
      background: templateDialogDraft.background === 'auto' ? '' : templateDialogDraft.background,
      style: templateDialogDraft.style,
      tags: parseTags(templateDialogDraft.tags),
    }
    setTemplateDialogSaving(true)
    try {
      if (templateDialogDraft.id) {
        await api.updateImagePromptTemplate(templateDialogDraft.id, payload)
        showToast(t('images.templateUpdated'), 'success')
      } else {
        await api.createImagePromptTemplate(payload)
        showToast(t('images.templateSaved'), 'success')
      }
      setTemplateDialogOpen(false)
      setTemplateDialogDraft(emptyTemplateDraft())
      await loadTemplates()
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.saveFailed'), 'error')
    } finally {
      setTemplateDialogSaving(false)
    }
  }

  const toggleFavorite = async (template: ImagePromptTemplate) => {
    try {
      await api.updateImagePromptTemplate(template.id, {
        name: template.name,
        prompt: template.prompt,
        model: template.model,
        size: template.size,
        quality: template.quality,
        output_format: template.output_format,
        background: template.background,
        style: template.style,
        tags: template.tags,
        favorite: !template.favorite,
      })
      await loadTemplates()
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.saveFailed'), 'error')
    }
  }

  const deleteTemplate = async (template: ImagePromptTemplate) => {
    const ok = await confirm({
      title: t('images.deleteTemplateTitle'),
      description: template.name,
      confirmText: t('common.delete'),
      tone: 'destructive',
    })
    if (!ok) return
    try {
      await api.deleteImagePromptTemplate(template.id)
      if (selectedTemplateId === template.id) setSelectedTemplateId(null)
      if (templateDialogDraft.id === template.id) {
        setTemplateDialogOpen(false)
        setTemplateDialogDraft(emptyTemplateDraft())
      }
      await loadTemplates()
      showToast(t('images.templateDeleted'), 'success')
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.deleteFailed'), 'error')
    }
  }

  const createJobPayload = (sourcePrompt = prompt): CreateImageJobPayload => {
    const payload: CreateImageJobPayload = {
      prompt: sourcePrompt,
      model,
      output_format: outputFormat,
    }
    if (size !== 'auto') payload.size = size
    if (quality !== 'auto') payload.quality = quality
    if (background !== 'auto') payload.background = background
    if (upscale) payload.upscale = upscale
    if (style.trim()) payload.style = style.trim()
    if (apiKeyID) payload.api_key_id = Number(apiKeyID)
    if (selectedTemplateId) payload.template_id = selectedTemplateId
    if (imageToImageMode && inputImageDataURLs.length > 0) payload.input_images = inputImageDataURLs
    return payload
  }

  const submitJob = async (payload = createJobPayload(), forceMode?: 'text' | 'edit') => {
    const isEditMode = forceMode != null
      ? forceMode === 'edit'
      : Array.isArray(payload.input_images) && payload.input_images.length > 0
    if (!payload.prompt.trim()) {
      showToast(t('images.promptRequired'), 'error')
      return
    }
    if (isEditMode && (!payload.input_images || payload.input_images.length === 0)) {
      showToast(t('images.inputImageRequired'), 'error')
      return
    }
    setSubmitting(true)
    try {
      const res = isEditMode
        ? await api.createImageEditJob(payload)
        : await api.createImageJob(payload)
      setCurrentJob(res.job)
      await loadJobs()
      showToast(t('images.jobCreated'), 'success')
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.createJobFailed'), 'error')
    } finally {
      setSubmitting(false)
    }
  }

  const rerunFromJob = (job: ImageGenerationJob) => {
    const params = jobParams(job)
    const nextModel = params.model || 'gpt-image-2'
    const nextSize = normalizeImageSizeForModel(nextModel, params.size || 'auto')
    const isEditJob = params.input_images && params.input_images.length > 0
    setPrompt(job.prompt)
    setModel(nextModel)
    setSize(nextSize)
    setQuality(params.quality || 'auto')
    setOutputFormat(params.output_format || 'png')
    setBackground(params.background || 'auto')
    setUpscale(normalizeUpscale(params.upscale))
    setStyle(params.style || '')
    setSelectedTemplateId(params.template_id ? Number(params.template_id) : null)
    if (isEditJob) {
      setImageToImageMode(true)
      setInputImageDataURLs(params.input_images!)
    } else {
      setImageToImageMode(false)
      setInputImageDataURLs([])
    }
    navigate('/images/studio')
    void submitJob({
      prompt: job.prompt,
      model: nextModel,
      size: nextSize !== 'auto' ? nextSize : undefined,
      quality: params.quality && params.quality !== 'auto' ? params.quality : undefined,
      output_format: params.output_format || 'png',
      background: params.background && params.background !== 'auto' ? params.background : undefined,
      upscale: normalizeUpscale(params.upscale) || undefined,
      style: params.style,
      api_key_id: apiKeyID ? Number(apiKeyID) : undefined,
      template_id: params.template_id ? Number(params.template_id) : undefined,
      input_images: isEditJob ? params.input_images : undefined,
    }, isEditJob ? 'edit' : 'text')
  }

  const rerunFromAsset = (asset: ImageAsset) => {
    const job = jobs.find(item => item.id === asset.job_id) || currentJob
    setPreviewAsset(null)
    if (job?.id === asset.job_id) {
      rerunFromJob(job)
      return
    }
    if (asset.revised_prompt) {
      const nextModel = asset.model || 'gpt-image-2'
      setPrompt(asset.revised_prompt)
      setModel(nextModel)
      setSize(current => normalizeImageSizeForModel(nextModel, current))
      setOutputFormat(asset.output_format || 'png')
      navigate('/images/studio')
      void submitJob({ prompt: asset.revised_prompt, model: nextModel, output_format: asset.output_format || 'png' })
    }
  }

  const saveAssetPromptAsTemplate = async (asset: ImageAsset) => {
    const sourcePrompt = promptForAsset(asset)
    if (!sourcePrompt.trim()) {
      showToast(t('images.promptRequired'), 'error')
      return
    }
    try {
      await api.createImagePromptTemplate({
        name: `${asset.model || 'image'} ${assetResolution(asset)}`,
        prompt: sourcePrompt,
        model: asset.model || 'gpt-image-2',
        size: asset.requested_size || '',
        quality: asset.quality || '',
        output_format: asset.output_format || 'png',
        tags: [t('images.galleryTag')],
      })
      await loadTemplates()
      showToast(t('images.templateSaved'), 'success')
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.saveFailed'), 'error')
    }
  }

  const copyPrompt = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      showToast(t('common.copied'), 'success')
    } catch {
      showToast(t('common.copyFailed'), 'error')
    }
  }

  const downloadAsset = async (asset: ImageAsset) => {
    try {
      let blob: Blob | null = null
      try {
        blob = await api.getImageAssetFile(asset.id, true)
        await writeCachedImageAsset(asset, blob)
      } catch {
        blob = await readCachedImageAsset(asset.id)
      }
      if (!blob) {
        throw new Error(t('images.downloadFailed'))
      }
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = asset.filename || `image-${asset.id}.${asset.output_format || 'png'}`
      document.body.appendChild(a)
      a.click()
      a.remove()
      URL.revokeObjectURL(url)
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.downloadFailed'), 'error')
    }
  }

  const deleteAsset = async (asset: ImageAsset) => {
    const ok = await confirm({
      title: t('images.deleteAssetTitle'),
      description: asset.filename,
      confirmText: t('common.delete'),
      tone: 'destructive',
    })
    if (!ok) return
    try {
      await api.deleteImageAsset(asset.id)
      const url = assetURLs[asset.id]
      if (url) URL.revokeObjectURL(url)
      await deleteCachedImageAsset(asset.id)
      assetURLRequestsRef.current.delete(asset.id)
      setAssetURLs(prev => {
        const next = { ...prev }
        delete next[asset.id]
        return next
      })
      setAssets(prev => prev.filter(item => item.id !== asset.id))
      setHistoryJobs(prev => prev.map(job => ({
        ...job,
        assets: job.assets?.filter(item => item.id !== asset.id),
      })))
      setPreviewAsset(prev => prev?.id === asset.id ? null : prev)
      await loadAssets()
      if (activeView === 'history') {
        await loadHistoryJobs()
      }
      if (currentJob?.assets?.some(item => item.id === asset.id)) {
        const res = await api.getImageJob(currentJob.id, { includeCache: true })
        setCurrentJob(res.job)
      }
      showToast(t('images.assetDeleted'), 'success')
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.deleteFailed'), 'error')
    }
  }

  const deleteJob = async (job: ImageGenerationJob) => {
    const ok = await confirm({
      title: t('images.deleteJobTitle'),
      description: t('images.deleteJobDesc', { id: job.id }),
      confirmText: t('common.delete'),
      tone: 'destructive',
    })
    if (!ok) return
    try {
      await api.deleteImageJob(job.id)
      const jobAssets = job.assets ?? []
      const deletedAssetIds = new Set(jobAssets.map(asset => asset.id))
      for (const asset of jobAssets) {
        const url = assetURLs[asset.id]
        if (url) URL.revokeObjectURL(url)
        assetURLRequestsRef.current.delete(asset.id)
      }
      await Promise.all(jobAssets.map(asset => deleteCachedImageAsset(asset.id)))
      setAssetURLs(prev => {
        const next = { ...prev }
        deletedAssetIds.forEach(id => {
          delete next[id]
        })
        return next
      })
      setAssets(prev => prev.filter(asset => !deletedAssetIds.has(asset.id)))
      setJobs(prev => prev.filter(item => item.id !== job.id))
      setHistoryJobs(prev => prev.filter(item => item.id !== job.id))
      setHistoryTotal(total => Math.max(0, total - 1))
      setPreviewAsset(prev => prev && deletedAssetIds.has(prev.id) ? null : prev)
      setCurrentJob(prev => prev?.id === job.id ? null : prev)
      await Promise.all([loadJobs(), loadAssets(), loadHistoryJobs()])
      showToast(t('images.jobDeleted'), 'success')
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.deleteFailed'), 'error')
    }
  }

  const latestAsset = currentJob?.assets?.[0]
  const recentJobs = jobs.slice(0, 3)
  const maxAssetPage = Math.max(1, Math.ceil(assetTotal / IMAGE_ASSET_PAGE_SIZE))
  const maxHistoryPage = Math.max(1, Math.ceil(historyTotal / IMAGE_JOB_HISTORY_PAGE_SIZE))
  const filteredHistoryJobs = historyStatusFilter === 'all'
    ? historyJobs
    : historyJobs.filter(job => job.status === historyStatusFilter)
  const templateSelectOptions = templates.length > 0
    ? [{ label: t('images.noTemplateSelected'), value: '' }, ...templates.map(template => ({ label: template.name || `#${template.id}`, value: String(template.id) }))]
    : [{ label: t('images.noTemplates'), value: '' }]
  const sizeOptions = useMemo(() => sizeOptionsForModel(model), [model])
  const backgroundOptions = useMemo(() => [
    { label: t('images.backgroundOptions.auto'), value: 'auto' },
    { label: t('images.backgroundOptions.opaque'), value: 'opaque' },
    { label: t('images.backgroundOptions.transparent'), value: 'transparent' },
  ], [t])
  const upscaleOptions = useMemo(() => [
    { label: t('images.upscaleOptions.none'), value: '' },
    { label: t('images.upscaleOptions.2k'), value: '2k' },
    { label: t('images.upscaleOptions.4k'), value: '4k' },
  ], [t])
  const hasGenerationDraft = Boolean(
    prompt.trim() ||
    selectedTemplateId ||
    templateName.trim() ||
    templateTags.trim() ||
    style.trim() ||
    model !== 'gpt-image-2' ||
    size !== 'auto' ||
    quality !== 'auto' ||
    outputFormat !== 'png' ||
    background !== 'auto' ||
    upscale ||
    apiKeyID ||
    imageToImageMode ||
    inputImageDataURLs.length > 0
  )

  const clearGenerationForm = () => {
    setSelectedTemplateId(null)
    setPrompt('')
    setModel('gpt-image-2')
    setSize('auto')
    setQuality('auto')
    setOutputFormat('png')
    setBackground('auto')
    setUpscale('')
    setStyle('')
    setAPIKeyID('')
    setTemplateName('')
    setTemplateTags('')
    setImageToImageMode(false)
    setInputImageDataURLs([])
  }

  const changeGenerationModel = (value: string) => {
    setModel(value)
    setSize(current => normalizeImageSizeForModel(value, current))
  }

  const generationForm = (
    <Card className="xl:sticky xl:top-4">
      <CardContent className="flex flex-col gap-4 xl:min-h-[calc(100dvh-168px)]">
        <Field label={t('images.selectTemplate')}>
          <Select
            value={selectedTemplateId ? String(selectedTemplateId) : ''}
            onValueChange={selectTemplateForGeneration}
            options={templateSelectOptions}
            disabled={templates.length === 0}
          />
        </Field>

        <div className="flex items-center gap-2">
          <span className="text-xs font-semibold text-muted-foreground">{t('images.mode')}:</span>
          <div className="flex rounded-md border border-input bg-muted/50 p-0.5">
            <button
              type="button"
              className={`rounded-[3px] px-3 py-1 text-xs font-semibold transition-colors ${!imageToImageMode ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
              onClick={() => setImageToImageMode(false)}
            >
              <ImageIcon className="mr-1 inline-block size-3" />
              {t('images.textToImage')}
            </button>
            <button
              type="button"
              className={`rounded-[3px] px-3 py-1 text-xs font-semibold transition-colors ${imageToImageMode ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
              onClick={() => setImageToImageMode(true)}
            >
              <Upload className="mr-1 inline-block size-3" />
              {t('images.imageToImage')}
            </button>
          </div>
        </div>

        <div className="grid gap-3 md:grid-cols-3">
          <Field label={t('images.model')}><Select value={model} onValueChange={changeGenerationModel} options={IMAGE_MODELS} compact /></Field>
          <Field label={t('images.size')}><Select value={size} onValueChange={setSize} options={sizeOptions} compact /></Field>
          <Field label={t('images.quality')}><Select value={quality} onValueChange={setQuality} options={QUALITY_OPTIONS} compact /></Field>
          <Field label={t('images.format')}><Select value={outputFormat} onValueChange={setOutputFormat} options={FORMAT_OPTIONS} compact /></Field>
          <Field label={t('images.background')}><Select value={background} onValueChange={setBackground} options={backgroundOptions} compact /></Field>
          <Field label={t('images.localUpscale')}><Select value={upscale} onValueChange={setUpscale} options={upscaleOptions} compact /></Field>
          <Field label={t('images.apiKey')}>
            <Select
              value={apiKeyID}
              onValueChange={setAPIKeyID}
              options={[{ label: t('images.autoApiKey'), value: '' }, ...apiKeys.map(key => ({ label: key.name ? `${key.name} · ${key.key}` : key.key, value: String(key.id) }))]}
              compact
            />
          </Field>
        </div>

        <Field label={t('images.style')}>
          <Input value={style} onChange={e => setStyle(e.target.value)} placeholder={t('images.stylePlaceholder')} />
        </Field>

        <StylePresetPicker value={style} onChange={setStyle} onApply={() => showToast(t('images.stylePresetApplied'), 'success')} />

        {imageToImageMode && (
          <div className="space-y-2">
            <div className="flex items-center justify-between">
              <span className="text-xs font-semibold text-muted-foreground">{t('images.inputImage')}</span>
              <label className="inline-flex cursor-pointer items-center rounded-md px-2 py-1 text-xs font-semibold text-primary hover:bg-primary/10 transition-colors">
                <Upload className="mr-1 size-3" />
                {t('images.upload')}
                <input
                  type="file"
                  accept="image/*"
                  multiple
                  className="hidden"
                  onChange={handleImageFileChange}
                />
              </label>
            </div>
            {inputImageDataURLs.length > 0 ? (
              <div className="flex flex-wrap gap-2">
                {inputImageDataURLs.map((dataURL, index) => (
                  <div key={`${index}-${dataURL.slice(0, 40)}`} className="group relative">
                    <img src={dataURL} alt={`Input ${index + 1}`} className="h-20 w-20 rounded-md border border-border object-cover" />
                    <button
                      type="button"
                      className="absolute -top-1.5 -right-1.5 flex size-5 items-center justify-center rounded-full bg-destructive text-destructive-foreground opacity-0 transition-opacity group-hover:opacity-100"
                      onClick={() => setInputImageDataURLs(prev => prev.filter((_, i) => i !== index))}
                      title={t('images.removeImage')}
                    >
                      <X className="size-3" />
                    </button>
                  </div>
                ))}
              </div>
            ) : (
              <label className="flex cursor-pointer flex-col items-center justify-center rounded-md border border-dashed border-border bg-muted/30 py-6 text-center text-xs text-muted-foreground hover:bg-muted/50 transition-colors">
                <Upload className="mb-1 size-4 opacity-50" />
                {t('images.inputImageHint')}
                <input
                  type="file"
                  accept="image/*"
                  multiple
                  className="hidden"
                  onChange={handleImageFileChange}
                />
              </label>
            )}
          </div>
        )}

        <label className="flex min-h-0 flex-1 flex-col space-y-1.5">
          <span className="text-xs font-semibold text-muted-foreground">{t('images.prompt')}</span>
          <textarea
            value={prompt}
            onChange={e => setPrompt(e.target.value)}
            className="min-h-[260px] w-full flex-1 resize-none rounded-md border border-input bg-transparent px-3 py-2 text-sm leading-6 shadow-xs outline-none transition-[border-color,box-shadow] placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 xl:min-h-0 dark:bg-input/30"
            placeholder={t('images.promptPlaceholder')}
          />
        </label>

        <div className="grid gap-3 md:grid-cols-[minmax(0,1fr)_220px]">
          <div className="grid gap-3 md:grid-cols-2">
            <Input value={templateName} onChange={e => setTemplateName(e.target.value)} placeholder={t('images.templateName')} />
            <Input value={templateTags} onChange={e => setTemplateTags(e.target.value)} placeholder={t('images.templateTags')} />
          </div>
          <div className="flex gap-2">
            <Button variant="outline" className="flex-1" disabled={!prompt.trim()} onClick={() => void saveCurrentPromptAsTemplate()}><Save className="size-4" />{t('images.saveTemplate')}</Button>
          </div>
        </div>

        <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border pt-4">
          <div className="text-xs text-muted-foreground">{size === 'auto' ? t('images.autoSizeHint') : t('images.explicitSizeHint', { size })}</div>
          <div className="flex flex-wrap items-center justify-end gap-2">
            <Button variant="outline" disabled={submitting || !hasGenerationDraft} onClick={clearGenerationForm}>
              <X className="size-4" />
              {t('images.clearSelection')}
            </Button>
            <Button disabled={submitting || !prompt.trim()} onClick={() => void submitJob(createJobPayload(), imageToImageMode ? 'edit' : 'text')}>
              {submitting ? <Loader2 className="size-4 animate-spin" /> : <Play className="size-4" />}
              {t('images.generate')}
            </Button>
          </div>
        </div>
      </CardContent>
    </Card>
  )

  const templateLibrary = (
    <div className="space-y-3">
      <div className="toolbar-surface">
        <div className="mb-3 flex items-center justify-between gap-3">
          <div className="flex items-center gap-2 font-semibold text-foreground">
            <Sparkles className="size-4 text-primary" />
            {t('images.templates')}
          </div>
          <div className="flex items-center gap-2">
            <Badge variant="outline" className="text-[11px]">{templates.length}</Badge>
            <Button size="sm" onClick={openNewTemplateDialog}>
              <Plus className="size-4" />
              {t('images.newTemplate')}
            </Button>
          </div>
        </div>
        <div className="relative">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input value={templateSearch} onChange={e => setTemplateSearch(e.target.value)} onBlur={() => void loadTemplates()} className="pl-9" placeholder={t('images.searchTemplates')} />
        </div>
        {allTags.length > 0 && (
          <div className="mt-2 flex flex-wrap gap-1.5">
            <button className={`rounded-md px-2 py-1 text-[11px] font-semibold ${selectedTag === '' ? 'bg-primary/10 text-primary' : 'bg-muted text-muted-foreground'}`} onClick={() => setSelectedTag('')}>
              {t('common.all')}
            </button>
            {allTags.map(tag => (
              <button key={tag} className={`rounded-md px-2 py-1 text-[11px] font-semibold ${selectedTag === tag ? 'bg-primary/10 text-primary' : 'bg-muted text-muted-foreground'}`} onClick={() => setSelectedTag(tag)}>
                {tag}
              </button>
            ))}
          </div>
        )}
      </div>

      <div className="grid gap-2 sm:grid-cols-2 2xl:grid-cols-3">
        {templates.map(template => (
          <TemplateCard
            key={template.id}
            template={template}
            active={selectedTemplateId === template.id}
            onApply={() => applyTemplate(template)}
            onFavorite={() => void toggleFavorite(template)}
            onEdit={() => openEditTemplateDialog(template)}
            onDelete={() => void deleteTemplate(template)}
          />
        ))}
      </div>
      {!loading && templates.length === 0 && (
        <div className="rounded-lg border border-dashed border-border bg-background/60 p-6 text-center text-sm text-muted-foreground">
          <Sparkles className="mx-auto mb-2 size-5 text-muted-foreground/70" />
          {t('images.noTemplates')}
        </div>
      )}
    </div>
  )

  const currentJobPanel = (
    <Card>
      <CardContent className="space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="font-semibold">{t('images.currentJob')}</h2>
          {currentJob && (
            <div className="flex items-center gap-1.5">
              <Badge className={jobStatusClass(currentJob.status)}>{t(`images.status.${currentJob.status}`, { defaultValue: currentJob.status })}</Badge>
              {!isImageJobBusy(currentJob) && (
                <Button size="icon-xs" variant="ghost" onClick={() => void deleteJob(currentJob)} aria-label={t('images.deleteJob')} title={t('images.deleteJob')}>
                  <Trash2 className="size-3" />
                </Button>
              )}
            </div>
          )}
        </div>
        {currentJob ? (
          <>
            <div className="space-y-1 text-xs text-muted-foreground">
              <div className="flex justify-between gap-3"><span>ID</span><span className="font-geist-mono">{currentJob.id}</span></div>
              <div className="flex justify-between gap-3"><span>{t('images.duration')}</span><span>{formatDuration(currentJob.duration_ms)}</span></div>
              <div className="flex justify-between gap-3"><span>{t('images.createdAt')}</span><span>{formatBeijingTime(currentJob.created_at)}</span></div>
              <div className="flex justify-between gap-3"><span>{t('images.apiKey')}</span><span className="truncate">{currentJob.api_key_name || currentJob.api_key_masked || '-'}</span></div>
            </div>
            {['queued', 'running'].includes(currentJob.status) && (
              <div className="flex items-center gap-2 rounded-lg bg-muted px-3 py-2 text-sm text-muted-foreground">
                <Loader2 className="size-4 animate-spin" />
                {t('images.waiting')}
              </div>
            )}
            {currentJob.error_message && <div className="rounded-lg border border-red-500/20 bg-red-500/10 p-3 text-sm text-red-700 dark:text-red-200">{currentJob.error_message}</div>}
            {latestAsset && (
              <AssetCard
                asset={latestAsset}
                imageURL={assetThumbnailURL(latestAsset, assetURLs)}
                prompt={currentJob.prompt}
                compact
                onPreview={() => setPreviewAsset(latestAsset)}
                onDownload={() => void downloadAsset(latestAsset)}
                onDelete={() => void deleteAsset(latestAsset)}
                onCopyPrompt={() => void copyPrompt(currentJob.prompt)}
                onRerun={() => rerunFromJob(currentJob)}
                onSaveTemplate={() => void saveAssetPromptAsTemplate(latestAsset)}
              />
            )}
          </>
        ) : (
          <div className="rounded-lg border border-dashed border-border p-6 text-center text-sm text-muted-foreground">{t('images.noCurrentJob')}</div>
        )}
      </CardContent>
    </Card>
  )

  const recentJobsPanel = (
    <Card>
      <CardContent className="space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="font-semibold">{t('images.recentJobs')}</h2>
          <div className="flex items-center gap-1">
            <Button size="xs" variant="ghost" onClick={() => navigate('/images/history')}>{t('images.viewAllJobs')}</Button>
            <Button size="icon-sm" variant="ghost" onClick={() => void loadJobs()}><RefreshCcw className="size-4" /></Button>
          </div>
        </div>
        <div className="space-y-2">
          {recentJobs.map(job => (
            <button key={job.id} className="w-full rounded-lg border border-border p-3 text-left transition-colors hover:bg-muted/50" onClick={() => setCurrentJob(job)}>
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-sm font-semibold">#{job.id}</span>
                <Badge className={jobStatusClass(job.status)}>{t(`images.status.${job.status}`, { defaultValue: job.status })}</Badge>
              </div>
              <div className="mt-1 line-clamp-2 text-xs leading-5 text-muted-foreground">{job.prompt}</div>
              <div className="mt-2 flex items-center justify-between text-[11px] text-muted-foreground">
                <span>{formatDuration(job.duration_ms)}</span>
                <span>{formatBeijingTime(job.created_at)}</span>
              </div>
            </button>
          ))}
          {!loading && recentJobs.length === 0 && (
            <div className="rounded-lg border border-dashed border-border p-5 text-center text-sm text-muted-foreground">{t('images.noJobs')}</div>
          )}
        </div>
      </CardContent>
    </Card>
  )

  const historyStatusOptions: Array<{ value: ImageJobStatusFilter; label: string }> = [
    { value: 'all', label: t('common.all') },
    ...IMAGE_JOB_STATUSES.map(status => ({ value: status, label: t(`images.status.${status}`) })),
  ]

  const selectHistoryJob = (job: ImageGenerationJob) => {
    setCurrentJob(job)
    navigate('/images/studio')
    void api.getImageJob(job.id, { includeCache: true }).then(res => setCurrentJob(res.job)).catch(() => {
      // The selected history row is already enough if the refresh fails.
    })
  }

  const historyView = (
    <section className="space-y-4">
      <Card>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 className="text-base font-semibold">{t('images.historyJobs')}</h2>
              <p className="mt-1 text-sm text-muted-foreground">{t('images.jobHistoryHint')}</p>
            </div>
            <Button size="icon-sm" variant="ghost" onClick={() => void loadHistoryJobs()} disabled={historyLoading}>
              {historyLoading ? <Loader2 className="size-4 animate-spin" /> : <RefreshCcw className="size-4" />}
            </Button>
          </div>

          <div className="flex flex-wrap gap-2">
            {historyStatusOptions.map(option => (
              <button
                key={option.value}
                type="button"
                className={`rounded-md px-3 py-1.5 text-xs font-semibold transition-colors ${
                  historyStatusFilter === option.value ? 'bg-primary/10 text-primary' : 'bg-muted text-muted-foreground hover:text-foreground'
                }`}
                onClick={() => setHistoryStatusFilter(option.value)}
              >
                {option.label}
              </button>
            ))}
          </div>
        </CardContent>
      </Card>

      <div className="space-y-3">
        {filteredHistoryJobs.map(job => (
          <HistoryJobCard
            key={job.id}
            job={job}
            imageURLs={assetURLs}
            onSelect={() => selectHistoryJob(job)}
            onPreview={asset => setPreviewAsset(asset)}
            onDownload={asset => void downloadAsset(asset)}
            onCopyPrompt={() => void copyPrompt(job.prompt)}
            onRerun={() => rerunFromJob(job)}
            onSaveTemplate={asset => void saveAssetPromptAsTemplate(asset)}
            onDeleteJob={() => void deleteJob(job)}
            onDelete={asset => void deleteAsset(asset)}
          />
        ))}
        {!historyLoading && filteredHistoryJobs.length === 0 && (
          <div className="rounded-lg border border-dashed border-border p-8 text-center text-sm text-muted-foreground">{t('images.noJobs')}</div>
        )}
      </div>

      <div className="flex items-center justify-end gap-2">
        <Button variant="outline" size="sm" disabled={historyPage <= 1 || historyLoading} onClick={() => setHistoryPage(page => Math.max(1, page - 1))}>{t('common.prev')}</Button>
        <span className="text-xs text-muted-foreground">{historyPage} / {maxHistoryPage}</span>
        <Button variant="outline" size="sm" disabled={historyPage >= maxHistoryPage || historyLoading} onClick={() => setHistoryPage(page => page + 1)}>{t('common.next')}</Button>
      </div>
    </section>
  )

  const galleryView = (
    <section className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold">{t('images.gallery')}</h2>
          <p className="mt-1 text-sm text-muted-foreground">{t('images.galleryHint')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" disabled={assetPage <= 1} onClick={() => setAssetPage(page => Math.max(1, page - 1))}>{t('common.prev')}</Button>
          <span className="text-xs text-muted-foreground">{assetPage} / {maxAssetPage}</span>
          <Button variant="outline" size="sm" disabled={assetPage >= maxAssetPage} onClick={() => setAssetPage(page => page + 1)}>{t('common.next')}</Button>
        </div>
      </div>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4">
        {assets.map(asset => (
          <AssetCard
            key={asset.id}
            asset={asset}
            imageURL={assetThumbnailURL(asset, assetURLs)}
            prompt={promptForAsset(asset)}
            gallery
            onPreview={() => setPreviewAsset(asset)}
            onDownload={() => void downloadAsset(asset)}
            onDelete={() => void deleteAsset(asset)}
            onCopyPrompt={() => void copyPrompt(promptForAsset(asset) || asset.revised_prompt || '')}
            onRerun={() => rerunFromAsset(asset)}
            onSaveTemplate={() => void saveAssetPromptAsTemplate(asset)}
          />
        ))}
      </div>
      {!loading && assets.length === 0 && (
        <div className="rounded-lg border border-dashed border-border p-8 text-center text-sm text-muted-foreground">{t('images.noAssets')}</div>
      )}
    </section>
  )

  return (
    <>
      <div className="relative">
        <PageHeader title={t('images.title')} description={t('images.description')} />
        {activeView === 'studio' && <ImageNoticeCarousel />}
      </div>
      <ImageStudioTabs activeView={activeView} />
      {confirmDialog}

      {activeView === 'studio' && (
        <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
          <main className="space-y-4">{generationForm}</main>
          <aside className="space-y-4">
            {currentJobPanel}
            {recentJobsPanel}
          </aside>
        </div>
      )}

      {activeView === 'prompts' && (
        <div>{templateLibrary}</div>
      )}

      {activeView === 'gallery' && galleryView}

      {activeView === 'history' && historyView}

      <AssetPreviewDialog
        asset={previewAsset}
        imageURL={previewAsset ? assetPreviewURL(previewAsset, assetURLs) : undefined}
        prompt={previewAsset ? promptForAsset(previewAsset) : ''}
        open={Boolean(previewAsset)}
        onClose={() => setPreviewAsset(null)}
        onDownload={asset => void downloadAsset(asset)}
        onCopyPrompt={asset => void copyPrompt(promptForAsset(asset) || asset.revised_prompt || '')}
        onRerun={rerunFromAsset}
        onSaveTemplate={asset => void saveAssetPromptAsTemplate(asset)}
        onDelete={asset => void deleteAsset(asset)}
      />
      <TemplateEditorDialog
        open={templateDialogOpen}
        draft={templateDialogDraft}
        saving={templateDialogSaving}
        onClose={() => setTemplateDialogOpen(false)}
        onChange={updateTemplateDialogDraft}
        onSave={() => void saveTemplateDialog()}
        onApplyStylePreset={() => showToast(t('images.stylePresetApplied'), 'success')}
      />
    </>
  )
}

function ImageNoticeCarousel() {
  const { t } = useTranslation()
  const [index, setIndex] = useState(0)
  const [paused, setPaused] = useState(false)
  const [overflowDistance, setOverflowDistance] = useState(0)
  const textFrameRef = useRef<HTMLDivElement>(null)
  const textRef = useRef<HTMLDivElement>(null)
  const currentIndex = index % IMAGE_NOTICE_KEYS.length
  const notice = t(IMAGE_NOTICE_KEYS[currentIndex])

  useEffect(() => {
    if (paused || IMAGE_NOTICE_KEYS.length <= 1) return
    const timer = window.setInterval(() => {
      setIndex(value => (value + 1) % IMAGE_NOTICE_KEYS.length)
    }, 4500)
    return () => window.clearInterval(timer)
  }, [paused])

  useLayoutEffect(() => {
    const measure = () => {
      const frame = textFrameRef.current
      const text = textRef.current
      if (!frame || !text) {
        setOverflowDistance(0)
        return
      }
      setOverflowDistance(Math.max(0, text.scrollWidth - frame.clientWidth))
    }
    measure()
    window.addEventListener('resize', measure)
    return () => window.removeEventListener('resize', measure)
  }, [notice])

  return (
    <div className="-mt-3 mb-4 flex justify-center md:absolute md:inset-x-0 md:top-0 md:mt-0 md:mb-0">
      <div
        className="flex h-10 w-full max-w-[620px] items-center gap-3 rounded-xl border border-primary/20 bg-primary/6 px-4 text-primary shadow-sm backdrop-blur-sm transition-colors hover:bg-primary/8 focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/45"
        tabIndex={0}
        role="status"
        aria-live="polite"
        onMouseEnter={() => setPaused(true)}
        onMouseLeave={() => setPaused(false)}
        onFocus={() => setPaused(true)}
        onBlur={() => setPaused(false)}
      >
        <Sparkles className="size-4 shrink-0" />
        <div ref={textFrameRef} className="relative min-w-0 flex-1 overflow-hidden">
          <div
            key={currentIndex}
            ref={textRef}
            className={`inline-block whitespace-nowrap text-sm font-semibold ${overflowDistance > 0 && !paused ? 'animate-image-notice-marquee' : ''}`}
            style={overflowDistance > 0 ? { '--image-notice-marquee-distance': `-${overflowDistance}px` } as React.CSSProperties : undefined}
          >
            {notice}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          {IMAGE_NOTICE_KEYS.map((key, dotIndex) => (
            <button
              key={key}
              type="button"
              aria-current={dotIndex === currentIndex ? 'true' : undefined}
              aria-label={t('images.noticeDotLabel', { index: dotIndex + 1, total: IMAGE_NOTICE_KEYS.length })}
              className={`size-2 rounded-full border-0 p-0 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/45 ${dotIndex === currentIndex ? 'bg-primary' : 'bg-primary/25 hover:bg-primary/45'}`}
              onClick={() => setIndex(dotIndex)}
            />
          ))}
        </div>
      </div>
    </div>
  )
}

function ImageStudioTabs({ activeView }: { activeView: ImageView }) {
  const { t } = useTranslation()
  const tabs = [
    { view: 'studio' as const, label: t('images.views.studio'), to: '/images/studio' },
    { view: 'prompts' as const, label: t('images.views.prompts'), to: '/images/prompts' },
    { view: 'gallery' as const, label: t('images.views.gallery'), to: '/images/gallery' },
    { view: 'history' as const, label: t('images.views.history'), to: '/images/history' },
  ]
  const activeIndex = Math.max(0, tabs.findIndex(tab => tab.view === activeView))

  return (
    <div className="mb-5 flex justify-center">
      <div className="relative grid w-full max-w-[620px] grid-cols-4 rounded-2xl border border-border bg-background/80 p-1 shadow-sm backdrop-blur-lg" role="tablist" aria-label={t('images.title')}>
        <div
          className="pointer-events-none absolute left-1 top-1 h-[calc(100%-0.5rem)] rounded-xl border border-primary/15 bg-primary/8 transition-transform duration-300 ease-out"
          style={{ width: 'calc((100% - 0.5rem) / 4)', transform: `translateX(${activeIndex * 100}%)` }}
        />
        {tabs.map(tab => (
          <NavLink
            key={tab.view}
            to={tab.to}
            role="tab"
            aria-selected={activeView === tab.view}
            className={`relative z-10 flex h-9 items-center justify-center rounded-xl px-3 text-sm font-semibold transition-colors ${
              activeView === tab.view ? 'text-primary' : 'text-muted-foreground hover:text-foreground'
            }`}
          >
            {tab.label}
          </NavLink>
        ))}
      </div>
    </div>
  )
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="space-y-1.5">
      <span className="text-xs font-semibold text-muted-foreground">{label}</span>
      {children}
    </label>
  )
}

function TemplateEditorDialog({
  open,
  draft,
  saving,
  onClose,
  onChange,
  onSave,
  onApplyStylePreset,
}: {
  open: boolean
  draft: TemplateEditorDraft
  saving: boolean
  onClose: () => void
  onChange: (patch: Partial<TemplateEditorDraft>) => void
  onSave: () => void
  onApplyStylePreset: () => void
}) {
  const { t } = useTranslation()
  const editing = Boolean(draft.id)
  const sizeOptions = useMemo(() => sizeOptionsForModel(draft.model), [draft.model])
  const backgroundOptions = useMemo(() => [
    { label: t('images.backgroundOptions.auto'), value: 'auto' },
    { label: t('images.backgroundOptions.opaque'), value: 'opaque' },
    { label: t('images.backgroundOptions.transparent'), value: 'transparent' },
  ], [t])

  const changeModel = (value: string) => {
    onChange({ model: value, size: normalizeImageSizeForModel(value, draft.size) })
  }

  return (
    <Dialog open={open} onOpenChange={nextOpen => { if (!nextOpen) onClose() }}>
      <DialogContent className="!flex max-h-[calc(100dvh-1rem)] !w-[min(980px,calc(100vw-1rem))] !max-w-none flex-col gap-0 overflow-hidden p-0">
        <DialogHeader className="border-b border-border px-5 pb-4 pr-12 pt-5">
          <DialogTitle>{editing ? t('images.editTemplate') : t('images.createTemplate')}</DialogTitle>
          <DialogDescription>{t('images.templateDialogDesc')}</DialogDescription>
        </DialogHeader>

        <div className="grid min-h-0 flex-1 gap-5 overflow-y-auto p-5 lg:grid-cols-[minmax(0,1fr)_320px]">
          <main className="space-y-4">
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label={t('images.templateName')}>
                <Input value={draft.name} onChange={e => onChange({ name: e.target.value })} placeholder={t('images.templateName')} />
              </Field>
              <Field label={t('images.templateTags')}>
                <Input value={draft.tags} onChange={e => onChange({ tags: e.target.value })} placeholder={t('images.templateTags')} />
              </Field>
            </div>

            <Field label={t('images.style')}>
              <Input value={draft.style} onChange={e => onChange({ style: e.target.value })} placeholder={t('images.stylePlaceholder')} />
            </Field>

            <StylePresetPicker value={draft.style} onChange={value => onChange({ style: value })} onApply={onApplyStylePreset} compact />

            <Field label={t('images.prompt')}>
              <textarea
                value={draft.prompt}
                onChange={e => onChange({ prompt: e.target.value })}
                className="min-h-[360px] w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 text-sm leading-6 shadow-xs outline-none transition-[border-color,box-shadow] placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30"
                placeholder={t('images.promptPlaceholder')}
              />
            </Field>
          </main>

          <aside className="space-y-3 rounded-md border border-border/70 bg-muted/15 p-4">
            <div>
              <h3 className="text-sm font-semibold">{t('images.templateDetails')}</h3>
              <p className="mt-1 text-xs leading-5 text-muted-foreground">{t('images.newTemplateHint')}</p>
            </div>
            <Field label={t('images.model')}>
              <Select value={draft.model} onValueChange={changeModel} options={IMAGE_MODELS} compact />
            </Field>
            <Field label={t('images.size')}>
              <Select value={draft.size} onValueChange={value => onChange({ size: value })} options={sizeOptions} compact />
            </Field>
            <Field label={t('images.quality')}>
              <Select value={draft.quality} onValueChange={value => onChange({ quality: value })} options={QUALITY_OPTIONS} compact />
            </Field>
            <Field label={t('images.format')}>
              <Select value={draft.outputFormat} onValueChange={value => onChange({ outputFormat: value })} options={FORMAT_OPTIONS} compact />
            </Field>
            <Field label={t('images.background')}>
              <Select value={draft.background} onValueChange={value => onChange({ background: value })} options={backgroundOptions} compact />
            </Field>
          </aside>
        </div>

        <DialogFooter className="border-t border-border px-5 py-4">
          <Button variant="outline" disabled={saving} onClick={onClose}>{t('common.cancel')}</Button>
          <Button disabled={saving || !draft.prompt.trim()} onClick={onSave}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
            {editing ? t('images.updateTemplate') : t('images.saveTemplate')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function StylePresetPicker({
  value,
  onChange,
  onApply,
  compact = false,
}: {
  value: string
  onChange: (value: string) => void
  onApply?: () => void
  compact?: boolean
}) {
  const { t } = useTranslation()

  const applyPreset = (presetValue: string) => {
    onChange(presetValue)
    onApply?.()
  }

  return (
    <div className={compact ? 'rounded-md border border-dashed border-border/80 bg-muted/20 p-2.5' : 'rounded-lg border border-border/70 bg-muted/20 p-3'}>
      <div className="mb-2 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2 text-xs font-semibold text-muted-foreground">
          <Sparkles className="size-3.5" />
          {t('images.stylePresets')}
        </div>
        {value.trim() && (
          <button type="button" className="text-xs font-semibold text-muted-foreground transition hover:text-foreground" onClick={() => onChange('')}>
            {t('images.clearStyle')}
          </button>
        )}
      </div>
      <div className={compact ? 'flex flex-wrap gap-1.5' : 'grid gap-2 sm:grid-cols-2 xl:grid-cols-4'}>
        {STYLE_PRESETS.map(preset => {
          const active = value.trim() === preset.value
          return (
            <button
              key={preset.id}
              type="button"
              onClick={() => applyPreset(preset.value)}
              className={`min-w-0 border text-left transition ${
                active
                  ? 'border-primary/35 bg-primary/10 text-primary shadow-xs'
                  : 'border-border/70 bg-background/70 text-foreground hover:border-primary/30 hover:bg-primary/5'
              } ${compact ? 'rounded-full px-2.5 py-1 text-xs' : 'rounded-md px-3 py-2'}`}
            >
              <span className={`block truncate font-semibold ${compact ? 'text-xs' : 'text-sm'}`}>{t(`images.stylePreset.${preset.id}`)}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}

function TemplateCard({
  template,
  active,
  onApply,
  onFavorite,
  onEdit,
  onDelete,
}: {
  template: ImagePromptTemplate
  active: boolean
  onApply: () => void
  onFavorite: () => void
  onEdit: () => void
  onDelete: () => void
}) {
  const { t } = useTranslation()
  return (
    <Card className={`gap-3 p-3 ${active ? 'border-primary/35 bg-primary/5' : ''}`}>
      <div className="flex items-start justify-between gap-2">
        <button className="min-w-0 text-left" onClick={onApply}>
          <div className="truncate text-sm font-semibold">{template.name}</div>
          <div className="mt-1 line-clamp-2 text-xs leading-5 text-muted-foreground">{template.prompt}</div>
        </button>
        <button className={`shrink-0 ${template.favorite ? 'text-amber-500' : 'text-muted-foreground'}`} onClick={onFavorite} aria-label={t('images.favorite')}>
          <Star className="size-4" fill={template.favorite ? 'currentColor' : 'none'} />
        </button>
      </div>
      <div className="flex flex-wrap gap-1">
        {template.tags.map(tag => <Badge key={tag} variant="outline" className="text-[10px]">{tag}</Badge>)}
      </div>
      <div className="flex items-center justify-between gap-2">
        <span className="text-[11px] text-muted-foreground">{template.model || 'gpt-image-2'}</span>
        <div className="flex gap-1">
          <Button size="icon-xs" variant="ghost" onClick={onEdit} aria-label={t('images.editTemplate')}><Pencil className="size-3" /></Button>
          <Button size="icon-xs" variant="ghost" onClick={onDelete} aria-label={t('common.delete')}><Trash2 className="size-3" /></Button>
        </div>
      </div>
    </Card>
  )
}

function HistoryJobCard({
  job,
  imageURLs,
  onSelect,
  onPreview,
  onDownload,
  onCopyPrompt,
  onRerun,
  onSaveTemplate,
  onDeleteJob,
  onDelete,
}: {
  job: ImageGenerationJob
  imageURLs: Record<number, string>
  onSelect: () => void
  onPreview: (asset: ImageAsset) => void
  onDownload: (asset: ImageAsset) => void
  onCopyPrompt: () => void
  onRerun: () => void
  onSaveTemplate: (asset: ImageAsset) => void
  onDeleteJob: () => void
  onDelete: (asset: ImageAsset) => void
}) {
  const { t } = useTranslation()
  const assets = job.assets ?? []
  const primaryAsset = assets[0]
  const imagesWereDeleted = job.status === 'succeeded' && assets.length === 0

  return (
    <Card className="overflow-hidden p-0">
      <div className="grid gap-0 xl:grid-cols-[minmax(0,1fr)_320px] 2xl:grid-cols-[minmax(0,1fr)_360px]">
        <div className="space-y-3 p-4">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <button type="button" className="min-w-0 flex-1 text-left" onClick={onSelect}>
              <div className="flex items-center gap-2">
                <span className="font-geist-mono text-base font-semibold">#{job.id}</span>
                <Badge className={jobStatusClass(job.status)}>{t(`images.status.${job.status}`, { defaultValue: job.status })}</Badge>
              </div>
              <div className="mt-2 line-clamp-2 text-sm leading-6 text-foreground">{job.prompt}</div>
            </button>
            <div className="flex shrink-0 flex-wrap justify-end gap-1.5">
              <Button size="xs" variant="outline" onClick={onSelect}>{t('images.selectJob')}</Button>
              <Button size="icon-xs" variant="ghost" onClick={onCopyPrompt} aria-label={t('images.copyPrompt')} title={t('images.copyPrompt')}><Copy className="size-3" /></Button>
              <Button size="icon-xs" variant="ghost" onClick={onRerun} aria-label={t('images.rerun')} title={t('images.rerun')}><RefreshCcw className="size-3" /></Button>
              {!isImageJobBusy(job) && (
                <Button size="icon-xs" variant="ghost" onClick={onDeleteJob} aria-label={t('images.deleteJob')} title={t('images.deleteJob')}><Trash2 className="size-3" /></Button>
              )}
            </div>
          </div>

          <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
            <HistoryMeta label={t('images.model')} value={jobModel(job)} />
            <HistoryMeta label={t('images.size')} value={jobRequestedSize(job)} />
            <HistoryMeta label={t('images.duration')} value={formatDuration(job.duration_ms)} />
            <HistoryMeta label={t('images.createdAt')} value={formatBeijingTime(job.created_at)} />
            <HistoryMeta label={t('images.apiKey')} value={job.api_key_name || job.api_key_masked || '-'} />
            <HistoryMeta label={t('images.assetsCount')} value={t('images.imageCount', { count: assets.length })} />
          </div>

          {job.error_message && (
            <div className="line-clamp-3 rounded-lg border border-red-500/20 bg-red-500/10 p-3 text-sm leading-6 text-red-700 dark:text-red-200">
              {job.error_message}
            </div>
          )}
        </div>

        <div className="border-t border-border bg-muted/15 p-3 xl:border-l xl:border-t-0">
          {assets.length > 0 ? (
            <div className="space-y-2">
              <div className={assets.length === 1 ? 'grid gap-2' : 'grid grid-cols-4 gap-2 xl:grid-cols-2'}>
                {assets.slice(0, 4).map(asset => (
                  <button
                    key={asset.id}
                    type="button"
                    className={`overflow-hidden rounded-md border border-border bg-background transition hover:border-primary/40 ${
                      assets.length === 1 ? 'h-44 sm:h-48 xl:h-52' : 'h-20 sm:h-24 xl:h-28'
                    }`}
                    onClick={() => onPreview(asset)}
                    aria-label={t('images.openPreview')}
                  >
                    {assetThumbnailURL(asset, imageURLs) ? (
                      <img src={assetThumbnailURL(asset, imageURLs)} alt={job.prompt || asset.filename} className="h-full w-full object-contain" />
                    ) : (
                      <span className="flex h-full w-full items-center justify-center text-muted-foreground">
                        <ImageIcon className="size-5" />
                      </span>
                    )}
                  </button>
                ))}
              </div>

              {primaryAsset && (
                <div className="flex flex-wrap gap-1.5">
                  <Button size="icon-xs" variant="outline" onClick={() => onDownload(primaryAsset)} aria-label={t('images.download')} title={t('images.download')}><Download className="size-3" /></Button>
                  <Button size="icon-xs" variant="outline" onClick={onCopyPrompt} aria-label={t('images.copyPrompt')} title={t('images.copyPrompt')}><Copy className="size-3" /></Button>
                  <Button size="icon-xs" variant="outline" onClick={onRerun} aria-label={t('images.rerun')} title={t('images.rerun')}><RefreshCcw className="size-3" /></Button>
                  <Button size="icon-xs" variant="outline" onClick={() => onSaveTemplate(primaryAsset)} aria-label={t('images.saveAsTemplate')} title={t('images.saveAsTemplate')}><Save className="size-3" /></Button>
                  <Button size="icon-xs" variant="ghost" onClick={() => onDelete(primaryAsset)} aria-label={t('common.delete')} title={t('common.delete')}><Trash2 className="size-3" /></Button>
                </div>
              )}
            </div>
          ) : (
            <div className="flex h-44 flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-border px-3 text-center text-sm text-muted-foreground sm:h-48 xl:h-52">
              {imagesWereDeleted ? (
                <>
                  <ImageIcon className="size-6 text-muted-foreground/70" />
                  <span>{t('images.assetDeletedInHistory')}</span>
                  <Button size="xs" variant="outline" onClick={onRerun}><RefreshCcw className="size-3" />{t('images.rerun')}</Button>
                </>
              ) : (
                <span>{job.status === 'failed' ? t('images.noAssets') : t('images.waiting')}</span>
              )}
            </div>
          )}
        </div>
      </div>
    </Card>
  )
}

function HistoryMeta({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 rounded-md bg-muted/40 px-3 py-2">
      <div className="text-[11px] font-semibold text-muted-foreground">{label}</div>
      <div className="mt-1 truncate text-sm text-foreground">{value}</div>
    </div>
  )
}

function AssetCard({
  asset,
  imageURL,
  prompt,
  compact = false,
  gallery = false,
  onPreview,
  onDownload,
  onDelete,
  onCopyPrompt,
  onRerun,
  onSaveTemplate,
}: {
  asset: ImageAsset
  imageURL?: string
  prompt: string
  compact?: boolean
  gallery?: boolean
  onPreview: () => void
  onDownload: () => void
  onDelete: () => void
  onCopyPrompt: () => void
  onRerun: () => void
  onSaveTemplate: () => void
}) {
  const { t } = useTranslation()
  const previewTitle = t('images.openPreview')
  const imageFrameClass = assetDisplayFrameClass(asset, compact, gallery)
  return (
    <Card className="gap-3 overflow-hidden p-0">
      <div className={`relative bg-muted ${imageFrameClass}`}>
        {imageURL ? (
          <button type="button" onClick={onPreview} className="group/image h-full w-full cursor-zoom-in" aria-label={previewTitle}>
            <img src={imageURL} alt={prompt || asset.filename} className="h-full w-full object-contain" />
            <span className="absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition group-hover/image:bg-black/20 group-hover/image:opacity-100">
              <span className="inline-flex size-10 items-center justify-center rounded-full bg-black/55 text-white shadow-lg">
                <Eye className="size-5" />
              </span>
            </span>
          </button>
        ) : (
          <div className="flex h-full items-center justify-center text-muted-foreground">
            <ImageIcon className="size-8" />
          </div>
        )}
      </div>
      <div className="space-y-2 px-3 pb-3">
        <div className="grid grid-cols-2 gap-2 text-[11px] text-muted-foreground">
          <span>{assetResolution(asset)}</span>
          <span className="text-right">{formatBytes(asset.bytes)}</span>
          <span>{asset.model}</span>
          <span className="text-right">{imageAssetFormat(asset)}</span>
        </div>
        {gallery ? (
          <div className="flex flex-wrap gap-1">
            <Button size="icon-xs" variant="outline" onClick={onDownload} aria-label={t('images.download')} title={t('images.download')}><Download className="size-3" /></Button>
            <Button size="icon-xs" variant="outline" onClick={onCopyPrompt} aria-label={t('images.copyPrompt')} title={t('images.copyPrompt')}><Copy className="size-3" /></Button>
            <Button size="icon-xs" variant="outline" onClick={onRerun} aria-label={t('images.rerun')} title={t('images.rerun')}><RefreshCcw className="size-3" /></Button>
            <Button size="icon-xs" variant="outline" onClick={onSaveTemplate} aria-label={t('images.saveAsTemplate')} title={t('images.saveAsTemplate')}><Save className="size-3" /></Button>
            <Button size="icon-xs" variant="ghost" onClick={onDelete} aria-label={t('common.delete')} title={t('common.delete')}><Trash2 className="size-3" /></Button>
          </div>
        ) : (
          <div className="flex flex-wrap gap-1">
            <Button size="xs" variant="outline" onClick={onDownload}><Download className="size-3" />{t('images.download')}</Button>
            <Button size="xs" variant="outline" onClick={onCopyPrompt}><Copy className="size-3" />{t('images.copyPrompt')}</Button>
            <Button size="xs" variant="outline" onClick={onRerun}><RefreshCcw className="size-3" />{t('images.rerun')}</Button>
            <Button size="xs" variant="outline" onClick={onSaveTemplate}><Save className="size-3" />{t('images.saveAsTemplate')}</Button>
            <Button size="icon-xs" variant="ghost" onClick={onDelete} aria-label={t('common.delete')}><Trash2 className="size-3" /></Button>
          </div>
        )}
      </div>
    </Card>
  )
}

function AssetPreviewDialog({
  asset,
  imageURL,
  prompt,
  open,
  onClose,
  onDownload,
  onCopyPrompt,
  onRerun,
  onSaveTemplate,
  onDelete,
}: {
  asset: ImageAsset | null
  imageURL?: string
  prompt: string
  open: boolean
  onClose: () => void
  onDownload: (asset: ImageAsset) => void
  onCopyPrompt: (asset: ImageAsset) => void
  onRerun: (asset: ImageAsset) => void
  onSaveTemplate: (asset: ImageAsset) => void
  onDelete: (asset: ImageAsset) => void
}) {
  const { t } = useTranslation()
  if (!asset) return null

  return (
    <Dialog open={open} onOpenChange={nextOpen => { if (!nextOpen) onClose() }}>
      <DialogContent className="!flex !h-[calc(100dvh-0.75rem)] !w-[min(1480px,calc(100vw-0.75rem))] !max-w-none flex-col gap-0 overflow-hidden p-0" showCloseButton={false}>
        <DialogHeader className="sr-only">
          <DialogTitle>{t('images.previewTitle')}</DialogTitle>
          <DialogDescription>{asset.filename}</DialogDescription>
        </DialogHeader>
        <button
          type="button"
          onClick={onClose}
          className="absolute right-3 top-3 z-10 inline-flex size-9 items-center justify-center rounded-full bg-black/60 text-white shadow-lg transition hover:bg-black/75"
          aria-label={t('common.close')}
        >
          <X className="size-4" />
        </button>
        <div className="flex min-h-0 flex-1 items-center justify-center bg-black/90 p-3 sm:p-5">
          {imageURL ? (
            <img key={imageURL} src={imageURL} alt={prompt || asset.filename} className="h-full max-h-full w-full max-w-full rounded-md object-contain shadow-2xl" />
          ) : (
            <div className="flex h-full w-full items-center justify-center text-white/70">
              <ImageIcon className="size-10" />
            </div>
          )}
        </div>
        <div className="shrink-0 border-t border-border bg-background p-2.5 sm:p-3">
          <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <div className="grid w-full grid-cols-2 gap-2 sm:w-auto sm:min-w-[450px] sm:grid-cols-3">
              <PreviewMeta label={t('images.resolution')} value={assetResolution(asset)} />
              <PreviewMeta label={t('images.fileSize')} value={formatBytes(asset.bytes)} />
              <PreviewMeta label={t('images.format')} value={imageAssetFormat(asset)} />
            </div>
            <TooltipProvider>
              <div className="flex w-full items-center justify-end gap-1.5 sm:w-auto">
                <PreviewAction label={t('images.download')} onClick={() => onDownload(asset)}>
                  <Download className="size-4" />
                </PreviewAction>
                <PreviewAction label={t('images.copyPrompt')} onClick={() => onCopyPrompt(asset)}>
                  <Copy className="size-4" />
                </PreviewAction>
                <PreviewAction label={t('images.rerun')} onClick={() => onRerun(asset)}>
                  <RefreshCcw className="size-4" />
                </PreviewAction>
                <PreviewAction label={t('images.saveAsTemplate')} onClick={() => onSaveTemplate(asset)}>
                  <Save className="size-4" />
                </PreviewAction>
                <PreviewAction label={t('common.delete')} variant="destructive" onClick={() => onDelete(asset)}>
                  <Trash2 className="size-4" />
                </PreviewAction>
              </div>
            </TooltipProvider>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

function PreviewMeta({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 rounded-md bg-muted/55 px-2.5 py-1.5">
      <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground/75">{label}</div>
      <div className="mt-1 truncate font-geist-mono text-[12px] text-foreground">{value}</div>
    </div>
  )
}

function PreviewAction({
  label,
  variant = 'outline',
  onClick,
  children,
}: {
  label: string
  variant?: 'outline' | 'destructive'
  onClick: () => void
  children: ReactNode
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button size="icon-sm" variant={variant} onClick={onClick} aria-label={label}>
          {children}
        </Button>
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  )
}
