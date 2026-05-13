import type { ChangeEvent, FormEvent, KeyboardEvent, ReactNode } from 'react'
import { useCallback, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import Modal from '../components/Modal'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import ToastNotice from '../components/ToastNotice'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { APIKeyRow } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
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
import {
  Check,
  Copy,
  CalendarClock,
  CircleDollarSign,
  Eye,
  EyeOff,
  Fingerprint,
  KeyRound,
  LockKeyhole,
  Pencil,
  Plus,
  ShieldCheck,
  Trash2,
  X,
} from 'lucide-react'

type ExpireMode = 'never' | '7' | '30' | '90' | 'custom'

interface CreateKeyFormState {
  name: string
  key: string
  quotaLimit: string
  expireMode: ExpireMode
  expiresAt: string
}

const initialCreateForm: CreateKeyFormState = {
  name: '',
  key: '',
  quotaLimit: '',
  expireMode: 'never',
  expiresAt: '',
}

export default function APIKeys() {
  const { t } = useTranslation()
  const [createDialogOpen, setCreateDialogOpen] = useState(false)
  const [createForm, setCreateForm] = useState<CreateKeyFormState>(initialCreateForm)
  const [createdKeyId, setCreatedKeyId] = useState<number | null>(null)
  const [visibleKeys, setVisibleKeys] = useState<Set<number>>(new Set())
  const [creating, setCreating] = useState(false)
  const [deletingIds, setDeletingIds] = useState<Set<number>>(new Set())
  const [editingId, setEditingId] = useState<number | null>(null)
  const [editingName, setEditingName] = useState('')
  const [saving, setSaving] = useState(false)
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()

  const loadKeys = useCallback(async () => {
    const response = await api.getAPIKeys()
    return response.keys ?? []
  }, [])

  const { data: keys, loading, error, reload } = useDataLoader<APIKeyRow[]>({
    initialData: [],
    load: loadKeys,
  })

  const latestKey = useMemo(() => {
    return keys
      .slice()
      .sort((a, b) => new Date(b.created_at || 0).getTime() - new Date(a.created_at || 0).getTime())[0]
  }, [keys])

  const expireOptions = useMemo(
    () => [
      { label: t('apiKeys.expireNever'), value: 'never' },
      { label: t('apiKeys.expire7Days'), value: '7' },
      { label: t('apiKeys.expire30Days'), value: '30' },
      { label: t('apiKeys.expire90Days'), value: '90' },
      { label: t('apiKeys.expireCustom'), value: 'custom' },
    ],
    [t]
  )

  const updateCreateForm = (patch: Partial<CreateKeyFormState>) => {
    setCreateForm((current) => ({ ...current, ...patch }))
  }

  const closeCreateDialog = () => {
    if (creating) return
    setCreateDialogOpen(false)
  }

  const handleCreateKey = async (event?: FormEvent<HTMLFormElement>) => {
    event?.preventDefault()
    setCreating(true)
    try {
      const quotaLimitText = createForm.quotaLimit.trim()
      let quotaLimit: number | undefined
      if (quotaLimitText) {
        quotaLimit = Number(quotaLimitText)
        if (!Number.isFinite(quotaLimit) || quotaLimit < 0) {
          showToast(t('apiKeys.quotaInvalid'), 'error')
          return
        }
      }

      const payload = {
        name: createForm.name.trim() || t('apiKeys.defaultName'),
        ...(createForm.key.trim() ? { key: createForm.key.trim() } : {}),
        ...(quotaLimit && quotaLimit > 0 ? { quota_limit: quotaLimit } : {}),
        ...buildExpirationPayload(createForm, t),
      }

      const result = await api.createAPIKey(payload)
      setCreatedKeyId(result.id)
      setVisibleKeys((prev) => new Set(prev).add(result.id))
      setCreateForm(initialCreateForm)
      setCreateDialogOpen(false)
      showToast(t('apiKeys.keyCreateSuccess'))
      void reload()
    } catch (error) {
      showToast(`${t('apiKeys.createFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setCreating(false)
    }
  }

  const handleDeleteKey = async (id: number) => {
    const confirmed = await confirm({
      title: t('apiKeys.deleteKeyTitle'),
      description: t('apiKeys.deleteKeyDesc'),
      confirmText: t('apiKeys.confirmDelete'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!confirmed) return

    setDeletingIds((prev) => new Set(prev).add(id))
    try {
      await api.deleteAPIKey(id)
      showToast(t('apiKeys.keyDeleted'))
      if (createdKeyId === id) setCreatedKeyId(null)
      setVisibleKeys((prev) => {
        const next = new Set(prev)
        next.delete(id)
        return next
      })
      void reload()
    } catch (error) {
      showToast(`${t('apiKeys.deleteFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setDeletingIds((prev) => {
        const next = new Set(prev)
        next.delete(id)
        return next
      })
    }
  }

  const handleCopy = async (text: string) => {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text)
        showToast(t('common.copied'))
        return
      }

      const textarea = document.createElement('textarea')
      textarea.value = text
      textarea.setAttribute('readonly', 'true')
      textarea.style.position = 'fixed'
      textarea.style.opacity = '0'
      textarea.style.pointerEvents = 'none'
      document.body.appendChild(textarea)
      textarea.select()
      textarea.setSelectionRange(0, text.length)
      const copied = document.execCommand('copy')
      document.body.removeChild(textarea)

      if (!copied) throw new Error('copy failed')
      showToast(t('common.copied'))
    } catch {
      showToast(t('common.copyFailed'), 'error')
    }
  }

  const toggleVisible = (id: number) => {
    setVisibleKeys((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const handleRenameKey = async (id: number) => {
    const trimmed = editingName.trim()
    if (!trimmed) return
    setSaving(true)
    try {
      await api.updateAPIKey(id, { name: trimmed })
      showToast(t('apiKeys.keyRenamed')) // TODO: Track B adds i18n key apiKeys.keyRenamed
      setEditingId(null)
      void reload()
    } catch (error) {
      showToast(`${t('apiKeys.renameFailed')}: ${getErrorMessage(error)}`, 'error') // TODO: Track B adds i18n key apiKeys.renameFailed
    } finally {
      setSaving(false)
    }
  }

  const startEditing = (keyRow: APIKeyRow) => {
    setEditingId(keyRow.id)
    setEditingName(keyRow.name)
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('apiKeys.loadingTitle')}
      loadingDescription={t('apiKeys.loadingDesc')}
      errorTitle={t('apiKeys.errorTitle')}
    >
      <>
        <PageHeader
          title={t('apiKeys.title')}
          description={t('apiKeys.description')}
          onRefresh={() => void reload()}
          actions={
            <Button onClick={() => setCreateDialogOpen(true)} className="max-sm:w-full">
              <Plus className="size-3.5" />
              {t('apiKeys.createKey')}
            </Button>
          }
        />

        <div className="mb-4 grid gap-3 md:grid-cols-3">
          <KeySummaryCard
            icon={<KeyRound className="size-5" />}
            label={t('apiKeys.totalKeys')}
            value={String(keys.length)}
            sub={keys.length > 0 ? t('apiKeys.totalKeysDesc') : t('apiKeys.noKeysShort')}
            tone="info"
          />
          <KeySummaryCard
            icon={<ShieldCheck className="size-5" />}
            label={t('apiKeys.authMode')}
            value={keys.length > 0 ? t('apiKeys.authEnabled') : t('apiKeys.authDisabled')}
            sub={keys.length > 0 ? t('apiKeys.authEnabledDesc') : t('apiKeys.authDisabledDesc')}
            tone={keys.length > 0 ? 'success' : 'warning'}
          />
          <KeySummaryCard
            icon={<Fingerprint className="size-5" />}
            label={t('apiKeys.newestKey')}
            value={latestKey?.name || '-'}
            sub={latestKey ? formatRelativeTime(latestKey.created_at, { variant: 'compact' }) : t('apiKeys.noLatest')}
            tone="neutral"
          />
        </div>

        <div className="space-y-4">
          <Card>
            <CardContent className="p-4">
              <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                <div>
                  <h3 className="text-base font-semibold text-foreground">{t('apiKeys.tableTitle')}</h3>
                  <p className="mt-1 text-sm text-muted-foreground">{t('apiKeys.tableDesc')}</p>
                </div>
                <Badge variant={keys.length > 0 ? 'default' : 'secondary'}>
                  {t('apiKeys.keyCount', { count: keys.length })}
                </Badge>
              </div>

              <StateShell
                variant="section"
                isEmpty={keys.length === 0}
                emptyTitle={t('apiKeys.noKeys')}
                emptyDescription={t('apiKeys.noKeysDesc')}
              >
                <div className="data-table-shell">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t('common.name')}</TableHead>
                        <TableHead>{t('apiKeys.keyColumn')}</TableHead>
                        <TableHead>{t('apiKeys.quotaColumn')}</TableHead>
                        <TableHead>{t('apiKeys.expiresColumn')}</TableHead>
                        <TableHead>{t('common.createdAt')}</TableHead>
                        <TableHead className="text-right">{t('common.actions')}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {keys.map((keyRow) => {
                        const isVisible = visibleKeys.has(keyRow.id)
                        const isNew = createdKeyId === keyRow.id
                        const displayKey = isVisible ? keyRow.raw_key : keyRow.key
                        const copyValue = keyRow.raw_key || keyRow.key
                        const status = getAPIKeyStatus(keyRow)
                        return (
                          <TableRow key={keyRow.id} className={isNew ? 'bg-[hsl(var(--success-bg))]' : ''}>
                            <TableCell className="font-medium text-foreground">
                              {editingId === keyRow.id ? (
                                <div className="flex items-center gap-1.5">
                                  <Input
                                    className="h-7 w-40 text-[13px]"
                                    value={editingName}
                                    onChange={(e: ChangeEvent<HTMLInputElement>) => setEditingName(e.target.value)}
                                    onKeyDown={(e: KeyboardEvent<HTMLInputElement>) => {
                                      if (e.key === 'Enter') void handleRenameKey(keyRow.id)
                                      if (e.key === 'Escape') setEditingId(null)
                                    }}
                                    autoFocus
                                    disabled={saving}
                                  />
                                  <Button variant="ghost" size="icon-xs" onClick={() => void handleRenameKey(keyRow.id)} disabled={saving || !editingName.trim()}>
                                    <Check className="size-3.5" />
                                  </Button>
                                  <Button variant="ghost" size="icon-xs" onClick={() => setEditingId(null)} disabled={saving}>
                                    <X className="size-3.5" />
                                  </Button>
                                </div>
                              ) : (
                                <div className="flex items-center gap-2">
                                  <span>{keyRow.name}</span>
                                  {isNew ? (
                                    <Badge variant="outline" className="border-transparent bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]">
                                      {t('apiKeys.newBadge')}
                                    </Badge>
                                  ) : null}
                                  {status !== 'active' ? (
                                    <Badge variant={status === 'expired' ? 'secondary' : 'destructive'}>
                                      {t(`apiKeys.status.${status}`)}
                                    </Badge>
                                  ) : null}
                                  <Button variant="ghost" size="icon-xs" onClick={() => startEditing(keyRow)} title={t('apiKeys.renameKey')}> {/* TODO: Track B adds i18n key apiKeys.renameKey */}
                                    <Pencil className="size-3" />
                                  </Button>
                                </div>
                              )}
                            </TableCell>
                            <TableCell>
                              <div className="flex min-w-[260px] items-center gap-2">
                                <code className="min-w-0 max-w-[420px] truncate rounded-md bg-muted px-2 py-1 font-mono text-[13px] text-foreground" title={displayKey}>
                                  {displayKey}
                                </code>
                                <Button
                                  variant="ghost"
                                  size="icon-xs"
                                  onClick={() => toggleVisible(keyRow.id)}
                                  title={isVisible ? t('apiKeys.hideKey') : t('apiKeys.showKey')}
                                >
                                  {isVisible ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
                                </Button>
                                <Button
                                  variant="ghost"
                                  size="icon-xs"
                                  onClick={() => void handleCopy(copyValue)}
                                  title={t('common.copy')}
                                >
                                  <Copy className="size-3.5" />
                                </Button>
                              </div>
                            </TableCell>
                            <TableCell className="min-w-[150px] text-sm text-muted-foreground">
                              <div className="space-y-1">
                                <div className="font-medium text-foreground">{formatQuotaLimit(keyRow, t)}</div>
                                {keyRow.quota_limit > 0 ? (
                                  <div className="h-1.5 w-28 overflow-hidden rounded-full bg-muted">
                                    <div
                                      className="h-full rounded-full bg-primary"
                                      style={{ width: `${Math.min(100, Math.max(0, (keyRow.quota_used / keyRow.quota_limit) * 100))}%` }}
                                    />
                                  </div>
                                ) : null}
                              </div>
                            </TableCell>
                            <TableCell className="text-muted-foreground">
                              {formatExpiration(keyRow, t)}
                            </TableCell>
                            <TableCell className="text-muted-foreground">
                              {formatRelativeTime(keyRow.created_at, { variant: 'compact' })}
                            </TableCell>
                            <TableCell>
                              <div className="flex justify-end">
                                <Button
                                  variant="destructive"
                                  size="sm"
                                  disabled={deletingIds.has(keyRow.id)}
                                  onClick={() => void handleDeleteKey(keyRow.id)}
                                >
                                  <Trash2 className="size-3.5" />
                                  {t('common.delete')}
                                </Button>
                              </div>
                            </TableCell>
                          </TableRow>
                        )
                      })}
                    </TableBody>
                  </Table>
                </div>
              </StateShell>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center sm:justify-between">
              <div className="flex min-w-0 items-start gap-3">
                <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                  <LockKeyhole className="size-4" />
                </div>
                <div className="min-w-0">
                  <div className="text-sm font-semibold text-foreground">{t('apiKeys.securityTitle')}</div>
                  <p className="mt-1 text-sm leading-relaxed text-muted-foreground">{t('apiKeys.keyAuthNote')}</p>
                </div>
              </div>
              <Button variant="outline" onClick={() => setCreateDialogOpen(true)} className="shrink-0">
                <Plus className="size-3.5" />
                {t('apiKeys.createKey')}
              </Button>
            </CardContent>
          </Card>
        </div>

        <Modal
          show={createDialogOpen}
          title={t('apiKeys.createTitle')}
          onClose={closeCreateDialog}
          contentClassName="sm:max-w-[620px]"
          footer={
            <>
              <Button type="button" variant="outline" onClick={closeCreateDialog} disabled={creating}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="create-api-key-form" disabled={creating}>
                <Plus className="size-3.5" />
                {creating ? t('apiKeys.creating') : t('apiKeys.createKey')}
              </Button>
            </>
          }
        >
          <form id="create-api-key-form" className="space-y-5" onSubmit={(event) => void handleCreateKey(event)}>
            <div className="flex items-start gap-3 rounded-lg border border-border bg-muted/20 p-3">
              <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                <Plus className="size-4" />
              </div>
              <p className="text-sm leading-relaxed text-muted-foreground">{t('apiKeys.createDesc')}</p>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <FormField label={t('apiKeys.nameLabel')}>
                <Input
                  placeholder={t('apiKeys.keyNamePlaceholder')}
                  value={createForm.name}
                  onChange={(event: ChangeEvent<HTMLInputElement>) => updateCreateForm({ name: event.target.value })}
                />
              </FormField>
              <FormField label={t('apiKeys.keyLabel')}>
                <Input
                  placeholder={t('apiKeys.keyValuePlaceholder')}
                  value={createForm.key}
                  onChange={(event: ChangeEvent<HTMLInputElement>) => updateCreateForm({ key: event.target.value })}
                />
              </FormField>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <FormField label={t('apiKeys.quotaLimitLabel')} icon={<CircleDollarSign className="size-3.5" />}>
                <Input
                  type="number"
                  min="0"
                  step="0.000001"
                  inputMode="decimal"
                  placeholder={t('apiKeys.quotaLimitPlaceholder')}
                  value={createForm.quotaLimit}
                  onChange={(event: ChangeEvent<HTMLInputElement>) => updateCreateForm({ quotaLimit: event.target.value })}
                />
              </FormField>
              <FormField label={t('apiKeys.expireModeLabel')} icon={<CalendarClock className="size-3.5" />}>
                <Select
                  value={createForm.expireMode}
                  onValueChange={(value) => updateCreateForm({ expireMode: value as ExpireMode })}
                  options={expireOptions}
                  compact
                />
              </FormField>
            </div>

            {createForm.expireMode === 'custom' ? (
              <FormField label={t('apiKeys.expiresAtLabel')}>
                <Input
                  type="datetime-local"
                  value={createForm.expiresAt}
                  onChange={(event: ChangeEvent<HTMLInputElement>) => updateCreateForm({ expiresAt: event.target.value })}
                />
              </FormField>
            ) : null}
          </form>
        </Modal>

        <ToastNotice toast={toast} />
        {confirmDialog}
      </>
    </StateShell>
  )
}

type Translator = (key: string, options?: Record<string, unknown>) => string

function buildExpirationPayload(form: CreateKeyFormState, t: Translator): { expires_in_days?: number; expires_at?: string } {
  if (form.expireMode === 'never') return {}
  if (form.expireMode !== 'custom') {
    return { expires_in_days: Number(form.expireMode) }
  }
  if (!form.expiresAt) {
    throw new Error(t('apiKeys.expiresAtRequired'))
  }
  const date = new Date(form.expiresAt)
  if (!Number.isFinite(date.getTime())) {
    throw new Error(t('apiKeys.expiresAtInvalid'))
  }
  if (date.getTime() <= Date.now()) {
    throw new Error(t('apiKeys.expiresAtPast'))
  }
  return { expires_at: date.toISOString() }
}

function getAPIKeyStatus(keyRow: APIKeyRow): 'active' | 'expired' | 'quota_exhausted' {
  if (keyRow.status === 'expired' || keyRow.status === 'quota_exhausted') {
    return keyRow.status
  }
  if (keyRow.expires_at && new Date(keyRow.expires_at).getTime() <= Date.now()) {
    return 'expired'
  }
  if (keyRow.quota_limit > 0 && keyRow.quota_used >= keyRow.quota_limit) {
    return 'quota_exhausted'
  }
  return 'active'
}

function formatQuotaLimit(keyRow: APIKeyRow, t: Translator) {
  if (!keyRow.quota_limit || keyRow.quota_limit <= 0) {
    return t('apiKeys.unlimited')
  }
  return t('apiKeys.quotaUsedOfLimit', {
    used: formatUSD(keyRow.quota_used),
    limit: formatUSD(keyRow.quota_limit),
  })
}

function formatExpiration(keyRow: APIKeyRow, t: Translator) {
  if (!keyRow.expires_at) {
    return t('apiKeys.neverExpires')
  }
  return formatBeijingTime(keyRow.expires_at)
}

function formatUSD(value: number) {
  if (!Number.isFinite(value)) return '$0'
  if (value >= 1) return `$${value.toFixed(2)}`
  if (value >= 0.01) return `$${value.toFixed(4)}`
  return `$${value.toFixed(6)}`
}

function FormField({
  label,
  icon,
  children,
}: {
  label: string
  icon?: ReactNode
  children: ReactNode
}) {
  return (
    <label className="block min-w-0">
      <span className="mb-1.5 flex items-center gap-1.5 text-xs font-semibold text-muted-foreground">
        {icon}
        {label}
      </span>
      {children}
    </label>
  )
}

function KeySummaryCard({
  icon,
  label,
  value,
  sub,
  tone,
}: {
  icon: ReactNode
  label: string
  value: string
  sub: string
  tone: 'neutral' | 'info' | 'success' | 'warning'
}) {
  const toneClassName = {
    neutral: 'bg-muted text-muted-foreground',
    info: 'bg-primary/10 text-primary',
    success: 'bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]',
    warning: 'bg-[hsl(var(--warning-bg))] text-[hsl(var(--warning))]',
  }[tone]

  return (
    <Card className="py-0">
      <CardContent className="flex items-center justify-between gap-3 p-4">
        <div className="min-w-0">
          <div className="text-[11px] font-bold uppercase text-muted-foreground">{label}</div>
          <div className="mt-2 truncate text-[24px] font-bold leading-none text-foreground">{value}</div>
          <div className="mt-2 truncate text-xs text-muted-foreground">{sub}</div>
        </div>
        <div className={`flex size-10 shrink-0 items-center justify-center rounded-lg ${toneClassName}`}>
          {icon}
        </div>
      </CardContent>
    </Card>
  )
}
