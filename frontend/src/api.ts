import type {
  AddAccountRequest,
  AdminErrorResponse,
  APIKeysResponse,
  AccountsResponse,
  CreateAccountResponse,
  CreateAPIKeyResponse,
  HealthResponse,
  MessageResponse,
  StatsResponse,
  UsageLogsResponse,
  UsageStats,
} from './types'

const BASE = '/api/admin'

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

  const res = await fetch(BASE + path, {
    ...options,
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return (await res.json()) as T
}

export const api = {
  getStats: () => request<StatsResponse>('/stats'),
  getAccounts: () => request<AccountsResponse>('/accounts'),
  addAccount: (data: AddAccountRequest) =>
    request<CreateAccountResponse>('/accounts', { method: 'POST', body: JSON.stringify(data) }),
  deleteAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}`, { method: 'DELETE' }),
  refreshAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/refresh`, { method: 'POST' }),
  getHealth: () => request<HealthResponse>('/health'),
  getUsageStats: () => request<UsageStats>('/usage/stats'),
  getUsageLogs: (limit = 50) => request<UsageLogsResponse>(`/usage/logs?limit=${limit}`),
  getAPIKeys: () => request<APIKeysResponse>('/keys'),
  createAPIKey: (name: string) =>
    request<CreateAPIKeyResponse>('/keys', {
      method: 'POST',
      body: JSON.stringify({ name }),
    }),
  deleteAPIKey: (id: number) =>
    request<MessageResponse>(`/keys/${id}`, { method: 'DELETE' }),
}
