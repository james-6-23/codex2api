import { formatImageStudioQuota } from '../lib/imageStudioQuota'
import { IMAGE_MODELS, imageQualityOptions, normalizeImageQualityForModel } from '../lib/imageStudioModels'
import { type FormEvent, type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { NavLink, useNavigate, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import {
  ArrowRight,
  ArrowUpRight,
  Check,
  ChevronDown,
  Clock3,
  Copy,
  Download,
  Expand,
  Eye,
  EyeOff,
  FolderOpen,
  History,
  Image as ImageIcon,
  Images,
  KeyRound,
  Languages,
  Lightbulb,
  Loader2,
  LogIn,
  LogOut,
  Pencil,
  RectangleHorizontal,
  RectangleVertical,
  RefreshCw,
  ShieldCheck,
  Sparkles,
  Square,
  Trash2,
  Upload,
  X,
} from 'lucide-react'
import { api } from '../api'
import { useBranding } from '../branding'
import Pagination from '../components/Pagination'
import type { CreateImageJobPayload, ImageAsset, ImageGenerationJob, ImageStudioQuota } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime } from '../utils/time'
import { CinematicThemeSwitcher } from '@/components/ui/cinematic-theme-switcher'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { cn } from '@/lib/utils'
import { SegmentedPillGroup } from '@/components/ui/segmented-pill-group'
import { GlideMenu, PixelLoader, ShimmerText, ValuePill } from '@/components/ui/beautiful'
import { StudioArtwork, type StudioScene } from '@/components/image-studio/StudioArtwork'
import { PortalQuota } from '@/components/image-studio/PortalQuota'
import './image-studio-portal.css'

const STORAGE_KEY = 'codex2api_image_studio_api_key'
const PORTAL_VIEWS = ['studio', 'history', 'gallery'] as const
type PortalView = (typeof PORTAL_VIEWS)[number]

const SIZE_OPTIONS = [
  { label: 'Auto', value: 'auto' },
  { label: '1024x1024', value: '1024x1024' },
  { label: '1536x864', value: '1536x864' },
  { label: '864x1536', value: '864x1536' },
  { label: '2048x2048', value: '2048x2048' },
  { label: '2560x1440', value: '2560x1440' },
  { label: '1440x2560', value: '1440x2560' },
]

const FORMAT_OPTIONS = [
  { label: 'PNG', value: 'png' },
  { label: 'JPEG', value: 'jpeg' },
  { label: 'WebP', value: 'webp' },
]
const MAX_INPUT_IMAGES = 4
const HISTORY_PAGE_SIZE = 20
const GALLERY_PAGE_SIZE = 16
const RECENT_JOB_COUNT = 3
const PORTAL_INSPIRATIONS: Array<{ scene: StudioScene; size: string }> = [
  { scene: 'landscape', size: '1536x864' },
  { scene: 'product', size: '1024x1024' },
  { scene: 'architecture', size: '864x1536' },
]

const PORTAL_QUICK_RATIOS = [
  { id: 'auto', size: 'auto', labelKey: 'images.aspect.auto', icon: Sparkles },
  { id: '1:1', size: '1024x1024', labelKey: 'images.aspect.square', icon: Square },
  { id: '16:9', size: '1536x864', labelKey: 'images.aspect.landscape', icon: RectangleHorizontal },
  { id: '9:16', size: '864x1536', labelKey: 'images.aspect.portrait', icon: RectangleVertical },
] as const

function portalRatioForSize(size: string): string {
  switch (size) {
    case '1024x1024':
    case '2048x2048':
      return '1:1'
    case '1536x864':
    case '2560x1440':
      return '16:9'
    case '864x1536':
    case '1440x2560':
      return '9:16'
    default:
      return 'auto'
  }
}

function PortalInspiration({ onSelect }: { onSelect: (scene: StudioScene, size: string) => void }) {
  const { t } = useTranslation()
  return (
    <div className="portal-inspiration animate-image-studio-fade-in">
      <span className="portal-eyebrow"><Lightbulb className="size-3.5" />{t('imageStudioPortal.design.inspirationEyebrow')}</span>
      <h2>{t('imageStudioPortal.design.inspirationTitle')}</h2>
      <p>{t('imageStudioPortal.design.inspirationDesc')}</p>
      <div className="portal-inspiration-grid">
        {PORTAL_INSPIRATIONS.map(({ scene, size }) => (
          <Button key={scene} type="button" variant="ghost" className="portal-inspiration-card" onClick={() => onSelect(scene, size)}>
            <div className="portal-inspiration-art">
              <StudioArtwork scene={scene} className="size-full" />
              <span className="portal-inspiration-size">{size.replace('x', '×')}</span>
              <span className="portal-inspiration-arrow"><ArrowUpRight className="size-3.5" /></span>
            </div>
            <span className="portal-inspiration-name">{t(`imageStudioPortal.design.inspiration.${scene}.title`)}</span>
            <span className="portal-inspiration-note">{t(`imageStudioPortal.design.inspiration.${scene}.description`)}</span>
          </Button>
        ))}
      </div>
      <span className="portal-inspiration-caption">{t('imageStudioPortal.design.inspirationCaption')}</span>
    </div>
  )
}

function normalizeView(value?: string): PortalView {
  return PORTAL_VIEWS.includes(value as PortalView) ? (value as PortalView) : 'studio'
}

function readStoredAPIKey(): { key: string; remember: boolean } {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return { key: '', remember: true }
    const parsed = JSON.parse(raw) as { key?: string; remember?: boolean }
    return {
      key: parsed.key?.trim() || '',
      remember: parsed.remember !== false,
    }
  } catch {
    return { key: '', remember: true }
  }
}

function writeStoredAPIKey(key: string, remember: boolean) {
  if (!remember) {
    localStorage.removeItem(STORAGE_KEY)
    return
  }
  localStorage.setItem(STORAGE_KEY, JSON.stringify({ key, remember: true }))
}

function clearStoredAPIKey() {
  localStorage.removeItem(STORAGE_KEY)
}

function maskKey(key: string): string {
  const value = key.trim()
  if (value.length <= 10) return '••••••••'
  return `${value.slice(0, 4)}••••${value.slice(-4)}`
}

function hasServerImageURL(asset: ImageAsset): boolean {
  return Boolean(asset.proxy_url || asset.thumbnail_url)
}

function imageSrc(asset: ImageAsset, localURLs: Record<number, string>): string {
  return asset.proxy_url || localURLs[asset.id] || asset.thumbnail_url || ''
}

function thumbnailSrc(asset: ImageAsset, localURLs: Record<number, string>): string {
  return asset.thumbnail_url || imageSrc(asset, localURLs)
}

function blobToDataURL(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result || ''))
    reader.onerror = () => reject(new Error('Failed to read image'))
    reader.readAsDataURL(blob)
  })
}

async function assetToDataURL(apiKey: string, asset: ImageAsset): Promise<string> {
  if (asset.cache_b64_json) {
    const mime = asset.mime_type?.trim() || 'image/png'
    return `data:${mime};base64,${asset.cache_b64_json}`
  }
  const blob = await api.getPortalImageAssetFile(apiKey, asset.id)
  return blobToDataURL(blob)
}

function jobStatusTone(status: string): 'success' | 'danger' | 'info' | 'neutral' {
  switch (status) {
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'danger'
    case 'running':
      return 'info'
    default:
      return 'neutral'
  }
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="flex min-w-0 flex-col gap-1.5">
      <span className="text-xs font-semibold text-muted-foreground">{label}</span>
      {children}
    </label>
  )
}

function PortalTabs({ activeView, className }: { activeView: PortalView; className?: string }) {
  const { t } = useTranslation()
  const tabs = [
    { view: 'studio' as const, label: t('imageStudioPortal.views.studio'), icon: Sparkles, to: '/image-studio/studio' },
    { view: 'history' as const, label: t('imageStudioPortal.views.history'), icon: History, to: '/image-studio/history' },
    { view: 'gallery' as const, label: t('imageStudioPortal.views.gallery'), icon: Images, to: '/image-studio/gallery' },
  ]
  return (
    <GlideMenu
      as="nav"
      className={cn('portal-navigation', className)}
      indicatorClassName="rounded-[9px]"
      aria-label={t('imageStudioPortal.title')}
    >
      {tabs.map((tab) => {
        const Icon = tab.icon
        const active = activeView === tab.view
        return (
          <NavLink
            key={tab.view}
            to={tab.to}
            data-glide-item="true"
            aria-current={active ? 'page' : undefined}
            className="portal-nav-link"
          >
            <Icon className="size-3.5 shrink-0" />
            <span>{tab.label}</span>
          </NavLink>
        )
      })}
    </GlideMenu>
  )
}

export default function ImageStudioPortal() {
  const { t, i18n } = useTranslation()
  const { siteName, siteLogo, defaultLogo } = useBranding()
  const { view } = useParams()
  const navigate = useNavigate()
  const activeView = normalizeView(view)
  const logoSrc = siteLogo || defaultLogo
  const fileInputRef = useRef<HTMLInputElement>(null)
  const promptRef = useRef<HTMLTextAreaElement>(null)
  const workspaceRef = useRef<HTMLDivElement>(null)
  const [mobilePanel, setMobilePanel] = useState<'compose' | 'canvas'>('compose')
  const [inspirationOpen, setInspirationOpen] = useState(false)

  const selectMobilePanel = (panel: 'compose' | 'canvas') => {
    setMobilePanel(panel)
    window.requestAnimationFrame(() => {
      if (window.matchMedia('(max-width: 1023px)').matches) workspaceRef.current?.scrollIntoView({ block: 'start' })
    })
  }

  const [apiKeyInput, setAPIKeyInput] = useState(() => readStoredAPIKey().key)
  const [remember, setRemember] = useState(() => {
    const stored = readStoredAPIKey()
    return stored.key ? stored.remember : true
  })
  const [activeAPIKey, setActiveAPIKey] = useState(() => readStoredAPIKey().key)
  const [quotaSnapshot, setQuotaSnapshot] = useState<{ key: string; value: ImageStudioQuota } | null>(null)
  const [quotaLoading, setQuotaLoading] = useState(false)
  const [quotaError, setQuotaError] = useState('')
  const quotaRequestRef = useRef(0)
  const quotaRefreshKeyRef = useRef<string | null>(null)
  const quota = quotaSnapshot?.key === activeAPIKey ? quotaSnapshot.value : null
  const quotaReady = quota !== null
  const quotaLocked = quota?.status === 'quota_exhausted' || quota?.status === 'expired'
  const quotaExpired = quota?.status === 'expired'
  const [showKey, setShowKey] = useState(false)
  const [bootstrapping, setBootstrapping] = useState(() => Boolean(readStoredAPIKey().key))
  const [loginError, setLoginError] = useState('')
  const [loginLoading, setLoginLoading] = useState(false)

  const [prompt, setPrompt] = useState('')
  const [model, setModel] = useState('gpt-image-2')
  const [size, setSize] = useState('auto')
  const [quality, setQuality] = useState('auto')
  useEffect(() => { setQuality(current => normalizeImageQualityForModel(current, model)) }, [model])
  const [outputFormat, setOutputFormat] = useState('png')
  const [style, setStyle] = useState('')
  const [imageToImageMode, setImageToImageMode] = useState(false)
  const [inputImageDataURLs, setInputImageDataURLs] = useState<string[]>([])
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [dragOver, setDragOver] = useState(false)
  const [galleryPickerOpen, setGalleryPickerOpen] = useState(false)
  const [galleryPickerSelected, setGalleryPickerSelected] = useState<Set<number>>(() => new Set())
  const [galleryPickerLoading, setGalleryPickerLoading] = useState(false)
  const [galleryPickerConfirming, setGalleryPickerConfirming] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [currentJob, setCurrentJob] = useState<ImageGenerationJob | null>(null)
  const [previewAsset, setPreviewAsset] = useState<ImageAsset | null>(null)
  const [previewPrompt, setPreviewPrompt] = useState('')
  const [toast, setToast] = useState('')
  const [galleryLoading, setGalleryLoading] = useState(false)

  const [historyJobs, setHistoryJobs] = useState<ImageGenerationJob[]>([])
  const [historyTotal, setHistoryTotal] = useState(0)
  const [historyPage, setHistoryPage] = useState(1)
  const [historyLoading, setHistoryLoading] = useState(false)

  const [assets, setAssets] = useState<ImageAsset[]>([])
  const [assetTotal, setAssetTotal] = useState(0)
  const [assetPage, setAssetPage] = useState(1)
  const [assetURLs, setAssetURLs] = useState<Record<number, string>>({})
  const assetURLsRef = useRef<Record<number, string>>({})
  const assetURLRequestsRef = useRef<Set<number>>(new Set())

  const showToast = useCallback((message: string) => {
    setToast(message)
    window.setTimeout(() => setToast(''), 3200)
  }, [])

  useEffect(() => {
    if (view && !PORTAL_VIEWS.includes(view as PortalView)) {
      navigate('/image-studio/studio', { replace: true })
    }
  }, [navigate, view])

  const verifyAndEnter = useCallback(async (key: string) => {
    return api.getPortalImageQuota(key)
  }, [])

  const refreshQuota = useCallback(async () => {
    if (!activeAPIKey || quotaRefreshKeyRef.current === activeAPIKey) return
    const requestID = ++quotaRequestRef.current
    quotaRefreshKeyRef.current = activeAPIKey
    setQuotaLoading(true)
    try {
      const value = await api.getPortalImageQuota(activeAPIKey)
      if (requestID !== quotaRequestRef.current) return
      setQuotaSnapshot({ key: activeAPIKey, value })
      setQuotaError('')
    } catch (error) {
      if (requestID === quotaRequestRef.current) setQuotaError(getErrorMessage(error))
    } finally {
      if (requestID === quotaRequestRef.current) {
        quotaRefreshKeyRef.current = null
        setQuotaLoading(false)
      }
    }
  }, [activeAPIKey])

  useEffect(() => {
    if (!activeAPIKey) {
      setBootstrapping(false)
      return
    }
    let cancelled = false
    const requestID = ++quotaRequestRef.current
    quotaRefreshKeyRef.current = null
    setQuotaError('')
    setQuotaLoading(true)
    setBootstrapping(true)
    void verifyAndEnter(activeAPIKey)
      .then(value => {
        if (cancelled || requestID !== quotaRequestRef.current) return
        setQuotaSnapshot({ key: activeAPIKey, value })
        setLoginError('')
      })
      .catch((err) => {
        if (cancelled) return
        clearStoredAPIKey()
        setQuotaSnapshot(null)
        setActiveAPIKey('')
        setLoginError(getErrorMessage(err))
      })
      .finally(() => {
        if (!cancelled) {
          setBootstrapping(false)
          setQuotaLoading(false)
        }
      })
    return () => {
      cancelled = true
      if (requestID === quotaRequestRef.current) quotaRequestRef.current += 1
    }
  }, [activeAPIKey, verifyAndEnter])

  useEffect(() => {
    if (!activeAPIKey || bootstrapping) return
    const refreshWhenVisible = () => {
      if (document.visibilityState === 'visible') void refreshQuota()
    }
    const timer = window.setInterval(refreshWhenVisible, 30000)
    window.addEventListener('focus', refreshWhenVisible)
    document.addEventListener('visibilitychange', refreshWhenVisible)
    return () => {
      window.clearInterval(timer)
      window.removeEventListener('focus', refreshWhenVisible)
      document.removeEventListener('visibilitychange', refreshWhenVisible)
    }
  }, [activeAPIKey, bootstrapping, refreshQuota])

  const settlementRefreshDelay = (quota?.refresh_after_seconds ?? 6) * 1000
  useEffect(() => {
    if (!activeAPIKey || !currentJob || !['succeeded', 'failed'].includes(currentJob.status)) return
    void refreshQuota()
    const timer = window.setTimeout(() => void refreshQuota(), settlementRefreshDelay)
    return () => window.clearTimeout(timer)
  }, [activeAPIKey, currentJob?.id, currentJob?.status, refreshQuota, settlementRefreshDelay])

  const loadHistory = useCallback(
    async (key = activeAPIKey, page = historyPage, pageSize = HISTORY_PAGE_SIZE) => {
      if (!key) return
      setHistoryLoading(true)
      try {
        const res = await api.getPortalImageJobs(key, {
          page,
          pageSize,
        })
        setHistoryJobs(res.jobs ?? [])
        setHistoryTotal(res.total ?? 0)
      } catch (err) {
        showToast(getErrorMessage(err))
      } finally {
        setHistoryLoading(false)
      }
    },
    [activeAPIKey, historyPage, showToast],
  )

  const loadAssets = useCallback(
    async (key = activeAPIKey, page = assetPage) => {
      if (!key) return
      setGalleryLoading(true)
      try {
        const res = await api.getPortalImageAssets(key, {
          page,
          pageSize: GALLERY_PAGE_SIZE,
        })
        setAssets(res.assets ?? [])
        setAssetTotal(res.total ?? 0)
      } catch (err) {
        showToast(getErrorMessage(err))
      } finally {
        setGalleryLoading(false)
      }
    },
    [activeAPIKey, assetPage, showToast],
  )

  useEffect(() => {
    if (!activeAPIKey || bootstrapping || !quotaReady || quotaExpired) return
    if (activeView === 'studio') void loadHistory(activeAPIKey, 1, RECENT_JOB_COUNT)
    if (activeView === 'history') void loadHistory()
    if (activeView === 'gallery') void loadAssets()
  }, [activeAPIKey, activeView, bootstrapping, loadAssets, loadHistory, quotaExpired, quotaReady])

  useEffect(() => {
    if (!currentJob || !['queued', 'running'].includes(currentJob.status) || !activeAPIKey || quotaExpired) return
    let cancelled = false
    let polling = false
    const timer = window.setInterval(async () => {
      if (polling || document.visibilityState !== 'visible') return
      polling = true
      try {
        const res = await api.getPortalImageJob(activeAPIKey, currentJob.id, { includeCache: false })
        if (cancelled) return
        setCurrentJob(res.job)
        if (!['queued', 'running'].includes(res.job.status)) {
          void loadHistory(activeAPIKey, 1)
          void loadAssets(activeAPIKey, 1)
        }
      } catch {
        // keep polling quiet
      } finally {
        polling = false
      }
    }, 2500)
    return () => {
      cancelled = true
      window.clearInterval(timer)
    }
  }, [activeAPIKey, currentJob, loadAssets, loadHistory, quotaExpired])

  useEffect(() => {
    assetURLsRef.current = assetURLs
  }, [assetURLs])

  useEffect(() => {
    return () => {
      Object.values(assetURLsRef.current).forEach((url) => URL.revokeObjectURL(url))
    }
  }, [])

  const visibleAssets = useMemo(() => {
    const historyAssets = activeView === 'studio'
      ? historyJobs.slice(0, RECENT_JOB_COUNT).flatMap(job => job.assets?.slice(0, 1) ?? [])
      : historyJobs.flatMap(job => job.assets ?? [])
    const merged = [...(currentJob?.assets ?? []), ...assets, ...historyAssets]
    const seen = new Set<number>()
    return merged.filter((asset) => {
      if (seen.has(asset.id)) return false
      seen.add(asset.id)
      return true
    })
  }, [activeView, assets, currentJob, historyJobs])

  useEffect(() => {
    if (!activeAPIKey) return
    visibleAssets.forEach((asset) => {
      if (hasServerImageURL(asset)) return
      if (assetURLsRef.current[asset.id] || assetURLRequestsRef.current.has(asset.id)) return
      if (asset.cache_b64_json) {
        try {
          const binary = atob(asset.cache_b64_json)
          const bytes = new Uint8Array(binary.length)
          for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i)
          const blob = new Blob([bytes], {
            type: asset.mime_type || 'image/png',
          })
          const url = URL.createObjectURL(blob)
          setAssetURLs((prev) => {
            const next = { ...prev, [asset.id]: url }
            assetURLsRef.current = next
            return next
          })
          return
        } catch {
          // fall through
        }
      }
      assetURLRequestsRef.current.add(asset.id)
      void api.getPortalImageAssetFile(activeAPIKey, asset.id)
        .then((blob) => {
          const url = URL.createObjectURL(blob)
          setAssetURLs((prev) => {
            if (prev[asset.id]) {
              URL.revokeObjectURL(url)
              return prev
            }
            const next = { ...prev, [asset.id]: url }
            assetURLsRef.current = next
            return next
          })
        })
        .catch(() => undefined)
        .finally(() => {
          assetURLRequestsRef.current.delete(asset.id)
        })
    })
  }, [activeAPIKey, visibleAssets])

  const handleLogin = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const trimmed = apiKeyInput.trim()
    if (!trimmed) {
      setLoginError(t('imageStudioPortal.keyRequired'))
      return
    }
    setLoginLoading(true)
    setLoginError('')
    try {
      await verifyAndEnter(trimmed)
      setActiveAPIKey(trimmed)
      writeStoredAPIKey(trimmed, remember)
    } catch (err) {
      setLoginError(getErrorMessage(err))
    } finally {
      setLoginLoading(false)
    }
  }

  const handleLogout = () => {
    quotaRequestRef.current += 1
    quotaRefreshKeyRef.current = null
    setQuotaSnapshot(null)
    setQuotaError('')
    clearStoredAPIKey()
    setActiveAPIKey('')
    setAPIKeyInput('')
    setCurrentJob(null)
    setHistoryJobs([])
    setAssets([])
    setLoginError('')
  }

  const appendInputImages = (files: FileList | File[]) => {
    const list = Array.from(files).filter((file) => file.type.startsWith('image/'))
    if (list.length === 0) return
    const remaining = MAX_INPUT_IMAGES - inputImageDataURLs.length
    if (remaining <= 0) {
      showToast(t('images.maxInputImages', { max: MAX_INPUT_IMAGES }))
      return
    }
    const filesToRead = list.slice(0, remaining)
    void Promise.all(
      filesToRead.map(
        (file) =>
          new Promise<string>((resolve, reject) => {
            const reader = new FileReader()
            reader.onload = () => resolve(reader.result as string)
            reader.onerror = () => reject(new Error('read failed'))
            reader.readAsDataURL(file)
          }),
      ),
    ).then((urls) => {
      setInputImageDataURLs((current) => [...current, ...urls].slice(0, MAX_INPUT_IMAGES))
    })
  }

  const openGalleryPicker = async () => {
    if (!activeAPIKey) return
    if (inputImageDataURLs.length >= MAX_INPUT_IMAGES) {
      showToast(t('images.maxInputImages', { max: MAX_INPUT_IMAGES }))
      return
    }
    setGalleryPickerOpen(true)
    setGalleryPickerSelected(new Set())
    // Always refresh a page of gallery assets for the picker.
    setGalleryPickerLoading(true)
    try {
      const res = await api.getPortalImageAssets(activeAPIKey, {
        page: 1,
        pageSize: 48,
      })
      setAssets(res.assets ?? [])
      setAssetTotal(res.total ?? 0)
      setAssetPage(1)
    } catch (err) {
      showToast(getErrorMessage(err))
    } finally {
      setGalleryPickerLoading(false)
    }
  }

  const toggleGalleryPickerAsset = (assetID: number) => {
    setGalleryPickerSelected((prev) => {
      const next = new Set(prev)
      if (next.has(assetID)) {
        next.delete(assetID)
        return next
      }
      const remaining = MAX_INPUT_IMAGES - inputImageDataURLs.length
      if (next.size >= remaining) {
        showToast(t('images.maxInputImages', { max: MAX_INPUT_IMAGES }))
        return prev
      }
      next.add(assetID)
      return next
    })
  }

  const confirmGalleryPicker = async () => {
    if (!activeAPIKey || galleryPickerSelected.size === 0) {
      setGalleryPickerOpen(false)
      return
    }
    const selectedAssets = assets.filter((asset) => galleryPickerSelected.has(asset.id))
    if (selectedAssets.length === 0) {
      setGalleryPickerOpen(false)
      return
    }
    setGalleryPickerConfirming(true)
    try {
      const remaining = MAX_INPUT_IMAGES - inputImageDataURLs.length
      const toAdd = selectedAssets.slice(0, remaining)
      const urls: string[] = []
      for (const asset of toAdd) {
        urls.push(await assetToDataURL(activeAPIKey, asset))
      }
      if (urls.length > 0) {
        setInputImageDataURLs((current) => [...current, ...urls].slice(0, MAX_INPUT_IMAGES))
        setImageToImageMode(true)
        showToast(t('imageStudioPortal.galleryImagesAdded', { count: urls.length }))
      }
      setGalleryPickerOpen(false)
      setGalleryPickerSelected(new Set())
    } catch (err) {
      showToast(getErrorMessage(err))
    } finally {
      setGalleryPickerConfirming(false)
    }
  }

  const createPayload = (): CreateImageJobPayload => {
    const payload: CreateImageJobPayload = {
      prompt: prompt.trim(),
      model,
      output_format: outputFormat,
    }
    if (size !== 'auto') payload.size = size
    if (quality !== 'auto') payload.quality = quality
    if (style.trim()) payload.style = style.trim()
    if (imageToImageMode && inputImageDataURLs.length > 0) {
      payload.input_images = inputImageDataURLs
    }
    return payload
  }

  const submitJob = async () => {
    if (!activeAPIKey) return
    if (quotaLocked) {
      showToast(t(`imageStudioPortal.quota.${quota.status}`))
      return
    }
    const payload = createPayload()
    const isEdit = Boolean(payload.input_images?.length)
    if (!payload.prompt) {
      showToast(t('images.promptRequired'))
      return
    }
    if (imageToImageMode && !payload.input_images?.length) {
      showToast(t('images.inputImageRequired'))
      return
    }
    setSubmitting(true)
    try {
      const res = isEdit
        ? await api.createPortalImageEditJob(activeAPIKey, payload)
        : await api.createPortalImageJob(activeAPIKey, payload)
      setCurrentJob(res.job)
      setInspirationOpen(false)
      selectMobilePanel('canvas')
      showToast(t('images.jobCreated'))
      setHistoryPage(1)
      void loadHistory(activeAPIKey, 1)
    } catch (err) {
      showToast(getErrorMessage(err))
      void refreshQuota()
    } finally {
      setSubmitting(false)
    }
  }

  const deleteJob = async (job: ImageGenerationJob) => {
    if (!activeAPIKey) return
    try {
      await api.deletePortalImageJob(activeAPIKey, job.id)
      if (currentJob?.id === job.id) setCurrentJob(null)
      showToast(t('images.jobDeleted'))
      void loadHistory()
      void loadAssets()
    } catch (err) {
      showToast(getErrorMessage(err))
    }
  }

  const deleteAsset = async (asset: ImageAsset) => {
    if (!activeAPIKey) return
    try {
      await api.deletePortalImageAsset(activeAPIKey, asset.id)
      showToast(t('images.assetDeleted'))
      void loadAssets()
    } catch (err) {
      showToast(getErrorMessage(err))
    }
  }

  const downloadAsset = async (asset: ImageAsset) => {
    if (!activeAPIKey) return
    try {
      const blob = await api.getPortalImageAssetFile(activeAPIKey, asset.id, true)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = asset.filename || `image-${asset.id}.png`
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      showToast(getErrorMessage(err))
    }
  }

  const promptForAsset = useCallback((asset: ImageAsset) => {
    if (currentJob?.id === asset.job_id) return currentJob.prompt
    const fromHistory = historyJobs.find((job) => job.id === asset.job_id)
    if (fromHistory) return fromHistory.prompt
    return asset.revised_prompt || ''
  }, [currentJob, historyJobs])

  const openPreview = useCallback((asset: ImageAsset, promptText?: string) => {
    setPreviewAsset(asset)
    setPreviewPrompt(promptText ?? promptForAsset(asset))
  }, [promptForAsset])

  const closePreview = useCallback(() => {
    setPreviewAsset(null)
    setPreviewPrompt('')
  }, [])

  const copyPreviewPrompt = async (text: string) => {
    const value = text.trim()
    if (!value) {
      showToast(t('imageStudioPortal.noPromptToCopy'))
      return
    }
    try {
      await navigator.clipboard.writeText(value)
      showToast(t('common.copied'))
    } catch {
      showToast(t('common.copyFailed'))
    }
  }

  const usePromptInStudio = (text: string) => {
    setPrompt(text)
    setMobilePanel('compose')
    closePreview()
    navigate('/image-studio/studio')
    showToast(t('imageStudioPortal.promptFilled'))
  }

  const applyInspiration = (scene: StudioScene, presetSize: string) => {
    setPrompt(t(`imageStudioPortal.design.inspiration.${scene}.prompt`))
    setSize(presetSize)
    setImageToImageMode(false)
    setInspirationOpen(false)
    setMobilePanel('compose')
    window.requestAnimationFrame(() => {
      promptRef.current?.focus({ preventScroll: true })
      if (window.matchMedia('(max-width: 1023px)').matches) workspaceRef.current?.scrollIntoView({ block: 'start' })
    })
  }

  const openInspiration = () => {
    setInspirationOpen(true)
    selectMobilePanel('canvas')
  }

  const statusLabel = (status: string) => {
    const key = `images.status.${status}`
    const translated = t(key)
    return translated === key ? status : translated
  }

  const toolbar = (
    <div className="portal-toolbar">
      <Button
        variant="outline"
        size="icon-sm"
        onClick={() => i18n.changeLanguage(i18n.language === 'zh' ? 'en' : 'zh')}
        title={i18n.language === 'zh' ? 'English' : '中文'}
      >
        <Languages className="size-4" />
      </Button>
      <CinematicThemeSwitcher size="compact" />
    </div>
  )

  if (bootstrapping) {
    return (
      <div className="portal-login-shell">
        <div aria-hidden="true" className="portal-login-grid" />
        <div className="relative flex flex-col items-center gap-4">
          <div className="portal-busy-loader-shell">
            <PixelLoader mode="orbit" size="lg" />
          </div>
          <ShimmerText className="text-sm font-medium">{t('common.loading')}</ShimmerText>
        </div>
      </div>
    )
  }

  if (!activeAPIKey) {
    return (
      <div className="portal-login-shell">
        <div aria-hidden="true" className="portal-login-grid" />
        <div className="absolute right-4 top-4 z-10">{toolbar}</div>
        <div className="portal-login-card animate-image-studio-fade-in">
          <div className="portal-login-showcase" aria-hidden="true">
            <div className="portal-login-art"><StudioArtwork scene="landscape" /></div>
            <div className="portal-login-art"><StudioArtwork scene="product" /></div>
            <div className="portal-login-art"><StudioArtwork scene="architecture" /></div>
          </div>
          <div className="p-7 sm:p-8">
            <div className="mb-6 flex flex-col items-center text-center">
              <div className="relative -mt-12 mb-3">
                <div className="absolute -inset-2 rounded-2xl bg-primary/15 blur-lg" />
                <img
                  src={logoSrc}
                  alt={siteName}
                  className="relative size-14 rounded-2xl border-2 border-card object-cover shadow-lg ring-1 ring-border/70"
                />
              </div>
              <span className="portal-eyebrow mb-2">
                <Sparkles className="size-3" />
                {siteName}
              </span>
              <h1 className="text-xl font-semibold tracking-tight text-foreground">
                {t('imageStudioPortal.loginTitle')}
              </h1>
              <p className="mt-1.5 max-w-sm text-[13px] leading-relaxed text-muted-foreground">
                {t('imageStudioPortal.loginSubtitle')}
              </p>
            </div>
            <form onSubmit={handleLogin} className="space-y-4">
              <div className="relative">
                <KeyRound className="pointer-events-none absolute left-3.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  type={showKey ? 'text' : 'password'}
                  value={apiKeyInput}
                  onChange={(event) => setAPIKeyInput(event.target.value)}
                  placeholder={t('imageStudioPortal.keyPlaceholder')}
                  className="h-11 rounded-xl pl-10 pr-10 font-mono text-[13px]"
                  autoComplete="off"
                  autoFocus
                />
                <button
                  type="button"
                  className="absolute right-2 top-1/2 inline-flex size-7 -translate-y-1/2 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                  onClick={() => setShowKey((v) => !v)}
                >
                  {showKey ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
                </button>
              </div>
              <label className="flex cursor-pointer items-start gap-2.5 rounded-xl border border-border/60 bg-muted/25 px-3 py-2.5 select-none transition-colors hover:border-border">
                <input
                  type="checkbox"
                  checked={remember}
                  onChange={(e) => setRemember(e.target.checked)}
                  className="mt-0.5 size-4 shrink-0 cursor-pointer rounded border-border accent-primary"
                />
                <span className="text-xs leading-relaxed">
                  <span className="font-medium text-foreground">{t('imageStudioPortal.rememberKey')}</span>
                  <span className="mt-0.5 block text-[11px] text-muted-foreground">
                    {t('imageStudioPortal.rememberHint')}
                  </span>
                </span>
              </label>
              {loginError ? (
                <div className="rounded-xl border border-destructive/30 bg-destructive/10 px-3.5 py-2.5 text-xs leading-relaxed text-destructive animate-image-studio-shake">
                  {loginError}
                </div>
              ) : null}
              <Button type="submit" className="portal-generate h-11 w-full" disabled={loginLoading}>
                {loginLoading ? <Loader2 className="size-4 animate-spin" /> : <LogIn className="size-4" />}
                {t('imageStudioPortal.loginButton')}
              </Button>
            </form>
            <p className="mt-5 flex items-center justify-center gap-1.5 text-center text-[11px] text-muted-foreground">
              <ShieldCheck className="size-3.5 text-primary/80" />
              {t('imageStudioPortal.loginHint')}
            </p>
          </div>
        </div>
      </div>
    )
  }

  const busy = currentJob && ['queued', 'running'].includes(currentJob.status)
  const resultAssets = currentJob?.assets ?? []

  return (
    <div className="image-studio-portal">
      <header className="portal-header">
        <div className="portal-header-inner">
          <div className="portal-brand">
            <img src={logoSrc} alt={siteName} className="portal-brand-logo" />
            <div className="min-w-0">
              <div className="portal-brand-title truncate leading-tight">{t('imageStudioPortal.title')}</div>
              <div className="truncate text-[11px] text-muted-foreground leading-tight">{siteName}</div>
            </div>
          </div>

          <div className="portal-header-navigation">
            <PortalTabs activeView={activeView} />
          </div>

          <PortalQuota maskedKey={maskKey(activeAPIKey)} quota={quota} loading={quotaLoading} error={quotaError} onRefresh={() => void refreshQuota()} />

          <div className="portal-account">
            {toolbar}
            <Button variant="ghost" size="sm" className="portal-logout" onClick={handleLogout} aria-label={t('imageStudioPortal.logout')}>
              <LogOut className="size-3.5" />
              <span className="hidden sm:inline">{t('imageStudioPortal.logout')}</span>
            </Button>
          </div>
        </div>
      </header>

      <main className="portal-main">
        {quotaLocked ? <div className="portal-quota-notice" role="status">{t(`imageStudioPortal.quota.${quota.status}`)}</div> : null}
        {activeView === 'studio' ? (
          <>
          <div className="portal-intro">
            <div><span className="portal-eyebrow">{t('imageStudioPortal.design.eyebrow')}</span><h1>{t('imageStudioPortal.design.title')}</h1></div>
            <p>{t('imageStudioPortal.design.subtitle')}</p>
          </div>
          <div ref={workspaceRef} className="portal-workspace" data-mobile-panel={mobilePanel}>
            <SegmentedPillGroup
              className="portal-mobile-switch"
              label={t('imageStudioPortal.design.mobileView')}
              value={mobilePanel}
              onChange={selectMobilePanel}
              options={[
                { value: 'compose', label: t('imageStudioPortal.design.editPanel'), icon: <Pencil className="size-4" /> },
                { value: 'canvas', label: t('imageStudioPortal.design.canvasPanel'), icon: <ImageIcon className="size-4" /> },
              ]}
            />
            <Card className="portal-composer">
              <div className="portal-panel-heading">
                <h2><Sparkles className="size-4" />{t('imageStudioPortal.studioHeading')}</h2>
                <span>{t('imageStudioPortal.design.yourPrompt')}</span>
              </div>

              <div className="portal-composer-body">
                <div className="portal-form-scroll">
                  <SegmentedPillGroup
                    label={t('imageStudioPortal.design.creationMode')}
                    value={imageToImageMode ? 'edit' : 'generate'}
                    onChange={value => setImageToImageMode(value === 'edit')}
                    options={[
                      { value: 'generate', label: t('images.textToImage'), icon: <Sparkles className="size-4" /> },
                      { value: 'edit', label: t('images.imageToImage'), icon: <Upload className="size-4" /> },
                    ]}
                  />

                  <div className="portal-prompt-field">
                    <div className="portal-field-heading">
                      <label htmlFor="portal-prompt">{t('images.prompt')}</label>
                      <Button type="button" size="sm" variant="ghost" className="portal-text-action" onClick={openInspiration}><Lightbulb className="size-3.5" />{t('imageStudioPortal.design.findInspiration')}</Button>
                    </div>
                    <div className="portal-prompt-editor">
                    <textarea
                      id="portal-prompt"
                      ref={promptRef}
                      value={prompt}
                      onChange={(e) => setPrompt(e.target.value)}
                      onKeyDown={(e) => {
                        if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && prompt.trim() && !submitting && !quotaLocked) {
                          e.preventDefault()
                          void submitJob()
                        }
                      }}
                      rows={5}
                      maxLength={8000}
                      placeholder={t('imageStudioPortal.design.promptPlaceholder')}
                    />
                    <div className="portal-prompt-footer"><span>{t('imageStudioPortal.design.promptHint')}</span><span>{prompt.length} / 8000</span></div>
                    </div>
                  </div>

                  <div className="portal-model-settings">
                    <Field label={t('images.model')}>
                      <Select value={model} onValueChange={setModel} options={IMAGE_MODELS} compact />
                    </Field>
                    <div className="flex min-w-0 flex-col gap-1.5">
                      <div className="flex items-center justify-between gap-2">
                        <span className="text-xs font-semibold text-foreground">{t('images.size')}</span>
                        <span className="font-mono text-[10.5px] text-muted-foreground">
                          {size === 'auto' ? t('imageStudioPortal.design.autoSize') : size.replace('x', ' × ')}
                        </span>
                      </div>
                      <div className="portal-ratio-pills" role="group" aria-label={t('images.size')}>
                        {PORTAL_QUICK_RATIOS.map((ratio) => {
                          const Icon = ratio.icon
                          const active = portalRatioForSize(size) === ratio.id
                          return (
                            <button
                              key={ratio.id}
                              type="button"
                              className="portal-ratio-pill"
                              aria-pressed={active}
                              onClick={() => setSize(ratio.size)}
                            >
                              <Icon />
                              <span>{t(ratio.labelKey)}</span>
                            </button>
                          )
                        })}
                      </div>
                      <Select
                        value={size}
                        onValueChange={setSize}
                        options={SIZE_OPTIONS.map(option => ({ ...option, label: option.value === 'auto' ? t('imageStudioPortal.design.autoSize') : option.label.replace('x', ' × ') }))}
                        compact
                      />
                    </div>
                  </div>

                  {imageToImageMode ? (
                    <div className="flex min-w-0 flex-col gap-1.5">
                      <div className="flex h-5 items-center justify-between gap-2">
                        <span className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
                          {t('images.inputImage')}
                          <span className="rounded-full bg-muted px-1.5 py-0.5 font-mono text-[10px] font-medium text-muted-foreground">
                            {inputImageDataURLs.length}/{MAX_INPUT_IMAGES}
                          </span>
                        </span>
                        <div className="flex items-center gap-0.5">
                          <Button
                            type="button"
                            size="sm"
                            variant="ghost"
                            className="h-6 px-1.5 text-[11px]"
                            onClick={() => fileInputRef.current?.click()}
                            disabled={inputImageDataURLs.length >= MAX_INPUT_IMAGES}
                          >
                            <Upload className="size-3" />
                            {t('images.upload')}
                          </Button>
                          <Button
                            type="button"
                            size="sm"
                            variant="ghost"
                            className="h-6 px-1.5 text-[11px]"
                            onClick={() => void openGalleryPicker()}
                            disabled={inputImageDataURLs.length >= MAX_INPUT_IMAGES}
                          >
                            <FolderOpen className="size-3" />
                            {t('imageStudioPortal.fromGallery')}
                          </Button>
                        </div>
                      </div>
                      <div
                        className={cn(
                          'relative flex min-h-[104px] flex-col items-center justify-center rounded-xl border border-dashed px-3 py-3 transition-all',
                          dragOver
                            ? 'border-primary bg-primary/10 shadow-sm'
                            : 'border-border/80 bg-muted/20 hover:border-primary/35 hover:bg-muted/35',
                        )}
                        onDragOver={(e) => {
                          e.preventDefault()
                          setDragOver(true)
                        }}
                        onDragLeave={() => setDragOver(false)}
                        onDrop={(e) => {
                          e.preventDefault()
                          setDragOver(false)
                          if (e.dataTransfer.files?.length) appendInputImages(e.dataTransfer.files)
                        }}
                      >
                        <input
                          ref={fileInputRef}
                          type="file"
                          accept="image/*"
                          multiple
                          className="hidden"
                          onChange={(e) => {
                            if (e.target.files?.length) appendInputImages(e.target.files)
                            e.target.value = ''
                          }}
                        />
                        {inputImageDataURLs.length === 0 ? (
                          <div className="flex w-full flex-col items-center gap-2.5 text-center text-muted-foreground">
                            <span className="flex size-8 items-center justify-center rounded-lg bg-primary/10 text-primary">
                              <Upload className="size-4" />
                            </span>
                            <div className="text-[11.5px] font-medium text-foreground">{t('imageStudioPortal.dropImages')}</div>
                            <div className="flex flex-wrap items-center justify-center gap-1.5">
                              <Button type="button" size="sm" variant="outline" className="h-7 text-xs" onClick={() => fileInputRef.current?.click()}>
                                <Upload className="size-3" />
                                {t('images.upload')}
                              </Button>
                              <Button type="button" size="sm" variant="outline" className="h-7 text-xs" onClick={() => void openGalleryPicker()}>
                                <FolderOpen className="size-3" />
                                {t('imageStudioPortal.fromGallery')}
                              </Button>
                            </div>
                          </div>
                        ) : (
                          <div className="flex w-full flex-wrap gap-2">
                            {inputImageDataURLs.map((url, index) => (
                              <div
                                key={`${index}-${url.slice(0, 24)}`}
                                className="group relative size-15 overflow-hidden rounded-xl border border-border/80 shadow-sm"
                              >
                                <img src={url} alt="" className="size-full object-cover transition-transform duration-200 group-hover:scale-105" />
                                <span className="absolute bottom-1 left-1 rounded bg-black/60 px-1 font-mono text-[9px] text-white">
                                  {index + 1}
                                </span>
                                <button
                                  type="button"
                                  className="absolute right-1 top-1 flex size-4.5 items-center justify-center rounded-full bg-black/70 text-white transition-colors hover:bg-destructive"
                                  onClick={() => setInputImageDataURLs((list) => list.filter((_, i) => i !== index))}
                                >
                                  <X className="size-2.5" />
                                </button>
                              </div>
                            ))}
                            {inputImageDataURLs.length < MAX_INPUT_IMAGES ? (
                              <>
                                <button
                                  type="button"
                                  onClick={() => fileInputRef.current?.click()}
                                  className="flex size-15 flex-col items-center justify-center gap-0.5 rounded-xl border border-dashed border-border text-muted-foreground transition-colors hover:border-primary/45 hover:bg-primary/5 hover:text-primary"
                                  title={t('images.upload')}
                                >
                                  <Upload className="size-3.5" />
                                </button>
                                <button
                                  type="button"
                                  onClick={() => void openGalleryPicker()}
                                  className="flex size-15 flex-col items-center justify-center gap-0.5 rounded-xl border border-dashed border-border text-muted-foreground transition-colors hover:border-primary/45 hover:bg-primary/5 hover:text-primary"
                                  title={t('imageStudioPortal.fromGallery')}
                                >
                                  <FolderOpen className="size-3.5" />
                                </button>
                              </>
                            ) : null}
                          </div>
                        )}
                      </div>
                    </div>
                  ) : null}

                  <div className="portal-advanced">
                    <Button
                      type="button"
                      variant="ghost"
                      onClick={() => setAdvancedOpen((v) => !v)}
                      aria-expanded={advancedOpen}
                      aria-controls="portal-advanced-settings"
                      className="portal-advanced-toggle"
                    >
                      <span>{t('images.advancedParams')}</span>
                      <ChevronDown className={cn('size-4 transition-transform motion-reduce:transition-none', advancedOpen && 'rotate-180')} />
                    </Button>
                    {advancedOpen ? (
                      <div id="portal-advanced-settings" className="portal-advanced-panel animate-image-studio-fade-in">
                        <Field label={t('images.quality')}>
                          <Select value={quality} onValueChange={setQuality} options={imageQualityOptions(model)} compact />
                        </Field>
                        <Field label={t('images.format')}>
                          <Select value={outputFormat} onValueChange={setOutputFormat} options={FORMAT_OPTIONS} compact />
                        </Field>
                        <div className="col-span-2">
                          <Field label={t('images.style')}>
                            <Input
                              value={style}
                              onChange={(e) => setStyle(e.target.value)}
                              placeholder={t('images.stylePlaceholder')}
                              className="h-9"
                            />
                          </Field>
                        </div>
                      </div>
                    ) : null}
                  </div>
                </div>

                <div className="portal-composer-footer">
                  {quota?.image_pricing?.[model]?.user_billing_mode === 'per_image' ? (
                    <p className="portal-price-pill" role="status">
                      <Sparkles className="size-3 text-primary" />
                      {t('imageStudioPortal.quota.imagePrice', { price: formatImageStudioQuota(quota.image_pricing[model].image_unit_price) })}
                    </p>
                  ) : null}
                  <Button
                    className="portal-generate"
                    disabled={submitting || !prompt.trim() || quotaLocked}
                    onClick={() => void submitJob()}
                  >
                    {submitting ? <RefreshCw className="size-4 animate-spin" /> : <Sparkles className="size-4" />}
                    {t('images.generate')}
                  </Button>
                  <div className="portal-submit-hint"><span>{t('imageStudioPortal.design.privateWorkspace')}</span><kbd>⌘ / Ctrl + Enter</kbd></div>
                </div>
              </div>
            </Card>

            <div className="portal-preview-column">
            <Card className="portal-canvas">
              <div className="portal-panel-heading">
                <div className="min-w-0">
                  <h2>{t('imageStudioPortal.canvasTitle')}</h2>
                  <p className="portal-canvas-subtitle">
                    {currentJob && !inspirationOpen
                      ? busy
                        ? statusLabel(currentJob.status)
                        : t('imageStudioPortal.latestResult') + ` · #${currentJob.id}`
                      : t('imageStudioPortal.design.canvasReady')}
                  </p>
                </div>
                {inspirationOpen && currentJob ? <Button size="sm" variant="ghost" className="portal-text-action" onClick={() => setInspirationOpen(false)}>{t('imageStudioPortal.design.backToResult')}<ArrowRight className="size-3.5" /></Button> : resultAssets[0] && !inspirationOpen ? (
                  <div className="flex shrink-0 gap-1">
                    <Button size="sm" variant="outline" className="h-8" onClick={() => openPreview(resultAssets[0], currentJob?.prompt)} aria-label={t('imageStudioPortal.viewFullscreen')}>
                      <Expand className="size-3.5" />
                      <span className="hidden sm:inline">{t('imageStudioPortal.preview')}</span>
                    </Button>
                    <Button size="sm" variant="outline" className="h-8" onClick={() => void downloadAsset(resultAssets[0])} aria-label={t('images.download')}>
                      <Download className="size-3.5" />
                    </Button>
                  </div>
                ) : null}
              </div>
              <div className={cn('portal-canvas-body', currentJob && !inspirationOpen && 'portal-canvas-has-job')}>
                {currentJob && !inspirationOpen ? (
                  busy ? (
                    <div className="flex w-full max-w-lg flex-col items-center gap-4 animate-image-studio-fade-in">
                      <div className="portal-busy-stage">
                        <div className="absolute inset-y-0 w-1/2 animate-image-studio-shimmer bg-gradient-to-r from-transparent via-primary/10 to-transparent" />
                        <div className="portal-busy-loader-shell relative">
                          <PixelLoader mode="orbit" size="lg" />
                        </div>
                        <div className="relative flex flex-col items-center gap-2 text-center">
                          <ShimmerText className="text-sm font-semibold">{statusLabel(currentJob.status)}</ShimmerText>
                          <div className="text-xs text-muted-foreground">{t('imageStudioPortal.design.runningHint')}</div>
                          <div className="mt-1 h-1.5 w-44 overflow-hidden rounded-full bg-muted">
                            <div className="h-full w-1/2 animate-image-studio-progress rounded-full bg-primary/80" />
                          </div>
                        </div>
                      </div>
                      {currentJob.prompt ? (
                        <p className="line-clamp-2 max-w-md text-center text-xs leading-relaxed text-muted-foreground">
                          {currentJob.prompt}
                        </p>
                      ) : null}
                    </div>
                  ) : currentJob.status === 'failed' ? (
                    <div className="flex max-w-md flex-col items-center gap-3.5 px-2 text-center animate-image-studio-fade-in">
                      <div className="rounded-2xl border border-destructive/25 bg-destructive/10 px-5 py-4 shadow-sm animate-image-studio-shake">
                        <div className="text-sm font-semibold text-destructive">{t('images.createJobFailed')}</div>
                        <div className="mt-1.5 text-xs leading-relaxed text-destructive/90">
                          {currentJob.error_message || t('imageStudioPortal.tryAgain')}
                        </div>
                      </div>
                      <Button size="sm" variant="outline" onClick={() => void submitJob()} disabled={submitting || !prompt.trim()}>
                        <RefreshCw className="size-3.5" />
                        {t('imageStudioPortal.retry')}
                      </Button>
                    </div>
                  ) : (
                    <div className="flex w-full flex-col items-center gap-3.5 animate-image-studio-result-in">
                      {resultAssets.length === 0 ? (
                        <div className="text-sm text-muted-foreground">{t('imageStudioPortal.emptyResult')}</div>
                      ) : (
                        <div className="flex w-full flex-wrap items-stretch justify-center gap-4">
                          {resultAssets.map((asset) => {
                            const src = imageSrc(asset, assetURLs)
                            return (
                              <div
                                key={asset.id}
                                className="portal-result-card group"
                              >
                                <Button
                                  type="button"
                                  variant="ghost"
                                  className="portal-result-image image-studio-checkerboard"
                                  onClick={() => openPreview(asset, currentJob.prompt)}
                                  title={t('imageStudioPortal.viewFullscreen')}
                                >
                                  {src ? (
                                    <img
                                      src={src}
                                      alt={asset.filename}
                                      className="portal-result-original"
                                    />
                                  ) : (
                                    <div className="flex size-full items-center justify-center text-muted-foreground">
                                      <PixelLoader mode="dots" size="md" />
                                    </div>
                                  )}
                                  <div className="pointer-events-none absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition-all duration-200 group-hover:bg-black/25 group-hover:opacity-100">
                                    <span className="inline-flex items-center gap-1.5 rounded-full border border-white/20 bg-black/65 px-3 py-1.5 text-xs font-medium text-white backdrop-blur-md shadow-lg">
                                      <Expand className="size-3.5" />
                                      {t('imageStudioPortal.viewFullscreen')}
                                    </span>
                                  </div>
                                </Button>
                                <div className="flex items-center justify-between gap-2 border-t border-border/70 bg-muted/20 px-3.5 py-2.5">
                                  <div className="flex min-w-0 items-center gap-1.5">
                                    <span className="truncate rounded-md bg-muted px-2 py-0.5 font-mono text-[11px] font-medium text-foreground/85">
                                      {asset.actual_size || (asset.width && asset.height ? `${asset.width}×${asset.height}` : asset.model)}
                                    </span>
                                    {asset.output_format ? (
                                      <span className="rounded-md border border-border/80 px-1.5 py-0.5 font-mono text-[10px] uppercase text-muted-foreground">
                                        {asset.output_format}
                                      </span>
                                    ) : null}
                                  </div>
                                  <div className="flex shrink-0 items-center gap-1">
                                    <Button
                                      size="icon-sm"
                                      variant="ghost"
                                      className="size-7"
                                      onClick={() => void copyPreviewPrompt(currentJob.prompt)}
                                      aria-label={t('images.copyPrompt')}
                                      title={t('images.copyPrompt')}
                                    >
                                      <Copy className="size-3.5" />
                                    </Button>
                                    <Button
                                      size="icon-sm"
                                      variant="ghost"
                                      className="size-7"
                                      onClick={() => openPreview(asset, currentJob.prompt)}
                                      aria-label={t('imageStudioPortal.viewFullscreen')}
                                      title={t('imageStudioPortal.viewFullscreen')}
                                    >
                                      <Expand className="size-3.5" />
                                    </Button>
                                    <Button size="icon-sm" variant="ghost" className="size-7" onClick={() => void downloadAsset(asset)} aria-label={t('images.download')} title={t('images.download')}>
                                      <Download className="size-3.5" />
                                    </Button>
                                  </div>
                                </div>
                              </div>
                            )
                          })}
                        </div>
                      )}
                      <p className="portal-result-hint">{t('imageStudioPortal.design.fullImageHint')}</p>
                    </div>
                  )
                ) : (
                  <PortalInspiration onSelect={applyInspiration} />
                )}
              </div>
            </Card>
            <section className="portal-recent" aria-label={t('imageStudioPortal.design.recentTitle')}>
              <div className="portal-recent-heading">
                <h2><History className="size-3.5 text-primary" />{t('imageStudioPortal.design.recentTitle')}</h2>
                <NavLink to="/image-studio/history">{t('imageStudioPortal.design.allHistory')}<ArrowRight className="size-3.5" /></NavLink>
              </div>
              {historyLoading && historyJobs.length === 0 ? <p className="portal-recent-empty">{t('common.loading')}</p> : historyJobs.length === 0 ? <p className="portal-recent-empty">{t('imageStudioPortal.design.recentEmpty')}</p> : (
                <div className="portal-recent-grid">
                  {historyJobs.slice(0, RECENT_JOB_COUNT).map(job => {
                    const asset = job.assets?.[0]
                    const src = asset ? thumbnailSrc(asset, assetURLs) : ''
                    return (
                      <Button
                        key={job.id}
                        type="button"
                        variant="ghost"
                        aria-pressed={currentJob?.id === job.id && !inspirationOpen}
                        className="portal-recent-item"
                        onClick={() => { setCurrentJob(job); setInspirationOpen(false); selectMobilePanel('canvas') }}
                      >
                        <span className="portal-recent-thumb">{src ? <img loading="lazy" decoding="async" src={src} alt="" /> : <ImageIcon className="size-4" />}</span>
                        <span className="portal-recent-description"><span>{job.prompt}</span><small><span className={cn('portal-status-dot', `portal-status-${job.status}`)} />{statusLabel(job.status)}</small></span>
                      </Button>
                    )
                  })}
                </div>
              )}
            </section>
            </div>
          </div>
          </>
        ) : null}

        {activeView === 'history' ? (
          <Card className="portal-collection-card gap-0 py-0 animate-image-studio-fade-in">
            <div className="portal-collection-head">
              <div className="flex items-center gap-3">
                <div className="portal-collection-icon">
                  <Clock3 className="size-4.5" />
                </div>
                <div>
                  <div className="flex flex-wrap items-center gap-2">
                    <h2 className="text-[15px] font-semibold tracking-tight">{t('imageStudioPortal.historyHeading')}</h2>
                    <ValuePill tone="primary" count={historyTotal}>{t('imageStudioPortal.jobCount', { count: historyTotal })}</ValuePill>
                  </div>
                  <div className="mt-0.5 text-xs text-muted-foreground">
                    {t('imageStudioPortal.historyDesc')}
                  </div>
                </div>
              </div>
              <Button size="sm" variant="outline" className="h-8.5 rounded-xl" onClick={() => void loadHistory()} disabled={historyLoading}>
                <RefreshCw className={cn('size-3.5', historyLoading && 'animate-spin')} />
                {t('common.refresh')}
              </Button>
            </div>
            <CardContent className="p-4 sm:p-5">
              {historyLoading && historyJobs.length === 0 ? (
                <div className="flex flex-col items-center justify-center gap-3 py-16 text-muted-foreground">
                  <PixelLoader mode="drive" size="md" label={t('common.loading')} />
                </div>
              ) : historyJobs.length === 0 ? (
                <div className="flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border bg-muted/15 py-16 text-center">
                  <span className="flex size-12 items-center justify-center rounded-2xl border border-border bg-card text-muted-foreground shadow-sm">
                    <History className="size-5" />
                  </span>
                  <div className="text-sm font-medium text-muted-foreground">{t('imageStudioPortal.emptyHistory')}</div>
                </div>
              ) : (
                <div className="space-y-2.5">
                  {historyJobs.map((job) => {
                    const thumb = job.assets?.[0]
                    const thumbSrc = thumb ? thumbnailSrc(thumb, assetURLs) : ''
                    const canPreview = Boolean(thumb)
                    return (
                      <div key={job.id} className="portal-history-row group">
                        <button
                          type="button"
                          disabled={!canPreview}
                          onClick={() => thumb && openPreview(thumb, job.prompt)}
                          className={cn(
                            'image-studio-checkerboard relative size-18 shrink-0 overflow-hidden rounded-xl border border-border/70 transition-all',
                            canPreview && 'cursor-zoom-in hover:border-primary/45 hover:ring-2 hover:ring-primary/20',
                          )}
                          title={canPreview ? t('imageStudioPortal.viewFullscreen') : undefined}
                        >
                          {thumbSrc ? (
                            <img loading="lazy" decoding="async" src={thumbSrc} alt="" className="size-full object-cover transition-transform duration-300 group-hover:scale-105" />
                          ) : (
                            <div className="flex size-full items-center justify-center text-muted-foreground/50">
                              <ImageIcon className="size-5" />
                            </div>
                          )}
                        </button>
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-2">
                            <ValuePill tone={jobStatusTone(job.status)} dot pulse={job.status === 'running'}>
                              {statusLabel(job.status)}
                            </ValuePill>
                            <span className="font-mono text-[11px] tabular-nums text-muted-foreground">#{job.id}</span>
                            <span className="text-[11px] text-muted-foreground">{formatBeijingTime(job.created_at)}</span>
                          </div>
                          <div className="mt-1.5 line-clamp-2 text-[13.5px] leading-relaxed text-foreground">{job.prompt}</div>
                          {job.error_message || job.warning ? (
                            <div className={`mt-1.5 line-clamp-1 rounded-lg px-2 py-1 text-xs ${job.status === 'failed' ? 'bg-destructive/8 text-destructive' : 'bg-amber-500/10 text-amber-700 dark:text-amber-300'}`}>
                              {job.error_message || job.warning}
                            </div>
                          ) : null}
                        </div>
                        <div className="flex shrink-0 flex-col gap-1.5 sm:flex-row sm:items-start">
                          {canPreview ? (
                            <Button
                              size="sm"
                              variant="outline"
                              className="h-8 rounded-lg"
                              onClick={() => thumb && openPreview(thumb, job.prompt)}
                            >
                              <Expand className="size-3.5" />
                              {t('imageStudioPortal.preview')}
                            </Button>
                          ) : null}
                          <Button
                            size="sm"
                            variant="outline"
                            className="h-8 rounded-lg"
                            onClick={() => usePromptInStudio(job.prompt)}
                            title={t('imageStudioPortal.usePrompt')}
                          >
                            <Pencil className="size-3.5" />
                            <span className="hidden sm:inline">{t('imageStudioPortal.usePrompt')}</span>
                          </Button>
                          <Button
                            size="icon-sm"
                            variant="outline"
                            className="h-8 w-8 rounded-lg hover:border-destructive/40 hover:text-destructive"
                            onClick={() => void deleteJob(job)}
                            disabled={['queued', 'running'].includes(job.status)}
                            title={t('common.delete')}
                          >
                            <Trash2 className="size-3.5" />
                          </Button>
                        </div>
                      </div>
                    )
                  })}
                </div>
              )}

              {historyTotal > HISTORY_PAGE_SIZE ? (
                <div className="mt-5">
                  <Pagination
                    page={historyPage}
                    totalPages={Math.max(1, Math.ceil(historyTotal / HISTORY_PAGE_SIZE))}
                    pageSize={HISTORY_PAGE_SIZE}
                    totalItems={historyTotal}
                    onPageChange={(page) => setHistoryPage(page)}
                  />
                </div>
              ) : null}
            </CardContent>
          </Card>
        ) : null}

        {activeView === 'gallery' ? (
          <Card className="portal-collection-card gap-0 py-0 animate-image-studio-fade-in">
            <div className="portal-collection-head">
              <div className="flex items-center gap-3">
                <div className="portal-collection-icon">
                  <Images className="size-4.5" />
                </div>
                <div>
                  <div className="flex flex-wrap items-center gap-2">
                    <h2 className="text-[15px] font-semibold tracking-tight">{t('imageStudioPortal.galleryHeading')}</h2>
                    <ValuePill tone="primary" count={assetTotal}>{t('imageStudioPortal.assetCount', { count: assetTotal })}</ValuePill>
                  </div>
                  <div className="mt-0.5 text-xs text-muted-foreground">
                    {t('imageStudioPortal.galleryDesc')}
                  </div>
                </div>
              </div>
              <Button size="sm" variant="outline" className="h-8.5 rounded-xl" onClick={() => void loadAssets()} disabled={galleryLoading}>
                <RefreshCw className={cn('size-3.5', galleryLoading && 'animate-spin')} />
                {t('common.refresh')}
              </Button>
            </div>
            <CardContent className="p-4 sm:p-5">
              {galleryLoading && assets.length === 0 ? (
                <div className="flex flex-col items-center justify-center gap-3 py-16 text-muted-foreground">
                  <PixelLoader mode="drive" size="md" label={t('common.loading')} />
                </div>
              ) : assets.length === 0 ? (
                <div className="flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border bg-muted/15 py-16 text-center">
                  <span className="flex size-12 items-center justify-center rounded-2xl border border-border bg-card text-muted-foreground shadow-sm">
                    <Images className="size-5" />
                  </span>
                  <div className="text-sm font-medium text-muted-foreground">{t('imageStudioPortal.emptyGallery')}</div>
                </div>
              ) : (
                <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 xl:grid-cols-5">
                  {assets.map((asset) => {
                    const src = thumbnailSrc(asset, assetURLs)
                    const resolution = asset.actual_size || (asset.width && asset.height ? `${asset.width}×${asset.height}` : '')
                    return (
                      <div key={asset.id} className="portal-gallery-tile group">
                        <button
                          type="button"
                          className="image-studio-checkerboard relative aspect-square w-full cursor-zoom-in overflow-hidden"
                          onClick={() => openPreview(asset)}
                          title={t('imageStudioPortal.viewFullscreen')}
                        >
                          {src ? (
                            <img loading="lazy" decoding="async" src={src} alt={asset.filename} className="size-full object-cover" />
                          ) : (
                            <div className="flex size-full items-center justify-center text-muted-foreground">
                              <PixelLoader mode="dots" size="sm" />
                            </div>
                          )}
                          {resolution ? (
                            <span className="pointer-events-none absolute left-2 top-2 rounded-md bg-black/55 px-1.5 py-0.5 font-mono text-[10px] text-white/95 backdrop-blur-md opacity-0 transition-opacity group-hover:opacity-100">
                              {resolution}
                            </span>
                          ) : null}
                          <div className="pointer-events-none absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition-all duration-200 group-hover:bg-black/20 group-hover:opacity-100">
                            <span className="inline-flex items-center gap-1 rounded-full border border-white/20 bg-black/65 px-2.5 py-1 text-[11px] font-medium text-white backdrop-blur-md">
                              <Expand className="size-3" />
                              {t('imageStudioPortal.preview')}
                            </span>
                          </div>
                        </button>
                        <div className="absolute inset-x-0 bottom-0 flex items-center justify-between gap-1 bg-gradient-to-t from-black/75 via-black/45 to-transparent p-2.5 pt-8 opacity-100 transition-opacity sm:opacity-0 sm:group-hover:opacity-100">
                          <span className="truncate font-mono text-[11px] text-white/90">#{asset.id}</span>
                          <div className="flex gap-1">
                            <Button
                              size="icon-sm"
                              variant="secondary"
                              className="size-7 rounded-lg border border-white/15 bg-black/45 text-white backdrop-blur-md hover:bg-black/70"
                              onClick={(e) => {
                                e.stopPropagation()
                                openPreview(asset)
                              }}
                              title={t('imageStudioPortal.viewFullscreen')}
                            >
                              <Expand className="size-3.5" />
                            </Button>
                            <Button
                              size="icon-sm"
                              variant="secondary"
                              className="size-7 rounded-lg border border-white/15 bg-black/45 text-white backdrop-blur-md hover:bg-black/70"
                              onClick={(e) => {
                                e.stopPropagation()
                                void downloadAsset(asset)
                              }}
                              title={t('images.download')}
                            >
                              <Download className="size-3.5" />
                            </Button>
                            <Button
                              size="icon-sm"
                              variant="secondary"
                              className="size-7 rounded-lg border border-white/15 bg-black/45 text-white backdrop-blur-md hover:bg-destructive/85"
                              onClick={(e) => {
                                e.stopPropagation()
                                void deleteAsset(asset)
                              }}
                              title={t('common.delete')}
                            >
                              <Trash2 className="size-3.5" />
                            </Button>
                          </div>
                        </div>
                      </div>
                    )
                  })}
                </div>
              )}

              {assetTotal > GALLERY_PAGE_SIZE ? (
                <div className="mt-5">
                  <Pagination
                    page={assetPage}
                    totalPages={Math.max(1, Math.ceil(assetTotal / GALLERY_PAGE_SIZE))}
                    pageSize={GALLERY_PAGE_SIZE}
                    totalItems={assetTotal}
                    onPageChange={(page) => setAssetPage(page)}
                  />
                </div>
              ) : null}
            </CardContent>
          </Card>
        ) : null}
      </main>

      {toast ? (
        <div className="pointer-events-none fixed inset-x-0 bottom-5 z-50 flex justify-center px-4">
          <div className="pointer-events-auto inline-flex items-center gap-2 rounded-full border border-border/80 bg-card/95 px-4 py-2 text-xs font-medium shadow-xl backdrop-blur-md animate-image-studio-fade-in">
            <Sparkles className="size-3.5 text-primary" />
            <span>{toast}</span>
          </div>
        </div>
      ) : null}

      <PortalImagePreviewDialog
        asset={previewAsset}
        imageURL={previewAsset ? imageSrc(previewAsset, assetURLs) : undefined}
        prompt={previewPrompt}
        open={Boolean(previewAsset)}
        onClose={closePreview}
        onDownload={(asset) => void downloadAsset(asset)}
        onCopyPrompt={(text) => void copyPreviewPrompt(text)}
        onUsePrompt={usePromptInStudio}
        onDelete={(asset) => {
          closePreview()
          void deleteAsset(asset)
        }}
      />

      <Dialog
        open={galleryPickerOpen}
        onOpenChange={(next) => {
          if (!next && !galleryPickerConfirming) {
            setGalleryPickerOpen(false)
            setGalleryPickerSelected(new Set())
          }
        }}
      >
        <DialogContent className="flex max-h-[min(88dvh,720px)] w-[min(720px,calc(100vw-1.5rem))] max-w-none flex-col gap-0 overflow-hidden p-0 sm:p-0">
          <DialogHeader className="shrink-0 border-b border-border px-4 py-3 sm:px-5">
            <DialogTitle>{t('imageStudioPortal.pickFromGallery')}</DialogTitle>
            <DialogDescription>
              {t('imageStudioPortal.pickFromGalleryDesc', {
                remaining: Math.max(0, MAX_INPUT_IMAGES - inputImageDataURLs.length),
                max: MAX_INPUT_IMAGES,
              })}
            </DialogDescription>
          </DialogHeader>

          <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3 sm:px-5">
            {galleryPickerLoading ? (
              <div className="flex items-center justify-center py-16 text-muted-foreground">
                <Loader2 className="size-6 animate-spin" />
              </div>
            ) : assets.length === 0 ? (
              <div className="flex flex-col items-center gap-2 rounded-2xl border border-dashed border-border py-14 text-center">
                <Images className="size-8 text-muted-foreground/50" />
                <div className="text-sm text-muted-foreground">{t('imageStudioPortal.emptyGallery')}</div>
                <p className="max-w-xs text-xs text-muted-foreground">{t('imageStudioPortal.pickFromGalleryEmptyHint')}</p>
              </div>
            ) : (
              <div className="grid grid-cols-3 gap-2 sm:grid-cols-4">
                {assets.map((asset) => {
                  const src = thumbnailSrc(asset, assetURLs)
                  const selected = galleryPickerSelected.has(asset.id)
                  return (
                    <button
                      key={asset.id}
                      type="button"
                      onClick={() => toggleGalleryPickerAsset(asset.id)}
                      className={cn(
                        'group relative aspect-square overflow-hidden rounded-xl border text-left transition-all',
                        selected
                          ? 'border-primary ring-2 ring-primary/35'
                          : 'border-border/70 hover:border-primary/30',
                      )}
                    >
                      <div className="image-studio-checkerboard size-full">
                        {src ? (
                          <img loading="lazy" decoding="async" src={src} alt={asset.filename} className="size-full object-cover" />
                        ) : (
                          <div className="flex size-full items-center justify-center text-muted-foreground">
                            <Loader2 className="size-4 animate-spin" />
                          </div>
                        )}
                      </div>
                      <div
                        className={cn(
                          'absolute right-1.5 top-1.5 flex size-6 items-center justify-center rounded-full border shadow-sm transition-colors',
                          selected
                            ? 'border-primary bg-primary text-primary-foreground'
                            : 'border-white/40 bg-black/45 text-white/90',
                        )}
                      >
                        {selected ? <Check className="size-3.5" /> : null}
                      </div>
                      <div className="absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/65 to-transparent px-1.5 py-1">
                        <span className="truncate text-[10px] text-white/90">#{asset.id}</span>
                      </div>
                    </button>
                  )
                })}
              </div>
            )}
          </div>

          <div className="flex shrink-0 items-center justify-between gap-2 border-t border-border px-4 py-3 sm:px-5">
            <div className="text-xs text-muted-foreground">
              {t('imageStudioPortal.gallerySelectedCount', {
                count: galleryPickerSelected.size,
              })}
            </div>
            <div className="flex items-center gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={galleryPickerConfirming}
                onClick={() => {
                  setGalleryPickerOpen(false)
                  setGalleryPickerSelected(new Set())
                }}
              >
                {t('common.cancel')}
              </Button>
              <Button
                type="button"
                size="sm"
                disabled={galleryPickerConfirming || galleryPickerSelected.size === 0}
                onClick={() => void confirmGalleryPicker()}
              >
                {galleryPickerConfirming ? <Loader2 className="size-3.5 animate-spin" /> : <Check className="size-3.5" />}
                {t('imageStudioPortal.addSelected')}
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function PortalImagePreviewDialog({
  asset,
  imageURL,
  prompt,
  open,
  onClose,
  onDownload,
  onCopyPrompt,
  onUsePrompt,
  onDelete,
}: {
  asset: ImageAsset | null
  imageURL?: string
  prompt: string
  open: boolean
  onClose: () => void
  onDownload: (asset: ImageAsset) => void
  onCopyPrompt: (text: string) => void
  onUsePrompt: (text: string) => void
  onDelete: (asset: ImageAsset) => void
}) {
  const { t } = useTranslation()
  if (!asset) return null

  const resolution =
    asset.actual_size ||
    (asset.width > 0 && asset.height > 0 ? `${asset.width}x${asset.height}` : asset.requested_size || '-')
  const format = asset.output_format?.trim()
    ? asset.output_format.toUpperCase()
    : asset.mime_type?.replace(/^image\//i, '').toUpperCase() || '-'

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) onClose() }}>
      <DialogContent
        className="!flex !h-[calc(100dvh-0.75rem)] !w-[min(1280px,calc(100vw-0.75rem))] !max-w-none flex-col gap-0 overflow-hidden p-0"
        showCloseButton={false}
      >
        <DialogHeader className="sr-only">
          <DialogTitle>{t('images.previewTitle')}</DialogTitle>
          <DialogDescription>{asset.filename || `#${asset.id}`}</DialogDescription>
        </DialogHeader>
        <button
          type="button"
          onClick={onClose}
          className="absolute right-3.5 top-3.5 z-10 inline-flex size-9 items-center justify-center rounded-full border border-white/15 bg-black/55 text-white shadow-lg backdrop-blur-md transition hover:bg-black/80"
          aria-label={t('common.close')}
        >
          <X className="size-4" />
        </button>
        <div className="portal-lightbox-stage">
          {imageURL ? (
            <img
              key={imageURL}
              src={imageURL}
              alt={prompt || asset.filename}
              className="h-full max-h-full w-full max-w-full rounded-lg object-contain shadow-2xl"
            />
          ) : (
            <div className="flex h-full w-full items-center justify-center text-white/75">
              <PixelLoader mode="orbit" size="lg" className="text-white" />
            </div>
          )}
        </div>
        <div className="shrink-0 border-t border-border bg-card p-3.5 sm:p-4">
          {prompt ? (
            <p className="mb-3 line-clamp-2 text-[13px] leading-relaxed text-muted-foreground">{prompt}</p>
          ) : null}
          <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <div className="grid w-full grid-cols-2 gap-2 sm:w-auto sm:min-w-[330px] sm:grid-cols-3">
              <div className="min-w-0 rounded-xl border border-border/70 bg-muted/40 px-3 py-1.5">
                <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground/80">
                  {t('images.resolution')}
                </div>
                <div className="mt-0.5 truncate font-mono text-xs font-medium text-foreground">{resolution}</div>
              </div>
              <div className="min-w-0 rounded-xl border border-border/70 bg-muted/40 px-3 py-1.5">
                <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground/80">
                  {t('images.format')}
                </div>
                <div className="mt-0.5 truncate font-mono text-xs font-medium text-foreground">{format}</div>
              </div>
              <div className="min-w-0 rounded-xl border border-border/70 bg-muted/40 px-3 py-1.5">
                <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground/80">
                  {t('images.model')}
                </div>
                <div className="mt-0.5 truncate font-mono text-xs font-medium text-foreground">{asset.model || '-'}</div>
              </div>
            </div>
            <div className="flex flex-wrap items-center justify-end gap-1.5">
              <Button size="sm" onClick={() => onDownload(asset)}>
                <Download className="size-3.5" />
                {t('images.download')}
              </Button>
              <Button size="sm" variant="outline" onClick={() => onCopyPrompt(prompt)} disabled={!prompt.trim()}>
                <Copy className="size-3.5" />
                {t('images.copyPrompt')}
              </Button>
              <Button size="sm" variant="outline" onClick={() => onUsePrompt(prompt)} disabled={!prompt.trim()}>
                <Pencil className="size-3.5" />
                {t('imageStudioPortal.usePrompt')}
              </Button>
              <Button size="sm" variant="destructive" onClick={() => onDelete(asset)}>
                <Trash2 className="size-3.5" />
                {t('common.delete')}
              </Button>
            </div>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
