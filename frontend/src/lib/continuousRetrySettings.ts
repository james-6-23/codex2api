const CONTINUOUS_RETRY_ERROR_CODE_PATTERN = /^[a-z0-9_.-]+$/
const CONTINUOUS_RETRY_DEFAULT_MAX_DURATION_SECONDS = 600

export function createContinuousRetrySaveQueue() {
  let tail: Promise<void> = Promise.resolve()

  return function enqueue<T>(task: () => Promise<T>): Promise<T> {
    const result = tail.then(task, task)
    tail = result.then(
      () => undefined,
      () => undefined,
    )
    return result
  }
}

export function buildContinuousRetryEnabledPatch(enabled: boolean) {
  return enabled
    ? { continuous_retry_enabled: true }
    : { continuous_retry_enabled: false, continuous_retry_catch_all: false }
}

export function buildContinuousRetryCatchAllPatch(enabled: boolean) {
  return enabled
    ? { continuous_retry_enabled: true, continuous_retry_catch_all: true }
    : { continuous_retry_catch_all: false }
}

export function parseContinuousRetryStatusCodes(raw: string): number[] {
  const values = raw
    .split(',')
    .map((value) => value.trim())
    .filter((value) => /^\d{3}$/.test(value))
    .map((value) => Number(value))
    .filter((value) => value >= 100 && value <= 599)

  return Array.from(new Set(values)).sort((a, b) => a - b)
}

export function parseContinuousRetryErrorCodes(raw: string): string[] {
  const values = raw
    .split(',')
    .map((value) => value.trim().toLowerCase())
    .filter((value) => value.length > 0 && value.length <= 128)
    .filter((value) => CONTINUOUS_RETRY_ERROR_CODE_PATTERN.test(value))

  return Array.from(new Set(values)).sort()
}

export function parseContinuousRetryMaxDurationSeconds(raw: string): number {
  const value = Number(raw)
  if (!Number.isFinite(value) || value === 0) {
    return CONTINUOUS_RETRY_DEFAULT_MAX_DURATION_SECONDS
  }
  return Math.min(900, Math.max(1, Math.trunc(value)))
}
