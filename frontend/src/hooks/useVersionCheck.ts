import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { SystemUpdateInfo } from '../types'

const REMOTE_RELEASE_CACHE_KEY = 'codex2api_latest_release'
const LEGACY_UPDATE_CACHE_KEY = 'codex2api_latest_version'
const CACHE_TTL = 10 * 60 * 1000
const POLL_INTERVAL = 30 * 60 * 1000

interface RemoteReleaseInfo {
  latest_version: string
  release_url?: string
  published_at?: string
}

interface CachedRemoteReleaseInfo extends RemoteReleaseInfo {
  checkedAt: number
}

function versionLabel(version?: string | null): string | null {
  if (!version) return null
  return version.startsWith('v') || version.startsWith('V') ? version : `v${version}`
}

function normalizeVersion(version?: string | null): string {
  return (version || '')
    .trim()
    .replace(/^refs\/tags\//i, '')
    .replace(/^v/i, '')
}

function parseVersion(version: string): [number, number, number] | null {
  const normalized = normalizeVersion(version).split(/[+-]/, 1)[0]
  if (!normalized) return null
  const parts = normalized.split('.')
  if (parts.length === 0 || parts.length > 3) return null

  const parsed: [number, number, number] = [0, 0, 0]
  for (let i = 0; i < parts.length; i += 1) {
    if (!/^\d+$/.test(parts[i])) return null
    parsed[i] = Number(parts[i])
  }
  return parsed
}

function compareVersions(a: string, b: string): number {
  const parsedA = parseVersion(a)
  const parsedB = parseVersion(b)
  if (!parsedA && !parsedB) return normalizeVersion(a).localeCompare(normalizeVersion(b))
  if (!parsedA) return -1
  if (!parsedB) return 1

  for (let i = 0; i < 3; i += 1) {
    if (parsedA[i] < parsedB[i]) return -1
    if (parsedA[i] > parsedB[i]) return 1
  }
  return 0
}

function hasRemoteUpdate(latestVersion?: string | null): boolean {
  if (!latestVersion || __APP_VERSION__ === 'dev') return false
  return compareVersions(__APP_VERSION__, latestVersion) < 0
}

function remoteReleaseFromInfo(info: Pick<SystemUpdateInfo, 'latest_version' | 'release_url' | 'published_at'>): RemoteReleaseInfo {
  return {
    latest_version: info.latest_version,
    release_url: info.release_url,
    published_at: info.published_at,
  }
}

function clearLegacyUpdateCache() {
  try {
    localStorage.removeItem(LEGACY_UPDATE_CACHE_KEY)
  } catch {
    // ignore localStorage failures
  }
}

function readCachedRemoteRelease(ignoreTTL = false): RemoteReleaseInfo | null {
  clearLegacyUpdateCache()
  try {
    const raw = localStorage.getItem(REMOTE_RELEASE_CACHE_KEY)
    if (!raw) return null

    const cached = JSON.parse(raw) as Partial<CachedRemoteReleaseInfo>
    if (typeof cached.latest_version !== 'string' || !cached.latest_version.trim()) return null
    if (typeof cached.checkedAt !== 'number') return null
    if (!ignoreTTL && Date.now() - cached.checkedAt >= CACHE_TTL) return null

    return {
      latest_version: cached.latest_version.trim(),
      release_url: typeof cached.release_url === 'string' ? cached.release_url : undefined,
      published_at: typeof cached.published_at === 'string' ? cached.published_at : undefined,
    }
  } catch {
    return null
  }
}

function writeCachedRemoteRelease(remote: RemoteReleaseInfo) {
  clearLegacyUpdateCache()
  try {
    localStorage.setItem(REMOTE_RELEASE_CACHE_KEY, JSON.stringify({ ...remote, checkedAt: Date.now() }))
  } catch {
    // ignore localStorage failures
  }
}

async function fetchUpdateInfo(forceNetwork = false): Promise<{ remote: RemoteReleaseInfo; info: SystemUpdateInfo | null } | null> {
  if (!forceNetwork) {
    const cached = readCachedRemoteRelease()
    if (cached) return { remote: cached, info: null }
  }

  try {
    const info = await api.getSystemUpdate()
    const remote = remoteReleaseFromInfo(info)
    writeCachedRemoteRelease(remote)
    return { remote, info }
  } catch {
    const cached = readCachedRemoteRelease(true)
    return cached ? { remote: cached, info: null } : null
  }
}

export function useVersionCheck(triggerKey?: string) {
  const [updateInfo, setUpdateInfo] = useState<SystemUpdateInfo | null>(null)
  const [latestVersion, setLatestVersion] = useState<string | null>(null)
  const [hasUpdate, setHasUpdate] = useState(false)
  const lastTriggerRef = useRef<string | undefined>(undefined)

  const check = useCallback(async (forceNetwork = false) => {
    if (__APP_VERSION__ === 'dev') return

    const result = await fetchUpdateInfo(forceNetwork)
    if (!result) {
      setUpdateInfo(null)
      setLatestVersion(null)
      setHasUpdate(false)
      return
    }

    const remoteLatestVersion = result.remote.latest_version
    setUpdateInfo((current) => {
      if (result.info) return result.info
      if (current && normalizeVersion(current.latest_version) === normalizeVersion(remoteLatestVersion)) {
        return current
      }
      return null
    })
    setLatestVersion(versionLabel(remoteLatestVersion))
    setHasUpdate(hasRemoteUpdate(remoteLatestVersion))
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
