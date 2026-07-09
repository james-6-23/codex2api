import type { ChangeEvent, DragEvent, ReactNode } from "react";
import { useCallback, useEffect, useRef, useState, useMemo } from "react";
import { api, getAdminKey, resetAdminAuthState } from "../api";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";import Pagination from "../components/Pagination";
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
  UpdateOpenAIResponsesAccountRequest,
  APIKeyRow,
  OpsOverviewResponse,
  AccountGroup,
  SystemSettings,
  RecycleBinAccountRow,
} from "../types";
import { getErrorMessage } from "../utils/error";
import { formatRelativeTime, formatBeijingTime } from "../utils/time";
import { formatLongUsageWindowLabel } from "../lib/usageFormat";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
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
  Cloud,
  Lock,
  Unlock,
  RotateCcw,
  Pencil,
  Check,
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
} from "lucide-react";
import { useTranslation } from "react-i18next";
import AccountUsageModal from "../components/AccountUsageModal";
import AccountHealthBar from "../components/AccountHealthBar";
import CodexInviteView from "../components/CodexInviteView";
import Sub2APIImportModal from "../components/Sub2APIImportModal";
import AccountQuotaDistributionChart from "../components/AccountQuotaDistributionChart";
import AccountRateLimitRecoveryChart from "../components/AccountRateLimitRecoveryChart";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  accountMatchesGroupFilter,
  pruneAccountGroupFilter,
  type AccountGroupFilterValue,
} from "../components/AccountGroupFilterSelect";
import ChipInput from "../components/ChipInput";

const OPERATION_PROGRESS_FLUSH_INTERVAL_MS = 200;
const ACCOUNT_ANALYSIS_VISIBILITY_KEY = "codex2api:accounts:analysis-visible";
const ACCOUNT_EMAIL_DOMAIN_VISIBILITY_KEY =
  "codex2api:accounts:email-domain-tags-visible";
const ACCOUNT_VISIBLE_COLUMNS_KEY = "codex2api:accounts:visible-columns";
const ACCOUNT_TABLE_COLUMNS = [
  "sequence",
  "email",
  "tags",
  "groups",
  "plan",
  "status",
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
  auto_pause_5h_threshold: number;
  auto_pause_7d_threshold: number;
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
  if (account.openai_responses_api) {
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

type BatchOperationAction = "batch_test" | "batch_delete" | "batch_refresh";

interface BatchOperationEvent {
  type: "start" | "progress" | "complete";
  action: BatchOperationAction;
  current?: number;
  total?: number;
  success?: number;
  failed?: number;
  banned?: number;
  rate_limited?: number;
  deleted?: number;
  account_id?: number;
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

export default function Accounts() {
  const { t, i18n } = useTranslation();
  const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS;
  const [showAdd, setShowAdd] = useState(false);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize(
    "accounts",
    20,
    pageSizeOptions,
  );
  const [statusFilter, setStatusFilter] = useState<
    | "all"
    | "normal"
    | "rate_limited"
    | "abnormal"
    | "banned"
    | "error"
    | "unsampled"
    | "disabled"
    | "locked"
  >("all");
  const [searchQuery, setSearchQuery] = useState("");
  const [planFilter, setPlanFilter] = useState<
    "all" | "pro" | "prolite" | "plus" | "team" | "k12" | "free"
  >("all");
  const [sortKey, setSortKey] = useState<
    "requests" | "usage" | "importTime" | null
  >(null);
  const [sortDir, setSortDir] = useState<"asc" | "desc">("desc");
  const [addForm, setAddForm] = useState<AddAccountRequest>({
    refresh_token: "",
    session_token: "",
    proxy_url: "",
  });
  const [addCustomHeadersText, setAddCustomHeadersText] = useState("");
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
  const [usageAccount, setUsageAccount] = useState<AccountRow | null>(null);
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
  const [editDispatchCountLimitInput, setEditDispatchCountLimitInput] =
    useState("");
  const [allowedAPIKeySelection, setAllowedAPIKeySelection] = useState<
    number[]
  >([]);
  const [editProxyUrl, setEditProxyUrl] = useState("");
  const [editCustomHeadersText, setEditCustomHeadersText] = useState("");
  const [testingProxyKey, setTestingProxyKey] = useState<string | null>(null);
  const [editOpenAIForm, setEditOpenAIForm] =
    useState<UpdateOpenAIResponsesAccountRequest>({
      name: "",
      base_url: "https://api.openai.com",
      api_key: "",
      models: [],
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
  const [showSub2APIImport, setShowSub2APIImport] = useState(false);
  const [showPasteImport, setShowPasteImport] = useState(false);
  const [pasteImportText, setPasteImportText] = useState("");
  const [dragging, setDragging] = useState(false);
  const dragCounter = useRef(0);
  const [showExportPicker, setShowExportPicker] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [showMigrate, setShowMigrate] = useState(false);
  const [showAnalysisCharts, setShowAnalysisCharts] = useState(
    getInitialAnalysisVisibility,
  );
  const [showRecycleBin, setShowRecycleBin] = useState(false);
  const [showInvite, setShowInvite] = useState(false);
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
  const [addMethod, setAddMethod] = useState<
    "rt" | "st" | "at" | "session" | "openai" | "oauth"
  >("oauth");
  const [atForm, setAtForm] = useState<AddATAccountRequest>({
    access_token: "",
    proxy_url: "",
  });
  const [sessionJson, setSessionJson] = useState("");
  const [sessionProxyUrl, setSessionProxyUrl] = useState("");
  // 允许重复添加：勾选后本次添加/导入跳过去重，强制新建（添加弹窗与导入弹窗共用）。
  const [allowDuplicate, setAllowDuplicate] = useState(false);
  const [openAIForm, setOpenAIForm] =
    useState<AddOpenAIResponsesAccountRequest>({
      base_url: "https://api.openai.com",
      api_key: "",
      models: [],
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
  const [tagFilter, setTagFilter] = useState<string>("");
  const [domainFilter, setDomainFilter] = useState<string>("");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(
    EMPTY_ACCOUNT_GROUP_FILTER,
  );
  const [allGroups, setAllGroups] = useState<AccountGroup[]>([]);
  const [showGroupManager, setShowGroupManager] = useState(false);
  const [groupDraft, setGroupDraft] = useState<AccountGroupDraft>({
    id: null,
    name: "",
    description: "",
    color: ACCOUNT_GROUP_COLORS[0],
    auto_pause_5h_threshold: 0,
    auto_pause_7d_threshold: 0,
  });
  const [groupSubmitting, setGroupSubmitting] = useState(false);
  const [showBatchMetaEditor, setShowBatchMetaEditor] = useState(false);
  const [batchTags, setBatchTags] = useState<string[]>([]);
  const [batchGroupIds, setBatchGroupIds] = useState<number[]>([]);
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
  const lazyModeRef = useRef<boolean | null>(null);
  const { toast, showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const ipApiLang = i18n.language?.startsWith("zh") ? "zh-CN" : "en";

  const handleTestProxyUrl = async (rawUrl: string, testKey: string) => {
    const url = rawUrl.trim();
    if (!url) {
      showToast(t("accounts.proxyUrlRequired"), "error");
      return;
    }
    if (testingProxyKey !== null) return;

    setTestingProxyKey(testKey);
    try {
      const result = await api.testProxy(url, undefined, ipApiLang);
      if (!result.success) {
        showToast(
          t("accounts.proxyTestFailed", {
            error: result.error || t("accounts.proxyTestUnknownError"),
          }),
          "error",
        );
        return;
      }

      const location =
        result.location ||
        [result.country, result.region, result.city].filter(Boolean).join(" ");
      showToast(
        t("accounts.proxyTestSuccess", {
          ip: result.ip || "-",
          location: location || "-",
          latency: result.latency_ms ?? 0,
        }),
      );
    } catch (error) {
      showToast(
        t("accounts.proxyTestFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setTestingProxyKey((current) => (current === testKey ? null : current));
    }
  };

  const renderProxyInput = ({
    value,
    onChange,
    testKey,
    label = t("accounts.proxyUrl"),
    placeholder = t("accounts.proxyUrlPlaceholder"),
    disabled = false,
  }: {
    value: string;
    onChange: (value: string) => void;
    testKey: string;
    label?: string;
    placeholder?: string;
    disabled?: boolean;
  }) => {
    const isTesting = testingProxyKey === testKey;
    const testDisabled = disabled || !value.trim() || testingProxyKey !== null;

    return (
      <div>
        <label className="block mb-2 text-sm font-semibold text-muted-foreground">
          {label}
        </label>
        <div className="flex flex-col gap-2 sm:flex-row">
          <Input
            className="min-w-0 flex-1"
            placeholder={placeholder}
            value={value}
            disabled={disabled}
            onChange={(event: ChangeEvent<HTMLInputElement>) =>
              onChange(event.target.value)
            }
          />
          <Button
            type="button"
            variant="outline"
            className="shrink-0 justify-center gap-1.5 sm:min-w-[108px]"
            disabled={testDisabled}
            onClick={() => void handleTestProxyUrl(value, testKey)}
          >
            <Zap className={`size-3.5 ${isTesting ? "animate-pulse" : ""}`} />
            {isTesting ? t("accounts.testingProxy") : t("accounts.testProxy")}
          </Button>
        </div>
      </div>
    );
  };

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
    };
  }, []);

  const closeOperationProgress = useCallback(() => {
    if (operationProgressHideTimer.current !== null) {
      window.clearTimeout(operationProgressHideTimer.current);
      operationProgressHideTimer.current = null;
    }
    setOperationProgress(null);
  }, []);

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
        message: event.error || event.message || prev?.message,
      }));
      if (event.type === "complete") {
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

  const loadAccounts = useCallback(async (options?: LoadOptions) => {
    const shouldLoadSettings = !options?.silent || lazyModeRef.current === null;
    const [
      accountsResponse,
      apiKeysResponse,
      opsOverview,
      groupsResponse,
      settings,
      healthBars,
    ] =
      await Promise.all([
        api.getAccounts(),
        api.getAPIKeys(),
        api.getOpsOverview().catch((): OpsOverviewResponse | null => null),
        api.listAccountGroups().catch(() => ({ groups: [] })),
        shouldLoadSettings
          ? api.getSettings().catch((): SystemSettings | null => null)
          : Promise.resolve<SystemSettings | null>(null),
        api
          .getAccountHealthBars()
          .then((res) => res.buckets)
          .catch((): Record<string, AccountHealthBucket[]> | null => null),
      ]);
    if (settings) {
      lazyModeRef.current = settings.lazy_mode;
    }
    setAllGroups(groupsResponse.groups ?? []);
    return {
      accounts: accountsResponse.accounts ?? [],
      apiKeys: apiKeysResponse.keys ?? [],
      opsOverview,
      lazyMode: lazyModeRef.current ?? false,
      healthBars: healthBars ?? {},
    };
  }, []);

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    accounts: AccountRow[];
    apiKeys: APIKeyRow[];
    opsOverview: OpsOverviewResponse | null;
    lazyMode: boolean;
    healthBars: Record<string, AccountHealthBucket[]>;
  }>({
    initialData: {
      accounts: [],
      apiKeys: [],
      opsOverview: null,
      lazyMode: false,
      healthBars: {},
    },
    load: loadAccounts,
  });
  const accounts = data.accounts;
  const apiKeys = data.apiKeys;
  const opsOverview = data.opsOverview;
  const lazyMode = data.lazyMode;
  const healthBars = data.healthBars;
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
    setPageMode(
      accounts.length < ACCOUNT_PERSONAL_MODE_AUTO_THRESHOLD
        ? "personal"
        : "pool",
    );
  }, [loading, accounts.length]);

  useEffect(() => {
    setGroupFilter((current) => pruneAccountGroupFilter(current, allGroups));
  }, [allGroups]);

  useEffect(() => {
    const needsUsageReload = (account: AccountRow) => {
      if (account.status !== "active" && account.status !== "ready") {
        return false;
      }

      const plan = normalizePlanType(account.plan_type);
      const has7d =
        account.usage_percent_7d !== null &&
        account.usage_percent_7d !== undefined;
      const has5h =
        account.usage_percent_5h !== null &&
        account.usage_percent_5h !== undefined;
      // 与 UsageCell 的显示判定保持一致:plan_type 可能滞后于真实订阅状态,
      // 看到 5h 重置时间就当订阅账号处理,触发拉取 5h 数据。
      const looksLikeSubscription =
        isPremiumUsagePlan(plan) || !!account.reset_5h_at;

      if (looksLikeSubscription) {
        return !has5h || !has7d;
      }
      return !has7d;
    };

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

    const timer = window.setTimeout(() => {
      void reloadSilently();
    }, 2500);

    return () => window.clearTimeout(timer);
  }, [accounts, reloadSilently]);

  const accountSummary = useMemo(() => {
    const rateLimitedWindowStats = getRateLimitedWindowStats(accounts);
    // 健康分类:异常(封禁/错误) > 限流 > 正常。禁用只是调度开关,保留独立计数但不影响健康分类。
    const bannedAccounts = accounts.filter(
      (account) => account.status === "unauthorized",
    ).length;
    const errorAccounts = accounts.filter(
      (account) => account.status === "error",
    ).length;
    const disabledAccounts = accounts.filter(
      (account) => account.enabled === false,
    ).length;
    const abnormalAccounts = accounts.filter(
      (account) =>
        account.status === "unauthorized" ||
        account.status === "error",
    ).length;
    const rateLimitedExclusive = accounts.filter(
      (account) =>
        account.status !== "unauthorized" &&
        account.status !== "error" &&
        isRateLimitedAccount(account),
    ).length;
    const normalAccounts = accounts.length - abnormalAccounts - rateLimitedExclusive;
    const unsampledAccounts = accounts.filter(isUnsampledQuotaAccount).length;
    return {
      totalAccounts: accounts.length,
      normalAccounts,
      rateLimitedAccounts: rateLimitedExclusive,
      rateLimited5hAccounts: rateLimitedWindowStats.fiveHour,
      rateLimited7dAccounts: rateLimitedWindowStats.sevenDay,
      abnormalAccounts,
      bannedAccounts,
      errorAccounts,
      unsampledAccounts,
      disabledAccounts,
      lockedAccounts: accounts.filter((account) => account.locked).length,
      subscriptionAccountsToLock: accounts.filter(
        (account) => isSubscriptionPlan(account.plan_type) && !account.locked,
      ),
      healthyAccounts: accounts.filter(
        (account) => account.health_tier === "healthy",
      ).length,
      warmAccounts: accounts.filter((account) => account.health_tier === "warm")
        .length,
      riskyAccounts: accounts.filter(
        (account) => account.health_tier === "risky",
      ).length,
    };
  }, [accounts]);
  const {
    totalAccounts,
    normalAccounts,
    rateLimitedAccounts,
    rateLimited5hAccounts,
    rateLimited7dAccounts,
    abnormalAccounts,
    bannedAccounts,
    errorAccounts,
    unsampledAccounts,
    disabledAccounts,
    lockedAccounts,
    subscriptionAccountsToLock,
    healthyAccounts,
    warmAccounts,
    riskyAccounts,
  } = accountSummary;

  const allTags = useMemo(() => {
    const tags = new Set<string>();
    for (const account of accounts) {
      for (const tag of account.tags ?? []) {
        tags.add(tag);
      }
    }
    return Array.from(tags).sort();
  }, [accounts]);

  const emailDomainStats = useMemo(() => {
    const byDomain = new Map<string, EmailDomainStat>();
    for (const account of accounts) {
      const domain = getAccountEmailDomain(account);
      if (!domain) continue;
      const stat = byDomain.get(domain) ?? { domain, total: 0, banned: 0 };
      stat.total += 1;
      if (account.status === "unauthorized") {
        stat.banned += 1;
      }
      byDomain.set(domain, stat);
    }
    return Array.from(byDomain.values()).sort((a, b) => {
      if (b.banned !== a.banned) return b.banned - a.banned;
      if (b.total !== a.total) return b.total - a.total;
      return a.domain.localeCompare(b.domain);
    });
  }, [accounts]);

  const filteredAccounts = useMemo(() => {
    const query = searchQuery.toLowerCase();
    return accounts.filter((account) => {
      switch (statusFilter) {
        case "normal":
          if (
            account.status === "unauthorized" ||
            account.status === "error" ||
            isRateLimitedAccount(account)
          )
            return false;
          break;
        case "rate_limited":
          if (
            account.status === "unauthorized" ||
            account.status === "error"
          )
            return false;
          if (!isRateLimitedAccount(account)) return false;
          break;
        case "abnormal":
          if (
            account.status !== "unauthorized" &&
            account.status !== "error"
          )
            return false;
          break;
        case "banned":
          if (account.status !== "unauthorized") return false;
          break;
        case "error":
          if (account.status !== "error") return false;
          break;
        case "unsampled":
          if (!isUnsampledQuotaAccount(account)) return false;
          break;
        case "disabled":
          if (account.enabled !== false) return false;
          break;
        case "locked":
          if (!account.locked) return false;
          break;
      }
      if (planFilter !== "all") {
        const plan = (account.plan_type || "").toLowerCase().trim();
        if (plan !== planFilter) return false;
      }
      if (query) {
        const email = (account.email || "").toLowerCase();
        const name = (account.name || "").toLowerCase();
        const domain = getAccountEmailDomain(account);
        if (!email.includes(query) && !name.includes(query) && !domain.includes(query))
          return false;
      }
      if (tagFilter && !(account.tags ?? []).includes(tagFilter)) return false;
      if (domainFilter && getAccountEmailDomain(account) !== domainFilter) return false;
      if (!accountMatchesGroupFilter(account.group_ids ?? [], groupFilter))
        return false;
      return true;
    });
  }, [accounts, domainFilter, groupFilter, planFilter, searchQuery, statusFilter, tagFilter]);

  const sortedAccounts = useMemo(() => {
    if (!sortKey) return filteredAccounts;
    return [...filteredAccounts].sort((a, b) => {
      let diff = 0;
      if (sortKey === "requests") {
        diff =
          (a.success_requests ?? 0) +
          (a.error_requests ?? 0) -
          ((b.success_requests ?? 0) + (b.error_requests ?? 0));
      } else if (sortKey === "usage") {
        diff = (a.usage_percent_7d ?? -1) - (b.usage_percent_7d ?? -1);
      } else if (sortKey === "importTime") {
        diff =
          new Date(a.created_at || 0).getTime() -
          new Date(b.created_at || 0).getTime();
      }
      return sortDir === "asc" ? diff : -diff;
    });
  }, [filteredAccounts, sortDir, sortKey]);

  const totalPages = Math.max(1, Math.ceil(sortedAccounts.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pagedAccounts = useMemo(
    () =>
      sortedAccounts.slice(
        (currentPage - 1) * pageSize,
        currentPage * pageSize,
      ),
    [currentPage, pageSize, sortedAccounts],
  );
  const pagedAccountIds = useMemo(
    () => pagedAccounts.map((account) => account.id),
    [pagedAccounts],
  );
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
    if (!accounts.some((account) => account.status === "refreshing")) {
      return;
    }

    const timer = window.setTimeout(() => {
      void reloadSilently();
    }, 2000);

    return () => window.clearTimeout(timer);
  }, [accounts, reloadSilently]);

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
            custom_headers: parsedCustomHeaders.value,
          }
        : {
            ...addForm,
            session_token: "",
            allow_duplicate: allowDuplicate,
            custom_headers: parsedCustomHeaders.value,
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
        return;
      }

      await api.addAccount(payload);
      showToast(t("accounts.addSuccess"));
      setShowAdd(false);
      setAddForm({ refresh_token: "", session_token: "", proxy_url: "" });
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
        custom_headers: parsedCustomHeaders.value,
      });
      setShowAdd(false);
      await readImportSSE(res);
      showToast(t("accounts.addSuccess"));
      setAtForm({ access_token: "", proxy_url: "" });
      setAddCustomHeadersText("");
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
      await importFiles([file], "json", sessionProxyUrl, addCustomHeadersText);
      setShowAdd(false);
      setSessionJson("");
      setAddCustomHeadersText("");
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
        models: [],
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

  const readImportSSE = async (res: Response) => {
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
    const reader = res.body?.getReader();
    if (!reader) {
      setImportProgress((p) => ({ ...p, done: true }));
      return;
    }
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
          const event = JSON.parse(line.slice(6)) as {
            type: string;
            current: number;
            total: number;
            success: number;
            updated: number;
            duplicate: number;
            failed: number;
          };
          setImportProgress((p) => ({
            ...p,
            current: event.current,
            total: event.total,
            success: event.success,
            updated: event.updated ?? 0,
            duplicate: event.duplicate,
            failed: event.failed,
            done: event.type === "complete",
          }));
          if (event.type === "complete") void reload();
        } catch {
          /* 忽略解析异常 */
        }
      }
    }
  };

  const importFiles = async (
    files: File[],
    format: "txt" | "json" | "json_at" | "at_txt",
    proxyOverride?: string,
    customHeadersText?: string,
  ) => {
    const parsedCustomHeaders = parseCustomHeadersText(customHeadersText ?? "");
    if (!parsedCustomHeaders.ok) {
      showToast("自定义请求头必须是 JSON 对象，且所有值必须是字符串", "error");
      return;
    }
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
    try {
      const formData = new FormData();
      if (format !== "txt") formData.append("format", format);
      const trimmedImportProxy = (proxyOverride ?? importProxyUrl).trim();
      if (trimmedImportProxy) formData.append("proxy_url", trimmedImportProxy);
      if (parsedCustomHeaders.value) {
        formData.append(
          "custom_headers",
          JSON.stringify(parsedCustomHeaders.value),
        );
      }
      if (allowDuplicate) formData.append("allow_duplicate", "true");
      for (const f of files) formData.append("file", f);
      const res = await fetch("/api/admin/accounts/import", {
        method: "POST",
        body: formData,
        headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
      });
      if (res.headers.get("content-type")?.includes("text/event-stream")) {
        await readImportSSE(res);
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
        } else {
          setImportProgress({
            show: true,
            current: data.total ?? 0,
            total: data.total ?? 0,
            success: data.success ?? 0,
            updated: data.updated ?? 0,
            duplicate: data.duplicate ?? 0,
            failed: data.failed ?? 0,
            done: true,
          });
          showToast(t("accounts.importCompleted"));
          void reload();
        }
      }
    } catch (error) {
      setImportProgress({
        show: true,
        current: 1,
        total: 1,
        success: 0,
        updated: 0,
        duplicate: 0,
        failed: 1,
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
    await importFiles(files, "txt", undefined, importCustomHeadersText);
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
    await importFiles(files, "at_txt", undefined, importCustomHeadersText);
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
      await importFiles(jsonFiles, "json", undefined, importCustomHeadersText);
    }
    if (txtFiles.length > 0) {
      await importFiles(txtFiles, "txt", undefined, importCustomHeadersText);
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
    await importFiles([file], "json", undefined, importCustomHeadersText);
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
      const params: { filter: "healthy" | "all"; ids?: number[] } = {
        filter: scope === "healthy" ? "healthy" : "all",
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
        downloadBlob(blob, `codex2api-${ts}-${data.length}.json`);
      } else {
        const text = data.map((e) => e.refresh_token).join("\n");
        const blob = new Blob([text], { type: "text/plain" });
        downloadBlob(blob, `codex2api-rt-${ts}-${data.length}.txt`);
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
      void reload();
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
      void reloadSilently();
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
      void reload();
    } catch (error) {
      showToast(
        t("accounts.lockFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleLockSubscriptionAccounts = async () => {
    const candidates = subscriptionAccountsToLock;
    if (candidates.length === 0) {
      showToast(t("accounts.noSubscriptionAccountsToLock"));
      return;
    }

    setBatchLoading(true);
    setLockingSubscriptionAccounts(true);
    try {
      const result = await api.batchUpdateAccounts({
        ids: candidates.map((account) => account.id),
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
      void reload();
    } catch (error) {
      showToast(
        t("accounts.enableFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
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
      void reload();
    } catch (error) {
      showToast(
        t("accounts.batchDeleteFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
    }
  };

  const handleBatchRefresh = async (ids?: number[]) => {
    const targetIds = ids ?? Array.from(selected);
    if (targetIds.length === 0) return;
    setBatchLoading(true);
    setBatchRefreshing(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/batch-refresh?stream=true",
        { ids: targetIds },
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
      showToast(t("accounts.resetStatusSuccess"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.resetStatusFailed", { error: getErrorMessage(error) }),
        "error",
      );
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
    const selectedAccounts = accounts.filter((account) =>
      selected.has(account.id),
    );
    const tagSet = new Set<string>();
    const groupSet = new Set<number>();
    for (const account of selectedAccounts) {
      for (const tag of account.tags ?? []) tagSet.add(tag);
      for (const id of account.group_ids ?? []) groupSet.add(id);
    }
    setBatchTags(Array.from(tagSet).sort());
    setBatchGroupIds(Array.from(groupSet).sort((a, b) => a - b));
    setShowBatchMetaEditor(true);
  };

  const handleBatchSaveMeta = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchMetaSubmitting(true);
    try {
      const result = await api.batchUpdateAccounts({
        ids,
        tags: batchTags,
        group_ids: batchGroupIds,
      });
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
    setBatchTesting(true);
    try {
      const result = await runStreamingAccountOperation(
        "/accounts/batch-test?stream=true",
        ids ? { ids } : undefined,
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
      await api.cleanBanned();
      showToast(t("accounts.cleanBannedSuccess"));
      void reload();
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
      await api.cleanRateLimited();
      showToast(t("accounts.cleanRateLimitedSuccess"));
      void reload();
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
      await api.cleanError();
      showToast(t("accounts.cleanErrorSuccess"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.cleanErrorFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningError(false);
    }
  };

  const openSchedulerEditor = (account: AccountRow) => {
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
    setEditDispatchCountLimitInput(
      formatDispatchCountLimitInput(account.dispatch_count_limit),
    );
    setAllowedAPIKeySelection(
      filterExistingAPIKeyIDs(account.allowed_api_key_ids ?? [], apiKeys),
    );
    setEditProxyUrl(account.proxy_url ?? "");
    setEditCustomHeadersText(formatCustomHeadersText(account.custom_headers));
    setEditTags(account.tags ?? []);
    setEditGroupIds(account.group_ids ?? []);
    setEditOpenAIForm({
      name: account.name ?? "",
      base_url: account.base_url || "https://api.openai.com",
      api_key: "",
      models: account.models ?? [],
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
    setEditDispatchCountLimitInput("");
    setAllowedAPIKeySelection([]);
    setEditProxyUrl("");
    setEditCustomHeadersText("");
    setEditTags([]);
    setEditGroupIds([]);
    setEditOpenAIForm({
      name: "",
      base_url: "https://api.openai.com",
      api_key: "",
      models: [],
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
    (parsedBaseConcurrency === null ||
      parsedBaseConcurrency < 1 ||
      parsedBaseConcurrency > 50);
  const editAutoPause5hThresholdInvalid = isPercentThresholdInputInvalid(
    editAutoPause5hThresholdInput,
  );
  const editAutoPause7dThresholdInvalid = isPercentThresholdInputInvalid(
    editAutoPause7dThresholdInput,
  );
  const editDispatchCountLimitInvalid = isDispatchCountLimitInputInvalid(
    editDispatchCountLimitInput,
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
      editDispatchCountLimitInvalid
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
        dispatch_count_limit: dispatchCountLimitInputToValue(
          editDispatchCountLimitInput,
        ),
        custom_headers: parsedCustomHeaders.value,
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

  const resetGroupDraft = () => {
    setGroupDraft({
      id: null,
      name: "",
      description: "",
      color: ACCOUNT_GROUP_COLORS[0],
      auto_pause_5h_threshold: 0,
      auto_pause_7d_threshold: 0,
    });
  };

  const startEditGroup = (group: AccountGroup) => {
    setGroupDraft({
      id: group.id,
      name: group.name,
      description: group.description ?? "",
      color: group.color || ACCOUNT_GROUP_COLORS[0],
      auto_pause_5h_threshold: group.auto_pause_5h_threshold ?? 0,
      auto_pause_7d_threshold: group.auto_pause_7d_threshold ?? 0,
    });
  };

  const handleSaveGroup = async () => {
    const name = groupDraft.name.trim();
    if (!name) {
      showToast(t("accounts.groupNameRequired"), "error");
      return;
    }
    setGroupSubmitting(true);
    try {
      const payload = {
        name,
        description: groupDraft.description.trim(),
        color: groupDraft.color.trim() || ACCOUNT_GROUP_COLORS[0],
        auto_pause_5h_threshold: groupDraft.auto_pause_5h_threshold,
        auto_pause_7d_threshold: groupDraft.auto_pause_7d_threshold,
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

  return (
    <div
      className="relative @container/accounts"
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
      <StateShell
        variant="page"
        loading={loading}
        error={error}
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
              onClose={() => setShowInvite(false)}
            />
          ) : null}
          <div className={showRecycleBin || showInvite ? "hidden" : "contents"}>
          <PageHeader
            title={t("accounts.title")}
            description={t("accounts.description")}
            onRefresh={() => void reload()}
            titleAdornment={
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
            }
            actions={
              <>
                {/* Desktop / tablet: full action strip */}
                <div className="hidden flex-wrap items-center justify-end gap-1.5 sm:flex">
                  <Button
                    variant="outline"
                    size="sm"
                    aria-pressed={showAnalysisCharts}
                    onClick={() => setShowAnalysisCharts((visible) => !visible)}
                  >
                    <BarChart3 className="size-3.5" />
                    {showAnalysisCharts
                      ? t("accounts.hideAnalysisCharts")
                      : t("accounts.showAnalysisCharts")}
                  </Button>
                  <HeaderActionMenu
                    label={t("accounts.maintenanceActions")}
                    icon={<Zap className="size-3.5" />}
                    items={[
                      {
                        key: "refresh-tokens",
                        label: t("accounts.refreshTokens"),
                        icon: (
                          <RefreshCw
                            className={`size-3.5 ${batchRefreshing ? "animate-spin" : ""}`}
                          />
                        ),
                        disabled:
                          batchLoading || batchTesting || accounts.length === 0,
                        onSelect: () =>
                          void handleBatchRefresh(
                            accounts.map((account) => account.id),
                          ),
                      },
                      {
                        key: "test-connection",
                        label: batchTesting
                          ? t("accounts.batchTesting")
                          : t("accounts.testConnection"),
                        icon: <FlaskConical className="size-3.5" />,
                        disabled:
                          batchLoading || batchTesting || accounts.length === 0,
                        onSelect: () => void handleBatchTest(),
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
                          accounts.length === 0,
                        title: t("accounts.lockSubscriptionAccountsHint", {
                          count: subscriptionAccountsToLock.length,
                        }),
                        onSelect: () => void handleLockSubscriptionAccounts(),
                      },
                    ]}
                  />
                  <HeaderActionMenu
                    label={t("accounts.cleanupActions")}
                    icon={<Trash2 className="size-3.5" />}
                    items={[
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
                    ]}
                  />
                  <HeaderActionMenu
                    label={t("accounts.dataActions")}
                    icon={<FolderOpen className="size-3.5" />}
                    items={[
                      {
                        key: "import",
                        label: importing
                          ? t("accounts.importing")
                          : t("accounts.importFile"),
                        icon: <Upload className="size-3.5" />,
                        disabled: importing,
                        onSelect: () => setShowImportPicker(true),
                      },
                      {
                        key: "export",
                        label: exporting
                          ? t("accounts.exporting")
                          : t("accounts.export"),
                        icon: <Download className="size-3.5" />,
                        disabled: exporting,
                        onSelect: () => setShowExportPicker(true),
                      },
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
                    ]}
                  />
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setShowRecycleBin(true)}
                  >
                    <Recycle className="size-3.5" />
                    {t("accounts.recycleBin")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setShowInvite(true)}
                  >
                    <Mail className="size-3.5" />
                    {t("invite.entry")}
                  </Button>
                  <Button size="sm" onClick={() => setShowAdd(true)}>
                    <Plus className="size-3.5" />
                    {t("accounts.addAccount")}
                  </Button>
                </div>

                {/* Mobile: primary + overflow menu */}
                <div className="flex w-full items-center gap-1.5 sm:hidden">
                  <Button
                    size="sm"
                    className="min-w-0 flex-1"
                    onClick={() => setShowAdd(true)}
                  >
                    <Plus className="size-3.5" />
                    {t("accounts.addAccount")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    className="shrink-0"
                    aria-pressed={showAnalysisCharts}
                    onClick={() => setShowAnalysisCharts((visible) => !visible)}
                    title={
                      showAnalysisCharts
                        ? t("accounts.hideAnalysisCharts")
                        : t("accounts.showAnalysisCharts")
                    }
                  >
                    <BarChart3 className="size-3.5" />
                  </Button>
                  <HeaderActionMenu
                    label={t("common.more")}
                    icon={<MoreHorizontal className="size-3.5" />}
                    align="end"
                    compact
                    items={[
                      {
                        key: "refresh-tokens",
                        label: t("accounts.refreshTokens"),
                        icon: (
                          <RefreshCw
                            className={`size-3.5 ${batchRefreshing ? "animate-spin" : ""}`}
                          />
                        ),
                        disabled:
                          batchLoading || batchTesting || accounts.length === 0,
                        onSelect: () =>
                          void handleBatchRefresh(
                            accounts.map((account) => account.id),
                          ),
                      },
                      {
                        key: "test-connection",
                        label: batchTesting
                          ? t("accounts.batchTesting")
                          : t("accounts.testConnection"),
                        icon: <FlaskConical className="size-3.5" />,
                        disabled:
                          batchLoading || batchTesting || accounts.length === 0,
                        onSelect: () => void handleBatchTest(),
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
                          accounts.length === 0,
                        onSelect: () => void handleLockSubscriptionAccounts(),
                      },
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
                      {
                        key: "import",
                        label: importing
                          ? t("accounts.importing")
                          : t("accounts.importFile"),
                        icon: <Upload className="size-3.5" />,
                        disabled: importing,
                        onSelect: () => setShowImportPicker(true),
                      },
                      {
                        key: "export",
                        label: exporting
                          ? t("accounts.exporting")
                          : t("accounts.export"),
                        icon: <Download className="size-3.5" />,
                        disabled: exporting,
                        onSelect: () => setShowExportPicker(true),
                      },
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
                    ]}
                  />
                </div>

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

          <div className="mb-4 grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-4">
            <CompactStat
              label={t("accounts.totalAccounts")}
              chipLabel={t("accounts.filterAll")}
              value={totalAccounts}
              tone="neutral"
            />
            <CompactStat
              label={t("accounts.normalAccounts")}
              chipLabel={t("accounts.filterNormal")}
              value={normalAccounts}
              tone="success"
            />
            <CompactStat
              label={t("accounts.rateLimited")}
              chipLabel={t("accounts.filterRateLimited")}
              value={rateLimitedAccounts}
              tone="warning"
              details={[
                { label: "5h", value: rateLimited5hAccounts },
                { label: "7d", value: rateLimited7dAccounts },
              ]}
            />
            <CompactStat
              label={t("accounts.abnormalAccounts")}
              chipLabel={t("accounts.filterAbnormal")}
              value={abnormalAccounts}
              tone="danger"
              details={[
                { label: t("accounts.abnormalBannedShort"), value: bannedAccounts },
                { label: t("accounts.abnormalErrorShort"), value: errorAccounts },
              ]}
            />
          </div>

          {showAnalysisCharts ? (
            <div className="mb-4 grid items-stretch gap-4 xl:grid-cols-2">
              <AccountQuotaDistributionChart
                accounts={accounts}
                compact
                className="min-w-0"
                onProbeStarted={() => {
                  showToast(t('accounts.quotaDistributionRefreshStarted'), 'success')
                  // 探针在后台并发执行；稍等一下再静默拉取，让首批结果有机会回流
                  window.setTimeout(() => {
                    void reloadSilently()
                  }, 4000)
                }}
                onProbeError={(message) => showToast(message, 'error')}
              />
              <AccountRateLimitRecoveryChart
                accounts={accounts}
                currentRpm={opsOverview?.traffic?.rpm}
                rpmLimit={opsOverview?.traffic?.rpm_limit}
                avgDurationMs={opsOverview?.traffic?.avg_duration_ms}
                compact
                className="min-w-0"
              />
            </div>
          ) : null}

          <div className="mb-3 grid gap-2 sm:gap-3 @min-[1600px]/accounts:grid-cols-[minmax(0,1fr)_max-content]">
            <div className="toolbar-surface flex items-center gap-1.5 overflow-x-auto overflow-y-visible [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden @min-[1600px]/accounts:flex-nowrap">
              <span className="shrink-0 whitespace-nowrap text-[12px] font-semibold text-foreground">
                {t("accounts.filter")}
              </span>
              {(
                [
                  ["all", t("accounts.filterAll")],
                  ["normal", t("accounts.filterNormal")],
                  ["rate_limited", t("accounts.filterRateLimited")],
                  ["abnormal", t("accounts.filterAbnormal")],
                  ["banned", t("accounts.filterBanned")],
                  ["error", t("accounts.filterError")],
                  ["unsampled", t("accounts.filterUnsampled")],
                  ["disabled", t("accounts.filterDisabled")],
                  ["locked", t("accounts.filterLocked")],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key}
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
                  {label}{" "}
                  {key === "all"
                    ? totalAccounts
                    : key === "normal"
                      ? normalAccounts
                      : key === "rate_limited"
                        ? rateLimitedAccounts
                        : key === "abnormal"
                          ? abnormalAccounts
                          : key === "banned"
                            ? bannedAccounts
                            : key === "error"
                              ? errorAccounts
                              : key === "unsampled"
                                ? unsampledAccounts
                                : key === "disabled"
                                  ? disabledAccounts
                                  : lockedAccounts}
                </button>
              ))}
            </div>

            <div className="toolbar-surface flex items-center gap-1.5 overflow-x-auto overflow-y-visible [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden @min-[1600px]/accounts:flex-nowrap">
              <span className="shrink-0 whitespace-nowrap text-[12px] font-semibold text-foreground">
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
          </div>

          <div className="mb-4 flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
            <div className="relative w-full shrink-0 sm:w-64">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                className="h-9 rounded-lg pl-9 text-[13px] sm:h-8"
                placeholder={t("accounts.searchPlaceholder")}
                value={searchQuery}
                onChange={(e: ChangeEvent<HTMLInputElement>) => {
                  setSearchQuery(e.target.value);
                  setPage(1);
                }}
              />
            </div>
            <div className="flex max-w-full shrink-0 items-center gap-1 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
              {(
                ["all", "pro", "prolite", "plus", "team", "k12", "free"] as const
              ).map(
                (key) => (
                  <button
                    key={key}
                    onClick={() => {
                      setPlanFilter(key);
                      setPage(1);
                    }}
                    className={`shrink-0 whitespace-nowrap rounded-md px-2.5 py-1.5 text-[12px] font-medium transition-colors ${
                      planFilter === key
                        ? "bg-background shadow-sm text-foreground"
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
                ),
              )}
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
              className="w-full min-w-0 sm:w-52 lg:w-64"
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
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="min-w-0"
              aria-pressed={showEmailDomainTags}
              onClick={() => setShowEmailDomainTags((visible) => !visible)}
            >
              {showEmailDomainTags ? (
                <EyeOff className="size-3.5" />
              ) : (
                <Eye className="size-3.5" />
              )}
              <span className="truncate">
                {showEmailDomainTags
                  ? t("accounts.hideEmailDomainTags")
                  : t("accounts.showEmailDomainTags")}
              </span>
            </Button>
            <AccountGroupFilterSelect
              className="w-full min-w-0 sm:w-40"
              groups={allGroups}
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
              className="min-w-0"
              onClick={() => setShowGroupManager(true)}
            >
              <FolderOpen className="size-3.5" />
              <span className="truncate">{t("accounts.groupManage")}</span>
            </Button>
            </div>
            {!isPersonalMode && (
            <div className="flex w-full shrink-0 items-center gap-1.5 @min-[1600px]/accounts:ml-auto @min-[1600px]/accounts:w-auto">
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
                  status: t("accounts.status"),
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

          {selected.size > 0 && (
            <div className="sticky top-2 z-20 mb-4 flex items-center justify-between gap-3 rounded-lg border border-primary/20 bg-card/95 px-3 py-2.5 text-sm font-semibold text-primary shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
              <span>{t("common.selected", { count: selected.size })}</span>
              <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchRefresh()}
                >
                  <RefreshCw
                    className={`size-3 mr-1 ${batchRefreshing ? "animate-spin" : ""}`}
                  />
                  {t("accounts.batchRefresh")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchTest(Array.from(selected))}
                >
                  <FlaskConical className="size-3 mr-1" />
                  {batchTesting
                    ? t("accounts.batchTesting")
                    : t("accounts.batchTest")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchEnabled(true)}
                >
                  <Power className="size-3 mr-1" />
                  {t("accounts.enable")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchEnabled(false)}
                >
                  <PowerOff className="size-3 mr-1" />
                  {t("accounts.disable")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchLock(true)}
                >
                  <Lock className="size-3 mr-1" />
                  {t("accounts.lock")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchLock(false)}
                >
                  <Unlock className="size-3 mr-1" />
                  {t("accounts.unlock")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={openBatchMetaEditor}
                >
                  <FolderOpen className="size-3 mr-1" />
                  {t("accounts.batchMetaEdit")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={openBatchQuotaAutoPauseEditor}
                >
                  <Hourglass className="size-3 mr-1" />
                  {t("accounts.batchAutoPauseEdit")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchResetStatus()}
                >
                  <RotateCcw className="size-3 mr-1" />
                  {t("accounts.batchResetStatus")}
                </Button>
                <Button
                  variant="destructive"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchDelete()}
                >
                  {t("accounts.batchDelete")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setSelected(new Set())}
                >
                  {t("accounts.cancelSelection")}
                </Button>
              </div>
            </div>
          )}

          <Card>
            <CardContent className="p-3 sm:p-4">
              <StateShell
                variant="section"
                isEmpty={accounts.length === 0}
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
                    {pagedAccounts.map((account, index) => {
                      const isSelected = selected.has(account.id);
                      return (
                        <AccountMobileCard
                          key={account.id}
                          account={account}
                          sequence={(currentPage - 1) * pageSize + index + 1}
                          selected={isSelected}
                          allGroups={allGroups}
                          lazyMode={lazyMode}
                          showEmailDomainTags={showEmailDomainTags}
                          healthBuckets={healthBars[String(account.id)]}
                          refreshing={refreshingIds.has(account.id)}
                          authJsonExporting={authJsonExportingIds.has(account.id)}
                          variant={isPersonalMode ? "personal" : "mobile"}
                          t={t}
                          onToggleSelect={() => toggleSelect(account.id)}
                          onEdit={() => openSchedulerEditor(account)}
                          onUsage={() => setUsageAccount(account)}
                          onTest={() => setTestingAccount(account)}
                          onRefresh={() => void handleRefresh(account)}
                          onGenerateAuthJson={() =>
                            void handleGenerateAuthJSON(account)
                          }
                          onToggleEnabled={() =>
                            void handleToggleEnabled(account)
                          }
                          onToggleLock={() => void handleToggleLock(account)}
                          onResetStatus={() => void handleResetStatus(account)}
                          onResetCredits={() =>
                            void handleResetCredits(account)
                          }
                          onDelete={() => void handleDelete(account)}
                          onUsageRefreshed={() => void reloadSilently()}
                        />
                      );
                    })}
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
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.groupsLabel")}
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
                        {visibleColumns.requests && (
                          <TableHead
                            className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                            onClick={() => {
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
                      {pagedAccounts.map((account, index) => {
                        const isSelected = selected.has(account.id);
                        return (
                          <TableRow
                            key={account.id}
                            className={isSelected ? "bg-primary/5" : ""}
                          >
                            <TableCell>
                              <input
                                type="checkbox"
                                className="size-4 cursor-pointer accent-primary"
                                checked={isSelected}
                                onChange={() => toggleSelect(account.id)}
                              />
                            </TableCell>
                            {visibleColumns.sequence && (
                              <TableCell
                                className="text-[14px] font-mono text-muted-foreground"
                                title={`ID ${account.id}`}
                              >
                                {(currentPage - 1) * pageSize + index + 1}
                              </TableCell>
                            )}
                            {visibleColumns.email && (
                              <TableCell className="min-w-[220px] whitespace-normal text-[14px] text-muted-foreground">
                                <div className="flex min-w-0 flex-col items-start gap-1">
                                  <span className="break-all">
                                    {account.openai_responses_api
                                      ? formatAccountName(account)
                                      : formatAccountListEmail(account)}
                                  </span>
                                  {account.chatgpt_account_id && (
                                    <span
                                      className="max-w-full truncate font-mono text-[10px] leading-tight text-muted-foreground/70"
                                      title={account.chatgpt_account_id}
                                    >
                                      {account.chatgpt_account_id}
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
                                    account.enabled === false ||
                                    account.locked ||
                                    (account.rate_limit_reset_credits ?? 0) >
                                      0) && (
                                    <div className="flex flex-wrap gap-1">
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
                                      {account.enabled === false && (
                                        <span className="inline-flex items-center rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium text-zinc-700 ring-1 ring-inset ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-300 dark:ring-zinc-400/20">
                                          <PowerOff className="mr-0.5 size-2.5" />
                                          {t("accounts.disabled")}
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
                                            setUsageAccount(account);
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
                                    </div>
                                  )}
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
                                />
                              </TableCell>
                            )}
                            {visibleColumns.plan && (
                              <TableCell>
                                <div className="flex flex-wrap items-center gap-1.5">
                                  <PlanBadge planType={account.plan_type} />
                                  <ExpiryBadge
                                    expiresAt={account.subscription_expires_at}
                                    planType={account.plan_type}
                                  />
                                </div>
                              </TableCell>
                            )}
                            {visibleColumns.status && (
                              <TableCell>
                                <div className="space-y-1.5">
                                  <div className="flex min-h-6 items-center gap-2 whitespace-nowrap">
                                    <StatusBadge
                                      status={account.status}
                                      detail={
                                        getAccountRateLimitWindow(account) ??
                                        undefined
                                      }
                                      errorMessage={account.error_message}
                                    />
                                    <AccountStatusCountdown account={account} />
                                    {(account.active_requests ?? 0) > 0 && (
                                      <span
                                        className="inline-flex items-center gap-1 rounded-md bg-blue-50 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-blue-600 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20"
                                        title={t("accounts.activeRequestsTooltip", {
                                          count: account.active_requests ?? 0,
                                        })}
                                      >
                                        <span
                                          className="size-1.5 animate-pulse rounded-full bg-blue-500 dark:bg-blue-400"
                                          aria-hidden
                                        />
                                        {account.active_requests}
                                      </span>
                                    )}
                                  </div>
                                  {account.status === "error" &&
                                    account.error_message && (
                                      <div
                                        className="max-w-[180px] truncate text-[11px] leading-tight text-red-500"
                                        title={account.error_message}
                                      >
                                        {account.error_message}
                                      </div>
                                    )}
                                  {(account.model_cooldowns?.length ?? 0) >
                                    0 && (
                                    <div className="text-[11px] leading-tight text-amber-600">
                                      model{" "}
                                      {account.model_cooldowns?.[0]?.model}
                                      {(account.model_cooldowns?.length ?? 0) >
                                      1
                                        ? ` +${(account.model_cooldowns?.length ?? 1) - 1}`
                                        : ""}
                                    </div>
                                  )}
                                  <div className="text-[11px] text-muted-foreground">
                                    {t("accounts.healthSummary", {
                                      health: formatHealthTier(
                                        account.health_tier,
                                        t,
                                      ),
                                      score: Math.round(
                                        getDispatchScore(account),
                                      ),
                                      concurrency:
                                        account.dynamic_concurrency_limit ??
                                        "-",
                                    })}
                                  </div>
                                  <div className="space-y-0.5 pt-0.5">
                                    <div className="text-[10px] text-muted-foreground/70">
                                      {t("accounts.healthBarLabel")}
                                    </div>
                                    <AccountHealthBar
                                      buckets={healthBars[String(account.id)]}
                                    />
                                  </div>
                                </div>
                              </TableCell>
                            )}
                            {visibleColumns.requests && (
                              <TableCell>
                                <div className="space-y-0.5 text-[13px]">
                                  <div className="flex items-center gap-2">
                                    <span className="text-emerald-600 font-medium">
                                      {account.success_requests ?? 0}
                                    </span>
                                    <span className="text-muted-foreground">
                                      /
                                    </span>
                                    <span className="text-red-500 font-medium">
                                      {account.error_requests ?? 0}
                                    </span>
                                  </div>
                                  {((account.retry_error_requests ?? 0) > 0 ||
                                    (account.rate_limit_attempts ?? 0) > 0) && (
                                    <div className="text-[11px] text-muted-foreground">
                                      retry {account.retry_error_requests ?? 0}{" "}
                                      · 429 {account.rate_limit_attempts ?? 0}
                                    </div>
                                  )}
                                </div>
                              </TableCell>
                            )}
                            {visibleColumns.usage && (
                              <TableCell>
                                <UsageCell
                                  account={account}
                                  onRefreshed={() => void reloadSilently()}
                                />
                              </TableCell>
                            )}
                            {visibleColumns.billed && (
                              <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">
                                <BilledCell account={account} />
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
                                <div className="flex items-center gap-1 justify-end">
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => openSchedulerEditor(account)}
                                    title={t("accounts.editScheduler")}
                                  >
                                    <Pencil className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => setUsageAccount(account)}
                                    title={t("accounts.usageDetail")}
                                  >
                                    <BarChart3 className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => setTestingAccount(account)}
                                    title={t("accounts.testConnection")}
                                  >
                                    <Zap className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    disabled={
                                      refreshingIds.has(account.id) ||
                                      account.at_only ||
                                      account.openai_responses_api
                                    }
                                    onClick={() => void handleRefresh(account)}
                                    title={
                                      account.at_only ||
                                      account.openai_responses_api
                                        ? t("accounts.atRefreshDisabled")
                                        : t("accounts.refreshAccessToken")
                                    }
                                  >
                                    <RefreshCw
                                      className={`size-3.5 ${refreshingIds.has(account.id) ? "animate-spin" : ""}`}
                                    />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    disabled={
                                      authJsonExportingIds.has(account.id) ||
                                      account.at_only ||
                                      account.openai_responses_api
                                    }
                                    onClick={() =>
                                      void handleGenerateAuthJSON(account)
                                    }
                                    title={
                                      account.at_only ||
                                      account.openai_responses_api
                                        ? t("accounts.authJsonDisabled")
                                        : t("accounts.generateAuthJson")
                                    }
                                  >
                                    <FileJson className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant={
                                      account.enabled === false
                                        ? "default"
                                        : "outline"
                                    }
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() =>
                                      void handleToggleEnabled(account)
                                    }
                                    title={
                                      account.enabled === false
                                        ? t("accounts.enableHint")
                                        : t("accounts.disableHint")
                                    }
                                  >
                                    {account.enabled === false ? (
                                      <Power className="size-3.5" />
                                    ) : (
                                      <PowerOff className="size-3.5" />
                                    )}
                                  </Button>
                                  <Button
                                    variant={
                                      account.locked ? "default" : "outline"
                                    }
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() =>
                                      void handleToggleLock(account)
                                    }
                                    title={
                                      account.locked
                                        ? t("accounts.unlockHint")
                                        : t("accounts.lockHint")
                                    }
                                  >
                                    {account.locked ? (
                                      <Lock className="size-3.5" />
                                    ) : (
                                      <Unlock className="size-3.5" />
                                    )}
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() =>
                                      void handleResetStatus(account)
                                    }
                                    title={t("accounts.resetStatusHint")}
                                  >
                                    <RotateCcw className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    disabled={
                                      (account.rate_limit_reset_credits ?? 0) <=
                                      0
                                    }
                                    onClick={() =>
                                      void handleResetCredits(account)
                                    }
                                    title={t("accounts.resetCreditsButton")}
                                  >
                                    <Timer className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="destructive"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => void handleDelete(account)}
                                    title={t("accounts.deleteAccount")}
                                  >
                                    <Trash2 className="size-3.5" />
                                  </Button>
                                </div>
                              </TableCell>
                            )}
                          </TableRow>
                        );
                      })}
                    </TableBody>
                  </Table>
                  </div>
                ) : null}
                <Pagination
                  page={currentPage}
                  totalPages={totalPages}
                  onPageChange={setPage}
                  totalItems={sortedAccounts.length}
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
            <div className="grid grid-cols-3 sm:grid-cols-6 gap-1 p-1 mb-5 rounded-xl bg-muted/50 border border-border">
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
                  testKey: "add-refresh-token",
                  onChange: (value) =>
                    setAddForm((form) => ({
                      ...form,
                      proxy_url: value,
                    })),
                })}
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
                  testKey: "add-session-token",
                  onChange: (value) =>
                    setAddForm((form) => ({
                      ...form,
                      proxy_url: value,
                    })),
                })}
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
                  testKey: "add-access-token",
                  onChange: (value) =>
                    setAtForm((form) => ({
                      ...form,
                      proxy_url: value,
                    })),
                })}
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
                  testKey: "add-session-json",
                  label: t("accounts.importProxyLabel"),
                  onChange: setSessionProxyUrl,
                })}
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
                  <p className="mt-1.5 text-xs text-muted-foreground">
                    {t("accounts.openaiModelsHint", {
                      count: openAIForm.models.length,
                    })}
                  </p>
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
                  testKey: "add-openai-responses",
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
                      testKey: "oauth-generate",
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
                testKey: "import-batch",
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
              <label className="flex cursor-pointer items-center gap-2 pt-1 text-xs text-muted-foreground">
                <input
                  type="checkbox"
                  className="size-3.5"
                  checked={allowDuplicate}
                  onChange={(e) => setAllowDuplicate(e.target.checked)}
                />
                {t("accounts.allowDuplicate")}
              </label>
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
              onClose={() => setUsageAccount(null)}
              onCreditsReset={() => void reload()}
            />
          )}

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
                          editDispatchCountLimitInvalid)) ||
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
              <div className="space-y-5">
                <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                  <div className="font-semibold text-foreground">
                    {formatAccountName(editingAccount)}
                  </div>
                  <div className="mt-1">
                    {t("accounts.schedulerEditDesc", {
                      plan: editingAccount.plan_type || "-",
                    })}
                  </div>
                </div>

                {(editingAccount.openai_responses_api ||
                  isOAuthAccount(editingAccount)) && (
                  <div className="flex gap-1 rounded-xl border border-border bg-muted/50 p-1">
                    <button
                      type="button"
                      onClick={() => setEditTab("scheduler")}
                      className={`flex-1 rounded-lg px-3 py-2 text-sm font-semibold transition-all ${
                        editTab === "scheduler"
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      {t("accounts.editTabScheduler")}
                    </button>
                    <button
                      type="button"
                      onClick={() => setEditTab("account")}
                      className={`flex-1 rounded-lg px-3 py-2 text-sm font-semibold transition-all ${
                        editTab === "account"
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      {t("accounts.editTabAccount")}
                    </button>
                  </div>
                )}

                {editTab === "account" &&
                editingAccount.openai_responses_api ? (
                  <div className="space-y-4">
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
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
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
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
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
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
                      <div className="mb-2 flex items-center justify-between gap-2">
                        <label className="text-sm font-semibold text-muted-foreground">
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
                      <p className="mt-1.5 text-xs text-muted-foreground">
                        {t("accounts.openaiModelsHint", {
                          count: editOpenAIForm.models.length,
                        })}
                      </p>
                    </div>
                    {renderModelMappingEditor({
                      value: editOpenAIModelMappingText,
                      onChange: setEditOpenAIModelMappingText,
                      mode: editOpenAIModelMappingMode,
                      onModeChange: setEditOpenAIModelMappingMode,
                      entries: editOpenAIModelMappingEntries,
                      onEntriesChange: setEditOpenAIModelMappingEntries,
                    })}
                    {renderProxyInput({
                      value: editOpenAIForm.proxy_url,
                      testKey: "edit-openai-responses",
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
                ) : editTab === "account" && isOAuthAccount(editingAccount) ? (
                  <div className="space-y-4">
                    <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                      <p className="font-semibold text-foreground mb-1">
                        {t("accounts.oauthEditIntroTitle")}
                      </p>
                      <p>{t("accounts.oauthEditIntroDesc")}</p>
                    </div>
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.oauthCurrentAccount")}
                      </label>
                      <div className="rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                        {formatAccountName(editingAccount)}
                      </div>
                    </div>
                    {editOAuthStep === "generate" ? (
                      <>
                        <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                          <p className="font-semibold text-foreground mb-1">
                            {t("accounts.oauthStep1Title")}
                          </p>
                          <p>{t("accounts.oauthStep1Desc")}</p>
                        </div>
                        {renderProxyInput({
                          value: editOAuthProxyUrl,
                          testKey: "edit-oauth-generate",
                          label: t("accounts.oauthProxyUrl"),
                          placeholder: t("accounts.oauthProxyUrlPlaceholder"),
                          onChange: setEditOAuthProxyUrl,
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
                        {editOAuthSession && (
                          <div className="rounded-xl border border-primary/30 bg-primary/5 px-4 py-3">
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
                          <label className="block mb-2 text-sm font-semibold text-muted-foreground">
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
                  <>
                    <div className="grid gap-4 md:grid-cols-2">
                      <div className="rounded-xl border border-border p-4">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.schedulerScoreLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.schedulerScoreHint")}
                        </div>
                        <div className="mt-3 flex gap-2">
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
                          <div className="mt-3 rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                            {t("accounts.schedulerScoreAutoValue", {
                              value: formatSignedNumber(
                                getDefaultScoreBias(editingAccount.plan_type),
                              ),
                            })}
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

                      <div className="rounded-xl border border-border p-4">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.schedulerConcurrencyLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.schedulerConcurrencyHint")}
                        </div>
                        <div className="mt-3 flex gap-2">
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
                          <div className="mt-3 rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                            {t("accounts.schedulerConcurrencyAutoValue", {
                              value:
                                getEffectiveBaseConcurrency(editingAccount),
                            })}
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

                      <div className="rounded-xl border border-border p-4">
                        <div className="flex items-start justify-between gap-4">
                          <div>
                            <div className="text-sm font-semibold text-foreground">
                              {t("accounts.schedulerSkipWarmLabel")}
                            </div>
                            <div className="mt-1 text-xs text-muted-foreground">
                              {t("accounts.schedulerSkipWarmHint")}
                            </div>
                          </div>
                          <button
                            type="button"
                            role="switch"
                            aria-label={t("accounts.schedulerSkipWarmLabel")}
                            aria-checked={skipWarmTier}
                            onClick={() =>
                              setSkipWarmTier((current) => !current)
                            }
                            className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60 focus-visible:ring-offset-2 ${skipWarmTier ? "bg-primary" : "bg-muted"}`}
                          >
                            <span
                              className={`pointer-events-none block size-4 rounded-full bg-white shadow transition-transform ${skipWarmTier ? "translate-x-4" : "translate-x-0"}`}
                            />
                          </button>
                        </div>
                      </div>

                      <div className="rounded-xl border border-border p-4 md:col-span-2">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.dispatchCountLimitTitle")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.dispatchCountLimitHint")}
                        </div>
                        <div className="mt-3">
                          <label className="mb-1.5 block text-xs font-semibold text-muted-foreground">
                            {t("accounts.dispatchCountLimitLabel")}
                          </label>
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

                      <div className="rounded-xl border border-border p-4 md:col-span-2">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.autoPauseTitle")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.autoPauseHint")}
                        </div>
                        <div className="mt-4 grid gap-4 md:grid-cols-2">
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

                      <div className="rounded-xl border border-border p-4">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.allowedAPIKeysLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.allowedAPIKeysHint")}
                        </div>
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

                      <div className="rounded-xl border border-border p-4">
                        {renderProxyInput({
                          value: editProxyUrl,
                          testKey: "edit-account-proxy",
                          onChange: setEditProxyUrl,
                        })}
                      </div>

                      <div className="rounded-xl border border-border p-4 md:col-span-2">
                        {renderCustomHeadersTextarea({
                          value: editCustomHeadersText,
                          onChange: setEditCustomHeadersText,
                        })}
                      </div>
                    </div>

                    <div className="grid gap-4 md:grid-cols-2">
                      <div className="rounded-xl border border-border p-4">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.tagsLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.tagsHint")}
                        </div>
                        <ChipInput
                          className="mt-3"
                          value={editTags}
                          onChange={setEditTags}
                          placeholder={t("accounts.tagsPlaceholder")}
                          maxVisible={3}
                        />
                      </div>

                      <div className="rounded-xl border border-border p-4">
                        <div className="flex items-start justify-between gap-3">
                          <div>
                            <div className="text-sm font-semibold text-foreground">
                              {t("accounts.groupsLabel")}
                            </div>
                            <div className="mt-1 text-xs text-muted-foreground">
                              {t("accounts.groupsHint")}
                            </div>
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
                            groups={allGroups}
                            value={editGroupIds}
                            onChange={setEditGroupIds}
                            allLabel={t("accounts.groupsUnbound")}
                            selectedLabel={t("accounts.groupsSelected", {
                              count: editGroupIds.length,
                            })}
                            placeholder={t("accounts.groupsPlaceholder")}
                            emptyLabel={t("accounts.groupsNone")}
                            emptyHint={t("accounts.groupsSelectHint")}
                          />
                        </div>
                      </div>
                    </div>

                    <div className="rounded-xl border border-border bg-white/60 px-4 py-4 dark:bg-white/5">
                      <div className="text-sm font-semibold text-foreground">
                        {t("accounts.schedulerPreviewTitle")}
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
                  </>
                )}
              </div>
            ) : null}
          </Modal>

          <Modal
            show={showBatchMetaEditor}
            title={t("accounts.batchMetaTitle")}
            contentClassName="sm:max-w-[560px]"
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
                  disabled={batchMetaSubmitting}
                >
                  {batchMetaSubmitting ? t("common.saving") : t("common.save")}
                </Button>
              </>
            }
          >
            <div className="space-y-4">
              <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
                {t("accounts.batchMetaDesc", { count: selected.size })}
              </div>
              <div className="rounded-xl border border-border p-4">
                <div className="text-sm font-semibold text-foreground">
                  {t("accounts.tagsLabel")}
                </div>
                <ChipInput
                  className="mt-3"
                  value={batchTags}
                  onChange={setBatchTags}
                  placeholder={t("accounts.tagsPlaceholder")}
                  maxVisible={6}
                />
              </div>
              <div className="rounded-xl border border-border p-4">
                <div className="text-sm font-semibold text-foreground">
                  {t("accounts.groupsLabel")}
                </div>
                <div className="mt-3">
                  <AccountGroupMultiSelect
                    groups={allGroups}
                    value={batchGroupIds}
                    onChange={setBatchGroupIds}
                    allLabel={t("accounts.groupsUnbound")}
                    selectedLabel={t("accounts.groupsSelected", {
                      count: batchGroupIds.length,
                    })}
                    placeholder={t("accounts.groupsPlaceholder")}
                    emptyLabel={t("accounts.groupsNone")}
                    emptyHint={t("accounts.groupsSelectHint")}
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
                  disabled={groupSubmitting || !groupDraft.name.trim()}
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
                              <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-semibold text-muted-foreground">
                                {t("accounts.groupMembers")}{" "}
                                {group.member_count}
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
    "all" | "pro" | "prolite" | "plus" | "team" | "free" | "api" | "unknown"
  >("all");
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

  const busy = batchActing || emptying || batchTesting;

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
                            {row.openai_responses_api
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

function ModelChipGrid({
  models,
  onRemove,
  emptyLabel,
}: {
  models: string[];
  onRemove: (model: string) => void;
  emptyLabel: string;
}) {
  if (models.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border bg-muted/20 px-3 py-3 text-sm text-muted-foreground">
        {emptyLabel}
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
}: {
  active: boolean;
  onClick: () => void;
  label: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-full px-3 py-1.5 text-xs font-semibold transition-colors ${
        active
          ? "bg-primary text-primary-foreground"
          : "bg-muted/50 text-muted-foreground hover:bg-muted"
      }`}
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
  return getAccountRateLimitWindow(account) !== null;
}

function isUnsampledQuotaAccount(account: AccountRow): boolean {
  const status = (account.status || "").toLowerCase();
  if (status === "unauthorized" || account.openai_responses_api) {
    return false;
  }
  // k12 等 team 型工作区可能只返回 5h 窗口：任一窗口有数据即算已采样，
  // 否则这类账号会永远显示"未采样" (issue #282)。
  const has7d =
    typeof account.usage_percent_7d === "number" &&
    Number.isFinite(account.usage_percent_7d);
  const has5h =
    typeof account.usage_percent_5h === "number" &&
    Number.isFinite(account.usage_percent_5h);
  return !has7d && !has5h;
}

function getAccountRateLimitWindow(
  account: AccountRow,
): RateLimitWindow | null {
  const status = (account.status || "").toLowerCase();
  const reason = (account.cooldown_reason || "").toLowerCase();
  const explicitlyRateLimited =
    status === "rate_limited" ||
    status === "usage_exhausted" ||
    status === "quota_paused" ||
    status === "rate_limited_5h" ||
    status === "rate_limited_7d" ||
    reason === "rate_limited" ||
    reason === "rate_limited_5h" ||
    reason === "rate_limited_7d";
  const has7dLimit = isActiveUsageWindowExhausted(
    account.usage_percent_7d,
    account.reset_7d_at,
  );
  const has5hLimit =
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

  return explicitlyRateLimited ? "5h" : null;
}

function getRateLimitedWindowStats(accounts: AccountRow[]): {
  total: number;
  fiveHour: number;
  sevenDay: number;
} {
  const stats = accounts.reduce(
    (stats, account) => {
      const window = getAccountRateLimitWindow(account);
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

interface HeaderActionMenuItem {
  key: string;
  label: string;
  icon: ReactNode;
  disabled?: boolean;
  title?: string;
  onSelect: () => void;
}

function HeaderActionMenu({
  label,
  icon,
  items,
  align = "end",
  compact = false,
}: {
  label: string;
  icon: ReactNode;
  items: HeaderActionMenuItem[];
  align?: "start" | "end";
  compact?: boolean;
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

  return (
    <div ref={rootRef} className="relative shrink-0">
      <Button
        type="button"
        variant="outline"
        size="sm"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={label}
        onClick={() => setOpen((current) => !current)}
        className={compact ? "px-2.5" : undefined}
      >
        {icon}
        {!compact ? (
          <>
            {label}
            <ChevronDown
              className={`size-3.5 transition-transform ${open ? "rotate-180" : ""}`}
            />
          </>
        ) : null}
      </Button>

      {open ? (
        <div
          className={`absolute top-[calc(100%+0.5rem)] z-50 max-h-[min(70dvh,420px)] w-[min(18rem,calc(100vw-2rem))] overflow-y-auto overflow-x-hidden rounded-xl border border-border bg-popover p-1.5 shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm ${
            align === "start" ? "left-0" : "right-0"
          }`}
        >
          <div role="menu" className="space-y-0.5">
            {items.map((item) => (
              <button
                key={item.key}
                type="button"
                role="menuitem"
                disabled={item.disabled}
                title={item.title}
                className="flex w-full items-center gap-2 rounded-lg px-2.5 py-2.5 text-left text-sm text-foreground transition-colors hover:bg-accent/70 disabled:cursor-not-allowed disabled:opacity-50"
                onClick={() => {
                  if (item.disabled) return;
                  setOpen(false);
                  item.onSelect();
                }}
              >
                <span className="flex size-5 shrink-0 items-center justify-center text-muted-foreground">
                  {item.icon}
                </span>
                <span className="min-w-0 flex-1 truncate">{item.label}</span>
              </button>
            ))}
          </div>
        </div>
      ) : null}
    </div>
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
    progress.action === "batch_delete"
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

function PlanBadge({ planType }: { planType?: string }) {
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

  return (
    <span
      className={`inline-flex min-w-0 max-w-full items-center truncate rounded-md px-2.5 py-1 text-[13px] font-semibold ring-1 ring-inset ${cls}`}
      title={label}
    >
      {label}
    </span>
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

function CompactStat({
  label,
  chipLabel,
  value,
  tone,
  details,
}: {
  label: string;
  chipLabel?: string;
  value: number;
  tone: "neutral" | "success" | "warning" | "danger";
  details?: Array<{ label: string; value: number }>;
}) {
  const toneStyle = {
    neutral: {
      chip: "bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300",
      dot: "bg-slate-500",
    },
    success: {
      chip: "bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300",
      dot: "bg-emerald-500",
    },
    warning: {
      chip: "bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300",
      dot: "bg-amber-500",
    },
    danger: {
      chip: "bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300",
      dot: "bg-red-500",
    },
  }[tone];

  return (
    <div className="flex min-h-[76px] items-center justify-between gap-2 rounded-xl border border-border bg-card/85 px-2.5 py-2 shadow-sm sm:min-h-[88px] sm:gap-3 sm:px-3 sm:py-2.5">
      <div className="min-w-0">
        <div className="truncate text-[11px] font-semibold text-muted-foreground sm:text-[12px]">
          {label}
        </div>
        <div className="mt-1 text-[22px] font-bold leading-none tabular-nums text-foreground sm:text-[24px]">
          {value}
        </div>
      </div>
      <div className="flex min-h-[52px] shrink-0 flex-col items-end gap-1 sm:min-h-[58px] sm:gap-1.5">
        <div
          className={`inline-flex items-center gap-1.5 rounded-md px-1.5 py-0.5 text-[11px] font-semibold sm:px-2 sm:py-1 sm:text-[12px] ${toneStyle.chip}`}
        >
          <span className={`size-1.5 rounded-full sm:size-2 ${toneStyle.dot}`} />
          <span className="max-w-[4.5rem] truncate sm:max-w-none">{chipLabel ?? label}</span>
        </div>
        {details && details.length > 0 && (
          <div className="flex flex-col items-end gap-0.5 text-[11px] font-semibold leading-4 text-muted-foreground">
            {details.map((item) => (
              <div
                key={item.label}
                className="grid grid-cols-[max-content_auto_max-content] items-center gap-x-0.5 whitespace-nowrap tabular-nums"
              >
                <span className="justify-self-start">{item.label}</span>
                <span className="justify-self-center">：</span>
                <span className="justify-self-end text-foreground">
                  {item.value}
                </span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
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
    neutral:
      "bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300",
    success:
      "bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300",
    warning:
      "bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300",
    danger: "bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300",
  }[tone];

  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 font-semibold ${toneStyle}`}
    >
      <span>{label}</span>
      <span>{value}</span>
    </span>
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
  const toneClass =
    tone === "purple"
      ? "bg-purple-500/10 text-purple-700 ring-purple-500/20 dark:text-purple-300"
      : "bg-blue-500/10 text-blue-700 ring-blue-500/20 dark:text-blue-300";

  return (
    <div className="mt-1.5 flex flex-wrap gap-1">
      {visible.map((item) => (
        <span
          key={item}
          className={`inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-semibold ring-1 ring-inset ${toneClass}`}
        >
          {item}
        </span>
      ))}
      {hidden > 0 && (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">
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
      className="inline-flex max-w-full items-center break-all rounded-md bg-cyan-500/10 px-1.5 py-0.5 text-left text-[10px] font-semibold leading-tight text-cyan-700 ring-1 ring-inset ring-cyan-500/20 dark:text-cyan-300"
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

function GroupChipList({ groups }: { groups: AccountGroup[] }) {
  if (groups.length === 0) return null;
  const visible = groups.slice(0, 3);
  const hidden = groups.length - visible.length;

  return (
    <div className="mt-1.5 flex flex-wrap gap-1">
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
    </div>
  );
}

function ColumnSettingsMenu({
  columns,
  onToggle,
  onReset,
  resetTitle,
  labels,
  title,
}: {
  columns: Record<AccountTableColumn, boolean>;
  onToggle: (column: AccountTableColumn) => void;
  onReset: () => void;
  resetTitle: string;
  labels: Record<AccountTableColumn, string>;
  title: string;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const handler = (event: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(event.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  return (
    <div ref={rootRef} className="relative">
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => setOpen((current) => !current)}
        title={title}
      >
        <SlidersHorizontal className="size-3.5" />
        {title}
      </Button>
      {open ? (
        <div className="absolute right-0 top-[calc(100%+0.5rem)] z-50 w-48 overflow-hidden rounded-lg border border-border bg-popover p-1.5 shadow-lg">
          <button
            type="button"
            className="mb-1 flex w-full items-center justify-center rounded-md px-2.5 py-1.5 text-xs font-semibold text-primary transition-colors hover:bg-accent/70"
            onClick={onReset}
          >
            {resetTitle}
          </button>
          {ACCOUNT_TABLE_COLUMNS.map((column) => (
            <button
              key={column}
              type="button"
              role="menuitemcheckbox"
              aria-checked={columns[column]}
              className="flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-sm text-foreground transition-colors hover:bg-accent/70"
              onClick={() => onToggle(column)}
            >
              <span
                className={`flex size-4 shrink-0 items-center justify-center rounded border ${columns[column] ? "border-primary bg-primary text-primary-foreground" : "border-border bg-background"}`}
              >
                {columns[column] ? <Check className="size-3" /> : null}
              </span>
              <span className="min-w-0 flex-1 truncate">{labels[column]}</span>
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}

function AccountMobileCard({
  account,
  sequence,
  selected,
  allGroups,
  lazyMode,
  showEmailDomainTags,
  healthBuckets,
  refreshing,
  authJsonExporting,
  variant = "mobile",
  t,
  onToggleSelect,
  onEdit,
  onUsage,
  onTest,
  onRefresh,
  onGenerateAuthJson,
  onToggleEnabled,
  onToggleLock,
  onResetStatus,
  onResetCredits,
  onDelete,
  onUsageRefreshed,
}: {
  account: AccountRow;
  sequence: number;
  selected: boolean;
  allGroups: AccountGroup[];
  lazyMode: boolean;
  showEmailDomainTags: boolean;
  healthBuckets: AccountHealthBucket[] | undefined;
  refreshing: boolean;
  authJsonExporting: boolean;
  variant?: "mobile" | "personal";
  t: ReturnType<typeof useTranslation>["t"];
  onToggleSelect: () => void;
  onEdit: () => void;
  onUsage: () => void;
  onTest: () => void;
  onRefresh: () => void;
  onGenerateAuthJson: () => void;
  onToggleEnabled: () => void;
  onToggleLock: () => void;
  onResetStatus: () => void;
  onResetCredits: () => void;
  onDelete: () => void;
  onUsageRefreshed?: () => void;
}) {
  const displayName = account.openai_responses_api
    ? formatAccountName(account)
    : formatAccountListEmail(account);
  const fullName = formatAccountName(account);
  const groups = resolveAccountGroups(account.group_ids ?? [], allGroups);
  const refreshDisabled =
    refreshing || account.at_only || account.openai_responses_api;
  const authJsonDisabled =
    authJsonExporting || account.at_only || account.openai_responses_api;
  // 自用模式用独立的信息架构：更强调账号身份、用量、健康和少量高频操作。
  const isPersonal = variant === "personal";
  const avatarInitial = (displayName.trim()[0] || "?").toUpperCase();
  const chatgptAccountId = account.chatgpt_account_id?.trim() ?? "";
  const resetCredits = account.rate_limit_reset_credits ?? 0;
  const hasStateBadges =
    account.at_only ||
    account.openai_responses_api ||
    account.enabled === false ||
    account.locked;
  const modelCooldownCount = account.model_cooldowns?.length ?? 0;

  if (isPersonal) {
    return (
      <article
        className={`group flex h-full min-w-0 flex-col overflow-hidden rounded-lg border bg-card shadow-sm transition-all hover:-translate-y-0.5 hover:shadow-lg ${
          selected
            ? "border-primary/40 bg-primary/5 ring-1 ring-primary/20"
            : "border-border"
        }`}
      >
        <div className="flex min-w-0 items-start gap-4 p-5 pb-4">
          <input
            type="checkbox"
            className="mt-2 size-4 shrink-0 cursor-pointer accent-primary"
            checked={selected}
            onChange={onToggleSelect}
            aria-label={fullName}
          />

          <div className="flex shrink-0 flex-col items-center gap-2">
            <div className="flex size-12 items-center justify-center rounded-lg bg-sky-50 text-lg font-semibold text-sky-700 ring-1 ring-inset ring-sky-200 dark:bg-sky-950/70 dark:text-sky-300 dark:ring-sky-800">
              {avatarInitial}
            </div>
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
          </div>

          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 flex-wrap items-center gap-1.5">
              <span className="rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-mono font-semibold text-muted-foreground">
                #{sequence}
              </span>
              <PlanBadge planType={account.plan_type} />
              <AccountStatusCountdown account={account} />
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
                <div
                  className="break-all text-lg font-semibold leading-tight text-foreground"
                  title={fullName}
                >
                  {displayName}
                </div>
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
              <div className="shrink-0">
                <StatusBadge
                  status={account.status}
                  detail={getAccountRateLimitWindow(account) ?? undefined}
                  errorMessage={account.error_message}
                />
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
                {account.enabled === false && (
                  <span className="inline-flex items-center rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium text-zinc-700 ring-1 ring-inset ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-300 dark:ring-zinc-400/20">
                    <PowerOff className="mr-0.5 size-2.5" />
                    {t("accounts.disabled")}
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
                <div className="flex items-baseline gap-2 text-[13px]">
                  <span className="text-base font-semibold text-emerald-600 dark:text-emerald-400">
                    {account.success_requests ?? 0}
                  </span>
                  <span className="text-muted-foreground">/</span>
                  <span className="font-semibold text-red-500">
                    {account.error_requests ?? 0}
                  </span>
                </div>
                {((account.retry_error_requests ?? 0) > 0 ||
                  (account.rate_limit_attempts ?? 0) > 0) && (
                  <div className="mt-1 text-[11px] text-muted-foreground">
                    retry {account.retry_error_requests ?? 0} · 429 {" "}
                    {account.rate_limit_attempts ?? 0}
                  </div>
                )}
              </AccountPersonalMetric>
              <AccountPersonalMetric
                label={t("accounts.billed")}
                icon={<Coins className="size-3.5" />}
                tone="amber"
              >
                <BilledCell account={account} />
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

        {((account.tags ?? []).length > 0 || groups.length > 0) && (
          <div className="mx-5 space-y-1.5 border-t border-border/70 py-3">
            <ChipList items={account.tags ?? []} tone="purple" />
            <GroupChipList groups={groups} />
          </div>
        )}

        <div className="mt-auto border-t border-border/70 bg-muted/15 p-4">
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
            <AccountMobileActionButton
              title={t("accounts.editScheduler")}
              label={t("accounts.editScheduler")}
              onClick={onEdit}
              icon={<Pencil className="size-3.5" />}
            />
            <AccountMobileActionButton
              title={t("accounts.testConnection")}
              label={t("accounts.testConnection")}
              onClick={onTest}
              icon={<Zap className="size-3.5" />}
            />
            <AccountMobileActionButton
              title={
                account.at_only || account.openai_responses_api
                  ? t("accounts.atRefreshDisabled")
                  : t("accounts.refreshAccessToken")
              }
              label={t("accounts.actionRefreshAT")}
              disabled={refreshDisabled}
              onClick={onRefresh}
              icon={
                <RefreshCw
                  className={`size-3.5 ${refreshing ? "animate-spin" : ""}`}
                />
              }
            />
            <AccountMobileActionButton
              title={
                account.at_only || account.openai_responses_api
                  ? t("accounts.authJsonDisabled")
                  : t("accounts.generateAuthJson")
              }
              label={t("accounts.actionAuthJson")}
              disabled={authJsonDisabled}
              onClick={onGenerateAuthJson}
              icon={<FileJson className="size-3.5" />}
            />
            <AccountMobileActionButton
              title={
                account.enabled === false
                  ? t("accounts.enableHint")
                  : t("accounts.disableHint")
              }
              label={
                account.enabled === false
                  ? t("accounts.actionEnableScheduling")
                  : t("accounts.actionDisableScheduling")
              }
              variant={account.enabled === false ? "default" : "outline"}
              onClick={onToggleEnabled}
              icon={
                account.enabled === false ? (
                  <Power className="size-3.5" />
                ) : (
                  <PowerOff className="size-3.5" />
                )
              }
            />
            <AccountMobileActionButton
              title={
                account.locked ? t("accounts.unlockHint") : t("accounts.lockHint")
              }
              label={
                account.locked
                  ? t("accounts.actionUnlockAccount")
                  : t("accounts.actionLockAccount")
              }
              variant={account.locked ? "default" : "outline"}
              onClick={onToggleLock}
              icon={
                account.locked ? (
                  <Lock className="size-3.5" />
                ) : (
                  <Unlock className="size-3.5" />
                )
              }
            />
            <AccountMobileActionButton
              title={t("accounts.resetStatusHint")}
              label={t("accounts.resetStatus")}
              onClick={onResetStatus}
              icon={<RotateCcw className="size-3.5" />}
            />
            <AccountMobileActionButton
              title={t("accounts.resetCreditsButton")}
              label={t("accounts.resetCreditsButton")}
              disabled={resetCredits <= 0}
              onClick={onResetCredits}
              icon={<Timer className="size-3.5" />}
            />
            <AccountMobileActionButton
              title={t("accounts.deleteAccount")}
              label={t("accounts.deleteAccount")}
              variant="destructive"
              onClick={onDelete}
              icon={<Trash2 className="size-3.5" />}
            />
          </div>
        </div>
      </article>
    );
  }

  return (
    <article
      className={`min-w-0 rounded-lg border bg-card p-3 shadow-sm ${
        selected ? "border-primary/40 bg-primary/5 ring-1 ring-primary/20" : "border-border"
      }`}
    >
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
                  <span className="rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-mono font-semibold text-muted-foreground">
                    #{sequence}
                  </span>
                  <PlanBadge planType={account.plan_type} />
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
                <div
                  className="mt-1 break-all text-[15px] font-semibold leading-tight text-foreground"
                  title={fullName}
                >
                  {displayName}
                </div>
                {chatgptAccountId && (
                  <div
                    className="mt-1 min-h-[14px] max-w-full truncate font-mono text-[10px] leading-tight text-muted-foreground/70"
                    title={chatgptAccountId}
                  >
                    {chatgptAccountId}
                  </div>
                )}
              </div>
              <div className="flex min-w-[112px] shrink-0 flex-col items-end">
                <StatusBadge
                  status={account.status}
                  detail={getAccountRateLimitWindow(account) ?? undefined}
                  errorMessage={account.error_message}
                />
                <div className="mt-1 flex min-h-6 items-center justify-end">
                  <AccountStatusCountdown account={account} />
                </div>
              </div>
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
            {account.enabled === false && (
              <span className="inline-flex items-center rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium text-zinc-700 ring-1 ring-inset ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-300 dark:ring-zinc-400/20">
                <PowerOff className="mr-0.5 size-2.5" />
                {t("accounts.disabled")}
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
          <div className="mt-1 text-[11px] text-muted-foreground">
            {t("accounts.healthSummary", {
              health: formatHealthTier(account.health_tier, t),
              score: Math.round(getDispatchScore(account)),
              concurrency: account.dynamic_concurrency_limit ?? "-",
            })}
          </div>
          <div className="mt-1.5 space-y-0.5">
            <div className="text-[10px] text-muted-foreground/70">
              {t("accounts.healthBarLabel")}
            </div>
            <AccountHealthBar buckets={healthBuckets} />
          </div>
        </div>
      </div>

      <div
        className="mt-3 grid min-w-0 grid-cols-2 gap-2 max-[380px]:grid-cols-1"
      >
        <AccountMobileMetric label={t("accounts.requests")} className="min-h-[84px]">
          <div className="flex items-center gap-2 text-[13px]">
            <span className="font-medium text-emerald-600">
              {account.success_requests ?? 0}
            </span>
            <span className="text-muted-foreground">/</span>
            <span className="font-medium text-red-500">
              {account.error_requests ?? 0}
            </span>
          </div>
          {((account.retry_error_requests ?? 0) > 0 ||
            (account.rate_limit_attempts ?? 0) > 0) && (
            <div className="mt-0.5 text-[11px] text-muted-foreground">
              retry {account.retry_error_requests ?? 0} · 429{" "}
              {account.rate_limit_attempts ?? 0}
            </div>
          )}
        </AccountMobileMetric>
        <AccountMobileMetric label={t("accounts.billed")} className="min-h-[84px]">
          <BilledCell account={account} />
        </AccountMobileMetric>
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
        <AccountMobileMetric label={t("accounts.importTime")} className="min-h-[84px]">
          {formatBeijingTime(account.created_at)}
        </AccountMobileMetric>
        <AccountMobileMetric
          label={t("accounts.usage")}
          className="col-span-2 min-h-[116px] max-[380px]:col-span-1"
        >
          <UsageCell account={account} onRefreshed={onUsageRefreshed} />
        </AccountMobileMetric>
      </div>

      {((account.tags ?? []).length > 0 ||
        (!isPersonal && showEmailDomainTags && getAccountEmailDomain(account)) ||
        groups.length > 0) && (
        <div className="mt-3 space-y-1.5 border-t border-border pt-2">
          <ChipList items={account.tags ?? []} tone="purple" />
          {!isPersonal && showEmailDomainTags && getAccountEmailDomain(account) && (
            <div className="mt-1.5 flex flex-wrap gap-1">
              <EmailDomainBadge domain={getAccountEmailDomain(account)} t={t} />
            </div>
          )}
          <GroupChipList groups={groups} />
        </div>
      )}

      <div
        className="mt-3 grid grid-cols-5 gap-1.5 max-[380px]:grid-cols-4"
      >
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
        <AccountMobileActionButton
          title={t("accounts.testConnection")}
          onClick={onTest}
          icon={<Zap className="size-3.5" />}
        />
        <AccountMobileActionButton
          title={
            account.at_only || account.openai_responses_api
              ? t("accounts.atRefreshDisabled")
              : t("accounts.refreshAccessToken")
          }
          disabled={refreshDisabled}
          onClick={onRefresh}
          icon={
            <RefreshCw
              className={`size-3.5 ${refreshing ? "animate-spin" : ""}`}
            />
          }
        />
        <AccountMobileActionButton
          title={
            account.at_only || account.openai_responses_api
              ? t("accounts.authJsonDisabled")
              : t("accounts.generateAuthJson")
          }
          disabled={authJsonDisabled}
          onClick={onGenerateAuthJson}
          icon={<FileJson className="size-3.5" />}
        />
        <AccountMobileActionButton
          title={
            account.enabled === false
              ? t("accounts.enableHint")
              : t("accounts.disableHint")
          }
          variant={account.enabled === false ? "default" : "outline"}
          onClick={onToggleEnabled}
          icon={
            account.enabled === false ? (
              <Power className="size-3.5" />
            ) : (
              <PowerOff className="size-3.5" />
            )
          }
        />
        <AccountMobileActionButton
          title={
            account.locked ? t("accounts.unlockHint") : t("accounts.lockHint")
          }
          variant={account.locked ? "default" : "outline"}
          onClick={onToggleLock}
          icon={
            account.locked ? (
              <Lock className="size-3.5" />
            ) : (
              <Unlock className="size-3.5" />
            )
          }
        />
        <AccountMobileActionButton
          title={t("accounts.resetStatusHint")}
          onClick={onResetStatus}
          icon={<RotateCcw className="size-3.5" />}
        />
        <AccountMobileActionButton
          title={t("accounts.resetCreditsButton")}
          disabled={(account.rate_limit_reset_credits ?? 0) <= 0}
          onClick={onResetCredits}
          icon={<Timer className="size-3.5" />}
        />
        <AccountMobileActionButton
          title={t("accounts.deleteAccount")}
          variant="destructive"
          onClick={onDelete}
          icon={<Trash2 className="size-3.5" />}
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
  type: "test_start" | "content" | "test_complete" | "error";
  text?: string;
  model?: string;
  success?: boolean;
  error?: string;
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
  const [output, setOutput] = useState<string[]>([]);
  const [status, setStatus] = useState<
    "connecting" | "streaming" | "success" | "error"
  >("connecting");
  const [errorMsg, setErrorMsg] = useState("");
  const [model, setModel] = useState("");
  const [selectedModel, setSelectedModel] = useState("");
  const [modelOptions, setModelOptions] = useState<string[]>([]);
  const [modelOptionsReady, setModelOptionsReady] = useState(false);
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

  const isOpenAIResponsesAccount = Boolean(account.openai_responses_api);

  const modelSelectOptions = useMemo(
    () =>
      uniqueTestModels(
        modelOptions,
        selectedModel,
        !isOpenAIResponsesAccount,
      ).map((item) => ({ label: item, value: item })),
    [isOpenAIResponsesAccount, modelOptions, selectedModel],
  );

  useEffect(() => {
    let active = true;

    const loadModels = async () => {
      try {
        const settings = await api.getSettings();
        if (!active) return;

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
        if (isOpenAIResponsesAccount) {
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
  }, [account.model_mapping, account.models, isOpenAIResponsesAccount]);

  useEffect(() => {
    if (!modelOptionsReady || !selectedModel) return;

    // 重置状态（StrictMode 二次 mount 时清理上一次的残留）
    setOutput([]);
    setStatus("connecting");
    setErrorMsg("");
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
                case "test_complete":
                  receivedTerminalEvent = true;
                  setStatus(event.success ? "success" : "error");
                  markSettled();
                  break;
                case "error":
                  receivedTerminalEvent = true;
                  setStatus("error");
                  setErrorMsg(event.error || t("accounts.unknownError"));
                  markSettled();
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

        if (!receivedTerminalEvent) {
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
    markSettled,
    modelOptionsReady,
    restoreOnSuccess,
    selectedModel,
    t,
  ]);

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [output]);

  const statusLabel = {
    connecting: `⏳ ${t("accounts.connecting")}`,
    streaming: `🔄 ${t("accounts.receivingResponse")}`,
    success: `✅ ${t("accounts.testSuccess")}`,
    error: `❌ ${t("accounts.testFailed")}`,
  }[status];

  const statusColor = {
    connecting: "text-muted-foreground",
    streaming: "text-blue-500",
    success: "text-emerald-500",
    error: "text-red-500",
  }[status];
  const formattedErrorMsg = errorMsg ? formatTestErrorMessage(errorMsg) : "";

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
        <Button
          variant="outline"
          onClick={() => {
            abortRef.current?.abort();
            onClose();
          }}
        >
          {t("common.close")}
        </Button>
      }
      contentClassName="sm:max-w-[680px]"
    >
      <div className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <span
            className={`flex items-center gap-1.5 text-sm font-semibold ${statusColor}`}
          >
            {statusLabel}
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
          <div className="max-h-[40vh] overflow-auto rounded-xl border border-red-200 bg-red-50 p-3.5 text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400">
            <div className="mb-2 text-sm font-semibold">
              {t("accounts.failureDetails")}
            </div>
            <pre
              className="text-[13px] leading-relaxed whitespace-pre-wrap break-all"
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
        <span className="text-[11px] font-medium text-muted-foreground w-7 shrink-0">
          {label}
        </span>
        <div className="flex-1 h-1.5 rounded-full bg-muted overflow-hidden min-w-[72px]">
          <div
            className={`h-full rounded-full transition-all ${usageBarColor(pct)}`}
            style={{ width: `${Math.min(100, pct)}%` }}
          />
        </div>
        <span className="text-[12px] font-semibold w-[42px] text-right shrink-0">
          {pct.toFixed(1)}%
        </span>
      </div>
      {detailText && (
        <div className="text-[11px] font-medium text-muted-foreground mt-0.5 pl-[34px]">
          {detailText}
        </div>
      )}
      {resetTime && (
        <div
          className="text-[11px] font-medium text-muted-foreground mt-0.5 pl-[34px]"
          title={resetTime.title}
        >
          ⏱ {resetTime.label}
        </div>
      )}
    </div>
  );
}

function UsageWindowStat({
  label,
  detail,
}: {
  label: string;
  detail?: AccountRow["usage_5h_detail"];
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
        <span className="w-7 shrink-0">{label}</span>
        <span>
          {formatCompactUsageNumber(detail?.requests)}{" "}
          {t("accounts.usageReqUnit")} /{" "}
          {formatCompactUsageNumber(detail?.tokens)}{" "}
          {t("accounts.usageTokUnit")}
        </span>
      </div>
      {(accountBilledText || userBilledText) && (
        <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground/80 pl-[34px]">
          {accountBilledText && (
            <span>
              {t("accounts.accountBilledLabel")}: ${accountBilledText}
            </span>
          )}
          {userBilledText && (
            <span>
              {t("accounts.userBilledLabel")}: ${userBilledText}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

// 用量列组件
//
// 显示策略不再单独依赖 plan_type:
// 当 plan_type 还停留在按 RT 刷新出来的旧值(例如 "free")、但账号实际已订阅、
// 后端已经返回 5h 窗口数据时,只看 plan_type 会把 5h 吞掉。
// 因此这里以"是否真的存在 5h / 7d 数据(含 reset 时间)"作为主判据,
// plan_type 仅作为 5h 数据缺位时的辅助提示。
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
      className="shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
    >
      <RefreshCw className={`size-3 ${refreshing ? "animate-spin" : ""}`} />
    </button>
  );

  const plan = normalizePlanType(account.plan_type);
  const has7d =
    account.usage_percent_7d !== null && account.usage_percent_7d !== undefined;
  const has5h =
    account.usage_percent_5h !== null && account.usage_percent_5h !== undefined;
  const has7dDetail = hasUsageWindowDetail(account.usage_7d_detail);
  const has5hDetail = hasUsageWindowDetail(account.usage_5h_detail);
  const has5hReset = !!account.reset_5h_at;
  const has7dReset = !!account.reset_7d_at;

  const fiveHourPresent = has5h || has5hDetail || has5hReset;
  const sevenDayPresent = has7d || has7dDetail || has7dReset;
  // 长窗口标签:free/team plan 实为月窗(约 30 天),按真实周期显示 30d 而非误标 7d (issue #324)
  const longWindowLabel = formatLongUsageWindowLabel(account);
  // plan 表明是订阅型时,即使数据暂未拉到也按订阅布局占位,避免抖动
  const planSuggestsPremium = isPremiumUsagePlan(plan);
  const showFiveHour = fiveHourPresent || planSuggestsPremium;

  if (showFiveHour) {
    if (!has5h && !has7d && !has5hDetail && !has7dDetail && !has5hReset && !has7dReset)
      return <span className="text-[12px] text-muted-foreground">-</span>;
    return (
      <div className={`${wide ? "w-full" : "w-52"} flex items-start gap-1`}>
        <div className="flex-1 space-y-1.5">
          {has5h ? (
            <UsageBar
              label="5h"
              pct={account.usage_percent_5h!}
              resetAt={account.reset_5h_at}
              detail={account.usage_5h_detail}
            />
          ) : (
            <UsageWindowStat label="5h" detail={account.usage_5h_detail} />
          )}
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

  if (sevenDayPresent) {
    return (
      <div className={`${wide ? "w-full" : "w-48"} flex items-start gap-1`}>
        <div className="flex-1">
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

  return <span className="text-[13px] text-muted-foreground">-</span>;
}

function BilledCell({ account }: { account: AccountRow }) {
  const h5 = typeof account.billed_5h === "number" ? account.billed_5h.toFixed(2) : null;
  const d7 = typeof account.billed_7d === "number" ? account.billed_7d.toFixed(2) : null;
  if (h5 === null && d7 === null) return <span className="text-[12px] text-muted-foreground">-</span>;
  const longLabel = formatLongUsageWindowLabel(account);
  return (
    <span className="text-[12px] text-muted-foreground">
      {h5 !== null ? `5h: $${h5}` : "5h: -"}
      {" / "}
      {d7 !== null ? `${longLabel}: $${d7}` : `${longLabel}: -`}
    </span>
  );
}

function getAccountStatusCountdownUntil(
  account: AccountRow,
): string | undefined {
  const status = account.status;
  if (
    account.cooldown_until &&
    (status === "rate_limited" || status === "error" || status === "cooldown")
  ) {
    return account.cooldown_until;
  }
  if (status === "quota_paused") {
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
  const [remaining, setRemaining] = useState("");
  const title = formatBeijingTime(until);

  useEffect(() => {
    const target = new Date(until).getTime();

    const update = () => {
      const diff = Math.max(0, target - Date.now());
      if (diff <= 0) {
        setRemaining("");
        return;
      }
      const h = Math.floor(diff / 3600000);
      const m = Math.floor((diff % 3600000) / 60000);
      const s = Math.floor((diff % 60000) / 1000);
      if (h > 0) {
        setRemaining(
          `${h}h ${String(m).padStart(2, "0")}m ${String(s).padStart(2, "0")}s`,
        );
      } else if (m > 0) {
        setRemaining(`${m}m ${String(s).padStart(2, "0")}s`);
      } else {
        setRemaining(`${s}s`);
      }
    };

    update();
    const id = setInterval(update, 1000);
    return () => clearInterval(id);
  }, [until]);

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
