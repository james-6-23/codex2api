import type { ChangeEvent, DragEvent } from 'react'
import { useCallback, useEffect, useRef, useState, useMemo } from 'react'
import { api, getAdminKey } from '../api'
import Modal from '../components/Modal'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import StatusBadge from '../components/StatusBadge'
import ToastNotice from '../components/ToastNotice'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type { AccountRow, AddAccountRequest, AddATAccountRequest, APIKeyRow } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatCompactEmail } from '../lib/utils'
import { formatRelativeTime, formatBeijingTime } from '../utils/time'
import { Card, CardContent } from '@/components/ui/card'
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
import { Plus, RefreshCw, Trash2, Zap, FlaskConical, Ban, Timer, AlertTriangle, Upload, Download, ArrowDownToLine, KeyRound, ExternalLink, FileText, FileJson, BarChart3, Search, Fingerprint, FolderOpen, Lock, Unlock, RotateCcw, Pencil, Check, ChevronDown, Copy, Power, PowerOff } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import AccountUsageModal from '../components/AccountUsageModal'

export default function Accounts() {
  const { t } = useTranslation()
  const pageSizeOptions = [10, 20, 50, 100]
  const [showAdd, setShowAdd] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [statusFilter, setStatusFilter] = useState<'all' | 'normal' | 'rate_limited' | 'banned' | 'error' | 'disabled' | 'locked'>('all')
  const [searchQuery, setSearchQuery] = useState('')
  const [planFilter, setPlanFilter] = useState<'all' | 'pro' | 'plus' | 'team' | 'free'>('all')
  const [sortKey, setSortKey] = useState<'requests' | 'usage' | 'importTime' | null>(null)
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc')
  const [addForm, setAddForm] = useState<AddAccountRequest>({
    refresh_token: '',
    proxy_url: '',
  })
  const [submitting, setSubmitting] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [refreshingIds, setRefreshingIds] = useState<Set<number>>(new Set())
  const [authJsonExportingIds, setAuthJsonExportingIds] = useState<Set<number>>(new Set())
  const [authJsonModal, setAuthJsonModal] = useState<{ account: AccountRow; json: string } | null>(null)
  const [batchLoading, setBatchLoading] = useState(false)
  const [batchTesting, setBatchTesting] = useState(false)
  const [cleaningBanned, setCleaningBanned] = useState(false)
  const [cleaningRateLimited, setCleaningRateLimited] = useState(false)
  const [cleaningError, setCleaningError] = useState(false)
  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null)
  const [usageAccount, setUsageAccount] = useState<AccountRow | null>(null)
  const [editingAccount, setEditingAccount] = useState<AccountRow | null>(null)
  const [editSubmitting, setEditSubmitting] = useState(false)
  const [scoreMode, setScoreMode] = useState<'default' | 'custom'>('default')
  const [scoreInput, setScoreInput] = useState('')
  const [concurrencyMode, setConcurrencyMode] = useState<'default' | 'custom'>('default')
  const [concurrencyInput, setConcurrencyInput] = useState('')
  const [allowedAPIKeySelection, setAllowedAPIKeySelection] = useState<number[]>([])
  const [importing, setImporting] = useState(false)
  const [showImportPicker, setShowImportPicker] = useState(false)
  const [dragging, setDragging] = useState(false)
  const dragCounter = useRef(0)
  const [showExportPicker, setShowExportPicker] = useState(false)
  const [exporting, setExporting] = useState(false)
  const [showMigrate, setShowMigrate] = useState(false)
  const [migrateUrl, setMigrateUrl] = useState('')
  const [migrateKey, setMigrateKey] = useState('')
  const [migrating, setMigrating] = useState(false)
  const [importProgress, setImportProgress] = useState<{ show: boolean; current: number; total: number; success: number; duplicate: number; failed: number; done: boolean }>({ show: false, current: 0, total: 0, success: 0, duplicate: 0, failed: 0, done: false })
  const [addMethod, setAddMethod] = useState<'rt' | 'at' | 'oauth'>('rt')
  const [atForm, setAtForm] = useState<AddATAccountRequest>({
    access_token: '',
    proxy_url: '',
  })
  const [oauthStep, setOauthStep] = useState<'generate' | 'exchange'>('generate')
  const [oauthSession, setOauthSession] = useState<{ session_id: string; auth_url: string } | null>(null)
  const [oauthProxyUrl, setOauthProxyUrl] = useState('')
  const [oauthCallbackUrl, setOauthCallbackUrl] = useState('')
  const [oauthName, setOauthName] = useState('')
  const [oauthGenerating, setOauthGenerating] = useState(false)
  const [oauthCompleting, setOauthCompleting] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const jsonInputRef = useRef<HTMLInputElement>(null)
  const atFileInputRef = useRef<HTMLInputElement>(null)
  const folderInputRef = useRef<HTMLInputElement>(null)
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()

  const loadAccounts = useCallback(async () => {
    const [accountsResponse, apiKeysResponse] = await Promise.all([api.getAccounts(), api.getAPIKeys()])
    return {
      accounts: accountsResponse.accounts ?? [],
      apiKeys: apiKeysResponse.keys ?? [],
    }
  }, [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{ accounts: AccountRow[]; apiKeys: APIKeyRow[] }>({
    initialData: {
      accounts: [],
      apiKeys: [],
    },
    load: loadAccounts,
  })
  const accounts = data.accounts
  const apiKeys = data.apiKeys
  const usageReloadAttemptsRef = useRef<Map<number, number>>(new Map())

  useEffect(() => {
    const needsUsageReload = (account: AccountRow) => {
      if (account.status !== 'active' && account.status !== 'ready') {
        return false
      }

      const plan = (account.plan_type || '').toLowerCase()
      const has7d = account.usage_percent_7d !== null && account.usage_percent_7d !== undefined
      const has5h = account.usage_percent_5h !== null && account.usage_percent_5h !== undefined

      if (plan === 'free') {
        return !has7d
      }
      if (plan === 'pro' || plan === 'team' || plan === 'plus' || plan === 'teamplus') {
        return !has5h || !has7d
      }
      return !has7d
    }

    const missingUsageIds = accounts.filter(needsUsageReload).map((account) => account.id)
    const missingUsageIdSet = new Set(missingUsageIds)
    for (const id of Array.from(usageReloadAttemptsRef.current.keys())) {
      if (!missingUsageIdSet.has(id)) {
        usageReloadAttemptsRef.current.delete(id)
      }
    }

    const retryIds = missingUsageIds.filter((id) => (usageReloadAttemptsRef.current.get(id) ?? 0) < 6)
    if (retryIds.length === 0) {
      return
    }

    for (const id of retryIds) {
      usageReloadAttemptsRef.current.set(id, (usageReloadAttemptsRef.current.get(id) ?? 0) + 1)
    }

    const timer = window.setTimeout(() => {
      void reloadSilently()
    }, 2500)

    return () => window.clearTimeout(timer)
  }, [accounts, reloadSilently])

  const totalAccounts = accounts.length
  const normalAccounts = accounts.filter((account) => account.status === 'active' || account.status === 'ready').length
  const rateLimitedAccounts = accounts.filter((account) => account.status === 'rate_limited' || account.status === 'usage_exhausted').length
  const bannedAccounts = accounts.filter((account) => account.status === 'unauthorized').length
  const errorAccounts = accounts.filter((account) => account.status === 'error').length
  const disabledAccounts = accounts.filter((account) => account.enabled === false).length
  const lockedAccounts = accounts.filter((account) => account.locked).length
  const healthyAccounts = accounts.filter((account) => account.health_tier === 'healthy').length
  const warmAccounts = accounts.filter((account) => account.health_tier === 'warm').length
  const riskyAccounts = accounts.filter((account) => account.health_tier === 'risky').length

  const filteredAccounts = accounts.filter((account) => {
    // 状态过滤
    switch (statusFilter) {
      case 'normal':
        if (account.status !== 'active' && account.status !== 'ready') return false
        break
      case 'rate_limited':
        if (account.status !== 'rate_limited' && account.status !== 'usage_exhausted') return false
        break
      case 'banned':
        if (account.status !== 'unauthorized') return false
        break
      case 'error':
        if (account.status !== 'error') return false
        break
      case 'disabled':
        if (account.enabled !== false) return false
        break
      case 'locked':
        if (!account.locked) return false
        break
    }
    // 套餐过滤
    if (planFilter !== 'all') {
      const plan = (account.plan_type || '').toLowerCase()
      if (plan !== planFilter) return false
    }
    // 搜索过滤
    if (searchQuery) {
      const q = searchQuery.toLowerCase()
      const email = (account.email || '').toLowerCase()
      const name = (account.name || '').toLowerCase()
      if (!email.includes(q) && !name.includes(q)) return false
    }
    return true
  })

  const sortedAccounts = [...filteredAccounts].sort((a, b) => {
    if (!sortKey) return 0
    let diff = 0
    if (sortKey === 'requests') {
      diff = ((a.success_requests ?? 0) + (a.error_requests ?? 0)) - ((b.success_requests ?? 0) + (b.error_requests ?? 0))
    } else if (sortKey === 'usage') {
      diff = (a.usage_percent_7d ?? -1) - (b.usage_percent_7d ?? -1)
    } else if (sortKey === 'importTime') {
      diff = new Date(a.created_at || 0).getTime() - new Date(b.created_at || 0).getTime()
    }
    return sortDir === 'asc' ? diff : -diff
  })

  const totalPages = Math.max(1, Math.ceil(sortedAccounts.length / pageSize))
  const currentPage = Math.min(page, totalPages)
  const pagedAccounts = sortedAccounts.slice((currentPage - 1) * pageSize, currentPage * pageSize)
  const allPageSelected = pagedAccounts.length > 0 && pagedAccounts.every((a) => selected.has(a.id))

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages)
    }
  }, [page, totalPages])

  useEffect(() => {
    if (!accounts.some((account) => account.status === 'refreshing')) {
      return
    }

    const timer = window.setTimeout(() => {
      void reloadSilently()
    }, 2000)

    return () => window.clearTimeout(timer)
  }, [accounts, reloadSilently])

  const toggleSelect = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const toggleSelectAll = () => {
    if (allPageSelected) {
      setSelected((prev) => {
        const next = new Set(prev)
        for (const a of pagedAccounts) next.delete(a.id)
        return next
      })
    } else {
      setSelected((prev) => {
        const next = new Set(prev)
        for (const a of pagedAccounts) next.add(a.id)
        return next
      })
    }
  }

  const handleAdd = async () => {
    if (!addForm.refresh_token.trim()) return
    setSubmitting(true)
    try {
      await api.addAccount(addForm)
      showToast(t('accounts.addSuccess'))
      setShowAdd(false)
      setAddForm({ refresh_token: '', proxy_url: '' })
      void reload()
    } catch (error) {
      showToast(t('accounts.addFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setSubmitting(false)
    }
  }

  const handleAddAT = async () => {
    if (!atForm.access_token.trim()) return
    setSubmitting(true)
    try {
      await api.addATAccount(atForm)
      showToast(t('accounts.addSuccess'))
      setShowAdd(false)
      setAtForm({ access_token: '', proxy_url: '' })
      void reload()
    } catch (error) {
      showToast(t('accounts.addFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setSubmitting(false)
    }
  }

  const handleOAuthGenerate = async () => {
    setOauthGenerating(true)
    try {
      const result = await api.generateOAuthURL({ proxy_url: oauthProxyUrl })
      setOauthSession(result)
      setOauthStep('exchange')
    } catch (error) {
      showToast(t('accounts.oauthFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setOauthGenerating(false)
    }
  }

  const handleOAuthComplete = async () => {
    if (!oauthSession) return
    let code = ''
    let state = ''
    const raw = oauthCallbackUrl.trim()
    try {
      const url = new URL(raw)
      code = url.searchParams.get('code') ?? ''
      state = url.searchParams.get('state') ?? ''
    } catch {
      const qs = raw.includes('?') ? raw.split('?')[1] : raw
      const params = new URLSearchParams(qs)
      code = params.get('code') ?? ''
      state = params.get('state') ?? ''
    }
    if (!code || !state) {
      showToast(t('accounts.oauthParseError'), 'error')
      return
    }
    setOauthCompleting(true)
    try {
      const result = await api.exchangeOAuthCode({
        session_id: oauthSession.session_id,
        code,
        state,
        name: oauthName.trim() || undefined,
        proxy_url: oauthProxyUrl.trim() || undefined,
      })
      showToast(result.email ? t('accounts.oauthSuccess', { email: result.email }) : t('accounts.oauthSuccessNoEmail'))
      setShowAdd(false)
      setAddMethod('rt')
      setOauthStep('generate')
      setOauthSession(null)
      setOauthCallbackUrl('')
      setOauthName('')
      void reload()
    } catch (error) {
      showToast(t('accounts.oauthFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setOauthCompleting(false)
    }
  }

  const readImportSSE = async (res: Response) => {
    setImportProgress({ show: true, current: 0, total: 0, success: 0, duplicate: 0, failed: 0, done: false })
    const reader = res.body?.getReader()
    if (!reader) return
    const decoder = new TextDecoder()
    let buffer = ''
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      const lines = buffer.split('\n')
      buffer = lines.pop() ?? ''
      for (const line of lines) {
        if (!line.startsWith('data: ')) continue
        try {
          const event = JSON.parse(line.slice(6)) as { type: string; current: number; total: number; success: number; duplicate: number; failed: number }
          setImportProgress(p => ({ ...p, current: event.current, total: event.total, success: event.success, duplicate: event.duplicate, failed: event.failed, done: event.type === 'complete' }))
          if (event.type === 'complete') void reload()
        } catch { /* 忽略解析异常 */ }
      }
    }
  }

  const importFiles = async (files: File[], format: 'txt' | 'json' | 'at_txt') => {
    setImporting(true)
    try {
      const formData = new FormData()
      if (format !== 'txt') formData.append('format', format)
      for (const f of files) formData.append('file', f)
      const res = await fetch('/api/admin/accounts/import', { method: 'POST', body: formData, headers: getAdminKey() ? { 'X-Admin-Key': getAdminKey() } : {} })
      if (res.headers.get('content-type')?.includes('text/event-stream')) {
        await readImportSSE(res)
      } else {
        const data = await res.json()
        if (!res.ok) {
          showToast(data.error ? t('accounts.importFailedWithReason', { error: data.error }) : t('accounts.importFailed'), 'error')
        } else {
          showToast(t('accounts.importCompleted'))
          void reload()
        }
      }
    } catch (error) {
      showToast(t('accounts.importFailedWithReason', { error: getErrorMessage(error) }), 'error')
    } finally {
      setImporting(false)
    }
  }

  const handleDragEnter = (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragCounter.current++
    if (dragCounter.current === 1) setDragging(true)
  }

  const handleDragOver = (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
  }

  const handleDragLeave = (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragCounter.current--
    if (dragCounter.current === 0) setDragging(false)
  }

  const readAllEntriesFromDirectory = (dirEntry: FileSystemDirectoryEntry): Promise<File[]> => {
    return new Promise((resolve) => {
      const files: File[] = []
      const readEntries = (reader: FileSystemDirectoryReader) => {
        reader.readEntries(async (entries) => {
          if (entries.length === 0) { resolve(files); return }
          for (const entry of entries) {
            if (entry.isFile) {
              const file = await new Promise<File>((res) => (entry as FileSystemFileEntry).file(res))
              files.push(file)
            } else if (entry.isDirectory) {
              const subFiles = await readAllEntriesFromDirectory(entry as FileSystemDirectoryEntry)
              files.push(...subFiles)
            }
          }
          readEntries(reader)
        })
      }
      readEntries(dirEntry.createReader())
    })
  }

  const handleDrop = async (e: DragEvent) => {
    e.preventDefault()
    e.stopPropagation()
    dragCounter.current = 0
    setDragging(false)
    if (importing) return

    // 检测是否拖入了文件夹
    const items = e.dataTransfer.items
    const hasDirectories = items && Array.from(items).some(
      item => item.webkitGetAsEntry?.()?.isDirectory
    )

    if (hasDirectories) {
      const allFiles: File[] = []
      for (const item of Array.from(items)) {
        const entry = item.webkitGetAsEntry?.()
        if (!entry) continue
        if (entry.isDirectory) {
          const dirFiles = await readAllEntriesFromDirectory(entry as FileSystemDirectoryEntry)
          allFiles.push(...dirFiles)
        } else if (entry.isFile) {
          const file = await new Promise<File>((res) => (entry as FileSystemFileEntry).file(res))
          allFiles.push(file)
        }
      }

      const validFiles = allFiles.filter(f => {
        const ext = f.name.split('.').pop()?.toLowerCase()
        return (ext === 'txt' || ext === 'json') && f.size > 0
      })

      if (validFiles.length === 0) {
        showToast(t('accounts.folderNoValidFiles'), 'error')
        return
      }

      const txtFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'txt')
      const jsonFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'json')

      if (jsonFiles.length > 0) {
        await importFiles(jsonFiles, 'json')
      }
      if (txtFiles.length > 0) {
        await importFiles(txtFiles, 'txt')
      }
      return
    }

    // 原有的文件拖放逻辑
    const files = Array.from(e.dataTransfer.files).filter(f => f.size > 0)
    if (files.length === 0) return

    const txtFiles: File[] = []
    const jsonFiles: File[] = []
    for (const f of files) {
      const ext = f.name.split('.').pop()?.toLowerCase()
      if (ext === 'txt') txtFiles.push(f)
      else if (ext === 'json') jsonFiles.push(f)
      else {
        showToast(t('accounts.unsupportedFileType', { name: f.name }), 'error')
        return
      }
    }

    if (jsonFiles.length > 0) {
      await importFiles(jsonFiles, 'json')
    }
    if (txtFiles.length > 0) {
      await importFiles(txtFiles, 'txt')
    }
  }

  const handleFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? [])
    if (files.length === 0) return
    if (files.some((file) => !file.name.toLowerCase().endsWith('.txt'))) {
      showToast(t('accounts.selectTxtFile'), 'error')
      return
    }
    setShowImportPicker(false)
    await importFiles(files, 'txt')
    if (fileInputRef.current) fileInputRef.current.value = ''
  }

  const handleJsonImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files
    if (!files || files.length === 0) return
    setShowImportPicker(false)
    await importFiles(Array.from(files), 'json')
    if (jsonInputRef.current) jsonInputRef.current.value = ''
  }

  const handleAtFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? [])
    if (files.length === 0) return
    if (files.some((file) => !file.name.toLowerCase().endsWith('.txt'))) {
      showToast(t('accounts.selectTxtFile'), 'error')
      return
    }
    setShowImportPicker(false)
    await importFiles(files, 'at_txt')
    if (atFileInputRef.current) atFileInputRef.current.value = ''
  }

  const handleFolderImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files
    if (!files || files.length === 0) return
    setShowImportPicker(false)

    const validFiles = Array.from(files).filter(f => {
      const ext = f.name.split('.').pop()?.toLowerCase()
      return (ext === 'txt' || ext === 'json') && f.size > 0
    })

    if (validFiles.length === 0) {
      showToast(t('accounts.folderNoValidFiles'), 'error')
      if (folderInputRef.current) folderInputRef.current.value = ''
      return
    }

    const txtFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'txt')
    const jsonFiles = validFiles.filter(f => f.name.split('.').pop()?.toLowerCase() === 'json')

    if (jsonFiles.length > 0) {
      await importFiles(jsonFiles, 'json')
    }
    if (txtFiles.length > 0) {
      await importFiles(txtFiles, 'txt')
    }

    if (folderInputRef.current) folderInputRef.current.value = ''
  }

  const handleExport = async (format: 'json' | 'txt', scope: 'healthy' | 'selected') => {
    setExporting(true)
    setShowExportPicker(false)
    try {
      const params: { filter: 'healthy' | 'all'; ids?: number[] } = {
        filter: scope === 'healthy' ? 'healthy' : 'all',
      }
      if (scope === 'selected') {
        params.ids = Array.from(selected)
        params.filter = 'all'
      }
      const data = await api.exportAccounts(params)
      if (data.length === 0) {
        showToast(t('accounts.exportNoAccounts'), 'error')
        return
      }
      const ts = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19)
      if (format === 'json') {
        const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
        downloadBlob(blob, `cpa-${ts}-${data.length}.json`)
      } else {
        const text = data.map(e => e.refresh_token).join('\n')
        const blob = new Blob([text], { type: 'text/plain' })
        downloadBlob(blob, `rt-${ts}-${data.length}.txt`)
      }
      showToast(t('accounts.exportSuccess', { count: data.length }))
    } catch (error) {
      showToast(`${t('accounts.exportFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setExporting(false)
    }
  }

  const handleGenerateAuthJSON = async (account: AccountRow) => {
    setAuthJsonExportingIds((prev) => new Set(prev).add(account.id))
    try {
      const blob = await api.downloadAccountAuthJSON(account.id)
      const json = formatJSONText(await blob.text())
      setAuthJsonModal({ account, json })
      showToast(t('accounts.authJsonGenerated'))
    } catch (error) {
      showToast(t('accounts.authJsonFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setAuthJsonExportingIds((prev) => {
        const next = new Set(prev)
        next.delete(account.id)
        return next
      })
    }
  }

  const handleCopyAuthJSON = async () => {
    if (!authJsonModal) return
    try {
      await copyTextToClipboard(authJsonModal.json)
      showToast(t('accounts.authJsonCopied'))
    } catch (error) {
      showToast(t('accounts.authJsonCopyFailed', { error: getErrorMessage(error) }), 'error')
    }
  }

  const handleExportAuthJSON = () => {
    if (!authJsonModal) return
    const blob = new Blob([`${authJsonModal.json}\n`], { type: 'application/json' })
    downloadBlob(blob, 'auth.json')
    showToast(t('accounts.authJsonExported'))
  }

  const handleMigrate = async () => {
    setMigrating(true)
    setShowMigrate(false)
    try {
      const res = await fetch('/api/admin/accounts/migrate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...(getAdminKey() ? { 'X-Admin-Key': getAdminKey() } : {}) },
        body: JSON.stringify({ url: migrateUrl.trim(), admin_key: migrateKey.trim() }),
      })
      if (res.headers.get('content-type')?.includes('text/event-stream')) {
        await readImportSSE(res)
      } else {
        const data = await res.json()
        if (!res.ok) {
          showToast(data.error ? `${t('accounts.migrateFailed')}: ${data.error}` : t('accounts.migrateFailed'), 'error')
        } else {
          showToast(t('accounts.migrateSuccess', { imported: data.imported ?? 0, duplicate: data.duplicate ?? 0, failed: data.failed ?? 0 }))
          void reload()
        }
      }
    } catch (error) {
      showToast(`${t('accounts.migrateFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setMigrating(false)
      setMigrateUrl('')
      setMigrateKey('')
    }
  }

  const handleDelete = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t('accounts.deleteTitle'),
      description: t('accounts.deleteDesc', { account: account.email || `ID ${account.id}` }),
      confirmText: t('accounts.deleteConfirm'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!confirmed) return
    try {
      await api.deleteAccount(account.id)
      showToast(t('accounts.deleted'))
      void reload()
    } catch (error) {
      showToast(t('accounts.deleteFailed', { error: getErrorMessage(error) }), 'error')
    }
  }

  const handleRefresh = async (account: AccountRow) => {
    setRefreshingIds((prev) => new Set(prev).add(account.id))
    try {
      const result = await api.refreshAccount(account.id)
      showToast(result.message || t('accounts.refreshRequested'))
      void reloadSilently()
    } catch (error) {
      showToast(t('accounts.refreshFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setRefreshingIds((prev) => {
        const next = new Set(prev)
        next.delete(account.id)
        return next
      })
    }
  }

  const handleToggleLock = async (account: AccountRow) => {
    const newLocked = !account.locked
    try {
      await api.toggleAccountLock(account.id, newLocked)
      showToast(newLocked ? t('accounts.lockSuccess') : t('accounts.unlockSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.lockFailed', { error: getErrorMessage(error) }), 'error')
    }
  }

  const handleToggleEnabled = async (account: AccountRow) => {
    const nextEnabled = account.enabled === false
    try {
      await api.toggleAccountEnabled(account.id, nextEnabled)
      showToast(nextEnabled ? t('accounts.enableSuccess') : t('accounts.disableSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.enableFailed', { error: getErrorMessage(error) }), 'error')
    }
  }

  const handleBatchDelete = async () => {
    if (selected.size === 0) return
    const confirmed = await confirm({
      title: t('accounts.batchDeleteTitle'),
      description: t('accounts.batchDeleteDesc', { count: selected.size }),
      confirmText: t('accounts.deleteConfirm'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!confirmed) return
    setBatchLoading(true)
    let success = 0
    let fail = 0
    for (const id of selected) {
      try {
        await api.deleteAccount(id)
        success++
      } catch {
        fail++
      }
    }
    showToast(t('accounts.batchDeleteDone', { success, fail }))
    setSelected(new Set())
    setBatchLoading(false)
    void reload()
  }

  const handleBatchRefresh = async () => {
    if (selected.size === 0) return
    setBatchLoading(true)
    let success = 0
    let fail = 0
    for (const id of selected) {
      try {
        await api.refreshAccount(id)
        success++
      } catch {
        fail++
      }
    }
    showToast(t('accounts.batchRefreshDone', { success, fail }))
    setBatchLoading(false)
    void reload()
  }

  const handleBatchLock = async (locked: boolean) => {
    if (selected.size === 0) return
    setBatchLoading(true)
    let success = 0
    let fail = 0
    for (const id of selected) {
      try {
        await api.toggleAccountLock(id, locked)
        success++
      } catch {
        fail++
      }
    }
    showToast(t(locked ? 'accounts.batchLockDone' : 'accounts.batchUnlockDone', { success, fail }))
    setBatchLoading(false)
    setSelected(new Set())
    void reload()
  }

  const handleBatchEnabled = async (enabled: boolean) => {
    if (selected.size === 0) return
    setBatchLoading(true)
    let success = 0
    let fail = 0
    try {
      for (const id of selected) {
        try {
          await api.toggleAccountEnabled(id, enabled)
          success++
        } catch {
          fail++
        }
      }
      showToast(t(enabled ? 'accounts.batchEnableDone' : 'accounts.batchDisableDone', { success, fail }))
      setSelected(new Set())
      void reload()
    } finally {
      setBatchLoading(false)
    }
  }

  const handleResetStatus = async (account: AccountRow) => {
    try {
      await api.resetAccountStatus(account.id)
      showToast(t('accounts.resetStatusSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.resetStatusFailed', { error: getErrorMessage(error) }), 'error')
    }
  }

  const handleBatchResetStatus = async () => {
    if (selected.size === 0) return
    setBatchLoading(true)
    try {
      const result = await api.batchResetStatus(Array.from(selected))
      showToast(t('accounts.batchResetStatusDone', { success: result.success, fail: result.failed }))
      setSelected(new Set())
      void reload()
    } catch (error) {
      showToast(t('accounts.resetStatusFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setBatchLoading(false)
    }
  }

  const handleBatchTest = async () => {
    setBatchTesting(true)
    try {
      const result = await api.batchTestAccounts()
      showToast(t('accounts.batchTestDone', {
        success: result.success,
        banned: result.banned,
        rateLimited: result.rate_limited,
        failed: result.failed,
      }))
      void reload()
    } catch (error) {
      showToast(t('accounts.batchTestFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setBatchTesting(false)
    }
  }

  const handleCleanBanned = async () => {
    const confirmed = await confirm({
      title: t('accounts.cleanBannedTitle'),
      description: t('accounts.cleanBannedDesc'),
      confirmText: t('accounts.cleanConfirm'),
      tone: 'warning',
    })
    if (!confirmed) return
    setCleaningBanned(true)
    try {
      await api.cleanBanned()
      showToast(t('accounts.cleanBannedSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.cleanBannedFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setCleaningBanned(false)
    }
  }

  const handleCleanRateLimited = async () => {
    const confirmed = await confirm({
      title: t('accounts.cleanRateLimitedTitle'),
      description: t('accounts.cleanRateLimitedDesc'),
      confirmText: t('accounts.cleanConfirm'),
      tone: 'warning',
    })
    if (!confirmed) return
    setCleaningRateLimited(true)
    try {
      await api.cleanRateLimited()
      showToast(t('accounts.cleanRateLimitedSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.cleanRateLimitedFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setCleaningRateLimited(false)
    }
  }

  const handleCleanError = async () => {
    const confirmed = await confirm({
      title: t('accounts.cleanErrorTitle'),
      description: t('accounts.cleanErrorDesc'),
      confirmText: t('accounts.cleanConfirm'),
      tone: 'warning',
    })
    if (!confirmed) return
    setCleaningError(true)
    try {
      await api.cleanError()
      showToast(t('accounts.cleanErrorSuccess'))
      void reload()
    } catch (error) {
      showToast(t('accounts.cleanErrorFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setCleaningError(false)
    }
  }

  const openSchedulerEditor = (account: AccountRow) => {
    setEditingAccount(account)
    setScoreMode(account.score_bias_override === null || account.score_bias_override === undefined ? 'default' : 'custom')
    setScoreInput(account.score_bias_override === null || account.score_bias_override === undefined ? '' : String(account.score_bias_override))
    setConcurrencyMode(account.base_concurrency_override === null || account.base_concurrency_override === undefined ? 'default' : 'custom')
    setConcurrencyInput(account.base_concurrency_override === null || account.base_concurrency_override === undefined ? '' : String(account.base_concurrency_override))
    setAllowedAPIKeySelection(filterExistingAPIKeyIDs(account.allowed_api_key_ids ?? [], apiKeys))
  }

  const closeSchedulerEditor = (force = false) => {
    if (editSubmitting && !force) return
    setEditingAccount(null)
    setScoreMode('default')
    setScoreInput('')
    setConcurrencyMode('default')
    setConcurrencyInput('')
    setAllowedAPIKeySelection([])
  }

  const parsedScoreBias = scoreMode === 'custom' ? parseIntegerInput(scoreInput) : null
  const parsedBaseConcurrency = concurrencyMode === 'custom' ? parseIntegerInput(concurrencyInput) : null
  const scoreInputInvalid = scoreMode === 'custom' && (parsedScoreBias === null || parsedScoreBias < -200 || parsedScoreBias > 200)
  const concurrencyInputInvalid = concurrencyMode === 'custom' && (parsedBaseConcurrency === null || parsedBaseConcurrency < 1 || parsedBaseConcurrency > 50)

  const editPreview = useMemo(() => {
    if (!editingAccount) return null

    const rawScore = Math.round(editingAccount.scheduler_score ?? 0)
    const appliedBias = scoreMode === 'custom'
      ? (parsedScoreBias ?? getEffectiveScoreBias(editingAccount))
      : getDefaultScoreBias(editingAccount.plan_type)
    const baseConcurrency = concurrencyMode === 'custom'
      ? (parsedBaseConcurrency ?? getEffectiveBaseConcurrency(editingAccount))
      : getEffectiveBaseConcurrency(editingAccount)

    return {
      rawScore,
      dispatchScore: computePreviewDispatchScore(editingAccount, rawScore, appliedBias),
      healthTier: editingAccount.health_tier,
      dynamicConcurrency: computePreviewDynamicConcurrency(editingAccount, baseConcurrency),
      appliedBias,
      baseConcurrency,
    }
  }, [editingAccount, scoreMode, parsedScoreBias, concurrencyMode, parsedBaseConcurrency])

  const handleSaveScheduler = async () => {
    if (!editingAccount) return
    if (scoreInputInvalid || concurrencyInputInvalid) {
      showToast(t('accounts.schedulerInvalidInput'), 'error')
      return
    }

    setEditSubmitting(true)
    try {
      const payload = {
        score_bias_override: scoreMode === 'custom' ? parsedScoreBias : null,
        base_concurrency_override: concurrencyMode === 'custom' ? parsedBaseConcurrency : null,
        allowed_api_key_ids: allowedAPIKeySelection,
      }
      await api.updateAccountScheduler(editingAccount.id, payload)
      showToast(t('accounts.schedulerSaveSuccess'))
      await reload()
      closeSchedulerEditor(true)
    } catch (error) {
      showToast(t('accounts.schedulerSaveFailed', { error: getErrorMessage(error) }), 'error')
    } finally {
      setEditSubmitting(false)
    }
  }

  return (
    <div
      className="relative"
      onDragEnter={handleDragEnter}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={(e) => void handleDrop(e)}
    >
      {dragging && (
        <div className="pointer-events-none absolute inset-0 z-50 flex items-center justify-center rounded-lg border-2 border-dashed border-primary bg-primary/5 backdrop-blur-sm">
          <div className="flex flex-col items-center gap-2 text-primary">
            <Upload className="size-10" />
            <span className="text-lg font-semibold">{t('accounts.dropToImport')}</span>
            <span className="text-sm text-muted-foreground">{t('accounts.dropHint')}</span>
          </div>
        </div>
      )}
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('accounts.loadingTitle')}
      loadingDescription={t('accounts.loadingDesc')}
      errorTitle={t('accounts.errorTitle')}
    >
      <>
        <PageHeader
          title={t('accounts.title')}
          description={t('accounts.description')}
          onRefresh={() => void reload()}
          actions={(
            <div className="flex flex-wrap items-center justify-end gap-1.5">
              <Button variant="outline" size="sm" disabled={batchTesting} onClick={() => void handleBatchTest()}>
                <FlaskConical className="size-3" />
                {batchTesting ? t('accounts.batchTesting') : t('accounts.batchTest')}
              </Button>
              <Button variant="outline" size="sm" disabled={cleaningBanned} onClick={() => void handleCleanBanned()}>
                <Ban className="size-3" />
                {cleaningBanned ? t('accounts.cleaning') : t('accounts.cleanBanned')}
              </Button>
              <Button variant="outline" size="sm" disabled={cleaningRateLimited} onClick={() => void handleCleanRateLimited()}>
                <Timer className="size-3" />
                {cleaningRateLimited ? t('accounts.cleaning') : t('accounts.cleanRateLimited')}
              </Button>
              <Button variant="outline" size="sm" disabled={cleaningError} onClick={() => void handleCleanError()}>
                <AlertTriangle className="size-3" />
                {cleaningError ? t('accounts.cleaning') : t('accounts.cleanError')}
              </Button>
              <Button onClick={() => setShowAdd(true)}>
                <Plus className="size-3.5" />
                {t('accounts.addAccount')}
              </Button>
              <Button variant="outline" disabled={importing} onClick={() => setShowImportPicker(true)}>
                <Upload className="size-3.5" />
                {importing ? t('accounts.importing') : t('accounts.importFile')}
              </Button>
              <Button variant="outline" disabled={exporting} onClick={() => setShowExportPicker(true)}>
                <Download className="size-3.5" />
                {exporting ? t('accounts.exporting') : t('accounts.export')}
              </Button>
              <Button variant="outline" disabled={migrating} onClick={() => setShowMigrate(true)}>
                <ArrowDownToLine className="size-3.5" />
                {migrating ? t('accounts.migrating') : t('accounts.migrateImport')}
              </Button>
              <input
                ref={fileInputRef}
                type="file"
                accept=".txt"
                multiple
                className="hidden"
                onChange={(e) => void handleFileImport(e)}
              />
              <input
                ref={jsonInputRef}
                type="file"
                accept=".json"
                multiple
                className="hidden"
                onChange={(e) => void handleJsonImport(e)}
              />
              <input
                ref={atFileInputRef}
                type="file"
                accept=".txt"
                multiple
                className="hidden"
                onChange={(e) => void handleAtFileImport(e)}
              />
              <input
                ref={folderInputRef}
                type="file"
                className="hidden"
                onChange={(e) => void handleFolderImport(e)}
                {...({ webkitdirectory: '', directory: '' } as React.InputHTMLAttributes<HTMLInputElement>)}
              />
            </div>
          )}
        />

        <div className="mb-4 grid grid-cols-2 gap-3 xl:grid-cols-5">
          <CompactStat label={t('accounts.totalAccounts')} chipLabel={t('accounts.filterAll')} value={totalAccounts} tone="neutral" />
          <CompactStat label={t('accounts.normalAccounts')} chipLabel={t('accounts.filterNormal')} value={normalAccounts} tone="success" />
          <CompactStat label={t('accounts.rateLimited')} chipLabel={t('accounts.filterRateLimited')} value={rateLimitedAccounts} tone="warning" />
          <CompactStat label={t('accounts.bannedAccounts')} chipLabel={t('accounts.filterBanned')} value={bannedAccounts} tone="danger" />
          <CompactStat label={t('accounts.errorAccounts')} chipLabel={t('accounts.filterError')} value={errorAccounts} tone="danger" />
        </div>

        <div className="toolbar-surface mb-3 flex flex-wrap items-center gap-2">
          <span className="font-semibold text-foreground">{t('accounts.filter')}</span>
          {([['all', t('accounts.filterAll')], ['normal', t('accounts.filterNormal')], ['rate_limited', t('accounts.filterRateLimited')], ['banned', t('accounts.filterBanned')], ['error', t('accounts.filterError')], ['disabled', t('accounts.filterDisabled')], ['locked', t('accounts.filterLocked')]] as const).map(([key, label]) => (
            <button
              key={key}
              onClick={() => { setStatusFilter(key); setPage(1) }}
              className={`rounded-md px-2.5 py-1 font-semibold transition-colors ${
                statusFilter === key
                  ? 'bg-primary text-primary-foreground'
                  : 'bg-muted/50 text-muted-foreground hover:bg-muted'
              }`}
            >
              {label} {key === 'all' ? totalAccounts : key === 'normal' ? normalAccounts : key === 'rate_limited' ? rateLimitedAccounts : key === 'banned' ? bannedAccounts : key === 'error' ? errorAccounts : key === 'disabled' ? disabledAccounts : lockedAccounts}
            </button>
          ))}
        </div>

        <div className="toolbar-surface mb-3 flex flex-wrap items-center gap-2">
          <span className="font-semibold text-foreground">{t('accounts.schedulerView')}</span>
          <SchedulerChip label={t('accounts.healthy')} value={healthyAccounts} tone="success" />
          <SchedulerChip label={t('accounts.warm')} value={warmAccounts} tone="warning" />
          <SchedulerChip label={t('accounts.risky')} value={riskyAccounts} tone="danger" />
          <SchedulerChip label={t('status.unauthorized')} value={bannedAccounts} tone="neutral" />
        </div>

        <div className="mb-4 flex flex-wrap items-center gap-2">
          <div className="relative w-72 max-sm:w-full">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 size-4 text-muted-foreground pointer-events-none" />
            <Input
              className="pl-9 h-8 rounded-lg text-[13px]"
              placeholder={t('accounts.searchPlaceholder')}
              value={searchQuery}
              onChange={(e: ChangeEvent<HTMLInputElement>) => { setSearchQuery(e.target.value); setPage(1) }}
            />
          </div>
          <div className="flex items-center gap-1 rounded-lg border border-border bg-muted/30 p-0.5">
            {(['all', 'pro', 'plus', 'team', 'free'] as const).map((key) => (
              <button
                key={key}
                onClick={() => { setPlanFilter(key); setPage(1) }}
                className={`rounded-md px-2.5 py-1 text-[12px] font-medium transition-colors ${
                  planFilter === key
                    ? 'bg-background shadow-sm text-foreground'
                    : 'text-muted-foreground hover:text-foreground'
                }`}
              >
                {key === 'all' ? t('accounts.filterAll') : key.charAt(0).toUpperCase() + key.slice(1)}
              </button>
            ))}
          </div>
        </div>

        {selected.size > 0 && (
          <div className="sticky top-2 z-20 mb-4 flex items-center justify-between gap-3 rounded-lg border border-primary/20 bg-card/95 px-3 py-2.5 text-sm font-semibold text-primary shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
            <span>{t('common.selected', { count: selected.size })}</span>
            <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchRefresh()}>
                {t('accounts.batchRefresh')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchEnabled(true)}>
                <Power className="size-3 mr-1" />{t('accounts.enable')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchEnabled(false)}>
                <PowerOff className="size-3 mr-1" />{t('accounts.disable')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchLock(true)}>
                <Lock className="size-3 mr-1" />{t('accounts.lock')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchLock(false)}>
                <Unlock className="size-3 mr-1" />{t('accounts.unlock')}
              </Button>
              <Button variant="outline" size="sm" disabled={batchLoading} onClick={() => void handleBatchResetStatus()}>
                <RotateCcw className="size-3 mr-1" />{t('accounts.batchResetStatus')}
              </Button>
              <Button variant="destructive" size="sm" disabled={batchLoading} onClick={() => void handleBatchDelete()}>
                {t('accounts.batchDelete')}
              </Button>
              <Button variant="outline" size="sm" onClick={() => setSelected(new Set())}>
                {t('accounts.cancelSelection')}
              </Button>
            </div>
          </div>
        )}

        <Card>
          <CardContent className="p-4">
            <StateShell
              variant="section"
              isEmpty={accounts.length === 0}
              emptyTitle={t('accounts.noData')}
              emptyDescription={t('accounts.noDataDesc')}
              action={<Button onClick={() => setShowAdd(true)}>{t('accounts.addAccount')}</Button>}
            >
              <div className="data-table-shell">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-10">
                        <input
                          type="checkbox"
                          className="size-4 cursor-pointer accent-primary"
                          checked={allPageSelected}
                          onChange={toggleSelectAll}
                        />
                      </TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.sequence')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.email')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.plan')}</TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.status')}</TableHead>
                      <TableHead
                        className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                        onClick={() => { if (sortKey === 'requests') { setSortDir(d => d === 'asc' ? 'desc' : 'asc') } else { setSortKey('requests'); setSortDir('desc') }; setPage(1) }}
                      >
                        {t('accounts.requests')} {sortKey === 'requests' ? (sortDir === 'desc' ? '↓' : '↑') : ''}
                      </TableHead>
                      <TableHead
                        className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                        onClick={() => { if (sortKey === 'usage') { setSortDir(d => d === 'asc' ? 'desc' : 'asc') } else { setSortKey('usage'); setSortDir('desc') }; setPage(1) }}
                      >
                        {t('accounts.usage')} {sortKey === 'usage' ? (sortDir === 'desc' ? '↓' : '↑') : ''}
                      </TableHead>
                      <TableHead
                        className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                        onClick={() => { if (sortKey === 'importTime') { setSortDir(d => d === 'asc' ? 'desc' : 'asc') } else { setSortKey('importTime'); setSortDir('desc') }; setPage(1) }}
                      >
                        {t('accounts.importTime')} {sortKey === 'importTime' ? (sortDir === 'desc' ? '↓' : '↑') : ''}
                      </TableHead>
                      <TableHead className="text-[13px] font-semibold">{t('accounts.updatedAt')}</TableHead>
                      <TableHead className="text-[13px] font-semibold text-right">{t('accounts.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {pagedAccounts.map((account, index) => (
                      <TableRow key={account.id} className={selected.has(account.id) ? 'bg-primary/5' : ''}>
                        <TableCell>
                          <input
                            type="checkbox"
                            className="size-4 cursor-pointer accent-primary"
                            checked={selected.has(account.id)}
                            onChange={() => toggleSelect(account.id)}
                          />
                        </TableCell>
                        <TableCell className="text-[14px] font-mono text-muted-foreground" title={`ID ${account.id}`}>
                          {(currentPage - 1) * pageSize + index + 1}
                        </TableCell>
                        <TableCell className="text-[14px] text-muted-foreground">
                          {formatCompactEmail(account.email)}
                          {account.at_only && (
                            <span className="ml-1.5 inline-flex items-center rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 ring-1 ring-inset ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20">
                              AT
                            </span>
                          )}
                          {account.enabled === false && (
                            <span className="ml-1.5 inline-flex items-center rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium text-zinc-700 ring-1 ring-inset ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-300 dark:ring-zinc-400/20">
                              <PowerOff className="size-2.5 mr-0.5" />{t('accounts.disabled')}
                            </span>
                          )}
                          {account.locked && (
                            <span className="ml-1.5 inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                              <Lock className="size-2.5 mr-0.5" />{t('accounts.lock')}
                            </span>
                          )}
                        </TableCell>
                        <TableCell
                          className="text-[13px] font-medium"
                        >
                          {account.plan_type || '-'}
                        </TableCell>
                        <TableCell>
                          <div className="space-y-1">
                            <StatusBadge status={account.status} />
                            {account.cooldown_until && (account.status === 'rate_limited' || account.status === 'error') && (
                              <CooldownTimer until={account.cooldown_until} />
                            )}
                            <div className="text-[11px] text-muted-foreground">
                              {t('accounts.healthSummary', {
                                health: formatHealthTier(account.health_tier, t),
                                score: Math.round(getDispatchScore(account)),
                                concurrency: account.dynamic_concurrency_limit ?? '-',
                              })}
                            </div>
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-2 text-[13px]">
                            <span className="text-emerald-600 font-medium">{account.success_requests ?? 0}</span>
                            <span className="text-muted-foreground">/</span>
                            <span className="text-red-500 font-medium">{account.error_requests ?? 0}</span>
                          </div>
                        </TableCell>
                        <TableCell>
                          <UsageCell account={account} />
                        </TableCell>
                        <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">{formatBeijingTime(account.created_at)}</TableCell>
                        <TableCell className="text-[14px] text-muted-foreground">{formatRelativeTime(account.updated_at)}</TableCell>
                        <TableCell className="text-right">
                          <div className="flex items-center gap-1 justify-end">
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => openSchedulerEditor(account)}
                              title={t('accounts.editScheduler')}
                            >
                              <Pencil className="size-3.5" />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => setUsageAccount(account)}
                              title={t('accounts.usageDetail')}
                            >
                              <BarChart3 className="size-3.5" />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => setTestingAccount(account)}
                              title={t('accounts.testConnection')}
                            >
                              <Zap className="size-3.5" />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              disabled={refreshingIds.has(account.id) || account.at_only}
                              onClick={() => void handleRefresh(account)}
                              title={account.at_only ? t('accounts.atRefreshDisabled') : t('accounts.refreshAccessToken')}
                            >
                              <RefreshCw className={`size-3.5 ${refreshingIds.has(account.id) ? 'animate-spin' : ''}`} />
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              disabled={authJsonExportingIds.has(account.id) || account.at_only}
                              onClick={() => void handleGenerateAuthJSON(account)}
                              title={account.at_only ? t('accounts.authJsonDisabled') : t('accounts.generateAuthJson')}
                            >
                              <FileJson className="size-3.5" />
                            </Button>
                            <Button
                              variant={account.enabled === false ? 'default' : 'outline'}
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => void handleToggleEnabled(account)}
                              title={account.enabled === false ? t('accounts.enableHint') : t('accounts.disableHint')}
                            >
                              {account.enabled === false ? <Power className="size-3.5" /> : <PowerOff className="size-3.5" />}
                            </Button>
                            <Button
                              variant={account.locked ? 'default' : 'outline'}
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => void handleToggleLock(account)}
                              title={account.locked ? t('accounts.unlockHint') : t('accounts.lockHint')}
                            >
                              {account.locked ? <Lock className="size-3.5" /> : <Unlock className="size-3.5" />}
                            </Button>
                            <Button
                              variant="outline"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => void handleResetStatus(account)}
                              title={t('accounts.resetStatusHint')}
                            >
                              <RotateCcw className="size-3.5" />
                            </Button>
                            <Button
                              variant="destructive"
                              size="icon"
                              className="h-7 w-8 px-0"
                              onClick={() => void handleDelete(account)}
                              title={t('accounts.deleteAccount')}
                            >
                              <Trash2 className="size-3.5" />
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <Pagination
                page={currentPage}
                totalPages={totalPages}
                onPageChange={setPage}
                totalItems={sortedAccounts.length}
                pageSize={pageSize}
                pageSizeOptions={pageSizeOptions}
                onPageSizeChange={(nextPageSize) => {
                  setPageSize(nextPageSize)
                  setPage(1)
                }}
              />
            </StateShell>
          </CardContent>
        </Card>

        <Modal
          show={showAdd}
          title={t('accounts.addTitle')}
          contentClassName="sm:max-w-[640px]"
          onClose={() => {
            setShowAdd(false)
            setAddMethod('rt')
            setOauthStep('generate')
            setOauthSession(null)
            setOauthCallbackUrl('')
            setOauthName('')
          }}
          footer={(
            <>
              <Button
                variant="outline"
                onClick={() => {
                  setShowAdd(false)
                  setAddMethod('rt')
                  setOauthStep('generate')
                  setOauthSession(null)
                  setOauthCallbackUrl('')
                  setOauthName('')
                }}
              >
                {t('common.cancel')}
              </Button>
              {addMethod === 'rt' ? (
                <Button onClick={() => void handleAdd()} disabled={submitting || !addForm.refresh_token.trim()}>
                  {submitting ? t('accounts.adding') : t('accounts.submit')}
                </Button>
              ) : addMethod === 'at' ? (
                <Button onClick={() => void handleAddAT()} disabled={submitting || !atForm.access_token.trim()}>
                  {submitting ? t('accounts.adding') : t('accounts.submit')}
                </Button>
              ) : oauthStep === 'generate' ? (
                <Button onClick={() => void handleOAuthGenerate()} disabled={oauthGenerating}>
                  {oauthGenerating ? t('accounts.oauthGenerating') : t('accounts.oauthGenerateBtn')}
                </Button>
              ) : (
                <Button
                  onClick={() => void handleOAuthComplete()}
                  disabled={oauthCompleting || !oauthCallbackUrl.trim()}
                >
                  {oauthCompleting ? t('accounts.oauthCompleting') : t('accounts.oauthCompleteBtn')}
                </Button>
              )}
            </>
          )}
        >
          {/* Tab switcher */}
          <div className="flex gap-1 p-1 mb-5 rounded-xl bg-muted/50 border border-border">
            <button
              onClick={() => setAddMethod('rt')}
              className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                addMethod === 'rt'
                  ? 'bg-background shadow-sm text-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              <RefreshCw className="size-3.5" />
              {t('accounts.addMethodRT')}
            </button>
            <button
              onClick={() => setAddMethod('at')}
              className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                addMethod === 'at'
                  ? 'bg-background shadow-sm text-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              <Fingerprint className="size-3.5" />
              {t('accounts.addMethodAT')}
            </button>
            <button
              onClick={() => { setAddMethod('oauth'); setOauthStep('generate'); setOauthSession(null); setOauthCallbackUrl('') }}
              className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                addMethod === 'oauth'
                  ? 'bg-background shadow-sm text-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              <KeyRound className="size-3.5" />
              {t('accounts.addMethodOAuth')}
            </button>
          </div>

          {addMethod === 'rt' ? (
            <div className="space-y-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.refreshTokenLabel')} *</label>
                <textarea
                  className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                  placeholder={t('accounts.refreshTokenPlaceholder')}
                  value={addForm.refresh_token}
                  onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                    setAddForm((form) => ({ ...form, refresh_token: event.target.value }))
                  }
                  rows={6}
                />
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.proxyUrl')}</label>
                <Input
                  placeholder={t('accounts.proxyUrlPlaceholder')}
                  value={addForm.proxy_url}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    setAddForm((form) => ({ ...form, proxy_url: event.target.value }))
                  }
                />
              </div>
            </div>
          ) : addMethod === 'at' ? (
            <div className="space-y-4">
              <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-800 dark:bg-amber-950/50 dark:text-amber-300">
                {t('accounts.atWarning')}
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.accessTokenLabel')} *</label>
                <textarea
                  className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                  placeholder={t('accounts.accessTokenPlaceholder')}
                  value={atForm.access_token}
                  onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                    setAtForm((form) => ({ ...form, access_token: event.target.value }))
                  }
                  rows={6}
                />
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.proxyUrl')}</label>
                <Input
                  placeholder={t('accounts.proxyUrlPlaceholder')}
                  value={atForm.proxy_url}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    setAtForm((form) => ({ ...form, proxy_url: event.target.value }))
                  }
                />
              </div>
            </div>
          ) : (
            <div className="space-y-5">
              {oauthStep === 'generate' ? (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="font-semibold text-foreground mb-1">{t('accounts.oauthStep1Title')}</p>
                    <p>{t('accounts.oauthStep1Desc')}</p>
                  </div>
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.oauthNameLabel')}</label>
                    <Input
                      placeholder={t('accounts.oauthNamePlaceholder')}
                      value={oauthName}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setOauthName(e.target.value)}
                    />
                  </div>
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.oauthProxyUrl')}</label>
                    <Input
                      placeholder={t('accounts.oauthProxyUrlPlaceholder')}
                      value={oauthProxyUrl}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setOauthProxyUrl(e.target.value)}
                    />
                  </div>
                </>
              ) : (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="font-semibold text-foreground mb-1">{t('accounts.oauthStep2Title')}</p>
                    <p>{t('accounts.oauthStep2Desc')}</p>
                  </div>
                  {oauthSession && (
                    <div className="rounded-xl border border-primary/30 bg-primary/5 px-4 py-3">
                      <p className="text-xs font-semibold text-muted-foreground mb-2">{t('accounts.oauthOpenLink')}</p>
                      <a
                        href={oauthSession.auth_url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="inline-flex items-center gap-1.5 text-sm font-semibold text-primary hover:underline break-all"
                      >
                        <ExternalLink className="size-3.5 shrink-0" />
                        {t('accounts.oauthOpenLink')}
                      </a>
                    </div>
                  )}
                  <div>
                    <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.oauthCallbackUrlLabel')}</label>
                    <Input
                      placeholder={t('accounts.oauthCallbackUrlPlaceholder')}
                      value={oauthCallbackUrl}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setOauthCallbackUrl(e.target.value)}
                    />
                    <p className="mt-1.5 text-xs text-muted-foreground">{t('accounts.oauthCallbackUrlHint')}</p>
                  </div>
                  <button
                    onClick={() => { setOauthStep('generate'); setOauthSession(null); setOauthCallbackUrl('') }}
                    className="text-xs text-muted-foreground hover:text-foreground underline underline-offset-2"
                  >
                    {t('accounts.oauthRestart')}
                  </button>
                </>
              )}
            </div>
          )}
        </Modal>

        <Modal
          show={showImportPicker}
          title={t('accounts.importTitle')}
          contentClassName="sm:max-w-[640px]"
          onClose={() => setShowImportPicker(false)}
        >
          <div className="grid grid-cols-2 gap-3">
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                fileInputRef.current?.click()
              }}
            >
              <FileText className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importTxt')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importTxtDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                jsonInputRef.current?.click()
              }}
            >
              <FileJson className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importJson')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importJsonDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                atFileInputRef.current?.click()
              }}
            >
              <Fingerprint className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importAtTxt')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importAtTxtDesc')}</div>
              </div>
            </button>
            <button
              className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
              onClick={() => {
                setShowImportPicker(false)
                folderInputRef.current?.click()
              }}
            >
              <FolderOpen className="size-5 shrink-0 text-muted-foreground" />
              <div>
                <div className="text-sm font-medium">{t('accounts.importFolder')}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importFolderDesc')}</div>
              </div>
            </button>
          </div>
        </Modal>

        <Modal
          show={showExportPicker}
          title={t('accounts.exportTitle')}
          contentClassName="sm:max-w-[580px]"
          onClose={() => setShowExportPicker(false)}
        >
          <div className="space-y-4">
            {/* 健康账号导出 */}
            <div>
              <div className="text-xs font-semibold text-muted-foreground mb-2">{t('accounts.exportScopeHealthy')}</div>
              <div className="grid grid-cols-2 gap-3">
                <button
                  className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                  onClick={() => void handleExport('json', 'healthy')}
                >
                  <FileJson className="size-5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0">
                    <div className="text-sm font-medium">CPA JSON</div>
                    <div className="text-[11px] text-muted-foreground">{t('accounts.exportHealthyJsonDesc')}</div>
                  </div>
                </button>
                <button
                  className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                  onClick={() => void handleExport('txt', 'healthy')}
                >
                  <FileText className="size-5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0">
                    <div className="text-sm font-medium">TXT</div>
                    <div className="text-[11px] text-muted-foreground">{t('accounts.exportHealthyTxtDesc')}</div>
                  </div>
                </button>
              </div>
            </div>
            {/* 已选账号导出 */}
            <div>
              <div className="text-xs font-semibold text-muted-foreground mb-2">{t('accounts.exportScopeSelected', { count: selected.size })}</div>
              <div className="grid grid-cols-2 gap-3">
                <button
                  className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
                  disabled={selected.size === 0}
                  onClick={() => void handleExport('json', 'selected')}
                >
                  <FileJson className="size-5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0">
                    <div className="text-sm font-medium">CPA JSON</div>
                    <div className="text-[11px] text-muted-foreground">{t('accounts.exportSelectedJsonDesc')}</div>
                  </div>
                </button>
                <button
                  className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
                  disabled={selected.size === 0}
                  onClick={() => void handleExport('txt', 'selected')}
                >
                  <FileText className="size-5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0">
                    <div className="text-sm font-medium">TXT</div>
                    <div className="text-[11px] text-muted-foreground">{t('accounts.exportSelectedTxtDesc')}</div>
                  </div>
                </button>
              </div>
            </div>
          </div>
        </Modal>

        <Modal
          show={Boolean(authJsonModal)}
          title={t('accounts.authJsonModalTitle')}
          contentClassName="sm:max-w-[720px]"
          onClose={() => setAuthJsonModal(null)}
        >
          {authJsonModal && (
            <div className="space-y-4">
              <div className="flex items-start gap-3 rounded-lg border border-border bg-muted/30 px-4 py-3">
                <FileJson className="mt-0.5 size-5 shrink-0 text-primary" />
                <div className="min-w-0 space-y-1">
                  <div className="text-sm font-semibold text-foreground">
                    {authJsonModal.account.email || `ID ${authJsonModal.account.id}`}
                  </div>
                  <p className="text-xs leading-relaxed text-muted-foreground">
                    {t('accounts.authJsonModalDesc')}
                  </p>
                </div>
              </div>

              <div>
                <div className="mb-2 text-xs font-semibold text-muted-foreground">{t('accounts.authJsonPreview')}</div>
                <textarea
                  readOnly
                  value={authJsonModal.json}
                  className="min-h-[260px] w-full resize-y rounded-lg border border-border bg-muted/30 p-3 text-[12px] leading-relaxed text-muted-foreground outline-none focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                  style={{ fontFamily: 'var(--font-geist-mono)' }}
                />
              </div>

              <div className="flex flex-wrap justify-end gap-2">
                <Button variant="outline" onClick={() => setAuthJsonModal(null)}>
                  {t('common.close')}
                </Button>
                <Button variant="outline" onClick={() => void handleCopyAuthJSON()}>
                  <Copy className="size-4" />
                  {t('accounts.copyAuthJson')}
                </Button>
                <Button onClick={handleExportAuthJSON}>
                  <Download className="size-4" />
                  {t('accounts.exportAuthJson')}
                </Button>
              </div>
            </div>
          )}
        </Modal>

        <Modal
          show={showMigrate}
          title={t('accounts.migrateTitle')}
          contentClassName="sm:max-w-[520px]"
          onClose={() => { setShowMigrate(false); setMigrateUrl(''); setMigrateKey('') }}
        >
          <div className="space-y-4">
            <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
              <p>{t('accounts.migrateDesc')}</p>
            </div>
            <div>
              <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.migrateUrlLabel')}</label>
              <Input
                placeholder={t('accounts.migrateUrlPlaceholder')}
                value={migrateUrl}
                onChange={(e: ChangeEvent<HTMLInputElement>) => setMigrateUrl(e.target.value)}
              />
            </div>
            <div>
              <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('accounts.migrateKeyLabel')}</label>
              <Input
                type="password"
                placeholder={t('accounts.migrateKeyPlaceholder')}
                value={migrateKey}
                onChange={(e: ChangeEvent<HTMLInputElement>) => setMigrateKey(e.target.value)}
              />
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <Button variant="outline" onClick={() => { setShowMigrate(false); setMigrateUrl(''); setMigrateKey('') }}>
                {t('common.cancel')}
              </Button>
              <Button
                onClick={() => void handleMigrate()}
                disabled={migrating || !migrateUrl.trim() || !migrateKey.trim()}
              >
                {migrating ? t('accounts.migrating') : t('accounts.migrateConfirm')}
              </Button>
            </div>
          </div>
        </Modal>

        {testingAccount && (
          <TestConnectionModal
            account={testingAccount}
            onSettled={() => {
              void reloadSilently()
            }}
            onClose={() => setTestingAccount(null)}
          />
        )}

        {usageAccount && (
          <AccountUsageModal account={usageAccount} onClose={() => setUsageAccount(null)} />
        )}

        <Modal
          show={Boolean(editingAccount)}
          title={t('accounts.schedulerEditTitle')}
          contentClassName="sm:max-w-[760px]"
          onClose={closeSchedulerEditor}
          footer={(
            <>
              <Button variant="outline" onClick={() => closeSchedulerEditor()} disabled={editSubmitting}>
                {t('common.cancel')}
              </Button>
              <Button onClick={() => void handleSaveScheduler()} disabled={editSubmitting || scoreInputInvalid || concurrencyInputInvalid}>
                {editSubmitting ? t('common.saving') : t('common.save')}
              </Button>
            </>
          )}
        >
          {editingAccount && editPreview ? (
            <div className="space-y-5">
              <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                <div className="font-semibold text-foreground">{editingAccount.email || `ID ${editingAccount.id}`}</div>
                <div className="mt-1">{t('accounts.schedulerEditDesc', { plan: editingAccount.plan_type || '-' })}</div>
              </div>

              <div className="grid gap-4 md:grid-cols-2">
                <div className="rounded-xl border border-border p-4">
                  <div className="text-sm font-semibold text-foreground">{t('accounts.schedulerScoreLabel')}</div>
                  <div className="mt-1 text-xs text-muted-foreground">{t('accounts.schedulerScoreHint')}</div>
                  <div className="mt-3 flex gap-2">
                    <TogglePill
                      active={scoreMode === 'default'}
                      onClick={() => setScoreMode('default')}
                      label={t('accounts.schedulerScoreAuto')}
                    />
                    <TogglePill
                      active={scoreMode === 'custom'}
                      onClick={() => setScoreMode('custom')}
                      label={t('accounts.schedulerCustom')}
                    />
                  </div>
                  {scoreMode === 'default' ? (
                    <div className="mt-3 rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                      {t('accounts.schedulerScoreAutoValue', { value: formatSignedNumber(getDefaultScoreBias(editingAccount.plan_type)) })}
                    </div>
                  ) : (
                    <div className="mt-3 space-y-2">
                      <Input
                        inputMode="numeric"
                        value={scoreInput}
                        onChange={(event: ChangeEvent<HTMLInputElement>) => setScoreInput(event.target.value)}
                        placeholder={t('accounts.schedulerScorePlaceholder')}
                      />
                      <div className={`text-xs ${scoreInputInvalid ? 'text-red-500' : 'text-muted-foreground'}`}>
                        {scoreInputInvalid ? t('accounts.schedulerScoreRange') : t('accounts.schedulerCustomValuePreview', { value: formatSignedNumber(parsedScoreBias ?? getEffectiveScoreBias(editingAccount)) })}
                      </div>
                    </div>
                  )}
                </div>

                <div className="rounded-xl border border-border p-4">
                  <div className="text-sm font-semibold text-foreground">{t('accounts.schedulerConcurrencyLabel')}</div>
                  <div className="mt-1 text-xs text-muted-foreground">{t('accounts.schedulerConcurrencyHint')}</div>
                  <div className="mt-3 flex gap-2">
                    <TogglePill
                      active={concurrencyMode === 'default'}
                      onClick={() => setConcurrencyMode('default')}
                      label={t('accounts.schedulerConcurrencyAuto')}
                    />
                    <TogglePill
                      active={concurrencyMode === 'custom'}
                      onClick={() => setConcurrencyMode('custom')}
                      label={t('accounts.schedulerCustom')}
                    />
                  </div>
                  {concurrencyMode === 'default' ? (
                    <div className="mt-3 rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                      {t('accounts.schedulerConcurrencyAutoValue', { value: getEffectiveBaseConcurrency(editingAccount) })}
                    </div>
                  ) : (
                    <div className="mt-3 space-y-2">
                      <Input
                        inputMode="numeric"
                        value={concurrencyInput}
                        onChange={(event: ChangeEvent<HTMLInputElement>) => setConcurrencyInput(event.target.value)}
                        placeholder={t('accounts.schedulerConcurrencyPlaceholder')}
                      />
                      <div className={`text-xs ${concurrencyInputInvalid ? 'text-red-500' : 'text-muted-foreground'}`}>
                        {concurrencyInputInvalid ? t('accounts.schedulerConcurrencyRange') : t('accounts.schedulerCustomValuePreview', { value: parsedBaseConcurrency ?? getEffectiveBaseConcurrency(editingAccount) })}
                      </div>
                    </div>
                  )}
                </div>
              </div>

              <div className="rounded-xl border border-border p-4">
                <div className="text-sm font-semibold text-foreground">{t('accounts.allowedAPIKeysLabel')}</div>
                <div className="mt-1 text-xs text-muted-foreground">{t('accounts.allowedAPIKeysHint')}</div>
                <div className="mt-3">
                  <APIKeyMultiSelect
                    options={apiKeys}
                    value={allowedAPIKeySelection}
                    disabled={apiKeys.length === 0}
                    onChange={setAllowedAPIKeySelection}
                    allLabel={t('accounts.allowedAPIKeysAll')}
                    selectedLabel={t('accounts.allowedAPIKeysSelected', { count: allowedAPIKeySelection.length })}
                    placeholder={t('accounts.allowedAPIKeysPlaceholder')}
                    emptyLabel={t('accounts.allowedAPIKeysNoOptions')}
                    emptyHint={t('accounts.allowedAPIKeysNoOptionsHint')}
                  />
                </div>
              </div>

              <div className="rounded-xl border border-border bg-white/60 px-4 py-4 dark:bg-white/5">
                <div className="text-sm font-semibold text-foreground">{t('accounts.schedulerPreviewTitle')}</div>
                <div className="mt-3 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
                  <PreviewItem label={t('accounts.schedulerPreviewRawScore')} value={String(editPreview.rawScore)} />
                  <PreviewItem label={t('accounts.schedulerPreviewDispatchScore')} value={String(editPreview.dispatchScore)} />
                  <PreviewItem label={t('accounts.schedulerPreviewHealthTier')} value={formatHealthTier(editPreview.healthTier, t)} />
                  <PreviewItem label={t('accounts.schedulerPreviewDynamicConcurrency')} value={String(editPreview.dynamicConcurrency)} />
                </div>
              </div>
            </div>
          ) : null}
        </Modal>

        <Modal
          show={importProgress.show}
          title={importProgress.done ? t('accounts.importDone') : t('accounts.importingProgress')}
          contentClassName="sm:max-w-[420px]"
          onClose={() => setImportProgress(p => ({ ...p, show: false }))}
        >
          <div className="space-y-4">
            <div className="w-full h-3 bg-muted rounded-full overflow-hidden">
              <div
                className="h-full bg-primary rounded-full transition-all duration-300 ease-out"
                style={{ width: importProgress.total > 0 ? `${Math.round((importProgress.current / importProgress.total) * 100)}%` : '0%' }}
              />
            </div>
            <div className="text-center text-sm text-muted-foreground">
              {importProgress.total > 0
                ? `${importProgress.current} / ${importProgress.total}  (${Math.round((importProgress.current / importProgress.total) * 100)}%)`
                : t('accounts.importPreparing')}
            </div>
            <div className="grid grid-cols-3 gap-3 text-center">
              <div className="rounded-xl bg-emerald-500/10 px-3 py-2">
                <div className="text-lg font-bold text-emerald-600">{importProgress.success}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importSuccess')}</div>
              </div>
              <div className="rounded-xl bg-amber-500/10 px-3 py-2">
                <div className="text-lg font-bold text-amber-600">{importProgress.duplicate}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importDuplicate')}</div>
              </div>
              <div className="rounded-xl bg-red-500/10 px-3 py-2">
                <div className="text-lg font-bold text-red-600">{importProgress.failed}</div>
                <div className="text-[11px] text-muted-foreground">{t('accounts.importFailedCount')}</div>
              </div>
            </div>
            {importProgress.done && (
              <p className="text-xs text-center text-muted-foreground">{t('accounts.importDoneHint')}</p>
            )}
          </div>
        </Modal>

        {confirmDialog}

        <ToastNotice toast={toast} />
      </>
    </StateShell>
    </div>
  )
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

function formatJSONText(text: string) {
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

async function copyTextToClipboard(text: string) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text)
    return
  }

  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.setAttribute('readonly', 'true')
  textarea.style.position = 'fixed'
  textarea.style.top = '-1000px'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  const copied = document.execCommand('copy')
  document.body.removeChild(textarea)
  if (!copied) {
    throw new Error('copy failed')
  }
}

function filterExistingAPIKeyIDs(selected: number[], apiKeys: APIKeyRow[]): number[] {
  if (!selected.length || !apiKeys.length) {
    return []
  }
  const existing = new Set(apiKeys.map((item) => item.id))
  return [...new Set(selected.filter((id) => existing.has(id)))].sort((a, b) => a - b)
}

function formatAPIKeyOptionLabel(apiKey: APIKeyRow): string {
  const name = apiKey.name?.trim() || `API Key #${apiKey.id}`
  return `${name} · ${apiKey.key}`
}

function APIKeyMultiSelect({
  options,
  value,
  disabled,
  onChange,
  allLabel,
  selectedLabel,
  placeholder,
  emptyLabel,
  emptyHint,
}: {
  options: APIKeyRow[]
  value: number[]
  disabled: boolean
  onChange: (value: number[]) => void
  allLabel: string
  selectedLabel: string
  placeholder: string
  emptyLabel: string
  emptyHint: string
}) {
  const [open, setOpen] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return

    const handlePointerDown = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false)
      }
    }

    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setOpen(false)
      }
    }

    document.addEventListener('mousedown', handlePointerDown)
    document.addEventListener('keydown', handleEscape)
    return () => {
      document.removeEventListener('mousedown', handlePointerDown)
      document.removeEventListener('keydown', handleEscape)
    }
  }, [open])

  const summary = value.length === 0 ? allLabel : selectedLabel

  const toggleOption = (id: number) => {
    if (disabled) return
    if (value.includes(id)) {
      onChange(value.filter((item) => item !== id))
      return
    }
    onChange([...value, id].sort((a, b) => a - b))
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        disabled={disabled}
        className={`flex w-full items-center justify-between gap-3 rounded-md border border-input bg-background px-3.5 py-3 text-left shadow-xs transition-[border-color,box-shadow] ${
          disabled
            ? 'cursor-not-allowed opacity-70'
            : 'hover:border-primary/30 hover:bg-accent/40'
        } ${open ? 'border-primary/35 ring-[3px] ring-primary/10' : ''}`}
        onClick={() => {
          if (!disabled) {
            setOpen((current) => !current)
          }
        }}
      >
        <div className="min-w-0">
          <div className="truncate text-[15px] text-foreground">{summary}</div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            {disabled ? emptyHint : placeholder}
          </div>
        </div>
        <ChevronDown className={`size-4 shrink-0 text-muted-foreground transition-transform ${open ? 'rotate-180' : ''}`} />
      </button>

      {open ? (
        <div className="absolute left-0 right-0 top-[calc(100%+0.5rem)] z-50 overflow-hidden rounded-lg border border-border bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm">
          {options.length === 0 ? (
            <div className="px-4 py-3 text-sm text-muted-foreground">{emptyLabel}</div>
          ) : (
            <div className="max-h-72 space-y-1 overflow-auto p-2">
              {options.map((option) => {
                const checked = value.includes(option.id)
                return (
                  <button
                    key={option.id}
                    type="button"
                    className={`flex w-full items-center gap-3 rounded-md px-3 py-2.5 text-left transition-colors ${
                      checked ? 'bg-primary/10 text-primary' : 'text-foreground hover:bg-accent/70'
                    }`}
                    onClick={() => toggleOption(option.id)}
                  >
                    <span className={`flex size-4 items-center justify-center rounded border ${checked ? 'border-primary bg-primary text-primary-foreground' : 'border-border bg-background text-transparent'}`}>
                      <Check className="size-3" />
                    </span>
                    <span className="min-w-0 flex-1 truncate text-sm">{formatAPIKeyOptionLabel(option)}</span>
                  </button>
                )
              })}
            </div>
          )}
        </div>
      ) : null}
    </div>
  )
}

function TogglePill({
  active,
  onClick,
  label,
}: {
  active: boolean
  onClick: () => void
  label: string
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-full px-3 py-1.5 text-xs font-semibold transition-colors ${
        active
          ? 'bg-primary text-primary-foreground'
          : 'bg-muted/50 text-muted-foreground hover:bg-muted'
      }`}
    >
      {label}
    </button>
  )
}

function PreviewItem({
  label,
  value,
}: {
  label: string
  value: string
}) {
  return (
    <div className="rounded-lg border border-border bg-muted/20 px-3 py-3">
      <div className="text-[11px] font-semibold text-muted-foreground">{label}</div>
      <div className="mt-1 text-base font-semibold text-foreground">{value}</div>
    </div>
  )
}

function parseIntegerInput(value: string): number | null {
  const trimmed = value.trim()
  if (!trimmed || !/^-?\d+$/.test(trimmed)) {
    return null
  }

  return Number.parseInt(trimmed, 10)
}

function getDispatchScore(account: AccountRow): number {
  return account.dispatch_score ?? account.scheduler_score ?? 0
}

function getDefaultScoreBias(planType?: string): number {
  switch ((planType || '').toLowerCase()) {
    case 'pro':
    case 'plus':
    case 'team':
      return 50
    default:
      return 0
  }
}

function getEffectiveScoreBias(account: AccountRow): number {
  if (typeof account.score_bias_effective === 'number') {
    return account.score_bias_effective
  }
  if (typeof account.score_bias_override === 'number') {
    return account.score_bias_override
  }
  return getDefaultScoreBias(account.plan_type)
}

function getEffectiveBaseConcurrency(account: AccountRow): number {
  if (typeof account.base_concurrency_effective === 'number' && account.base_concurrency_effective > 0) {
    return account.base_concurrency_effective
  }
  if (typeof account.base_concurrency_override === 'number' && account.base_concurrency_override > 0) {
    return account.base_concurrency_override
  }
  if (typeof account.dynamic_concurrency_limit === 'number' && account.dynamic_concurrency_limit > 0) {
    return account.dynamic_concurrency_limit
  }
  return 1
}

function computePreviewDispatchScore(account: AccountRow, rawScore: number, appliedBias: number): number {
  if (
    (account.health_tier === 'healthy' || account.health_tier === 'warm') &&
    (account.status === 'active' || account.status === 'ready')
  ) {
    return rawScore + appliedBias
  }
  return rawScore
}

function computePreviewDynamicConcurrency(account: AccountRow, baseConcurrency: number): number {
  switch (account.health_tier) {
    case 'healthy':
      return baseConcurrency
    case 'warm':
      return Math.max(1, Math.floor(baseConcurrency / 2))
    case 'risky':
      return 1
    case 'banned':
      return 0
    default:
      return account.dynamic_concurrency_limit ?? baseConcurrency
  }
}

function formatSignedNumber(value: number): string {
  if (value > 0) return `+${value}`
  return String(value)
}

function CompactStat({
  label,
  chipLabel,
  value,
  tone,
}: {
  label: string
  chipLabel?: string
  value: number
  tone: 'neutral' | 'success' | 'warning' | 'danger'
}) {
  const toneStyle = {
    neutral: {
      chip: 'bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300',
      dot: 'bg-slate-500',
    },
    success: {
      chip: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300',
      dot: 'bg-emerald-500',
    },
    warning: {
      chip: 'bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300',
      dot: 'bg-amber-500',
    },
    danger: {
      chip: 'bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300',
      dot: 'bg-red-500',
    },
  }[tone]

  return (
    <div className="flex items-center justify-between rounded-lg border border-border bg-card/85 px-3 py-2.5 shadow-sm">
      <div className="min-w-0">
        <div className="text-[12px] font-semibold text-muted-foreground">{label}</div>
        <div className="mt-1 text-[24px] font-bold leading-none text-foreground">{value}</div>
      </div>
      <div className={`inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-[12px] font-semibold ${toneStyle.chip}`}>
        <span className={`size-2 rounded-full ${toneStyle.dot}`} />
        {chipLabel ?? label}
      </div>
    </div>
  )
}

function SchedulerChip({
  label,
  value,
  tone,
}: {
  label: string
  value: number
  tone: 'neutral' | 'success' | 'warning' | 'danger'
}) {
  const toneStyle = {
    neutral: 'bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300',
    success: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300',
    warning: 'bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300',
    danger: 'bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300',
  }[tone]

  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 font-semibold ${toneStyle}`}>
      <span>{label}</span>
      <span>{value}</span>
    </span>
  )
}

function formatHealthTier(healthTier?: string, t?: any) {
  if (!t) return 'Unknown'
  switch (healthTier) {
    case 'healthy':
      return t('accounts.healthy')
    case 'warm':
      return t('accounts.warm')
    case 'risky':
      return t('accounts.risky')
    case 'banned':
      return t('accounts.quarantine')
    default:
      return t('accounts.unknown')
  }
}

// ==================== 测试连接弹窗 ====================

interface TestEvent {
  type: 'test_start' | 'content' | 'test_complete' | 'error'
  text?: string
  model?: string
  success?: boolean
  error?: string
}

function formatTestErrorMessage(message: string) {
  const normalized = message.trim()
  const jsonStart = normalized.indexOf('{')

  if (jsonStart === -1) {
    return normalized
  }

  const prefix = normalized.slice(0, jsonStart).trim().replace(/[：:]\s*$/, '')
  const jsonText = normalized.slice(jsonStart)

  try {
    const parsed = JSON.parse(jsonText)
    const prettyJson = JSON.stringify(parsed, null, 2)
    return prefix ? `${prefix}\n${prettyJson}` : prettyJson
  } catch {
    return normalized
  }
}

function formatTestOutput(text: string) {
  try {
    const parsed = JSON.parse(text);
    return JSON.stringify(parsed, null, 2);
  } catch {
    return text;
  }
}

const DEFAULT_TEST_MODEL = 'gpt-5.4'

function isConnectionTestModel(model: string) {
  const value = model.trim().toLowerCase()
  return value !== '' && !value.includes('image')
}

function extractTextModels(modelsResp: Awaited<ReturnType<typeof api.getModels>>) {
  if (modelsResp.items && modelsResp.items.length > 0) {
    return modelsResp.items
      .filter((item) => item.enabled && item.category !== 'image' && !item.id.includes('image'))
      .map((item) => item.id)
  }
  return (modelsResp.models ?? []).filter(isConnectionTestModel)
}

function uniqueTestModels(models: string[], preferredModel?: string) {
  const seen = new Set<string>()
  const result: string[] = []
  const candidates = [
    preferredModel ?? '',
    ...models,
    DEFAULT_TEST_MODEL,
  ]

  for (const model of candidates) {
    const value = model.trim()
    if (!isConnectionTestModel(value) || seen.has(value)) continue
    seen.add(value)
    result.push(value)
  }
  return result
}

function TestConnectionModal({
  account,
  onClose,
  onSettled,
}: {
  account: AccountRow
  onClose: () => void
  onSettled: () => void
}) {
  const { t } = useTranslation()
  const [output, setOutput] = useState<string[]>([])
  const [status, setStatus] = useState<'connecting' | 'streaming' | 'success' | 'error'>('connecting')
  const [errorMsg, setErrorMsg] = useState('')
  const [model, setModel] = useState('')
  const [selectedModel, setSelectedModel] = useState('')
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [modelOptionsReady, setModelOptionsReady] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const outputEndRef = useRef<HTMLDivElement>(null)
  const settledRef = useRef(false)
  const onSettledRef = useRef(onSettled)
  onSettledRef.current = onSettled

  const markSettled = useCallback(() => {
    if (settledRef.current) return
    settledRef.current = true
    onSettledRef.current()
  }, [])

  const modelSelectOptions = useMemo(
    () => uniqueTestModels(modelOptions, selectedModel).map((item) => ({ label: item, value: item })),
    [modelOptions, selectedModel]
  )

  useEffect(() => {
    let active = true

    const loadModels = async () => {
      try {
        const [modelsResp, settings] = await Promise.all([api.getModels(), api.getSettings()])
        if (!active) return

	        const upstreamModels = extractTextModels(modelsResp)
	        const preferredModel = isConnectionTestModel(settings.test_model) ? settings.test_model : DEFAULT_TEST_MODEL
	        const nextModels = uniqueTestModels(upstreamModels, preferredModel)
	        setModelOptions(nextModels)
	        setSelectedModel((current) => current || nextModels[0] || DEFAULT_TEST_MODEL)
	      } catch {
	        if (!active) return
	        const fallbackModels = uniqueTestModels([], DEFAULT_TEST_MODEL)
	        setModelOptions(fallbackModels)
	        setSelectedModel((current) => current || fallbackModels[0])
      } finally {
        if (active) {
          setModelOptionsReady(true)
        }
      }
    }

    void loadModels()

    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    if (!modelOptionsReady || !selectedModel) return

    // 重置状态（StrictMode 二次 mount 时清理上一次的残留）
    setOutput([])
    setStatus('connecting')
    setErrorMsg('')
    setModel(selectedModel)
    settledRef.current = false

    const controller = new AbortController()
    abortRef.current = controller

    const run = async () => {
      if (controller.signal.aborted) return

      try {
        const params = new URLSearchParams({ model: selectedModel })
        const res = await fetch(`/api/admin/accounts/${account.id}/test?${params.toString()}`, {
          signal: controller.signal,
          headers: getAdminKey() ? { 'X-Admin-Key': getAdminKey() } : {},
        })

        if (!res.ok) {
          const body = await res.text()
          let msg = `HTTP ${res.status}`
          try {
            const parsed = JSON.parse(body)
            if (parsed.error) msg = parsed.error
          } catch { /* ignore */ }
          setStatus('error')
          setErrorMsg(msg)
          markSettled()
          return
        }

        const reader = res.body?.getReader()
        if (!reader) {
          setStatus('error')
          setErrorMsg(t('accounts.browserStreamingUnsupported'))
          markSettled()
          return
        }

        const decoder = new TextDecoder()
        let buffer = ''
        let receivedTerminalEvent = false

        const processEventLines = (lines: string[]) => {
          for (const line of lines) {
            const trimmed = line.trim()
            if (!trimmed.startsWith('data: ')) continue

            try {
              const event: TestEvent = JSON.parse(trimmed.slice(6))

              switch (event.type) {
                case 'test_start':
                  setModel(event.model || selectedModel)
                  setStatus('streaming')
                  break
                case 'content':
                  if (event.text) {
                    setOutput((prev) => [...prev, event.text!])
                  }
                  break
                case 'test_complete':
                  receivedTerminalEvent = true
                  setStatus(event.success ? 'success' : 'error')
                  markSettled()
                  break
                case 'error':
                  receivedTerminalEvent = true
                  setStatus('error')
                  setErrorMsg(event.error || t('accounts.unknownError'))
                  markSettled()
                  break
              }
            } catch { /* ignore non-JSON lines */ }
          }
        }

        while (true) {
          const { done, value } = await reader.read()
          if (done) {
            buffer += decoder.decode()
            break
          }

          buffer += decoder.decode(value, { stream: true })
          const lines = buffer.split('\n')
          buffer = lines.pop() || ''
          processEventLines(lines)
        }

        if (buffer.trim()) {
          processEventLines([buffer])
        }

        if (!receivedTerminalEvent) {
          setStatus('error')
          setErrorMsg(t('accounts.connectionEndedUnexpectedly'))
          markSettled()
        }
      } catch (err: unknown) {
        if (err instanceof DOMException && err.name === 'AbortError') return
        setStatus('error')
        setErrorMsg(err instanceof Error ? err.message : t('accounts.connectionFailed'))
        markSettled()
      }
    }

    // 延迟 50ms 启动，确保 StrictMode cleanup 有足够时间执行 abort
    const timer = window.setTimeout(() => {
      void run()
    }, 50)

    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [account.id, markSettled, modelOptionsReady, selectedModel, t])

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [output])

  const statusLabel = {
    connecting: `⏳ ${t('accounts.connecting')}`,
    streaming: `🔄 ${t('accounts.receivingResponse')}`,
    success: `✅ ${t('accounts.testSuccess')}`,
    error: `❌ ${t('accounts.testFailed')}`,
  }[status]

  const statusColor = {
    connecting: 'text-muted-foreground',
    streaming: 'text-blue-500',
    success: 'text-emerald-500',
    error: 'text-red-500',
  }[status]
  const formattedErrorMsg = errorMsg ? formatTestErrorMessage(errorMsg) : ''

  return (
    <Modal
      show={true}
      title={t('accounts.testConnectionTitle', { account: account.email || `ID ${account.id}` })}
      onClose={() => {
        abortRef.current?.abort()
        onClose()
      }}
      footer={
        <Button
          variant="outline"
          onClick={() => {
            abortRef.current?.abort()
            onClose()
          }}
        >
          {t('common.close')}
        </Button>
      }
      contentClassName="sm:max-w-[680px]"
    >
      <div className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <span className={`flex items-center gap-1.5 text-sm font-semibold ${statusColor}`}>
            {statusLabel}
          </span>
          <Select
            className="w-52 max-w-full"
            compact
            value={selectedModel}
            onValueChange={setSelectedModel}
            options={modelSelectOptions}
            placeholder={model || t('settings.testModel')}
            disabled={!modelOptionsReady || modelSelectOptions.length === 0}
          />
        </div>

        {(output.length > 0 || status === 'connecting' || status === 'streaming') && (
          <div
            className="min-h-[80px] max-h-[240px] overflow-auto rounded-lg border border-border bg-muted/30 p-3 text-[13px] leading-relaxed whitespace-pre-wrap break-all"
            style={{ fontFamily: 'var(--font-geist-mono)' }}
          >
            {output.length === 0 && status === 'connecting' && (
              <span className="text-muted-foreground animate-pulse">{t('accounts.sendingTestRequest')}</span>
            )}
            {output.join('')}
            <div ref={outputEndRef} />
          </div>
        )}

        {errorMsg && (
          <div className="max-h-[40vh] overflow-auto rounded-xl border border-red-200 bg-red-50 p-3.5 text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400">
            <div className="mb-2 text-sm font-semibold">{t('accounts.failureDetails')}</div>
            <pre
              className="text-[13px] leading-relaxed whitespace-pre-wrap break-all"
              style={{ fontFamily: 'var(--font-geist-mono)' }}
            >
              {formattedErrorMsg}
            </pre>
          </div>
        )}

        {status === 'success' && (
          <div className="flex items-center gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-2.5 text-sm text-emerald-700 dark:border-emerald-900/50 dark:bg-emerald-950/30 dark:text-emerald-400">
            <RotateCcw className="size-4 shrink-0" />
            {t('accounts.testAutoReset')}
          </div>
        )}
      </div>
    </Modal>
  )
}

// 格式化重置时间为具体时间
function formatResetAt(resetAt: string | undefined): string | null {
  if (!resetAt) return null
  const d = new Date(resetAt)
  if (d.getTime() <= Date.now()) return null
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function formatCompactUsageNumber(value?: number): string {
  const n = Number(value || 0)
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`
  return String(n)
}

function hasUsageWindowDetail(detail?: AccountRow['usage_5h_detail']): boolean {
  return Boolean(detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0))
}

// 用量进度条颜色
function usageBarColor(pct: number): string {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 70) return 'bg-amber-500'
  return 'bg-emerald-500'
}

// 单行用量进度条
function UsageBar({ label, pct, resetAt, detail }: { label: string; pct: number; resetAt?: string; detail?: AccountRow['usage_5h_detail'] }) {
  const resetText = formatResetAt(resetAt)
  const detailText = hasUsageWindowDetail(detail)
    ? `${formatCompactUsageNumber(detail?.requests)} req / ${formatCompactUsageNumber(detail?.tokens)} tok`
    : ''
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <span className="text-[11px] font-medium text-muted-foreground w-5 shrink-0">{label}</span>
        <div className="flex-1 h-1.5 rounded-full bg-muted overflow-hidden min-w-[72px]">
          <div className={`h-full rounded-full transition-all ${usageBarColor(pct)}`} style={{ width: `${Math.min(100, pct)}%` }} />
        </div>
        <span className="text-[12px] font-semibold w-[42px] text-right shrink-0">{pct.toFixed(1)}%</span>
      </div>
      {detailText && <div className="text-[11px] font-medium text-muted-foreground mt-0.5 pl-[26px]">{detailText}</div>}
      {resetText && <div className="text-[11px] font-medium text-muted-foreground mt-0.5 pl-[26px]">⏱ {resetText}</div>}
    </div>
  )
}

function UsageWindowStat({ label, detail }: { label: string; detail?: AccountRow['usage_5h_detail'] }) {
  if (!detail || !hasUsageWindowDetail(detail)) return null

  const accountBilledText = typeof detail.account_billed === 'number' ? detail.account_billed.toFixed(4) : ''
  const userBilledText = typeof detail.user_billed === 'number' ? detail.user_billed.toFixed(4) : ''

  return (
    <div className="flex flex-col gap-0.5">
      <div className="flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
        <span className="w-5 shrink-0">{label}</span>
        <span>{formatCompactUsageNumber(detail?.requests)} req / {formatCompactUsageNumber(detail?.tokens)} tok</span>
      </div>
      {(accountBilledText || userBilledText) && (
        <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground/80 pl-6">
          {accountBilledText && <span>账号: ${accountBilledText}</span>}
          {userBilledText && <span>用户: ${userBilledText}</span>}
        </div>
      )}
    </div>
  )
}

// 用量列组件
function UsageCell({ account }: { account: AccountRow }) {
  const plan = (account.plan_type || '').toLowerCase()
  const has7d = account.usage_percent_7d !== null && account.usage_percent_7d !== undefined
  const has5h = account.usage_percent_5h !== null && account.usage_percent_5h !== undefined
  const has7dDetail = hasUsageWindowDetail(account.usage_7d_detail)
  const has5hDetail = hasUsageWindowDetail(account.usage_5h_detail)

  if (plan === 'free') {
    if (!has7d && !has7dDetail) return <span className="text-[12px] text-muted-foreground">-</span>
    return (
      <div className="w-48">
        {has7d ? (
          <UsageBar label="7d" pct={account.usage_percent_7d!} resetAt={account.reset_7d_at} detail={account.usage_7d_detail} />
        ) : (
          <UsageWindowStat label="7d" detail={account.usage_7d_detail} />
        )}
      </div>
    )
  }

  if (plan === 'pro' || plan === 'team' || plan === 'plus' || plan === 'teamplus') {
    if (!has5h && !has7d && !has5hDetail && !has7dDetail) return <span className="text-[12px] text-muted-foreground">-</span>
    return (
      <div className="w-52 space-y-1.5">
        {has5h ? (
          <UsageBar label="5h" pct={account.usage_percent_5h!} resetAt={account.reset_5h_at} detail={account.usage_5h_detail} />
        ) : (
          <UsageWindowStat label="5h" detail={account.usage_5h_detail} />
        )}
        {has7d ? (
          <UsageBar label="7d" pct={account.usage_percent_7d!} resetAt={account.reset_7d_at} detail={account.usage_7d_detail} />
        ) : (
          <UsageWindowStat label="7d" detail={account.usage_7d_detail} />
        )}
      </div>
    )
  }

  if (has7d || has7dDetail) {
    return (
      <div className="w-48">
        {has7d ? (
          <UsageBar label="7d" pct={account.usage_percent_7d!} resetAt={account.reset_7d_at} detail={account.usage_7d_detail} />
        ) : (
          <UsageWindowStat label="7d" detail={account.usage_7d_detail} />
        )}
      </div>
    )
  }
  return <span className="text-[13px] text-muted-foreground">-</span>
}

// 冷却倒计时组件
function CooldownTimer({ until }: { until: string }) {
  const [remaining, setRemaining] = useState('')

  useEffect(() => {
    const target = new Date(until).getTime()

    const update = () => {
      const diff = Math.max(0, target - Date.now())
      if (diff <= 0) {
        setRemaining('')
        return
      }
      const h = Math.floor(diff / 3600000)
      const m = Math.floor((diff % 3600000) / 60000)
      const s = Math.floor((diff % 60000) / 1000)
      if (h > 0) {
        setRemaining(`${h}h ${String(m).padStart(2, '0')}m ${String(s).padStart(2, '0')}s`)
      } else if (m > 0) {
        setRemaining(`${m}m ${String(s).padStart(2, '0')}s`)
      } else {
        setRemaining(`${s}s`)
      }
    }

    update()
    const id = setInterval(update, 1000)
    return () => clearInterval(id)
  }, [until])

  if (!remaining) return null
  return <span className="text-[11px] font-mono text-amber-600">⏳ {remaining}</span>
}
