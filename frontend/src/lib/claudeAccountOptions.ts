/**
 * Curated IANA zones used by the Claude account forms.
 *
 * The API still accepts any valid IANA name. These entries cover the common
 * operator locations while making the UTC offset explicit, so a selection is
 * understandable without memorising an IANA identifier.
 */
export const CLAUDE_TIMEZONE_CUSTOM = '__custom__'

export interface ClaudeTimezoneOption {
  value: string
  label: string
}

export const CLAUDE_TIMEZONE_OPTIONS: ClaudeTimezoneOption[] = [
  { value: 'UTC', label: 'UTC+00:00 · UTC' },
  { value: 'America/Los_Angeles', label: 'UTC−08:00 (standard) · America/Los_Angeles' },
  { value: 'America/Denver', label: 'UTC−07:00 (standard) · America/Denver' },
  { value: 'America/Chicago', label: 'UTC−06:00 (standard) · America/Chicago' },
  { value: 'America/New_York', label: 'UTC−05:00 (standard) · America/New_York' },
  { value: 'America/Sao_Paulo', label: 'UTC−03:00 · America/Sao_Paulo' },
  { value: 'Europe/London', label: 'UTC+00:00 (standard) · Europe/London' },
  { value: 'Europe/Berlin', label: 'UTC+01:00 (standard) · Europe/Berlin' },
  { value: 'Europe/Moscow', label: 'UTC+03:00 · Europe/Moscow' },
  { value: 'Asia/Dubai', label: 'UTC+04:00 · Asia/Dubai' },
  { value: 'Asia/Kolkata', label: 'UTC+05:30 · Asia/Kolkata' },
  { value: 'Asia/Bangkok', label: 'UTC+07:00 · Asia/Bangkok' },
  { value: 'Asia/Shanghai', label: 'UTC+08:00 · Asia/Shanghai' },
  { value: 'Asia/Singapore', label: 'UTC+08:00 · Asia/Singapore' },
  { value: 'Asia/Tokyo', label: 'UTC+09:00 · Asia/Tokyo' },
  { value: 'Australia/Sydney', label: 'UTC+10:00 (standard) · Australia/Sydney' },
  { value: 'Pacific/Auckland', label: 'UTC+12:00 (standard) · Pacific/Auckland' },
]

const CLAUDE_TIMEZONE_OPTION_MAP = new Map(
  CLAUDE_TIMEZONE_OPTIONS.map((option) => [option.value, option]),
)

export function findClaudeTimezoneOption(value: string | null | undefined): ClaudeTimezoneOption | undefined {
  const normalized = value?.trim() ?? ''
  if (!normalized) return undefined
  return CLAUDE_TIMEZONE_OPTION_MAP.get(normalized)
}

export function claudeTimezoneLabel(value: string | null | undefined): string {
  const normalized = value?.trim() ?? ''
  if (!normalized) return ''
  return findClaudeTimezoneOption(normalized)?.label ?? normalized
}
