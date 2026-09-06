import type { ChangeEvent, DragEvent, ReactNode } from "react";
import { memo, useCallback, useEffect, useRef, useState, useMemo } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { api, getAdminKey, resetAdminAuthState } from "../api";
import type { ProxyRow } from "../api";
import { ProxyField } from "../components/ProxyField";
import AccountProxyBadge from "../components/AccountProxyBadge";
import AccountProxyQuickEditor from "../components/AccountProxyQuickEditor";
import {
  buildProxyBindingContext,
  type ProxyBindingContext,
} from "../lib/accountProxyBinding";
import Modal from "../components/Modal";
import ChannelLogo from "../components/ChannelLogo";
import { useVisibleChannels } from "../visibleChannels";
import ModelLogo from "../components/ModelLogo";
import OperationResultsModal from "../components/OperationResultsModal";
import { cn } from "@/lib/utils";
import {
  clampCodexTestPercent,
  codexTestTokenMetrics,
  codexTestWindowKind,
  formatCodexTestMS,
  formatCodexTestReset,
  isFinalCodexTestDiagnostics,
} from "../lib/codexConnectionTest";
import type { CodexTestDiagnostics, CodexTestWindow } from "../lib/codexConnectionTest";
import GrokAccounts from "./GrokAccounts";
import AntigravityAccounts from "./AntigravityAccounts";
import ClaudeAccounts from "./ClaudeAccounts";
import { mergeAccountLiveState, useAccountLiveState } from "../hooks/useAccountLiveState";
import PageHeader from "../components/PageHeader";
import {
  HeaderActionMenu,
  type HeaderActionMenuItem,
  type HeaderActionMenuSection,
} from "../components/HeaderActionMenu";
import ColumnSettingsMenu from "../components/ColumnSettingsMenu";
import { CompactStat } from "../components/CompactStat";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import { useDataLoader, type LoadOptions } from "../hooks/useDataLoader";
import {
  useConfirmDialog,
  type ConfirmDialogOptions,
} from "../hooks/useConfirmDialog";
import {
  DEFAULT_PAGE_SIZE_OPTIONS,
  usePersistedPageSize,
} from "../hooks/usePersistedPageSize";
import { useToast } from "../hooks/useToast";
import type {
  AccountRow,
  AccountHealthBucket,
  AddAccountRequest,
  AddATAccountRequest,
  AddOpenAIResponsesAccountRequest,
  CodexClientMetadataMode,
  CodexFingerprintMode,
  UpdateOpenAIResponsesAccountRequest,
  APIKeyRow,
  AccountAnalysisResponse,
  AccountGroup,
  SystemSettings,
  RecycleBinAccountRow,
  AgentIdentityImportItem,
  AccountListSummary,
  AccountEmailDomainFacet,
  AccountOperationSelector,
  AccountPageStatsItem,
  AccountLiveStateResponse,
  UpstreamChannel,
  OpenAIResponsesBalanceResponse,
} from "../types";
import { getErrorMessage } from "../utils/error";
import { formatRelativeTime, formatBeijingTime } from "../utils/time";
import { buildBatchMetadataUpdate } from "../lib/accountBatchUpdate";
import {
  collectAccountOperationResult,
  snapshotAccountOperationResults,
  type AccountOperationResult,
  type AccountOperationResultsState,
} from "../lib/accountOperationResults";
import { operationProgressMessage } from "../lib/operationProgressMessage";
import {
  readOperationResultsVisibility,
  writeOperationResultsVisibility,
} from "../lib/operationResultsPreference";
import {
  isLargePoolSortDisabled,
  resolveDisabledAccountSorts,
} from "../lib/accountListSort";
import {
  formatLongUsageWindowLabel,
  getAccountStatusBadgeStatus,
  isOfficialCostHiddenAccount,
  isOfficialCostTooNew,
  needsOfficialCostReload,
  needsUsageReload,
  officialUsdValue,
  supportsOfficialUsage,
} from "../lib/usageFormat";
import {
  applyOptionalWorkspaceRouteHeader,
  applyWorkspaceRouteHeader,
} from "../lib/workspaceRoute";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Plus,
  RefreshCw,
  Trash2,
  Zap,
  FlaskConical,
  Ban,
  Timer,
  AlertTriangle,
  Upload,
  Download,
  ArrowDownToLine,
  Eye,
  EyeOff,
  KeyRound,
  ExternalLink,
  FileText,
  FileJson,
  BarChart3,
  Search,
  Fingerprint,
  FolderOpen,
  Layers,
  Cloud,
  Lock,
  Unlock,
  RotateCcw,
  Pencil,
  Check,
  CheckCircle,
  XCircle,
  Loader2,
  ChevronDown,
  Copy,
  Cookie,
  Coins,
  Power,
  PowerOff,
  Hourglass,
  X,
  SlidersHorizontal,
  LayoutGrid,
  Rows3,
  Recycle,
  Mail,
  ArchiveRestore,
  ArrowLeft,
  ToggleLeft,
  ToggleRight,
  MoreHorizontal,
  Sparkles,
  Wallet,
  Banknote,
  Gauge,
  Sliders,
  Flame,
  ShieldAlert,
  Globe,
  Tag,
  Users,
  ShieldCheck,
  Activity,
  Shield,
  ArrowUpRight,
  Settings2,
  ListChecks,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import AccountUsageModal from "../components/AccountUsageModal";
import AccountHealthBar from "../components/AccountHealthBar";
import AccountDetailSheet from "../components/AccountDetailSheet";
import RequestCountPills, {
  CountBreakdownTooltip,
} from "../components/RequestCountPills";
import { buildModelCountBreakdown } from "../lib/requestErrorStatus";
import {
  accountStateSurfaceClass,
  accountStateTableRowClass,
  renderAccountStateOverlay,
  resolveAccountOverlayKind,
} from "../components/AccountStateOverlay";
import CodexInviteView from "../components/CodexInviteView";
import InviteGuideModal from "../components/InviteGuideModal";
import Sub2APIImportModal from "../components/Sub2APIImportModal";
import AccountQuotaDistributionChart from "../components/AccountQuotaDistributionChart";
import AccountRateLimitRecoveryChart from "../components/AccountRateLimitRecoveryChart";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import AccountQuickConfigSheet from "../components/AccountQuickConfigSheet";
import { useImportGroupIds } from "../hooks/useImportGroupIds";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  accountMatchesGroupFilter,
  isAccountGroupFilterEmpty,
  pruneAccountGroupFilter,
  type AccountGroupFilterValue,
} from "../components/AccountGroupFilterSelect";
import ChipInput from "../components/ChipInput";

const OPERATION_PROGRESS_FLUSH_INTERVAL_MS = 200;

// 账号导入的体积约束。后端对导入端点放宽到 200MB;单个文件无法再切分,超过
// 上限直接拒绝。多文件按累计大小切成多批,每批控制在 150MB 内(留余量给
// multipart 边界与其它表单字段),避免单个请求体触发后端上限。
const IMPORT_MAX_FILE_BYTES = 200 * 1024 * 1024;
const IMPORT_BATCH_MAX_BYTES = 150 * 1024 * 1024;
const API_BALANCE_CACHE_TTL_MS = 60_000;
const API_BALANCE_ERROR_CACHE_TTL_MS = 15_000;

type APIBalanceLoadState = {
  loading: boolean;
  data?: OpenAIResponsesBalanceResponse;
  error?: string;
};

const apiBalanceCache = new Map<
  number,
  { expiresAt: number; state: APIBalanceLoadState }
>();
const apiBalanceInflight = new Map<number, Promise<APIBalanceLoadState>>();

function invalidateAPIAccountBalance(accountId: number) {
  apiBalanceCache.delete(accountId);
}

function loadAPIAccountBalance(
  accountId: number,
  force = false,
): Promise<APIBalanceLoadState> {
  if (force) invalidateAPIAccountBalance(accountId);
  const cached = apiBalanceCache.get(accountId);
  if (cached && cached.expiresAt > Date.now()) {
    return Promise.resolve(cached.state);
  }
  if (!force) {
    const inflight = apiBalanceInflight.get(accountId);
    if (inflight) return inflight;
  }

  let promise: Promise<APIBalanceLoadState>;
  promise = api
    .getOpenAIResponsesBalance(accountId, undefined, force)
    .then<APIBalanceLoadState>((data) => ({ loading: false, data }))
    .catch<APIBalanceLoadState>((error) => ({
      loading: false,
      error: getErrorMessage(error),
    }))
    .then((state) => {
      if (apiBalanceInflight.get(accountId) === promise) {
        apiBalanceCache.set(accountId, {
          expiresAt:
            Date.now() +
            (state.data ? API_BALANCE_CACHE_TTL_MS : API_BALANCE_ERROR_CACHE_TTL_MS),
          state,
        });
      }
      return state;
    })
    .finally(() => {
      if (apiBalanceInflight.get(accountId) === promise) {
        apiBalanceInflight.delete(accountId);
      }
    });
  apiBalanceInflight.set(accountId, promise);
  return promise;
}

const formatMB = (bytes: number): string =>
  `${Math.round(bytes / (1024 * 1024))}MB`;

function AccountConcurrencyBadge({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const active = Math.max(0, account.active_requests ?? 0);
  const occupied = Math.max(active, account.occupied_requests ?? active);
  if (occupied === 0) return null;

  const buffered = occupied - active;
  const showOccupied = account.session_slot_buffer_enabled === true;
  const title = showOccupied
    ? t("accounts.occupiedRequestsTooltip", { active, occupied, buffered })
    : t("accounts.activeRequestsTooltip", { count: active });

  return (
    <span
      className="inline-flex items-center gap-1 rounded-md bg-blue-50 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-blue-600 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20"
      title={title}
    >
      <span
        className="size-1.5 animate-pulse rounded-full bg-blue-500 dark:bg-blue-400"
        aria-hidden
      />
      {showOccupied ? `${active}/${occupied}` : active}
    </span>
  );
}

// splitFilesIntoBatches 按累计字节大小把文件分批。单个文件即便超过 batchMax
// 也自成一批(交由后端上限兜底),保证每个文件都被投递。
function splitFilesIntoBatches(files: File[], batchMax: number): File[][] {
  const batches: File[][] = [];
  let current: File[] = [];
  let currentSize = 0;
  for (const file of files) {
    if (current.length > 0 && currentSize + file.size > batchMax) {
      batches.push(current);
      current = [];
      currentSize = 0;
    }
    current.push(file);
    currentSize += file.size;
  }
  if (current.length > 0) batches.push(current);
  return batches.length > 0 ? batches : [[]];
}

const ACCOUNT_ANALYSIS_VISIBILITY_KEY = "codex2api:accounts:analysis-visible";
const ACCOUNT_EMAIL_DOMAIN_VISIBILITY_KEY =
  "codex2api:accounts:email-domain-tags-visible";
const ACCOUNT_VISIBLE_COLUMNS_KEY = "codex2api:accounts:visible-columns";
const ACCOUNT_TABLE_COLUMNS = [
  "sequence",
  "email",
  "tags",
  "groups",
  "proxy",
  "priority",
  "plan",
  "status",
  "today",
  "requests",
  "usage",
  "billed",
  "importTime",
  "updatedAt",
  "actions",
] as const;
const ACCOUNT_GROUP_COLORS = [
  "#2563eb",
  "#16a34a",
  "#d97706",
  "#dc2626",
  "#7c3aed",
  "#0891b2",
  "#64748b",
] as const;
const CUSTOM_HEADERS_PLACEHOLDER = `{
  "Authorization": "Bearer upstream-token",
  "X-Custom-Header": "value"
}`;
const MODEL_MAPPING_PLACEHOLDER = `{
  "client-model": "upstream-model",
  "legacy-*": "gpt-4.1"
}`;
type AccountTableColumn = (typeof ACCOUNT_TABLE_COLUMNS)[number];
type CustomHeadersParseResult =
  | { ok: true; value: Record<string, string> | null }
  | { ok: false };
type ModelMappingParseResult =
  | { ok: true; value: string }
  | { ok: false };
type ModelMappingEntriesParseResult =
  | { ok: true; entries: ModelMappingEntry[] }
  | { ok: false };
type ModelMappingMode = "form" | "json";
type ModelMappingEntry = {
  from: string;
  to: string;
};
type AccountGroupDraft = {
  id: number | null;
  name: string;
  description: string;
  color: string;
  baseConcurrencyInput: string;
  auto_pause_5h_threshold: number;
  auto_pause_7d_threshold: number;
  proxyURLsInput: string;
  channel: UpstreamChannel;
};

function getDefaultAccountVisibleColumns(): Record<
  AccountTableColumn,
  boolean
> {
  return Object.fromEntries(
    ACCOUNT_TABLE_COLUMNS.map((column) => [column, column !== "tags"]),
  ) as Record<AccountTableColumn, boolean>;
}

function getInitialAccountVisibleColumns(): Record<
  AccountTableColumn,
  boolean
> {
  const fallback = getDefaultAccountVisibleColumns();
  try {
    const raw = window.localStorage.getItem(ACCOUNT_VISIBLE_COLUMNS_KEY);
    if (!raw) return fallback;
    const parsed = JSON.parse(raw) as Partial<
      Record<AccountTableColumn, boolean>
    >;
    return Object.fromEntries(
      ACCOUNT_TABLE_COLUMNS.map((column) => [
        column,
        column === "tags" ? parsed[column] === true : parsed[column] !== false,
      ]),
    ) as Record<AccountTableColumn, boolean>;
  } catch {
    return fallback;
  }
}

function persistAccountVisibleColumns(
  columns: Record<AccountTableColumn, boolean>,
) {
  try {
    window.localStorage.setItem(
      ACCOUNT_VISIBLE_COLUMNS_KEY,
      JSON.stringify(columns),
    );
  } catch {
    // Keep the in-memory preference working when localStorage is unavailable.
  }
}

const ACCOUNT_VIEW_MODE_KEY = "codex2api:accounts:view-mode";
type AccountViewMode = "table" | "grid";
type EmailDomainStat = {
  domain: string;
  total: number;
  banned: number;
};

function getInitialAccountViewMode(): AccountViewMode {
  try {
    const raw = window.localStorage.getItem(ACCOUNT_VIEW_MODE_KEY);
    if (raw === "grid" || raw === "table") return raw;
  } catch {
    // ignore
  }
  return "table";
}

function persistAccountViewMode(mode: AccountViewMode) {
  try {
    window.localStorage.setItem(ACCOUNT_VIEW_MODE_KEY, mode);
  } catch {
    // ignore
  }
}

// 账号管理页面级模式：号池模式（pool，默认，完整管理布局）/ 自用模式
// （personal，主体列表改为每行 2 列卡片）。
const ACCOUNT_PAGE_MODE_KEY = "codex2api:accounts:page-mode";
type AccountPageMode = "pool" | "personal";

// 自用模式自动判定阈值：用户从未手动设置过时，号池账号数 < 该值则默认自用模式。
const ACCOUNT_PERSONAL_MODE_AUTO_THRESHOLD = 10;

// 返回用户保存过的页面模式；从未设置过返回 null（用于触发按账号数自动判定）。
function getStoredAccountPageMode(): AccountPageMode | null {
  try {
    const raw = window.localStorage.getItem(ACCOUNT_PAGE_MODE_KEY);
    if (raw === "pool" || raw === "personal") return raw;
  } catch {
    // ignore
  }
  return null;
}

function persistAccountPageMode(mode: AccountPageMode) {
  try {
    window.localStorage.setItem(ACCOUNT_PAGE_MODE_KEY, mode);
  } catch {
    // ignore
  }
}

function getAccountEmailDomain(account: AccountRow): string {
  return (account.email_domain || "").trim().toLowerCase();
}

function emailDomainTag(domain: string): string {
  return domain ? `@${domain}` : "";
}

function formatAccountListEmail(account: AccountRow): string {
  return account.email?.trim() || account.name || `ID ${account.id}`;
}

function formatAccessTokenBadge(account: AccountRow): string {
  return account.access_token_type === "codex_at" ? "codex_at" : "AT";
}

// getCreditBalanceDisplay 返回 credits 积分余额徽标应显示的文本；无余额（或未探测）返回 null。
function getCreditBalanceDisplay(account: AccountRow): string | null {
  if (!account.credits_has_credits) return null;
  if (account.credits_unlimited) return "∞";
  const balance = (account.credits_balance ?? "").trim();
  return balance ? balance : null;
}

function getInitialAnalysisVisibility(): boolean {
  try {
    return (
      window.localStorage.getItem(ACCOUNT_ANALYSIS_VISIBILITY_KEY) !== "false"
    );
  } catch {
    return true;
  }
}

function persistAnalysisVisibility(visible: boolean) {
  try {
    window.localStorage.setItem(
      ACCOUNT_ANALYSIS_VISIBILITY_KEY,
      visible ? "true" : "false",
    );
  } catch {
    // Local storage can be unavailable in restricted browser modes; keep the in-memory toggle working.
  }
}

function getInitialEmailDomainVisibility(): boolean {
  try {
    return (
      window.localStorage.getItem(ACCOUNT_EMAIL_DOMAIN_VISIBILITY_KEY) !== "false"
    );
  } catch {
    return true;
  }
}

function persistEmailDomainVisibility(visible: boolean) {
  try {
    window.localStorage.setItem(
      ACCOUNT_EMAIL_DOMAIN_VISIBILITY_KEY,
      visible ? "true" : "false",
    );
  } catch {
    // Keep the in-memory toggle working when localStorage is unavailable.
  }
}

function parseModelTokens(value: string): string[] {
  const seen = new Set<string>();
  return value
    .split(/[\n,\t ]+/)
    .map((item) => item.trim())
    .filter((item) => {
      if (!item) return false;
      const key = item.toLowerCase();
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
}

/** Codex 官方 OAuth/AT 账号（非 OpenAI Responses 中转、非 Grok），即走 Codex 出站路径的账号。 */
function isCodexOfficialAccount(account: AccountRow): boolean {
  return !account.openai_responses_api && !account.grok_api;
}

function codexFingerprintModeOptions(
  t: ReturnType<typeof useTranslation>["t"],
): { value: CodexFingerprintMode; label: string }[] {
  return [
    { value: "off", label: t("accounts.codexFingerprintModeOff") },
    { value: "device", label: t("accounts.codexFingerprintModeDevice") },
    { value: "session", label: t("accounts.codexFingerprintModeSession") },
    { value: "full", label: t("accounts.codexFingerprintModeFull") },
  ];
}

function codexFingerprintModeDetail(
  t: ReturnType<typeof useTranslation>["t"],
  mode: CodexFingerprintMode,
): string {
  switch (mode) {
    case "device":
      return t("accounts.codexFingerprintModeDeviceDetail");
    case "session":
      return t("accounts.codexFingerprintModeSessionDetail");
    case "full":
      return t("accounts.codexFingerprintModeFullDetail");
    default:
      return t("accounts.codexFingerprintModeOffDetail");
  }
}

function formatCustomHeadersText(
  headers?: Record<string, string> | null,
): string {
  if (!headers || Object.keys(headers).length === 0) return "";
  return JSON.stringify(headers, null, 2);
}

function parseCustomHeadersText(value: string): CustomHeadersParseResult {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, value: null };

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return { ok: false };
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false };
  }

  const entries = Object.entries(parsed as Record<string, unknown>);
  if (entries.some(([, headerValue]) => typeof headerValue !== "string")) {
    return { ok: false };
  }

  return {
    ok: true,
    value: Object.fromEntries(entries) as Record<string, string>,
  };
}

function emptyModelMappingEntries(): ModelMappingEntry[] {
  return [{ from: "", to: "" }];
}

function parseModelMappingEntries(value: string): ModelMappingEntriesParseResult {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, entries: emptyModelMappingEntries() };

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return { ok: false };
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false };
  }

  const entries = Object.entries(parsed as Record<string, unknown>);
  if (
    entries.some(
      ([from, to]) => !from.trim() || typeof to !== "string" || !to.trim(),
    )
  ) {
    return { ok: false };
  }

  return {
    ok: true,
    entries:
      entries.length > 0
        ? entries.map(([from, to]) => ({
            from,
            to: String(to),
          }))
        : emptyModelMappingEntries(),
  };
}

function parseModelMappingText(value: string): ModelMappingParseResult {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, value: "" };
  if (!parseModelMappingEntries(trimmed).ok) return { ok: false };
  return { ok: true, value: trimmed };
}

function exactModelMappingAliases(
  value?: string,
  supportedModels: string[] = [],
): string[] {
  const parsed = parseModelMappingEntries(value ?? "");
  if (!parsed.ok) return [];
  const supported = new Set(
    supportedModels.map((model) => model.trim().toLowerCase()).filter(Boolean),
  );
  return parsed.entries
    .filter((entry) => {
      const alias = entry.from.trim();
      const target = entry.to.trim().toLowerCase();
      return (
        alias &&
        !alias.includes("*") &&
        isConnectionTestModel(alias) &&
        (supported.size === 0 || supported.has(target))
      );
    })
    .map((entry) => entry.from.trim());
}

function serializeModelMappingEntries(
  entries: ModelMappingEntry[],
): ModelMappingParseResult {
  const out: Record<string, string> = {};
  const seen = new Set<string>();
  for (const entry of entries) {
    const from = entry.from.trim();
    const to = entry.to.trim();
    if (!from && !to) continue;
    if (!from || !to) return { ok: false };
    const key = from.toLowerCase();
    if (seen.has(key)) return { ok: false };
    seen.add(key);
    out[from] = to;
  }
  if (Object.keys(out).length === 0) {
    return { ok: true, value: "" };
  }
  return { ok: true, value: JSON.stringify(out, null, 2) };
}

function resolveModelMappingValue(
  mode: ModelMappingMode,
  text: string,
  entries: ModelMappingEntry[],
): ModelMappingParseResult {
  return mode === "json"
    ? parseModelMappingText(text)
    : serializeModelMappingEntries(entries);
}

function mergeModelLists(current: string[], incoming: string[]): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const item of [...current, ...incoming]) {
    const value = item.trim();
    if (!value) continue;
    const key = value.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    result.push(value);
  }
  return result;
}

function formatAccountName(account: AccountRow): string {
  if (account.openai_responses_api || account.grok_api) {
    return account.name?.trim() || `ID ${account.id}`;
  }
  return account.email || account.name || `ID ${account.id}`;
}

function isOAuthAccount(account: AccountRow | null): boolean {
  return account?.account_type === "oauth";
}

function parseOAuthCallbackParams(rawUrl: string): { code: string; state: string } {
  const raw = rawUrl.trim();
  try {
    const url = new URL(raw);
    return {
      code: url.searchParams.get("code") ?? "",
      state: url.searchParams.get("state") ?? "",
    };
  } catch {
    const qs = raw.includes("?") ? raw.split("?")[1] : raw;
    const params = new URLSearchParams(qs);
    return {
      code: params.get("code") ?? "",
      state: params.get("state") ?? "",
    };
  }
}

function formatQuotaAutoPausePercentInput(value?: number | null): string {
  if (typeof value !== "number" || value <= 0) return "";
  const percent = value * 100;
  if (Number.isInteger(percent)) return String(percent);
  return String(Number(percent.toFixed(2)));
}

function isPercentThresholdInputInvalid(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed) return false;
  const parsed = Number(trimmed);
  return !Number.isFinite(parsed) || parsed < 0 || parsed > 100;
}

function percentThresholdInputToRatio(value: string): number | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const parsed = Number(trimmed);
  if (!Number.isFinite(parsed) || parsed <= 0) return null;
  return parsed / 100;
}

function formatDispatchCountLimitInput(value?: number | null): string {
  if (typeof value !== "number" || value <= 0) return "";
  return String(Math.trunc(value));
}

function isDispatchCountLimitInputInvalid(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed) return false;
  if (!/^\d+$/.test(trimmed)) return true;
  const parsed = Number.parseInt(trimmed, 10);
  return parsed < 0 || parsed > 1000000;
}

function dispatchCountLimitInputToValue(value: string): number | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const parsed = Number.parseInt(trimmed, 10);
  if (!Number.isFinite(parsed) || parsed <= 0) return null;
  return parsed;
}

function formatSchedulerPriorityInput(value?: number | null): string {
  if (typeof value !== "number" || value === 0) return "";
  return String(Math.trunc(value));
}

function isSchedulerPriorityInputInvalid(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed) return false;
  if (!/^-?\d+$/.test(trimmed)) return true;
  const parsed = Number.parseInt(trimmed, 10);
  return parsed < -100 || parsed > 100;
}

function schedulerPriorityInputToValue(value: string): number | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const parsed = Number.parseInt(trimmed, 10);
  if (!Number.isFinite(parsed) || parsed === 0) return null;
  return parsed;
}

function getMediaQueryMatch(query: string): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") {
    return false;
  }
  return window.matchMedia(query).matches;
}

function useMediaQuery(query: string) {
  const [matches, setMatches] = useState(() => getMediaQueryMatch(query));

  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") {
      return;
    }
    const media = window.matchMedia(query);
    const update = () => setMatches(media.matches);
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, [query]);

  return matches;
}

type BatchOperationAction = "batch_test" | "batch_delete" | "batch_refresh" | "clean";

interface BatchOperationEvent {
  type: "start" | "progress" | "complete";
  action: BatchOperationAction;
  status?: string;
  http_status?: number;
  current?: number;
  total?: number;
  success?: number;
  failed?: number;
  banned?: number;
  rate_limited?: number;
  deleted?: number;
  account_id?: number;
  account_name?: string;
  account_email?: string;
  message?: string;
  error?: string;
}

interface OperationProgressState {
  show: boolean;
  action: BatchOperationAction;
  title: string;
  current: number;
  total: number;
  success: number;
  failed: number;
  banned: number;
  rateLimited: number;
  deleted: number;
  done: boolean;
  message?: string;
}

async function readOperationSSE(
  res: Response,
  onEvent: (event: BatchOperationEvent) => void,
) {
  const reader = res.body?.getReader();
  if (!reader) return;

  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split("\n");
    buffer = lines.pop() ?? "";
    for (const line of lines) {
      if (!line.startsWith("data: ")) continue;
      try {
        onEvent(JSON.parse(line.slice(6)) as BatchOperationEvent);
      } catch {
        /* 忽略格式异常的进度帧 */
      }
    }
  }
}

async function readAdminStreamError(res: Response): Promise<string> {
  const body = await res.text();
  if (!body.trim()) return `HTTP ${res.status}`;
  try {
    const parsed = JSON.parse(body) as { error?: string };
    if (parsed.error?.trim()) return parsed.error;
  } catch {
    /* ignore */
  }
  return body;
}

async function postAdminSSE(path: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = {};
  const adminKey = getAdminKey();
  if (adminKey) headers["X-Admin-Key"] = adminKey;
  if (body !== undefined) headers["Content-Type"] = "application/json";

  const res = await fetch(`/api/admin${path}`, {
    method: "POST",
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    cache: "no-store",
  });
  if (!res.ok) {
    if (res.status === 401) resetAdminAuthState();
    throw new Error(await readAdminStreamError(res));
  }
  return res;
}

// 搜索框草稿态隔离在本组件内:宿主 Accounts 有上百个 state,若每次击键都写
// 宿主 state,一个字符就是一次 1.3 万行组件的整树重渲染。这里只在去抖后上抛。
const AccountSearchInput = memo(function AccountSearchInput({
  value,
  onCommit,
  placeholder,
  className,
}: {
  value: string;
  onCommit: (next: string) => void;
  placeholder?: string;
  className?: string;
}) {
  const [draft, setDraft] = useState(value);

  // 外部重置(如"清除筛选")时同步草稿,让输入框跟着清空。
  useEffect(() => {
    setDraft(value);
  }, [value]);

  useEffect(() => {
    if (draft === value) return;
    const timer = window.setTimeout(() => onCommit(draft), 250);
    return () => window.clearTimeout(timer);
  }, [draft, value, onCommit]);

  return (
    <Input
      className={className}
      placeholder={placeholder}
      value={draft}
      onChange={(e: ChangeEvent<HTMLInputElement>) => setDraft(e.target.value)}
    />
  );
});

// 行操作统一走稳定引用的 actions 对象:memo 行组件的 props 里不能出现宿主每轮
// 渲染新建的闭包,否则 memo 形同虚设。实现由宿主用 ref 每轮刷新,转发层只建一次。
interface AccountRowActions {
  toggleSelect: (id: number) => void;
  openDetail: (account: AccountRow) => void;
  openSchedulerEditor: (account: AccountRow) => void;
  openQuickConfig: (account: AccountRow) => void;
  openQuickGroupEditor: (account: AccountRow) => void;
  openQuickProxyEditor: (account: AccountRow) => void;
  openUsage: (account: AccountRow) => void;
  // 直接打开用量弹窗的官方统计 tab（成本列的官方胶囊）。
  openOfficialUsage: (account: AccountRow) => void;
  openTesting: (account: AccountRow) => void;
  refresh: (account: AccountRow) => void;
  generateAuthJson: (account: AccountRow) => void;
  toggleEnabled: (account: AccountRow) => void;
  toggleLock: (account: AccountRow) => void;
  resetStatus: (account: AccountRow) => void | Promise<void>;
  resetCredits: (account: AccountRow) => void;
  openModelsEditor: (account: AccountRow) => void;
  remove: (account: AccountRow) => void;
  usageRefreshed: () => void;
}

function formatCountdownRemaining(untilMs: number, nowMs: number): string {
  const diff = Math.max(0, untilMs - nowMs);
  if (diff <= 0) return "";
  const hours = Math.floor(diff / 3600000);
  const minutes = Math.floor((diff % 3600000) / 60000);
  const seconds = Math.floor((diff % 60000) / 1000);
  if (hours > 0) {
    return `${hours}h ${String(minutes).padStart(2, "0")}m ${String(seconds).padStart(2, "0")}s`;
  }
  if (minutes > 0) {
    return `${minutes}m ${String(seconds).padStart(2, "0")}s`;
  }
  return `${seconds}s`;
}

function useCountdownRemaining(until?: string): string {
  const [remaining, setRemaining] = useState("");

  useEffect(() => {
    if (!until) {
      setRemaining("");
      return;
    }
    const target = new Date(until).getTime();
    const update = () => {
      setRemaining(formatCountdownRemaining(target, Date.now()));
    };
    update();
    const id = setInterval(update, 1000);
    return () => clearInterval(id);
  }, [until]);

  return remaining;
}

// memo 边界:宿主 Accounts 组件有上百个 state,任何弹窗/表单/进度条状态变化都会
// 整树重渲染;行数据没变时这里直接跳过,交互卡顿的大头就在这。
const AccountTableRow = memo(function AccountTableRow({
  account,
  sequence,
  selected,
  detailOpen,
  visibleColumns,
  showEmailDomainTags,
  allGroups,
  proxyCtx,
  healthBuckets,
  lazyMode,
  refreshing,
  authJsonExporting,
  t,
  actions,
}: {
  account: AccountRow;
  sequence: number;
  selected: boolean;
  detailOpen: boolean;
  visibleColumns: Record<AccountTableColumn, boolean>;
  showEmailDomainTags: boolean;
  allGroups: AccountGroup[];
  proxyCtx: ProxyBindingContext;
  healthBuckets: AccountHealthBucket[] | undefined;
  lazyMode: boolean;
  refreshing: boolean;
  authJsonExporting: boolean;
  t: ReturnType<typeof useTranslation>["t"];
  actions: AccountRowActions;
}) {
  const tableOverlayKind = resolveAccountOverlayKind(account);
  const tableOverlay = renderAccountStateOverlay(account, t, {
    compact: true,
    markerOnly: true,
    onRecover: () => actions.resetStatus(account),
  });

  return (
                          <TableRow
                            key={account.id}
                            data-state={selected ? "selected" : undefined}
                            className={`cursor-pointer ${
                              detailOpen
                                ? "bg-primary/8"
                                : selected
                                  ? "bg-primary/5"
                                  : ""
                            }${accountStateTableRowClass(account)}`}
                            onClick={(event) => {
                              const target = event.target as HTMLElement | null;
                              if (
                                target?.closest(
                                  'button, a, input, label, [role="menuitem"], [role="menu"], [data-slot="button"], [data-slot="select-trigger"]',
                                )
                              ) {
                                return;
                              }
                              actions.openDetail(account);
                            }}
                          >
                            <TableCell>
                              <div className="flex items-center gap-1">
                                <input
                                  type="checkbox"
                                  className="size-4 cursor-pointer accent-primary"
                                  checked={selected}
                                  onChange={() => actions.toggleSelect(account.id)}
                                  onClick={(event) => event.stopPropagation()}
                                />
                                {!visibleColumns.status && tableOverlayKind ? (
                                  <span className="sr-only">
                                    {tableOverlayKind === "disabled"
                                      ? t("accounts.disabledOverlay")
                                      : t("accounts.overloadOverlay")}
                                  </span>
                                ) : null}
                                {!visibleColumns.status &&
                                tableOverlayKind === "overload" ? (
                                  <button
                                    type="button"
                                    className="inline-flex size-7 items-center justify-center rounded-md text-orange-700 transition-colors hover:bg-orange-500/10 dark:text-orange-300"
                                    title={t("accounts.overloadRecover")}
                                    aria-label={t("accounts.overloadRecover")}
                                    onClick={(event) => {
                                      event.preventDefault();
                                      event.stopPropagation();
                                      void actions.resetStatus(account);
                                    }}
                                  >
                                    <RotateCcw className="size-3.5" />
                                  </button>
                                ) : null}
                              </div>
                            </TableCell>
                            {visibleColumns.sequence && (
                              <TableCell
                                className="text-[14px] font-mono text-muted-foreground"
                                title={`ID ${account.id}`}
                              >
                                {sequence}
                              </TableCell>
                            )}
                            {visibleColumns.email && (
                              <TableCell className="min-w-[220px] whitespace-normal text-[14px] text-muted-foreground">
                                <div className="flex min-w-0 items-center gap-2.5">
                                <span className="flex size-8 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-card ring-1 ring-border shadow-sm">
                                  {account.openai_responses_api ? (
                                    <ModelLogo model="openai" variant="plain" size={17} />
                                  ) : (
                                    <ChannelLogo channel="codex" size={32} className="rounded-lg" />
                                  )}
                                </span>
                                <div className="flex min-w-0 flex-col items-start gap-1">
                                  <button
                                    type="button"
                                    className="break-all text-left font-medium text-foreground transition-colors hover:text-primary"
                                    title={t("accounts.openDetail")}
                                    onClick={(event) => {
                                      event.stopPropagation();
                                      actions.openDetail(account);
                                    }}
                                  >
                                    {account.openai_responses_api || account.grok_api
                                      ? formatAccountName(account)
                                      : formatAccountListEmail(account)}
                                  </button>
                                  {account.effective_workspace_id && (
                                    <span
                                      className={cn(
                                        "max-w-full truncate font-mono text-[10px] leading-tight",
                                        account.workspace_id_override
                                          ? "rounded bg-sky-500/10 px-1.5 py-0.5 font-semibold text-sky-700 dark:text-sky-300"
                                          : "text-muted-foreground/70",
                                      )}
                                      title={
                                        account.workspace_id_override
                                          ? `${t("accounts.workspaceRouteBadge")}: ${account.effective_workspace_id}`
                                          : account.effective_workspace_id
                                      }
                                    >
                                      {account.workspace_id_override
                                        ? `${t("accounts.workspaceRouteBadge")} · `
                                        : ""}
                                      {account.effective_workspace_id}
                                    </span>
                                  )}
                                  {showEmailDomainTags &&
                                    getAccountEmailDomain(account) && (
                                    <EmailDomainBadge
                                      domain={getAccountEmailDomain(account)}
                                      t={t}
                                    />
                                  )}
                                  {(account.at_only ||
                                    account.openai_responses_api ||
                                    account.grok_api ||
                                    account.agent_identity ||
                                    account.locked ||
                                    (account.rate_limit_reset_credits ?? 0) >
                                      0 ||
                                    getCreditBalanceDisplay(account) !==
                                      null) && (
                                    <div className="flex flex-wrap gap-1">
                                      {account.at_only && (
                                        <span className="inline-flex items-center rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 ring-1 ring-inset ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20">
                                          {formatAccessTokenBadge(account)}
                                        </span>
                                      )}
                                      {account.agent_identity && (
                                        <span className="inline-flex items-center gap-0.5 rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                                          <Fingerprint className="size-2.5" />
                                          Agent Identity
                                        </span>
                                      )}
                                      {account.openai_responses_api && (
                                        <span className="inline-flex items-center rounded-md bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 ring-1 ring-inset ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20">
                                          Responses API
                                        </span>
                                      )}
                                      {account.grok_api && (
                                        <span className="inline-flex items-center gap-0.5 rounded-md bg-zinc-900 px-1.5 py-0.5 text-[10px] font-medium text-white ring-1 ring-inset ring-zinc-700 dark:bg-white dark:text-zinc-900 dark:ring-zinc-300">
                                          <Sparkles className="size-2.5" />
                                          Grok
                                        </span>
                                      )}
                                      {account.locked && (
                                        <span className="inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                                          <Lock className="mr-0.5 size-2.5" />
                                          {t("accounts.lock")}
                                        </span>
                                      )}
                                      {(account.rate_limit_reset_credits ??
                                        0) > 0 && (
                                        <button
                                          type="button"
                                          onClick={(e) => {
                                            e.stopPropagation();
                                            actions.openUsage(account);
                                          }}
                                          className="inline-flex items-center rounded-md bg-violet-50 px-1.5 py-0.5 text-[10px] font-medium text-violet-700 ring-1 ring-inset ring-violet-600/20 transition-colors hover:bg-violet-100 dark:bg-violet-950 dark:text-violet-400 dark:ring-violet-400/20 dark:hover:bg-violet-900"
                                          title={t("accounts.resetCreditsBadge", {
                                            count:
                                              account.rate_limit_reset_credits ??
                                              0,
                                          })}
                                        >
                                          <RotateCcw className="mr-0.5 size-2.5" />
                                          {account.rate_limit_reset_credits ?? 0}
                                        </button>
                                      )}
                                      {getCreditBalanceDisplay(account) !==
                                        null && (
                                        <button
                                          type="button"
                                          onClick={(e) => {
                                            e.stopPropagation();
                                            actions.openUsage(account);
                                          }}
                                          className="inline-flex items-center rounded-md bg-teal-50 px-1.5 py-0.5 text-[10px] font-medium text-teal-700 ring-1 ring-inset ring-teal-600/20 transition-colors hover:bg-teal-100 dark:bg-teal-950 dark:text-teal-400 dark:ring-teal-400/20 dark:hover:bg-teal-900"
                                          title={
                                            account.credits_unlimited
                                              ? t(
                                                  "accounts.creditsBalanceUnlimited",
                                                )
                                              : t(
                                                  "accounts.creditsBalanceBadge",
                                                  {
                                                    balance:
                                                      getCreditBalanceDisplay(
                                                        account,
                                                      ),
                                                  },
                                                )
                                          }
                                        >
                                          <Coins className="mr-0.5 size-2.5" />
                                          {getCreditBalanceDisplay(account)}
                                        </button>
                                      )}
                                    </div>
                                  )}
                                </div>
                                </div>
                              </TableCell>
                            )}
                            {visibleColumns.tags && (
                              <TableCell className="min-w-[120px]">
                                <ChipList
                                  items={account.tags ?? []}
                                  tone="purple"
                                />
                                {showEmailDomainTags &&
                                  getAccountEmailDomain(account) && (
                                  <div className="mt-1.5 flex flex-wrap gap-1">
                                    <EmailDomainBadge
                                      domain={getAccountEmailDomain(account)}
                                      t={t}
                                    />
                                  </div>
                                )}
                              </TableCell>
                            )}
                            {visibleColumns.groups && (
                              <TableCell className="min-w-[140px]">
                                <GroupChipList
                                  groups={resolveAccountGroups(
                                    account.group_ids ?? [],
                                    allGroups,
                                  )}
                                  onClick={() => actions.openQuickGroupEditor(account)}
                                  emptyLabel={t("accounts.groupQuickEdit")}
                                />
                              </TableCell>
                            )}
                            {visibleColumns.proxy && (
                              <TableCell className="min-w-[120px] max-w-[180px]">
                                <AccountProxyBadge
                                  account={account}
                                  ctx={proxyCtx}
                                  onClick={() =>
                                    actions.openQuickProxyEditor(account)
                                  }
                                />
                              </TableCell>
                            )}
                            {visibleColumns.priority && (
                              <TableCell>
                                <SchedulerPriorityBadge account={account} />
                              </TableCell>
                            )}
                            {visibleColumns.plan && (
                              <TableCell>
                                <div className="flex flex-wrap items-center gap-1.5">
                                  <PlanBadge
                                    planType={account.plan_type}
                                    workspaceId={accountWorkspaceId(account)}
                                  />
                                  <ExpiryBadge
                                    expiresAt={account.subscription_expires_at}
                                    planType={account.plan_type}
                                  />
                                </div>
                              </TableCell>
                            )}
                            {visibleColumns.status && (
                              <TableCell data-account-state-cell="status">
                                {tableOverlay ?? (
                                  <div
                                    className="min-w-[168px] max-w-[240px] space-y-1.5"
                                    title={[
                                      t("accounts.healthSummary", {
                                        health: formatHealthTier(
                                          account.health_tier,
                                          t,
                                        ),
                                        score: Math.round(
                                          getDispatchScore(account),
                                        ),
                                        concurrency:
                                          account.dynamic_concurrency_limit ?? "-",
                                      }),
                                      account.status === "error" &&
                                      account.error_message
                                        ? account.error_message
                                        : "",
                                      (account.model_cooldowns?.length ?? 0) > 0
                                        ? `model ${account.model_cooldowns?.[0]?.model}${(account.model_cooldowns?.length ?? 0) > 1 ? ` +${(account.model_cooldowns?.length ?? 1) - 1}` : ""}`
                                        : "",
                                    ]
                                      .filter(Boolean)
                                      .join("\n")}
                                  >
                                    <div className="flex min-h-6 flex-wrap items-center gap-1.5">
                                      <StatusBadge
                                        status={getAccountStatusBadgeStatus(account)}
                                        detail={
                                          account.status === "overload_paused"
                                            ? undefined
                                            : getAccountRateLimitWindow(account) ??
                                              undefined
                                        }
                                        errorMessage={account.error_message}
                                      />
                                      <UsingCreditsBadge account={account} />
                                      {account.status !== "overload_paused" && (
                                        <AccountStatusCountdown account={account} />
                                      )}
                                      <AccountConcurrencyBadge account={account} />
                                    </div>
                                    <AccountHealthBar
                                      buckets={healthBuckets}
                                    />
                                  </div>
                                )}
                              </TableCell>
                            )}
                            {visibleColumns.today && (
                              <TableCell>
                                <TodayStatsCell account={account} />
                              </TableCell>
                            )}
                            {visibleColumns.requests && (
                              <TableCell>
                                <RequestCountPills account={account} compact />
                              </TableCell>
                            )}
                            {visibleColumns.usage && (
                              <TableCell>
                                <UsageCell
                                  account={account}
                                  onRefreshed={() => actions.usageRefreshed()}
                                />
                              </TableCell>
                            )}
                            {visibleColumns.billed && (
                              <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">
                                <BilledCell account={account} onOpenOfficial={actions.openOfficialUsage} />
                              </TableCell>
                            )}
                            {visibleColumns.importTime && (
                              <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">
                                {formatBeijingTime(account.created_at)}
                              </TableCell>
                            )}
                            {visibleColumns.updatedAt && (
                              <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">
                                {lazyMode ? (
                                  <div className="space-y-0.5 leading-tight">
                                    <div title={t("accounts.recordUpdatedAt")}>
                                      <span className="mr-1 text-[11px] text-muted-foreground/70">
                                        {t("accounts.recordUpdatedAtShort")}
                                      </span>
                                      {formatRelativeTime(account.updated_at)}
                                    </div>
                                    <div title={t("accounts.usageUpdatedAt")}>
                                      <span className="mr-1 text-[11px] text-muted-foreground/70">
                                        {t("accounts.usageUpdatedAtShort")}
                                      </span>
                                      {account.codex_usage_updated_at
                                        ? formatRelativeTime(
                                            account.codex_usage_updated_at,
                                          )
                                        : t("accounts.noUsageUpdatedAt")}
                                    </div>
                                  </div>
                                ) : (
                                  formatRelativeTime(account.updated_at)
                                )}
                              </TableCell>
                            )}
                            {visibleColumns.actions && (
                              <TableCell className="text-right">
                                <div className="flex items-center justify-end gap-0.5">
                                  <Button
                                    variant="ghost"
                                    size="icon-sm"
                                    className="size-8 text-muted-foreground hover:text-primary"
                                    onClick={() => actions.openQuickConfig(account)}
                                    title="账号指纹与快捷配置"
                                  >
                                    <Fingerprint className="size-3.5 text-primary" />
                                  </Button>
                                  <Button
                                    variant="ghost"
                                    size="icon-sm"
                                    className="size-8"
                                    onClick={() => actions.openSchedulerEditor(account)}
                                    title={t("accounts.editScheduler")}
                                  >
                                    <Pencil className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="ghost"
                                    size="icon-sm"
                                    className="size-8"
                                    onClick={() => actions.openUsage(account)}
                                    title={t("accounts.usageDetail")}
                                  >
                                    <BarChart3 className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="ghost"
                                    size="icon-sm"
                                    className="size-8"
                                    onClick={() => actions.openTesting(account)}
                                    title={t("accounts.testConnection")}
                                  >
                                    <Zap className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="ghost"
                                    size="icon-sm"
                                    className="size-8 text-destructive hover:bg-destructive/10 hover:text-destructive"
                                    onClick={() => actions.remove(account)}
                                    title={t("accounts.deleteAccount")}
                                  >
                                    <Trash2 className="size-3.5" />
                                  </Button>
                                  <AccountRowActionsMenu
                                    t={t}
                                    account={account}
                                    refreshing={refreshing}
                                    authJsonExporting={authJsonExporting}
                                    includeTest={false}
                                    includeDelete={false}
                                    onTest={() => actions.openTesting(account)}
                                    onRefresh={() => actions.refresh(account)}
                                    onGenerateAuthJson={() =>
                                      actions.generateAuthJson(account)
                                    }
                                    onToggleEnabled={() =>
                                      actions.toggleEnabled(account)
                                    }
                                    onToggleLock={() =>
                                      actions.toggleLock(account)
                                    }
                                    onResetStatus={() =>
                                      actions.resetStatus(account)
                                    }
                                    onResetCredits={() =>
                                      actions.resetCredits(account)
                                    }
                                    onEditModels={() => actions.openModelsEditor(account)}
                                    onDelete={() => actions.remove(account)}
                                  />
                                </div>
                              </TableCell>
                            )}
                          </TableRow>
  );
});

// AccountMobileCard 的 memo 包装:内联回调只在本组件重渲染时重建,
// 行数据不变则整卡跳过。
const AccountCardItem = memo(function AccountCardItem({
  account,
  sequence,
  selected,
  detailOpen,
  allGroups,
  proxyCtx,
  lazyMode,
  showEmailDomainTags,
  healthBuckets,
  refreshing,
  authJsonExporting,
  variant,
  visibleColumns,
  t,
  actions,
}: {
  account: AccountRow;
  sequence: number;
  selected: boolean;
  detailOpen: boolean;
  allGroups: AccountGroup[];
  proxyCtx: ProxyBindingContext;
  lazyMode: boolean;
  showEmailDomainTags: boolean;
  healthBuckets: AccountHealthBucket[] | undefined;
  refreshing: boolean;
  authJsonExporting: boolean;
  variant: "mobile" | "personal";
  visibleColumns?: Record<AccountTableColumn, boolean>;
  t: ReturnType<typeof useTranslation>["t"];
  actions: AccountRowActions;
}) {
  return (
    <AccountMobileCard
      account={account}
      sequence={sequence}
      selected={selected}
      detailOpen={detailOpen}
      allGroups={allGroups}
      proxyCtx={proxyCtx}
      lazyMode={lazyMode}
      showEmailDomainTags={showEmailDomainTags}
      healthBuckets={healthBuckets}
      refreshing={refreshing}
      authJsonExporting={authJsonExporting}
      variant={variant}
      visibleColumns={visibleColumns}
      t={t}
      onToggleSelect={() => actions.toggleSelect(account.id)}
      onOpenDetail={() => actions.openDetail(account)}
      onEdit={() => actions.openSchedulerEditor(account)}
      onEditGroups={() => actions.openQuickGroupEditor(account)}
      onEditProxy={() => actions.openQuickProxyEditor(account)}
      onUsage={() => actions.openUsage(account)}
      onOpenOfficialUsage={() => actions.openOfficialUsage(account)}
      onTest={() => actions.openTesting(account)}
      onRefresh={() => actions.refresh(account)}
      onGenerateAuthJson={() => actions.generateAuthJson(account)}
      onToggleEnabled={() => actions.toggleEnabled(account)}
      onToggleLock={() => actions.toggleLock(account)}
      onResetStatus={() => actions.resetStatus(account)}
      onResetCredits={() => actions.resetCredits(account)}
      onEditModels={() => actions.openModelsEditor(account)}
      onDelete={() => actions.remove(account)}
      onUsageRefreshed={() => actions.usageRefreshed()}
    />
  );
});

export default function Accounts() {
  const { t } = useTranslation();
  const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS;
  const [showAdd, setShowAdd] = useState(false);
  // providerView 由路由驱动，刷新浏览器后停留在当前上游视图。
  const location = useLocation();
  const navigate = useNavigate();
  // ?groupManager=1 深链直接打开分组管理器(Grok 页的「管理分组」跳转入口,issue #487)。
  useEffect(() => {
    const params = new URLSearchParams(location.search);
    if (params.get("groupManager") === "1") {
      setShowGroupManager(true);
      params.delete("groupManager");
      const query = params.toString();
      navigate(
        { pathname: location.pathname, search: query ? `?${query}` : "" },
        { replace: true },
      );
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [location.search]);
  // ?pending=1 落到 Codex 账号页审核区（API Keys 门户卡片 / Grok 横幅共用）。
  useEffect(() => {
    const params = new URLSearchParams(location.search);
    if (params.get("pending") !== "1") return;
    const path = location.pathname.replace(/\/+$/, "");
    if (path.endsWith("/accounts/grok") || path.endsWith("/accounts/invite")) {
      navigate({ pathname: "/accounts", search: "?pending=1" }, { replace: true });
      return;
    }
    const timer = window.setTimeout(() => {
      document.getElementById("pending-review")?.scrollIntoView({ behavior: "smooth", block: "start" });
    }, 120);
    return () => window.clearTimeout(timer);
  }, [location.pathname, location.search, navigate]);
  const normalizedPath = location.pathname.replace(/\/+$/, "");
  const providerView: UpstreamChannel = normalizedPath.endsWith("/accounts/grok")
    ? "grok"
    : normalizedPath.endsWith("/accounts/antigravity")
      ? "antigravity"
      : normalizedPath.endsWith("/accounts/claude")
        ? "claude"
        : "codex";
  const { channels: visibleChannels, isChannelVisible } = useVisibleChannels();
  // 设置页隐藏了某个渠道后，直接打开它的账号路由要回落到 Codex 视图。
  useEffect(() => {
    if (!isChannelVisible(providerView)) navigate("/accounts", { replace: true });
  }, [isChannelVisible, navigate, providerView]);
  const setProviderView = useCallback(
    (view: UpstreamChannel) => {
      navigate(
        view === "grok"
          ? "/accounts/grok"
          : view === "antigravity"
            ? "/accounts/antigravity"
            : view === "claude"
              ? "/accounts/claude"
              : "/accounts",
      );
    },
    [navigate],
  );
  // Codex 邀请视图同样由路由驱动（/accounts/invite），与 Grok 视图一致：
  // 可直接分享链接、刷新浏览器停在原处、浏览器返回键能退回账号列表。
  const showInvite = normalizedPath.endsWith("/accounts/invite");
  const setShowInvite = useCallback(
    (open: boolean) => {
      navigate(open ? "/accounts/invite" : "/accounts");
    },
    [navigate],
  );
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize(
    "accounts",
    20,
    pageSizeOptions,
  );
  const [statusFilter, setStatusFilter] = useState<
    | "all"
    | "normal"
    | "scheduling"
    | "rate_limited"
    | "abnormal"
    | "banned"
    | "error"
    | "unsampled"
    | "disabled"
    | "locked"
  >("all");
  // 搜索词只保留去抖后的提交值;草稿态在 AccountSearchInput 内部,击键不触发本组件渲染。
  const [debouncedSearchQuery, setDebouncedSearchQuery] = useState("");
  const accountPageAbortRef = useRef<AbortController | null>(null);
  const [planFilter, setPlanFilter] = useState<
    "all" | "pro" | "prolite" | "plus" | "team" | "k12" | "free"
  >("all");
  // 账号类型：oauth=官方 OAuth 账号，api_key=Responses API 中转账号（issue #522）
  const [authFilter, setAuthFilter] = useState<"all" | "oauth" | "api_key">(
    "all",
  );
  const [sortKey, setSortKey] = useState<
    | "requests"
    | "today"
    | "usage"
    | "importTime"
    | "schedulerPriority"
    | "group"
    | null
  >(null);
  const [sortDir, setSortDir] = useState<"asc" | "desc">("desc");

  const commitSearchQuery = useCallback((next: string) => {
    setDebouncedSearchQuery(next);
    setPage(1);
  }, []);

  useEffect(() => () => accountPageAbortRef.current?.abort(), []);
  const [addForm, setAddForm] = useState<AddAccountRequest>({
    refresh_token: "",
    session_token: "",
    proxy_url: "",
  });
  const [addCustomHeadersText, setAddCustomHeadersText] = useState("");
  const [workspaceRouteID, setWorkspaceRouteID] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [refreshingIds, setRefreshingIds] = useState<Set<number>>(new Set());
  const [authJsonExportingIds, setAuthJsonExportingIds] = useState<Set<number>>(
    new Set(),
  );
  const [authJsonModal, setAuthJsonModal] = useState<{
    account: AccountRow;
    json: string;
  } | null>(null);
  const [batchLoading, setBatchLoading] = useState(false);
  const [batchRefreshing, setBatchRefreshing] = useState(false);
  const [batchTesting, setBatchTesting] = useState(false);
  const [operationProgress, setOperationProgress] =
    useState<OperationProgressState | null>(null);
  const [operationResults, setOperationResults] =
    useState<AccountOperationResultsState | null>(null);
  const [showOperationResults, setShowOperationResults] = useState(
    readOperationResultsVisibility,
  );
  const showOperationResultsRef = useRef(showOperationResults);
  const operationResultMap = useRef<Map<number, AccountOperationResult>>(
    new Map(),
  );
  const operationProgressHideTimer = useRef<number | null>(null);
  const operationProgressFrame = useRef<number | null>(null);
  const operationProgressFlushTimer = useRef<number | null>(null);
  const lastOperationProgressFlushAt = useRef(0);
  const pendingOperationProgress = useRef<{
    title: string;
    event: BatchOperationEvent;
  } | null>(null);
  const [lockingSubscriptionAccounts, setLockingSubscriptionAccounts] =
    useState(false);
  const [cleaningBanned, setCleaningBanned] = useState(false);
  const [cleaningRateLimited, setCleaningRateLimited] = useState(false);
  const [cleaningError, setCleaningError] = useState(false);
  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null);
  const [quickConfigAccount, setQuickConfigAccount] = useState<AccountRow | null>(null);
  const [usageAccount, setUsageAccount] = useState<AccountRow | null>(null);
  // 用量弹窗打开时停在哪个 tab。列表里点「官方结算」成本直接落到官方统计,
  // 其余入口保持默认的概览。
  const [usageInitialPage, setUsageInitialPage] = useState<
    "overview" | "official"
  >("overview");
  const [detailAccountId, setDetailAccountId] = useState<number | null>(null);
  const [detailAccountData, setDetailAccountData] = useState<AccountRow | null>(null);
  const detailNavigationTargetRef = useRef<"first" | "last" | null>(null);
  const [editingAccount, setEditingAccount] = useState<AccountRow | null>(null);
  const [editSubmitting, setEditSubmitting] = useState(false);
  const [editTab, setEditTab] = useState<"scheduler" | "account">("scheduler");
  const [scoreMode, setScoreMode] = useState<"default" | "custom">("default");
  const [scoreInput, setScoreInput] = useState("");
  const [concurrencyMode, setConcurrencyMode] = useState<"default" | "custom">(
    "default",
  );
  const [concurrencyInput, setConcurrencyInput] = useState("");
  const [skipWarmTier, setSkipWarmTier] = useState(false);
  const [editAutoPause5hThresholdInput, setEditAutoPause5hThresholdInput] =
    useState("");
  const [editAutoPause7dThresholdInput, setEditAutoPause7dThresholdInput] =
    useState("");
  const [editAutoPause5hDisabled, setEditAutoPause5hDisabled] =
    useState(false);
  const [editAutoPause7dDisabled, setEditAutoPause7dDisabled] =
    useState(false);
  const [editIgnoreUsageLimitStatusMode, setEditIgnoreUsageLimitStatusMode] =
    useState<"inherit" | "enabled" | "disabled">("inherit");
  const [editDispatchCountLimitInput, setEditDispatchCountLimitInput] =
    useState("");
  const [editSchedulerPriorityInput, setEditSchedulerPriorityInput] =
    useState("");
  const [allowedAPIKeySelection, setAllowedAPIKeySelection] = useState<
    number[]
  >([]);
  const [editProxyUrl, setEditProxyUrl] = useState("");
  const [editCustomHeadersText, setEditCustomHeadersText] = useState("");
  const [editCodexFingerprintMode, setEditCodexFingerprintMode] =
    useState<CodexFingerprintMode>("off");
  // 代理池条目：账号表单里"从代理池选择"下拉的数据源。加载失败静默留空
  // （选择器为空时自动隐藏，不影响手动填代理）。
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  useEffect(() => {
    if (providerView !== "codex") return;
    let cancelled = false;
    void api
      .listProxies()
      .then((res) => {
        if (!cancelled) setProxyPool(res.proxies ?? []);
      })
      .catch(() => {
        if (!cancelled) setProxyPool([]);
      });
    return () => {
      cancelled = true;
    };
  }, [providerView]);
  const [editOpenAIForm, setEditOpenAIForm] =
    useState<UpdateOpenAIResponsesAccountRequest>({
      name: "",
      base_url: "https://api.openai.com",
      api_key: "",
      balance_query_url: "",
      models: [],
      codex_client_metadata_mode: "auto",
      proxy_url: "",
    });
  const [openAIModelDraft, setOpenAIModelDraft] = useState("");
  const [editOpenAIModelDraft, setEditOpenAIModelDraft] = useState("");
  const [editOpenAIModelMappingText, setEditOpenAIModelMappingText] =
    useState("");
  const [editOpenAIModelMappingMode, setEditOpenAIModelMappingMode] =
    useState<ModelMappingMode>("form");
  const [editOpenAIModelMappingEntries, setEditOpenAIModelMappingEntries] =
    useState<ModelMappingEntry[]>(emptyModelMappingEntries);
  const [editOpenAIModelsLoading, setEditOpenAIModelsLoading] = useState(false);
  const [importing, setImporting] = useState(false);
  const [showImportPicker, setShowImportPicker] = useState(false);
  const [importProxyUrl, setImportProxyUrl] = useState("");
  const [importCustomHeadersText, setImportCustomHeadersText] = useState("");
  const [importWorkspaceRouteID, setImportWorkspaceRouteID] = useState("");
  const [showSub2APIImport, setShowSub2APIImport] = useState(false);
  const [showPasteImport, setShowPasteImport] = useState(false);
  const [pasteImportText, setPasteImportText] = useState("");
  const [dragging, setDragging] = useState(false);
  const dragCounter = useRef(0);
  const [showExportPicker, setShowExportPicker] = useState(false);
  // 导出时是否带上账号绑定的代理。默认关闭：代理 URL 常含明文用户名密码，
  // 只有在确实要迁移「号池 + 代理绑定关系」时才该打开。
  const [exportIncludeProxy, setExportIncludeProxy] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [showMigrate, setShowMigrate] = useState(false);
  const [showAnalysisCharts, setShowAnalysisCharts] = useState(
    getInitialAnalysisVisibility,
  );
  const [accountAnalysis, setAccountAnalysis] =
    useState<AccountAnalysisResponse | null>(null);
  const [accountAnalysisLoading, setAccountAnalysisLoading] = useState(false);
  const [accountAnalysisError, setAccountAnalysisError] = useState<string | null>(null);
  const accountAnalysisAbortRef = useRef<AbortController | null>(null);
  const [showRecycleBin, setShowRecycleBin] = useState(false);
  const [showEmailDomainTags, setShowEmailDomainTags] = useState(
    getInitialEmailDomainVisibility,
  );
  const [migrateUrl, setMigrateUrl] = useState("");
  const [migrateKey, setMigrateKey] = useState("");
  const [migrating, setMigrating] = useState(false);
  const [importProgress, setImportProgress] = useState<{
    show: boolean;
    current: number;
    total: number;
    success: number;
    updated: number;
    duplicate: number;
    failed: number;
    done: boolean;
  }>({
    show: false,
    current: 0,
    total: 0,
    success: 0,
    updated: 0,
    duplicate: 0,
    failed: 0,
    done: false,
  });
  // 导入后的邀请积分引导。ids 是本次导入新建的账号,后端会过滤出能发邀请的。
  const [inviteGuide, setInviteGuide] = useState<{ show: boolean; ids: number[] }>({
    show: false,
    ids: [],
  });
  const [addMethod, setAddMethod] = useState<
    "rt" | "st" | "at" | "session" | "openai" | "oauth" | "agentIdentity"
  >("oauth");
  const [agentIdentityJson, setAgentIdentityJson] = useState("");
  const [agentIdentityProxyUrl, setAgentIdentityProxyUrl] = useState("");
  const agentIdentityFileInputRef = useRef<HTMLInputElement | null>(null);
  const [agentIdentityFilesImporting, setAgentIdentityFilesImporting] =
    useState(false);
  const [agentIdentityFilesResult, setAgentIdentityFilesResult] = useState<{
    total: number;
    imported: number;
    failed: number;
    items: AgentIdentityImportItem[];
  } | null>(null);
  const [atForm, setAtForm] = useState<AddATAccountRequest>({
    access_token: "",
    proxy_url: "",
  });
  const [sessionJson, setSessionJson] = useState("");
  const [sessionProxyUrl, setSessionProxyUrl] = useState("");
  // 允许重复添加：勾选后本次添加/导入跳过去重，强制新建（添加弹窗与导入弹窗共用）。
  const [allowDuplicate, setAllowDuplicate] = useState(false);
  // 采用文件内代理：勾选后 JSON 文件里带的 proxy_url 生效，并自动注册进代理池。
  // 只对 JSON 系格式有意义，TXT 一行一个 token 物理上带不了代理。
  const [importFileProxies, setImportFileProxies] = useState(false);
  const [openAIForm, setOpenAIForm] =
    useState<AddOpenAIResponsesAccountRequest>({
      base_url: "https://api.openai.com",
      api_key: "",
      balance_query_url: "",
      models: [],
      codex_client_metadata_mode: "auto",
      proxy_url: "",
    });
  const [openAIModelMappingText, setOpenAIModelMappingText] = useState("");
  const [openAIModelMappingMode, setOpenAIModelMappingMode] =
    useState<ModelMappingMode>("form");
  const [openAIModelMappingEntries, setOpenAIModelMappingEntries] = useState<
    ModelMappingEntry[]
  >(emptyModelMappingEntries);
  const [openAIModelsLoading, setOpenAIModelsLoading] = useState(false);
  const [oauthStep, setOauthStep] = useState<"generate" | "exchange">(
    "generate",
  );
  const [oauthSession, setOauthSession] = useState<{
    session_id: string;
    auth_url: string;
  } | null>(null);
  const [oauthProxyUrl, setOauthProxyUrl] = useState("");
  const [oauthCallbackUrl, setOauthCallbackUrl] = useState("");
  const [oauthName, setOauthName] = useState("");
  const [oauthGenerating, setOauthGenerating] = useState(false);
  const [oauthCompleting, setOauthCompleting] = useState(false);
  const [editOAuthStep, setEditOAuthStep] = useState<"generate" | "exchange">(
    "generate",
  );
  const [editOAuthSession, setEditOAuthSession] = useState<{
    session_id: string;
    auth_url: string;
  } | null>(null);
  const [editOAuthProxyUrl, setEditOAuthProxyUrl] = useState("");
  const [editOAuthCallbackUrl, setEditOAuthCallbackUrl] = useState("");
  const [editOAuthGenerating, setEditOAuthGenerating] = useState(false);
  const [editOAuthUpdating, setEditOAuthUpdating] = useState(false);
  const [editTags, setEditTags] = useState<string[]>([]);
  const [editGroupIds, setEditGroupIds] = useState<number[]>([]);
  const [quickGroupAccount, setQuickGroupAccount] = useState<AccountRow | null>(
    null,
  );
  const [quickGroupIds, setQuickGroupIds] = useState<number[]>([]);
  const [quickGroupSubmitting, setQuickGroupSubmitting] = useState(false);
  // 代理徽章点开的快速绑定弹窗：只改 proxy_url 一个字段。
  const [quickProxyAccount, setQuickProxyAccount] = useState<AccountRow | null>(
    null,
  );
  // OAuth 账号“支持模型”白名单编辑器状态;空白名单表示该账号可调度所有模型。
  const [modelsAccount, setModelsAccount] = useState<AccountRow | null>(null);
  const [modelsDraft, setModelsDraft] = useState<string[]>([]);
  const [modelsInputDraft, setModelsInputDraft] = useState("");
  const [modelsSyncing, setModelsSyncing] = useState(false);
  const [modelsProbing, setModelsProbing] = useState(false);
  const [modelsSaving, setModelsSaving] = useState(false);
  // 探测看板：逐模型的实时测试状态（pending→testing→结果）。
  const [probeBoard, setProbeBoard] = useState<ModelProbeItem[]>([]);
  const [tagFilter, setTagFilter] = useState<string>("");
  const [domainFilter, setDomainFilter] = useState<string>("");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(
    EMPTY_ACCOUNT_GROUP_FILTER,
  );
  const [allGroups, setAllGroups] = useState<AccountGroup[]>([]);
  // 分组按渠道隔离(issue #487):Codex 页的所有分组选择器只出 codex 渠道分组;
  // 管理器仍显示全部渠道(带徽标),徽标解析也用全量以兼容迁移前的跨渠道成员。
  const codexGroups = useMemo(
    () => allGroups.filter((group) => group.channel === "codex"),
    [allGroups],
  );
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([]);
  // 代理徽章的判定上下文。fail-closed(钉住的托管代理已不在启用池)只在代理池
  // 开启时成立,所以开关与全局代理都得跟着代理池一起进来。
  const [proxyPoolEnabled, setProxyPoolEnabled] = useState(false);
  const [globalProxyURL, setGlobalProxyURL] = useState("");
  // 单个对象且引用稳定:memo 行组件的 props 里不能出现每轮新建的数组/对象,
  // 否则整表 memo 失效(账号页性能优化的既有教训)。
  // 分组用全量而非 codexGroups:后端解析组代理时不看渠道,迁移前挂在别的渠道组里
  // 的存量成员照样吃那条组代理,按渠道过滤会把它误报成"无组代理"。
  const proxyBindingCtx = useMemo<ProxyBindingContext>(
    () =>
      buildProxyBindingContext({
        proxies: proxyPool,
        groups: allGroups,
        poolEnabled: proxyPoolEnabled,
        globalProxy: globalProxyURL,
      }),
    [proxyPool, allGroups, proxyPoolEnabled, globalProxyURL],
  );
  const [lazyMode, setLazyMode] = useState(false);
  const [accountPortalEnabled, setAccountPortalEnabled] = useState(false);
  const [panelPendingCount, setPanelPendingCount] = useState(0);
  const [grokSelfServicePending, setGrokSelfServicePending] = useState(0);
  // 导入/添加账号时直接绑定的分组（记住上次选择，添加弹窗与导入弹窗共用，与 allowDuplicate 同风格）。
  const {
    groupIds: importGroupIds,
    setGroupIds: setImportGroupIds,
    prune: pruneImportGroupIds,
  } = useImportGroupIds();
  const [showGroupManager, setShowGroupManager] = useState(false);
  const [groupDraft, setGroupDraft] = useState<AccountGroupDraft>({
    id: null,
    name: "",
    description: "",
    color: ACCOUNT_GROUP_COLORS[0],
    baseConcurrencyInput: "",
    auto_pause_5h_threshold: 0,
    auto_pause_7d_threshold: 0,
    proxyURLsInput: "",
    channel: "codex",
  });
  const [groupSubmitting, setGroupSubmitting] = useState(false);
  const [showBatchMetaEditor, setShowBatchMetaEditor] = useState(false);
  const [batchMetaMode, setBatchMetaMode] = useState<"all" | "groups">("all");
  const [batchUpdateTags, setBatchUpdateTags] = useState(false);
  const [batchTags, setBatchTags] = useState<string[]>([]);
  const [batchUpdateGroups, setBatchUpdateGroups] = useState(false);
  const [batchGroupIds, setBatchGroupIds] = useState<number[]>([]);
  const [batchUpdateScoreBias, setBatchUpdateScoreBias] = useState(false);
  const [batchScoreBiasInput, setBatchScoreBiasInput] = useState("");
  const [batchUpdateBaseConcurrency, setBatchUpdateBaseConcurrency] =
    useState(false);
  const [batchBaseConcurrencyInput, setBatchBaseConcurrencyInput] =
    useState("");
  const [batchUpdateSchedulerPriority, setBatchUpdateSchedulerPriority] =
    useState(false);
  const [batchSchedulerPriorityInput, setBatchSchedulerPriorityInput] =
    useState("");
  const [
    batchUpdateCodexFingerprintMode,
    setBatchUpdateCodexFingerprintMode,
  ] = useState(false);
  const [batchCodexFingerprintMode, setBatchCodexFingerprintMode] =
    useState<CodexFingerprintMode>("off");
  const [batchMetaSubmitting, setBatchMetaSubmitting] = useState(false);
  const [showBatchQuotaAutoPauseEditor, setShowBatchQuotaAutoPauseEditor] =
    useState(false);
  const [batchAutoPause5hThresholdInput, setBatchAutoPause5hThresholdInput] =
    useState("");
  const [batchAutoPause7dThresholdInput, setBatchAutoPause7dThresholdInput] =
    useState("");
  const [batchAutoPause5hDisabled, setBatchAutoPause5hDisabled] =
    useState(false);
  const [batchAutoPause7dDisabled, setBatchAutoPause7dDisabled] =
    useState(false);
  const [batchQuotaAutoPauseSubmitting, setBatchQuotaAutoPauseSubmitting] =
    useState(false);
  const [visibleColumns, setVisibleColumns] = useState<
    Record<AccountTableColumn, boolean>
  >(getInitialAccountVisibleColumns);
  const [viewMode, setViewMode] = useState<AccountViewMode>(
    getInitialAccountViewMode,
  );
  const [pageMode, setPageMode] = useState<AccountPageMode>(
    () => getStoredAccountPageMode() ?? "pool",
  );
  // 用户是否手动设置过页面模式（设置过则一律尊重用户，不再按账号数自动判定）。
  const pageModeUserSetRef = useRef(getStoredAccountPageMode() !== null);
  // 自动判定只在首次（账号加载完成后）应用一次。
  const pageModeAutoAppliedRef = useRef(false);
  const isDesktopLayout = useMediaQuery("(min-width: 1024px)");
  const fileInputRef = useRef<HTMLInputElement>(null);
  const jsonInputRef = useRef<HTMLInputElement>(null);
  const jsonAtInputRef = useRef<HTMLInputElement>(null);
  const atFileInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);
  const selectAllRef = useRef<HTMLInputElement>(null);
  const { toast, showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  // 代理字段统一走 ProxyField(手填+测试+代理池下拉),与 Grok/Claude/Antigravity 同构。
  const renderProxyInput = ({
    value,
    onChange,
    label = t("accounts.proxyUrl"),
    placeholder = t("accounts.proxyUrlPlaceholder"),
    disabled = false,
  }: {
    value: string;
    onChange: (value: string) => void;
    label?: string;
    placeholder?: string;
    disabled?: boolean;
  }) => (
    <ProxyField
      value={value}
      onChange={onChange}
      proxies={proxyPool}
      label={label}
      labelClassName="text-sm"
      placeholder={placeholder}
      disabled={disabled}
    />
  );

  const renderCustomHeadersTextarea = ({
    value,
    onChange,
  }: {
    value: string;
    onChange: (value: string) => void;
  }) => (
    <div>
      <div className="flex items-center justify-between mb-2">
        <label className="block text-sm font-semibold text-muted-foreground">
          上游自定义请求头 JSON
        </label>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => onChange(CUSTOM_HEADERS_PLACEHOLDER)}
        >
          插入模板
        </Button>
      </div>
      <textarea
        className="w-full min-h-[140px] p-3 border border-input rounded-xl bg-background text-sm resize-y font-mono focus:outline-none focus:ring-2 focus:ring-ring"
        placeholder={CUSTOM_HEADERS_PLACEHOLDER}
        value={value}
        onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
          onChange(event.target.value)
        }
        rows={6}
        spellCheck={false}
      />
      <p className="mt-1.5 text-xs text-muted-foreground">
        留空表示不设置；JSON 必须是对象，所有请求头值都必须是字符串。
      </p>
    </div>
  );

  const renderWorkspaceRouteInput = ({
    value = workspaceRouteID,
    onChange = setWorkspaceRouteID,
  }: {
    value?: string;
    onChange?: (value: string) => void;
  } = {}) => (
    <div className="rounded-xl border border-sky-200 bg-sky-50/70 p-4 dark:border-sky-900 dark:bg-sky-950/30">
      <div className="mb-2 flex items-center gap-2">
        <Layers className="size-4 text-sky-600 dark:text-sky-400" />
        <label className="text-sm font-semibold text-foreground">
          {t("accounts.workspaceRouteLabel")}
        </label>
      </div>
      <Input
        value={value}
        placeholder={t("accounts.workspaceRoutePlaceholder")}
        onChange={(event: ChangeEvent<HTMLInputElement>) =>
          onChange(event.target.value)
        }
      />
      <p className="mt-2 text-xs leading-5 text-muted-foreground">
        {t("accounts.workspaceRouteHint")}
      </p>
    </div>
  );

  const renderModelMappingEditor = ({
    value,
    onChange,
    mode,
    onModeChange,
    entries,
    onEntriesChange,
  }: {
    value: string;
    onChange: (value: string) => void;
    mode: ModelMappingMode;
    onModeChange: (value: ModelMappingMode) => void;
    entries: ModelMappingEntry[];
    onEntriesChange: (value: ModelMappingEntry[]) => void;
  }) => {
    const switchToForm = () => {
      const parsed = parseModelMappingEntries(value);
      if (!parsed.ok) {
        showToast("当前 JSON 无法转成填空模式，请先修正 JSON", "error");
        return;
      }
      onEntriesChange(parsed.entries);
      onModeChange("form");
    };

    const switchToJSON = () => {
      const serialized = serializeModelMappingEntries(entries);
      if (!serialized.ok) {
        showToast("模型映射行必须成对填写，源模型不能重复", "error");
        return;
      }
      onChange(serialized.value);
      onModeChange("json");
    };

    const updateEntry = (
      index: number,
      field: keyof ModelMappingEntry,
      nextValue: string,
    ) => {
      onEntriesChange(
        entries.map((entry, entryIndex) =>
          entryIndex === index ? { ...entry, [field]: nextValue } : entry,
        ),
      );
    };

    const removeEntry = (index: number) => {
      const next = entries.filter((_, entryIndex) => entryIndex !== index);
      onEntriesChange(next.length > 0 ? next : emptyModelMappingEntries());
    };

    const insertTemplate = () => {
      if (mode === "json") {
        onChange(MODEL_MAPPING_PLACEHOLDER);
        return;
      }
      onEntriesChange([
        { from: "client-model", to: "upstream-model" },
        { from: "legacy-*", to: "gpt-4.1" },
      ]);
    };

    return (
      <div>
      <div className="flex items-center justify-between mb-2">
        <label className="block text-sm font-semibold text-muted-foreground">
          单渠道模型映射
        </label>
        <div className="flex items-center gap-2">
          <div className="inline-flex rounded-lg border border-border bg-muted/40 p-0.5">
            <button
              type="button"
              onClick={switchToForm}
              className={`rounded-md px-2.5 py-1 text-xs font-semibold transition-colors ${
                mode === "form"
                  ? "bg-background text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              填空
            </button>
            <button
              type="button"
              onClick={switchToJSON}
              className={`rounded-md px-2.5 py-1 text-xs font-semibold transition-colors ${
                mode === "json"
                  ? "bg-background text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              JSON
            </button>
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={insertTemplate}
          >
            插入模板
          </Button>
        </div>
      </div>
      {mode === "form" ? (
        <div className="space-y-2">
          {entries.map((entry, index) => (
            <div
              key={index}
              className="grid grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]"
            >
              <Input
                placeholder="客户端模型，如 client-model / legacy-*"
                value={entry.from}
                onChange={(event: ChangeEvent<HTMLInputElement>) =>
                  updateEntry(index, "from", event.target.value)
                }
              />
              <Input
                placeholder="上游模型，如 gpt-4.1"
                value={entry.to}
                onChange={(event: ChangeEvent<HTMLInputElement>) =>
                  updateEntry(index, "to", event.target.value)
                }
              />
              <Button
                type="button"
                variant="outline"
                size="icon"
                className="h-10 w-10"
                onClick={() => removeEntry(index)}
                disabled={
                  entries.length === 1 && !entry.from.trim() && !entry.to.trim()
                }
                title="删除映射"
              >
                <X className="size-4" />
              </Button>
            </div>
          ))}
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() =>
              onEntriesChange([...entries, { from: "", to: "" }])
            }
          >
            <Plus className="size-3.5" />
            添加映射
          </Button>
        </div>
      ) : (
        <textarea
          className="w-full min-h-[140px] p-3 border border-input rounded-xl bg-background text-sm resize-y font-mono focus:outline-none focus:ring-2 focus:ring-ring"
          placeholder={MODEL_MAPPING_PLACEHOLDER}
          value={value}
          onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
            onChange(event.target.value)
          }
          rows={6}
          spellCheck={false}
        />
      )}
      <p className="mt-1.5 text-xs text-muted-foreground">
        留空表示使用原模型；左侧是客户端请求模型，右侧是该渠道上游模型，支持 * 通配。JSON 模式格式为 {"{"}"client-model":"upstream-model"{"}"}。
      </p>
    </div>
    );
  };

  useEffect(() => {
    return () => {
      if (operationProgressHideTimer.current !== null) {
        window.clearTimeout(operationProgressHideTimer.current);
      }
      if (operationProgressFrame.current !== null) {
        window.cancelAnimationFrame(operationProgressFrame.current);
      }
      if (operationProgressFlushTimer.current !== null) {
        window.clearTimeout(operationProgressFlushTimer.current);
      }
      pendingOperationProgress.current = null;
      operationResultMap.current.clear();
    };
  }, []);

  const closeOperationProgress = useCallback(() => {
    if (operationProgressHideTimer.current !== null) {
      window.clearTimeout(operationProgressHideTimer.current);
      operationProgressHideTimer.current = null;
    }
    setOperationProgress(null);
  }, []);

  const handleOperationResultsVisibilityChange = useCallback(
    (visible: boolean) => {
      showOperationResultsRef.current = visible;
      setShowOperationResults(visible);
      writeOperationResultsVisibility(visible);
      if (!visible) {
        setOperationResults(null);
      }
    },
    [],
  );

  const scheduleOperationProgressClose = useCallback(() => {
    if (operationProgressHideTimer.current !== null) {
      window.clearTimeout(operationProgressHideTimer.current);
    }
    operationProgressHideTimer.current = window.setTimeout(() => {
      setOperationProgress(null);
      operationProgressHideTimer.current = null;
    }, 5000);
  }, []);

  const commitOperationProgressEvent = useCallback(
    (title: string, event: BatchOperationEvent) => {
      const results = snapshotAccountOperationResults(
        operationResultMap.current,
      );
      setOperationProgress((prev) => ({
        show: true,
        action: event.action,
        title,
        current: event.current ?? prev?.current ?? 0,
        total: event.total ?? prev?.total ?? 0,
        success: event.success ?? prev?.success ?? 0,
        failed: event.failed ?? prev?.failed ?? 0,
        banned: event.banned ?? prev?.banned ?? 0,
        rateLimited: event.rate_limited ?? prev?.rateLimited ?? 0,
        deleted: event.deleted ?? prev?.deleted ?? 0,
        done: event.type === "complete",
        message: operationProgressMessage(event, prev?.message),
      }));
      if (event.type === "complete") {
        if (
          showOperationResultsRef.current &&
          (event.action === "batch_test" ||
            event.action === "batch_refresh")
        ) {
          setOperationResults({
            action: event.action,
            results,
          });
        }
        scheduleOperationProgressClose();
      }
    },
    [scheduleOperationProgressClose],
  );

  const flushOperationProgressEvent = useCallback(() => {
    operationProgressFrame.current = null;
    const pending = pendingOperationProgress.current;
    if (!pending) return;
    pendingOperationProgress.current = null;
    lastOperationProgressFlushAt.current = performance.now();
    commitOperationProgressEvent(pending.title, pending.event);
  }, [commitOperationProgressEvent]);

  const applyOperationProgressEvent = useCallback(
    (title: string, event: BatchOperationEvent) => {
      collectAccountOperationResult(operationResultMap.current, event);
      if (event.type === "start") {
        setOperationResults(null);
      }
      if (operationProgressHideTimer.current !== null) {
        window.clearTimeout(operationProgressHideTimer.current);
        operationProgressHideTimer.current = null;
      }

      pendingOperationProgress.current = { title, event };

      if (event.type === "complete") {
        if (operationProgressFrame.current !== null) {
          window.cancelAnimationFrame(operationProgressFrame.current);
          operationProgressFrame.current = null;
        }
        if (operationProgressFlushTimer.current !== null) {
          window.clearTimeout(operationProgressFlushTimer.current);
          operationProgressFlushTimer.current = null;
        }
        flushOperationProgressEvent();
        return;
      }

      if (
        operationProgressFrame.current === null &&
        operationProgressFlushTimer.current === null
      ) {
        const now = performance.now();
        const delay = Math.max(
          0,
          OPERATION_PROGRESS_FLUSH_INTERVAL_MS -
            (now - lastOperationProgressFlushAt.current),
        );
        if (delay > 0) {
          operationProgressFlushTimer.current = window.setTimeout(() => {
            operationProgressFlushTimer.current = null;
            operationProgressFrame.current = window.requestAnimationFrame(
              flushOperationProgressEvent,
            );
          }, delay);
          return;
        }
        operationProgressFrame.current = window.requestAnimationFrame(
          flushOperationProgressEvent,
        );
      }
    },
    [flushOperationProgressEvent],
  );

  const runStreamingAccountOperation = useCallback(
    async (
      path: string,
      body: unknown,
      title: string,
    ): Promise<BatchOperationEvent | null> => {
      let finalEvent: BatchOperationEvent | null = null;
      const res = await postAdminSSE(path, body);
      await readOperationSSE(res, (event) => {
        applyOperationProgressEvent(title, event);
        if (event.type === "complete") {
          finalEvent = event;
        }
      });
      return finalEvent;
    },
    [applyOperationProgressEvent],
  );

  const [pagedHealthBars, setPagedHealthBars] = useState<
    Record<string, AccountHealthBucket[]>
  >({});

  const loadAccounts = useCallback(async (_options?: LoadOptions) => {
    accountPageAbortRef.current?.abort();
    const controller = new AbortController();
    accountPageAbortRef.current = controller;
    const accountsResponse = await api.getAccountsPage({
      channel: "codex",
      page,
      pageSize,
      search: debouncedSearchQuery,
      status: statusFilter,
      plan: planFilter,
      authKind: authFilter,
      tag: tagFilter,
      emailDomain: domainFilter,
      groupInclude: groupFilter.include,
      groupExclude: groupFilter.exclude,
      ungrouped: groupFilter.ungrouped,
      sort: sortKey === "importTime"
        ? "created_at"
        : sortKey === "schedulerPriority"
          ? "scheduler_priority"
          : sortKey ?? undefined,
      order: sortDir,
    }, controller.signal);
    return {
      accounts: accountsResponse.accounts ?? [],
      total: accountsResponse.total,
      page: accountsResponse.page,
      summary: accountsResponse.summary,
      facets: accountsResponse.facets,
      snapshotAt: accountsResponse.snapshot_at,
      statsState: accountsResponse.stats_state,
      disabledSorts: accountsResponse.disabled_sorts ?? [],
    };
  }, [authFilter, debouncedSearchQuery, domainFilter, groupFilter.exclude, groupFilter.include, groupFilter.ungrouped, page, pageSize, planFilter, sortDir, sortKey, statusFilter, tagFilter]);

  const loadAccountAnalysis = useCallback(async (opts?: { silent?: boolean }) => {
    accountAnalysisAbortRef.current?.abort();
    const controller = new AbortController();
    accountAnalysisAbortRef.current = controller;
    if (!opts?.silent) {
      setAccountAnalysisLoading(true);
      setAccountAnalysisError(null);
    }
    try {
      const response = await api.getAccountAnalysis("codex", controller.signal);
      if (!controller.signal.aborted) setAccountAnalysis(response);
    } catch (analysisError) {
      if (!controller.signal.aborted && !opts?.silent) {
        setAccountAnalysisError(getErrorMessage(analysisError));
      }
    } finally {
      if (!controller.signal.aborted && !opts?.silent) setAccountAnalysisLoading(false);
    }
  }, []);

  // Auxiliary data is intentionally independent from the account page request:
  // failures or slow settings/overview/group calls must not hold up the first row.
  useEffect(() => {
    if (providerView !== "codex") return;
    let cancelled = false;
    void api.getAPIKeys()
      .then((response) => { if (!cancelled) setAPIKeys(response.keys ?? []); })
      .catch(() => undefined);
    void api.listAccountGroups()
      .then((response) => { if (!cancelled) setAllGroups(response.groups ?? []); })
      .catch(() => undefined);
    void api.getSettings()
      .then((settings) => {
        if (cancelled) return;
        setLazyMode(settings.lazy_mode);
        setAccountPortalEnabled(Boolean(settings.public_account_portal_page_enabled));
        setProxyPoolEnabled(Boolean(settings.proxy_pool_enabled));
        setGlobalProxyURL((settings.proxy_url ?? "").trim());
      })
      .catch(() => undefined);
    return () => { cancelled = true; };
  }, [providerView]);

  useEffect(() => {
    if (providerView !== "grok") return;
    let cancelled = false;
    void api.getAccountsPage({
      channel: "codex",
      page: 1,
      pageSize: 1,
      status: "disabled",
      tag: "self-service",
    })
      .then((response) => {
        if (!cancelled) setGrokSelfServicePending(response.total ?? 0);
      })
      .catch(() => undefined);
    return () => { cancelled = true; };
  }, [providerView]);

  const { data, setData, loading, error, reload, reloadSilently } = useDataLoader<{
    accounts: AccountRow[];
    total: number;
    page: number;
    summary: AccountListSummary | null;
    facets: { tags: string[]; email_domains: AccountEmailDomainFacet[] };
    snapshotAt: string;
    statsState: "ready" | "stale" | "warming";
    disabledSorts: string[];
  }>({
    initialData: {
      accounts: [],
      total: 0,
      page: 1,
      summary: null,
      facets: { tags: [], email_domains: [] },
      snapshotAt: "",
      statsState: "warming",
      disabledSorts: [],
    },
    load: loadAccounts,
    // Grok 视图与本组件共用挂载:此时 codex 列表链(列表→health-bars→page-stats
    // →静默重载循环)必须整体停摆,否则在 Grok 页后台空转并连带整树重渲染。
    enabled: providerView === "codex",
  });
  const disabledSorts = useMemo(
    () => resolveDisabledAccountSorts(data.disabledSorts, data.summary?.total),
    [data.disabledSorts, data.summary?.total],
  );
  const usageSortBlocked = isLargePoolSortDisabled("requests", disabledSorts);
  const guardUsageSort = useCallback(
    (key: "requests" | "today") => {
      if (!isLargePoolSortDisabled(key, disabledSorts)) {
        return false;
      }
      showToast(t("accounts.largePoolSortDisabled"), "warning");
      return true;
    },
    [disabledSorts, showToast, t],
  );
  // 禁用窗口(冷启动首轮聚合)只拦截新的排序点击,不清用户已选的排序:
  // 后端会把被禁用的排序静默降级成默认序,聚合完成后自动按原选择恢复。
  // data.snapshotAt 变化说明列表快照重建过(含删除/封禁后的缓存失效),
  // 分析图跟着重取,避免统计卡已更新而额度分布仍显示旧账号集。
  useEffect(() => {
    if (!showAnalysisCharts || providerView !== "codex") {
      accountAnalysisAbortRef.current?.abort();
      return undefined;
    }
    void loadAccountAnalysis();
    return () => accountAnalysisAbortRef.current?.abort();
  }, [loadAccountAnalysis, showAnalysisCharts, data.snapshotAt, providerView]);
  const refreshAccountRow = useCallback(async (id: number) => {
    const account = await api.getAccount(id);
    setDetailAccountData((current) => current?.id === id ? account : current);
    setData((current) => ({
      ...current,
      accounts: current.accounts.map((item) => item.id === id ? account : item),
    }));
    return account;
  }, [setData]);
  // 删除后先本地摘行,再静默对齐统计卡。reload() 会把顶部打成「加载中」,
  // 大号池上连点删除等于反复整页转圈。
  const refreshAfterAccountRemoval = useCallback((ids: number[]) => {
    if (ids.length === 0) {
      void reloadSilently();
      return;
    }
    const drop = new Set(ids);
    let remainingOnPage = 0;
    setData((current) => {
      const accounts = current.accounts.filter((account) => !drop.has(account.id));
      remainingOnPage = accounts.length;
      return {
        ...current,
        accounts,
        total: Math.max(0, current.total - ids.length),
        summary: current.summary
          ? { ...current.summary, total: Math.max(0, current.summary.total - ids.length) }
          : current.summary,
      };
    });
    setSelected((current) => {
      if (current.size === 0) return current;
      const next = new Set(current);
      let changed = false;
      for (const id of ids) {
        if (next.delete(id)) changed = true;
      }
      return changed ? next : current;
    });
    if (remainingOnPage === 0 && page > 1) {
      setPage(page - 1);
      return;
    }
    void reloadSilently();
  }, [page, reloadSilently, setData]);
  const visibleAccountIDs = useMemo(
    () => data.accounts.map((account) => account.id),
    [data.accounts],
  );
  const applyAccountLiveState = useCallback((response: AccountLiveStateResponse) => {
    setData((current) => {
      const accounts = mergeAccountLiveState(current.accounts, response);
      return accounts === current.accounts ? current : { ...current, accounts };
    });
  }, [setData]);
  useAccountLiveState(visibleAccountIDs, applyAccountLiveState, providerView === "codex");
  const loadAccountDetail = useCallback(
    (account: AccountRow) =>
      account.detail_loaded ? Promise.resolve(account) : api.getAccount(account.id),
    [],
  );
  // page-stats 存独立 state 并在渲染时合并:分页基础行不含成本/用量明细,
  // 若把 stats 写回 data.accounts,任何静默列表刷新都会用基础行整体覆盖,
  // 成本列在 ID 不变时永久变 "-" (issue #499)。
  const [accountPageStats, setAccountPageStats] = useState<Record<string, AccountPageStatsItem>>({});
  // 用量弹窗里手动刷新官方统计后 bump 一次,强制重拉本页 stats——
  // 否则官方成本胶囊要等翻页/改筛选才出现,看起来像刷新没生效。
  const [pageStatsReloadToken, setPageStatsReloadToken] = useState(0);
  const handleOfficialUsageRefreshed = useCallback(
    (patch?: { accountId: number; officialUsd: number | null }) => {
      if (patch) {
        setAccountPageStats((current) => ({
          ...current,
          [String(patch.accountId)]: {
            ...current[String(patch.accountId)],
            official_usd: patch.officialUsd ?? undefined,
            official_usd_7d: patch.officialUsd ?? undefined,
            official_usage_synced: true,
          },
        }));
      }
      setPageStatsReloadToken((token) => token + 1);
    },
    [],
  );
  // Codex 视图的统计卡/额度分布/列表/批量操作一律排除 Grok 账号
  // （Grok 账号由顶部切换后的 Grok 页单独统计与管理）。
  const allAccounts = useMemo(
    () =>
      data.accounts.map((account) => {
        const stats = accountPageStats[String(account.id)];
        if (!stats) return account;
        // 行自身已带的字段优先(如 refreshAccountRow 拉回的完整详情比
        // 本页 stats 快照更新),page-stats 只补基础行缺失的部分。
        // 官方结算例外：点进官方统计刷新后必须以最新快照为准，
        // 否则行上的旧额度会挡住这次同步时间点。
        const merged = { ...account };
        if (merged.billed_5h == null && stats.billed_5h != null) merged.billed_5h = stats.billed_5h;
        if (merged.billed_7d == null && stats.billed_7d != null) merged.billed_7d = stats.billed_7d;
        const officialUsd = stats.official_usd ?? stats.official_usd_7d;
        if (officialUsd != null) {
          merged.official_usd = officialUsd;
          merged.official_usd_7d = officialUsd;
        } else if (stats.official_usage_synced) {
          merged.official_usd = undefined;
          merged.official_usd_7d = undefined;
        }
        if (stats.official_usage_synced != null) {
          merged.official_usage_synced = stats.official_usage_synced;
        }
        if (!merged.usage_5h_detail && stats.usage_5h_detail) merged.usage_5h_detail = stats.usage_5h_detail;
        if (!merged.usage_7d_detail && stats.usage_7d_detail) merged.usage_7d_detail = stats.usage_7d_detail;
        if (!merged.usage_today_detail && stats.usage_today_detail) merged.usage_today_detail = stats.usage_today_detail;
        return merged;
      }),
    [data.accounts, accountPageStats],
  );
  const accounts = useMemo(
    () => allAccounts.filter((account) => !account.grok_api),
    [allAccounts],
  );
  const accountPageIDsKey = useMemo(
    () => accounts.map((account) => account.id).join(","),
    [accounts],
  );
  const healthBars = pagedHealthBars;

  useEffect(() => {
    if (!accountPageIDsKey) {
      setPagedHealthBars({});
      return;
    }
    let cancelled = false;
    const ids = accountPageIDsKey.split(",").map(Number);
    void api.getAccountHealthBars(ids)
      .then((response) => {
        if (!cancelled) setPagedHealthBars(response.buckets ?? {});
      })
      .catch((err: unknown) => {
        // 健康条缺失只是降级显示,但失败必须留痕,否则无从排查(issue #493)。
        console.warn("account health bars load failed:", err);
      });
    return () => { cancelled = true; };
  }, [accountPageIDsKey]);

  useEffect(() => {
    if (!accountPageIDsKey) {
      setAccountPageStats({});
      return undefined;
    }
    const controller = new AbortController();
    const ids = accountPageIDsKey.split(",").map(Number);
    void api.getAccountPageStats(ids, controller.signal)
      .then((response) => {
        if (controller.signal.aborted) return;
        setAccountPageStats(response.stats ?? {});
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // 该请求失败时成本/用量列会静默空白,必须留痕(issue #493)。
        console.warn("account page stats load failed:", err);
      });
    return () => controller.abort();
  }, [accountPageIDsKey, pageStatsReloadToken]);
  const officialCostReloadAttemptsRef = useRef(0);
  const missingOfficialCostKey = useMemo(
    () =>
      accounts
        .filter(needsOfficialCostReload)
        .map((account) => account.id)
        .join(","),
    [accounts],
  );
  useEffect(() => {
    officialCostReloadAttemptsRef.current = 0;
  }, [accountPageIDsKey]);
  // 官方结算只存在于 page-stats 的快照字段。后台探针有启动延迟，列表打开时
  // 经常还是空的；后端会给当前页做即时回补，这里按缺字段重拉，直到胶囊出现。
  useEffect(() => {
    if (providerView !== "codex") return undefined;
    if (!missingOfficialCostKey) return undefined;
    if (officialCostReloadAttemptsRef.current >= 6) return undefined;
    const attempt = officialCostReloadAttemptsRef.current;
    const delay = 1500 * 2 ** Math.min(attempt, 5);
    const timer = window.setTimeout(() => {
      if (document.hidden) return;
      officialCostReloadAttemptsRef.current += 1;
      setPageStatsReloadToken((token) => token + 1);
    }, delay);
    return () => window.clearTimeout(timer);
  }, [missingOfficialCostKey, pageStatsReloadToken, providerView]);
  const usageReloadAttemptsRef = useRef<Map<number, number>>(new Map());
  // 测试连接后需要强制刷新用量的账号 id：即使其用量数据已存在（如已显示 100%），
  // 也要在后台探针跑完后重新拉取，确保进度条更新为最新值。
  const forceUsageReloadRef = useRef<Set<number>>(new Set());

  useEffect(() => {
    persistAnalysisVisibility(showAnalysisCharts);
  }, [showAnalysisCharts]);

  useEffect(() => {
    persistEmailDomainVisibility(showEmailDomainTags);
  }, [showEmailDomainTags]);

  useEffect(() => {
    persistAccountVisibleColumns(visibleColumns);
  }, [visibleColumns]);

  useEffect(() => {
    persistAccountViewMode(viewMode);
  }, [viewMode]);

  // 首次升级（用户从未手动设置过页面模式）时，按号池账号数自动判定：
  // 账号数 < 阈值则默认开启自用模式。等账号加载完成后只应用一次；之后用户在
  // 下拉里手动切换才会持久化并一律尊重用户选择。
  useEffect(() => {
    if (pageModeUserSetRef.current) return;
    if (pageModeAutoAppliedRef.current) return;
    if (loading) return;
    pageModeAutoAppliedRef.current = true;
    const personal = data.total < ACCOUNT_PERSONAL_MODE_AUTO_THRESHOLD;
    setPageMode(personal ? "personal" : "pool");
    // 静默换布局曾被当成"排序/成本功能消失"上报(issue #493),
    // 自动切换必须告知用户以及怎么切回去。
    if (personal) {
      showToast(t("accounts.personalModeAutoApplied"));
    }
  }, [loading, data.total, showToast, t]);

  useEffect(() => {
    setGroupFilter((current) => pruneAccountGroupFilter(current, allGroups));
    pruneImportGroupIds(allGroups);
  }, [allGroups, pruneImportGroupIds]);

  useEffect(() => {
    if (providerView !== "codex") return;
    const missingUsageIds = accounts
      .filter(needsUsageReload)
      .map((account) => account.id);
    // 测试连接后被标记强制刷新的账号也要参与重拉，即使其用量数据已存在。
    // 仅保留仍在当前列表中的 id，避免泄漏。
    const accountIdSet = new Set(accounts.map((account) => account.id));
    const forceIds = Array.from(forceUsageReloadRef.current).filter((id) =>
      accountIdSet.has(id),
    );
    forceUsageReloadRef.current = new Set(forceIds);
    const reloadIds = Array.from(new Set([...missingUsageIds, ...forceIds]));
    const reloadIdSet = new Set(reloadIds);
    for (const id of Array.from(usageReloadAttemptsRef.current.keys())) {
      if (!reloadIdSet.has(id)) {
        usageReloadAttemptsRef.current.delete(id);
      }
    }

    const retryIds = reloadIds.filter(
      (id) => (usageReloadAttemptsRef.current.get(id) ?? 0) < 6,
    );
    if (retryIds.length === 0) {
      return;
    }

    let minAttempt = Number.POSITIVE_INFINITY;
    for (const id of retryIds) {
      minAttempt = Math.min(
        minAttempt,
        usageReloadAttemptsRef.current.get(id) ?? 0,
      );
    }
    for (const id of retryIds) {
      usageReloadAttemptsRef.current.set(
        id,
        (usageReloadAttemptsRef.current.get(id) ?? 0) + 1,
      );
    }
    // 强制刷新的账号已安排本轮重拉，移除标记，避免无谓地反复重拉到上限。
    // 若重拉后数据仍未更新且该账号确实缺数据，会由 needsUsageReload 接管继续重试。
    for (const id of forceIds) {
      forceUsageReloadRef.current.delete(id);
    }

    // 指数退避:2.5s/5s/10s/20s/40s/80s。固定 2.5s 会让列表加载后的十几秒里
    // 反复整树重渲染;退避既压低churn,又把慢探针的覆盖窗口拉长到 ~2.5 分钟。
    // 测试连接触发的强制刷新保持首档延迟,用户在等即时反馈。
    const delay =
      forceIds.length > 0 ? 2500 : 2500 * 2 ** Math.min(minAttempt, 5);
    let visibilityHandler: (() => void) | null = null;
    const timer = window.setTimeout(() => {
      // 页面在后台时挂起本轮重拉,回到前台再补,不在不可见标签页里空转打请求。
      if (document.hidden) {
        visibilityHandler = () => {
          if (document.hidden || visibilityHandler === null) return;
          document.removeEventListener("visibilitychange", visibilityHandler);
          visibilityHandler = null;
          void reloadSilently();
        };
        document.addEventListener("visibilitychange", visibilityHandler);
        return;
      }
      void reloadSilently();
    }, delay);

    return () => {
      window.clearTimeout(timer);
      if (visibilityHandler !== null) {
        document.removeEventListener("visibilitychange", visibilityHandler);
        visibilityHandler = null;
      }
    };
  }, [accounts, reloadSilently, providerView]);

  // stats_state 补拉:统计缓存分两层(请求数缓存→列表快照)且都是"先返回旧值
  // 后台重建",从 stale 转 ready 需要连续两三次轮询。上面的静默刷新退避后间隔
  // 最长 80s,不补拉的话「统计刷新中…」会常驻几分钟。这里在非 ready 时用 3s
  // 短间隔追到 ready 为止;带次数上限,防后端统计查询持续失败时退化成常驻轮询。
  const statsStaleRetriesRef = useRef(0);
  useEffect(() => {
    if (loading) return undefined;
    if (data.statsState === "ready") {
      statsStaleRetriesRef.current = 0;
      return undefined;
    }
    const maxStatsRetries = data.disabledSorts.includes("requests") ? 60 : 5;
    if (statsStaleRetriesRef.current >= maxStatsRetries) return undefined;
    const timer = window.setTimeout(() => {
      if (document.hidden) return;
      statsStaleRetriesRef.current += 1;
      void reloadSilently();
    }, 3000);
    return () => window.clearTimeout(timer);
    // 依赖 data 本身(而非 data.statsState):连续两次都返回 stale 时字符串不变,
    // 只有对象身份变化才能把补拉定时器重新拉起来。
  }, [loading, data, reloadSilently]);

  const accountSummary = {
    totalAccounts: data.summary?.total ?? data.total,
    normalAccounts: data.summary?.normal ?? 0,
    schedulingAccounts: data.summary?.active ?? 0,
    rateLimitedAccounts: data.summary?.rate_limited ?? 0,
    rateLimited5hAccounts: data.summary?.rate_limited_5h ?? 0,
    rateLimited7dAccounts: data.summary?.rate_limited_7d ?? 0,
    abnormalAccounts: data.summary?.abnormal ?? 0,
    bannedAccounts: data.summary?.banned ?? 0,
    errorAccounts: data.summary?.error ?? 0,
    unsampledAccounts: data.summary?.unsampled ?? 0,
    disabledAccounts: data.summary?.disabled ?? 0,
    lockedAccounts: data.summary?.locked ?? 0,
    subscriptionAccountsToLockCount: data.summary?.subscription_unlocked ?? 0,
    healthyAccounts: data.summary?.healthy ?? 0,
    warmAccounts: data.summary?.warm ?? 0,
    riskyAccounts: data.summary?.risky ?? 0,
    oauthAccounts: data.summary?.oauth ?? 0,
    apiKeyAccounts: data.summary?.api_key ?? 0,
    selfServicePendingAccounts: data.summary?.self_service_pending ?? 0,
  };
  const {
    totalAccounts,
    normalAccounts,
    schedulingAccounts,
    rateLimitedAccounts,
    rateLimited5hAccounts,
    rateLimited7dAccounts,
    abnormalAccounts,
    bannedAccounts,
    errorAccounts,
    unsampledAccounts,
    disabledAccounts,
    lockedAccounts,
    subscriptionAccountsToLockCount,
    healthyAccounts,
    warmAccounts,
    riskyAccounts,
    oauthAccounts,
    apiKeyAccounts,
    selfServicePendingAccounts,
  } = accountSummary;
  const selfServicePendingCount = Math.max(selfServicePendingAccounts, panelPendingCount);
  const pendingReviewRequested = new URLSearchParams(location.search).get("pending") === "1";
  const showPendingReviewPanel = accountPortalEnabled || selfServicePendingCount > 0 || pendingReviewRequested;
  const scrollToPendingReview = useCallback(() => {
    document.getElementById("pending-review")?.scrollIntoView({ behavior: "smooth", block: "start" });
  }, []);
  const handlePendingCountChange = useCallback((count: number) => {
    setPanelPendingCount(count);
  }, []);

  const allTags = data.facets.tags;
  const emailDomainStats = data.facets.email_domains;
  const currentAccountSelector = useMemo<AccountOperationSelector>(() => ({
    channel: "codex",
    search: debouncedSearchQuery || undefined,
    status: statusFilter === "all" ? undefined : statusFilter,
    plan: planFilter === "all" ? undefined : planFilter,
    auth_kind: authFilter === "all" ? undefined : authFilter,
    tag: tagFilter || undefined,
    email_domain: domainFilter || undefined,
    group_include: groupFilter.include.length > 0 ? groupFilter.include : undefined,
    group_exclude: groupFilter.exclude.length > 0 ? groupFilter.exclude : undefined,
    ungrouped: groupFilter.ungrouped || undefined,
  }), [authFilter, debouncedSearchQuery, domainFilter, groupFilter.exclude, groupFilter.include, groupFilter.ungrouped, planFilter, statusFilter, tagFilter]);

  // 服务端已完成全池筛选、排序和分页。
  const filteredAccounts = accounts;
  const sortedAccounts = accounts;

  const totalPages = Math.max(1, Math.ceil(data.total / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pagedAccounts = accounts;
  const pagedAccountIds = useMemo(
    () => pagedAccounts.map((account) => account.id),
    [pagedAccounts],
  );
  const detailListAccount = useMemo(
    () =>
      detailAccountId == null
        ? null
        : (accounts.find((account) => account.id === detailAccountId) ?? null),
    [accounts, detailAccountId],
  );
  const detailAccount =
    detailAccountData?.id === detailAccountId
      ? detailAccountData
      : detailListAccount;
  const detailNavIndex = useMemo(() => {
    if (detailAccountId == null) return -1;
    return sortedAccounts.findIndex((account) => account.id === detailAccountId);
  }, [detailAccountId, sortedAccounts]);
  const openAccountDetail = useCallback((account: AccountRow) => {
    setDetailAccountData(account);
    setDetailAccountId(account.id);
    setQuickConfigAccount(account);
  }, []);
  const closeAccountDetail = useCallback(() => {
    setDetailAccountId(null);
    setDetailAccountData(null);
    setQuickConfigAccount(null);
  }, []);
  const goDetailPrev = useCallback(() => {
    if (detailNavIndex > 0) {
      const target = sortedAccounts[detailNavIndex - 1] ?? null;
      setDetailAccountData(target);
      setDetailAccountId(target?.id ?? null);
      if (target) setQuickConfigAccount(target);
      return;
    }
    if (currentPage > 1) {
      detailNavigationTargetRef.current = "last";
      setPage(currentPage - 1);
    }
  }, [currentPage, detailNavIndex, sortedAccounts]);
  const goDetailNext = useCallback(() => {
    if (detailNavIndex >= 0 && detailNavIndex < sortedAccounts.length - 1) {
      const target = sortedAccounts[detailNavIndex + 1] ?? null;
      setDetailAccountData(target);
      setDetailAccountId(target?.id ?? null);
      if (target) setQuickConfigAccount(target);
      return;
    }
    if (currentPage < totalPages) {
      detailNavigationTargetRef.current = "first";
      setPage(currentPage + 1);
    }
  }, [currentPage, detailNavIndex, sortedAccounts, totalPages]);

  useEffect(() => {
    if (detailAccountId == null) return undefined;
    const controller = new AbortController();
    void api.getAccount(detailAccountId, controller.signal)
      .then((account) => {
        if (!controller.signal.aborted) setDetailAccountData(account);
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, [detailAccountId, detailListAccount?.enabled, detailListAccount?.locked, detailListAccount?.status, detailListAccount?.updated_at]);

  useEffect(() => {
    const target = detailNavigationTargetRef.current;
    if (!target || data.page !== page || accounts.length === 0) return;
    const account = target === "first" ? accounts[0] : accounts[accounts.length - 1];
    detailNavigationTargetRef.current = null;
    setDetailAccountData(account ?? null);
    setDetailAccountId(account?.id ?? null);
  }, [accounts, data.page, page]);
  // 自用模式（personal）下，主体列表强制走每行 2 列卡片，桌面端也不渲染表格。
  const isPersonalMode = pageMode === "personal";
  const shouldRenderMobileCards =
    isPersonalMode || viewMode === "grid" || !isDesktopLayout;
  const shouldRenderDesktopTable =
    !isPersonalMode && viewMode !== "grid" && isDesktopLayout;
  const pageSelectedCount = useMemo(
    () =>
      pagedAccountIds.reduce(
        (count, id) => count + (selected.has(id) ? 1 : 0),
        0,
      ),
    [pagedAccountIds, selected],
  );
  const allPageSelected =
    pagedAccountIds.length > 0 && pageSelectedCount === pagedAccountIds.length;
  const somePageSelected = pageSelectedCount > 0 && !allPageSelected;

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages);
    }
  }, [page, totalPages]);

  useEffect(() => {
    if (providerView !== "codex") return;
    if (!accounts.some((account) => account.status === "refreshing")) {
      return;
    }

    const timer = window.setTimeout(() => {
      void reloadSilently();
    }, 2000);

    return () => window.clearTimeout(timer);
  }, [accounts, reloadSilently, providerView]);

  useEffect(() => {
    if (selectAllRef.current) {
      selectAllRef.current.indeterminate = somePageSelected;
    }
  }, [somePageSelected]);

  const toggleSelect = useCallback((id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const toggleSelectAll = useCallback(() => {
    if (allPageSelected) {
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of pagedAccountIds) next.delete(id);
        return next;
      });
    } else {
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of pagedAccountIds) next.add(id);
        return next;
      });
    }
  }, [allPageSelected, pagedAccountIds]);

  const handleAdd = async (credential: "rt" | "st" = "rt") => {
    const parsedCustomHeaders = parseCustomHeadersText(addCustomHeadersText);
    if (!parsedCustomHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }
    const payload: AddAccountRequest =
      credential === "st"
        ? {
            ...addForm,
            refresh_token: "",
            allow_duplicate: allowDuplicate,
            custom_headers: applyWorkspaceRouteHeader(
              parsedCustomHeaders.value,
              workspaceRouteID,
            ),
            group_ids: importGroupIds,
          }
        : {
            ...addForm,
            session_token: "",
            allow_duplicate: allowDuplicate,
            custom_headers: applyWorkspaceRouteHeader(
              parsedCustomHeaders.value,
              workspaceRouteID,
            ),
            group_ids: importGroupIds,
          };
    if (
      !payload.refresh_token?.trim() &&
      !payload.session_token?.trim()
    ) {
      return;
    }
    setSubmitting(true);
    try {
      const credentialText =
        credential === "st" ? payload.session_token ?? "" : payload.refresh_token ?? "";
      const credentialCount = credentialText
        .split("\n")
        .map((line) => line.trim())
        .filter(Boolean).length;

      if (credentialCount > 1) {
        const res = await postAdminSSE("/accounts?stream=true", payload);
        setShowAdd(false);
        await readImportSSE(res);
        showToast(t("accounts.addSuccess"));
        setAddForm({ refresh_token: "", session_token: "", proxy_url: "" });
        setAddCustomHeadersText("");
        setWorkspaceRouteID("");
        return;
      }

      await api.addAccount(payload);
      showToast(t("accounts.addSuccess"));
      setShowAdd(false);
      setAddForm({ refresh_token: "", session_token: "", proxy_url: "" });
      setAddCustomHeadersText("");
      setWorkspaceRouteID("");
      void reload();
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const handleAddAT = async () => {
    if (!atForm.access_token.trim()) return;
    const parsedCustomHeaders = parseCustomHeadersText(addCustomHeadersText);
    if (!parsedCustomHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }
    setSubmitting(true);
    try {
      // 始终走流式：即使只添加一个 access_token 也展示进度条，并能反映
      // 身份去重/合并结果（已有账号更新、重复跳过）。
      const res = await postAdminSSE("/accounts/at?stream=true", {
        ...atForm,
        allow_duplicate: allowDuplicate,
        custom_headers: applyWorkspaceRouteHeader(
          parsedCustomHeaders.value,
          workspaceRouteID,
        ),
        group_ids: importGroupIds,
      });
      setShowAdd(false);
      await readImportSSE(res);
      showToast(t("accounts.addSuccess"));
      setAtForm({ access_token: "", proxy_url: "" });
      setAddCustomHeadersText("");
      setWorkspaceRouteID("");
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const addOpenAIModelValues = useCallback((raw: string) => {
    const nextModels = parseModelTokens(raw);
    if (nextModels.length === 0) return;
    setOpenAIForm((form) => ({
      ...form,
      models: mergeModelLists(form.models, nextModels),
    }));
    setOpenAIModelDraft("");
  }, []);

  const removeOpenAIModel = useCallback((model: string) => {
    setOpenAIForm((form) => ({
      ...form,
      models: form.models.filter((item) => item !== model),
    }));
  }, []);

  const clearOpenAIModels = useCallback(() => {
    setOpenAIForm((form) => ({ ...form, models: [] }));
  }, []);

  const addEditOpenAIModelValues = useCallback((raw: string) => {
    const nextModels = parseModelTokens(raw);
    if (nextModels.length === 0) return;
    setEditOpenAIForm((form) => ({
      ...form,
      models: mergeModelLists(form.models, nextModels),
    }));
    setEditOpenAIModelDraft("");
  }, []);

  const removeEditOpenAIModel = useCallback((model: string) => {
    setEditOpenAIForm((form) => ({
      ...form,
      models: form.models.filter((item) => item !== model),
    }));
  }, []);

  const clearEditOpenAIModels = useCallback(() => {
    setEditOpenAIForm((form) => ({ ...form, models: [] }));
  }, []);

  const handleFetchOpenAIModels = async () => {
    if (!openAIForm.api_key.trim()) return;
    setOpenAIModelsLoading(true);
    try {
      const result = await api.fetchOpenAIResponsesModels({
        base_url: openAIForm.base_url,
        api_key: openAIForm.api_key,
        proxy_url: openAIForm.proxy_url,
      });
      const models = result.models ?? [];
      setOpenAIForm((form) => ({
        ...form,
        base_url: result.base_url || form.base_url,
        models,
      }));
      showToast(
        t("accounts.openaiModelsFetchSuccess", { count: models.length }),
      );
    } catch (error) {
      showToast(
        t("accounts.openaiModelsFetchFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setOpenAIModelsLoading(false);
    }
  };

  const handleAddSession = async () => {
    if (!sessionJson.trim() || importing) return;
    setSubmitting(true);
    try {
      // 解析 session JSON，构造为文件导入
      const trimmed = sessionJson.trim();
      const parsed = JSON.parse(trimmed) as Record<string, unknown>;
      // 支持单个对象或数组
      const items = Array.isArray(parsed) ? parsed : [parsed];
      const blob = new Blob([JSON.stringify(items)], { type: "application/json" });
      const file = new File([blob], "session.json", { type: "application/json" });
      await importFiles(
        [file],
        "json",
        sessionProxyUrl,
        addCustomHeadersText,
        workspaceRouteID,
      );
      setShowAdd(false);
      setSessionJson("");
      setAddCustomHeadersText("");
      setWorkspaceRouteID("");
    } catch (error) {
      if (error instanceof SyntaxError) {
        showToast(t("accounts.sessionJsonInvalid"), "error");
      } else {
        showToast(
          t("accounts.addFailed", { error: getErrorMessage(error) }),
          "error",
        );
      }
    } finally {
      setSubmitting(false);
    }
  };
  const handleAddAgentIdentity = async () => {
    if (!agentIdentityJson.trim() || submitting) return;
    setSubmitting(true);
    try {
      const res = await api.importCodexAgentIdentity({
        auth_json: agentIdentityJson,
        proxy_url: agentIdentityProxyUrl.trim() || undefined,
      });
      showToast(
        res.email
          ? t("accounts.agentIdentitySuccess", { email: res.email })
          : t("accounts.addSuccess"),
      );
      setShowAdd(false);
      setAgentIdentityJson("");
      setAgentIdentityProxyUrl("");
      void reload();
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const handleImportAgentIdentityFiles = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    setAgentIdentityFilesImporting(true);
    setAgentIdentityFilesResult(null);
    try {
      const files = await Promise.all(
        Array.from(fileList).map((file) => file.text()),
      );
      const res = await api.batchImportCodexAgentIdentity({
        files,
        proxy_url: agentIdentityProxyUrl.trim() || undefined,
      });
      setAgentIdentityFilesResult(res);
      if (res.imported > 0) {
        showToast(
          t("accounts.agentIdentityFileImportDone", {
            imported: res.imported,
            total: res.total,
          }),
        );
        void reload();
      }
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setAgentIdentityFilesImporting(false);
      if (agentIdentityFileInputRef.current)
        agentIdentityFileInputRef.current.value = "";
    }
  };

  const handleAddOpenAIResponses = async () => {
    const models = openAIForm.models;
    if (!openAIForm.api_key.trim() || models.length === 0) return;
    const parsedCustomHeaders = parseCustomHeadersText(addCustomHeadersText);
    if (!parsedCustomHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }
    const parsedModelMapping = resolveModelMappingValue(
      openAIModelMappingMode,
      openAIModelMappingText,
      openAIModelMappingEntries,
    );
    if (!parsedModelMapping.ok) {
      showToast("单渠道模型映射必须成对填写；JSON 模式必须是字符串对象，源模型不能重复", "error");
      return;
    }
    setSubmitting(true);
    try {
      await api.addOpenAIResponsesAccount({
        ...openAIForm,
        models,
        model_mapping: parsedModelMapping.value,
        custom_headers: parsedCustomHeaders.value,
      });
      showToast(t("accounts.addSuccess"));
      setShowAdd(false);
      setOpenAIForm({
        base_url: "https://api.openai.com",
        api_key: "",
        balance_query_url: "",
        models: [],
        codex_client_metadata_mode: "auto",
        proxy_url: "",
      });
      setOpenAIModelDraft("");
      setOpenAIModelMappingText("");
      setOpenAIModelMappingMode("form");
      setOpenAIModelMappingEntries(emptyModelMappingEntries());
      setAddCustomHeadersText("");
      void reload();
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const handleFetchEditOpenAIModels = async () => {
    if (!editingAccount?.openai_responses_api) return;
    setEditOpenAIModelsLoading(true);
    try {
      const result = await api.fetchOpenAIResponsesModels({
        account_id: editingAccount.id,
        base_url: editOpenAIForm.base_url,
        api_key: editOpenAIForm.api_key ?? "",
        proxy_url: editOpenAIForm.proxy_url,
      });
      const models = result.models ?? [];
      setEditOpenAIForm((form) => ({
        ...form,
        base_url: result.base_url || form.base_url,
        models,
      }));
      showToast(
        t("accounts.openaiModelsFetchSuccess", { count: models.length }),
      );
    } catch (error) {
      showToast(
        t("accounts.openaiModelsFetchFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setEditOpenAIModelsLoading(false);
    }
  };

  const handleSaveOpenAIAccountSettings = async () => {
    if (!editingAccount?.openai_responses_api) return;
    if (!editOpenAIForm.base_url.trim() || editOpenAIForm.models.length === 0) {
      showToast(t("accounts.openaiAccountInvalid"), "error");
      return;
    }
    const parsedCustomHeaders = parseCustomHeadersText(editCustomHeadersText);
    if (!parsedCustomHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }
    const parsedModelMapping = resolveModelMappingValue(
      editOpenAIModelMappingMode,
      editOpenAIModelMappingText,
      editOpenAIModelMappingEntries,
    );
    if (!parsedModelMapping.ok) {
      showToast("单渠道模型映射必须成对填写；JSON 模式必须是字符串对象，源模型不能重复", "error");
      return;
    }
    setEditSubmitting(true);
    try {
      await api.updateOpenAIResponsesAccount(editingAccount.id, {
        ...editOpenAIForm,
        api_key: editOpenAIForm.api_key?.trim() || undefined,
        model_mapping: parsedModelMapping.value,
        custom_headers: parsedCustomHeaders.value,
      });
      invalidateAPIAccountBalance(editingAccount.id);
      showToast(t("accounts.openaiAccountSaveSuccess"));
      await reload();
      closeSchedulerEditor(true);
    } catch (error) {
      showToast(
        t("accounts.openaiAccountSaveFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setEditSubmitting(false);
    }
  };

  const startOAuthSession = async () => {
    const result = await api.generateOAuthURL({ proxy_url: oauthProxyUrl });
    setOauthSession(result);
    setOauthCallbackUrl("");
    setOauthStep("exchange");
    return result;
  };

  const handleOAuthGenerate = async () => {
    setOauthGenerating(true);
    try {
      await startOAuthSession();
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setOauthGenerating(false);
    }
  };

  const handleOAuthRestart = async () => {
    setOauthGenerating(true);
    setOauthSession(null);
    setOauthCallbackUrl("");
    try {
      await startOAuthSession();
    } catch (error) {
      setOauthStep("generate");
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setOauthGenerating(false);
    }
  };

  const handleOAuthCopyLink = async () => {
    if (!oauthSession?.auth_url) return;
    try {
      await copyTextToClipboard(oauthSession.auth_url);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };

  const handleOAuthComplete = async () => {
    if (!oauthSession) return;
    const { code, state } = parseOAuthCallbackParams(oauthCallbackUrl);
    if (!code || !state) {
      showToast(t("accounts.oauthParseError"), "error");
      return;
    }
    setOauthCompleting(true);
    try {
      const result = await api.exchangeOAuthCode({
        session_id: oauthSession.session_id,
        code,
        state,
        name: oauthName.trim() || undefined,
        proxy_url: oauthProxyUrl.trim() || undefined,
      });
      showToast(
        result.email
          ? t("accounts.oauthSuccess", { email: result.email })
          : t("accounts.oauthSuccessNoEmail"),
      );
      setShowAdd(false);
      setAddMethod("oauth");
      setOauthStep("generate");
      setOauthSession(null);
      setOauthCallbackUrl("");
      setOauthName("");
      setAddCustomHeadersText("");
      void reload();
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setOauthCompleting(false);
    }
  };

  const startEditOAuthSession = async () => {
    const result = await api.generateOAuthURL({
      proxy_url: editOAuthProxyUrl.trim() || undefined,
    });
    setEditOAuthSession(result);
    setEditOAuthCallbackUrl("");
    setEditOAuthStep("exchange");
    return result;
  };

  const handleEditOAuthGenerate = async () => {
    setEditOAuthGenerating(true);
    try {
      await startEditOAuthSession();
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditOAuthGenerating(false);
    }
  };

  const handleEditOAuthRestart = async () => {
    setEditOAuthGenerating(true);
    setEditOAuthSession(null);
    setEditOAuthCallbackUrl("");
    try {
      await startEditOAuthSession();
    } catch (error) {
      setEditOAuthStep("generate");
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditOAuthGenerating(false);
    }
  };

  const handleEditOAuthCopyLink = async () => {
    if (!editOAuthSession?.auth_url) return;
    try {
      await copyTextToClipboard(editOAuthSession.auth_url);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };

  const handleUpdateOAuthAccount = async () => {
    if (!editingAccount || !isOAuthAccount(editingAccount)) return;
    if (!editOAuthSession) {
      showToast(t("accounts.oauthGenerateFirst"), "error");
      return;
    }
    const { code, state } = parseOAuthCallbackParams(editOAuthCallbackUrl);
    if (!code || !state) {
      showToast(t("accounts.oauthParseError"), "error");
      return;
    }

    setEditSubmitting(true);
    setEditOAuthUpdating(true);
    try {
      const result = await api.updateOAuthAccount(editingAccount.id, {
        session_id: editOAuthSession.session_id,
        code,
        state,
        proxy_url: editOAuthProxyUrl.trim() || undefined,
      });
      showToast(
        result.email
          ? t("accounts.oauthUpdateSuccess", { email: result.email })
          : t("accounts.oauthUpdateSuccessNoEmail"),
      );
      await reload();
      closeSchedulerEditor(true);
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditOAuthUpdating(false);
      setEditSubmitting(false);
    }
  };

  // maybeOpenInviteGuide 在导入结束后按设置决定是否弹出邀请积分引导。
  // 开关读失败时不弹:宁可少一次引导,也不要在设置不可用时打扰用户。
  const maybeOpenInviteGuide = useCallback(async (ids: number[]) => {
    try {
      const settings = await api.getInviteGuideSettings();
      if (!settings.enabled) return;
    } catch {
      return;
    }
    setInviteGuide({ show: true, ids });
  }, []);

  // readImportSSE 读取单批导入的 SSE 进度流。baseline 是此前已完成批次的累计值,
  // 本批实时进度叠加其上;markDoneOnComplete=false 时(还有后续批次)不置 done,
  // 让进度条跨批保持运行态。本批结束后把本批终值累加进 baseline(原地修改)。
  const readImportSSE = async (
    res: Response,
    baseline?: {
      current: number;
      total: number;
      success: number;
      updated: number;
      duplicate: number;
      failed: number;
    },
    markDoneOnComplete = true,
    // createdIDs 原地收集本次导入新建的账号 ID(complete 事件下发),跨批累加,
    // 供导入结束后拉取邀请收益方案。
    createdIDs?: number[],
    // proxySummary 原地累加代理注册结果(complete 事件下发),跨批累加,
    // 供导入结束后在 toast 里汇报。
    proxySummary?: { imported: number; skipped: number; warnings: string[] },
  ) => {
    const base = baseline ?? {
      current: 0,
      total: 0,
      success: 0,
      updated: 0,
      duplicate: 0,
      failed: 0,
    };
    setImportProgress({
      show: true,
      current: base.current,
      total: base.total,
      success: base.success,
      updated: base.updated,
      duplicate: base.duplicate,
      failed: base.failed,
      done: false,
    });
    const reader = res.body?.getReader();
    if (!reader) {
      setImportProgress((p) => ({ ...p, done: markDoneOnComplete }));
      return;
    }
    const decoder = new TextDecoder();
    let buffer = "";
    // 本批最后一次事件的终值,用于结束后累加进 baseline。
    let last = {
      current: 0,
      total: 0,
      success: 0,
      updated: 0,
      duplicate: 0,
      failed: 0,
    };
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() ?? "";
      for (const line of lines) {
        if (!line.startsWith("data: ")) continue;
        try {
          const event = JSON.parse(line.slice(6)) as {
            type: string;
            current: number;
            total: number;
            success: number;
            updated: number;
            duplicate: number;
            failed: number;
            created_ids?: number[];
            proxies_imported?: number;
            proxies_skipped?: number;
            warning?: string;
          };
          if (event.type === "complete" && createdIDs && event.created_ids?.length) {
            createdIDs.push(...event.created_ids);
          }
          if (event.type === "complete" && proxySummary) {
            proxySummary.imported += event.proxies_imported ?? 0;
            proxySummary.skipped += event.proxies_skipped ?? 0;
            // 同一条告警在多批之间会重复出现，去重后再展示。
            if (event.warning && !proxySummary.warnings.includes(event.warning)) {
              proxySummary.warnings.push(event.warning);
            }
          }
          last = {
            current: event.current ?? 0,
            total: event.total ?? 0,
            success: event.success ?? 0,
            updated: event.updated ?? 0,
            duplicate: event.duplicate ?? 0,
            failed: event.failed ?? 0,
          };
          setImportProgress((p) => ({
            ...p,
            current: base.current + last.current,
            total: base.total + last.total,
            success: base.success + last.success,
            updated: base.updated + last.updated,
            duplicate: base.duplicate + last.duplicate,
            failed: base.failed + last.failed,
            done: markDoneOnComplete && event.type === "complete",
          }));
          // 单批(旧调用方)或分批最后一批完成时刷新列表;中间批不刷新,
          // 避免多批导入反复整表重载。
          if (markDoneOnComplete && event.type === "complete") void reload();
        } catch {
          /* 忽略解析异常 */
        }
      }
    }
    if (baseline) {
      baseline.current += last.current;
      baseline.total += last.total;
      baseline.success += last.success;
      baseline.updated += last.updated;
      baseline.duplicate += last.duplicate;
      baseline.failed += last.failed;
    }
  };

  const importFiles = async (
    files: File[],
    format: "txt" | "json" | "json_at" | "at_txt",
    proxyOverride?: string,
    customHeadersText?: string,
    workspaceOverride?: string,
  ) => {
    const parsedCustomHeaders = parseCustomHeadersText(customHeadersText ?? "");
    if (!parsedCustomHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }

    // 单个文件超过后端导入上限(200MB)无法再切分,直接拒绝并提示。
    const oversized = files.find((f) => f.size > IMPORT_MAX_FILE_BYTES);
    if (oversized) {
      showToast(
        t("accounts.importFileTooLarge", {
          name: oversized.name,
          max: formatMB(IMPORT_MAX_FILE_BYTES),
        }),
        "error",
      );
      return;
    }

    // 按累计大小把文件切成多批,每批控制在 IMPORT_BATCH_MAX_BYTES 内,避免单个
    // 请求体过大触发后端限制;后端导入无状态且按凭据幂等去重,分批完全安全。
    const batches = splitFilesIntoBatches(files, IMPORT_BATCH_MAX_BYTES);

    setImporting(true);
    setImportProgress({
      show: true,
      current: 0,
      total: 0,
      success: 0,
      updated: 0,
      duplicate: 0,
      failed: 0,
      done: false,
    });

    // 跨批累加的基线:每批的 SSE/JSON 进度都是相对本批的,叠加到已完成批次之上。
    const totals = {
      current: 0,
      total: 0,
      success: 0,
      updated: 0,
      duplicate: 0,
      failed: 0,
    };
    // 本次导入(含所有批次)新建的账号 ID,用于导入完成后的邀请收益引导。
    const importedAccountIDs: number[] = [];
    // 本次导入注册进代理池的代理统计,跨批累加后在结束 toast 里汇报。
    const proxySummary = { imported: 0, skipped: 0, warnings: [] as string[] };
    // 只有 JSON 系格式的文件才可能携带代理,后端也只对这两种格式认这个开关。
    const carriesFileProxies =
      importFileProxies && (format === "json" || format === "json_at");

    try {
      for (let i = 0; i < batches.length; i++) {
        const batch = batches[i];
        const formData = new FormData();
        if (format !== "txt") formData.append("format", format);
        const trimmedImportProxy = (proxyOverride ?? importProxyUrl).trim();
        if (trimmedImportProxy) formData.append("proxy_url", trimmedImportProxy);
        if (carriesFileProxies) formData.append("import_proxy", "true");
        const routedHeaders = applyOptionalWorkspaceRouteHeader(
          parsedCustomHeaders.value,
          workspaceOverride,
        );
        if (routedHeaders) {
          formData.append("custom_headers", JSON.stringify(routedHeaders));
        }
        if (allowDuplicate) formData.append("allow_duplicate", "true");
        if (importGroupIds.length > 0) {
          formData.append("group_ids", JSON.stringify(importGroupIds));
        }
        for (const f of batch) formData.append("file", f);

        const res = await fetch("/api/admin/accounts/import", {
          method: "POST",
          body: formData,
          headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
        });

        // 只有最后一批完成后才标记 done,让进度条在多批之间保持运行态。
        const isLastBatch = i === batches.length - 1;
        if (res.headers.get("content-type")?.includes("text/event-stream")) {
          await readImportSSE(
            res,
            totals,
            isLastBatch,
            importedAccountIDs,
            proxySummary,
          );
        } else {
          const data = await res.json();
          if (!res.ok) {
            setImportProgress((p) => ({ ...p, show: false }));
            showToast(
              data.error
                ? t("accounts.importFailedWithReason", { error: data.error })
                : t("accounts.importFailed"),
              "error",
            );
            return;
          }
          totals.current += data.total ?? 0;
          totals.total += data.total ?? 0;
          totals.success += data.success ?? 0;
          totals.updated += data.updated ?? 0;
          totals.duplicate += data.duplicate ?? 0;
          totals.failed += data.failed ?? 0;
          // 纯 Agent Identity 文件走的是这条非流式分支,代理统计同样在这里下发。
          proxySummary.imported += data.proxies_imported ?? 0;
          proxySummary.skipped += data.proxies_skipped ?? 0;
          if (data.warning && !proxySummary.warnings.includes(data.warning)) {
            proxySummary.warnings.push(data.warning);
          }
          setImportProgress({ show: true, ...totals, done: isLastBatch });
          if (isLastBatch) void reload();
        }
      }
      // Toast 只有一个槽位,连续调用会互相顶掉——代理结果和告警必须拼进同一条。
      if (carriesFileProxies) {
        const parts = [
          t("accounts.importCompleted"),
          t("accounts.importProxySummary", {
            imported: proxySummary.imported,
            skipped: proxySummary.skipped,
          }),
          ...proxySummary.warnings,
        ];
        showToast(
          parts.join(" "),
          proxySummary.warnings.length > 0 ? "error" : "success",
        );
      } else {
        showToast(t("accounts.importCompleted"));
      }
      // 引导只在有新建账号时触发;开关状态由后端判定,前端拿到 enabled=false
      // 就不展示,避免把开关语义复制两份。
      if (importedAccountIDs.length > 0) {
        void maybeOpenInviteGuide(importedAccountIDs);
      }
    } catch (error) {
      setImportProgress({
        show: true,
        current: Math.max(totals.current, 1),
        total: Math.max(totals.total, 1),
        success: totals.success,
        updated: totals.updated,
        duplicate: totals.duplicate,
        failed: totals.failed + 1,
        done: true,
      });
      showToast(
        t("accounts.importFailedWithReason", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setImporting(false);
    }
  };

  const handleDragEnter = (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    dragCounter.current++;
    if (dragCounter.current === 1) setDragging(true);
  };

  const handleDragOver = (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
  };

  const handleDragLeave = (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    dragCounter.current--;
    if (dragCounter.current === 0) setDragging(false);
  };

  const readAllEntriesFromDirectory = (
    dirEntry: FileSystemDirectoryEntry,
  ): Promise<File[]> => {
    return new Promise((resolve) => {
      const files: File[] = [];
      const readEntries = (reader: FileSystemDirectoryReader) => {
        reader.readEntries(async (entries) => {
          if (entries.length === 0) {
            resolve(files);
            return;
          }
          for (const entry of entries) {
            if (entry.isFile) {
              const file = await new Promise<File>((res) =>
                (entry as FileSystemFileEntry).file(res),
              );
              files.push(file);
            } else if (entry.isDirectory) {
              const subFiles = await readAllEntriesFromDirectory(
                entry as FileSystemDirectoryEntry,
              );
              files.push(...subFiles);
            }
          }
          readEntries(reader);
        });
      };
      readEntries(dirEntry.createReader());
    });
  };

  const handleDrop = async (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    dragCounter.current = 0;
    setDragging(false);
    if (importing) return;

    // 检测是否拖入了文件夹
    const items = e.dataTransfer.items;
    const hasDirectories =
      items &&
      Array.from(items).some((item) => item.webkitGetAsEntry?.()?.isDirectory);

    if (hasDirectories) {
      const allFiles: File[] = [];
      for (const item of Array.from(items)) {
        const entry = item.webkitGetAsEntry?.();
        if (!entry) continue;
        if (entry.isDirectory) {
          const dirFiles = await readAllEntriesFromDirectory(
            entry as FileSystemDirectoryEntry,
          );
          allFiles.push(...dirFiles);
        } else if (entry.isFile) {
          const file = await new Promise<File>((res) =>
            (entry as FileSystemFileEntry).file(res),
          );
          allFiles.push(file);
        }
      }

      const validFiles = allFiles.filter((f) => {
        const ext = f.name.split(".").pop()?.toLowerCase();
        return (ext === "txt" || ext === "json") && f.size > 0;
      });

      if (validFiles.length === 0) {
        showToast(t("accounts.folderNoValidFiles"), "error");
        return;
      }

      const txtFiles = validFiles.filter(
        (f) => f.name.split(".").pop()?.toLowerCase() === "txt",
      );
      const jsonFiles = validFiles.filter(
        (f) => f.name.split(".").pop()?.toLowerCase() === "json",
      );

      if (jsonFiles.length > 0) {
        await importFiles(jsonFiles, "json");
      }
      if (txtFiles.length > 0) {
        await importFiles(txtFiles, "txt");
      }
      return;
    }

    // 原有的文件拖放逻辑
    const files = Array.from(e.dataTransfer.files).filter((f) => f.size > 0);
    if (files.length === 0) return;

    const txtFiles: File[] = [];
    const jsonFiles: File[] = [];
    for (const f of files) {
      const ext = f.name.split(".").pop()?.toLowerCase();
      if (ext === "txt") txtFiles.push(f);
      else if (ext === "json") jsonFiles.push(f);
      else {
        showToast(t("accounts.unsupportedFileType", { name: f.name }), "error");
        return;
      }
    }

    if (jsonFiles.length > 0) {
      await importFiles(jsonFiles, "json");
    }
    if (txtFiles.length > 0) {
      await importFiles(txtFiles, "txt");
    }
  };

  const handleFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    if (files.length === 0) return;
    if (files.some((file) => !file.name.toLowerCase().endsWith(".txt"))) {
      showToast(t("accounts.selectTxtFile"), "error");
      return;
    }
    setShowImportPicker(false);
    await importFiles(
      files,
      "txt",
      undefined,
      importCustomHeadersText,
      importWorkspaceRouteID.trim() || undefined,
    );
    if (fileInputRef.current) fileInputRef.current.value = "";
  };

  const handleJsonImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files;
    if (!files || files.length === 0) return;
    setShowImportPicker(false);
    await importFiles(
      Array.from(files),
      "json",
      undefined,
      importCustomHeadersText,
      importWorkspaceRouteID.trim() || undefined,
    );
    if (jsonInputRef.current) jsonInputRef.current.value = "";
  };

  const handleJsonAtImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files;
    if (!files || files.length === 0) return;
    setShowImportPicker(false);
    await importFiles(
      Array.from(files),
      "json_at",
      undefined,
      importCustomHeadersText,
      importWorkspaceRouteID.trim() || undefined,
    );
    if (jsonAtInputRef.current) jsonAtInputRef.current.value = "";
  };

  const handleAtFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    if (files.length === 0) return;
    if (files.some((file) => !file.name.toLowerCase().endsWith(".txt"))) {
      showToast(t("accounts.selectTxtFile"), "error");
      return;
    }
    setShowImportPicker(false);
    await importFiles(
      files,
      "at_txt",
      undefined,
      importCustomHeadersText,
      importWorkspaceRouteID.trim() || undefined,
    );
    if (atFileInputRef.current) atFileInputRef.current.value = "";
  };

  const handleFolderImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files;
    if (!files || files.length === 0) return;
    setShowImportPicker(false);

    const validFiles = Array.from(files).filter((f) => {
      const ext = f.name.split(".").pop()?.toLowerCase();
      return (ext === "txt" || ext === "json") && f.size > 0;
    });

    if (validFiles.length === 0) {
      showToast(t("accounts.folderNoValidFiles"), "error");
      if (folderInputRef.current) folderInputRef.current.value = "";
      return;
    }

    const txtFiles = validFiles.filter(
      (f) => f.name.split(".").pop()?.toLowerCase() === "txt",
    );
    const jsonFiles = validFiles.filter(
      (f) => f.name.split(".").pop()?.toLowerCase() === "json",
    );

    if (jsonFiles.length > 0) {
      await importFiles(
        jsonFiles,
        "json",
        undefined,
        importCustomHeadersText,
        importWorkspaceRouteID.trim() || undefined,
      );
    }
    if (txtFiles.length > 0) {
      await importFiles(
        txtFiles,
        "txt",
        undefined,
        importCustomHeadersText,
        importWorkspaceRouteID.trim() || undefined,
      );
    }

    if (folderInputRef.current) folderInputRef.current.value = "";
  };

  const handlePasteImport = async () => {
    if (!pasteImportText.trim() || importing) return;
    const trimmed = pasteImportText.trim();
    let items: unknown[];
    try {
      const parsed = JSON.parse(trimmed);
      items = Array.isArray(parsed) ? parsed : [parsed];
    } catch {
      showToast(t("accounts.sessionJsonInvalid"), "error");
      return;
    }
    const blob = new Blob([JSON.stringify(items)], { type: "application/json" });
    const file = new File([blob], "paste.json", { type: "application/json" });
    await importFiles(
      [file],
      "json",
      undefined,
      importCustomHeadersText,
      importWorkspaceRouteID.trim() || undefined,
    );
    setShowPasteImport(false);
    setPasteImportText("");
  };

  const handleExport = async (
    format: "json" | "txt",
    scope: "healthy" | "selected",
  ) => {
    setExporting(true);
    setShowExportPicker(false);
    try {
      // Codex 账号页只导出 codex 渠道:不带 channel 时后端会把 Grok 账号一并
      // 打进 codex 命名的导出文件(Grok 页有专属导出入口)。
      const params: {
        filter: "healthy" | "all";
        ids?: number[];
        channel: "codex";
        includeProxy?: boolean;
      } = {
        filter: scope === "healthy" ? "healthy" : "all",
        channel: "codex",
        // TXT 只导出 refresh_token,带上代理也写不进去,徒然让响应多带一份明文口令。
        includeProxy: format === "json" && exportIncludeProxy,
      };
      if (scope === "selected") {
        params.ids = Array.from(selected);
        params.filter = "all";
      }
      const data = await api.exportAccounts(params);
      if (data.length === 0) {
        showToast(t("accounts.exportNoAccounts"), "error");
        return;
      }
      const ts = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
      if (format === "json") {
        const blob = new Blob([JSON.stringify(data, null, 2)], {
          type: "application/json",
        });
        downloadBlob(blob, `codex2api-codex-${ts}-${data.length}.json`);
      } else {
        const text = data.map((e) => e.refresh_token).join("\n");
        const blob = new Blob([text], { type: "text/plain" });
        downloadBlob(blob, `codex2api-codex-rt-${ts}-${data.length}.txt`);
      }
      showToast(t("accounts.exportSuccess", { count: data.length }));
    } catch (error) {
      showToast(
        `${t("accounts.exportFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setExporting(false);
    }
  };

  const handleGenerateAuthJSON = async (account: AccountRow) => {
    setAuthJsonExportingIds((prev) => new Set(prev).add(account.id));
    try {
      const blob = await api.downloadAccountAuthJSON(account.id);
      const json = formatJSONText(await blob.text());
      setAuthJsonModal({ account, json });
      showToast(t("accounts.authJsonGenerated"));
    } catch (error) {
      showToast(
        t("accounts.authJsonFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setAuthJsonExportingIds((prev) => {
        const next = new Set(prev);
        next.delete(account.id);
        return next;
      });
    }
  };

  const handleCopyAuthJSON = async () => {
    if (!authJsonModal) return;
    try {
      await copyTextToClipboard(authJsonModal.json);
      showToast(t("accounts.authJsonCopied"));
    } catch (error) {
      showToast(
        t("accounts.authJsonCopyFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleExportAuthJSON = () => {
    if (!authJsonModal) return;
    const blob = new Blob([`${authJsonModal.json}\n`], {
      type: "application/json",
    });
    downloadBlob(blob, "auth.json");
    showToast(t("accounts.authJsonExported"));
  };

  const handleMigrate = async () => {
    setMigrating(true);
    setShowMigrate(false);
    try {
      const res = await fetch("/api/admin/accounts/migrate", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          ...(getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {}),
        },
        body: JSON.stringify({
          url: migrateUrl.trim(),
          admin_key: migrateKey.trim(),
        }),
      });
      if (res.headers.get("content-type")?.includes("text/event-stream")) {
        await readImportSSE(res);
      } else {
        const data = await res.json();
        if (!res.ok) {
          showToast(
            data.error
              ? `${t("accounts.migrateFailed")}: ${data.error}`
              : t("accounts.migrateFailed"),
            "error",
          );
        } else {
          showToast(
            t("accounts.migrateSuccess", {
              imported: data.imported ?? 0,
              duplicate: data.duplicate ?? 0,
              failed: data.failed ?? 0,
            }),
          );
          void reload();
        }
      }
    } catch (error) {
      showToast(
        `${t("accounts.migrateFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setMigrating(false);
      setMigrateUrl("");
      setMigrateKey("");
    }
  };

  const handleDelete = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t("accounts.deleteTitle"),
      description: t("accounts.deleteDesc", {
        account: account.email || `ID ${account.id}`,
      }),
      confirmText: t("accounts.deleteConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    try {
      await api.deleteAccount(account.id);
      showToast(t("accounts.deleted"));
      refreshAfterAccountRemoval([account.id]);
    } catch (error) {
      showToast(
        t("accounts.deleteFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleRefresh = async (account: AccountRow) => {
    setRefreshingIds((prev) => new Set(prev).add(account.id));
    try {
      const result = await api.refreshAccount(account.id);
      showToast(result.message || t("accounts.refreshRequested"));
      await refreshAccountRow(account.id);
    } catch (error) {
      showToast(
        t("accounts.refreshFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setRefreshingIds((prev) => {
        const next = new Set(prev);
        next.delete(account.id);
        return next;
      });
    }
  };

  const handleToggleLock = async (account: AccountRow) => {
    const newLocked = !account.locked;
    try {
      await api.toggleAccountLock(account.id, newLocked);
      showToast(
        newLocked ? t("accounts.lockSuccess") : t("accounts.unlockSuccess"),
      );
      await refreshAccountRow(account.id);
    } catch (error) {
      showToast(
        t("accounts.lockFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleLockSubscriptionAccounts = async () => {
    if (subscriptionAccountsToLockCount === 0) {
      showToast(t("accounts.noSubscriptionAccountsToLock"));
      return;
    }

    setBatchLoading(true);
    setLockingSubscriptionAccounts(true);
    try {
      const result = await api.batchUpdateAccounts({
        selector: { channel: "codex", subscription_unlocked: true },
        locked: true,
      });
      showToast(
        t("accounts.lockSubscriptionAccountsDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      void reload();
    } catch (error) {
      showToast(
        t("accounts.lockFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
      setLockingSubscriptionAccounts(false);
    }
  };

  const handleToggleEnabled = async (account: AccountRow) => {
    const nextEnabled = account.enabled === false;
    try {
      await api.toggleAccountEnabled(account.id, nextEnabled);
      showToast(
        nextEnabled
          ? t("accounts.enableSuccess")
          : t("accounts.disableSuccess"),
      );
      await refreshAccountRow(account.id);
    } catch (error) {
      showToast(
        t("accounts.enableFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  // 通过审核:启用自助提交的账号(enabled=true)。
  const handleApprovePending = async (account: AccountRow) => {
    try {
      await api.toggleAccountEnabled(account.id, true);
      showToast(t("accounts.pendingReview.approved"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.enableFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  // 拒绝:删除自助提交的账号。
  const handleRejectPending = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t("accounts.pendingReview.rejectTitle"),
      description: t("accounts.pendingReview.rejectDesc", {
        account: account.email || `ID ${account.id}`,
      }),
      confirmText: t("accounts.pendingReview.rejectConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    try {
      await api.deleteAccount(account.id);
      showToast(t("accounts.pendingReview.rejected"));
      refreshAfterAccountRemoval([account.id]);
    } catch (error) {
      showToast(
        t("accounts.deleteFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  // 保存账号备注(PATCH /accounts/:id/note)。
  const handleSaveNote = async (account: AccountRow, note: string) => {
    await api.updateAccountNote(account.id, note);
    showToast(t("accounts.pendingReview.noteSaved"));
    void reloadSilently();
  };

  const handleBatchDelete = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    const confirmed = await confirm({
      title: t("accounts.batchDeleteTitle"),
      description: t("accounts.batchDeleteDesc", { count: ids.length }),
      confirmText: t("accounts.deleteConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchLoading(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/batch-delete?stream=true",
        { ids },
        t("accounts.batchDeleteProgressTitle"),
      );
      const success = result?.success ?? result?.deleted ?? 0;
      const fail = result?.failed ?? 0;
      showToast(t("accounts.batchDeleteDone", { success, fail }));
      setSelected(new Set());
      void reloadSilently();
    } catch (error) {
      showToast(
        t("accounts.batchDeleteFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
    }
  };

  const handleBatchRefresh = async (ids?: number[], allRefreshable = false) => {
    const targetIds = ids ?? (allRefreshable ? [] : Array.from(selected));
    if (!allRefreshable && targetIds.length === 0) return;
    setBatchLoading(true);
    setBatchRefreshing(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/batch-refresh?stream=true",
        allRefreshable
          ? { selector: { channel: "codex", refreshable_only: true } }
          : { ids: targetIds },
        t("accounts.batchRefreshProgressTitle"),
      );
      const success = result?.success ?? 0;
      const fail = result?.failed ?? 0;
      showToast(t("accounts.batchRefreshDone", { success, fail }));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.batchRefreshFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
      setBatchRefreshing(false);
    }
  };

  const handleBatchLock = async (locked: boolean) => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchLoading(true);
    try {
      const result = await api.batchUpdateAccounts({ ids, locked });
      showToast(
        t(locked ? "accounts.batchLockDone" : "accounts.batchUnlockDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      setSelected(new Set());
      void reload();
    } catch (error) {
      showToast(
        t("accounts.lockFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
    }
  };

  const handleBatchEnabled = async (enabled: boolean) => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchLoading(true);
    try {
      const result = await api.batchUpdateAccounts({ ids, enabled });
      showToast(
        t(enabled ? "accounts.batchEnableDone" : "accounts.batchDisableDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      setSelected(new Set());
      void reload();
    } catch (error) {
      showToast(
        t("accounts.enableFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
    }
  };

  const handleResetStatus = async (account: AccountRow) => {
    try {
      await api.resetAccountStatus(account.id);
      showToast(
        account.status === "overload_paused"
          ? t("accounts.overloadRecoverSuccess")
          : t("accounts.resetStatusSuccess"),
      );
      await refreshAccountRow(account.id);
    } catch (error) {
      showToast(
        t("accounts.resetStatusFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleSaveModelCooldownPolicy = async (
    account: AccountRow,
    data: {
      mode: "off" | "fixed" | "adaptive" | null;
      seconds: number | null;
      backoff_enabled: boolean | null;
    },
  ) => {
    try {
      await api.updateAccountModelCooldownPolicy(account.id, data);
      showToast(t("accounts.modelCooldownPolicySaved"));
      void reloadSilently();
    } catch (error) {
      showToast(
        t("accounts.modelCooldownPolicySaveFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    }
  };

  const handleClearModelCooldown = async (
    account: AccountRow,
    model: string,
  ) => {
    try {
      await api.clearAccountModelCooldown(account.id, model);
      showToast(t("accounts.modelCooldownCleared", { model }));
      void reloadSilently();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  };

  const handleClearAllModelCooldowns = async (account: AccountRow) => {
    try {
      const result = await api.clearAllAccountModelCooldowns(account.id);
      showToast(
        t("accounts.allModelCooldownsCleared", { count: result.cleared }),
      );
      void reloadSilently();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  };

  // 主动重置额度：消耗 1 次「主动重置次数」立即重置该账号额度（带二次确认）。
  const handleResetCredits = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t("accounts.resetCreditsButton"),
      description: t("accounts.resetCreditsConfirmMessage"),
      confirmText: t("accounts.resetCreditsConfirmButton"),
      tone: "warning",
    });
    if (!confirmed) return;
    try {
      await api.resetCredits(account.id);
      showToast(t("accounts.resetCreditsSuccess"));
      void reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  };

  const handleBatchResetStatus = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchLoading(true);
    try {
      const result = await api.batchResetStatus(ids);
      showToast(
        t("accounts.batchResetStatusDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      setSelected(new Set());
      void reload();
    } catch (error) {
      showToast(
        t("accounts.resetStatusFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
    }
  };

  const openBatchMetaEditor = () => {
    setBatchMetaMode("all");
    setBatchUpdateTags(false);
    setBatchTags([]);
    setBatchUpdateGroups(false);
    setBatchGroupIds([]);
    setBatchUpdateScoreBias(false);
    setBatchScoreBiasInput("");
    setBatchUpdateBaseConcurrency(false);
    setBatchBaseConcurrencyInput("");
    setBatchUpdateSchedulerPriority(false);
    setBatchSchedulerPriorityInput("");
    setBatchUpdateCodexFingerprintMode(false);
    setBatchCodexFingerprintMode("off");
    setShowBatchMetaEditor(true);
  };

  const openBatchGroupEditor = () => {
    setBatchMetaMode("groups");
    setBatchUpdateTags(false);
    setBatchTags([]);
    setBatchUpdateGroups(true);
    setBatchGroupIds([]);
    setBatchUpdateScoreBias(false);
    setBatchScoreBiasInput("");
    setBatchUpdateBaseConcurrency(false);
    setBatchBaseConcurrencyInput("");
    setBatchUpdateSchedulerPriority(false);
    setBatchSchedulerPriorityInput("");
    setBatchUpdateCodexFingerprintMode(false);
    setBatchCodexFingerprintMode("off");
    setShowBatchMetaEditor(true);
  };

  const openQuickGroupEditor = (account: AccountRow) => {
    setQuickGroupAccount(account);
    setQuickGroupIds([...(account.group_ids ?? [])]);
  };

  const handleQuickGroupSave = async () => {
    if (!quickGroupAccount) return;
    setQuickGroupSubmitting(true);
    try {
      await api.updateAccountScheduler(quickGroupAccount.id, {
        group_ids: quickGroupIds,
      });
      showToast(t("accounts.groupQuickSaveDone"));
      await Promise.all([reload(), reloadGroups()]);
      setQuickGroupAccount(null);
      setQuickGroupIds([]);
    } catch (error) {
      showToast(
        t("accounts.groupQuickSaveFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setQuickGroupSubmitting(false);
    }
  };

  const populateModelsEditor = (account: AccountRow) => {
    setModelsAccount(account);
    setModelsDraft([...(account.models ?? [])]);
    setModelsInputDraft("");
    setProbeBoard([]);
  };

  const openModelsEditor = (account: AccountRow) => {
    void loadAccountDetail(account)
      .then(populateModelsEditor)
      .catch((error) => showToast(getErrorMessage(error), "error"));
  };

  const openTestingAccount = (account: AccountRow) => {
    void loadAccountDetail(account)
      .then(setTestingAccount)
      .catch((error) => showToast(getErrorMessage(error), "error"));
  };

  const closeModelsEditor = () => {
    if (modelsSaving || modelsSyncing || modelsProbing) return;
    setModelsAccount(null);
    setModelsDraft([]);
    setModelsInputDraft("");
    setProbeBoard([]);
  };

  const addModelsDraftValues = (raw: string) => {
    const next = parseModelTokens(raw);
    if (next.length === 0) return;
    setModelsDraft((current) => mergeModelLists(current, next));
    setModelsInputDraft("");
  };

  const removeModelsDraftValue = (model: string) => {
    setModelsDraft((current) => current.filter((item) => item !== model));
  };

  const clearModelsDraft = () => {
    setModelsDraft([]);
  };

  const handleSyncModelsUpstream = async () => {
    if (!modelsAccount) return;
    setModelsSyncing(true);
    try {
      const result = await api.syncAccountModelsUpstream(modelsAccount.id);
      const fetched = result.models ?? [];
      setModelsDraft((current) => mergeModelLists(current, fetched));
      showToast(
        t("accounts.supportedModelsSyncDone", { count: fetched.length }),
      );
    } catch (error) {
      showToast(
        t("accounts.supportedModelsSyncFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setModelsSyncing(false);
    }
  };

  const handleProbeModels = async () => {
    if (!modelsAccount) return;
    setModelsProbing(true);
    setProbeBoard([]);
    let available: string[] = [];
    try {
      const res = await fetch(
        `/api/admin/accounts/${modelsAccount.id}/models/probe?stream=true`,
        {
          method: "POST",
          headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
        },
      );
      if (!res.ok) {
        const data = await res.json().catch(() => null);
        throw new Error(data?.error || `HTTP ${res.status}`);
      }
      const reader = res.body?.getReader();
      if (!reader) throw new Error("no stream");
      const decoder = new TextDecoder();
      let buffer = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";
        for (const line of lines) {
          if (!line.startsWith("data: ")) continue;
          try {
            const ev = JSON.parse(line.slice(6)) as {
              type: string;
              models?: string[];
              model?: string;
              outcome?: string;
              detail?: string;
              available?: string[];
            };
            if (ev.type === "start") {
              setProbeBoard(
                (ev.models ?? []).map((m) => ({
                  model: m,
                  status: "pending" as ModelProbeStatus,
                })),
              );
            } else if (ev.type === "testing") {
              setProbeBoard((board) =>
                board.map((it) =>
                  it.model === ev.model ? { ...it, status: "testing" } : it,
                ),
              );
            } else if (ev.type === "result") {
              setProbeBoard((board) =>
                board.map((it) =>
                  it.model === ev.model
                    ? {
                        ...it,
                        status: (ev.outcome as ModelProbeStatus) ?? "error",
                        detail: ev.detail,
                      }
                    : it,
                ),
              );
            } else if (ev.type === "done") {
              available = ev.available ?? [];
            }
          } catch {
            /* 忽略解析异常 */
          }
        }
      }
      if (available.length === 0) {
        showToast(t("accounts.supportedModelsProbeNone"), "error");
      } else {
        setModelsDraft((current) => mergeModelLists(current, available));
        showToast(
          t("accounts.supportedModelsProbeDone", { count: available.length }),
        );
      }
    } catch (error) {
      showToast(
        t("accounts.supportedModelsProbeFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setModelsProbing(false);
    }
  };

  const handleSaveModels = async () => {
    if (!modelsAccount) return;
    setModelsSaving(true);
    try {
      await api.updateAccountModels(modelsAccount.id, modelsDraft);
      showToast(t("accounts.supportedModelsSaveDone"));
      void reloadSilently();
      setModelsAccount(null);
      setModelsDraft([]);
      setModelsInputDraft("");
    } catch (error) {
      showToast(
        t("accounts.supportedModelsSaveFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setModelsSaving(false);
    }
  };

  const batchScoreBiasTrimmed = batchScoreBiasInput.trim();
  const batchScoreBiasValue = batchScoreBiasTrimmed
    ? parseIntegerInput(batchScoreBiasTrimmed)
    : null;
  const batchScoreBiasInvalid =
    batchUpdateScoreBias &&
    batchScoreBiasTrimmed !== "" &&
    (batchScoreBiasValue === null ||
      batchScoreBiasValue < -200 ||
      batchScoreBiasValue > 200);
  const batchBaseConcurrencyTrimmed = batchBaseConcurrencyInput.trim();
  const batchBaseConcurrencyValue = batchBaseConcurrencyTrimmed
    ? parseIntegerInput(batchBaseConcurrencyTrimmed)
    : null;
  const batchBaseConcurrencyInvalid =
    batchUpdateBaseConcurrency &&
    batchBaseConcurrencyTrimmed !== "" &&
    (batchBaseConcurrencyValue === null || batchBaseConcurrencyValue < 1);
  const batchSchedulerPriorityInvalid =
    batchUpdateSchedulerPriority &&
    isSchedulerPriorityInputInvalid(batchSchedulerPriorityInput);
  const batchMetaHasUpdates =
    batchUpdateTags ||
    batchUpdateGroups ||
    batchUpdateScoreBias ||
    batchUpdateBaseConcurrency ||
    batchUpdateSchedulerPriority ||
    batchUpdateCodexFingerprintMode;
  const batchMetaInvalid =
    batchScoreBiasInvalid ||
    batchBaseConcurrencyInvalid ||
    batchSchedulerPriorityInvalid;

  const handleBatchSaveMeta = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0 || !batchMetaHasUpdates) return;
    if (batchMetaInvalid) {
      showToast(t("accounts.schedulerInvalidInput"), "error");
      return;
    }
    setBatchMetaSubmitting(true);
    try {
      const result = await api.batchUpdateAccounts(
        buildBatchMetadataUpdate({
          ids,
          updateTags: batchUpdateTags,
          tags: batchTags,
          updateGroups: batchUpdateGroups,
          groupIds: batchGroupIds,
          updateScoreBias: batchUpdateScoreBias,
          scoreBias: batchScoreBiasValue,
          updateBaseConcurrency: batchUpdateBaseConcurrency,
          baseConcurrency: batchBaseConcurrencyValue,
          updateSchedulerPriority: batchUpdateSchedulerPriority,
          schedulerPriority: schedulerPriorityInputToValue(
            batchSchedulerPriorityInput,
          ),
          updateCodexFingerprintMode: batchUpdateCodexFingerprintMode,
          codexFingerprintMode: batchCodexFingerprintMode,
        }),
      );
      showToast(
        t("accounts.batchMetaDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      setShowBatchMetaEditor(false);
      await Promise.all([reload(), reloadGroups()]);
    } catch (error) {
      showToast(
        t("accounts.batchMetaFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchMetaSubmitting(false);
    }
  };

  const openBatchQuotaAutoPauseEditor = () => {
    setBatchAutoPause5hThresholdInput("");
    setBatchAutoPause7dThresholdInput("");
    setBatchAutoPause5hDisabled(false);
    setBatchAutoPause7dDisabled(false);
    setShowBatchQuotaAutoPauseEditor(true);
  };

  const handleBatchSaveQuotaAutoPause = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    if (
      isPercentThresholdInputInvalid(batchAutoPause5hThresholdInput) ||
      isPercentThresholdInputInvalid(batchAutoPause7dThresholdInput)
    ) {
      showToast(t("accounts.autoPauseThresholdRange"), "error");
      return;
    }
    setBatchQuotaAutoPauseSubmitting(true);
    try {
      const payload = {
        auto_pause_5h_threshold: percentThresholdInputToRatio(
          batchAutoPause5hThresholdInput,
        ),
        auto_pause_7d_threshold: percentThresholdInputToRatio(
          batchAutoPause7dThresholdInput,
        ),
        auto_pause_5h_disabled: batchAutoPause5hDisabled,
        auto_pause_7d_disabled: batchAutoPause7dDisabled,
      };
      const result = await api.batchUpdateAccounts({ ids, ...payload });
      showToast(
        t("accounts.batchAutoPauseDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      setShowBatchQuotaAutoPauseEditor(false);
      await reload();
    } catch (error) {
      showToast(
        t("accounts.batchAutoPauseFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchQuotaAutoPauseSubmitting(false);
    }
  };

  const handleBatchTest = async (ids?: number[]) => {
    if (ids && ids.length === 0) return;
    if (!ids && data.total === 0) return;
    setBatchTesting(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/batch-test?stream=true",
        ids ? { ids } : { selector: currentAccountSelector },
        t("accounts.batchTestProgressTitle"),
      );
      showToast(
        t("accounts.batchTestDone", {
          success: result?.success ?? 0,
          banned: result?.banned ?? 0,
          rateLimited: result?.rate_limited ?? 0,
          failed: result?.failed ?? 0,
        }),
      );
      await reloadSilently();
    } catch (error) {
      showToast(
        t("accounts.batchTestFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchTesting(false);
    }
  };

  const handleCleanBanned = async () => {
    const confirmed = await confirm({
      title: t("accounts.cleanBannedTitle"),
      description: t("accounts.cleanBannedDesc"),
      confirmText: t("accounts.cleanConfirm"),
      tone: "warning",
    });
    if (!confirmed) return;
    setCleaningBanned(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/clean-banned?stream=true",
        undefined,
        t("accounts.cleanBannedProgressTitle"),
      );
      showToast(
        t("accounts.cleanBannedSuccessCount", {
          count: result?.deleted ?? result?.success ?? 0,
        }),
      );
      await reloadSilently();
    } catch (error) {
      showToast(
        t("accounts.cleanBannedFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningBanned(false);
    }
  };

  const handleCleanRateLimited = async () => {
    const confirmed = await confirm({
      title: t("accounts.cleanRateLimitedTitle"),
      description: t("accounts.cleanRateLimitedDesc"),
      confirmText: t("accounts.cleanConfirm"),
      tone: "warning",
    });
    if (!confirmed) return;
    setCleaningRateLimited(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/clean-rate-limited?stream=true",
        undefined,
        t("accounts.cleanRateLimitedProgressTitle"),
      );
      showToast(
        t("accounts.cleanRateLimitedSuccessCount", {
          count: result?.deleted ?? result?.success ?? 0,
        }),
      );
      await reloadSilently();
    } catch (error) {
      showToast(
        t("accounts.cleanRateLimitedFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningRateLimited(false);
    }
  };

  const handleCleanError = async () => {
    const confirmed = await confirm({
      title: t("accounts.cleanErrorTitle"),
      description: t("accounts.cleanErrorDesc"),
      confirmText: t("accounts.cleanConfirm"),
      tone: "warning",
    });
    if (!confirmed) return;
    setCleaningError(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/clean-error?stream=true",
        undefined,
        t("accounts.cleanErrorProgressTitle"),
      );
      showToast(
        t("accounts.cleanErrorSuccessCount", {
          count: result?.deleted ?? result?.success ?? 0,
        }),
      );
      await reloadSilently();
    } catch (error) {
      showToast(
        t("accounts.cleanErrorFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningError(false);
    }
  };

  const populateSchedulerEditor = (account: AccountRow) => {
    setEditingAccount(account);
    setEditTab("scheduler");
    setScoreMode(
      account.score_bias_override === null ||
        account.score_bias_override === undefined
        ? "default"
        : "custom",
    );
    setScoreInput(
      account.score_bias_override === null ||
        account.score_bias_override === undefined
        ? ""
        : String(account.score_bias_override),
    );
    setConcurrencyMode(
      account.base_concurrency_override === null ||
        account.base_concurrency_override === undefined
        ? "default"
        : "custom",
    );
    setConcurrencyInput(
      account.base_concurrency_override === null ||
        account.base_concurrency_override === undefined
        ? ""
        : String(account.base_concurrency_override),
    );
    setSkipWarmTier(account.skip_warm_tier ?? false);
    setEditAutoPause5hThresholdInput(
      formatQuotaAutoPausePercentInput(account.auto_pause_5h_threshold),
    );
    setEditAutoPause7dThresholdInput(
      formatQuotaAutoPausePercentInput(account.auto_pause_7d_threshold),
    );
    setEditAutoPause5hDisabled(account.auto_pause_5h_disabled ?? false);
    setEditAutoPause7dDisabled(account.auto_pause_7d_disabled ?? false);
    setEditIgnoreUsageLimitStatusMode(
      account.ignore_usage_limit_status_override === true
        ? "enabled"
        : account.ignore_usage_limit_status_override === false
          ? "disabled"
          : "inherit",
    );
    setEditDispatchCountLimitInput(
      formatDispatchCountLimitInput(account.dispatch_count_limit),
    );
    setEditSchedulerPriorityInput(
      formatSchedulerPriorityInput(account.scheduler_priority),
    );
    setAllowedAPIKeySelection(
      filterExistingAPIKeyIDs(account.allowed_api_key_ids ?? [], apiKeys),
    );
    setEditProxyUrl(account.proxy_url ?? "");
    setEditCustomHeadersText(formatCustomHeadersText(account.custom_headers));
    setEditCodexFingerprintMode(account.codex_fingerprint_mode ?? "off");
    setEditTags(account.tags ?? []);
    setEditGroupIds(account.group_ids ?? []);
    setEditOpenAIForm({
      name: account.name ?? "",
      base_url: account.base_url || "https://api.openai.com",
      api_key: "",
      balance_query_url: account.balance_query_url ?? "",
      models: account.models ?? [],
      codex_client_metadata_mode:
        account.codex_client_metadata_mode ?? "auto",
      proxy_url: account.proxy_url ?? "",
    });
    setEditOpenAIModelDraft("");
    setEditOpenAIModelMappingText(account.model_mapping ?? "");
    setEditOpenAIModelMappingMode("form");
    {
      const parsedMapping = parseModelMappingEntries(account.model_mapping ?? "");
      setEditOpenAIModelMappingEntries(
        parsedMapping.ok ? parsedMapping.entries : emptyModelMappingEntries(),
      );
    }
    setEditOAuthStep("generate");
    setEditOAuthSession(null);
    setEditOAuthProxyUrl(account.proxy_url ?? "");
    setEditOAuthCallbackUrl("");
    setEditOAuthGenerating(false);
    setEditOAuthUpdating(false);
  };

  const openSchedulerEditor = (account: AccountRow) => {
    void loadAccountDetail(account)
      .then(populateSchedulerEditor)
      .catch((error) => showToast(getErrorMessage(error), "error"));
  };

  const closeSchedulerEditor = (force = false) => {
    if (editSubmitting && !force) return;
    setEditingAccount(null);
    setEditTab("scheduler");
    setScoreMode("default");
    setScoreInput("");
    setConcurrencyMode("default");
    setConcurrencyInput("");
    setSkipWarmTier(false);
    setEditAutoPause5hThresholdInput("");
    setEditAutoPause7dThresholdInput("");
    setEditAutoPause5hDisabled(false);
    setEditAutoPause7dDisabled(false);
    setEditIgnoreUsageLimitStatusMode("inherit");
    setEditDispatchCountLimitInput("");
    setEditSchedulerPriorityInput("");
    setAllowedAPIKeySelection([]);
    setEditProxyUrl("");
    setEditCustomHeadersText("");
    setEditCodexFingerprintMode("off");
    setEditTags([]);
    setEditGroupIds([]);
    setEditOpenAIForm({
      name: "",
      base_url: "https://api.openai.com",
      api_key: "",
      balance_query_url: "",
      models: [],
      codex_client_metadata_mode: "auto",
      proxy_url: "",
    });
    setEditOpenAIModelDraft("");
    setEditOpenAIModelMappingText("");
    setEditOpenAIModelMappingMode("form");
    setEditOpenAIModelMappingEntries(emptyModelMappingEntries());
    setEditOAuthStep("generate");
    setEditOAuthSession(null);
    setEditOAuthProxyUrl("");
    setEditOAuthCallbackUrl("");
    setEditOAuthGenerating(false);
    setEditOAuthUpdating(false);
  };

  const parsedScoreBias =
    scoreMode === "custom" ? parseIntegerInput(scoreInput) : null;
  const parsedBaseConcurrency =
    concurrencyMode === "custom" ? parseIntegerInput(concurrencyInput) : null;
  const scoreInputInvalid =
    scoreMode === "custom" &&
    (parsedScoreBias === null ||
      parsedScoreBias < -200 ||
      parsedScoreBias > 200);
  const concurrencyInputInvalid =
    concurrencyMode === "custom" &&
    (parsedBaseConcurrency === null || parsedBaseConcurrency < 1);
  const editAutoPause5hThresholdInvalid = isPercentThresholdInputInvalid(
    editAutoPause5hThresholdInput,
  );
  const editAutoPause7dThresholdInvalid = isPercentThresholdInputInvalid(
    editAutoPause7dThresholdInput,
  );
  const editDispatchCountLimitInvalid = isDispatchCountLimitInputInvalid(
    editDispatchCountLimitInput,
  );
  const editSchedulerPriorityInvalid = isSchedulerPriorityInputInvalid(
    editSchedulerPriorityInput,
  );
  const editDispatchCountLimitPreview =
    editDispatchCountLimitInvalid
      ? null
      : dispatchCountLimitInputToValue(editDispatchCountLimitInput);
  const editDispatchCountResetTime =
    editDispatchCountLimitPreview && editingAccount
      ? formatResetAt(editingAccount.dispatch_count_reset_at)
      : null;
  const batchAutoPause5hThresholdInvalid = isPercentThresholdInputInvalid(
    batchAutoPause5hThresholdInput,
  );
  const batchAutoPause7dThresholdInvalid = isPercentThresholdInputInvalid(
    batchAutoPause7dThresholdInput,
  );
  const openAIAccountInputInvalid = Boolean(
    editingAccount?.openai_responses_api &&
    editTab === "account" &&
    (!editOpenAIForm.base_url.trim() || editOpenAIForm.models.length === 0),
  );

  const editPreview = useMemo(() => {
    if (!editingAccount) return null;

    const rawScore = Math.round(editingAccount.scheduler_score ?? 0);
    const appliedBias =
      scoreMode === "custom"
        ? (parsedScoreBias ?? getEffectiveScoreBias(editingAccount))
        : getDefaultScoreBias(editingAccount.plan_type);
    const baseConcurrency =
      concurrencyMode === "custom"
        ? (parsedBaseConcurrency ?? getEffectiveBaseConcurrency(editingAccount))
        : getEffectiveBaseConcurrency(editingAccount);
    const healthTier = getPreviewHealthTier(editingAccount, skipWarmTier);

    return {
      rawScore,
      dispatchScore: computePreviewDispatchScore(
        editingAccount,
        rawScore,
        appliedBias,
      ),
      healthTier,
      dynamicConcurrency: computePreviewDynamicConcurrency(
        healthTier,
        editingAccount,
        baseConcurrency,
      ),
      appliedBias,
      baseConcurrency,
    };
  }, [
    editingAccount,
    scoreMode,
    parsedScoreBias,
    concurrencyMode,
    parsedBaseConcurrency,
    skipWarmTier,
  ]);

  const handleSaveScheduler = async () => {
    if (!editingAccount) return;
    if (
      scoreInputInvalid ||
      concurrencyInputInvalid ||
      editAutoPause5hThresholdInvalid ||
      editAutoPause7dThresholdInvalid ||
      editDispatchCountLimitInvalid ||
      editSchedulerPriorityInvalid
    ) {
      showToast(t("accounts.schedulerInvalidInput"), "error");
      return;
    }
    const parsedCustomHeaders = parseCustomHeadersText(editCustomHeadersText);
    if (!parsedCustomHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }

    setEditSubmitting(true);
    try {
      const payload = {
        score_bias_override: scoreMode === "custom" ? parsedScoreBias : null,
        base_concurrency_override:
          concurrencyMode === "custom" ? parsedBaseConcurrency : null,
        skip_warm_tier: skipWarmTier,
        allowed_api_key_ids: allowedAPIKeySelection,
        proxy_url: editProxyUrl.trim() || null,
        tags: editTags,
        group_ids: editGroupIds,
        auto_pause_5h_threshold: percentThresholdInputToRatio(
          editAutoPause5hThresholdInput,
        ),
        auto_pause_7d_threshold: percentThresholdInputToRatio(
          editAutoPause7dThresholdInput,
        ),
        auto_pause_5h_disabled: editAutoPause5hDisabled,
        auto_pause_7d_disabled: editAutoPause7dDisabled,
        ignore_usage_limit_status_override:
          editIgnoreUsageLimitStatusMode === "inherit"
            ? null
            : editIgnoreUsageLimitStatusMode === "enabled",
        dispatch_count_limit: dispatchCountLimitInputToValue(
          editDispatchCountLimitInput,
        ),
        scheduler_priority: schedulerPriorityInputToValue(
          editSchedulerPriorityInput,
        ),
        custom_headers: parsedCustomHeaders.value,
        // 指纹收敛只作用于 Codex 官方出站路径，中转/Grok 账号不下发该字段。
        ...(isCodexOfficialAccount(editingAccount)
          ? { codex_fingerprint_mode: editCodexFingerprintMode }
          : {}),
      };
      await api.updateAccountScheduler(editingAccount.id, payload);
      showToast(t("accounts.schedulerSaveSuccess"));
      await Promise.all([reload(), reloadGroups()]);
      closeSchedulerEditor(true);
    } catch (error) {
      showToast(
        t("accounts.schedulerSaveFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditSubmitting(false);
    }
  };

  const handleSaveAccountEditor = async () => {
    if (editingAccount?.openai_responses_api && editTab === "account") {
      await handleSaveOpenAIAccountSettings();
      return;
    }
    if (isOAuthAccount(editingAccount) && editTab === "account") {
      await handleUpdateOAuthAccount();
      return;
    }
    await handleSaveScheduler();
  };

  const reloadGroups = async () => {
    const res = await api.listAccountGroups();
    setAllGroups(res.groups ?? []);
  };

  /** Inline create from multi-select (batch / quick / account editor). Returns new group id. */
  const handleCreateGroupInline = async (
    name: string,
  ): Promise<number | null> => {
    const trimmed = name.trim();
    if (!trimmed) {
      showToast(t("accounts.groupNameRequired"), "error");
      return null;
    }
    try {
      const res = await api.createAccountGroup({
        name: trimmed,
        color: ACCOUNT_GROUP_COLORS[allGroups.length % ACCOUNT_GROUP_COLORS.length],
      });
      await reloadGroups();
      showToast(t("accounts.groupCreated"));
      return res.id;
    } catch (error) {
      showToast(getErrorMessage(error), "error");
      return null;
    }
  };

  const parsedGroupBaseConcurrency = parseIntegerInput(
    groupDraft.baseConcurrencyInput,
  );
  const groupBaseConcurrencyInvalid =
    groupDraft.baseConcurrencyInput.trim() !== "" &&
    (parsedGroupBaseConcurrency === null || parsedGroupBaseConcurrency < 1);

  const resetGroupDraft = () => {
    setGroupDraft({
      id: null,
      name: "",
      description: "",
      color: ACCOUNT_GROUP_COLORS[0],
      baseConcurrencyInput: "",
      auto_pause_5h_threshold: 0,
      auto_pause_7d_threshold: 0,
      proxyURLsInput: "",
      channel: "codex",
    });
  };

  const startEditGroup = (group: AccountGroup) => {
    setGroupDraft({
      id: group.id,
      name: group.name,
      description: group.description ?? "",
      color: group.color || ACCOUNT_GROUP_COLORS[0],
      baseConcurrencyInput:
        typeof group.base_concurrency_override === "number" &&
        group.base_concurrency_override > 0
          ? String(group.base_concurrency_override)
          : "",
      auto_pause_5h_threshold: group.auto_pause_5h_threshold ?? 0,
      auto_pause_7d_threshold: group.auto_pause_7d_threshold ?? 0,
      proxyURLsInput: (group.proxy_urls ?? []).join("\n"),
      channel: group.channel,
    });
  };

  const handleSaveGroup = async () => {
    const name = groupDraft.name.trim();
    if (!name) {
      showToast(t("accounts.groupNameRequired"), "error");
      return;
    }
    if (groupBaseConcurrencyInvalid) {
      showToast(t("accounts.groupBaseConcurrencyRange"), "error");
      return;
    }
    setGroupSubmitting(true);
    try {
      const payload = {
        name,
        description: groupDraft.description.trim(),
        color: groupDraft.color.trim() || ACCOUNT_GROUP_COLORS[0],
        base_concurrency_override:
          groupDraft.baseConcurrencyInput.trim() === ""
            ? null
            : parsedGroupBaseConcurrency,
        auto_pause_5h_threshold: groupDraft.auto_pause_5h_threshold,
        auto_pause_7d_threshold: groupDraft.auto_pause_7d_threshold,
        proxy_urls: groupDraft.proxyURLsInput
          .split("\n")
          .map((line) => line.trim())
          .filter(Boolean),
        channel: groupDraft.channel,
      };
      if (groupDraft.id === null) {
        await api.createAccountGroup(payload);
        showToast(t("accounts.groupCreated"));
      } else {
        await api.updateAccountGroup(groupDraft.id, payload);
        showToast(t("accounts.groupUpdated"));
      }
      await reloadGroups();
      resetGroupDraft();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setGroupSubmitting(false);
    }
  };

  const handleDeleteGroup = async (group: AccountGroup) => {
    const force = group.member_count > 0;
    const confirmed = await confirm({
      title: t("accounts.groupDeleteTitle"),
      description: force
        ? t("accounts.groupDeleteWithMembers")
        : t("accounts.groupDeleteEmpty"),
      confirmText: force ? t("accounts.groupDeleteForce") : t("common.delete"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setGroupSubmitting(true);
    try {
      await api.deleteAccountGroup(group.id, force);
      showToast(t("accounts.groupDeleted"));
      setEditGroupIds((current) => current.filter((id) => id !== group.id));
      setBatchGroupIds((current) => current.filter((id) => id !== group.id));
      setGroupFilter((current) =>
        pruneAccountGroupFilter(
          current,
          allGroups.filter((item) => item.id !== group.id),
        ),
      );
      if (groupDraft.id === group.id) resetGroupDraft();
      await Promise.all([reload(), reloadGroups()]);
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setGroupSubmitting(false);
    }
  };

  // 行操作实现每轮渲染都刷进 ref(始终指向最新闭包),对外暴露的 rowActions
  // 只创建一次,保证 memo 行组件的 props 引用稳定。
  const rowActionsImplRef = useRef<AccountRowActions | null>(null);
  rowActionsImplRef.current = {
    toggleSelect,
    openDetail: openAccountDetail,
    openSchedulerEditor,
    openQuickConfig: (account) => setQuickConfigAccount(account),
    openQuickGroupEditor,
    openQuickProxyEditor: (account) => setQuickProxyAccount(account),
    openUsage: (account) => {
      setUsageInitialPage("overview");
      setUsageAccount(account);
    },
    openOfficialUsage: (account) => {
      setUsageInitialPage("official");
      setUsageAccount(account);
    },
    openTesting: openTestingAccount,
    refresh: (account) => void handleRefresh(account),
    generateAuthJson: (account) => void handleGenerateAuthJSON(account),
    toggleEnabled: (account) => void handleToggleEnabled(account),
    toggleLock: (account) => void handleToggleLock(account),
    resetStatus: (account) => handleResetStatus(account),
    resetCredits: (account) => void handleResetCredits(account),
    openModelsEditor,
    remove: (account) => void handleDelete(account),
    usageRefreshed: () => void reloadSilently(),
  };
  const rowActions = useMemo<AccountRowActions>(
    () => ({
      toggleSelect: (id) => rowActionsImplRef.current?.toggleSelect(id),
      openDetail: (a) => rowActionsImplRef.current?.openDetail(a),
      openSchedulerEditor: (a) => rowActionsImplRef.current?.openSchedulerEditor(a),
      openQuickConfig: (a) => rowActionsImplRef.current?.openQuickConfig(a),
      openQuickGroupEditor: (a) => rowActionsImplRef.current?.openQuickGroupEditor(a),
      openQuickProxyEditor: (a) => rowActionsImplRef.current?.openQuickProxyEditor(a),
      openUsage: (a) => rowActionsImplRef.current?.openUsage(a),
      openOfficialUsage: (a) => rowActionsImplRef.current?.openOfficialUsage(a),
      openTesting: (a) => rowActionsImplRef.current?.openTesting(a),
      refresh: (a) => rowActionsImplRef.current?.refresh(a),
      generateAuthJson: (a) => rowActionsImplRef.current?.generateAuthJson(a),
      toggleEnabled: (a) => rowActionsImplRef.current?.toggleEnabled(a),
      toggleLock: (a) => rowActionsImplRef.current?.toggleLock(a),
      resetStatus: (a) => rowActionsImplRef.current?.resetStatus(a),
      resetCredits: (a) => rowActionsImplRef.current?.resetCredits(a),
      openModelsEditor: (a) => rowActionsImplRef.current?.openModelsEditor(a),
      remove: (a) => rowActionsImplRef.current?.remove(a),
      usageRefreshed: () => rowActionsImplRef.current?.usageRefreshed(),
    }),
    [],
  );

  // 四个账号视图共用同一切换器（独立页面通过 headerSlot 注入）。
  // 滑块动画 + 品牌 logo，与仪表盘渠道过滤器视觉一致。
  // useMemo 保持引用稳定,否则每轮渲染的新元素会击穿独立账号页的 memo 边界。
  const providerSwitcherOptions = useMemo(
    () =>
      (
        [
          ["codex", t("accounts.providerViewCodex")],
          ["grok", t("accounts.providerViewGrok")],
          ["antigravity", t("accounts.providerViewAntigravity")],
          ["claude", t("accounts.providerViewClaude")],
        ] as const
      ).filter(([key]) => visibleChannels.includes(key)),
    [t, visibleChannels],
  );
  const providerSwitcher = useMemo(() => (
    <div
      className="relative grid w-full max-w-[560px] items-center rounded-lg border border-border bg-muted/40 p-0.5"
      style={{ gridTemplateColumns: `repeat(${providerSwitcherOptions.length}, minmax(0, 1fr))` }}
    >
      <span
        aria-hidden
        className="absolute inset-y-0.5 left-0.5 rounded-md bg-background shadow-sm transition-transform duration-300 ease-out"
        style={{
          width: `calc((100% - 4px) / ${providerSwitcherOptions.length})`,
          transform: `translateX(${Math.max(0, providerSwitcherOptions.findIndex(([key]) => key === providerView)) * 100}%)`,
        }}
      />
      {providerSwitcherOptions.map(([key, label]) => (
        <button
          key={key}
          type="button"
          onClick={() => setProviderView(key)}
          aria-pressed={providerView === key}
          className={cn(
            "relative z-10 inline-flex min-w-0 items-center justify-center gap-1 rounded-md px-1.5 py-2 text-xs font-semibold transition-all duration-200 active:scale-[0.97] sm:gap-2 sm:px-3 sm:text-sm",
            providerView === key
              ? "text-foreground"
              : "text-muted-foreground opacity-75 grayscale hover:opacity-100 hover:grayscale-0 hover:text-foreground",
          )}
        >
          <ChannelLogo channel={key} size={18} />
          <span className="min-w-0 truncate">{label}</span>
        </button>
      ))}
    </div>
  ), [providerView, setProviderView, t, providerSwitcherOptions]);

  if (providerView === "grok") {
    // key 触发渠道切换时整块内容淡入过渡，切换器由 headerSlot 常驻不闪。
    return (
      <div key="provider-grok" className="animate-channel-switch-in">
        {grokSelfServicePending > 0 ? (
          <button
            type="button"
            onClick={() => navigate("/accounts?pending=1")}
            className="mb-3 flex w-full items-center gap-2.5 rounded-xl border border-amber-500/40 bg-amber-500/[0.06] px-3 py-2.5 text-left shadow-sm transition-colors hover:bg-amber-500/[0.1]"
          >
            <span className="flex size-9 shrink-0 items-center justify-center rounded-xl bg-amber-500/15 text-amber-600 dark:text-amber-300">
              <Hourglass className="size-4" />
            </span>
            <span className="min-w-0">
              <span className="block text-sm font-semibold tracking-tight text-foreground">
                {t("accounts.pendingReview.grokBanner", { count: grokSelfServicePending })}
              </span>
              <span className="block text-[11px] text-muted-foreground">
                {t("accounts.pendingReview.jumpHint")}
              </span>
            </span>
          </button>
        ) : null}
        <GrokAccounts
          headerSlot={providerSwitcher}
          showOperationResults={showOperationResults}
          onShowOperationResultsChange={
            handleOperationResultsVisibilityChange
          }
        />
      </div>
    );
  }

  if (providerView === "antigravity") {
    return (
      <div key="provider-antigravity" className="animate-channel-switch-in">
        <AntigravityAccounts headerSlot={providerSwitcher} />
      </div>
    );
  }

  if (providerView === "claude") {
    return (
      <div key="provider-claude" className="animate-channel-switch-in">
        <ClaudeAccounts headerSlot={providerSwitcher} />
      </div>
    );
  }

  return (
    <div
      key="provider-codex"
      className="relative @container/accounts animate-channel-switch-in"
      onDragEnter={handleDragEnter}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={(e) => void handleDrop(e)}
    >
      {dragging && (
        <div className="pointer-events-none absolute inset-0 z-50 flex items-center justify-center rounded-lg border-2 border-dashed border-primary bg-primary/5 backdrop-blur-sm">
          <div className="flex flex-col items-center gap-2 text-primary">
            <Upload className="size-10" />
            <span className="text-lg font-semibold">
              {t("accounts.dropToImport")}
            </span>
            <span className="text-sm text-muted-foreground">
              {t("accounts.dropHint")}
            </span>
          </div>
        </div>
      )}
      <OperationProgressToast
        progress={operationProgress}
        onClose={closeOperationProgress}
      />
      <OperationResultsModal
        state={showOperationResults ? operationResults : null}
        accounts={accounts}
        channel="codex"
        onClose={() => setOperationResults(null)}
      />
      <StateShell
        variant="page"
        loading={loading && accounts.length === 0}
        error={accounts.length === 0 ? error : null}
        onRetry={() => void reload()}
        loadingTitle={t("accounts.loadingTitle")}
        loadingDescription={t("accounts.loadingDesc")}
        errorTitle={t("accounts.errorTitle")}
      >
        <>
          {showRecycleBin ? (
            <RecycleBinView
              onClose={() => setShowRecycleBin(false)}
              onChanged={() => void reloadSilently()}
              confirm={confirm}
              runStreamingOperation={runStreamingAccountOperation}
            />
          ) : null}
          {showInvite ? (
            <CodexInviteView
              accounts={accounts}
              loading={loading}
              onClose={() => setShowInvite(false)}
            />
          ) : null}
          <div className={showRecycleBin || showInvite ? "hidden" : "contents"}>
          <PageHeader
            title={t("accounts.title")}
            description={t("accounts.description")}
            onRefresh={() => void reload()}
            hideTitle
            actionsBelow
            titleAdornment={
              <div className="flex items-center gap-2">
                {providerSwitcher}
                <Select
                  className="w-32"
                  compact
                  value={pageMode}
                  onValueChange={(value) => {
                    const mode = value === "personal" ? "personal" : "pool";
                    // 用户手动选择：标记并持久化，之后一律尊重用户、不再自动判定。
                    pageModeUserSetRef.current = true;
                    persistAccountPageMode(mode);
                    setPageMode(mode);
                  }}
                  options={[
                    { value: "pool", label: t("accounts.pageModePool") },
                    { value: "personal", label: t("accounts.pageModePersonal") },
                  ]}
                />
              </div>
            }
            actions={
              <>
                {(() => {
                  // 「管理」只收低频项；测试连接 / 导入 / 导出 / 清理常驻在外。
                  const manageSections: HeaderActionMenuSection[] = [
                    {
                      key: "maintenance",
                      label: t("accounts.maintenanceActions"),
                      items: [
                        {
                          key: "refresh-tokens",
                          label: t("accounts.refreshTokens"),
                          icon: (
                            <RefreshCw
                              className={`size-3.5 ${batchRefreshing ? "animate-spin" : ""}`}
                            />
                          ),
                          disabled:
                            batchLoading ||
                            batchTesting ||
                            data.total === 0,
                          onSelect: () =>
                            void handleBatchRefresh(undefined, true),
                        },
                        {
                          key: "lock-subscription",
                          label: lockingSubscriptionAccounts
                            ? t("accounts.lockingSubscriptionAccounts")
                            : t("accounts.lockSubscriptionAccounts"),
                          icon: <Lock className="size-3.5" />,
                          disabled:
                            batchLoading ||
                            batchTesting ||
                            lockingSubscriptionAccounts ||
                            subscriptionAccountsToLockCount === 0,
                          title: t("accounts.lockSubscriptionAccountsHint", {
                            count: subscriptionAccountsToLockCount,
                          }),
                          onSelect: () => void handleLockSubscriptionAccounts(),
                        },
                      ],
                    },
                    {
                      key: "data",
                      label: t("accounts.dataActions"),
                      items: [
                        {
                          key: "migrate",
                          label: migrating
                            ? t("accounts.migrating")
                            : t("accounts.migrateImport"),
                          icon: <ArrowDownToLine className="size-3.5" />,
                          disabled: migrating,
                          onSelect: () => setShowMigrate(true),
                        },
                        {
                          key: "sub2api",
                          label: t("accounts.sub2api.entry"),
                          icon: <Cloud className="size-3.5" />,
                          onSelect: () => setShowSub2APIImport(true),
                        },
                      ],
                    },
                    {
                      key: "tools",
                      label: t("accounts.toolsActions"),
                      items: [
                        {
                          key: "recycle",
                          label: t("accounts.recycleBin"),
                          icon: <Recycle className="size-3.5" />,
                          onSelect: () => setShowRecycleBin(true),
                        },
                        {
                          key: "invite",
                          label: t("invite.entry"),
                          icon: <Mail className="size-3.5" />,
                          onSelect: () => setShowInvite(true),
                        },
                      ],
                    },
                  ];

                  const cleanupItems: HeaderActionMenuItem[] = [
                    {
                      key: "clean-banned",
                      label: cleaningBanned
                        ? t("accounts.cleaning")
                        : t("accounts.cleanBanned"),
                      icon: <Ban className="size-3.5" />,
                      disabled: cleaningBanned,
                      onSelect: () => void handleCleanBanned(),
                    },
                    {
                      key: "clean-rate-limited",
                      label: cleaningRateLimited
                        ? t("accounts.cleaning")
                        : t("accounts.cleanRateLimited"),
                      icon: <Timer className="size-3.5" />,
                      disabled: cleaningRateLimited,
                      onSelect: () => void handleCleanRateLimited(),
                    },
                    {
                      key: "clean-error",
                      label: cleaningError
                        ? t("accounts.cleaning")
                        : t("accounts.cleanError"),
                      icon: <AlertTriangle className="size-3.5" />,
                      disabled: cleaningError,
                      onSelect: () => void handleCleanError(),
                    },
                  ];

                  return (
                    <div className="flex w-full flex-wrap items-center gap-1.5 sm:w-auto sm:justify-end">
                      <Button
                        size="sm"
                        className="min-w-0 sm:flex-none"
                        onClick={() => setShowAdd(true)}
                      >
                        <Plus className="size-3.5" />
                        {t("accounts.addAccount")}
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        className="shrink-0"
                        disabled={
                          batchLoading || batchTesting || data.total === 0
                        }
                        onClick={() => void handleBatchTest()}
                      >
                        <FlaskConical className="size-3.5" />
                        <span className="hidden md:inline">
                          {batchTesting
                            ? t("accounts.batchTesting")
                            : t("accounts.testConnection")}
                        </span>
                      </Button>
                      <label
                        className="inline-flex h-8 shrink-0 cursor-pointer items-center gap-2 rounded-md border border-border bg-background px-2.5 text-xs font-medium text-muted-foreground"
                        title={t(
                          "accounts.operationResultsPreferenceHint",
                        )}
                      >
                        <Switch
                          checked={showOperationResults}
                          onCheckedChange={
                            handleOperationResultsVisibilityChange
                          }
                          aria-label={t(
                            "accounts.operationResultsPreference",
                          )}
                        />
                        <span className="hidden lg:inline">
                          {t("accounts.operationResultsPreference")}
                        </span>
                      </label>
                      <Button
                        variant="outline"
                        size="sm"
                        className="shrink-0"
                        disabled={importing}
                        onClick={() => setShowImportPicker(true)}
                      >
                        <Upload className="size-3.5" />
                        <span className="hidden md:inline">
                          {importing
                            ? t("accounts.importing")
                            : t("accounts.importFile")}
                        </span>
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        className="shrink-0"
                        disabled={exporting}
                        onClick={() => setShowExportPicker(true)}
                      >
                        <Download className="size-3.5" />
                        <span className="hidden md:inline">
                          {exporting
                            ? t("accounts.exporting")
                            : t("accounts.export")}
                        </span>
                      </Button>
                      <HeaderActionMenu
                        label={t("accounts.cleanupActions")}
                        icon={<Trash2 className="size-3.5" />}
                        align="end"
                        items={cleanupItems}
                      />
                      <Button
                        variant="outline"
                        size="sm"
                        className="shrink-0"
                        aria-pressed={showAnalysisCharts}
                        onClick={() =>
                          setShowAnalysisCharts((visible) => !visible)
                        }
                        title={
                          showAnalysisCharts
                            ? t("accounts.hideAnalysisCharts")
                            : t("accounts.showAnalysisCharts")
                        }
                      >
                        <BarChart3 className="size-3.5" />
                        <span className="hidden sm:inline">
                          {showAnalysisCharts
                            ? t("accounts.hideAnalysisCharts")
                            : t("accounts.showAnalysisCharts")}
                        </span>
                      </Button>
                      <HeaderActionMenu
                        label={t("accounts.manageActions")}
                        icon={<SlidersHorizontal className="size-3.5" />}
                        align="end"
                        sections={manageSections}
                      />
                    </div>
                  );
                })()}

                <input
                  ref={fileInputRef}
                  type="file"
                  accept=".txt"
                  multiple
                  className="hidden"
                  onChange={(e) => void handleFileImport(e)}
                />
                <input
                  ref={jsonInputRef}
                  type="file"
                  accept=".json"
                  multiple
                  className="hidden"
                  onChange={(e) => void handleJsonImport(e)}
                />
                <input
                  ref={jsonAtInputRef}
                  type="file"
                  accept=".json"
                  multiple
                  className="hidden"
                  onChange={(e) => void handleJsonAtImport(e)}
                />
                <input
                  ref={atFileInputRef}
                  type="file"
                  accept=".txt"
                  multiple
                  className="hidden"
                  onChange={(e) => void handleAtFileImport(e)}
                />
                <input
                  ref={folderInputRef}
                  type="file"
                  className="hidden"
                  onChange={(e) => void handleFolderImport(e)}
                  {...({
                    webkitdirectory: "",
                    directory: "",
                  } as React.InputHTMLAttributes<HTMLInputElement>)}
                />
              </>
            }
          />

          {error && accounts.length > 0 ? (
            <div className="mb-2 flex items-center justify-between gap-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive" role="alert">
              <span className="truncate">{error}</span>
              <Button variant="outline" size="sm" onClick={() => void reload()}>
                {t("common.retry")}
              </Button>
            </div>
          ) : null}
          {loading || disabledSorts.length > 0 ? (
            <div className="mb-2 flex items-center justify-end gap-1.5 text-xs text-muted-foreground" role="status">
              <Loader2 className="size-3 animate-spin" />
              {loading ? t("common.loading") : t("accounts.statsWarming")}
            </div>
          ) : null}
          <div className="mb-4 grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-5">
            <CompactStat
              label={t("accounts.totalAccounts")}
              chipLabel={t("accounts.filterAll")}
              value={totalAccounts}
              tone="neutral"
              active={statusFilter === "all"}
              onClick={() => {
                setStatusFilter("all");
                setPage(1);
              }}
            />
            <CompactStat
              label={t("accounts.normalAccounts")}
              chipLabel={t("accounts.filterNormal")}
              value={normalAccounts}
              tone="success"
              active={statusFilter === "normal"}
              onClick={() => {
                setStatusFilter("normal");
                setPage(1);
              }}
            />
            <CompactStat
              label={t("accounts.schedulingAccounts")}
              chipLabel={t("accounts.filterScheduling")}
              value={schedulingAccounts}
              tone="warning"
              active={statusFilter === "scheduling"}
              onClick={() => {
                setStatusFilter("scheduling");
                setPage(1);
              }}
            />
            <CompactStat
              label={t("accounts.rateLimited")}
              chipLabel={t("accounts.filterRateLimited")}
              value={rateLimitedAccounts}
              tone="warning"
              active={statusFilter === "rate_limited"}
              details={[
                { label: "5h", value: rateLimited5hAccounts },
                { label: "7d", value: rateLimited7dAccounts },
              ]}
              onClick={() => {
                setStatusFilter("rate_limited");
                setPage(1);
              }}
            />
            <CompactStat
              label={t("accounts.abnormalAccounts")}
              chipLabel={t("accounts.filterAbnormal")}
              value={abnormalAccounts}
              tone="danger"
              active={statusFilter === "abnormal"}
              details={[
                { label: t("accounts.abnormalBannedShort"), value: bannedAccounts },
                { label: t("accounts.abnormalErrorShort"), value: errorAccounts },
              ]}
              onClick={() => {
                setStatusFilter("abnormal");
                setPage(1);
              }}
            />
          </div>

          {showAnalysisCharts && accountAnalysis ? (
            <div className="mb-4 grid items-stretch gap-4 xl:grid-cols-2">
              <AccountQuotaDistributionChart
                analysis={accountAnalysis.quota}
                compact
                className="min-w-0"
                onRefreshAnalysis={() => loadAccountAnalysis({ silent: true })}
                onProbeStarted={() => {
                  showToast(t('accounts.quotaDistributionRefreshStarted'), 'success')
                }}
                onProbeError={(message) => showToast(message, 'error')}
              />
              <AccountRateLimitRecoveryChart
                analysis={accountAnalysis}
                compact
                className="min-w-0"
              />
            </div>
          ) : showAnalysisCharts ? (
            <div className="mb-4 flex min-h-28 items-center justify-center rounded-xl border border-dashed border-border bg-muted/20 px-4 text-sm text-muted-foreground">
              {accountAnalysisError ? (
                <div className="flex flex-wrap items-center justify-center gap-3 text-center">
                  <span>{accountAnalysisError}</span>
                  <Button variant="outline" size="sm" onClick={() => void loadAccountAnalysis()}>
                    {t("common.retry")}
                  </Button>
                </div>
              ) : (
                <span>{accountAnalysisLoading ? t("common.loading") : t("common.noData")}</span>
              )}
            </div>
          ) : null}

          <div className="toolbar-surface mb-3 flex flex-col gap-2.5">
            <div className="flex items-center gap-1.5 overflow-x-auto [-mx-3] [px-3] [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
              <span className="shrink-0 whitespace-nowrap text-[12px] font-semibold text-foreground">
                {t("accounts.filter")}
              </span>
              {showPendingReviewPanel ? (
                <button
                  type="button"
                  onClick={scrollToPendingReview}
                  className="shrink-0 whitespace-nowrap rounded-lg bg-amber-500/15 px-2.5 py-1.5 text-[12px] font-semibold text-amber-800 transition-colors hover:bg-amber-500/20 dark:text-amber-200"
                >
                  {t("accounts.pendingReview.chip", { count: selfServicePendingCount })}
                </button>
              ) : null}
              {(
                [
                  ["all", t("accounts.filterAll"), totalAccounts],
                  ["normal", t("accounts.filterNormal"), normalAccounts],
                  [
                    "scheduling",
                    t("accounts.filterScheduling"),
                    schedulingAccounts,
                  ],
                  [
                    "rate_limited",
                    t("accounts.filterRateLimited"),
                    rateLimitedAccounts,
                  ],
                  ["abnormal", t("accounts.filterAbnormal"), abnormalAccounts],
                  ["banned", t("accounts.filterBanned"), bannedAccounts],
                  ["error", t("accounts.filterError"), errorAccounts],
                  [
                    "unsampled",
                    t("accounts.filterUnsampled"),
                    unsampledAccounts,
                  ],
                  ["disabled", t("accounts.filterDisabled"), disabledAccounts],
                  ["locked", t("accounts.filterLocked"), lockedAccounts],
                ] as const
              ).map(([key, label, count]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => {
                    setStatusFilter(key);
                    setPage(1);
                  }}
                  className={`shrink-0 whitespace-nowrap rounded-lg px-2.5 py-1.5 text-[12px] font-semibold transition-colors ${
                    statusFilter === key
                      ? "bg-primary text-primary-foreground"
                      : "bg-muted/50 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {label} {count}
                </button>
              ))}
            </div>

            <div className="flex flex-wrap items-center gap-1.5">
              <span className="mr-0.5 shrink-0 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                {t("accounts.schedulerView")}
              </span>
              <SchedulerChip
                label={t("accounts.healthy")}
                value={healthyAccounts}
                tone="success"
              />
              <SchedulerChip
                label={t("accounts.warm")}
                value={warmAccounts}
                tone="warning"
              />
              <SchedulerChip
                label={t("accounts.risky")}
                value={riskyAccounts}
                tone="danger"
              />
              <SchedulerChip
                label={t("status.unauthorized")}
                value={bannedAccounts}
                tone="neutral"
              />
            </div>

            <div className="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
              <div className="relative w-full min-w-0 shrink-0 sm:w-64">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <AccountSearchInput
                  className="h-9 rounded-lg pl-9 text-[13px] sm:h-8"
                  placeholder={t("accounts.searchPlaceholder")}
                  value={debouncedSearchQuery}
                  onCommit={commitSearchQuery}
                />
              </div>
              <div className="flex max-w-full shrink-0 items-center gap-0.5 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
                {(
                  ["all", "pro", "prolite", "plus", "team", "k12", "free"] as const
                ).map((key) => (
                  <button
                    key={key}
                    onClick={() => {
                      setPlanFilter(key);
                      setPage(1);
                    }}
                    className={`shrink-0 whitespace-nowrap rounded-md px-2.5 py-1.5 text-[12px] font-medium transition-colors ${
                      planFilter === key
                        ? "bg-background text-foreground shadow-sm"
                        : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {key === "all"
                      ? t("accounts.filterAll")
                      : key === "prolite"
                        ? "ProLite"
                        : key === "k12"
                          ? "K12"
                          : key.charAt(0).toUpperCase() + key.slice(1)}
                  </button>
                ))}
              </div>

              {/* 账号类型：快速把 Responses API 中转账号从 OAuth 官方账号里筛出来（issue #522） */}
              <div className="flex max-w-full shrink-0 items-center gap-0.5 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
                {(
                  [
                    ["all", t("accounts.filterAll"), totalAccounts],
                    ["oauth", "OAuth", oauthAccounts],
                    ["api_key", "API", apiKeyAccounts],
                  ] as const
                ).map(([key, label, count]) => (
                  <button
                    key={key}
                    onClick={() => {
                      setAuthFilter(key);
                      setPage(1);
                    }}
                    className={`shrink-0 whitespace-nowrap rounded-md px-2.5 py-1.5 text-[12px] font-medium transition-colors ${
                      authFilter === key
                        ? "bg-background text-foreground shadow-sm"
                        : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {key === "all" ? label : `${label} ${count}`}
                  </button>
                ))}
              </div>

              <div className="grid grid-cols-2 gap-2 sm:flex sm:flex-wrap sm:items-center sm:gap-2">
                <Select
                  className="w-full min-w-0 sm:w-36"
                  compact
                  value={tagFilter || "all"}
                  onValueChange={(value) => {
                    setTagFilter(value === "all" ? "" : value);
                    setPage(1);
                  }}
                  options={[
                    { value: "all", label: t("accounts.tagsFilter") },
                    ...allTags.map((tag) => ({ value: tag, label: tag })),
                  ]}
                />
                <Select
                  className="w-full min-w-0 sm:w-44 lg:w-52"
                  compact
                  value={domainFilter || "all"}
                  onValueChange={(value) => {
                    setDomainFilter(value === "all" ? "" : value);
                    setPage(1);
                  }}
                  options={[
                    { value: "all", label: t("accounts.emailDomainFilter") },
                    ...emailDomainStats.map((stat) => ({
                      value: stat.domain,
                      triggerLabel: stat.domain,
                      label: t("accounts.emailDomainFilterOption", {
                        domain: stat.domain,
                        banned: stat.banned,
                        total: stat.total,
                      }),
                    })),
                  ]}
                />
                <AccountGroupFilterSelect
                  className="w-full min-w-0 sm:w-40"
                  groups={codexGroups}
                  value={groupFilter}
                  onChange={(value) => {
                    setGroupFilter(value);
                    setPage(1);
                  }}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="w-full min-w-0 px-2 sm:w-auto"
                  aria-pressed={sortKey === "group"}
                  title={t("accounts.groupSortHint")}
                  onClick={() => {
                    if (sortKey === "group") {
                      setSortDir((current) =>
                        current === "desc" ? "asc" : "desc",
                      );
                    } else {
                      setSortKey("group");
                      setSortDir("asc");
                    }
                    setPage(1);
                  }}
                >
                  <Layers className="size-3.5 shrink-0" />
                  <span className="truncate">
                    {t("accounts.groupSort")}
                  </span>
                  {sortKey === "group" ? (
                    <span aria-hidden="true" className="shrink-0">
                      {sortDir === "desc" ? "↓" : "↑"}
                    </span>
                  ) : null}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="w-full min-w-0 px-2 sm:w-auto"
                  aria-pressed={sortKey === "schedulerPriority"}
                  title={t("accounts.schedulerPrioritySortHint")}
                  onClick={() => {
                    if (sortKey === "schedulerPriority") {
                      setSortDir((current) =>
                        current === "desc" ? "asc" : "desc",
                      );
                    } else {
                      setSortKey("schedulerPriority");
                      setSortDir("desc");
                    }
                    setPage(1);
                  }}
                >
                  <SlidersHorizontal className="size-3.5 shrink-0" />
                  <span className="truncate">
                    {t("accounts.schedulerPrioritySort")}
                  </span>
                  {sortKey === "schedulerPriority" ? (
                    <span aria-hidden="true" className="shrink-0">
                      {sortDir === "desc" ? "↓" : "↑"}
                    </span>
                  ) : null}
                </Button>
                {/* 卡片布局(自用模式/网格/移动端)没有可排序表头,这里补一个
                    紧凑排序入口,避免升级后"排序功能消失"(issue #493)。 */}
                {!shouldRenderDesktopTable && (
                  <div className="col-span-2 flex items-center gap-1.5 sm:col-span-1 sm:w-auto">
                    <Select
                      className="w-full min-w-0 sm:w-32"
                      compact
                      value={
                        sortKey === "requests" ||
                        sortKey === "today" ||
                        sortKey === "usage" ||
                        sortKey === "importTime"
                          ? sortKey
                          : "default"
                      }
                      onValueChange={(value) => {
                        if (value === "requests" || value === "today") {
                          if (guardUsageSort(value)) return;
                        }
                        if (value === "default") {
                          setSortKey(null);
                        } else {
                          setSortKey(value as "requests" | "today" | "usage" | "importTime");
                          setSortDir("desc");
                        }
                        setPage(1);
                      }}
                      options={[
                        { value: "default", label: t("accounts.cardSortDefault") },
                        { value: "requests", label: t("accounts.requests") },
                        { value: "today", label: t("accounts.todayStats") },
                        { value: "usage", label: t("accounts.usage") },
                        { value: "importTime", label: t("accounts.importTime") },
                      ]}
                    />
                    {(sortKey === "requests" ||
                      sortKey === "today" ||
                      sortKey === "usage" ||
                      sortKey === "importTime") && (
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="shrink-0 px-2.5"
                        aria-label={t("accounts.cardSortDirection")}
                        onClick={() => {
                          setSortDir((current) => (current === "desc" ? "asc" : "desc"));
                          setPage(1);
                        }}
                      >
                        <span aria-hidden="true">{sortDir === "desc" ? "↓" : "↑"}</span>
                      </Button>
                    )}
                  </div>
                )}
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="w-full min-w-0 px-2 sm:w-auto"
                  aria-pressed={showEmailDomainTags}
                  onClick={() => setShowEmailDomainTags((visible) => !visible)}
                >
                  {showEmailDomainTags ? (
                    <EyeOff className="size-3.5 shrink-0" />
                  ) : (
                    <Eye className="size-3.5 shrink-0" />
                  )}
                  <span className="truncate">
                    {showEmailDomainTags
                      ? t("accounts.hideEmailDomainTags")
                      : t("accounts.showEmailDomainTags")}
                  </span>
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="w-full min-w-0 px-2 sm:w-auto"
                  onClick={() => setShowGroupManager(true)}
                >
                  <FolderOpen className="size-3.5 shrink-0" />
                  <span className="truncate">{t("accounts.groupManage")}</span>
                </Button>
              </div>

              {!isPersonalMode && (
                <div className="flex w-full shrink-0 items-center gap-1.5 sm:ml-auto sm:w-auto">
                  <div className="hidden lg:inline-flex items-center rounded-md border border-border bg-muted/50 p-0.5">
                    <button
                      type="button"
                      onClick={() => setViewMode("table")}
                      title={t("accounts.viewModeTable")}
                      aria-label={t("accounts.viewModeTable")}
                      aria-pressed={viewMode === "table"}
                      className={`inline-flex items-center gap-1 rounded-sm px-2 py-1 text-[12px] font-medium transition-colors ${
                        viewMode === "table"
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      <Rows3 className="size-3.5" />
                      {t("accounts.viewModeTable")}
                    </button>
                    <button
                      type="button"
                      onClick={() => setViewMode("grid")}
                      title={t("accounts.viewModeGrid")}
                      aria-label={t("accounts.viewModeGrid")}
                      aria-pressed={viewMode === "grid"}
                      className={`inline-flex items-center gap-1 rounded-sm px-2 py-1 text-[12px] font-medium transition-colors ${
                        viewMode === "grid"
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      <LayoutGrid className="size-3.5" />
                      {t("accounts.viewModeGrid")}
                    </button>
                  </div>
                  <ColumnSettingsMenu
                    columnOrder={ACCOUNT_TABLE_COLUMNS}
                    columns={visibleColumns}
                    onToggle={(column) =>
                      setVisibleColumns((current) => ({
                        ...current,
                        [column]: !current[column],
                      }))
                    }
                    onReset={() =>
                      setVisibleColumns(getDefaultAccountVisibleColumns())
                    }
                    resetTitle={t("accounts.columnReset")}
                    labels={{
                      sequence: t("accounts.sequence"),
                      email: t("accounts.email"),
                      plan: t("accounts.plan"),
                      tags: t("accounts.tagsLabel"),
                      groups: t("accounts.groupsLabel"),
                      proxy: t("accounts.proxyColumn"),
                      priority: t("accounts.schedulerPriorityColumn"),
                      status: t("accounts.status"),
                      today: t("accounts.todayStats"),
                      requests: t("accounts.requests"),
                      usage: t("accounts.usage"),
                      billed: t("accounts.billed"),
                      importTime: t("accounts.importTime"),
                      updatedAt: t("accounts.updatedAt"),
                      actions: t("accounts.actions"),
                    }}
                    title={t("accounts.columnSettings")}
                  />
                </div>
              )}
            </div>

            {(statusFilter !== "all" ||
              planFilter !== "all" ||
              Boolean(tagFilter) ||
              Boolean(domainFilter) ||
              !isAccountGroupFilterEmpty(groupFilter)) && (
              <div className="flex flex-wrap items-center gap-1.5 border-t border-border/60 pt-2">
                {statusFilter !== "all" && (
                  <button
                    type="button"
                    onClick={() => {
                      setStatusFilter("all");
                      setPage(1);
                    }}
                    className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2.5 py-1 text-[11px] font-medium text-primary transition-colors hover:bg-primary/15"
                  >
                    {statusFilter === "normal"
                      ? t("accounts.filterNormal")
                      : statusFilter === "scheduling"
                        ? t("accounts.filterScheduling")
                      : statusFilter === "rate_limited"
                        ? t("accounts.filterRateLimited")
                        : statusFilter === "abnormal"
                          ? t("accounts.filterAbnormal")
                          : statusFilter === "banned"
                            ? t("accounts.filterBanned")
                            : statusFilter === "error"
                              ? t("accounts.filterError")
                              : statusFilter === "unsampled"
                                ? t("accounts.filterUnsampled")
                                : statusFilter === "disabled"
                                  ? t("accounts.filterDisabled")
                                  : t("accounts.filterLocked")}
                    <X className="size-3" />
                  </button>
                )}
                {planFilter !== "all" && (
                  <button
                    type="button"
                    onClick={() => {
                      setPlanFilter("all");
                      setPage(1);
                    }}
                    className="inline-flex items-center gap-1 rounded-full bg-muted px-2.5 py-1 text-[11px] font-medium text-foreground transition-colors hover:bg-muted/80"
                  >
                    {planFilter === "prolite"
                      ? "ProLite"
                      : planFilter === "k12"
                        ? "K12"
                        : planFilter.charAt(0).toUpperCase() + planFilter.slice(1)}
                    <X className="size-3" />
                  </button>
                )}
                {tagFilter && (
                  <button
                    type="button"
                    onClick={() => {
                      setTagFilter("");
                      setPage(1);
                    }}
                    className="inline-flex items-center gap-1 rounded-full bg-muted px-2.5 py-1 text-[11px] font-medium text-foreground transition-colors hover:bg-muted/80"
                  >
                    {tagFilter}
                    <X className="size-3" />
                  </button>
                )}
                {domainFilter && (
                  <button
                    type="button"
                    onClick={() => {
                      setDomainFilter("");
                      setPage(1);
                    }}
                    className="inline-flex items-center gap-1 rounded-full bg-muted px-2.5 py-1 text-[11px] font-medium text-foreground transition-colors hover:bg-muted/80"
                  >
                    {domainFilter}
                    <X className="size-3" />
                  </button>
                )}
                {!isAccountGroupFilterEmpty(groupFilter) && (
                  <button
                    type="button"
                    onClick={() => {
                      setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
                      setPage(1);
                    }}
                    className="inline-flex items-center gap-1 rounded-full bg-muted px-2.5 py-1 text-[11px] font-medium text-foreground transition-colors hover:bg-muted/80"
                  >
                    {t("accounts.groupsLabel")}
                    <X className="size-3" />
                  </button>
                )}
                <button
                  type="button"
                  onClick={() => {
                    setStatusFilter("all");
                    setPlanFilter("all");
                    setTagFilter("");
                    setDomainFilter("");
                    setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
                    setDebouncedSearchQuery("");
                    setPage(1);
                  }}
                  className="ml-auto text-[11px] font-medium text-muted-foreground transition-colors hover:text-foreground"
                >
                  {t("accounts.clearFilters")}
                </button>
              </div>
            )}
          </div>

          {selected.size > 0 && (
            <div className="sticky top-2 z-20 mb-4 flex items-center justify-between gap-3 rounded-xl border border-primary/20 bg-card/95 px-3 py-2 text-sm shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
              <span className="font-semibold text-primary">
                {t("common.selected", { count: selected.size })}
              </span>
              <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
                {/* 卡片视图（移动端/grid/自用）没有表头全选框，这里是唯一的全选入口（issue #522） */}
                {!allPageSelected && (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={batchLoading || batchTesting}
                    onClick={toggleSelectAll}
                  >
                    <ListChecks className="size-3.5" />
                    <span>{t("accounts.selectCurrentPage")}</span>
                  </Button>
                )}
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchRefresh()}
                >
                  <RefreshCw
                    className={`size-3.5 ${batchRefreshing ? "animate-spin" : ""}`}
                  />
                  <span className="hidden sm:inline">{t("accounts.batchRefresh")}</span>
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchTest(Array.from(selected))}
                >
                  <FlaskConical className="size-3.5" />
                  <span className="hidden sm:inline">
                    {batchTesting
                      ? t("accounts.batchTesting")
                      : t("accounts.batchTest")}
                  </span>
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchEnabled(true)}
                >
                  <Power className="size-3.5" />
                  <span className="hidden sm:inline">{t("accounts.enable")}</span>
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchEnabled(false)}
                >
                  <PowerOff className="size-3.5" />
                  <span className="hidden sm:inline">{t("accounts.disable")}</span>
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={openBatchGroupEditor}
                >
                  <FolderOpen className="size-3.5" />
                  <span className="hidden sm:inline">
                    {t("accounts.batchGroupEdit")}
                  </span>
                </Button>
                <HeaderActionMenu
                  label={t("accounts.batchMore")}
                  icon={<MoreHorizontal className="size-3.5" />}
                  align="end"
                  compact
                  items={[
                    {
                      key: "lock",
                      label: t("accounts.lock"),
                      icon: <Lock className="size-3.5" />,
                      disabled: batchLoading || batchTesting,
                      onSelect: () => void handleBatchLock(true),
                    },
                    {
                      key: "unlock",
                      label: t("accounts.unlock"),
                      icon: <Unlock className="size-3.5" />,
                      disabled: batchLoading || batchTesting,
                      onSelect: () => void handleBatchLock(false),
                    },
                    {
                      key: "meta",
                      label: t("accounts.batchMetaEdit"),
                      icon: <FolderOpen className="size-3.5" />,
                      disabled: batchLoading || batchTesting,
                      onSelect: openBatchMetaEditor,
                    },
                    {
                      key: "auto-pause",
                      label: t("accounts.batchAutoPauseEdit"),
                      icon: <Hourglass className="size-3.5" />,
                      disabled: batchLoading || batchTesting,
                      onSelect: openBatchQuotaAutoPauseEditor,
                    },
                    {
                      key: "reset-status",
                      label: t("accounts.batchResetStatus"),
                      icon: <RotateCcw className="size-3.5" />,
                      disabled: batchLoading || batchTesting,
                      onSelect: () => void handleBatchResetStatus(),
                    },
                    {
                      key: "delete",
                      label: t("accounts.batchDelete"),
                      icon: <Trash2 className="size-3.5" />,
                      disabled: batchLoading || batchTesting,
                      destructive: true,
                      onSelect: () => void handleBatchDelete(),
                    },
                  ]}
                />
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setSelected(new Set())}
                >
                  {t("accounts.cancelSelection")}
                </Button>
              </div>
            </div>
          )}

          {showPendingReviewPanel ? (
            <PendingSelfServiceReviewPanel
              onApprove={handleApprovePending}
              onReject={handleRejectPending}
              onSaveNote={handleSaveNote}
              onCountChange={handlePendingCountChange}
            />
          ) : null}

          <Card>
            <CardContent className="p-3 sm:p-4">
              <StateShell
                variant="section"
                isEmpty={accounts.length === 0}
                emptyIcon={<ChannelLogo channel="codex" size={30} />}
                emptyTitle={t("accounts.noData")}
                emptyDescription={t("accounts.noDataDesc")}
                action={
                  <Button onClick={() => setShowAdd(true)}>
                    {t("accounts.addAccount")}
                  </Button>
                }
              >
                {shouldRenderMobileCards ? (
                  <div
                    className={
                      isPersonalMode
                        ? "grid gap-3 grid-cols-1 md:grid-cols-2"
                        : viewMode === "grid"
                          ? "grid gap-3 grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4"
                          : "grid gap-3 lg:hidden"
                    }
                  >
                    {pagedAccounts.map((account, index) => (
                      <AccountCardItem
                        key={account.id}
                        account={account}
                        sequence={(currentPage - 1) * pageSize + index + 1}
                        selected={selected.has(account.id)}
                        detailOpen={detailAccountId === account.id}
                        allGroups={allGroups}
                        proxyCtx={proxyBindingCtx}
                        lazyMode={lazyMode}
                        showEmailDomainTags={showEmailDomainTags}
                        healthBuckets={healthBars[String(account.id)]}
                        refreshing={refreshingIds.has(account.id)}
                        authJsonExporting={authJsonExportingIds.has(account.id)}
                        variant={isPersonalMode ? "personal" : "mobile"}
                        visibleColumns={visibleColumns}
                        t={t}
                        actions={rowActions}
                      />
                    ))}
                  </div>
                ) : null}

                {shouldRenderDesktopTable ? (
                  <div
                    className={`data-table-shell hidden lg:block ${
                      sortedAccounts.length <= pageSize ? "account-table-shell-fit-content" : ""
                    }`}
                  >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead className="w-10">
                          <input
                            ref={selectAllRef}
                            type="checkbox"
                            className="size-4 cursor-pointer accent-primary"
                            checked={allPageSelected}
                            onChange={toggleSelectAll}
                          />
                        </TableHead>
                        {visibleColumns.sequence && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.sequence")}
                          </TableHead>
                        )}
                        {visibleColumns.email && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.email")}
                          </TableHead>
                        )}
                        {visibleColumns.tags && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.tagsLabel")}
                          </TableHead>
                        )}
                        {visibleColumns.groups && (
                          <TableHead
                            className="cursor-pointer select-none text-[13px] font-semibold transition-colors hover:text-primary"
                            onClick={() => {
                              if (sortKey === "group") {
                                setSortDir((current) =>
                                  current === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("group");
                                setSortDir("asc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.groupsLabel")}{" "}
                            {sortKey === "group"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.proxy && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.proxyColumn")}
                          </TableHead>
                        )}
                        {visibleColumns.priority && (
                          <TableHead
                            className="cursor-pointer select-none text-[13px] font-semibold transition-colors hover:text-primary"
                            onClick={() => {
                              if (sortKey === "schedulerPriority") {
                                setSortDir((current) =>
                                  current === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("schedulerPriority");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.schedulerPriorityColumn")}{" "}
                            {sortKey === "schedulerPriority"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.plan && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.plan")}
                          </TableHead>
                        )}
                        {visibleColumns.status && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.status")}
                          </TableHead>
                        )}
                        {visibleColumns.today && (
                          <TableHead
                            className={`text-[13px] font-semibold select-none transition-colors ${
                              usageSortBlocked
                                ? "cursor-not-allowed text-muted-foreground"
                                : "cursor-pointer hover:text-primary"
                            }`}
                            title={
                              usageSortBlocked
                                ? t("accounts.largePoolSortDisabled")
                                : t("accounts.todayStatsHint")
                            }
                            onClick={() => {
                              if (guardUsageSort("today")) return;
                              if (sortKey === "today") {
                                setSortDir((d) =>
                                  d === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("today");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.todayStats")}{" "}
                            {sortKey === "today"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.requests && (
                          <TableHead
                            className={`text-[13px] font-semibold select-none transition-colors ${
                              usageSortBlocked
                                ? "cursor-not-allowed text-muted-foreground"
                                : "cursor-pointer hover:text-primary"
                            }`}
                            title={
                              usageSortBlocked
                                ? t("accounts.largePoolSortDisabled")
                                : undefined
                            }
                            onClick={() => {
                              if (guardUsageSort("requests")) return;
                              if (sortKey === "requests") {
                                setSortDir((d) =>
                                  d === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("requests");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.requests")}{" "}
                            {sortKey === "requests"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.usage && (
                          <TableHead
                            className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                            onClick={() => {
                              if (sortKey === "usage") {
                                setSortDir((d) =>
                                  d === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("usage");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.usage")}{" "}
                            {sortKey === "usage"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.billed && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.billed")}
                          </TableHead>
                        )}
                        {visibleColumns.importTime && (
                          <TableHead
                            className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                            onClick={() => {
                              if (sortKey === "importTime") {
                                setSortDir((d) =>
                                  d === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("importTime");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.importTime")}{" "}
                            {sortKey === "importTime"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.updatedAt && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.updatedAt")}
                          </TableHead>
                        )}
                        {visibleColumns.actions && (
                          <TableHead className="text-[13px] font-semibold text-right">
                            {t("accounts.actions")}
                          </TableHead>
                        )}
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {pagedAccounts.map((account, index) => (
                        <AccountTableRow
                          key={account.id}
                          account={account}
                          sequence={(currentPage - 1) * pageSize + index + 1}
                          selected={selected.has(account.id)}
                          detailOpen={detailAccountId === account.id}
                          visibleColumns={visibleColumns}
                          showEmailDomainTags={showEmailDomainTags}
                          allGroups={allGroups}
                          proxyCtx={proxyBindingCtx}
                          healthBuckets={healthBars[String(account.id)]}
                          lazyMode={lazyMode}
                          refreshing={refreshingIds.has(account.id)}
                          authJsonExporting={authJsonExportingIds.has(account.id)}
                          t={t}
                          actions={rowActions}
                        />
                      ))}
                    </TableBody>
                  </Table>
                  </div>
                ) : null}
                <Pagination
                  page={currentPage}
                  totalPages={totalPages}
                  onPageChange={setPage}
                  totalItems={data.total}
                  pageSize={pageSize}
                  pageSizeOptions={pageSizeOptions}
                  onPageSizeChange={(nextPageSize) => {
                    setPageSize(nextPageSize);
                    setPage(1);
                  }}
                />
              </StateShell>
            </CardContent>
          </Card>

          <Modal
            show={showAdd}
            title={t("accounts.addTitle")}
            contentClassName="sm:max-w-[780px]"
            onClose={() => {
              setShowAdd(false);
              setAllowDuplicate(false);
              setAddMethod("oauth");
              setOauthStep("generate");
              setOauthSession(null);
              setOauthCallbackUrl("");
              setOauthName("");
              setOpenAIForm({
                base_url: "https://api.openai.com",
                api_key: "",
                models: [],
                proxy_url: "",
              });
              setOpenAIModelDraft("");
              setOpenAIModelMappingText("");
              setOpenAIModelMappingMode("form");
              setOpenAIModelMappingEntries(emptyModelMappingEntries());
              setSessionJson("");
              setSessionProxyUrl("");
              setAddCustomHeadersText("");
              setWorkspaceRouteID("");
            }}
            footer={
              <>
                {(addMethod === "rt" ||
                  addMethod === "st" ||
                  addMethod === "at" ||
                  addMethod === "session") && (
                  <label className="mr-auto flex cursor-pointer items-center gap-2 text-xs text-muted-foreground">
                    <input
                      type="checkbox"
                      className="size-3.5"
                      checked={allowDuplicate}
                      onChange={(e) => setAllowDuplicate(e.target.checked)}
                    />
                    {t("accounts.allowDuplicate")}
                  </label>
                )}
                <Button
                  variant="outline"
                  onClick={() => {
                    setShowAdd(false);
                    setAllowDuplicate(false);
                    setAddMethod("oauth");
                    setOauthStep("generate");
                    setOauthSession(null);
                    setOauthCallbackUrl("");
                    setOauthName("");
                    setOpenAIForm({
                      base_url: "https://api.openai.com",
                      api_key: "",
                      models: [],
                      proxy_url: "",
                    });
                    setOpenAIModelDraft("");
                    setOpenAIModelMappingText("");
                    setOpenAIModelMappingMode("form");
                    setOpenAIModelMappingEntries(emptyModelMappingEntries());
                    setSessionJson("");
                    setSessionProxyUrl("");
                    setAddCustomHeadersText("");
                    setWorkspaceRouteID("");
                  }}
                >
                  {t("common.cancel")}
                </Button>
                {addMethod === "rt" ? (
                  <Button
                    onClick={() => void handleAdd()}
                    disabled={submitting || !addForm.refresh_token?.trim()}
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : addMethod === "st" ? (
                  <Button
                    onClick={() => void handleAdd("st")}
                    disabled={submitting || !addForm.session_token?.trim()}
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : addMethod === "at" ? (
                  <Button
                    onClick={() => void handleAddAT()}
                    disabled={submitting || !atForm.access_token.trim()}
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : addMethod === "session" ? (
                  <Button
                    onClick={() => void handleAddSession()}
                    disabled={importing || submitting || !sessionJson.trim()}
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : addMethod === "openai" ? (
                  <Button
                    onClick={() => void handleAddOpenAIResponses()}
                    disabled={
                      submitting ||
                      !openAIForm.api_key.trim() ||
                      openAIForm.models.length === 0
                    }
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : addMethod === "agentIdentity" ? (
                  <Button
                    onClick={() => void handleAddAgentIdentity()}
                    disabled={submitting || !agentIdentityJson.trim()}
                  >
                    {submitting
                      ? t("accounts.adding")
                      : t("accounts.agentIdentityImportBtn")}
                  </Button>
                ) : oauthStep === "generate" ? (
                  <Button
                    onClick={() => void handleOAuthGenerate()}
                    disabled={oauthGenerating}
                  >
                    {oauthGenerating
                      ? t("accounts.oauthGenerating")
                      : t("accounts.oauthGenerateBtn")}
                  </Button>
                ) : (
                  <Button
                    onClick={() => void handleOAuthComplete()}
                    disabled={oauthCompleting || !oauthCallbackUrl.trim()}
                  >
                    {oauthCompleting
                      ? t("accounts.oauthCompleting")
                      : t("accounts.oauthCompleteBtn")}
                  </Button>
                )}
              </>
            }
          >
            {/* Tab switcher */}
            <div className="grid grid-cols-2 sm:grid-cols-4 gap-1 p-1 mb-5 rounded-xl bg-muted/50 border border-border">
              <button
                onClick={() => {
                  setAddMethod("oauth");
                  setOauthStep("generate");
                  setOauthSession(null);
                  setOauthCallbackUrl("");
                }}
                className={`min-w-0 flex-1 flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold whitespace-nowrap transition-all ${
                  addMethod === "oauth"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <KeyRound className="size-3.5" />
                {t("accounts.addMethodOAuth")}
              </button>
              <button
                onClick={() => setAddMethod("rt")}
                className={`min-w-0 flex-1 flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold whitespace-nowrap transition-all ${
                  addMethod === "rt"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <RefreshCw className="size-3.5" />
                {t("accounts.addMethodRT")}
              </button>
              <button
                onClick={() => setAddMethod("st")}
                className={`min-w-0 flex-1 flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold whitespace-nowrap transition-all ${
                  addMethod === "st"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <Cookie className="size-3.5" />
                {t("accounts.addMethodSessionToken")}
              </button>
              <button
                onClick={() => setAddMethod("at")}
                className={`min-w-0 flex-1 flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold whitespace-nowrap transition-all ${
                  addMethod === "at"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <Fingerprint className="size-3.5" />
                {t("accounts.addMethodAT")}
              </button>
              <button
                onClick={() => setAddMethod("session")}
                className={`min-w-0 flex-1 flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold whitespace-nowrap transition-all ${
                  addMethod === "session"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <ExternalLink className="size-3.5" />
                {t("accounts.addMethodSession")}
              </button>
              <button
                onClick={() => setAddMethod("openai")}
                className={`min-w-0 flex-1 flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold whitespace-nowrap transition-all ${
                  addMethod === "openai"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <KeyRound className="size-3.5" />
                {t("accounts.addMethodOpenAI")}
              </button>
              <button
                onClick={() => setAddMethod("agentIdentity")}
                className={`min-w-0 flex-1 flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold whitespace-nowrap transition-all ${
                  addMethod === "agentIdentity"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <Fingerprint className="size-3.5" />
                {t("accounts.addMethodAgentIdentity")}
              </button>
            </div>

            {addMethod === "rt" ? (
              <div className="space-y-4">
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.refreshTokenLabel")} *
                  </label>
                  <textarea
                    className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                    placeholder={t("accounts.refreshTokenPlaceholder")}
                    value={addForm.refresh_token ?? ""}
                    onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                      setAddForm((form) => ({
                        ...form,
                        refresh_token: event.target.value,
                      }))
                    }
                    rows={6}
                  />
                </div>
                {renderProxyInput({
                  value: addForm.proxy_url,
                  onChange: (value) =>
                    setAddForm((form) => ({
                      ...form,
                      proxy_url: value,
                    })),
                })}
                {renderWorkspaceRouteInput()}
                {renderCustomHeadersTextarea({
                  value: addCustomHeadersText,
                  onChange: setAddCustomHeadersText,
                })}
              </div>
            ) : addMethod === "st" ? (
              <div className="space-y-4">
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.sessionTokenLabel")} *
                  </label>
                  <textarea
                    className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                    placeholder={t("accounts.sessionTokenPlaceholder")}
                    value={addForm.session_token ?? ""}
                    onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                      setAddForm((form) => ({
                        ...form,
                        session_token: event.target.value,
                      }))
                    }
                    rows={6}
                  />
                </div>
                {renderProxyInput({
                  value: addForm.proxy_url,
                  onChange: (value) =>
                    setAddForm((form) => ({
                      ...form,
                      proxy_url: value,
                    })),
                })}
                {renderWorkspaceRouteInput()}
                {renderCustomHeadersTextarea({
                  value: addCustomHeadersText,
                  onChange: setAddCustomHeadersText,
                })}
              </div>
            ) : addMethod === "at" ? (
              <div className="space-y-4">
                <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-800 dark:bg-amber-950/50 dark:text-amber-300">
                  {t("accounts.atWarning")}
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.accessTokenLabel")} *
                  </label>
                  <textarea
                    className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                    placeholder={t("accounts.accessTokenPlaceholder")}
                    value={atForm.access_token}
                    onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                      setAtForm((form) => ({
                        ...form,
                        access_token: event.target.value,
                      }))
                    }
                    rows={6}
                  />
                </div>
                {renderProxyInput({
                  value: atForm.proxy_url,
                  onChange: (value) =>
                    setAtForm((form) => ({
                      ...form,
                      proxy_url: value,
                    })),
                })}
                {renderWorkspaceRouteInput()}
                {renderCustomHeadersTextarea({
                  value: addCustomHeadersText,
                  onChange: setAddCustomHeadersText,
                })}
              </div>
            ) : addMethod === "session" ? (
              <div className="space-y-4">
                <div className="rounded-xl border border-teal-200 bg-teal-50 px-4 py-3 text-sm text-teal-800 dark:border-teal-800 dark:bg-teal-950/50 dark:text-teal-300">
                  {t("accounts.sessionHint")}
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.sessionJsonLabel")} *
                  </label>
                  <textarea
                    className="w-full min-h-[260px] p-3 border border-input rounded-xl bg-background text-sm resize-y font-mono focus:outline-none focus:ring-2 focus:ring-ring"
                    placeholder={t("accounts.sessionJsonPlaceholder")}
                    value={sessionJson}
                    onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                      setSessionJson(event.target.value)
                    }
                    rows={10}
                  />
                </div>
                {renderProxyInput({
                  value: sessionProxyUrl,
                  label: t("accounts.importProxyLabel"),
                  onChange: setSessionProxyUrl,
                })}
                {renderWorkspaceRouteInput()}
                {renderCustomHeadersTextarea({
                  value: addCustomHeadersText,
                  onChange: setAddCustomHeadersText,
                })}
              </div>
            ) : addMethod === "openai" ? (
              <div className="space-y-4">
                <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                  <p className="font-semibold text-foreground mb-1">
                    {t("accounts.openaiResponsesTitle")}
                  </p>
                  <p>{t("accounts.openaiResponsesDesc")}</p>
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.openaiNameLabel")}
                  </label>
                  <Input
                    placeholder={t("accounts.openaiNamePlaceholder")}
                    value={openAIForm.name ?? ""}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        name: event.target.value,
                      }))
                    }
                  />
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.openaiBaseUrl")} *
                  </label>
                  <Input
                    placeholder="https://api.openai.com"
                    value={openAIForm.base_url}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        base_url: event.target.value,
                      }))
                    }
                  />
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.openaiApiKey")} *
                  </label>
                  <Input
                    type="password"
                    placeholder="sk-proj-..."
                    value={openAIForm.api_key}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        api_key: event.target.value,
                      }))
                    }
                  />
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.apiBalanceQueryUrl")}
                  </label>
                  <Input
                    placeholder="/v1/usage"
                    value={openAIForm.balance_query_url ?? ""}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        balance_query_url: event.target.value,
                      }))
                    }
                  />
                  <p className="mt-1.5 text-xs text-muted-foreground">
                    {t("accounts.apiBalanceQueryUrlHint")}
                  </p>
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.codexClientMetadataMode")}
                  </label>
                  <Select
                    value={
                      openAIForm.codex_client_metadata_mode ?? "auto"
                    }
                    onValueChange={(value) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        codex_client_metadata_mode:
                          value as CodexClientMetadataMode,
                      }))
                    }
                    options={[
                      {
                        value: "auto",
                        label: t("accounts.codexClientMetadataAuto"),
                      },
                      {
                        value: "always",
                        label: t("accounts.codexClientMetadataAlways"),
                      },
                      {
                        value: "off",
                        label: t("accounts.codexClientMetadataOff"),
                      },
                    ]}
                  />
                </div>
                <div>
                  <div className="mb-2 flex items-center justify-between gap-2">
                    <label className="text-sm font-semibold text-muted-foreground">
                      {t("accounts.openaiModels")} *
                    </label>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => void handleFetchOpenAIModels()}
                      disabled={
                        openAIModelsLoading || !openAIForm.api_key.trim()
                      }
                    >
                      <RefreshCw
                        className={`size-3.5 ${openAIModelsLoading ? "animate-spin" : ""}`}
                      />
                      {openAIModelsLoading
                        ? t("accounts.openaiModelsFetching")
                        : t("accounts.openaiModelsFetch")}
                    </Button>
                  </div>
                  <div className="mb-3 flex gap-2">
                    <Input
                      placeholder={t("accounts.openaiModelsPlaceholder")}
                      value={openAIModelDraft}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setOpenAIModelDraft(event.target.value)
                      }
                      onKeyDown={(event) => {
                        if (event.key === "Enter") {
                          event.preventDefault();
                          addOpenAIModelValues(openAIModelDraft);
                        }
                      }}
                      onPaste={(event) => {
                        const pasted = event.clipboardData.getData("text");
                        if (parseModelTokens(pasted).length > 1) {
                          event.preventDefault();
                          addOpenAIModelValues(pasted);
                        }
                      }}
                    />
                    <Button
                      type="button"
                      variant="outline"
                      onClick={() => addOpenAIModelValues(openAIModelDraft)}
                      disabled={!openAIModelDraft.trim()}
                    >
                      <Plus className="size-3.5" />
                      {t("accounts.openaiModelsAdd")}
                    </Button>
                  </div>
                  <ModelChipGrid
                    models={openAIForm.models}
                    onRemove={removeOpenAIModel}
                    emptyLabel={t("accounts.openaiModelsEmpty")}
                  />
                  <div className="mt-1.5 flex items-center justify-between gap-2">
                    <p className="text-xs text-muted-foreground">
                      {t("accounts.openaiModelsHint", {
                        count: openAIForm.models.length,
                      })}
                    </p>
                    {openAIForm.models.length > 0 && (
                      <button
                        type="button"
                        className="shrink-0 text-xs text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
                        disabled={openAIModelsLoading || submitting}
                        onClick={clearOpenAIModels}
                      >
                        {t("accounts.supportedModelsClearAll")}
                      </button>
                    )}
                  </div>
                </div>
                {renderModelMappingEditor({
                  value: openAIModelMappingText,
                  onChange: setOpenAIModelMappingText,
                  mode: openAIModelMappingMode,
                  onModeChange: setOpenAIModelMappingMode,
                  entries: openAIModelMappingEntries,
                  onEntriesChange: setOpenAIModelMappingEntries,
                })}
                {renderProxyInput({
                  value: openAIForm.proxy_url,
                  onChange: (value) =>
                    setOpenAIForm((form) => ({
                      ...form,
                      proxy_url: value,
                    })),
                })}
                {renderCustomHeadersTextarea({
                  value: addCustomHeadersText,
                  onChange: setAddCustomHeadersText,
                })}
              </div>
            ) : addMethod === "agentIdentity" ? (
              <div className="space-y-4">
                <div className="rounded-xl border border-blue-200 bg-blue-50 px-4 py-3 text-sm text-blue-800 dark:border-blue-800 dark:bg-blue-950/50 dark:text-blue-300">
                  {t("accounts.agentIdentityHint")}
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.agentIdentityJsonLabel")} *
                  </label>
                  <textarea
                    className="w-full min-h-[240px] p-3 border border-input rounded-xl bg-background text-sm resize-y font-mono focus:outline-none focus:ring-2 focus:ring-ring"
                    placeholder={t("accounts.agentIdentityJsonPlaceholder")}
                    value={agentIdentityJson}
                    onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                      setAgentIdentityJson(event.target.value)
                    }
                    rows={10}
                  />
                  <p className="mt-1.5 text-xs text-muted-foreground">
                    {t("accounts.agentIdentityNote")}
                  </p>
                </div>

                {/* 多文件批量导入 */}
                <div className="rounded-xl border border-dashed border-border bg-muted/20 px-4 py-3">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="min-w-0">
                      <p className="text-sm font-semibold text-foreground">
                        {t("accounts.agentIdentityFileImportTitle")}
                      </p>
                      <p className="mt-0.5 text-xs text-muted-foreground">
                        {t("accounts.agentIdentityFileImportDesc")}
                      </p>
                    </div>
                    <input
                      ref={agentIdentityFileInputRef}
                      type="file"
                      accept=".json,application/json"
                      multiple
                      className="hidden"
                      onChange={(e) =>
                        void handleImportAgentIdentityFiles(e.target.files)
                      }
                    />
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      disabled={agentIdentityFilesImporting}
                      onClick={() => agentIdentityFileInputRef.current?.click()}
                    >
                      {agentIdentityFilesImporting ? (
                        <>
                          <Loader2 className="size-3.5 animate-spin" />
                          {t("accounts.agentIdentityFileImporting")}
                        </>
                      ) : (
                        <>
                          <Upload className="size-3.5" />
                          {t("accounts.agentIdentityFileImportBtn")}
                        </>
                      )}
                    </Button>
                  </div>
                  {agentIdentityFilesResult ? (
                    <div className="mt-3 space-y-2 border-t border-border pt-3">
                      <p className="text-sm font-semibold text-foreground">
                        {t("accounts.agentIdentityFileImportSummary", {
                          imported: agentIdentityFilesResult.imported,
                          total: agentIdentityFilesResult.total,
                        })}
                      </p>
                      <div className="max-h-40 space-y-1 overflow-y-auto">
                        {agentIdentityFilesResult.items.map((item, index) => (
                          <div
                            key={index}
                            className="flex items-start gap-1.5 text-xs"
                          >
                            {item.ok ? (
                              <CheckCircle className="mt-0.5 size-3.5 shrink-0 text-emerald-500" />
                            ) : (
                              <XCircle className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                            )}
                            <span className="min-w-0 flex-1 break-all text-muted-foreground">
                              {item.email || `#${index + 1}`}
                              {item.ok
                                ? null
                                : item.error
                                  ? ` — ${item.error}`
                                  : ""}
                            </span>
                          </div>
                        ))}
                      </div>
                    </div>
                  ) : null}
                </div>

                {renderProxyInput({
                  value: agentIdentityProxyUrl,
                  onChange: setAgentIdentityProxyUrl,
                })}
              </div>
            ) : (
              <div className="space-y-5">
                {oauthStep === "generate" ? (
                  <>
                    <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                      <p className="font-semibold text-foreground mb-1">
                        {t("accounts.oauthStep1Title")}
                      </p>
                      <p>{t("accounts.oauthStep1Desc")}</p>
                    </div>
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.oauthNameLabel")}
                      </label>
                      <Input
                        placeholder={t("accounts.oauthNamePlaceholder")}
                        value={oauthName}
                        onChange={(e: ChangeEvent<HTMLInputElement>) =>
                          setOauthName(e.target.value)
                        }
                      />
                    </div>
                    {renderProxyInput({
                      value: oauthProxyUrl,
                      label: t("accounts.oauthProxyUrl"),
                      placeholder: t("accounts.oauthProxyUrlPlaceholder"),
                      onChange: setOauthProxyUrl,
                    })}
                  </>
                ) : (
                  <>
                    <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                      <p className="font-semibold text-foreground mb-1">
                        {t("accounts.oauthStep2Title")}
                      </p>
                      <p>{t("accounts.oauthStep2Desc")}</p>
                    </div>
                    {oauthSession && (
                      <div className="rounded-xl border border-primary/30 bg-primary/5 px-4 py-3">
                        <p className="text-xs font-semibold text-muted-foreground mb-2">
                          {t("accounts.oauthOpenLink")}
                        </p>
                        <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-start">
                          <a
                            href={oauthSession.auth_url}
                            target="_blank"
                            rel="noopener noreferrer"
                            title={oauthSession.auth_url}
                            className="inline-flex min-h-10 min-w-0 max-w-full flex-1 items-start gap-1.5 overflow-hidden rounded-lg border bg-background px-3 py-2 text-sm font-semibold text-primary hover:bg-muted/50"
                          >
                            <ExternalLink className="mt-0.5 size-3.5 shrink-0" />
                            <span className="block min-w-0 flex-1 break-all leading-relaxed [overflow-wrap:anywhere]">
                              {oauthSession.auth_url}
                            </span>
                          </a>
                          <Button
                            type="button"
                            variant="outline"
                            onClick={() => void handleOAuthCopyLink()}
                            className="w-full shrink-0 sm:w-auto"
                          >
                            <Copy className="size-4" />
                            {t("common.copy")}
                          </Button>
                        </div>
                      </div>
                    )}
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.oauthCallbackUrlLabel")}
                      </label>
                      <Input
                        placeholder={t("accounts.oauthCallbackUrlPlaceholder")}
                        value={oauthCallbackUrl}
                        onChange={(e: ChangeEvent<HTMLInputElement>) =>
                          setOauthCallbackUrl(e.target.value)
                        }
                      />
                      <p className="mt-1.5 text-xs text-muted-foreground">
                        {t("accounts.oauthCallbackUrlHint")}
                      </p>
                    </div>
                    <button
                      type="button"
                      onClick={() => void handleOAuthRestart()}
                      disabled={oauthGenerating}
                      className="text-xs text-muted-foreground hover:text-foreground underline underline-offset-2"
                    >
                      {oauthGenerating
                        ? t("accounts.oauthGenerating")
                        : t("accounts.oauthRestart")}
                    </button>
                  </>
                )}
              </div>
            )}
            {(addMethod === "rt" ||
              addMethod === "st" ||
              addMethod === "at") && (
              <div className="mt-4 space-y-1.5 border-t border-border pt-4">
                <label className="text-sm font-semibold text-muted-foreground">
                  {t("accounts.importGroupsLabel")}
                </label>
                <AccountGroupMultiSelect
                  groups={codexGroups}
                  value={importGroupIds}
                  onChange={setImportGroupIds}
                  allLabel={t("accounts.groupsUnbound")}
                  selectedLabel={t("accounts.groupsSelected", {
                    count: importGroupIds.length,
                  })}
                  placeholder={t("accounts.importGroupsPlaceholder")}
                  emptyLabel={t("accounts.groupsNone")}
                  emptyHint={t("accounts.groupsSelectHint")}
                  onCreateGroup={handleCreateGroupInline}
                  createLabel={t("accounts.groupCreate")}
                  createPlaceholder={t("accounts.groupNamePlaceholder")}
                  creatingLabel={t("accounts.groupCreating")}
                  createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
                />
                <p className="text-[11px] text-muted-foreground">
                  {t("accounts.importGroupsHint")}
                </p>
              </div>
            )}
          </Modal>

          <Modal
            show={showImportPicker}
            title={t("accounts.importTitle")}
            contentClassName="sm:max-w-[640px]"
            onClose={() => {
              setShowImportPicker(false);
              setShowPasteImport(false);
              setPasteImportText('');
            }}
          >
            <div className="mb-4 space-y-1.5">
              {renderProxyInput({
                value: importProxyUrl,
                label: t("accounts.importProxyLabel"),
                onChange: setImportProxyUrl,
              })}
              <p className="text-[11px] text-muted-foreground">
                {t("accounts.importProxyHint")}
              </p>
              {renderCustomHeadersTextarea({
                value: importCustomHeadersText,
                onChange: setImportCustomHeadersText,
              })}
              {renderWorkspaceRouteInput({
                value: importWorkspaceRouteID,
                onChange: setImportWorkspaceRouteID,
              })}
              <label className="flex cursor-pointer items-center gap-2 pt-1 text-xs text-muted-foreground">
                <input
                  type="checkbox"
                  className="size-3.5"
                  checked={allowDuplicate}
                  onChange={(e) => setAllowDuplicate(e.target.checked)}
                />
                {t("accounts.allowDuplicate")}
              </label>
              <label className="flex cursor-pointer items-center gap-2 text-xs text-muted-foreground">
                <input
                  type="checkbox"
                  className="size-3.5"
                  checked={importFileProxies}
                  onChange={(e) => setImportFileProxies(e.target.checked)}
                />
                {t("accounts.importFileProxies")}
              </label>
              <p className="text-[11px] text-muted-foreground">
                {t("accounts.importFileProxiesHint")}
              </p>
              <div className="space-y-1.5 pt-1">
                <label className="text-xs font-medium text-foreground">
                  {t("accounts.importGroupsLabel")}
                </label>
                <AccountGroupMultiSelect
                  groups={codexGroups}
                  value={importGroupIds}
                  onChange={setImportGroupIds}
                  allLabel={t("accounts.groupsUnbound")}
                  selectedLabel={t("accounts.groupsSelected", {
                    count: importGroupIds.length,
                  })}
                  placeholder={t("accounts.importGroupsPlaceholder")}
                  emptyLabel={t("accounts.groupsNone")}
                  emptyHint={t("accounts.groupsSelectHint")}
                  onCreateGroup={handleCreateGroupInline}
                  createLabel={t("accounts.groupCreate")}
                  createPlaceholder={t("accounts.groupNamePlaceholder")}
                  creatingLabel={t("accounts.groupCreating")}
                  createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
                />
                <p className="text-[11px] text-muted-foreground">
                  {t("accounts.importGroupsHint")}
                </p>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  fileInputRef.current?.click();
                }}
              >
                <FileText className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importTxt")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importTxtDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  jsonInputRef.current?.click();
                }}
              >
                <FileJson className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importJson")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importJsonDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  jsonAtInputRef.current?.click();
                }}
              >
                <FileJson className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importJsonAt")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importJsonAtDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  atFileInputRef.current?.click();
                }}
              >
                <Fingerprint className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importAtTxt")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importAtTxtDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  folderInputRef.current?.click();
                }}
              >
                <FolderOpen className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importFolder")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importFolderDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowPasteImport(true);
                  setPasteImportText("");
                }}
              >
                <Copy className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importPasteText")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importPasteTextDesc")}
                  </div>
                </div>
              </button>
            </div>

            {showPasteImport && (
              <div className="mt-4 space-y-3">
                <textarea
                  className="w-full min-h-[240px] p-3 border border-input rounded-xl bg-background text-sm resize-y font-mono focus:outline-none focus:ring-2 focus:ring-ring"
                  placeholder={t("accounts.sessionJsonPlaceholder")}
                  value={pasteImportText}
                  onChange={(e) => setPasteImportText(e.target.value)}
                  rows={10}
                />
                <div className="flex justify-end gap-2">
                  <Button
                    variant="outline"
                    onClick={() => {
                      setShowPasteImport(false);
                      setPasteImportText("");
                    }}
                  >
                    {t("common.cancel")}
                  </Button>
                  <Button
                    onClick={() => void handlePasteImport()}
                    disabled={importing || !pasteImportText.trim()}
                  >
                    {t("accounts.submit")}
                  </Button>
                </div>
              </div>
            )}
          </Modal>
          <Sub2APIImportModal
            show={showSub2APIImport}
            onClose={() => setShowSub2APIImport(false)}
            onImportStart={async (res) => {
              setImporting(true);
              try {
                await readImportSSE(res);
              } finally {
                setImporting(false);
              }
            }}
            onShowToast={(message, kind) => showToast(message, kind ?? "success")}
          />

          <Modal
            show={showExportPicker}
            title={t("accounts.exportTitle")}
            contentClassName="sm:max-w-[580px]"
            onClose={() => setShowExportPicker(false)}
          >
            <div className="space-y-4">
              {/* 健康账号导出 */}
              <div>
                <div className="text-xs font-semibold text-muted-foreground mb-2">
                  {t("accounts.exportScopeHealthy")}
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                    onClick={() => void handleExport("json", "healthy")}
                  >
                    <FileJson className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">CPA JSON</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportHealthyJsonDesc")}
                      </div>
                    </div>
                  </button>
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                    onClick={() => void handleExport("txt", "healthy")}
                  >
                    <FileText className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">TXT</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportHealthyTxtDesc")}
                      </div>
                    </div>
                  </button>
                </div>
              </div>
              {/* 已选账号导出 */}
              <div>
                <div className="text-xs font-semibold text-muted-foreground mb-2">
                  {t("accounts.exportScopeSelected", { count: selected.size })}
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
                    disabled={selected.size === 0}
                    onClick={() => void handleExport("json", "selected")}
                  >
                    <FileJson className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">CPA JSON</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportSelectedJsonDesc")}
                      </div>
                    </div>
                  </button>
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
                    disabled={selected.size === 0}
                    onClick={() => void handleExport("txt", "selected")}
                  >
                    <FileText className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">TXT</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportSelectedTxtDesc")}
                      </div>
                    </div>
                  </button>
                </div>
              </div>
              {/* 代理配置只对 JSON 导出生效，TXT 只有 refresh_token 一列。 */}
              <div className="border-t border-border pt-3">
                <label className="flex cursor-pointer items-start gap-2 text-xs text-muted-foreground">
                  <input
                    type="checkbox"
                    className="mt-0.5 size-3.5 shrink-0"
                    checked={exportIncludeProxy}
                    onChange={(e) => setExportIncludeProxy(e.target.checked)}
                  />
                  <span className="min-w-0">
                    <span className="font-medium text-foreground">
                      {t("accounts.exportIncludeProxy")}
                    </span>
                    <span className="mt-0.5 block text-[11px] text-muted-foreground">
                      {t("accounts.exportIncludeProxyHint")}
                    </span>
                  </span>
                </label>
                {exportIncludeProxy && (
                  <p className="mt-2 rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-[11px] text-amber-600 dark:text-amber-400">
                    {t("accounts.exportIncludeProxyWarning")}
                  </p>
                )}
              </div>
            </div>
          </Modal>

          <Modal
            show={Boolean(authJsonModal)}
            title={t("accounts.authJsonModalTitle")}
            contentClassName="sm:max-w-[720px]"
            onClose={() => setAuthJsonModal(null)}
          >
            {authJsonModal && (
              <div className="space-y-4">
                <div className="flex items-start gap-3 rounded-lg border border-border bg-muted/30 px-4 py-3">
                  <FileJson className="mt-0.5 size-5 shrink-0 text-primary" />
                  <div className="min-w-0 space-y-1">
                    <div className="text-sm font-semibold text-foreground">
                      {authJsonModal.account.email ||
                        `ID ${authJsonModal.account.id}`}
                    </div>
                    <p className="text-xs leading-relaxed text-muted-foreground">
                      {t("accounts.authJsonModalDesc")}
                    </p>
                  </div>
                </div>

                <div>
                  <div className="mb-2 text-xs font-semibold text-muted-foreground">
                    {t("accounts.authJsonPreview")}
                  </div>
                  <textarea
                    readOnly
                    value={authJsonModal.json}
                    className="min-h-[260px] w-full resize-y rounded-lg border border-border bg-muted/30 p-3 text-[12px] leading-relaxed text-muted-foreground outline-none focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                    style={{ fontFamily: "var(--font-geist-mono)" }}
                  />
                </div>

                <div className="flex flex-wrap justify-end gap-2">
                  <Button
                    variant="outline"
                    onClick={() => setAuthJsonModal(null)}
                  >
                    {t("common.close")}
                  </Button>
                  <Button
                    variant="outline"
                    onClick={() => void handleCopyAuthJSON()}
                  >
                    <Copy className="size-4" />
                    {t("accounts.copyAuthJson")}
                  </Button>
                  <Button onClick={handleExportAuthJSON}>
                    <Download className="size-4" />
                    {t("accounts.exportAuthJson")}
                  </Button>
                </div>
              </div>
            )}
          </Modal>

          <Modal
            show={showMigrate}
            title={t("accounts.migrateTitle")}
            contentClassName="sm:max-w-[520px]"
            onClose={() => {
              setShowMigrate(false);
              setMigrateUrl("");
              setMigrateKey("");
            }}
          >
            <div className="space-y-4">
              <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                <p>{t("accounts.migrateDesc")}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                  {t("accounts.migrateUrlLabel")}
                </label>
                <Input
                  placeholder={t("accounts.migrateUrlPlaceholder")}
                  value={migrateUrl}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setMigrateUrl(e.target.value)
                  }
                />
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                  {t("accounts.migrateKeyLabel")}
                </label>
                <Input
                  type="password"
                  placeholder={t("accounts.migrateKeyPlaceholder")}
                  value={migrateKey}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setMigrateKey(e.target.value)
                  }
                />
              </div>
              <div className="flex justify-end gap-2 pt-2">
                <Button
                  variant="outline"
                  onClick={() => {
                    setShowMigrate(false);
                    setMigrateUrl("");
                    setMigrateKey("");
                  }}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  onClick={() => void handleMigrate()}
                  disabled={
                    migrating || !migrateUrl.trim() || !migrateKey.trim()
                  }
                >
                  {migrating
                    ? t("accounts.migrating")
                    : t("accounts.migrateConfirm")}
                </Button>
              </div>
            </div>
          </Modal>

          {testingAccount && (
            <TestConnectionModal
              account={testingAccount}
              onSettled={() => {
                // 标记该账号强制刷新用量，配合后台探针确保进度条更新为最新值。
                forceUsageReloadRef.current.add(testingAccount.id);
                usageReloadAttemptsRef.current.delete(testingAccount.id);
                void reloadSilently();
              }}
              onClose={() => setTestingAccount(null)}
            />
          )}

          {usageAccount && (
            <AccountUsageModal
              account={usageAccount}
              // key 让不同入口切换时重建弹窗,否则 initialPage 只在首次挂载生效,
              // 先开概览再点官方成本会停在旧 tab。
              key={`${usageAccount.id}-${usageInitialPage}`}
              initialPage={usageInitialPage}
              onClose={() => {
                setUsageAccount(null);
                setUsageInitialPage("overview");
              }}
              // 必须静默重拉：reload() 会把 StateShell 切成整页 loading，
              // 而这个弹窗就渲染在 StateShell 里 —— 一开积分开关整个界面连同弹窗
              // 就被卸载重建（用量数据重新拉、滚动位置丢失），观感就是"闪一下全刷新"。
              onCreditsReset={() => void reloadSilently()}
              onOfficialUsageRefreshed={handleOfficialUsageRefreshed}
            />
          )}

          <AccountDetailSheet
            account={detailAccount}
            groups={
              detailAccount
                ? resolveAccountGroups(detailAccount.group_ids ?? [], allGroups)
                : []
            }
            healthBuckets={
              detailAccount
                ? healthBars[String(detailAccount.id)]
                : undefined
            }
            sequence={
              detailNavIndex >= 0
                ? (currentPage - 1) * pageSize + detailNavIndex + 1
                : undefined
            }
            usageSlot={
              detailAccount ? (
                <UsageCell
                  account={detailAccount}
                  wide
                  onRefreshed={() => void reloadSilently()}
                />
              ) : null
            }
            canGoPrev={detailNavIndex > 0 || currentPage > 1}
            canGoNext={
              (detailNavIndex >= 0 && detailNavIndex < sortedAccounts.length - 1) ||
              currentPage < totalPages
            }
            refreshing={
              detailAccount
                ? refreshingIds.has(detailAccount.id)
                : false
            }
            authJsonExporting={
              detailAccount
                ? authJsonExportingIds.has(detailAccount.id)
                : false
            }
            onClose={closeAccountDetail}
            onPrev={goDetailPrev}
            onNext={goDetailNext}
            onQuickConfig={() => {
              if (!detailAccount) return;
              setQuickConfigAccount(detailAccount);
            }}
            onEdit={() => {
              if (!detailAccount) return;
              openSchedulerEditor(detailAccount);
            }}
            onUsage={() => {
              if (!detailAccount) return;
              setUsageAccount(detailAccount);
            }}
            onTest={() => {
              if (!detailAccount) return;
              openTestingAccount(detailAccount);
            }}
            onRefresh={() => {
              if (!detailAccount) return;
              void handleRefresh(detailAccount);
            }}
            onGenerateAuthJson={() => {
              if (!detailAccount) return;
              void handleGenerateAuthJSON(detailAccount);
            }}
            onToggleEnabled={() => {
              if (!detailAccount) return;
              void handleToggleEnabled(detailAccount);
            }}
            onToggleLock={() => {
              if (!detailAccount) return;
              void handleToggleLock(detailAccount);
            }}
            onResetStatus={() => {
              if (!detailAccount) return;
              void handleResetStatus(detailAccount);
            }}
            onSaveModelCooldownPolicy={(data) => {
              if (!detailAccount) return;
              void handleSaveModelCooldownPolicy(detailAccount, data);
            }}
            onClearModelCooldown={(model) => {
              if (!detailAccount) return;
              void handleClearModelCooldown(detailAccount, model);
            }}
            onClearAllModelCooldowns={() => {
              if (!detailAccount) return;
              void handleClearAllModelCooldowns(detailAccount);
            }}
            onResetCredits={() => {
              if (!detailAccount) return;
              void handleResetCredits(detailAccount);
            }}
            onDelete={() => {
              if (!detailAccount) return;
              void handleDelete(detailAccount);
            }}
          />

          <AccountQuickConfigSheet
            account={quickConfigAccount}
            groups={allGroups}
            show={Boolean(quickConfigAccount)}
            onClose={() => setQuickConfigAccount(null)}
            onSaved={() => void reloadSilently()}
          />

          <Modal
            show={Boolean(editingAccount)}
            title={t("accounts.schedulerEditTitle")}
            contentClassName="sm:max-w-[760px]"
            onClose={closeSchedulerEditor}
            footer={
              <>
                <Button
                  variant="outline"
                  onClick={() => closeSchedulerEditor()}
                  disabled={editSubmitting || editOAuthGenerating}
                >
                  {t("common.cancel")}
                </Button>
                {isOAuthAccount(editingAccount) && editTab === "account" ? (
                  <Button
                    onClick={() =>
                      editOAuthStep === "generate"
                        ? void handleEditOAuthGenerate()
                        : void handleUpdateOAuthAccount()
                    }
                    disabled={
                      editOAuthGenerating ||
                      editOAuthUpdating ||
                      (editOAuthStep === "exchange" &&
                        !editOAuthCallbackUrl.trim())
                    }
                  >
                    {editOAuthStep === "generate"
                      ? editOAuthGenerating
                        ? t("accounts.oauthGenerating")
                        : t("accounts.oauthGenerateBtn")
                      : editOAuthUpdating
                        ? t("accounts.oauthCompleting")
                        : t("accounts.oauthUpdateAuth")}
                  </Button>
                ) : (
                  <Button
                    onClick={() => void handleSaveAccountEditor()}
                    disabled={
                      editSubmitting ||
                      (editTab === "scheduler" &&
                        (scoreInputInvalid ||
                          concurrencyInputInvalid ||
                          editAutoPause5hThresholdInvalid ||
                          editAutoPause7dThresholdInvalid ||
                          editDispatchCountLimitInvalid ||
                          editSchedulerPriorityInvalid)) ||
                      openAIAccountInputInvalid
                    }
                  >
                    {editSubmitting ? t("common.saving") : t("common.save")}
                  </Button>
                )}
              </>
            }
          >
            {editingAccount && editPreview ? (
              <div className="space-y-6">
                {/* 顶栏：账号状态与简要概览 */}
                <div className="relative overflow-hidden rounded-xl border border-border/80 bg-gradient-to-r from-primary/5 via-muted/30 to-background p-4 shadow-2xs">
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <div className="min-w-0 space-y-1">
                      <div className="flex items-center gap-2">
                        <span className="text-base font-bold text-foreground tracking-tight">
                          {formatAccountName(editingAccount)}
                        </span>
                        <Badge variant="outline" className="capitalize text-xs font-semibold bg-background/80 border-border/80">
                          {editingAccount.plan_type || "Standard"}
                        </Badge>
                      </div>
                      <p className="text-xs text-muted-foreground">
                        {t("accounts.schedulerEditDesc", {
                          plan: editingAccount.plan_type || "-",
                        })}
                      </p>
                    </div>
                    <div className="flex items-center gap-2 shrink-0">
                      <span className="inline-flex items-center gap-1.5 rounded-full border border-primary/20 bg-primary/10 px-2.5 py-1 text-xs font-mono font-medium text-primary">
                        ID: {editingAccount.id}
                      </span>
                    </div>
                  </div>
                </div>

                {/* 选项卡切换 */}
                {(editingAccount.openai_responses_api ||
                  isOAuthAccount(editingAccount)) && (
                  <div className="flex gap-1.5 rounded-xl border border-border/60 bg-muted/40 p-1.5 shadow-2xs">
                    <button
                      type="button"
                      onClick={() => setEditTab("scheduler")}
                      className={`flex flex-1 items-center justify-center gap-2 rounded-lg px-3 py-2 text-sm font-semibold transition-all duration-150 ${
                        editTab === "scheduler"
                          ? "bg-background text-foreground shadow-xs ring-1 ring-border/50"
                          : "text-muted-foreground hover:text-foreground hover:bg-background/40"
                      }`}
                    >
                      <SlidersHorizontal className="size-4 text-primary" />
                      <span>{t("accounts.editTabScheduler")}</span>
                    </button>
                    <button
                      type="button"
                      onClick={() => setEditTab("account")}
                      className={`flex flex-1 items-center justify-center gap-2 rounded-lg px-3 py-2 text-sm font-semibold transition-all duration-150 ${
                        editTab === "account"
                          ? "bg-background text-foreground shadow-xs ring-1 ring-border/50"
                          : "text-muted-foreground hover:text-foreground hover:bg-background/40"
                      }`}
                    >
                      <KeyRound className="size-4 text-primary" />
                      <span>{t("accounts.editTabAccount")}</span>
                    </button>
                  </div>
                )}

                {editTab === "account" &&
                editingAccount.openai_responses_api ? (
                  <div className="space-y-4">
                    <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs space-y-4">
                      <div className="flex items-center gap-2 font-semibold text-foreground text-sm border-b border-border/50 pb-3">
                        <Settings2 className="size-4 text-primary" />
                        <span>OpenAI Responses API 参数</span>
                      </div>
                      <div>
                        <label className="block mb-2 text-xs font-semibold text-muted-foreground">
                          {t("accounts.openaiNameLabel")}
                        </label>
                        <Input
                          placeholder={t("accounts.openaiNamePlaceholder")}
                          value={editOpenAIForm.name ?? ""}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            setEditOpenAIForm((form) => ({
                              ...form,
                              name: event.target.value,
                            }))
                          }
                        />
                      </div>
                      <div>
                        <label className="block mb-2 text-xs font-semibold text-muted-foreground">
                          {t("accounts.openaiBaseUrl")} *
                        </label>
                        <Input
                          placeholder="https://api.openai.com"
                          value={editOpenAIForm.base_url}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            setEditOpenAIForm((form) => ({
                              ...form,
                              base_url: event.target.value,
                            }))
                          }
                        />
                      </div>
                      <div>
                        <label className="block mb-2 text-xs font-semibold text-muted-foreground">
                          {t("accounts.openaiApiKey")}
                        </label>
                        <Input
                          type="password"
                          placeholder={t("accounts.openaiApiKeyKeepPlaceholder")}
                          value={editOpenAIForm.api_key ?? ""}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            setEditOpenAIForm((form) => ({
                              ...form,
                              api_key: event.target.value,
                            }))
                          }
                        />
                      </div>
                      <div>
                        <label className="block mb-2 text-xs font-semibold text-muted-foreground">
                          {t("accounts.apiBalanceQueryUrl")}
                        </label>
                        <Input
                          placeholder="/v1/usage"
                          value={editOpenAIForm.balance_query_url ?? ""}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            setEditOpenAIForm((form) => ({
                              ...form,
                              balance_query_url: event.target.value,
                            }))
                          }
                        />
                        <p className="mt-1.5 text-xs text-muted-foreground">
                          {t("accounts.apiBalanceQueryUrlHint")}
                        </p>
                      </div>
                      <div>
                        <label className="block mb-2 text-xs font-semibold text-muted-foreground">
                          {t("accounts.codexClientMetadataMode")}
                        </label>
                        <Select
                          value={
                            editOpenAIForm.codex_client_metadata_mode ?? "auto"
                          }
                          onValueChange={(value) =>
                            setEditOpenAIForm((form) => ({
                              ...form,
                              codex_client_metadata_mode:
                                value as CodexClientMetadataMode,
                            }))
                          }
                          options={[
                            {
                              value: "auto",
                              label: t("accounts.codexClientMetadataAuto"),
                            },
                            {
                              value: "always",
                              label: t("accounts.codexClientMetadataAlways"),
                            },
                            {
                              value: "off",
                              label: t("accounts.codexClientMetadataOff"),
                            },
                          ]}
                        />
                      </div>
                    </div>

                    <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs space-y-4">
                      <div>
                        <div className="mb-2 flex items-center justify-between gap-2">
                          <label className="text-xs font-semibold text-muted-foreground">
                            {t("accounts.openaiModels")} *
                          </label>
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            onClick={() => void handleFetchEditOpenAIModels()}
                            disabled={editOpenAIModelsLoading}
                          >
                            <RefreshCw
                              className={`size-3.5 ${editOpenAIModelsLoading ? "animate-spin" : ""}`}
                            />
                            {editOpenAIModelsLoading
                              ? t("accounts.openaiModelsFetching")
                              : t("accounts.openaiModelsFetch")}
                          </Button>
                        </div>
                        <div className="mb-3 flex gap-2">
                          <Input
                            placeholder={t("accounts.openaiModelsPlaceholder")}
                            value={editOpenAIModelDraft}
                            onChange={(event: ChangeEvent<HTMLInputElement>) =>
                              setEditOpenAIModelDraft(event.target.value)
                            }
                            onKeyDown={(event) => {
                              if (event.key === "Enter") {
                                event.preventDefault();
                                addEditOpenAIModelValues(editOpenAIModelDraft);
                              }
                            }}
                            onPaste={(event) => {
                              const pasted = event.clipboardData.getData("text");
                              if (parseModelTokens(pasted).length > 1) {
                                event.preventDefault();
                                addEditOpenAIModelValues(pasted);
                              }
                            }}
                          />
                          <Button
                            type="button"
                            variant="outline"
                            onClick={() =>
                              addEditOpenAIModelValues(editOpenAIModelDraft)
                            }
                            disabled={!editOpenAIModelDraft.trim()}
                          >
                            <Plus className="size-3.5" />
                            {t("accounts.openaiModelsAdd")}
                          </Button>
                        </div>
                        <ModelChipGrid
                          models={editOpenAIForm.models}
                          onRemove={removeEditOpenAIModel}
                          emptyLabel={t("accounts.openaiModelsEmpty")}
                        />
                        <div className="mt-1.5 flex items-center justify-between gap-2">
                          <p className="text-xs text-muted-foreground">
                            {t("accounts.openaiModelsHint", {
                              count: editOpenAIForm.models.length,
                            })}
                          </p>
                          {editOpenAIForm.models.length > 0 && (
                            <button
                              type="button"
                              className="shrink-0 text-xs text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
                              disabled={editOpenAIModelsLoading || editSubmitting}
                              onClick={clearEditOpenAIModels}
                            >
                              {t("accounts.supportedModelsClearAll")}
                            </button>
                          )}
                        </div>
                      </div>
                      {renderModelMappingEditor({
                        value: editOpenAIModelMappingText,
                        onChange: setEditOpenAIModelMappingText,
                        mode: editOpenAIModelMappingMode,
                        onModeChange: setEditOpenAIModelMappingMode,
                        entries: editOpenAIModelMappingEntries,
                        onEntriesChange: setEditOpenAIModelMappingEntries,
                      })}
                    </div>

                    <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs space-y-4">
                      {renderProxyInput({
                        value: editOpenAIForm.proxy_url,
                        onChange: (value) =>
                          setEditOpenAIForm((form) => ({
                            ...form,
                            proxy_url: value,
                          })),
                      })}
                      {renderCustomHeadersTextarea({
                        value: editCustomHeadersText,
                        onChange: setEditCustomHeadersText,
                      })}
                    </div>
                  </div>
                ) : editTab === "account" && isOAuthAccount(editingAccount) ? (
                  <div className="space-y-4">
                    <div className="rounded-xl border border-border/80 bg-muted/30 p-4.5 text-sm text-muted-foreground">
                      <p className="font-semibold text-foreground mb-1">
                        {t("accounts.oauthEditIntroTitle")}
                      </p>
                      <p>{t("accounts.oauthEditIntroDesc")}</p>
                    </div>
                    <div>
                      <label className="block mb-2 text-xs font-semibold text-muted-foreground">
                        {t("accounts.oauthCurrentAccount")}
                      </label>
                      <div className="rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                        {formatAccountName(editingAccount)}
                      </div>
                    </div>
                    {editOAuthStep === "generate" ? (
                      <>
                        <div className="rounded-xl border border-border/80 bg-muted/30 p-4.5 text-sm text-muted-foreground">
                          <p className="font-semibold text-foreground mb-1">
                            {t("accounts.oauthStep1Title")}
                          </p>
                          <p>{t("accounts.oauthStep1Desc")}</p>
                        </div>
                        {renderProxyInput({
                          value: editOAuthProxyUrl,
                          label: t("accounts.oauthProxyUrl"),
                          placeholder: t("accounts.oauthProxyUrlPlaceholder"),
                          onChange: setEditOAuthProxyUrl,
                        })}
                      </>
                    ) : (
                      <>
                        <div className="rounded-xl border border-border/80 bg-muted/30 p-4.5 text-sm text-muted-foreground">
                          <p className="font-semibold text-foreground mb-1">
                            {t("accounts.oauthStep2Title")}
                          </p>
                          <p>{t("accounts.oauthStep2Desc")}</p>
                        </div>
                        {editOAuthSession && (
                          <div className="rounded-xl border border-primary/30 bg-primary/5 p-4.5">
                            <p className="text-xs font-semibold text-muted-foreground mb-2">
                              {t("accounts.oauthAuthLinkLabel")}
                            </p>
                            <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-start">
                              <a
                                href={editOAuthSession.auth_url}
                                target="_blank"
                                rel="noopener noreferrer"
                                title={editOAuthSession.auth_url}
                                className="inline-flex min-h-10 min-w-0 max-w-full flex-1 items-start gap-1.5 overflow-hidden rounded-lg border bg-background px-3 py-2 text-sm font-semibold text-primary hover:bg-muted/50"
                              >
                                <ExternalLink className="mt-0.5 size-3.5 shrink-0" />
                                <span className="block min-w-0 flex-1 break-all leading-relaxed [overflow-wrap:anywhere]">
                                  {editOAuthSession.auth_url}
                                </span>
                              </a>
                              <Button
                                type="button"
                                variant="outline"
                                onClick={() => void handleEditOAuthCopyLink()}
                                className="w-full shrink-0 sm:w-auto"
                              >
                                <Copy className="size-4" />
                                {t("common.copy")}
                              </Button>
                            </div>
                          </div>
                        )}
                        <div>
                          <label className="block mb-2 text-xs font-semibold text-muted-foreground">
                            {t("accounts.oauthCallbackUrlLabel")}
                          </label>
                          <Input
                            placeholder={t("accounts.oauthCallbackUrlPlaceholder")}
                            value={editOAuthCallbackUrl}
                            onChange={(event: ChangeEvent<HTMLInputElement>) =>
                              setEditOAuthCallbackUrl(event.target.value)
                            }
                          />
                          <p className="mt-1.5 text-xs text-muted-foreground">
                            {t("accounts.oauthCallbackUrlHint")}
                          </p>
                        </div>
                        <button
                          type="button"
                          onClick={() => void handleEditOAuthRestart()}
                          disabled={editOAuthGenerating}
                          className="text-xs text-muted-foreground hover:text-foreground underline underline-offset-2"
                        >
                          {editOAuthGenerating
                            ? t("accounts.oauthGenerating")
                            : t("accounts.oauthRestart")}
                        </button>
                      </>
                    )}
                  </div>
                ) : (
                  <div className="space-y-6">
                    {/* 分组 1: 调度与并发加权 */}
                    <div className="space-y-3">
                      <div className="flex items-center gap-2 text-xs font-bold uppercase tracking-wider text-muted-foreground/80">
                        <Gauge className="size-3.5 text-primary" />
                        <span>{t("accounts.categoryDispatch")}</span>
                        <div className="h-px flex-1 bg-border/60" />
                      </div>

                      <div className="grid gap-4 md:grid-cols-2">
                        {/* 权重分 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors">
                          <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                            <Gauge className="size-4 text-blue-500" />
                            <span>{t("accounts.schedulerScoreLabel")}</span>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                            {t("accounts.schedulerScoreHint")}
                          </p>
                          <div className="mt-3.5 flex gap-2">
                            <TogglePill
                              active={scoreMode === "default"}
                              onClick={() => setScoreMode("default")}
                              label={t("accounts.schedulerScoreAuto")}
                            />
                            <TogglePill
                              active={scoreMode === "custom"}
                              onClick={() => setScoreMode("custom")}
                              label={t("accounts.schedulerCustom")}
                            />
                          </div>
                          {scoreMode === "default" ? (
                            <div className="mt-3 flex items-center justify-between rounded-lg border border-border/60 bg-muted/20 px-3.5 py-2.5 text-xs">
                              <span className="text-muted-foreground font-medium">当前默认加权</span>
                              <span className="font-mono font-semibold text-blue-600 dark:text-blue-400 bg-blue-500/10 px-2 py-0.5 rounded border border-blue-500/20">
                                {formatSignedNumber(getDefaultScoreBias(editingAccount.plan_type))}
                              </span>
                            </div>
                          ) : (
                            <div className="mt-3 space-y-2">
                              <Input
                                inputMode="numeric"
                                value={scoreInput}
                                onChange={(
                                  event: ChangeEvent<HTMLInputElement>,
                                ) => setScoreInput(event.target.value)}
                                placeholder={t(
                                  "accounts.schedulerScorePlaceholder",
                                )}
                              />
                              <div
                                className={`text-xs ${scoreInputInvalid ? "text-red-500" : "text-muted-foreground"}`}
                              >
                                {scoreInputInvalid
                                  ? t("accounts.schedulerScoreRange")
                                  : t("accounts.schedulerCustomValuePreview", {
                                      value: formatSignedNumber(
                                        parsedScoreBias ??
                                          getEffectiveScoreBias(editingAccount),
                                      ),
                                    })}
                              </div>
                            </div>
                          )}
                        </div>

                        {/* 基础并发 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors">
                          <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                            <Zap className="size-4 text-amber-500" />
                            <span>{t("accounts.schedulerConcurrencyLabel")}</span>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                            {t("accounts.schedulerConcurrencyHint")}
                          </p>
                          <div className="mt-3.5 flex gap-2">
                            <TogglePill
                              active={concurrencyMode === "default"}
                              onClick={() => setConcurrencyMode("default")}
                              label={t("accounts.schedulerConcurrencyAuto")}
                            />
                            <TogglePill
                              active={concurrencyMode === "custom"}
                              onClick={() => setConcurrencyMode("custom")}
                              label={t("accounts.schedulerCustom")}
                            />
                          </div>
                          {concurrencyMode === "default" ? (
                            <div className="mt-3 flex items-center justify-between rounded-lg border border-border/60 bg-muted/20 px-3.5 py-2.5 text-xs">
                              <span className="text-muted-foreground font-medium">当前基础并发</span>
                              <span className="font-mono font-semibold text-amber-600 dark:text-amber-400 bg-amber-500/10 px-2 py-0.5 rounded border border-amber-500/20">
                                {getEffectiveBaseConcurrency(editingAccount)}
                              </span>
                            </div>
                          ) : (
                            <div className="mt-3 space-y-2">
                              <Input
                                inputMode="numeric"
                                value={concurrencyInput}
                                onChange={(
                                  event: ChangeEvent<HTMLInputElement>,
                                ) => setConcurrencyInput(event.target.value)}
                                placeholder={t(
                                  "accounts.schedulerConcurrencyPlaceholder",
                                )}
                              />
                              <div
                                className={`text-xs ${concurrencyInputInvalid ? "text-red-500" : "text-muted-foreground"}`}
                              >
                                {concurrencyInputInvalid
                                  ? t("accounts.schedulerConcurrencyRange")
                                  : t("accounts.schedulerCustomValuePreview", {
                                      value:
                                        parsedBaseConcurrency ??
                                        getEffectiveBaseConcurrency(
                                          editingAccount,
                                        ),
                                    })}
                              </div>
                            </div>
                          )}
                        </div>

                        {/* 调度优先级 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors md:col-span-2">
                          <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                            <Sliders className="size-4 text-indigo-500" />
                            <span>{t("accounts.schedulerPriorityTitle")}</span>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                            {t("accounts.schedulerPriorityHint")}
                          </p>
                          <div className="mt-3">
                            <Input
                              inputMode="numeric"
                              value={editSchedulerPriorityInput}
                              placeholder={t(
                                "accounts.schedulerPriorityPlaceholder",
                              )}
                              onChange={(event: ChangeEvent<HTMLInputElement>) =>
                                setEditSchedulerPriorityInput(event.target.value)
                              }
                            />
                            <div
                              className={`mt-1.5 text-xs ${editSchedulerPriorityInvalid ? "text-red-500" : "text-muted-foreground"}`}
                            >
                              {editSchedulerPriorityInvalid
                                ? t("accounts.schedulerPriorityRange")
                                : t("accounts.schedulerPriorityDefault")}
                            </div>
                          </div>
                        </div>
                      </div>
                    </div>

                    {/* 分组 2: 熔断与限流 */}
                    <div className="space-y-3">
                      <div className="flex items-center gap-2 text-xs font-bold uppercase tracking-wider text-muted-foreground/80 pt-1">
                        <ShieldCheck className="size-3.5 text-primary" />
                        <span>{t("accounts.categorySafety")}</span>
                        <div className="h-px flex-1 bg-border/60" />
                      </div>

                      <div className="grid gap-4 md:grid-cols-2">
                        {/* 跳过预热层级 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors">
                          <div className="flex items-start justify-between gap-4">
                            <div className="space-y-1">
                              <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                                <Flame className="size-4 text-orange-500" />
                                <span>{t("accounts.schedulerSkipWarmLabel")}</span>
                              </div>
                              <p className="text-xs text-muted-foreground leading-relaxed">
                                {t("accounts.schedulerSkipWarmHint")}
                              </p>
                            </div>
                            <button
                              type="button"
                              role="switch"
                              aria-label={t("accounts.schedulerSkipWarmLabel")}
                              aria-checked={skipWarmTier}
                              onClick={() =>
                                setSkipWarmTier((current) => !current)
                              }
                              className={`relative inline-flex h-5.5 w-10 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60 focus-visible:ring-offset-2 ${skipWarmTier ? "bg-primary" : "bg-muted"}`}
                            >
                              <span
                                className={`pointer-events-none block size-4.5 rounded-full bg-white shadow-xs transition-transform ${skipWarmTier ? "translate-x-4.5" : "translate-x-0"}`}
                              />
                            </button>
                          </div>
                        </div>

                        {/* 请求次数限流 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors">
                          <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                            <Timer className="size-4 text-purple-500" />
                            <span>{t("accounts.dispatchCountLimitTitle")}</span>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                            {t("accounts.dispatchCountLimitHint")}
                          </p>
                          <div className="mt-3">
                            <Input
                              inputMode="numeric"
                              value={editDispatchCountLimitInput}
                              placeholder={t(
                                "accounts.dispatchCountLimitPlaceholder",
                              )}
                              onChange={(event: ChangeEvent<HTMLInputElement>) =>
                                setEditDispatchCountLimitInput(event.target.value)
                              }
                            />
                            <div
                              className={`mt-1.5 text-xs ${editDispatchCountLimitInvalid ? "text-red-500" : "text-muted-foreground"}`}
                            >
                              {editDispatchCountLimitInvalid
                                ? t("accounts.dispatchCountLimitRange")
                                : editDispatchCountLimitPreview
                                  ? t("accounts.dispatchCountLimitStatus", {
                                      used:
                                        editingAccount.dispatch_count_used ?? 0,
                                      limit: editDispatchCountLimitPreview,
                                    })
                                  : t("accounts.dispatchCountLimitDisabled")}
                            </div>
                            {editDispatchCountResetTime ? (
                              <div
                                className="mt-1 text-xs text-muted-foreground"
                                title={editDispatchCountResetTime.title}
                              >
                                {t("accounts.dispatchCountLimitResetAt", {
                                  time: editDispatchCountResetTime.label,
                                })}
                              </div>
                            ) : null}
                          </div>
                        </div>

                        {/* 用量自动暂停 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors md:col-span-2">
                          <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                            <ShieldAlert className="size-4 text-rose-500" />
                            <span>{t("accounts.autoPauseTitle")}</span>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                            {t("accounts.autoPauseHint")}
                          </p>
                          <div className="mt-3.5 flex flex-col gap-3 rounded-lg border border-border/50 bg-muted/20 p-3 sm:flex-row sm:items-center sm:justify-between">
                            <div className="min-w-0">
                              <div className="text-xs font-semibold text-foreground">
                                {t("accounts.ignoreUsageLimitStatus")}
                              </div>
                              <div className="mt-0.5 text-xs text-muted-foreground">
                                {t("accounts.ignoreUsageLimitStatusHint")}
                              </div>
                            </div>
                            <div className="flex shrink-0 flex-wrap gap-2">
                              <TogglePill
                                active={editIgnoreUsageLimitStatusMode === "inherit"}
                                onClick={() => setEditIgnoreUsageLimitStatusMode("inherit")}
                                label={t("accounts.ignoreUsageLimitStatusInherit")}
                              />
                              <TogglePill
                                active={editIgnoreUsageLimitStatusMode === "enabled"}
                                onClick={() => setEditIgnoreUsageLimitStatusMode("enabled")}
                                label={t("common.enabled")}
                              />
                              <TogglePill
                                active={editIgnoreUsageLimitStatusMode === "disabled"}
                                onClick={() => setEditIgnoreUsageLimitStatusMode("disabled")}
                                label={t("common.disabled")}
                              />
                            </div>
                          </div>
                          <div className="mt-3.5 grid gap-4 md:grid-cols-2">
                            <QuotaAutoPauseWindowEditor
                              disabledLabel={t("accounts.autoPause5hDisabled")}
                              disabledHint={t("accounts.autoPauseDisabledHint")}
                              thresholdLabel={t(
                                "accounts.autoPause5hThreshold",
                              )}
                              thresholdHint={t("accounts.autoPauseThresholdHint")}
                              thresholdPlaceholder={t(
                                "accounts.autoPauseThresholdPlaceholder",
                              )}
                              thresholdValue={editAutoPause5hThresholdInput}
                              thresholdInvalid={editAutoPause5hThresholdInvalid}
                              disabled={editAutoPause5hDisabled}
                              invalidLabel={t("accounts.autoPauseThresholdRange")}
                              onThresholdChange={setEditAutoPause5hThresholdInput}
                              onDisabledChange={setEditAutoPause5hDisabled}
                            />
                            <QuotaAutoPauseWindowEditor
                              disabledLabel={t("accounts.autoPause7dDisabled")}
                              disabledHint={t("accounts.autoPauseDisabledHint")}
                              thresholdLabel={t(
                                "accounts.autoPause7dThreshold",
                              )}
                              thresholdHint={t("accounts.autoPauseThresholdHint")}
                              thresholdPlaceholder={t(
                                "accounts.autoPauseThresholdPlaceholder",
                              )}
                              thresholdValue={editAutoPause7dThresholdInput}
                              thresholdInvalid={editAutoPause7dThresholdInvalid}
                              disabled={editAutoPause7dDisabled}
                              invalidLabel={t("accounts.autoPauseThresholdRange")}
                              onThresholdChange={setEditAutoPause7dThresholdInput}
                              onDisabledChange={setEditAutoPause7dDisabled}
                            />
                          </div>
                        </div>
                      </div>
                    </div>

                    {/* 分组 3: 网络、路由与身份 */}
                    <div className="space-y-3">
                      <div className="flex items-center gap-2 text-xs font-bold uppercase tracking-wider text-muted-foreground/80 pt-1">
                        <Globe className="size-3.5 text-primary" />
                        <span>{t("accounts.categoryNetwork")}</span>
                        <div className="h-px flex-1 bg-border/60" />
                      </div>

                      <div className="grid gap-4 md:grid-cols-2">
                        {/* 允许绑定的 API Keys */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors md:col-span-2">
                          <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                            <KeyRound className="size-4 text-emerald-500" />
                            <span>{t("accounts.allowedAPIKeysLabel")}</span>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                            {t("accounts.allowedAPIKeysHint")}
                          </p>
                          <div className="mt-3">
                            <APIKeyMultiSelect
                              options={apiKeys}
                              value={allowedAPIKeySelection}
                              disabled={apiKeys.length === 0}
                              onChange={setAllowedAPIKeySelection}
                              allLabel={t("accounts.allowedAPIKeysAll")}
                              selectedLabel={t(
                                "accounts.allowedAPIKeysSelected",
                                {
                                  count: allowedAPIKeySelection.length,
                                },
                              )}
                              placeholder={t("accounts.allowedAPIKeysPlaceholder")}
                              emptyLabel={t("accounts.allowedAPIKeysNoOptions")}
                              emptyHint={t("accounts.allowedAPIKeysNoOptionsHint")}
                            />
                          </div>
                        </div>

                        {/* 代理服务器 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors md:col-span-2">
                          {renderProxyInput({
                            value: editProxyUrl,
                            onChange: setEditProxyUrl,
                          })}
                        </div>

                        {/* 设备指纹收敛 */}
                        {isCodexOfficialAccount(editingAccount) ? (
                          <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors md:col-span-2">
                            <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                              <Fingerprint className="size-4 text-teal-500" />
                              <span>{t("accounts.codexFingerprintModeTitle")}</span>
                            </div>
                            <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                              {t("accounts.codexFingerprintModeHint")}
                            </p>
                            <Select
                              className="mt-3"
                              value={editCodexFingerprintMode}
                              onValueChange={(value) =>
                                setEditCodexFingerprintMode(
                                  value as CodexFingerprintMode,
                                )
                              }
                              options={codexFingerprintModeOptions(t)}
                            />
                            <div className="mt-2 rounded-lg border border-border/50 bg-muted/20 px-3 py-2 text-xs text-muted-foreground">
                              {codexFingerprintModeDetail(
                                t,
                                editCodexFingerprintMode,
                              )}
                            </div>
                          </div>
                        ) : null}

                        {/* 自定义请求头 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors md:col-span-2">
                          {renderCustomHeadersTextarea({
                            value: editCustomHeadersText,
                            onChange: setEditCustomHeadersText,
                          })}
                        </div>
                      </div>
                    </div>

                    {/* 分组 4: 组织与属性 */}
                    <div className="space-y-3">
                      <div className="flex items-center gap-2 text-xs font-bold uppercase tracking-wider text-muted-foreground/80 pt-1">
                        <Tag className="size-3.5 text-primary" />
                        <span>{t("accounts.categoryOrg")}</span>
                        <div className="h-px flex-1 bg-border/60" />
                      </div>

                      <div className="grid gap-4 md:grid-cols-2">
                        {/* 标签 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors">
                          <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                            <Tag className="size-4 text-violet-500" />
                            <span>{t("accounts.tagsLabel")}</span>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                            {t("accounts.tagsHint")}
                          </p>
                          <ChipInput
                            className="mt-3"
                            value={editTags}
                            onChange={setEditTags}
                            placeholder={t("accounts.tagsPlaceholder")}
                            maxVisible={3}
                          />
                        </div>

                        {/* 分组 */}
                        <div className="rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs hover:border-border/90 transition-colors">
                          <div className="flex items-start justify-between gap-3">
                            <div>
                              <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                                <Users className="size-4 text-blue-500" />
                                <span>{t("accounts.groupsLabel")}</span>
                              </div>
                              <p className="mt-1 text-xs text-muted-foreground leading-relaxed">
                                {t("accounts.groupsHint")}
                              </p>
                            </div>
                            <Button
                              type="button"
                              variant="outline"
                              size="xs"
                              onClick={() => setShowGroupManager(true)}
                            >
                              <FolderOpen className="size-3" />
                              {t("accounts.groupManage")}
                            </Button>
                          </div>
                          <div className="mt-3">
                            <AccountGroupMultiSelect
                              groups={codexGroups}
                              value={editGroupIds}
                              onChange={setEditGroupIds}
                              allLabel={t("accounts.groupsUnbound")}
                              selectedLabel={t("accounts.groupsSelected", {
                                count: editGroupIds.length,
                              })}
                              placeholder={t("accounts.groupsPlaceholder")}
                              emptyLabel={t("accounts.groupsNone")}
                              emptyHint={t("accounts.groupsSelectHint")}
                              onCreateGroup={handleCreateGroupInline}
                              createLabel={t("accounts.groupCreate")}
                              createPlaceholder={t("accounts.groupNamePlaceholder")}
                              creatingLabel={t("accounts.groupCreating")}
                              createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
                            />
                          </div>
                        </div>
                      </div>
                    </div>

                    {/* 实时调度预览栏 */}
                    <div className="rounded-xl border border-border/80 bg-gradient-to-r from-muted/40 via-background to-muted/20 p-4.5 shadow-2xs">
                      <div className="flex items-center gap-2 font-semibold text-foreground text-sm">
                        <Activity className="size-4 text-primary" />
                        <span>{t("accounts.schedulerPreviewTitle")}</span>
                      </div>
                      <div className="mt-3 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
                        <PreviewItem
                          label={t("accounts.schedulerPreviewRawScore")}
                          value={String(editPreview.rawScore)}
                        />
                        <PreviewItem
                          label={t("accounts.schedulerPreviewDispatchScore")}
                          value={String(editPreview.dispatchScore)}
                        />
                        <PreviewItem
                          label={t("accounts.schedulerPreviewHealthTier")}
                          value={formatHealthTier(editPreview.healthTier, t)}
                        />
                        <PreviewItem
                          label={t(
                            "accounts.schedulerPreviewDynamicConcurrency",
                          )}
                          value={String(editPreview.dynamicConcurrency)}
                        />
                      </div>
                    </div>
                  </div>
                )}
              </div>
            ) : null}
          </Modal>

          <AccountProxyQuickEditor
            account={quickProxyAccount}
            accountLabel={
              quickProxyAccount ? formatAccountName(quickProxyAccount) : ""
            }
            proxies={proxyPool}
            ctx={proxyBindingCtx}
            onClose={() => setQuickProxyAccount(null)}
            onSaved={async () => {
              await reload();
            }}
          />

          <Modal
            show={Boolean(quickGroupAccount)}
            title={t("accounts.groupQuickTitle")}
            contentClassName="sm:max-w-[520px]"
            onClose={() => {
              if (quickGroupSubmitting) return;
              setQuickGroupAccount(null);
              setQuickGroupIds([]);
            }}
            footer={
              <>
                <Button
                  type="button"
                  variant="outline"
                  disabled={quickGroupSubmitting}
                  onClick={() => {
                    setQuickGroupAccount(null);
                    setQuickGroupIds([]);
                  }}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  type="button"
                  disabled={quickGroupSubmitting}
                  onClick={() => void handleQuickGroupSave()}
                >
                  {quickGroupSubmitting
                    ? t("common.saving")
                    : quickGroupIds.length === 0
                      ? t("accounts.groupQuickClear")
                      : t("accounts.groupQuickSave")}
                </Button>
              </>
            }
          >
            <div className="space-y-4">
              <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
                <div className="font-semibold text-foreground">
                  {quickGroupAccount
                    ? formatAccountName(quickGroupAccount)
                    : ""}
                </div>
                <div className="mt-1">{t("accounts.groupQuickDesc")}</div>
              </div>
              <div className="flex items-center justify-end">
                <Button
                  type="button"
                  variant="outline"
                  size="xs"
                  disabled={quickGroupSubmitting}
                  onClick={() => setShowGroupManager(true)}
                >
                  <FolderOpen className="size-3" />
                  {t("accounts.groupManage")}
                </Button>
              </div>
              <AccountGroupMultiSelect
                groups={codexGroups}
                value={quickGroupIds}
                onChange={setQuickGroupIds}
                allLabel={t("accounts.groupsUnbound")}
                selectedLabel={t("accounts.groupsSelected", {
                  count: quickGroupIds.length,
                })}
                placeholder={t("accounts.groupsPlaceholder")}
                emptyLabel={t("accounts.groupsNone")}
                emptyHint={t("accounts.groupsSelectHint")}
                disabled={quickGroupSubmitting}
                onCreateGroup={handleCreateGroupInline}
                createLabel={t("accounts.groupCreate")}
                createPlaceholder={t("accounts.groupNamePlaceholder")}
                creatingLabel={t("accounts.groupCreating")}
                createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
              />
            </div>
          </Modal>

          <Modal
            show={Boolean(modelsAccount)}
            title={t("accounts.supportedModelsTitle")}
            contentClassName="sm:max-w-[560px]"
            onClose={closeModelsEditor}
            footer={
              <>
                <Button
                  type="button"
                  variant="outline"
                  disabled={modelsSaving || modelsSyncing || modelsProbing}
                  onClick={closeModelsEditor}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  type="button"
                  disabled={modelsSaving || modelsSyncing || modelsProbing}
                  onClick={() => void handleSaveModels()}
                >
                  {modelsSaving
                    ? t("common.saving")
                    : modelsDraft.length === 0
                      ? t("accounts.supportedModelsClearSave")
                      : t("common.save")}
                </Button>
              </>
            }
          >
            <div className="space-y-4">
              <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
                <div className="font-semibold text-foreground">
                  {modelsAccount ? formatAccountName(modelsAccount) : ""}
                </div>
                <div className="mt-1">{t("accounts.supportedModelsDesc")}</div>
              </div>

              {/* 自动获取：探测/同步按钮 + 说明，成一体 */}
              <div className="rounded-lg border border-border bg-muted/10 p-3">
                <div className="flex flex-wrap gap-2">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={modelsSyncing || modelsSaving || modelsProbing}
                    onClick={() => void handleProbeModels()}
                  >
                    <Zap
                      className={`size-3.5 ${modelsProbing ? "animate-pulse" : ""}`}
                    />
                    {modelsProbing
                      ? t("accounts.supportedModelsProbing")
                      : t("accounts.supportedModelsProbe")}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={modelsSyncing || modelsSaving || modelsProbing}
                    onClick={() => void handleSyncModelsUpstream()}
                  >
                    <RefreshCw
                      className={`size-3.5 ${modelsSyncing ? "animate-spin" : ""}`}
                    />
                    {modelsSyncing
                      ? t("accounts.supportedModelsSyncing")
                      : t("accounts.supportedModelsSync")}
                  </Button>
                </div>
                <p className="mt-2 text-xs text-muted-foreground">
                  {t("accounts.supportedModelsProbeHint")}
                </p>
                {probeBoard.length > 0 && (
                  <div className="mt-3">
                    <ModelProbeBoard items={probeBoard} t={t} />
                  </div>
                )}
              </div>

              {/* 手动添加 */}
              <div className="flex gap-2">
                <Input
                  placeholder={t("accounts.openaiModelsPlaceholder")}
                  value={modelsInputDraft}
                  disabled={modelsSaving}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    setModelsInputDraft(event.target.value)
                  }
                  onKeyDown={(event) => {
                    if (event.key === "Enter") {
                      event.preventDefault();
                      addModelsDraftValues(modelsInputDraft);
                    }
                  }}
                  onPaste={(event) => {
                    const pasted = event.clipboardData.getData("text");
                    if (parseModelTokens(pasted).length > 1) {
                      event.preventDefault();
                      addModelsDraftValues(pasted);
                    }
                  }}
                />
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => addModelsDraftValues(modelsInputDraft)}
                  disabled={!modelsInputDraft.trim() || modelsSaving}
                >
                  <Plus className="size-3.5" />
                  {t("accounts.openaiModelsAdd")}
                </Button>
              </div>

              {/* 白名单列表：计数 + 清空 在上，紧凑 pills 在下 */}
              <div className="space-y-2">
                <div className="flex items-center justify-between gap-2">
                  <span className="text-xs text-muted-foreground">
                    {modelsDraft.length === 0
                      ? t("accounts.supportedModelsHintAll")
                      : t("accounts.supportedModelsHintCount", {
                          count: modelsDraft.length,
                        })}
                  </span>
                  {modelsDraft.length > 0 && (
                    <button
                      type="button"
                      className="shrink-0 text-xs text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
                      disabled={modelsSaving}
                      onClick={clearModelsDraft}
                    >
                      {t("accounts.supportedModelsClearAll")}
                    </button>
                  )}
                </div>
                <ModelChipGrid
                  variant="pills"
                  models={modelsDraft}
                  onRemove={removeModelsDraftValue}
                  emptyLabel={t("accounts.supportedModelsEmpty")}
                />
              </div>
            </div>
          </Modal>

          <Modal
            show={showBatchMetaEditor}
            title={t(
              batchMetaMode === "groups"
                ? "accounts.batchGroupTitle"
                : "accounts.batchMetaTitle",
            )}
            contentClassName="sm:max-w-[760px]"
            onClose={() => {
              if (batchMetaSubmitting) return;
              setShowBatchMetaEditor(false);
            }}
            footer={
              <>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setShowBatchMetaEditor(false)}
                  disabled={batchMetaSubmitting}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  type="button"
                  onClick={() => void handleBatchSaveMeta()}
                  disabled={
                    batchMetaSubmitting ||
                    !batchMetaHasUpdates ||
                    batchMetaInvalid
                  }
                >
                  {batchMetaSubmitting
                    ? t("common.saving")
                    : batchMetaMode === "groups"
                      ? batchGroupIds.length === 0
                        ? t("accounts.batchGroupClear")
                        : t("accounts.batchGroupReplace")
                      : t("common.save")}
                </Button>
              </>
            }
          >
            <div className="space-y-4">
              <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
                {t(
                  batchMetaMode === "groups"
                    ? "accounts.batchGroupDesc"
                    : "accounts.batchMetaDesc",
                  { count: selected.size },
                )}
              </div>
              {batchMetaMode === "all" ? (
                <div className="rounded-xl border border-border p-4">
                  <div className="flex items-start justify-between gap-4">
                    <div className="min-w-0">
                      <div className="text-sm font-semibold text-foreground">
                        {t("accounts.tagsLabel")}
                      </div>
                      <div className="mt-1 text-xs text-muted-foreground">
                        {t("accounts.batchMetaFieldHint")}
                      </div>
                    </div>
                    <label className="flex shrink-0 items-center gap-2 text-xs font-medium text-muted-foreground">
                      <span>
                        {t(
                          batchUpdateTags
                            ? "common.enabled"
                            : "common.disabled",
                        )}
                      </span>
                      <Switch
                        checked={batchUpdateTags}
                        onCheckedChange={setBatchUpdateTags}
                        aria-label={`${t("accounts.batchMetaTitle")}: ${t("accounts.tagsLabel")}`}
                      />
                    </label>
                  </div>
                  <ChipInput
                    className="mt-3"
                    value={batchTags}
                    onChange={setBatchTags}
                    placeholder={t(
                      batchUpdateTags
                        ? "accounts.tagsPlaceholder"
                        : "accounts.batchMetaFieldHint",
                    )}
                    disabled={!batchUpdateTags}
                    maxVisible={6}
                  />
                </div>
              ) : null}
              {batchMetaMode === "all" ? (
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="rounded-xl border border-border p-4">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.schedulerScoreLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.schedulerScoreHint")}
                        </div>
                      </div>
                      <Switch
                        checked={batchUpdateScoreBias}
                        onCheckedChange={setBatchUpdateScoreBias}
                        aria-label={`${t("accounts.batchMetaTitle")}: ${t("accounts.schedulerScoreLabel")}`}
                      />
                    </div>
                    <Input
                      className="mt-3"
                      inputMode="numeric"
                      value={batchScoreBiasInput}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setBatchScoreBiasInput(event.target.value)
                      }
                      placeholder={t("accounts.schedulerScorePlaceholder")}
                      disabled={!batchUpdateScoreBias}
                    />
                    <div
                      className={`mt-1.5 text-xs ${batchScoreBiasInvalid ? "text-red-500" : "text-muted-foreground"}`}
                    >
                      {batchScoreBiasInvalid
                        ? t("accounts.schedulerScoreRange")
                        : t("accounts.batchMetaResetHint")}
                    </div>
                  </div>

                  <div className="rounded-xl border border-border p-4">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.schedulerConcurrencyLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.schedulerConcurrencyHint")}
                        </div>
                      </div>
                      <Switch
                        checked={batchUpdateBaseConcurrency}
                        onCheckedChange={setBatchUpdateBaseConcurrency}
                        aria-label={`${t("accounts.batchMetaTitle")}: ${t("accounts.schedulerConcurrencyLabel")}`}
                      />
                    </div>
                    <Input
                      className="mt-3"
                      inputMode="numeric"
                      value={batchBaseConcurrencyInput}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setBatchBaseConcurrencyInput(event.target.value)
                      }
                      placeholder={t(
                        "accounts.schedulerConcurrencyPlaceholder",
                      )}
                      disabled={!batchUpdateBaseConcurrency}
                    />
                    <div
                      className={`mt-1.5 text-xs ${batchBaseConcurrencyInvalid ? "text-red-500" : "text-muted-foreground"}`}
                    >
                      {batchBaseConcurrencyInvalid
                        ? t("accounts.schedulerConcurrencyRange")
                        : t("accounts.batchMetaResetHint")}
                    </div>
                  </div>

                  <div className="rounded-xl border border-border p-4">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.schedulerPriorityTitle")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.schedulerPriorityHint")}
                        </div>
                      </div>
                      <Switch
                        checked={batchUpdateSchedulerPriority}
                        onCheckedChange={setBatchUpdateSchedulerPriority}
                        aria-label={`${t("accounts.batchMetaTitle")}: ${t("accounts.schedulerPriorityTitle")}`}
                      />
                    </div>
                    <Input
                      className="mt-3"
                      inputMode="numeric"
                      value={batchSchedulerPriorityInput}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setBatchSchedulerPriorityInput(event.target.value)
                      }
                      placeholder={t(
                        "accounts.schedulerPriorityPlaceholder",
                      )}
                      disabled={!batchUpdateSchedulerPriority}
                    />
                    <div
                      className={`mt-1.5 text-xs ${batchSchedulerPriorityInvalid ? "text-red-500" : "text-muted-foreground"}`}
                    >
                      {batchSchedulerPriorityInvalid
                        ? t("accounts.schedulerPriorityRange")
                        : t("accounts.batchMetaResetHint")}
                    </div>
                  </div>

                  <div className="rounded-xl border border-border p-4 md:col-span-2">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.codexFingerprintModeTitle")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.codexFingerprintModeBatchHint")}
                        </div>
                      </div>
                      <Switch
                        checked={batchUpdateCodexFingerprintMode}
                        onCheckedChange={setBatchUpdateCodexFingerprintMode}
                        aria-label={`${t("accounts.batchMetaTitle")}: ${t("accounts.codexFingerprintModeTitle")}`}
                      />
                    </div>
                    <Select
                      className="mt-3"
                      value={batchCodexFingerprintMode}
                      onValueChange={(value) =>
                        setBatchCodexFingerprintMode(
                          value as CodexFingerprintMode,
                        )
                      }
                      options={codexFingerprintModeOptions(t)}
                      disabled={!batchUpdateCodexFingerprintMode}
                    />
                    <div className="mt-1.5 text-xs text-muted-foreground">
                      {codexFingerprintModeDetail(t, batchCodexFingerprintMode)}
                    </div>
                  </div>
                </div>
              ) : null}
              <div className="rounded-xl border border-border p-4">
                <div className="flex items-start justify-between gap-4">
                  <div className="min-w-0">
                    <div className="text-sm font-semibold text-foreground">
                      {t("accounts.groupsLabel")}
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {t(
                        batchMetaMode === "groups"
                          ? "accounts.batchGroupFieldHint"
                          : "accounts.batchMetaFieldHint",
                      )}
                    </div>
                  </div>
                  <div className="flex shrink-0 items-center gap-2">
                    {batchUpdateGroups ? (
                      <Button
                        type="button"
                        variant="outline"
                        size="xs"
                        onClick={() => setShowGroupManager(true)}
                      >
                        <FolderOpen className="size-3" />
                        {t("accounts.groupManage")}
                      </Button>
                    ) : null}
                    {batchMetaMode === "all" ? (
                      <label className="flex shrink-0 items-center gap-2 text-xs font-medium text-muted-foreground">
                        <span>
                          {t(
                            batchUpdateGroups
                              ? "common.enabled"
                              : "common.disabled",
                          )}
                        </span>
                        <Switch
                          checked={batchUpdateGroups}
                          onCheckedChange={setBatchUpdateGroups}
                          aria-label={`${t("accounts.batchMetaTitle")}: ${t("accounts.groupsLabel")}`}
                        />
                      </label>
                    ) : null}
                  </div>
                </div>
                <div className="mt-3">
                  <AccountGroupMultiSelect
                    groups={codexGroups}
                    value={batchGroupIds}
                    onChange={setBatchGroupIds}
                    allLabel={t(
                      batchUpdateGroups
                        ? "accounts.groupsUnbound"
                        : "accounts.batchMetaFieldHint",
                    )}
                    selectedLabel={t("accounts.groupsSelected", {
                      count: batchGroupIds.length,
                    })}
                    placeholder={t(
                      batchUpdateGroups
                        ? "accounts.groupsPlaceholder"
                        : "accounts.batchMetaFieldHint",
                    )}
                    emptyLabel={t("accounts.groupsNone")}
                    emptyHint={t(
                      batchUpdateGroups
                        ? "accounts.groupsSelectHint"
                        : "accounts.batchMetaFieldHint",
                    )}
                    disabled={!batchUpdateGroups}
                    onCreateGroup={
                      batchUpdateGroups ? handleCreateGroupInline : undefined
                    }
                    createLabel={t("accounts.groupCreate")}
                    createPlaceholder={t("accounts.groupNamePlaceholder")}
                    creatingLabel={t("accounts.groupCreating")}
                    createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
                  />
                </div>
              </div>
            </div>
          </Modal>

          <Modal
            show={showBatchQuotaAutoPauseEditor}
            title={t("accounts.batchAutoPauseTitle")}
            contentClassName="sm:max-w-[680px]"
            onClose={() => {
              if (batchQuotaAutoPauseSubmitting) return;
              setShowBatchQuotaAutoPauseEditor(false);
            }}
            footer={
              <>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setShowBatchQuotaAutoPauseEditor(false)}
                  disabled={batchQuotaAutoPauseSubmitting}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  type="button"
                  onClick={() => void handleBatchSaveQuotaAutoPause()}
                  disabled={
                    batchQuotaAutoPauseSubmitting ||
                    batchAutoPause5hThresholdInvalid ||
                    batchAutoPause7dThresholdInvalid
                  }
                >
                  {batchQuotaAutoPauseSubmitting
                    ? t("common.saving")
                    : t("common.save")}
                </Button>
              </>
            }
          >
            <div className="space-y-4">
              <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
                {t("accounts.batchAutoPauseDesc", { count: selected.size })}
              </div>
              <div className="grid gap-4 md:grid-cols-2">
                <QuotaAutoPauseWindowEditor
                  disabledLabel={t("accounts.autoPause5hDisabled")}
                  disabledHint={t("accounts.autoPauseDisabledHint")}
                  thresholdLabel={t("accounts.autoPause5hThreshold")}
                  thresholdHint={t("accounts.autoPauseThresholdHint")}
                  thresholdPlaceholder={t(
                    "accounts.autoPauseThresholdPlaceholder",
                  )}
                  thresholdValue={batchAutoPause5hThresholdInput}
                  thresholdInvalid={batchAutoPause5hThresholdInvalid}
                  disabled={batchAutoPause5hDisabled}
                  invalidLabel={t("accounts.autoPauseThresholdRange")}
                  onThresholdChange={setBatchAutoPause5hThresholdInput}
                  onDisabledChange={setBatchAutoPause5hDisabled}
                />
                <QuotaAutoPauseWindowEditor
                  disabledLabel={t("accounts.autoPause7dDisabled")}
                  disabledHint={t("accounts.autoPauseDisabledHint")}
                  thresholdLabel={t("accounts.autoPause7dThreshold")}
                  thresholdHint={t("accounts.autoPauseThresholdHint")}
                  thresholdPlaceholder={t(
                    "accounts.autoPauseThresholdPlaceholder",
                  )}
                  thresholdValue={batchAutoPause7dThresholdInput}
                  thresholdInvalid={batchAutoPause7dThresholdInvalid}
                  disabled={batchAutoPause7dDisabled}
                  invalidLabel={t("accounts.autoPauseThresholdRange")}
                  onThresholdChange={setBatchAutoPause7dThresholdInput}
                  onDisabledChange={setBatchAutoPause7dDisabled}
                />
              </div>
            </div>
          </Modal>

          <Modal
            show={showGroupManager}
            title={t("accounts.groupManageTitle")}
            contentClassName="sm:max-w-[820px]"
            bodyClassName="space-y-3"
            onClose={() => {
              if (groupSubmitting) return;
              setShowGroupManager(false);
              resetGroupDraft();
            }}
            footer={
              <>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => {
                    setShowGroupManager(false);
                    resetGroupDraft();
                  }}
                  disabled={groupSubmitting}
                >
                  {t("common.close")}
                </Button>
                <Button
                  type="button"
                  onClick={() => void handleSaveGroup()}
                  disabled={
                    groupSubmitting ||
                    !groupDraft.name.trim() ||
                    groupBaseConcurrencyInvalid
                  }
                >
                  {groupSubmitting
                    ? t("common.saving")
                    : groupDraft.id === null
                      ? t("accounts.groupCreate")
                      : t("common.save")}
                </Button>
              </>
            }
          >
            <div className="flex items-start justify-between gap-3 rounded-lg border border-border bg-muted/20 px-3 py-2.5">
              <div className="min-w-0">
                <div className="text-sm font-semibold text-foreground">
                  {t("accounts.groupManageTitle")}
                </div>
                <div className="mt-0.5 text-xs text-muted-foreground">
                  {t("accounts.groupEmptyDesc")}
                </div>
              </div>
              <Badge variant="secondary" className="shrink-0">
                {t("accounts.groupMembers")}{" "}
                {allGroups.reduce((sum, group) => sum + group.member_count, 0)}
              </Badge>
            </div>

            <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(300px,0.55fr)]">
              <div className="min-h-[260px] overflow-hidden rounded-lg border border-border bg-background">
                <div className="flex h-10 items-center justify-between border-b border-border px-3">
                  <div className="text-xs font-semibold text-muted-foreground">
                    {t("accounts.groupsLabel")}
                  </div>
                  <Badge variant="outline">{allGroups.length}</Badge>
                </div>
                {allGroups.length === 0 ? (
                  <div className="flex min-h-[218px] flex-col items-center justify-center px-4 text-center">
                    <FolderOpen className="mb-3 size-8 text-muted-foreground" />
                    <div className="text-sm font-semibold text-foreground">
                      {t("accounts.groupEmpty")}
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {t("accounts.groupEmptyDesc")}
                    </div>
                  </div>
                ) : (
                  <div className="max-h-[360px] overflow-y-auto p-2">
                    {allGroups.map((group) => {
                      const active = groupDraft.id === group.id;
                      const color = normalizeGroupColor(group.color);
                      return (
                        <div
                          key={group.id}
                          className={`flex items-center gap-3 rounded-md border px-3 py-2 transition-colors ${
                            active
                              ? "border-primary/40 bg-primary/5 shadow-sm"
                              : "border-transparent bg-transparent hover:border-border hover:bg-muted/30"
                          }`}
                        >
                          <span
                            className="size-3 shrink-0 rounded-full"
                            style={{ backgroundColor: color }}
                          />
                          <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-2">
                              <span className="truncate text-sm font-semibold text-foreground">
                                {group.name}
                              </span>
                              <span
                                className={`inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-semibold ${
                                  group.channel === "grok"
                                    ? "bg-violet-50 text-violet-700 dark:bg-violet-950 dark:text-violet-300"
                                    : group.channel === "antigravity"
                                      ? "bg-emerald-50 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300"
                                      : group.channel === "claude"
                                        ? "bg-orange-50 text-orange-700 dark:bg-orange-950 dark:text-orange-300"
                                        : "bg-sky-50 text-sky-700 dark:bg-sky-950 dark:text-sky-300"
                                }`}
                              >
                                <ChannelLogo channel={group.channel} size={11} />
                                {group.channel === "grok"
                                  ? t("accounts.providerViewGrok")
                                  : group.channel === "antigravity"
                                    ? t("accounts.providerViewAntigravity")
                                    : group.channel === "claude"
                                      ? t("accounts.providerViewClaude")
                                      : t("accounts.providerViewCodex")}
                              </span>
                              <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-semibold text-muted-foreground">
                                {t("accounts.groupMembers")}{" "}
                                {group.member_count}
                              </span>
                              <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-semibold text-muted-foreground">
                                {typeof group.base_concurrency_override === "number" &&
                                group.base_concurrency_override > 0
                                  ? t("accounts.groupBaseConcurrencyValue", {
                                      value: group.base_concurrency_override,
                                    })
                                  : t("accounts.groupBaseConcurrencyInherited")}
                              </span>
                            </div>
                            <div className="mt-0.5 truncate text-xs text-muted-foreground">
                              {group.description ||
                                t("accounts.groupNoDescription")}
                            </div>
                          </div>
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            onClick={() => startEditGroup(group)}
                            title={t("accounts.groupEdit")}
                          >
                            <Pencil className="size-3.5" />
                          </Button>
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            onClick={() => void handleDeleteGroup(group)}
                            disabled={groupSubmitting}
                            title={t("common.delete")}
                          >
                            <Trash2 className="size-3.5 text-red-500" />
                          </Button>
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-border bg-background p-3">
                <div className="flex h-8 items-center justify-between gap-3">
                  <div className="text-sm font-semibold text-foreground">
                    {groupDraft.id === null
                      ? t("accounts.groupCreateTitle")
                      : t("accounts.groupEditTitle")}
                  </div>
                  {groupDraft.id !== null ? (
                    <Button
                      type="button"
                      variant="ghost"
                      size="xs"
                      onClick={resetGroupDraft}
                    >
                      <Plus className="size-3" />
                      {t("accounts.groupCreate")}
                    </Button>
                  ) : null}
                </div>
                <div className="mt-4 space-y-3">
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupName")}
                    </span>
                    <Input
                      value={groupDraft.name}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          name: event.target.value,
                        }))
                      }
                      placeholder={t("accounts.groupNamePlaceholder")}
                      maxLength={80}
                    />
                  </label>
                  <div className="space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupChannelLabel")}
                    </span>
                    {(() => {
                      const channelLocked =
                        groupDraft.id !== null &&
                        (allGroups.find((g) => g.id === groupDraft.id)
                          ?.member_count ?? 0) > 0;
                      return (
                        <>
                          <div className="flex gap-2">
                            {(["codex", "grok", "antigravity", "claude"] as const).map((channel) => (
                              <button
                                key={channel}
                                type="button"
                                disabled={channelLocked}
                                className={`inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-xs font-semibold transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${
                                  groupDraft.channel === channel
                                    ? "border-primary bg-primary/10 text-primary"
                                    : "border-border text-muted-foreground hover:bg-muted/50"
                                }`}
                                onClick={() =>
                                  setGroupDraft((draft) => ({
                                    ...draft,
                                    channel,
                                  }))
                                }
                              >
                                <ChannelLogo channel={channel} size={14} />
                                {channel === "grok"
                                  ? t("accounts.providerViewGrok")
                                  : channel === "antigravity"
                                    ? t("accounts.providerViewAntigravity")
                                    : channel === "claude"
                                      ? t("accounts.providerViewClaude")
                                      : t("accounts.providerViewCodex")}
                              </button>
                            ))}
                          </div>
                          <p className="text-[11px] text-muted-foreground">
                            {channelLocked
                              ? t("accounts.groupChannelLocked")
                              : t("accounts.groupChannelHint")}
                          </p>
                        </>
                      );
                    })()}
                  </div>
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupDescription")}
                    </span>
                    <Input
                      value={groupDraft.description}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          description: event.target.value,
                        }))
                      }
                      placeholder={t("accounts.groupDescriptionPlaceholder")}
                      maxLength={240}
                    />
                  </label>
                  <div className="space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupColor")}
                    </span>
                    <div className="flex flex-wrap gap-2">
                      {ACCOUNT_GROUP_COLORS.map((color) => (
                        <button
                          key={color}
                          type="button"
                          className={`size-8 rounded-lg border transition-transform hover:scale-105 ${
                            normalizeGroupColor(groupDraft.color) === color
                              ? "border-foreground ring-2 ring-ring/30"
                              : "border-border"
                          }`}
                          style={{ backgroundColor: color }}
                          onClick={() =>
                            setGroupDraft((draft) => ({ ...draft, color }))
                          }
                          aria-label={color}
                        />
                      ))}
                    </div>
                    <Input
                      className="h-8 font-mono text-xs"
                      value={groupDraft.color}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          color: event.target.value,
                        }))
                      }
                      placeholder={t("accounts.groupColorPlaceholder")}
                      maxLength={20}
                    />
                  </div>
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupBaseConcurrencyLabel")}
                    </span>
                    <Input
                      type="number"
                      min={1}
                      step={1}
                      inputMode="numeric"
                      value={groupDraft.baseConcurrencyInput}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          baseConcurrencyInput: event.target.value,
                        }))
                      }
                      placeholder={t(
                        "accounts.groupBaseConcurrencyPlaceholder",
                      )}
                    />
                    <p
                      className={`text-[11px] ${groupBaseConcurrencyInvalid ? "text-red-500" : "text-muted-foreground"}`}
                    >
                      {groupBaseConcurrencyInvalid
                        ? t("accounts.groupBaseConcurrencyRange")
                        : t("accounts.groupBaseConcurrencyHint")}
                    </p>
                  </label>
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupProxyURLsLabel")}
                    </span>
                    <textarea
                      className="w-full min-h-[80px] p-3 border border-input rounded-xl bg-background text-sm resize-y font-mono focus:outline-none focus:ring-2 focus:ring-ring"
                      value={groupDraft.proxyURLsInput}
                      onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          proxyURLsInput: event.target.value,
                        }))
                      }
                      placeholder={t("accounts.groupProxyURLsPlaceholder")}
                      rows={3}
                      spellCheck={false}
                    />
                    <p className="text-[11px] text-muted-foreground">
                      {t("accounts.groupProxyURLsHint")}
                    </p>
                  </label>
                  <div className="space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupAutoPause5hThreshold")}
                    </span>
                    <Input
                      type="number"
                      min={0}
                      max={100}
                      step={0.1}
                      inputMode="decimal"
                      placeholder={t('settings.globalAutoPausePlaceholder')}
                      value={groupDraft.auto_pause_5h_threshold > 0 ? (groupDraft.auto_pause_5h_threshold * 100).toFixed(1).replace(/\.0$/, '') : ''}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => {
                        const raw = e.target.value
                        const ratio = raw === '' ? 0 : Math.max(0, Math.min(1, parseFloat(raw) / 100))
                        setGroupDraft(d => ({ ...d, auto_pause_5h_threshold: isNaN(ratio) ? 0 : ratio }))
                      }}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupAutoPause7dThreshold")}
                    </span>
                    <Input
                      type="number"
                      min={0}
                      max={100}
                      step={0.1}
                      inputMode="decimal"
                      placeholder={t('settings.globalAutoPausePlaceholder')}
                      value={groupDraft.auto_pause_7d_threshold > 0 ? (groupDraft.auto_pause_7d_threshold * 100).toFixed(1).replace(/\.0$/, '') : ''}
                      onChange={(e: ChangeEvent<HTMLInputElement>) => {
                        const raw = e.target.value
                        const ratio = raw === '' ? 0 : Math.max(0, Math.min(1, parseFloat(raw) / 100))
                        setGroupDraft(d => ({ ...d, auto_pause_7d_threshold: isNaN(ratio) ? 0 : ratio }))
                      }}
                    />
                    <p className="text-[11px] text-muted-foreground">{t("accounts.groupAutoPauseHint")}</p>
                  </div>
                </div>
              </div>
            </div>
          </Modal>

          <InviteGuideModal
            show={inviteGuide.show}
            accountIds={inviteGuide.ids}
            onClose={() => setInviteGuide((p) => ({ ...p, show: false }))}
            onGoInvite={(accountEmail) => {
              setInviteGuide((p) => ({ ...p, show: false }));
              navigate(
                accountEmail
                  ? `/accounts/invite?account=${encodeURIComponent(accountEmail)}`
                  : "/accounts/invite",
              );
            }}
          />

          <Modal
            show={importProgress.show}
            title={
              importProgress.done
                ? t("accounts.importDone")
                : t("accounts.importingProgress")
            }
            contentClassName="sm:max-w-[420px]"
            onClose={() => setImportProgress((p) => ({ ...p, show: false }))}
          >
            <div className="space-y-4">
              <div className="w-full h-3 bg-muted rounded-full overflow-hidden">
                <div
                  className="h-full bg-primary rounded-full transition-all duration-300 ease-out"
                  style={{
                    width:
                      importProgress.total > 0
                        ? `${Math.round((importProgress.current / importProgress.total) * 100)}%`
                        : "0%",
                  }}
                />
              </div>
              <div className="text-center text-sm text-muted-foreground">
                {importProgress.total > 0
                  ? `${importProgress.current} / ${importProgress.total}  (${Math.round((importProgress.current / importProgress.total) * 100)}%)`
                  : t("accounts.importPreparing")}
              </div>
              <div className="grid grid-cols-4 gap-2 text-center">
                <div className="rounded-xl bg-emerald-500/10 px-2 py-2">
                  <div className="text-lg font-bold text-emerald-600">
                    {importProgress.success}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importSuccess")}
                  </div>
                </div>
                <div className="rounded-xl bg-sky-500/10 px-2 py-2">
                  <div className="text-lg font-bold text-sky-600">
                    {importProgress.updated}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importUpdated")}
                  </div>
                </div>
                <div className="rounded-xl bg-amber-500/10 px-2 py-2">
                  <div className="text-lg font-bold text-amber-600">
                    {importProgress.duplicate}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importDuplicate")}
                  </div>
                </div>
                <div className="rounded-xl bg-red-500/10 px-2 py-2">
                  <div className="text-lg font-bold text-red-600">
                    {importProgress.failed}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importFailedCount")}
                  </div>
                </div>
              </div>
              {importProgress.done && (
                <p className="text-xs text-center text-muted-foreground">
                  {t("accounts.importDoneHint")}
                </p>
              )}
            </div>
          </Modal>
          </div>

          {confirmDialog}
        </>
      </StateShell>
    </div>
  );
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

function RecycleBinView({
  onClose,
  onChanged,
  confirm,
  runStreamingOperation,
}: {
  onClose: () => void;
  onChanged: () => void;
  confirm: (options: ConfirmDialogOptions) => Promise<boolean>;
  runStreamingOperation: (
    path: string,
    body: unknown,
    title: string,
  ) => Promise<BatchOperationEvent | null>;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [rows, setRows] = useState<RecycleBinAccountRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actingId, setActingId] = useState<number | null>(null);
  const [batchActing, setBatchActing] = useState(false);
  const [batchTesting, setBatchTesting] = useState(false);
  const [emptying, setEmptying] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<Set<number>>(new Set());
  const [testingRow, setTestingRow] = useState<RecycleBinAccountRow | null>(
    null,
  );
  const [planFilter, setPlanFilter] = useState<
    | "all"
    | "pro"
    | "prolite"
    | "plus"
    | "team"
    | "k12"
    | "free"
    | "api"
    | "unknown"
  >("all");
  const [exporting, setExporting] = useState(false);
  const [showEmptyConfirm, setShowEmptyConfirm] = useState(false);
  const [emptyConfirmText, setEmptyConfirmText] = useState("");
  const [autoRestore, setAutoRestore] = useState(() => {
    try {
      return (
        window.localStorage.getItem(RECYCLE_BIN_AUTO_RESTORE_KEY) === "1"
      );
    } catch {
      return false;
    }
  });
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize(
    "recycle-bin",
    20,
    DEFAULT_PAGE_SIZE_OPTIONS,
  );

  const busy = batchActing || emptying || batchTesting || exporting;

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const resp = await api.getRecycleBinAccounts();
      const next = resp.accounts ?? [];
      setRows(next);
      const validIds = new Set(next.map((row) => row.id));
      setSelectedIds(
        (prev) => new Set([...prev].filter((id) => validIds.has(id))),
      );
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const stats = useMemo(() => {
    const relay = rows.filter((row) => row.openai_responses_api).length;
    const dayAgo = Date.now() - 24 * 60 * 60 * 1000;
    const recent24h = rows.filter((row) => {
      if (!row.deleted_at) return false;
      const ts = new Date(row.deleted_at).getTime();
      return Number.isFinite(ts) && ts >= dayAgo;
    }).length;
    const planCounts = new Map<string, number>();
    for (const row of rows) {
      const plan =
        (row.plan_type || "").trim() || t("accounts.recycleBinPlanUnknown");
      planCounts.set(plan, (planCounts.get(plan) ?? 0) + 1);
    }
    const plans = [...planCounts.entries()].sort((a, b) => b[1] - a[1]);
    return {
      total: rows.length,
      oauth: rows.length - relay,
      relay,
      recent24h,
      plans,
    };
  }, [rows, t]);

  const planDetailItems = useMemo(
    () => stats.plans.map(([label, value]) => ({ label, value })),
    [stats.plans],
  );

  const filteredRows = useMemo(() => {
    const keyword = search.trim().toLowerCase();
    return rows.filter((row) => {
      if (planFilter !== "all") {
        const plan = (row.plan_type || "").toLowerCase().trim();
        if (planFilter === "unknown") {
          if (plan !== "") return false;
        } else if (plan !== planFilter) {
          return false;
        }
      }
      if (!keyword) return true;
      return [row.email, row.name, row.base_url, row.plan_type]
        .filter(Boolean)
        .some((field) => String(field).toLowerCase().includes(keyword));
    });
  }, [rows, search, planFilter]);

  const totalPages = Math.max(1, Math.ceil(filteredRows.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pagedRows = useMemo(
    () =>
      filteredRows.slice(
        (currentPage - 1) * pageSize,
        currentPage * pageSize,
      ),
    [filteredRows, currentPage, pageSize],
  );

  useEffect(() => {
    setPage(1);
  }, [search, planFilter]);

  const allPageSelected =
    pagedRows.length > 0 && pagedRows.every((row) => selectedIds.has(row.id));

  const toggleSelect = (id: number) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const toggleSelectAll = () => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (allPageSelected) {
        pagedRows.forEach((row) => next.delete(row.id));
      } else {
        pagedRows.forEach((row) => next.add(row.id));
      }
      return next;
    });
  };

  const handleRestore = async (row: RecycleBinAccountRow) => {
    setActingId(row.id);
    try {
      await api.restoreAccount(row.id);
      showToast(t("accounts.recycleBinRestored"));
      void load();
      onChanged();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setActingId(null);
    }
  };

  const handlePurge = async (row: RecycleBinAccountRow) => {
    const confirmed = await confirm({
      title: t("accounts.recycleBinPurgeTitle"),
      description: t("accounts.recycleBinPurgeDesc", {
        account: row.email || row.name || `ID ${row.id}`,
      }),
      confirmText: t("accounts.recycleBinPurge"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setActingId(row.id);
    try {
      await api.purgeAccount(row.id);
      showToast(t("accounts.recycleBinPurged"));
      void load();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setActingId(null);
    }
  };

  const handleBatchRestore = async () => {
    const ids = [...selectedIds];
    if (ids.length === 0) return;
    setBatchActing(true);
    let success = 0;
    let failed = 0;
    for (const id of ids) {
      try {
        await api.restoreAccount(id);
        success++;
      } catch {
        failed++;
      }
    }
    setBatchActing(false);
    if (failed > 0) {
      showToast(
        `${t("accounts.recycleBinBatchRestored", { count: success })} · ${t("accounts.recycleBinBatchFailed", { count: failed })}`,
        "error",
      );
    } else {
      showToast(t("accounts.recycleBinBatchRestored", { count: success }));
    }
    void load();
    onChanged();
  };

  const handleBatchPurge = async () => {
    const ids = [...selectedIds];
    if (ids.length === 0) return;
    const confirmed = await confirm({
      title: t("accounts.recycleBinPurgeSelectedTitle"),
      description: t("accounts.recycleBinPurgeSelectedDesc", {
        count: ids.length,
      }),
      confirmText: t("accounts.recycleBinPurgeSelected"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchActing(true);
    let success = 0;
    let failed = 0;
    for (const id of ids) {
      try {
        await api.purgeAccount(id);
        success++;
      } catch {
        failed++;
      }
    }
    setBatchActing(false);
    if (failed > 0) {
      showToast(
        `${t("accounts.recycleBinBatchPurged", { count: success })} · ${t("accounts.recycleBinBatchFailed", { count: failed })}`,
        "error",
      );
    } else {
      showToast(t("accounts.recycleBinBatchPurged", { count: success }));
    }
    void load();
  };

  const toggleAutoRestore = () => {
    setAutoRestore((prev) => {
      const next = !prev;
      try {
        window.localStorage.setItem(
          RECYCLE_BIN_AUTO_RESTORE_KEY,
          next ? "1" : "0",
        );
      } catch {
        /* localStorage 不可用时仅保留会话内状态 */
      }
      return next;
    });
  };

  const handleBatchTestRun = async (ids?: number[]) => {
    if (ids && ids.length === 0) return;
    setBatchTesting(true);
    try {
      const result = await runStreamingOperation(
        "/accounts/recycle-bin/batch-test?stream=true",
        { ...(ids ? { ids } : {}), restore_on_success: autoRestore },
        t("accounts.recycleBinBatchTestProgressTitle"),
      );
      showToast(
        t("accounts.batchTestDone", {
          success: result?.success ?? 0,
          banned: result?.banned ?? 0,
          rateLimited: result?.rate_limited ?? 0,
          failed: result?.failed ?? 0,
        }),
      );
    } catch (error) {
      showToast(
        t("accounts.batchTestFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchTesting(false);
      void load();
      if (autoRestore) {
        onChanged();
      }
    }
  };

  const handleExport = async (
    format: "json" | "txt",
    scope: "selected" | "filtered",
  ) => {
    const ids =
      scope === "selected"
        ? [...selectedIds]
        : filteredRows.map((row) => row.id);
    if (ids.length === 0) {
      showToast(t("accounts.exportNoAccounts"), "error");
      return;
    }
    setExporting(true);
    try {
      // 回收站导出不带代理:这里是"恢复误删账号"的入口,不是迁移入口,没有
      // 承载勾选项的弹窗,默认不外泄代理里的明文口令。需要迁移代理绑定关系
      // 请用账号页的导出。
      const data = await api.exportRecycleBinAccounts(ids);
      if (data.length === 0) {
        showToast(t("accounts.exportNoAccounts"), "error");
        return;
      }
      const ts = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
      let exportedCount = data.length;
      if (format === "json") {
        const blob = new Blob([JSON.stringify(data, null, 2)], {
          type: "application/json",
        });
        downloadBlob(blob, `codex2api-recycle-${ts}-${data.length}.json`);
      } else {
        // TXT：每行一个邮箱（无邮箱则用 account_id 兜底），不导出 token。
        const lines = data
          .map((e) => {
            const email = (e.email || "").trim();
            if (email) return email;
            return (e.account_id || "").trim();
          })
          .filter(Boolean);
        if (lines.length === 0) {
          showToast(t("accounts.exportNoAccounts"), "error");
          return;
        }
        exportedCount = lines.length;
        const blob = new Blob([`${lines.join("\n")}\n`], {
          type: "text/plain;charset=utf-8",
        });
        downloadBlob(
          blob,
          `codex2api-recycle-emails-${ts}-${lines.length}.txt`,
        );
      }
      showToast(t("accounts.exportSuccess", { count: exportedCount }));
    } catch (error) {
      showToast(
        `${t("accounts.exportFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setExporting(false);
    }
  };

  const emptyKeyword = t("accounts.recycleBinEmptyKeyword");
  const emptyConfirmMatched = emptyConfirmText.trim() === emptyKeyword;

  const openEmptyConfirm = () => {
    setEmptyConfirmText("");
    setShowEmptyConfirm(true);
  };

  const handleEmpty = async () => {
    if (!emptyConfirmMatched) return;
    setShowEmptyConfirm(false);
    setEmptying(true);
    try {
      await api.emptyRecycleBin();
      showToast(t("accounts.recycleBinEmptied"));
      void load();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setEmptying(false);
    }
  };

  return (
    <>
      <PageHeader
        title={t("accounts.recycleBinTitle")}
        description={t("accounts.recycleBinDesc")}
        onRefresh={() => void load()}
        actions={
          <div className="flex flex-wrap items-center gap-1.5 sm:justify-end">
            <Button
              variant="outline"
              size="sm"
              onClick={onClose}
            >
              <ArrowLeft className="size-3.5" />
              {t("accounts.recycleBinBack")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              aria-pressed={autoRestore}
              onClick={toggleAutoRestore}
              title={t("accounts.recycleBinAutoRestoreHint")}
            >
              {autoRestore ? (
                <ToggleRight className="size-4 text-emerald-500" />
              ) : (
                <ToggleLeft className="size-4 text-muted-foreground" />
              )}
              <span className="max-sm:hidden">{t("accounts.recycleBinAutoRestore")}</span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={busy || loading || filteredRows.length === 0}
              onClick={() => void handleExport("json", "filtered")}
              title={t("accounts.recycleBinExportFilteredJson")}
            >
              <Download className="size-3.5" />
              <span className="max-sm:hidden">
                {exporting
                  ? t("accounts.recycleBinExporting")
                  : t("accounts.recycleBinExportFilteredJson")}
              </span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={busy || loading || filteredRows.length === 0}
              onClick={() => void handleExport("txt", "filtered")}
              title={t("accounts.recycleBinExportFilteredTxt")}
            >
              <FileText className="size-3.5" />
              <span className="max-sm:hidden">
                {t("accounts.recycleBinExportFilteredTxt")}
              </span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={busy || loading || rows.length === 0}
              onClick={() => void handleBatchTestRun()}
            >
              <FlaskConical
                className={`size-3.5 ${batchTesting ? "animate-pulse" : ""}`}
              />
              <span className="max-sm:hidden">
                {batchTesting
                  ? t("accounts.batchTesting")
                  : t("accounts.recycleBinTestAll")}
              </span>
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={busy || loading || rows.length === 0}
              onClick={openEmptyConfirm}
            >
              <Trash2 className="size-3.5" />
              <span className="max-sm:hidden">{t("accounts.recycleBinEmptyBin")}</span>
            </Button>
          </div>
        }
      />

      <div className="mb-4 grid grid-cols-1 gap-3 sm:grid-cols-3">
        <CompactStat
          label={t("accounts.recycleBinStatTotal")}
          value={stats.total}
          tone="neutral"
          details={[
            { label: t("accounts.recycleBinTypeOauth"), value: stats.oauth },
            { label: t("accounts.recycleBinTypeRelay"), value: stats.relay },
          ]}
        />
        <CompactStat
          label={t("accounts.recycleBinStatPlans")}
          value={stats.plans.length}
          tone="success"
          details={planDetailItems}
        />
        <CompactStat
          label={t("accounts.recycleBinStatRecent24h")}
          value={stats.recent24h}
          tone="danger"
        />
      </div>

      <Card>
        <CardContent className="p-0">
          <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border px-4 py-3">
            <div className="flex flex-wrap items-center gap-2">
              <div className="relative w-full sm:w-72">
                <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  placeholder={t("accounts.recycleBinSearchPlaceholder")}
                  className="pl-8"
                />
              </div>
              <div className="flex shrink-0 items-center gap-1 rounded-lg border border-border bg-muted/30 p-0.5 max-sm:w-full max-sm:flex-wrap">
                {(
                  [
                    "all",
                    "pro",
                    "prolite",
                    "plus",
                    "team",
                    "k12",
                    "free",
                    "api",
                    "unknown",
                  ] as const
                ).map((key) => (
                  <button
                    key={key}
                    onClick={() => setPlanFilter(key)}
                    className={`whitespace-nowrap rounded-md px-2.5 py-1 text-[12px] font-medium transition-colors ${
                      planFilter === key
                        ? "bg-background shadow-sm text-foreground"
                        : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {key === "all"
                      ? t("accounts.filterAll")
                      : key === "unknown"
                        ? t("accounts.recycleBinPlanUnknown")
                        : key === "prolite"
                          ? "ProLite"
                          : key === "k12"
                            ? "K12"
                            : key === "api"
                              ? "API"
                              : key.charAt(0).toUpperCase() + key.slice(1)}
                  </button>
                ))}
              </div>
            </div>
            {selectedIds.size > 0 ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="text-xs text-muted-foreground">
                  {t("accounts.recycleBinSelected", {
                    count: selectedIds.size,
                  })}
                </span>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => void handleExport("json", "selected")}
                  title={t("accounts.recycleBinExportSelectedJson")}
                >
                  <Download className="size-3.5" />
                  {t("accounts.recycleBinExportSelectedJson")}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => void handleExport("txt", "selected")}
                  title={t("accounts.recycleBinExportSelectedTxt")}
                >
                  <FileText className="size-3.5" />
                  {t("accounts.recycleBinExportSelectedTxt")}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => void handleBatchTestRun([...selectedIds])}
                >
                  <FlaskConical className="size-3.5" />
                  {t("accounts.recycleBinTestSelected")}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => void handleBatchRestore()}
                >
                  <ArchiveRestore className="size-3.5" />
                  {t("accounts.recycleBinRestoreSelected")}
                </Button>
                <Button
                  size="sm"
                  variant="destructive"
                  disabled={busy}
                  onClick={() => void handleBatchPurge()}
                >
                  <Trash2 className="size-3.5" />
                  {t("accounts.recycleBinPurgeSelected")}
                </Button>
              </div>
            ) : null}
          </div>

          {loading ? (
            <div className="px-6 py-10 text-center text-sm text-muted-foreground">
              {t("common.loading")}
            </div>
          ) : error ? (
            <div className="px-6 py-10 text-center text-sm text-destructive">
              {error}
            </div>
          ) : rows.length === 0 ? (
            <div className="px-6 py-10 text-center text-sm text-muted-foreground">
              {t("accounts.recycleBinEmptyState")}
            </div>
          ) : filteredRows.length === 0 ? (
            <div className="px-6 py-10 text-center text-sm text-muted-foreground">
              {t("accounts.recycleBinNoMatch")}
            </div>
          ) : (
            <>
              <div className="overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-10">
                        <input
                          type="checkbox"
                          className="size-4 cursor-pointer accent-primary"
                          checked={allPageSelected}
                          onChange={toggleSelectAll}
                        />
                      </TableHead>
                      <TableHead className="w-14">
                        {t("accounts.sequence")}
                      </TableHead>
                      <TableHead>{t("accounts.recycleBinAccount")}</TableHead>
                      <TableHead>{t("accounts.recycleBinPlan")}</TableHead>
                      <TableHead>{t("accounts.recycleBinType")}</TableHead>
                      <TableHead>
                        {t("accounts.recycleBinImportedAt")}
                      </TableHead>
                      <TableHead>
                        {t("accounts.recycleBinDeletedAt")}
                      </TableHead>
                      <TableHead>{t("accounts.recycleBinLastTest")}</TableHead>
                      <TableHead className="text-right">
                        {t("accounts.recycleBinActions")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {pagedRows.map((row, index) => (
                      <TableRow key={row.id}>
                        <TableCell>
                          <input
                            type="checkbox"
                            className="size-4 cursor-pointer accent-primary"
                            checked={selectedIds.has(row.id)}
                            onChange={() => toggleSelect(row.id)}
                          />
                        </TableCell>
                        <TableCell className="tabular-nums text-muted-foreground">
                          {(currentPage - 1) * pageSize + index + 1}
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-col gap-0.5">
                            <span className="font-medium">
                              {row.email || row.name || `ID ${row.id}`}
                            </span>
                            {row.openai_responses_api && row.base_url ? (
                              <span className="text-xs text-muted-foreground">
                                {row.base_url}
                              </span>
                            ) : null}
                          </div>
                        </TableCell>
                        <TableCell>
                          <Badge variant="outline">
                            {row.plan_type?.trim() ||
                              t("accounts.recycleBinPlanUnknown")}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <Badge variant="secondary">
                            {row.claude_api
                              ? t("accounts.providerViewClaude")
                              : row.openai_responses_api
                                ? t("accounts.recycleBinTypeRelay")
                                : t("accounts.recycleBinTypeOauth")}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-col gap-0.5">
                            <span className="text-sm tabular-nums">
                              {formatBeijingTime(row.created_at)}
                            </span>
                            <span className="text-xs text-muted-foreground">
                              {formatRelativeTime(row.created_at)}
                            </span>
                          </div>
                        </TableCell>
                        <TableCell>
                          {row.deleted_at ? (
                            <div className="flex flex-col gap-0.5">
                              <span className="text-sm tabular-nums">
                                {formatBeijingTime(row.deleted_at)}
                              </span>
                              <span className="text-xs text-muted-foreground">
                                {formatRelativeTime(row.deleted_at)}
                              </span>
                            </div>
                          ) : (
                            "-"
                          )}
                        </TableCell>
                        <TableCell>
                          {(() => {
                            const st = row.last_test_status;
                            if (!st) {
                              return (
                                <span className="text-muted-foreground">
                                  -
                                </span>
                              );
                            }
                            const passed = st === "success";
                            const limited = st === "rate_limited";
                            const badgeClass = passed
                              ? "border-emerald-300 text-emerald-600 dark:border-emerald-800 dark:text-emerald-400"
                              : limited
                                ? "border-amber-300 text-amber-600 dark:border-amber-800 dark:text-amber-400"
                                : "border-red-300 text-red-600 dark:border-red-900 dark:text-red-400";
                            const label = passed
                              ? t("accounts.recycleBinTestPassed")
                              : limited
                                ? t("accounts.recycleBinTestRateLimited")
                                : t("accounts.recycleBinTestFailedLabel");
                            return (
                              <div className="flex flex-col items-start gap-0.5">
                                <Badge variant="outline" className={badgeClass}>
                                  {label}
                                </Badge>
                                {row.last_test_at ? (
                                  <span className="text-xs tabular-nums text-muted-foreground">
                                    {formatBeijingTime(row.last_test_at)}
                                  </span>
                                ) : null}
                              </div>
                            );
                          })()}
                        </TableCell>
                        <TableCell className="text-right">
                          <div className="flex items-center justify-end gap-1.5">
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={actingId === row.id || busy}
                              onClick={() => setTestingRow(row)}
                            >
                              <FlaskConical className="size-3.5" />
                              {t("accounts.recycleBinTest")}
                            </Button>
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={actingId === row.id || busy}
                              onClick={() => void handleRestore(row)}
                            >
                              <ArchiveRestore className="size-3.5" />
                              {t("accounts.recycleBinRestore")}
                            </Button>
                            <Button
                              size="sm"
                              variant="destructive"
                              disabled={actingId === row.id || busy}
                              onClick={() => void handlePurge(row)}
                            >
                              <Trash2 className="size-3.5" />
                              {t("accounts.recycleBinPurge")}
                            </Button>
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <div className="border-t border-border px-4 py-3">
                <Pagination
                  page={currentPage}
                  totalPages={totalPages}
                  onPageChange={setPage}
                  totalItems={filteredRows.length}
                  pageSize={pageSize}
                  pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
                  onPageSizeChange={(nextPageSize) => {
                    setPageSize(nextPageSize);
                    setPage(1);
                  }}
                />
              </div>
            </>
          )}
        </CardContent>
      </Card>

      {testingRow ? (
        <TestConnectionModal
          account={recycleBinRowToAccountRow(testingRow)}
          onSettled={() => {
            void load();
            if (autoRestore) {
              onChanged();
            }
          }}
          onClose={() => setTestingRow(null)}
          successHint={
            autoRestore
              ? t("accounts.recycleBinTestAutoRestoredHint")
              : t("accounts.recycleBinTestSuccessHint")
          }
          restoreOnSuccess={autoRestore}
        />
      ) : null}

      <Modal
        show={showEmptyConfirm}
        title={t("accounts.recycleBinEmptyTitle")}
        onClose={() => setShowEmptyConfirm(false)}
        footer={
          <>
            <Button
              variant="outline"
              onClick={() => setShowEmptyConfirm(false)}
            >
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={!emptyConfirmMatched || busy}
              onClick={() => void handleEmpty()}
            >
              <Trash2 className="size-3.5" />
              {t("accounts.recycleBinEmptyBin")}
            </Button>
          </>
        }
      >
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            {t("accounts.recycleBinEmptyConfirmPrompt", {
              count: rows.length,
            })}
          </p>
          <p className="text-sm">
            {t("accounts.recycleBinEmptyTypeToConfirm")}
            <code className="ml-1 rounded bg-muted px-1.5 py-0.5 font-semibold text-destructive">
              {emptyKeyword}
            </code>
          </p>
          <Input
            value={emptyConfirmText}
            onChange={(e) => setEmptyConfirmText(e.target.value)}
            placeholder={emptyKeyword}
            autoFocus
          />
        </div>
      </Modal>
    </>
  );
}

const RECYCLE_BIN_AUTO_RESTORE_KEY = "codex2api_recycle_bin_auto_restore";

// recycleBinRowToAccountRow 将回收站行转换为 TestConnectionModal 需要的最小 AccountRow。
function recycleBinRowToAccountRow(row: RecycleBinAccountRow): AccountRow {
  return {
    id: row.id,
    name: row.name,
    email: row.email,
    plan_type: row.plan_type,
    status: "deleted",
    openai_responses_api: row.openai_responses_api,
    claude_api: row.claude_api,
    base_url: row.base_url,
    models: row.models,
    proxy_url: "",
    created_at: row.created_at,
    updated_at: row.deleted_at || row.created_at,
  };
}

function formatJSONText(text: string) {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

async function copyTextToClipboard(text: string) {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall back for browsers that block clipboard writes.
    }
  }

  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "fixed";
  textarea.style.top = "0";
  textarea.style.left = "0";
  textarea.style.width = "1px";
  textarea.style.height = "1px";
  textarea.style.opacity = "0";
  textarea.style.pointerEvents = "none";
  document.body.appendChild(textarea);
  textarea.focus({ preventScroll: true });
  textarea.select();
  textarea.setSelectionRange(0, text.length);
  const copied = document.execCommand("copy");
  document.body.removeChild(textarea);
  if (!copied) {
    throw new Error("copy failed");
  }
}

function filterExistingAPIKeyIDs(
  selected: number[],
  apiKeys: APIKeyRow[],
): number[] {
  if (!selected.length || !apiKeys.length) {
    return [];
  }
  const existing = new Set(apiKeys.map((item) => item.id));
  return [...new Set(selected.filter((id) => existing.has(id)))].sort(
    (a, b) => a - b,
  );
}

function formatAPIKeyOptionLabel(apiKey: APIKeyRow): string {
  const name = apiKey.name?.trim() || `API Key #${apiKey.id}`;
  return `${name} · ${apiKey.key}`;
}

function APIKeyMultiSelect({
  options,
  value,
  disabled,
  onChange,
  allLabel,
  selectedLabel,
  placeholder,
  emptyLabel,
  emptyHint,
}: {
  options: APIKeyRow[];
  value: number[];
  disabled: boolean;
  onChange: (value: number[]) => void;
  allLabel: string;
  selectedLabel: string;
  placeholder: string;
  emptyLabel: string;
  emptyHint: string;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;

    const handlePointerDown = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };

    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
      }
    };

    document.addEventListener("mousedown", handlePointerDown);
    document.addEventListener("keydown", handleEscape);
    return () => {
      document.removeEventListener("mousedown", handlePointerDown);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [open]);

  const summary = value.length === 0 ? allLabel : selectedLabel;

  const toggleOption = (id: number) => {
    if (disabled) return;
    if (value.includes(id)) {
      onChange(value.filter((item) => item !== id));
      return;
    }
    onChange([...value, id].sort((a, b) => a - b));
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        disabled={disabled}
        className={`flex w-full items-center justify-between gap-3 rounded-md border border-input bg-background px-3.5 py-3 text-left shadow-xs transition-[border-color,box-shadow] ${
          disabled
            ? "cursor-not-allowed opacity-70"
            : "hover:border-primary/30 hover:bg-accent/40"
        } ${open ? "border-primary/35 ring-[3px] ring-primary/10" : ""}`}
        onClick={() => {
          if (!disabled) {
            setOpen((current) => !current);
          }
        }}
      >
        <div className="min-w-0">
          <div className="truncate text-[15px] text-foreground">{summary}</div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            {disabled ? emptyHint : placeholder}
          </div>
        </div>
        <ChevronDown
          className={`size-4 shrink-0 text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`}
        />
      </button>

      {open ? (
        <div className="absolute left-0 right-0 top-[calc(100%+0.5rem)] z-50 overflow-hidden rounded-lg border border-border bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm">
          {options.length === 0 ? (
            <div className="px-4 py-3 text-sm text-muted-foreground">
              {emptyLabel}
            </div>
          ) : (
            <div className="max-h-72 space-y-1 overflow-auto p-2">
              {options.map((option) => {
                const checked = value.includes(option.id);
                return (
                  <button
                    key={option.id}
                    type="button"
                    className={`flex w-full items-center gap-3 rounded-md px-3 py-2.5 text-left transition-colors ${
                      checked
                        ? "bg-primary/10 text-primary"
                        : "text-foreground hover:bg-accent/70"
                    }`}
                    onClick={() => toggleOption(option.id)}
                  >
                    <span
                      className={`flex size-4 items-center justify-center rounded border ${checked ? "border-primary bg-primary text-primary-foreground" : "border-border bg-background text-transparent"}`}
                    >
                      <Check className="size-3" />
                    </span>
                    <span className="min-w-0 flex-1 truncate text-sm">
                      {formatAPIKeyOptionLabel(option)}
                    </span>
                  </button>
                );
              })}
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}

type ModelProbeStatus =
  | "pending"
  | "testing"
  | "available"
  | "unsupported"
  | "throttled"
  | "error";

type ModelProbeItem = {
  model: string;
  status: ModelProbeStatus;
  detail?: string;
};

// ModelProbeBoard 逐模型实时探测看板：每个模型显示 等待/测试中/结果 的动画状态。
function ModelProbeBoard({
  items,
  t,
}: {
  items: ModelProbeItem[];
  t: ReturnType<typeof useTranslation>["t"];
}) {
  const tested = items.filter(
    (it) => it.status !== "pending" && it.status !== "testing",
  ).length;
  const availableCount = items.filter(
    (it) => it.status === "available",
  ).length;

  const statusMeta: Record<
    ModelProbeStatus,
    { icon: ReactNode; ring: string; text: string; label: string }
  > = {
    pending: {
      icon: <Hourglass className="size-3.5" />,
      ring: "border-border bg-muted/20",
      text: "text-muted-foreground/70",
      label: t("accounts.probeStatePending"),
    },
    testing: {
      icon: <RefreshCw className="size-3.5 animate-spin" />,
      ring: "border-primary/40 bg-primary/5",
      text: "text-primary",
      label: t("accounts.probeStateTesting"),
    },
    available: {
      icon: <Check className="size-3.5" />,
      ring: "border-emerald-500/30 bg-emerald-500/5",
      text: "text-emerald-600 dark:text-emerald-400",
      label: t("accounts.probeStateAvailable"),
    },
    unsupported: {
      icon: <Ban className="size-3.5" />,
      ring: "border-rose-500/30 bg-rose-500/5",
      text: "text-rose-600 dark:text-rose-400",
      label: t("accounts.probeStateUnsupported"),
    },
    throttled: {
      icon: <Timer className="size-3.5" />,
      ring: "border-amber-500/30 bg-amber-500/5",
      text: "text-amber-600 dark:text-amber-400",
      label: t("accounts.probeStateThrottled"),
    },
    error: {
      icon: <AlertTriangle className="size-3.5" />,
      ring: "border-border bg-muted/30",
      text: "text-muted-foreground",
      label: t("accounts.probeStateError"),
    },
  };

  return (
    <div className="space-y-2 rounded-lg border border-border bg-card p-3">
      <div className="flex items-center justify-between text-xs">
        <span className="font-medium text-foreground">
          {t("accounts.probeBoardTitle")}
        </span>
        <span className="tabular-nums text-muted-foreground">
          {t("accounts.probeBoardProgress", {
            tested,
            total: items.length,
            available: availableCount,
          })}
        </span>
      </div>
      <div className="grid grid-cols-1 gap-1.5 sm:grid-cols-2">
        {items.map((it) => {
          const meta = statusMeta[it.status];
          return (
            <div
              key={it.model}
              className={`flex items-center gap-2 rounded-md border px-2.5 py-1.5 transition-colors duration-300 ${meta.ring}`}
              title={it.detail || meta.label}
            >
              <span className={`shrink-0 ${meta.text}`}>{meta.icon}</span>
              <span className="min-w-0 flex-1 truncate font-mono text-[12px] text-foreground">
                {it.model}
              </span>
              <span className={`shrink-0 text-[11px] ${meta.text}`}>
                {meta.label}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function ModelChipGrid({
  models,
  onRemove,
  emptyLabel,
  variant = "grid",
}: {
  models: string[];
  onRemove: (model: string) => void;
  emptyLabel: string;
  variant?: "grid" | "pills";
}) {
  if (models.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border bg-muted/20 px-3 py-3 text-sm text-muted-foreground">
        {emptyLabel}
      </div>
    );
  }
  if (variant === "pills") {
    return (
      <div className="flex flex-wrap gap-1.5 rounded-lg border border-border bg-muted/10 p-2.5">
        {models.map((model) => (
          <span
            key={model}
            className="inline-flex items-center gap-1 rounded-md border border-border bg-background py-1 pl-2 pr-1 text-[12px]"
            title={model}
          >
            <span className="font-mono text-foreground">{model}</span>
            <button
              type="button"
              className="inline-flex size-4 shrink-0 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              onClick={() => onRemove(model)}
              aria-label={`Remove ${model}`}
            >
              <X className="size-3" />
            </button>
          </span>
        ))}
      </div>
    );
  }
  return (
    <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
      {models.map((model) => (
        <div
          key={model}
          className="flex min-h-10 items-center justify-between gap-2 rounded-md border border-border bg-background px-3 py-2 text-sm"
          title={model}
        >
          <span className="min-w-0 truncate font-mono text-[12px] text-foreground">
            {model}
          </span>
          <button
            type="button"
            className="inline-flex size-6 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            onClick={() => onRemove(model)}
            aria-label={`Remove ${model}`}
          >
            <X className="size-3.5" />
          </button>
        </div>
      ))}
    </div>
  );
}

function TogglePill({
  active,
  onClick,
  label,
  className,
}: {
  active: boolean;
  onClick: () => void;
  label: string;
  className?: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-lg px-3 py-1.5 text-xs font-semibold transition-all duration-150 ${
        active
          ? "bg-primary text-primary-foreground shadow-2xs ring-1 ring-primary/20"
          : "bg-muted/60 text-muted-foreground hover:bg-muted hover:text-foreground"
      } ${className ?? ""}`}
    >
      {label}
    </button>
  );
}

function PreviewItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-border bg-muted/20 px-3 py-3">
      <div className="text-[11px] font-semibold text-muted-foreground">
        {label}
      </div>
      <div className="mt-1 text-base font-semibold text-foreground">
        {value}
      </div>
    </div>
  );
}

function QuotaAutoPauseWindowEditor({
  disabledLabel,
  disabledHint,
  thresholdLabel,
  thresholdHint,
  thresholdPlaceholder,
  thresholdValue,
  thresholdInvalid,
  disabled,
  invalidLabel,
  onThresholdChange,
  onDisabledChange,
}: {
  disabledLabel: string;
  disabledHint: string;
  thresholdLabel: string;
  thresholdHint: string;
  thresholdPlaceholder: string;
  thresholdValue: string;
  thresholdInvalid: boolean;
  disabled: boolean;
  invalidLabel: string;
  onThresholdChange: (value: string) => void;
  onDisabledChange: (value: boolean) => void;
}) {
  return (
    <div className="rounded-lg border border-border bg-muted/10 p-3">
      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="text-sm font-semibold text-foreground">
            {disabledLabel}
          </div>
          <div className="mt-1 text-xs text-muted-foreground">
            {disabledHint}
          </div>
        </div>
        <button
          type="button"
          role="switch"
          aria-label={disabledLabel}
          aria-checked={disabled}
          onClick={() => onDisabledChange(!disabled)}
          className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60 focus-visible:ring-offset-2 ${disabled ? "bg-primary" : "bg-muted"}`}
        >
          <span
            className={`pointer-events-none block size-4 rounded-full bg-white shadow transition-transform ${disabled ? "translate-x-4" : "translate-x-0"}`}
          />
        </button>
      </div>
      <label className="mt-4 block text-sm font-semibold text-muted-foreground">
        {thresholdLabel}
      </label>
      <Input
        className="mt-2"
        type="number"
        min={0}
        max={100}
        step={0.1}
        inputMode="decimal"
        value={thresholdValue}
        placeholder={thresholdPlaceholder}
        onChange={(event: ChangeEvent<HTMLInputElement>) =>
          onThresholdChange(event.target.value)
        }
      />
      <div
        className={`mt-1.5 text-xs ${thresholdInvalid ? "text-red-500" : "text-muted-foreground"}`}
      >
        {thresholdInvalid ? invalidLabel : thresholdHint}
      </div>
    </div>
  );
}

function parseIntegerInput(value: string): number | null {
  const trimmed = value.trim();
  if (!trimmed || !/^-?\d+$/.test(trimmed)) {
    return null;
  }

  return Number.parseInt(trimmed, 10);
}

function getDispatchScore(account: AccountRow): number {
  return account.dispatch_score ?? account.scheduler_score ?? 0;
}

function getSchedulerPriority(account: AccountRow): number {
  const value = account.scheduler_priority;
  return typeof value === "number" && Number.isFinite(value)
    ? Math.trunc(value)
    : 0;
}

// UsingCreditsBadge 紧跟在限流徽章后面：账号显示仍是「限流」（用量窗口客观上确实打满了），
// 这个积分徽章表示"正在用积分顶替限流，因此仍在参与调度"。
// 后端 using_credits 已经算过全部前提（两个开关 + 当下确有余额 + 窗口真打满），
// 这里只负责显示，不重算，避免两边判定漂移。
function UsingCreditsBadge({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  if (!account.using_credits) return null;
  // 纯图标：余额已经在邮箱下方的徽章里显示过，这里再写一遍是重复信息，
  // 而且会把「限流 | 7d」和倒计时之间撑开。语义靠 title / aria-label 承载。
  return (
    <span
      className="inline-flex size-5 shrink-0 items-center justify-center rounded-md border border-teal-500/30 bg-teal-500/10 text-teal-700 dark:text-teal-300"
      title={t("accounts.usingCreditsBadgeTooltip")}
      aria-label={t("accounts.usingCreditsBadge")}
    >
      <Coins className="size-3" />
    </span>
  );
}

function SchedulerPriorityBadge({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const priority = getSchedulerPriority(account);
  const value = priority > 0 ? `+${priority}` : String(priority);
  const tone =
    priority > 0
      ? "border-blue-500/25 bg-blue-500/10 text-blue-700 dark:text-blue-300"
      : priority < 0
        ? "border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300"
        : "border-border bg-muted/40 text-muted-foreground";

  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-md border px-1.5 py-0.5 text-[10px] font-semibold tabular-nums ${tone}`}
      title={t("accounts.schedulerPriorityBadgeTitle", { value })}
    >
      P {value}
    </span>
  );
}

// OpenAI reports the $100 Pro tier as "prolite" — functionally a Pro plan with
// a smaller usage cap. Keep behavioral comparisons (usage windows, plan filter,
// scheduler bias) aligned with the Go side by folding it into "pro".
function normalizePlanType(planType?: string): string {
  const raw = (planType || "").toLowerCase().trim();
  if (raw === "prolite" || raw === "pro_lite" || raw === "pro-lite")
    return "pro";
  return raw;
}

function isFutureTime(value?: string): boolean {
  if (!value) return false;
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) && timestamp > Date.now();
}

function isUsageWindowExhausted(value?: number | null): boolean {
  return typeof value === "number" && Number.isFinite(value) && value >= 100;
}

function isActiveUsageWindowExhausted(
  value?: number | null,
  resetAt?: string,
): boolean {
  return isUsageWindowExhausted(value) && (!resetAt || isFutureTime(resetAt));
}

function isActiveAutoPauseWindowReached(
  value?: number | null,
  threshold?: number | null,
  disabled?: boolean,
  resetAt?: string,
): boolean {
  if (disabled || typeof threshold !== "number" || threshold <= 0) {
    return false;
  }
  if (typeof value !== "number") {
    return false;
  }
  if (resetAt && !isFutureTime(resetAt)) {
    return false;
  }
  return value / 100 >= threshold;
}

// Plans that carry a rolling 5h usage window (mirrors Go isPremium5hPlan).
// k12/edu are paid education workspaces with 5h limits (issue #307/#309).
function isSparkUsagePlan(planType?: string): boolean {
  return normalizePlanType(planType) === "pro";
}

function isPremiumUsagePlan(planType?: string): boolean {
  return [
    "plus",
    "pro",
    "team",
    "teamplus",
    "k12",
    "edu",
    "education",
    "go",
  ].includes(normalizePlanType(planType));
}

type RateLimitWindow = "5h" | "7d";

function isRateLimitedAccount(account: AccountRow): boolean {
  // 积分顶替限流的账号：状态徽章仍写「限流」（用量窗口客观上确实打满了），但调度侧
  // 照常放行。健康分类与筛选按"可用"计，否则一个正在正常干活的账号会被算进限流数、
  // 被「限流」筛选捞出来，与旁边的积分徽章互相矛盾。
  if (account.using_credits) return false;
  return getAccountRateLimitWindow(account) !== null;
}

function getAccountRateLimitWindow(
  account: AccountRow,
): RateLimitWindow | null {
  const status = (account.status || "").toLowerCase();
  const reason = (account.cooldown_reason || "").toLowerCase();
  const explicitlyRateLimited =
    status === "rate_limited" ||
    status === "responses_rate_limited" ||
    status === "usage_exhausted" ||
    status === "quota_paused" ||
    status === "rate_limited_5h" ||
    status === "rate_limited_7d" ||
    reason === "rate_limited" ||
    reason === "responses_rate_limited" ||
    reason === "rate_limited_5h" ||
    reason === "rate_limited_7d";
  const usageWindowsAreInformational =
    account.ignore_usage_limit_status_effective === true;
  const has7dLimit =
    !usageWindowsAreInformational &&
    isActiveUsageWindowExhausted(
      account.usage_percent_7d,
      account.reset_7d_at,
    );
  const has5hLimit =
    !usageWindowsAreInformational &&
    isPremiumUsagePlan(account.plan_type) &&
    isActiveUsageWindowExhausted(account.usage_percent_5h, account.reset_5h_at);
  const has5hAutoPause = isActiveAutoPauseWindowReached(
    account.usage_percent_5h,
    account.auto_pause_5h_threshold,
    account.auto_pause_5h_disabled,
    account.reset_5h_at,
  );
  const has7dAutoPause = isActiveAutoPauseWindowReached(
    account.usage_percent_7d,
    account.auto_pause_7d_threshold,
    account.auto_pause_7d_disabled,
    account.reset_7d_at,
  );

  // Prefer the longer 7d window when both windows are exhausted so each account
  // belongs to exactly one bucket and 5h + 7d stays equal to total limited.
  if (
    status === "usage_exhausted" ||
    status === "rate_limited_7d" ||
    reason === "rate_limited_7d" ||
    has7dLimit ||
    has7dAutoPause
  ) {
    return "7d";
  }

  if (
    status === "rate_limited_5h" ||
    reason === "rate_limited_5h" ||
    has5hLimit ||
    has5hAutoPause
  ) {
    return "5h";
  }

  if (!explicitlyRateLimited) {
    return null;
  }

  // A generic Responses 429 does not identify its quota window. Until the
  // immediate WHAM refresh replaces that transient state, keep the badge
  // aligned with the usage bar that actually exists instead of inventing 5h.
  const has5hWindow =
    typeof account.usage_percent_5h === "number" || !!account.reset_5h_at;
  const has7dWindow =
    typeof account.usage_percent_7d === "number" ||
    hasUsageWindowDetail(account.usage_7d_detail) ||
    !!account.reset_7d_at;
  if (has7dWindow && !has5hWindow) {
    return "7d";
  }
  return "5h";
}

function getRateLimitedWindowStats(accounts: AccountRow[]): {
  total: number;
  fiveHour: number;
  sevenDay: number;
} {
  const stats = accounts.reduce(
    (stats, account) => {
      // 走 isRateLimitedAccount 而不是直接取窗口：积分顶替限流的账号按可用计，
      // 这样 5h + 7d 仍然等于总限流数，不会比汇总里的「限流」多出几个。
      const window = isRateLimitedAccount(account)
        ? getAccountRateLimitWindow(account)
        : null;
      if (!window) {
        return stats;
      }
      if (window === "7d") {
        stats.sevenDay += 1;
      } else {
        stats.fiveHour += 1;
      }
      return stats;
    },
    { fiveHour: 0, sevenDay: 0 },
  );

  return {
    ...stats,
    total: stats.fiveHour + stats.sevenDay,
  };
}

function isSubscriptionPlan(planType?: string): boolean {
  const normalized = normalizePlanType(planType);
  if (!normalized || normalized === "free") return false;
  if (
    [
      "plus",
      "pro",
      "team",
      "teamplus",
      "enterprise",
      "business",
      "edu",
      "education",
      "k12",
      "go",
    ].includes(normalized)
  ) {
    return true;
  }
  return (
    normalized.includes("plus") ||
    normalized.startsWith("pro") ||
    normalized.startsWith("team") ||
    normalized.includes("enterprise") ||
    normalized.includes("business")
  );
}

function OperationProgressToast({
  progress,
  onClose,
}: {
  progress: OperationProgressState | null;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  if (!progress?.show) return null;

  const percent =
    progress.total > 0
      ? Math.min(100, Math.max(0, Math.round((progress.current / progress.total) * 100)))
      : 0;
  const metrics =
    progress.action === "batch_delete" || progress.action === "clean"
      ? [
          {
            label: t("accounts.operationProgressDeleted"),
            value: progress.deleted || progress.success,
            tone: "text-emerald-600 dark:text-emerald-400",
          },
          {
            label: t("accounts.operationProgressFailed"),
            value: progress.failed,
            tone: "text-red-600 dark:text-red-400",
          },
        ]
      : progress.action === "batch_refresh"
        ? [
            {
              label: t("accounts.operationProgressSuccess"),
              value: progress.success,
              tone: "text-emerald-600 dark:text-emerald-400",
            },
            {
              label: t("accounts.operationProgressFailed"),
              value: progress.failed,
              tone: "text-red-600 dark:text-red-400",
            },
          ]
        : [
            {
              label: t("accounts.operationProgressSuccess"),
              value: progress.success,
              tone: "text-emerald-600 dark:text-emerald-400",
            },
            {
              label: t("accounts.operationProgressBanned"),
              value: progress.banned,
              tone: "text-red-600 dark:text-red-400",
            },
            {
              label: t("accounts.operationProgressRateLimited"),
              value: progress.rateLimited,
              tone: "text-amber-600 dark:text-amber-400",
            },
            {
              label: t("accounts.operationProgressFailed"),
              value: progress.failed,
              tone: "text-red-600 dark:text-red-400",
            },
          ];

  return (
    <div className="fixed right-4 top-4 z-[80] w-[min(380px,calc(100vw-2rem))] rounded-lg border border-border bg-card/98 p-4 text-card-foreground shadow-xl backdrop-blur">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            {progress.done ? (
              <Check className="size-4 shrink-0 text-emerald-600 dark:text-emerald-400" />
            ) : (
              <Hourglass className="size-4 shrink-0 animate-pulse text-primary" />
            )}
            <div className="truncate text-sm font-semibold">{progress.title}</div>
          </div>
          <div className="mt-1 text-xs text-muted-foreground">
            {progress.done
              ? t("accounts.operationProgressDone")
              : t("accounts.operationProgressRunning")}
            {" · "}
            {progress.current}/{progress.total || 0}
          </div>
        </div>
        <button
          type="button"
          className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
          onClick={onClose}
          aria-label={t("common.close")}
        >
          <X className="size-4" />
        </button>
      </div>

      <div className="mt-3 h-2 overflow-hidden rounded-full bg-muted">
        <div
          className={`h-full rounded-full transition-all duration-300 ease-out ${
            progress.done ? "bg-emerald-500" : "bg-primary"
          }`}
          style={{ width: `${percent}%` }}
        />
      </div>

      <div className="mt-3 grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
        {metrics.map((metric) => (
          <div key={metric.label} className="rounded-md bg-muted/40 px-2 py-1.5">
            <div className="text-muted-foreground">{metric.label}</div>
            <div className={`mt-0.5 font-semibold ${metric.tone}`}>
              {metric.value}
            </div>
          </div>
        ))}
      </div>

      {progress.message ? (
        <div className="mt-3 max-h-10 overflow-hidden break-words text-xs text-muted-foreground">
          {progress.message}
        </div>
      ) : null}
    </div>
  );
}

function formatPlanLabel(planType?: string): string {
  const raw = (planType || "").trim();
  if (!raw) return "-";
  const lower = raw.toLowerCase();
  if (lower === "prolite" || lower === "pro_lite" || lower === "pro-lite")
    return "ProLite";
  return raw;
}

function ExpiryBadge({ expiresAt, planType }: { expiresAt?: string; planType?: string }) {
  const { t, i18n } = useTranslation();
  if (!expiresAt) return null;
  const plan = (planType || "").toLowerCase().trim();
  if (plan === "" || plan === "free" || plan === "api") return null;

  const timestamp = Date.parse(expiresAt);
  if (Number.isNaN(timestamp)) return null;

  const days = Math.floor((timestamp - Date.now()) / 86_400_000);
  const localDate = new Date(timestamp).toLocaleDateString(i18n.language);

  if (days < 0) {
    return (
      <span
        title={t("accounts.subscriptionExpiredTitle", { date: localDate })}
        className="inline-flex items-center rounded-md bg-zinc-200 px-1.5 py-0.5 text-[11px] font-medium text-zinc-700 ring-1 ring-inset ring-zinc-400/30 dark:bg-zinc-700/50 dark:text-zinc-300 dark:ring-zinc-500/30"
      >
        {t("accounts.subscriptionExpiredDays", { days: -days })}
      </span>
    );
  }
  if (days <= 3) {
    return (
      <span
        title={t("accounts.subscriptionExpiresTitle", { date: localDate })}
        className="inline-flex items-center rounded-md bg-red-100 px-1.5 py-0.5 text-[11px] font-semibold text-red-700 ring-1 ring-inset ring-red-500/30 dark:bg-red-500/20 dark:text-red-300 dark:ring-red-400/30"
      >
        {days === 0
          ? t("accounts.subscriptionExpiresToday")
          : t("accounts.subscriptionExpiresDays", { days })}
      </span>
    );
  }
  if (days <= 7) {
    return (
      <span
        title={t("accounts.subscriptionExpiresTitle", { date: localDate })}
        className="inline-flex items-center rounded-md bg-amber-100 px-1.5 py-0.5 text-[11px] font-medium text-amber-700 ring-1 ring-inset ring-amber-500/30 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/30"
      >
        {t("accounts.subscriptionExpiresDays", { days })}
      </span>
    );
  }
  return null;
}

function isWorkspacePlan(planType?: string): boolean {
  const normalized = normalizePlanType(planType);
  return (
    normalized === "team" ||
    normalized === "teamplus" ||
    normalized === "k12" ||
    normalized === "edu" ||
    normalized === "education"
  );
}

function accountWorkspaceId(account: Pick<AccountRow, "effective_workspace_id" | "token_workspace_id">): string {
  return (account.effective_workspace_id || account.token_workspace_id || "").trim();
}

function PlanBadge({
  planType,
  workspaceId,
}: {
  planType?: string;
  workspaceId?: string;
}) {
  const { t } = useTranslation();
  const label = formatPlanLabel(planType);
  if (label === "-")
    return <span className="text-[12px] text-muted-foreground">-</span>;

  const style: Record<string, string> = {
    pro: "bg-violet-100 text-violet-700 ring-violet-500/30 dark:bg-violet-500/20 dark:text-violet-300 dark:ring-violet-400/30",
    prolite:
      "bg-purple-50 text-purple-600 ring-purple-400/25 dark:bg-purple-500/15 dark:text-purple-300 dark:ring-purple-400/25",
    plus: "bg-blue-100 text-blue-700 ring-blue-500/30 dark:bg-blue-500/20 dark:text-blue-300 dark:ring-blue-400/30",
    team: "bg-amber-100 text-amber-700 ring-amber-500/30 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/30",
    k12: "bg-emerald-100 text-emerald-700 ring-emerald-500/30 dark:bg-emerald-500/20 dark:text-emerald-300 dark:ring-emerald-400/30",
    free: "bg-zinc-100 text-zinc-500 ring-zinc-400/20 dark:bg-zinc-500/10 dark:text-zinc-400 dark:ring-zinc-400/15",
  };

  const normalized = normalizePlanType(planType);
  const key =
    normalized === "pro" && label === "ProLite" ? "prolite" : normalized;
  const cls =
    style[key] ||
    "bg-slate-100 text-slate-600 ring-slate-400/20 dark:bg-slate-500/15 dark:text-slate-300 dark:ring-slate-400/20";
  const trimmedWorkspaceId = workspaceId?.trim() ?? "";
  const badge = (
    <span
      className={`inline-flex min-w-0 max-w-full items-center truncate rounded-md px-2.5 py-1 text-[13px] font-semibold ring-1 ring-inset ${cls}`}
    >
      {label}
    </span>
  );
  if (!isWorkspacePlan(planType) || !trimmedWorkspaceId) {
    return badge;
  }

  return (
    <TooltipProvider delayDuration={0} skipDelayDuration={0}>
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex min-w-0 max-w-full cursor-help">
            {badge}
          </span>
        </TooltipTrigger>
        <TooltipContent
          side="top"
          sideOffset={6}
          className="max-w-[360px] font-mono text-[11px]"
        >
          {t("accounts.planWorkspaceId", { id: trimmedWorkspaceId })}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}

function getDefaultScoreBias(planType?: string): number {
  switch (normalizePlanType(planType)) {
    case "pro":
    case "plus":
    case "team":
    case "k12":
      return 50;
    default:
      return 0;
  }
}

function getEffectiveScoreBias(account: AccountRow): number {
  if (typeof account.score_bias_effective === "number") {
    return account.score_bias_effective;
  }
  if (typeof account.score_bias_override === "number") {
    return account.score_bias_override;
  }
  return getDefaultScoreBias(account.plan_type);
}

function getEffectiveBaseConcurrency(account: AccountRow): number {
  if (
    typeof account.base_concurrency_effective === "number" &&
    account.base_concurrency_effective > 0
  ) {
    return account.base_concurrency_effective;
  }
  if (
    typeof account.base_concurrency_override === "number" &&
    account.base_concurrency_override > 0
  ) {
    return account.base_concurrency_override;
  }
  if (
    typeof account.dynamic_concurrency_limit === "number" &&
    account.dynamic_concurrency_limit > 0
  ) {
    return account.dynamic_concurrency_limit;
  }
  return 1;
}

function computePreviewDispatchScore(
  account: AccountRow,
  rawScore: number,
  appliedBias: number,
): number {
  if (
    (account.health_tier === "healthy" || account.health_tier === "warm") &&
    (account.status === "active" || account.status === "ready")
  ) {
    return rawScore + appliedBias;
  }
  return rawScore;
}

function getPreviewHealthTier(
  account: AccountRow,
  skipWarmTier: boolean,
): string | undefined {
  if (skipWarmTier && account.health_tier === "warm") return "healthy";
  return account.health_tier;
}

function computePreviewDynamicConcurrency(
  healthTier: string | undefined,
  account: AccountRow,
  baseConcurrency: number,
): number {
  switch (healthTier) {
    case "healthy":
      return baseConcurrency;
    case "warm":
      return Math.max(1, Math.floor(baseConcurrency / 2));
    case "risky":
      return 1;
    case "banned":
      return 0;
    default:
      return account.dynamic_concurrency_limit ?? baseConcurrency;
  }
}

function formatSignedNumber(value: number): string {
  if (value > 0) return `+${value}`;
  return String(value);
}

function SchedulerChip({
  label,
  value,
  tone,
}: {
  label: string;
  value: number;
  tone: "neutral" | "success" | "warning" | "danger";
}) {
  const toneStyle = {
    neutral: "bg-muted text-muted-foreground",
    success:
      "bg-emerald-500/10 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300",
    warning:
      "bg-amber-500/10 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300",
    danger: "bg-red-500/10 text-red-700 dark:bg-red-500/15 dark:text-red-300",
  }[tone];
  const dotStyle = {
    neutral: "bg-slate-400",
    success: "bg-emerald-500",
    warning: "bg-amber-500",
    danger: "bg-red-500",
  }[tone];

  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[12px] font-medium ${toneStyle}`}
    >
      <span className={`size-1.5 rounded-full ${dotStyle}`} />
      <span>{label}</span>
      <span className="tabular-nums">{value}</span>
    </span>
  );
}

function AccountRowActionsMenu({
  t,
  account,
  refreshing,
  authJsonExporting,
  includeTest = true,
  includeDelete = true,
  onTest,
  onRefresh,
  onGenerateAuthJson,
  onToggleEnabled,
  onToggleLock,
  onResetStatus,
  onResetCredits,
  onEditModels,
  onDelete,
}: {
  t: ReturnType<typeof useTranslation>["t"];
  account: AccountRow;
  refreshing: boolean;
  authJsonExporting: boolean;
  includeTest?: boolean;
  includeDelete?: boolean;
  onTest: () => void;
  onRefresh: () => void;
  onGenerateAuthJson: () => void;
  onToggleEnabled: () => void;
  onToggleLock: () => void;
  onResetStatus: () => void;
  onResetCredits: () => void;
  onEditModels?: () => void;
  onDelete: () => void;
}) {
  const refreshDisabled =
    refreshing || account.at_only || account.openai_responses_api;
  const authJsonDisabled =
    authJsonExporting ||
    account.at_only ||
    account.openai_responses_api ||
    account.grok_api ||
    account.agent_identity;
  const resetCredits = account.rate_limit_reset_credits ?? 0;

  const items: HeaderActionMenuItem[] = [
    ...(includeTest
      ? [
          {
            key: "test",
            label: t("accounts.testConnection"),
            icon: <Zap className="size-3.5" />,
            onSelect: onTest,
          },
        ]
      : []),
    {
      key: "refresh",
      label: t("accounts.refreshAccessToken"),
      icon: (
        <RefreshCw
          className={`size-3.5 ${refreshing ? "animate-spin" : ""}`}
        />
      ),
      disabled: refreshDisabled,
      title:
        account.at_only || account.openai_responses_api
          ? t("accounts.atRefreshDisabled")
          : undefined,
      onSelect: onRefresh,
    },
    {
      key: "auth-json",
      label: t("accounts.generateAuthJson"),
      icon: <FileJson className="size-3.5" />,
      disabled: authJsonDisabled,
      title:
        account.at_only ||
        account.openai_responses_api ||
        account.grok_api ||
        account.agent_identity
          ? t("accounts.authJsonDisabled")
          : undefined,
      onSelect: onGenerateAuthJson,
    },
    {
      key: "toggle-enabled",
      label:
        account.enabled === false
          ? t("accounts.actionEnableScheduling")
          : t("accounts.actionDisableScheduling"),
      icon:
        account.enabled === false ? (
          <Power className="size-3.5" />
        ) : (
          <PowerOff className="size-3.5" />
        ),
      onSelect: onToggleEnabled,
    },
    {
      key: "toggle-lock",
      label: account.locked
        ? t("accounts.actionUnlockAccount")
        : t("accounts.actionLockAccount"),
      icon: account.locked ? (
        <Unlock className="size-3.5" />
      ) : (
        <Lock className="size-3.5" />
      ),
      onSelect: onToggleLock,
    },
    {
      key: "reset-status",
      label:
        account.status === "overload_paused"
          ? t("accounts.overloadRecover")
          : t("accounts.resetStatus"),
      icon: <RotateCcw className="size-3.5" />,
      onSelect: onResetStatus,
    },
    {
      key: "reset-credits",
      label: t("accounts.resetCreditsButton"),
      icon: <Timer className="size-3.5" />,
      disabled: resetCredits <= 0,
      onSelect: onResetCredits,
    },
    // 支持模型白名单仅适用于 OAuth(ChatGPT)账号,relay/Grok 账号不显示。
    ...(onEditModels && !account.openai_responses_api && !account.grok_api
      ? [
          {
            key: "edit-models",
            label: t("accounts.supportedModelsAction"),
            icon: <SlidersHorizontal className="size-3.5" />,
            onSelect: onEditModels,
          },
        ]
      : []),
    ...(includeDelete
      ? [
          {
            key: "delete",
            label: t("accounts.deleteAccount"),
            icon: <Trash2 className="size-3.5" />,
            destructive: true,
            onSelect: onDelete,
          },
        ]
      : []),
  ];

  return (
    <HeaderActionMenu
      label={t("accounts.rowActions")}
      icon={<MoreHorizontal className="size-3.5" />}
      align="end"
      compact
      items={items}
    />
  );
}

function ChipList({
  items,
  tone,
}: {
  items: string[];
  tone: "purple" | "blue";
}) {
  if (items.length === 0) return null;
  const visible = items.slice(0, 3);
  const hidden = items.length - visible.length;
  // Keep tag chips intentionally muted so semantic status colors stay dominant.
  const toneClass =
    tone === "purple"
      ? "bg-muted text-muted-foreground ring-border/80"
      : "bg-muted/80 text-muted-foreground ring-border/70";

  return (
    <div className="mt-1.5 flex flex-wrap gap-1">
      {visible.map((item) => (
        <span
          key={item}
          className={`inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset ${toneClass}`}
        >
          {item}
        </span>
      ))}
      {hidden > 0 && (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
          +{hidden}
        </span>
      )}
    </div>
  );
}

function EmailDomainBadge({
  domain,
  t,
}: {
  domain: string;
  t: ReturnType<typeof useTranslation>["t"];
}) {
  const label = emailDomainTag(domain);
  if (!label) return null;

  return (
    <span
      className="inline-flex max-w-full items-center break-all rounded-md bg-muted px-1.5 py-0.5 text-left text-[10px] font-medium leading-tight text-muted-foreground ring-1 ring-inset ring-border/80"
      title={`${t("accounts.emailDomainSystemTag")}: ${label}`}
    >
      {label}
    </span>
  );
}

function normalizeGroupColor(color?: string): string {
  const value = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(value) ? value : ACCOUNT_GROUP_COLORS[0];
}

function resolveAccountGroups(
  ids: number[],
  groups: AccountGroup[],
): AccountGroup[] {
  if (ids.length === 0 || groups.length === 0) return [];
  const byID = new Map(groups.map((group) => [group.id, group]));
  return ids.map((id) => byID.get(id)).filter(Boolean) as AccountGroup[];
}

/** Sort key for clustering accounts that share the same group membership. */
function getAccountGroupSortMeta(
  account: { group_ids?: number[] | null },
  groups: AccountGroup[],
): { order: number; key: string } {
  const resolved = resolveAccountGroups(account.group_ids ?? [], groups);
  if (resolved.length === 0) {
    // Ungrouped accounts sort after every named group in ascending order.
    return { order: Number.MAX_SAFE_INTEGER, key: "" };
  }
  const sorted = [...resolved].sort((a, b) => {
    if (a.sort_order !== b.sort_order) return a.sort_order - b.sort_order;
    return a.name.localeCompare(b.name, "zh");
  });
  return {
    order: sorted[0].sort_order,
    key: sorted.map((group) => group.name).join("\0"),
  };
}

function GroupChipList({
  groups,
  onClick,
  emptyLabel,
}: {
  groups: AccountGroup[];
  onClick?: () => void;
  emptyLabel?: string;
}) {
  if (groups.length === 0 && !onClick) return null;
  const visible = groups.slice(0, 3);
  const hidden = groups.length - visible.length;

  const content = (
    <>
      {groups.length === 0 ? (
        <span className="inline-flex items-center gap-1 rounded-md border border-dashed border-border px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">
          <Plus className="size-2.5" />
          {emptyLabel}
        </span>
      ) : null}
      {visible.map((group) => {
        const color = normalizeGroupColor(group.color);
        return (
          <span
            key={group.id}
            className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold"
            style={{
              backgroundColor: `${color}14`,
              color,
              boxShadow: `inset 0 0 0 1px ${color}33`,
            }}
            title={group.description || group.name}
          >
            <span className="size-1.5 rounded-full bg-current" />
            {group.name}
          </span>
        );
      })}
      {hidden > 0 && (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">
          +{hidden}
        </span>
      )}
      {onClick && groups.length > 0 ? (
        <Pencil className="mt-0.5 size-3 text-muted-foreground opacity-60 transition-opacity group-hover:opacity-100" />
      ) : null}
    </>
  );

  if (onClick) {
    return (
      <button
        type="button"
        className="group mt-1.5 flex flex-wrap items-center gap-1 text-left"
        onClick={onClick}
        title={emptyLabel}
      >
        {content}
      </button>
    );
  }

  return <div className="mt-1.5 flex flex-wrap gap-1">{content}</div>;
}

function AccountMobileCard({
  account,
  sequence,
  selected,
  detailOpen = false,
  allGroups,
  proxyCtx,
  lazyMode,
  showEmailDomainTags,
  healthBuckets,
  refreshing,
  authJsonExporting,
  variant = "mobile",
  visibleColumns,
  t,
  onToggleSelect,
  onOpenDetail,
  onEdit,
  onEditGroups,
  onEditProxy,
  onUsage,
  onTest,
  onRefresh,
  onGenerateAuthJson,
  onToggleEnabled,
  onToggleLock,
  onResetStatus,
  onResetCredits,
  onEditModels,
  onDelete,
  onUsageRefreshed,
  onOpenOfficialUsage,
}: {
  account: AccountRow;
  sequence: number;
  selected: boolean;
  detailOpen?: boolean;
  allGroups: AccountGroup[];
  proxyCtx: ProxyBindingContext;
  lazyMode: boolean;
  showEmailDomainTags: boolean;
  healthBuckets: AccountHealthBucket[] | undefined;
  refreshing: boolean;
  authJsonExporting: boolean;
  variant?: "mobile" | "personal";
  visibleColumns?: Record<AccountTableColumn, boolean>;
  t: ReturnType<typeof useTranslation>["t"];
  onToggleSelect: () => void;
  onOpenDetail: () => void;
  onEdit: () => void;
  onEditGroups: () => void;
  onEditProxy: () => void;
  onUsage: () => void;
  onTest: () => void;
  onRefresh: () => void;
  onGenerateAuthJson: () => void;
  onToggleEnabled: () => void;
  onToggleLock: () => void;
  onResetStatus: () => void;
  onResetCredits: () => void;
  onEditModels?: () => void;
  onDelete: () => void;
  onUsageRefreshed?: () => void;
  // 成本列的官方胶囊点击后跳到用量弹窗的官方统计 tab。
  onOpenOfficialUsage?: () => void;
}) {
  const displayName = account.openai_responses_api
    ? formatAccountName(account)
    : formatAccountListEmail(account);
  const fullName = formatAccountName(account);
  const groups = resolveAccountGroups(account.group_ids ?? [], allGroups);
  // 自用模式用独立的信息架构：更强调账号身份、用量、健康和少量高频操作。
  const isPersonal = variant === "personal";
  const avatarInitial = (displayName.trim()[0] || "?").toUpperCase();
  const chatgptAccountId = account.chatgpt_account_id?.trim() ?? "";
  const resetCredits = account.rate_limit_reset_credits ?? 0;
  const hasStateBadges =
    account.at_only ||
    account.openai_responses_api ||
    account.grok_api ||
    account.locked;
  const modelCooldownCount = account.model_cooldowns?.length ?? 0;

  if (isPersonal) {
    return (
      <article
        className={`group relative flex h-full min-w-0 flex-col overflow-hidden rounded-xl border bg-card shadow-sm transition-colors ${
          detailOpen || selected
            ? "border-primary/40 bg-primary/5 ring-1 ring-primary/20"
            : "border-border hover:border-border/80"
        }${accountStateSurfaceClass(account)}`}
      >
        {renderAccountStateOverlay(account, t, {
          onRecover: onResetStatus,
        })}
        <div className="flex min-w-0 items-start gap-4 p-5 pb-4">
          <input
            type="checkbox"
            className="mt-2 size-4 shrink-0 cursor-pointer accent-primary"
            checked={selected}
            onChange={onToggleSelect}
            aria-label={fullName}
          />

          <div className="flex shrink-0 flex-col items-center gap-2">
            <button
              type="button"
              onClick={onOpenDetail}
              title={t("accounts.openDetail")}
              className="flex size-12 items-center justify-center rounded-lg bg-sky-50 text-lg font-semibold text-sky-700 ring-1 ring-inset ring-sky-200 transition-colors hover:bg-sky-100 dark:bg-sky-950/70 dark:text-sky-300 dark:ring-sky-800 dark:hover:bg-sky-900"
            >
              {avatarInitial}
            </button>
            {resetCredits > 0 && (
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onUsage();
                }}
                className="inline-flex items-center gap-1 rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 ring-1 ring-inset ring-amber-600/20 transition-colors hover:bg-amber-100 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20 dark:hover:bg-amber-900"
                title={t("accounts.resetCreditsBadge", { count: resetCredits })}
              >
                <RotateCcw className="size-2.5" />
                {resetCredits}
              </button>
            )}
            {getCreditBalanceDisplay(account) !== null && (
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onUsage();
                }}
                className="inline-flex items-center gap-1 rounded-md bg-teal-50 px-1.5 py-0.5 text-[10px] font-medium text-teal-700 ring-1 ring-inset ring-teal-600/20 transition-colors hover:bg-teal-100 dark:bg-teal-950 dark:text-teal-300 dark:ring-teal-400/20 dark:hover:bg-teal-900"
                title={
                  account.credits_unlimited
                    ? t("accounts.creditsBalanceUnlimited")
                    : t("accounts.creditsBalanceBadge", {
                        balance: getCreditBalanceDisplay(account),
                      })
                }
              >
                <Coins className="size-2.5" />
                {getCreditBalanceDisplay(account)}
              </button>
            )}
          </div>

          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 flex-wrap items-center gap-1.5">
              <span className="rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-mono font-semibold text-muted-foreground">
                #{sequence}
              </span>
              <PlanBadge
                planType={account.plan_type}
                workspaceId={accountWorkspaceId(account)}
              />
              <SchedulerPriorityBadge account={account} />
              <UsingCreditsBadge account={account} />
              {account.status !== "overload_paused" && (
                <AccountStatusCountdown account={account} />
              )}
              <ExpiryBadge
                expiresAt={account.subscription_expires_at}
                planType={account.plan_type}
              />
              {showEmailDomainTags && getAccountEmailDomain(account) && (
                <EmailDomainBadge domain={getAccountEmailDomain(account)} t={t} />
              )}
            </div>

            <div className="mt-2 flex min-w-0 flex-wrap items-start justify-between gap-3">
              <div className="min-w-0 flex-1">
                <button
                  type="button"
                  onClick={onOpenDetail}
                  title={fullName}
                  className="break-all text-left text-lg font-semibold leading-tight text-foreground transition-colors hover:text-primary"
                >
                  {displayName}
                </button>
                {chatgptAccountId && (
                  <div
                    className="mt-1 max-w-full truncate font-mono text-[10px] leading-tight text-muted-foreground/70"
                    title={chatgptAccountId}
                  >
                    {chatgptAccountId}
                  </div>
                )}
                <div className="mt-1 text-xs text-muted-foreground">
                  {t("accounts.healthSummary", {
                    health: formatHealthTier(account.health_tier, t),
                    score: Math.round(getDispatchScore(account)),
                    concurrency: account.dynamic_concurrency_limit ?? "-",
                  })}
                </div>
              </div>
              <div
                className="shrink-0"
                aria-hidden={Boolean(resolveAccountOverlayKind(account))}
              >
                <div className="flex flex-wrap items-center justify-end gap-1.5">
                  <StatusBadge
                    status={getAccountStatusBadgeStatus(account)}
                    detail={
                      account.status === "overload_paused"
                        ? undefined
                        : getAccountRateLimitWindow(account) ?? undefined
                    }
                    errorMessage={account.error_message}
                  />
                  <AccountConcurrencyBadge account={account} />
                </div>
              </div>
            </div>

            {hasStateBadges && (
              <div className="mt-3 flex min-h-6 min-w-0 flex-wrap items-center gap-1.5">
                {account.at_only && (
                  <span className="inline-flex items-center rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 ring-1 ring-inset ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20">
                    {formatAccessTokenBadge(account)}
                  </span>
                )}
                {account.openai_responses_api && (
                  <span className="inline-flex items-center rounded-md bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 ring-1 ring-inset ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20">
                    Responses API
                  </span>
                )}
                {account.grok_api && (
                  <span className="inline-flex items-center gap-0.5 rounded-md bg-zinc-900 px-1.5 py-0.5 text-[10px] font-medium text-white ring-1 ring-inset ring-zinc-700 dark:bg-white dark:text-zinc-900 dark:ring-zinc-300">
                    <Sparkles className="size-2.5" />
                    Grok
                    {account.grok_auth_kind === "api_key" ? " · API Key" : " · OAuth"}
                  </span>
                )}
                {account.locked && (
                  <span className="inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                    <Lock className="mr-0.5 size-2.5" />
                    {t("accounts.lock")}
                  </span>
                )}
              </div>
            )}
          </div>
        </div>

        {(account.status === "error" && account.error_message) ||
        modelCooldownCount > 0 ? (
          <div className="mx-5 space-y-2 border-t border-border/70 pt-3">
            {account.status === "error" && account.error_message && (
              <div
                className="flex min-w-0 items-start gap-2 rounded-md bg-red-50 px-3 py-2 text-xs leading-snug text-red-700 ring-1 ring-inset ring-red-500/20 dark:bg-red-950/40 dark:text-red-300 dark:ring-red-400/20"
                title={account.error_message}
              >
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                <span className="line-clamp-3 break-words">
                  {account.error_message}
                </span>
              </div>
            )}
            {modelCooldownCount > 0 && (
              <div className="rounded-md bg-amber-50 px-3 py-2 text-xs leading-snug text-amber-700 ring-1 ring-inset ring-amber-500/20 dark:bg-amber-950/40 dark:text-amber-300 dark:ring-amber-400/20">
                model {account.model_cooldowns?.[0]?.model}
                {modelCooldownCount > 1 ? ` +${modelCooldownCount - 1}` : ""}
              </div>
            )}
          </div>
        ) : null}

        <div className="grid min-w-0 gap-4 px-5 py-4 xl:grid-cols-[minmax(0,1.25fr)_minmax(220px,0.75fr)]">
          <div className="min-w-0 space-y-3">
            <div className="border-t border-border/70 pt-3">
              <div className="mb-3 flex items-center justify-between gap-2">
                <div className="flex min-w-0 items-center gap-2 text-xs font-semibold text-muted-foreground">
                  <BarChart3 className="size-3.5 shrink-0 text-sky-600 dark:text-sky-400" />
                  <span className="truncate">{t("accounts.usage")}</span>
                </div>
                <button
                  type="button"
                  onClick={onUsage}
                  className="inline-flex shrink-0 items-center gap-1 rounded-md px-2 py-1 text-[11px] font-medium text-primary transition-colors hover:bg-primary/10"
                >
                  <ExternalLink className="size-3" />
                  {t("accounts.actionUsageDetail")}
                </button>
              </div>
              <UsageCell account={account} wide onRefreshed={onUsageRefreshed} />
            </div>

            <div className="grid min-w-0 gap-2 sm:grid-cols-2">
              <AccountPersonalMetric
                label={t("accounts.requests")}
                icon={<Zap className="size-3.5" />}
                tone="emerald"
              >
                <RequestCountPills account={account} />
              </AccountPersonalMetric>
              <AccountPersonalMetric
                label={t("accounts.billed")}
                icon={<Coins className="size-3.5" />}
                tone="amber"
              >
                <BilledCell
                  account={account}
                  onOpenOfficial={onOpenOfficialUsage ? () => onOpenOfficialUsage() : undefined}
                />
              </AccountPersonalMetric>
            </div>
          </div>

          <div className="min-w-0 space-y-3">
            <div className="border-t border-border/70 pt-3">
              <div className="mb-2 flex items-center justify-between gap-2 text-xs font-semibold text-muted-foreground">
                <span>{t("accounts.healthBarLabel")}</span>
                <span className="shrink-0">
                  {formatHealthTier(account.health_tier, t)}
                </span>
              </div>
              <AccountHealthBar buckets={healthBuckets} />
            </div>

            <div className="grid min-w-0 gap-2 sm:grid-cols-2 xl:grid-cols-1">
              <AccountPersonalMetric
                label={t("accounts.updatedAt")}
                icon={<RefreshCw className="size-3.5" />}
                tone="sky"
              >
                {lazyMode ? (
                  <div className="space-y-0.5">
                    <div>
                      <span className="mr-1 text-muted-foreground/70">
                        {t("accounts.recordUpdatedAtShort")}
                      </span>
                      {formatRelativeTime(account.updated_at)}
                    </div>
                    <div>
                      <span className="mr-1 text-muted-foreground/70">
                        {t("accounts.usageUpdatedAtShort")}
                      </span>
                      {account.codex_usage_updated_at
                        ? formatRelativeTime(account.codex_usage_updated_at)
                        : t("accounts.noUsageUpdatedAt")}
                    </div>
                  </div>
                ) : (
                  formatRelativeTime(account.updated_at)
                )}
              </AccountPersonalMetric>
              <AccountPersonalMetric
                label={t("accounts.importTime")}
                icon={<FolderOpen className="size-3.5" />}
                tone="zinc"
              >
                {formatBeijingTime(account.created_at)}
              </AccountPersonalMetric>
            </div>
          </div>
        </div>

        <div className="mx-5 space-y-1.5 border-t border-border/70 py-3">
          <ChipList items={account.tags ?? []} tone="purple" />
          <GroupChipList
            groups={groups}
            onClick={onEditGroups}
            emptyLabel={t("accounts.groupQuickEdit")}
          />
          <div className="mt-1.5 flex">
            <AccountProxyBadge
              account={account}
              ctx={proxyCtx}
              onClick={onEditProxy}
            />
          </div>
        </div>

        <div className="mt-auto border-t border-border/70 bg-muted/15 p-4">
          <div className="flex flex-wrap items-center gap-2">
            <AccountMobileActionButton
              title={t("accounts.openDetail")}
              label={t("accounts.openDetail")}
              onClick={onOpenDetail}
              icon={<Eye className="size-3.5" />}
            />
            <AccountMobileActionButton
              title={t("accounts.editScheduler")}
              label={t("accounts.editScheduler")}
              onClick={onEdit}
              icon={<Pencil className="size-3.5" />}
            />
            <AccountMobileActionButton
              title={t("accounts.usageDetail")}
              label={t("accounts.actionUsageDetail")}
              onClick={onUsage}
              icon={<BarChart3 className="size-3.5" />}
            />
            <AccountRowActionsMenu
              t={t}
              account={account}
              refreshing={refreshing}
              authJsonExporting={authJsonExporting}
              onTest={onTest}
              onRefresh={onRefresh}
              onGenerateAuthJson={onGenerateAuthJson}
              onToggleEnabled={onToggleEnabled}
              onToggleLock={onToggleLock}
              onResetStatus={onResetStatus}
              onResetCredits={onResetCredits}
              onEditModels={onEditModels}
              onDelete={onDelete}
            />
          </div>
        </div>
      </article>
    );
  }

  return (
    <article
      className={`relative min-w-0 rounded-xl border bg-card p-3 shadow-sm transition-colors ${
        detailOpen || selected
          ? "border-primary/40 bg-primary/5 ring-1 ring-primary/20"
          : "border-border"
      }${accountStateSurfaceClass(account, " overflow-hidden")}`}
    >
      {renderAccountStateOverlay(account, t, {
        compact: true,
        onRecover: onResetStatus,
      })}
      <div className="flex min-w-0 items-start gap-3">
        <input
          type="checkbox"
          className="mt-1 size-4 shrink-0 cursor-pointer accent-primary"
          checked={selected}
          onChange={onToggleSelect}
          aria-label={fullName}
        />
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-start justify-between gap-2">
            <div className="min-w-0 flex-1">
              <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                  {(!visibleColumns || visibleColumns.sequence) && (
                    <span className="rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-mono font-semibold text-muted-foreground">
                      #{sequence}
                    </span>
                  )}
                  {(!visibleColumns || visibleColumns.plan) && (
                    <PlanBadge
                      planType={account.plan_type}
                      workspaceId={accountWorkspaceId(account)}
                    />
                  )}
                  {(!visibleColumns || visibleColumns.priority) && (
                    <SchedulerPriorityBadge account={account} />
                  )}
                  <ExpiryBadge
                    expiresAt={account.subscription_expires_at}
                    planType={account.plan_type}
                  />
                  {showEmailDomainTags && getAccountEmailDomain(account) && (
                    <EmailDomainBadge
                      domain={getAccountEmailDomain(account)}
                      t={t}
                    />
                  )}
                </div>
                <button
                  type="button"
                  className="mt-1 break-all text-left text-[15px] font-semibold leading-tight text-foreground transition-colors hover:text-primary"
                  title={t("accounts.openDetail")}
                  onClick={onOpenDetail}
                >
                  {displayName}
                </button>
                {chatgptAccountId && (
                  <div
                    className="mt-1 min-h-[14px] max-w-full truncate font-mono text-[10px] leading-tight text-muted-foreground/70"
                    title={chatgptAccountId}
                  >
                    {chatgptAccountId}
                  </div>
                )}
              </div>
              {(!visibleColumns || visibleColumns.status) && (
                <div
                  className="flex min-w-[112px] shrink-0 flex-col items-end"
                  aria-hidden={Boolean(resolveAccountOverlayKind(account))}
                >
                  <StatusBadge
                    status={getAccountStatusBadgeStatus(account)}
                    detail={
                      account.status === "overload_paused"
                        ? undefined
                        : getAccountRateLimitWindow(account) ?? undefined
                    }
                    errorMessage={account.error_message}
                  />
                  <div className="mt-1 flex min-h-6 flex-wrap items-center justify-end gap-1.5">
                    <UsingCreditsBadge account={account} />
                    {account.status !== "overload_paused" && (
                      <AccountStatusCountdown account={account} />
                    )}
                  </div>
                </div>
              )}
            </div>

          <div className="mt-2 flex min-h-6 min-w-0 flex-wrap items-center gap-1.5">
            {account.at_only && (
              <span className="inline-flex items-center rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 ring-1 ring-inset ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20">
                {formatAccessTokenBadge(account)}
              </span>
            )}
            {account.openai_responses_api && (
              <span className="inline-flex items-center rounded-md bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 ring-1 ring-inset ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20">
                Responses API
              </span>
            )}
            {account.grok_api && (
              <span className="inline-flex items-center gap-0.5 rounded-md bg-zinc-900 px-1.5 py-0.5 text-[10px] font-medium text-white ring-1 ring-inset ring-zinc-700 dark:bg-white dark:text-zinc-900 dark:ring-zinc-300">
                <Sparkles className="size-2.5" />
                Grok
                {account.grok_auth_kind === "api_key" ? " · API Key" : " · OAuth"}
              </span>
            )}
            {account.locked && (
              <span className="inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                <Lock className="mr-0.5 size-2.5" />
                {t("accounts.lock")}
              </span>
            )}
          </div>

          {account.status === "error" && account.error_message && (
            <div
              className="mt-2 line-clamp-3 break-words text-[11px] leading-tight text-red-500"
              title={account.error_message}
            >
              {account.error_message}
            </div>
          )}
          {(account.model_cooldowns?.length ?? 0) > 0 && (
            <div className="mt-2 text-[11px] leading-tight text-amber-600">
              model {account.model_cooldowns?.[0]?.model}
              {(account.model_cooldowns?.length ?? 0) > 1
                ? ` +${(account.model_cooldowns?.length ?? 1) - 1}`
                : ""}
            </div>
          )}
          {(!visibleColumns || visibleColumns.usage) && (
            <div
              className="mt-1.5"
              title={t("accounts.healthSummary", {
                health: formatHealthTier(account.health_tier, t),
                score: Math.round(getDispatchScore(account)),
                concurrency: account.dynamic_concurrency_limit ?? "-",
              })}
            >
              <AccountHealthBar buckets={healthBuckets} />
            </div>
          )}
          {Math.max(account.active_requests ?? 0, account.occupied_requests ?? 0) > 0 && (
            <div className="mt-1">
              <AccountConcurrencyBadge account={account} />
            </div>
          )}
        </div>
      </div>

      <div
        className="mt-3 grid min-w-0 grid-cols-2 gap-2 max-[380px]:grid-cols-1"
      >
        {(!visibleColumns || visibleColumns.requests) && (
          <AccountMobileMetric label={t("accounts.requests")} className="min-h-[84px]">
            <RequestCountPills account={account} compact />
          </AccountMobileMetric>
        )}
        {(!visibleColumns || visibleColumns.billed) && (
          <AccountMobileMetric label={t("accounts.billed")} className="min-h-[84px]">
            <BilledCell
              account={account}
              onOpenOfficial={onOpenOfficialUsage ? () => onOpenOfficialUsage() : undefined}
            />
          </AccountMobileMetric>
        )}
        {(!visibleColumns || visibleColumns.updatedAt) && (
          <AccountMobileMetric label={t("accounts.updatedAt")} className="min-h-[84px]">
            {lazyMode ? (
              <div className="space-y-0.5">
                <div>
                  <span className="mr-1 text-muted-foreground/70">
                    {t("accounts.recordUpdatedAtShort")}
                  </span>
                  {formatRelativeTime(account.updated_at)}
                </div>
                <div>
                  <span className="mr-1 text-muted-foreground/70">
                    {t("accounts.usageUpdatedAtShort")}
                  </span>
                  {account.codex_usage_updated_at
                    ? formatRelativeTime(account.codex_usage_updated_at)
                    : t("accounts.noUsageUpdatedAt")}
                </div>
              </div>
            ) : (
              formatRelativeTime(account.updated_at)
            )}
          </AccountMobileMetric>
        )}
        {(!visibleColumns || visibleColumns.importTime) && (
          <AccountMobileMetric label={t("accounts.importTime")} className="min-h-[84px]">
            {formatBeijingTime(account.created_at)}
          </AccountMobileMetric>
        )}
        {(!visibleColumns || visibleColumns.usage) && (
          <AccountMobileMetric
            label={t("accounts.usage")}
            className="col-span-2 min-h-[116px] max-[380px]:col-span-1"
          >
            <UsageCell account={account} onRefreshed={onUsageRefreshed} />
          </AccountMobileMetric>
        )}
      </div>

      <div className="mt-3 space-y-1.5 border-t border-border pt-2">
        {(!visibleColumns || visibleColumns.tags) && (
          <ChipList items={account.tags ?? []} tone="purple" />
        )}
        {!isPersonal && showEmailDomainTags && getAccountEmailDomain(account) && (
          <div className="mt-1.5 flex flex-wrap gap-1">
            <EmailDomainBadge domain={getAccountEmailDomain(account)} t={t} />
          </div>
        )}
        {(!visibleColumns || visibleColumns.groups) && (
          <GroupChipList
            groups={groups}
            onClick={onEditGroups}
            emptyLabel={t("accounts.groupQuickEdit")}
          />
        )}
        {(!visibleColumns || visibleColumns.proxy) && (
          <div className="mt-1.5 flex">
            <AccountProxyBadge
              account={account}
              ctx={proxyCtx}
              onClick={onEditProxy}
            />
          </div>
        )}
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-1.5">
        <AccountMobileActionButton
          title={t("accounts.openDetail")}
          onClick={onOpenDetail}
          icon={<Eye className="size-3.5" />}
        />
        <AccountMobileActionButton
          title={t("accounts.editScheduler")}
          onClick={onEdit}
          icon={<Pencil className="size-3.5" />}
        />
        <AccountMobileActionButton
          title={t("accounts.usageDetail")}
          onClick={onUsage}
          icon={<BarChart3 className="size-3.5" />}
        />
        <AccountRowActionsMenu
          t={t}
          account={account}
          refreshing={refreshing}
          authJsonExporting={authJsonExporting}
          onTest={onTest}
          onRefresh={onRefresh}
          onGenerateAuthJson={onGenerateAuthJson}
          onToggleEnabled={onToggleEnabled}
          onToggleLock={onToggleLock}
          onResetStatus={onResetStatus}
          onResetCredits={onResetCredits}
          onEditModels={onEditModels}
          onDelete={onDelete}
        />
      </div>
    </article>
  );
}

function AccountMobileMetric({
  label,
  children,
  className = "",
  premium = false,
}: {
  label: string;
  children: ReactNode;
  className?: string;
  premium?: boolean;
}) {
  return (
    <div
      className={`min-w-0 ${
        premium
          ? "rounded-xl bg-muted/40 p-3 ring-1 ring-inset ring-border/40"
          : "rounded-lg border border-border bg-muted/20 p-2"
      } ${className}`}
    >
      <div
        className={`mb-1 font-bold uppercase text-muted-foreground ${
          premium ? "text-[10px] tracking-wider" : "text-[11px]"
        }`}
      >
        {label}
      </div>
      <div className="min-w-0 break-words text-[12px] leading-snug text-foreground">
        {children}
      </div>
    </div>
  );
}

type AccountPersonalMetricTone = "emerald" | "amber" | "sky" | "zinc";

function AccountPersonalMetric({
  label,
  icon,
  children,
  tone = "zinc",
}: {
  label: string;
  icon: ReactNode;
  children: ReactNode;
  tone?: AccountPersonalMetricTone;
}) {
  const borderClass: Record<AccountPersonalMetricTone, string> = {
    emerald: "border-emerald-500/70",
    amber: "border-amber-500/70",
    sky: "border-sky-500/70",
    zinc: "border-zinc-400/70",
  };
  const iconClass: Record<AccountPersonalMetricTone, string> = {
    emerald:
      "text-emerald-600 ring-emerald-600/15 dark:text-emerald-400 dark:ring-emerald-400/15",
    amber:
      "text-amber-600 ring-amber-600/15 dark:text-amber-400 dark:ring-amber-400/15",
    sky: "text-sky-600 ring-sky-600/15 dark:text-sky-400 dark:ring-sky-400/15",
    zinc: "text-zinc-500 ring-zinc-500/15 dark:text-zinc-400 dark:ring-zinc-400/15",
  };

  return (
    <div className={`min-w-0 border-l-2 pl-3 ${borderClass[tone]}`}>
      <div className="mb-1 flex min-w-0 items-center gap-2">
        <span
          className={`inline-flex size-5 shrink-0 items-center justify-center rounded-md bg-background/80 ring-1 ring-inset ${iconClass[tone]}`}
        >
          {icon}
        </span>
        <span className="min-w-0 truncate text-[11px] font-semibold text-muted-foreground">
          {label}
        </span>
      </div>
      <div className="min-w-0 break-words text-[12px] leading-snug text-foreground">
        {children}
      </div>
    </div>
  );
}

function AccountMobileActionButton({
  title,
  icon,
  label,
  onClick,
  disabled,
  variant = "outline",
}: {
  title: string;
  icon: ReactNode;
  label?: string;
  onClick: () => void;
  disabled?: boolean;
  variant?: "default" | "outline" | "destructive";
}) {
  // 带 label 时图标在上、文字在下，按钮等高等宽（自用模式用）；否则纯图标。
  if (label) {
    return (
      <Button
        type="button"
        variant={variant}
        className="flex h-auto w-full flex-col items-center justify-center gap-1 px-1 py-2 text-[11px] font-medium leading-none"
        disabled={disabled}
        onClick={onClick}
        title={title}
        aria-label={title}
      >
        {icon}
        <span className="max-w-full truncate">{label}</span>
      </Button>
    );
  }
  return (
    <Button
      type="button"
      variant={variant}
      size="icon-sm"
      className="h-9 w-full"
      disabled={disabled}
      onClick={onClick}
      title={title}
      aria-label={title}
    >
      {icon}
    </Button>
  );
}

function formatHealthTier(healthTier?: string, t?: any) {
  if (!t) return "Unknown";
  switch (healthTier) {
    case "healthy":
      return t("accounts.healthy");
    case "warm":
      return t("accounts.warm");
    case "risky":
      return t("accounts.risky");
    case "banned":
      return t("accounts.quarantine");
    default:
      return t("accounts.unknown");
  }
}

// ==================== 测试连接弹窗 ====================

interface TestEvent {
  type: "test_start" | "content" | "diagnostics" | "test_complete" | "error";
  text?: string;
  model?: string;
  success?: boolean;
  error?: string;
  // Codex/Responses 测连诊断:首帧只有响应头信息,流结束后的最终帧带 duration_ms,
  // 且可能出现在 test_complete/error 之后,因此要读到 SSE 关闭再刷新列表。
  codex_diagnostics?: CodexTestDiagnostics;
}

function formatTestErrorMessage(message: string) {
  const normalized = message.trim();
  const jsonStart = normalized.indexOf("{");

  if (jsonStart === -1) {
    return normalized;
  }

  const prefix = normalized
    .slice(0, jsonStart)
    .trim()
    .replace(/[：:]\s*$/, "");
  const jsonText = normalized.slice(jsonStart);

  try {
    const parsed = JSON.parse(jsonText);
    const prettyJson = JSON.stringify(parsed, null, 2);
    return prefix ? `${prefix}\n${prettyJson}` : prettyJson;
  } catch {
    return normalized;
  }
}

function formatTestOutput(text: string) {
  try {
    const parsed = JSON.parse(text);
    return JSON.stringify(parsed, null, 2);
  } catch {
    return text;
  }
}

const DEFAULT_TEST_MODEL = "gpt-5.4";

function isConnectionTestModel(model: string) {
  const value = model.trim().toLowerCase();
  return value !== "" && !value.includes("image");
}

function extractTextModels(
  modelsResp: Awaited<ReturnType<typeof api.getModels>>,
) {
  if (modelsResp.items && modelsResp.items.length > 0) {
    return modelsResp.items
      .filter(
        (item) =>
          item.enabled &&
          item.category !== "image" &&
          !item.id.includes("image"),
      )
      .map((item) => item.id);
  }
  return (modelsResp.models ?? []).filter(isConnectionTestModel);
}

function uniqueTestModels(
  models: string[],
  preferredModel?: string,
  includeDefault = true,
) {
  const seen = new Set<string>();
  const result: string[] = [];
  const candidates = [
    preferredModel ?? "",
    ...models,
    ...(includeDefault ? [DEFAULT_TEST_MODEL] : []),
  ];

  for (const model of candidates) {
    const value = model.trim();
    if (!isConnectionTestModel(value) || seen.has(value)) continue;
    seen.add(value);
    result.push(value);
  }
  return result;
}

function TestConnectionModal({
  account,
  onClose,
  onSettled,
  successHint,
  restoreOnSuccess,
}: {
  account: AccountRow;
  onClose: () => void;
  onSettled: () => void;
  successHint?: string;
  restoreOnSuccess?: boolean;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [output, setOutput] = useState<string[]>([]);
  const [status, setStatus] = useState<
    "connecting" | "streaming" | "success" | "error"
  >("connecting");
  const [errorMsg, setErrorMsg] = useState("");
  const [model, setModel] = useState("");
  const [selectedModel, setSelectedModel] = useState("");
  const [modelOptions, setModelOptions] = useState<string[]>([]);
  const [modelOptionsReady, setModelOptionsReady] = useState(false);
  const [diagnostics, setDiagnostics] = useState<CodexTestDiagnostics | null>(null);
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [headersOpen, setHeadersOpen] = useState(false);
  const [rawOpen, setRawOpen] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const abortRef = useRef<AbortController | null>(null);
  const outputEndRef = useRef<HTMLDivElement>(null);
  const settledRef = useRef(false);
  const onSettledRef = useRef(onSettled);
  onSettledRef.current = onSettled;

  const markSettled = useCallback(() => {
    if (settledRef.current) return;
    settledRef.current = true;
    onSettledRef.current();
  }, []);

  const isClaudeAccount = Boolean(account.claude_api);
  // Grok 与 openai_responses 同属"账号自带模型清单"的 relay 风格账号，
  // Claude 也使用账号级原生 Messages 模型清单，但走独立分支。
  const isOpenAIResponsesAccount = Boolean(
    account.openai_responses_api || account.grok_api,
  );

  const modelSelectOptions = useMemo(
    () =>
      uniqueTestModels(
        modelOptions,
        selectedModel,
        !isOpenAIResponsesAccount && !isClaudeAccount,
      ).map((item) => ({ label: item, value: item })),
    [isClaudeAccount, isOpenAIResponsesAccount, modelOptions, selectedModel],
  );

  useEffect(() => {
    let active = true;

    const loadModels = async () => {
      try {
        const settings = await api.getSettings();
        if (!active) return;

        if (isClaudeAccount) {
          const accountModels = (account.models ?? []).filter(
            (model) => isConnectionTestModel(model) && model.toLowerCase().startsWith("claude-"),
          );
          const fallbackModels = uniqueTestModels(
            accountModels.length > 0 ? accountModels : ["claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"],
            undefined,
            false,
          );
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0] || "");
          return;
        }

        if (isOpenAIResponsesAccount) {
          const accountModels = (account.models ?? []).filter(
            isConnectionTestModel,
          );
          const mappingAliases = exactModelMappingAliases(
            account.model_mapping,
            accountModels,
          );
          const testModels = uniqueTestModels(
            [...mappingAliases, ...accountModels],
            undefined,
            false,
          );
          const preferredModel =
            testModels.find(
              (item) =>
                item.toLowerCase() === settings.test_model.toLowerCase(),
            ) ??
            mappingAliases[0] ??
            accountModels[0];
          const nextModels = uniqueTestModels(
            testModels,
            preferredModel,
            false,
          );
          setModelOptions(nextModels);
          setSelectedModel((current) => current || nextModels[0] || "");
          return;
        }

        const modelsResp = await api.getModels();
        if (!active) return;
        const upstreamModels = extractTextModels(modelsResp);
        const preferredModel = isConnectionTestModel(settings.test_model)
          ? settings.test_model
          : DEFAULT_TEST_MODEL;
        const nextModels = uniqueTestModels(upstreamModels, preferredModel);
        setModelOptions(nextModels);
        setSelectedModel(
          (current) => current || nextModels[0] || DEFAULT_TEST_MODEL,
        );
      } catch {
        if (!active) return;
        if (isClaudeAccount) {
          const accountModels = (account.models ?? []).filter(
            (model) => isConnectionTestModel(model) && model.toLowerCase().startsWith("claude-"),
          );
          const fallbackModels = uniqueTestModels(
            accountModels.length > 0 ? accountModels : ["claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"],
            undefined,
            false,
          );
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0] || "");
        } else if (isOpenAIResponsesAccount) {
          const accountModels = (account.models ?? []).filter(
            isConnectionTestModel,
          );
          const mappingAliases = exactModelMappingAliases(
            account.model_mapping,
            accountModels,
          );
          const fallbackModels = uniqueTestModels(
            [...mappingAliases, ...accountModels],
            undefined,
            false,
          );
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0] || "");
        } else {
          const fallbackModels = uniqueTestModels([], DEFAULT_TEST_MODEL);
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0]);
        }
      } finally {
        if (active) {
          setModelOptionsReady(true);
        }
      }
    };

    void loadModels();

    return () => {
      active = false;
    };
  }, [account.claude_api, account.model_mapping, account.models, isClaudeAccount, isOpenAIResponsesAccount]);

  useEffect(() => {
    if (!modelOptionsReady || !selectedModel) return;

    // 重置状态（StrictMode 二次 mount 时清理上一次的残留）
    setOutput([]);
    setStatus("connecting");
    setErrorMsg("");
    setDiagnostics(null);
    setModel(selectedModel);
    settledRef.current = false;

    const controller = new AbortController();
    abortRef.current = controller;

    const run = async () => {
      if (controller.signal.aborted) return;

      try {
        const params = new URLSearchParams({ model: selectedModel });
        if (restoreOnSuccess) {
          params.set("restore_on_success", "true");
        }
        const res = await fetch(
          `/api/admin/accounts/${account.id}/test?${params.toString()}`,
          {
            signal: controller.signal,
            headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
          },
        );

        if (!res.ok) {
          const body = await res.text();
          let msg = `HTTP ${res.status}`;
          try {
            const parsed = JSON.parse(body);
            if (parsed.error) msg = parsed.error;
          } catch {
            /* ignore */
          }
          setStatus("error");
          setErrorMsg(msg);
          markSettled();
          return;
        }

        const reader = res.body?.getReader();
        if (!reader) {
          setStatus("error");
          setErrorMsg(t("accounts.browserStreamingUnsupported"));
          markSettled();
          return;
        }

        const decoder = new TextDecoder();
        let buffer = "";
        let receivedTerminalEvent = false;

        const processEventLines = (lines: string[]) => {
          for (const line of lines) {
            const trimmed = line.trim();
            if (!trimmed.startsWith("data: ")) continue;

            try {
              const event: TestEvent = JSON.parse(trimmed.slice(6));
              // 请求阶段失败时后端不单发 diagnostics 帧,而是把诊断挂在 error 事件上,
              // 因此不分事件类型,带了就收。
              if (event.codex_diagnostics) {
                setDiagnostics(event.codex_diagnostics);
              }

              switch (event.type) {
                case "test_start":
                  setModel(event.model || selectedModel);
                  setStatus("streaming");
                  break;
                case "content":
                  if (event.text) {
                    setOutput((prev) => [...prev, event.text!]);
                  }
                  break;
                case "diagnostics":
                  break;
                case "test_complete":
                  receivedTerminalEvent = true;
                  setStatus(event.success ? "success" : "error");
                  break;
                case "error":
                  receivedTerminalEvent = true;
                  setStatus("error");
                  setErrorMsg(event.error || t("accounts.unknownError"));
                  break;
              }
            } catch {
              /* ignore non-JSON lines */
            }
          }
        };

        while (true) {
          const { done, value } = await reader.read();
          if (done) {
            buffer += decoder.decode();
            break;
          }

          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split("\n");
          buffer = lines.pop() || "";
          processEventLines(lines);
        }

        if (buffer.trim()) {
          processEventLines([buffer]);
        }

        if (receivedTerminalEvent) {
          // 等服务端关闭 SSE 后再刷新列表：后端会在连接结束时提交状态并失效
          // 账号快照，提前刷新会重新读到“未采样”的旧缓存。
          markSettled();
        } else {
          setStatus("error");
          setErrorMsg(t("accounts.connectionEndedUnexpectedly"));
          markSettled();
        }
      } catch (err: unknown) {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setStatus("error");
        setErrorMsg(
          err instanceof Error ? err.message : t("accounts.connectionFailed"),
        );
        markSettled();
      }
    };

    // 延迟 50ms 启动，确保 StrictMode cleanup 有足够时间执行 abort
    const timer = window.setTimeout(() => {
      void run();
    }, 50);

    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [
    account.id,
    attempt,
    markSettled,
    modelOptionsReady,
    restoreOnSuccess,
    selectedModel,
    t,
  ]);

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [output]);

  const statusText = {
    connecting: t("accounts.connecting"),
    streaming: t("accounts.receivingResponse"),
    success: t("accounts.testSuccess"),
    error: t("accounts.testFailed"),
  }[status];
  const StatusIcon = {
    connecting: Loader2,
    streaming: Loader2,
    success: CheckCircle,
    error: XCircle,
  }[status];
  const statusIconSpin = status === "connecting" || status === "streaming";

  const statusColor = {
    connecting: "text-muted-foreground",
    streaming: "text-blue-500",
    success: "text-emerald-500",
    error: "text-red-500",
  }[status];
  const formattedErrorMsg = errorMsg ? formatTestErrorMessage(errorMsg) : "";
  const handleCopyFailureDetails = async () => {
    try {
      await copyTextToClipboard(formattedErrorMsg);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };
  const running = status === "connecting" || status === "streaming";
  const diagnosticsFinal = isFinalCodexTestDiagnostics(diagnostics);
  const handleCopyDiagnostics = async () => {
    try {
      await copyTextToClipboard(
        JSON.stringify(
          {
            status,
            model,
            error: errorMsg || undefined,
            output: output.join(""),
            diagnostics,
          },
          null,
          2,
        ),
      );
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };
  const timingTiles = [
    {
      key: "http",
      label: t("accounts.testDiagHTTPStatus"),
      value: diagnostics?.http_status ? String(diagnostics.http_status) : "—",
      tone:
        !diagnostics?.http_status
          ? "text-foreground"
          : diagnostics.http_status < 300
            ? "text-emerald-600 dark:text-emerald-400"
            : diagnostics.http_status === 429
              ? "text-amber-600 dark:text-amber-400"
              : "text-red-600 dark:text-red-400",
    },
    { key: "headers", label: t("accounts.testDiagHeadersTime"), value: formatCodexTestMS(diagnostics?.headers_ms), tone: "text-foreground" },
    { key: "first", label: t("accounts.testDiagFirstContent"), value: formatCodexTestMS(diagnostics?.first_content_ms), tone: "text-foreground" },
    { key: "total", label: t("accounts.testDiagDuration"), value: formatCodexTestMS(diagnostics?.duration_ms), tone: "text-foreground" },
  ];
  const windowLabel = (window?: CodexTestWindow) => {
    const kind = codexTestWindowKind(window);
    return kind === "5h"
      ? t("accounts.testDiagWindow5h")
      : kind === "7d"
        ? t("accounts.testDiagWindow7d")
        : kind === "short"
          ? t("accounts.testDiagWindowShort")
          : t("accounts.testDiagWindowUnknown");
  };
  const usageWindows = [diagnostics?.primary_window, diagnostics?.secondary_window]
    .filter((window): window is CodexTestWindow => Boolean(window))
    .map((window) => ({
      label: windowLabel(window),
      percent: clampCodexTestPercent(window.used_percent),
      reset: formatCodexTestReset(window.reset_after_seconds),
    }));
  // 安全缓冲单独成行:x-codex-safety-buffering-faster-model 只是官方 CLI 的备用切换
  // 目标,混在响应头表里容易被误读成"实际回答模型"。
  const safetyBuffering = (() => {
    if (!diagnostics) return undefined;
    const enabled = diagnostics.safety_buffering_enabled;
    const faster = diagnostics.safety_buffering_faster_model;
    if (enabled === undefined && !faster && !diagnostics.safety_buffered) return undefined;
    const parts = [
      enabled === undefined ? null : enabled ? t("accounts.testDiagSafetyBufferingOn") : t("accounts.testDiagSafetyBufferingOff"),
      faster ? t("accounts.testDiagSafetyBufferingFaster", { model: faster }) : null,
      diagnostics.safety_buffered ? t("accounts.testDiagSafetyBufferingTriggered") : null,
    ].filter(Boolean);
    return parts.join(" · ");
  })();
  const identityRows: Array<{ label: string; value?: string; hint?: string }> = [
    { label: t("accounts.testDiagResponseModel"), value: diagnostics?.response_model },
    { label: t("accounts.testDiagTransport"), value: diagnostics?.transport },
    { label: t("accounts.testDiagPlan"), value: diagnostics?.plan_type },
    { label: t("accounts.testDiagSafetyBuffering"), value: safetyBuffering, hint: t("accounts.testDiagSafetyBufferingHint") },
    { label: "Response ID", value: diagnostics?.response_id },
    { label: "Request ID", value: diagnostics?.request_id },
    { label: "CF-Ray", value: diagnostics?.cf_ray },
    { label: t("accounts.testDiagResponseStatus"), value: diagnostics?.response_status },
    { label: t("accounts.testDiagIncompleteReason"), value: diagnostics?.incomplete_reason },
    { label: t("accounts.testDiagErrorType"), value: diagnostics?.error_type },
    { label: t("accounts.testDiagErrorCode"), value: diagnostics?.error_code },
  ].filter((row) => row.value);
  const tokenMetrics = codexTestTokenMetrics(diagnostics?.usage);
  const tokenLabels = {
    input_tokens: t("accounts.testDiagInputTokens"),
    output_tokens: t("accounts.testDiagOutputTokens"),
    cached_input_tokens: t("accounts.testDiagCachedTokens"),
    reasoning_output_tokens: t("accounts.testDiagReasoningTokens"),
  } as const;
  const tokenBarColors = ["bg-blue-500", "bg-emerald-500", "bg-violet-500", "bg-amber-500"];
  const hasDetails = Boolean(
    identityRows.length > 0 ||
      diagnostics?.usage ||
      diagnostics?.response_headers?.length ||
      diagnostics?.response_body,
  );
  const monoStyle = { fontFamily: "var(--font-geist-mono)" } as const;

  return (
    <Modal
      show={true}
      title={t("accounts.testConnectionTitle", {
        account: formatAccountName(account),
      })}
      onClose={() => {
        abortRef.current?.abort();
        onClose();
      }}
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          {diagnostics ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="mr-auto"
              disabled={running}
              onClick={() => void handleCopyDiagnostics()}
            >
              <Copy className="size-3.5" />
              {t("accounts.testDiagCopy")}
            </Button>
          ) : null}
          <Button
            variant="outline"
            onClick={() => {
              abortRef.current?.abort();
              onClose();
            }}
          >
            {t("common.close")}
          </Button>
          <Button
            type="button"
            disabled={running || !modelOptionsReady || !selectedModel}
            onClick={() => setAttempt((value) => value + 1)}
          >
            <RefreshCw className={cn("size-3.5", running && "animate-spin")} />
            {t("accounts.testDiagRetry")}
          </Button>
        </div>
      }
      contentClassName="sm:max-w-[760px]"
    >
      <div className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <span
            className={`flex items-center gap-1.5 text-sm font-semibold ${statusColor}`}
          >
            <StatusIcon
              className={cn("size-4", statusIconSpin && "animate-spin")}
            />
            {statusText}
          </span>
          <Select
            className="w-52 max-w-full"
            compact
            value={selectedModel}
            onValueChange={setSelectedModel}
            options={modelSelectOptions}
            placeholder={model || t("settings.testModel")}
            disabled={!modelOptionsReady || modelSelectOptions.length === 0}
          />
        </div>

        {diagnostics && (
          <div className="grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
            {timingTiles.map((tile) => (
              <div key={tile.key} className="rounded-md bg-muted/40 px-2.5 py-2">
                <div className="text-[11px] text-muted-foreground">{tile.label}</div>
                <div
                  className={cn("mt-0.5 text-sm font-semibold tabular-nums", tile.tone)}
                  style={monoStyle}
                >
                  {tile.value}
                </div>
              </div>
            ))}
          </div>
        )}

        {usageWindows.length > 0 && (
          <div className="space-y-2 rounded-xl border border-border bg-muted/30 px-4 py-3">
            <div className="flex items-center justify-between gap-2 text-xs font-semibold text-muted-foreground">
              <span className="flex items-center gap-1.5">
                <Gauge className="size-3.5 text-primary" />
                {t("accounts.testDiagUsageWindows")}
              </span>
              {diagnostics?.plan_type ? (
                <Badge variant="outline" className="text-[11px] font-medium">
                  {diagnostics.plan_type}
                </Badge>
              ) : null}
            </div>
            {usageWindows.map((window, index) => (
              <div key={`${window.label}-${index}`} className="space-y-1">
                <div className="flex items-center justify-between gap-2 text-xs">
                  <span className="text-foreground">{window.label}</span>
                  <span className="tabular-nums text-muted-foreground" style={monoStyle}>
                    {window.percent === null ? "—" : `${window.percent.toFixed(window.percent >= 10 ? 0 : 1)}%`}
                    {window.reset ? ` · ${t("accounts.testDiagResetIn", { time: window.reset })}` : ""}
                  </span>
                </div>
                <div aria-hidden className="h-1.5 overflow-hidden rounded-full bg-muted">
                  <div
                    className={cn(
                      "h-full rounded-full transition-[width]",
                      window.percent !== null && window.percent >= 100
                        ? "bg-red-500"
                        : window.percent !== null && window.percent >= 80
                          ? "bg-amber-500"
                          : "bg-emerald-500",
                    )}
                    style={{ width: `${window.percent ?? 0}%` }}
                  />
                </div>
              </div>
            ))}
          </div>
        )}

        {(output.length > 0 ||
          status === "connecting" ||
          status === "streaming") && (
          <div
            className="min-h-[80px] max-h-[240px] overflow-auto rounded-lg border border-border bg-muted/30 p-3 text-[13px] leading-relaxed whitespace-pre-wrap break-all"
            style={{ fontFamily: "var(--font-geist-mono)" }}
          >
            {output.length === 0 && status === "connecting" && (
              <span className="text-muted-foreground animate-pulse">
                {t("accounts.sendingTestRequest")}
              </span>
            )}
            {output.join("")}
            <div ref={outputEndRef} />
          </div>
        )}

        {errorMsg && (
          <div className="rounded-xl border border-red-200 bg-red-50 p-3.5 text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400">
            <div className="mb-2 flex items-center justify-between gap-3">
              <div className="text-sm font-semibold">
                {t("accounts.failureDetails")}
              </div>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-7 shrink-0 px-2 text-red-600 hover:bg-red-100 hover:text-red-700 dark:text-red-400 dark:hover:bg-red-900/40 dark:hover:text-red-300"
                onClick={() => void handleCopyFailureDetails()}
                title={t("common.copy")}
              >
                <Copy className="size-3.5" />
                {t("common.copy")}
              </Button>
            </div>
            <pre
              className="max-h-[34vh] overflow-auto text-[13px] leading-relaxed whitespace-pre-wrap break-all"
              style={{ fontFamily: "var(--font-geist-mono)" }}
            >
              {formattedErrorMsg}
            </pre>
          </div>
        )}

        {status === "success" && (
          <div className="flex items-center gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-2.5 text-sm text-emerald-700 dark:border-emerald-900/50 dark:bg-emerald-950/30 dark:text-emerald-400">
            <RotateCcw className="size-4 shrink-0" />
            {successHint ?? t("accounts.testAutoReset")}
          </div>
        )}

        {diagnostics && hasDetails && (
          <div className="rounded-xl border border-border">
            <button
              type="button"
              className="flex w-full items-center gap-2 rounded-xl px-4 py-2.5 text-left text-xs font-semibold text-foreground hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
              aria-expanded={detailsOpen}
              onClick={() => setDetailsOpen((open) => !open)}
            >
              <Activity className="size-3.5 text-primary" />
              <span>{t("accounts.testDiagDetails")}</span>
              {running && !diagnosticsFinal && (
                <span className="text-[11px] font-normal text-muted-foreground">
                  · {t("accounts.testDiagPending")}
                </span>
              )}
              <ChevronDown
                className={cn(
                  "ml-auto size-4 shrink-0 text-muted-foreground transition-transform",
                  detailsOpen && "rotate-180",
                )}
              />
            </button>
            {detailsOpen && (
              <div className="space-y-4 border-t border-border px-4 py-3">
                {identityRows.length > 0 && (
                  <dl className="grid grid-cols-1 gap-x-4 gap-y-2 text-xs sm:grid-cols-2">
                    {identityRows.map((row) => (
                      <div key={row.label} className={cn("min-w-0", row.hint && "sm:col-span-2")}>
                        <dt className="text-[11px] text-muted-foreground">{row.label}</dt>
                        <dd className="mt-0.5 break-all text-foreground" style={monoStyle}>
                          {row.value}
                        </dd>
                        {row.hint && (
                          <p className="mt-1 text-[11px] leading-relaxed text-muted-foreground">{row.hint}</p>
                        )}
                      </div>
                    ))}
                  </dl>
                )}

                {diagnostics.usage && (
                  <div className="space-y-2">
                    <div className="flex items-center justify-between gap-2 text-xs font-semibold text-muted-foreground">
                      <span>{t("accounts.testDiagTokens")}</span>
                      {typeof diagnostics.usage.total_tokens === "number" && (
                        <span className="tabular-nums" style={monoStyle}>
                          {t("accounts.testDiagTotalTokens")} {diagnostics.usage.total_tokens.toLocaleString()}
                        </span>
                      )}
                    </div>
                    <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
                      {tokenMetrics.map((metric, index) => (
                        <div key={metric.key} className="rounded-md bg-muted/40 px-2.5 py-2">
                          <div className="text-[11px] text-muted-foreground">{tokenLabels[metric.key]}</div>
                          <div className="mt-0.5 text-sm font-semibold tabular-nums" style={monoStyle}>
                            {metric.value?.toLocaleString() ?? "—"}
                          </div>
                          <div aria-hidden className="mt-1.5 h-1 overflow-hidden rounded-full bg-muted">
                            <div
                              className={cn("h-full rounded-full transition-[width]", tokenBarColors[index])}
                              style={{ width: `${metric.percent}%` }}
                            />
                          </div>
                        </div>
                      ))}
                    </div>
                  </div>
                )}

                {diagnostics.response_headers && diagnostics.response_headers.length > 0 && (
                  <div className="rounded-lg border border-border/70">
                    <button
                      type="button"
                      className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs font-medium hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                      aria-expanded={headersOpen}
                      onClick={() => setHeadersOpen((open) => !open)}
                    >
                      <span>{t("accounts.testDiagHeaders")}</span>
                      <span className="rounded bg-muted px-1.5 text-[10px] tabular-nums text-muted-foreground">
                        {diagnostics.response_headers.length}
                      </span>
                      <ChevronDown
                        className={cn(
                          "ml-auto size-3.5 shrink-0 text-muted-foreground transition-transform",
                          headersOpen && "rotate-180",
                        )}
                      />
                    </button>
                    {headersOpen && (
                      <div className="max-h-56 overflow-auto border-t border-border/70">
                        <table className="w-full table-fixed text-left text-[11px]">
                          <tbody>
                            {diagnostics.response_headers.map((header, index) => (
                              <tr key={`${header.name}-${index}`} className="border-b border-border/40 last:border-0">
                                <th scope="row" className="w-[44%] break-all px-3 py-1.5 align-top font-normal text-muted-foreground" style={monoStyle}>
                                  {header.name}
                                </th>
                                <td className="break-all px-3 py-1.5 align-top" style={monoStyle}>
                                  {header.value}
                                </td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      </div>
                    )}
                  </div>
                )}

                {diagnostics.response_body && (
                  <div className="rounded-lg border border-border/70">
                    <button
                      type="button"
                      className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs font-medium hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                      aria-expanded={rawOpen}
                      onClick={() => setRawOpen((open) => !open)}
                    >
                      <span>{t("accounts.testDiagRawBody")}</span>
                      {diagnostics.body_truncated && (
                        <span className="text-[10px] font-normal text-muted-foreground">
                          · {t("accounts.testDiagBodyTruncated")}
                        </span>
                      )}
                      <ChevronDown
                        className={cn(
                          "ml-auto size-3.5 shrink-0 text-muted-foreground transition-transform",
                          rawOpen && "rotate-180",
                        )}
                      />
                    </button>
                    {rawOpen && (
                      <div className="border-t border-border/70 px-3 py-2.5">
                        <p className="mb-2 text-[11px] leading-relaxed text-muted-foreground">
                          {t("accounts.testDiagRawHint")}
                        </p>
                        <pre
                          className="max-h-64 overflow-auto whitespace-pre-wrap break-all text-[11px] leading-relaxed"
                          style={monoStyle}
                        >
                          {diagnostics.response_body}
                        </pre>
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}
          </div>
        )}
      </div>
    </Modal>
  );
}

interface ResetTimeLabel {
  label: string;
  title: string;
}

// 格式化重置时间为具体时间，列表显示到秒，tooltip 保留完整日期。
function formatResetAt(resetAt: string | undefined): ResetTimeLabel | null {
  if (!resetAt) return null;
  const d = new Date(resetAt);
  if (Number.isNaN(d.getTime()) || d.getTime() <= Date.now()) return null;
  const full = formatBeijingTime(resetAt, "");
  if (!full) return null;
  return {
    label: full.slice(5),
    title: full,
  };
}

function formatCompactUsageNumber(value?: number): string {
  const n = Number(value || 0);
  if (n >= 1_000_000)
    return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`;
  return String(n);
}

function hasUsageWindowDetail(detail?: AccountRow["usage_5h_detail"]): boolean {
  return Boolean(
    detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0),
  );
}

// 用量进度条颜色
function usageBarColor(pct: number): string {
  if (pct >= 90) return "bg-red-500";
  if (pct >= 70) return "bg-amber-500";
  return "bg-emerald-500";
}

// 标签 / 条 / 百分比 三列固定，避免 7d 与 spark 把进度条拉成不同长度。
const USAGE_BAR_LABEL_CLASS =
  "w-10 shrink-0 text-[11px] font-medium text-muted-foreground";
const USAGE_BAR_TRACK_CLASS =
  "h-1.5 w-[88px] shrink-0 rounded-full bg-muted overflow-hidden";
const USAGE_BAR_META_CLASS =
  "text-[11px] font-medium text-muted-foreground mt-0.5 pl-[46px]";

// 单行用量进度条
function UsageBar({
  label,
  pct,
  resetAt,
  detail,
}: {
  label: string;
  pct: number;
  resetAt?: string;
  detail?: AccountRow["usage_5h_detail"];
}) {
  const resetTime = formatResetAt(resetAt);
  const { t } = useTranslation();
  const detailText = hasUsageWindowDetail(detail)
    ? `${formatCompactUsageNumber(detail?.requests)} ${t("accounts.usageReqUnit")} / ${formatCompactUsageNumber(detail?.tokens)} ${t("accounts.usageTokUnit")}`
    : "";
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <span className={USAGE_BAR_LABEL_CLASS}>{label}</span>
        <div className={USAGE_BAR_TRACK_CLASS}>
          <div
            className={`h-full rounded-full transition-all ${usageBarColor(pct)}`}
            style={{ width: `${Math.min(100, pct)}%` }}
          />
        </div>
        <span className="w-[42px] shrink-0 text-right text-[12px] font-semibold tabular-nums">
          {pct.toFixed(1)}%
        </span>
      </div>
      {detailText && <div className={USAGE_BAR_META_CLASS}>{detailText}</div>}
      {resetTime && (
        <div className={USAGE_BAR_META_CLASS} title={resetTime.title}>
          ⏱ {resetTime.label}
        </div>
      )}
    </div>
  );
}

function UsageWindowStat({
  label,
  detail,
  apiAccount = false,
}: {
  label: string;
  detail?: AccountRow["usage_5h_detail"];
  apiAccount?: boolean;
}) {
  const { t } = useTranslation();
  if (!detail || !hasUsageWindowDetail(detail)) return null;

  const accountBilledText =
    typeof detail.account_billed === "number"
      ? detail.account_billed.toFixed(4)
      : "";
  const userBilledText =
    typeof detail.user_billed === "number" ? detail.user_billed.toFixed(4) : "";

  return (
    <div className="flex flex-col gap-0.5">
      <div className="flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
        <span className={USAGE_BAR_LABEL_CLASS}>{label}</span>
        <span>
          {formatCompactUsageNumber(detail?.requests)}{" "}
          {t("accounts.usageReqUnit")} /{" "}
          {formatCompactUsageNumber(detail?.tokens)}{" "}
          {t("accounts.usageTokUnit")}
        </span>
      </div>
      {(accountBilledText || userBilledText) && (
        <div
          className={cn(
            "pl-[46px] text-[10px]",
            apiAccount
              ? "flex flex-col items-start gap-0.5 font-medium"
              : "flex items-center gap-1.5 text-muted-foreground/80",
          )}
        >
          {accountBilledText && (
            <span
              className={cn(
                apiAccount &&
                  "text-emerald-700 dark:text-emerald-400",
              )}
            >
              {t("accounts.accountBilledLabel")}: ${accountBilledText}
            </span>
          )}
          {userBilledText && (
            <span
              className={cn(
                apiAccount && "text-sky-700 dark:text-sky-400",
              )}
            >
              {t("accounts.userBilledLabel")}: ${userBilledText}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

// 今日统计列:网关侧口径,服务器时区当天 0 点起(参考 sub2api 的今日统计列)。
// page-stats 对本页每个账号必回该字段,零值是真实的"今天没跑";
// 字段缺失说明 stats 还没到(加载中/失败),显示占位而不是 0。
function TodayStatsCell({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const detail = account.usage_today_detail;
  if (!detail) {
    return <span className="text-[12px] font-mono text-muted-foreground/40">-</span>;
  }
  const requests = detail.requests ?? 0;
  const tokens = detail.tokens ?? 0;
  const accountBilled =
    typeof detail.account_billed === "number" ? detail.account_billed : 0;
  const userBilled =
    typeof detail.user_billed === "number" ? detail.user_billed : 0;

  const tooltip = [
    `${t("accounts.todayStats")}:`,
    `Requests: ${requests.toLocaleString()} req`,
    `Tokens: ${tokens.toLocaleString()} tok`,
    `${t("accounts.accountBilledLabel")}: $${accountBilled.toFixed(4)}`,
    userBilled > 0 ? `${t("accounts.userBilledLabel")}: $${userBilled.toFixed(4)}` : "",
  ]
    .filter(Boolean)
    .join("\n");
  const modelBreakdown = buildModelCountBreakdown(detail.model_counts, requests);

  const content = (
    <div
      className={cn(
        "flex flex-col items-start gap-1 whitespace-nowrap text-[12px] tabular-nums",
        requests > 0 ? "cursor-help" : "cursor-default",
      )}
      title={requests > 0 ? undefined : tooltip}
      tabIndex={requests > 0 ? 0 : undefined}
      aria-label={
        requests > 0
          ? t("accounts.todayModelTooltipAria", { count: requests })
          : undefined
      }
    >
      <div className="flex items-center gap-2">
        <span
          className={cn(
            "inline-flex items-center gap-1",
            requests > 0
              ? "font-semibold text-foreground"
              : "font-normal text-muted-foreground/50",
          )}
        >
          <Activity
            className={cn(
              "size-3 shrink-0",
              requests > 0 ? "text-sky-500" : "text-muted-foreground/40",
            )}
            aria-hidden
          />
          <span>{requests > 0 ? requests.toLocaleString() : 0}</span>
          <span className="text-[11px] font-normal text-muted-foreground/60">req</span>
        </span>

        <span
          className={cn(
            "inline-flex items-center gap-1",
            tokens > 0
              ? "font-semibold text-foreground"
              : "font-normal text-muted-foreground/50",
          )}
        >
          <Sparkles
            className={cn(
              "size-3 shrink-0",
              tokens > 0 ? "text-purple-500 dark:text-purple-400" : "text-muted-foreground/40",
            )}
            aria-hidden
          />
          <span>{formatCompactUsageNumber(tokens)}</span>
          <span className="text-[11px] font-normal text-muted-foreground/60">tok</span>
        </span>
      </div>

      <div className="flex items-center gap-1.5 text-[11px]">
        {accountBilled > 0 ? (
          <span
            className="inline-flex items-center gap-1 rounded-md bg-emerald-500/10 px-1.5 py-0.5 font-mono text-[11px] font-medium tabular-nums text-emerald-700 ring-1 ring-inset ring-emerald-500/20 dark:text-emerald-400"
            title={t("accounts.accountBilledLabel")}
          >
            <Coins className="size-3 shrink-0 text-emerald-500" aria-hidden />
            ${accountBilled < 0.01 ? "<0.01" : accountBilled.toFixed(2)}
          </span>
        ) : (
          <span
            className="inline-flex items-center gap-1 rounded-md bg-slate-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-slate-500 dark:text-slate-400 ring-1 ring-inset ring-slate-500/20"
            title={t("accounts.accountBilledLabel")}
          >
            <Coins className="size-3 shrink-0 opacity-50" aria-hidden />
            $0.00
          </span>
        )}

        {userBilled > 0 && Math.abs(userBilled - accountBilled) > 0.0001 && (
          <span
            className="inline-flex items-center gap-1 rounded-md bg-sky-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-sky-700 ring-1 ring-inset ring-sky-500/20 dark:text-sky-400"
            title={`${t("accounts.userBilledLabel")}: $${userBilled.toFixed(4)}`}
          >
            U: ${userBilled < 0.01 ? "<0.01" : userBilled.toFixed(2)}
          </span>
        )}
      </div>
    </div>
  );

  if (requests <= 0) {
    return content;
  }

  return (
    <CountBreakdownTooltip
      title={t("accounts.todayModelTooltipTitle")}
      empty={t("accounts.todayModelEmpty")}
      total={requests}
      rows={modelBreakdown.map((row) => {
        const success = detail.model_success_counts?.[row.key];
        return {
          key: row.key,
          label: row.key === "unknown" ? t("accounts.unknownModel") : row.key,
          count: row.count,
          percent: row.percent,
          avgFirstTokenMs: detail.model_avg_first_token_ms?.[row.key],
          successRate:
            typeof success === "number" && row.count > 0
              ? (success / row.count) * 100
              : undefined,
        };
      })}
      showModelIcon
      tone="today"
      barClassName="bg-gradient-to-r from-sky-400 to-violet-300"
    >
      {content}
    </CountBreakdownTooltip>
  );
}

// 用量列组件
//
// 显示策略不再单独依赖 plan_type:
// 当 plan_type 还停留在按 RT 刷新出来的旧值(例如 "free")、但账号实际已订阅、
// 后端已经返回 5h 窗口数据时,只看 plan_type 会把 5h 吞掉。
// 因此这里以"是否真的存在 5h / 7d 数据(含 reset 时间)"作为主判据。
function UsageCell({
  account,
  wide = false,
  onRefreshed,
}: {
  account: AccountRow;
  wide?: boolean;
  onRefreshed?: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [refreshing, setRefreshing] = useState(false);

  const handleRefresh = useCallback(async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await api.refreshAccountUsage(account.id);
      onRefreshed?.();
    } catch (err) {
      showToast(
        err instanceof Error ? err.message : t("accounts.usageRefreshFailed"),
        "error",
      );
    } finally {
      setRefreshing(false);
    }
  }, [account.id, onRefreshed, refreshing, showToast, t]);

  const refreshButton = (
    <button
      type="button"
      onClick={handleRefresh}
      disabled={refreshing}
      title={t("accounts.refreshUsage")}
      aria-label={t("accounts.refreshUsage")}
      className={cn(
        "shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50",
        account.openai_responses_api && "mr-6",
      )}
    >
      <RefreshCw className={`size-3 ${refreshing ? "animate-spin" : ""}`} />
    </button>
  );

  const has7d =
    account.usage_percent_7d !== null && account.usage_percent_7d !== undefined;
  const has5h =
    account.usage_percent_5h !== null && account.usage_percent_5h !== undefined;
  const hasSparkPct =
    account.usage_percent_spark !== null &&
    account.usage_percent_spark !== undefined;
  const showSpark = isSparkUsagePlan(account.plan_type);
  const has7dDetail = hasUsageWindowDetail(account.usage_7d_detail);
  const has5hReset = !!account.reset_5h_at;
  const has7dReset = !!account.reset_7d_at;

  const fiveHourPresent = has5h || has5hReset;
  const sevenDayPresent = has7d || has7dDetail || has7dReset;
  // 长窗口标签:free/team plan 实为月窗(约 30 天),按真实周期显示 30d 而非误标 7d (issue #324)
  const longWindowLabel = formatLongUsageWindowLabel(account);
  // 5h 是上游可选窗口：仅数据存在时展示，不再因 premium plan 强制占位（issue #382）
  const showFiveHour = fiveHourPresent;

  const sparkBar = showSpark ? (
    hasSparkPct ? (
      <UsageBar
        label="spark"
        pct={account.usage_percent_spark!}
        resetAt={account.reset_spark_at}
      />
    ) : (
      <UsageBar
        label="spark"
        pct={0}
        resetAt={account.reset_spark_at}
      />
    )
  ) : null;

  if (showFiveHour) {
    if (!has5h && !has7d && !has7dDetail && !has5hReset && !has7dReset && !showSpark)
      return <span className="text-[12px] text-muted-foreground">-</span>;
    return (
      <div className={`${wide ? "w-full" : "w-56"} flex items-start gap-1`}>
        <div className="w-[188px] space-y-1.5">
          {has5h ? (
            <UsageBar
              label="5h"
              pct={account.usage_percent_5h!}
              resetAt={account.reset_5h_at}
              detail={account.usage_5h_detail}
            />
          ) : (
            <UsageWindowStat
              label="5h"
              detail={account.usage_5h_detail}
              apiAccount={account.openai_responses_api}
            />
          )}
          {sparkBar}
          {has7d ? (
            <UsageBar
              label={longWindowLabel}
              pct={account.usage_percent_7d!}
              resetAt={account.reset_7d_at}
              detail={account.usage_7d_detail}
            />
          ) : (
            <UsageWindowStat label={longWindowLabel} detail={account.usage_7d_detail} />
          )}
        </div>
        {refreshButton}
      </div>
    );
  }

  if (showSpark) {
    return (
      <div className={`${wide ? "w-full" : "w-56"} flex items-start gap-1`}>
        <div className="w-[188px] space-y-1.5">
          {sparkBar}
          {has7d ? (
            <UsageBar
              label={longWindowLabel}
              pct={account.usage_percent_7d!}
              resetAt={account.reset_7d_at}
              detail={account.usage_7d_detail}
            />
          ) : (
            <UsageWindowStat
              label={longWindowLabel}
              detail={account.usage_7d_detail}
              apiAccount={account.openai_responses_api}
            />
          )}
        </div>
        {refreshButton}
      </div>
    );
  }

  if (sevenDayPresent) {
    return (
      <div className={`${wide ? "w-full" : "w-56"} flex items-start gap-1`}>
        <div className="w-[188px]">
          {has7d ? (
            <UsageBar
              label={longWindowLabel}
              pct={account.usage_percent_7d!}
              resetAt={account.reset_7d_at}
              detail={account.usage_7d_detail}
            />
          ) : (
            <UsageWindowStat
              label={longWindowLabel}
              detail={account.usage_7d_detail}
              apiAccount={account.openai_responses_api}
            />
          )}
        </div>
        {refreshButton}
      </div>
    );
  }

  return <span className="text-[13px] text-muted-foreground">-</span>;
}

// 官方胶囊转圈的兜底超时:页面级重拉最多 6 次退避(累计约 95 秒)就会停,
// 超过这个时间还没有数据说明上游拉不到(鉴权失败/官方统计滞后),
// 再转下去只会让用户以为一直在加载。
const OFFICIAL_PENDING_SPIN_TIMEOUT_MS = 100_000;

function formatAPIAccountBalance(data: OpenAIResponsesBalanceResponse): string {
  if (data.unlimited) return "∞";
  const unit = data.unit.trim().toUpperCase();
  if (unit === "USD" || unit === "$") return formatOfficialUSD(data.balance);
  if (unit === "CNY" || unit === "RMB" || unit === "¥") {
    return `¥${data.balance.toLocaleString(undefined, { maximumFractionDigits: 2 })}`;
  }
  const value = data.balance.toLocaleString(undefined, {
    maximumFractionDigits: 4,
  });
  return unit && unit !== "QUOTA" ? `${value} ${data.unit}` : `${value} quota`;
}

function APIAccountBalanceBadge({ accountId }: { accountId: number }) {
  const { t } = useTranslation();
  const cached = apiBalanceCache.get(accountId);
  const [state, setState] = useState<APIBalanceLoadState>(() =>
    cached && cached.expiresAt > Date.now()
      ? cached.state
      : { loading: false },
  );
  const [visible, setVisible] = useState(false);
  const badgeRef = useRef<HTMLButtonElement>(null);

  const refresh = useCallback((force = false) => {
    setState({ loading: true });
    void loadAPIAccountBalance(accountId, force).then(setState);
  }, [accountId]);

  useEffect(() => {
    const element = badgeRef.current;
    if (!element || typeof IntersectionObserver === "undefined") {
      setVisible(true);
      return;
    }
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          setVisible(true);
          observer.disconnect();
        }
      },
      { rootMargin: "120px" },
    );
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (!visible) return;
    let active = true;
    setState((current) => (current.data || current.error ? current : { loading: true }));
    void loadAPIAccountBalance(accountId).then((next) => {
      if (active) setState(next);
    });
    return () => {
      active = false;
    };
  }, [accountId, visible]);

  const title = state.data
    ? t("accounts.apiBalanceTooltip", {
        source: state.data.source,
        time: formatRelativeTime(state.data.queried_at),
      })
    : state.error
      ? t("accounts.apiBalanceFailed", { error: state.error })
      : t("accounts.apiBalanceLoading");

  return (
    <button
      type="button"
      ref={badgeRef}
      onClick={(event) => {
        event.stopPropagation();
        refresh(true);
      }}
      className={cn(
        "inline-flex items-center gap-1 whitespace-nowrap rounded-md px-1.5 py-0.5 font-mono text-[11px] tabular-nums ring-1 ring-inset transition-colors",
        state.error
          ? "bg-red-500/10 text-red-700/70 ring-red-500/20 hover:bg-red-500/20 dark:text-red-400/70"
          : "bg-emerald-500/10 text-emerald-700 ring-emerald-500/20 hover:bg-emerald-500/20 dark:text-emerald-400",
      )}
      title={title}
    >
      {state.loading ? (
        <Loader2 className="size-3 shrink-0 animate-spin" aria-hidden />
      ) : (
        <Wallet className="size-3 shrink-0" aria-hidden />
      )}
      {t("accounts.apiBalanceLabel")}: {state.data ? formatAPIAccountBalance(state.data) : "—"}
    </button>
  );
}

// 成本列并排两套账,颜色区分口径:
// 上行(石板色)是网关自己的日志算出来的,只含经由本网关转发的请求;
// 下行(琥珀色)是 OpenAI 官方结算数,还包含用户直接用官方客户端的消耗。
// 两者不该相等,差额就是账号在网关之外的用量。
function BilledCell({
  account,
  onOpenOfficial,
}: {
  account: AccountRow;
  // 传了才把官方胶囊变成可点按钮（跳到用量弹窗的官方统计 tab）。
  onOpenOfficial?: (account: AccountRow) => void;
}) {
  const { t } = useTranslation();
  const official = officialUsdValue(account);
  const showOfficial =
    supportsOfficialUsage(account) && !isOfficialCostHiddenAccount(account);
  const showAPIBalance = account.openai_responses_api === true;
  // synced 表示后端已成功同步过但上游没有数据(官方统计有滞后):
  // 这是确定的"暂无数据",不是"还在加载",不该转圈。
  // 导入未满一天、封禁/错误号也不转圈：官方结算要到次日才出数。
  const officialSynced = account.official_usage_synced === true;
  const officialPending =
    showOfficial &&
    official === null &&
    !officialSynced &&
    !isOfficialCostTooNew(account);
  const [officialSpinTimedOut, setOfficialSpinTimedOut] = useState(false);
  useEffect(() => {
    if (!officialPending) return undefined;
    const timer = window.setTimeout(
      () => setOfficialSpinTimedOut(true),
      OFFICIAL_PENDING_SPIN_TIMEOUT_MS,
    );
    return () => window.clearTimeout(timer);
  }, [officialPending]);
  const officialSpinning = officialPending && !officialSpinTimedOut;

  const h5 = typeof account.billed_5h === "number" ? account.billed_5h.toFixed(2) : null;
  const d7 = typeof account.billed_7d === "number" ? account.billed_7d.toFixed(2) : null;
  const has5hWindow =
    (account.usage_percent_5h !== null && account.usage_percent_5h !== undefined) ||
    !!account.reset_5h_at;
  const visibleH5 = has5hWindow ? h5 : null;
  if (visibleH5 === null && d7 === null && !showOfficial && !showAPIBalance) {
    return <span className="text-[12px] text-muted-foreground">-</span>;
  }
  const longLabel = formatLongUsageWindowLabel(account);
  const officialLabel =
    official !== null ? formatOfficialUSD(official) : "—";
  const officialEmpty = showOfficial && official === null;
  const officialClassName = officialEmpty
    ? "inline-flex items-center gap-1 whitespace-nowrap rounded-md bg-amber-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-amber-700/70 ring-1 ring-inset ring-amber-500/20 dark:text-amber-400/70"
    : "inline-flex items-center gap-1 whitespace-nowrap rounded-md bg-amber-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-amber-700 ring-1 ring-inset ring-amber-500/20 dark:text-amber-400";
  const officialTitle = officialEmpty
    ? officialSpinning
      ? t("accounts.billedOfficialPending")
      : `${t("accounts.billedOfficialNoData")}\n${t("accounts.billedOfficialOpen")}`
    : `${t("accounts.billedOfficialHint")}\n${t("accounts.billedOfficialOpen")}`;
  return (
    <div className="flex flex-col items-start gap-1">
      {showAPIBalance && <APIAccountBalanceBadge accountId={account.id} />}
      {(visibleH5 !== null || d7 !== null) && (
        <span
          className="inline-flex items-center gap-1 whitespace-nowrap rounded-md bg-slate-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-slate-700 ring-1 ring-inset ring-slate-500/20 dark:text-slate-300"
          title={t("accounts.billedGatewayHint")}
        >
          <Wallet className="size-3 shrink-0" aria-hidden />
          {visibleH5 !== null && `5h: $${visibleH5}`}
          {visibleH5 !== null && d7 !== null && " / "}
          {d7 !== null ? `${longLabel}: $${d7}` : null}
        </span>
      )}
      {showOfficial &&
        (onOpenOfficial ? (
          <button
            type="button"
            // 行本身点击会打开账号详情，成本胶囊要抢在它前面并阻断冒泡。
            onClick={(event) => {
              event.stopPropagation();
              onOpenOfficial(account);
            }}
            className={`${officialClassName} cursor-pointer transition-colors hover:bg-amber-500/20 hover:ring-amber-500/40`}
            title={officialTitle}
          >
            {officialSpinning ? (
              <Loader2 className="size-3 shrink-0 animate-spin" aria-hidden />
            ) : (
              <Banknote className="size-3 shrink-0" aria-hidden />
            )}
            {t("accounts.billedOfficialLabel")}: {officialLabel}
          </button>
        ) : (
          <span className={officialClassName} title={officialTitle}>
            {officialSpinning ? (
              <Loader2 className="size-3 shrink-0 animate-spin" aria-hidden />
            ) : (
              <Banknote className="size-3 shrink-0" aria-hidden />
            )}
            {t("accounts.billedOfficialLabel")}: {officialLabel}
          </span>
        ))}
    </div>
  );
}

// 官方成本可能很小(几分钱)也可能上千,统一两位小数,极小值不显示成 $0.00。
function formatOfficialUSD(value: number): string {
  if (!Number.isFinite(value) || value === 0) return "$0";
  if (Math.abs(value) < 0.01) return "<$0.01";
  return `$${value.toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })}`;
}

function getAccountStatusCountdownUntil(
  account: AccountRow,
): string | undefined {
  const status = account.status;
  const rateLimited =
    status === "rate_limited" ||
    status === "responses_rate_limited" ||
    status === "rate_limited_5h" ||
    status === "rate_limited_7d";
  if (
    account.cooldown_until &&
    (rateLimited ||
      status === "error" ||
      status === "cooldown" ||
      status === "overload_paused")
  ) {
    return account.cooldown_until;
  }
  // 限流态但没有 cooldown：积分顶替限流会主动释放本地用量判罚（cooldown_until 被清空），
  // premium 5h 打满也是直接由用量窗口判定、从不落 cooldown。这两种情况下倒计时改用窗口
  // 重置时间——徽章上写着「限流 | 7d」，右边就该显示它多久恢复，而不是整个消失。
  if (rateLimited || status === "quota_paused") {
    const window = getAccountRateLimitWindow(account);
    if (window === "7d") {
      return account.reset_7d_at;
    }
    if (window === "5h") {
      return account.reset_5h_at;
    }
  }
  if (status === "usage_exhausted") {
    return account.reset_7d_at;
  }
  return undefined;
}

function AccountStatusCountdown({ account }: { account: AccountRow }) {
  const until = getAccountStatusCountdownUntil(account);
  if (!until) return null;
  return <CooldownTimer until={until} />;
}

// 冷却倒计时组件
function CooldownTimer({ until }: { until: string }) {
  const remaining = useCountdownRemaining(until);
  const title = formatBeijingTime(until);

  if (!remaining) return null;
  return (
    <span
      className="inline-flex h-6 min-w-[112px] shrink-0 items-center justify-center gap-1.5 rounded-full bg-amber-50 px-2 text-[11px] font-mono leading-none tabular-nums text-amber-700 ring-1 ring-inset ring-amber-200/70 dark:bg-amber-950/40 dark:text-amber-300 dark:ring-amber-400/20"
      title={title}
    >
      <Hourglass className="size-3 shrink-0" aria-hidden="true" />
      {remaining}
    </span>
  );
}

// 自助提交待审核面板:列出 enabled=false 且带 self-service 标签的账号,
// 支持查看/编辑备注、通过(启用)与拒绝(删除)。
const SELF_SERVICE_TAG = "self-service";

function PendingSelfServiceReviewPanel({
  onApprove,
  onReject,
  onSaveNote,
  onCountChange,
}: {
  onApprove: (account: AccountRow) => Promise<void>;
  onReject: (account: AccountRow) => Promise<void>;
  onSaveNote: (account: AccountRow, note: string) => Promise<void>;
  onCountChange?: (count: number) => void;
}) {
  const { t } = useTranslation();
  const [pending, setPending] = useState<AccountRow[]>([]);
  const [pendingPage, setPendingPage] = useState(1);
  const [pendingTotal, setPendingTotal] = useState(0);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loadingPending, setLoadingPending] = useState(true);
  const pendingPageSize = 20;
  const loadPending = useCallback(async () => {
    try {
      const response = await api.getAccountsPage({
        channel: "codex",
        page: pendingPage,
        pageSize: pendingPageSize,
        status: "disabled",
        tag: SELF_SERVICE_TAG,
        sort: "created_at",
        order: "asc",
      });
      const total = response.total ?? 0;
      setPending(response.accounts ?? []);
      setPendingTotal(total);
      setLoadError(null);
      onCountChange?.(total);
      if (response.page !== pendingPage) setPendingPage(response.page);
    } catch (error) {
      setLoadError(getErrorMessage(error));
    } finally {
      setLoadingPending(false);
    }
  }, [onCountChange, pendingPage]);
  useEffect(() => { void loadPending(); }, [loadPending]);
  useEffect(() => {
    const refresh = () => {
      if (!document.hidden) void loadPending();
    };
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, [loadPending]);
  const [editingId, setEditingId] = useState<number | null>(null);
  const [draftNote, setDraftNote] = useState("");
  const [savingId, setSavingId] = useState<number | null>(null);

  const startEdit = (account: AccountRow) => {
    setEditingId(account.id);
    setDraftNote(account.note ?? "");
  };

  const cancelEdit = () => {
    setEditingId(null);
    setDraftNote("");
  };

  const saveNote = async (account: AccountRow) => {
    setSavingId(account.id);
    try {
      await onSaveNote(account, draftNote.trim());
      await loadPending();
      setEditingId(null);
      setDraftNote("");
    } finally {
      setSavingId(null);
    }
  };

  return (
    <Card id="pending-review" className="border-amber-500/40 bg-amber-500/[0.04] shadow-sm scroll-mt-4">
      <CardContent className="p-3 sm:p-4">
        <div className="mb-3 flex items-center gap-2.5">
          <div className="flex size-9 items-center justify-center rounded-xl bg-amber-500/15 text-amber-600 dark:text-amber-300">
            <Hourglass className="size-4" />
          </div>
          <div className="min-w-0">
            <div className="text-sm font-semibold tracking-tight">
              {t("accounts.pendingReview.title")}
            </div>
            <div className="text-[11px] text-muted-foreground">
              {t("accounts.pendingReview.count", { count: pendingTotal })} ·{" "}
              {t("accounts.pendingReview.desc")}
            </div>
          </div>
        </div>

        {loadingPending && pending.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("common.loading")}</p>
        ) : loadError ? (
          <div className="flex flex-wrap items-center justify-between gap-2 text-sm text-destructive">
            <span>{t("accounts.pendingReview.loadFailed", { error: loadError })}</span>
            <Button size="sm" variant="outline" onClick={() => void loadPending()}>
              {t("common.retry")}
            </Button>
          </div>
        ) : pending.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("accounts.pendingReview.empty")}</p>
        ) : (
        <>
        <div className="space-y-2">
          {pending.map((account) => {
            const editing = editingId === account.id;
            const saving = savingId === account.id;
            return (
              <div
                key={account.id}
                className="flex flex-col gap-3 rounded-xl border border-border/70 bg-card p-3 sm:flex-row sm:items-start sm:justify-between"
              >
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="inline-flex items-center gap-1.5 text-sm font-semibold text-foreground">
                      <Mail className="size-3.5 text-muted-foreground" />
                      {account.email || `ID ${account.id}`}
                    </span>
                    <Badge className="border-transparent bg-amber-500/14 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300">
                      {SELF_SERVICE_TAG}
                    </Badge>
                    {account.plan_type ? (
                      <span className="text-[11px] text-muted-foreground">
                        {account.plan_type}
                      </span>
                    ) : null}
                  </div>

                  {editing ? (
                    <div className="mt-2 flex flex-col gap-2 sm:flex-row sm:items-center">
                      <Input
                        value={draftNote}
                        onChange={(e) => setDraftNote(e.target.value)}
                        placeholder={t("accounts.pendingReview.notePlaceholder")}
                        className="h-9"
                      />
                      <div className="flex shrink-0 gap-1.5">
                        <Button
                          size="sm"
                          className="h-9"
                          disabled={saving}
                          onClick={() => void saveNote(account)}
                        >
                          {saving ? (
                            <RotateCcw className="size-3.5 animate-spin" />
                          ) : (
                            <Check className="size-3.5" />
                          )}
                          {t("common.save")}
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          className="h-9"
                          disabled={saving}
                          onClick={cancelEdit}
                        >
                          <X className="size-3.5" />
                        </Button>
                      </div>
                    </div>
                  ) : (
                    <button
                      type="button"
                      onClick={() => startEdit(account)}
                      className="mt-1.5 flex w-full items-start gap-1.5 rounded-md text-left text-xs leading-relaxed text-muted-foreground transition-colors hover:text-foreground"
                      title={t("accounts.pendingReview.editNote")}
                    >
                      <Pencil className="mt-0.5 size-3 shrink-0" />
                      <span className="min-w-0 break-words">
                        {account.note || t("accounts.pendingReview.noNote")}
                      </span>
                    </button>
                  )}
                </div>

                <div className="flex shrink-0 gap-1.5">
                  <Button
                    size="sm"
                    className="h-9"
                    onClick={() => void onApprove(account).then(loadPending)}
                  >
                    <Check className="size-3.5" />
                    {t("accounts.pendingReview.approve")}
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    className="h-9"
                    onClick={() => void onReject(account).then(loadPending)}
                  >
                    <Trash2 className="size-3.5" />
                    {t("accounts.pendingReview.reject")}
                  </Button>
                </div>
              </div>
            );
          })}
        </div>
        {pendingTotal > pendingPageSize ? (
          <div className="mt-3">
            <Pagination
              page={pendingPage}
              totalPages={Math.max(1, Math.ceil(pendingTotal / pendingPageSize))}
              totalItems={pendingTotal}
              pageSize={pendingPageSize}
              onPageChange={setPendingPage}
            />
          </div>
        ) : null}
        </>
        )}
      </CardContent>
    </Card>
  );
}
