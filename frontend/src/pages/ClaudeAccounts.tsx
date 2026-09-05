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
  Unlock,
  MoreHorizontal,
  Trash2,
  Plus,
  Loader2,
  SlidersHorizontal,
  Download,
  Upload,
  Search,
  Eye,
  EyeOff,
  FolderOpen,
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Power,
  PowerOff,
  ListChecks,
  Zap,
  FileJson,
  Hourglass,
  Wallet,
} from "lucide-react";

import { api } from "../api";
import type { NamedBlob } from "../api";
import type { ProxyRow } from "../api";
import type {
  AccountRow,
  AccountGroup,
  AccountListSummary,
  AccountEmailDomainFacet,
  AccountPageStatsItem,
  AccountHealthBucket,
  AccountsPageParams,
  ClaudeCredentialExportEntry,
} from "../types";
import AccountUsageModal from "../components/AccountUsageModal";
import ClaudeConnectionTestModal from "../components/ClaudeConnectionTestModal";
import AccountDetailSheet from "../components/AccountDetailSheet";
import AccountHealthBar from "../components/AccountHealthBar";
import RequestCountPills from "../components/RequestCountPills";
import ColumnSettingsMenu from "../components/ColumnSettingsMenu";
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
import { Card, CardContent } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  HeaderActionMenu,
  type HeaderActionMenuItem,
  type HeaderActionMenuSection,
} from "../components/HeaderActionMenu";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
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
  return pct >= 90 ? "bg-red-500" : pct >= 70 ? "bg-amber-500" : "bg-emerald-500";
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
// tone 只含配色,cls 是详情面板用的小徽章;表格列用 Codex PlanBadge 同尺寸另拼。
function claudePlanBadge(plan: string): { label: string; tone: string; cls: string } {
  const p = plan.trim().toLowerCase();
  const base = "inline-flex items-center rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset";
  const build = (label: string, tone: string) => ({ label, tone, cls: `${base} ${tone}` });
  switch (p) {
    case "pro":
      return build("Pro", "bg-purple-50 text-purple-700 ring-purple-600/20 dark:bg-purple-950 dark:text-purple-300 dark:ring-purple-400/20");
    case "max-5x":
      return build("Max 5x", "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20");
    case "max-20x":
      return build("Max 20x", "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20");
    case "max":
      return build("Max", "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20");
    case "team":
      return build("Team", "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20");
    case "enterprise":
      return build("Enterprise", "bg-indigo-50 text-indigo-700 ring-indigo-600/20 dark:bg-indigo-950 dark:text-indigo-300 dark:ring-indigo-400/20");
    case "business":
      return build("Business", "bg-indigo-50 text-indigo-700 ring-indigo-600/20 dark:bg-indigo-950 dark:text-indigo-300 dark:ring-indigo-400/20");
    case "free":
      return build("Free", "bg-zinc-100 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-400 dark:ring-zinc-500/20");
    default:
      return build(plan, "bg-purple-50 text-purple-700 ring-purple-600/20 dark:bg-purple-950 dark:text-purple-300 dark:ring-purple-400/20");
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

// 排序键(表头 / 排序按钮点击切换,同键再点翻转方向,与 Codex 一致)。
// 未选排序时不传 sort:显式 updated_at 排序会被采样/刷新改写时间戳而抖动,
// 省略则走后端确定性 ID 序,刷新后行位置不变。
type SortKey = "group" | "priority" | "usage" | "requests" | "today" | "importTime";
type SortDir = "asc" | "desc";
const SORT_FIELD: Record<SortKey, NonNullable<AccountsPageParams["sort"]>> = {
  group: "group",
  priority: "scheduler_priority",
  usage: "usage",
  requests: "requests",
  today: "today",
  importTime: "created_at",
};
const SORT_DEFAULT_DIR: Record<SortKey, SortDir> = {
  group: "asc",
  priority: "desc",
  usage: "desc",
  requests: "desc",
  today: "desc",
  importTime: "desc",
};

// 可显隐列(序号/邮箱/操作为固定核心列,不参与切换)。持久化到 localStorage,与 Codex 一致;
// 标签列与 Codex 同样默认隐藏。
const CLAUDE_TOGGLE_COLUMNS = [
  "tags",
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
  return Object.fromEntries(CLAUDE_TOGGLE_COLUMNS.map((c) => [c, c !== "tags"])) as ClaudeColVisibility;
}

function loadClaudeCols(): ClaudeColVisibility {
  const fallback = defaultClaudeCols();
  try {
    const raw = window.localStorage.getItem(CLAUDE_COLS_KEY);
    if (!raw) return fallback;
    const parsed = JSON.parse(raw) as Partial<ClaudeColVisibility>;
    return Object.fromEntries(
      CLAUDE_TOGGLE_COLUMNS.map((c) => [c, typeof parsed[c] === "boolean" ? parsed[c] : fallback[c]]),
    ) as ClaudeColVisibility;
  } catch {
    return fallback;
  }
}

// LiveCountdown 显示限流/重置的剩余时间,每秒刷新。
// plain=true 为弱化文本样式;默认是与 Codex CooldownTimer 同款的琥珀沙漏胶囊(限流冷却)。
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
  const sec = remain % 60;
  const text = d > 0 ? `${d}d${h}h` : h > 0 ? `${h}h${m}m` : m > 0 ? `${m}m${sec}s` : `${sec}s`;
  if (plain) {
    return (
      <span className="text-[11px] font-medium text-muted-foreground tabular-nums">
        {label} {text}
      </span>
    );
  }
  return (
    <span
      className="inline-flex h-6 min-w-[112px] shrink-0 items-center justify-center gap-1.5 rounded-full bg-amber-50 px-2 text-[11px] font-mono leading-none tabular-nums text-amber-700 ring-1 ring-inset ring-amber-200/70 dark:bg-amber-950/40 dark:text-amber-300 dark:ring-amber-400/20"
      title={`${label} ${new Date(target).toLocaleString()}`}
    >
      <Hourglass className="size-3 shrink-0" aria-hidden="true" />
      {text}
    </span>
  );
}

// 标签 / 条 / 百分比 三列固定宽度(与 Codex UsageBar 同款),避免 5h 与 7d 把进度条拉成不同长度。
const USAGE_BAR_LABEL_CLASS = "w-10 shrink-0 text-[11px] font-medium text-muted-foreground";
const USAGE_BAR_TRACK_CLASS = "h-1.5 w-[88px] shrink-0 overflow-hidden rounded-full bg-muted";
const USAGE_BAR_META_CLASS = "mt-0.5 pl-[46px] text-[11px] font-medium text-muted-foreground";

function hasWindowDetail(detail?: AccountRow["usage_5h_detail"]): boolean {
  return Boolean(detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0));
}

// UsageWindow 单条用量窗口(5h / 7d)。视觉对齐 Codex 的 UsageBar / UsageWindowStat:
// - percent 有真实观测(Anthropic 统一限流头)→ 进度条 + 百分比,明细与 ⏱重置各占一行;
// - 仅有网关侧明细(req/tok/$)→ 明细行;
// - 两者都无 → 不渲染(由父级统一显示 "-")。
function UsageWindow({
  label,
  pct,
  reset,
  detail,
}: {
  label: string;
  pct: number | null;
  reset?: string;
  detail?: AccountRow["usage_5h_detail"];
}) {
  const { t } = useTranslation();
  const hasDetail = hasWindowDetail(detail);
  if (pct === null && !hasDetail) return null;
  const billed = typeof detail?.account_billed === "number" && detail.account_billed > 0 ? detail.account_billed : null;
  const detailText = hasDetail
    ? `${formatCompactNum(detail?.requests)} ${t("accounts.usageReqUnit")} / ${formatCompactNum(detail?.tokens)} ${t("accounts.usageTokUnit")}`
    : "";
  if (pct === null) {
    return (
      <div className="flex flex-col gap-0.5">
        <div className="flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
          <span className={USAGE_BAR_LABEL_CLASS}>{label}</span>
          <span>{detailText}</span>
        </div>
        {billed !== null ? (
          <div className="pl-[46px] text-[10px] text-muted-foreground/80">
            {t("accounts.accountBilledLabel")}: ${billed.toFixed(4)}
          </div>
        ) : null}
      </div>
    );
  }
  const resetTs = reset ? new Date(reset).getTime() : NaN;
  const rt = Number.isFinite(resetTs) && resetTs > Date.now() ? formatShortDateTime(reset) : null;
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <span className={USAGE_BAR_LABEL_CLASS}>{label}</span>
        <div className={USAGE_BAR_TRACK_CLASS}>
          <div className={cn("h-full rounded-full transition-all", usageTone(pct))} style={{ width: `${Math.min(100, pct)}%` }} />
        </div>
        <span className="w-[42px] shrink-0 text-right text-[12px] font-semibold tabular-nums">{pct.toFixed(1)}%</span>
      </div>
      {detailText ? <div className={USAGE_BAR_META_CLASS}>{detailText}</div> : null}
      {rt ? (
        <div className={USAGE_BAR_META_CLASS} title={rt.title}>
          ⏱ {rt.label}
        </div>
      ) : null}
    </div>
  );
}

// Upper bound on concurrent one-time usage backfill requests per page mount.
const LEGACY_USAGE_BACKFILL_BATCH = 4;

function ClaudeScopedUsageWindows({ windows }: { windows?: AccountRow["claude_usage_windows"] }) {
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
  const [sortKey, setSortKey] = useState<SortKey | null>(null);
  const [sortDir, setSortDir] = useState<SortDir>("desc");
  const [showFilters, setShowFilters] = useState(false);
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
  }, [debouncedSearch, statusFilter, healthTier, planFilter, authFilter, tagFilter, domainFilter, groupFilter, sortKey, sortDir, pageSize]);

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
      const sort = sortKey ? SORT_FIELD[sortKey] : undefined;
      const order: SortDir = sortKey ? sortDir : "asc";
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
    sortDir,
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
  const allPageSelected = accounts.length > 0 && accounts.every((a) => selected.has(a.id));
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

  const handleBatchDelete = useCallback(async () => {
    if (selectedIds.length === 0) return;
    const ok = await confirm({
      title: t("accounts.batchDeleteTitle"),
      description: t("accounts.batchDeleteDesc", { count: selectedIds.length }),
    });
    if (!ok) return;
    try {
      await api.batchDeleteAccounts(selectedIds);
      setSelected(new Set());
      void reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [confirm, reload, selectedIds, showToast, t]);

  // ── 排序(与 Codex 一致:表头/按钮点击切换,同键再点翻转方向) ──
  const toggleSort = useCallback((key: SortKey) => {
    if (sortKey === key) {
      setSortDir((current) => (current === "desc" ? "asc" : "desc"));
    } else {
      setSortKey(key);
      setSortDir(SORT_DEFAULT_DIR[key]);
    }
  }, [sortKey]);


  // ── 派生 UI 数据 ──────────────────────────────────────────
  const statChips = useMemo(() => {
    const s = summary;
    const c: Array<{ id: ClaudeStatusFilter; label: string; count: number }> = [
      { id: "all", label: t("claude.statAll"), count: s?.total ?? total },
      { id: "normal", label: t("claude.statNormal"), count: s?.normal ?? 0 },
      { id: "scheduling", label: t("claude.statScheduling"), count: s?.active ?? 0 },
      { id: "rate_limited", label: t("claude.statRateLimited"), count: s?.rate_limited ?? 0 },
      { id: "abnormal", label: t("claude.statAbnormal"), count: s?.abnormal ?? 0 },
      { id: "banned", label: t("claude.statBanned"), count: s?.banned ?? 0 },
      { id: "error", label: t("claude.statError"), count: s?.error ?? 0 },
      { id: "unsampled", label: t("claude.statUnsampled"), count: s?.unsampled ?? 0 },
      { id: "disabled", label: t("claude.statDisabled"), count: s?.disabled ?? 0 },
      { id: "locked", label: t("claude.statLocked"), count: s?.locked ?? 0 },
    ];
    return c;
  }, [summary, total, t]);
  const statusLabelFor = useCallback(
    (id: ClaudeStatusFilter) => statChips.find((chip) => chip.id === id)?.label ?? id,
    [statChips],
  );

  const healthChips = useMemo(() => {
    const s = summary;
    return [
      { id: "healthy" as HealthTier, label: t("claude.healthHealthy"), count: s?.healthy ?? 0, tone: "success" as const },
      { id: "warm" as HealthTier, label: t("claude.healthWarm"), count: s?.warm ?? 0, tone: "warning" as const },
      { id: "risky" as HealthTier, label: t("claude.healthRisky"), count: s?.risky ?? 0, tone: "danger" as const },
      { id: "banned" as HealthTier, label: t("claude.healthBanned"), count: s?.banned ?? 0, tone: "neutral" as const },
    ];
  }, [summary, t]);

  const planTabs = useMemo(() => {
    const plans = knownPlans.filter(Boolean).sort();
    return ["all", ...plans];
  }, [knownPlans]);
  const planLabel = (plan: string) => (plan === "all" ? t("accounts.filterAll") : claudePlanBadge(plan).label);

  // Claude 账号当前只支持 OAuth;不展示一个永远为 0 的 API Key 筛选,避免
  // 运营误以为 Claude API Key 可以走同一原生链路。
  const authTabs: Array<{ id: AuthFilter; label: string; count?: number }> = [
    { id: "all", label: t("accounts.filterAll") },
    { id: "oauth", label: "OAuth", count: summary?.oauth || summary?.total || 0 },
  ];

  const hasFilterPills =
    statusFilter !== "all" ||
    healthTier !== null ||
    planFilter !== "all" ||
    authFilter !== "all" ||
    tagFilter !== "all" ||
    domainFilter !== "all" ||
    !isAccountGroupFilterEmpty(groupFilter);
  const filtersActive = hasFilterPills || debouncedSearch.length > 0;

  const clearFilters = useCallback(() => {
    setStatusFilter("all");
    setHealthTier(null);
    setPlanFilter("all");
    setAuthFilter("all");
    setTagFilter("all");
    setDomainFilter("all");
    setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
    setSearch("");
    setSortKey(null);
    setSortDir("desc");
  }, []);

  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const poolEmpty = !loading && total === 0 && !filtersActive;

  const openAdd = (tab: "oauth" | "import") => {
    setAddInitialTab(tab);
    setShowAdd(true);
  };

  // 「管理」只收低频项;导入 / 导出 / 分析常驻在外(与 Codex 页一致)。
  const manageSections: HeaderActionMenuSection[] = [
    {
      key: "maintenance",
      label: t("accounts.maintenanceActions"),
      items: [
        {
          key: "refresh-models",
          label: t("claude.refreshAllModels"),
          icon: <RefreshCw className="size-3.5" />,
          disabled: total === 0,
          onSelect: () => void handleRefreshAllModels(),
        },
      ],
    },
    {
      key: "data",
      label: t("accounts.dataActions"),
      items: [
        {
          key: "export-healthy",
          label: t("claude.exportHealthy"),
          icon: <Download className="size-3.5" />,
          disabled: exporting || total === 0,
          onSelect: () => void handleExport("healthy"),
        },
      ],
    },
  ];

  const filterPillClass =
    "inline-flex max-w-full items-center gap-1.5 rounded-md border border-border/60 bg-background/70 px-2 py-1 text-[11px] font-medium text-muted-foreground transition-colors hover:border-primary/30 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50";
  const segmentGroupClass =
    "flex h-8 max-w-full shrink-0 items-center gap-0.5 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden";
  const segmentButtonClass = (active: boolean) =>
    cn(
      "flex h-6 shrink-0 items-center whitespace-nowrap rounded-md px-2 text-xs font-medium transition-colors",
      active ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
    );
  const sortOptions = [
    { value: "default", label: t("claude.sortDefault") },
    { value: "group", label: t("claude.sortGroup") },
    { value: "priority", label: t("claude.sortPriority") },
    { value: "usage", label: t("claude.sortUsage") },
    { value: "requests", label: t("claude.sortRequests") },
    { value: "today", label: t("claude.sortToday") },
    { value: "importTime", label: t("accounts.importTime") },
  ];
  const renderSortHead = (key: SortKey, label: string, title?: string) => (
    <TableHead aria-sort={sortKey === key ? (sortDir === "desc" ? "descending" : "ascending") : "none"} title={title}>
      <button
        type="button"
        onClick={() => toggleSort(key)}
        className={cn(
          "group inline-flex items-center gap-1.5 rounded py-1 text-xs font-medium outline-none transition-colors hover:text-primary focus-visible:ring-2 focus-visible:ring-ring/50",
          sortKey === key && "text-primary",
        )}
      >
        {label}
        {sortKey === key ? (
          sortDir === "desc" ? <ArrowDown className="size-3.5" /> : <ArrowUp className="size-3.5" />
        ) : <ArrowUpDown className="size-3 text-muted-foreground/35 group-hover:text-primary/70" />}
      </button>
    </TableHead>
  );

  return (
    <div className="relative @container/claude-accounts">
      <PageHeader
        title={t("claude.title")}
        description={t("claude.subtitle")}
        hideTitle={Boolean(headerSlot)}
        actionsBelow
        titleAdornment={headerSlot}
        actions={
          <div className="flex w-full flex-wrap items-center gap-1.5 sm:gap-2 [&>button]:h-8">
            <Button size="sm" className="min-w-0 sm:flex-none" onClick={() => openAdd("oauth")}>
              <Plus className="size-3.5" />
              {t("claude.addAccount")}
            </Button>
            <Button variant="outline" size="sm" className="size-8 p-0 md:w-auto md:px-2.5" title={t("claude.importCredentials")} aria-label={t("claude.importCredentials")} onClick={() => openAdd("import")}>
              <Upload className="size-3.5" />
              <span className="hidden md:inline">{t("claude.importCredentials")}</span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="size-8 p-0 md:w-auto md:px-2.5"
              title={selectedIds.length > 0 ? t("claude.exportSelected") : t("claude.exportAll")}
              aria-label={selectedIds.length > 0 ? t("claude.exportSelected") : t("claude.exportAll")}
              disabled={exporting || total === 0}
              onClick={() => void handleExport(selectedIds.length > 0 ? "selected" : "all")}
            >
              <Download className="size-3.5" />
              <span className="hidden md:inline">
                {exporting ? t("claude.exporting") : selectedIds.length > 0 ? t("claude.exportSelected") : t("claude.exportAll")}
              </span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              className={cn("size-8 p-0 sm:w-auto sm:px-2.5", showAnalysis && "border-primary/25 bg-primary/5 text-primary")}
              aria-label={showAnalysis ? t("accounts.hideAnalysisCharts") : t("accounts.showAnalysisCharts")}
              aria-pressed={showAnalysis}
              title={showAnalysis ? t("accounts.hideAnalysisCharts") : t("accounts.showAnalysisCharts")}
              onClick={() => setShowAnalysis((v) => !v)}
            >
              <BarChart3 className="size-3.5" />
              <span className="hidden sm:inline">
                {showAnalysis ? t("accounts.hideAnalysisCharts") : t("accounts.showAnalysisCharts")}
              </span>
            </Button>
            <HeaderActionMenu
              label={t("accounts.manageActions")}
              icon={<SlidersHorizontal className="size-3.5" />}
              align="end"
              sections={manageSections}
              compact
            />
            <Button
              variant="outline"
              size="icon-sm"
              title={t("common.refresh")}
              aria-label={t("common.refresh")}
              disabled={loading}
              onClick={() => { void reload(); void loadAnalysis(); }}
            >
              <RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
            </Button>
          </div>
        }
      />

      {loadError && accounts.length > 0 ? (
        <div className="mb-2 flex items-center justify-between gap-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive" role="alert">
          <span className="truncate">{loadError}</span>
          <Button variant="outline" size="sm" onClick={() => void reload()}>{t("common.retry")}</Button>
        </div>
      ) : null}
      {loading && accounts.length > 0 ? (
        <div className="mb-2 flex items-center justify-end gap-1.5 text-xs text-muted-foreground" role="status">
          <Loader2 className="size-3 animate-spin" />
          {t("common.loading")}
        </div>
      ) : null}

      {/* 统计卡(复用共享 CompactStat,与 Codex 同款:状态药丸 + 5h/7d·封禁/错误 details) */}
      <div className="claude-stat-strip mb-4 flex snap-x gap-2 overflow-x-auto pb-1 @4xl/claude-accounts:grid @4xl/claude-accounts:grid-cols-5 @4xl/claude-accounts:gap-3 [&>button]:w-40 [&>button]:shrink-0 [&>button]:snap-start @4xl/claude-accounts:[&>button]:w-full">
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
          onClick={() => setStatusFilter("normal")}
        />
        <CompactStat
          label={t("accounts.schedulingAccounts")}
          chipLabel={t("claude.statScheduling")}
          value={summary?.active ?? 0}
          tone="warning"
          active={statusFilter === "scheduling"}
          onClick={() => setStatusFilter("scheduling")}
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
          onClick={() => setStatusFilter("rate_limited")}
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
          onClick={() => setStatusFilter("abnormal")}
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
        <div className="mb-4 flex min-h-28 items-center justify-center rounded-xl border border-dashed border-border bg-muted/20 px-4 text-sm text-muted-foreground">
          {analysisError ? (
            <div className="flex flex-wrap items-center justify-center gap-3 text-center">
              <span>{analysisError}</span>
              <Button variant="outline" size="sm" onClick={() => void loadAnalysis()}>{t("common.retry")}</Button>
            </div>
          ) : (
            <span>{analysisLoading ? t("common.loading") : t("common.noData")}</span>
          )}
        </div>
      ) : null}

      {/* 搜索与状态共用一行;其余筛选按可用宽度排列,窄屏仍可按需展开。 */}
      <div className="toolbar-surface mb-3 flex flex-col gap-2 px-3 py-2.5">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
          <div className="relative min-w-0 flex-1 @4xl/claude-accounts:w-56 @4xl/claude-accounts:flex-none @6xl/claude-accounts:w-64">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground/70" />
            <Input
              className="h-8 rounded-lg bg-background/70 pl-9 pr-8 text-[13px] shadow-none"
              placeholder={t("claude.searchPlaceholder")}
              aria-label={t("claude.searchPlaceholder")}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            {search ? (
              <button type="button" className="absolute right-1.5 top-1/2 flex size-6 -translate-y-1/2 items-center justify-center rounded-md text-muted-foreground hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50" aria-label={t("claude.clearSearch")} onClick={() => setSearch("")}>
                <X className="size-3.5" />
              </button>
            ) : null}
          </div>
          <div className="order-last flex w-full min-w-0 items-center gap-0.5 overflow-x-auto @4xl/claude-accounts:order-none @4xl/claude-accounts:w-auto @4xl/claude-accounts:flex-1">
            {statChips.map((chip) => (
              <button
                key={chip.id}
                type="button"
                aria-pressed={statusFilter === chip.id}
                onClick={() => setStatusFilter(chip.id)}
                className={cn(
                  "inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-lg px-2 py-1 text-xs font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50",
                  statusFilter === chip.id
                    ? "bg-primary/10 text-primary"
                    : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
                )}
              >
                {chip.label}
                <span className={cn("min-w-4 rounded px-1 text-center text-[10px] tabular-nums", statusFilter === chip.id ? "bg-primary/10 text-primary" : "bg-muted/70 text-muted-foreground")}>
                  {chip.count}
                </span>
              </button>
            ))}
          </div>
          <Button
            variant="outline"
            size="sm"
            aria-expanded={showFilters}
            aria-controls="claude-account-filters"
            onClick={() => setShowFilters((current) => !current)}
            className={cn("h-8 @4xl/claude-accounts:hidden", (showFilters || filtersActive) && "border-primary/25 bg-primary/5 text-primary")}
          >
            <SlidersHorizontal className="size-3.5" />
            {t("accounts.filter")}
          </Button>
          <div className="ml-auto shrink-0">
            <ClaudeColumnSettingsMenu
              columns={visibleCols}
              onToggle={(column) => setVisibleCols((current) => ({ ...current, [column]: !current[column] }))}
              onReset={() => setVisibleCols(defaultClaudeCols())}
            />
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 border-t border-border/50 pt-2">
          <div className="flex min-h-8 min-w-0 max-w-full items-center gap-1 overflow-x-auto py-0.5">
            <span className="mr-1 shrink-0 text-[11px] font-medium text-muted-foreground">{t("claude.schedulingView")}</span>
            {healthChips.map((h) => (
              <ClaudeSchedulerChip key={h.id} label={h.label} value={h.count} tone={h.tone} active={healthTier === h.id} onClick={() => setHealthTier(healthTier === h.id ? null : h.id)} />
            ))}
          </div>

          <div id="claude-account-filters" className={cn("w-full flex-col gap-1.5 @4xl/claude-accounts:contents", showFilters ? "flex" : "hidden")}>
            <div className="flex max-w-full flex-wrap items-center gap-1.5 @4xl/claude-accounts:ml-1">
              {planTabs.length > 1 ? (
                <div className={segmentGroupClass}>
                  {planTabs.map((p) => (
                    <button key={p} type="button" aria-pressed={planFilter === p} onClick={() => setPlanFilter(p)} className={segmentButtonClass(planFilter === p)}>{planLabel(p)}</button>
                  ))}
                </div>
              ) : null}
              <div className={segmentGroupClass}>
                {authTabs.map((a) => (
                  <button key={a.id} type="button" aria-pressed={authFilter === a.id} onClick={() => setAuthFilter(a.id)} className={segmentButtonClass(authFilter === a.id)}>
                    {typeof a.count === "number" ? `${a.label} ${a.count}` : a.label}
                  </button>
                ))}
              </div>
            </div>

            <div className="grid grid-cols-2 gap-1.5 @4xl/claude-accounts:contents">
              <Select className="min-w-0 @4xl/claude-accounts:w-28" compact value={tagFilter} onValueChange={setTagFilter} options={[{ value: "all", label: t("accounts.tagsFilter") }, ...tags.map((tag) => ({ value: tag, label: tag }))]} />
              <Select
                className="min-w-0 @4xl/claude-accounts:w-36"
                compact
                value={domainFilter}
                onValueChange={setDomainFilter}
                options={[
                  { value: "all", label: t("accounts.emailDomainFilter") },
                  ...domains.map((d) => ({ value: d.domain, triggerLabel: d.domain, label: t("accounts.emailDomainFilterOption", { domain: d.domain, banned: d.banned, total: d.total }) })),
                ]}
              />
              <AccountGroupFilterSelect className="min-w-0 @4xl/claude-accounts:w-32" groups={claudeGroups} value={groupFilter} onChange={setGroupFilter} />
              <div className="flex min-w-0 items-center gap-1 @4xl/claude-accounts:w-36">
                <Select
                  className="min-w-0 flex-1"
                  compact
                  value={sortKey ?? "default"}
                  onValueChange={(value) => {
                    const next = value === "default" ? null : value as SortKey;
                    setSortKey(next);
                    setSortDir(next ? SORT_DEFAULT_DIR[next] : "desc");
                  }}
                  options={sortOptions}
                />
                {sortKey ? (
                  <Button variant="ghost" size="icon-sm" className="text-primary" title={sortDir === "desc" ? t("claude.sortAscending") : t("claude.sortDescending")} aria-label={sortDir === "desc" ? t("claude.sortAscending") : t("claude.sortDescending")} onClick={() => setSortDir((current) => current === "desc" ? "asc" : "desc")}>
                    {sortDir === "desc" ? <ArrowDown className="size-3.5" /> : <ArrowUp className="size-3.5" />}
                  </Button>
                ) : null}
              </div>
              <Button type="button" variant="ghost" size="sm" className="justify-start px-2 text-xs text-muted-foreground @4xl/claude-accounts:ml-auto" aria-pressed={!hideDomainTags} onClick={() => setHideDomainTags((v) => !v)}>
                {hideDomainTags ? <Eye className="size-3.5" /> : <EyeOff className="size-3.5" />}
                {hideDomainTags ? t("accounts.showEmailDomainTags") : t("accounts.hideEmailDomainTags")}
              </Button>
              <Button type="button" variant="ghost" size="sm" className="justify-start px-2 text-xs text-muted-foreground" onClick={() => setShowManageGroups(true)}>
                <FolderOpen className="size-3.5" />{t("accounts.groupManage")}
              </Button>
            </div>
          </div>
        </div>

        {filtersActive || sortKey !== null ? (
          <div className="flex flex-wrap items-center gap-1.5 border-t border-border/60 pt-2">
            {debouncedSearch ? (
              <button type="button" onClick={() => setSearch("")} className={filterPillClass} title={debouncedSearch}>
                <Search className="size-3 shrink-0" /><span className="max-w-40 truncate">{debouncedSearch}</span><X className="size-3 shrink-0" />
              </button>
            ) : null}
            {sortKey !== null ? (
              <button type="button" onClick={() => { setSortKey(null); setSortDir("desc"); }} className={filterPillClass} title={t("claude.sortDefault")}>
                {sortDir === "desc" ? <ArrowDown className="size-3" /> : <ArrowUp className="size-3" />}
                {sortOptions.find((option) => option.value === sortKey)?.label}<X className="size-3" />
              </button>
            ) : null}
            {statusFilter !== "all" ? (
              <button
                type="button"
                onClick={() => setStatusFilter("all")}
                className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2.5 py-1 text-[11px] font-medium text-primary transition-colors hover:bg-primary/15"
              >
                {statusLabelFor(statusFilter)}
                <X className="size-3" />
              </button>
            ) : null}
            {healthTier !== null ? (
              <button type="button" onClick={() => setHealthTier(null)} className={filterPillClass}>
                {healthChips.find((h) => h.id === healthTier)?.label ?? healthTier}
                <X className="size-3" />
              </button>
            ) : null}
            {planFilter !== "all" ? (
              <button type="button" onClick={() => setPlanFilter("all")} className={filterPillClass}>
                {planLabel(planFilter)}
                <X className="size-3" />
              </button>
            ) : null}
            {authFilter !== "all" ? (
              <button type="button" onClick={() => setAuthFilter("all")} className={filterPillClass}>
                {authTabs.find((a) => a.id === authFilter)?.label ?? authFilter}
                <X className="size-3" />
              </button>
            ) : null}
            {tagFilter !== "all" ? (
              <button type="button" onClick={() => setTagFilter("all")} className={filterPillClass}>
                {tagFilter}
                <X className="size-3" />
              </button>
            ) : null}
            {domainFilter !== "all" ? (
              <button type="button" onClick={() => setDomainFilter("all")} className={filterPillClass}>
                {domainFilter}
                <X className="size-3" />
              </button>
            ) : null}
            {!isAccountGroupFilterEmpty(groupFilter) ? (
              <button type="button" onClick={() => setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER)} className={filterPillClass}>
                {t("accounts.groupsLabel")}
                <X className="size-3" />
              </button>
            ) : null}
            <button
              type="button"
              onClick={clearFilters}
              className="ml-auto text-[11px] font-medium text-muted-foreground transition-colors hover:text-foreground"
            >
              {t("accounts.clearFilters")}
            </button>
          </div>
        ) : null}
      </div>

      {/* 批量操作条(与 Codex 同款 sticky 玻璃条) */}
      {selectedIds.length > 0 ? (
        <div className="sticky top-2 z-20 mb-4 flex items-center justify-between gap-3 rounded-xl border border-primary/20 bg-card/95 px-3 py-2 text-sm shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
          <span className="font-semibold text-primary">{t("common.selected", { count: selectedIds.length })}</span>
          <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
            {!allPageSelected ? (
              <Button variant="outline" size="sm" onClick={toggleSelectAll}>
                <ListChecks className="size-3.5" />
                <span>{t("accounts.selectCurrentPage")}</span>
              </Button>
            ) : null}
            <Button variant="outline" size="sm" disabled={exporting} onClick={() => void handleExport("selected")}>
              <Download className="size-3.5" />
              <span className="hidden sm:inline">{t("claude.exportSelected")}</span>
            </Button>
            <Button variant="outline" size="sm" onClick={() => void runBatch({ enabled: true })}>
              <Power className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.enable")}</span>
            </Button>
            <Button variant="outline" size="sm" onClick={() => void runBatch({ enabled: false })}>
              <PowerOff className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.disable")}</span>
            </Button>
            <HeaderActionMenu
              label={t("accounts.batchMore")}
              icon={<MoreHorizontal className="size-3.5" />}
              align="end"
              compact
              items={[
                { key: "lock", label: t("accounts.lock"), icon: <Lock className="size-3.5" />, onSelect: () => void runBatch({ locked: true }) },
                { key: "unlock", label: t("accounts.unlock"), icon: <Unlock className="size-3.5" />, onSelect: () => void runBatch({ locked: false }) },
                { key: "delete", label: t("accounts.batchDelete"), icon: <Trash2 className="size-3.5" />, destructive: true, onSelect: () => void handleBatchDelete() },
              ]}
            />
            <Button variant="ghost" size="sm" onClick={() => setSelected(new Set())}>
              {t("accounts.cancelSelection")}
            </Button>
          </div>
        </div>
      ) : null}

      {/* 账号列表(与 Codex 同款:Card > StateShell > data-table-shell > 共享 Table) */}
      <Card>
        <CardContent className="p-3 sm:p-4">
          <StateShell
            variant="section"
            loading={loading && accounts.length === 0}
            error={accounts.length === 0 ? loadError : null}
            onRetry={() => void reload()}
            isEmpty={accounts.length === 0}
            emptyIcon={<ChannelLogo channel="claude" size={30} />}
            emptyTitle={poolEmpty ? t("claude.emptyTitle") : t("claude.noMatchesTitle")}
            emptyDescription={poolEmpty ? t("claude.emptyDescription") : t("claude.noMatchesDescription")}
            action={
              poolEmpty ? (
                <Button onClick={() => openAdd("oauth")}>
                  <Plus className="size-4" />
                  {t("claude.addAccount")}
                </Button>
              ) : (
                <Button variant="outline" onClick={clearFilters}>
                  {t("claude.clearFilters")}
                </Button>
              )
            }
          >
            <div className={cn("data-table-shell claude-account-table", accounts.length <= pageSize && "account-table-shell-fit-content")}>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-10">
                      <input
                        type="checkbox"
                        className="size-4 cursor-pointer accent-primary"
                        checked={allPageSelected}
                        onChange={toggleSelectAll}
                        aria-label={t("accounts.selectAll")}
                      />
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("accounts.sequence")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("accounts.email")}</TableHead>
                    {visibleCols.tags ? <TableHead className="text-[13px] font-semibold">{t("accounts.tagsLabel")}</TableHead> : null}
                    {visibleCols.groups ? (
                      renderSortHead("group", t("accounts.groupsLabel"))
                    ) : null}
                    {visibleCols.proxy ? <TableHead className="text-[13px] font-semibold">{t("accounts.proxyColumn")}</TableHead> : null}
                    {visibleCols.priority ? (
                      renderSortHead("priority", t("accounts.schedulerPriorityColumn"))
                    ) : null}
                    {visibleCols.plan ? <TableHead className="text-[13px] font-semibold">{t("accounts.plan")}</TableHead> : null}
                    {visibleCols.status ? <TableHead className="text-[13px] font-semibold">{t("accounts.status")}</TableHead> : null}
                    {visibleCols.today ? (
                      renderSortHead("today", t("accounts.todayStats"), t("accounts.todayStatsHint"))
                    ) : null}
                    {visibleCols.requests ? (
                      renderSortHead("requests", t("accounts.requests"))
                    ) : null}
                    {visibleCols.usage ? (
                      renderSortHead("usage", t("accounts.usage"))
                    ) : null}
                    {visibleCols.cost ? <TableHead className="text-[13px] font-semibold">{t("accounts.billed")}</TableHead> : null}
                    {visibleCols.importTime ? (
                      renderSortHead("importTime", t("accounts.importTime"))
                    ) : null}
                    {visibleCols.updatedAt ? <TableHead className="text-[13px] font-semibold">{t("accounts.updatedAt")}</TableHead> : null}
                    <TableHead data-account-actions className="text-right text-xs font-medium">{t("accounts.actions")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {displayRows.map((acc, idx) => (
                    <ClaudeAccountRow
                      key={acc.id}
                      acc={acc}
                      no={(page - 1) * pageSize + idx + 1}
                      selected={selected.has(acc.id)}
                      detailOpen={detailTarget?.id === acc.id}
                      onToggleSelect={() => toggleSelect(acc.id)}
                      groupMap={groupMap}
                      healthBuckets={healthBars[String(acc.id)]}
                      showDomainTags={!hideDomainTags}
                      columns={visibleCols}
                      proxyCtx={proxyBindingCtx}
                      authJsonExporting={authJsonExportingIds.has(acc.id)}
                      onEditProxy={() => setQuickProxyAccount(acc)}
                      onRefresh={() => void handleRefresh(acc)}
                      onRefreshModels={() => void handleRefreshModels(acc)}
                      onExportOne={() => void handleExportOne(acc)}
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
                </TableBody>
              </Table>
            </div>
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
          </StateShell>
        </CardContent>
      </Card>

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
            <div className="space-y-1.5 rounded-xl border border-border bg-card p-3">
              <UsageWindow label={t("claude.usage5h")} pct={claudeUsagePct(detailTarget.usage_percent_5h)} reset={detailTarget.reset_5h_at} detail={detailTarget.usage_5h_detail} />
              <UsageWindow label={t("claude.usage7d")} pct={claudeUsagePct(detailTarget.usage_percent_7d)} reset={detailTarget.reset_7d_at} detail={detailTarget.usage_7d_detail} />
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
        <ClaudeConnectionTestModal
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

// ── 号池模式表格行(视觉对齐 Codex 表格行;数据取 Claude 真实链路) ──
// 整行可点开详情;点到按钮/输入/菜单等交互元素时不触发,与 Codex 行为一致。
const ROW_INTERACTIVE_SELECTOR =
  'button, a, input, label, [role="menuitem"], [role="menu"], [data-slot="button"], [data-slot="select-trigger"]';

function ClaudeAccountRow({
  acc,
  no,
  selected,
  detailOpen,
  onToggleSelect,
  groupMap,
  healthBuckets,
  showDomainTags,
  columns,
  proxyCtx,
  authJsonExporting,
  onEditProxy,
  onRefresh,
  onRefreshModels,
  onExportOne,
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
  detailOpen: boolean;
  onToggleSelect: () => void;
  groupMap: Map<number, AccountGroup>;
  healthBuckets?: AccountHealthBucket[];
  showDomainTags: boolean;
  columns: ClaudeColVisibility;
  proxyCtx: ProxyBindingContext;
  authJsonExporting: boolean;
  onEditProxy: () => void;
  onRefresh: () => void;
  onRefreshModels: () => void;
  onExportOne: () => void;
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
  const tableOverlayKind = resolveAccountOverlayKind(acc);
  const tableOverlay = renderAccountStateOverlay(acc, t, {
    compact: true,
    markerOnly: true,
    onRecover: onResetStatus,
  });
  const hasUsage =
    pct5h !== null ||
    pct7d !== null ||
    hasWindowDetail(acc.usage_5h_detail) ||
    hasWindowDetail(acc.usage_7d_detail) ||
    (acc.claude_usage_windows ?? []).some((window) => window.model_scoped);

  const rowMenuItems: HeaderActionMenuItem[] = [
    { key: "edit", label: t("claude.editTitle"), icon: <Pencil className="size-3.5" />, onSelect: onEdit },
    { key: "usage", label: t("accounts.usageDetail"), icon: <BarChart3 className="size-3.5" />, onSelect: onUsage },
    { key: "test", label: t("accounts.testConnection"), icon: <Zap className="size-3.5" />, onSelect: onTest },
    {
      key: "refresh",
      label: t("accounts.refreshAccessToken"),
      icon: <RefreshCw className="size-3.5" />,
      onSelect: onRefresh,
    },
    {
      key: "export",
      label: t("claude.exportCredential"),
      icon: authJsonExporting ? <Loader2 className="size-3.5 animate-spin" /> : <FileJson className="size-3.5" />,
      disabled: authJsonExporting,
      onSelect: onExportOne,
    },
    {
      key: "toggle-enabled",
      label: disabled ? t("accounts.actionEnableScheduling") : t("accounts.actionDisableScheduling"),
      icon: disabled ? <Power className="size-3.5" /> : <PowerOff className="size-3.5" />,
      onSelect: onToggleEnabled,
    },
    {
      key: "toggle-lock",
      label: acc.locked ? t("accounts.actionUnlockAccount") : t("accounts.actionLockAccount"),
      icon: acc.locked ? <Unlock className="size-3.5" /> : <Lock className="size-3.5" />,
      onSelect: onToggleLock,
    },
    {
      key: "reset-status",
      label: acc.status === "overload_paused" ? t("accounts.overloadRecover") : t("accounts.resetStatus"),
      icon: <RotateCcw className="size-3.5" />,
      onSelect: onResetStatus,
    },
    {
      key: "edit-models",
      label: t("claude.modelsWhitelistAction"),
      icon: <SlidersHorizontal className="size-3.5" />,
      onSelect: onEditModels,
    },
    {
      key: "refresh-models",
      label: t("claude.refreshModels"),
      icon: <RefreshCw className="size-3.5" />,
      onSelect: onRefreshModels,
    },
    { key: "delete", label: t("accounts.deleteAccount"), icon: <Trash2 className="size-3.5" />, destructive: true, onSelect: onDelete },
  ];

  return (
    <TableRow
      data-state={selected ? "selected" : undefined}
      className={cn(
        "cursor-pointer",
        detailOpen ? "bg-primary/8" : selected ? "bg-primary/5" : "",
        accountStateTableRowClass(acc),
      )}
      onClick={(event) => {
        const target = event.target as HTMLElement | null;
        if (target?.closest(ROW_INTERACTIVE_SELECTOR)) return;
        onOpenDetail();
      }}
    >
      {/* 勾选 */}
      <TableCell>
        <div className="flex items-center gap-1">
          <input
            type="checkbox"
            className="size-4 cursor-pointer accent-primary"
            checked={selected}
            onChange={onToggleSelect}
            onClick={(event) => event.stopPropagation()}
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
      </TableCell>
      {/* 序号 */}
      <TableCell className="text-[14px] font-mono text-muted-foreground" title={`ID ${acc.id}`}>
        {no}
      </TableCell>
      {/* 邮箱 */}
      <TableCell className="min-w-[220px] whitespace-normal text-[14px] text-muted-foreground">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="flex size-8 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-card ring-1 ring-border shadow-sm">
            <ChannelLogo channel="claude" size={32} className="rounded-lg" />
          </span>
          <div className="flex min-w-0 flex-col items-start gap-1">
            <button
              type="button"
              className="break-all text-left font-medium text-foreground transition-colors hover:text-primary"
              title={t("accounts.openDetail")}
              onClick={(event) => {
                event.stopPropagation();
                onOpenDetail();
              }}
            >
              {acc.email || acc.name || `#${acc.id}`}
            </button>
            {showDomainTags && acc.email_domain ? (
              <span
                className="inline-flex max-w-full items-center break-all rounded-md bg-muted px-1.5 py-0.5 text-left text-[10px] font-medium leading-tight text-muted-foreground ring-1 ring-inset ring-border/80"
                title={`${t("accounts.emailDomainSystemTag")}: @${acc.email_domain}`}
              >
                @{acc.email_domain}
              </span>
            ) : null}
            {acc.locked || acc.models?.length || acc.last_used_at ? (
              <div className="flex flex-wrap items-center gap-1">
                {acc.locked ? (
                  <span className="inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                    <Lock className="mr-0.5 size-2.5" />
                    {t("accounts.lock")}
                  </span>
                ) : null}
                {acc.models?.length ? (
                  <button
                    type="button"
                    onClick={(event) => {
                      event.stopPropagation();
                      onEditModels();
                    }}
                    className="inline-flex items-center rounded-md bg-orange-50 px-1.5 py-0.5 text-[10px] font-medium text-orange-700 ring-1 ring-inset ring-orange-600/20 transition-colors hover:bg-orange-100 dark:bg-orange-950 dark:text-orange-300 dark:ring-orange-400/20 dark:hover:bg-orange-900"
                    title={t("claude.modelsWhitelistAction")}
                  >
                    {t("claude.modelCount", { count: acc.models.length })}
                  </button>
                ) : null}
                {acc.last_used_at ? (
                  <span className="text-[10px] text-muted-foreground/70" title={formatShortDateTime(acc.last_used_at)?.title}>
                    {t("claude.lastUsed")}: {formatRelativeShort(acc.last_used_at, t)}
                  </span>
                ) : null}
              </div>
            ) : null}
          </div>
        </div>
      </TableCell>
      {columns.tags ? (
        <TableCell className="min-w-[120px]">
          <ClaudeChipList items={acc.tags ?? []} />
        </TableCell>
      ) : null}
      {columns.groups ? (
        <TableCell className="min-w-[140px]">
          <ClaudeGroupChipList groups={accGroups} onClick={onAssignGroups} emptyLabel={t("accounts.groupQuickEdit")} />
        </TableCell>
      ) : null}
      {columns.proxy ? (
        <TableCell className="min-w-[120px] max-w-[180px]">
          <AccountProxyBadge account={acc} ctx={proxyCtx} onClick={onEditProxy} />
        </TableCell>
      ) : null}
      {columns.priority ? (
        <TableCell>
          <ClaudePriorityBadge acc={acc} />
        </TableCell>
      ) : null}
      {columns.plan ? (
        <TableCell>
          <div className="flex flex-wrap items-center gap-1.5">
            {acc.plan_type ? (
              (() => {
                const badge = claudePlanBadge(acc.plan_type);
                return (
                  <span className={cn("inline-flex min-w-0 max-w-full items-center truncate rounded-md px-2.5 py-1 text-[13px] font-semibold ring-1 ring-inset", badge.tone)}>
                    {badge.label}
                  </span>
                );
              })()
            ) : (
              <span className="text-[12px] text-muted-foreground">-</span>
            )}
            <ClaudeExpiryBadge expiresAt={acc.subscription_expires_at} planType={acc.plan_type} />
          </div>
        </TableCell>
      ) : null}
      {columns.status ? (
        <TableCell data-account-state-cell="status">
          {tableOverlay ?? (
            <div className="min-w-[168px] max-w-[240px] space-y-1.5">
              <div className="flex min-h-6 flex-wrap items-center gap-1.5">
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
                <div className="truncate text-[11px] text-muted-foreground" title={acc.claude_usage_probe_error || undefined}>
                  {t("claude.lastSample")}: {acc.claude_usage_probe_at ? formatRelativeShort(acc.claude_usage_probe_at, t) : t("claude.samplingState.notSampled")}
                  {acc.claude_usage_probe_error ? ` · ${acc.claude_usage_probe_error}` : ""}
                </div>
              ) : null}
              <AccountHealthBar buckets={healthBuckets} />
            </div>
          )}
        </TableCell>
      ) : null}
      {columns.today ? (
        <TableCell>
          <ClaudeTodayStatsCell acc={acc} />
        </TableCell>
      ) : null}
      {columns.requests ? (
        <TableCell>
          <RequestCountPills account={acc} compact />
        </TableCell>
      ) : null}
      {columns.usage ? (
        <TableCell>
          {hasUsage ? (
            <div className="flex w-56 items-start gap-1">
              <div className="w-[188px] space-y-1.5">
                <UsageWindow label={t("claude.usage5h")} pct={pct5h} reset={acc.reset_5h_at} detail={acc.usage_5h_detail} />
                <UsageWindow label={t("claude.usage7d")} pct={pct7d} reset={acc.reset_7d_at} detail={acc.usage_7d_detail} />
                <ClaudeScopedUsageWindows windows={acc.claude_usage_windows} />
              </div>
              <UsageRefreshButton onRefresh={onUsageRefreshed} title={t("accounts.refreshUsage")} />
            </div>
          ) : (
            <div className="flex items-center gap-1">
              <span className="text-[13px] text-muted-foreground">-</span>
              <UsageRefreshButton onRefresh={onUsageRefreshed} title={t("accounts.refreshUsage")} />
            </div>
          )}
        </TableCell>
      ) : null}
      {columns.cost ? (
        <TableCell className="whitespace-nowrap text-[13px] text-muted-foreground">
          <ClaudeBilledCell acc={acc} />
        </TableCell>
      ) : null}
      {columns.importTime ? (
        <TableCell className="whitespace-nowrap text-[13px] text-muted-foreground">
          {formatBeijingTime(acc.created_at)}
        </TableCell>
      ) : null}
      {columns.updatedAt ? (
        <TableCell className="whitespace-nowrap text-[13px] text-muted-foreground">
          {formatRelativeTime(acc.updated_at)}
        </TableCell>
      ) : null}
      {/* 操作 */}
      <TableCell data-account-actions className="text-right">
        <div className="flex items-center justify-end gap-0.5">
          <div className="hidden items-center gap-0.5 @4xl/claude-accounts:flex">
          <Button variant="ghost" size="icon-sm" className="size-8" onClick={onEdit} title={t("claude.editTitle")} aria-label={t("claude.editTitle")}>
            <Pencil className="size-3.5" />
          </Button>
          <Button variant="ghost" size="icon-sm" className="size-8" onClick={onUsage} title={t("accounts.usageDetail")} aria-label={t("accounts.usageDetail")}>
            <BarChart3 className="size-3.5" />
          </Button>
          <Button variant="ghost" size="icon-sm" className="size-8" onClick={onTest} title={t("accounts.testConnection")} aria-label={t("accounts.testConnection")}>
            <Zap className="size-3.5" />
          </Button>
          </div>
          <HeaderActionMenu
            label={t("accounts.rowActions")}
            icon={<MoreHorizontal className="size-3.5" />}
            align="end"
            compact
            items={rowMenuItems}
            triggerVariant="ghost"
          />
        </div>
      </TableCell>
    </TableRow>
  );
}

// ClaudeSchedulerChip 调度视图药丸(与 Codex SchedulerChip 同款外观);Claude 页保留点击按健康档过滤。
function ClaudeSchedulerChip({
  label,
  value,
  tone,
  active,
  onClick,
}: {
  label: string;
  value: number;
  tone: "neutral" | "success" | "warning" | "danger";
  active: boolean;
  onClick: () => void;
}) {
  const toneStyle = {
    neutral: "bg-muted text-muted-foreground",
    success: "bg-emerald-500/10 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300",
    warning: "bg-amber-500/10 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300",
    danger: "bg-red-500/10 text-red-700 dark:bg-red-500/15 dark:text-red-300",
  }[tone];
  const dotStyle = {
    neutral: "bg-slate-400",
    success: "bg-emerald-500",
    warning: "bg-amber-500",
    danger: "bg-red-500",
  }[tone];
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        "inline-flex h-6 shrink-0 items-center gap-1.5 rounded-full px-2 text-[11px] font-medium transition-shadow",
        toneStyle,
        active ? "ring-2 ring-primary/40" : "hover:ring-1 hover:ring-border",
      )}
    >
      <span className={cn("size-1.5 rounded-full", dotStyle)} />
      <span>{label}</span>
      <span className="tabular-nums">{value}</span>
    </button>
  );
}

// ClaudePriorityBadge 调度优先级徽章(与 Codex SchedulerPriorityBadge 同款:正数蓝/负数琥珀/零灰)。
function ClaudePriorityBadge({ acc }: { acc: AccountRow }) {
  const { t } = useTranslation();
  const raw = acc.scheduler_priority;
  const priority = typeof raw === "number" && Number.isFinite(raw) ? Math.trunc(raw) : 0;
  const value = priority > 0 ? `+${priority}` : String(priority);
  const tone =
    priority > 0
      ? "border-blue-500/25 bg-blue-500/10 text-blue-700 dark:text-blue-300"
      : priority < 0
        ? "border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300"
        : "border-border bg-muted/40 text-muted-foreground";
  return (
    <span
      className={cn("inline-flex shrink-0 items-center rounded-md border px-1.5 py-0.5 text-[10px] font-semibold tabular-nums", tone)}
      title={t("accounts.schedulerPriorityBadgeTitle", { value })}
    >
      P {value}
    </span>
  );
}

// ClaudeExpiryBadge 订阅到期提醒(与 Codex ExpiryBadge 同款阈值:≤3d 红、≤7d 琥珀、已过期灰)。
function ClaudeExpiryBadge({ expiresAt, planType }: { expiresAt?: string; planType?: string }) {
  const { t, i18n } = useTranslation();
  if (!expiresAt) return null;
  const plan = (planType || "").toLowerCase().trim();
  if (plan === "" || plan === "free") return null;
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
        {days === 0 ? t("accounts.subscriptionExpiresToday") : t("accounts.subscriptionExpiresDays", { days })}
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

// ClaudeChipList 标签芯片(与 Codex ChipList 同款:最多 3 个 + N,弱化配色不抢状态色)。
function ClaudeChipList({ items }: { items: string[] }) {
  if (items.length === 0) return null;
  const visible = items.slice(0, 3);
  const hidden = items.length - visible.length;
  return (
    <div className="flex flex-wrap gap-1">
      {visible.map((item) => (
        <span key={item} className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground ring-1 ring-inset ring-border/80">
          {item}
        </span>
      ))}
      {hidden > 0 ? (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">+{hidden}</span>
      ) : null}
    </div>
  );
}

// ClaudeGroupChipList 分组芯片(与 Codex GroupChipList 同款:最多 3 个 + N,悬停出铅笔,空态虚线快捷入口)。
function ClaudeGroupChipList({
  groups,
  onClick,
  emptyLabel,
}: {
  groups: AccountGroup[];
  onClick: () => void;
  emptyLabel: string;
}) {
  const visible = groups.slice(0, 3);
  const hidden = groups.length - visible.length;
  return (
    <button type="button" className="group flex flex-wrap items-center gap-1 text-left" onClick={onClick} title={emptyLabel}>
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
            style={{ backgroundColor: `${color}14`, color, boxShadow: `inset 0 0 0 1px ${color}33` }}
            title={group.description || group.name}
          >
            <span className="size-1.5 rounded-full bg-current" />
            {group.name}
          </span>
        );
      })}
      {hidden > 0 ? (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">+{hidden}</span>
      ) : null}
      {groups.length > 0 ? (
        <Pencil className="mt-0.5 size-3 text-muted-foreground opacity-60 transition-opacity group-hover:opacity-100" />
      ) : null}
    </button>
  );
}

// ClaudeTodayStatsCell 今日统计(与 Codex TodayStatsCell 同款:req/tok 两列 + 成本胶囊)。
function ClaudeTodayStatsCell({ acc }: { acc: AccountRow }) {
  const { t } = useTranslation();
  const detail = acc.usage_today_detail;
  if (!detail) return <span className="text-[12px] font-mono text-muted-foreground/40">-</span>;
  const requests = detail.requests ?? 0;
  const tokens = detail.tokens ?? 0;
  const billed = typeof detail.account_billed === "number" ? detail.account_billed : 0;
  const tooltip = [
    `${t("accounts.todayStats")}:`,
    `Requests: ${requests.toLocaleString()} req`,
    `Tokens: ${tokens.toLocaleString()} tok`,
    `${t("accounts.accountBilledLabel")}: $${billed.toFixed(4)}`,
  ].join("\n");
  return (
    <div className="flex flex-col items-start gap-1 whitespace-nowrap text-[12px] tabular-nums" title={tooltip}>
      <div className="flex items-center gap-2">
        <span className={cn("inline-flex items-center gap-1", requests > 0 ? "font-semibold text-foreground" : "font-normal text-muted-foreground/50")}>
          <Activity className={cn("size-3 shrink-0", requests > 0 ? "text-sky-500" : "text-muted-foreground/40")} aria-hidden />
          <span>{requests > 0 ? requests.toLocaleString() : 0}</span>
          <span className="text-[11px] font-normal text-muted-foreground/60">{t("accounts.usageReqUnit")}</span>
        </span>
        <span className={cn("inline-flex items-center gap-1", tokens > 0 ? "font-semibold text-foreground" : "font-normal text-muted-foreground/50")}>
          <Sparkles className={cn("size-3 shrink-0", tokens > 0 ? "text-purple-500 dark:text-purple-400" : "text-muted-foreground/40")} aria-hidden />
          <span>{formatCompactNum(tokens)}</span>
          <span className="text-[11px] font-normal text-muted-foreground/60">{t("accounts.usageTokUnit")}</span>
        </span>
      </div>
      <div className="flex items-center gap-1.5 text-[11px]">
        {billed > 0 ? (
          <span
            className="inline-flex items-center gap-1 rounded-md bg-emerald-500/10 px-1.5 py-0.5 font-mono text-[11px] font-medium tabular-nums text-emerald-700 ring-1 ring-inset ring-emerald-500/20 dark:text-emerald-400"
            title={t("accounts.accountBilledLabel")}
          >
            <Coins className="size-3 shrink-0 text-emerald-500" aria-hidden />
            ${billed < 0.01 ? "<0.01" : billed.toFixed(2)}
          </span>
        ) : (
          <span
            className="inline-flex items-center gap-1 rounded-md bg-slate-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-slate-500 ring-1 ring-inset ring-slate-500/20 dark:text-slate-400"
            title={t("accounts.accountBilledLabel")}
          >
            <Coins className="size-3 shrink-0 opacity-50" aria-hidden />
            $0.00
          </span>
        )}
      </div>
    </div>
  );
}

// ClaudeBilledCell 成本列(与 Codex BilledCell 的网关口径胶囊同款:5h / 7d 账号侧计费)。
function ClaudeBilledCell({ acc }: { acc: AccountRow }) {
  const { t } = useTranslation();
  const h5 = typeof acc.usage_5h_detail?.account_billed === "number" ? acc.usage_5h_detail.account_billed.toFixed(2) : null;
  const d7 = typeof acc.usage_7d_detail?.account_billed === "number" ? acc.usage_7d_detail.account_billed.toFixed(2) : null;
  if (h5 === null && d7 === null) return <span className="text-[12px] text-muted-foreground">-</span>;
  return (
    <span
      className="inline-flex items-center gap-1 whitespace-nowrap rounded-md bg-slate-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-slate-700 ring-1 ring-inset ring-slate-500/20 dark:text-slate-300"
      title={t("accounts.billedGatewayHint")}
    >
      <Wallet className="size-3 shrink-0" aria-hidden />
      {h5 !== null ? `5h: $${h5}` : null}
      {h5 !== null && d7 !== null ? " / " : null}
      {d7 !== null ? `7d: $${d7}` : null}
    </span>
  );
}

function ClaudeColumnSettingsMenu({
  columns,
  onToggle,
  onReset,
}: {
  columns: ClaudeColVisibility;
  onToggle: (column: ClaudeCol) => void;
  onReset: () => void;
}) {
  const { t } = useTranslation();
  const labelFor: Record<ClaudeCol, string> = {
    tags: t("accounts.tagsLabel"),
    groups: t("accounts.groupsLabel"),
    proxy: t("accounts.proxyColumn"),
    priority: t("accounts.schedulerPriorityColumn"),
    plan: t("accounts.plan"),
    status: t("accounts.status"),
    today: t("accounts.todayStats"),
    requests: t("accounts.requests"),
    usage: t("accounts.usage"),
    cost: t("accounts.billed"),
    importTime: t("accounts.importTime"),
    updatedAt: t("accounts.updatedAt"),
  };
  return (
    <ColumnSettingsMenu
      columns={columns}
      columnOrder={CLAUDE_TOGGLE_COLUMNS}
      labels={labelFor}
      onToggle={onToggle}
      onReset={onReset}
      title={t("accounts.columnSettings")}
      resetTitle={t("accounts.columnReset")}
    />
  );
}

// UsageRefreshButton 用量刷新按钮:点击时旋转动画,请求完成后停止(与 Codex 用量列的刷新按钮一致)。
function UsageRefreshButton({ onRefresh, title }: { onRefresh: () => void | Promise<void>; title: string }) {
  const [spinning, setSpinning] = useState(false);
  return (
    <button
      type="button"
      title={title}
      aria-label={title}
      disabled={spinning}
      onClick={async (event) => {
        event.stopPropagation();
        setSpinning(true);
        try {
          await onRefresh();
        } finally {
          setSpinning(false);
        }
      }}
      className="shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
    >
      <RefreshCw className={cn("size-3", spinning && "animate-spin")} />
    </button>
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
