export type ToastType = 'success' | 'error'
export type ISODateString = string

export interface ToastState {
  msg: string
  type: ToastType
}

export type AccountStatus = 'active' | 'ready' | 'cooldown' | 'error' | 'paused' | string

export interface StatsResponse {
  total: number
  available: number
  error: number
  today_requests: number
}

export interface AccountRow {
  id: number
  name: string
  email: string
  plan_type: string
  status: AccountStatus
  proxy_url: string
  updated_at: ISODateString
  active_requests?: number
  total_requests?: number
  last_used_at?: ISODateString
}

export type AccountsResponse = ApiListResponse<'accounts', AccountRow>

export interface AddAccountRequest {
  name: string
  refresh_token: string
  proxy_url: string
}

export interface MessageResponse {
  message: string
}

export interface CreateAccountResponse extends MessageResponse {
  id: number
}

export interface AdminErrorResponse {
  error: string
}

export interface HealthResponse {
  status: 'ok' | string
  available: number
  total: number
}

export interface UsageStats {
  total_requests: number
  total_tokens: number
  total_prompt_tokens: number
  total_completion_tokens: number
  today_requests: number
  today_tokens: number
  rpm: number
  tpm: number
  avg_duration_ms: number
  error_rate: number
}

export interface UsageLog {
  id: number
  account_id: number
  endpoint: string
  model: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  status_code: number
  duration_ms: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  first_token_ms: number
  reasoning_effort: string
  inbound_endpoint: string
  upstream_endpoint: string
  stream: boolean
  created_at: ISODateString
}

export type UsageLogsResponse = ApiListResponse<'logs', UsageLog>

export interface APIKeyRow {
  id: number
  name: string
  key: string
  created_at: ISODateString
}

export type APIKeysResponse = ApiListResponse<'keys', APIKeyRow>

export interface CreateAPIKeyResponse {
  id: number
  key: string
  name: string
}

export type ApiListResponse<K extends string, T> = {
  [P in K]: T[]
}
