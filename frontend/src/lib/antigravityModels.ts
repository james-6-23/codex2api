// Fallback only: account-synchronized model discovery takes precedence.
// These are public gateway IDs, never Cloud Code's private backing IDs.
export const ANTIGRAVITY_DEFAULT_MODELS = [
  'gemini-3.8-flash-low', 'gemini-3.8-flash-medium', 'gemini-3.8-flash-high',
  'gemini-3.7-flash-low', 'gemini-3.7-flash-medium', 'gemini-3.7-flash-high',
  'gemini-3.6-flash-low', 'gemini-3.6-flash-medium', 'gemini-3.6-flash-high',
  'gemini-3.5-flash-low', 'gemini-3.5-flash-medium', 'gemini-3.5-flash-high',
  'gemini-3.1-pro-low', 'gemini-3.1-pro-high',
  'claude-opus-4-6-thinking', 'claude-sonnet-4-6', 'gpt-oss-120b-medium',
]
