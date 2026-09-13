export const USAGE_SEARCH_SCOPES = ['all', 'session', 'request', 'error', 'model', 'endpoint', 'account', 'newapi_user', 'ip', 'user_agent', 'api_key'] as const
export type UsageSearchScope = typeof USAGE_SEARCH_SCOPES[number]
