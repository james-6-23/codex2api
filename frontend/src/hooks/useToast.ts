import { useCallback, useEffect, useRef, useState } from 'react'
import type { ToastState, ToastType } from '../types'

export function useToast(timeoutMs = 3000) {
  const [toast, setToast] = useState<ToastState | null>(null)
  const timeoutRef = useRef<number | null>(null)

  const clearToastTimer = useCallback(() => {
    if (timeoutRef.current !== null) {
      window.clearTimeout(timeoutRef.current)
      timeoutRef.current = null
    }
  }, [])

  const showToast = useCallback((msg: string, type: ToastType = 'success') => {
    clearToastTimer()
    setToast({ msg, type })
    timeoutRef.current = window.setTimeout(() => {
      setToast(null)
      timeoutRef.current = null
    }, timeoutMs)
  }, [clearToastTimer, timeoutMs])

  useEffect(() => clearToastTimer, [clearToastTimer])

  return { toast, showToast, setToast }
}
