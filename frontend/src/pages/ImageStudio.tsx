import { IMAGE_MODELS, imageQualityOptions, normalizeImageQualityForModel } from '../lib/imageStudioModels'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { NavLink, useNavigate, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { StudioArtwork } from '../components/image-studio/StudioArtwork'
import { StudioCollectionHero, StudioCollectionSearch, StudioCollectionEmpty, StudioCollectionLoading, StudioCollectionPagination } from '../components/image-studio/StudioCollectionChrome'
import { imageAssetOrientation, selectImageTemplates, selectImageAssets, selectImageJobs } from '../lib/imageStudioCollections'
import type { ImageTemplateSort, ImageOrientation } from '../lib/imageStudioCollections'
import './image-studio.css'
import './image-studio-collections.css'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import type { APIKeyRow, CreateImageJobPayload, ImageAsset, ImageGenerationJob, ImagePromptTemplate, ImagePromptTemplatePayload } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
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
import { GlideMenu, PixelLoader, ShimmerText, ValuePill } from '@/components/ui/beautiful'
import {
  ArrowRight,
  ArrowUpRight,
  CheckCircle2,
  CircleAlert,
  Columns3,
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Clapperboard,
  Copy,
  Download,
  Eye,
  FolderOpen,
  Grid2X2,
  History,
  Image as ImageIcon,
  Info,
  Images,
  LayoutTemplate,
  Loader2,
  Monitor,
  Package,
  Palette,
  Pencil,
  Plus,
  RectangleHorizontal,
  RectangleVertical,
  RefreshCcw,
  Save,
  Search,
  ShoppingBag,
  SlidersHorizontal,
  Shuffle,
  Sparkles,
  Square,
  Star,
  Sticker,
  Timer,
  Tag,
  Trash2,
  Upload,
  Wand2,
  X,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { cn } from '@/lib/utils'

const IMAGE_VIEWS = ['studio', 'prompts', 'gallery', 'history'] as const
type ImageView = typeof IMAGE_VIEWS[number]
const IMAGE_ASSET_PAGE_SIZE = 16
const IMAGE_JOB_HISTORY_PAGE_SIZE = 20
const IMAGE_JOB_STATUSES = ['queued', 'running', 'succeeded', 'failed'] as const
type ImageJobStatusFilter = 'all' | typeof IMAGE_JOB_STATUSES[number]
const IMAGE_ASSET_CACHE_DB = 'codex2api-image-assets'
const IMAGE_ASSET_CACHE_STORE = 'assets'
const IMAGE_ASSET_CACHE_VERSION = 1
const IMAGE_MODEL_2K_SUFFIX = '-2k'
const IMAGE_MODEL_4K_SUFFIX = '-4k'
type ImageSizeTier = '1k' | '2k' | '4k'

function imageSizeTierForModel(model: string): ImageSizeTier {
  const lower = model.trim().toLowerCase()
  if (lower.endsWith(IMAGE_MODEL_4K_SUFFIX)) return '4k'
  if (lower.endsWith(IMAGE_MODEL_2K_SUFFIX)) return '2k'
  return '1k'
}
const IMAGE_NOTICE_KEYS = ['images.notices.pngFallback', 'images.notices.transparent', 'images.notices.highQuality', 'images.notices.accountRouting']

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

const ASPECT_RATIO_IDS = ['auto', '1:1', '16:9', '9:16'] as const
type AspectRatioId = typeof ASPECT_RATIO_IDS[number]

const ASPECT_RATIO_SIZE_MAP: Record<ImageSizeTier, Record<Exclude<AspectRatioId, 'auto'>, string>> = {
  '1k': {
    '1:1': '1024x1024',
    '16:9': '1536x864',
    '9:16': '864x1536',
  },
  '2k': {
    '1:1': '2048x2048',
    '16:9': '2560x1440',
    '9:16': '1440x2560',
  },
  '4k': {
    '1:1': '2880x2880',
    '16:9': '3840x2160',
    '9:16': '2160x3840',
  },
}

const SIZE_TO_ASPECT: Record<string, AspectRatioId> = {
  auto: 'auto',
  '1024x1024': '1:1',
  '2048x2048': '1:1',
  '2880x2880': '1:1',
  '1536x864': '16:9',
  '2560x1440': '16:9',
  '3840x2160': '16:9',
  '864x1536': '9:16',
  '1440x2560': '9:16',
  '2160x3840': '9:16',
}

const ASPECT_RATIO_ICONS: Record<AspectRatioId, LucideIcon> = {
  auto: Sparkles,
  '1:1': Square,
  '16:9': RectangleHorizontal,
  '9:16': RectangleVertical,
}


const FORMAT_OPTIONS = [
  { label: 'PNG', value: 'png' },
  { label: 'WebP', value: 'webp' },
  { label: 'JPEG', value: 'jpeg' },
]

const UPSCALE_VALUES = ['', 'none', '2k', '4k'] as const

const STYLE_PRESETS = [
  {
    id: 'cinematic',
    value: 'Cinematic realistic photography, natural light, subtle film grain, rich but controlled color grading, soft shadows, professional composition, high detail.',
    icon: Clapperboard,
    swatch: 'bg-gradient-to-br from-amber-700 via-stone-800 to-slate-950',
  },
  {
    id: 'commerce',
    value: 'Clean commercial product photography, premium studio lighting, crisp edges, realistic materials, neutral background, catalog-ready composition, high detail.',
    icon: ShoppingBag,
    swatch: 'bg-gradient-to-br from-slate-100 via-white to-sky-100 text-slate-700',
  },
  {
    id: 'sticker',
    value: 'Cute sticker illustration, bold clean outline, simple readable shapes, vibrant colors, playful expression, isolated subject, transparent-background friendly.',
    icon: Sticker,
    swatch: 'bg-gradient-to-br from-pink-400 via-rose-300 to-orange-300 text-rose-900',
  },
  {
    id: 'toy',
    value: 'Premium 3D designer toy style, soft rounded forms, glossy vinyl material, studio render lighting, collectible figure presentation, charming details.',
    icon: Package,
    swatch: 'bg-gradient-to-br from-violet-500 via-fuchsia-400 to-amber-300 text-violet-950',
  },
  {
    id: 'icon',
    value: 'Modern flat vector icon style, geometric shapes, simple silhouette, balanced negative space, clean edges, limited color palette, app-icon ready.',
    icon: LayoutTemplate,
    swatch: 'bg-gradient-to-br from-blue-500 via-indigo-500 to-cyan-400',
  },
  {
    id: 'poster',
    value: 'Vintage editorial poster style, bold typography space, textured print grain, strong focal composition, retro color palette, dramatic visual hierarchy.',
    icon: Palette,
    swatch: 'bg-gradient-to-br from-red-700 via-amber-600 to-yellow-500',
  },
  {
    id: 'anime',
    value: 'Polished anime illustration style, expressive character design, clean line art, soft cel shading, luminous color accents, detailed atmosphere.',
    icon: Sparkles,
    swatch: 'bg-gradient-to-br from-fuchsia-500 via-purple-400 to-sky-300 text-fuchsia-950',
  },
  {
    id: 'wallpaper',
    value: 'Minimal premium wallpaper style, spacious composition, refined lighting, elegant color contrast, calm background depth, suitable for desktop or mobile wallpaper.',
    icon: Monitor,
    swatch: 'bg-gradient-to-br from-slate-900 via-indigo-950 to-emerald-900',
  },
] as const

const MAX_INPUT_IMAGES = 10
const STUDIO_STARTERS = [
  { id: 'landscape', styleID: 'cinematic' },
  { id: 'product', styleID: 'commerce' },
  { id: 'architecture', styleID: 'wallpaper' },
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
  switch (imageSizeTierForModel(model)) {
    case '2k':
      return SIZE_OPTIONS.filter(option => SIZE_2K_VALUES.has(option.value))
    case '4k':
      return SIZE_OPTIONS.filter(option => SIZE_4K_VALUES.has(option.value))
    default:
      return SIZE_OPTIONS
  }
}

function aspectFromSize(size: string): AspectRatioId {
  return SIZE_TO_ASPECT[stringsTrimOrAuto(size)] ?? 'auto'
}

function sizeForAspect(model: string, aspect: AspectRatioId): string {
  if (aspect === 'auto') return 'auto'
  return ASPECT_RATIO_SIZE_MAP[imageSizeTierForModel(model)][aspect]
}

function normalizeImageSizeForModel(model: string, size: string): string {
  const value = stringsTrimOrAuto(size)
  if (sizeOptionsForModel(model).some(option => option.value === value)) return value
  return sizeForAspect(model, aspectFromSize(value))
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

// 生成中的画布显示已用时；任务结束后停止计时，避免无谓的重渲染。
function useElapsedSeconds(startedAt: string | undefined, active: boolean): number | null {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    setNow(Date.now())
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [active, startedAt])
  if (!active || !startedAt) return null
  const start = Date.parse(startedAt)
  if (Number.isNaN(start)) return null
  return Math.max(0, Math.floor((now - start) / 1000))
}

function jobModel(job: ImageGenerationJob): string {
  const params = jobParams(job)
  return params.model || job.assets?.[0]?.model || '-'
}

function jobRequestedSize(job: ImageGenerationJob): string {
  const params = jobParams(job)
  return params.size || job.assets?.[0]?.requested_size || job.assets?.[0]?.actual_size || 'Auto'
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
    return new Blob(chunks, {
      type: asset.mime_type || 'application/octet-stream',
    })
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
  const { showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [templates, setTemplates] = useState<ImagePromptTemplate[]>([])
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([])
  const [jobs, setJobs] = useState<ImageGenerationJob[]>([])
  const [historyJobs, setHistoryJobs] = useState<ImageGenerationJob[]>([])
  const [historyTotal, setHistoryTotal] = useState(0)
  const [historyPage, setHistoryPage] = useState(1)
  const [historyStatusFilter, setHistoryStatusFilter] = useState<ImageJobStatusFilter>('all')
  const [historyLoading, setHistoryLoading] = useState(false)
  const [historySearch, setHistorySearch] = useState('')
  const [assets, setAssets] = useState<ImageAsset[]>([])
  const [assetsLoading, setAssetsLoading] = useState(false)
  const [gallerySearch, setGallerySearch] = useState('')
  const [galleryOrientation, setGalleryOrientation] = useState<ImageOrientation>('all')
  const [galleryLayout, setGalleryLayout] = useState<'grid' | 'masonry'>('grid')
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
  const [templateFavoritesOnly, setTemplateFavoritesOnly] = useState(false)
  const [templateSort, setTemplateSort] = useState<ImageTemplateSort>('updated')
  const [selectedTemplateId, setSelectedTemplateId] = useState<number | null>(null)
  const [templateDialogOpen, setTemplateDialogOpen] = useState(false)
  const [templateDialogDraft, setTemplateDialogDraft] = useState<TemplateEditorDraft>(() => emptyTemplateDraft())
  const [templateDialogSaving, setTemplateDialogSaving] = useState(false)

  const [prompt, setPrompt] = useState('')
  const [model, setModel] = useState('gpt-image-2')
  const [size, setSize] = useState('auto')
  const [quality, setQuality] = useState('auto')
  useEffect(() => { setQuality(current => normalizeImageQualityForModel(current, model)) }, [model])
  const [outputFormat, setOutputFormat] = useState('png')
  const [background, setBackground] = useState('auto')
  const [upscale, setUpscale] = useState('')
  const [style, setStyle] = useState('')
  const [apiKeyID, setAPIKeyID] = useState('')
  const [templateName, setTemplateName] = useState('')
  const [templateTags, setTemplateTags] = useState('')
  const [imageToImageMode, setImageToImageMode] = useState(false)
  const [inputImageDataURLs, setInputImageDataURLs] = useState<string[]>([])
  const inputImageDataURLsRef = useRef(inputImageDataURLs)
  inputImageDataURLsRef.current = inputImageDataURLs
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [saveTemplateOpen, setSaveTemplateOpen] = useState(false)
  const [templatePickerOpen, setTemplatePickerOpen] = useState(false)
  const promptInputRef = useRef<HTMLTextAreaElement>(null)
  const templateNameInputRef = useRef<HTMLInputElement>(null)
  const inspirationIndexRef = useRef(0)

  useEffect(() => {
    if (advancedOpen && saveTemplateOpen) templateNameInputRef.current?.focus()
  }, [advancedOpen, saveTemplateOpen])

  const appendInputImages = useCallback((files: FileList | File[]) => {
    const list = Array.from(files).filter(file => file.type.startsWith('image/'))
    if (list.length === 0) return

      // Read length outside the state updater so toast/FileReader side effects run once
      // (React may re-invoke pure updaters under StrictMode / concurrent rendering).
      const prevLength = inputImageDataURLsRef.current.length
      if (prevLength >= MAX_INPUT_IMAGES) {
        showToast(t('images.maxInputImages', { max: MAX_INPUT_IMAGES }), 'error')
        return
      }
      const remaining = MAX_INPUT_IMAGES - prevLength
      const filesToRead = list.slice(0, remaining)
      if (list.length > remaining) {
        showToast(t('images.maxInputImages', { max: MAX_INPUT_IMAGES }), 'error')
      }

      void Promise.allSettled(
        filesToRead.map(
          file =>
            new Promise<string>((resolve, reject) => {
              const reader = new FileReader()
              reader.onload = () => resolve(reader.result as string)
              reader.onerror = () => reject(new Error('Failed to read file'))
              reader.readAsDataURL(file)
            }),
        ),
      ).then(results => {
        const dataURLs: string[] = []
        for (const r of results) {
          if (r.status === 'fulfilled') dataURLs.push(r.value)
        }
        if (dataURLs.length > 0) {
          setInputImageDataURLs(current => [...current, ...dataURLs].slice(0, MAX_INPUT_IMAGES))
        }
        if (dataURLs.length < results.length) {
          showToast(t('images.loadFailed'), 'error')
        }
      })
    },
    [showToast, t],
  )

  const handleImageFileChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      if (e.target.files?.length) appendInputImages(e.target.files)
      e.target.value = ''
    },
    [appendInputImages],
  )

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
    const tags = new Map<string, string>()
    templates.forEach(template => template.tags.forEach(tag => {
      const key = tag.normalize('NFKC').trim().toLowerCase()
      if (key && !tags.has(key)) tags.set(key, tag.trim())
    }))
    return Array.from(tags.values()).sort((a, b) => a.localeCompare(b))
  }, [templates])
  const loadTemplates = useCallback(async () => {
    const res = await api.getImagePromptTemplates()
    setTemplates(res.templates ?? [])
  }, [])

  const loadJobs = useCallback(async () => {
    const res = await api.getImageJobs({ page: 1, pageSize: 3 })
    setJobs(res.jobs ?? [])
  }, [])

  const loadHistoryJobs = useCallback(async () => {
    setHistoryLoading(true)
    try {
      const res = await api.getImageJobs({ page: historyPage, pageSize: IMAGE_JOB_HISTORY_PAGE_SIZE })
      const total = res.total ?? 0
      const lastPage = Math.max(1, Math.ceil(total / IMAGE_JOB_HISTORY_PAGE_SIZE))
      setHistoryTotal(total)
      if (historyPage > lastPage) { setHistoryPage(lastPage); return }
      setHistoryJobs(res.jobs ?? [])
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.loadFailed'), 'error')
    } finally {
      setHistoryLoading(false)
    }
  }, [historyPage, showToast, t])

  const loadAssets = useCallback(async () => {
    setAssetsLoading(true)
    try {
      const res = await api.getImageAssets({ page: assetPage, pageSize: IMAGE_ASSET_PAGE_SIZE })
      const total = res.total ?? 0
      const lastPage = Math.max(1, Math.ceil(total / IMAGE_ASSET_PAGE_SIZE))
      setAssetTotal(total)
      if (assetPage > lastPage) { setAssetPage(lastPage); return }
      setAssets(res.assets ?? [])
    } finally {
      setAssetsLoading(false)
    }
  }, [assetPage])

  const loadInitial = useCallback(async () => {
    setLoading(true)
    try {
      const [keysRes] = await Promise.all([
        api.getAPIKeys({ view: 'lite' }),
        loadTemplates(),
        loadJobs(),
      ])
      setAPIKeys(keysRes.keys ?? [])
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('images.loadFailed'), 'error')
    } finally {
      setLoading(false)
    }
  }, [loadJobs, loadTemplates, showToast, t])

  useEffect(() => {
    void loadInitial()
  }, [loadInitial])

  useEffect(() => {
    void loadAssets().catch(err => {
      showToast(err instanceof Error ? err.message : t('images.loadFailed'), 'error')
    })
  }, [loadAssets, showToast, t])

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
    let cancelled = false
    let polling = false
    const timer = window.setInterval(async () => {
      if (polling || document.visibilityState !== 'visible') return
      polling = true
      try {
        const res = await api.getImageJob(currentJob.id, {
          includeCache: false,
        })
        if (cancelled) return
        setCurrentJob(res.job)
        if (!['queued', 'running'].includes(res.job.status)) {
          await Promise.all([loadJobs(), loadAssets(), loadTemplates(), loadHistoryJobs()])
        }
      } catch {
        // keep polling quiet; the visible job state is enough context
      } finally { polling = false }
    }, 4000)
    return () => { cancelled = true; window.clearInterval(timer) }
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

  const rerunFromJob = async (summary: ImageGenerationJob) => {
    let job: ImageGenerationJob
    try { job = (await api.getImageJob(summary.id, { summary: false })).job }
    catch (err) { showToast(err instanceof Error ? err.message : t('images.loadFailed'), 'error'); return }

    const params = jobParams(job)
    if (params.input_images?.some(value => value.startsWith('queue-input:'))) {
      showToast('参考图临时文件已清理，请重新选择参考图后生成', 'error'); return
    }
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
        const res = await api.getImageJob(currentJob.id, {
          includeCache: false,
        })
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
  const filteredTemplates = useMemo(() => selectImageTemplates(templates, {
    query: templateSearch, tag: selectedTag, favoritesOnly: templateFavoritesOnly, sort: templateSort,
  }), [templates, templateSearch, selectedTag, templateFavoritesOnly, templateSort])
  const filteredAssets = useMemo(() => selectImageAssets(assets, {
    query: gallerySearch, orientation: galleryOrientation,
  }, promptForAsset), [assets, gallerySearch, galleryOrientation, promptForAsset])
  const filteredHistoryJobs = useMemo(() => selectImageJobs(historyJobs, {
    query: historySearch, status: historyStatusFilter,
  }), [historyJobs, historySearch, historyStatusFilter])
  const historyGroups = useMemo(() => {
    const groups = new Map<string, ImageGenerationJob[]>()
    for (const job of filteredHistoryJobs) {
      const day = formatBeijingTime(job.created_at).split(' ')[0]
      const group = groups.get(day) ?? []
      group.push(job)
      groups.set(day, group)
    }
    return Array.from(groups.entries())
  }, [filteredHistoryJobs])
  const previewAssets = activeView === 'gallery' ? filteredAssets : activeView === 'history' ? filteredHistoryJobs.flatMap(job => job.assets ?? []) : currentJob?.assets ?? []
  const previewIndex = previewAsset ? previewAssets.findIndex(asset => asset.id === previewAsset.id) : -1
  const navigatePreview = (direction: -1 | 1) => {
    const next = previewAssets[previewIndex + direction]
    if (previewIndex >= 0 && next) setPreviewAsset(next)
  }
  const templateFavorites = templates.filter(template => template.favorite).length
  const templateFiltersActive = Boolean(templateSearch.trim() || selectedTag || templateFavoritesOnly)
  const galleryFiltersActive = Boolean(gallerySearch.trim() || galleryOrientation !== 'all')
  const historyFiltersActive = Boolean(historySearch.trim() || historyStatusFilter !== 'all')
  const resetTemplateFilters = () => { setTemplateSearch(''); setSelectedTag(''); setTemplateFavoritesOnly(false) }
  const resetGalleryFilters = () => { setGallerySearch(''); setGalleryOrientation('all') }
  const resetHistoryFilters = () => { setHistorySearch(''); setHistoryStatusFilter('all') }
  const openStarterTemplate = (index: number) => {
    const starter = STUDIO_STARTERS[index]
    setTemplateDialogDraft({
      ...emptyTemplateDraft(),
      name: t(`images.workspace.starters.${starter.id}.title`),
      prompt: t(`images.workspace.starters.${starter.id}.prompt`),
      style: STYLE_PRESETS.find(preset => preset.id === starter.styleID)?.value || '',
    })
    setTemplateDialogOpen(true)
  }
  const templateSelectOptions =
    templates.length > 0
      ? [
          { label: t('images.noTemplateSelected'), value: '' },
          ...templates.map(template => ({
            label: template.name || `#${template.id}`,
            value: String(template.id),
          })),
        ]
      : [{ label: t('images.noTemplates'), value: '' }]
  const backgroundOptions = useMemo(
    () => [
      { label: t('images.backgroundOptions.auto'), value: 'auto' },
      { label: t('images.backgroundOptions.opaque'), value: 'opaque' },
      {
        label: t('images.backgroundOptions.transparent'),
        value: 'transparent',
      },
    ],
    [t],
  )
  const upscaleOptions = useMemo(
    () => [
      { label: t('images.upscaleOptions.auto'), value: '' },
      { label: t('images.upscaleOptions.none'), value: 'none' },
      { label: t('images.upscaleOptions.2k'), value: '2k' },
      { label: t('images.upscaleOptions.4k'), value: '4k' },
    ],
    [t],
  )
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
    setSize(current => sizeForAspect(value, aspectFromSize(current)))
  }

  const selectedAspect = aspectFromSize(size)
  const selectedPreset = STYLE_PRESETS.find(preset => preset.value === style.trim())
  const advancedCount = [quality !== 'auto', outputFormat !== 'png', background !== 'auto', Boolean(upscale), Boolean(apiKeyID), Boolean(style.trim() && !selectedPreset)].filter(Boolean).length

  const applyInspiration = (index: number) => {
    const starter = STUDIO_STARTERS[index % STUDIO_STARTERS.length]
    setPrompt(t(`images.workspace.starters.${starter.id}.prompt`))
    setStyle(STYLE_PRESETS.find(preset => preset.id === starter.styleID)?.value || '')
    setSelectedTemplateId(null)
    inspirationIndexRef.current = index + 1
    promptInputRef.current?.focus()
  }

  const submitGeneration = () => {
    if (!submitting) void submitJob(createJobPayload(), imageToImageMode ? 'edit' : 'text')
  }

  const generationForm = (
    <section className="studio-composer" aria-label={t('images.workspace.createPanel')}>
      <div className="studio-panel-heading">
        <h2><SlidersHorizontal className="size-4 text-primary" />{t('images.workspace.createPanel')}</h2>
        <span>CREATE</span>
      </div>
      <div className="studio-form-scroll">
        <div className="studio-mode-switch" role="group" aria-label={t('images.mode')}>
          <button type="button" aria-pressed={!imageToImageMode} onClick={() => setImageToImageMode(false)}>
            <Wand2 className="size-3.5" />{t('images.textToImage')}
          </button>
          <button type="button" aria-pressed={imageToImageMode} onClick={() => setImageToImageMode(true)}>
            <Images className="size-3.5" />{t('images.imageToImage')}
          </button>
        </div>

        <div>
          <div className="studio-field-heading">
            <label htmlFor="image-studio-prompt">{t('images.prompt')}</label>
            <button type="button" className="studio-text-action" onClick={() => applyInspiration(inspirationIndexRef.current)}>
              <Shuffle className="size-3" />{t('images.workspace.inspireMe')}
            </button>
          </div>
          <div className="studio-prompt-editor">
            <textarea
              ref={promptInputRef}
              id="image-studio-prompt"
              value={prompt}
              onChange={e => setPrompt(e.target.value)}
              onKeyDown={e => {
                if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  submitGeneration()
                }
              }}
              placeholder={t('images.workspace.promptPlaceholder')}
            />
            <div className="studio-prompt-footer">
              <span className="tabular-nums">{t('images.promptChars', { count: prompt.length })}</span>
              <kbd title={t('images.promptShortcut')}>⌘ / Ctrl ↵</kbd>
            </div>
          </div>
          <div className="mt-2.5 flex items-center justify-between gap-2">
            <button
              type="button"
              className="studio-text-action"
              aria-expanded={templatePickerOpen}
              aria-controls="studio-template-picker"
              onClick={() => setTemplatePickerOpen(open => !open)}
            >
              <LayoutTemplate className="size-3" />{t('images.selectTemplate')}
              <ChevronDown className={cn('size-3 transition-transform', templatePickerOpen && 'rotate-180')} />
            </button>
            <button
              type="button"
              className="studio-text-action text-muted-foreground disabled:opacity-40"
              disabled={!prompt.trim()}
              onClick={() => { setAdvancedOpen(true); setSaveTemplateOpen(true) }}
            >
              <Save className="size-3" />{t('images.saveAsTemplate')}
            </button>
          </div>
          {templatePickerOpen && (
            <div id="studio-template-picker" className="mt-2">
              <Select
                value={selectedTemplateId ? String(selectedTemplateId) : ''}
                onValueChange={selectTemplateForGeneration}
                options={templateSelectOptions}
                disabled={templates.length === 0}
                compact
              />
            </div>
          )}
        </div>

        {imageToImageMode && (
          <ReferenceImageDropzone
            images={inputImageDataURLs}
            onFiles={appendInputImages}
            onFileInput={handleImageFileChange}
            onRemove={index => setInputImageDataURLs(prev => prev.filter((_, i) => i !== index))}
            compact
          />
        )}

        <div className="studio-settings-section">
          <Field label={t('images.model')}>
            <Select value={model} onValueChange={changeGenerationModel} options={IMAGE_MODELS} compact />
          </Field>
          <div>
            <div className="studio-field-heading">
              <div>{t('images.aspectRatio')}</div>
              <span className="font-mono">{size === 'auto' ? t('images.workspace.adaptiveSize') : size.replace('x', ' × ')}</span>
            </div>
            <div className="studio-ratios" role="group" aria-label={t('images.aspectRatio')}>
              {ASPECT_RATIO_IDS.map(aspect => {
                const Icon = ASPECT_RATIO_ICONS[aspect]
                const aspectLabel = t(`images.aspect.${aspect === '1:1' ? 'square' : aspect === '16:9' ? 'landscape' : aspect === '9:16' ? 'portrait' : 'auto'}`)
                return (
                  <button key={aspect} type="button" aria-pressed={selectedAspect === aspect} onClick={() => setSize(sizeForAspect(model, aspect))}>
                    <Icon /><span>{aspectLabel}</span>
                  </button>
                )
              })}
            </div>
          </div>
          <StylePresetPicker value={style} onChange={setStyle} studio />
        </div>

        <div className="border-t border-border pt-1">
          <button
            type="button"
            className="studio-advanced-toggle"
            aria-expanded={advancedOpen}
            aria-controls="studio-advanced-params"
            onClick={() => setAdvancedOpen(open => !open)}
          >
            <span className="flex items-center gap-2">
              <SlidersHorizontal className="size-3.5" />{t('images.advancedParams')}
              {advancedCount > 0 && <span className="rounded bg-primary/10 px-1.5 text-[10px] text-primary">{advancedCount}</span>}
            </span>
            <ChevronDown className={cn('size-3.5 transition-transform', advancedOpen && 'rotate-180')} />
          </button>
          {advancedOpen && (
            <div id="studio-advanced-params" className="studio-advanced-content">
              <Field label={t('images.quality')}>
                <Select value={quality} onValueChange={setQuality} options={imageQualityOptions(model).map(option => ({ ...option, label: t(`images.workspace.quality.${option.value}`) }))} compact />
              </Field>
              <Field label={t('images.format')}>
                <Select value={outputFormat} onValueChange={setOutputFormat} options={FORMAT_OPTIONS} compact />
              </Field>
              <Field label={t('images.background')}>
                <Select value={background} onValueChange={setBackground} options={backgroundOptions} compact />
              </Field>
              <Field label={t('images.localUpscale')}>
                <Select value={upscale} onValueChange={setUpscale} options={upscaleOptions} compact />
              </Field>
              <div className="studio-full-field">
                <Field label={t('images.apiKey')}>
                  <Select
                    value={apiKeyID}
                    onValueChange={setAPIKeyID}
                    options={[
                      { label: t('images.autoApiKey'), value: '' },
                      ...apiKeys.map(key => ({ label: key.name ? `${key.name} · ${key.key}` : key.key, value: String(key.id) })),
                    ]}
                    compact
                  />
                </Field>
              </div>
              <div className="studio-full-field">
                <Field label={t('images.style')}>
                  <Input value={style} onChange={e => setStyle(e.target.value)} placeholder={t('images.stylePlaceholder')} />
                </Field>
              </div>
              <div className="studio-full-field space-y-2 border-t border-border pt-3">
                <button type="button" className="studio-text-action" aria-expanded={saveTemplateOpen} aria-controls="studio-save-template" onClick={() => setSaveTemplateOpen(open => !open)}>
                  <Save className="size-3" />{t('images.saveTemplateSection')}
                  <ChevronDown className={cn('size-3 transition-transform', saveTemplateOpen && 'rotate-180')} />
                </button>
                {saveTemplateOpen && (
                  <div id="studio-save-template" className="space-y-2">
                    <Input ref={templateNameInputRef} aria-label={t('images.templateName')} value={templateName} onChange={e => setTemplateName(e.target.value)} placeholder={t('images.templateName')} />
                    <Input aria-label={t('images.templateTags')} value={templateTags} onChange={e => setTemplateTags(e.target.value)} placeholder={t('images.templateTags')} />
                    <Button className="w-full" variant="outline" size="sm" disabled={!prompt.trim()} onClick={() => void saveCurrentPromptAsTemplate()}>
                      <Save className="size-3.5" />{t('images.saveTemplate')}
                    </Button>
                  </div>
                )}
              </div>
            </div>
          )}
        </div>
      </div>

      <div className="studio-submit-area">
        <div className="studio-submit-summary">
          <span className="flex min-w-0 items-center gap-1.5">
            <span>{outputFormat.toUpperCase()}</span><span>·</span>
            <span>{t(`images.workspace.quality.${quality}`, { defaultValue: quality })}</span>
            {selectedPreset && <><span>·</span><span className="truncate">{t(`images.stylePreset.${selectedPreset.id}`)}</span></>}
          </span>
          <button type="button" className="inline-flex shrink-0 items-center gap-1 rounded disabled:opacity-40 hover:text-foreground" disabled={submitting || !hasGenerationDraft} onClick={clearGenerationForm}>
            <RefreshCcw className="size-3" />{t('images.clearSelection')}
          </button>
        </div>
        <Button className="studio-submit-button" disabled={submitting || !prompt.trim()} onClick={submitGeneration}>
          {submitting ? <Loader2 className="size-4 animate-spin" /> : <Wand2 className="size-4" />}
          {submitting ? t('images.workspace.submitting') : t('images.generateImage')}
          <ArrowRight className="ml-auto size-4" />
        </Button>
      </div>
    </section>
  )

  const studioCanvas = (
    <StudioCanvas
      currentJob={currentJob}
      latestAsset={latestAsset}
      imageURL={latestAsset ? assetPreviewURL(latestAsset, assetURLs) : undefined}
      prompt={currentJob?.prompt || (latestAsset ? promptForAsset(latestAsset) : '')}
      onInspire={applyInspiration}
      onPreview={() => latestAsset && setPreviewAsset(latestAsset)}
      onDownload={() => latestAsset && void downloadAsset(latestAsset)}
      onCopyPrompt={() => {
        if (currentJob?.prompt) void copyPrompt(currentJob.prompt)
        else if (latestAsset) void copyPrompt(promptForAsset(latestAsset) || latestAsset.revised_prompt || '')
      }}
      onRerun={() => currentJob && rerunFromJob(currentJob)}
      onSaveTemplate={() => latestAsset && void saveAssetPromptAsTemplate(latestAsset)}
      onDelete={() => latestAsset && void deleteAsset(latestAsset)}
    />
  )

  const templateLibrary = (
    <section className="studio-collection">
      <StudioCollectionHero eyebrow="PROMPT LIBRARY" title={t('images.collections.promptsTitle')} description={t('images.collections.promptsDescription')}
        actions={<Button size="sm" onClick={openNewTemplateDialog}><Plus className="size-3.5" />{t('images.newTemplate')}</Button>}>
        <div className="studio-collection-stats">
          <div className="studio-collection-stat"><span><LayoutTemplate className="size-3.5" />{t('images.collections.totalTemplates')}</span><strong>{templates.length}</strong></div>
          <span className="studio-collection-divider" aria-hidden="true" />
          <div className="studio-collection-stat"><span><Star className="size-3.5" />{t('images.collections.favorites')}</span><strong>{templateFavorites}</strong></div>
        </div>
      </StudioCollectionHero>

      <div className="studio-library-layout">
        <aside className="studio-library-sidebar" aria-label={t('images.collections.libraryFilters')}>
          <h3>{t('images.collections.myLibrary')}</h3>
          <button type="button" className="studio-library-filter" aria-pressed={!templateFavoritesOnly && !selectedTag} onClick={() => { setTemplateFavoritesOnly(false); setSelectedTag('') }}>
            <FolderOpen className="size-4" /><span>{t('images.collections.allTemplates')}</span><small>{templates.length}</small>
          </button>
          <button type="button" className="studio-library-filter" aria-pressed={templateFavoritesOnly} onClick={() => { setTemplateFavoritesOnly(true); setSelectedTag('') }}>
            <Star className="size-4" /><span>{t('images.collections.favorites')}</span><small>{templateFavorites}</small>
          </button>
          {allTags.length > 0 && (
            <div className="studio-library-tags">
              <h3>{t('images.collections.tags')}</h3>
              {allTags.map(tag => (
                <button key={tag} type="button" className="studio-library-filter" aria-pressed={selectedTag === tag} onClick={() => { setSelectedTag(selectedTag === tag ? '' : tag); setTemplateFavoritesOnly(false) }}>
                  <Tag className="size-3.5" /><span title={tag}>{tag}</span><small>{templates.filter(template => template.tags.some(item => item.normalize('NFKC').trim().toLowerCase() === tag.normalize('NFKC').toLowerCase())).length}</small>
                </button>
              ))}
            </div>
          )}
          <p className="studio-library-tip">{t('images.collections.libraryTip')}</p>
        </aside>
        <div className="studio-library-content">
          <div className="studio-collection-toolbar">
            <StudioCollectionSearch value={templateSearch} onChange={setTemplateSearch} placeholder={t('images.searchTemplates')} />
            <label className="studio-collection-sort"><span className="sr-only">{t('images.collections.sortTemplates')}</span>
              <Select value={templateSort} onValueChange={value => setTemplateSort(value as ImageTemplateSort)} options={['updated', 'used', 'name'].map(value => ({ value, label: t(`images.collections.sort.${value}`) }))} compact />
            </label>
          </div>
          <div className="studio-collection-result-count" aria-live="polite">
            <span>{t('images.collections.templateResults', { count: filteredTemplates.length })}</span>
            {templateFiltersActive && <button type="button" className="studio-text-action" onClick={resetTemplateFilters}><X className="size-3" />{t('images.collections.resetFilters')}</button>}
          </div>
          {loading ? <StudioCollectionLoading /> : filteredTemplates.length > 0 ? (
            <div className="studio-template-grid">
              {filteredTemplates.map(template => (
                <TemplateCard key={template.id} template={template} active={selectedTemplateId === template.id} onApply={() => applyTemplate(template)} onFavorite={() => void toggleFavorite(template)} onEdit={() => openEditTemplateDialog(template)} onDelete={() => void deleteTemplate(template)} />
              ))}
              {!templateFiltersActive && <button type="button" className="studio-template-add" onClick={openNewTemplateDialog}><span><Plus className="size-5" /></span>{t('images.collections.addTemplate')}</button>}
            </div>
          ) : (
            <StudioCollectionEmpty icon={templateFiltersActive ? Search : LayoutTemplate} title={t(templateFiltersActive ? 'images.collections.noMatches' : 'images.collections.emptyTemplatesTitle')} description={t(templateFiltersActive ? 'images.collections.noTemplateMatches' : 'images.collections.emptyTemplatesDescription')}>
              <Button size="sm" variant={templateFiltersActive ? 'outline' : 'default'} onClick={templateFiltersActive ? resetTemplateFilters : openNewTemplateDialog}>
                {templateFiltersActive ? <RefreshCcw className="size-3.5" /> : <Plus className="size-3.5" />}{t(templateFiltersActive ? 'images.collections.resetFilters' : 'images.newTemplate')}
              </Button>
            </StudioCollectionEmpty>
          )}
          {!loading && templates.length === 0 && !templateFiltersActive && (
            <div className="studio-template-starters">
              <h3>{t('images.collections.templateStarters')}</h3>
              <div className="studio-template-starter-grid">
                {STUDIO_STARTERS.map((starter, index) => (
                  <button key={starter.id} type="button" className="studio-template-starter" onClick={() => openStarterTemplate(index)}>
                    <StudioArtwork scene={starter.id} />
                    <span><strong>{t(`images.workspace.starters.${starter.id}.title`)}</strong><small>{t('images.collections.editStarter')}</small></span>
                    <ArrowUpRight className="size-3.5 shrink-0 text-muted-foreground" />
                  </button>
                ))}
              </div>
            </div>
          )}
        </div>
      </div>
    </section>
  )

  const timelineJobs = currentJob
    ? [currentJob, ...recentJobs.filter(job => job.id !== currentJob.id)]
    : recentJobs
  const jobTimelinePanel = (
    <section className="studio-timeline" aria-label={t('images.recentJobs')}>
      <div className="studio-timeline-heading">
        <h2><History className="size-3.5 text-muted-foreground" />{t('images.recentJobs')}</h2>
        <div className="flex items-center gap-1">
          <Button size="xs" variant="ghost" onClick={() => navigate('/images/history')}>
            {t('images.viewAllJobs')}<ArrowUpRight className="size-3" />
          </Button>
          <Button size="icon-xs" variant="ghost" aria-label={t('images.workspace.refreshJobs')} onClick={() => void loadJobs()}>
            <RefreshCcw className="size-3" />
          </Button>
        </div>
      </div>
      <div className="studio-timeline-scroll">
        {loading && timelineJobs.length === 0 ? (
          <div className="flex min-h-16 items-center gap-2 text-xs text-muted-foreground" role="status">
            <Loader2 className="size-3.5 animate-spin" />{t('common.loading')}
          </div>
        ) : timelineJobs.length === 0 ? (
          <div className="flex min-h-16 w-full items-center gap-3 rounded-lg border border-dashed border-border px-3 text-xs text-muted-foreground">
            <ImageIcon className="size-5 opacity-50" />{t('images.workspace.noRecentJobs')}
          </div>
        ) : timelineJobs.map(job => {
          const thumb = job.assets?.[0]
          const thumbURL = thumb ? assetThumbnailURL(thumb, assetURLs) : undefined
          return (
            <button key={job.id} type="button" className="studio-job" aria-pressed={currentJob?.id === job.id} onClick={() => setCurrentJob(job)}>
              <span className="studio-job-thumb">
                {thumbURL ? <img src={thumbURL} alt="" className="size-full object-cover" /> : isImageJobBusy(job) ? <Loader2 className="size-4 animate-spin text-primary" /> : <ImageIcon className="size-4 text-muted-foreground" />}
              </span>
              <span className="min-w-0 flex-1">
                <span className="mb-1 block truncate text-[11px] font-medium">{job.prompt || `#${job.id}`}</span>
                <span className="flex items-center justify-between gap-2">
                  <span className={cn('rounded px-1.5 py-0.5 text-[9px]', jobStatusClass(job.status))}>
                    {t(`images.status.${job.status}`, { defaultValue: job.status })}
                  </span>
                  <span className="text-[9px] tabular-nums text-muted-foreground">{formatRelativeTime(job.created_at, { variant: 'compact' })}</span>
                </span>
              </span>
            </button>
          )
        })}
      </div>
    </section>
  )

  const historyStatusOptions: Array<{
    value: ImageJobStatusFilter
    label: string
  }> = [
    { value: 'all', label: t('common.all') },
    ...IMAGE_JOB_STATUSES.map(status => ({
      value: status,
      label: t(`images.status.${status}`),
    })),
  ]

  const selectHistoryJob = (job: ImageGenerationJob) => {
    setCurrentJob(job)
    navigate('/images/studio')
    void api.getImageJob(job.id, { includeCache: false }).then(res => setCurrentJob(res.job)).catch(() => {
      // The selected history row is already enough if the refresh fails.
    })
  }

  const historyView = (
    <section className="studio-collection">
      <StudioCollectionHero eyebrow="CREATION HISTORY" title={t('images.collections.historyTitle')} description={t('images.collections.historyDescription')}
        actions={<Button variant="outline" size="sm" disabled={historyLoading} onClick={() => void loadHistoryJobs()}><RefreshCcw className={cn('size-3.5', historyLoading && 'animate-spin')} />{t('images.collections.refreshHistory')}</Button>}>
        <div className="studio-collection-stats"><div className="studio-collection-stat"><span><History className="size-3.5" />{t('images.collections.totalJobs')}</span><strong>{historyTotal}</strong><small>{t('images.collections.savedHistory')}</small></div></div>
      </StudioCollectionHero>
      <div>
        <div className="mb-2 flex items-center gap-1.5 text-[10px] text-muted-foreground"><Info className="size-3" />{t('images.collections.pageOverview')}</div>
        <div className="studio-history-stats">
          {[
            { label: 'pageJobs', count: historyJobs.length, icon: History, tone: 'neutral' },
            { label: 'completedJobs', count: historyJobs.filter(job => job.status === 'succeeded').length, icon: CheckCircle2, tone: 'success' },
            { label: 'activeJobs', count: historyJobs.filter(isImageJobBusy).length, icon: Timer, tone: 'busy' },
            { label: 'failedJobs', count: historyJobs.filter(job => job.status === 'failed').length, icon: CircleAlert, tone: 'error' },
          ].map(stat => <div key={stat.label} className="studio-history-stat" data-tone={stat.tone}><span><stat.icon className="size-4" /></span><span><strong>{stat.count}</strong><small>{t(`images.collections.${stat.label}`)}</small></span></div>)}
        </div>
      </div>
      <div>
        <div className="studio-collection-toolbar">
          <StudioCollectionSearch value={historySearch} onChange={setHistorySearch} placeholder={t('images.collections.searchHistory')} />
          <div className="flex flex-wrap items-center gap-1.5" role="group" aria-label={t('images.collections.statusFilter')}>
            {historyStatusOptions.map(option => <FilterChip key={option.value} active={historyStatusFilter === option.value} onClick={() => setHistoryStatusFilter(option.value)}>{option.label}</FilterChip>)}
          </div>
        </div>
        <div className="studio-collection-result-count" aria-live="polite">
          <span>{t('images.collections.jobResults', { count: filteredHistoryJobs.length })}</span>
          {historyFiltersActive && <button type="button" className="studio-text-action" onClick={resetHistoryFilters}><X className="size-3" />{t('images.collections.resetFilters')}</button>}
        </div>
        {historyLoading ? <StudioCollectionLoading rows /> : filteredHistoryJobs.length > 0 ? (
          <div className="studio-history-groups">
            {historyGroups.map(([day, dayJobs]) => (
              <section key={day} aria-label={day}>
                <h3 className="studio-history-date"><span>{day}</span><span>{t('images.collections.dayJobs', { count: dayJobs.length })}</span></h3>
                <div className="studio-history-list">
                  {dayJobs.map(job => <HistoryJobCard key={job.id} job={job} imageURLs={assetURLs} onSelect={() => selectHistoryJob(job)} onPreview={asset => setPreviewAsset(asset)} onDownload={asset => void downloadAsset(asset)} onCopyPrompt={() => void copyPrompt(job.prompt)} onRerun={() => rerunFromJob(job)} onSaveTemplate={asset => void saveAssetPromptAsTemplate(asset)} onDeleteJob={() => void deleteJob(job)} onDelete={asset => void deleteAsset(asset)} />)}
                </div>
              </section>
            ))}
          </div>
        ) : (
          <StudioCollectionEmpty icon={historyFiltersActive ? Search : History} title={t(historyFiltersActive ? 'images.collections.noMatches' : 'images.collections.emptyHistoryTitle')} description={t(historyFiltersActive ? 'images.collections.noPageMatches' : 'images.collections.emptyHistoryDescription')}>
            <Button size="sm" variant="outline" onClick={historyFiltersActive ? resetHistoryFilters : () => navigate('/images/studio')}>{t(historyFiltersActive ? 'images.collections.resetFilters' : 'images.collections.startCreating')}<ArrowRight className="size-3.5" /></Button>
          </StudioCollectionEmpty>
        )}
      </div>
      <StudioCollectionPagination page={historyPage} pageSize={IMAGE_JOB_HISTORY_PAGE_SIZE} total={historyTotal} loading={historyLoading} onChange={setHistoryPage} />
    </section>
  )

  const galleryView = (
    <section className="studio-collection">
      <StudioCollectionHero eyebrow="YOUR GALLERY" title={t('images.collections.galleryTitle')} description={t('images.collections.galleryDescription')}
        actions={<Button size="sm" onClick={() => navigate('/images/studio')}><Plus className="size-3.5" />{t('images.collections.continueCreating')}</Button>}>
        <div className="studio-collection-stats"><div className="studio-collection-stat"><span><Images className="size-3.5" />{t('images.collections.totalImages')}</span><strong>{assetTotal}</strong><small>{t('images.collections.savedImages')}</small></div></div>
      </StudioCollectionHero>
      <div>
        <div className="studio-collection-toolbar">
          <StudioCollectionSearch value={gallerySearch} onChange={setGallerySearch} placeholder={t('images.collections.searchGallery')} />
          <div className="studio-gallery-controls">
            <span className="studio-page-filter-note">{t('images.collections.pageFilters')}</span>
            <div className="studio-gallery-orientation" role="group" aria-label={t('images.collections.orientationFilter')}>
              {(['all', 'square', 'landscape', 'portrait'] as const).map(orientation => <FilterChip key={orientation} active={galleryOrientation === orientation} onClick={() => setGalleryOrientation(orientation)}>{t(`images.collections.orientation.${orientation}`)}</FilterChip>)}
            </div>
          </div>
          <Button size="icon-sm" variant="ghost" disabled={assetsLoading} aria-label={t('images.collections.refreshGallery')} title={t('images.collections.refreshGallery')} onClick={() => void loadAssets().catch(err => showToast(err instanceof Error ? err.message : t('images.loadFailed'), 'error'))}><RefreshCcw className={cn('size-3.5', assetsLoading && 'animate-spin')} /></Button>
        </div>
        <div className="studio-collection-result-count">
          <span aria-live="polite">{t('images.collections.imageResults', { count: filteredAssets.length })}</span>
          {galleryFiltersActive && <button type="button" className="studio-text-action" onClick={resetGalleryFilters}><X className="size-3" />{t('images.collections.resetFilters')}</button>}
          <div className="studio-layout-switch" role="group" aria-label={t('images.collections.galleryLayout')}>
            <button type="button" aria-pressed={galleryLayout === 'grid'} aria-label={t('images.collections.gridView')} title={t('images.collections.gridView')} onClick={() => setGalleryLayout('grid')}><Grid2X2 className="size-3.5" /></button>
            <button type="button" aria-pressed={galleryLayout === 'masonry'} aria-label={t('images.collections.masonryView')} title={t('images.collections.masonryView')} onClick={() => setGalleryLayout('masonry')}><Columns3 className="size-3.5" /></button>
          </div>
        </div>
        {loading || assetsLoading ? <StudioCollectionLoading /> : filteredAssets.length > 0 ? (
          <div className="studio-gallery-grid" data-layout={galleryLayout}>
            {filteredAssets.map(asset => <AssetCard key={asset.id} asset={asset} imageURL={assetThumbnailURL(asset, assetURLs)} prompt={promptForAsset(asset)} onPreview={() => setPreviewAsset(asset)} onDownload={() => void downloadAsset(asset)} onDelete={() => void deleteAsset(asset)} onCopyPrompt={() => void copyPrompt(promptForAsset(asset) || asset.revised_prompt || '')} onRerun={() => rerunFromAsset(asset)} onSaveTemplate={() => void saveAssetPromptAsTemplate(asset)} />)}
          </div>
        ) : (
          <StudioCollectionEmpty icon={galleryFiltersActive ? Search : Images} title={t(galleryFiltersActive ? 'images.collections.noMatches' : 'images.collections.emptyGalleryTitle')} description={t(galleryFiltersActive ? 'images.collections.noPageMatches' : 'images.collections.emptyGalleryDescription')}>
            <Button size="sm" variant="outline" onClick={galleryFiltersActive ? resetGalleryFilters : () => navigate('/images/studio')}>{t(galleryFiltersActive ? 'images.collections.resetFilters' : 'images.collections.startCreating')}<ArrowRight className="size-3.5" /></Button>
          </StudioCollectionEmpty>
        )}
      </div>
      <StudioCollectionPagination page={assetPage} pageSize={IMAGE_ASSET_PAGE_SIZE} total={assetTotal} loading={assetsLoading || loading} onChange={setAssetPage} />
    </section>
  )

  return (
    <div className="image-studio-page">
      <header className="studio-header">
        <div>
          <div className="studio-eyebrow">IMAGE STUDIO</div>
          <h1>{t('images.title')}</h1>
          <p>{t('images.workspace.description')}</p>
        </div>
        <div className="studio-header-note">
          <ValuePill tone="neutral">
            <span className="inline-flex items-center gap-1.5">
              <Images className="size-3.5" />
              {t('images.imageCount', { count: assetTotal })}
            </span>
          </ValuePill>
          <ValuePill tone="primary">
            <span className="inline-flex items-center gap-1.5">
              <Sparkles className="size-3.5" />
              {t('images.workspace.headerNote')}
            </span>
          </ValuePill>
        </div>
      </header>
      <ImageStudioTabs activeView={activeView} />
      {confirmDialog}

      {activeView === 'studio' && (
        <>
          <div className="studio-workspace">
            {generationForm}
            <div className="studio-stage">
              {studioCanvas}
              {jobTimelinePanel}
            </div>
          </div>
          <ImageStudioTips />
        </>
      )}

      {activeView === 'prompts' && (
        <div>{templateLibrary}</div>
      )}

      {activeView === 'gallery' && galleryView}

      {activeView === 'history' && historyView}

      <AssetPreviewDialog
        asset={previewAsset}
        position={previewIndex}
        count={previewAssets.length}
        onNavigate={navigatePreview}
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
    </div>
  )
}

function ImageStudioTips() {
  const { t } = useTranslation()
  return (
    <details className="studio-tips">
      <summary><Info className="size-3.5" />{t('images.workspace.generationTips')}<ChevronDown className="size-3 transition-transform" /></summary>
      <div className="studio-tips-list">
        {IMAGE_NOTICE_KEYS.map(key => <p key={key}>{t(key)}</p>)}
      </div>
    </details>
  )
}

function ImageStudioTabs({ activeView }: { activeView: ImageView }) {
  const { t } = useTranslation()
  const tabs = [
    {
      view: 'studio' as const,
      label: t('images.views.studio'),
      to: '/images/studio',
      icon: Wand2,
    },
    {
      view: 'prompts' as const,
      label: t('images.views.prompts'),
      to: '/images/prompts',
      icon: LayoutTemplate,
    },
    {
      view: 'gallery' as const,
      label: t('images.views.gallery'),
      to: '/images/gallery',
      icon: Images,
    },
    {
      view: 'history' as const,
      label: t('images.views.history'),
      to: '/images/history',
      icon: History,
    },
  ]
  return (
    <div className="studio-navigation-wrap">
      <GlideMenu
        as="nav"
        className="studio-navigation"
        indicatorClassName="rounded-[9px]"
        aria-label={t('images.title')}
      >
        {tabs.map(tab => {
          const Icon = tab.icon
          return (
            <NavLink
              key={tab.view}
              to={tab.to}
              data-glide-item="true"
              aria-current={activeView === tab.view ? 'page' : undefined}
              className="studio-nav-link"
            >
              <Icon className="size-3.5 shrink-0" />
              <span>{tab.label}</span>
            </NavLink>
          )
        })}
      </GlideMenu>
    </div>
  )
}

function StudioCanvas({
  currentJob,
  latestAsset,
  imageURL,
  prompt,
  onInspire,
  onPreview,
  onDownload,
  onCopyPrompt,
  onRerun,
  onSaveTemplate,
  onDelete,
}: {
  currentJob: ImageGenerationJob | null
  latestAsset?: ImageAsset
  imageURL?: string
  prompt: string
  onInspire: (index: number) => void
  onPreview: () => void
  onDownload: () => void
  onCopyPrompt: () => void
  onRerun: () => void
  onSaveTemplate: () => void
  onDelete: () => void
}) {
  const { t } = useTranslation()
  const busy = currentJob ? isImageJobBusy(currentJob) : false
  const failed = currentJob?.status === 'failed'
  const hasResult = Boolean(latestAsset && imageURL)
  const stageLabel = currentJob?.status === 'queued' ? t('images.canvasQueued') : t('images.canvasGenerating')
  const elapsedSeconds = useElapsedSeconds(currentJob?.started_at || currentJob?.created_at, busy)

  return (
    <section className="studio-canvas" aria-label={t('images.workspace.canvasTitle')}>
      <div className="studio-canvas-header">
        <h2><ImageIcon className="size-3.5 text-muted-foreground" />{t('images.workspace.canvasTitle')}</h2>
        <div className="flex items-center gap-2">
          {currentJob ? (
            <>
              <span className="font-mono text-[10px] text-muted-foreground">#{currentJob.id}</span>
              <Badge className={cn(jobStatusClass(currentJob.status), 'text-[10px]')}>
                {t(`images.status.${currentJob.status}`, { defaultValue: currentJob.status })}
              </Badge>
            </>
          ) : <span className="text-[10px] text-muted-foreground">{t('images.workspace.canvasReady')}</span>}
        </div>
      </div>
      <div className="studio-canvas-body">
        {!currentJob && (
          <div className="studio-empty animate-image-studio-fade-in">
            <div className="studio-art-stack" aria-hidden="true">
              <div className="studio-art-print"><StudioArtwork scene="landscape" /><span className="studio-art-label">01 / EXPLORE</span></div>
              <div className="studio-art-print"><StudioArtwork scene="architecture" /><span className="studio-art-label">03 / IMAGINE</span></div>
              <div className="studio-art-print"><StudioArtwork scene="product" /><span className="studio-art-label">02 / CREATE</span></div>
            </div>
            <h3>{t('images.workspace.emptyTitle')}</h3>
            <p className="studio-empty-description">{t('images.workspace.emptyDescription')}</p>
            <div className="studio-inspiration-label">{t('images.workspace.startWithIdea')}</div>
            <div className="studio-inspirations">
              {STUDIO_STARTERS.map((starter, index) => (
                <button key={starter.id} type="button" className="studio-inspiration" onClick={() => onInspire(index)}>
                  <StudioArtwork scene={starter.id} className="studio-inspiration-art" />
                  <span className="studio-inspiration-copy">
                    <strong>{t(`images.workspace.starters.${starter.id}.title`)}</strong>
                    <small>{t(`images.workspace.starters.${starter.id}.subtitle`)}</small>
                  </span>
                  <ArrowUpRight className="size-3 shrink-0 text-muted-foreground" />
                </button>
              ))}
            </div>
          </div>
        )}

        {currentJob && busy && (
          <div className="flex w-full max-w-md flex-col items-center gap-4 py-5 animate-image-studio-fade-in" role="status">
            <div className="studio-busy-stage">
              <div className="absolute inset-y-0 w-1/2 animate-image-studio-shimmer bg-gradient-to-r from-transparent via-primary/10 to-transparent" />
              <div className="studio-busy-loader-shell relative">
                <PixelLoader mode="orbit" size="lg" />
              </div>
              <div className="relative flex flex-col items-center gap-2 text-center">
                <ShimmerText className="text-base font-semibold">{stageLabel}</ShimmerText>
                <p className="text-xs tabular-nums text-muted-foreground" aria-live="off">
                  {elapsedSeconds !== null ? t('images.elapsedSeconds', { seconds: elapsedSeconds }) : t('images.canvasGeneratingHint')}
                </p>
                <div className="mt-1 h-1.5 w-36 overflow-hidden rounded-full bg-muted">
                  <div className="h-full w-1/2 animate-image-studio-progress rounded-full bg-primary/80" />
                </div>
              </div>
            </div>
            <p className="line-clamp-2 max-w-sm text-center text-xs leading-relaxed text-muted-foreground">{prompt}</p>
          </div>
        )}

        {currentJob && failed && !hasResult && (
          <div className="flex max-w-sm flex-col items-center gap-4 py-8 text-center" role="alert">
            <div className="flex size-12 items-center justify-center rounded-2xl border border-destructive/20 bg-destructive/8 text-destructive"><ImageIcon className="size-5" /></div>
            <h3 className="text-base font-semibold">{t('images.canvasFailed')}</h3>
            {currentJob.error_message && <p className="max-h-40 overflow-auto break-words text-xs leading-relaxed text-muted-foreground">{currentJob.error_message}</p>}
            <Button size="sm" variant="outline" onClick={onRerun}><RefreshCcw className="size-3.5" />{t('images.rerun')}</Button>
          </div>
        )}

        {currentJob && !busy && !failed && !hasResult && (
          <div className="flex flex-col items-center gap-3 py-8 text-xs text-muted-foreground" role="status">
            {latestAsset ? <><Loader2 className="size-5 animate-spin" />{t('images.workspace.loadingImage')}</> : <><ImageIcon className="size-6" />{t('images.noAssets')}</>}
          </div>
        )}

        {hasResult && latestAsset && !busy && (
          <div key={latestAsset.id} className="studio-result animate-image-studio-result-in">
            <button type="button" className="studio-result-image image-studio-checkerboard cursor-zoom-in" onClick={onPreview} aria-label={t('images.openPreview')}>
              <img src={imageURL} alt={prompt || latestAsset.filename} />
            </button>
            <div className="studio-result-actions">
              <Button size="sm" variant="ghost" onClick={onPreview}><Eye className="size-3.5" />{t('images.openPreview')}</Button>
              <Button size="sm" onClick={onDownload}><Download className="size-3.5" />{t('images.download')}</Button>
              <span className="mx-1 h-4 w-px bg-border" aria-hidden="true" />
              <Button size="icon-sm" variant="ghost" onClick={onCopyPrompt} aria-label={t('images.copyPrompt')} title={t('images.copyPrompt')}><Copy className="size-3.5" /></Button>
              <Button size="icon-sm" variant="ghost" onClick={onRerun} aria-label={t('images.rerun')} title={t('images.rerun')}><RefreshCcw className="size-3.5" /></Button>
              <Button size="icon-sm" variant="ghost" onClick={onSaveTemplate} aria-label={t('images.saveAsTemplate')} title={t('images.saveAsTemplate')}><Save className="size-3.5" /></Button>
              <Button size="icon-sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={onDelete} aria-label={t('common.delete')} title={t('common.delete')}><Trash2 className="size-3.5" /></Button>
            </div>
            {currentJob?.warning && <p className="max-w-lg rounded-lg border border-amber-500/25 bg-amber-500/10 px-3 py-2 text-center text-xs leading-relaxed text-amber-800 dark:text-amber-200">{currentJob.warning}</p>}
            {prompt && <button type="button" onClick={onCopyPrompt} title={t('images.copyPrompt')} className="line-clamp-2 max-w-lg text-center text-xs leading-relaxed text-muted-foreground hover:text-foreground">{prompt}</button>}
          </div>
        )}
      </div>
      <div className="studio-canvas-footer">
        {latestAsset ? (
          <>
            <span className="flex flex-wrap items-center gap-x-3 gap-y-1"><span>{assetResolution(latestAsset)}</span><span>{imageAssetFormat(latestAsset)}</span><span>{formatBytes(latestAsset.bytes)}</span></span>
            <span className="flex items-center gap-1.5"><Timer className="size-3" />{currentJob && currentJob.duration_ms > 0 ? formatDuration(currentJob.duration_ms) : latestAsset.model}</span>
          </>
        ) : (
          <><span className="inline-flex items-center gap-1.5"><Sparkles className="size-3" />{t('images.workspace.canvasFooter')}</span><span className="font-mono tracking-wider">IMAGE STUDIO</span></>
        )}
      </div>
    </section>
  )
}

function ReferenceImageDropzone({
  images,
  onFiles,
  onFileInput,
  onRemove,
  compact = false,
}: {
  images: string[]
  onFiles: (files: FileList | File[]) => void
  onFileInput: (e: React.ChangeEvent<HTMLInputElement>) => void
  onRemove: (index: number) => void
  compact?: boolean
}) {
  const { t } = useTranslation()
  const [dragging, setDragging] = useState(false)
  const dragDepth = useRef(0)
  const thumbClass = compact
    ? 'h-16 w-16 rounded-lg sm:h-[4.5rem] sm:w-[4.5rem]'
    : 'h-24 w-24 rounded-xl sm:h-28 sm:w-28'

  const onDragEnter = (e: React.DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragDepth.current += 1
    setDragging(true)
  }
  const onDragLeave = (e: React.DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragDepth.current = Math.max(0, dragDepth.current - 1)
    if (dragDepth.current === 0) setDragging(false)
  }
  const onDragOver = (e: React.DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
  }
  const onDrop = (e: React.DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragDepth.current = 0
    setDragging(false)
    if (e.dataTransfer.files?.length) onFiles(e.dataTransfer.files)
  }

  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between">
        <span className="text-xs font-semibold text-muted-foreground">{t('images.inputImage')}</span>
        {images.length > 0 ? (
          <label className="inline-flex cursor-pointer items-center rounded-md px-2 py-1 text-xs font-semibold text-primary transition-colors hover:bg-primary/10">
            <Upload className="mr-1 size-3" />
            {t('images.upload')}
            <input type="file" accept="image/*" multiple className="hidden" onChange={onFileInput} />
          </label>
        ) : null}
      </div>
      {images.length > 0 ? (
        <div
          className={cn(
            'rounded-xl border border-border/80 bg-muted/15 p-2 transition-colors',
            dragging && 'border-primary/50 bg-primary/8',
          )}
          onDragEnter={onDragEnter}
          onDragLeave={onDragLeave}
          onDragOver={onDragOver}
          onDrop={onDrop}
        >
          <div className="flex flex-wrap gap-1.5">
            {images.map((dataURL, index) => (
              <div key={`${index}-${dataURL.slice(0, 40)}`} className="group relative">
                <img
                  src={dataURL}
                  alt={`Input ${index + 1}`}
                  className={cn(thumbClass, 'border border-border object-cover shadow-sm')}
                />
                <button
                  type="button"
                  className="absolute -right-1.5 -top-1.5 flex size-5 items-center justify-center rounded-full bg-destructive text-destructive-foreground opacity-0 transition-opacity group-hover:opacity-100 max-sm:opacity-100"
                  onClick={() => onRemove(index)}
                  title={t('images.removeImage')}
                >
                  <X className="size-3" />
                </button>
              </div>
            ))}
            {images.length < MAX_INPUT_IMAGES && (
              <label
                className={cn(
                  thumbClass,
                  'flex cursor-pointer flex-col items-center justify-center gap-0.5 border border-dashed border-border/80 text-muted-foreground transition-colors hover:border-primary/40 hover:bg-primary/5 hover:text-primary',
                )}
              >
                <Plus className="size-3.5" />
                <span className="text-[10px] font-semibold">{t('images.upload')}</span>
                <input type="file" accept="image/*" multiple className="hidden" onChange={onFileInput} />
              </label>
            )}
          </div>
        </div>
      ) : (
        <label
          className={cn(
            'flex h-full min-h-[88px] cursor-pointer flex-col items-center justify-center rounded-xl border border-dashed px-3 text-center transition-all sm:min-h-[100px]',
            compact ? 'py-4' : 'py-9',
            dragging
              ? 'border-primary bg-primary/10 text-primary shadow-sm'
              : 'border-border bg-muted/25 text-muted-foreground hover:border-primary/35 hover:bg-muted/40',
          )}
          onDragEnter={onDragEnter}
          onDragLeave={onDragLeave}
          onDragOver={onDragOver}
          onDrop={onDrop}
        >
          <Upload className={cn('opacity-70', compact ? 'mb-1 size-4' : 'mb-2 size-4')} />
          <span className="text-xs font-semibold text-foreground">{t('images.dropImageTitle')}</span>
          {!compact && (
            <span className="mt-1 max-w-[220px] text-[11px] leading-relaxed">{t('images.inputImageHint')}</span>
          )}
          <input type="file" accept="image/*" multiple className="hidden" onChange={onFileInput} />
        </label>
      )}
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

function FilterChip({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: ReactNode
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        'inline-flex h-7 items-center gap-1 rounded-full border px-2.5 text-[11px] font-semibold transition-colors',
        active
          ? 'border-primary/30 bg-primary/10 text-primary'
          : 'border-border/70 bg-background/60 text-muted-foreground hover:border-primary/25 hover:bg-muted/60 hover:text-foreground',
      )}
    >
      {children}
    </button>
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
  const backgroundOptions = useMemo(
    () => [
      { label: t('images.backgroundOptions.auto'), value: 'auto' },
      { label: t('images.backgroundOptions.opaque'), value: 'opaque' },
      {
        label: t('images.backgroundOptions.transparent'),
        value: 'transparent',
      },
    ],
    [t],
  )

  const changeModel = (value: string) => {
    onChange({
      model: value,
      size: normalizeImageSizeForModel(value, draft.size),
      quality: normalizeImageQualityForModel(draft.quality, value),
    })
  }

  return (
    <Dialog open={open} onOpenChange={nextOpen => { if (!nextOpen) onClose() }}>
      <DialogContent className="!flex max-h-[calc(100dvh-1rem)] !w-[min(980px,calc(100vw-1rem))] !max-w-none flex-col gap-0 overflow-hidden p-0 sm:p-0">
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
              <Select value={draft.quality} onValueChange={value => onChange({ quality: value })} options={imageQualityOptions(draft.model)} compact />
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
  studio = false,
}: {
  value: string
  onChange: (value: string) => void
  onApply?: () => void
  compact?: boolean
  studio?: boolean
}) {
  const { t } = useTranslation()

  const applyPreset = (presetValue: string) => {
    onChange(presetValue)
    onApply?.()
  }

  return (
    <div className={cn('space-y-2', compact && 'space-y-1.5')}>
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-1.5 text-xs font-semibold text-muted-foreground">
          <Sparkles className="size-3.5" />
          {t('images.stylePresets')}
        </div>
        {value.trim() ? (
          <button type="button" className="text-[11px] font-semibold text-muted-foreground transition hover:text-foreground" onClick={() => onChange('')}>
            {t('images.clearStyle')}
          </button>
        ) : null}
      </div>
      <div
        className={cn(
          studio ? 'studio-style-grid' : 'flex gap-2 overflow-x-auto pb-1 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
          !studio && (compact ? 'snap-x snap-mandatory' : 'sm:flex-wrap sm:overflow-visible sm:pb-0'),
        )}
      >
        {STYLE_PRESETS.map(preset => {
          const active = value.trim() === preset.value
          const Icon = preset.icon
          return (
            <button
              key={preset.id}
              type="button"
              onClick={() => applyPreset(preset.value)}
              aria-pressed={active}
              className={cn(
                'group relative flex w-[84px] shrink-0 snap-start flex-col items-center gap-1.5 rounded-xl border p-1.5 text-center transition-all duration-200',
                active
                  ? 'border-primary/45 bg-primary/8 shadow-sm ring-2 ring-primary/20'
                  : 'border-border/70 bg-background/60 hover:-translate-y-0.5 hover:border-primary/30 hover:bg-muted/30 hover:shadow-sm',
              )}
            >
              <span
                className={cn(
                  'relative flex h-12 w-full items-center justify-center overflow-hidden rounded-lg text-white shadow-inner',
                  preset.swatch,
                )}
              >
                <span className="pointer-events-none absolute inset-0 bg-gradient-to-t from-black/15 via-transparent to-white/20" />
                <Icon className="relative size-5 drop-shadow-sm transition-transform duration-200 group-hover:scale-110" />
                {active ? (
                  <span className="absolute right-0.5 top-0.5 flex size-3.5 items-center justify-center rounded-full bg-primary text-primary-foreground shadow-sm">
                    <Check className="size-2.5" strokeWidth={3} />
                  </span>
                ) : null}
              </span>
              <span className={cn('line-clamp-2 w-full text-[10px] font-semibold leading-tight', active ? 'text-primary' : 'text-foreground')}>
                {t(`images.stylePreset.${preset.id}`)}
              </span>
            </button>
          )
        })}
      </div>
    </div>
  )
}

function TemplateCard({ template, active, onApply, onFavorite, onEdit, onDelete }: {
  template: ImagePromptTemplate; active: boolean; onApply: () => void; onFavorite: () => void; onEdit: () => void; onDelete: () => void
}) {
  const { t } = useTranslation()
  const preset = STYLE_PRESETS.find(item => item.value === template.style?.trim())
  const Icon = preset?.icon || LayoutTemplate
  return (
    <article className="studio-template-card" data-active={active}>
      <div className="studio-template-card-top">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="studio-template-symbol"><Icon className="size-4" /></span>
          <span className="truncate text-[10px] text-muted-foreground">{preset ? t(`images.stylePreset.${preset.id}`) : t('images.collections.customTemplate')}</span>
        </div>
        <button type="button" className="studio-template-favorite" onClick={onFavorite} aria-label={t(template.favorite ? 'images.collections.removeFavorite' : 'images.favorite')} aria-pressed={template.favorite} title={t(template.favorite ? 'images.collections.removeFavorite' : 'images.favorite')}><Star className="size-4" fill={template.favorite ? 'currentColor' : 'none'} /></button>
      </div>
      <button type="button" className="studio-template-body" onClick={onEdit} title={t('images.editTemplate')}>
        <h3 className="line-clamp-2">{template.name || t('images.untitledTemplate')}</h3>
        <p className="line-clamp-4">{template.prompt}</p>
      </button>
      <div className="studio-template-tags">
        {template.tags.slice(0, 4).map(tag => <span key={tag} title={tag}># {tag}</span>)}
        {template.tags.length > 4 && <span title={template.tags.slice(4).join(', ')}>+{template.tags.length - 4}</span>}
        {template.tags.length === 0 && <span>{t('images.collections.untagged')}</span>}
      </div>
      <div className="studio-template-meta">
        <span className="font-mono">{template.model || 'gpt-image-2'}</span>
        {template.size && <span>{template.size}</span>}
        {template.output_format && <span>{template.output_format.toUpperCase()}</span>}
      </div>
      <div className="studio-template-meta">
        <span>{t('images.collections.templateUses', { count: template.usage_count })}</span>
        <span title={formatBeijingTime(template.updated_at)}>{formatRelativeTime(template.updated_at, { variant: 'compact' })}</span>
      </div>
      <div className="studio-template-footer">
        <Button size="sm" variant="secondary" onClick={onApply}><Wand2 className="size-3.5" />{t('images.collections.useTemplate')}<ArrowUpRight className="size-3.5" /></Button>
        <Button size="icon-sm" variant="ghost" onClick={onEdit} aria-label={t('images.editTemplate')} title={t('images.editTemplate')}><Pencil className="size-3.5" /></Button>
        <Button size="icon-sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={onDelete} aria-label={t('images.deleteTemplateTitle')} title={t('images.deleteTemplateTitle')}><Trash2 className="size-3.5" /></Button>
      </div>
    </article>
  )
}

function HistoryJobCard({ job, imageURLs, onSelect, onPreview, onDownload, onCopyPrompt, onRerun, onSaveTemplate, onDeleteJob, onDelete }: {
  job: ImageGenerationJob; imageURLs: Record<number, string>; onSelect: () => void; onPreview: (asset: ImageAsset) => void; onDownload: (asset: ImageAsset) => void; onCopyPrompt: () => void; onRerun: () => void; onSaveTemplate: (asset: ImageAsset) => void; onDeleteJob: () => void; onDelete: (asset: ImageAsset) => void
}) {
  const { t } = useTranslation()
  const assets = job.assets ?? []
  const primaryAsset = assets[0]
  const thumbnail = primaryAsset ? assetThumbnailURL(primaryAsset, imageURLs) : undefined
  const busy = isImageJobBusy(job)
  const params = jobParams(job)
  return (
    <article className="studio-history-card" data-status={job.status}>
      <div className="studio-history-main">
        <button type="button" className="studio-history-thumb" onClick={() => primaryAsset ? onPreview(primaryAsset) : onSelect()} aria-label={t(primaryAsset ? 'images.openPreview' : 'images.selectJob')}>
          {primaryAsset ? <StudioAssetThumbnail src={thumbnail} alt={job.prompt || primaryAsset.filename} /> : busy ? <Loader2 className="size-5 animate-spin text-primary" /> : job.status === 'failed' ? <CircleAlert className="size-5 text-destructive/70" /> : <ImageIcon className="size-5" />}
        </button>
        <div className="min-w-0">
          <div className="studio-history-meta-line">
            <span>#{job.id}</span>
            <Badge className={cn(jobStatusClass(job.status), 'text-[10px]')}>{t(`images.status.${job.status}`, { defaultValue: job.status })}</Badge>
            <time dateTime={job.created_at} title={formatBeijingTime(job.created_at)}>{formatBeijingTime(job.created_at).split(' ')[1] || '—'}</time>
          </div>
          <button type="button" className="studio-history-prompt line-clamp-2" onClick={onSelect}>{job.prompt || '—'}</button>
          <div className="studio-history-facts">
            <span className="font-mono">{jobModel(job)}</span>
            <span><Square className="size-3" />{jobRequestedSize(job)}</span>
            <span><Timer className="size-3" />{busy ? t('images.waiting') : formatDuration(job.duration_ms)}</span>
            <span><Images className="size-3" />{t('images.imageCount', { count: assets.length })}</span>
          </div>
          {job.error_message && <div className="studio-history-error"><span className="line-clamp-2">{job.error_message}</span></div>}
          {job.warning && <div className="mt-2 text-[11px] leading-relaxed text-amber-700 dark:text-amber-300"><span className="line-clamp-2">{job.warning}</span></div>}
          {job.status === 'succeeded' && assets.length === 0 && <p className="mt-2 text-[11px] text-muted-foreground">{t('images.assetDeletedInHistory')}</p>}
        </div>
        <div className="studio-history-actions">
          <Button size="sm" variant="outline" onClick={onSelect}>{t('images.collections.viewInStudio')}<ArrowUpRight className="size-3.5" /></Button>
          <Button size="icon-sm" variant="ghost" onClick={onCopyPrompt} aria-label={t('images.copyPrompt')} title={t('images.copyPrompt')}><Copy className="size-3.5" /></Button>
          <Button size="icon-sm" variant="ghost" onClick={onRerun} aria-label={t('images.rerun')} title={t('images.rerun')}><RefreshCcw className="size-3.5" /></Button>
          {!busy && <Button size="icon-sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={onDeleteJob} aria-label={t('images.deleteJob')} title={t('images.deleteJob')}><Trash2 className="size-3.5" /></Button>}
        </div>
      </div>
      <details className="studio-history-details">
        <summary><ChevronDown className="size-3 transition-transform" />{t('images.collections.jobDetails')}</summary>
        <div className="studio-history-details-body">
          <dl className="studio-history-details-meta">
            <HistoryMeta label={t('images.createdAt')} value={formatBeijingTime(job.created_at)} />
            <HistoryMeta label={t('images.apiKey')} value={job.api_key_name || job.api_key_masked || '—'} />
            <HistoryMeta label={t('images.model')} value={jobModel(job)} />
            <HistoryMeta label={t('images.size')} value={jobRequestedSize(job)} />
            <HistoryMeta label={t('images.quality')} value={params.quality || 'auto'} />
            <HistoryMeta label={t('images.format')} value={(params.output_format || primaryAsset?.output_format || '—').toUpperCase()} />
          </dl>
          <p className="mb-2 text-[10px] text-muted-foreground">{t('images.prompt')}</p>
          <div className="studio-history-full-prompt">{job.prompt || '—'}</div>
          {job.error_message && <div className="studio-history-error whitespace-pre-wrap">{job.error_message}</div>}
          {job.warning && <div className="mt-3 rounded-lg border border-amber-500/20 bg-amber-500/5 p-3 text-xs leading-relaxed whitespace-pre-wrap text-amber-700 dark:text-amber-300">{job.warning}</div>}
          {assets.length > 0 && <div className="studio-history-previews">{assets.map(asset => <button key={asset.id} type="button" onClick={() => onPreview(asset)} aria-label={t('images.openPreview')}><StudioAssetThumbnail src={assetThumbnailURL(asset, imageURLs)} alt={asset.filename} /></button>)}</div>}
          {primaryAsset && <div className="mt-3 flex flex-wrap gap-2">
            <Button size="sm" variant="outline" onClick={() => onDownload(primaryAsset)}><Download className="size-3.5" />{t('images.download')}</Button>
            <Button size="sm" variant="outline" onClick={() => onSaveTemplate(primaryAsset)}><Save className="size-3.5" />{t('images.saveAsTemplate')}</Button>
            <Button size="sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={() => onDelete(primaryAsset)}><Trash2 className="size-3.5" />{t('images.collections.deleteImage')}</Button>
          </div>}
        </div>
      </details>
    </article>
  )
}

function HistoryMeta({ label, value }: { label: string; value: string }) {
  return <div><dt>{label}</dt><dd>{value}</dd></div>
}

function StudioAssetThumbnail({ src, alt }: { src?: string; alt: string }) {
  const { t } = useTranslation()
  const [failedURL, setFailedURL] = useState<string | undefined>()
  if (!src || src === failedURL) return <span className="flex size-full flex-col items-center justify-center gap-2 text-muted-foreground"><ImageIcon className="size-5" /><span className="text-[10px]">{t(src ? 'images.collections.imageUnavailable' : 'images.workspace.loadingImage')}</span></span>
  return <img src={src} alt={alt} loading="lazy" decoding="async" onError={() => setFailedURL(src)} />
}

function AssetCard({ asset, imageURL, prompt, onPreview, onDownload, onDelete, onCopyPrompt, onRerun, onSaveTemplate }: {
  asset: ImageAsset; imageURL?: string; prompt: string; onPreview: () => void; onDownload: () => void; onDelete: () => void; onCopyPrompt: () => void; onRerun: () => void; onSaveTemplate: () => void
}) {
  const { t } = useTranslation()
  const orientation = imageAssetOrientation(asset)
  const actualSize = /^(\d+)\s*[x×]\s*(\d+)$/i.exec(asset.actual_size?.trim() || '')
  const width = asset.width || Number(actualSize?.[1])
  const height = asset.height || Number(actualSize?.[2])
  const ratio = width > 0 && height > 0 ? width / height : 1
  return (
    <article className="studio-gallery-card">
      <button type="button" className="studio-gallery-image" style={{ '--studio-image-ratio': ratio } as React.CSSProperties} onClick={onPreview} aria-label={t('images.openPreview')}>
        <StudioAssetThumbnail src={imageURL} alt={prompt || asset.filename} />
        <span className="studio-gallery-zoom"><Eye className="size-4" /></span>
      </button>
      <div className="studio-gallery-copy">
        <button type="button" onClick={onPreview}><h3 className="line-clamp-2">{prompt || asset.revised_prompt || asset.filename}</h3></button>
        <div className="studio-gallery-facts"><span className="studio-gallery-format">{imageAssetFormat(asset)}</span><span>{assetResolution(asset)}</span><span>{formatBytes(asset.bytes)}</span>{orientation !== 'unknown' && <span>{t(`images.collections.orientation.${orientation}`)}</span>}</div>
        <div className="studio-gallery-model"><span className="font-mono">{asset.model}</span><time dateTime={asset.created_at} title={formatBeijingTime(asset.created_at)}>{formatRelativeTime(asset.created_at, { variant: 'compact' })}</time></div>
      </div>
      <div className="studio-gallery-actions">
        <Button size="sm" variant="ghost" onClick={onDownload}><Download className="size-3.5" />{t('images.download')}</Button>
        <Button size="icon-sm" variant="ghost" onClick={onCopyPrompt} aria-label={t('images.copyPrompt')} title={t('images.copyPrompt')}><Copy className="size-3.5" /></Button>
        <Button size="icon-sm" variant="ghost" onClick={onRerun} aria-label={t('images.rerun')} title={t('images.rerun')}><RefreshCcw className="size-3.5" /></Button>
        <Button size="icon-sm" variant="ghost" onClick={onSaveTemplate} aria-label={t('images.saveAsTemplate')} title={t('images.saveAsTemplate')}><Save className="size-3.5" /></Button>
        <Button size="icon-sm" variant="ghost" className="text-muted-foreground hover:text-destructive" onClick={onDelete} aria-label={t('images.collections.deleteImage')} title={t('images.collections.deleteImage')}><Trash2 className="size-3.5" /></Button>
      </div>
    </article>
  )
}

function AssetPreviewDialog({ asset, imageURL, prompt, open, position, count, onNavigate, onClose, onDownload, onCopyPrompt, onRerun, onSaveTemplate, onDelete }: {
  asset: ImageAsset | null; imageURL?: string; prompt: string; open: boolean; position: number; count: number; onNavigate: (direction: -1 | 1) => void; onClose: () => void; onDownload: (asset: ImageAsset) => void; onCopyPrompt: (asset: ImageAsset) => void; onRerun: (asset: ImageAsset) => void; onSaveTemplate: (asset: ImageAsset) => void; onDelete: (asset: ImageAsset) => void
}) {
  const { t } = useTranslation()
  useEffect(() => {
    if (!open || position < 0 || count < 2) return
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return
      const target = event.target as HTMLElement | null
      if (target?.closest('input, textarea, select, [contenteditable="true"]')) return
      if (event.key === 'ArrowLeft' && position > 0) { event.preventDefault(); onNavigate(-1) }
      if (event.key === 'ArrowRight' && position < count - 1) { event.preventDefault(); onNavigate(1) }
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [open, position, count, onNavigate])
  if (!asset) return null
  return (
    <Dialog open={open} onOpenChange={nextOpen => { if (!nextOpen) onClose() }}>
      <DialogContent className="studio-asset-dialog !flex !h-[calc(100dvh-1.5rem)] !w-[min(1480px,calc(100vw-1.5rem))] !max-w-none flex-col gap-0 overflow-hidden p-0 sm:p-0" showCloseButton={false}>
        <DialogHeader className="studio-preview-header">
          <DialogTitle className="text-sm">{t('images.previewTitle')}</DialogTitle>
          <DialogDescription className="truncate text-[11px]">{asset.filename}</DialogDescription>
        </DialogHeader>
        <Button type="button" size="icon-sm" variant="ghost" onClick={onClose} className="absolute right-3 top-3 z-10" aria-label={t('common.close')}><X className="size-4" /></Button>
        <div className="studio-preview-layout">
          <div className="studio-preview-image">
            {imageURL ? <img key={imageURL} src={imageURL} alt={prompt || asset.filename} /> : <PixelLoader mode="orbit" size="lg" className="text-white" />}
          </div>
          <aside className="studio-preview-info">
            <h3>{t('images.collections.creationDetails')}</h3>
            <div className="grid grid-cols-2 gap-2">
              <PreviewMeta label={t('images.resolution')} value={assetResolution(asset)} />
              <PreviewMeta label={t('images.format')} value={imageAssetFormat(asset)} />
              <PreviewMeta label={t('images.fileSize')} value={formatBytes(asset.bytes)} />
              <PreviewMeta label={t('images.createdAt')} value={formatBeijingTime(asset.created_at)} />
            </div>
            <div className="mt-2"><PreviewMeta label={t('images.model')} value={asset.model} /></div>
            <div className="mt-6 flex items-center justify-between gap-2"><h3>{t('images.prompt')}</h3><Button size="icon-xs" variant="ghost" onClick={() => onCopyPrompt(asset)} disabled={!prompt.trim() && !asset.revised_prompt} aria-label={t('images.copyPrompt')} title={t('images.copyPrompt')}><Copy className="size-3" /></Button></div>
            <p className="studio-preview-prompt">{prompt || asset.revised_prompt || t('images.collections.noPrompt')}</p>
          </aside>
        </div>
        <div className="studio-preview-footer">
          <div className="studio-preview-navigation">
            {position >= 0 && count > 1 ? <>
              <Button size="icon-sm" variant="outline" onClick={() => onNavigate(-1)} disabled={position <= 0} aria-label={t('images.collections.previousImage')} title={t('images.collections.previousImage')}><ChevronLeft className="size-4" /></Button>
              <span aria-live="polite">{position + 1}<span> / {count}</span></span>
              <Button size="icon-sm" variant="outline" onClick={() => onNavigate(1)} disabled={position >= count - 1} aria-label={t('images.collections.nextImage')} title={t('images.collections.nextImage')}><ChevronRight className="size-4" /></Button>
            </> : <span className="text-[11px] text-muted-foreground">{assetResolution(asset)}</span>}
          </div>
          <TooltipProvider><div className="flex flex-wrap items-center gap-2">
            <Button size="sm" onClick={() => onDownload(asset)}><Download className="size-3.5" />{t('images.download')}</Button>
            <PreviewAction label={t('images.rerun')} onClick={() => onRerun(asset)}><RefreshCcw className="size-4" /></PreviewAction>
            <PreviewAction label={t('images.saveAsTemplate')} onClick={() => onSaveTemplate(asset)}><Save className="size-4" /></PreviewAction>
            <PreviewAction label={t('common.delete')} variant="destructive" onClick={() => onDelete(asset)}><Trash2 className="size-4" /></PreviewAction>
          </div></TooltipProvider>
        </div>
      </DialogContent>
    </Dialog>
  )
}

function PreviewMeta({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 rounded-md bg-muted/55 px-2.5 py-1.5">
      <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground/75">{label}</div>
      <div className="mt-1 break-words font-geist-mono text-[12px] text-foreground">{value}</div>
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
