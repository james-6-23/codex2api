import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { SystemUpdateInfo } from '../types'
import { hasRemoteUpdate, isComparableVersion, normalizeVersion, resolveBuildVersions, versionLabel } from '../lib/versionCheck'

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

async function fetchUpdateInfo(
  forceNetwork = false,
  forceUpstream = false,
): Promise<{ remote: RemoteReleaseInfo; info: SystemUpdateInfo | null } | null> {
  if (!forceNetwork && !forceUpstream) {
    const cached = readCachedRemoteRelease()
    if (cached) return { remote: cached, info: null }
  }

  try {
    const info = await api.getSystemUpdate(forceUpstream ? { force: true } : undefined)
    const remote = remoteReleaseFromInfo(info)
    return { remote, info }
  } catch {
    if (forceUpstream) return null
    const cached = readCachedRemoteRelease(true)
    return cached ? { remote: cached, info: null } : null
  }
}

export interface VersionRefreshOptions {
  includeDev?: boolean
  forceUpstream?: boolean
}

export function useVersionCheck(triggerKey?: string) {
  const [updateInfo, setUpdateInfo] = useState<SystemUpdateInfo | null>(null)
  const [latestVersion, setLatestVersion] = useState<string | null>(null)
  const [backendVersion, setBackendVersion] = useState<string | null>(null)
  const [healthResolved, setHealthResolved] = useState(false)
  const activeRef = useRef(false)
  const healthRequestRef = useRef(0)
  const updateRequestRef = useRef(0)
  const lastTriggerRef = useRef<string | undefined>(undefined)
  const versions = resolveBuildVersions(__APP_VERSION__, backendVersion)
  const hasUpdate = healthResolved && hasRemoteUpdate(versions.currentVersion, latestVersion)
  const isCustomBuild = !isComparableVersion(versions.currentVersion)

  const check = useCallback(async (forceNetwork = false, options?: VersionRefreshOptions): Promise<SystemUpdateInfo | null> => {
    const includeDev = Boolean(options?.includeDev)
    const forceUpstream = Boolean(options?.forceUpstream)
    if ((!includeDev && __APP_VERSION__ === 'dev') || !activeRef.current) return null

    const healthRequest = ++healthRequestRef.current
    const updateRequest = ++updateRequestRef.current
    // Runtime identity is independent of both the release cache and GitHub.
    const healthPending = (async () => {
      let runtimeVersion: string | null = null
      try {
        const health = await api.getHealth({ timeoutMs: 5_000 })
        runtimeVersion = typeof health.build_version === 'string' ? health.build_version : null
      } catch {
        // Legacy servers and failed probes keep the frontend build label.
      }
      if (activeRef.current && healthRequest === healthRequestRef.current) {
        setBackendVersion(runtimeVersion)
        setHealthResolved(true)
      }
    })()

    const updatePending = (async (): Promise<SystemUpdateInfo | null> => {
      const result = await fetchUpdateInfo(forceNetwork, forceUpstream)
      if (!activeRef.current || updateRequest !== updateRequestRef.current) return null
      if (!result) {
        if (!forceUpstream) {
          setUpdateInfo(null)
          setLatestVersion(null)
        }
        return null
      }

      if (result.info) writeCachedRemoteRelease(result.remote)
      const remoteLatestVersion = result.remote.latest_version
      setUpdateInfo((current) => {
        if (result.info) return result.info
        if (current && normalizeVersion(current.latest_version) === normalizeVersion(remoteLatestVersion)) {
          return current
        }
        return null
      })
      setLatestVersion(versionLabel(remoteLatestVersion))
      return result.info
    })()

    const [, info] = await Promise.all([healthPending, updatePending])
    return info
  }, [])

  useEffect(() => {
    activeRef.current = true
    void check()
    const timer = setInterval(() => void check(), POLL_INTERVAL)
    return () => {
      activeRef.current = false
      healthRequestRef.current += 1
      updateRequestRef.current += 1
      clearInterval(timer)
    }
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

  return { ...versions, isCustomBuild, hasUpdate, latestVersion, updateInfo, refreshVersion: check }
}
