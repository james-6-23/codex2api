import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { ChangeEvent, ReactNode } from "react";
import {
  FolderOpen,
  Plus,
  RefreshCw,
  Trash2,
  Power,
  PowerOff,
  X,
  KeyRound,
  FileJson,
  Search,
  Sparkles,
  ExternalLink,
  Copy,
  Link2,
  Loader2,
  FlaskConical,
  Zap,
  CheckCircle2,
  XCircle,
  Rows3,
  LayoutGrid,
  Upload,
  Download,
  FileText,
  RotateCcw,
  Pencil,
  BarChart3,
  Layers,
} from "lucide-react";
import { api, getAdminKey } from "../api";
import type { ProxyRow } from "../api";
import { ProxyField } from "../components/ProxyField";
import AccountProxyBadge from "../components/AccountProxyBadge";
import AccountProxyQuickEditor from "../components/AccountProxyQuickEditor";
import {
  buildProxyBindingContext,
  type ProxyBindingContext,
} from "../lib/accountProxyBinding";
import type {
  AccountGroup,
  AccountRow,
  AccountHealthBucket,
  AccountListSummary,
  AccountOperationSelector,
  AddGrokAccountRequest,
  BatchUpdateGrokModelsRequest,
  GrokSSOImportItem,
  GrokAccountState,
  GrokModelCatalogItem,
  GrokProtocol,
  AccountLiveStateResponse,
} from "../types";
import AccountDetailSheet from "../components/AccountDetailSheet";
import RequestCountPills from "../components/RequestCountPills";
import {
  disabledAccountSurfaceClass,
  disabledAccountTableRowClass,
  renderDisabledAccountOverlay,
} from "../components/AccountStateOverlay";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  isAccountGroupFilterEmpty,
  pruneAccountGroupFilter,
  type AccountGroupFilterValue,
} from "../components/AccountGroupFilterSelect";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import { useImportGroupIds } from "../hooks/useImportGroupIds";
import { useAccountTableColumns } from "../hooks/useAccountTableColumns";
import { useIsDesktop } from "../hooks/useMediaQuery";
import AccountHealthBar from "../components/AccountHealthBar";
import AccountUsageModal from "../components/AccountUsageModal";
import Modal from "../components/Modal";
import ModelLogo from "../components/ModelLogo";
import OperationResultsModal from "../components/OperationResultsModal";
import PageHeader from "../components/PageHeader";
import ColumnSettingsMenu from "../components/ColumnSettingsMenu";
import { CompactStat } from "../components/CompactStat";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import ChannelLogo from "../components/ChannelLogo";
import StatusBadge from "../components/StatusBadge";
import { mergeAccountLiveState, useAccountLiveState } from "../hooks/useAccountLiveState";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { useOperationProgress } from "../hooks/useOperationProgress";
import {
  DEFAULT_PAGE_SIZE_OPTIONS,
  usePersistedPageSize,
} from "../hooks/usePersistedPageSize";
import OperationProgressToast from "../components/OperationProgressToast";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import {
  resolveAccountGrokPlan,
  type GrokPlanFilter,
} from "../lib/grokPlan";
import {
  isLargePoolSortDisabled,
  resolveDisabledAccountSorts,
} from "../lib/accountListSort";
import {
  emptyModelMappingEntries,
  parseModelMappingEntries,
  serializeModelMappingEntries,
  type ModelMappingEntry,
} from "../lib/modelMapping";
import { cn } from "@/lib/utils";

const DEFAULT_GROK_TEST_MODELS = [
  "grok-4.6",
  "grok-4.5",
  "grok-4",
  "grok-3-fast",
  "grok-3",
  "grok-2",
];

// 前端渲染成"限流"的账号状态集合(与 StatusBadge locale 一致)。free 账号进这些状态
// 但拿不到用量数字时,GrokUsageCell 用满格灰条兜底表意"已耗尽"。
const GROK_LIMITED_STATUSES = new Set([
  "usage_limited",
  "usage_exhausted",
  "rate_limited",
  "rate_limited_5h",
  "rate_limited_7d",
]);

// 与 Codex 账号页一致的表格/卡片双布局，选择持久化到 localStorage。
const GROK_VIEW_MODE_KEY = "codex2api:grok-accounts:view-mode";
const GROK_TABLE_COLUMNS = [
  "sequence", "plan", "proxy", "status", "requests", "usage", "models", "updatedAt",
] as const;
type GrokColumnVisibility = Record<(typeof GROK_TABLE_COLUMNS)[number], boolean>;

// 批量导入的分片大小。后端单次上限是 5000，但一次请求要串行落库/刷新几千条，
// 墙钟时间会长到浏览器或反代先断开；切成小片可以让每次请求都在一分钟量级完成，
// 同时给用户可见的进度，失败也只影响当前这一片。
const GROK_IMPORT_CHUNK_SIZE = 200;
type GrokViewMode = "table" | "grid";

function getInitialGrokViewMode(): GrokViewMode {
  try {
    const raw = window.localStorage.getItem(GROK_VIEW_MODE_KEY);
    if (raw === "grid" || raw === "table") return raw;
  } catch {
    // ignore
  }
  return "table";
}

// addMethod：Device 授权 / 粘贴 auth.json / xAI API Key / SSO 批量导入
type AddMethod = "oauth_link" | "oauth" | "api_key" | "sso";
type StatusFilter =
  | "all"
  | "active"
  | "rate_limited"
  | "disabled"
  | "banned"
  | "error";
type AuthFilter = "all" | "oauth" | "api_key";
type DeviceStep = "idle" | "waiting";
type GrokSortKey = "usage" | "requests" | "updated" | "group";
type GrokSortDir = "asc" | "desc";

const FALLBACK_GROUP_COLOR = "#2563eb";

function normalizeGroupColor(color?: string): string {
  const value = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(value) ? value : FALLBACK_GROUP_COLOR;
}

// GrokRowHandlers 是行组件的全部动作回调。父组件用 ref 转发保证引用恒定,
// 配合 memo 行组件让"勾选/单行 busy"只重渲染受影响的行。
interface GrokRowHandlers {
  toggleSelect: (id: number) => void;
  openDetail: (account: AccountRow) => void;
  test: (account: AccountRow) => void;
  usage: (account: AccountRow) => void;
  refresh: (account: AccountRow) => void;
  toggleEnabled: (account: AccountRow) => void;
  edit: (account: AccountRow) => void;
  editGroups: (account: AccountRow) => void;
  editProxy: (account: AccountRow) => void;
  remove: (account: AccountRow) => void;
  usageRefreshed: (account: AccountRow) => void;
}

function resolveAccountGroups(
  ids: number[] | undefined | null,
  groups: AccountGroup[],
): AccountGroup[] {
  if (!ids?.length || groups.length === 0) return [];
  const byID = new Map(groups.map((group) => [group.id, group]));
  return ids.map((id) => byID.get(id)).filter(Boolean) as AccountGroup[];
}

function accountUsageSortValue(account: AccountRow): number {
  const monthly = account.grok_billing?.monthly_percent;
  if (typeof monthly === "number") return monthly;
  const weekly = account.grok_billing?.weekly_percent;
  if (typeof weekly === "number") return weekly;
  if (typeof account.usage_percent_7d === "number") return account.usage_percent_7d;
  if (typeof account.usage_percent_5h === "number") return account.usage_percent_5h;
  return -1;
}

function accountRequestsSortValue(account: AccountRow): number {
  return (account.success_requests ?? 0) + (account.error_requests ?? 0);
}

function accountGroupSortKey(
  account: AccountRow,
  groups: AccountGroup[],
): string {
  const resolved = resolveAccountGroups(account.group_ids, groups);
  if (resolved.length === 0) return "\uFFFF";
  const sorted = [...resolved].sort((a, b) => {
    if (a.sort_order !== b.sort_order) return a.sort_order - b.sort_order;
    return a.name.localeCompare(b.name, "zh");
  });
  return sorted.map((g) => g.name).join("\0");
}

function GrokGroupChips({
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
            className="inline-flex max-w-[7.5rem] items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold"
            style={{
              backgroundColor: `${color}14`,
              color,
              boxShadow: `inset 0 0 0 1px ${color}33`,
            }}
            title={group.description || group.name}
          >
            <span className="size-1.5 shrink-0 rounded-full bg-current" />
            <span className="truncate">{group.name}</span>
          </span>
        );
      })}
      {hidden > 0 ? (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">
          +{hidden}
        </span>
      ) : null}
      {onClick && groups.length > 0 ? (
        <Pencil className="mt-0.5 size-3 text-muted-foreground opacity-60 transition-opacity group-hover:opacity-100" />
      ) : null}
    </>
  );
  if (onClick) {
    return (
      <button
        type="button"
        className="group flex flex-wrap items-center gap-1 text-left"
        onClick={(event) => {
          event.stopPropagation();
          onClick();
        }}
        title={emptyLabel}
      >
        {content}
      </button>
    );
  }
  return content;
}

const EMPTY_FORM: AddGrokAccountRequest = {
  auth_kind: "oauth",
  auth_json: "",
  api_key: "",
  base_url: "",
  models: [],
  proxy_url: "",
};

async function copyTextToClipboard(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.left = "-9999px";
  document.body.appendChild(ta);
  ta.select();
  document.execCommand("copy");
  document.body.removeChild(ta);
}

// Grok 官方默认上游；base_url 为默认值时列表不显示（无信息量），仅自定义上游才展示。
const GROK_DEFAULT_HOSTS = new Set([
  "cli-chat-proxy.grok.com/v1",
  "api.x.ai/v1",
]);

function shortHost(raw?: string | null): string {
  const value = (raw ?? "").trim();
  if (!value) return "";
  let host = "";
  try {
    const url = new URL(value.includes("://") ? value : `https://${value}`);
    host = url.host + (url.pathname && url.pathname !== "/" ? url.pathname.replace(/\/$/, "") : "");
  } catch {
    host = value.replace(/^https?:\/\//, "").replace(/\/$/, "");
  }
  return GROK_DEFAULT_HOSTS.has(host) ? "" : host;
}

// 套餐徽章：使用后端解析出的官方 tier 展示名；付费档琥珀，Free 绿色。
// 表格用常规尺寸、空值显示占位「—」；卡片用 compact 尺寸、空值不渲染。
function GrokPlanBadge({
  account,
  compact = false,
  className,
}: {
  account: Pick<AccountRow, "plan_type" | "grok_plan">;
  compact?: boolean;
  className?: string;
}) {
  const plan = resolveAccountGrokPlan(account);
  if (!plan) {
    return compact ? null : (
      <span className="text-[12px] text-muted-foreground">—</span>
    );
  }
  const tone = plan.paid
    ? "bg-amber-500/10 text-amber-700 ring-amber-600/20 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/20"
    : plan.key === "free"
      ? "bg-emerald-500/10 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/20 dark:text-emerald-300 dark:ring-emerald-400/20"
      : "bg-muted text-muted-foreground ring-border";
  return (
    <span
      className={cn(
        "inline-flex items-center whitespace-nowrap rounded-md ring-1 ring-inset",
        compact
          ? "px-1.5 py-0.5 text-[10px] font-semibold"
          : "px-2 py-1 text-xs font-semibold",
        tone,
        className,
      )}
    >
      {plan.display}
    </span>
  );
}

function parseModelTokens(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

function accountLabel(account: AccountRow): string {
  return account.name || account.email || `#${account.id}`;
}

function isAccountError(account: AccountRow): boolean {
  return account.status === "error" || account.status === "unauthorized";
}

function isAccountBanned(account: AccountRow): boolean {
  return account.status === "unauthorized";
}

// 限流：status 或 cooldown_reason 命中限流类状态（与 StatusBadge / 用量条一致）。
function isAccountRateLimited(account: AccountRow): boolean {
  if (isAccountBanned(account) || account.status === "error") return false;
  const status = (account.status ?? "").toLowerCase();
  const reason = (account.cooldown_reason ?? "").toLowerCase();
  return GROK_LIMITED_STATUSES.has(status) || GROK_LIMITED_STATUSES.has(reason);
}

// 「正常」：已启用、非封禁/错误、非限流。
function isAccountActive(account: AccountRow): boolean {
  return (
    account.enabled !== false &&
    !isAccountError(account) &&
    !isAccountRateLimited(account)
  );
}

function GrokAccounts({
  headerSlot,
  showOperationResults = false,
  onShowOperationResultsChange,
}: {
  // headerSlot 由账号管理页注入 Codex/Grok 顶部切换器，渲染在标题旁。
  headerSlot?: ReactNode;
  showOperationResults?: boolean;
  onShowOperationResultsChange?: (visible: boolean) => void;
} = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const navigate = useNavigate();
  // 与 Codex 账号页一致：用系统自定义确认弹窗，不用 window.confirm。
  const { confirm, confirmDialog } = useConfirmDialog();
  // 批量测试的右上角进度浮层，与 Codex 账号页共用同一实现。
  const {
    operationProgress,
    operationResults,
    runStreamingOperation,
    reportOperationEvent,
    closeOperationProgress,
    closeOperationResults,
  } = useOperationProgress(showOperationResults);

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [allGroups, setAllGroups] = useState<AccountGroup[]>([]);
  // 分组按渠道隔离(issue #487):Grok 页的分组选择器/筛选只出 grok 渠道分组;
  // 徽标解析仍用全量,迁移前挂在 codex 组里的存量成员照常显示。
  const grokGroups = useMemo(
    () => allGroups.filter((group) => group.channel === "grok"),
    [allGroups],
  );
  // 导入/添加 Grok 账号时直接绑定的分组（与 Codex 账号页共用记忆，见 useImportGroupIds）。
  const {
    groupIds: importGroupIds,
    setGroupIds: setImportGroupIds,
    prune: pruneImportGroupIds,
  } = useImportGroupIds();
  const [healthBars, setHealthBars] = useState<
    Record<string, AccountHealthBucket[]>
  >({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [totalAccounts, setTotalAccounts] = useState(0);
  const [serverSummary, setServerSummary] = useState<AccountListSummary | null>(null);
  const [statsState, setStatsState] = useState<"ready" | "stale" | "warming">("warming");
  const [disabledSorts, setDisabledSorts] = useState<string[]>([]);
  const requestAbortRef = useRef<AbortController | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);

  const [showAdd, setShowAdd] = useState(false);
  const [addMethod, setAddMethod] = useState<AddMethod>("oauth_link");
  const [form, setForm] = useState<AddGrokAccountRequest>(EMPTY_FORM);
  // 代理池条目：账号表单"从代理池选择"下拉的数据源；加载失败静默留空
  // （选择器为空时自动隐藏，不影响手动填代理）。
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  useEffect(() => {
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
  }, []);
  // 代理徽章判定上下文：fail-closed(钉住的托管代理已不在启用池)只在代理池开启时
  // 成立,所以开关与全局代理必须跟着代理池一起读进来。
  const [proxyPoolEnabled, setProxyPoolEnabled] = useState(false);
  const [globalProxyURL, setGlobalProxyURL] = useState("");
  useEffect(() => {
    let cancelled = false;
    void api
      .getSettings()
      .then((settings) => {
        if (cancelled) return;
        setProxyPoolEnabled(Boolean(settings.proxy_pool_enabled));
        setGlobalProxyURL((settings.proxy_url ?? "").trim());
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);
  const [quickProxyAccount, setQuickProxyAccount] = useState<AccountRow | null>(
    null,
  );
  // 分组用全量而非 grokGroups:后端解析组代理不看渠道,按渠道过滤会把跨渠道的
  // 存量成员误报成"无组代理"。对象引用稳定,memo 行组件才不会整表重渲。
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
  const [modelDraft, setModelDraft] = useState("");
  const [modelsLoading, setModelsLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  // SSO 批量导入：粘贴 sso token（JSON 或每行一个），后端自动转 Build 账号
  const [ssoTokens, setSsoTokens] = useState("");
  const [ssoImporting, setSsoImporting] = useState(false);
  const [ssoResult, setSsoResult] = useState<{
    total: number;
    imported: number;
    failed: number;
    items: GrokSSOImportItem[];
  } | null>(null);

  // 导入入口：选择器弹窗 + 三种来源（JSON 凭据文件 / sso.txt / refreshtoken.txt）
  const [showImportPicker, setShowImportPicker] = useState(false);
  // 采用文件内代理：勾选后 JSON 凭据文件里带的 proxy_url 生效，并自动注册进代理池。
  // sso.txt / refreshtoken.txt 一行一个 token，物理上带不了代理，只对 JSON 生效。
  const [importFileProxies, setImportFileProxies] = useState(false);
  const authFileInputRef = useRef<HTMLInputElement | null>(null);
  const ssoFileInputRef = useRef<HTMLInputElement | null>(null);
  const refreshFileInputRef = useRef<HTMLInputElement | null>(null);
  const [importBusy, setImportBusy] = useState(false);
  // 分片导入的进度（done/total 为分片数）；单片导入时为 null。
  const [importProgress, setImportProgress] = useState<{
    done: number;
    total: number;
  } | null>(null);
  const [importResult, setImportResult] = useState<{
    total: number;
    imported: number;
    failed: number;
    items: GrokSSOImportItem[];
  } | null>(null);

  // Device Code 授权：start → 展示 user_code → 自动 poll
  const [deviceStep, setDeviceStep] = useState<DeviceStep>("idle");
  const [deviceSession, setDeviceSession] = useState<{
    session_id: string;
    user_code: string;
    verification_url: string;
    interval: number;
  } | null>(null);
  const [deviceStarting, setDeviceStarting] = useState(false);
  const [devicePolling, setDevicePolling] = useState(false);
  const devicePollTimer = useRef<number | null>(null);

  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null);
  const [usageAccount, setUsageAccount] = useState<AccountRow | null>(null);
  const [quickGroupAccount, setQuickGroupAccount] = useState<AccountRow | null>(
    null,
  );
  const [quickGroupIds, setQuickGroupIds] = useState<number[]>([]);
  const [quickGroupSubmitting, setQuickGroupSubmitting] = useState(false);
  // 与 Codex 账号页一致：右侧详情 Sheet，按过滤后的列表顺序可左右切换。
  const [detailAccountId, setDetailAccountId] = useState<number | null>(null);
  const [detailAccountData, setDetailAccountData] = useState<AccountRow | null>(null);
  const [detailGrokState, setDetailGrokState] = useState<GrokAccountState | null>(null);
  const [detailGrokStateLoading, setDetailGrokStateLoading] = useState(false);
  const [detailGrokStateError, setDetailGrokStateError] = useState<string | null>(null);
  const [detailGrokAction, setDetailGrokAction] = useState<"sync" | "probe" | null>(null);
  const detailNavigationTargetRef = useRef<"first" | "last" | null>(null);
  const [batchTesting, setBatchTesting] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [exporting, setExporting] = useState(false);
  const [batchBusy, setBatchBusy] = useState(false);
  const [batchModelsOpen, setBatchModelsOpen] = useState(false);
  const [batchModelsDraft, setBatchModelsDraft] = useState<string[]>([]);
  const [batchModelInput, setBatchModelInput] = useState("");
  const selectAllRef = useRef<HTMLInputElement>(null);

  const [searchQuery, setSearchQuery] = useState("");
  const [debouncedSearchQuery, setDebouncedSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [authFilter, setAuthFilter] = useState<AuthFilter>("all");
  const [planFilter, setPlanFilter] = useState<GrokPlanFilter>("all");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(
    EMPTY_ACCOUNT_GROUP_FILTER,
  );
  const [sortKey, setSortKey] = useState<GrokSortKey | null>(null);
  const [sortDir, setSortDir] = useState<GrokSortDir>("desc");
  const [cleaning, setCleaning] = useState(false);
  const [viewMode, setViewMode] = useState<GrokViewMode>(getInitialGrokViewMode);
  const isDesktop = useIsDesktop();
  const { columns: visibleColumns, toggleColumn, resetColumns } = useAccountTableColumns(
    "codex2api:grok-accounts:visible-columns",
    GROK_TABLE_COLUMNS,
  );
  // 与 Codex 账号页一致：服务端分页 + 本地记忆每页条数。
  const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS;
  const [page, setPage] = useState(1);
  const [loadedPage, setLoadedPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize(
    "grok-accounts",
    20,
    pageSizeOptions,
  );

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDebouncedSearchQuery(searchQuery);
      setPage(1);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [searchQuery]);

  useEffect(() => {
    let cancelled = false;
    void api.listAccountGroups()
      .then((response) => {
        if (cancelled) return;
        const groups = response.groups ?? [];
        setAllGroups(groups);
        setGroupFilter((current) => pruneAccountGroupFilter(current, groups));
        pruneImportGroupIds(groups);
      })
      .catch(() => undefined);
    return () => { cancelled = true; };
  }, [pruneImportGroupIds]);

  useEffect(() => {
    try {
      window.localStorage.setItem(GROK_VIEW_MODE_KEY, viewMode);
    } catch {
      // ignore
    }
  }, [viewMode]);

  const reload = useCallback(async (options?: { silent?: boolean }) => {
    requestAbortRef.current?.abort();
    const controller = new AbortController();
    requestAbortRef.current = controller;
    // silent 供后台补拉使用:不把整页切回 loading,避免统计追平时闪烁。
    if (!options?.silent) setLoading(true);
    try {
      const res = await api.getAccountsPage({
        channel: "grok",
        page,
        pageSize,
        search: debouncedSearchQuery,
        status: statusFilter,
        authKind: authFilter,
        plan: planFilter,
        groupInclude: groupFilter.include,
        groupExclude: groupFilter.exclude,
        ungrouped: groupFilter.ungrouped,
        sort: sortKey === "updated" ? "updated_at" : sortKey ?? undefined,
        order: sortDir,
      }, controller.signal);
      if (controller.signal.aborted) return;
      const grokAccounts = (res.accounts ?? []).filter((a) => a.grok_api);
      setAccounts(grokAccounts);
      setLoadedPage(res.page);
      setTotalAccounts(res.total ?? 0);
      setServerSummary(res.summary ?? null);
      setStatsState(res.stats_state ?? "ready");
      setDisabledSorts(res.disabled_sorts ?? []);
      if (res.page !== page) setPage(res.page);
      // 选择集只保留仍然存在的账号，避免已删除账号残留在批量选择里。
      setError(null);
      setLoading(false);
      const pageIDs = grokAccounts.map((account) => account.id);
      void api.getAccountHealthBars(pageIDs)
        .then((bars) => {
          if (!controller.signal.aborted) setHealthBars(bars.buckets ?? {});
        })
        .catch(() => undefined);
      void api.getAccountPageStats(pageIDs, controller.signal)
        .then((response) => {
          if (controller.signal.aborted) return;
          setAccounts((current) => current.map((account) => {
            const stats = response.stats[String(account.id)];
            return stats ? { ...account, ...stats } : account;
          }));
        })
        .catch(() => undefined);
    } catch (err) {
      if (controller.signal.aborted) return;
      const message = getErrorMessage(err);
      setError(message);
      showToast(message, "error");
      setLoading(false);
    }
  }, [authFilter, debouncedSearchQuery, groupFilter.exclude, groupFilter.include, groupFilter.ungrouped, page, pageSize, planFilter, showToast, sortDir, sortKey, statusFilter]);

  const refreshAccountRow = useCallback(async (id: number) => {
    const account = await api.getAccount(id);
    setAccounts((current) => current.map((item) => item.id === id ? account : item));
    setDetailAccountData((current) => current?.id === id ? account : current);
    return account;
  }, []);
  const refreshAfterAccountRemoval = useCallback((ids: number[]) => {
    if (ids.length === 0) {
      void reload({ silent: true });
      return;
    }
    const drop = new Set(ids);
    let remainingOnPage = 0;
    setAccounts((current) => {
      const next = current.filter((account) => !drop.has(account.id));
      remainingOnPage = next.length;
      return next;
    });
    setTotalAccounts((current) => Math.max(0, current - ids.length));
    setServerSummary((current) =>
      current
        ? { ...current, total: Math.max(0, current.total - ids.length) }
        : current,
    );
    if (remainingOnPage === 0 && page > 1) {
      setPage(page - 1);
      return;
    }
    void reload({ silent: true });
  }, [page, reload]);
  const visibleAccountIDs = useMemo(
    () => accounts.map((account) => account.id),
    [accounts],
  );
  const applyAccountLiveState = useCallback((response: AccountLiveStateResponse) => {
    setAccounts((current) => mergeAccountLiveState(current, response));
  }, []);
  useAccountLiveState(visibleAccountIDs, applyAccountLiveState);
  const loadAccountDetail = useCallback(
    (account: AccountRow) =>
      account.detail_loaded ? Promise.resolve(account) : api.getAccount(account.id),
    [],
  );
  const openTestingAccount = useCallback((account: AccountRow) => {
    void loadAccountDetail(account)
      .then(setTestingAccount)
      .catch((error) => showToast(getErrorMessage(error), "error"));
  }, [loadAccountDetail, showToast]);

  useEffect(() => {
    void reload();
  }, [reload]);

  useEffect(() => () => requestAbortRef.current?.abort(), []);

  // stats_state 补拉:与 Codex 页同理——统计缓存两层 stale-while-revalidate,
  // 从 stale 转 ready 需要连续几次轮询;非 ready 时用 3s 短间隔静默追平,
  // 带次数上限防后端统计查询持续失败时退化成常驻轮询。
  const statsStaleRetriesRef = useRef(0);
  useEffect(() => {
    if (loading) return undefined;
    if (statsState === "ready") {
      statsStaleRetriesRef.current = 0;
      return undefined;
    }
    const maxStatsRetries = disabledSorts.includes("requests") ? 60 : 5;
    if (statsStaleRetriesRef.current >= maxStatsRetries) return undefined;
    const timer = window.setTimeout(() => {
      if (document.hidden) return;
      statsStaleRetriesRef.current += 1;
      void reload({ silent: true });
    }, 3000);
    return () => window.clearTimeout(timer);
    // accounts 作为"每次响应都会变化"的信号:连续两次都返回 stale 时
    // statsState 字符串不变,不依赖它定时器就不会被重新拉起。
  }, [accounts, disabledSorts, loading, reload, statsState]);

  // 导入/添加账号后,后端的 billing 用量探针是异步的(OAuth 号还要先刷 AT,
  // 通常 2~10s 才写回)。导入完成那一刻 reload 拿到的还是没有用量的账号,
  // 这里按梯度静默补刷几次,把探针结果自动带出来,免得用户手动刷新。
  const usageSettleTimersRef = useRef<number[]>([]);
  const scheduleUsageSettleReloads = useCallback(() => {
    usageSettleTimersRef.current.forEach((timer) => window.clearTimeout(timer));
    // 后端探针链 = 刷 AT + billing + 连通性测试(带并发闸),单号约 5~15s,
    // 批量导入时更久;30s 档兜住多数场景,更大批量由用户手动刷新或定期探测收尾。
    usageSettleTimersRef.current = [2500, 7000, 15000, 30000].map((delay) =>
      window.setTimeout(() => {
        void reload();
      }, delay),
    );
  }, [reload]);
  useEffect(
    () => () => {
      usageSettleTimersRef.current.forEach((timer) =>
        window.clearTimeout(timer),
      );
    },
    [],
  );

  const stats = {
    total: serverSummary?.total ?? totalAccounts,
    active: serverSummary?.active ?? 0,
    rateLimited: serverSummary?.rate_limited ?? 0,
    disabled: serverSummary?.disabled ?? 0,
    banned: serverSummary?.banned ?? 0,
    errorOnly: serverSummary?.error ?? 0,
    oauth: serverSummary?.oauth ?? 0,
    apiKey: serverSummary?.api_key ?? 0,
  };

  // 服务端已完成全池筛选、排序和分页；浏览器只渲染当前页。
  const sortedAccounts = accounts;
  const pagedAccounts = accounts;
  const currentGrokSelector = useMemo<AccountOperationSelector>(() => ({
    channel: "grok",
    search: debouncedSearchQuery || undefined,
    status: statusFilter === "all" ? undefined : statusFilter,
    auth_kind: authFilter === "all" ? undefined : authFilter,
    plan: planFilter === "all" ? undefined : planFilter,
    group_include: groupFilter.include.length > 0 ? groupFilter.include : undefined,
    group_exclude: groupFilter.exclude.length > 0 ? groupFilter.exclude : undefined,
    ungrouped: groupFilter.ungrouped || undefined,
  }), [authFilter, debouncedSearchQuery, groupFilter.exclude, groupFilter.include, groupFilter.ungrouped, planFilter, statusFilter]);
  const hasActiveGrokFilters = Boolean(
    debouncedSearchQuery ||
      statusFilter !== "all" ||
      authFilter !== "all" ||
      planFilter !== "all" ||
      !isAccountGroupFilterEmpty(groupFilter),
  );

  // 筛选/排序变化时回到第 1 页，避免停留在空页。
  useEffect(() => {
    setPage(1);
  }, [authFilter, groupFilter, planFilter, sortDir, sortKey, statusFilter]);

  const totalPages = Math.max(1, Math.ceil(totalAccounts / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pagedAccountIds = useMemo(
    () => pagedAccounts.map((a) => a.id),
    [pagedAccounts],
  );

  // 批量选择：表头全选仅作用于当前页（与 Codex 账号页一致）。
  const pageSelectedCount = useMemo(
    () => pagedAccountIds.reduce((n, id) => n + (selected.has(id) ? 1 : 0), 0),
    [pagedAccountIds, selected],
  );
  const allPageSelected =
    pagedAccountIds.length > 0 && pageSelectedCount === pagedAccountIds.length;
  const somePageSelected = pageSelectedCount > 0 && !allPageSelected;

  const detailListAccount = useMemo(
    () =>
      detailAccountId == null
        ? null
        : (accounts.find((a) => a.id === detailAccountId) ?? null),
    [accounts, detailAccountId],
  );
  const detailAccount =
    detailAccountData?.id === detailAccountId
      ? detailAccountData
      : detailListAccount;
  const detailNavIndex = useMemo(() => {
    if (detailAccountId == null) return -1;
    return sortedAccounts.findIndex((a) => a.id === detailAccountId);
  }, [detailAccountId, sortedAccounts]);
  const openAccountDetail = useCallback((account: AccountRow) => {
    setDetailAccountData(account);
    setDetailAccountId(account.id);
  }, []);
  const closeAccountDetail = useCallback(() => {
    setDetailAccountId(null);
    setDetailAccountData(null);
    setDetailGrokState(null);
    setDetailGrokStateError(null);
  }, []);
  const goDetailPrev = useCallback(() => {
    if (detailNavIndex > 0) {
      const target = sortedAccounts[detailNavIndex - 1] ?? null;
      setDetailAccountData(target);
      setDetailAccountId(target?.id ?? null);
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
      return;
    }
    if (currentPage < totalPages) {
      detailNavigationTargetRef.current = "first";
      setPage(currentPage + 1);
    }
  }, [currentPage, detailNavIndex, sortedAccounts, totalPages]);

  const resolvedDisabledSorts = useMemo(
    () => resolveDisabledAccountSorts(disabledSorts, serverSummary?.total ?? totalAccounts),
    [disabledSorts, serverSummary?.total, totalAccounts],
  );
  const usageSortBlocked = isLargePoolSortDisabled("requests", resolvedDisabledSorts);
  const toggleSort = useCallback((key: GrokSortKey) => {
    if (isLargePoolSortDisabled(key, resolvedDisabledSorts)) {
      showToast(t("accounts.largePoolSortDisabled"), "warning");
      return;
    }
    setSortKey((current) => {
      if (current === key) {
        setSortDir((dir) => (dir === "desc" ? "asc" : "desc"));
        return current;
      }
      setSortDir(key === "group" || key === "updated" ? "asc" : "desc");
      return key;
    });
  }, [resolvedDisabledSorts, showToast, t]);
  // 禁用窗口(冷启动首轮聚合)只拦截新的排序点击,不清用户已选的排序:
  // 后端会把被禁用的排序静默降级成默认序,聚合完成后自动按原选择恢复。

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

  // Grok 控制面状态可能包含目录与能力明细，列表接口保持轻量；只有打开详情时才按需读取。
  useEffect(() => {
    if (detailAccountId == null) {
      setDetailGrokState(null);
      setDetailGrokStateError(null);
      setDetailGrokStateLoading(false);
      return undefined;
    }
    const controller = new AbortController();
    setDetailGrokState(null);
    setDetailGrokStateError(null);
    setDetailGrokStateLoading(true);
    void api.getGrokAccountState(detailAccountId, controller.signal)
      .then((state) => {
        if (!controller.signal.aborted) setDetailGrokState(state);
      })
      .catch((error) => {
        if (!controller.signal.aborted) setDetailGrokStateError(getErrorMessage(error));
      })
      .finally(() => {
        if (!controller.signal.aborted) setDetailGrokStateLoading(false);
      });
    return () => controller.abort();
  }, [detailAccountId]);

  const syncDetailGrokState = useCallback(async () => {
    if (detailAccountId == null || detailGrokAction) return;
    setDetailGrokAction("sync");
    setDetailGrokStateError(null);
    try {
      const result = await api.syncGrokAccountState(detailAccountId);
      setDetailGrokState(result.state);
      showToast(t("grok.stateSyncDone", { count: result.models?.length ?? 0 }));
      await refreshAccountRow(detailAccountId);
    } catch (error) {
      const message = getErrorMessage(error);
      setDetailGrokStateError(message);
      showToast(message, "error");
    } finally {
      setDetailGrokAction(null);
    }
  }, [detailAccountId, detailGrokAction, refreshAccountRow, showToast, t]);

  const probeDetailGrokCapabilities = useCallback(async () => {
    if (detailAccountId == null || detailGrokAction) return;
    setDetailGrokAction("probe");
    setDetailGrokStateError(null);
    try {
      const result = await api.probeGrokAccountCapabilities(detailAccountId);
      setDetailGrokState(result.state);
      showToast(t("grok.capabilityProbeDone", { count: result.results?.length ?? 0 }));
    } catch (error) {
      const message = getErrorMessage(error);
      setDetailGrokStateError(message);
      showToast(message, "error");
    } finally {
      setDetailGrokAction(null);
    }
  }, [detailAccountId, detailGrokAction, showToast, t]);

  useEffect(() => {
    const target = detailNavigationTargetRef.current;
    if (!target || loadedPage !== page || accounts.length === 0) return;
    const account = target === "first" ? accounts[0] : accounts[accounts.length - 1];
    detailNavigationTargetRef.current = null;
    setDetailAccountData(account ?? null);
    setDetailAccountId(account?.id ?? null);
  }, [accounts, loadedPage, page]);

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
    setSelected((prev) => {
      const next = new Set(prev);
      if (allPageSelected) {
        for (const id of pagedAccountIds) next.delete(id);
      } else {
        for (const id of pagedAccountIds) next.add(id);
      }
      return next;
    });
  }, [allPageSelected, pagedAccountIds]);

  const clearSelection = useCallback(() => setSelected(new Set()), []);

  // 行回调经由 ref 转发:rowHandlers 引用恒定(memo 行不因父组件重渲染而失效),
  // ref.current 每次渲染刷新,始终指向最新的处理函数。
  const rowHandlersRef = useRef<GrokRowHandlers>(null as unknown as GrokRowHandlers);
  rowHandlersRef.current = {
    toggleSelect,
    openDetail: openAccountDetail,
    test: (account) => openTestingAccount(account),
    usage: (account) => setUsageAccount(account),
    refresh: (account) => void handleRefresh(account),
    toggleEnabled: (account) => void handleToggleEnabled(account),
    // openEdit/handleRefresh 等在组件体更靠后定义,这里一律用闭包延迟取值,避开 TDZ。
    edit: (account) => openEdit(account),
    editGroups: (account) => openQuickGroupEditor(account),
    editProxy: (account) => setQuickProxyAccount(account),
    remove: (account) => void handleDelete(account),
    usageRefreshed: (account) => {
      void refreshAccountRow(account.id);
    },
  };
  const rowHandlers = useMemo<GrokRowHandlers>(
    () => ({
      toggleSelect: (id) => rowHandlersRef.current.toggleSelect(id),
      openDetail: (account) => rowHandlersRef.current.openDetail(account),
      test: (account) => rowHandlersRef.current.test(account),
      usage: (account) => rowHandlersRef.current.usage(account),
      refresh: (account) => rowHandlersRef.current.refresh(account),
      toggleEnabled: (account) => rowHandlersRef.current.toggleEnabled(account),
      edit: (account) => rowHandlersRef.current.edit(account),
      editGroups: (account) => rowHandlersRef.current.editGroups(account),
      editProxy: (account) => rowHandlersRef.current.editProxy(account),
      remove: (account) => rowHandlersRef.current.remove(account),
      usageRefreshed: (account) => rowHandlersRef.current.usageRefreshed(account),
    }),
    [],
  );

  // 快速设置账号分组(issue #487):Grok 账号导入后也能补挂/调整分组,
  // 与 Codex 账号页同一交互——点行内分组徽标打开,保存走 scheduler 接口。
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
      await reload();
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

  const credentialReady =
    addMethod === "api_key"
      ? Boolean(form.api_key?.trim())
      : addMethod === "oauth"
        ? Boolean(form.auth_json?.trim())
        : false;

  const stopDevicePoll = useCallback(() => {
    if (devicePollTimer.current != null) {
      window.clearTimeout(devicePollTimer.current);
      devicePollTimer.current = null;
    }
    setDevicePolling(false);
  }, []);

  const resetAddForm = () => {
    stopDevicePoll();
    setForm(EMPTY_FORM);
    setModelDraft("");
    setAddMethod("oauth_link");
    setDeviceStep("idle");
    setDeviceSession(null);
    setDeviceStarting(false);
    setSsoTokens("");
    setSsoResult(null);
    setSsoImporting(false);
  };

  useEffect(() => () => stopDevicePoll(), [stopDevicePoll]);

  const addModels = (raw: string) => {
    const tokens = parseModelTokens(raw);
    if (tokens.length === 0) return;
    setForm((f) => {
      const seen = new Set((f.models ?? []).map((m) => m.toLowerCase()));
      const merged = [...(f.models ?? [])];
      for (const tok of tokens) {
        if (!seen.has(tok.toLowerCase())) {
          seen.add(tok.toLowerCase());
          merged.push(tok);
        }
      }
      return { ...f, models: merged };
    });
    setModelDraft("");
  };

  const removeModel = (model: string) =>
    setForm((f) => ({
      ...f,
      models: (f.models ?? []).filter((m) => m !== model),
    }));

  const handleFetchModels = async () => {
    if (!credentialReady) return;
    setModelsLoading(true);
    try {
      const payload: AddGrokAccountRequest = {
        ...form,
        auth_kind: addMethod === "api_key" ? "api_key" : "oauth",
      };
      const res = await api.fetchGrokModels(payload);
      setForm((f) => ({ ...f, models: res.models ?? [] }));
      showToast(t("grok.modelsFetched", { count: (res.models ?? []).length }));
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setModelsLoading(false);
    }
  };

  // 编辑已存在的 Grok 账号：声明模型白名单 / base_url / 代理 / 映射。
  // 后端 UpdateGrokAccount 会整体重写这几项，所以表单需回填当前值再整体提交，避免清空。
  const [editAccount, setEditAccount] = useState<AccountRow | null>(null);
  const [editForm, setEditForm] = useState<{
    models: string[];
    base_url: string;
    proxy_url: string;
  }>({ models: [], base_url: "", proxy_url: "" });
  const [editModelMappingEntries, setEditModelMappingEntries] = useState<
    ModelMappingEntry[]
  >(emptyModelMappingEntries);
  const [editModelDraft, setEditModelDraft] = useState("");
  const [editSubmitting, setEditSubmitting] = useState(false);

  const populateEdit = (account: AccountRow) => {
    setEditAccount(account);
    setEditForm({
      models: account.models ?? [],
      base_url: account.base_url ?? "",
      proxy_url: account.proxy_url ?? "",
    });
    const parsedMapping = parseModelMappingEntries(account.model_mapping ?? "");
    setEditModelMappingEntries(
      parsedMapping.ok ? parsedMapping.entries : emptyModelMappingEntries(),
    );
    setEditModelDraft("");
  };

  const openEdit = (account: AccountRow) => {
    void loadAccountDetail(account)
      .then(populateEdit)
      .catch((error) => showToast(getErrorMessage(error), "error"));
  };

  const mergeModels = (existing: string[], incoming: string[]): string[] => {
    const seen = new Set(existing.map((m) => m.toLowerCase()));
    const merged = [...existing];
    for (const tok of incoming) {
      if (!seen.has(tok.toLowerCase())) {
        seen.add(tok.toLowerCase());
        merged.push(tok);
      }
    }
    return merged;
  };

  const editAddModels = (raw: string) => {
    const tokens = parseModelTokens(raw);
    if (tokens.length === 0) return;
    setEditForm((f) => ({ ...f, models: mergeModels(f.models, tokens) }));
    setEditModelDraft("");
  };

  const editRemoveModel = (model: string) =>
    setEditForm((f) => ({ ...f, models: f.models.filter((m) => m !== model) }));

  const updateEditModelMapping = (
    index: number,
    field: keyof ModelMappingEntry,
    value: string,
  ) =>
    setEditModelMappingEntries((entries) =>
      entries.map((entry, entryIndex) =>
        entryIndex === index ? { ...entry, [field]: value } : entry,
      ),
    );

  const removeEditModelMapping = (index: number) =>
    setEditModelMappingEntries((entries) => {
      const next = entries.filter((_, entryIndex) => entryIndex !== index);
      return next.length > 0 ? next : emptyModelMappingEntries();
    });

  const editFillCommonModels = () =>
    setEditForm((f) => ({
      ...f,
      models: mergeModels(f.models, DEFAULT_GROK_TEST_MODELS),
    }));

  const handleSaveEdit = async () => {
    if (!editAccount) return;
    const modelMapping = serializeModelMappingEntries(editModelMappingEntries);
    if (!modelMapping.ok) {
      showToast(t("grok.modelMappingInvalid"), "error");
      return;
    }
    setEditSubmitting(true);
    try {
      const isApiKey = editAccount.grok_auth_kind === "api_key";
      await api.updateGrokAccount(editAccount.id, {
        auth_kind: (editAccount.grok_auth_kind ??
          "oauth") as AddGrokAccountRequest["auth_kind"],
        models: editForm.models,
        // OAuth 端点固定官方 cli-chat-proxy，Base URL 字段已隐藏；提交空值交
        // 后端规整为默认，避免持久化用户已看不到的自定义值。
        base_url: isApiKey ? editForm.base_url.trim() : "",
        model_mapping: modelMapping.value,
        proxy_url: editForm.proxy_url.trim(),
      });
      showToast(t("grok.editSaved"));
      setEditAccount(null);
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setEditSubmitting(false);
    }
  };

  const handleAdd = async () => {
    if (addMethod === "oauth_link") return;
    if (!credentialReady) return;
    setSubmitting(true);
    try {
      const isApiKey = addMethod === "api_key";
      await api.addGrokAccount({
        ...form,
        // OAuth 走固定官方端点，忽略表单里可能残留的自定义 base_url
        // （用户从 API Key 模式切过来时不再上送）。
        base_url: isApiKey ? form.base_url : "",
        auth_kind: isApiKey ? "api_key" : "oauth",
        group_ids: importGroupIds,
      });
      showToast(t("grok.addSuccess"));
      setShowAdd(false);
      resetAddForm();
      void reload();
      scheduleUsageSettleReloads();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSubmitting(false);
    }
  };

  const handleImportSSO = async () => {
    if (!ssoTokens.trim()) return;
    setSsoImporting(true);
    setSsoResult(null);
    try {
      const res = await api.importGrokSSO({
        tokens: ssoTokens,
        base_url: form.base_url?.trim() || undefined,
        models: form.models?.length ? form.models : undefined,
        proxy_url: form.proxy_url?.trim() || undefined,
        group_ids: importGroupIds,
      });
      setSsoResult(res);
      if (res.imported > 0) {
        showToast(t("grok.ssoImportDone", { imported: res.imported, total: res.total }));
        void reload();
        scheduleUsageSettleReloads();
      }
      if (res.imported === res.total) {
        // 全部成功：清空输入，方便继续导入下一批
        setSsoTokens("");
      }
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSsoImporting(false);
    }
  };

  // GrokImportChunkResult 覆盖三个导入端点的共同响应形态。代理三项只有 JSON 凭据
  // 文件那条链路会返回（且必须开了「采用文件内代理」），其余端点恒为 undefined。
  type GrokImportChunkResult = {
    total: number;
    imported: number;
    failed: number;
    items: GrokSSOImportItem[];
    proxies_imported?: number;
    proxies_skipped?: number;
    proxy_warning?: string;
  };

  // runImport 统一跑一次导入调用：置忙、展示结果、成功后刷新列表。
  const runImport = async (
    fn: () => Promise<GrokImportChunkResult>,
    totalItems = 0,
  ) => runImportChunks([fn], totalItems);

  // runImportChunks 按分片顺序调用导入接口并合并结果。
  // 分片是为了避开中间层超时：后端单次上限已放宽到 5000，但一个请求要串行落库
  // 几千条，墙钟时间会长到浏览器/反代先断开，用户既看不到进度也不知道进了多少。
  // 每片结束就把已合并的结果写回去，中途失败也能看到前面那些片的明细。
  // totalItems 是全部待导入条数(调用方按文件/行数预先算好),用于右上角进度浮层
  // (与 Codex 账号页批量操作同款);传 0 则进度条按分片完成时的累计条数走。
  const runImportChunks = async (
    chunks: Array<() => Promise<GrokImportChunkResult>>,
    totalItems = 0,
  ) => {
    if (chunks.length === 0) return;
    setImportBusy(true);
    setImportResult(null);
    setShowImportPicker(false);
    setImportProgress(
      chunks.length > 1 ? { done: 0, total: chunks.length } : null,
    );
    const progressTitle = t("grok.importProgressTitle");
    reportOperationEvent(progressTitle, {
      type: "start",
      action: "grok_import",
      current: 0,
      total: totalItems,
    });
    const merged = {
      total: 0,
      imported: 0,
      failed: 0,
      items: [] as GrokSSOImportItem[],
    };
    // 代理注册结果按分片累加。carried 只有在后端确实处理了这个开关时才为真
    // （响应里出现代理计数），据此决定收尾 toast 要不要带代理那段。
    const proxySummary = {
      carried: false,
      imported: 0,
      skipped: 0,
      warnings: [] as string[],
    };
    const reportMerged = (type: "progress" | "complete", error?: string) =>
      reportOperationEvent(progressTitle, {
        type,
        action: "grok_import",
        current: merged.total,
        total: Math.max(totalItems, merged.total),
        success: merged.imported,
        failed: merged.failed,
        error,
      });
    try {
      for (let i = 0; i < chunks.length; i++) {
        const res = await chunks[i]();
        merged.total += res.total ?? 0;
        merged.imported += res.imported ?? 0;
        merged.failed += res.failed ?? 0;
        merged.items = merged.items.concat(res.items ?? []);
        if (res.proxies_imported !== undefined) {
          proxySummary.carried = true;
          proxySummary.imported += res.proxies_imported;
          proxySummary.skipped += res.proxies_skipped ?? 0;
          // 分片之间的告警多半一模一样(同一批文件),去重后再拼。
          const warning = res.proxy_warning?.trim();
          if (warning && !proxySummary.warnings.includes(warning)) {
            proxySummary.warnings.push(warning);
          }
        }
        if (chunks.length > 1) {
          setImportProgress({ done: i + 1, total: chunks.length });
        }
        reportMerged(i === chunks.length - 1 ? "complete" : "progress");
      }
      // 全部成功时不再弹明细弹窗(右上角进度浮层已给出结果);
      // 有失败才弹,保留逐号失败原因供排查。命中既有身份被合并/复活的条目
      // 也要弹明细——否则"导入成功却没多出新账号"会让人以为导入没生效。
      if (
        merged.failed > 0 ||
        merged.items.some((item) => item.updated || item.revived)
      ) {
        setImportResult({ ...merged, items: [...merged.items] });
      }
      if (merged.imported > 0) {
        const done = t("grok.fileImportDone", {
          imported: merged.imported,
          total: merged.total,
        });
        // Toast 只有一个槽位,连续调用会互相顶掉——代理结果和告警必须拼进同一条。
        if (proxySummary.carried) {
          const parts = [
            done,
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
          showToast(done);
        }
        void reload();
        scheduleUsageSettleReloads();
      }
    } catch (err) {
      const message = getErrorMessage(err);
      showToast(message, "error");
      // 中途失败:浮层收尾并带上错误,已完成分片的结果留在弹窗里。
      reportMerged("complete", message);
      if (merged.total > 0) {
        setImportResult({ ...merged, items: [...merged.items] });
      }
      if (merged.imported > 0) {
        void reload();
        scheduleUsageSettleReloads();
      }
    } finally {
      setImportBusy(false);
      setImportProgress(null);
    }
  };

  // chunkList 把待导入项切成固定大小的分片。
  const chunkList = <T,>(items: T[], size: number): T[][] => {
    const chunks: T[][] = [];
    for (let i = 0; i < items.length; i += size) {
      chunks.push(items.slice(i, i + size));
    }
    return chunks;
  };

  // JSON 凭据文件（CPA / auth.json，可多选）
  const handleImportAuthFiles = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    const files = await Promise.all(
      Array.from(fileList).map((file) => file.text()),
    );
    if (authFileInputRef.current) authFileInputRef.current.value = "";
    await runImportChunks(
      chunkList(files, GROK_IMPORT_CHUNK_SIZE).map(
        (part) => () =>
          api.batchImportGrokAccounts({
            files: part,
            group_ids: importGroupIds,
            import_proxy: importFileProxies || undefined,
          }),
      ),
      files.length,
    );
  };

  // countImportLines 统计文本导入的有效行数(空行/注释行不算),供进度条总数。
  const countImportLines = (text: string) =>
    text
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter((line) => line !== "" && !line.startsWith("#")).length;

  // sso.txt（每行一个 sso token，自动转 Build 账号）
  const handleImportSsoFile = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    const text = await fileList[0].text();
    if (ssoFileInputRef.current) ssoFileInputRef.current.value = "";
    await runImport(
      () => api.importGrokSSO({ tokens: text, group_ids: importGroupIds }),
      countImportLines(text),
    );
  };

  // refreshtoken.txt（每行一个 refresh_token，刷出 OAuth 账号）
  const handleImportRefreshFile = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    const text = await fileList[0].text();
    if (refreshFileInputRef.current) refreshFileInputRef.current.value = "";
    const lines = text
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter((line) => line !== "" && !line.startsWith("#"));
    if (lines.length === 0) {
      await runImport(() =>
        api.importGrokRefreshTokens({ tokens: text, group_ids: importGroupIds }),
      );
      return;
    }
    await runImportChunks(
      chunkList(lines, GROK_IMPORT_CHUNK_SIZE).map(
        (part) => () =>
          api.importGrokRefreshTokens({
            tokens: part.join("\n"),
            group_ids: importGroupIds,
          }),
      ),
      lines.length,
    );
  };

  const scheduleDevicePoll = useCallback(
    (sessionId: string, intervalSec: number) => {
      stopDevicePoll();
      const delay = Math.max(3, intervalSec) * 1000;
      devicePollTimer.current = window.setTimeout(() => {
        void (async () => {
          setDevicePolling(true);
          try {
            const result = await api.pollGrokDeviceAuth({
              session_id: sessionId,
              proxy_url: form.proxy_url?.trim() || undefined,
              name: form.name?.trim() || undefined,
            });
            if (result.status === "authorized") {
              stopDevicePoll();
              showToast(
                result.email
                  ? t("grok.oauthSuccess", { email: result.email })
                  : t("grok.addSuccess"),
              );
              setShowAdd(false);
              resetAddForm();
              void reload();
              scheduleUsageSettleReloads();
              return;
            }
            // pending — continue
            const nextInterval =
              result.slow_down
                ? Math.max(intervalSec + 5, 10)
                : result.interval ?? intervalSec;
            setDeviceSession((prev) =>
              prev
                ? { ...prev, interval: nextInterval, user_code: result.user_code || prev.user_code }
                : prev,
            );
            scheduleDevicePoll(sessionId, nextInterval);
          } catch (err) {
            stopDevicePoll();
            showToast(getErrorMessage(err), "error");
            setDeviceStep("idle");
            setDeviceSession(null);
          } finally {
            setDevicePolling(false);
          }
        })();
      }, delay);
    },
    [form.name, form.proxy_url, reload, scheduleUsageSettleReloads, showToast, stopDevicePoll, t],
  );

  const handleDeviceStart = async () => {
    setDeviceStarting(true);
    stopDevicePoll();
    try {
      const result = await api.startGrokDeviceAuth({
        proxy_url: form.proxy_url?.trim() || undefined,
        name: form.name?.trim() || undefined,
        base_url: form.base_url?.trim() || undefined,
        models: form.models?.length ? form.models : undefined,
      });
      const session = {
        session_id: result.session_id,
        user_code: result.user_code,
        verification_url: result.verification_url,
        interval: result.interval || 5,
      };
      setDeviceSession(session);
      setDeviceStep("waiting");
      // 自动打开验证页
      window.open(result.verification_url, "_blank", "noopener,noreferrer");
      scheduleDevicePoll(session.session_id, session.interval);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setDeviceStarting(false);
    }
  };

  const handleDeviceCopyCode = async () => {
    if (!deviceSession?.user_code) return;
    try {
      await copyTextToClipboard(deviceSession.user_code);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };

  const handleDeviceRestart = async () => {
    stopDevicePoll();
    setDeviceSession(null);
    setDeviceStep("idle");
    await handleDeviceStart();
  };

  const handleToggleEnabled = async (account: AccountRow) => {
    setBusyId(account.id);
    const next = account.enabled === false;
    try {
      await api.toggleAccountEnabled(account.id, next);
      await refreshAccountRow(account.id);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleRefresh = async (account: AccountRow) => {
    setBusyId(account.id);
    try {
      await api.refreshAccount(account.id);
      showToast(t("grok.refreshDone"));
      await refreshAccountRow(account.id);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleDelete = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t("grok.deleteTitle"),
      description: t("grok.deleteDesc", {
        account: account.email || account.name || `ID ${account.id}`,
      }),
      confirmText: t("grok.deleteConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBusyId(account.id);
    try {
      await api.deleteAccount(account.id);
      if (detailAccountId === account.id) setDetailAccountId(null);
      refreshAfterAccountRemoval([account.id]);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleToggleLock = async (account: AccountRow) => {
    setBusyId(account.id);
    const next = !account.locked;
    try {
      await api.toggleAccountLock(account.id, next);
      showToast(next ? t("accounts.lockSuccess") : t("accounts.unlockSuccess"));
      await refreshAccountRow(account.id);
    } catch (err) {
      showToast(
        t("accounts.lockFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBusyId(null);
    }
  };

  const handleResetStatus = async (account: AccountRow) => {
    setBusyId(account.id);
    try {
      await api.resetAccountStatus(account.id);
      showToast(t("accounts.resetStatusSuccess"));
      await refreshAccountRow(account.id);
    } catch (err) {
      showToast(
        t("accounts.resetStatusFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBusyId(null);
    }
  };

  // 导出 Grok 账号凭据：有勾选则导出选中，否则导出全部。
  // 单账号后端给裸 JSON，多账号给 ZIP（内部每账号一个 <邮箱>.json）。
  // 文件名取服务端 Content-Disposition，命名规则只在后端一处维护。
  const handleExport = async () => {
    if (accounts.length === 0) return;
    const ids = selected.size > 0 ? Array.from(selected) : undefined;
    setExporting(true);
    try {
      const { blob, filename } = await api.exportGrokAccounts(ids);
    const count = ids ? ids.length : totalAccounts;
      const fallback = `codex2api-grok-${new Date()
        .toISOString()
        .replace(/[:.]/g, "-")
        .slice(0, 19)}-${count}.${blob.type.includes("zip") ? "zip" : "json"}`;
      downloadBlob(blob, filename || fallback);
      showToast(t("grok.exportSuccess", { count }));
    } catch (err) {
      showToast(
        t("grok.exportFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setExporting(false);
    }
  };

  const handleBatchTest = async (testIds?: number[]) => {
    if (totalAccounts === 0 && selected.size === 0 && !testIds?.length) return;

    // 必须显式传 ids，否则后端会连 Codex 账号一起测。
    // 范围优先级：显式 ids → 已选 → 当前筛选 → 全部（后两者要确认）。
    let ids: number[] | null = null;
    if (testIds && testIds.length > 0) {
      ids = testIds;
    } else if (selected.size > 0) {
      ids = Array.from(selected);
    } else if (hasActiveGrokFilters) {
      const confirmed = await confirm({
        title: t("grok.batchTestFilteredTitle"),
        description: t("grok.batchTestFilteredDesc", {
          count: totalAccounts,
        }),
        confirmText: t("accounts.batchTest"),
      });
      if (!confirmed) return;
    } else {
      const confirmed = await confirm({
        title: t("grok.batchTestAllTitle"),
        description: t("grok.batchTestAllDesc", { count: totalAccounts }),
        confirmText: t("accounts.batchTest"),
        tone: "destructive",
        confirmVariant: "destructive",
      });
      if (!confirmed) return;
    }
    if (ids && ids.length === 0) return;

    setBatchTesting(true);
    try {
      const result = await runStreamingOperation(
        "/accounts/batch-test?stream=true",
        ids ? { ids } : { selector: currentGrokSelector },
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
      await reload();
    } catch (err) {
      showToast(
        t("accounts.batchTestFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBatchTesting(false);
    }
  };

  const selectedIds = useMemo(() => Array.from(selected), [selected]);

  const handleBatchRefresh = async () => {
    // Selected IDs are retained across pages; the server validates stale IDs.
    // API-key accounts cannot refresh and are reported as failures by the job.
    if (selectedIds.length === 0) {
      showToast(t("grok.batchNoOAuth"), "error");
      return;
    }
    setBatchBusy(true);
    try {
      const res = await api.batchRefreshAccounts(selectedIds);
      showToast(
        t("accounts.batchRefreshDone", {
          success: res.success ?? 0,
          fail: res.failed ?? 0,
        }),
      );
      clearSelection();
      await reload();
    } catch (err) {
      showToast(
        t("accounts.batchRefreshFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBatchBusy(false);
    }
  };

  const handleBatchResetStatus = async () => {
    if (selectedIds.length === 0) return;
    setBatchBusy(true);
    try {
      const res = await api.batchResetStatus(selectedIds);
      showToast(
        t("accounts.batchResetStatusDone", {
          success: res.success ?? 0,
          fail: res.failed ?? 0,
        }),
      );
      clearSelection();
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBatchBusy(false);
    }
  };

  const openBatchSetModels = () => {
    if (selectedIds.length === 0) return;
    setBatchModelsDraft([]);
    setBatchModelInput("");
    setBatchModelsOpen(true);
  };

  const batchAddModels = (raw: string) => {
    const tokens = parseModelTokens(raw);
    if (tokens.length === 0) return;
    setBatchModelsDraft((current) => mergeModels(current, tokens));
    setBatchModelInput("");
  };

  const batchRemoveModel = (model: string) =>
    setBatchModelsDraft((current) => current.filter((item) => item !== model));

  const batchTogglePreset = (model: string) => {
    setBatchModelsDraft((current) => {
      const exists = current.some(
        (item) => item.toLowerCase() === model.toLowerCase(),
      );
      if (exists) {
        return current.filter(
          (item) => item.toLowerCase() !== model.toLowerCase(),
        );
      }
      return mergeModels(current, [model]);
    });
  };

  const handleBatchSetModels = async () => {
    if (selectedIds.length === 0) return;
    setBatchBusy(true);
    try {
      const payload: BatchUpdateGrokModelsRequest = {
        ids: selectedIds,
        models: batchModelsDraft,
      };
      const res = await api.batchUpdateGrokModels(payload);
      showToast(
        t("grok.batchSetModelsDone", {
          success: res.success ?? 0,
          fail: res.failed ?? 0,
        }),
      );
      setBatchModelsOpen(false);
      clearSelection();
      await reload();
    } catch (err) {
      showToast(
        t("grok.batchSetModelsFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBatchBusy(false);
    }
  };

  const handleBatchEnabled = async (enabled: boolean) => {
    if (selectedIds.length === 0) return;
    setBatchBusy(true);
    try {
      await api.batchUpdateAccounts({ ids: selectedIds, enabled });
      clearSelection();
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBatchBusy(false);
    }
  };

  const handleBatchDelete = async () => {
    if (selectedIds.length === 0) return;
    const confirmed = await confirm({
      title: t("accounts.batchDeleteTitle"),
      description: t("accounts.batchDeleteDesc", { count: selectedIds.length }),
      confirmText: t("accounts.deleteConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchBusy(true);
    try {
      const res = await api.batchDeleteAccounts(selectedIds);
      showToast(
        t("accounts.batchDeleteDone", {
          success: res.success ?? res.deleted ?? 0,
          fail: res.failed ?? 0,
        }),
      );
      clearSelection();
      await reload({ silent: true });
    } catch (err) {
      showToast(
        t("accounts.batchDeleteFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBatchBusy(false);
    }
  };

  // 一键清理封禁/错误账号：仅作用于 Grok 账号，走 grok 专用端点。
  const handleCleanBanned = async () => {
    const confirmed = await confirm({
      title: t("grok.cleanBannedTitle"),
      description: t("grok.cleanBannedDesc", { count: stats.banned }),
      confirmText: t("grok.cleanConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setCleaning(true);
    try {
      const result = await runStreamingOperation(
        "/accounts/grok/clean-banned?stream=true",
        undefined,
        t("grok.cleanBannedProgressTitle"),
      );
      showToast(t("grok.cleanDone", { count: result?.deleted ?? result?.success ?? 0 }));
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setCleaning(false);
    }
  };

  const handleCleanError = async () => {
    const confirmed = await confirm({
      title: t("grok.cleanErrorTitle"),
      description: t("grok.cleanErrorDesc", { count: stats.errorOnly }),
      confirmText: t("grok.cleanConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setCleaning(true);
    try {
      const result = await runStreamingOperation(
        "/accounts/grok/clean-error?stream=true",
        undefined,
        t("grok.cleanErrorProgressTitle"),
      );
      showToast(t("grok.cleanDone", { count: result?.deleted ?? result?.success ?? 0 }));
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setCleaning(false);
    }
  };

  return (
    <div className="relative @container/accounts">
      <OperationProgressToast
        progress={operationProgress}
        onClose={closeOperationProgress}
      />
      <OperationResultsModal
        state={showOperationResults ? operationResults : null}
        accounts={accounts}
        channel="grok"
        onClose={closeOperationResults}
      />
      <StateShell
        variant="page"
        loading={loading && accounts.length === 0}
        error={accounts.length === 0 ? error : null}
        onRetry={() => void reload()}
        loadingTitle={t("grok.loadingTitle")}
        loadingDescription={t("grok.loadingDesc")}
        errorTitle={t("grok.errorTitle")}
      >
        <PageHeader
          title={t("grok.pageTitle")}
          description={t("grok.pageSubtitle")}
          onRefresh={() => void reload()}
          hideTitle
          actionsBelow
          titleAdornment={headerSlot}
          actions={
            <div className="flex flex-wrap items-center gap-1.5">
              <Button size="sm" onClick={() => setShowAdd(true)}>
                <Plus className="size-3.5" />
                {t("grok.addAccount")}
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={importBusy}
                onClick={() => setShowImportPicker(true)}
              >
                {importBusy ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <Upload className="size-3.5" />
                )}
                <span className="hidden sm:inline">
                  {importBusy
                    ? importProgress
                      ? t("grok.fileImportProgress", {
                          done: importProgress.done,
                          total: importProgress.total,
                        })
                      : t("grok.fileImporting")
                    : t("grok.fileImportBtn")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchTesting || totalAccounts === 0}
                onClick={() => void handleBatchTest()}
              >
                <FlaskConical
                  className={cn("size-3.5", batchTesting && "animate-pulse")}
                />
                <span className="hidden sm:inline">
                  {batchTesting
                    ? t("accounts.batchTesting")
                    : t("accounts.testConnection")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={exporting || totalAccounts === 0}
                onClick={() => void handleExport()}
                title={t("grok.exportHint")}
              >
                {exporting ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <Download className="size-3.5" />
                )}
                <span className="hidden sm:inline">
                  {selected.size > 0
                    ? t("grok.exportSelectedBtn", { count: selected.size })
                    : t("grok.exportBtn")}
                </span>
              </Button>
              <label
                className="inline-flex h-8 shrink-0 cursor-pointer items-center gap-2 rounded-md border border-border bg-background px-2.5 text-xs font-medium text-muted-foreground"
                title={t("accounts.operationResultsPreferenceHint")}
              >
                <Switch
                  checked={showOperationResults}
                  onCheckedChange={onShowOperationResultsChange}
                  disabled={!onShowOperationResultsChange}
                  aria-label={t("accounts.operationResultsPreference")}
                />
                <span className="hidden lg:inline">
                  {t("accounts.operationResultsPreference")}
                </span>
              </label>
              <Button
                variant="outline"
                size="sm"
                className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                disabled={cleaning || stats.banned === 0}
                onClick={() => void handleCleanBanned()}
              >
                <Trash2 className="size-3.5" />
                <span className="hidden sm:inline">{t("grok.cleanBanned")}</span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                disabled={cleaning || stats.errorOnly === 0}
                onClick={() => void handleCleanError()}
              >
                <Trash2 className="size-3.5" />
                <span className="hidden sm:inline">{t("grok.cleanError")}</span>
              </Button>
            </div>
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
        {loading || resolvedDisabledSorts.length > 0 ? (
          <div className="mb-2 flex items-center justify-end gap-1.5 text-xs text-muted-foreground" role="status">
            <Loader2 className="size-3 animate-spin" />
            {loading ? t("common.loading") : t("accounts.statsWarming")}
          </div>
        ) : null}
        <div className="mb-4 grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-4">
          <CompactStat
            label={t("grok.statTotal")}
            chipLabel={t("accounts.filterAll")}
            value={stats.total}
            tone="neutral"
            active={statusFilter === "all"}
            onClick={() => setStatusFilter("all")}
          />
          <CompactStat
            label={t("grok.statActive")}
            chipLabel={t("accounts.filterNormal")}
            value={stats.active}
            tone="success"
            active={statusFilter === "active"}
            onClick={() => setStatusFilter("active")}
          />
          <CompactStat
            label={t("grok.statOAuth")}
            chipLabel={t("grok.authKindOAuth")}
            value={stats.oauth}
            tone="neutral"
            active={authFilter === "oauth"}
            onClick={() =>
              setAuthFilter((prev) => (prev === "oauth" ? "all" : "oauth"))
            }
          />
          <CompactStat
            label={t("grok.statApiKey")}
            chipLabel={t("grok.authKindApiKey")}
            value={stats.apiKey}
            tone="neutral"
            active={authFilter === "api_key"}
            onClick={() =>
              setAuthFilter((prev) => (prev === "api_key" ? "all" : "api_key"))
            }
          />
        </div>

        <div className="toolbar-surface mb-3 flex flex-col gap-2.5">
          <div className="flex items-center gap-1.5 overflow-x-auto [-mx-3] [px-3] [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
            <span className="shrink-0 whitespace-nowrap text-[12px] font-semibold text-foreground">
              {t("accounts.filter")}
            </span>
            {(
              [
                ["all", t("accounts.filterAll"), stats.total],
                ["active", t("accounts.filterNormal"), stats.active],
                [
                  "rate_limited",
                  t("accounts.filterRateLimited"),
                  stats.rateLimited,
                ],
                ["disabled", t("accounts.filterDisabled"), stats.disabled],
                ["banned", t("accounts.filterBanned"), stats.banned],
                ["error", t("accounts.filterError"), stats.errorOnly],
              ] as const
            ).map(([key, label, count]) => (
              <button
                key={key}
                type="button"
                onClick={() => setStatusFilter(key)}
                className={cn(
                  "shrink-0 whitespace-nowrap rounded-lg px-2.5 py-1.5 text-[12px] font-semibold transition-colors",
                  statusFilter === key
                    ? "bg-primary text-primary-foreground"
                    : "bg-muted/50 text-muted-foreground hover:bg-muted",
                )}
              >
                {label} {count}
              </button>
            ))}
          </div>

          <div className="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
            <div className="relative w-full shrink-0 sm:w-64">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                className="h-9 rounded-lg pl-9 text-[13px] sm:h-8"
                placeholder={t("grok.searchPlaceholder")}
                value={searchQuery}
                onChange={(e: ChangeEvent<HTMLInputElement>) =>
                  setSearchQuery(e.target.value)
                }
              />
            </div>
            <div className="flex max-w-full shrink-0 items-center gap-0.5 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5">
              {(
                [
                  ["all", t("accounts.filterAll")],
                  ["oauth", t("grok.authKindOAuth")],
                  ["api_key", t("grok.authKindApiKey")],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => setAuthFilter(key)}
                  className={cn(
                    "shrink-0 whitespace-nowrap rounded-md px-2.5 py-1.5 text-[12px] font-medium transition-colors",
                    authFilter === key
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
            <div className="flex max-w-full shrink-0 items-center gap-0.5 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5">
              {(
                [
                  ["all", t("accounts.filterAll")],
                  ["free", t("grok.planFree")],
                  ["supergrok", t("grok.planSuperGrok")],
                  ["supergrok_heavy", t("grok.planSuperGrokHeavy")],
                  ["supergrok_lite", t("grok.planSuperGrokLite")],
                  ["supergrok_plus", t("grok.planSuperGrokPlus")],
                  ["other", t("grok.planOther")],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => setPlanFilter(key)}
                  aria-pressed={planFilter === key}
                  className={cn(
                    "shrink-0 whitespace-nowrap rounded-md px-2.5 py-1.5 text-[12px] font-medium transition-colors",
                    planFilter === key
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
            <AccountGroupFilterSelect
              className="w-full min-w-0 sm:w-40"
              groups={grokGroups}
              value={groupFilter}
              onChange={setGroupFilter}
            />
            <div className="hidden shrink-0 items-center rounded-md border border-border bg-muted/50 p-0.5 lg:inline-flex lg:ml-auto">
              {(
                [
                  ["table", Rows3, t("accounts.viewModeTable")],
                  ["grid", LayoutGrid, t("accounts.viewModeGrid")],
                ] as const
              ).map(([key, Icon, label]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => setViewMode(key)}
                  title={label}
                  aria-label={label}
                  aria-pressed={viewMode === key}
                  className={cn(
                    "inline-flex items-center gap-1 rounded-sm px-2 py-1 text-[12px] font-medium transition-colors",
                    viewMode === key
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <Icon className="size-3.5" />
                  {label}
                </button>
              ))}
            </div>
            {viewMode === "table" && isDesktop ? (
              <ColumnSettingsMenu
                columns={visibleColumns}
                columnOrder={GROK_TABLE_COLUMNS}
                onToggle={toggleColumn}
                onReset={resetColumns}
                title={t("accounts.columnSettings")}
                resetTitle={t("accounts.columnReset")}
                labels={{
                  sequence: t("accounts.sequence"),
                  plan: t("grok.colPlan"),
                  proxy: t("accounts.proxyColumn"),
                  status: t("grok.colStatus"),
                  requests: t("accounts.requests"),
                  usage: t("accounts.usage"),
                  models: t("grok.colModels"),
                  updatedAt: t("grok.colUpdated"),
                }}
              />
            ) : null}
          </div>

          <div className="flex flex-wrap items-center gap-1">
            {(
              [
                ["usage", t("grok.sortUsage"), t("grok.sortUsageHint")],
                [
                  "requests",
                  t("grok.sortRequests"),
                  t("grok.sortRequestsHint"),
                ],
                ["updated", t("grok.sortUpdated"), t("grok.sortUpdatedHint")],
                ["group", t("grok.sortGroup"), t("grok.sortGroupHint")],
              ] as const
            ).map(([key, label, hint]) => (
              <button
                key={key}
                type="button"
                title={
                  key === "requests" && usageSortBlocked
                    ? t("accounts.largePoolSortDisabled")
                    : hint
                }
                aria-pressed={sortKey === key}
                onClick={() => toggleSort(key)}
                className={cn(
                  "inline-flex h-8 items-center gap-1 rounded-md border px-2 text-[11px] font-medium transition-colors",
                  key === "requests" && usageSortBlocked
                    ? "cursor-not-allowed border-border bg-muted/40 text-muted-foreground"
                    : sortKey === key
                      ? "border-primary/30 bg-primary/10 text-primary"
                      : "border-border bg-background text-muted-foreground hover:border-primary/25 hover:bg-accent/50 hover:text-foreground",
                )}
              >
                {label}
                {sortKey === key ? (
                  <span aria-hidden="true">
                    {sortDir === "desc" ? "↓" : "↑"}
                  </span>
                ) : null}
              </button>
            ))}
          </div>
        </div>

        {selected.size > 0 && (
          <div className="sticky top-2 z-20 mb-3 flex items-center justify-between gap-3 rounded-xl border border-primary/20 bg-card/95 px-3 py-2 text-sm shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
            <span className="font-semibold text-primary">
              {t("common.selected", { count: selected.size })}
            </span>
            <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchRefresh()}
              >
                <RefreshCw
                  className={cn("size-3.5", batchBusy && "animate-spin")}
                />
                <span className="hidden sm:inline">
                  {t("accounts.batchRefresh")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchTest(selectedIds)}
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
                disabled={batchBusy || batchTesting}
                onClick={openBatchSetModels}
              >
                <Layers className="size-3.5" />
                <span className="hidden sm:inline">
                  {t("grok.batchSetModels")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchEnabled(true)}
              >
                <Power className="size-3.5" />
                <span className="hidden sm:inline">{t("accounts.enable")}</span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchEnabled(false)}
              >
                <PowerOff className="size-3.5" />
                <span className="hidden sm:inline">{t("accounts.disable")}</span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchResetStatus()}
              >
                <RotateCcw className="size-3.5" />
                <span className="hidden sm:inline">
                  {t("accounts.batchResetStatus")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchDelete()}
              >
                <Trash2 className="size-3.5" />
                <span className="hidden sm:inline">
                  {t("accounts.batchDelete")}
                </span>
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={batchBusy}
                onClick={clearSelection}
              >
                <X className="size-3.5" />
                <span className="hidden sm:inline">
                  {t("accounts.clearSelection")}
                </span>
              </Button>
            </div>
          </div>
        )}

        <StateShell
          variant="section"
          isEmpty={sortedAccounts.length === 0}
          emptyIcon={<ChannelLogo channel="grok" size={30} />}
          emptyTitle={
            accounts.length === 0
              ? t("grok.emptyTitle")
              : t("grok.noMatchTitle")
          }
          emptyDescription={
            accounts.length === 0
              ? t("grok.emptyDesc")
              : t("grok.noMatchDesc")
          }
          action={
            accounts.length === 0 ? (
              <div className="flex flex-wrap items-center justify-center gap-1.5">
                <Button onClick={() => setShowAdd(true)}>
                  <Plus className="size-3.5" />
                  {t("grok.addAccount")}
                </Button>
                <Button
                  variant="outline"
                  disabled={importBusy}
                  onClick={() => setShowImportPicker(true)}
                >
                  {importBusy ? (
                    <Loader2 className="size-3.5 animate-spin" />
                  ) : (
                    <Upload className="size-3.5" />
                  )}
                  {importBusy
                    ? importProgress
                      ? t("grok.fileImportProgress", {
                          done: importProgress.done,
                          total: importProgress.total,
                        })
                      : t("grok.fileImporting")
                    : t("grok.fileImportBtn")}
                </Button>
              </div>
            ) : (
              <Button
                variant="outline"
                onClick={() => {
                  setSearchQuery("");
                  setStatusFilter("all");
                  setAuthFilter("all");
                  setPlanFilter("all");
                  setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
                  setSortKey(null);
                }}
              >
                {t("grok.clearFilters")}
              </Button>
            )
          }
        >
          {viewMode === "table" && isDesktop ? (
            <div className="data-table-shell hidden lg:block">
              <Table className="[&_td]:px-2.5 [&_th]:px-2.5 [&_td]:py-4">
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-9">
                      <input
                        ref={selectAllRef}
                        type="checkbox"
                        className="size-4 cursor-pointer rounded border-border accent-primary"
                        aria-label={t("accounts.selectAll")}
                        title={t("accounts.selectAll")}
                        checked={allPageSelected}
                        onChange={toggleSelectAll}
                      />
                    </TableHead>
                    {visibleColumns.sequence ? (
                      <TableHead className="w-10 text-[13px] font-semibold">
                        {t("accounts.sequence")}
                      </TableHead>
                    ) : null}
                    <TableHead className="text-[13px] font-semibold">
                      {t("grok.colAccount")}
                    </TableHead>
                    {visibleColumns.plan ? (
                      <TableHead className="text-center text-[13px] font-semibold">
                        {t("grok.colPlan")}
                      </TableHead>
                    ) : null}
                    {visibleColumns.proxy ? (
                      <TableHead className="text-[13px] font-semibold">
                        {t("accounts.proxyColumn")}
                      </TableHead>
                    ) : null}
                    {visibleColumns.status ? (
                      <TableHead className="text-[13px] font-semibold">
                        {t("grok.colStatus")}
                      </TableHead>
                    ) : null}
                    {visibleColumns.requests ? (
                      <TableHead
                        className={cn(
                          "select-none text-[13px] font-semibold transition-colors",
                          usageSortBlocked
                            ? "cursor-not-allowed text-muted-foreground"
                            : "cursor-pointer hover:text-primary",
                        )}
                        title={
                          usageSortBlocked
                            ? t("accounts.largePoolSortDisabled")
                            : t("grok.sortRequestsHint")
                        }
                        onClick={() => toggleSort("requests")}
                      >
                        {t("accounts.requests")}{" "}
                        {sortKey === "requests"
                          ? sortDir === "desc"
                            ? "↓"
                            : "↑"
                          : ""}
                      </TableHead>
                    ) : null}
                    {visibleColumns.usage ? (
                      <TableHead
                        className="min-w-[170px] cursor-pointer select-none text-[13px] font-semibold transition-colors hover:text-primary"
                        onClick={() => toggleSort("usage")}
                      >
                        {t("accounts.usage")}{" "}
                        {sortKey === "usage"
                          ? sortDir === "desc"
                            ? "↓"
                            : "↑"
                          : ""}
                      </TableHead>
                    ) : null}
                    {visibleColumns.models ? (
                      <TableHead className="text-[13px] font-semibold">
                        {t("grok.colModels")}
                      </TableHead>
                    ) : null}
                    {visibleColumns.updatedAt ? (
                      <TableHead
                        className="cursor-pointer select-none text-[13px] font-semibold transition-colors hover:text-primary"
                        onClick={() => toggleSort("updated")}
                      >
                        {t("grok.colUpdated")}{" "}
                        {sortKey === "updated"
                          ? sortDir === "desc"
                            ? "↓"
                            : "↑"
                          : ""}
                      </TableHead>
                    ) : null}
                    <TableHead className="text-right text-[13px] font-semibold">
                      {t("accounts.actions")}
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {pagedAccounts.map((account, index) => (
                    <MemoGrokAccountTableRow
                      visibleColumns={visibleColumns}
                      key={account.id}
                      account={account}
                      allGroups={allGroups}
                      proxyCtx={proxyBindingCtx}
                      sequence={(currentPage - 1) * pageSize + index + 1}
                      busy={busyId === account.id}
                      batchTesting={batchTesting}
                      selected={selected.has(account.id)}
                      detailOpen={detailAccountId === account.id}
                      healthBuckets={healthBars[String(account.id)]}
                      handlers={rowHandlers}
                    />
                  ))}
                </TableBody>
              </Table>
            </div>
          ) : null}
          {/* 桌面 table 模式下卡片列表原先只是 CSS 隐藏(lg:hidden),React 仍然
              渲染了整份 DOM——大号池下等于双倍渲染。按视口只渲染可见的那一份。 */}
          {viewMode !== "table" || !isDesktop ? (
            <div
              className={cn(
                "grid grid-cols-1 gap-3 xl:grid-cols-2",
                viewMode === "table" && "lg:hidden",
              )}
            >
              {pagedAccounts.map((account, index) => (
                <MemoGrokAccountCard
                  key={account.id}
                  account={account}
                  allGroups={allGroups}
                  proxyCtx={proxyBindingCtx}
                  sequence={(currentPage - 1) * pageSize + index + 1}
                  busy={busyId === account.id}
                  batchTesting={batchTesting}
                  selected={selected.has(account.id)}
                  detailOpen={detailAccountId === account.id}
                  handlers={rowHandlers}
                />
              ))}
            </div>
          ) : null}
          <Pagination
            page={currentPage}
            totalPages={totalPages}
            onPageChange={setPage}
            totalItems={totalAccounts}
            pageSize={pageSize}
            pageSizeOptions={pageSizeOptions}
            onPageSizeChange={(nextPageSize) => {
              setPageSize(nextPageSize);
              setPage(1);
            }}
          />
        </StateShell>
      </StateShell>

      <Modal
        show={showAdd}
        title={t("grok.addTitle")}
        contentClassName="sm:max-w-[560px]"
        onClose={() => {
          setShowAdd(false);
          resetAddForm();
        }}
        footer={
          <>
            <Button
              variant="outline"
              onClick={() => {
                setShowAdd(false);
                resetAddForm();
              }}
            >
              {t("common.cancel")}
            </Button>
            {addMethod === "oauth_link" ? (
              deviceStep === "idle" ? (
                <Button
                  onClick={() => void handleDeviceStart()}
                  disabled={deviceStarting}
                >
                  {deviceStarting
                    ? t("grok.oauthGenerating")
                    : t("grok.oauthGenerateBtn")}
                </Button>
              ) : (
                <Button
                  variant="outline"
                  onClick={() => void handleDeviceRestart()}
                  disabled={deviceStarting}
                >
                  {deviceStarting
                    ? t("grok.oauthGenerating")
                    : t("grok.oauthRestart")}
                </Button>
              )
            ) : addMethod === "sso" ? (
              <Button
                onClick={() => void handleImportSSO()}
                disabled={ssoImporting || !ssoTokens.trim()}
              >
                {ssoImporting ? (
                  <>
                    <Loader2 className="size-3.5 animate-spin" />
                    {t("grok.ssoImporting")}
                  </>
                ) : (
                  <>
                    <Upload className="size-3.5" />
                    {t("grok.ssoImportBtn")}
                  </>
                )}
              </Button>
            ) : (
              <Button
                onClick={() => void handleAdd()}
                disabled={submitting || !credentialReady}
              >
                {submitting ? t("grok.adding") : t("grok.submit")}
              </Button>
            )}
          </>
        }
      >
        <div className="space-y-4">
          <div>
            <label className="mb-2 block text-sm font-medium text-muted-foreground">
              {t("grok.authKind")}
            </label>
            <div className="grid grid-cols-2 gap-1 rounded-xl border border-border bg-muted/30 p-1 sm:grid-cols-4">
              {(
                [
                  {
                    kind: "oauth_link" as AddMethod,
                    icon: Link2,
                    label: t("grok.authKindLink"),
                  },
                  {
                    kind: "oauth" as AddMethod,
                    icon: FileJson,
                    label: t("grok.authKindOAuth"),
                  },
                  {
                    kind: "api_key" as AddMethod,
                    icon: KeyRound,
                    label: t("grok.authKindApiKey"),
                  },
                  {
                    kind: "sso" as AddMethod,
                    icon: Upload,
                    label: t("grok.authKindSSO"),
                  },
                ] as const
              ).map(({ kind, icon: Icon, label }) => (
                <button
                  key={kind}
                  type="button"
                  onClick={() => {
                    setAddMethod(kind);
                    if (kind !== "oauth_link") {
                      stopDevicePoll();
                      setDeviceStep("idle");
                      setDeviceSession(null);
                    }
                    if (kind !== "sso") {
                      setSsoResult(null);
                    }
                    setForm((f) => ({
                      ...f,
                      auth_kind: kind === "api_key" ? "api_key" : "oauth",
                    }));
                  }}
                  className={cn(
                    "inline-flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold transition-all",
                    addMethod === kind
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <Icon className="size-3.5" />
                  <span className="truncate">{label}</span>
                </button>
              ))}
            </div>
          </div>

          {addMethod === "oauth_link" ? (
            <div className="space-y-4">
              {deviceStep === "idle" ? (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="mb-1 font-semibold text-foreground">
                      {t("grok.oauthStep1Title")}
                    </p>
                    <p>{t("grok.oauthStep1Desc")}</p>
                  </div>
                  <div>
                    <label className="mb-2 block text-sm font-medium text-muted-foreground">
                      {t("grok.nameLabel")}
                    </label>
                    <Input
                      placeholder={t("grok.namePlaceholder")}
                      value={form.name ?? ""}
                      onChange={(e: ChangeEvent<HTMLInputElement>) =>
                        setForm((f) => ({ ...f, name: e.target.value }))
                      }
                    />
                  </div>
                  <div>
                    <ProxyField
                      value={form.proxy_url ?? ""}
                      onChange={(url) => setForm((f) => ({ ...f, proxy_url: url }))}
                      proxies={proxyPool}
                      label={t("grok.proxyUrl")}
                      labelClassName="mb-2 text-sm font-medium"
                      placeholder="http://user:pass@host:port"
                    />
                  </div>
                </>
              ) : (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="mb-1 font-semibold text-foreground">
                      {t("grok.oauthStep2Title")}
                    </p>
                    <p>{t("grok.oauthStep2Desc")}</p>
                  </div>
                  {deviceSession ? (
                    <>
                      <div className="rounded-xl border border-primary/30 bg-primary/5 px-4 py-4">
                        <p className="mb-2 text-xs font-semibold text-muted-foreground">
                          {t("grok.oauthUserCodeLabel")}
                        </p>
                        <div className="flex flex-wrap items-center gap-2">
                          <code className="rounded-lg bg-background px-3 py-2 font-mono text-lg font-bold tracking-wider text-foreground">
                            {deviceSession.user_code}
                          </code>
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            onClick={() => void handleDeviceCopyCode()}
                          >
                            <Copy className="size-3.5" />
                            {t("common.copy")}
                          </Button>
                        </div>
                      </div>
                      <div className="rounded-xl border border-border px-4 py-3">
                        <p className="mb-2 text-xs font-semibold text-muted-foreground">
                          {t("grok.oauthOpenLink")}
                        </p>
                        <a
                          href={deviceSession.verification_url}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="inline-flex items-start gap-1.5 text-sm font-semibold text-primary hover:underline"
                        >
                          <ExternalLink className="mt-0.5 size-3.5 shrink-0" />
                          <span className="break-all">
                            {deviceSession.verification_url}
                          </span>
                        </a>
                      </div>
                      <div className="flex items-center gap-2 text-sm text-muted-foreground">
                        <Loader2
                          className={cn(
                            "size-4",
                            devicePolling || devicePollTimer.current
                              ? "animate-spin"
                              : "",
                          )}
                        />
                        {t("grok.oauthWaiting")}
                      </div>
                    </>
                  ) : null}
                </>
              )}
            </div>
          ) : addMethod === "sso" ? (
            <div className="space-y-4">
              <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                <p className="mb-1 font-semibold text-foreground">
                  {t("grok.ssoTitle")}
                </p>
                <p>{t("grok.ssoDesc")}</p>
              </div>
              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.ssoTokensLabel")} *
                </label>
                <textarea
                  className="min-h-[140px] w-full rounded-lg border border-input bg-transparent px-3 py-2 font-mono text-sm shadow-xs outline-none transition-[color,box-shadow] focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
                  placeholder={t("grok.ssoTokensPlaceholder")}
                  value={ssoTokens}
                  onChange={(e: ChangeEvent<HTMLTextAreaElement>) =>
                    setSsoTokens(e.target.value)
                  }
                />
                <p className="mt-1.5 text-xs text-muted-foreground">
                  {t("grok.ssoTokensHint")}
                </p>
              </div>

              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.baseUrl")}
                </label>
                <Input
                  placeholder={t("grok.baseUrlPlaceholder")}
                  value={form.base_url ?? ""}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setForm((f) => ({ ...f, base_url: e.target.value }))
                  }
                />
              </div>

              <div>
                <ProxyField
                  value={form.proxy_url ?? ""}
                  onChange={(url) => setForm((f) => ({ ...f, proxy_url: url }))}
                  proxies={proxyPool}
                  label={t("grok.proxyUrl")}
                  labelClassName="mb-2 text-sm font-medium"
                  placeholder="http://user:pass@host:port"
                />
              </div>

              {ssoResult ? (
                <div className="space-y-2 rounded-xl border border-border bg-muted/20 px-4 py-3">
                  <p className="text-sm font-semibold text-foreground">
                    {t("grok.ssoResultSummary", {
                      imported: ssoResult.imported,
                      total: ssoResult.total,
                    })}
                  </p>
                  <div className="max-h-40 space-y-1 overflow-y-auto">
                    {ssoResult.items.map((item, index) => (
                      <div
                        key={index}
                        className="flex items-start gap-1.5 text-xs"
                      >
                        {item.ok ? (
                          <CheckCircle2 className="mt-0.5 size-3.5 shrink-0 text-[hsl(var(--success))]" />
                        ) : (
                          <XCircle className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                        )}
                        <span className="min-w-0 flex-1 break-all text-muted-foreground">
                          {item.email || item.name || `#${index + 1}`}
                          {item.ok ? null : item.error ? ` — ${item.error}` : ""}
                        </span>
                      </div>
                    ))}
                  </div>
                </div>
              ) : null}
            </div>
          ) : (
            <>
              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.nameLabel")}
                </label>
                <Input
                  placeholder={t("grok.namePlaceholder")}
                  value={form.name ?? ""}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setForm((f) => ({ ...f, name: e.target.value }))
                  }
                />
              </div>

              {addMethod === "oauth" ? (
                <div>
                  <label className="mb-2 block text-sm font-medium text-muted-foreground">
                    {t("grok.authJson")} *
                  </label>
                  <textarea
                    className="min-h-[120px] w-full rounded-lg border border-input bg-transparent px-3 py-2 font-mono text-sm shadow-xs outline-none transition-[color,box-shadow] focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
                    placeholder={t("grok.authJsonPlaceholder")}
                    value={form.auth_json ?? ""}
                    onChange={(e: ChangeEvent<HTMLTextAreaElement>) =>
                      setForm((f) => ({ ...f, auth_json: e.target.value }))
                    }
                  />
                  <p className="mt-1.5 text-xs text-muted-foreground">
                    {t("grok.authJsonHint")}
                  </p>
                </div>
              ) : (
                <div>
                  <label className="mb-2 block text-sm font-medium text-muted-foreground">
                    {t("grok.apiKey")} *
                  </label>
                  <Input
                    type="password"
                    placeholder="xai-..."
                    value={form.api_key ?? ""}
                    onChange={(e: ChangeEvent<HTMLInputElement>) =>
                      setForm((f) => ({ ...f, api_key: e.target.value }))
                    }
                  />
                </div>
              )}

              {/* OAuth 端点固定为 grok 官方 cli-chat-proxy，无需手填 Base URL；
                  仅 API Key 方式允许自定义上游（默认 api.x.ai）。 */}
              {addMethod === "api_key" ? (
                <div>
                  <label className="mb-2 block text-sm font-medium text-muted-foreground">
                    {t("grok.baseUrl")}
                  </label>
                  <Input
                    placeholder={t("grok.baseUrlPlaceholder")}
                    value={form.base_url ?? ""}
                    onChange={(e: ChangeEvent<HTMLInputElement>) =>
                      setForm((f) => ({ ...f, base_url: e.target.value }))
                    }
                  />
                  <p className="mt-1.5 text-xs text-muted-foreground">
                    {t("grok.baseUrlHint")}
                  </p>
                </div>
              ) : null}

              <div>
                <div className="mb-2 flex items-center justify-between gap-2">
                  <label className="text-sm font-medium text-muted-foreground">
                    {t("grok.models")}
                  </label>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => void handleFetchModels()}
                    disabled={modelsLoading || !credentialReady}
                  >
                    <RefreshCw
                      className={cn("size-3", modelsLoading && "animate-spin")}
                    />
                    {modelsLoading
                      ? t("grok.modelsFetching")
                      : t("grok.modelsFetch")}
                  </Button>
                </div>
                <div className="mb-2 flex gap-2">
                  <Input
                    placeholder={t("grok.modelsPlaceholder")}
                    value={modelDraft}
                    onChange={(e: ChangeEvent<HTMLInputElement>) =>
                      setModelDraft(e.target.value)
                    }
                    onKeyDown={(e) => {
                      if (e.key === "Enter") {
                        e.preventDefault();
                        addModels(modelDraft);
                      }
                    }}
                  />
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => addModels(modelDraft)}
                    disabled={!modelDraft.trim()}
                  >
                    <Plus className="size-3.5" />
                  </Button>
                </div>
                {(form.models ?? []).length === 0 ? (
                  <p className="text-xs text-muted-foreground">
                    {t("grok.modelsEmpty")}
                  </p>
                ) : (
                  <div className="flex flex-wrap gap-1.5">
                    {(form.models ?? []).map((model) => (
                      <span
                        key={model}
                        className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-0.5 text-xs font-medium"
                      >
                        {model}
                        <button
                          type="button"
                          onClick={() => removeModel(model)}
                          className="text-muted-foreground hover:text-foreground"
                        >
                          <X className="size-3" />
                        </button>
                      </span>
                    ))}
                  </div>
                )}
              </div>

              <div>
                <ProxyField
                  value={form.proxy_url ?? ""}
                  onChange={(url) => setForm((f) => ({ ...f, proxy_url: url }))}
                  proxies={proxyPool}
                  label={t("grok.proxyUrl")}
                  labelClassName="mb-2 text-sm font-medium"
                  placeholder="http://user:pass@host:port"
                />
              </div>
            </>
          )}
          {addMethod !== "oauth_link" && (
            <div className="space-y-1.5 border-t border-border pt-4">
              <label className="block text-sm font-medium text-muted-foreground">
                {t("accounts.importGroupsLabel")}
              </label>
              <AccountGroupMultiSelect
                groups={grokGroups}
                value={importGroupIds}
                onChange={setImportGroupIds}
                allLabel={t("accounts.groupsUnbound")}
                selectedLabel={t("accounts.groupsSelected", {
                  count: importGroupIds.length,
                })}
                placeholder={t("accounts.importGroupsPlaceholder")}
                emptyLabel={t("accounts.groupsNone")}
                emptyHint={t("accounts.groupsSelectHint")}
              />
              <p className="text-xs text-muted-foreground">
                {t("accounts.importGroupsHint")}
              </p>
            </div>
          )}
        </div>
      </Modal>

      {testingAccount ? (
        <GrokTestConnectionModal
          account={testingAccount}
          onClose={() => setTestingAccount(null)}
          onSettled={() => void reload()}
        />
      ) : null}

      {usageAccount ? (
        <AccountUsageModal
          account={usageAccount}
          onClose={() => setUsageAccount(null)}
          showCreditSettings={false}
        />
      ) : null}

      {/* 代理徽章直达的快速绑定弹窗：与 Codex 账号页共用组件 */}
      <AccountProxyQuickEditor
        account={quickProxyAccount}
        accountLabel={quickProxyAccount ? accountLabel(quickProxyAccount) : ""}
        proxies={proxyPool}
        ctx={proxyBindingCtx}
        onClose={() => setQuickProxyAccount(null)}
        onSaved={() => reload()}
      />

      {/* 快速设置账号分组(issue #487):与 Codex 账号页同一交互 */}
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
                ? quickGroupAccount.name || quickGroupAccount.email
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
              onClick={() => navigate("/accounts?groupManager=1")}
            >
              <FolderOpen className="size-3" />
              {t("accounts.groupManage")}
            </Button>
          </div>
          <AccountGroupMultiSelect
            groups={grokGroups}
            value={quickGroupIds}
            onChange={setQuickGroupIds}
            allLabel={t("accounts.groupsUnbound")}
            selectedLabel={t("accounts.groupsSelected", {
              count: quickGroupIds.length,
            })}
            placeholder={t("accounts.importGroupsPlaceholder")}
            emptyLabel={t("accounts.groupsNone")}
            emptyHint={t("accounts.groupsSelectHint")}
          />
        </div>
      </Modal>

      <AccountDetailSheet
        account={detailAccount}
        groups={
          detailAccount
            ? resolveAccountGroups(detailAccount.group_ids, allGroups)
            : []
        }
        healthBuckets={
          detailAccount ? healthBars[String(detailAccount.id)] : undefined
        }
        sequence={
          detailNavIndex >= 0
            ? (currentPage - 1) * pageSize + detailNavIndex + 1
            : undefined
        }
        usageSlot={
          detailAccount ? (
            <GrokUsageCell
              account={detailAccount}
              detailed
              onRefreshed={() => {
                void refreshAccountRow(detailAccount.id);
              }}
            />
          ) : null
        }
        providerSlot={
          detailAccount ? (
            <GrokStateDetail
              account={detailAccount}
              state={detailGrokState}
              loading={detailGrokStateLoading}
              error={detailGrokStateError}
              action={detailGrokAction}
              onSync={() => void syncDetailGrokState()}
              onProbe={() => void probeDetailGrokCapabilities()}
            />
          ) : null
        }
        canGoPrev={detailNavIndex > 0 || currentPage > 1}
        canGoNext={
          (detailNavIndex >= 0 && detailNavIndex < sortedAccounts.length - 1) ||
          currentPage < totalPages
        }
        refreshing={detailAccount ? busyId === detailAccount.id : false}
        onClose={closeAccountDetail}
        onPrev={goDetailPrev}
        onNext={goDetailNext}
        onEdit={() => {
          if (!detailAccount) return;
          openEdit(detailAccount);
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
          // Grok 不支持导出 auth.json；Sheet 内已对 grok 账号隐藏该按钮。
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
          void api
            .updateAccountModelCooldownPolicy(detailAccount.id, data)
            .then(() => {
              showToast(t("accounts.modelCooldownPolicySaved"));
              return reload();
            })
            .catch((error) =>
              showToast(
                t("accounts.modelCooldownPolicySaveFailed", {
                  error: getErrorMessage(error),
                }),
                "error",
              ),
            );
        }}
        onClearModelCooldown={(model) => {
          if (!detailAccount) return;
          void api
            .clearAccountModelCooldown(detailAccount.id, model)
            .then(() => {
              showToast(t("accounts.modelCooldownCleared", { model }));
              return reload();
            })
            .catch((error) => showToast(getErrorMessage(error), "error"));
        }}
        onClearAllModelCooldowns={() => {
          if (!detailAccount) return;
          void api
            .clearAllAccountModelCooldowns(detailAccount.id)
            .then((result) => {
              showToast(
                t("accounts.allModelCooldownsCleared", {
                  count: result.cleared,
                }),
              );
              return reload();
            })
            .catch((error) => showToast(getErrorMessage(error), "error"));
        }}
        onResetCredits={() => {
          // Grok 无额度券；Sheet 内已隐藏。
        }}
        onDelete={() => {
          if (!detailAccount) return;
          void handleDelete(detailAccount);
        }}
      />

      {/* 导入来源选择弹窗（点「导入文件」先弹提示，风格对齐 Codex 导入） */}
      <Modal
        show={showImportPicker}
        title={t("grok.importPickerTitle")}
        onClose={() => setShowImportPicker(false)}
        contentClassName="sm:max-w-[560px]"
      >
        <p className="mb-4 text-sm text-muted-foreground">
          {t("grok.importPickerDesc")}
        </p>
        <div className="mb-4 space-y-1.5">
          <label className="text-xs font-medium text-foreground">
            {t("accounts.importGroupsLabel")}
          </label>
          <AccountGroupMultiSelect
            groups={grokGroups}
            value={importGroupIds}
            onChange={setImportGroupIds}
            allLabel={t("accounts.groupsUnbound")}
            selectedLabel={t("accounts.groupsSelected", {
              count: importGroupIds.length,
            })}
            placeholder={t("accounts.importGroupsPlaceholder")}
            emptyLabel={t("accounts.groupsNone")}
            emptyHint={t("accounts.groupsSelectHint")}
          />
          <p className="text-[11px] text-muted-foreground">
            {t("accounts.importGroupsHint")}
          </p>
          <label className="flex cursor-pointer items-center gap-2 pt-1 text-xs text-muted-foreground">
            <input
              type="checkbox"
              className="size-3.5"
              checked={importFileProxies}
              onChange={(e) => setImportFileProxies(e.target.checked)}
            />
            {t("accounts.importFileProxies")}
          </label>
          <p className="text-[11px] text-muted-foreground">
            {t("grok.importFileProxiesHint")}
          </p>
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <button
            type="button"
            className="flex items-start gap-3 rounded-xl border border-border px-4 py-3 text-left transition-colors hover:bg-muted/50"
            onClick={() => authFileInputRef.current?.click()}
          >
            <FileJson className="size-5 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium">
                {t("grok.importOptJson")}
              </div>
              <div className="text-[11px] text-muted-foreground">
                {t("grok.importOptJsonDesc")}
              </div>
            </div>
          </button>
          <button
            type="button"
            className="flex items-start gap-3 rounded-xl border border-border px-4 py-3 text-left transition-colors hover:bg-muted/50"
            onClick={() => ssoFileInputRef.current?.click()}
          >
            <FileText className="size-5 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium">{t("grok.importOptSso")}</div>
              <div className="text-[11px] text-muted-foreground">
                {t("grok.importOptSsoDesc")}
              </div>
            </div>
          </button>
          <button
            type="button"
            className="flex items-start gap-3 rounded-xl border border-border px-4 py-3 text-left transition-colors hover:bg-muted/50"
            onClick={() => refreshFileInputRef.current?.click()}
          >
            <KeyRound className="size-5 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium">
                {t("grok.importOptRefresh")}
              </div>
              <div className="text-[11px] text-muted-foreground">
                {t("grok.importOptRefreshDesc")}
              </div>
            </div>
          </button>
        </div>
      </Modal>

      {/* 隐藏文件输入：三种来源 */}
      <input
        ref={authFileInputRef}
        type="file"
        accept=".json,application/json"
        multiple
        className="hidden"
        onChange={(e) => void handleImportAuthFiles(e.target.files)}
      />
      <input
        ref={ssoFileInputRef}
        type="file"
        accept=".txt,text/plain"
        className="hidden"
        onChange={(e) => void handleImportSsoFile(e.target.files)}
      />
      <input
        ref={refreshFileInputRef}
        type="file"
        accept=".txt,text/plain"
        className="hidden"
        onChange={(e) => void handleImportRefreshFile(e.target.files)}
      />

      <Modal
        show={Boolean(importResult)}
        title={t("grok.fileImportTitle")}
        onClose={() => setImportResult(null)}
        contentClassName="sm:max-w-[520px]"
        footer={
          <Button variant="outline" onClick={() => setImportResult(null)}>
            {t("common.close")}
          </Button>
        }
      >
        {importResult ? (
          <div className="space-y-3">
            <p className="text-sm font-semibold text-foreground">
              {t("grok.ssoResultSummary", {
                imported: importResult.imported,
                total: importResult.total,
              })}
            </p>
            <div className="max-h-72 space-y-1 overflow-y-auto rounded-lg border border-border bg-muted/20 px-3 py-2">
              {importResult.items.map((item, index) => (
                <div key={index} className="flex items-start gap-1.5 text-xs">
                  {item.ok ? (
                    <CheckCircle2 className="mt-0.5 size-3.5 shrink-0 text-[hsl(var(--success))]" />
                  ) : (
                    <XCircle className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                  )}
                  <span className="min-w-0 flex-1 break-all text-muted-foreground">
                    {item.email || item.name || `#${index + 1}`}
                    {item.ok
                      ? item.revived
                        ? ` — ${t("grok.importItemRevived", { id: item.id })}`
                        : item.updated
                          ? ` — ${t("grok.importItemUpdated", { id: item.id })}`
                          : null
                      : item.error
                        ? ` — ${item.error}`
                        : ""}
                  </span>
                </div>
              ))}
            </div>
          </div>
        ) : null}
      </Modal>

      <Modal
        show={batchModelsOpen}
        title={t("grok.batchSetModelsTitle")}
        contentClassName="sm:max-w-[560px]"
        onClose={() => {
          if (!batchBusy) setBatchModelsOpen(false);
        }}
        footer={
          <>
            <Button
              variant="outline"
              disabled={batchBusy}
              onClick={() => setBatchModelsOpen(false)}
            >
              {t("common.cancel")}
            </Button>
            <Button
              onClick={() => void handleBatchSetModels()}
              disabled={batchBusy}
            >
              {batchBusy ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : null}
              {t("grok.batchSetModelsApply", { count: selectedIds.length })}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            {t("grok.batchSetModelsDesc", { count: selectedIds.length })}
          </p>
          <div>
            <div className="mb-2 flex items-center justify-between gap-2">
              <label className="text-sm font-medium text-muted-foreground">
                {t("grok.models")}
              </label>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={batchBusy || batchModelsDraft.length === 0}
                onClick={() => setBatchModelsDraft([])}
              >
                {t("grok.batchSetModelsClear")}
              </Button>
            </div>
            <p className="mb-2 text-xs text-muted-foreground">
              {t("grok.batchSetModelsPresetHint")}
            </p>
            <div className="mb-3 flex flex-wrap gap-1.5">
              {DEFAULT_GROK_TEST_MODELS.map((model) => {
                const isPicked = batchModelsDraft.some(
                  (item) => item.toLowerCase() === model.toLowerCase(),
                );
                return (
                  <button
                    key={model}
                    type="button"
                    disabled={batchBusy}
                    onClick={() => batchTogglePreset(model)}
                    className={cn(
                      "rounded-md px-2 py-1 font-mono text-[11px] font-medium ring-1 ring-inset transition-colors",
                      isPicked
                        ? "bg-primary/10 text-primary ring-primary/30"
                        : "bg-muted/40 text-muted-foreground ring-border hover:bg-accent hover:text-foreground",
                    )}
                  >
                    {model}
                  </button>
                );
              })}
            </div>
            <div className="mb-2 flex gap-2">
              <Input
                placeholder={t("grok.modelsPlaceholder")}
                value={batchModelInput}
                disabled={batchBusy}
                onChange={(e: ChangeEvent<HTMLInputElement>) =>
                  setBatchModelInput(e.target.value)
                }
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    batchAddModels(batchModelInput);
                  }
                }}
              />
              <Button
                type="button"
                variant="outline"
                disabled={batchBusy || !batchModelInput.trim()}
                onClick={() => batchAddModels(batchModelInput)}
              >
                <Plus className="size-3.5" />
              </Button>
            </div>
            {batchModelsDraft.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                {t("grok.batchSetModelsEmptyHint")}
              </p>
            ) : (
              <div className="flex flex-wrap gap-1.5">
                {batchModelsDraft.map((model) => (
                  <span
                    key={model}
                    className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-0.5 text-xs font-medium"
                  >
                    {model}
                    <button
                      type="button"
                      disabled={batchBusy}
                      onClick={() => batchRemoveModel(model)}
                      className="text-muted-foreground hover:text-foreground"
                    >
                      <X className="size-3" />
                    </button>
                  </span>
                ))}
              </div>
            )}
          </div>
        </div>
      </Modal>

      <Modal
        show={editAccount !== null}
        title={t("grok.editTitle")}
        contentClassName="sm:max-w-[560px]"
        onClose={() => setEditAccount(null)}
        footer={
          <>
            <Button variant="outline" onClick={() => setEditAccount(null)}>
              {t("common.cancel")}
            </Button>
            <Button
              onClick={() => void handleSaveEdit()}
              disabled={editSubmitting}
            >
              {editSubmitting ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : null}
              {t("common.save")}
            </Button>
          </>
        }
      >
        {editAccount ? (
          <div className="space-y-4">
            <div className="rounded-lg bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
              {accountLabel(editAccount)}
            </div>

            <div>
              <div className="mb-2 flex items-center justify-between gap-2">
                <label className="text-sm font-medium text-muted-foreground">
                  {t("grok.models")}
                </label>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={editFillCommonModels}
                >
                  <Plus className="size-3" />
                  {t("grok.modelsQuickAdd")}
                </Button>
              </div>
              <div className="mb-2 flex gap-2">
                <Input
                  placeholder={t("grok.modelsPlaceholder")}
                  value={editModelDraft}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setEditModelDraft(e.target.value)
                  }
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      editAddModels(editModelDraft);
                    }
                  }}
                />
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => editAddModels(editModelDraft)}
                  disabled={!editModelDraft.trim()}
                >
                  <Plus className="size-3.5" />
                </Button>
              </div>
              {editForm.models.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  {t("grok.editModelsEmptyHint")}
                </p>
              ) : (
                <div className="flex flex-wrap gap-1.5">
                  {editForm.models.map((model) => (
                    <span
                      key={model}
                      className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-0.5 text-xs font-medium"
                    >
                      {model}
                      <button
                        type="button"
                        onClick={() => editRemoveModel(model)}
                        className="text-muted-foreground hover:text-foreground"
                      >
                        <X className="size-3" />
                      </button>
                    </span>
                  ))}
                </div>
              )}
            </div>

            <div>
              <label className="mb-2 block text-sm font-medium text-muted-foreground">
                {t("grok.modelMapping")}
              </label>
              <div className="space-y-2">
                {editModelMappingEntries.map((entry, index) => (
                  <div
                    key={index}
                    className="grid grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]"
                  >
                    <Input
                      placeholder={t("grok.modelMappingFromPlaceholder")}
                      value={entry.from}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        updateEditModelMapping(index, "from", event.target.value)
                      }
                    />
                    <Input
                      placeholder={t("grok.modelMappingToPlaceholder")}
                      value={entry.to}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        updateEditModelMapping(index, "to", event.target.value)
                      }
                    />
                    <Button
                      type="button"
                      variant="outline"
                      size="icon"
                      className="h-10 w-10"
                      onClick={() => removeEditModelMapping(index)}
                      disabled={
                        editModelMappingEntries.length === 1 &&
                        !entry.from.trim() &&
                        !entry.to.trim()
                      }
                      title={t("grok.modelMappingRemove")}
                      aria-label={t("grok.modelMappingRemove")}
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
                    setEditModelMappingEntries((entries) => [
                      ...entries,
                      { from: "", to: "" },
                    ])
                  }
                >
                  <Plus className="size-3.5" />
                  {t("grok.modelMappingAdd")}
                </Button>
              </div>
              <p className="mt-1.5 text-xs leading-5 text-muted-foreground">
                {t("grok.modelMappingHint")}
              </p>
            </div>

            {/* OAuth 账号端点固定为官方 cli-chat-proxy，不显示 Base URL；
                仅 API Key 账号允许自定义上游（默认 api.x.ai）。 */}
            {editAccount.grok_auth_kind === "api_key" ? (
              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.baseUrl")}
                </label>
                <Input
                  placeholder={t("grok.baseUrlPlaceholder")}
                  value={editForm.base_url}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setEditForm((f) => ({ ...f, base_url: e.target.value }))
                  }
                />
              </div>
            ) : null}

            <div>
              <ProxyField
                value={editForm.proxy_url}
                onChange={(url) => setEditForm((f) => ({ ...f, proxy_url: url }))}
                proxies={proxyPool}
                label={t("grok.proxyUrl")}
                labelClassName="mb-2 text-sm font-medium"
                placeholder="http://user:pass@host:port"
              />
            </div>
          </div>
        ) : null}
      </Modal>

      {confirmDialog}
    </div>
  );
}

// Memo 包装层:props 全部是稳定引用/原始值(handlers 经 ref 转发恒定),
// 勾选、单行 busy 等局部状态变化只重渲染受影响的行,而不是整页行×2。
// groups 在包装层内按账号 memo 解析,避免父组件每次渲染生成新数组打穿 memo。
const MemoGrokAccountTableRow = memo(function MemoGrokAccountTableRow({
  account,
  allGroups,
  proxyCtx,
  sequence,
  visibleColumns,
  busy,
  batchTesting,
  selected,
  detailOpen,
  healthBuckets,
  handlers,
}: {
  account: AccountRow;
  allGroups: AccountGroup[];
  proxyCtx: ProxyBindingContext;
  sequence: number;
  visibleColumns: GrokColumnVisibility;
  busy: boolean;
  batchTesting: boolean;
  selected: boolean;
  detailOpen: boolean;
  healthBuckets?: AccountHealthBucket[];
  handlers: GrokRowHandlers;
}) {
  const groups = useMemo(
    () => resolveAccountGroups(account.group_ids, allGroups),
    [account.group_ids, allGroups],
  );
  return (
    <GrokAccountTableRow
      account={account}
      groups={groups}
      proxyCtx={proxyCtx}
      sequence={sequence}
      visibleColumns={visibleColumns}
      busy={busy}
      batchTesting={batchTesting}
      selected={selected}
      detailOpen={detailOpen}
      healthBuckets={healthBuckets}
      onToggleSelect={() => handlers.toggleSelect(account.id)}
      onOpenDetail={() => handlers.openDetail(account)}
      onTest={() => handlers.test(account)}
      onUsage={() => handlers.usage(account)}
      onRefresh={() => handlers.refresh(account)}
      onToggleEnabled={() => handlers.toggleEnabled(account)}
      onEdit={() => handlers.edit(account)}
      onEditGroups={() => handlers.editGroups(account)}
      onEditProxy={() => handlers.editProxy(account)}
      onDelete={() => handlers.remove(account)}
      onUsageRefreshed={() => handlers.usageRefreshed(account)}
    />
  );
});

const MemoGrokAccountCard = memo(function MemoGrokAccountCard({
  account,
  allGroups,
  proxyCtx,
  sequence,
  busy,
  batchTesting,
  selected,
  detailOpen,
  handlers,
}: {
  account: AccountRow;
  allGroups: AccountGroup[];
  proxyCtx: ProxyBindingContext;
  sequence: number;
  busy: boolean;
  batchTesting: boolean;
  selected: boolean;
  detailOpen: boolean;
  handlers: GrokRowHandlers;
}) {
  const groups = useMemo(
    () => resolveAccountGroups(account.group_ids, allGroups),
    [account.group_ids, allGroups],
  );
  return (
    <GrokAccountCard
      account={account}
      groups={groups}
      proxyCtx={proxyCtx}
      sequence={sequence}
      busy={busy}
      batchTesting={batchTesting}
      selected={selected}
      detailOpen={detailOpen}
      onToggleSelect={() => handlers.toggleSelect(account.id)}
      onOpenDetail={() => handlers.openDetail(account)}
      onTest={() => handlers.test(account)}
      onUsage={() => handlers.usage(account)}
      onRefresh={() => handlers.refresh(account)}
      onToggleEnabled={() => handlers.toggleEnabled(account)}
      onEdit={() => handlers.edit(account)}
      onEditGroups={() => handlers.editGroups(account)}
      onEditProxy={() => handlers.editProxy(account)}
      onDelete={() => handlers.remove(account)}
      onUsageRefreshed={() => handlers.usageRefreshed(account)}
    />
  );
});

function GrokAccountCard({
  account,
  groups = [],
  proxyCtx,
  sequence,
  busy,
  batchTesting,
  selected,
  detailOpen,
  onToggleSelect,
  onOpenDetail,
  onTest,
  onUsage,
  onRefresh,
  onToggleEnabled,
  onEdit,
  onEditGroups,
  onEditProxy,
  onDelete,
  onUsageRefreshed,
}: {
  account: AccountRow;
  groups?: AccountGroup[];
  proxyCtx: ProxyBindingContext;
  sequence: number;
  busy: boolean;
  batchTesting: boolean;
  selected: boolean;
  detailOpen: boolean;
  onToggleSelect: () => void;
  onOpenDetail: () => void;
  onTest: () => void;
  onUsage: () => void;
  onRefresh: () => void;
  onToggleEnabled: () => void;
  onEdit: () => void;
  onEditGroups: () => void;
  onEditProxy: () => void;
  onDelete: () => void;
  onUsageRefreshed: () => void;
}) {
  const { t } = useTranslation();
  const disabled = account.enabled === false;
  const isOAuth = account.grok_auth_kind === "oauth";
  const models = account.models ?? [];
  const host = shortHost(account.base_url);
  const label = accountLabel(account);

  return (
    <article
      className={cn(
        "group relative flex min-w-0 cursor-pointer flex-col overflow-hidden rounded-xl border bg-card shadow-sm transition-[border-color,box-shadow,background-color] duration-200",
        detailOpen
          ? "border-primary/60 ring-1 ring-primary/30"
          : selected
            ? "border-primary/40 ring-1 ring-primary/20"
            : "border-border hover:border-border hover:shadow-md",
        disabledAccountSurfaceClass(account),
      )}
      onClick={(event) => {
        const target = event.target as HTMLElement | null;
        if (
          target?.closest(
            'button, a, input, label, [role="menuitem"], [role="menu"], [data-slot="button"]',
          )
        ) {
          return;
        }
        onOpenDetail();
      }}
    >
      {renderDisabledAccountOverlay(account, t)}
      <div className="flex flex-1 flex-col gap-3.5 p-4 sm:p-5">
        {/* Header: identity + status + actions */}
        <div className="flex min-w-0 items-start gap-3">
          <input
            type="checkbox"
            className="mt-1 size-4 shrink-0 cursor-pointer rounded border-border accent-primary"
            aria-label={t("accounts.selectAll")}
            checked={selected}
            onChange={onToggleSelect}
            onClick={(event) => event.stopPropagation()}
          />
          <ModelLogo
            model="grok"
            size={44}
            variant="ring"
            title="Grok"
            className="shrink-0 shadow-sm"
          />

          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <span className="rounded-md bg-muted/80 px-1.5 py-0.5 font-mono text-[11px] font-semibold text-muted-foreground">
                #{sequence}
              </span>
              <StatusBadge
                status={disabled ? "paused" : (account.status ?? "unknown")}
                errorMessage={account.error_message}
              />
              {(account.active_requests ?? 0) > 0 && (
                <span
                  className="inline-flex items-center gap-1 rounded-md bg-blue-50 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-blue-600 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20"
                  title={t("accounts.activeRequestsTooltip", { count: account.active_requests ?? 0 })}
                >
                  <span className="size-1.5 animate-pulse rounded-full bg-blue-500 dark:bg-blue-400" aria-hidden />
                  {account.active_requests}
                </span>
              )}
            </div>
            <h3
              className="mt-1.5 break-all text-[15px] font-semibold leading-snug tracking-tight text-foreground transition-colors hover:text-primary sm:text-base"
              title={label}
            >
              {label}
            </h3>
            {host ? (
              <p
                className="mt-1 max-w-full truncate font-mono text-[11px] leading-tight text-muted-foreground/75"
                title={account.base_url ?? undefined}
              >
                {host}
              </p>
            ) : null}
          </div>

          <div className="flex shrink-0 items-center gap-0.5 rounded-lg border border-border/80 bg-muted/30 p-0.5">
            <GrokAccountActions
              account={account}
              busy={busy}
              batchTesting={batchTesting}
              onTest={onTest}
              onUsage={onUsage}
              onRefresh={onRefresh}
              onToggleEnabled={onToggleEnabled}
              onEdit={onEdit}
              onDelete={onDelete}
            />
          </div>
        </div>

        {/* Meta badges */}
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="inline-flex items-center gap-0.5 rounded-md bg-muted/60 px-1.5 py-0.5 text-[10px] font-medium text-foreground ring-1 ring-inset ring-border">
            <Sparkles className="size-2.5" />
            Grok
          </span>
          <span
            className={cn(
              "inline-flex items-center gap-0.5 rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
              isOAuth
                ? "bg-violet-500/10 text-violet-700 ring-violet-600/20 dark:bg-violet-500/20 dark:text-violet-300 dark:ring-violet-400/20"
                : "bg-sky-500/10 text-sky-700 ring-sky-600/20 dark:bg-sky-500/20 dark:text-sky-300 dark:ring-sky-400/20",
            )}
          >
            {isOAuth ? (
              <FileJson className="size-2.5" />
            ) : (
              <KeyRound className="size-2.5" />
            )}
            {isOAuth
              ? t("grok.authKindOAuthShort")
              : t("grok.authKindApiKey")}
          </span>
          <GrokPlanBadge account={account} compact />
          <GrokGroupChips
            groups={groups}
            onClick={onEditGroups}
            emptyLabel={t("accounts.groupQuickEdit")}
          />
          <AccountProxyBadge
            account={account}
            ctx={proxyCtx}
            onClick={onEditProxy}
          />
        </div>

        {/* Usage panel */}
        <div className="rounded-lg border border-border/70 bg-muted/25 px-3 py-2.5">
          <GrokUsageCell
            account={account}
            compact
            onRefreshed={onUsageRefreshed}
          />
        </div>

        {/* Footer: models + updated */}
        <div className="mt-auto flex min-w-0 flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-3">
          <div className="flex min-w-0 flex-1 flex-wrap items-center gap-1">
            <span className="shrink-0 text-[11px] font-medium text-muted-foreground">
              {t("grok.colModels")}
            </span>
            {models.length === 0 ? (
              <span className="text-[11px] text-muted-foreground/70">
                {t("grok.noModels")}
              </span>
            ) : (
              <>
                {models.slice(0, 4).map((model) => (
                  <span
                    key={model}
                    className="max-w-[9rem] truncate rounded-md bg-background px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground ring-1 ring-inset ring-border"
                    title={model}
                  >
                    {model}
                  </span>
                ))}
                {models.length > 4 ? (
                  <span className="text-[10px] font-medium text-muted-foreground">
                    +{models.length - 4}
                  </span>
                ) : null}
              </>
            )}
          </div>
          <span
            className="shrink-0 text-[11px] text-muted-foreground"
            title={
              account.updated_at
                ? formatBeijingTime(account.updated_at) || undefined
                : undefined
            }
          >
            {account.updated_at
              ? t("grok.updatedAgo", {
                  time: formatRelativeTime(account.updated_at),
                })
              : "—"}
          </span>
        </div>
      </div>
    </article>
  );
}

// 卡片右上角与表格行共用同一组操作按钮，避免两处漂移。
function GrokAccountActions({
  account,
  busy,
  batchTesting,
  onTest,
  onUsage,
  onRefresh,
  onToggleEnabled,
  onEdit,
  onDelete,
}: {
  account: AccountRow;
  busy: boolean;
  batchTesting: boolean;
  onTest: () => void;
  onUsage: () => void;
  onRefresh: () => void;
  onToggleEnabled: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const disabled = account.enabled === false;
  const isOAuth = account.grok_auth_kind === "oauth";

  return (
    <>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        title={t("accounts.testConnection")}
        disabled={busy || batchTesting}
        onClick={onTest}
      >
        <Zap className="size-3.5" />
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        title={t("accounts.usageDetail")}
        onClick={onUsage}
      >
        <BarChart3 className="size-3.5" />
      </Button>
      {isOAuth ? (
        <Button
          variant="ghost"
          size="icon-sm"
          className="size-8"
          title={t("grok.actionRefresh")}
          disabled={busy}
          onClick={onRefresh}
        >
          <RefreshCw className={cn("size-3.5", busy && "animate-spin")} />
        </Button>
      ) : null}
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        title={disabled ? t("grok.actionEnable") : t("grok.actionDisable")}
        disabled={busy}
        onClick={onToggleEnabled}
      >
        {disabled ? (
          <Power className="size-3.5" />
        ) : (
          <PowerOff className="size-3.5" />
        )}
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        title={t("grok.actionEdit")}
        disabled={busy}
        onClick={onEdit}
      >
        <Pencil className="size-3.5" />
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8 text-destructive hover:bg-destructive/10 hover:text-destructive"
        title={t("grok.actionDelete")}
        disabled={busy}
        onClick={onDelete}
      >
        <Trash2 className="size-3.5" />
      </Button>
    </>
  );
}

// 表格行：与 Codex 账号表格同风格的列表布局（仅桌面端渲染）。
function GrokAccountTableRow({
  account,
  groups = [],
  proxyCtx,
  sequence,
  visibleColumns,
  busy,
  batchTesting,
  selected,
  detailOpen,
  onToggleSelect,
  onOpenDetail,
  healthBuckets,
  onTest,
  onUsage,
  onRefresh,
  onToggleEnabled,
  onEdit,
  onEditGroups,
  onEditProxy,
  onDelete,
  onUsageRefreshed,
}: {
  account: AccountRow;
  groups?: AccountGroup[];
  proxyCtx: ProxyBindingContext;
  sequence: number;
  visibleColumns: GrokColumnVisibility;
  busy: boolean;
  batchTesting: boolean;
  selected: boolean;
  detailOpen: boolean;
  onToggleSelect: () => void;
  onOpenDetail: () => void;
  healthBuckets?: AccountHealthBucket[];
  onTest: () => void;
  onUsage: () => void;
  onRefresh: () => void;
  onToggleEnabled: () => void;
  onEdit: () => void;
  onEditGroups: () => void;
  onEditProxy: () => void;
  onDelete: () => void;
  onUsageRefreshed: () => void;
}) {
  const { t } = useTranslation();
  const disabled = account.enabled === false;
  const isOAuth = account.grok_auth_kind === "oauth";
  const models = account.models ?? [];
  const host = shortHost(account.base_url);
  const label = accountLabel(account);
  const tableOverlay = renderDisabledAccountOverlay(account, t, {
    compact: true,
    markerOnly: true,
  });

  return (
    <TableRow
      className={cn(
        "cursor-pointer",
        detailOpen ? "bg-primary/8" : selected && "bg-primary/5",
        disabledAccountTableRowClass(account),
      )}
      onClick={(event) => {
        const target = event.target as HTMLElement | null;
        if (
          target?.closest(
            'button, a, input, label, [role="menuitem"], [role="menu"], [data-slot="button"]',
          )
        ) {
          return;
        }
        onOpenDetail();
      }}
    >
      <TableCell className="w-9">
        <input
          type="checkbox"
          className="size-4 cursor-pointer rounded border-border accent-primary"
          aria-label={t("accounts.selectAll")}
          checked={selected}
          onChange={onToggleSelect}
          onClick={(event) => event.stopPropagation()}
        />
      </TableCell>
      {visibleColumns.sequence ? (
        <TableCell className="font-mono text-[12px] text-muted-foreground">
          #{sequence}
        </TableCell>
      ) : null}
      <TableCell>
        <div className="flex min-w-0 items-center gap-2.5">
          <ModelLogo
            model="grok"
            size={32}
            variant="ring"
            title="Grok"
            className="shrink-0"
          />
          <div className="min-w-0">
            <div className="flex min-w-0 items-center gap-1.5">
              <button
                type="button"
                className="max-w-[200px] truncate text-left text-[13px] font-semibold text-foreground transition-colors hover:text-primary"
                title={t("accounts.openDetail")}
                onClick={(event) => {
                  event.stopPropagation();
                  onOpenDetail();
                }}
              >
                {label}
              </button>
              <span
                className={cn(
                  "inline-flex shrink-0 items-center gap-0.5 whitespace-nowrap rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
                  isOAuth
                    ? "bg-violet-500/10 text-violet-700 ring-violet-600/20 dark:bg-violet-500/20 dark:text-violet-300 dark:ring-violet-400/20"
                    : "bg-sky-500/10 text-sky-700 ring-sky-600/20 dark:bg-sky-500/20 dark:text-sky-300 dark:ring-sky-400/20",
                )}
                title={t("grok.authKind")}
              >
                {isOAuth ? (
                  <FileJson className="size-2.5" />
                ) : (
                  <KeyRound className="size-2.5" />
                )}
                {isOAuth
                  ? t("grok.authKindOAuthShort")
                  : t("grok.authKindApiKey")}
              </span>
            </div>
            <div className="mt-1 flex min-w-0 flex-wrap items-center gap-1">
                <GrokGroupChips
                  groups={groups}
                  onClick={onEditGroups}
                  emptyLabel={t("accounts.groupQuickEdit")}
                />
              </div>
            {host ? (
              <div
                className="max-w-[200px] truncate font-mono text-[11px] text-muted-foreground/75"
                title={account.base_url ?? undefined}
              >
                {host}
              </div>
            ) : null}
          </div>
        </div>
      </TableCell>
      {visibleColumns.plan ? (
        <TableCell className="text-center">
          <GrokPlanBadge account={account} />
        </TableCell>
      ) : null}
      {visibleColumns.proxy ? (
        <TableCell className="min-w-[120px] max-w-[180px]">
          <AccountProxyBadge
            account={account}
            ctx={proxyCtx}
            onClick={onEditProxy}
          />
        </TableCell>
      ) : null}
      {visibleColumns.status ? (
        <TableCell data-account-state-cell="status">
          {tableOverlay ?? (
            <div className="space-y-1.5">
              <StatusBadge
                status={disabled ? "paused" : (account.status ?? "unknown")}
                errorMessage={account.error_message}
              />
              {(account.active_requests ?? 0) > 0 && (
                <span
                  className="inline-flex items-center gap-1 rounded-md bg-blue-50 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-blue-600 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20"
                  title={t("accounts.activeRequestsTooltip", { count: account.active_requests ?? 0 })}
                >
                  <span className="size-1.5 animate-pulse rounded-full bg-blue-500 dark:bg-blue-400" aria-hidden />
                  {account.active_requests}
                </span>
              )}
              <AccountHealthBar buckets={healthBuckets} />
            </div>
          )}
        </TableCell>
      ) : null}
      {visibleColumns.requests ? (
        <TableCell>
          <RequestCountPills account={account} compact />
        </TableCell>
      ) : null}
      {visibleColumns.usage ? (
        <TableCell className="min-w-[170px]">
          <GrokUsageCell account={account} onRefreshed={onUsageRefreshed} />
        </TableCell>
      ) : null}
      {visibleColumns.models ? (
        <TableCell>
          {models.length === 0 ? (
            <span className="text-[12px] text-muted-foreground/70">
              {t("grok.noModels")}
            </span>
          ) : (
            <div className="flex max-w-[150px] flex-wrap items-center gap-1">
              {models.slice(0, 2).map((model) => (
                <span
                  key={model}
                  className="max-w-[9rem] truncate rounded-md bg-muted/50 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground ring-1 ring-inset ring-border"
                  title={model}
                >
                  {model}
                </span>
              ))}
              {models.length > 2 ? (
                <span
                  className="text-[10px] font-medium text-muted-foreground"
                  title={models.join(", ")}
                >
                  +{models.length - 2}
                </span>
              ) : null}
            </div>
          )}
        </TableCell>
      ) : null}
      {visibleColumns.updatedAt ? (
        <TableCell>
          <span
            className="whitespace-nowrap text-[12px] text-muted-foreground"
            title={
              account.updated_at
                ? formatBeijingTime(account.updated_at) || undefined
                : undefined
            }
          >
            {account.updated_at
              ? formatRelativeTime(account.updated_at)
              : "—"}
          </span>
        </TableCell>
      ) : null}
      <TableCell className="text-right">
        <div className="inline-flex items-center gap-0.5">
          <GrokAccountActions
            account={account}
            busy={busy}
            batchTesting={batchTesting}
            onTest={onTest}
            onUsage={onUsage}
            onRefresh={onRefresh}
            onToggleEnabled={onToggleEnabled}
            onEdit={onEdit}
            onDelete={onDelete}
          />
        </div>
      </TableCell>
    </TableRow>
  );
}

function grokFormatDollars(cents?: number | null): string {
  if (cents === null || cents === undefined || !Number.isFinite(cents))
    return "--";
  return `$${(cents / 100).toFixed(2)}`;
}

function grokStateRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function grokStateString(record: Record<string, unknown> | null, ...keys: string[]): string {
  if (!record) return "";
  for (const key of keys) {
    const value = record[key];
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

function grokStateOptionalBoolean(
  record: Record<string, unknown> | null,
  ...keys: string[]
): boolean | null {
  if (!record) return null;
  for (const key of keys) {
    if (typeof record[key] === "boolean") return record[key] as boolean;
  }
  return null;
}

function grokStateNumber(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  const record = grokStateRecord(value);
  if (record && typeof record.val === "number" && Number.isFinite(record.val)) {
    return record.val;
  }
  return null;
}

function findGrokStateFact(state: GrokAccountState | null, kind: string) {
  if (!state) return undefined;
  if (state.facts?.[kind]) return state.facts[kind];
  return Object.values(state.facts ?? {}).find(
    (fact) => fact.kind === kind || fact.kind.endsWith(`_${kind}`),
  );
}

function latestGrokCatalog(state: GrokAccountState | null) {
  if (!state?.catalogs?.length) return undefined;
  return [...state.catalogs].sort((a, b) =>
    (b.snapshot.observed_at ?? "").localeCompare(a.snapshot.observed_at ?? ""),
  )[0];
}

function grokProtocolLabel(protocol: GrokProtocol | string): string {
  if (protocol === "chat_completions") return "Chat Completions";
  if (protocol === "messages") return "Messages";
  if (protocol === "responses") return "Responses";
  return protocol || "—";
}

function grokCapabilityTone(status?: string): string {
  switch ((status ?? "").toLowerCase()) {
    case "ok":
      return "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300";
    case "unauthorized":
    case "subscription_required":
    case "exhausted":
    case "unavailable":
      return "bg-red-500/10 text-red-700 dark:text-red-300";
    case "rate_limited":
    case "version_required":
      return "bg-amber-500/10 text-amber-700 dark:text-amber-300";
    default:
      return "bg-muted text-muted-foreground";
  }
}

function GrokStateDetail({
  account,
  state,
  loading,
  error,
  action,
  onSync,
  onProbe,
}: {
  account: AccountRow;
  state: GrokAccountState | null;
  loading: boolean;
  error: string | null;
  action: "sync" | "probe" | null;
  onSync: () => void;
  onProbe: () => void;
}) {
  const { t } = useTranslation();
  const userFact = findGrokStateFact(state, "user");
  const settingsFact = findGrokStateFact(state, "settings");
  const billingFact = findGrokStateFact(state, "billing");
  const user = grokStateRecord(userFact?.payload);
  const settings = grokStateRecord(settingsFact?.payload);
  const billingRoot = grokStateRecord(billingFact?.payload);
  const billing = grokStateRecord(billingRoot?.config) ?? billingRoot;
  const currentPeriod = grokStateRecord(billing?.currentPeriod ?? billing?.current_period);

  const livePlan = grokStateString(user, "subscriptionTier", "subscription_tier");
  const settingsPlan = grokStateString(
    settings,
    "subscription_tier_display",
    "subscriptionTierDisplay",
  );
  const jwtPlan = state?.identity?.jwt_tier ?? "";
  const archivePlan = state?.identity?.archive_plan ?? account.plan_type ?? "";
  const allowAccess = grokStateOptionalBoolean(settings, "allow_access", "allowAccess");
  const accessLabel =
    allowAccess === true
      ? t("grok.accessAllowed")
      : allowAccess === false
        ? t("grok.accessGated")
        : t("grok.accessUnknown");
  const accessTone =
    allowAccess === true
      ? "text-emerald-700 dark:text-emerald-300"
      : allowAccess === false
        ? "text-red-700 dark:text-red-300"
        : "text-muted-foreground";

  const periodType = grokStateString(currentPeriod, "type") || t("grok.billingPeriodUnknown");
  const periodStart = grokStateString(currentPeriod, "start");
  const periodEnd =
    grokStateString(currentPeriod, "end") ||
    grokStateString(billing, "billingPeriodEnd", "billing_period_end");
  const usagePercent = grokStateNumber(
    billing?.creditUsagePercent ?? billing?.credit_usage_percent,
  );
  const prepaid = grokStateNumber(billing?.prepaidBalance ?? billing?.prepaid_balance);
  const onDemandCap = grokStateNumber(billing?.onDemandCap ?? billing?.on_demand_cap);
  const onDemandUsed = grokStateNumber(billing?.onDemandUsed ?? billing?.on_demand_used);

  const catalog = latestGrokCatalog(state);
  const visibleModels = (catalog?.items ?? []).filter((item) => item.hidden !== true);
  const primaryModel: GrokModelCatalogItem | undefined =
    visibleModels.find((item) => item.model_id === settings?.default_model) ?? visibleModels[0];
  const primaryBackend = primaryModel?.api_backend ?? "";
  const protocols: GrokProtocol[] = ["responses", "chat_completions", "messages"];

  return (
    <section className="space-y-2.5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
          {t("grok.stateTitle")}
        </h3>
        <div className="flex items-center gap-1.5">
          <Button
            type="button"
            variant="outline"
            size="xs"
            disabled={loading || action !== null}
            onClick={onSync}
            title={t("grok.stateSyncHint")}
          >
            <RefreshCw className={cn("size-3", action === "sync" && "animate-spin")} />
            {action === "sync" ? t("grok.stateSyncing") : t("grok.stateSync")}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="xs"
            disabled={loading || action !== null || visibleModels.length === 0}
            onClick={onProbe}
            title={t("grok.capabilityProbeHint")}
          >
            <FlaskConical className={cn("size-3", action === "probe" && "animate-pulse")} />
            {action === "probe" ? t("grok.capabilityProbing") : t("grok.capabilityProbe")}
          </Button>
        </div>
      </div>

      <div className="space-y-3 rounded-xl border border-border bg-card p-3">
        {loading ? (
          <div className="flex items-center gap-2 py-3 text-xs text-muted-foreground">
            <Loader2 className="size-3.5 animate-spin" />
            {t("grok.stateLoading")}
          </div>
        ) : null}
        {error ? (
          <div className="rounded-lg bg-red-500/10 px-2.5 py-2 text-xs text-red-700 dark:text-red-300">
            {error}
          </div>
        ) : null}
        {!loading && !state ? (
          <div className="py-2 text-xs text-muted-foreground">{t("grok.stateEmpty")}</div>
        ) : null}

        {state ? (
          <>
            <div className="grid grid-cols-2 gap-2">
              {[
                [t("grok.planLive"), livePlan || "—", userFact?.observed_at],
                [t("grok.planSettings"), settingsPlan || "—", settingsFact?.observed_at],
                [
                  t("grok.planJwt"),
                  jwtPlan
                    ? `${jwtPlan}${state.identity?.jwt_tier_trust === "verified" ? "" : ` · ${t("grok.planUnverified")}`}`
                    : "—",
                  undefined,
                ],
                [t("grok.planArchive"), archivePlan || "—", undefined],
              ].map(([label, value, observedAt]) => (
                <div key={String(label)} className="min-w-0 rounded-lg bg-muted/35 px-2.5 py-2">
                  <div className="text-[10px] text-muted-foreground">{label}</div>
                  <div className="mt-0.5 truncate text-xs font-semibold" title={String(value)}>
                    {value}
                  </div>
                  {observedAt ? (
                    <div className="mt-0.5 text-[9px] text-muted-foreground/75">
                      {formatRelativeTime(String(observedAt))}
                    </div>
                  ) : null}
                </div>
              ))}
            </div>

            <div className="flex items-center justify-between gap-2 rounded-lg border border-border px-2.5 py-2 text-xs">
              <div>
                <div className="font-semibold">{t("grok.accessGate")}</div>
                <div className="mt-0.5 text-[10px] text-muted-foreground">
                  {allowAccess === null
                    ? t("grok.accessUnknownHint")
                    : settingsFact?.observed_at
                      ? formatBeijingTime(settingsFact.observed_at, "")
                      : "—"}
                </div>
              </div>
              <span className={cn("font-semibold", accessTone)}>{accessLabel}</span>
            </div>

            <div className="space-y-1.5 rounded-lg border border-border px-2.5 py-2 text-xs">
              <div className="flex items-center justify-between gap-2">
                <span className="font-semibold">{t("grok.billingCurrentPeriod")}</span>
                <span className="text-[10px] text-muted-foreground">{periodType}</span>
              </div>
              <div className="grid grid-cols-2 gap-x-3 gap-y-1 text-[11px]">
                <span className="text-muted-foreground">{t("grok.billingUsage")}</span>
                <span className="text-right tabular-nums">
                  {usagePercent === null ? "—" : `${usagePercent.toFixed(1)}%`}
                </span>
                <span className="text-muted-foreground">{t("grok.billingPeriod")}</span>
                <span className="text-right" title={[periodStart, periodEnd].filter(Boolean).join(" ~ ")}>
                  {periodEnd ? formatBeijingTime(periodEnd, "") : "—"}
                </span>
                <span className="text-muted-foreground">{t("grok.billingPrepaid")}</span>
                <span className="text-right tabular-nums">
                  {prepaid === null ? "—" : grokFormatDollars(Math.abs(prepaid))}
                </span>
                <span className="text-muted-foreground">{t("grok.billingPayg")}</span>
                <span className="text-right tabular-nums">
                  {onDemandCap === null
                    ? "—"
                    : `${grokFormatDollars(Math.abs(onDemandUsed ?? 0))} / ${grokFormatDollars(Math.abs(onDemandCap))}`}
                </span>
              </div>
            </div>

            <div className="space-y-2 rounded-lg border border-border px-2.5 py-2 text-xs">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div>
                  <span className="font-semibold">{t("grok.catalogTitle")}</span>
                  <span className="ml-1.5 text-[10px] text-muted-foreground">
                    {t("grok.catalogModels", { count: visibleModels.length })}
                  </span>
                </div>
                <span className="rounded-md bg-primary/10 px-1.5 py-0.5 text-[10px] font-semibold text-primary">
                  {primaryBackend ? grokProtocolLabel(primaryBackend) : t("grok.catalogBackendUnknown")}
                </span>
              </div>
              {visibleModels.length > 0 ? (
                <div className="flex flex-wrap gap-1">
                  {visibleModels.slice(0, 8).map((item) => (
                    <span
                      key={`${item.origin}:${item.model_id}`}
                      className="rounded-md bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground"
                      title={`${item.model_id} · ${grokProtocolLabel(item.api_backend ?? "")}`}
                    >
                      {item.model_id}
                    </span>
                  ))}
                  {visibleModels.length > 8 ? (
                    <span className="rounded-md bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                      +{visibleModels.length - 8}
                    </span>
                  ) : null}
                </div>
              ) : (
                <div className="text-[10px] text-muted-foreground">{t("grok.catalogEmpty")}</div>
              )}

              <div className="grid grid-cols-1 gap-1.5 pt-1 sm:grid-cols-3">
                {protocols.map((protocol) => {
                  const capability = state.capabilities
                    ?.filter((item) => item.protocol === protocol)
                    .sort((a, b) => (b.observed_at ?? "").localeCompare(a.observed_at ?? ""))[0];
                  const native = capability?.status === "ok";
                  const route = native
                    ? t("grok.capabilityNative")
                    : primaryBackend
                      ? protocol === primaryBackend
                        ? t("grok.capabilityCatalogRoute")
                        : t("grok.capabilityConverted", {
                            backend: grokProtocolLabel(primaryBackend),
                          })
                      : t("grok.capabilityUnrouted");
                  return (
                    <div key={protocol} className="rounded-md bg-muted/35 px-2 py-1.5">
                      <div className="truncate text-[10px] font-semibold">
                        {grokProtocolLabel(protocol)}
                      </div>
                      <span
                        className={cn(
                          "mt-1 inline-flex max-w-full rounded px-1 py-0.5 text-[9px] font-semibold",
                          grokCapabilityTone(capability?.status),
                        )}
                        title={capability?.provider_code || capability?.status || route}
                      >
                        {capability?.status || t("grok.capabilityUntested")}
                      </span>
                      <div className="mt-1 truncate text-[9px] text-muted-foreground" title={route}>
                        {route}
                      </div>
                      {capability?.observed_at ? (
                        <div
                          className="mt-0.5 truncate text-[9px] text-muted-foreground/75"
                          title={formatBeijingTime(capability.observed_at, "")}
                        >
                          {formatRelativeTime(capability.observed_at)}
                        </div>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            </div>
          </>
        ) : null}
      </div>
    </section>
  );
}

function GrokUsageCell({
  account,
  onRefreshed,
  compact = false,
  detailed = false,
}: {
  account: AccountRow;
  onRefreshed?: () => void;
  compact?: boolean;
  // detailed 展示 billing 完整视图（产品用量、按量付费、月度金额），卡片视图启用。
  detailed?: boolean;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [refreshing, setRefreshing] = useState(false);

  const handleRefreshUsage = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await api.refreshAccountUsage(account.id);
      onRefreshed?.();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setRefreshing(false);
    }
  };

  const billing = account.grok_billing;
  const weeklyPct = billing?.weekly_percent ?? account.usage_percent_5h;
  const monthlyPct = billing?.monthly_percent ?? account.usage_percent_7d;
  const weeklyResetAt = billing?.weekly_period_end ?? account.reset_5h_at;
  const monthlyResetAt = billing?.monthly_period_end ?? account.reset_7d_at;
  const products = detailed ? (billing?.product_usage ?? []) : [];
  const paygCap = detailed ? (billing?.on_demand_cap_cents ?? null) : null;
  const paygUsed = billing?.on_demand_used_cents ?? null;
  const paygEnabled = paygCap !== null && paygCap > 0;
  const monthlyAmount =
    billing?.monthly_used_cents !== null &&
    billing?.monthly_used_cents !== undefined &&
    billing?.monthly_limit_cents !== null &&
    billing?.monthly_limit_cents !== undefined
      ? `${grokFormatDollars(Math.min(billing.monthly_used_cents, billing.monthly_limit_cents))} / ${grokFormatDollars(billing.monthly_limit_cents)}`
      : undefined;
  const weeklyPeriodTitle =
    billing?.weekly_period_start && billing?.weekly_period_end
      ? `${formatBeijingTime(billing.weekly_period_start, "")} ~ ${formatBeijingTime(billing.weekly_period_end, "")}`
      : undefined;

  const hasWeekly = weeklyPct !== null && weeklyPct !== undefined;
  const hasMonthly = monthlyPct !== null && monthlyPct !== undefined;

  // free 档 token 用量条(滚动 24h 窗口):429 错误体解析的权威值优先(耗尽期间),
  // 否则用逐请求 x-ratelimit 头快照;两者都有时取观测时间较新者。
  const isFreePlan = (account.plan_type ?? "").trim().toLowerCase() === "free";
  const rl = account.grok_rate_limit;
  const fq = account.grok_free_quota;
  const fqFresh =
    fq && Date.now() - new Date(fq.exhausted_at).getTime() < 24 * 3600 * 1000;
  const rlUsable = rl && (rl.limit_tokens ?? 0) > 0;
  // 账号是否处于"限流"状态(涵盖 rate_limited/usage_limited 等所有渲染成"限流"的状态)。
  const usageLimited =
    GROK_LIMITED_STATUSES.has(account.status ?? "") ||
    GROK_LIMITED_STATUSES.has(account.cooldown_reason ?? "");
  let freeQuotaBar: {
    pct: number;
    used: number;
    limit: number;
    observedAt?: string;
    exhausted: boolean;
  } | null = null;
  if (isFreePlan || fqFresh) {
    const preferFq =
      fqFresh &&
      (!rlUsable ||
        !rl?.updated_at ||
        new Date(fq!.exhausted_at).getTime() >=
          new Date(rl.updated_at).getTime());
    if (preferFq) {
      freeQuotaBar = {
        pct: Math.min(100, (fq!.used_tokens / fq!.limit_tokens) * 100),
        used: fq!.used_tokens,
        limit: fq!.limit_tokens,
        observedAt: fq!.exhausted_at,
        exhausted: true,
      };
    } else if (rlUsable && !(isFreePlan && usageLimited)) {
      // 限流的 free 账号不采用 x-ratelimit 快照:它多半是限流前的过时观测(remaining≈满),
      // 会算出误导性的低用量(0.0%)与"限流"徽章矛盾;此时落到下面的"耗尽"兜底灰条。
      const used = Math.max(0, rl!.limit_tokens! - (rl!.remaining_tokens ?? 0));
      freeQuotaBar = {
        pct: Math.min(100, (used / rl!.limit_tokens!) * 100),
        used,
        limit: rl!.limit_tokens!,
        observedAt: rl!.updated_at,
        exhausted: false,
      };
    }
  }

  // 兜底:free 账号显示"限流"却拿不到任何可信用量数字时——超支 402、普通 429、批量测试旧路径
  // 误标 rate_limited、手动置限流、或只有过时的乐观 rl 快照——画一条满格灰条表意"已耗尽",
  // 避免与"限流"徽章观感割裂(退化成 "usage —" 或误显示 0.0%)。不依赖账号重测纠正状态。
  const freeQuotaExhaustedNoDetail =
    !freeQuotaBar && isFreePlan && usageLimited;

  const refreshButton = (
    <button
      type="button"
      onClick={() => void handleRefreshUsage()}
      disabled={refreshing}
      title={t("accounts.refreshUsage")}
      aria-label={t("accounts.refreshUsage")}
      className="shrink-0 rounded-md p-1 text-muted-foreground transition-colors hover:bg-background hover:text-foreground disabled:opacity-50"
    >
      <RefreshCw className={cn("size-3.5", refreshing && "animate-spin")} />
    </button>
  );

  if (
    !hasWeekly &&
    !hasMonthly &&
    products.length === 0 &&
    !freeQuotaBar &&
    !freeQuotaExhaustedNoDetail
  ) {
    return (
      <div className="flex items-center justify-between gap-2">
        <span className="text-[12px] text-muted-foreground">
          {t("accounts.usage")} —
        </span>
        {refreshButton}
      </div>
    );
  }

  // 表格视图用单行内联条压缩行高，明细与重置时间进 tooltip；卡片视图完整展示。
  const inline = !detailed;

  const bars: ReactNode[] = [];
  if (freeQuotaExhaustedNoDetail) {
    // 满格灰条(muted):表意"限流/已耗尽"但无上游用量明细,右侧显示"耗尽"而非百分比。
    bars.push(
      <GrokUsageBar
        key="free-quota-exhausted"
        label={t("grok.freeQuota")}
        shortLabel={t("grok.freeQuotaShort")}
        pct={100}
        tone="muted"
        valueLabel={t("grok.freeQuotaExhaustedShort")}
        titleText={[t("grok.freeQuotaWindow"), t("grok.freeQuotaNoDetail")].join(
          " · ",
        )}
        inline={inline}
      />,
    );
  } else if (freeQuotaBar) {
    bars.push(
      <GrokUsageBar
        key="free-quota"
        label={t("grok.freeQuota")}
        shortLabel={t("grok.freeQuotaShort")}
        pct={freeQuotaBar.pct}
        amountText={
          detailed
            ? `${grokFormatCompactNumber(freeQuotaBar.used)} / ${grokFormatCompactNumber(freeQuotaBar.limit)} ${t("accounts.usageTokUnit")}`
            : undefined
        }
        titleText={[
          t("grok.freeQuotaWindow"),
          `${grokFormatCompactNumber(freeQuotaBar.used)} / ${grokFormatCompactNumber(freeQuotaBar.limit)} ${t("accounts.usageTokUnit")}`,
          freeQuotaBar.exhausted ? t("grok.freeQuotaExhausted") : null,
          freeQuotaBar.observedAt
            ? `${t("grok.rateLimitUpdated")} ${formatBeijingTime(freeQuotaBar.observedAt, "")}`
            : null,
        ]
          .filter(Boolean)
          .join(" · ")}
        inline={inline}
      />,
    );
  }
  if (hasWeekly) {
    bars.push(
      <GrokUsageBar
        key="weekly"
        label={t("grok.quotaWeekly")}
        shortLabel={t("grok.quotaWeeklyShort")}
        pct={weeklyPct!}
        resetAt={weeklyResetAt}
        detail={account.usage_5h_detail}
        titleText={weeklyPeriodTitle}
        inline={inline}
      />,
    );
  }
  for (const [index, item] of products.entries()) {
    bars.push(
      <GrokUsageBar
        key={`product-${index}-${item.product}`}
        label={t("grok.productUsage", { product: item.product })}
        shortLabel={t("grok.productUsage", { product: item.product })}
        pct={item.usage_percent ?? null}
      />,
    );
  }
  if (detailed && paygEnabled) {
    bars.push(
      <GrokUsageBar
        key="payg"
        label={t("grok.payAsYouGo")}
        shortLabel={t("grok.payAsYouGo")}
        pct={
          paygUsed !== null && paygCap! > 0
            ? Math.min(100, Math.max(0, (paygUsed / paygCap!) * 100))
            : null
        }
        amountText={`${grokFormatDollars(paygUsed ?? 0)} / ${grokFormatDollars(paygCap)}`}
      />,
    );
  }
  if (hasMonthly) {
    bars.push(
      <GrokUsageBar
        key="monthly"
        label={t("grok.quotaMonthly")}
        shortLabel={t("grok.quotaMonthlyShort")}
        pct={monthlyPct!}
        resetAt={monthlyResetAt}
        detail={account.usage_7d_detail}
        amountText={detailed ? monthlyAmount : undefined}
        titleText={detailed ? undefined : monthlyAmount}
        inline={inline}
      />,
    );
  }

  return (
    <div className="flex items-center gap-2">
      <div
        className={cn(
          "min-w-0 flex-1",
          compact && bars.length >= 2
            ? "grid grid-cols-1 gap-2.5 sm:grid-cols-2 sm:gap-3"
            : "space-y-2",
        )}
      >
        {bars}
        {detailed && !paygEnabled && billing ? (
          <div className="flex items-center gap-1.5 self-end text-[11px] text-muted-foreground">
            <span className="font-semibold">{t("grok.payAsYouGo")}</span>
            <span>{t("grok.paygDisabled")}</span>
          </div>
        ) : null}
        {detailed && account.grok_rate_limit ? (
          <div
            className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5 self-end text-[11px] text-muted-foreground sm:col-span-2"
            title={
              account.grok_rate_limit.updated_at
                ? `${t("grok.rateLimitUpdated")} ${formatBeijingTime(account.grok_rate_limit.updated_at, "")}`
                : undefined
            }
          >
            <span className="font-semibold">{t("grok.rateLimitLabel")}</span>
            <span className="tabular-nums">
              {grokFormatCompactNumber(account.grok_rate_limit.remaining_tokens)}
              /
              {grokFormatCompactNumber(account.grok_rate_limit.limit_tokens)}{" "}
              {t("accounts.usageTokUnit")}
            </span>
            <span className="text-muted-foreground/60">·</span>
            <span className="tabular-nums">
              {grokFormatCompactNumber(account.grok_rate_limit.remaining_requests)}
              /
              {grokFormatCompactNumber(account.grok_rate_limit.limit_requests)}{" "}
              {t("accounts.usageReqUnit")}
            </span>
          </div>
        ) : null}
      </div>
      {refreshButton}
    </div>
  );
}

function grokUsageBarColor(pct: number): string {
  if (pct >= 90) return "bg-destructive";
  if (pct >= 70) return "bg-[hsl(var(--warning))]";
  return "bg-[hsl(var(--success))]";
}

function grokUsageTrackColor(pct: number): string {
  if (pct >= 90) return "bg-destructive/15";
  if (pct >= 70) return "bg-[hsl(var(--warning))]/15";
  return "bg-[hsl(var(--success))]/15";
}

function grokUsageTextColor(pct: number): string {
  if (pct >= 90) return "text-destructive";
  if (pct >= 70) return "text-[hsl(var(--warning))]";
  return "text-[hsl(var(--success))]";
}

function grokFormatResetAt(
  resetAt?: string | null,
): { label: string; title: string } | null {
  if (!resetAt) return null;
  const d = new Date(resetAt);
  if (Number.isNaN(d.getTime()) || d.getTime() <= Date.now()) return null;
  const full = formatBeijingTime(resetAt, "");
  if (!full) return null;
  return { label: full.slice(5), title: full };
}

function grokFormatCompactNumber(value?: number): string {
  const n = Number(value || 0);
  if (n >= 1_000_000)
    return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`;
  return String(n);
}

function GrokUsageBar({
  label,
  shortLabel,
  pct,
  resetAt,
  detail,
  amountText,
  titleText,
  inline = false,
  tone,
  valueLabel,
}: {
  label: string;
  shortLabel: string;
  // pct 为 null 时表示上游未给出该项用量（渲染 "--" 与空进度条）。
  pct: number | null;
  resetAt?: string | null;
  detail?: AccountRow["usage_5h_detail"];
  amountText?: string;
  titleText?: string;
  // inline 渲染单行紧凑条（表格视图），明细/重置时间收进 tooltip。
  inline?: boolean;
  // tone="muted" 用中性灰渲染整条（无权威用量、仅表意"耗尽"时用）。
  tone?: "muted";
  // valueLabel 覆盖右侧的百分比文本（如"耗尽"），不传则显示 pct%。
  valueLabel?: string;
}) {
  const { t } = useTranslation();
  const resetTime = grokFormatResetAt(resetAt);
  const hasDetail = Boolean(
    detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0),
  );
  const detailText = hasDetail
    ? `${grokFormatCompactNumber(detail?.requests)} ${t("accounts.usageReqUnit")} / ${grokFormatCompactNumber(detail?.tokens)} ${t("accounts.usageTokUnit")}`
    : "";
  const clamped = pct === null ? 0 : Math.min(100, Math.max(0, pct));
  const muted = tone === "muted";
  const barColor = muted ? "bg-muted-foreground/40" : grokUsageBarColor(clamped);
  const trackColor = muted
    ? "bg-muted"
    : pct === null
      ? "bg-muted/60"
      : grokUsageTrackColor(clamped);
  const textColor =
    muted || pct === null ? "text-muted-foreground" : grokUsageTextColor(clamped);
  const valueText = valueLabel ?? (pct === null ? "--" : `${pct.toFixed(1)}%`);

  if (inline) {
    const tooltip = [label, titleText, detailText || null]
      .filter(Boolean)
      .join(" · ");
    return (
      <div className="min-w-0" title={tooltip || undefined}>
        <div className="flex min-w-0 items-center gap-1.5">
          <span className="w-7 shrink-0 text-[11px] font-semibold text-muted-foreground">
            {shortLabel}
          </span>
          <div
            className={cn(
              "h-2 min-w-0 flex-1 overflow-hidden rounded-full",
              trackColor,
            )}
          >
            <div
              className={cn(
                "h-full rounded-full transition-all duration-300",
                barColor,
              )}
              style={{ width: `${clamped}%` }}
            />
          </div>
          <span
            className={cn(
              "w-11 shrink-0 text-right text-[11px] font-semibold tabular-nums",
              textColor,
            )}
          >
            {valueText}
          </span>
        </div>
        {resetTime ? (
          <div
            className="mt-0.5 pl-[34px] text-[10px] font-medium text-muted-foreground/80"
            title={resetTime.title}
          >
            {/* 表格空间紧张，重置时间去掉秒（完整时间在 tooltip） */}
            ⏱ {t("grok.quotaReset")} {resetTime.label.slice(0, 11)}
          </div>
        ) : null}
      </div>
    );
  }

  return (
    <div className="min-w-0">
      <div className="mb-1 flex items-center justify-between gap-2">
        <span
          className="truncate text-[11px] font-semibold text-muted-foreground"
          title={titleText ?? label}
        >
          <span className="sm:hidden">{shortLabel}</span>
          <span className="hidden sm:inline">{label}</span>
        </span>
        <span className="flex min-w-0 shrink-0 items-baseline gap-1.5">
          {amountText ? (
            <span className="text-[10px] font-medium tabular-nums text-muted-foreground">
              {amountText}
            </span>
          ) : null}
          <span
            className={cn("text-[12px] font-semibold tabular-nums", textColor)}
          >
            {valueText}
          </span>
        </span>
      </div>
      <div className={cn("h-2 overflow-hidden rounded-full", trackColor)}>
        <div
          className={cn(
            "h-full rounded-full transition-all duration-300",
            barColor,
          )}
          style={{ width: `${clamped}%` }}
        />
      </div>
      {detailText ? (
        <div className="mt-1 text-[10px] font-medium text-muted-foreground">
          {detailText}
        </div>
      ) : null}
      {resetTime ? (
        <div
          className="mt-0.5 text-[10px] font-medium text-muted-foreground/80"
          title={resetTime.title}
        >
          ⏱ {t("grok.quotaReset")} {resetTime.label}
        </div>
      ) : null}
    </div>
  );
}

type TestEvent = {
  type: string;
  text?: string;
  model?: string;
  success?: boolean;
  error?: string;
};

function GrokTestConnectionModal({
  account,
  onClose,
  onSettled,
}: {
  account: AccountRow;
  onClose: () => void;
  onSettled: () => void;
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

  useEffect(() => {
    const accountModels = (account.models ?? []).filter(
      (m) => m.trim() && !m.toLowerCase().includes("image"),
    );
    const next =
      accountModels.length > 0
        ? accountModels
        : [...DEFAULT_GROK_TEST_MODELS];
    setModelOptions(next);
    setSelectedModel(next[0] ?? "");
    setModelOptionsReady(true);
  }, [account.models]);

  useEffect(() => {
    if (!modelOptionsReady || !selectedModel) return;

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
        let receivedTerminal = false;

        const processLines = (lines: string[]) => {
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
                  if (event.text) setOutput((prev) => [...prev, event.text!]);
                  break;
                case "test_complete":
                  receivedTerminal = true;
                  setStatus(event.success ? "success" : "error");
                  markSettled();
                  break;
                case "error":
                  receivedTerminal = true;
                  setStatus("error");
                  setErrorMsg(event.error || t("accounts.unknownError"));
                  markSettled();
                  break;
              }
            } catch {
              /* ignore */
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
          processLines(lines);
        }
        if (buffer.trim()) processLines([buffer]);
        if (!receivedTerminal) {
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

    const timer = window.setTimeout(() => void run(), 50);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [account.id, markSettled, modelOptionsReady, selectedModel, t]);

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
    success: CheckCircle2,
    error: XCircle,
  }[status];
  const statusIconSpin = status === "connecting" || status === "streaming";
  const statusColor = {
    connecting: "text-muted-foreground",
    streaming: "text-[hsl(var(--info))]",
    success: "text-[hsl(var(--success))]",
    error: "text-destructive",
  }[status];

  return (
    <Modal
      show
      title={t("accounts.testConnectionTitle", {
        account: accountLabel(account),
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
            className={cn(
              "flex items-center gap-1.5 text-sm font-semibold",
              statusColor,
            )}
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
            options={modelOptions.map((item) => ({
              label: item,
              value: item,
            }))}
            placeholder={model || t("settings.testModel")}
            disabled={!modelOptionsReady || modelOptions.length === 0}
          />
        </div>

        {(output.length > 0 ||
          status === "connecting" ||
          status === "streaming") && (
          <div
            className="max-h-[240px] min-h-[80px] overflow-auto rounded-lg border border-border bg-muted/30 p-3 text-[13px] leading-relaxed break-all whitespace-pre-wrap"
            style={{ fontFamily: "var(--font-geist-mono)" }}
          >
            {output.length === 0 && status === "connecting" ? (
              <span className="animate-pulse text-muted-foreground">
                {t("accounts.sendingTestRequest")}
              </span>
            ) : (
              output.join("")
            )}
            <div ref={outputEndRef} />
          </div>
        )}

        {status === "error" && errorMsg ? (
          <div className="rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
            {errorMsg}
          </div>
        ) : null}
      </div>
    </Modal>
  );
}

// downloadBlob 触发浏览器下载（与 Codex 账号页同款实现）。
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

// memo 边界:宿主 Accounts 组件的 codex 侧状态变化不应连带整棵 Grok 视图重渲染。
export default memo(GrokAccounts);
