import type {
  AccountEventTrendPoint,
  AccountUsageDetail,
  AddAccountRequest,
  AddATAccountRequest,
  AdminErrorResponse,
  APIKeysResponse,
  AccountsResponse,
  ChartAggregation,
  CreateAccountResponse,
  CreateAPIKeyResponse,
  CreateImageJobPayload,
  HealthResponse,
  ImageAssetsResponse,
  ImagePromptTemplate,
  ImageJobResponse,
  ImageJobsResponse,
  ImagePromptTemplatePayload,
  ImagePromptTemplatesResponse,
  MessageResponse,
  ModelSyncResponse,
  ModelsResponse,
  OAuthExchangeResponse,
  OAuthURLResponse,
  OpsOverviewResponse,
  PromptFilterLogsResponse,
  PromptFilterRulesResponse,
  PromptFilterTestResponse,
  StatsResponse,
  CPAExportEntry,
  SystemSettings,
  UpdateAccountSchedulerRequest,
  UsageLogsResponse,
  UsageLogsPagedResponse,
  UsageStats,
} from './types'

const BASE = '/api/admin'
export const ADMIN_AUTH_REQUIRED_EVENT = 'codex2api:admin-auth-required'
const ADMIN_AUTH_RESET_KEY = 'admin_auth_reset_at'

export function getAdminKey(): string {
  return localStorage.getItem('admin_key') ?? ''
}

export function clearAdminKey() {
  localStorage.removeItem('admin_key')
}

export function setAdminKey(key: string) {
  if (key) {
    localStorage.setItem('admin_key', key)
  } else {
    clearAdminKey()
  }
}

export function resetAdminAuthState() {
  clearAdminKey()
  localStorage.setItem(ADMIN_AUTH_RESET_KEY, String(Date.now()))
  window.dispatchEvent(new Event(ADMIN_AUTH_REQUIRED_EVENT))
}

function extractAdminErrorMessage(body: string, status: number): string {
  if (!body.trim()) {
    return `HTTP ${status}`
  }

  try {
    const parsed = JSON.parse(body) as Partial<AdminErrorResponse>
    if (typeof parsed.error === 'string' && parsed.error.trim()) {
      return parsed.error
    }
  } catch {
    // ignore JSON parse error and fall back to raw text
  }

  return body
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (options.body !== undefined && options.body !== null && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const adminKey = getAdminKey()
  if (adminKey) {
    headers.set('X-Admin-Key', adminKey)
  }

  const res = await fetch(BASE + path, {
    ...options,
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    if (res.status === 401) {
      resetAdminAuthState()
    }
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return (await res.json()) as T
}

async function requestBlob(path: string, options: RequestInit = {}): Promise<Blob> {
  const headers = new Headers(options.headers)

  const adminKey = getAdminKey()
  if (adminKey) {
    headers.set('X-Admin-Key', adminKey)
  }

  const res = await fetch(BASE + path, {
    ...options,
    cache: options.cache ?? 'no-store',
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    if (res.status === 401) {
      resetAdminAuthState()
    }
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return res.blob()
}

export const api = {
  getStats: () => request<StatsResponse>('/stats'),
  getAccounts: () => request<AccountsResponse>('/accounts'),
  addAccount: (data: AddAccountRequest) =>
    request<CreateAccountResponse>('/accounts', { method: 'POST', body: JSON.stringify(data) }),
  addATAccount: (data: AddATAccountRequest) =>
    request<CreateAccountResponse>('/accounts/at', { method: 'POST', body: JSON.stringify(data) }),
  deleteAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}`, { method: 'DELETE' }),
  refreshAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/refresh`, { method: 'POST' }),
  updateAccountScheduler: (id: number, data: UpdateAccountSchedulerRequest) =>
    request<MessageResponse>(`/accounts/${id}/scheduler`, { method: 'PATCH', body: JSON.stringify(data) }),
  toggleAccountEnabled: (id: number, enabled: boolean) =>
    request<MessageResponse>(`/accounts/${id}/enable`, { method: 'POST', body: JSON.stringify({ enabled }) }),
  toggleAccountLock: (id: number, locked: boolean) =>
    request<MessageResponse>(`/accounts/${id}/lock`, { method: 'POST', body: JSON.stringify({ locked }) }),
  resetAccountStatus: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/reset-status`, { method: 'POST' }),
  batchResetStatus: (ids: number[]) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-reset-status', { method: 'POST', body: JSON.stringify({ ids }) }),
  getAccountUsage: (id: number) =>
    request<AccountUsageDetail>(`/accounts/${id}/usage`),
  getHealth: () => request<HealthResponse>('/health'),
  getOpsOverview: () => request<OpsOverviewResponse>('/ops/overview'),
  getUsageStats: () => request<UsageStats>('/usage/stats'),
  getUsageLogs: (params: { start?: string; end?: string; limit?: number } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start && params.end) {
      searchParams.set('start', params.start)
      searchParams.set('end', params.end)
    } else if (params.limit) {
      searchParams.set('limit', String(params.limit))
    }
    return request<UsageLogsResponse>(`/usage/logs?${searchParams.toString()}`)
  },
  getUsageLogsPaged: (params: { start: string; end: string; page: number; pageSize?: number; email?: string; model?: string; endpoint?: string; apiKeyId?: string; fast?: string; stream?: string }) => {
    const searchParams = new URLSearchParams()
    searchParams.set('start', params.start)
    searchParams.set('end', params.end)
    searchParams.set('page', String(params.page))
    if (params.pageSize) searchParams.set('page_size', String(params.pageSize))
    if (params.email) searchParams.set('email', params.email)
    if (params.model) searchParams.set('model', params.model)
    if (params.endpoint) searchParams.set('endpoint', params.endpoint)
    if (params.apiKeyId) searchParams.set('api_key_id', params.apiKeyId)
    if (params.fast) searchParams.set('fast', params.fast)
    if (params.stream) searchParams.set('stream', params.stream)
    return request<UsageLogsPagedResponse>(`/usage/logs?${searchParams.toString()}`)
  },
  getChartData: (params: { start: string; end: string; bucketMinutes: number }) => {
    const searchParams = new URLSearchParams()
    searchParams.set('start', params.start)
    searchParams.set('end', params.end)
    searchParams.set('bucket_minutes', String(params.bucketMinutes))
    return request<ChartAggregation>(`/usage/chart-data?${searchParams.toString()}`)
  },
  getAccountEventTrend: (params: { start: string; end: string; bucketMinutes: number }) => {
    const sp = new URLSearchParams()
    sp.set('start', params.start)
    sp.set('end', params.end)
    sp.set('bucket_minutes', String(params.bucketMinutes))
    return request<{ trend: AccountEventTrendPoint[] }>(`/accounts/event-trend?${sp.toString()}`)
  },
  getAPIKeys: () => request<APIKeysResponse>('/keys'),
  createAPIKey: (name: string, key?: string) =>
    request<CreateAPIKeyResponse>('/keys', {
      method: 'POST',
      body: JSON.stringify({ name, ...(key ? { key } : {}) }),
    }),
  deleteAPIKey: (id: number) =>
    request<MessageResponse>(`/keys/${id}`, { method: 'DELETE' }),
  getImagePromptTemplates: (params: { q?: string; tag?: string } = {}) => {
    const sp = new URLSearchParams()
    if (params.q) sp.set('q', params.q)
    if (params.tag) sp.set('tag', params.tag)
    const query = sp.toString()
    return request<ImagePromptTemplatesResponse>(`/image-prompts${query ? `?${query}` : ''}`)
  },
  createImagePromptTemplate: (data: ImagePromptTemplatePayload) =>
    request<{ template: ImagePromptTemplate }>('/image-prompts', { method: 'POST', body: JSON.stringify(data) }),
  updateImagePromptTemplate: (id: number, data: ImagePromptTemplatePayload) =>
    request<{ template: ImagePromptTemplate }>(`/image-prompts/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  deleteImagePromptTemplate: (id: number) =>
    request<MessageResponse>(`/image-prompts/${id}`, { method: 'DELETE' }),
  createImageJob: (data: CreateImageJobPayload) =>
    request<ImageJobResponse>('/images/jobs', { method: 'POST', body: JSON.stringify(data) }),
  getImageJobs: (params: { page?: number; pageSize?: number } = {}) => {
    const sp = new URLSearchParams()
    if (params.page) sp.set('page', String(params.page))
    if (params.pageSize) sp.set('page_size', String(params.pageSize))
    return request<ImageJobsResponse>(`/images/jobs?${sp.toString()}`)
  },
  getImageJob: (id: number, params: { includeCache?: boolean } = {}) => {
    const sp = new URLSearchParams()
    if (params.includeCache) sp.set('include_cache', '1')
    const query = sp.toString()
    return request<ImageJobResponse>(`/images/jobs/${id}${query ? `?${query}` : ''}`)
  },
  getImageAssets: (params: { page?: number; pageSize?: number } = {}) => {
    const sp = new URLSearchParams()
    if (params.page) sp.set('page', String(params.page))
    if (params.pageSize) sp.set('page_size', String(params.pageSize))
    return request<ImageAssetsResponse>(`/images/assets?${sp.toString()}`)
  },
  getImageAssetFile: (id: number, download = false, thumbKB = 0) => {
    const sp = new URLSearchParams()
    if (download) sp.set('download', '1')
    if (thumbKB > 0) sp.set('thumb_kb', String(thumbKB))
    const query = sp.toString()
    return requestBlob(`/images/assets/${id}/file${query ? `?${query}` : ''}`)
  },
  deleteImageAsset: (id: number) =>
    request<MessageResponse>(`/images/assets/${id}`, { method: 'DELETE' }),
  clearUsageLogs: () =>
    request<MessageResponse>('/usage/logs', { method: 'DELETE' }),
  getSettings: () => request<SystemSettings>('/settings'),
  updateSettings: (data: Partial<SystemSettings>) =>
    request<SystemSettings>('/settings', { method: 'PUT', body: JSON.stringify(data) }),
  getPromptFilterLogs: (params: number | { page?: number; pageSize?: number; limit?: number; source?: string; action?: string; endpoint?: string; model?: string; apiKeyId?: string; q?: string } = 100) => {
    const search = new URLSearchParams()
    if (typeof params === 'number') {
      search.set('limit', String(params))
    } else {
      if (params.page) search.set('page', String(params.page))
      if (params.pageSize) search.set('page_size', String(params.pageSize))
      if (params.limit) search.set('limit', String(params.limit))
      if (params.source) search.set('source', params.source)
      if (params.action) search.set('action', params.action)
      if (params.endpoint) search.set('endpoint', params.endpoint)
      if (params.model) search.set('model', params.model)
      if (params.apiKeyId) search.set('api_key_id', params.apiKeyId)
      if (params.q) search.set('q', params.q)
    }
    return request<PromptFilterLogsResponse>(`/prompt-filter/logs?${search.toString()}`)
  },
  clearPromptFilterLogs: () =>
    request<MessageResponse>('/prompt-filter/logs', { method: 'DELETE' }),
  testPromptFilter: (data: { text: string; endpoint?: string; model?: string }) =>
    request<PromptFilterTestResponse>('/prompt-filter/test', { method: 'POST', body: JSON.stringify(data) }),
  getPromptFilterRules: () =>
    request<PromptFilterRulesResponse>('/prompt-filter/rules'),
  getModels: () => request<ModelsResponse>('/models'),
  syncModels: () => request<ModelSyncResponse>('/models/sync', { method: 'POST' }),
  batchTestAccounts: () =>
    request<{ total: number; success: number; failed: number; banned: number; rate_limited: number }>('/accounts/batch-test', { method: 'POST' }),
  cleanBanned: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-banned', { method: 'POST' }),
  cleanRateLimited: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-rate-limited', { method: 'POST' }),
  cleanError: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-error', { method: 'POST' }),
  exportAccounts: (params: { filter: 'healthy' | 'all'; ids?: number[] }) => {
    const sp = new URLSearchParams({ filter: params.filter })
    if (params.ids && params.ids.length > 0) sp.set('ids', params.ids.join(','))
    return request<CPAExportEntry[]>(`/accounts/export?${sp.toString()}`)
  },
  downloadAccountAuthJSON: (id: number) =>
    requestBlob(`/accounts/${id}/auth-json`),
  migrateAccounts: (data: { url: string; admin_key: string }) =>
    request<{ message: string; total: number; imported: number; duplicate: number; failed: number }>(
      '/accounts/migrate', { method: 'POST', body: JSON.stringify(data) }),
  // Proxies
  listProxies: () =>
    request<{ proxies: ProxyRow[] }>('/proxies'),
  addProxies: (data: { urls?: string[]; url?: string; label?: string }) =>
    request<{ message: string; inserted: number; total: number }>('/proxies', { method: 'POST', body: JSON.stringify(data) }),
  deleteProxy: (id: number) =>
    request<MessageResponse>(`/proxies/${id}`, { method: 'DELETE' }),
  updateProxy: (id: number, data: { label?: string; enabled?: boolean }) =>
    request<MessageResponse>(`/proxies/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  batchDeleteProxies: (ids: number[]) =>
    request<{ message: string; deleted: number }>('/proxies/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) }),
  testProxy: (url: string, id?: number, lang?: string) =>
    request<ProxyTestResult>('/proxies/test', { method: 'POST', body: JSON.stringify({ url, id, lang }) }),
  // OAuth
  generateOAuthURL: (data: { proxy_url?: string; redirect_uri?: string }) =>
    request<OAuthURLResponse>('/oauth/generate-auth-url', { method: 'POST', body: JSON.stringify(data) }),
  exchangeOAuthCode: (data: { session_id: string; code: string; state: string; name?: string; proxy_url?: string }) =>
    request<OAuthExchangeResponse>('/oauth/exchange-code', { method: 'POST', body: JSON.stringify(data) }),
}

export interface ProxyRow {
  id: number
  url: string
  label: string
  enabled: boolean
  created_at: string
  test_ip: string
  test_location: string
  test_latency_ms: number
}

export interface ProxyTestResult {
  success: boolean
  ip?: string
  country?: string
  region?: string
  city?: string
  isp?: string
  latency_ms?: number
  location?: string
  error?: string
}
