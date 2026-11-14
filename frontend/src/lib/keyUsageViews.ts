export const KEY_USAGE_VIEWS = ['overview', 'quota', 'accounts', 'logs'] as const
export type KeyUsageView = typeof KEY_USAGE_VIEWS[number]

export function normalizeKeyUsageView(value?: string): KeyUsageView {
  return KEY_USAGE_VIEWS.includes(value as KeyUsageView) ? value as KeyUsageView : 'overview'
}
