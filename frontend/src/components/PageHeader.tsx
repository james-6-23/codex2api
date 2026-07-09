import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'

interface PageHeaderProps {
  title: string
  description?: string
  onRefresh?: () => void
  refreshLabel?: string
  actions?: ReactNode
  actionMeta?: ReactNode
  // titleAdornment 渲染在标题文字右侧（如模式切换下拉）。
  titleAdornment?: ReactNode
  className?: string
}

export default function PageHeader({
  title,
  description,
  onRefresh,
  refreshLabel,
  actions,
  actionMeta,
  titleAdornment,
  className,
}: PageHeaderProps) {
  const { t } = useTranslation()
  const hasActions = Boolean(onRefresh) || Boolean(actions) || Boolean(actionMeta)
  const resolvedRefreshLabel = refreshLabel ?? t('common.refresh')

  return (
    <div
      data-slot="page-header"
      className={cn(
        'mb-4 flex flex-col gap-3 sm:mb-6 sm:flex-row sm:items-end sm:justify-between sm:gap-5',
        className,
      )}
    >
      <div className="min-w-0 max-w-[760px]">
        <div className="flex flex-wrap items-center gap-2 sm:gap-3">
          <h2 className="text-[22px] font-semibold leading-tight tracking-tight text-foreground sm:text-[28px]">
            {title}
          </h2>
          {titleAdornment}
        </div>
        {description ? (
          <p className="mt-1 max-w-[640px] text-[13px] leading-relaxed text-muted-foreground sm:mt-2 sm:text-sm">
            {description}
          </p>
        ) : null}
      </div>
      {hasActions ? (
        <div className="flex min-w-0 flex-col gap-2 sm:items-end">
          {actionMeta ? (
            <div className="text-left text-xs text-muted-foreground sm:text-right">
              {actionMeta}
            </div>
          ) : null}
          <div className="flex min-w-0 flex-wrap items-center gap-1.5 sm:justify-end sm:gap-2">
            {actions}
            {onRefresh ? (
              <Button
                variant="outline"
                size="sm"
                onClick={onRefresh}
                className="shrink-0"
              >
                <RefreshCw className="size-3.5" />
                <span className="max-[380px]:hidden">{resolvedRefreshLabel}</span>
              </Button>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  )
}
