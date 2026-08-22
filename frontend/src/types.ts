export type ToastType = 'success' | 'error' | 'warning' | 'info'
export type ISODateString = string

export interface ToastState {
  msg: string
  type: ToastType
}

export type AccountStatus = 'active' | 'ready' | 'cooldown' | 'error' | 'refreshing' | 'paused' | 'quota_paused' | string
export type CodexClientMetadataMode = 'auto' | 'always' | 'off'
/** Codex 官方出站请求的设备指纹收敛档位，默认 off（不收敛）。 */
export type CodexFingerprintMode = 'off' | 'device' | 'session' | 'full'
export type ModelCooldownMode = 'off' | 'fixed' | 'adaptive'

export interface StatsChannelCounts {
  total: number
  available: number
  rate_limited: number
  error: number
  today_requests: number
}

export interface StatsResponse {
  total: number
  available: number
  rate_limited: number
  error: number
  today_requests: number
  // 按上游渠道(codex/grok)拆分的账号与今日请求计数
  channels?: Record<string, StatsChannelCounts>
}

export interface AccountUsageWindow {
  requests: number
  tokens: number
  account_billed?: number
  user_billed?: number
  model_counts?: Record<string, number>
  model_success_counts?: Record<string, number>
  model_avg_first_token_ms?: Record<string, number>
}

export interface GrokProductUsage {
  product: string
  usage_percent?: number | null
}

// Grok billing 完整额度视图（后端 grok_billing_detail 凭据透出）。
export interface GrokBillingDetail {
  plan?: string
  weekly_percent?: number | null
  weekly_period_start?: string
  weekly_period_end?: string
  product_usage?: GrokProductUsage[]
  on_demand_cap_cents?: number | null
  on_demand_used_cents?: number | null
  monthly_limit_cents?: number | null
  monthly_used_cents?: number | null
  monthly_percent?: number | null
  monthly_period_start?: string
  monthly_period_end?: string
  updated_at?: string
}

export interface GrokRateLimitSnapshot {
  limit_tokens?: number
  remaining_tokens?: number
  limit_requests?: number
  remaining_requests?: number
  updated_at?: string
}

// 免费额度耗尽时从上游 429 错误体解析的权威用量(滚动 24h 窗口)。
export interface GrokFreeQuotaSnapshot {
  used_tokens: number
  limit_tokens: number
  model?: string
  exhausted_at: string
}

export interface GrokPlanInfo {
  key: string
  display: string
  paid: boolean
  billing: boolean
}

export interface AccountRow {
  detail_loaded?: boolean
  id: number
  name: string
  email: string
  email_domain?: string
  chatgpt_account_id?: string
  token_workspace_id?: string
  workspace_id_override?: string
  effective_workspace_id?: string
  plan_type: string
  subscription_expires_at?: string
  status: AccountStatus
  error_message?: string
  at_only?: boolean
  access_token_type?: string
  account_type?: string
  openai_responses_api?: boolean
  grok_api?: boolean
  agent_identity?: boolean
  grok_auth_kind?: string
  grok_plan?: GrokPlanInfo
  grok_billing?: GrokBillingDetail
  // 上游逐请求返回的配额余量(x-ratelimit-* 头),运行时快照
  grok_rate_limit?: GrokRateLimitSnapshot
  grok_free_quota?: GrokFreeQuotaSnapshot
  base_url?: string
  balance_query_url?: string
  models?: string[]
  model_mapping?: string
  codex_client_metadata_mode?: CodexClientMetadataMode
  codex_fingerprint_mode?: CodexFingerprintMode
  custom_headers?: Record<string, string> | null
  health_tier?: string
  scheduler_score?: number
  dispatch_score?: number
  score_bias_override?: number | null
  score_bias_effective?: number
  base_concurrency_override?: number | null
  base_concurrency_effective?: number
  skip_warm_tier?: boolean
  dynamic_concurrency_limit?: number
  allowed_api_key_ids?: number[]
  tags?: string[]
  // 通用备注;自助提交的账号会带上「自助提交联系人: ...」这类说明。
  note?: string
  group_ids?: number[]
  scheduler_breakdown?: {
    unauthorized_penalty: number
    rate_limit_penalty: number
    timeout_penalty: number
    server_penalty: number
    failure_penalty: number
    success_bonus: number
    usage_penalty_7d: number
    usage_urgency_bonus_5h?: number
    usage_urgency_bonus_7d?: number
    latency_penalty: number
    success_rate_penalty?: number
  }
  last_unauthorized_at?: ISODateString
  last_rate_limited_at?: ISODateString
  last_timeout_at?: ISODateString
  last_server_error_at?: ISODateString
  proxy_url: string
  created_at: ISODateString
  updated_at: ISODateString
  codex_usage_updated_at?: ISODateString
  active_requests?: number
  total_requests?: number
  last_used_at?: ISODateString
  success_requests?: number
  error_requests?: number
  retry_error_requests?: number
  rate_limit_attempts?: number
  error_status_counts?: Record<string, number>
  success_model_counts?: Record<string, number>
  usage_percent_7d?: number | null
  usage_percent_5h?: number | null
  usage_percent_spark?: number | null
  rate_limit_reset_credits?: number | null
  applicable_reset_credits?: number | null
  credits_balance?: string | null
  credits_has_credits?: boolean | null
  credits_unlimited?: boolean | null
  credits_overage_limit_reached?: boolean | null
  auto_pause_5h_threshold?: number | null
  auto_pause_7d_threshold?: number | null
  auto_pause_5h_disabled?: boolean
  auto_pause_7d_disabled?: boolean
  ignore_usage_limit_status_override?: boolean | null
  ignore_usage_limit_status_effective?: boolean
  dispatch_count_limit?: number | null
  scheduler_priority?: number | null
  dispatch_count_used?: number
  dispatch_count_reset_at?: ISODateString
  dispatch_count_limited?: boolean
  usage_5h_detail?: AccountUsageWindow
  usage_7d_detail?: AccountUsageWindow
  // 今日(服务器时区当天 0 点起)网关侧聚合,由 page-stats 补齐。
  usage_today_detail?: AccountUsageWindow
  reset_5h_at?: ISODateString
  reset_7d_at?: ISODateString
  reset_spark_at?: ISODateString
  // 长窗口(7d 槽)真实类型: "monthly"(free/team 月窗)/"weekly"/未知。
  // free/team plan 的长窗口实为约 30 天,标签应显示 30d 而非 7d (issue #324)。
  usage_window_7d_kind?: 'monthly' | 'weekly' | ''
  usage_window_7d_seconds?: number
  billed_5h?: number
  billed_7d?: number
  // 官方结算口径的近 7 天成本(美元)。来自 account_daily_usage 快照,与
  // billed_7d(本地日志算的网关成本)是两套账,列表里并排展示。
  official_usd_7d?: number
  // 官方快照已成功同步过但上游窗口内没有数据(官方统计有滞后)。
  // 有这个标记时不再重拉 page-stats,胶囊显示静态"暂无数据"而非转圈。
  official_usage_synced?: boolean
  cooldown_until?: ISODateString
  cooldown_reason?: string
  model_cooldowns?: Array<{
    model: string
    reason: string
    reset_at: ISODateString
    remaining_seconds: number
  }>
  model_cooldown_mode_override?: ModelCooldownMode | null
  model_cooldown_seconds_override?: number | null
  model_cooldown_backoff_override?: boolean | null
  model_cooldown_mode_effective?: ModelCooldownMode
  model_cooldown_seconds_effective?: number
  model_cooldown_backoff_effective?: boolean
  enabled?: boolean
  locked?: boolean
  credit_enabled?: boolean
  credit_skip_usage_window?: boolean
  // using_credits 与 status 并列：用量窗口已打满但积分顶着，status 仍是 active
  // （确实可调度），这个标记只用于在状态徽章旁并列一个「使用积分」徽章。
  using_credits?: boolean
  // 图片配额信息
  image_quota_remaining?: number
  image_quota_total?: number
  today_used_count?: number
  image_quota_reset_at?: ISODateString
}

export type AccountsResponse = ApiListResponse<'accounts', AccountRow>

export interface AccountListSummary {
  total: number
  normal: number
  active: number
  overload_paused: number
  rate_limited: number
  rate_limited_5h: number
  rate_limited_7d: number
  abnormal: number
  banned: number
  error: number
  unsampled: number
  disabled: number
  locked: number
  healthy: number
  warm: number
  risky: number
  oauth: number
  api_key: number
  subscription_unlocked: number
  unauthorized_24h: number
  rate_limited_1h: number
  timeout_15m: number
}

export interface AccountEmailDomainFacet {
  domain: string
  total: number
  banned: number
}

export interface AccountsPageResponse extends AccountsResponse {
  page: number
  page_size: number
  total: number
  summary: AccountListSummary
  facets: {
    tags: string[]
    email_domains: AccountEmailDomainFacet[]
  }
  snapshot_at: ISODateString
  stats_state: 'ready' | 'stale' | 'warming'
  disabled_sorts?: string[]
}

export interface AccountPageStatsItem {
  usage_5h_detail?: AccountUsageWindow
  usage_7d_detail?: AccountUsageWindow
  usage_today_detail?: AccountUsageWindow
  billed_5h?: number
  billed_7d?: number
  official_usd_7d?: number
  official_usage_synced?: boolean
}

export interface AccountPageStatsResponse {
  stats: Record<string, AccountPageStatsItem>
}

export interface AccountLiveStateResponse {
  accounts: Record<string, { active_requests: number }>
}

export interface AccountsPageParams {
  channel?: 'codex' | 'grok'
  page: number
  pageSize: number
  search?: string
  status?: string
  plan?: string
  authKind?: string
  tag?: string
  emailDomain?: string
  groupInclude?: number[]
  groupExclude?: number[]
  ungrouped?: boolean
  healthTier?: 'healthy' | 'warm' | 'risky' | 'banned' | 'attention'
  proxyUrl?: string
  proxyFilter?: 'all' | 'unbound' | 'this' | 'other'
  sort?: 'requests' | 'today' | 'usage' | 'created_at' | 'updated_at' | 'scheduler_priority' | 'group' | 'risk' | 'dispatch_score' | 'latency_penalty' | 'unauthorized'
  order?: 'asc' | 'desc'
}

export interface AccountQuotaAnalysisBucket {
  min: number
  max: number
  count: number
}

export interface AccountQuotaAnalysis {
  total: number
  sampled: number
  unsampled: number
  high_usage: number
  exhausted: number
  average_used: number | null
  buckets: AccountQuotaAnalysisBucket[]
}

export interface AccountAnalysisTimeBucket {
  start_at: number
  end_at: number
  count: number
  cooldown_count?: number
}

export interface AccountRecoveryAnalysis {
  total: number
  recoverable: number
  unknown: number
  next_at: number | null
  buckets: AccountAnalysisTimeBucket[]
}

export interface AccountResetAnalysis {
  total: number
  known: number
  unknown: number
  next_at: number | null
  buckets: AccountAnalysisTimeBucket[]
}

export interface AccountPressureForecastAnalysis {
  sampled: number
  threshold: number
  predicted_at: number | null
  predicted_count: number
  unknown: number
  rpm: number
  effective_rpm_limit: number
  rpm_pressure: number | null
  active_pressure: number
  rate_limit_pressure: number
  dispatchable_accounts: number
  avg_concurrency: number
  high_pressure_at: number | null
  supply_shortage_at: number | null
  risk_level: 'low' | 'medium' | 'high'
  confidence: number
}

export interface AccountAnalysisResponse {
  channel: 'codex' | 'grok'
  quota: Record<'5h' | '7d', AccountQuotaAnalysis>
  recovery: Record<'5h' | '7d', AccountRecoveryAnalysis>
  reset: AccountResetAnalysis
  forecasts: Record<'5h' | '7d', AccountPressureForecastAnalysis>
  snapshot_at: ISODateString
  stats_state: 'ready' | 'stale' | 'warming'
}

export interface AccountOperationSelector {
  channel: 'codex' | 'grok'
  search?: string
  status?: string
  plan?: string
  auth_kind?: string
  tag?: string
  email_domain?: string
  group_include?: number[]
  group_exclude?: number[]
  ungrouped?: boolean
  refreshable_only?: boolean
  subscription_unlocked?: boolean
}

// 单张「主动重置次数」券的有效期明细（issue #322）。
export interface ResetCreditItem {
  id: string
  granted_at?: ISODateString
  expires_at: ISODateString
}

export interface ResetCreditsDetailResponse {
  available_count: number
  credits: ResetCreditItem[]
}

// AccountHealthBucket 是「健康状态」条单个时间窗口内的请求成败计数。
export interface AccountHealthBucket {
  success: number
  failed: number
}

// AccountHealthBarsResponse 是 GET /api/accounts/health-bars 的响应。
// buckets 按账号 ID（字符串）映射到由旧到新的 block_count 个时间桶。
export interface AccountHealthBarsResponse {
  buckets: Record<string, AccountHealthBucket[]>
  block_count: number
  block_minutes: number
}

export interface InviteItem {
  referral_id?: string
  email?: string
  invite_url?: string
}

export interface InviteResult {
  ok: boolean
  status_code: number
  request_id?: string
  program_id: string
  entrypoint: string
  emails: string[]
  invites?: InviteItem[]
  // upstream_message 是上游 detail 里的原因（如「此人已收到推荐邀请」），
  // failed_emails 是被拒的收件人。收件人级被拒与账号资格无关，别报成「账号无资格」。
  upstream_message?: string
  failed_emails?: string[]
  // challenged 为真表示被 Cloudflare 挑战拦下，不是上游的业务结论。
  // 此时 status_code（通常 403）不能解读成「无资格」，应提示重试。
  challenged?: boolean
  upstream?: unknown
  upstream_raw?: string
}

export interface InviteResponse {
  ok: boolean
  result: InviteResult
}

// InviteGrant 是一条奖励条目（邀请人 / 受邀人各一条）。
export interface InviteGrant {
  recipient?: string
  grant_type?: string
  amount?: number
  reward_id?: string
}

// InviteTimeFrameRule 是一条配额规则。capacity_type 区分两种独立上限：
// send 是发送次数、reward 是能拿到奖励的次数（后者通常远小于前者）。
export interface InviteTimeFrameRule {
  invites_sent: number
  invites_total: number
  time_frame?: string
  type?: string
  capacity_type?: string
}

// InviteEligibility 是 GET /api/accounts/:id/invite/eligibility 的 result。
// remaining_* 缺失表示上游没给这个字段，与「明确为 0」不同，不要当成配额用尽。
export interface InviteEligibility {
  ok: boolean
  status_code: number
  request_id?: string
  should_show: boolean
  ineligible_reason?: string
  ineligible_reason_code?: string
  program_id?: string
  entrypoint?: string
  offer_id?: string
  grants?: InviteGrant[]
  remaining_send_capacity?: number
  remaining_reward_capacity?: number
  title?: string
  description?: string
  rules?: string[]
  time_frame_rules?: InviteTimeFrameRule[]
  requires_explicit_confirmation?: boolean
  upstream_message?: string
  challenged?: boolean
  upstream?: unknown
  upstream_raw?: string
}

export interface InviteEligibilityResponse {
  ok: boolean
  result: InviteEligibility
}

// InviteTrackingItem 是一条已发邀请记录。
export interface InviteTrackingItem {
  referral_id?: string
  email?: string
  status?: string
  can_resend?: boolean
  invite_url?: string
  resend_available_at?: string
  grants?: InviteGrant[]
  created_at?: string
  expires_at?: string
}

export interface InviteTracking {
  ok: boolean
  status_code: number
  request_id?: string
  items: InviteTrackingItem[]
  cursor?: string
  upstream_message?: string
  challenged?: boolean
  upstream?: unknown
  upstream_raw?: string
}

export interface InviteTrackingResponse {
  ok: boolean
  result: InviteTracking
}

export interface RecycleBinAccountRow {
  id: number
  name: string
  email: string
  plan_type: string
  at_only?: boolean
  access_token_type?: string
  openai_responses_api?: boolean
  base_url?: string
  models?: string[]
  created_at: ISODateString
  deleted_at?: ISODateString
  last_test_status?: string
  last_test_at?: ISODateString
}

export type RecycleBinAccountsResponse = ApiListResponse<'accounts', RecycleBinAccountRow>

export interface AddAccountRequest {
  name?: string
  refresh_token?: string
  session_token?: string
  proxy_url: string
  allow_duplicate?: boolean
  custom_headers?: Record<string, string> | null
  /** 添加/导入时直接绑定的账号分组；命中已存在账号时不改其分组。 */
  group_ids?: number[]
}

export interface AddATAccountRequest {
  name?: string
  access_token: string
  proxy_url: string
  allow_duplicate?: boolean
  custom_headers?: Record<string, string> | null
  /** 添加/导入时直接绑定的账号分组；命中已存在账号时不改其分组。 */
  group_ids?: number[]
}

// Codex Agent Identity auth.json 导入（auth_mode=agentIdentity，动态签名，不存 AT/RT）。
export interface ImportAgentIdentityRequest {
  name?: string
  auth_json: string
  proxy_url?: string
}

export interface ImportAgentIdentityResponse {
  message: string
  id: number
  email?: string
}

// Agent Identity auth.json 文件批量导入(每项一个文件的原始 JSON 内容)。
export interface AgentIdentityBatchImportRequest {
  files: string[]
  proxy_url?: string
}

export interface AgentIdentityImportItem {
  email?: string
  id?: number
  ok: boolean
  error?: string
}

export interface AgentIdentityBatchImportResponse {
  total: number
  imported: number
  failed: number
  items: AgentIdentityImportItem[]
}

export interface AddOpenAIResponsesAccountRequest {
  name?: string
  base_url: string
  api_key: string
  balance_query_url?: string
  models: string[]
  model_mapping?: string
  codex_client_metadata_mode?: CodexClientMetadataMode
  proxy_url: string
  custom_headers?: Record<string, string> | null
}

export interface UpdateOpenAIResponsesAccountRequest {
  name?: string
  base_url: string
  api_key?: string
  balance_query_url?: string
  models: string[]
  model_mapping?: string
  codex_client_metadata_mode?: CodexClientMetadataMode
  proxy_url: string
  custom_headers?: Record<string, string> | null
}

export interface FetchOpenAIResponsesModelsRequest {
  account_id?: number
  base_url: string
  api_key: string
  proxy_url?: string
}

export interface FetchOpenAIResponsesModelsResponse {
  base_url: string
  models: string[]
}

export interface OpenAIResponsesBalanceResponse {
  balance: number
  unit: string
  source: string
  unlimited?: boolean
  queried_at: ISODateString
}

export type GrokAuthKind = 'oauth' | 'api_key'

export interface AddGrokAccountRequest {
  name?: string
  auth_kind: GrokAuthKind
  auth_json?: string
  api_key?: string
  base_url?: string
  models?: string[]
  model_mapping?: string
  proxy_url?: string
  /** 添加/导入时直接绑定的账号分组；命中已存在账号时不改其分组。 */
  group_ids?: number[]
}

export type UpdateGrokAccountRequest = AddGrokAccountRequest

export interface BatchUpdateGrokModelsRequest {
  ids: number[]
  models: string[]
}

export interface BatchUpdateGrokModelsResponse {
  message: string
  success: number
  failed: number
  models: string[]
}

export interface FetchGrokModelsResponse {
  models: string[]
}

export type GrokFactKind = 'user' | 'settings' | 'billing' | 'auto_topup'
export type GrokProtocol = 'responses' | 'chat_completions' | 'messages'

/** A sanitized control-plane observation. Token material is never included. */
export interface GrokAccountFact {
  account_id: number
  kind: GrokFactKind | string
  status: string
  http_status?: number
  payload?: Record<string, unknown> | null
  field_presence?: Record<string, string>
  credential_generation: number
  source?: string
  observed_at?: ISODateString
  expires_at?: ISODateString
  updated_at?: ISODateString
}

export interface GrokAccountIdentitySummary {
  credential_family_id: string
  archive_plan?: string
  archive_plan_source?: string
  jwt_tier?: string
  jwt_tier_trust?: string
}

export interface GrokModelCatalogSnapshot {
  account_id: number
  origin: string
  credential_generation: number
  auth_kind?: string
  status: string
  http_etag?: string
  etag_hint?: string
  etag_hint_observed_at?: ISODateString
  observed_at?: ISODateString
  expires_at?: ISODateString
  updated_at?: ISODateString
}

export interface GrokModelCatalogItem {
  account_id: number
  origin: string
  model_id: string
  display_name?: string
  description?: string
  base_url?: string
  api_base_url?: string
  api_backend?: GrokProtocol | string
  context_window?: number
  max_output_tokens?: number
  reasoning?: boolean | null
  backend_search?: boolean | null
  stream_tool_calls?: boolean | null
  supported_in_api?: boolean | null
  hidden?: boolean | null
  first_seen_at?: ISODateString
  observed_at?: ISODateString
}

export interface GrokModelCatalog {
  snapshot: GrokModelCatalogSnapshot
  items: GrokModelCatalogItem[]
}

export interface GrokModelCapability {
  account_id: number
  model_id: string
  origin: string
  protocol: GrokProtocol | string
  credential_generation: number
  status: string
  http_status?: number
  provider_code?: string
  source?: string
  retry_after_seconds?: number | null
  observed_at?: ISODateString
  expires_at?: ISODateString
  updated_at?: ISODateString
}

export interface GrokAccountState {
  account_id: number
  credential_generation: number
  identity?: GrokAccountIdentitySummary | null
  facts: Record<string, GrokAccountFact>
  catalogs: GrokModelCatalog[]
  capabilities: GrokModelCapability[]
}

export interface GrokStateSyncResponse {
  message: string
  state: GrokAccountState
  models: string[]
  synced_facts?: string[]
  errors?: Record<string, string>
}

export interface GrokCapabilityProbeResult {
  model_id: string
  protocol: GrokProtocol | string
  status: string
  http_status?: number
  provider_code?: string
  retry_after_seconds?: number | null
  observed_at?: ISODateString
}

export interface GrokCapabilityProbeResponse {
  message: string
  state: GrokAccountState
  results: GrokCapabilityProbeResult[]
}

// Grok Device Code OAuth（与 CLIProxyAPI / Grok CLI 一致）。
export interface GrokDeviceStartRequest {
  proxy_url?: string
  name?: string
  base_url?: string
  models?: string[]
}

export interface GrokDeviceStartResponse {
  session_id: string
  user_code: string
  verification_uri?: string
  verification_uri_complete?: string
  verification_url: string
  expires_in: number
  interval: number
}

export interface GrokDevicePollRequest {
  session_id: string
  proxy_url?: string
  name?: string
}

export interface GrokDevicePollResponse {
  status: 'pending' | 'authorized' | string
  slow_down?: boolean
  interval?: number
  user_code?: string
  expires_at?: string
  message?: string
  id?: number
  email?: string
}

// Grok Web SSO 批量导入：用 sso token 自动换成 Build(OAuth) 账号。
export interface GrokSSOImportRequest {
  tokens: string
  base_url?: string
  models?: string[]
  proxy_url?: string
  /** 添加/导入时直接绑定的账号分组；命中已存在账号时不改其分组。 */
  group_ids?: number[]
}

export interface GrokSSOImportItem {
  name?: string
  email?: string
  id?: number
  ok: boolean
  error?: string
}

export interface GrokSSOImportResponse {
  total: number
  imported: number
  failed: number
  items: GrokSSOImportItem[]
}

// Grok 凭据文件批量导入（CPA.json / auth.json）：每项是一个文件的原始 JSON 内容。
export interface GrokBatchImportRequest {
  files: string[]
  base_url?: string
  models?: string[]
  proxy_url?: string
  /** 添加/导入时直接绑定的账号分组；命中已存在账号时不改其分组。 */
  group_ids?: number[]
}

// 结果结构与 SSO 导入一致，复用 GrokSSOImportItem。
export interface GrokBatchImportResponse {
  total: number
  imported: number
  failed: number
  items: GrokSSOImportItem[]
}

export interface UpdateAccountSchedulerRequest {
  score_bias_override?: number | null
  base_concurrency_override?: number | null
  skip_warm_tier?: boolean
  allowed_api_key_ids?: number[] | null
  proxy_url?: string | null
  tags?: string[] | null
  group_ids?: number[] | null
  auto_pause_5h_threshold?: number | null
  auto_pause_7d_threshold?: number | null
  auto_pause_5h_disabled?: boolean
  auto_pause_7d_disabled?: boolean
  ignore_usage_limit_status_override?: boolean | null
  dispatch_count_limit?: number | null
  scheduler_priority?: number | null
  custom_headers?: Record<string, string> | null
  codex_fingerprint_mode?: CodexFingerprintMode | null
}

export interface BatchUpdateAccountsRequest extends UpdateAccountSchedulerRequest {
  ids?: number[]
  selector?: AccountOperationSelector
  enabled?: boolean
  locked?: boolean
}

export interface AccountGroup {
  id: number
  name: string
  description: string
  color: string
  sort_order: number
  member_count: number
  base_concurrency_override: number | null
  auto_pause_5h_threshold: number
  auto_pause_7d_threshold: number
  proxy_urls: string[]
  channel: 'codex' | 'grok'
  created_at: ISODateString
  updated_at: ISODateString
}

export interface AccountGroupsResponse {
  groups: AccountGroup[]
}

export interface CreateAccountGroupRequest {
  name: string
  description?: string
  color?: string
  sort_order?: number
  base_concurrency_override?: number | null
  auto_pause_5h_threshold?: number
  auto_pause_7d_threshold?: number
  proxy_urls?: string[]
  channel?: 'codex' | 'grok'
}

export interface UpdateAccountGroupRequest {
  name?: string
  description?: string
  color?: string
  sort_order?: number
  base_concurrency_override?: number | null
  auto_pause_5h_threshold?: number
  auto_pause_7d_threshold?: number
  proxy_urls?: string[]
  channel?: 'codex' | 'grok'
}

export interface AccountModelStat {
  model: string
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  account_billed: number
  user_billed: number
}

// 官方结算用量（wham daily-workspace-usage-counts 落库后的快照）。
// 与本地 usage_logs 聚合是两套口径：这份是 OpenAI 侧的权威账单数据。
export interface WhamDailyUsageSplit {
  client_id?: string
  model?: string
  users: number
  threads: number
  turns: number
  credits: number
  uncached_text_input_tokens?: number
  cached_text_input_tokens?: number
  text_output_tokens?: number
  text_total_tokens?: number
}

export interface WhamDailyUsageItem {
  day: string
  credits: number
  usd: number
  users: number
  threads: number
  turns: number
  uncached_input_tokens: number
  cached_input_tokens: number
  output_tokens: number
  total_tokens: number
  // 当天的记录在上游结算前不含 token 明细，settled=false 时 token 数还不可信。
  settled: boolean
  clients: WhamDailyUsageSplit[]
  models: WhamDailyUsageSplit[]
}

export interface WhamDailyUsageResponse {
  days: number
  items: WhamDailyUsageItem[]
  totals: {
    credits: number
    usd: number
    total_tokens: number
    turns: number
  }
  credits_per_usd: number
  retention_days: number
  last_synced_at?: string
  refresh_error?: string
}

export interface AccountUsageDayStat {
  date: string
  label: string
  requests: number
  tokens: number
  account_billed: number
  user_billed: number
}

export interface AccountKeyStat {
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  requests: number
  tokens: number
  account_billed: number
  user_billed: number
}

export interface AccountUsageDetail {
  period_days: number
  active_days: number
  total_requests: number
  total_tokens: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  cache_hit_rate: number
  total_account_billed: number
  total_user_billed: number
  avg_daily_account_billed: number
  avg_daily_user_billed: number
  avg_daily_requests: number
  avg_daily_tokens: number
  avg_duration_ms: number
  avg_first_token_ms: number
  p95_duration_ms: number
  error_requests: number
  error_rate: number
  retry_requests: number
  first_token_samples: number
  stream_requests: number
  stream_rate: number
  compact_requests: number
  compact_rate: number
  today: AccountUsageDayStat
  highest_cost_day?: AccountUsageDayStat
  highest_request_day?: AccountUsageDayStat
  history: AccountUsageDayStat[]
  models: AccountModelStat[]
  by_api_key: AccountKeyStat[]
}

export interface MessageResponse {
  message: string
}

export interface SystemUpdateInfo {
  current_version: string
  latest_version: string
  has_update: boolean
  supported: boolean
  unsupported_reason?: string
  runtime_os: string
  runtime_arch: string
  mode: string
  release_url?: string
  asset_name?: string
  published_at?: string
  warning?: string
}

export interface SystemUpdateResult extends MessageResponse {
  current_version: string
  latest_version: string
  need_restart: boolean
  restarting: boolean
  mode: string
  backup_path?: string
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

export interface SiteBranding {
  site_name: string
  site_logo: string
  background_image: string
  background_opacity: number
  background_blur: number
  background_glass_opacity: number
  background_glass_blur: number
}

export interface BackgroundUploadResponse {
  url: string
  filename: string
  mime_type: string
  bytes: number
}

export interface AccountEventTrendPoint {
  bucket: string
  added: number
  deleted: number
}

export interface OpsOverviewResponse {
  updated_at: ISODateString
  uptime_seconds: number
  database_driver: string
  database_label: string
  cache_driver: string
  cache_label: string
  cpu: {
    percent: number
    cores: number
  }
  memory: {
    percent: number
    used_bytes: number
    total_bytes: number
    process_bytes: number
    heap_alloc_bytes?: number
    heap_inuse_bytes?: number
    heap_released_bytes?: number
    num_gc?: number
  }
  response_cache?: {
    effective_config: {
      generation: number
      local_max_bytes: number
      local_max_entry_bytes: number
      reconstruct_max_bytes: number
    }
    applied_config: {
      generation: number
      local_max_bytes: number
      local_max_entry_bytes: number
      reconstruct_max_bytes: number
    }
    entries: number
    max_entries: number
    current_bytes: number
    max_bytes: number
    high_water_bytes: number
    largest_entry_bytes: number
    local_hits: number
    local_misses: number
    remote_hits: number
    remote_misses: number
    expirations: number
    count_evictions: number
    byte_evictions: number
    oversize_bypasses: number
    oversize_rejections: number
    known_unavailable_errors: number
    last_config_sync_at: ISODateString | ''
    last_config_sync_error: string
  }
  runtime: {
    goroutines: number
    available_accounts: number
    total_accounts: number
  }
  requests: {
    active: number
    total: number
  }
  postgres: {
    healthy: boolean
    open: number
    in_use: number
    idle: number
    max_open: number
    wait_count: number
    usage_percent: number
  }
  redis: {
    healthy: boolean
    total_conns: number
    idle_conns: number
    stale_conns: number
    pool_size: number
    usage_percent: number
  }
  traffic: {
    qps: number
    qps_peak: number
    tps: number
    tps_peak: number
    rpm: number
    tpm: number
    error_rate: number
    today_requests: number
    today_tokens: number
    rpm_limit: number
    avg_duration_ms: number
  }
}

export type RuntimeHealthStatus = 'ok' | 'degraded' | 'error' | string

export interface RuntimeCheck {
  component: string
  status: RuntimeHealthStatus
  code: string
  message: string
}

export interface RuntimeStatusResponse {
  updated_at: ISODateString
  status: RuntimeHealthStatus
  service: {
    status: RuntimeHealthStatus
    service_url: string
    admin_url: string
    api_base_url: string
    uptime_seconds: number
    goroutines: number
    go_version: string
    os: string
    arch: string
    pid: number
  }
  database: {
    status: RuntimeHealthStatus
    driver: string
    label: string
    location: string
    healthy: boolean
    error?: string
    open: number
    in_use: number
    idle: number
    max_open: number
    wait_count: number
    usage_percent: number
  }
  cache: {
    status: RuntimeHealthStatus
    driver: string
    label: string
    healthy: boolean
    error?: string
    total_conns: number
    idle_conns: number
    stale_conns: number
    pool_size: number
    usage_percent: number
  }
  usage_log: {
    status: RuntimeHealthStatus
    mode: string
    enabled: boolean
    batch_size: number
    flush_interval_seconds: number
    buffer_length: number
    buffer_capacity: number
    buffer_limit?: number
    dropped_total?: number
  }
  probes: {
    status: RuntimeHealthStatus
    lazy_mode: boolean
    background_refresh_interval_minutes: number
	    usage_probe_max_age_minutes: number
	    usage_probe_concurrency: number
	    usage_probe_responses_fallback_enabled: boolean
	    recovery_probe_interval_minutes: number
    usage_probe_running: boolean
    recovery_probe_running: boolean
    auto_cleanup_running: boolean
  }
  accounts: {
    status: RuntimeHealthStatus
    total: number
    available: number
    active_requests: number
    total_requests: number
    status_counts: Record<string, number>
  }
  image_storage: {
    status: RuntimeHealthStatus
    backend: string
    local_dir?: string
    bucket?: string
    prefix?: string
    healthy: boolean
    error?: string
  }
  admin_auth: {
    status: RuntimeHealthStatus
    source: string
    configured: boolean
  }
  checks: RuntimeCheck[]
}

export interface PromptFilterPatternQuarantine {
  index: number
  name: string
  code: string
  message: string
}

export interface SystemSettings {
  site_name: string
  site_logo: string
  background_image: string
  background_opacity: number
  background_blur: number
  background_glass_opacity: number
  background_glass_blur: number
  max_concurrency: number
  global_rpm: number
  test_model: string
  test_content: string
  test_concurrency: number
  background_refresh_interval_minutes: number
	  usage_probe_max_age_minutes: number
	  usage_probe_concurrency: number
	  usage_probe_responses_fallback_enabled: boolean
	  recovery_probe_interval_minutes: number
  lazy_mode: boolean
  proxy_url?: string
  pg_max_conns: number
  redis_pool_size: number
  auto_clean_unauthorized: boolean
  auto_clean_rate_limited: boolean
  admin_secret: string
  admin_secret_configured?: boolean
  admin_auth_source: 'env' | 'database' | 'disabled' | string
  auto_clean_full_usage: boolean
  auto_clean_error: boolean
  auto_clean_expired: boolean
  auto_reset_credits_enabled: boolean
  auto_reset_credits_before_expiry_min: number
  proxy_pool_enabled: boolean
  fast_scheduler_enabled: boolean
  codex_force_websocket: boolean
  codex_ws_weak_network_mode: boolean
  codex_ws_keepalive_enabled: boolean
  codex_ws_keepalive_interval_sec: number
  codex_ws_hide_upstream_errors: boolean
  codex_ws_silent_retry_enabled: boolean
  codex_ws_silent_max_retries: number
  codex_ws_size_router_enabled: boolean
  codex_ws_busy_acquire_max_wait_sec: number
  codex_ws_busy_overflow_enabled: boolean
  codex_ws_busy_patience_sec: number
  codex_ws_stateless_slots: number
  // GitHub 访问（issue #522）：token 只写不读，响应仅回 configured
  github_token?: string
  github_token_configured?: boolean
  github_proxy_url: string
  // Codex 过载熔断：窗口内 server_is_overloaded 占比达阈值自动暂停调度
  codex_overload_pause_enabled: boolean
  codex_overload_threshold_percent: number
  codex_overload_pause_minutes: number
  codex_overload_window_minutes: number
  codex_continue_thinking_enabled: boolean
  overflow_auto_compact_enabled: boolean
  compact_via_responses_enabled: boolean
  codex_preflight_sse_passthrough_enabled: boolean
  codex_continue_max_rounds: number
  utls_shutdown_timeout_minutes: number
  scheduler_mode: string
  affinity_mode?: string
  session_affinity_spread?: boolean
  grok_affinity_mode?: string
  grok_probe_enabled?: boolean
  grok_probe_interval_minutes?: number
  grok_max_rate_limit_retries?: number
  grok_follow_up_effort_enabled?: boolean
  grok_follow_up_tool_effort?: string
  grok_follow_up_small_effort?: string
  grok_oauth_client_id?: string
  /** 环境变量 GROK_OAUTH_CLIENT_ID 是否正压着上面的设置（只读，后端下发）。 */
  grok_oauth_client_id_env_override?: boolean
  /** 实际生效的 client_id（只读，后端下发）。 */
  grok_oauth_client_id_effective?: string
  max_retries: number
  max_rate_limit_retries: number
  retry_interval_ms: number
  transport_retry_policy: string
  /** 新导入/新建 Codex 账号默认盖上的设备指纹收敛档位（off/device/session/full）。 */
  codex_fingerprint_default_mode: string
  allow_remote_migration: boolean
  database_driver: string
  database_label: string
  cache_driver: string
  cache_label: string
  response_cache_local_max_bytes: number
  response_cache_local_max_entry_bytes: number
  response_cache_reconstruct_max_bytes: number
  readonly response_cache_config_generation: number
  relay_model_cooldown_mode: ModelCooldownMode
  relay_model_cooldown_seconds: number
  relay_model_cooldown_backoff_enabled: boolean
  oauth_model_cooldown_mode: ModelCooldownMode
  oauth_model_cooldown_seconds: number
  oauth_model_cooldown_backoff_enabled: boolean
  expired_cleaned?: number
  model_mapping: string
  codex_model_mapping: string
  payload_rules: string
  reasoning_effort_models: string
  resin_url: string
  resin_platform_name: string
  prompt_filter_enabled: boolean
  prompt_filter_mode: 'monitor' | 'warn' | 'block' | string
  prompt_filter_threshold: number
  prompt_filter_strict_threshold: number
  prompt_filter_strict_terminal_enabled: boolean
  prompt_filter_advanced_config: string
  prompt_filter_log_matches: boolean
  prompt_filter_max_text_length: number
  prompt_filter_sensitive_words: string
  prompt_filter_custom_patterns: string
  prompt_filter_custom_patterns_expected?: string
  prompt_filter_pattern_quarantines?: PromptFilterPatternQuarantine[]
  prompt_filter_disabled_patterns: string
  prompt_filter_review_enabled: boolean
  prompt_filter_review_api_key?: string
  prompt_filter_review_api_key_configured?: boolean
  prompt_filter_review_api_key_count?: number
  prompt_filter_review_base_url: string
  prompt_filter_review_model: string
  prompt_filter_review_timeout_seconds: number
  prompt_filter_review_fail_closed: boolean
  client_compat_mode: 'preserve' | 'auto' | 'force' | string
  codex_min_cli_version: string
  codex_cli_version_sync_enabled: boolean
  codex_cli_version_sync_interval_hours: number
  codex_synced_cli_version?: string
  codex_user_agent_config: string
  usage_log_mode: 'full' | 'errors' | 'off' | string
  usage_log_batch_size: number
  usage_log_flush_interval_seconds: number
  stream_flush_policy: 'immediate' | 'coalesce' | string
  stream_flush_interval_ms: number
  first_token_mode: 'strict' | 'loose' | string
  first_token_timeout_seconds: number
  first_token_excludes_ws_acquire: boolean
  billing_tier_policy: 'actual' | 'requested' | string
  show_full_usage_numbers: boolean
  public_key_usage_page_enabled: boolean
  public_image_studio_page_enabled: boolean
  public_account_portal_page_enabled: boolean
  image_storage_backend: 'local' | 's3' | string
  image_s3_endpoint: string
  image_s3_region: string
  image_s3_bucket: string
  image_s3_access_key: string
  image_s3_secret_key: string
  image_s3_secret_key_configured?: boolean
  image_s3_prefix: string
  image_s3_force_path_style: boolean
  auto_pause_5h_threshold: number
  auto_pause_7d_threshold: number
  auto_pause_5h_guard_band_percent: number
  auto_pause_5h_guard_concurrency: number
  smart_pacing_enabled: boolean
  smart_pacing_min_concurrency: number
  smart_pacing_windows: string
  ignore_usage_limit_status: boolean
}

export interface SetupHintsResponse {
  service_url?: string
  admin_url?: string
  api_base_url?: string
  database?: {
    driver?: string
    label?: string
    location?: string
  }
  cache?: {
    driver?: string
    label?: string
  }
  data?: {
    image_local_dir?: string
    image_storage_backend?: string
  }
  usage?: {
    log_mode?: string
    batch_size?: number
    flush_interval_seconds?: number
  }
}

export interface PromptFilterMatch {
  name: string
  weight: number
  category?: string
  strict?: boolean
  signal_only?: boolean
}

export interface PromptFilterVerdict {
  enabled: boolean
  mode: string
  action: 'allow' | 'warn' | 'block' | string
  score: number
  raw_score: number
  threshold: number
  strict_hit: boolean
  matched: PromptFilterMatch[]
  text_preview?: string
  reason?: string
  extracted_chars: number
  reviewed?: boolean
  review_flagged?: boolean
  review_error?: string
  review_model?: string
}

export interface PromptFilterLog {
  id: number
  created_at: ISODateString
  source: string
  endpoint: string
  protocol?: string
  provider?: string
  model: string
  action: string
  mode: string
  score: number
  audit_score?: number
  threshold: number
  policy_profile?: string
  reason_code?: string
  primary_origin?: string
  strike_eligible?: boolean
  matched_patterns: string
  match_context?: string
  text_preview: string
  full_text: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  client_ip: string
  error_code: string
  review_model: string
  review_flagged: boolean
	review_error: string
	reviewed: boolean
	review_confidence: number | null
	review_threshold: number | null
	review_reason: string
	review_endpoint: string
	review_request_mode: string
	review_latency_ms: number | null
	request_correlation_id?: string
	newapi_policy_status?: string
	newapi_platform?: string
  newapi_user_id?: string
  newapi_request_id?: string
  newapi_decision_id?: string
  session_hash?: string
  client_ip_hash?: string
}

export interface PromptFilterLogsResponse {
  logs: PromptFilterLog[]
  total: number
  page: number
  page_size: number
}

export type PromptPolicyEvaluationState = 'completed' | 'not_run' | 'unavailable' | 'legacy_unknown'
export type PromptPolicyLocalOutcome = 'no_hit' | 'audit_hit' | 'warn' | 'block'
export type PromptPolicyLocalComparison = 'confirmed_miss' | 'upstream_only' | 'evidence_unavailable' | 'local_detected' | 'not_comparable' | 'legacy_unknown'

export interface PromptPolicyIncident {
	id: number
	incident_id: string
	request_correlation_id?: string
	created_at: ISODateString
	attempt_index: number
	transport: string
	endpoint: string
	protocol: string
	provider: string
	model: string
	status_code: number
	account_id: number
	account_name: string
	account_platform: string
	account_group_ids: number[]
	account_group_names: string[]
	api_key_id: number
	api_key_name: string
	api_key_masked: string
	api_key_allowed_group_ids: number[]
	api_key_allowed_group_names: string[]
	routing_snapshot_state: 'event_snapshot' | 'current_inferred' | 'unavailable'
	platform: string
	newapi_policy_status?: string
	newapi_platform?: string
	newapi_user_id?: string
	newapi_request_id?: string
	session_hash?: string
	client_ip_hash?: string
	source_ref?: string
	upstream_error_code: string
	upstream_error: string
	local_evaluation_state: PromptPolicyEvaluationState
	local_outcome: PromptPolicyLocalOutcome
	local_action: string
	local_score: number | null
	local_raw_score: number | null
	local_audit_score: number | null
	local_audit_raw_score: number | null
	local_threshold: number
	local_mode: string
	local_policy_profile: string
	local_reason_code: string
	local_reason: string
	local_primary_origin: string
	local_strike_eligible: boolean
	local_review_model: string
	local_review_flagged: boolean
	local_review_error: string
	local_matched_patterns: string
	prompt_fingerprint: string
	prompt_preview: string
	prompt_text: string
	prompt_available: boolean
	local_comparison: PromptPolicyLocalComparison
	candidate_id?: number
	candidate_evidence_id?: number
	local_miss: boolean
}

export interface PromptPolicyIncidentsResponse {
	incidents: PromptPolicyIncident[]
	total: number
	page: number
	page_size: number
}

export interface PromptPolicyAuditHealth {
	ok: boolean
	status: 'healthy' | 'degraded' | string
	storage_ready: boolean
	prompt_filter_enabled: boolean
	review_enabled: boolean
	review_fail_closed: boolean
	review_pool: {
		configured: number
		available: number
		cooling_down: number
		probing: number
		next_retry_at?: ISODateString
	}
	conversation_lock_enabled: boolean
	incident_count: number
	latest_incident_id?: string
	latest_incident_at?: ISODateString
	queue: {
		enqueued: number
		completed: number
		dropped_high: number
		dropped_low: number
		failed: number
		pending_high: number
		pending_low: number
		retained_bytes: number
	}
}

export interface PromptPolicyIncidentDetailResponse {
	incident: PromptPolicyIncident
	matches: PromptFilterMatch[]
	candidate?: {
		id: number
		status: string
		kind: string
		name: string
		category: string
		evidence_count: number
		sample_preview?: string
	}
	evidence?: {
		id: number
		source_kind: string
		source_ref?: string
		prompt_policy_incident_id?: string
		observed_at: ISODateString
	}
}

export type PromptRiskSubjectType = 'newapi_user' | 'session' | 'api_key' | 'client_ip' | 'upstream_account'
export type PromptRiskLevel = 'low' | 'observed' | 'elevated' | 'high' | 'critical'

export interface PromptRiskScoreBreakdown {
  local_signal: number
  upstream_signal: number
  recurrence: number
  identity_confidence: number
}

export type PromptRiskTrustStatus = 'active' | 'suspended' | 'revoked' | 'expired'

export interface PromptRiskTrustPolicy {
  id: number
  subject_type: PromptRiskSubjectType
  subject_key: string
  status: PromptRiskTrustStatus | string
  source: 'manual' | 'automatic' | string
  reason?: string
  risk_threshold: number
  valid_until: ISODateString
  last_evaluated_at?: ISODateString
  last_risk_score: number
  last_risk_level?: PromptRiskLevel | string
  bypass_count: number
  last_bypass_at?: ISODateString
  model_review_count: number
  last_model_review_at?: ISODateString
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptRiskTrustEvent {
  id: number
  policy_id: number
  subject_type: PromptRiskSubjectType
  subject_key: string
  event_type: string
  reason?: string
  risk_score: number
  risk_level?: PromptRiskLevel | string
  request_id_hash?: string
  created_at: ISODateString
}

export interface PromptRiskAdaptiveReviewBasis {
  enabled: boolean
  review_enabled: boolean
  eligible: boolean
  decision: 'disabled' | 'not_person' | 'adaptive_active' | 'suspended' | 'eligible' | 'building_history' | 'unavailable' | string
  clean_review_count: number
  positive_evidence_count: number
  min_clean_reviews: number
  min_observation_hours: number
  observation_hours: number
  sample_percent: number
  force_review_interval_minutes: number
  trust_duration_hours: number
  risk_threshold: number
  first_clean_at?: ISODateString
  last_clean_at?: ISODateString
  next_forced_review_at?: ISODateString
  force_review_due: boolean
}

export interface PromptRiskProfile {
  subject_type: PromptRiskSubjectType
  subject_key: string
  subject_display: string
  platform?: string
  newapi_user_id?: string
  newapi_user_name?: string
  newapi_user_email?: string
  newapi_user_group?: string
  is_person: boolean
  identity_confidence: number
  risk_score: number
  risk_level: PromptRiskLevel
  recommended_actions: string[]
  score_breakdown: PromptRiskScoreBreakdown
  has_activity: boolean
  identity_source?: string
  identity_updated_at?: ISODateString
  latest_at: ISODateString
  event_count: number
  events_10m: number
  events_24h: number
  events_7d: number
  events_30d: number
  upstream_cy_count: number
  confirmed_miss_count: number
  local_block_count: number
  local_warn_count: number
  distinct_fingerprints: number
  repeated_fingerprints: number
  api_key_id?: number
  api_key_name?: string
  api_key_masked?: string
  account_id?: number
  account_name?: string
  trust_policy?: PromptRiskTrustPolicy
  conversation_lock?: PromptConversationLock
}

export interface PromptConversationLock {
  id: number
  lock_key: string
  status: 'active' | 'unlocked'
  platform: string
  newapi_user_id: string
  session_fingerprint: string
  session_hash: string
  incident_id?: string
  decision_id: string
  request_id?: string
  reason_code: string
  endpoint?: string
  model?: string
  trigger_count: number
  unlock_count: number
  locked_at: ISODateString
  unlocked_at?: ISODateString
  unlock_reason?: string
  restriction_scope?: 'conversation' | 'user_cooldown'
  expires_at?: ISODateString
  remaining_seconds?: number
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptRiskEvent {
  id: number
  created_at: ISODateString
  source_type: string
  source_id: string
  incident_id?: string
  prompt_filter_log_id?: number
  request_correlation_id?: string
  subject_type: PromptRiskSubjectType
  subject_key: string
  subject_display: string
  platform?: string
  newapi_user_id?: string
  newapi_user_name?: string
  newapi_user_email?: string
  newapi_user_group?: string
  is_person: boolean
  identity_confidence: number
  event_kind: string
  request_risk_score: number
  evidence_confidence: number
  reason_code?: string
  action?: string
  local_outcome?: string
  local_comparison?: string
  endpoint?: string
  model?: string
  prompt_fingerprint?: string
  prompt_preview?: string
  api_key_id?: number
  api_key_name?: string
  api_key_masked?: string
  account_id?: number
  account_name?: string
}

export interface PromptRiskProfilesResponse {
  profiles: PromptRiskProfile[]
  total: number
  page: number
  page_size: number
  scoring_version: string
  guardrail: string
}

export interface PromptRiskProfileDetailResponse {
  profile: PromptRiskProfile
  events: PromptRiskEvent[]
  trust_events: PromptRiskTrustEvent[]
  adaptive_review_basis: PromptRiskAdaptiveReviewBasis
  event_total: number
  event_page: number
  event_page_size: number
  trust_event_total: number
  trust_event_page: number
  trust_event_page_size: number
  scoring_version: string
  guardrail: string
}

export interface PromptFilterTestResponse {
  verdict: PromptFilterVerdict
  decision?: PromptGuardDecision
  protocol?: string
  provider?: string
  endpoint?: string
  model?: string
}

export interface PromptReviewTestRequest {
  text: string
  api_key?: string
  base_url: string
  model: string
  request_mode: 'moderations' | 'chat_completions' | string
  system_prompt: string
  user_prompt_template: string
  payload_template: string
  confidence_threshold: number
  moderation_thresholds: Record<string, number>
  timeout_seconds: number
  max_concurrent: number
  max_text_length: number
  test_all_keys?: boolean
}

export interface PromptReviewKeyTestResult {
  key_index: number
  key_id?: string
  key_masked?: string
  ok: boolean
  endpoint?: string
  model?: string
  flagged: boolean
  confidence: number
  reason?: string
  highest_category?: string
  decision_category?: string
  decision_score?: number
  decision_threshold?: number
  category_scores?: Record<string, number>
  moderation_thresholds?: Record<string, number>
  latency_ms: number
  error?: string
}

export interface PromptReviewAPIKeyDescriptor {
  id: string
  index: number
  masked: string
}

export interface PromptReviewAPIKeysResponse {
  items: PromptReviewAPIKeyDescriptor[]
  count: number
}

export interface PromptReviewProfile {
  id: string
  name: string
  base_url: string
  model: string
  request_mode: string
  timeout_seconds: number
  key_count: number
  active: boolean
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptReviewProfilesResponse {
  profiles: PromptReviewProfile[]
}

export interface PromptReviewTestResponse {
  ok: boolean
  endpoint: string
  model: string
  flagged: boolean
  confidence: number
  confidence_threshold: number
  reason?: string
  highest_category?: string
  decision_category?: string
  decision_score?: number
  decision_threshold?: number
  category_scores?: Record<string, number>
  moderation_thresholds?: Record<string, number>
  latency_ms: number
  key_count?: number
  results?: PromptReviewKeyTestResult[]
}

export interface PromptFilterRulePatternTestResponse {
  matched: boolean
  error?: string
}

export type PromptGuardMode = 'inherit' | 'off' | 'shadow' | 'warn' | 'enforce'

export type PromptGuardProfile = 'balanced' | 'strict' | 'research'

export type PromptGuardProvider = 'openai' | 'anthropic' | 'xai' | 'unknown'

export interface PromptGuardDecision {
  enabled: boolean
  mode: string
  profile: string
  application_prompt_kind?: string
  action: string
  would_action: string
  score: number
  raw_score: number
  audit_score?: number
  audit_raw_score?: number
  reason_code?: string
  reason?: string
  terminal?: boolean
  strike_eligible?: boolean
  truncated?: boolean
  current_user_truncated?: boolean
  auxiliary_truncated?: boolean
  primary_origin?: string
  primary_detector?: string
  signals?: PromptGuardSignal[]
  errors?: string[]
}

export interface PromptGuardSignal {
  detector: string
  family: string
  category?: string
  correlation_key?: string
  origin: string
  layer_mode: string
  score: number
  raw_score: number
  confidence: number
  suggested_action: string
  terminal_candidate?: boolean
  strike_eligible?: boolean
  reason?: string
  matches?: PromptFilterMatch[]
}

export interface PromptGuardPerformanceConfig {
  async_shadow_auxiliary_enabled: boolean
  exact_segment_cache_enabled: boolean
  exact_segment_cache_entries: number
  exact_segment_cache_ttl_seconds: number
  max_segments: number
  max_current_user_bytes: number
  max_auxiliary_bytes: number
  scan_chunk_bytes: number
  scan_overlap_bytes: number
  shadow_workers: number
  shadow_queue_size: number
  shadow_overflow_mode: 'drop'
}

export type PromptGuardLayer =
  | 'current_user'
  | 'history'
  | 'system'
  | 'developer'
  | 'instructions'
  | 'tool_output'
  | 'tool_arguments'
  | 'attachment_refs'
  | 'session_context'
  | 'attachment_content'

export interface PromptGuardConfig {
  mode: PromptGuardMode
  default_profile: PromptGuardProfile
  allow_trusted_overrides: boolean
  provider_profiles: Partial<Record<PromptGuardProvider, PromptGuardProfile>>
  layers: Record<PromptGuardLayer, { mode: PromptGuardMode }>
  performance: PromptGuardPerformanceConfig
}

export type AdvancedConfigObject = Record<string, unknown>

export interface AdvancedConfigDocument {
  ok: boolean
  value: AdvancedConfigObject | null
  error: 'invalid_json' | 'root_not_object' | null
}

export interface AdvancedConfigPatch {
  path: readonly string[]
  value?: unknown
  remove?: boolean
}

export interface AdvancedConfigPatchResult extends AdvancedConfigDocument {
  serialized: string
}

function isAdvancedConfigObject(value: unknown): value is AdvancedConfigObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/**
 * Parse the persisted advanced configuration without normalizing or rebuilding
 * it. Callers can derive a typed view separately while retaining this original
 * tree as the source of truth for compatible field-level updates.
 */
export function parseAdvancedConfigDocument(raw: string): AdvancedConfigDocument {
  try {
    const value = JSON.parse(raw || '{}') as unknown
    if (!isAdvancedConfigObject(value)) {
      return { ok: false, value: null, error: 'root_not_object' }
    }
    return { ok: true, value, error: null }
  } catch {
    return { ok: false, value: null, error: 'invalid_json' }
  }
}

export function readAdvancedConfigPath(
  value: AdvancedConfigObject | null,
  path: readonly string[],
): unknown {
  let current: unknown = value
  for (const key of path) {
    if (!isAdvancedConfigObject(current)) return undefined
    current = current[key]
  }
  return current
}

/**
 * Apply only explicitly edited JSON paths to a freshly parsed document. This
 * preserves unknown top-level and nested fields, including future enum values.
 * Invalid JSON is returned untouched so the UI can block saving instead of
 * silently replacing it with defaults.
 */
export function patchAdvancedConfigDocument(
  raw: string,
  patches: readonly AdvancedConfigPatch[],
): AdvancedConfigPatchResult {
  const parsed = parseAdvancedConfigDocument(raw)
  if (!parsed.ok || !parsed.value) {
    return { ...parsed, serialized: raw }
  }

  const root = parsed.value
  for (const patch of patches) {
    if (patch.path.length === 0) continue
    let current = root
    for (const key of patch.path.slice(0, -1)) {
      const child = current[key]
      if (isAdvancedConfigObject(child)) {
        current = child
      } else {
        const next: AdvancedConfigObject = {}
        current[key] = next
        current = next
      }
    }
    const leaf = patch.path[patch.path.length - 1]
    if (patch.remove) delete current[leaf]
    else current[leaf] = patch.value
  }

  return {
    ok: true,
    value: root,
    error: null,
    serialized: JSON.stringify(root),
  }
}

export interface PromptFilterRule {
  name: string
  pattern: string
  weight: number
  category?: string
  strict?: boolean
  enabled?: boolean
  builtin?: boolean
}

export interface PromptFilterRulesResponse {
  builtin_patterns: PromptFilterRule[]
  custom_patterns: PromptFilterRule[]
  disabled_patterns: string[]
}

export interface PromptIntelligenceRuleDraft {
  name: string
  pattern: string
  weight: number
  category: string
  strict: boolean
  rationale?: string
  source_url?: string
  change_type?: 'new' | 'update' | string
}

export interface PromptIntelligenceCandidate extends PromptIntelligenceRuleDraft {
  id: number
  fingerprint: string
  kind: 'pattern' | 'evidence' | string
  lifecycle_status: 'pending' | 'published' | 'dismissed' | 'superseded' | string
  source?: string
  evidence_count: number
  sample_preview?: string
  protocol?: string
  provider?: string
  model?: string
  api_key_id?: number
  api_key_name?: string
  ai_analyzed?: boolean
  ai_analysis_count?: number
  ai_analyzed_at?: string
  latest_ai_analysis?: PromptIntelligenceAIAnalysisResponse
  created_at?: string
  updated_at?: string
  last_seen_at?: string
}

export interface PromptIntelligenceRunCandidate extends PromptIntelligenceRuleDraft {
  id?: number
  fingerprint?: string
  kind?: 'pattern' | 'evidence' | string
  lifecycle_status?: string
  status?: 'new' | 'update' | string
  source?: string
  evidence_count?: number
  sample_preview?: string
}

export interface PromptIntelligenceCandidatesResponse {
  candidates: PromptIntelligenceCandidate[]
  total: number
}

export interface PromptIntelligenceEvidence {
  id: number
  source_kind: string
  source_ref?: string
  sample_preview?: string
  metadata: Record<string, unknown>
  protocol?: string
  provider?: string
  model?: string
  api_key_id?: number
  api_key_name?: string
  observed_at: string
}

export interface PromptIntelligenceEvidenceResponse {
  candidate: PromptIntelligenceCandidate
  evidence: PromptIntelligenceEvidence[]
}

export type PromptIntelligenceAIProvider = 'review' | 'account_pool'
export type PromptIdentityUpdateMode = 'suggest' | 'guarded_auto'

export interface PromptIntelligenceAIAnalysisRequest {
  provider: PromptIntelligenceAIProvider
  model?: string
  api_key_id?: number
  identity_update_mode: PromptIdentityUpdateMode
}

export interface PromptIntelligenceGatewayKey {
  id: number
  name: string
  masked: string
  status: 'active' | 'expired' | 'quota_exhausted' | string
}

export interface PromptIntelligenceAIProvidersResponse {
  review: { configured: boolean; model: string; key_count: number }
  gateway_keys: PromptIntelligenceGatewayKey[]
}

export interface PromptIntelligenceAIIdentityPatch {
  clauses: string[]
  rationale?: string
}

export interface PromptIntelligenceAIDecision {
  decision: 'no_change' | 'rule' | 'identity' | 'both'
  confidence: number
  reason: string
  rule?: PromptIntelligenceRuleDraft
  identity_patch?: PromptIntelligenceAIIdentityPatch
}

export interface PromptIdentityUpdateResult {
  mode: 'suggest' | 'guarded_auto' | 'manual' | 'rollback' | string
  suggested: boolean
  eligible: boolean
  applied: boolean
  rolled_back?: boolean
  analysis_evidence_id: number
  revision_evidence_id?: number
  clauses?: string[]
  block_reason?: string
}

export interface PromptIntelligenceAIAnalysisResponse {
  analysis_evidence_id: number
  provider: PromptIntelligenceAIProvider
  model: string
  decision: PromptIntelligenceAIDecision
  rule_candidate?: PromptIntelligenceCandidate
  rule_error?: string
  identity_update: PromptIdentityUpdateResult
}

export interface PromptIntelligenceHistoryResponse {
  runs: PromptIntelligenceRun[]
  total: number
}

export interface PromptIntelligenceRun {
  started_at: string
  finished_at: string
  queries: string[]
  sources: Array<{ provider: string; title: string; url: string; description: string; updated_at: string }>
  candidates: PromptIntelligenceRunCandidate[]
  model_calls: number
  staged?: number
  added?: number
  errors: string[]
}

export interface ModelInfo {
  id: string
  enabled: boolean
  category: string
  source: string
  pro_only: boolean
  api_key_auth_available: boolean
  last_seen_at?: string
  updated_at?: string
}

export interface ModelsResponse {
  models: string[]
  // Grok 渠道账号声明模型的并集;渠道选 grok 时模型下拉用这份
  grok_models?: string[]
  items?: ModelInfo[]
  last_synced_at?: string
  source_url: string
  warning?: string
}

export interface ModelSyncResponse {
  added: number
  updated: number
  unchanged: number
  skipped: string[]
  models: string[]
  items: ModelInfo[]
  last_synced_at: string
  source_url: string
}

export interface CPAExportEntry {
  type: string
  email: string
  expired: string
  id_token: string
  account_id: string
  access_token: string
  last_refresh: string
  refresh_token: string
}

export interface UsageStats {
  total_requests: number
  total_tokens: number
  total_prompt_tokens: number
  total_completion_tokens: number
  total_input_tokens?: number
  total_cached_tokens: number
  total_cache_rate?: number
  total_account_billed: number
  total_user_billed: number
  avg_account_billed_per_request: number
  avg_user_billed_per_request: number
  today_requests: number
  today_tokens: number
  today_input_tokens?: number
  today_prompt_tokens?: number
  today_completion_tokens?: number
  today_cached_tokens?: number
  today_cache_rate?: number
  today_account_billed: number
  today_user_billed: number
  rpm: number
  tpm: number
  avg_duration_ms: number
  avg_first_token_ms?: number
  error_rate: number
  feature_stats: UsageFeatureStats
  model_stats: UsageModelStat[]
  endpoint_stats: UsageEndpointStat[]
  api_key_stats: UsageAPIKeyStat[]
}

export interface UsageModelStat {
  model: string
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  account_billed: number
  user_billed: number
  error_count: number
}

export interface UsageFeatureStats {
  stream_requests: number
  sync_requests: number
  fast_requests: number
  cache_hit_requests: number
  reasoning_requests: number
  image_requests: number
  retry_requests: number
  error_requests: number
}

export interface UsageEndpointStat {
  endpoint: string
  requests: number
  tokens: number
  error_count: number
  user_billed: number
}

export interface UsageAPIKeyStat {
  api_key_id: number
  label: string
  requests: number
  tokens: number
  error_count: number
  user_billed: number
}

// APIKeyTokenStat 是 /usage/api-keys 端点返回项，比 UsageAPIKeyStat 字段更细
// （分列 input/output/cached token），且不限条数。
export interface APIKeyTokenStat {
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  label: string
  requests: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  total_tokens: number
  error_count: number
  user_billed: number
}

// APIKeyAccountGroup 是上游账号分组的精简展示项（Token 用量明细用）。
export interface APIKeyAccountGroup {
  id: number
  name: string
  color: string
}

// APIKeyAccountStat 是 /usage/api-keys/:id/accounts 端点返回项：
// 某个下游 Key 在时间区间内按上游账号拆分的用量（账号"按 Key 分解"的转置）。
export interface APIKeyAccountStat {
  account_id: number
  account_name: string
  account_email: string
  account_deleted?: boolean
  groups?: APIKeyAccountGroup[]
  requests: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  total_tokens: number
  error_count: number
  account_billed: number
  user_billed: number
}

export interface APIKeyAccountGroupUsage {
  id: number
  name: string
  color: string
  accounts: number
  requests: number
  total_tokens: number
  account_billed: number
  user_billed: number
}

export interface APIKeyAccountUsageSummary {
  accounts: number
  requests: number
  total_tokens: number
  account_billed: number
  user_billed: number
}

export interface APIKeyAccountUsageReconciliation {
  grouped_total: APIKeyAccountUsageSummary
  ungrouped: APIKeyAccountUsageSummary
  duplicate: APIKeyAccountUsageSummary
  unique_grouped_accounts: number
  multi_group_accounts: number
}

export interface APIKeyAccountStatsResponse {
  items: APIKeyAccountStat[]
  groups: APIKeyAccountGroupUsage[]
  summary: APIKeyAccountUsageSummary
  reconciliation?: APIKeyAccountUsageReconciliation
  /** Active accounts use current memberships; deleted accounts use their last retained membership. */
  membership_basis: 'current_and_deleted_last_membership'
}

export interface UsageLog {
  id: number
  account_id: number
  // 上游渠道(codex/grok),写入时固化;历史行回填,可能为空
  channel?: string
  client_ip: string
  client_user_agent: string
  upstream_user_agent: string
  user_agent_overridden: boolean
  internal_reason: string
  parent_request_id: string
  endpoint: string
  model: string
  effective_model: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  status_code: number
  duration_ms: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  first_token_ms: number
  ws_acquire_ms?: number
  reasoning_effort: string
  inbound_endpoint: string
  upstream_endpoint: string
  stream: boolean
  compact: boolean
  has_compaction_history: boolean
  via_websocket?: boolean
  cached_tokens: number
  service_tier: string
  requested_service_tier: string
  actual_service_tier: string
  billing_service_tier: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  image_count: number
  image_width: number
  image_height: number
  image_bytes: number
  image_format: string
  image_size: string
  account_name: string
  account_email: string
  created_at: ISODateString
  account_billed: number
  user_billed: number
  input_cost: number
  output_cost: number
  cache_read_cost: number
  total_cost: number
  input_price_per_mtoken: number
  output_price_per_mtoken: number
  cache_read_price_per_mtoken: number
  rate_multiplier: number
  long_context?: boolean
  long_context_threshold?: number
  is_retry_attempt: boolean
  attempt_index: number
  upstream_error_kind: string
	error_message: string
	prompt_policy_incident_id?: string
}

export type UsageLogsResponse = ApiListResponse<'logs', UsageLog>

export interface UsageLogsPagedResponse {
  logs: UsageLog[]
  total: number
}

export interface OpsErrorSummary {
  total_errors: number
  status_4xx: number
  status_5xx: number
  unauthorized: number
  rate_limited: number
  canceled: number
  timeouts: number
  retry_attempts: number
  avg_duration_ms: number
}

export interface ChartTimelinePoint {
  bucket: string
  requests: number
  avg_latency: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  errors_4xx: number
  errors_5xx: number
}

export interface ChartModelPoint {
  model: string
  requests: number
}

export interface ChartAggregation {
  timeline: ChartTimelinePoint[]
  models: ChartModelPoint[]
}

export interface ModelPricingOverride {
  source?: string
  input?: number
  cached_input?: number
  output?: number
  input_priority?: number
  cached_input_priority?: number
  output_priority?: number
  input_long?: number
  cached_input_long?: number
  output_long?: number
	input_long_priority?: number
	cached_input_long_priority?: number
	output_long_priority?: number
	long_context_threshold_tokens?: number
}

export interface OfficialPricingSyncConfig {
	enabled: boolean
	interval_minutes: number
	include_openai: boolean
	include_grok: boolean
	last_attempt_at?: string
	last_success_at?: string
	last_error?: string
	last_warning?: string
}

export interface OfficialPricingSyncResult {
	fetched: number
	applied: number
	skipped: number
	missing?: string[]
	sources: string[]
	warnings?: string[]
	synced_at: string
}

/**
 * 单条「该 Key × 某账号分组 / 某账号」的用量上限（issue #439）。
 * 0 或缺省表示该维度不限；超额后默认把该 scope 的账号从调度候选中剔除。
 */
export interface APIKeyScopeLimit {
  scope_type: 'group' | 'account'
  scope_id: number
  /** skip: 剔除该 scope 后继续用其它账号（默认）；reject: 直接 429。 */
  on_exhausted?: 'skip' | 'reject'
  cost_5h?: number
  cost_1d?: number
  cost_7d?: number
  cost_30d?: number
  token_5h?: number
  token_1d?: number
  token_7d?: number
  token_30d?: number
  requests_1d?: number
  /** 该 Key 在这条 scope 上的最大在途请求数（进程内软上限）。 */
  max_concurrency?: number
  /** 累计额度：不随时间回落，用完需手动重置。 */
  quota_cost?: number
  quota_tokens?: number
  quota_requests?: number
}

/** 某条 scope 限额在某窗口的当前用量（GET /api/admin/keys/:id/scope-usage）。 */
export interface APIKeyScopeUsageWindow {
  window: string
  requests: number
  tokens: number
  user_billed: number
  cost_limit?: number
  token_limit?: number
  request_limit?: number
  exhausted: boolean
}

export interface APIKeyScopeCumulativeUsage {
  used_cost: number
  used_tokens: number
  used_requests: number
  quota_cost?: number
  quota_tokens?: number
  quota_requests?: number
  reset_count: number
  last_reset_at?: string
  exhausted: boolean
}

/** 一条 scope 预算被判定耗尽的运行态统计（网关进程内，重启清零）。 */
export interface APIKeyScopeSkipStat {
  requests: number
  first_at: string
  last_at: string
  last_message: string
}

export interface APIKeyScopeUsageItem {
  scope_type: 'group' | 'account'
  scope_id: number
  scope_name: string
  scope_exists: boolean
  on_exhausted: 'skip' | 'reject'
  windows: APIKeyScopeUsageWindow[]
  cumulative?: APIKeyScopeCumulativeUsage
  skips?: APIKeyScopeSkipStat
}

/** 列表页用的 scope 预算概览（只给最紧的那个窗口）。 */
export interface APIKeyScopeSummaryItem {
  scope_type: 'group' | 'account'
  scope_id: number
  scope_name: string
  on_exhausted: 'skip' | 'reject'
  window: string
  metric: string
  ratio: number
  exhausted: boolean
  skip_requests?: number
}

export interface APIKeyLimits {
  model_allow?: string[]
  model_deny?: string[]
  plan_allow?: string[]
  no_affinity_group_ids?: number[]
  rpm?: number
  rpd?: number
  max_concurrency?: number
  cost_limit_5h?: number
  cost_limit_7d?: number
  cost_limit_30d?: number
  /** 自然日(服务器本地时区)金额上限,零点清零;与滑动窗口语义不同(issue #460)。 */
  cost_limit_daily?: number
  token_limit_5h?: number
  token_limit_7d?: number
  token_limit_30d?: number
  token_limit_daily?: number
  disable_image_generation?: boolean
  /** 图片工具策略：""/"allow" 放行、"strip" 剥离后继续文本请求、"block" 命中即 403。 */
  image_generation_policy?: "allow" | "strip" | "block"
  upstream_channel?: "codex" | "grok"
  /** 允许该 Key 使用 ChatGPT Live（/v1/live）。默认关闭。 */
  allow_live?: boolean
  /** 分组 / 账号维度的用量预算（issue #439）。 */
  scope_limits?: APIKeyScopeLimit[]
}

export interface APIKeyWindowUsage {
  requests?: number
  tokens?: number
  user_billed?: number
  cost_5h: number
  cost_7d: number
  cost_30d: number
  cost_today?: number
}

export interface APIKeyRow {
  id: number
  name: string
  key: string
  raw_key: string
  quota_limit: number
  quota_used: number
  total_used: number
  reset_count: number
  last_reset_at?: ISODateString | null
  expires_at?: ISODateString | null
  status?: 'active' | 'expired' | 'quota_exhausted'
  allowed_group_ids?: number[]
  limits?: APIKeyLimits
  window_usage?: APIKeyWindowUsage
  last_used_at?: ISODateString | null
  created_at: ISODateString
}

export type APIKeysResponse = ApiListResponse<'keys', APIKeyRow>

export type PromptFilterScope = 'inherit' | 'local_only' | 'off'

export interface PromptFilterNewAPIBinding {
  api_key_id: number
  platform_code: string
  platform_name: string
  enabled: boolean
  require_signed_identity: boolean
  prompt_filter_scope: PromptFilterScope
  secret_configured: boolean
  secret_masked: string
  previous_secret_active: boolean
  previous_secret_expires_at?: ISODateString | null
  updated_at: ISODateString
  /** 仅在创建或轮换成功的响应中出现，列表和详情接口不会回显明文。 */
  secret?: string
}

export interface PromptFilterNewAPIBindingsResponse {
  bindings: PromptFilterNewAPIBinding[]
}

export interface CreatePromptFilterNewAPIBindingRequest {
  api_key_id: number
  platform_code: string
  platform_name: string
  enabled?: boolean
  require_signed_identity?: boolean
  prompt_filter_scope?: PromptFilterScope
}

export type UpdatePromptFilterNewAPIBindingRequest = Partial<
  Omit<CreatePromptFilterNewAPIBindingRequest, 'api_key_id'>
>

export interface CreateAPIKeyRequest {
  name: string
  key?: string
  quota_limit?: number
  quota?: number
  expires_at?: string
  expires_in_days?: number
  allowed_group_ids?: number[]
  limits?: APIKeyLimits
}

export interface UpdateAPIKeyRequest {
  name?: string
  quota_limit?: number | null
  quota?: number | null
  reset_quota?: boolean
  expires_at?: string | null
  expires_in_days?: number
  allowed_group_ids?: number[]
  limits?: APIKeyLimits
}

export interface PublicAPIKeyUsageKey {
  name: string
  key: string
  quota_limit: number
  quota_used: number
  total_used: number
  reset_count: number
  last_reset_at?: ISODateString | null
  expires_at?: ISODateString | null
  limits: APIKeyLimits
  status: 'active' | 'expired' | 'quota_exhausted'
  created_at: ISODateString
}

export interface PublicAPIKeyUsageRange {
  name: 'today' | '7d' | '30d' | 'all' | string
  start?: ISODateString | null
  end: ISODateString
}

export interface PublicAPIKeyWindowUsage {
  requests: number
  tokens: number
  user_billed: number
  /** 窗口内最早一笔用量时间(无用量时缺省)。 */
  oldest_at?: ISODateString
  /** fixed=自然日固定窗口(reset_at 清零);sliding=滑动窗口(decay_at 开始回落)。 */
  window_kind: 'fixed' | 'sliding'
  reset_at?: ISODateString
  decay_at?: ISODateString
}

export interface PublicAPIKeyUsageWindows {
  today: PublicAPIKeyWindowUsage
  last_5h: PublicAPIKeyWindowUsage
  last_7d: PublicAPIKeyWindowUsage
  last_30d: PublicAPIKeyWindowUsage
}

export interface PublicAPIKeyUsageSummary {
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  error_count: number
  user_billed: number
  avg_duration_ms: number
  avg_first_token_ms: number
  rpm: number
  tpm: number
}

export interface PublicAPIKeyUsageBreakdown {
  name: string
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  error_count: number
  user_billed: number
}

export interface PublicAPIKeyUsageLog {
  id: number
  endpoint: string
  model: string
  effective_model: string
  status_code: number
  duration_ms: number
  first_token_ms: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  total_tokens: number
  user_billed: number
  input_cost: number
  output_cost: number
  cache_read_cost: number
  total_cost: number
  input_price_per_mtoken: number
  output_price_per_mtoken: number
  cache_read_price_per_mtoken: number
  rate_multiplier: number
  long_context: boolean
  service_tier: string
  stream: boolean
  compact: boolean
  has_compaction_history: boolean
  via_websocket: boolean
  upstream_error_kind: string
  created_at: ISODateString
}

export interface PublicAPIKeyUsageReport {
  summary: PublicAPIKeyUsageSummary
  windows: PublicAPIKeyUsageWindows
  models: PublicAPIKeyUsageBreakdown[]
  endpoints: PublicAPIKeyUsageBreakdown[]
  recent_logs: PublicAPIKeyUsageLog[]
  recent_logs_total: number
  recent_logs_page: number
  recent_logs_page_size: number
}

export interface PublicAPIKeyUsageResponse {
  key: PublicAPIKeyUsageKey
  range: PublicAPIKeyUsageRange
  usage: PublicAPIKeyUsageReport
}

export interface CreateAPIKeyResponse {
  id: number
  key: string
  name: string
  quota_limit: number
  quota_used: number
  expires_at?: ISODateString | null
  allowed_group_ids?: number[]
}

export interface ImagePromptTemplate {
  id: number
  name: string
  prompt: string
  model: string
  size: string
  quality: string
  output_format: string
  background: string
  style: string
  tags: string[]
  favorite: boolean
  usage_count: number
  last_used_at?: ISODateString
  created_at: ISODateString
  updated_at: ISODateString
}

export interface ImageAsset {
  id: number
  job_id: number
  template_id: number
  filename: string
  proxy_url?: string
  thumbnail_url?: string
  mime_type: string
  bytes: number
  width: number
  height: number
  model: string
  requested_size: string
  actual_size: string
  quality: string
  output_format: string
  revised_prompt: string
  created_at: ISODateString
  cache_b64_json?: string
}

export interface ImageGenerationJob {
  id: number
  status: 'queued' | 'running' | 'succeeded' | 'failed' | string
  prompt: string
  params_json: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  error_message: string
  warning?: string
  duration_ms: number
  created_at: ISODateString
  started_at?: ISODateString
  completed_at?: ISODateString
  assets?: ImageAsset[]
}

export interface ImagePromptTemplatesResponse {
  templates: ImagePromptTemplate[]
}

export interface ImageJobResponse {
  job: ImageGenerationJob
}

export interface ImageJobsResponse {
  jobs: ImageGenerationJob[]
  total: number
}

export interface ImageAssetsResponse {
  assets: ImageAsset[]
  total: number
}

export interface ImagePromptTemplatePayload {
  name?: string
  prompt?: string
  model?: string
  size?: string
  quality?: string
  output_format?: string
  background?: string
  style?: string
  tags?: string[]
  favorite?: boolean
}

export interface CreateImageJobPayload {
  prompt: string
  model?: string
  size?: string
  quality?: string
  output_format?: string
  background?: string
  style?: string
  upscale?: string
  strict_size?: boolean
  upscale_fit?: 'pad' | 'cover'
  api_key_id?: number
  template_id?: number
  input_images?: string[]
}

export type ApiListResponse<K extends string, T> = {
  [P in K]: T[]
}

export interface OAuthURLResponse {
  auth_url: string
  session_id: string
}

// 公开账号自助门户:生成授权链接的响应。
export interface AccountPortalAuthURLResponse {
  auth_url: string
  session_id: string
}

// 公开账号自助门户:提交授权码的响应。
export interface AccountPortalSubmitResponse {
  message: string
}

export interface UpdateOAuthAccountRequest {
  session_id: string
  code: string
  state: string
  proxy_url?: string
}

export interface OAuthExchangeResponse {
  message: string
  id: number
  email: string
  plan_type: string
}

export interface ObservedInstructionsSample {
  model: string
  originator: string
  instructions: string
  length: number
  truncated: boolean
  observed_at: string
}

export interface ObservedInstructionsResponse {
  samples: ObservedInstructionsSample[]
}
