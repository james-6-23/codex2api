export interface UsageRequestDiagnosticDetail {
  request_type: string
  diagnostics: Record<string, unknown> | null
}

const requestTypes = new Set(['user', 'related_internal', 'independent_internal', 'related_unclassified', 'compaction', 'gateway_internal', 'unknown'])

export function usageRequestTypeLabelKey(value?: string): string {
  if (!value) return 'usage.diagnostics.types.not_recorded'
  return `usage.diagnostics.types.${requestTypes.has(value) ? value : 'unknown'}`
}

export function diagnosticRecord(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
}

export function diagnosticEntries(value: unknown, includeMissing = false): [string, unknown][] {
  const record = diagnosticRecord(value)
  const fields = includeMissing
    ? { thread_source: '', request_kind: '', subagent_kind: '', session_id: '', thread_id: '', parent_thread_id: '', turn_id: '', root_turn_id: '', ...record }
    : record
  return Object.entries(fields).filter(([key]) => !key.startsWith('_'))
}

export function diagnosticValueText(value: unknown): string | null {
  if (value === null || value === undefined || value === '' || value === '0001-01-01T00:00:00Z') return null
  if (typeof value === 'string') return value
  if (typeof value === 'boolean' || typeof value === 'number') return String(value)
  if (Array.isArray(value)) return value.length ? value.map(String).join(', ') : null
  return JSON.stringify(value)
}
