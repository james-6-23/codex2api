import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { SystemUpdateInfo } from '../types'

const CACHE_KEY = 'codex2api_latest_version'
const CACHE_TTL = 10 * 60 * 1000
const POLL_INTERVAL = 30 * 60 * 1000

interface CachedUpdateInfo {
  info: SystemUpdateInfo
  checkedAt: number
}

function versionLabel(version?: string | null): string | null {
  if (!version) return null
  return version.startsWith('v') || version.startsWith('V') ? version : `v${version}`
}

function readCachedInfo(ignoreTTL = false): SystemUpdateInfo | null {
  try {
    const raw = localStorage.getItem(CACHE_KEY)
    if (!raw) return null
    const cached = JSON.parse(raw) as Partial<CachedUpdateInfo>
    if (!ignoreTTL && typeof cached.checkedAt === 'number' && Date.now() - cached.checkedAt >= CACHE_TTL) {
      return null
    }
    return cached.info?.latest_version ? cached.info : null
  } catch {
    return null
  }
}

function writeCachedInfo(info: SystemUpdateInfo) {
  try {
    localStorage.setItem(CACHE_KEY, JSON.stringify({ info, checkedAt: Date.now() }))
  } catch {
    // ignore localStorage write failures
  }
}

async function fetchUpdateInfo(forceNetwork = false): Promise<SystemUpdateInfo | null> {
  if (!forceNetwork) {
    const cached = readCachedInfo()
    if (cached) return cached
  }

  try {
    const info = await api.getSystemUpdate()
    writeCachedInfo(info)
    return info
  } catch {
    return readCachedInfo(true)
  }
}

export function useVersionCheck(triggerKey?: string) {
  const [updateInfo, setUpdateInfo] = useState<SystemUpdateInfo | null>(null)
  const [latestVersion, setLatestVersion] = useState<string | null>(null)
  const [hasUpdate, setHasUpdate] = useState(false)
  const lastTriggerRef = useRef<string | undefined>(undefined)

  const check = useCallback(async (forceNetwork = false) => {
    if (__APP_VERSION__ === 'dev') return

    const info = await fetchUpdateInfo(forceNetwork)
    if (!info) return

    setUpdateInfo(info)
    setLatestVersion(versionLabel(info.latest_version))
    setHasUpdate(Boolean(info.has_update))
  }, [])

  useEffect(() => {
    void check()
    const timer = setInterval(() => void check(), POLL_INTERVAL)
    return () => clearInterval(timer)
  }, [check])

  useEffect(() => {
    if (triggerKey === undefined) return
    if (lastTriggerRef.current === undefined) {
      lastTriggerRef.current = triggerKey
      return
    }
    if (lastTriggerRef.current === triggerKey) return
    lastTriggerRef.current = triggerKey
    void check(true)
  }, [check, triggerKey])

  return { hasUpdate, latestVersion, updateInfo, refreshVersion: check }
}
