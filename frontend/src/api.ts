import type {
  AccountEventTrendPoint,
  AccountUsageDetail,
  AddAccountRequest,
  AddATAccountRequest,
  AddOpenAIResponsesAccountRequest,
  AdminErrorResponse,
  APIKeysResponse,
  APIKeyTokenStat,
  AccountsResponse,
  ChartAggregation,
  CreateAccountResponse,
  CreateAPIKeyResponse,
  CreateAPIKeyRequest,
  CodexAuditReport,
  FetchOpenAIResponsesModelsRequest,
  FetchOpenAIResponsesModelsResponse,
  CreateImageJobPayload,
  HealthResponse,
  ImageAssetsResponse,
  ImagePromptTemplate,
  ImageJobResponse,
  ImageJobsResponse,
  ImagePromptTemplatePayload,
  ImagePromptTemplatesResponse,
  InviteResponse,
  MessageResponse,
  ModelSyncResponse,
  ModelsResponse,
  OAuthExchangeResponse,
  OAuthURLResponse,
  OpsErrorSummary,
  OpsOverviewResponse,
  PromptFilterLog,
  PromptFilterLogsResponse,
  PromptFilterRulePatternTestResponse,
  PromptFilterRulesResponse,
  PromptFilterTestResponse,
  SemanticReviewConnectionTestResponse,
  PublicAPIKeyUsageResponse,
  RecycleBinAccountsResponse,
  RuntimeStatusResponse,
  SiteBranding,
  StatsResponse,
  SystemUpdateInfo,
  SystemUpdateResult,
  SetupHintsResponse,
  CPAExportEntry,
  SystemSettings,
  UpdateAccountSchedulerRequest,
  UpdateAPIKeyRequest,
  UpdateOAuthAccountRequest,
  UpdateOpenAIResponsesAccountRequest,
  UsageLogsResponse,
  UsageLogsPagedResponse,
  UsageStats,
  AccountGroup,
  AccountGroupsResponse,
  AccountHealthBarsResponse,
  BatchUpdateAccountsRequest,
  BackgroundUploadResponse,
  CreateAccountGroupRequest,
  UpdateAccountGroupRequest,
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
  const isFormData = typeof FormData !== 'undefined' && options.body instanceof FormData
  if (options.body !== undefined && options.body !== null && !isFormData && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

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

  return (await res.json()) as T
}

async function requestPublic<T>(path: string, options: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...options,
    cache: options.cache ?? 'no-store',
  })

  if (!res.ok) {
    const body = await res.text()
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return (await res.json()) as T
}

async function requestAPIKeyUsage<T>(path: string, apiKey: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  headers.set('Authorization', `Bearer ${apiKey}`)

  const res = await fetch('/api/key-usage' + path, {
    ...options,
    cache: options.cache ?? 'no-store',
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
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

function buildOpsErrorSearchParams(params: {
  start: string
  end: string
  status?: string
  errorKind?: string
  endpoint?: string
  apiKeyId?: string
  stream?: string
  fast?: string
  q?: string
  dedupe?: boolean
  excludeStatus?: string
}) {
  const search = new URLSearchParams()
  search.set('start', params.start)
  search.set('end', params.end)
  if (params.status) search.set('status', params.status)
  if (params.errorKind) search.set('error_kind', params.errorKind)
  if (params.endpoint) search.set('endpoint', params.endpoint)
  if (params.apiKeyId) search.set('api_key_id', params.apiKeyId)
  if (params.stream) search.set('stream', params.stream)
  if (params.fast) search.set('fast', params.fast)
  if (params.q) search.set('q', params.q)
  if (typeof params.dedupe === 'boolean') search.set('dedupe', String(params.dedupe))
  if (params.excludeStatus) search.set('exclude_status', params.excludeStatus)
  return search
}

export const api = {
  getBranding: () => requestPublic<SiteBranding>('/api/branding'),
  getPublicAPIKeyUsage: (apiKey: string, range = '30d', params: { page?: number; pageSize?: number } = {}) => {
    const search = new URLSearchParams()
    search.set('range', range)
    if (params.page) search.set('page', String(params.page))
    if (params.pageSize) search.set('page_size', String(params.pageSize))
    return requestAPIKeyUsage<PublicAPIKeyUsageResponse>(`/summary?${search.toString()}`, apiKey)
  },
  getStats: () => request<StatsResponse>('/stats'),
  getAccounts: () => request<AccountsResponse>('/accounts'),
  addAccount: (data: AddAccountRequest) =>
    request<CreateAccountResponse>('/accounts', { method: 'POST', body: JSON.stringify(data) }),
  addATAccount: (data: AddATAccountRequest) =>
    request<CreateAccountResponse>('/accounts/at', { method: 'POST', body: JSON.stringify(data) }),
  addOpenAIResponsesAccount: (data: AddOpenAIResponsesAccountRequest) =>
    request<CreateAccountResponse>('/accounts/openai-responses', { method: 'POST', body: JSON.stringify(data) }),
  fetchOpenAIResponsesModels: (data: FetchOpenAIResponsesModelsRequest) =>
    request<FetchOpenAIResponsesModelsResponse>('/accounts/openai-responses/models', { method: 'POST', body: JSON.stringify(data) }),
  updateOpenAIResponsesAccount: (id: number, data: UpdateOpenAIResponsesAccountRequest) =>
    request<MessageResponse>(`/accounts/${id}/openai-responses`, { method: 'PATCH', body: JSON.stringify(data) }),
  deleteAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}`, { method: 'DELETE' }),
  getRecycleBinAccounts: () =>
    request<RecycleBinAccountsResponse>('/accounts/recycle-bin'),
  restoreAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/restore`, { method: 'POST' }),
  purgeAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/purge`, { method: 'DELETE' }),
  emptyRecycleBin: () =>
    request<{ message: string; purged: number }>('/accounts/recycle-bin', {
      method: 'DELETE',
      body: JSON.stringify({ confirm: 'EMPTY-RECYCLE-BIN' }),
    }),
  refreshAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/refresh`, { method: 'POST' }),
  forceUsageProbe: () =>
    request<{ triggered: boolean; concurrency: number; reason?: string; mode?: string }>(`/accounts/usage/probe`, { method: 'POST' }),
  refreshAccountUsage: (id: number) =>
    request<{
      refreshed: boolean
      usage_percent_5h?: number
      usage_percent_7d?: number
      reset_5h_at?: string
      reset_7d_at?: string
    }>(`/accounts/${id}/usage/refresh`, { method: 'POST' }),
  updateAccountScheduler: (id: number, data: UpdateAccountSchedulerRequest) =>
    request<MessageResponse>(`/accounts/${id}/scheduler`, { method: 'PATCH', body: JSON.stringify(data) }),
  listAccountGroups: () => request<AccountGroupsResponse>('/account-groups'),
  createAccountGroup: (data: CreateAccountGroupRequest) =>
    request<{ id: number; message: string }>('/account-groups', { method: 'POST', body: JSON.stringify(data) }),
  updateAccountGroup: (id: number, data: UpdateAccountGroupRequest) =>
    request<MessageResponse>(`/account-groups/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  deleteAccountGroup: (id: number, force = false) =>
    request<MessageResponse>(`/account-groups/${id}${force ? '?force=true' : ''}`, { method: 'DELETE' }),
  toggleAccountEnabled: (id: number, enabled: boolean) =>
    request<MessageResponse>(`/accounts/${id}/enable`, { method: 'POST', body: JSON.stringify({ enabled }) }),
  toggleAccountLock: (id: number, locked: boolean) =>
    request<MessageResponse>(`/accounts/${id}/lock`, { method: 'POST', body: JSON.stringify({ locked }) }),
  batchUpdateAccounts: (data: BatchUpdateAccountsRequest) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-update', { method: 'POST', body: JSON.stringify(data) }),
  resetAccountStatus: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/reset-status`, { method: 'POST' }),
  resetCredits: (id: number) =>
    request<{ message: string; rate_limit_reset_credits?: number }>(`/accounts/${id}/reset-credits`, { method: 'POST' }),
  getAccountHealthBars: () =>
    request<AccountHealthBarsResponse>('/accounts/health-bars'),
  sendInvite: (id: number, data: { emails?: string[]; emails_text?: string; referral_key?: string; proxy_url?: string; max_emails?: number }) =>
    request<InviteResponse>(`/accounts/${id}/invite`, { method: 'POST', body: JSON.stringify(data) }),
  batchResetStatus: (ids: number[]) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-reset-status', { method: 'POST', body: JSON.stringify({ ids }) }),
  getAccountUsage: (id: number, days?: number) => {
    const search = new URLSearchParams()
    if (typeof days === 'number') search.set('days', String(days))
    const qs = search.toString()
    return request<AccountUsageDetail>(`/accounts/${id}/usage${qs ? `?${qs}` : ''}`)
  },
  updateAccountCredit: (id: number, data: { credit_enabled: boolean; credit_skip_usage_window: boolean }) =>
    request<MessageResponse>(`/accounts/${id}/credit`, { method: 'PATCH', body: JSON.stringify(data) }),
  getHealth: () => request<HealthResponse>('/health'),
  getCodexAuditReport: (params: { hours?: number; start?: string; end?: string; bucketMinutes?: number; limit?: number } = {}) => {
    const search = new URLSearchParams()
    if (params.hours) search.set('hours', String(params.hours))
    if (params.start) search.set('start', params.start)
    if (params.end) search.set('end', params.end)
    if (params.bucketMinutes) search.set('bucket_minutes', String(params.bucketMinutes))
    if (params.limit) search.set('limit', String(params.limit))
    return request<CodexAuditReport>(`/audit/codex2api?${search.toString()}`)
  },
  getOpsOverview: () => request<OpsOverviewResponse>('/ops/overview'),
  getRuntimeStatus: () => request<RuntimeStatusResponse>('/runtime-status'),
  getSystemUpdate: () => request<SystemUpdateInfo>('/system/update'),
  performSystemUpdate: () =>
    request<SystemUpdateResult>('/system/update', { method: 'POST' }),
  getOpsErrorSummary: (params: {
    start: string
    end: string
    status?: string
    errorKind?: string
    endpoint?: string
    apiKeyId?: string
    stream?: string
    fast?: string
    q?: string
  }) => {
    const search = buildOpsErrorSearchParams(params)
    return request<OpsErrorSummary>(`/ops/errors/summary?${search.toString()}`)
  },
  getOpsErrors: (params: {
    start: string
    end: string
    page: number
    pageSize?: number
    status?: string
    errorKind?: string
    endpoint?: string
    apiKeyId?: string
    stream?: string
    fast?: string
    q?: string
  }) => {
    const search = buildOpsErrorSearchParams(params)
    search.set('page', String(params.page))
    if (params.pageSize) search.set('page_size', String(params.pageSize))
    return request<UsageLogsPagedResponse>(`/ops/errors?${search.toString()}`)
  },
  downloadOpsErrors: (params: {
    start: string
    end: string
    status?: string
    errorKind?: string
    endpoint?: string
    apiKeyId?: string
    stream?: string
    fast?: string
    q?: string
    dedupe?: boolean
    excludeStatus?: string
  }) => {
    const search = buildOpsErrorSearchParams(params)
    return requestBlob(`/ops/errors/export?${search.toString()}`)
  },
  getUsageStats: (params: { start?: string; end?: string } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start) searchParams.set('start', params.start)
    if (params.end) searchParams.set('end', params.end)
    const qs = searchParams.toString()
    return request<UsageStats>(qs ? `/usage/stats?${qs}` : '/usage/stats')
  },
  getAPIKeyTokenStats: (params: { start?: string; end?: string } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start) searchParams.set('start', params.start)
    if (params.end) searchParams.set('end', params.end)
    const qs = searchParams.toString()
    return request<{ items: APIKeyTokenStat[] }>(
      qs ? `/usage/api-keys?${qs}` : '/usage/api-keys',
    )
  },
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
  getUsageLogsPaged: (params: { start: string; end: string; page: number; pageSize?: number; email?: string; model?: string; endpoint?: string; apiKeyId?: string; accountId?: string; fast?: string; stream?: string }) => {
    const searchParams = new URLSearchParams()
    searchParams.set('start', params.start)
    searchParams.set('end', params.end)
    searchParams.set('page', String(params.page))
    if (params.pageSize) searchParams.set('page_size', String(params.pageSize))
    if (params.email) searchParams.set('email', params.email)
    if (params.model) searchParams.set('model', params.model)
    if (params.endpoint) searchParams.set('endpoint', params.endpoint)
    if (params.apiKeyId) searchParams.set('api_key_id', params.apiKeyId)
    if (params.accountId) searchParams.set('account_id', params.accountId)
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
  createAPIKey: (data: CreateAPIKeyRequest) =>
    request<CreateAPIKeyResponse>('/keys', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  deleteAPIKey: (id: number) =>
    request<MessageResponse>(`/keys/${id}`, { method: 'DELETE' }),
  updateAPIKey: (id: number, data: UpdateAPIKeyRequest) =>
    request<MessageResponse>(`/keys/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
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
  createImageEditJob: (data: CreateImageJobPayload) =>
    request<ImageJobResponse>('/images/edit-jobs', { method: 'POST', body: JSON.stringify(data) }),
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
  deleteImageJob: (id: number) =>
    request<MessageResponse>(`/images/jobs/${id}`, { method: 'DELETE' }),
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
  getSetupHints: () => request<SetupHintsResponse>('/setup-hints'),
  getSettings: () => request<SystemSettings>('/settings'),
  updateSettings: (data: Partial<SystemSettings>) =>
    request<SystemSettings>('/settings', { method: 'PUT', body: JSON.stringify(data) }),
  uploadBackground: (file: File) => {
    const form = new FormData()
    form.set('file', file)
    return request<BackgroundUploadResponse>('/settings/background-upload', { method: 'POST', body: form })
  },
  testImageStorageConnection: (data: {
    endpoint: string
    region: string
    bucket: string
    access_key: string
    secret_key: string
    prefix: string
    force_path_style: boolean
  }) =>
    request<{ ok: boolean; bucket: string }>('/settings/image-storage/test', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
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
  matchPromptFilterLog: (params: { at: string; endpoint?: string; apiKeyId?: number; source?: string }) => {
    const search = new URLSearchParams()
    search.set('at', params.at)
    if (params.endpoint) search.set('endpoint', params.endpoint)
    if (params.apiKeyId) search.set('api_key_id', String(params.apiKeyId))
    if (params.source) search.set('source', params.source)
    return request<{ found: boolean; log: PromptFilterLog | null }>(`/prompt-filter/logs/match?${search.toString()}`)
  },
  testPromptFilter: (data: { text: string; endpoint?: string; model?: string }) =>
    request<PromptFilterTestResponse>('/prompt-filter/test', { method: 'POST', body: JSON.stringify(data) }),
  testSemanticReviewConnection: (data: { text?: string; endpoint?: string; request_model?: string } = {}) =>
    request<SemanticReviewConnectionTestResponse>('/prompt-filter/semantic-review/test', { method: 'POST', body: JSON.stringify(data) }),
  testPromptFilterRulePattern: (data: { pattern: string; text: string }) =>
    request<PromptFilterRulePatternTestResponse>('/prompt-filter/rules/test', { method: 'POST', body: JSON.stringify(data) }),
  getPromptFilterRules: () =>
    request<PromptFilterRulesResponse>('/prompt-filter/rules'),
  getModels: () => request<ModelsResponse>('/models'),
  syncModels: () => request<ModelSyncResponse>('/models/sync', { method: 'POST' }),
  batchTestAccounts: (ids?: number[]) =>
    request<{ total: number; success: number; failed: number; banned: number; rate_limited: number }>('/accounts/batch-test', {
      method: 'POST',
      body: ids ? JSON.stringify({ ids }) : undefined,
    }),
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
  updateProxy: (id: number, data: { url?: string; label?: string; enabled?: boolean }) =>
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
  updateOAuthAccount: (id: number, data: UpdateOAuthAccountRequest) =>
    request<OAuthExchangeResponse>(`/accounts/${id}/oauth/exchange-code`, { method: 'POST', body: JSON.stringify(data) }),
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
