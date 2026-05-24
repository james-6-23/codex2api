import { createContext, useCallback, useContext, useEffect, useRef, useState, type PropsWithChildren } from 'react'
import type { ToastState, ToastType } from '../types'
import ToastNotice from './ToastNotice'

const DEFAULT_TOAST_MS = 4500

interface ToastContextValue {
  toast: ToastState | null
  showToast: (msg: string, type?: ToastType, timeoutMs?: number) => void
  setToast: (toast: ToastState | null) => void
}

const ToastContext = createContext<ToastContextValue | null>(null)

export function ToastProvider({ children, defaultTimeoutMs = DEFAULT_TOAST_MS }: PropsWithChildren<{ defaultTimeoutMs?: number }>) {
  const [toast, setToast] = useState<ToastState | null>(null)
  const timeoutRef = useRef<number | null>(null)

  const clearToastTimer = useCallback(() => {
    if (timeoutRef.current !== null) {
      window.clearTimeout(timeoutRef.current)
      timeoutRef.current = null
    }
  }, [])

  const showToast = useCallback<ToastContextValue['showToast']>((msg, type = 'success', timeoutMs) => {
    clearToastTimer()
    setToast({ msg, type })
    const ms = timeoutMs ?? defaultTimeoutMs
    timeoutRef.current = window.setTimeout(() => {
      setToast(null)
      timeoutRef.current = null
    }, ms)
  }, [clearToastTimer, defaultTimeoutMs])

  useEffect(() => clearToastTimer, [clearToastTimer])

  return (
    <ToastContext.Provider value={{ toast, showToast, setToast }}>
      {children}
      <ToastNotice toast={toast} />
    </ToastContext.Provider>
  )
}

export function useToastContext(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (!ctx) {
    throw new Error('useToastContext must be used inside <ToastProvider>')
  }
  return ctx
}
