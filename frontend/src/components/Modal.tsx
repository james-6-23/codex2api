import type { ReactNode } from 'react'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from '@/components/ui/dialog'
import { cn } from '@/lib/utils'

interface ModalProps {
  show: boolean
  title: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  contentClassName?: string
  bodyClassName?: string
  titleClassName?: string
  showCloseButton?: boolean
}

export default function Modal({
  show,
  title,
  onClose,
  children,
  footer,
  contentClassName,
  bodyClassName,
  titleClassName,
  showCloseButton = true,
}: ModalProps) {
  return (
    <Dialog open={show} onOpenChange={(open) => { if (!open) onClose() }}>
      <DialogContent
        showCloseButton={showCloseButton}
        className={cn(
          'max-h-[calc(100dvh-1.5rem-env(safe-area-inset-top,0px)-env(safe-area-inset-bottom,0px))] overflow-hidden p-0 sm:max-w-[520px]',
          contentClassName
        )}
      >
        <div className="flex max-h-[calc(100dvh-1.5rem-env(safe-area-inset-top,0px)-env(safe-area-inset-bottom,0px))] min-w-0 flex-col">
          <DialogHeader className="min-w-0 shrink-0 border-b px-5 pt-5 pb-3.5 pr-12 sm:px-6 sm:pt-6 sm:pb-4">
            <DialogTitle className={cn('min-w-0 text-lg leading-snug break-all sm:text-xl', titleClassName)}>
              {title}
            </DialogTitle>
          </DialogHeader>
          <div className={cn('min-h-0 flex-1 overflow-y-auto px-5 py-4 sm:px-6', bodyClassName)}>{children}</div>
          {footer ? (
            <DialogFooter className="shrink-0 border-t px-5 py-3.5 sm:px-6 sm:py-4 safe-pb">
              {footer}
            </DialogFooter>
          ) : null}
        </div>
      </DialogContent>
    </Dialog>
  )
}
