import type { APIKeyModelRequestLimit } from '../types'

export const MAX_MODEL_REQUEST_RULES = 64
// Keep this in sync with database.modelRequestPattern. Only * is a wildcard.
const MODEL_REQUEST_PATTERN = /^[A-Za-z0-9_./:+*\-]{1,200}$/

export interface ModelRequestLimitFormState {
  id?: string
  model: string
  maxRequests: string
  timezone: string
  resetWeekday: number
  resetTime: string
}

export function newModelRequestLimitRow(): ModelRequestLimitFormState {
  return {
    model: '',
    maxRequests: '',
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
    resetWeekday: 1,
    resetTime: '00:00',
  }
}

export function modelRequestLimitsFromAPIKey(rules?: APIKeyModelRequestLimit[]): ModelRequestLimitFormState[] {
  return (rules ?? []).map((rule) => ({
    id: rule.id,
    model: rule.model,
    maxRequests: String(rule.max_requests),
    timezone: rule.timezone,
    resetWeekday: rule.reset_weekday,
    resetTime: rule.reset_time,
  }))
}

export function modelRequestLimitsToPayload(
  rows: ModelRequestLimitFormState[],
  translate: (key: string) => string = (key) => key,
): APIKeyModelRequestLimit[] {
  if (rows.length > MAX_MODEL_REQUEST_RULES) {
    throw new Error(translate('modelRequests.tooManyRules'))
  }
  return rows.map((row) => {
    const model = row.model.trim()
    const maxRequests = Number(row.maxRequests.trim())
    const timezone = row.timezone.trim()
    if (!MODEL_REQUEST_PATTERN.test(model)) throw new Error(translate('modelRequests.invalidModel'))
    if (!Number.isSafeInteger(maxRequests) || maxRequests <= 0) {
      throw new Error(translate('modelRequests.invalidLimit'))
    }
    if (!timezone || timezone === 'Local') throw new Error(translate('modelRequests.invalidTimezone'))
    if (!Number.isInteger(row.resetWeekday) || row.resetWeekday < 1 || row.resetWeekday > 7 || !/^([01]\d|2[0-3]):[0-5]\d$/.test(row.resetTime)) {
      throw new Error(translate('modelRequests.invalidReset'))
    }
    // The server validates timezone names and rule ownership. Never invent IDs:
    // existing IDs keep their usage ledger; new rules receive one on save.
    return {
      ...(row.id ? { id: row.id } : {}),
      model,
      window: 'week',
      max_requests: maxRequests,
      timezone,
      reset_weekday: row.resetWeekday,
      reset_time: row.resetTime,
    }
  })
}
