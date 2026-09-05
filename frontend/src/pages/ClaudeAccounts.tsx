import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  X,
  Activity,
  Sparkles,
  Coins,
  BarChart3,
  Pencil,
  ExternalLink,
  RefreshCw,
  RotateCcw,
  Lock,
  MoreHorizontal,
  Trash2,
  Columns3,
  Plus,
  CheckCircle,
  XCircle,
  Loader2,
  FlaskConical,
  SlidersHorizontal,
  Download,
  Upload,
} from "lucide-react";

import { api, getAdminKey } from "../api";
import type { NamedBlob } from "../api";
import type { ProxyRow } from "../api";
import type {
  AccountRow,
  AccountGroup,
  AccountListSummary,
  AccountEmailDomainFacet,
  AccountPageStatsItem,
  AccountHealthBucket,
  ClaudeCredentialExportEntry,
} from "../types";
import AccountUsageModal from "../components/AccountUsageModal";
import AccountDetailSheet from "../components/AccountDetailSheet";
import AccountHealthBar from "../components/AccountHealthBar";
import RequestCountPills from "../components/RequestCountPills";
import { CompactStat } from "../components/CompactStat";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import AccountQuotaDistributionChart from "../components/AccountQuotaDistributionChart";
import AccountRateLimitRecoveryChart from "../components/AccountRateLimitRecoveryChart";
import StateShell from "../components/StateShell";
import type { AccountAnalysisResponse } from "../types";
import { ProxyField } from "../components/ProxyField";
import AccountProxyBadge from "../components/AccountProxyBadge";
import AccountProxyQuickEditor from "../components/AccountProxyQuickEditor";
import {
  buildProxyBindingContext,
  type ProxyBindingContext,
} from "../lib/accountProxyBinding";
import ChipInput from "../components/ChipInput";
import { AccountGroupManagerModal, ACCOUNT_GROUP_COLORS } from "../components/AccountGroupManagerModal";
import { Select } from "../components/ui/select";
import ChannelLogo from "../components/ChannelLogo";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import StatusBadge from "../components/StatusBadge";
import Pagination from "../components/Pagination";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  isAccountGroupFilterEmpty,
  pruneAccountGroupFilter,
} from "../components/AccountGroupFilterSelect";
import type { AccountGroupFilterValue } from "../components/AccountGroupFilterSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import {
  accountStateTableRowClass,
  resolveAccountOverlayKind,
  renderAccountStateOverlay,
} from "../components/AccountStateOverlay";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { getErrorMessage } from "../utils/error";
import { getAccountStatusBadgeStatus } from "../lib/usageFormat";
import {
  CLAUDE_TIMEZONE_CUSTOM,
  CLAUDE_TIMEZONE_OPTIONS,
  claudeTimezoneLabel,
  findClaudeTimezoneOption,
} from "../lib/claudeAccountOptions";

const FALLBACK_GROUP_COLOR = "#2563eb";
function normalizeGroupColor(color?: string): string {
  const v = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(v) ? v : FALLBACK_GROUP_COLOR;
}

// extractCode 从粘贴内容里取授权码：支持整条回调 URL、code#state、或纯 code。
function extractCode(input: string): string {
  const raw = input.trim();
  if (!raw) return "";
  if (raw.startsWith("http://") || raw.startsWith("https://")) {
    try {
      const u = new URL(raw);
      const code = u.searchParams.get("code");
      if (code) return code.trim();
    } catch {
      // fall through
    }
  }
  return raw;
}

// claudeUsagePct 取用量百分比(0-100)。后端解析 Anthropic 统一限流头后,
// usage_percent_5h/7d 为真实窗口利用率;null/undefined 表示尚无上游观测。
function claudeUsagePct(v: unknown): number | null {
	if (v === null || v === undefined || (typeof v === "string" && v.trim() === "")) return null;
	const n = typeof v === "number" ? v : Number(v);
	return Number.isFinite(n) && n >= 0 ? Math.min(100, n) : null;
}

function usageTone(pct: number): string {
  return pct >= 90 ? "bg-rose-500" : pct >= 70 ? "bg-amber-500" : "bg-emerald-500";
}

// formatCompactNum 紧凑数字:1234 → 1.2k。
function formatCompactNum(v: unknown): string {
  const n = typeof v === "number" ? v : Number(v);
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(Math.round(n));
}

// pad2 两位补零。
const pad2 = (n: number) => String(n).padStart(2, "0");

// formatShortDateTime "MM-DD HH:mm" 短格式(与 Codex 卡片的 ⏱ 重置时间一致口径)。
function formatShortDateTime(iso?: string): { label: string; title: string } | null {
  if (!iso) return null;
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return null;
  return {
    label: `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`,
    title: d.toLocaleString(),
  };
}

// formatRelativeShort 相对时间:刚刚 / Xm / Xh / Xd 前。
function formatRelativeShort(iso: string | undefined, t: (k: string) => string): string {
  if (!iso) return "-";
  const ts = new Date(iso).getTime();
  if (!Number.isFinite(ts)) return "-";
  const diff = Math.max(0, Date.now() - ts);
  const m = Math.floor(diff / 60000);
  if (m < 1) return t("claude.justNow");
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h${m % 60}m`;
  return `${Math.floor(h / 24)}d${h % 24}h`;
}

// maybeOfferSaveProxyToPool 手动输入(非代理池)的代理保存后,若该代理不在代理管理中,
// 询问是否存入代理池,方便后续复用与负载均衡。confirm 返回 true 才写入。
async function maybeOfferSaveProxyToPool(
  url: string,
  proxies: ProxyRow[],
  confirm: (opts: { title: string; description: string }) => Promise<boolean>,
  showToast: (msg: string, type?: "success" | "error") => void,
  t: (k: string, o?: Record<string, unknown>) => string,
): Promise<void> {
  const trimmed = url.trim();
  if (!trimmed) return;
  if (proxies.some((p) => p.url === trimmed)) return; // 已在池中
  const ok = await confirm({
    title: t("claude.saveProxyToPoolTitle"),
    description: trimmed,
  });
  if (!ok) return;
  try {
    await api.addProxies({ url: trimmed });
    showToast(t("claude.saveProxyToPoolDone"), "success");
  } catch (error) {
    showToast(getErrorMessage(error), "error");
  }
}

// downloadNamedBlob 处理管理员凭据导出。文件名只信任后端的安全响应头，
// 缺省时使用固定回退名；对象 URL 使用后立即回收，避免大号池导出长期占内存。
function downloadNamedBlob(payload: NamedBlob, fallbackName: string): void {
  const objectURL = URL.createObjectURL(payload.blob)
  const anchor = document.createElement("a")
  anchor.href = objectURL
  anchor.download = payload.filename || fallbackName
  anchor.rel = "noopener"
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000)
}

// avatarInitial 头像首字母。
function avatarInitial(acc: AccountRow): string {
  const s = (acc.email || acc.name || "").trim();
  return s ? s[0].toUpperCase() : "C";
}

// claudePlanBadge 按订阅档位配色(pro/max-5x/max-20x/team/enterprise/free)。
function claudePlanBadge(plan: string): { label: string; cls: string } {
  const p = plan.trim().toLowerCase();
  const base = "inline-flex items-center rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset";
  switch (p) {
    case "pro":
      return { label: "Pro", cls: `${base} bg-purple-50 text-purple-700 ring-purple-600/20 dark:bg-purple-950 dark:text-purple-300 dark:ring-purple-400/20` };
    case "max-5x":
      return { label: "Max 5x", cls: `${base} bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20` };
    case "max-20x":
      return { label: "Max 20x", cls: `${base} bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20` };
    case "max":
      return { label: "Max", cls: `${base} bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20` };
    case "team":
      return { label: "Team", cls: `${base} bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20` };
    case "enterprise":
      return { label: "Enterprise", cls: `${base} bg-indigo-50 text-indigo-700 ring-indigo-600/20 dark:bg-indigo-950 dark:text-indigo-300 dark:ring-indigo-400/20` };
    case "business":
      return { label: "Business", cls: `${base} bg-indigo-50 text-indigo-700 ring-indigo-600/20 dark:bg-indigo-950 dark:text-indigo-300 dark:ring-indigo-400/20` };
    case "free":
      return { label: "Free", cls: `${base} bg-zinc-100 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-400 dark:ring-zinc-500/20` };
    default:
      return { label: plan, cls: `${base} bg-purple-50 text-purple-700 ring-purple-600/20 dark:bg-purple-950 dark:text-purple-300 dark:ring-purple-400/20` };
  }
}

// Claude 模型白名单边界：该页面只允许原生 Claude 模型，不能把其它
// provider 的模型误写入 Claude 账号。后端 endpoint 仍会做通用名称校验，
// 这里再做一次 provider-aware 过滤，避免管理端误配导致调度边界漂移。
const CLAUDE_MODEL_ID_RE = /^claude-[a-z0-9][a-z0-9._-]*$/i;

function isClaudeModelID(value: unknown): value is string {
  return typeof value === "string" && CLAUDE_MODEL_ID_RE.test(value.trim());
}

function normalizeClaudeModelList(values: unknown): string[] {
  if (!Array.isArray(values)) return [];
  const seen = new Set<string>();
  const result: string[] = [];
  for (const value of values) {
    if (!isClaudeModelID(value)) continue;
    const model = value.trim();
    const key = model.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    result.push(model);
  }
  return result;
}

function parseClaudeModelTokens(raw: string): { accepted: string[]; rejected: string[] } {
  const accepted: string[] = [];
  const rejected: string[] = [];
  for (const token of raw.split(/[\s,，、]+/).map((item) => item.trim()).filter(Boolean)) {
    if (isClaudeModelID(token)) accepted.push(token);
    else rejected.push(token);
  }
  return { accepted: normalizeClaudeModelList(accepted), rejected };
}

function mergeClaudeModelLists(...lists: unknown[]): string[] {
  return normalizeClaudeModelList(lists.flatMap((list) => Array.isArray(list) ? list : []));
}

// 状态过滤项 → 后端 status 参数。
type ClaudeStatusFilter =
  | "all"
  | "normal"
  | "scheduling"
  | "rate_limited"
  | "abnormal"
  | "banned"
  | "error"
  | "unsampled"
  | "disabled"
  | "locked";

type AuthFilter = "all" | "oauth" | "api_key";
type HealthTier = "healthy" | "warm" | "risky" | "banned";

type SortKey = "default" | "group" | "priority" | "usage" | "requests" | "today";
const SORT_MAP: Record<SortKey, { sort: NonNullable<Parameters<typeof api.getAccountsPage>[0]["sort"]> | undefined; order: "asc" | "desc" }> = {
  // An explicit updated_at sort is unstable because sampling/refresh updates
  // that timestamp. Omitting sort uses the backend's deterministic ID order,
  // matching Codex and keeping rows in place after refresh.
  default: { sort: undefined, order: "asc" },
  group: { sort: "group", order: "asc" },
  priority: { sort: "scheduler_priority", order: "desc" },
  usage: { sort: "usage", order: "desc" },
  requests: { sort: "requests", order: "desc" },
  today: { sort: "today", order: "desc" },
};

// 可显隐列(序号/邮箱/操作为固定核心列,不参与切换)。持久化到 localStorage,与 Codex 一致。
const CLAUDE_TOGGLE_COLUMNS = [
  "groups",
  "proxy",
  "priority",
  "plan",
  "status",
  "today",
  "requests",
  "usage",
  "cost",
  "importTime",
  "updatedAt",
] as const;
type ClaudeCol = (typeof CLAUDE_TOGGLE_COLUMNS)[number];
type ClaudeColVisibility = Record<ClaudeCol, boolean>;
const CLAUDE_COLS_KEY = "codex2api:claude-accounts:visible-columns";
// 分析面板显隐同样持久化,避免切到 Codex 页再切回来时又展开;默认收起。
const CLAUDE_ANALYSIS_VISIBILITY_KEY = "codex2api:claude-accounts:analysis-visible";

function loadClaudeAnalysisVisibility(): boolean {
  try {
    return window.localStorage.getItem(CLAUDE_ANALYSIS_VISIBILITY_KEY) === "true";
  } catch {
    return false;
  }
}

function persistClaudeAnalysisVisibility(visible: boolean) {
  try {
    window.localStorage.setItem(CLAUDE_ANALYSIS_VISIBILITY_KEY, visible ? "true" : "false");
  } catch {
    /* localStorage 不可用时仅保留会话内状态 */
  }
}

function defaultClaudeCols(): ClaudeColVisibility {
  return Object.fromEntries(CLAUDE_TOGGLE_COLUMNS.map((c) => [c, true])) as ClaudeColVisibility;
}

function loadClaudeCols(): ClaudeColVisibility {
  const fallback = defaultClaudeCols();
  try {
    const raw = window.localStorage.getItem(CLAUDE_COLS_KEY);
    if (!raw) return fallback;
    const parsed = JSON.parse(raw) as Partial<ClaudeColVisibility>;
    return Object.fromEntries(
      CLAUDE_TOGGLE_COLUMNS.map((c) => [c, typeof parsed[c] === "boolean" ? parsed[c] : true]),
    ) as ClaudeColVisibility;
  } catch {
    return fallback;
  }
}

// LiveCountdown 显示限流/重置的剩余时间,每秒刷新。
// plain=true 为弱化文本样式(用量条下的 ⏱ 重置行);默认琥珀徽章(限流冷却)。
function LiveCountdown({ until, label, plain = false }: { until?: string; label: string; plain?: boolean }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!until) return;
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [until]);
  if (!until) return null;
  const target = new Date(until).getTime();
  if (!Number.isFinite(target)) return null;
  const remain = Math.max(0, Math.floor((target - now) / 1000));
  if (remain <= 0) return null;
  const d = Math.floor(remain / 86400);
  const h = Math.floor((remain % 86400) / 3600);
  const m = Math.floor((remain % 3600) / 60);
  const s = remain % 60;
  const text = d > 0 ? `${d}d${h}h` : h > 0 ? `${h}h${m}m` : m > 0 ? `${m}m${s}s` : `${s}s`;
  if (plain) {
    return (
      <span className="text-[11px] font-medium text-muted-foreground tabular-nums">
        {label} {text}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1 rounded-md bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-600 tabular-nums dark:text-amber-400">
      {label} {text}
    </span>
  );
}

// UsageWindow 单条用量窗口(5h / 7d)。视觉对齐 Codex 的 UsageBar/UsageWindowStat:
// - percent 有真实观测(Anthropic 统一限流头)→ 进度条 + 百分比 + ⏱重置倒计时;
// - 仅有网关侧明细(req/tok/$)→ 明细行;
// - 两者都无 → 不渲染(由父级统一显示 "-")。
function UsageWindow({
  label,
  pct,
  reset,
  resetLabel,
  detail,
}: {
  label: string;
  pct: number | null;
  reset?: string;
  resetLabel: string;
  detail?: AccountRow["usage_5h_detail"];
}) {
  const hasDetail = !!detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0);
  const billed = typeof detail?.account_billed === "number" && detail.account_billed > 0 ? detail.account_billed : null;
  if (pct === null && !hasDetail) return null;
  const rt = formatShortDateTime(reset);
  // 明细(req/tok/$)进 tooltip,行内只留 标签+进度条+百分比+⏱重置,收窄整列。
  const detailTitle = [
    hasDetail ? `${formatCompactNum(detail?.requests)} req / ${formatCompactNum(detail?.tokens)} tok` : "",
    billed !== null ? `$${billed.toFixed(4)}` : "",
    rt ? `${resetLabel} ${rt.title}` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <div className="flex items-center gap-1.5 whitespace-nowrap" title={detailTitle || undefined}>
      <span className="w-[30px] shrink-0 text-[11px] font-medium text-muted-foreground">{label}</span>
      <span className="h-1.5 w-14 shrink-0 overflow-hidden rounded-full bg-muted">
        {pct !== null ? (
          <span className={cn("block h-full rounded-full transition-all", usageTone(pct))} style={{ width: `${Math.min(100, pct)}%` }} />
        ) : null}
      </span>
      <span className="w-[40px] shrink-0 text-right text-[12px] font-semibold tabular-nums">
        {pct !== null ? `${pct.toFixed(1)}%` : "—"}
      </span>
      {rt ? (
        <span className="shrink-0 text-[10px] tabular-nums text-muted-foreground/70">⏱{rt.label}</span>
      ) : null}
    </div>
  );
}

// Upper bound on concurrent one-time usage backfill requests per page mount.
const LEGACY_USAGE_BACKFILL_BATCH = 4;

function ClaudeScopedUsageWindows({ windows }: { windows?: AccountRow["claude_usage_windows"] }) {
  const { t } = useTranslation();
  const scoped = (windows ?? []).filter((window) => window.model_scoped && window.name !== "5h" && window.name !== "7d");
  if (scoped.length === 0) return null;
  return (
    <>
      {scoped.map((window) => (
        <UsageWindow
          key={window.name}
          label={window.model_family === "fable" ? "Fable" : (window.label || window.name)}
          pct={claudeUsagePct(window.utilization)}
          reset={window.reset_at}
          resetLabel={t("claude.resetIn")}
        />
      ))}
    </>
  );
}

function ClaudeConcurrencyBadge({ acc }: { acc: AccountRow }) {
  const { t } = useTranslation();
  const active = Math.max(0, acc.active_requests ?? 0);
  const occupied = Math.max(active, acc.occupied_requests ?? active);
  if (occupied === 0) return null;
  const buffered = occupied - active;
  const showOccupied = acc.session_slot_buffer_enabled === true;
  return (
    <span
      className="inline-flex items-center gap-1 rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium tabular-nums text-blue-700 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-950 dark:text-blue-300"
      title={showOccupied
        ? t("accounts.occupiedRequestsTooltip", { active, occupied, buffered })
        : t("accounts.activeRequestsTooltip", { count: active })}
    >
      <span className="size-1.5 animate-pulse rounded-full bg-blue-500" aria-hidden />
      {showOccupied ? `${active}/${occupied}` : active}
    </span>
  );
}

export default function ClaudeAccounts({ headerSlot }: { headerSlot?: ReactNode } = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [summary, setSummary] = useState<AccountListSummary | null>(null);
  const [tags, setTags] = useState<string[]>([]);
  const [domains, setDomains] = useState<AccountEmailDomainFacet[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  // 代理池开关 + 全局代理:代理徽章要靠这两个才能把"未自绑"的账号判成继承池/全局/直连。
  const [proxyPoolEnabled, setProxyPoolEnabled] = useState(false);
  const [globalProxyURL, setGlobalProxyURL] = useState("");
  const [quickProxyAccount, setQuickProxyAccount] = useState<AccountRow | null>(null);
  const [groups, setGroups] = useState<AccountGroup[]>([]);

  const [showAdd, setShowAdd] = useState(false);
  const [addInitialTab, setAddInitialTab] = useState<"oauth" | "import">("oauth");
  const [exporting, setExporting] = useState(false);
  const [authJsonExportingIds, setAuthJsonExportingIds] = useState<Set<number>>(new Set());
  const [showManageGroups, setShowManageGroups] = useState(false);
  const [assignTarget, setAssignTarget] = useState<AccountRow | null>(null);
  const [usageTarget, setUsageTarget] = useState<AccountRow | null>(null);
  const [editTarget, setEditTarget] = useState<AccountRow | null>(null);
  const [modelsTarget, setModelsTarget] = useState<AccountRow | null>(null);
  const [detailTarget, setDetailTarget] = useState<AccountRow | null>(null);
  const [testingTarget, setTestingTarget] = useState<AccountRow | null>(null);
  const detailAbortRef = useRef<AbortController | null>(null);
  const detailRequestSeqRef = useRef(0);
  useEffect(() => () => detailAbortRef.current?.abort(), []);
  // page-stats 独立拉取:分页基础行不含 5h/7d/今日 的网关侧用量明细,单独补齐(与 Codex 页同构)。
  const [pageStats, setPageStats] = useState<Record<string, AccountPageStatsItem>>({});
  const [pageStatsToken, setPageStatsToken] = useState(0);
  const [liveState, setLiveState] = useState<Record<string, { active_requests: number; occupied_requests: number }>>({});
  const [liveSessionSlotBufferEnabled, setLiveSessionSlotBufferEnabled] = useState(false);
  // 健康状态条(近 200 分钟成败分桶,与 Codex 卡片同源接口)。
  const [healthBars, setHealthBars] = useState<Record<string, AccountHealthBucket[]>>({});
  // 额度分布 + 限流恢复分析(号池模式面板,与 Codex 同源接口/组件)。
  const [analysis, setAnalysis] = useState<AccountAnalysisResponse | null>(null);
  const [showAnalysis, setShowAnalysis] = useState(loadClaudeAnalysisVisibility);
  useEffect(() => {
    persistClaudeAnalysisVisibility(showAnalysis);
  }, [showAnalysis]);
  const [analysisLoading, setAnalysisLoading] = useState(false);
  const [analysisError, setAnalysisError] = useState<string | null>(null);
  const analysisAbortRef = useRef<AbortController | null>(null);

  const loadAnalysis = useCallback(async () => {
    if (!showAnalysis) return;
    analysisAbortRef.current?.abort();
    const controller = new AbortController();
    analysisAbortRef.current = controller;
    setAnalysisLoading(true);
    setAnalysisError(null);
    try {
      const res = await api.getAccountAnalysis("claude", controller.signal);
      if (!controller.signal.aborted) setAnalysis(res);
    } catch (error) {
      if (!controller.signal.aborted) setAnalysisError(getErrorMessage(error));
    } finally {
      if (analysisAbortRef.current === controller) {
        analysisAbortRef.current = null;
        setAnalysisLoading(false);
      }
    }
  }, [showAnalysis]);

  const samplingSignature = useMemo(
    () => accounts.map((acc) => `${acc.id}:${acc.claude_usage_probe_at ?? ""}:${acc.claude_usage_probe_error ?? ""}`).join("|"),
    [accounts],
  );
  useEffect(() => {
    void loadAnalysis();
    return () => analysisAbortRef.current?.abort();
  }, [loadAnalysis, samplingSignature]);

  // 过滤 / 排序 / 分页
  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<ClaudeStatusFilter>("all");
  const [healthTier, setHealthTier] = useState<HealthTier | null>(null);
  const [planFilter, setPlanFilter] = useState<string>("all");
  const [authFilter, setAuthFilter] = useState<AuthFilter>("all");
  const [tagFilter, setTagFilter] = useState<string>("all");
  const [domainFilter, setDomainFilter] = useState<string>("all");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(EMPTY_ACCOUNT_GROUP_FILTER);
  const [sortKey, setSortKey] = useState<SortKey>("default");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [hideDomainTags, setHideDomainTags] = useState(false);
  const [visibleCols, setVisibleCols] = useState<ClaudeColVisibility>(loadClaudeCols);
  useEffect(() => {
    try {
      window.localStorage.setItem(CLAUDE_COLS_KEY, JSON.stringify(visibleCols));
    } catch {
      /* localStorage 不可用时忽略 */
    }
  }, [visibleCols]);
  const [knownPlans, setKnownPlans] = useState<string[]>([]);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const reloadAbortRef = useRef<AbortController | null>(null);
  const reloadGenerationRef = useRef(0);
  const legacyUsageRefreshRef = useRef<Set<number>>(new Set());

  // 搜索防抖
  useEffect(() => {
    const id = window.setTimeout(() => setDebouncedSearch(search.trim()), 300);
    return () => window.clearTimeout(id);
  }, [search]);

  // 筛选变化时回到第一页
  useEffect(() => {
    setPage(1);
  }, [debouncedSearch, statusFilter, healthTier, planFilter, authFilter, tagFilter, domainFilter, groupFilter, sortKey, pageSize]);

  const claudeGroups = useMemo(() => groups.filter((g) => g.channel === "claude"), [groups]);
  const groupMap = useMemo(() => new Map(claudeGroups.map((g) => [g.id, g])), [claudeGroups]);

  const reloadGroups = useCallback(async () => {
    try {
      const res = await api.listAccountGroups();
      setGroups(res.groups ?? []);
    } catch {
      /* ignore */
    }
  }, []);

  const reload = useCallback(async (options?: { silent?: boolean }) => {
    reloadAbortRef.current?.abort();
    if (!options?.silent) setLoading(true);
    if (!options?.silent) setLoadError(null);
    const controller = new AbortController();
    reloadAbortRef.current = controller;
    const generation = ++reloadGenerationRef.current;
    try {
      const { sort, order } = SORT_MAP[sortKey];
      const res = await api.getAccountsPage(
        {
          channel: "claude",
          page,
          pageSize,
          search: debouncedSearch || undefined,
          status: statusFilter === "all" ? undefined : statusFilter,
          healthTier: healthTier ?? undefined,
          plan: planFilter === "all" ? undefined : planFilter,
          authKind: authFilter === "all" ? undefined : authFilter,
          tag: tagFilter === "all" ? undefined : tagFilter,
          emailDomain: domainFilter === "all" ? undefined : domainFilter,
          groupInclude: groupFilter.include,
          groupExclude: groupFilter.exclude,
          ungrouped: groupFilter.ungrouped,
          sort,
          order,
        },
        controller.signal,
      );
      if (controller.signal.aborted || generation !== reloadGenerationRef.current) return;
      const rows = res.accounts ?? [];
      setLoadError(null);
      setAccounts(rows);
      setSummary(res.summary ?? null);
      setTags(res.facets?.tags ?? []);
      setDomains(res.facets?.email_domains ?? []);
      setTotal(res.total ?? rows.length);
      if (res.page && res.page !== page) setPage(res.page);
      // 累积已知套餐,供套餐 Tab 使用。
      setKnownPlans((prev) => {
        const set = new Set(prev);
        for (const r of rows) if (r.plan_type) set.add(r.plan_type);
        return set.size === prev.length ? prev : Array.from(set);
      });
    } catch (error) {
      if (!controller.signal.aborted && generation === reloadGenerationRef.current) {
        const message = getErrorMessage(error);
        setLoadError(message);
        if (!options?.silent) showToast(message, "error");
      }
    } finally {
      if (!options?.silent && !controller.signal.aborted && generation === reloadGenerationRef.current) setLoading(false);
    }
  }, [
    page,
    pageSize,
    debouncedSearch,
    statusFilter,
    healthTier,
    planFilter,
    authFilter,
    tagFilter,
    domainFilter,
    groupFilter,
    sortKey,
    showToast,
  ]);

  useEffect(() => {
    void reload();
    return () => reloadAbortRef.current?.abort();
  }, [reload]);

  // 导入接口只负责入队，首轮 native Messages 采样在后台完成。对仍未
  // 采样的 Claude 账号做有限次数静默轮询，让页面自动显示采样结果，同时
  // 避免无限刷新或在后台标签页持续制造请求。
  const pendingSamplingKey = useMemo(
    () => accounts
      .filter((acc) => acc.claude_api && !acc.claude_usage_probe_at && !acc.claude_usage_probe_error)
      .map((acc) => acc.id)
      .join(","),
    [accounts],
  );
  useEffect(() => {
    if (!pendingSamplingKey) return undefined;
    let attempts = 0;
    let requestInFlight = false;
    const maxAttempts = 20;
    const samplingPollTimer = window.setInterval(() => {
      if (attempts >= maxAttempts) {
        window.clearInterval(samplingPollTimer);
        return;
      }
      if (document.visibilityState === "hidden") return;
      if (requestInFlight) return;
      attempts += 1;
      requestInFlight = true;
      void reload({ silent: true }).finally(() => {
        requestInFlight = false;
      });
    }, 3000);
    return () => window.clearInterval(samplingPollTimer);
  }, [pendingSamplingKey, reload]);

  // Older Claude rows were sampled before the OAuth usage-window field
  // existed. Refresh each such row exactly once so the model-scoped Fable
  // window is backfilled without the operator clicking every row. The backend
  // marks the row as probed on every attempt (even with no windows), so rows
  // whose OAuth endpoint is unavailable never re-qualify and never trigger the
  // paid Messages fallback again on the next page visit.
  const legacyUsageRefreshKey = useMemo(
    () => accounts
      .filter((acc) => acc.claude_api && Boolean(acc.claude_usage_probe_at) && !acc.claude_usage_windows_probed && !acc.claude_usage_probe_error)
      .map((acc) => acc.id)
      .join(","),
    [accounts],
  );
  useEffect(() => {
    if (!legacyUsageRefreshKey) return undefined;
    const pending = legacyUsageRefreshKey
      .split(",")
      .map(Number)
      .filter((id) => Number.isFinite(id) && !legacyUsageRefreshRef.current.has(id));
    if (pending.length === 0) return undefined;
    const batch = pending.slice(0, LEGACY_USAGE_BACKFILL_BATCH);
    batch.forEach((id) => legacyUsageRefreshRef.current.add(id));
    let cancelled = false;
    void Promise.all(
      batch.map((id) => api.refreshAccountUsage(id).catch(() => null)),
    ).finally(() => {
      if (!cancelled) void reload({ silent: true });
    });
    return () => {
      cancelled = true;
    };
  }, [legacyUsageRefreshKey, reload]);

  const mergeLiveStateIntoAccount = useCallback((account: AccountRow): AccountRow => {
    const live = liveState[String(account.id)];
    return live
      ? {
          ...account,
          active_requests: live.active_requests,
          occupied_requests: live.occupied_requests,
          session_slot_buffer_enabled: liveSessionSlotBufferEnabled,
        }
      : account;
  }, [liveSessionSlotBufferEnabled, liveState]);

  useEffect(() => {
    if (!detailTarget) return;
    const live = liveState[String(detailTarget.id)];
    if (!live) return;
    setDetailTarget((current) => current && current.id === detailTarget.id
      ? {
          ...current,
          active_requests: live.active_requests,
          occupied_requests: live.occupied_requests,
          session_slot_buffer_enabled: liveSessionSlotBufferEnabled,
        }
      : current);
  }, [detailTarget?.id, liveSessionSlotBufferEnabled, liveState]);

  const refreshOpenDetail = useCallback(async (id: number) => {
    if (detailTarget?.id !== id) return;
    detailAbortRef.current?.abort();
    const controller = new AbortController();
    detailAbortRef.current = controller;
    const requestSeq = ++detailRequestSeqRef.current;
    try {
      const detail = await api.getAccount(id, controller.signal);
      if (!controller.signal.aborted && requestSeq === detailRequestSeqRef.current) {
        setDetailTarget((current) => current?.id === id ? mergeLiveStateIntoAccount(detail) : current);
      }
    } catch {
      // The list refresh remains authoritative if the optional detail refresh fails.
    } finally {
      if (detailAbortRef.current === controller) detailAbortRef.current = null;
    }
  }, [detailTarget?.id, mergeLiveStateIntoAccount]);

  const openDetail = useCallback(async (acc: AccountRow) => {
    detailAbortRef.current?.abort();
    const controller = new AbortController();
    detailAbortRef.current = controller;
    const requestSeq = ++detailRequestSeqRef.current;
    setDetailTarget(mergeLiveStateIntoAccount(acc));
    try {
      const detail = await api.getAccount(acc.id, controller.signal);
      if (!controller.signal.aborted && requestSeq === detailRequestSeqRef.current) {
        setDetailTarget(mergeLiveStateIntoAccount(detail));
      }
    } catch {
      // 列表行本身已包含安全的基础信息，详情请求失败时仍可查看。
    } finally {
      if (detailAbortRef.current === controller) detailAbortRef.current = null;
    }
  }, [mergeLiveStateIntoAccount]);

  const closeDetail = useCallback(() => {
    detailAbortRef.current?.abort();
    detailRequestSeqRef.current += 1;
    setDetailTarget(null);
  }, []);

  // 模型白名单编辑始终从详情接口读取最新代际，避免用户在列表停留期间
  // 账号刷新/换 token 后把旧配置覆盖回去。Modal 内保存时还会做一次
  // updated_at 乐观并发校验，后端 endpoint 只负责持久化已过滤的模型名。
  const openModelsEditor = useCallback(async (acc: AccountRow) => {
    try {
      const detail = await api.getAccount(acc.id);
      if (detail.claude_api !== true) {
        throw new Error(t("claude.modelsWhitelistNotClaude"));
      }
      setModelsTarget(detail);
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [showToast, t]);

  const handleSaveDetailCooldownPolicy = useCallback(async (account: AccountRow, data: {
    mode: "off" | "fixed" | "adaptive" | null;
    seconds: number | null;
    backoff_enabled: boolean | null;
  }) => {
    try {
      await api.updateAccountModelCooldownPolicy(account.id, data);
      showToast(t("accounts.modelCooldownPolicySaved"), "success");
      await refreshOpenDetail(account.id);
      void reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [refreshOpenDetail, reload, showToast, t]);

  const handleClearDetailCooldown = useCallback(async (account: AccountRow, model: string) => {
    try {
      await api.clearAccountModelCooldown(account.id, model);
      showToast(t("accounts.modelCooldownCleared", { model }), "success");
      await refreshOpenDetail(account.id);
      void reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [refreshOpenDetail, reload, showToast, t]);

  const handleClearAllDetailCooldowns = useCallback(async (account: AccountRow) => {
    try {
      const result = await api.clearAllAccountModelCooldowns(account.id);
      showToast(t("accounts.allModelCooldownsCleared", { count: result.cleared }), "success");
      await refreshOpenDetail(account.id);
      void reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [refreshOpenDetail, reload, showToast, t]);

  const handleClaudeTestSettled = useCallback(() => {
    void reload({ silent: true });
    void loadAnalysis();
  }, [loadAnalysis, reload]);

  // 拉取当前页账号的网关侧用量明细(req/tok/$,5h/7d/今日窗口)。
  const accountIDsKey = useMemo(() => accounts.map((a) => a.id).join(","), [accounts]);
  useEffect(() => {
    if (!accountIDsKey) {
      setPageStats({});
      return;
    }
    const controller = new AbortController();
    void api
      .getAccountPageStats(accountIDsKey.split(",").map(Number), controller.signal)
      .then((res) => {
        if (!controller.signal.aborted) setPageStats(res.stats ?? {});
      })
      .catch(() => {
        /* stats 失败不阻断列表 */
      });
    return () => controller.abort();
  }, [accountIDsKey, pageStatsToken]);

  // 当前页会话占用是易变状态，单独轻量轮询，避免把整页账号快照频繁
  // 重拉；切页/卸载时立即取消，保证旧页数据不会覆盖新页。
  useEffect(() => {
    if (!accountIDsKey) {
      setLiveState({});
      setLiveSessionSlotBufferEnabled(false);
      return undefined;
    }
    const controller = new AbortController();
    let active = true;
    let requestInFlight = false;
    let requestSeq = 0;
    const ids = accountIDsKey.split(",").map(Number);
    const loadLiveState = async () => {
      if (requestInFlight) return;
      requestInFlight = true;
      const currentSeq = ++requestSeq;
      try {
        const res = await api.getAccountLiveState(ids, controller.signal);
        if (active && !controller.signal.aborted && currentSeq === requestSeq) {
          setLiveState(res.accounts ?? {});
          setLiveSessionSlotBufferEnabled(res.session_slot_buffer_enabled === true);
        }
      } catch {
        // 实时状态失败不阻断账号列表，保留上一次快照。
      } finally {
        requestInFlight = false;
      }
    };
    void loadLiveState();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void loadLiveState();
    }, 5000);
    return () => {
      active = false;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [accountIDsKey]);

  // 刷新单个账号用量:触发上游探针(有则)+ 重拉本页 page-stats 明细。
  const handleRefreshUsage = useCallback(
    async (acc: AccountRow) => {
      try {
        const refreshed = await api.refreshAccountUsage(acc.id);
        setAccounts((prev) =>
          prev.map((row) =>
            row.id === acc.id
              ? {
                  ...row,
                  ...(refreshed.usage_percent_5h !== undefined ? { usage_percent_5h: refreshed.usage_percent_5h } : {}),
                  ...(refreshed.usage_percent_7d !== undefined ? { usage_percent_7d: refreshed.usage_percent_7d } : {}),
                  ...(refreshed.reset_5h_at ? { reset_5h_at: refreshed.reset_5h_at } : {}),
                  ...(refreshed.reset_7d_at ? { reset_7d_at: refreshed.reset_7d_at } : {}),
                  ...(row.claude_api && refreshed.claude_usage_probe_at
                    ? {
                        claude_usage_probe_at: refreshed.claude_usage_probe_at,
                        claude_usage_probe_error: refreshed.claude_usage_probe_error,
                      }
                    : {}),
                  ...(row.claude_api && refreshed.claude_usage_windows_probed
                    ? { claude_usage_windows_probed: true, claude_usage_windows: refreshed.claude_usage_windows ?? [] }
                    : {}),
                }
              : row,
          ),
        );
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
      setPageStatsToken((v) => v + 1);
      void reload({ silent: true });
    },
    [reload, showToast],
  );

  // 健康状态条数据。
  useEffect(() => {
    if (!accountIDsKey) {
      setHealthBars({});
      return;
    }
    let cancelled = false;
    void api
      .getAccountHealthBars(accountIDsKey.split(",").map(Number))
      .then((res) => {
        if (!cancelled) setHealthBars(res.buckets ?? {});
      })
      .catch(() => {
        /* 健康条失败不阻断列表 */
      });
    return () => {
      cancelled = true;
    };
  }, [accountIDsKey]);

  // 渲染行 = 基础行 + page-stats 补齐(只补缺失字段,基础行已有的以基础行为准)。
  const displayRows = useMemo(() => {
    return accounts.map((acc) => {
      const stats = pageStats[String(acc.id)];
      const live = liveState[String(acc.id)];
      if (!stats && !live) return acc;
      const merged = { ...acc };
      if (live) {
        merged.active_requests = live.active_requests;
        merged.occupied_requests = live.occupied_requests;
        merged.session_slot_buffer_enabled = liveSessionSlotBufferEnabled;
      }
      if (stats) {
        if (!merged.usage_5h_detail && stats.usage_5h_detail) merged.usage_5h_detail = stats.usage_5h_detail;
        if (!merged.usage_7d_detail && stats.usage_7d_detail) merged.usage_7d_detail = stats.usage_7d_detail;
        if (!merged.usage_today_detail && stats.usage_today_detail) merged.usage_today_detail = stats.usage_today_detail;
        if (merged.official_usd == null && stats.official_usd != null) merged.official_usd = stats.official_usd;
        if (merged.official_usd_7d == null && stats.official_usd_7d != null) merged.official_usd_7d = stats.official_usd_7d;
      }
      return merged;
    });
  }, [accounts, liveSessionSlotBufferEnabled, liveState, pageStats]);

  useEffect(() => {
    void reloadGroups();
    let cancelled = false;
    void api
      .listProxies()
      .then((res) => {
        if (!cancelled) setProxyPool(res.proxies ?? []);
      })
      .catch(() => {
        if (!cancelled) setProxyPool([]);
      });
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
  }, [reloadGroups]);

  // 分组用全量 groups 而非 claudeGroups:后端解析组代理不看渠道,按渠道过滤会把
  // 跨渠道的存量成员误报成"无组代理"。
  const proxyBindingCtx = useMemo<ProxyBindingContext>(
    () =>
      buildProxyBindingContext({
        proxies: proxyPool,
        groups,
        poolEnabled: proxyPoolEnabled,
        globalProxy: globalProxyURL,
      }),
    [proxyPool, groups, proxyPoolEnabled, globalProxyURL],
  );

  useEffect(() => {
    setGroupFilter((current) => pruneAccountGroupFilter(current, claudeGroups));
  }, [claudeGroups]);

  // ── 账号操作 ──────────────────────────────────────────────
  const handleDelete = useCallback(
    async (acc: AccountRow) => {
      const ok = await confirm({
        title: t("claude.deleteConfirm"),
        description: acc.email || acc.name || `#${acc.id}`,
      });
      if (!ok) return;
      try {
        await api.deleteAccount(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [confirm, reload, showToast, t],
  );

  const handleRefresh = useCallback(
    async (acc: AccountRow) => {
      try {
        await api.refreshAccount(acc.id);
        await refreshOpenDetail(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [refreshOpenDetail, reload, showToast],
  );

  const handleRefreshModels = useCallback(
    async (acc: AccountRow) => {
      try {
        const res = await api.refreshClaudeModels(acc.id);
        showToast(t("claude.modelsRefreshed", { count: res.count }));
        await refreshOpenDetail(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [refreshOpenDetail, reload, showToast, t],
  );

	const handleRefreshAllModels = useCallback(async () => {
		try {
			const result = await api.refreshAllClaudeModels();
			showToast(t("claude.allModelsRefreshedSummary", result), result.failed > 0 ? "warning" : "success");
			void reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [reload, showToast, t]);

  const handleToggleEnabled = useCallback(
    async (acc: AccountRow) => {
      const next = acc.enabled === false;
      try {
        await api.toggleAccountEnabled(acc.id, next);
        showToast(next ? t("claude.enabledToast") : t("claude.disabledToast"), "success");
        await refreshOpenDetail(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [refreshOpenDetail, reload, showToast, t],
  );

  const handleToggleLock = useCallback(
    async (acc: AccountRow) => {
      const next = !acc.locked;
      try {
        await api.toggleAccountLock(acc.id, next);
        showToast(next ? t("claude.lockedToast") : t("claude.unlockedToast"), "success");
        await refreshOpenDetail(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [refreshOpenDetail, reload, showToast, t],
  );

  const handleResetStatus = useCallback(
    async (acc: AccountRow) => {
      try {
        await api.resetAccountStatus(acc.id);
        showToast(t("claude.statusReset"), "success");
        await refreshOpenDetail(acc.id);
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [refreshOpenDetail, reload, showToast, t],
  );

  // ── 批量操作 ──────────────────────────────────────────────
  const selectedIds = useMemo(() => Array.from(selected), [selected]);

  const handleExport = useCallback(async (scope: "all" | "healthy" | "selected") => {
    if (scope === "selected" && selectedIds.length === 0) return;
    const ids = scope === "selected" ? selectedIds : undefined;
    const ok = await confirm({
      title: scope === "selected" ? t("claude.exportSelectedConfirmTitle") : t("claude.exportConfirmTitle"),
      description: scope === "selected"
        ? t("claude.exportSelectedConfirmDescription", { count: selectedIds.length })
        : t("claude.exportConfirmDescription"),
    });
    if (!ok) return;
    setExporting(true);
    try {
      const result = await api.exportClaudeAccounts(ids, scope === "healthy" ? "healthy" : "all");
      downloadNamedBlob(result, "codex2api-claude-credentials.json");
      showToast(t("claude.exportSuccess", { count: result.count ?? (ids?.length || 1) }), "success");
    } catch (error) {
      showToast(t("claude.exportFailed") + ": " + getErrorMessage(error), "error");
    } finally {
      setExporting(false);
    }
  }, [confirm, selectedIds, showToast, t]);

  const handleExportOne = useCallback(async (account: AccountRow) => {
    setAuthJsonExportingIds((current) => new Set(current).add(account.id));
    try {
      const result = await api.exportClaudeAccounts([account.id], "all");
      downloadNamedBlob(result, `claude-account-${account.id}.json`);
      showToast(t("claude.exportSuccess", { count: result.count ?? 1 }), "success");
    } catch (error) {
      showToast(t("claude.exportFailed") + ": " + getErrorMessage(error), "error");
    } finally {
      setAuthJsonExportingIds((current) => {
        const next = new Set(current);
        next.delete(account.id);
        return next;
      });
    }
  }, [showToast, t]);

  const toggleSelect = useCallback((id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);
  const allSelected = accounts.length > 0 && accounts.every((a) => selected.has(a.id));
  const toggleSelectAll = useCallback(() => {
    setSelected((prev) => {
      if (accounts.every((a) => prev.has(a.id))) return new Set();
      return new Set(accounts.map((a) => a.id));
    });
  }, [accounts]);

  const runBatch = useCallback(
    async (patch: { enabled?: boolean; locked?: boolean }) => {
      if (selectedIds.length === 0) return;
      try {
        await api.batchUpdateAccounts({ ids: selectedIds, ...patch });
        setSelected(new Set());
        void reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [selectedIds, reload, showToast],
  );

  // ── 派生 UI 数据 ──────────────────────────────────────────
  const statChips = useMemo(() => {
    const s = summary;
    const c: Array<{ id: ClaudeStatusFilter; label: string; count: number; tone?: string }> = [
      { id: "all", label: t("claude.statAll"), count: s?.total ?? total },
      { id: "normal", label: t("claude.statNormal"), count: s?.normal ?? 0, tone: "text-emerald-600 dark:text-emerald-400" },
      { id: "scheduling", label: t("claude.statScheduling"), count: s?.active ?? 0, tone: "text-sky-600 dark:text-sky-400" },
      { id: "rate_limited", label: t("claude.statRateLimited"), count: s?.rate_limited ?? 0, tone: "text-amber-600 dark:text-amber-400" },
      { id: "abnormal", label: t("claude.statAbnormal"), count: s?.abnormal ?? 0, tone: "text-rose-600 dark:text-rose-400" },
      { id: "banned", label: t("claude.statBanned"), count: s?.banned ?? 0, tone: "text-rose-600 dark:text-rose-400" },
      { id: "error", label: t("claude.statError"), count: s?.error ?? 0, tone: "text-rose-600 dark:text-rose-400" },
      { id: "unsampled", label: t("claude.statUnsampled"), count: s?.unsampled ?? 0 },
      { id: "disabled", label: t("claude.statDisabled"), count: s?.disabled ?? 0 },
      { id: "locked", label: t("claude.statLocked"), count: s?.locked ?? 0 },
    ];
    return c;
  }, [summary, total, t]);

  const healthChips = useMemo(() => {
    const s = summary;
    return [
      { id: "healthy" as HealthTier, label: t("claude.healthHealthy"), count: s?.healthy ?? 0, dot: "bg-emerald-500" },
      { id: "warm" as HealthTier, label: t("claude.healthWarm"), count: s?.warm ?? 0, dot: "bg-amber-500" },
      { id: "risky" as HealthTier, label: t("claude.healthRisky"), count: s?.risky ?? 0, dot: "bg-rose-500" },
      { id: "banned" as HealthTier, label: t("claude.healthBanned"), count: s?.banned ?? 0, dot: "bg-zinc-500" },
    ];
  }, [summary, t]);

  const planTabs = useMemo(() => {
    const plans = knownPlans.filter(Boolean).sort();
    return ["all", ...plans];
  }, [knownPlans]);

  // Claude 账号当前只支持 OAuth；不展示一个永远为 0 的 API Key 筛选，避免
  // 运营误以为 Claude API Key 可以走同一原生链路。
  const authTabs: Array<{ id: AuthFilter; label: string; count?: number }> = [
    { id: "all", label: t("claude.authAll") },
    { id: "oauth", label: t("claude.authOAuth"), count: summary?.oauth || summary?.total || 0 },
  ];

  const filtersActive =
    statusFilter !== "all" ||
    healthTier !== null ||
    planFilter !== "all" ||
    authFilter !== "all" ||
    tagFilter !== "all" ||
    domainFilter !== "all" ||
    !isAccountGroupFilterEmpty(groupFilter) ||
    sortKey !== "default" ||
    debouncedSearch.length > 0;

  const clearFilters = useCallback(() => {
    setStatusFilter("all");
    setHealthTier(null);
    setPlanFilter("all");
    setAuthFilter("all");
    setTagFilter("all");
    setDomainFilter("all");
    setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
    setSortKey("default");
    setSearch("");
  }, []);

  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  const selectFieldCls =
    "h-8 rounded-md border border-input bg-background px-2 text-xs text-foreground outline-none focus-visible:border-ring";

  return (
    <div>
      <PageHeader
        title={t("claude.title")}
        description={t("claude.subtitle")}
        hideTitle={Boolean(headerSlot)}
        actionsBelow
        titleAdornment={headerSlot}
        onRefresh={() => { void reload(); void loadAnalysis(); }}
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" onClick={() => setShowAnalysis((v) => !v)}>
              <BarChart3 className="size-3.5" />
              {showAnalysis ? t("usage.hideAnalysis") : t("usage.showAnalysis")}
            </Button>
            <Button variant="outline" size="sm" onClick={() => void handleRefreshAllModels()}>
              {t("claude.refreshAllModels")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={exporting}
              onClick={() => void handleExport(selectedIds.length > 0 ? "selected" : "all")}
            >
              <Download className="size-3.5" />
              {exporting ? t("claude.exporting") : selectedIds.length > 0 ? t("claude.exportSelected") : t("claude.exportAll")}
            </Button>
            <Button variant="outline" size="sm" onClick={() => setShowManageGroups(true)}>
              {t("claude.manageGroups")}
            </Button>
            <Button
              variant="outline"
              onClick={() => {
                setAddInitialTab("import");
                setShowAdd(true);
              }}
            >
              <Upload className="size-3.5" />
              {t("claude.importCredentials")}
            </Button>
            <Button
              onClick={() => {
                setAddInitialTab("oauth");
                setShowAdd(true);
              }}
            >
              {t("claude.addAccount")}
            </Button>
          </div>
        }
      />

      {/* 统计卡(复用共享 CompactStat,与 Codex 同款:状态药丸 + 5h/7d·封禁/错误 details) */}
      <div className="mb-4 grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-5">
        <CompactStat
          label={t("accounts.totalAccounts")}
          chipLabel={t("claude.statAll")}
          value={summary?.total ?? total}
          tone="neutral"
          active={statusFilter === "all"}
          onClick={() => setStatusFilter("all")}
        />
        <CompactStat
          label={t("accounts.normalAccounts")}
          chipLabel={t("claude.statNormal")}
          value={summary?.normal ?? 0}
          tone="success"
          active={statusFilter === "normal"}
          onClick={() => setStatusFilter(statusFilter === "normal" ? "all" : "normal")}
        />
        <CompactStat
          label={t("accounts.schedulingAccounts")}
          chipLabel={t("claude.statScheduling")}
          value={summary?.active ?? 0}
          tone="warning"
          active={statusFilter === "scheduling"}
          onClick={() => setStatusFilter(statusFilter === "scheduling" ? "all" : "scheduling")}
        />
        <CompactStat
          label={t("accounts.rateLimited")}
          chipLabel={t("claude.statRateLimited")}
          value={summary?.rate_limited ?? 0}
          tone="warning"
          active={statusFilter === "rate_limited"}
          details={[
            { label: "5h", value: summary?.rate_limited_5h ?? 0 },
            { label: "7d", value: summary?.rate_limited_7d ?? 0 },
          ]}
          onClick={() => setStatusFilter(statusFilter === "rate_limited" ? "all" : "rate_limited")}
        />
        <CompactStat
          label={t("accounts.abnormalAccounts")}
          chipLabel={t("claude.statAbnormal")}
          value={summary?.abnormal ?? 0}
          tone="danger"
          active={statusFilter === "abnormal"}
          details={[
            { label: t("accounts.abnormalBannedShort"), value: summary?.banned ?? 0 },
            { label: t("accounts.abnormalErrorShort"), value: summary?.error ?? 0 },
          ]}
          onClick={() => setStatusFilter(statusFilter === "abnormal" ? "all" : "abnormal")}
        />
      </div>

      {/* 额度分布 + 限流恢复(号池模式分析面板,与 Codex 同款组件) */}
      {showAnalysis && analysis ? (
        <div className="mb-4 grid items-stretch gap-4 xl:grid-cols-2">
          <AccountQuotaDistributionChart
            analysis={analysis.quota}
            compact
            className="min-w-0"
            onRefreshAnalysis={() => void loadAnalysis()}
            onProbeError={(message) => showToast(message, "error")}
            descKey="claude.quotaDesc"
            emptyKey="claude.quotaEmpty"
            showProbe={false}
          />
          <AccountRateLimitRecoveryChart analysis={analysis} compact className="min-w-0" />
        </div>
      ) : showAnalysis ? (
        <div className="mb-4 rounded-xl border border-dashed border-border bg-muted/20 px-4 py-5 text-sm text-muted-foreground">
          {analysisLoading ? t("common.loading") : analysisError ? (
            <div className="flex flex-wrap items-center justify-between gap-3">
              <span className="break-words">{analysisError}</span>
              <Button variant="outline" size="sm" onClick={() => void loadAnalysis()}>{t("common.retry")}</Button>
            </div>
          ) : t("common.loading")}
        </div>
      ) : null}

      {/* 统计芯片 */}
      <div className="mb-3 flex flex-wrap items-center gap-1.5">
        {statChips.map((chip) => {
          const active = statusFilter === chip.id;
          return (
            <button
              key={chip.id}
              type="button"
              onClick={() => setStatusFilter(chip.id)}
              className={cn(
                "inline-flex items-center gap-1.5 rounded-lg border px-2.5 py-1 text-xs font-semibold transition-colors",
                active
                  ? "border-primary/40 bg-primary/10 text-primary"
                  : "border-border bg-muted/40 text-muted-foreground hover:text-foreground",
              )}
            >
              <span className={cn(!active && chip.tone)}>{chip.label}</span>
              <span className="rounded-md bg-background/60 px-1 text-[10px] font-bold tabular-nums">{chip.count}</span>
            </button>
          );
        })}
      </div>

      {/* 调度视图(点击按健康档过滤) */}
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <span className="text-[11px] font-medium text-muted-foreground">{t("claude.schedulingView")}</span>
        {healthChips.map((h) => {
          const active = healthTier === h.id;
          return (
            <button
              key={h.id}
              type="button"
              onClick={() => setHealthTier(active ? null : h.id)}
              className={cn(
                "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs transition-colors",
                active ? "bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground",
              )}
            >
              <span className={cn("size-1.5 rounded-full", h.dot)} />
              {h.label}
              <span className="font-semibold tabular-nums text-foreground">{h.count}</span>
            </button>
          );
        })}
      </div>

      {/* 套餐 Tab */}
      {planTabs.length > 1 ? (
        <div className="mb-2 flex flex-wrap items-center gap-1">
          {planTabs.map((p) => {
            const active = planFilter === p;
            return (
              <button
                key={p}
                type="button"
                onClick={() => setPlanFilter(p)}
                className={cn(
                  "rounded-md px-2 py-1 text-xs font-medium transition-colors",
                  active ? "bg-primary text-primary-foreground" : "bg-muted/40 text-muted-foreground hover:text-foreground",
                )}
              >
                {p === "all" ? t("claude.planAll") : p}
              </button>
            );
          })}
        </div>
      ) : null}

      {/* 过滤条:OAuth/API + 分组 + 标签 + 域名 + 排序 + 搜索 */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <div className="inline-flex overflow-hidden rounded-md border border-border">
          {authTabs.map((a) => (
            <button
              key={a.id}
              type="button"
              onClick={() => setAuthFilter(a.id)}
              className={cn(
                "px-2.5 py-1 text-xs font-medium transition-colors",
                authFilter === a.id ? "bg-primary text-primary-foreground" : "bg-background text-muted-foreground hover:text-foreground",
              )}
            >
              {a.label}
              {typeof a.count === "number" ? <span className="ml-1 tabular-nums opacity-70">{a.count}</span> : null}
            </button>
          ))}
        </div>

        <AccountGroupFilterSelect
          groups={claudeGroups}
          value={groupFilter}
          onChange={setGroupFilter}
          className="w-40"
        />

        <Select
          compact
          className="w-32"
          value={tagFilter}
          onValueChange={setTagFilter}
          options={[{ value: "all", label: t("claude.allTags") }, ...tags.map((tag) => ({ value: tag, label: tag }))]}
        />

        <Select
          compact
          className="w-36"
          value={domainFilter}
          onValueChange={setDomainFilter}
          options={[
            { value: "all", label: t("claude.allDomains") },
            ...domains.map((d) => ({ value: d.domain, label: `${d.domain} (${d.total})` })),
          ]}
        />

        <Select
          compact
          className="w-32"
          value={sortKey}
          onValueChange={(v) => setSortKey(v as SortKey)}
          options={[
            { value: "default", label: t("claude.sortDefault") },
            { value: "group", label: t("claude.sortGroup") },
            { value: "priority", label: t("claude.sortPriority") },
            { value: "usage", label: t("claude.sortUsage") },
            { value: "requests", label: t("claude.sortRequests") },
            { value: "today", label: t("claude.sortToday") },
          ]}
        />

        <button
          type="button"
          onClick={() => setHideDomainTags((v) => !v)}
          className={cn(
            "rounded-md border border-border px-2 py-1 text-xs transition-colors",
            hideDomainTags ? "bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {hideDomainTags ? t("claude.showDomainTags") : t("claude.hideDomainTags")}
        </button>

        <ColumnsMenu visible={visibleCols} onChange={setVisibleCols} />

        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder={t("claude.searchPlaceholder")}
          className="h-8 max-w-xs flex-1"
        />

        {filtersActive ? (
          <Button variant="ghost" size="sm" onClick={clearFilters}>
            <X className="size-3.5" />
            {t("claude.clearFilters")}
          </Button>
        ) : null}
      </div>

      {/* 批量操作条 */}
      {selectedIds.length > 0 ? (
        <div className="mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-primary/30 bg-primary/5 px-3 py-2 text-xs">
          <span className="font-semibold text-primary">{t("claude.selectedCount", { count: selectedIds.length })}</span>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ enabled: true })}>
            {t("claude.batchEnable")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ enabled: false })}>
            {t("claude.batchDisable")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ locked: true })}>
            {t("claude.batchLock")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void runBatch({ locked: false })}>
            {t("claude.batchUnlock")}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setSelected(new Set())}>
            {t("claude.clearSelection")}
          </Button>
        </div>
      ) : null}

      {/* 账号列表 */}
      {loadError && accounts.length > 0 ? (
        <div className="mb-3 flex items-center justify-between gap-3 rounded-lg border border-rose-500/30 bg-rose-500/5 px-3 py-2 text-xs text-rose-700 dark:text-rose-300">
          <span className="break-words">{loadError}</span>
          <Button variant="outline" size="sm" onClick={() => void reload()}>{t("common.retry")}</Button>
        </div>
      ) : null}
      {loading && accounts.length === 0 ? (
        <div className="py-16 text-center text-sm text-muted-foreground">{t("common.loading")}</div>
      ) : loadError && accounts.length === 0 ? (
        <div className="rounded-xl border border-rose-500/30 bg-rose-500/5 py-12 text-center text-sm text-rose-700 dark:text-rose-300">
          <div>{loadError}</div>
          <Button className="mt-3" variant="outline" size="sm" onClick={() => void reload()}>{t("common.retry")}</Button>
        </div>
      ) : total === 0 && !filtersActive ? (
        /* 空号池占位卡(与 Antigravity 页同款 StateShell):提示添加账号并直达授权弹窗 */
        <StateShell
          variant="page"
          isEmpty
          emptyIcon={<ChannelLogo channel="claude" size={30} />}
          emptyTitle={t("claude.emptyTitle")}
          emptyDescription={t("claude.emptyDescription")}
          action={
            <Button
              onClick={() => {
                setAddInitialTab("oauth");
                setShowAdd(true);
              }}
            >
              <Plus className="size-4" />
              {t("claude.addAccount")}
            </Button>
          }
        >
          {null}
        </StateShell>
      ) : accounts.length === 0 ? (
        <StateShell
          variant="page"
          isEmpty
          emptyIcon={<ChannelLogo channel="claude" size={30} />}
          emptyTitle={t("claude.noMatchesTitle")}
          emptyDescription={t("claude.noMatchesDescription")}
          action={
            <Button variant="outline" onClick={clearFilters}>
              {t("claude.clearFilters")}
            </Button>
          }
        >
          {null}
        </StateShell>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-border bg-card shadow-sm">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-border text-left text-[11px] font-semibold uppercase tracking-wide text-muted-foreground [&>th]:whitespace-nowrap">
                <th className="w-10 px-3 py-2.5">
                  <input
                    type="checkbox"
                    className="size-3.5 cursor-pointer accent-primary"
                    checked={allSelected}
                    onChange={toggleSelectAll}
                    aria-label={t("accounts.selectAll")}
                  />
                </th>
                <th className="px-2 py-2.5 text-center">{t("accounts.sequence")}</th>
                <th className="px-2 py-2.5">{t("accounts.email")}</th>
                {visibleCols.groups ? <th className="px-2 py-2.5 text-center">{t("accounts.groupsLabel")}</th> : null}
                {visibleCols.proxy ? <th className="px-2 py-2.5 text-center">{t("accounts.proxyColumn")}</th> : null}
                {visibleCols.priority ? <th className="px-2 py-2.5 text-center">{t("accounts.schedulerPriorityColumn")}</th> : null}
                {visibleCols.plan ? <th className="px-2 py-2.5 text-center">{t("accounts.plan")}</th> : null}
                {visibleCols.status ? <th className="px-2 py-2.5 text-center">{t("accounts.status")}</th> : null}
                {visibleCols.today ? <th className="px-2 py-2.5 text-center">{t("claude.todayLabel")}</th> : null}
                {visibleCols.requests ? <th className="px-2 py-2.5 text-center">{t("accounts.requests")}</th> : null}
                {visibleCols.usage ? <th className="px-2 py-2.5 text-center">{t("accounts.usage")}</th> : null}
                {visibleCols.cost ? <th className="px-2 py-2.5 text-center">{t("claude.costLabel")}</th> : null}
                {visibleCols.importTime ? <th className="px-2 py-2.5 text-center">{t("accounts.importTime")}</th> : null}
                {visibleCols.updatedAt ? <th className="px-2 py-2.5 text-center">{t("accounts.updatedAt")}</th> : null}
                <th className="px-2 py-2.5 text-right">{t("accounts.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {displayRows.map((acc, idx) => (
                <ClaudeAccountRow
                  key={acc.id}
                  acc={acc}
                  no={(page - 1) * pageSize + idx + 1}
                  selected={selected.has(acc.id)}
                  onToggleSelect={() => toggleSelect(acc.id)}
                  groupMap={groupMap}
                  healthBuckets={healthBars[String(acc.id)]}
                  hideDomainTags={hideDomainTags}
                  columns={visibleCols}
                  proxyCtx={proxyBindingCtx}
                  onEditProxy={() => setQuickProxyAccount(acc)}
                  onRefresh={() => void handleRefresh(acc)}
                  onRefreshModels={() => void handleRefreshModels(acc)}
                  onToggleEnabled={() => void handleToggleEnabled(acc)}
                  onToggleLock={() => void handleToggleLock(acc)}
                  onResetStatus={() => void handleResetStatus(acc)}
                  onAssignGroups={() => setAssignTarget(acc)}
                  onUsage={() => setUsageTarget(acc)}
                  onUsageRefreshed={() => handleRefreshUsage(acc)}
                  onOpenDetail={() => void openDetail(acc)}
                  onTest={() => setTestingTarget(acc)}
                  onEdit={() => setEditTarget(acc)}
                  onEditModels={() => void openModelsEditor(acc)}
                  onDelete={() => void handleDelete(acc)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {total > 0 ? (
        <div className="mt-4">
          <Pagination
            page={page}
            totalPages={totalPages}
            onPageChange={setPage}
            totalItems={total}
            pageSize={pageSize}
            onPageSizeChange={(next) => {
              setPageSize(next);
              setPage(1);
            }}
            pageSizeOptions={[10, 20, 50, 100]}
          />
        </div>
      ) : null}

      {showAdd ? (
        <ClaudeAddModal
          proxies={proxyPool}
          groups={claudeGroups}
          initialTab={addInitialTab}
          onClose={() => setShowAdd(false)}
          onAdded={() => {
            setShowAdd(false);
            void reload();
          }}
        />
      ) : null}

      {showManageGroups ? (
        <AccountGroupManagerModal
          channel="claude"
          groups={claudeGroups}
          title={t("claude.manageGroups")}
          onClose={() => setShowManageGroups(false)}
          onChanged={() => {
            void reloadGroups();
            void reload();
          }}
        />
      ) : null}

      {assignTarget ? (
        <AssignGroupsModal
          account={assignTarget}
          groups={claudeGroups}
          onGroupsChanged={reloadGroups}
          onClose={() => setAssignTarget(null)}
          onSaved={() => {
            setAssignTarget(null);
            // 先刷新分组列表(内联新建的组要进 groupMap,否则芯片渲染不出),再刷新账号行。
            void reloadGroups();
            void reload();
          }}
        />
      ) : null}

      {usageTarget ? (
        <AccountUsageModal
          account={usageTarget}
          onClose={() => setUsageTarget(null)}
          showCreditSettings={false}
          officialUsage={false}
        />
      ) : null}

      {editTarget ? (
        <EditAccountModal
          account={editTarget}
          proxies={proxyPool}
          tagOptions={tags}
          onClose={() => setEditTarget(null)}
          onSaved={() => {
            setEditTarget(null);
            void reload();
          }}
        />
      ) : null}

      {modelsTarget ? (
        <ClaudeModelsModal
          account={modelsTarget}
          onClose={() => setModelsTarget(null)}
          onSaved={() => {
            setModelsTarget(null);
            void reload({ silent: true });
            if (detailTarget?.id === modelsTarget.id) void refreshOpenDetail(modelsTarget.id);
          }}
        />
      ) : null}

      {detailTarget ? (
        <AccountDetailSheet
          account={detailTarget}
          groups={(detailTarget.group_ids ?? []).map((id) => groupMap.get(id)).filter(Boolean) as AccountGroup[]}
          healthBuckets={healthBars[String(detailTarget.id)]}
          usageSlot={
            <div className="space-y-1 rounded-xl border border-border bg-card p-3">
              <UsageWindow label={t("claude.usage5h")} pct={claudeUsagePct(detailTarget.usage_percent_5h)} reset={detailTarget.reset_5h_at} resetLabel={t("claude.resetIn")} detail={detailTarget.usage_5h_detail} />
              <UsageWindow label={t("claude.usage7d")} pct={claudeUsagePct(detailTarget.usage_percent_7d)} reset={detailTarget.reset_7d_at} resetLabel={t("claude.resetIn")} detail={detailTarget.usage_7d_detail} />
              <ClaudeScopedUsageWindows windows={detailTarget.claude_usage_windows} />
            </div>
          }
          providerSlot={
            <section className="space-y-2.5">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{t("claude.providerTitle")}</h3>
                <Button
                  type="button"
                  variant="ghost"
                  size="xs"
                  className="h-7 text-[11px]"
                  onClick={() => {
                    const target = detailTarget;
                    closeDetail();
                    void openModelsEditor(target);
                  }}
                >
                  <SlidersHorizontal className="size-3" />
                  {t("claude.modelsWhitelistAction")}
                </Button>
              </div>
              <div className="space-y-2 rounded-xl border border-orange-200/70 bg-orange-50/50 p-3 text-xs dark:border-orange-900/60 dark:bg-orange-950/20">
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.authOAuth")}</span><span className="font-medium">{t("claude.providerProtocol")}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.subscriptionPlan")}</span><span>{(() => { const badge = claudePlanBadge(detailTarget.plan_type || "claude"); return <span className={badge.cls}>{badge.label}</span>; })()}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.subscriptionExpires")}</span><span className="text-right">{formatShortDateTime(detailTarget.subscription_expires_at)?.label ?? t("claude.metadataUnknown")}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.fingerprintModeLabel")}</span><span className="text-right">{detailTarget.claude_fingerprint_mode === "force" ? t("claude.fpForce") : detailTarget.claude_fingerprint_mode === "preserve" ? t("claude.fpPreserve") : t("claude.fpFollowGlobal")}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.clientPlatformLabel")}</span><span className="text-right">{detailTarget.claude_client_platform === "claude_code_cli_only" ? t("claude.clientPlatformCLIOnly") : t("claude.clientPlatformUnrestricted")}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.versionPolicyLabel")}</span><span className="text-right">{detailTarget.claude_version_policy === "fixed" ? t("claude.versionPolicyFixed") : detailTarget.claude_version_policy === "minimum" ? t("claude.versionPolicyMinimum") : t("claude.versionPolicyPassthrough")}{detailTarget.claude_client_version ? ` · ${detailTarget.claude_client_version}` : ""}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.timezoneLabel")}</span><span className="max-w-[250px] text-right">{detailTarget.timezone ? claudeTimezoneLabel(detailTarget.timezone) : t("claude.metadataUnknown")}</span></div>
                <div className="flex items-start justify-between gap-3"><span className="shrink-0 text-muted-foreground">{t("claude.upstreamUserAgent")}</span><span className="max-w-[260px] break-all text-right font-mono text-[10px]">{detailTarget.claude_user_agent || t("claude.uaNotConfigured")}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.modelsLabel")}</span><span className="max-w-[230px] text-right">{detailTarget.models?.length ? t("claude.modelsWhitelistCount", { count: normalizeClaudeModelList(detailTarget.models).length }) : t("claude.modelsWhitelistAll")}</span></div>
                <div className="flex justify-between gap-3"><span className="text-muted-foreground">{t("claude.lastSample")}</span><span title={detailTarget.claude_usage_probe_at ? formatShortDateTime(detailTarget.claude_usage_probe_at)?.title : undefined}>{detailTarget.claude_usage_probe_at ? formatRelativeShort(detailTarget.claude_usage_probe_at, t) : t("claude.samplingState.notSampled")}</span></div>
                {detailTarget.claude_usage_probe_error ? <div className="break-words text-rose-600 dark:text-rose-300">{detailTarget.claude_usage_probe_error}</div> : null}
              </div>
            </section>
          }
          onClose={closeDetail}
          onEdit={() => { setEditTarget(detailTarget); closeDetail(); }}
          onUsage={() => { setUsageTarget(detailTarget); closeDetail(); }}
          onTest={() => { closeDetail(); setTestingTarget(detailTarget); }}
          onRefresh={() => void handleRefresh(detailTarget)}
          authJsonExporting={authJsonExportingIds.has(detailTarget.id)}
          onGenerateAuthJson={() => void handleExportOne(detailTarget)}
          onToggleEnabled={() => void handleToggleEnabled(detailTarget)}
          onToggleLock={() => void handleToggleLock(detailTarget)}
          onResetStatus={() => void handleResetStatus(detailTarget)}
          onSaveModelCooldownPolicy={(data) => void handleSaveDetailCooldownPolicy(detailTarget, data)}
          onClearModelCooldown={(model) => void handleClearDetailCooldown(detailTarget, model)}
          onClearAllModelCooldowns={() => void handleClearAllDetailCooldowns(detailTarget)}
          onResetCredits={() => undefined}
          onDelete={() => { closeDetail(); void handleDelete(detailTarget); }}
        />
      ) : null}

      {testingTarget ? (
        <ClaudeTestModal
          account={testingTarget}
          onClose={() => setTestingTarget(null)}
          onSettled={handleClaudeTestSettled}
        />
      ) : null}

      <AccountProxyQuickEditor
        account={quickProxyAccount}
        accountLabel={
          quickProxyAccount
            ? quickProxyAccount.email || quickProxyAccount.name || `#${quickProxyAccount.id}`
            : ""
        }
        proxies={proxyPool}
        ctx={proxyBindingCtx}
        onClose={() => setQuickProxyAccount(null)}
        onSaved={() => reload({ silent: true })}
      />

      {confirmDialog}
    </div>
  );
}

// ── 号池模式表格行(视觉对齐 Codex Pool Mode 表格;数据取 Claude 真实链路) ──
function ClaudeAccountRow({
  acc,
  no,
  selected,
  onToggleSelect,
  groupMap,
  healthBuckets,
  hideDomainTags,
  columns,
  proxyCtx,
  onEditProxy,
  onRefresh,
  onRefreshModels,
  onToggleEnabled,
  onToggleLock,
  onResetStatus,
  onAssignGroups,
  onUsage,
  onUsageRefreshed,
  onOpenDetail,
  onTest,
  onEdit,
  onEditModels,
  onDelete,
}: {
  acc: AccountRow;
  no: number;
  selected: boolean;
  onToggleSelect: () => void;
  groupMap: Map<number, AccountGroup>;
  healthBuckets?: AccountHealthBucket[];
  hideDomainTags: boolean;
  columns: ClaudeColVisibility;
  proxyCtx: ProxyBindingContext;
  onEditProxy: () => void;
  onRefresh: () => void;
  onRefreshModels: () => void;
  onToggleEnabled: () => void;
  onToggleLock: () => void;
  onResetStatus: () => void;
  onAssignGroups: () => void;
  onUsage: () => void;
  onUsageRefreshed: () => void | Promise<void>;
  onOpenDetail: () => void;
  onTest: () => void;
  onEdit: () => void;
  onEditModels: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const pct5h = claudeUsagePct(acc.usage_percent_5h);
  const pct7d = claudeUsagePct(acc.usage_percent_7d);
  const disabled = acc.enabled === false;
  const cooldownReason = (acc.status || "").toLowerCase().includes("rate") ? acc.error_message : "";
  const accGroups = (acc.group_ids || []).map((id) => groupMap.get(id)).filter(Boolean) as AccountGroup[];
  const today = acc.usage_today_detail;
  const billed5h = typeof acc.usage_5h_detail?.account_billed === "number" ? acc.usage_5h_detail.account_billed : 0;
  const billed7d = typeof acc.usage_7d_detail?.account_billed === "number" ? acc.usage_7d_detail.account_billed : 0;
  const todayBilled = typeof today?.account_billed === "number" ? today.account_billed : 0;
  const created = formatShortDateTime(acc.created_at);
  const tableOverlayKind = resolveAccountOverlayKind(acc);

  const iconBtn =
    "inline-flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground";

  return (
    <tr
      className={cn(
        "border-b border-border/60 align-middle transition-colors last:border-b-0 hover:bg-muted/30",
        accountStateTableRowClass(acc),
        selected && "bg-primary/5",
      )}
    >
      {/* 勾选 */}
      <td className="px-3 py-3">
        <div className="flex items-center gap-1">
          <input
            type="checkbox"
            className="size-3.5 cursor-pointer accent-primary"
            checked={selected}
            onChange={onToggleSelect}
            aria-label={acc.email || acc.name}
          />
          {!columns.status && tableOverlayKind ? (
            <span className="sr-only">
              {tableOverlayKind === "disabled" ? t("accounts.disabledOverlay") : t("accounts.overloadOverlay")}
            </span>
          ) : null}
          {!columns.status && tableOverlayKind === "overload" ? (
            <button
              type="button"
              className="inline-flex size-7 items-center justify-center rounded-md text-orange-700 transition-colors hover:bg-orange-500/10 dark:text-orange-300"
              title={t("accounts.overloadRecover")}
              aria-label={t("accounts.overloadRecover")}
              onClick={(event) => {
                event.preventDefault();
                event.stopPropagation();
                onResetStatus();
              }}
            >
              <RotateCcw className="size-3.5" />
            </button>
          ) : null}
        </div>
      </td>
      {/* 序号 */}
      <td className="px-2 py-3 text-center font-mono text-xs text-muted-foreground">{no}</td>
      {/* 邮箱 */}
      <td className="w-full min-w-[220px] px-2 py-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-orange-50 ring-1 ring-inset ring-orange-200 dark:bg-orange-950/70 dark:ring-orange-800">
            <ChannelLogo channel="claude" size={20} />
          </span>
          <div className="min-w-0">
            <button
              type="button"
              className="break-all text-left text-[13px] font-medium leading-snug text-foreground hover:text-primary"
              title={t("accounts.openDetail")}
              onClick={onOpenDetail}
            >
              {acc.email || acc.name || `#${acc.id}`}
            </button>
            <div className="mt-0.5 flex flex-wrap items-center gap-1">
              <span className="rounded bg-muted/70 px-1 py-0.5 font-mono text-[10px] text-muted-foreground">ID {acc.id}</span>
              {acc.models?.length ? <span className="rounded bg-orange-500/10 px-1 py-0.5 text-[10px] text-orange-700 dark:text-orange-300">{t("claude.modelCount", { count: acc.models.length })}</span> : null}
              {acc.last_used_at ? <span className="text-[10px] text-muted-foreground/70">{t("claude.lastUsed")}: {formatRelativeShort(acc.last_used_at, t)}</span> : null}
              {!hideDomainTags && acc.email_domain ? (
                <span className="rounded bg-muted/70 px-1 py-0.5 text-[10px] text-muted-foreground">@{acc.email_domain}</span>
              ) : null}
              {acc.locked ? (
                <span className="inline-flex items-center rounded bg-blue-50 px-1 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                  <Lock className="mr-0.5 size-2.5" />
                  {t("claude.statLocked")}
                </span>
              ) : null}
            </div>
          </div>
        </div>
      </td>
      {columns.groups ? (
      <td className="min-w-[110px] px-2 py-3">
        <div className="flex flex-wrap items-center justify-center gap-1">
          {accGroups.map((g) => {
            const color = normalizeGroupColor(g.color);
            return (
              <button
                key={g.id}
                type="button"
                onClick={onAssignGroups}
                className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold transition-opacity hover:opacity-85"
                style={{ backgroundColor: `${color}14`, color, boxShadow: `inset 0 0 0 1px ${color}33` }}
                title={g.description || g.name}
              >
                <span className="size-1.5 rounded-full bg-current" />
                {g.name}
              </button>
            );
          })}
          <button
            type="button"
            onClick={onAssignGroups}
            className="inline-flex items-center gap-1 rounded-md border border-dashed border-border px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground transition-colors hover:border-primary/50 hover:text-primary"
          >
            <Plus className="size-2.5" />
            {t("claude.assignGroups")}
          </button>
        </div>
      </td>
      ) : null}
      {columns.proxy ? (
      <td className="min-w-[120px] max-w-[180px] px-2 py-3">
        <div className="flex items-center justify-center">
          <AccountProxyBadge account={acc} ctx={proxyCtx} onClick={onEditProxy} />
        </div>
      </td>
      ) : null}
      {columns.priority ? (
      <td className="px-2 py-3 text-center">
        <span className="rounded-md bg-muted px-1.5 py-0.5 font-mono text-[11px] font-semibold text-muted-foreground" title={t("claude.priorityLabel")}>
          P {acc.scheduler_priority ?? 0}
        </span>
      </td>
      ) : null}
      {columns.plan ? (
      <td className="whitespace-nowrap px-2 py-3 text-center">
        {acc.plan_type ? (
          (() => {
            const b = claudePlanBadge(acc.plan_type);
            return <span className={b.cls}>{b.label}</span>;
          })()
        ) : (
          <span className="text-xs text-muted-foreground/50">-</span>
        )}
      </td>
      ) : null}
      {columns.status ? (
      <td className="min-w-[170px] px-2 py-3">
        <div className="flex flex-col items-center space-y-1.5">
          {renderAccountStateOverlay(acc, t, {
            compact: true,
            markerOnly: true,
            onRecover: acc.status === "overload_paused" ? onResetStatus : undefined,
          }) ?? (
            <>
              <div className="flex flex-wrap items-center justify-center gap-1">
				<StatusBadge status={getAccountStatusBadgeStatus(acc)} errorMessage={acc.error_message} detail={cooldownReason} />
                <LiveCountdown until={acc.cooldown_until} label={t("claude.resetIn")} />
                <ClaudeConcurrencyBadge acc={acc} />
                {acc.claude_api ? (
              <span
                className={cn(
                  "inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
                  acc.claude_usage_probe_error
                    ? "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300"
                    : acc.claude_usage_probe_at
                      ? "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-300"
                      : "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300",
                )}
                title={acc.claude_usage_probe_error || t("claude.samplingState.notSampled")}
              >
                {acc.claude_usage_probe_error
                  ? t("claude.samplingState.error")
                  : acc.claude_usage_probe_at
                    ? t("claude.samplingState.sampled")
                    : t("claude.samplingState.unsampled")}
              </span>
                ) : null}
              </div>
              {acc.claude_api ? (
                <div className="text-[10px] text-muted-foreground" title={acc.claude_usage_probe_error || undefined}>
                  {t("claude.lastSample")}: {acc.claude_usage_probe_at ? formatRelativeShort(acc.claude_usage_probe_at, t) : t("claude.samplingState.notSampled")}
                  {acc.claude_usage_probe_error ? ` · ${acc.claude_usage_probe_error}` : ""}
                </div>
              ) : null}
              <AccountHealthBar buckets={healthBuckets} />
            </>
          )}
        </div>
      </td>
      ) : null}
      {columns.today ? (
      <td className="px-2 py-3 text-center">
        {today ? (
          <div className="inline-flex flex-col items-center space-y-1 whitespace-nowrap text-[12px] tabular-nums">
            <div className="flex items-center gap-1.5">
              <span className={cn("inline-flex items-center gap-0.5", (today.requests ?? 0) > 0 ? "font-semibold text-foreground" : "text-muted-foreground/50")}>
                <Activity className={cn("size-3", (today.requests ?? 0) > 0 ? "text-sky-500" : "text-muted-foreground/40")} aria-hidden />
                {(today.requests ?? 0).toLocaleString()}
              </span>
              <span className={cn("inline-flex items-center gap-0.5", (today.tokens ?? 0) > 0 ? "font-semibold text-foreground" : "text-muted-foreground/50")}>
                <Sparkles className={cn("size-3", (today.tokens ?? 0) > 0 ? "text-purple-500 dark:text-purple-400" : "text-muted-foreground/40")} aria-hidden />
                {formatCompactNum(today.tokens)}
              </span>
            </div>
            <span
              className={cn(
                "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[10px] ring-1 ring-inset",
                todayBilled > 0
                  ? "bg-emerald-500/10 font-medium text-emerald-700 ring-emerald-500/20 dark:text-emerald-400"
                  : "bg-slate-500/10 text-slate-500 ring-slate-500/20 dark:text-slate-400",
              )}
            >
              <Coins className={cn("size-2.5", todayBilled > 0 ? "text-emerald-500" : "opacity-50")} aria-hidden />
              ${todayBilled > 0 ? (todayBilled < 0.01 ? "<0.01" : todayBilled.toFixed(2)) : "0.00"}
            </span>
          </div>
        ) : (
          <span className="font-mono text-xs text-muted-foreground/40">-</span>
        )}
      </td>
      ) : null}
      {columns.requests ? (
      <td className="px-2 py-3 text-center">
        <div className="flex justify-center">
          <RequestCountPills account={acc} compact />
        </div>
      </td>
      ) : null}
      {columns.usage ? (
      <td className="px-2 py-3">
        <div className="flex items-center justify-center gap-1.5">
          <div className="min-w-0 space-y-1">
            {pct5h !== null || pct7d !== null || acc.usage_5h_detail || acc.usage_7d_detail || (acc.claude_usage_windows ?? []).some((window) => window.model_scoped) ? (
              <>
                <UsageWindow label={t("claude.usage5h")} pct={pct5h} reset={acc.reset_5h_at} resetLabel={t("claude.resetIn")} detail={acc.usage_5h_detail} />
                <UsageWindow label={t("claude.usage7d")} pct={pct7d} reset={acc.reset_7d_at} resetLabel={t("claude.resetIn")} detail={acc.usage_7d_detail} />
                <ClaudeScopedUsageWindows windows={acc.claude_usage_windows} />
              </>
            ) : (
              <span className="text-xs text-muted-foreground/50">-</span>
            )}
          </div>
          <UsageRefreshButton onRefresh={onUsageRefreshed} title={t("accounts.refreshUsage")} />
        </div>
      </td>
      ) : null}
      {columns.cost ? (
      <td className="px-2 py-3 text-center">
        <span className="inline-flex items-center whitespace-nowrap rounded-md bg-muted/60 px-1.5 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
          5h: ${billed5h.toFixed(2)} / 7d: ${billed7d.toFixed(2)}
        </span>
      </td>
      ) : null}
      {columns.importTime ? (
      <td className="whitespace-nowrap px-2 py-3 text-center text-xs tabular-nums text-muted-foreground" title={created?.title}>
        {created?.label ?? "-"}
      </td>
      ) : null}
      {columns.updatedAt ? (
      <td className="whitespace-nowrap px-2 py-3 text-center text-xs text-muted-foreground">{formatRelativeShort(acc.updated_at, t)}</td>
      ) : null}
      {/* 操作 */}
      <td className="px-2 py-3">
        <div className="flex items-center justify-end gap-0.5">
          <button type="button" className={iconBtn} onClick={onEdit} title={t("claude.editTitle")} aria-label={t("claude.editTitle")}>
            <Pencil className="size-3.5" />
          </button>
          <button type="button" className={iconBtn} onClick={onUsage} title={t("accounts.actionUsageDetail")} aria-label={t("accounts.actionUsageDetail")}>
            <BarChart3 className="size-3.5" />
          </button>
          <button type="button" className={iconBtn} onClick={onTest} title={t("accounts.testConnection")} aria-label={t("accounts.testConnection")}>
            <FlaskConical className="size-3.5" />
          </button>
          <button type="button" className={iconBtn} onClick={onEditModels} title={t("claude.modelsWhitelistAction")} aria-label={t("claude.modelsWhitelistAction")}>
            <SlidersHorizontal className="size-3.5" />
          </button>
          <button type="button" className={iconBtn} onClick={onRefreshModels} title={t("claude.refreshModels")} aria-label={t("claude.refreshModels")}>
            <RefreshCw className="size-3.5" />
          </button>
          <button
            type="button"
            className="inline-flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-rose-500/10 hover:text-rose-600 dark:hover:text-rose-400"
            onClick={onDelete}
            title={t("common.delete")}
            aria-label={t("common.delete")}
          >
            <Trash2 className="size-3.5" />
          </button>
          <RowOverflowMenu
            items={[
              { key: "refresh", label: t("common.refresh"), onClick: onRefresh },
              { key: "reset", label: t("claude.resetStatus"), onClick: onResetStatus },
              { key: "lock", label: acc.locked ? t("claude.unlock") : t("claude.lock"), onClick: onToggleLock },
              { key: "toggle", label: disabled ? t("claude.enable") : t("claude.disable"), onClick: onToggleEnabled },
            ]}
          />
        </div>
      </td>
    </tr>
  );
}

// ColumnsMenu 列显隐下拉(与 Codex 的列控制一致):勾选切换,状态持久化到 localStorage。
function ColumnsMenu({
  visible,
  onChange,
}: {
  visible: ClaudeColVisibility;
  onChange: (next: ClaudeColVisibility) => void;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onEsc);
    };
  }, [open]);

  const labelFor: Record<ClaudeCol, string> = {
    groups: t("accounts.groupsLabel"),
    proxy: t("accounts.proxyColumn"),
    priority: t("accounts.schedulerPriorityColumn"),
    plan: t("accounts.plan"),
    status: t("accounts.status"),
    today: t("claude.todayLabel"),
    requests: t("accounts.requests"),
    usage: t("accounts.usage"),
    cost: t("claude.costLabel"),
    importTime: t("accounts.importTime"),
    updatedAt: t("accounts.updatedAt"),
  };
  const hiddenCount = CLAUDE_TOGGLE_COLUMNS.filter((c) => !visible[c]).length;

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-muted-foreground transition-colors hover:text-foreground"
        aria-expanded={open}
      >
        <Columns3 className="size-3.5" />
        {t("claude.columns")}
        {hiddenCount > 0 ? <span className="tabular-nums opacity-70">({hiddenCount})</span> : null}
      </button>
      {open ? (
        <div className="absolute right-0 top-full z-40 mt-1 w-44 overflow-hidden rounded-lg border border-border bg-popover py-1 shadow-lg">
          {CLAUDE_TOGGLE_COLUMNS.map((c) => (
            <label key={c} className="flex cursor-pointer items-center gap-2 px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-muted">
              <input
                type="checkbox"
                checked={visible[c]}
                onChange={() => onChange({ ...visible, [c]: !visible[c] })}
              />
              {labelFor[c]}
            </label>
          ))}
        </div>
      ) : null}
    </div>
  );
}

// UsageRefreshButton 用量刷新按钮:点击时旋转动画,请求完成后停止(与全站刷新按钮一致)。
function UsageRefreshButton({ onRefresh, title }: { onRefresh: () => void | Promise<void>; title: string }) {
  const [spinning, setSpinning] = useState(false);
  return (
    <button
      type="button"
      title={title}
      aria-label={title}
      disabled={spinning}
      onClick={async () => {
        setSpinning(true);
        try {
          await onRefresh();
        } finally {
          setSpinning(false);
        }
      }}
      className="mt-0.5 shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-60"
    >
      <RefreshCw className={cn("size-3", spinning && "animate-spin")} />
    </button>
  );
}

// RowOverflowMenu "…" 溢出菜单:表格在 overflow 容器内,菜单用 fixed 定位避免被裁剪。
function RowOverflowMenu({
  items,
}: {
  items: Array<{ key: string; label: string; onClick: () => void; danger?: boolean }>;
}) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ top: number; right: number } | null>(null);
  const btnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const close = () => setOpen(false);
    const onDown = (e: MouseEvent) => {
      if (!menuRef.current?.contains(e.target as Node) && !btnRef.current?.contains(e.target as Node)) close();
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") close();
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
    };
  }, [open]);

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        className="inline-flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
        onClick={() => {
          const rect = btnRef.current?.getBoundingClientRect();
          if (rect) setPos({ top: rect.bottom + 4, right: Math.max(8, window.innerWidth - rect.right) });
          setOpen((v) => !v);
        }}
        aria-expanded={open}
        aria-label="more"
      >
        <MoreHorizontal className="size-3.5" />
      </button>
      {open && pos ? (
        <div
          ref={menuRef}
          className="fixed z-50 w-32 overflow-hidden rounded-lg border border-border bg-popover py-1 shadow-lg"
          style={{ top: pos.top, right: pos.right }}
        >
          {items.map((item) => (
            <button
              key={item.key}
              type="button"
              onClick={() => {
                setOpen(false);
                item.onClick();
              }}
              className={cn(
                "block w-full px-3 py-1.5 text-left text-xs transition-colors hover:bg-muted",
                item.danger ? "text-rose-600 dark:text-rose-400" : "text-foreground",
              )}
            >
              {item.label}
            </button>
          ))}
        </div>
      ) : null}
    </>
  );
}

// ── 账号分组指派弹窗 ──────────────────────────────────────
function AssignGroupsModal({
  account,
  groups,
  onClose,
  onSaved,
  onGroupsChanged,
}: {
  account: AccountRow;
  groups: AccountGroup[];
  onClose: () => void;
  onSaved: () => void;
  onGroupsChanged?: () => void | Promise<void>;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [selected, setSelected] = useState<number[]>(account.group_ids ?? []);
  const [busy, setBusy] = useState(false);

  // 内联建组:与其他页一致,复用 createAccountGroup(channel=claude),返回新 id 供自动勾选。
  const createGroupInline = useCallback(
    async (name: string): Promise<number | null> => {
      try {
        // 颜色按调色板循环取(与 Codex 内联建组一致),避免新组都是同一颜色。
        const color = ACCOUNT_GROUP_COLORS[groups.length % ACCOUNT_GROUP_COLORS.length];
        const res = await api.createAccountGroup({ name: name.trim(), channel: "claude", color });
        // 新组即时同步到父级 claudeGroups,保证保存后行内芯片能从 groupMap 取到它。
        await onGroupsChanged?.();
        return res.id ?? null;
      } catch (error) {
        showToast(getErrorMessage(error), "error");
        return null;
      }
    },
    [groups.length, onGroupsChanged, showToast],
  );

  const save = useCallback(async () => {
    setBusy(true);
    try {
      await api.batchUpdateAccounts({ ids: [account.id], group_ids: selected });
      showToast(t("claude.groupsUpdated"), "success");
      onSaved();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  }, [account.id, selected, onSaved, showToast, t]);

  return (
    <Modal
      show
      onClose={onClose}
      title={t("claude.assignGroupsTitle")}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void save()} disabled={busy}>
            {t("claude.save")}
          </Button>
        </div>
      }
    >
      <div className="space-y-2">
        <p className="text-xs text-muted-foreground">{account.email || account.name || `#${account.id}`}</p>
        <AccountGroupMultiSelect
          groups={groups}
          value={selected}
          onChange={setSelected}
          allLabel={t("accounts.groupsUnbound")}
          selectedLabel={t("accounts.groupsSelected", { count: selected.length })}
          placeholder={t("accounts.importGroupsPlaceholder")}
          emptyLabel={t("accounts.groupsNone")}
          emptyHint={t("accounts.groupsSelectHint")}
          onCreateGroup={createGroupInline}
          createLabel={t("accounts.groupCreate")}
          createPlaceholder={t("accounts.groupNamePlaceholder")}
          creatingLabel={t("accounts.groupCreating")}
          createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
        />
      </div>
    </Modal>
  );
}

// ── 账号编辑弹窗:仅 Claude 账号真实可调的字段 ─────────────
// 代理(影响出站 IP 一致性)、标签、调度优先级、5h/7d 自动暂停阈值
// (阈值对照 Anthropic 统一限流头回填的真实窗口利用率)。
function EditAccountModal({
  account,
  proxies,
  tagOptions,
  onClose,
  onSaved,
}: {
  account: AccountRow;
  proxies: ProxyRow[];
  tagOptions: string[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const [proxyUrl, setProxyUrl] = useState(account.proxy_url ?? "");
  const [tags, setTags] = useState<string[]>(account.tags ?? []);
  const [priority, setPriority] = useState(
    account.scheduler_priority != null ? String(account.scheduler_priority) : "",
  );
  const [scoreBias, setScoreBias] = useState(
    account.score_bias_override != null ? String(account.score_bias_override) : "",
  );
  const [concurrency, setConcurrency] = useState(
    account.base_concurrency_override != null ? String(account.base_concurrency_override) : "",
  );
  const [pause5h, setPause5h] = useState(
    account.auto_pause_5h_threshold != null ? String(account.auto_pause_5h_threshold) : "",
  );
  const [pause7d, setPause7d] = useState(
    account.auto_pause_7d_threshold != null ? String(account.auto_pause_7d_threshold) : "",
  );
  const [fpMode, setFpMode] = useState<"" | "preserve" | "force">(
    (account.claude_fingerprint_mode as "" | "preserve" | "force") ?? "",
  );
  const [clientPlatform, setClientPlatform] = useState<"" | "any" | "claude_code_cli_only">(
    (account.claude_client_platform_override as "" | "any" | "claude_code_cli_only") ?? "",
  );
  const [versionPolicy, setVersionPolicy] = useState<"" | "passthrough" | "fixed" | "minimum">(
    (account.claude_version_policy_override as "" | "passthrough" | "fixed" | "minimum") ?? "",
  );
  const [clientVersion, setClientVersion] = useState(account.claude_client_version_override ?? "");
  const [timezone, setTimezone] = useState(account.timezone ?? "");
  const [timezoneCustom, setTimezoneCustom] = useState(
    Boolean(account.timezone && !findClaudeTimezoneOption(account.timezone)),
  );
  const [busy, setBusy] = useState(false);

  const parseNum = (v: string): number | null => {
    const s = v.trim();
    if (!s) return null;
    const n = Number(s);
    return Number.isFinite(n) ? n : null;
  };

  const save = useCallback(async () => {
    setBusy(true);
    try {
      await api.updateAccountScheduler(account.id, {
        proxy_url: proxyUrl.trim() || null,
        tags,
        scheduler_priority: parseNum(priority),
        score_bias_override: parseNum(scoreBias),
        base_concurrency_override: parseNum(concurrency),
        auto_pause_5h_threshold: parseNum(pause5h),
        auto_pause_7d_threshold: parseNum(pause7d),
        claude_fingerprint_mode: fpMode,
        claude_client_platform: clientPlatform || null,
        claude_version_policy: versionPolicy || null,
        claude_client_version: clientVersion.trim() || null,
        timezone: timezone.trim(),
      });
      showToast(t("claude.saved"), "success");
      // 手动输入的代理若不在代理管理中,询问是否存入(需在关闭弹窗前完成)。
      await maybeOfferSaveProxyToPool(proxyUrl, proxies, confirm, showToast, t);
      onSaved();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  }, [account.id, proxyUrl, proxies, confirm, tags, priority, scoreBias, concurrency, pause5h, pause7d, fpMode, clientPlatform, versionPolicy, clientVersion, timezone, onSaved, showToast, t]);

  const field = (label: string, node: ReactNode, hint?: string) => (
    <div className="space-y-1">
      <span className="text-xs font-semibold text-muted-foreground">{label}</span>
      {node}
      {hint ? <p className="text-[10px] leading-tight text-muted-foreground/70">{hint}</p> : null}
    </div>
  );

  const timezoneChoice = timezoneCustom
    ? CLAUDE_TIMEZONE_CUSTOM
    : findClaudeTimezoneOption(timezone)?.value ?? (timezone.trim() ? CLAUDE_TIMEZONE_CUSTOM : "");
  const timezoneOptions = [
    { value: "", label: t("claude.timezoneUnset") },
    ...CLAUDE_TIMEZONE_OPTIONS,
    { value: CLAUDE_TIMEZONE_CUSTOM, label: t("claude.timezoneCustom") },
  ];

  return (
    <Modal
      show
      onClose={onClose}
      title={t("claude.editTitle")}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void save()} disabled={busy}>
            {t("claude.save")}
          </Button>
        </div>
      }
    >
      <div className="max-h-[70vh] space-y-4 overflow-y-auto pr-1">
        <p className="text-xs text-muted-foreground">{account.email || account.name || `#${account.id}`}</p>

        {/* 身份/网络 */}
        <div className="space-y-3 rounded-lg border border-border/60 p-3">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("claude.editSectionIdentity")}
          </div>
          {field(
            t("claude.proxyLabel"),
            <ProxyField value={proxyUrl} onChange={setProxyUrl} proxies={proxies} label="" />,
            t("claude.proxyHint"),
          )}
          {field(
            t("claude.fingerprintModeLabel"),
            <Select
              value={fpMode}
              onValueChange={(value) => setFpMode(value as "" | "preserve" | "force")}
              options={[
                { value: "", label: t("claude.fpFollowGlobal") },
                { value: "preserve", label: t("claude.fpPreserve") },
                { value: "force", label: t("claude.fpForce") },
              ]}
            />,
            t("claude.fingerprintModeHint"),
          )}
          {field(
            t("claude.clientPlatformLabel"),
            <Select
              value={clientPlatform}
              onValueChange={(value) => setClientPlatform(value as "" | "any" | "claude_code_cli_only")}
              options={[
                { value: "", label: t("claude.clientPlatformAny") },
                { value: "any", label: t("claude.clientPlatformUnrestricted") },
                { value: "claude_code_cli_only", label: t("claude.clientPlatformCLIOnly") },
              ]}
            />,
            t("claude.clientPlatformHint"),
          )}
          {field(
            t("claude.versionPolicyLabel"),
            <div className="space-y-1.5">
              <Select
                value={versionPolicy}
                onValueChange={(value) => setVersionPolicy(value as "" | "passthrough" | "fixed" | "minimum")}
                options={[
                  { value: "", label: t("claude.versionPolicyPassthrough") },
                  { value: "passthrough", label: t("claude.versionPolicyPassthroughExplicit") },
                  { value: "fixed", label: t("claude.versionPolicyFixed") },
                  { value: "minimum", label: t("claude.versionPolicyMinimum") },
                ]}
              />
              {versionPolicy === "fixed" || versionPolicy === "minimum" ? (
                <Input value={clientVersion} onChange={(e) => setClientVersion(e.target.value)} placeholder="2.1.251" />
              ) : null}
            </div>,
            t("claude.clientVersionHint"),
          )}
          {field(
            t("claude.timezoneLabelEdit"),
            <div className="space-y-1.5">
              <Select
                value={timezoneChoice}
                onValueChange={(value) => {
                  if (value === CLAUDE_TIMEZONE_CUSTOM) {
                    setTimezoneCustom(true);
                    if (findClaudeTimezoneOption(timezone)) setTimezone("");
                    return;
                  }
                  setTimezoneCustom(false);
                  setTimezone(value);
                }}
                options={timezoneOptions}
              />
              {timezoneCustom ? (
                <Input value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder="Asia/Shanghai" />
              ) : null}
              {timezone ? <p className="text-[10px] text-muted-foreground">{claudeTimezoneLabel(timezone)}</p> : null}
            </div>,
            t("claude.timezoneHint"),
          )}
        </div>

        {/* 调度 */}
        <div className="space-y-3 rounded-lg border border-border/60 p-3">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("claude.editSectionScheduling")}
          </div>
          <div className="grid grid-cols-2 gap-3">
            {field(
              t("claude.concurrencyLabel"),
              <Input value={concurrency} onChange={(e) => setConcurrency(e.target.value)} placeholder={t("claude.followGlobalPlaceholder")} inputMode="numeric" />,
              t("claude.concurrencyHint"),
            )}
            {field(
              t("claude.priorityLabel"),
              <Input value={priority} onChange={(e) => setPriority(e.target.value)} placeholder="0" inputMode="numeric" />,
            )}
            {field(
              t("claude.scoreBiasLabel"),
              <Input value={scoreBias} onChange={(e) => setScoreBias(e.target.value)} placeholder="0" inputMode="numeric" />,
              t("claude.scoreBiasHint"),
            )}
          </div>
        </div>

        {/* 自动暂停 */}
        <div className="space-y-3 rounded-lg border border-border/60 p-3">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("claude.editSectionAutoPause")}
          </div>
          <div className="grid grid-cols-2 gap-3">
            {field(t("claude.autoPause5hLabel"), <Input value={pause5h} onChange={(e) => setPause5h(e.target.value)} placeholder="90" inputMode="numeric" />)}
            {field(t("claude.autoPause7dLabel"), <Input value={pause7d} onChange={(e) => setPause7d(e.target.value)} placeholder="90" inputMode="numeric" />)}
          </div>
        </div>

        {/* 标签 */}
        {field(
          t("claude.tagsLabel"),
          <ChipInput
            value={tags}
            onChange={setTags}
            options={tagOptions}
            placeholder={t("claude.tagsPlaceholder")}
            maxVisible={8}
          />,
        )}
      </div>
      {confirmDialog}
    </Modal>
  );
}

// ClaudeModelsModal 仅编辑 Claude 原生模型白名单。前端先做 provider-aware
// 过滤，后端 endpoint 也会做同样的命名空间校验；保存前重新读取详情并以 updated_at
// 作为当前账号凭据代际的乐观锁，避免旧 token/旧目录覆盖新状态。
function ClaudeModelsModal({
  account,
  onClose,
  onSaved,
}: {
  account: AccountRow;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [models, setModels] = useState(() => normalizeClaudeModelList(account.models));
  // 模型级冷却映射(来自 model_cooldowns):区分「需购买 credits」与「限流中」。
  // credits_required 是套餐不含该模型的计费门槛(如 Pro 用 fable-5),非临时限流。
  const cooldownByModel = useMemo(() => {
    const map = new Map<string, { reason: string; credits: boolean }>();
    for (const cd of account.model_cooldowns ?? []) {
      const reason = (cd.reason || "").toLowerCase();
      map.set(cd.model.toLowerCase(), { reason: cd.reason, credits: reason.includes("credit") });
    }
    return map;
  }, [account.model_cooldowns]);
  const [input, setInput] = useState("");
  const [inputError, setInputError] = useState("");
  const [conflict, setConflict] = useState("");
  const [baseUpdatedAt, setBaseUpdatedAt] = useState(account.updated_at);
  const [syncing, setSyncing] = useState(false);
  const [saving, setSaving] = useState(false);

  const addModels = useCallback(() => {
    const parsed = parseClaudeModelTokens(input);
    if (parsed.accepted.length > 0) {
      setModels((current) => mergeClaudeModelLists(current, parsed.accepted));
    }
    setInputError(parsed.rejected.length > 0
      ? t("claude.modelsWhitelistInvalid", { models: parsed.rejected.join(", ") })
      : "");
    if (parsed.accepted.length > 0 || parsed.rejected.length > 0) setInput("");
  }, [input, t]);

  const reloadLatest = useCallback(async () => {
    setSaving(true);
    try {
      const latest = await api.getAccount(account.id);
      if (latest.claude_api !== true) {
        setConflict(t("claude.modelsWhitelistNotClaude"));
        return;
      }
      setModels(normalizeClaudeModelList(latest.models));
      setBaseUpdatedAt(latest.updated_at);
      setConflict("");
      setInputError("");
    } catch (error) {
      setConflict(getErrorMessage(error));
    } finally {
      setSaving(false);
    }
  }, [account.id, t]);

  const syncUpstream = useCallback(async () => {
    setSyncing(true);
    setInputError("");
    try {
      const result = await api.syncAccountModelsUpstream(account.id);
      const upstream = normalizeClaudeModelList(result.models);
      if (upstream.length === 0) {
        setInputError(t("claude.modelsWhitelistSyncEmpty"));
      } else {
        setModels((current) => mergeClaudeModelLists(current, upstream));
        showToast(t("claude.modelsWhitelistSyncDone", { count: upstream.length }), "success");
      }
    } catch (error) {
      setInputError(t("claude.modelsWhitelistSyncFailed", { error: getErrorMessage(error) }));
    } finally {
      setSyncing(false);
    }
  }, [account.id, showToast, t]);

  const save = useCallback(async () => {
    if (saving || syncing) return;
    setSaving(true);
    setConflict("");
    try {
      const latest = await api.getAccount(account.id);
      if (latest.id !== account.id || latest.claude_api !== true) {
        setConflict(t("claude.modelsWhitelistNotClaude"));
        return;
      }
      if (baseUpdatedAt && latest.updated_at && latest.updated_at !== baseUpdatedAt) {
        setModels(normalizeClaudeModelList(latest.models));
        setBaseUpdatedAt(latest.updated_at);
        setConflict(t("claude.modelsWhitelistConflict"));
        return;
      }
      const requested = normalizeClaudeModelList(models);
      const result = await api.updateAccountModels(account.id, requested);
      // Treat an unexpected provider model in a server response as a failed
      // write from the UI perspective; never present it as a Claude whitelist.
      const returned = normalizeClaudeModelList(result.models);
      const rawReturned = Array.isArray(result.models) ? result.models : [];
      if (rawReturned.some((value) => !isClaudeModelID(value))) {
        setConflict(t("claude.modelsWhitelistResponseInvalid"));
        return;
      }
      setModels(returned);
      onSaved();
    } catch (error) {
      showToast(t("claude.modelsWhitelistSaveFailed", { error: getErrorMessage(error) }), "error");
    } finally {
      setSaving(false);
    }
  }, [account.id, baseUpdatedAt, models, onSaved, saving, showToast, syncing, t]);

  return (
    <Modal
      show
      onClose={() => { if (!saving && !syncing) onClose(); }}
      title={t("claude.modelsWhitelistTitle")}
      contentClassName="sm:max-w-[620px]"
      footer={
        <div className="flex w-full justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose} disabled={saving || syncing}>{t("common.cancel")}</Button>
          <Button type="button" onClick={() => void save()} disabled={saving || syncing}>
            {saving ? t("common.saving") : models.length === 0 ? t("claude.modelsWhitelistClearSave") : t("common.save")}
          </Button>
        </div>
      }
    >
      <div className="space-y-4">
        <div className="rounded-lg border border-orange-200/70 bg-orange-50/50 p-3 text-xs dark:border-orange-900/60 dark:bg-orange-950/20">
          <div className="font-semibold text-foreground">{account.email || account.name || `#${account.id}`}</div>
          <p className="mt-1 leading-relaxed text-muted-foreground">{t("claude.modelsWhitelistDescription")}</p>
          <p className="mt-1 font-mono text-[10px] text-muted-foreground/70">{t("claude.modelsWhitelistVersionHint")}</p>
        </div>

        {conflict ? (
          <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-800 dark:text-amber-200">
            <span className="break-words">{conflict}</span>
            <Button type="button" variant="outline" size="sm" onClick={() => void reloadLatest()} disabled={saving || syncing}>{t("claude.modelsWhitelistReload")}</Button>
          </div>
        ) : null}

        <div className="flex flex-wrap gap-2">
          <Input
            className="min-w-[220px] flex-1"
            value={input}
            onChange={(event) => setInput(event.target.value)}
            onKeyDown={(event) => { if (event.key === "Enter") { event.preventDefault(); addModels(); } }}
            placeholder={t("claude.modelsWhitelistPlaceholder")}
            disabled={saving || syncing}
          />
          <Button type="button" variant="outline" onClick={addModels} disabled={!input.trim() || saving || syncing}>
            <Plus className="size-3.5" />
            {t("claude.modelsWhitelistAdd")}
          </Button>
          <Button type="button" variant="outline" onClick={() => void syncUpstream()} disabled={saving || syncing}>
            <RefreshCw className={cn("size-3.5", syncing && "animate-spin")} />
            {syncing ? t("claude.modelsWhitelistSyncing") : t("claude.modelsWhitelistSync")}
          </Button>
        </div>
        {inputError ? <p className="break-words text-xs text-rose-600 dark:text-rose-300">{inputError}</p> : null}

        <div className="space-y-2">
          <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
            <span>{models.length === 0 ? t("claude.modelsWhitelistAll") : t("claude.modelsWhitelistCount", { count: models.length })}</span>
            {models.length > 0 ? <button type="button" className="hover:text-foreground" onClick={() => setModels([])} disabled={saving || syncing}>{t("claude.modelsWhitelistClear")}</button> : null}
          </div>
          {models.length > 0 ? (
            <div className="flex max-h-52 flex-wrap gap-1.5 overflow-y-auto rounded-lg border border-border bg-muted/10 p-2.5">
              {models.map((model) => {
                const cd = cooldownByModel.get(model.toLowerCase());
                return (
                  <span key={model.toLowerCase()} className="inline-flex items-center gap-1 rounded-md border border-border bg-background py-1 pl-2 pr-1 text-[12px]">
                    <span className="font-mono text-foreground">{model}</span>
                    {cd?.credits ? (
                      <span className="inline-flex items-center rounded bg-amber-500/15 px-1 py-0.5 text-[10px] font-medium text-amber-600 dark:text-amber-400" title={t("claude.modelNeedsCreditsHint")}>
                        {t("claude.modelNeedsCredits")}
                      </span>
                    ) : cd ? (
                      <span className="inline-flex items-center rounded bg-rose-500/15 px-1 py-0.5 text-[10px] font-medium text-rose-600 dark:text-rose-400" title={cd.reason}>
                        {t("claude.modelRateLimited")}
                      </span>
                    ) : (
                      <span className="inline-flex items-center rounded bg-emerald-500/12 px-1 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400">
                        {t("claude.modelAvailable")}
                      </span>
                    )}
                    <button type="button" className="inline-flex size-4 items-center justify-center rounded text-muted-foreground hover:bg-muted hover:text-foreground" onClick={() => setModels((current) => current.filter((item) => item.toLowerCase() !== model.toLowerCase()))} disabled={saving || syncing} aria-label={t("claude.modelsWhitelistRemove", { model })}>
                      <X className="size-3" />
                    </button>
                  </span>
                );
              })}
            </div>
          ) : (
            <div className="rounded-lg border border-dashed border-border bg-muted/20 px-3 py-3 text-sm text-muted-foreground">{t("claude.modelsWhitelistAllHint")}</div>
          )}
        </div>
      </div>
    </Modal>
  );
}

// ── 添加账号弹窗:网页 OAuth 两步式 / 导入 token JSON ──────
type ClaudeTestEvent = {
  type: "test_start" | "content" | "test_complete" | "error";
  model?: string;
  text?: string;
  error?: string;
  success?: boolean;
};

function ClaudeTestModal({
  account,
  onClose,
  onSettled,
}: {
  account: AccountRow;
  onClose: () => void;
  onSettled: () => void;
}) {
  const { t } = useTranslation();
	const [status, setStatus] = useState<"connecting" | "streaming" | "success" | "error">("connecting");
	const [output, setOutput] = useState<string[]>([]);
	const [errorMessage, setErrorMessage] = useState("");
	const settledRef = useRef(false);
	const onSettledRef = useRef(onSettled);
	onSettledRef.current = onSettled;
	const modelOptions = useMemo(() => {
		const blockedForCredits = new Set(
			(account.model_cooldowns ?? [])
				.filter((cooldown) => (cooldown.reason || "").toLowerCase().includes("credit"))
				.map((cooldown) => cooldown.model.toLowerCase()),
		);
		const configured = (account.models ?? []).filter((item) => {
			const normalized = item.trim().toLowerCase();
			return normalized.startsWith("claude-") && !blockedForCredits.has(normalized);
		});
		return configured.length > 0
			? configured
			: ["claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"].filter(
					(model) => !blockedForCredits.has(model),
				);
	}, [account.model_cooldowns, account.models]);
	const [selectedModel, setSelectedModel] = useState(modelOptions[0] || "");
	const model = selectedModel;

	useEffect(() => {
		if (!modelOptions.includes(selectedModel)) {
			setSelectedModel(modelOptions[0] || "");
		}
	}, [modelOptions, selectedModel]);

	const markSettled = useCallback(() => {
		if (settledRef.current) return;
		settledRef.current = true;
		onSettledRef.current();
	}, []);

	useEffect(() => {
		if (!model) return;
		setStatus("connecting");
    setOutput([]);
    setErrorMessage("");
    settledRef.current = false;
    const controller = new AbortController();
    const run = async () => {
      try {
        const query = new URLSearchParams({ model });
        const response = await fetch(`/api/admin/accounts/${account.id}/test?${query.toString()}`, {
          signal: controller.signal,
          headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
		});
		if (!response.ok) {
			const body = await response.text();
			let message = `HTTP ${response.status}`;
			try {
				const parsed = JSON.parse(body) as { error?: string | { message?: string } };
				if (typeof parsed.error === "string") message = parsed.error;
				else if (parsed.error?.message) message = parsed.error.message;
			} catch {
				if (body.trim()) message = body.trim().slice(0, 500);
			}
			setStatus("error");
			setErrorMessage(`${t("accounts.testFailed")}: ${message}`);
			markSettled();
          return;
        }
		const reader = response.body?.getReader();
		if (!reader) throw new Error(t("accounts.browserStreamingUnsupported"));
		const decoder = new TextDecoder();
		let buffer = "";
		let receivedTerminalEvent = false;
        const process = (lines: string[]) => {
          for (const line of lines) {
            const trimmed = line.trim();
            if (!trimmed.startsWith("data: ")) continue;
            try {
              const event = JSON.parse(trimmed.slice(6)) as ClaudeTestEvent;
              if (event.type === "test_start") setStatus("streaming");
              if (event.type === "content" && event.text) setOutput((prev) => [...prev, event.text!]);
				if (event.type === "test_complete") {
					receivedTerminalEvent = true;
					setStatus(event.success ? "success" : "error");
					if (!event.success) setErrorMessage(t("accounts.testFailed"));
				}
				if (event.type === "error") {
					receivedTerminalEvent = true;
					setStatus("error");
					setErrorMessage(event.error || t("accounts.unknownError"));
				}
            } catch {
              // Ignore comments/partial SSE frames.
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
          process(lines);
		}
		if (buffer.trim()) process([buffer]);
		// The server invalidates its account snapshot in a handler defer after
		// the terminal event. Refresh only once the SSE stream has closed.
		if (receivedTerminalEvent) {
			markSettled();
		} else {
			setStatus("error");
			setErrorMessage(t("accounts.connectionEndedUnexpectedly"));
          markSettled();
        }
      } catch (error) {
        if (controller.signal.aborted) return;
        setStatus("error");
        setErrorMessage(error instanceof Error ? error.message : t("accounts.connectionFailed"));
        markSettled();
      }
    };
    void run();
    return () => controller.abort();
	}, [account.id, markSettled, model, t]);

  const StatusIcon = status === "success" ? CheckCircle : status === "error" ? XCircle : Loader2;
  return (
    <Modal
      show
      title={`${t("accounts.testConnectionTitle", { account: account.email || account.name || `#${account.id}` })} · Claude`}
      onClose={onClose}
      footer={<Button onClick={onClose}>{t("common.close")}</Button>}
    >
      <div className="space-y-3">
        <div className="flex items-center gap-2 text-sm">
          <StatusIcon className={cn("size-4", status === "connecting" || status === "streaming" ? "animate-spin text-blue-500" : status === "success" ? "text-emerald-500" : "text-rose-500")} />
          <span>{status === "connecting" ? t("accounts.connecting") : status === "streaming" ? t("accounts.receivingResponse") : status === "success" ? t("accounts.testSuccess") : t("accounts.testFailed")}</span>
	          {modelOptions.length > 0 ? (
	            <Select
	              compact
	              className="ml-auto w-48"
	              value={model}
	              onValueChange={setSelectedModel}
	              options={modelOptions.map((item) => ({ value: item, label: item }))}
	            />
	          ) : null}
        </div>
        {errorMessage ? <div className="break-words rounded-lg bg-rose-500/10 px-3 py-2 text-xs text-rose-700 dark:text-rose-300">{errorMessage}</div> : null}
        <pre className="max-h-64 overflow-auto whitespace-pre-wrap rounded-lg border border-border bg-muted/30 p-3 text-xs leading-relaxed">{output.join("") || (status === "success" ? t("accounts.testSuccess") : t("common.loading"))}</pre>
      </div>
    </Modal>
  );
}

function ClaudeAddModal({
  proxies,
  groups,
  initialTab = "oauth",
  onClose,
  onAdded,
}: {
  proxies: ProxyRow[];
  groups: AccountGroup[];
  initialTab?: "oauth" | "import";
  onClose: () => void;
  onAdded: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const [tab, setTab] = useState<"oauth" | "import">(initialTab);

  const [proxyUrl, setProxyUrl] = useState("");
  const [useProxyPool, setUseProxyPool] = useState(false);
  const [name, setName] = useState("");
  const [timezone, setTimezone] = useState("");
  const [timezoneCustom, setTimezoneCustom] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [groupIds, setGroupIds] = useState<Set<number>>(new Set());

  const [authUrl, setAuthUrl] = useState("");
  const [state, setState] = useState("");
  const [callback, setCallback] = useState("");
  const [tokenJson, setTokenJson] = useState("");
  const fileInputRef = useRef<HTMLInputElement>(null);

  const toggleGroup = useCallback((id: number) => {
    setGroupIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  // 添加成功后,如选择了分组则批量指派(用新账号返回的 id)。
  const applyGroups = useCallback(
    async (id?: number) => {
      if (groupIds.size === 0 || !id) return;
      try {
        await api.batchUpdateAccounts({ ids: [id], group_ids: Array.from(groupIds) });
      } catch {
        /* 分组指派失败不阻断添加流程 */
      }
    },
    [groupIds],
  );

  // 生成授权链接只展示,不自动弹授权页:由用户确认链接后自行打开或复制到别处授权。
  const [authUrlLoading, setAuthUrlLoading] = useState(false);
  const genAuthUrl = useCallback(async () => {
    setAuthUrlLoading(true);
    try {
      const res = await api.generateClaudeAuthURL();
      setAuthUrl(res.auth_url);
      setState(res.state);
    } catch (error) {
      showToast(t("claude.authUrlFailed") + ": " + getErrorMessage(error), "error");
    } finally {
      setAuthUrlLoading(false);
    }
  }, [showToast, t]);

  const submitOAuth = useCallback(async () => {
    const code = extractCode(callback);
    if (!state || !code) {
      showToast(t("claude.exchangeFailed"), "error");
      return;
    }
    setSubmitting(true);
    try {
      const res = await api.exchangeClaudeOAuthCode({
        state,
        code,
        name: name.trim() || undefined,
        proxy_url: useProxyPool ? undefined : proxyUrl.trim() || undefined,
        use_proxy_pool: useProxyPool || undefined,
        timezone: timezone.trim() || undefined,
      });
      await applyGroups(res?.id);
      showToast(t("claude.added"), "success");
      if (!useProxyPool) await maybeOfferSaveProxyToPool(proxyUrl, proxies, confirm, showToast, t);
      onAdded();
    } catch (error) {
      showToast(t("claude.exchangeFailed") + ": " + getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  }, [callback, name, onAdded, proxyUrl, proxies, confirm, showToast, state, t, timezone, useProxyPool, applyGroups]);

  const submitImport = useCallback(async () => {
    let parsed: Record<string, unknown> | unknown[];
    try {
      const decoded = JSON.parse(tokenJson) as unknown;
      if (!decoded || typeof decoded !== "object") throw new Error("object required");
      parsed = decoded as Record<string, unknown> | unknown[];
    } catch {
      showToast(t("claude.invalidJson"), "error");
      return;
    }
    const documents = Array.isArray(parsed)
      ? parsed
      : Array.isArray(parsed.accounts)
        ? parsed.accounts
        : [parsed];
    const firstDocument = documents[0];
    if (!firstDocument || typeof firstDocument !== "object" || Array.isArray(firstDocument)
      || typeof (firstDocument as Record<string, unknown>).access_token !== "string"
      || typeof (firstDocument as Record<string, unknown>).refresh_token !== "string") {
      showToast(t("claude.invalidJson"), "error");
      return;
    }
    const hasImportedProxy = documents.some((document) =>
      Boolean(document && typeof document === "object" && !Array.isArray(document)
        && typeof (document as Record<string, unknown>).proxy_url === "string"
        && String((document as Record<string, unknown>).proxy_url).trim()),
    );
    if (hasImportedProxy && !useProxyPool && !proxyUrl.trim()) {
      const keepImportedProxy = await confirm({
        title: t("claude.importProxyConfirmTitle"),
        description: t("claude.importProxyConfirmDescription"),
      });
      if (!keepImportedProxy) return;
    }
    setSubmitting(true);
    try {
      const selectedGroupRefs = groups
        .filter((group) => groupIds.has(group.id))
        .map((group) => ({ name: group.name, channel: "claude" as const }));
      const applyOverrides = (document: unknown): ClaudeCredentialExportEntry => {
        const source = document as Record<string, unknown>;
        return {
          ...source,
          name: name.trim() || source.name,
          proxy_url: useProxyPool ? undefined : proxyUrl.trim() || source.proxy_url,
          use_proxy_pool: useProxyPool || undefined,
          timezone: timezone.trim() || source.timezone,
          ...(selectedGroupRefs.length > 0 && !Array.isArray(source.group_refs)
            ? { group_refs: selectedGroupRefs }
            : {}),
        } as unknown as ClaudeCredentialExportEntry;
      };
      const payload = Array.isArray(parsed)
        ? documents.map(applyOverrides)
        : Array.isArray(parsed.accounts)
          ? { ...parsed, accounts: documents.map(applyOverrides) }
          : applyOverrides(parsed);
      const res = await api.importClaudeCredentialBundle(payload);
      const imported = "imported" in res ? res.imported : ("id" in res && res.id ? 1 : 0);
      await applyGroups("id" in res ? res.id : undefined);
      showToast(imported > 0 ? t("claude.added") : t("claude.importNothingAdded"), imported > 0 ? "success" : "warning");
      if (!useProxyPool) await maybeOfferSaveProxyToPool(proxyUrl, proxies, confirm, showToast, t);
      onAdded();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  }, [groups, groupIds, name, onAdded, proxyUrl, proxies, confirm, showToast, t, timezone, tokenJson, useProxyPool, applyGroups]);

  const handleImportFile = useCallback(async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    if (file.size > 8 * 1024 * 1024) {
      showToast(t("claude.importFileTooLarge"), "error");
      return;
    }
    try {
      setTokenJson(await file.text());
      setTab("import");
      showToast(t("claude.importFileLoaded"), "info");
    } catch (error) {
      showToast(t("claude.invalidJson") + ": " + getErrorMessage(error), "error");
    }
  }, [showToast, t]);

  const commonFields = (
    <div className="space-y-2">
      <ProxyField value={proxyUrl} onChange={setProxyUrl} proxies={proxies} label={t("claude.proxyLabel")} disabled={useProxyPool} />
      <label className="flex items-center gap-2 text-xs text-muted-foreground">
        <input type="checkbox" checked={useProxyPool} onChange={(e) => setUseProxyPool(e.target.checked)} />
        {t("claude.useProxyPool")}
      </label>
      <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={t("claude.namePlaceholder")} />
      <div className="space-y-1">
        <Select
          value={timezoneCustom ? CLAUDE_TIMEZONE_CUSTOM : (findClaudeTimezoneOption(timezone)?.value ?? (timezone.trim() ? CLAUDE_TIMEZONE_CUSTOM : ""))}
          onValueChange={(value) => {
            if (value === CLAUDE_TIMEZONE_CUSTOM) {
              setTimezoneCustom(true);
              if (findClaudeTimezoneOption(timezone)) setTimezone("");
              return;
            }
            setTimezoneCustom(false);
            setTimezone(value);
          }}
          options={[
            { value: "", label: t("claude.timezoneUnset") },
            ...CLAUDE_TIMEZONE_OPTIONS,
            { value: CLAUDE_TIMEZONE_CUSTOM, label: t("claude.timezoneCustom") },
          ]}
        />
        {findClaudeTimezoneOption(timezone) ? <p className="text-[10px] text-muted-foreground">{claudeTimezoneLabel(timezone)}</p> : null}
        {timezoneCustom ? <Input value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder={t("claude.timezonePlaceholder")} /> : null}
      </div>
      {groups.length > 0 ? (
        <div className="space-y-1">
          <span className="text-xs font-semibold text-muted-foreground">{t("claude.filterGroup")}</span>
          <div className="flex flex-wrap gap-1.5">
            {groups.map((g) => {
              const on = groupIds.has(g.id);
              return (
                <button
                  key={g.id}
                  type="button"
                  onClick={() => toggleGroup(g.id)}
                  className={cn(
                    "inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[11px] transition-colors",
                    on ? "border-transparent text-white" : "border-border text-muted-foreground",
                  )}
                  style={on ? { backgroundColor: normalizeGroupColor(g.color) } : undefined}
                >
                  <span className="size-2 rounded-full" style={{ backgroundColor: normalizeGroupColor(g.color) }} />
                  {g.name}
                </button>
              );
            })}
          </div>
        </div>
      ) : null}
      <input ref={fileInputRef} type="file" accept=".json,application/json" className="hidden" onChange={handleImportFile} />
      {tab === "import" ? (
        <Button type="button" variant="outline" size="sm" onClick={() => fileInputRef.current?.click()}>
          <Upload className="size-3.5" />
          {t("claude.chooseCredentialFile")}
        </Button>
      ) : null}
    </div>
  );

  return (
    <Modal
      show
      onClose={onClose}
      title={t("claude.addAccount")}
      contentClassName="sm:max-w-[680px]"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          {tab === "oauth" ? (
            <Button onClick={() => void submitOAuth()} disabled={submitting}>
              {t("claude.exchange")}
            </Button>
          ) : (
            <Button onClick={() => void submitImport()} disabled={submitting}>
              {t("claude.import")}
            </Button>
          )}
        </div>
      }
    >
      <div className="space-y-4">
        <div className="flex gap-2">
          <Button variant={tab === "oauth" ? "default" : "ghost"} size="sm" onClick={() => setTab("oauth")}>
            {t("claude.tabOAuth")}
          </Button>
          <Button variant={tab === "import" ? "default" : "ghost"} size="sm" onClick={() => setTab("import")}>
            {t("claude.tabImport")}
          </Button>
        </div>

        {tab === "oauth" ? (
          <div className="space-y-3">
            <p className="text-xs text-muted-foreground">{t("claude.step1")}</p>
            {/* 先生成并展示授权链接(不自动弹授权页),用户核对后自行打开/复制 */}
            {!authUrl ? (
              <Button variant="secondary" size="sm" disabled={authUrlLoading} onClick={() => void genAuthUrl()}>
                {authUrlLoading ? <Loader2 className="size-3.5 animate-spin" /> : <Plus className="size-3.5" />}
                {t("claude.genAuthUrl")}
              </Button>
            ) : (
              <div className="space-y-2 rounded-lg border border-border bg-muted/30 p-3">
                <p className="text-xs text-muted-foreground">{t("claude.authUrlReady")}</p>
                {/* 完整 URL 直接作为可点击链接展示:全量换行(break-all)不出滚动条 */}
                <a
                  href={authUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="block w-full rounded-md border border-input bg-background p-2 font-mono text-[11px] leading-snug break-all text-primary underline decoration-primary/40 underline-offset-2 hover:decoration-primary"
                >
                  {authUrl}
                </a>
                <div className="flex flex-wrap items-center gap-2">
                  <Button size="sm" onClick={() => window.open(authUrl, "_blank", "noopener,noreferrer")}>
                    <ExternalLink className="size-3.5" />
                    {t("claude.openAuth")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      void navigator.clipboard?.writeText(authUrl);
                      showToast(t("claude.authUrlCopied"), "success");
                    }}
                  >
                    {t("claude.copyLink")}
                  </Button>
                  <Button variant="ghost" size="sm" disabled={authUrlLoading} onClick={() => void genAuthUrl()}>
                    <RefreshCw className={cn("size-3.5", authUrlLoading && "animate-spin")} />
                    {t("claude.regenAuthUrl")}
                  </Button>
                </div>
              </div>
            )}
            <p className="text-xs text-muted-foreground">{t("claude.step2")}</p>
            <Input value={callback} onChange={(e) => setCallback(e.target.value)} placeholder={t("claude.callbackPlaceholder")} />
            {commonFields}
          </div>
        ) : (
          <div className="space-y-3">
            <p className="text-xs text-muted-foreground">{t("claude.importHint")}</p>
            <textarea
              value={tokenJson}
              onChange={(e) => setTokenJson(e.target.value)}
              placeholder={t("claude.importPlaceholder")}
              rows={6}
              className="w-full rounded-md border border-input bg-background p-2 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/20"
            />
            {commonFields}
          </div>
        )}
      </div>
      {confirmDialog}
    </Modal>
  );
}
