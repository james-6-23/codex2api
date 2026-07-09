import { AlertCircle, CheckCircle2, Info, TriangleAlert } from 'lucide-react'
import type { ToastState, ToastType } from '../types'

const toneByType: Record<ToastType, string> = {
  success:
    'border-emerald-500/25 bg-emerald-500/12 text-emerald-800 shadow-[0_12px_28px_rgba(16,185,129,0.14)] dark:border-emerald-500/25 dark:bg-emerald-500/14 dark:text-emerald-200',
  error:
    'border-red-500/25 bg-red-500/12 text-red-800 shadow-[0_12px_28px_rgba(239,68,68,0.14)] dark:border-red-500/25 dark:bg-red-500/14 dark:text-red-200',
  warning:
    'border-amber-500/25 bg-amber-500/12 text-amber-900 shadow-[0_12px_28px_rgba(245,158,11,0.14)] dark:border-amber-500/25 dark:bg-amber-500/14 dark:text-amber-200',
  info:
    'border-sky-500/25 bg-sky-500/12 text-sky-900 shadow-[0_12px_28px_rgba(14,165,233,0.14)] dark:border-sky-500/25 dark:bg-sky-500/14 dark:text-sky-200',
}

const iconByType: Record<ToastType, typeof CheckCircle2> = {
  success: CheckCircle2,
  error: AlertCircle,
  warning: TriangleAlert,
  info: Info,
}

export default function ToastNotice({ toast }: { toast: ToastState | null }) {
  if (!toast) return null

  const type: ToastType = toast.type || 'success'
  const Icon = iconByType[type] || CheckCircle2

  return (
    <div
      className={`pointer-events-none fixed z-[2000] flex max-w-[min(360px,calc(100vw-1.5rem))] items-start gap-2.5 rounded-2xl border px-3.5 py-3 text-[13px] font-medium backdrop-blur-xl
        top-4 right-4
        max-lg:top-auto max-lg:right-3 max-lg:left-3 max-lg:bottom-[calc(5.5rem+env(safe-area-inset-bottom,0px))] max-lg:max-w-none
        ${toneByType[type]}`}
      style={{ animation: 'toast-slide-in 0.22s ease' }}
      role="status"
      aria-live="polite"
    >
      <span className="mt-0.5 shrink-0 opacity-90">
        <Icon className="size-4" />
      </span>
      <span className="min-w-0 flex-1 break-words leading-5">{toast.msg}</span>
    </div>
  )
}
