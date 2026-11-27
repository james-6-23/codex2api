export function normalizeVersion(version?: string | null): string {
  return (version || '')
    .trim()
    .replace(/^refs\/tags\//i, '')
    .replace(/^v/i, '')
}

export function versionLabel(version?: string | null): string | null {
  if (!version) return null
  return version.startsWith('v') || version.startsWith('V') ? version : `v${version}`
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

function isReleaseVersion(version?: string | null): boolean {
  const match = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(normalizeVersion(version))
  if (!match || !match.slice(1, 4).every((part) => Number.isSafeInteger(Number(part)))) return false
  return !match[4]?.split('.').some((part) => /^0\d+$/.test(part))
}

export function resolveBuildVersions(frontendVersion: string, backendVersion?: string | null) {
  const versionMismatch = isReleaseVersion(frontendVersion)
    && isReleaseVersion(backendVersion)
    && normalizeVersion(frontendVersion) !== normalizeVersion(backendVersion)
  // Release-like numeric identities keep the legacy fallback when malformed.
  const backendIdentity = normalizeVersion(backendVersion)
  const customBackend = isReleaseVersion(frontendVersion)
    && /^[0-9a-z._-]{1,64}$/i.test(backendIdentity) && !/^\d+(?:[.+-]|$)/.test(backendIdentity)

  return {
    frontendVersion,
    // Preserve the existing release label when it already matches the service.
    currentVersion: customBackend ? backendVersion!.trim() : versionMismatch ? versionLabel(normalizeVersion(backendVersion))! : frontendVersion,
    versionMismatch,
  }
}

export function isComparableVersion(version: string): boolean {
  return parseVersion(version) !== null
}

function compareVersions(a: string, b: string): number {
  const parsedA = parseVersion(a)
  const parsedB = parseVersion(b)
  // A custom build has no known ordering relative to an upstream release.
  if (!parsedA || !parsedB) return 0

  for (let i = 0; i < 3; i += 1) {
    if (parsedA[i] < parsedB[i]) return -1
    if (parsedA[i] > parsedB[i]) return 1
  }
  return 0
}

export function hasRemoteUpdate(currentVersion: string, latestVersion?: string | null): boolean {
  if (!latestVersion || currentVersion === 'dev') return false
  return compareVersions(currentVersion, latestVersion) < 0
}
