import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'

interface PageHeaderProps {
  title: string
  description?: string
  onRefresh?: () => void
  refreshLabel?: string
  actions?: ReactNode
  actionMeta?: ReactNode
  // titleAdornment 渲染在标题文字右侧（如模式切换下拉）。
  titleAdornment?: ReactNode
}

export default function PageHeader({
  title,
  description,
  onRefresh,
  refreshLabel,
  actions,
  actionMeta,
  titleAdornment,
}: PageHeaderProps) {
  const { t } = useTranslation()
  const hasActions = Boolean(onRefresh) || Boolean(actions) || Boolean(actionMeta)
  const resolvedRefreshLabel = refreshLabel ?? t('common.refresh')

  return (
    <div data-slot="page-header" className="mb-6 flex w-full min-w-0 max-w-full items-end justify-between gap-5 max-sm:flex-col max-sm:items-stretch">
      <div className="min-w-0 max-w-[760px] max-sm:max-w-full">
        <div className="flex flex-wrap items-center gap-3">
          <h2 className="text-2xl font-semibold leading-tight text-foreground sm:text-[28px]">
            {title}
          </h2>
          {titleAdornment}
        </div>
        {description ? (
          <p className="mt-2 max-w-[640px] text-muted-foreground text-sm leading-relaxed">
            {description}
          </p>
        ) : null}
      </div>
      {hasActions ? (
        <div className="flex min-w-0 flex-col items-end gap-2 max-sm:w-full max-sm:items-stretch">
          {actionMeta ? (
            <div className="text-right text-xs text-muted-foreground max-sm:text-left">
              {actionMeta}
            </div>
          ) : null}
          <div className="flex min-w-0 flex-wrap items-center justify-end gap-2 max-sm:w-full max-sm:justify-start">
            {actions}
            {onRefresh ? (
              <Button variant="outline" onClick={onRefresh} className="max-sm:w-full">
                <RefreshCw className="size-3.5" />
                {resolvedRefreshLabel}
              </Button>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  )
}
