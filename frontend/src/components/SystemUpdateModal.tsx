import {
  ArrowDown,
  ArrowUpCircle,
  Calendar,
  CheckCircle2,
  Container,
  Cpu,
  Download,
  ExternalLink,
  FileText,
  Info,
  Loader2,
  RefreshCw,
  Server,
  Terminal,
  TriangleAlert,
  X,
  XCircle,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { SystemUpdateInfo } from '../types'
import { buildVersionLabel } from '../lib/buildVersion'
import { Button } from '@/components/ui/button'
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import { cn } from '@/lib/utils'

interface SystemUpdateModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  siteName: string
  logoSrc?: string
  currentVersion: string
  frontendVersion: string
  versionMismatch: boolean
  isCustomBuild: boolean
  hasUpdate: boolean
  latestVersion: string | null
  updateInfo: SystemUpdateInfo | null
  releaseURL?: string
  checking: boolean
  updatingVersion: boolean
  restartingAfterUpdate: boolean
  updateError: string | null
  canApplyUpdate: boolean
  onCheckUpdates: () => void
  onApplyUpdate: () => void
}

function safeReleaseHref(value: string): string {
  try {
    const parsed = new URL(value, 'https://github.com')
    if (parsed.protocol === 'https:' || parsed.protocol === 'http:') {
      return parsed.toString()
    }
  } catch {
    // ignore malformed URLs
  }
  return ''
}

function formatReleaseDate(value?: string): string | null {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return null
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

export default function SystemUpdateModal({
  open,
  onOpenChange,
  siteName,
  logoSrc,
  currentVersion,
  frontendVersion,
  versionMismatch,
  isCustomBuild,
  hasUpdate,
  latestVersion,
  updateInfo,
  releaseURL,
  checking,
  updatingVersion,
  restartingAfterUpdate,
  updateError,
  canApplyUpdate,
  onCheckUpdates,
  onApplyUpdate,
}: SystemUpdateModalProps) {
  const { t } = useTranslation()

  const resolvedBuildType = updateInfo?.build_type || (isCustomBuild
    ? (currentVersion === 'dev' ? 'dev' : 'custom')
    : 'release')

  const buildTypeLabel = resolvedBuildType === 'release'
    ? t('systemUpdate.buildTypes.release')
    : resolvedBuildType === 'dev'
      ? t('systemUpdate.buildTypes.dev')
      : t('systemUpdate.buildTypes.custom')

  const resolvedDeploymentMode = updateInfo?.deployment_mode || (updateInfo?.mode === 'binary' ? 'binary' : undefined)
  const deploymentLabel = resolvedDeploymentMode === 'docker'
    ? t('systemUpdate.deploymentModes.docker')
    : resolvedDeploymentMode === 'binary'
      ? t('systemUpdate.deploymentModes.binary')
      : '-'

  const runtimePlatform = updateInfo?.runtime_os && updateInfo?.runtime_arch
    ? `${updateInfo.runtime_os}/${updateInfo.runtime_arch}`
    : undefined

  const publishedDate = formatReleaseDate(updateInfo?.published_at)

  const statusBadge = (() => {
    if (restartingAfterUpdate) {
      return {
        label: t('common.restarting'),
        icon: RefreshCw,
        spin: true,
        dot: 'bg-sky-500 animate-pulse',
        className: 'border-sky-500/20 bg-sky-500/10 text-sky-700 dark:text-sky-300',
      }
    }
    if (updatingVersion) {
      return {
        label: t('common.updating'),
        icon: RefreshCw,
        spin: true,
        dot: 'bg-sky-500 animate-pulse',
        className: 'border-sky-500/20 bg-sky-500/10 text-sky-700 dark:text-sky-300',
      }
    }
    if (checking) {
      return {
        label: t('systemUpdate.status.checking'),
        icon: RefreshCw,
        spin: true,
        dot: 'bg-sky-500 animate-pulse',
        className: 'border-sky-500/20 bg-sky-500/10 text-sky-700 dark:text-sky-300',
      }
    }
    if (updateError) {
      return {
        label: t('common.updateFailed'),
        icon: XCircle,
        spin: false,
        dot: 'bg-destructive',
        className: 'border-destructive/25 bg-destructive/10 text-destructive',
      }
    }
    if (isCustomBuild) {
      return {
        label: resolvedBuildType === 'dev' ? t('systemUpdate.buildTypes.dev') : t('common.versionCustomBuild'),
        icon: Cpu,
        spin: false,
        dot: 'bg-muted-foreground/70',
        className: 'border-border/80 bg-muted/70 text-muted-foreground',
      }
    }
    if (hasUpdate) {
      return {
        label: t('systemUpdate.status.available'),
        icon: ArrowUpCircle,
        spin: false,
        dot: 'bg-emerald-500 animate-pulse',
        className: 'border-emerald-500/25 bg-emerald-500/12 text-emerald-700 dark:text-emerald-300',
      }
    }
    if (updateInfo?.warning) {
      return {
        label: t('systemUpdate.status.warning'),
        icon: TriangleAlert,
        spin: false,
        dot: 'bg-amber-500',
        className: 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300',
      }
    }
    if (latestVersion) {
      return {
        label: t('common.versionLatest'),
        icon: CheckCircle2,
        spin: false,
        dot: 'bg-emerald-500',
        className: 'border-emerald-500/20 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
      }
    }
    return {
      label: t('systemUpdate.status.notChecked'),
      icon: CheckCircle2,
      spin: false,
      dot: 'bg-muted-foreground/60',
      className: 'border-border/80 bg-muted/70 text-muted-foreground',
    }
  })()

  const StatusIcon = statusBadge.icon
  const DeploymentIcon = resolvedDeploymentMode === 'docker' ? Container : Server
  const releaseNotes = updateInfo?.release_notes?.trim() || ''
  const updateUnavailableReason = updateInfo?.unsupported_reason
  const showInstallButton = hasUpdate || updatingVersion || restartingAfterUpdate
  const normalizedLatestDisplay = latestVersion || (updateInfo?.latest_version ? `v${updateInfo.latest_version.replace(/^v/i, '')}` : '-')

  return (
    <Dialog
      open={open}
      onOpenChange={(nextOpen) => {
        if (!nextOpen && restartingAfterUpdate) return
        onOpenChange(nextOpen)
      }}
    >
      <DialogContent
        id="version-details"
        showCloseButton={false}
        onOpenAutoFocus={(event) => {
          event.preventDefault()
        }}
        onEscapeKeyDown={(event) => {
          if (restartingAfterUpdate) event.preventDefault()
        }}
        onPointerDownOutside={(event) => {
          if (restartingAfterUpdate) event.preventDefault()
        }}
        onInteractOutside={(event) => {
          if (restartingAfterUpdate) event.preventDefault()
        }}
        className="grid max-h-[min(88dvh,680px)] grid-cols-1 gap-0 overflow-y-auto bg-card p-0 sm:max-w-[56rem] sm:p-0 md:h-[min(84dvh,648px)] md:grid-cols-[296px_minmax(0,1fr)] md:overflow-hidden"
      >
        {/* Left inspector rail */}
        <aside
          className={cn(
            'flex flex-col border-b border-border/65 bg-muted/35 md:h-full md:overflow-y-auto md:border-r md:border-b-0 dark:bg-white/[0.025]',
            hasUpdate && 'bg-gradient-to-b from-emerald-500/[0.07] via-muted/30 to-muted/35',
          )}
        >
          {/* Brand header */}
          <div className="flex items-start justify-between gap-3 px-5 pt-4 pb-3.5">
            <div className="flex min-w-0 items-center gap-3">
              <div className="relative flex size-10 shrink-0 items-center justify-center rounded-xl border border-border/75 bg-card shadow-xs">
                {logoSrc ? (
                  <img src={logoSrc} alt={siteName} className="size-7 rounded-lg object-cover" />
                ) : (
                  <ArrowUpCircle className="size-5 text-primary" />
                )}
                {hasUpdate ? (
                  <span
                    aria-hidden="true"
                    className="absolute -top-1 -right-1 flex size-3 items-center justify-center rounded-full bg-emerald-500 ring-2 ring-card"
                  >
                    <span className="size-1.5 rounded-full bg-white" />
                  </span>
                ) : null}
              </div>

              <div className="min-w-0">
                <DialogTitle className="truncate text-[15.5px] leading-tight font-semibold tracking-tight text-foreground">
                  {t('systemUpdate.title')}
                </DialogTitle>
                <p className="mt-0.5 truncate text-xs text-muted-foreground">
                  {siteName}
                </p>
              </div>
            </div>

            {!restartingAfterUpdate ? (
              <DialogClose
                aria-label={t('common.close')}
                className="flex size-8 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-muted/80 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-hidden md:hidden"
              >
                <X className="size-4" />
              </DialogClose>
            ) : null}
          </div>

          {/* Version transition card */}
          <div className="px-5">
            <div className="rounded-xl border border-border/75 bg-card p-3.5 shadow-2xs">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-medium text-muted-foreground">
                  {t('systemUpdate.currentVersionLabel')}
                </span>
                <span
                  className={cn(
                    'inline-flex h-5 shrink-0 items-center gap-1.5 rounded-full border px-2 text-[11px] font-semibold',
                    statusBadge.className,
                  )}
                >
                  {statusBadge.spin ? (
                    <StatusIcon className="size-3 animate-spin" />
                  ) : (
                    <span aria-hidden="true" className={cn('size-1.5 rounded-full', statusBadge.dot)} />
                  )}
                  {statusBadge.label}
                </span>
              </div>

              <p
                className="mt-1 truncate font-mono text-[17px] leading-snug font-bold tracking-tight text-foreground tabular-nums"
                title={currentVersion}
              >
                {buildVersionLabel(currentVersion)}
              </p>

              <div className="my-2.5 flex items-center gap-2">
                <div className="h-px flex-1 bg-border/70" />
                <span
                  aria-hidden="true"
                  className={cn(
                    'flex size-5 items-center justify-center rounded-full border text-muted-foreground',
                    hasUpdate
                      ? 'border-emerald-500/30 bg-emerald-500/12 text-emerald-600 dark:text-emerald-400'
                      : 'border-border/75 bg-muted/50',
                  )}
                >
                  <ArrowDown className="size-3" />
                </span>
                <div className="h-px flex-1 bg-border/70" />
              </div>

              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-medium text-muted-foreground">
                  {t('systemUpdate.latestVersionLabel')}
                </span>
                {releaseURL && !checking ? (
                  <a
                    href={releaseURL}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex shrink-0 items-center gap-0.5 text-[11px] font-medium text-primary transition-colors hover:underline"
                  >
                    {t('systemUpdate.releasePage')}
                    <ExternalLink className="size-2.5" />
                  </a>
                ) : null}
              </div>

              {checking && !latestVersion ? (
                <div className="mt-1.5 h-5 w-20 animate-pulse rounded bg-muted" />
              ) : (
                <p
                  className={cn(
                    'mt-1 truncate font-mono text-[17px] leading-snug font-bold tracking-tight tabular-nums',
                    hasUpdate ? 'text-emerald-600 dark:text-emerald-400' : 'text-foreground',
                  )}
                  title={normalizedLatestDisplay}
                >
                  {normalizedLatestDisplay}
                </p>
              )}
            </div>
          </div>

          {/* Environment specs */}
          <dl className="mt-3 divide-y divide-border/55 px-5 text-xs">
            <div className="flex items-center justify-between gap-3 py-2">
              <dt className="inline-flex items-center gap-1.5 text-muted-foreground">
                <Cpu className="size-3.5 shrink-0 opacity-70" />
                {t('systemUpdate.buildLabel')}
              </dt>
              <dd className="truncate font-medium text-foreground">{buildTypeLabel}</dd>
            </div>

            <div className="flex items-center justify-between gap-3 py-2">
              <dt className="inline-flex items-center gap-1.5 text-muted-foreground">
                <DeploymentIcon className="size-3.5 shrink-0 opacity-70" />
                {t('systemUpdate.deploymentLabel')}
              </dt>
              <dd className="truncate font-medium text-foreground">
                {checking && !updateInfo ? (
                  <span className="inline-block h-3.5 w-12 animate-pulse rounded bg-muted align-middle" />
                ) : (
                  deploymentLabel
                )}
              </dd>
            </div>

            {runtimePlatform ? (
              <div className="flex items-center justify-between gap-3 py-2">
                <dt className="inline-flex items-center gap-1.5 text-muted-foreground">
                  <Terminal className="size-3.5 shrink-0 opacity-70" />
                  {t('systemUpdate.platformLabel')}
                </dt>
                <dd className="truncate font-mono text-[11.5px] font-medium text-foreground">{runtimePlatform}</dd>
              </div>
            ) : null}

            {publishedDate ? (
              <div className="flex items-center justify-between gap-3 py-2">
                <dt className="inline-flex items-center gap-1.5 text-muted-foreground">
                  <Calendar className="size-3.5 shrink-0 opacity-70" />
                  {t('systemUpdate.publishedAtLabel')}
                </dt>
                <dd className="truncate font-mono text-[11.5px] font-medium text-foreground tabular-nums">
                  <time dateTime={updateInfo?.published_at}>{publishedDate}</time>
                </dd>
              </div>
            ) : null}

            <div className="flex items-center justify-between gap-3 py-2">
              <dt className="inline-flex items-center gap-1.5 text-muted-foreground">
                <span aria-hidden="true" className="mx-1 size-1.5 rounded-full bg-emerald-500" />
                {t('systemUpdate.channelLabel')}
              </dt>
              <dd className="truncate font-medium text-foreground">{t('systemUpdate.channelStable')}</dd>
            </div>
          </dl>

          {/* Contextual notices tucked cleanly into left rail */}
          {(isCustomBuild || versionMismatch || updateError || (hasUpdate && updateUnavailableReason) || updateInfo?.warning) ? (
            <div className="mt-auto space-y-2 px-5 pt-2.5 pb-4">
              {updateError ? (
                <div
                  role="alert"
                  className="flex items-start gap-2 rounded-lg border border-destructive/25 bg-destructive/10 px-3 py-2 text-[11.5px] leading-relaxed font-medium text-destructive"
                >
                  <XCircle className="mt-0.5 size-3.5 shrink-0" />
                  <span className="min-w-0 break-words">{updateError}</span>
                </div>
              ) : null}

              {versionMismatch ? (
                <div
                  role="status"
                  className="flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/[0.08] px-3 py-2 text-[11.5px] leading-relaxed text-amber-800 dark:text-amber-300"
                >
                  <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
                  <div className="min-w-0">
                    <div className="font-medium">{t('common.versionMismatch')}</div>
                    <div className="mt-0.5 font-mono text-[11px] opacity-85">
                      {t('common.frontendBuildVersion', { version: frontendVersion })}
                    </div>
                  </div>
                </div>
              ) : null}

              {hasUpdate && updateUnavailableReason ? (
                <div
                  role="status"
                  className="flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/[0.08] px-3 py-2 text-[11.5px] leading-relaxed font-medium text-amber-800 dark:text-amber-300"
                >
                  <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
                  <span className="min-w-0 break-words">{updateUnavailableReason}</span>
                </div>
              ) : null}

              {isCustomBuild ? (
                <div
                  role="status"
                  className="flex items-start gap-2 rounded-lg border border-border/70 bg-card/70 px-3 py-2 text-[11.5px] leading-relaxed text-muted-foreground"
                >
                  <Info className="mt-0.5 size-3.5 shrink-0 opacity-75" />
                  <span className="min-w-0 break-words">{t('common.versionCustomBuildHint')}</span>
                </div>
              ) : null}

              {updateInfo?.warning ? (
                <div
                  role="status"
                  className="flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/[0.08] px-3 py-2 text-[11.5px] leading-relaxed text-amber-800 dark:text-amber-300"
                >
                  <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
                  <span className="min-w-0 break-words">{updateInfo.warning}</span>
                </div>
              ) : null}
            </div>
          ) : (
            <div className="h-5 shrink-0" />
          )}
        </aside>

        {/* Right editorial release notes & actions pane */}
        <div className="flex min-w-0 flex-col bg-card md:h-full md:overflow-hidden">
          {/* Right pane header */}
          <div className="flex shrink-0 items-start justify-between gap-4 border-b border-border/60 px-5 py-4 sm:px-6">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <FileText className="size-4 shrink-0 text-primary" />
                <h3 className="text-[15px] leading-snug font-semibold tracking-tight text-foreground">
                  {t('systemUpdate.releaseNotes')}
                </h3>
                <span className="inline-flex items-center rounded-md border border-border/75 bg-muted/50 px-2 py-0.5 font-mono text-[11px] font-semibold text-foreground/85">
                  {checking && !latestVersion ? (
                    <span className="inline-block h-3 w-11 animate-pulse rounded bg-muted align-middle" />
                  ) : (
                    normalizedLatestDisplay
                  )}
                </span>
              </div>
              <DialogDescription className="mt-1 text-xs text-muted-foreground">
                {t('systemUpdate.description')}
              </DialogDescription>
            </div>

            {!restartingAfterUpdate ? (
              <DialogClose
                aria-label={t('common.close')}
                className="hidden size-8 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-muted/80 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-hidden md:flex"
              >
                <X className="size-4" />
              </DialogClose>
            ) : null}
          </div>

          {/* Spacious release notes reader */}
          <div className="max-h-[300px] min-h-0 flex-1 overflow-y-auto px-5 py-5 sm:px-6 md:max-h-none">
            {checking && !releaseNotes ? (
              <div className="space-y-3 py-1" aria-busy="true" aria-label={t('systemUpdate.status.checking')}>
                <div className="h-16 w-full animate-pulse rounded-xl bg-muted/70" />
                <div className="h-4 w-32 animate-pulse rounded bg-muted" />
                <div className="h-3.5 w-11/12 animate-pulse rounded bg-muted/80" />
                <div className="h-3.5 w-4/5 animate-pulse rounded bg-muted/80" />
                <div className="h-3.5 w-3/4 animate-pulse rounded bg-muted/80" />
              </div>
            ) : releaseNotes ? (
              <div className="min-w-0 space-y-3 break-words text-[13.5px] leading-[1.72] text-muted-foreground [&_blockquote]:my-1 [&_blockquote]:rounded-xl [&_blockquote]:border [&_blockquote]:border-primary/15 [&_blockquote]:bg-primary/[0.045] [&_blockquote]:px-4 [&_blockquote]:py-3 [&_blockquote]:text-[13px] [&_blockquote]:leading-[1.68] [&_blockquote]:text-foreground/90 [&_code]:rounded [&_code]:bg-muted/85 [&_code]:px-1.5 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-[12px] [&_code]:text-foreground/90 [&_h1]:mt-5 [&_h1]:mb-2 [&_h1]:border-b [&_h1]:border-border/55 [&_h1]:pb-1.5 [&_h1]:text-[14px] [&_h1]:font-semibold [&_h1]:text-foreground [&_h1]:first:mt-0 [&_h2]:mt-5 [&_h2]:mb-2.5 [&_h2]:border-b [&_h2]:border-border/55 [&_h2]:pb-1.5 [&_h2]:text-[13.5px] [&_h2]:font-semibold [&_h2]:tracking-tight [&_h2]:text-foreground [&_h2]:first:mt-0 [&_h3]:mt-3.5 [&_h3]:mb-1.5 [&_h3]:text-[13px] [&_h3]:font-semibold [&_h3]:text-foreground [&_h3]:first:mt-0 [&_li]:my-1.5 [&_li]:marker:text-muted-foreground/55 [&_ol]:list-decimal [&_ol]:pl-5 [&_pre]:overflow-x-auto [&_pre]:rounded-xl [&_pre]:border [&_pre]:border-border/60 [&_pre]:bg-muted/55 [&_pre]:p-3.5 [&_strong]:font-semibold [&_strong]:text-foreground [&_ul]:list-disc [&_ul]:pl-4.5">
                <Markdown
                  skipHtml
                  remarkPlugins={[remarkGfm]}
                  urlTransform={safeReleaseHref}
                  components={{
                    a: ({ href, children }) =>
                      href ? (
                        <a
                          href={href}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="font-medium text-primary underline-offset-2 hover:underline"
                        >
                          {children}
                        </a>
                      ) : (
                        <span>{children}</span>
                      ),
                  }}
                >
                  {releaseNotes}
                </Markdown>
              </div>
            ) : (
              <div className="flex h-full min-h-[160px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-border/75 bg-muted/20 px-6 py-8 text-center">
                <FileText className="size-6 text-muted-foreground/50" />
                <p className="text-xs text-muted-foreground">{t('systemUpdate.emptyReleaseNotes')}</p>
                {releaseURL ? (
                  <a
                    href={releaseURL}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="mt-1 inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
                  >
                    {t('common.viewReleaseNotes')}
                    <ExternalLink className="size-3" />
                  </a>
                ) : null}
              </div>
            )}
          </div>

          {/* Docked action bar */}
          <div className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-t border-border/60 bg-muted/20 px-5 py-3.5 sm:px-6">
            <span className="text-xs text-muted-foreground">
              {t('systemUpdate.releaseSource')}
            </span>

            <div className="ml-auto flex items-center gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={checking || updatingVersion || restartingAfterUpdate}
                onClick={onCheckUpdates}
              >
                <RefreshCw className={cn('size-3.5', checking && 'animate-spin')} />
                {t('systemUpdate.actions.check')}
              </Button>

              {showInstallButton ? (
                <Button
                  type="button"
                  size="sm"
                  disabled={!canApplyUpdate || updatingVersion || restartingAfterUpdate}
                  title={!canApplyUpdate ? updateUnavailableReason : undefined}
                  onClick={onApplyUpdate}
                >
                  {restartingAfterUpdate ? (
                    <RefreshCw className="size-3.5 animate-spin" />
                  ) : updatingVersion ? (
                    <Loader2 className="size-3.5 animate-spin" />
                  ) : (
                    <Download className="size-3.5" />
                  )}
                  {restartingAfterUpdate
                    ? t('common.restarting')
                    : updatingVersion
                      ? t('common.updating')
                      : t('common.updateNow')}
                </Button>
              ) : null}
            </div>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
