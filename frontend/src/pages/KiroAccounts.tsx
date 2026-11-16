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
  LogIn,
  KeyRound,
  Plus,
} from "lucide-react";

import { api } from "../api";
import type { NamedBlob, ProxyRow } from "../api";
import type {
  APIKeyRow,
  AccountRow,
  AccountGroup,
  AccountListSummary,
  AccountEmailDomainFacet,
  AccountPageStatsItem,
  AccountHealthBucket,
  AccountsPageParams,
  KiroDeviceLoginStart,
  KiroUsageSnapshot,
} from "../types";
import { useIntelligenceBars } from "../hooks/useIntelligenceBars";
import AccountIntelligenceBar, { AccountIntelligenceActions } from "../components/AccountIntelligenceBar";
import type { IntelligenceBar } from "../lib/intelligenceCheck";
import AccountUsageBar from "../components/AccountUsageBar";
import AccountUsageModal from "../components/AccountUsageModal";
import AccountDetailSheet from "../components/AccountDetailSheet";
import AccountHealthBar from "../components/AccountHealthBar";
import RequestCountPills from "../components/RequestCountPills";
import ColumnSettingsMenu from "../components/ColumnSettingsMenu";
import { CompactStat } from "../components/CompactStat";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import BatchAccountGroupModal from "../components/BatchAccountGroupModal";
import APIKeyMultiSelect from "../components/APIKeyMultiSelect";
import StateShell from "../components/StateShell";
import { ProxyField } from "../components/ProxyField";
import AccountProxyBadge from "../components/AccountProxyBadge";
import AccountProxyQuickEditor from "../components/AccountProxyQuickEditor";
import { buildProxyBindingContext, type ProxyBindingContext } from "../lib/accountProxyBinding";
import ChipInput from "../components/ChipInput";
import { AccountGroupManagerModal, ACCOUNT_GROUP_COLORS } from "../components/AccountGroupManagerModal";
import { Select } from "../components/ui/select";
import ChannelLogo from "../components/ChannelLogo";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import StatusBadge from "../components/StatusBadge";
import Pagination from "../components/Pagination";
import TestConnectionModal from "../components/TestConnectionModal";
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
import { HeaderActionMenu, type HeaderActionMenuItem, type HeaderActionMenuSection } from "../components/HeaderActionMenu";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import { accountStateTableRowClass, resolveAccountOverlayKind, renderAccountStateOverlay } from "../components/AccountStateOverlay";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { getErrorMessage } from "../utils/error";
import { getAccountStatusBadgeStatus } from "../lib/usageFormat";
import { scheduleAccountStatsPoll } from "../lib/accountStatsPoll";

// Kiro 账号页：表格、筛选、批量操作、详情、编辑配置与 Codex / Claude 账号页同构；
// Kiro 专属的是导入（kiro.rs / Kiro IDE / Kiro Account Manager 三种 JSON）、Social 与设备码登录、
// credit 额度（Kiro 按积分计费，没有 5h/7d 窗口，积分用完即不可用、到重置日恢复）和 KAM / kiro.rs 导出。

const TEXTAREA_CLASS =
  "flex min-h-[160px] w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50";

const FALLBACK_GROUP_COLOR = "#2563eb";
function normalizeGroupColor(color?: string): string {
  const v = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(v) ? v : FALLBACK_GROUP_COLOR;
}

function downloadNamedBlob(payload: NamedBlob, fallbackName: string): void {
  const objectURL = URL.createObjectURL(payload.blob);
  const anchor = document.createElement("a");
  anchor.href = objectURL;
  anchor.download = payload.filename || fallbackName;
  anchor.rel = "noopener";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
}

function accountLabel(account: AccountRow): string {
  return account.email || account.name || `#${account.id}`;
}

function percent(used: number, limit: number): number | null {
  return limit > 0 ? (used / limit) * 100 : null;
}

function formatCredits(value: number): string {
  return Number.isInteger(value) ? String(value) : value.toFixed(2);
}

function formatCompactNum(v: unknown): string {
  const n = typeof v === "number" ? v : Number(v);
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(Math.round(n));
}

function formatRelativeShort(iso: string | undefined, t: (k: string) => string): string {
  if (!iso) return "-";
  const ts = new Date(iso).getTime();
  if (!Number.isFinite(ts)) return "-";
  const diff = Math.max(0, Date.now() - ts);
  const m = Math.floor(diff / 60000);
  if (m < 1) return t("kiro.justNow");
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h${m % 60}m`;
  return `${Math.floor(h / 24)}d${h % 24}h`;
}

// kiroPlanBadge 套餐徽章：按后端归一的 kiro_plan 配色，认不出时显示订阅标题原文。
const KIRO_PLAN_LABELS: Record<string, string> = {
  "kiro free": "Free",
  "kiro pro": "Pro",
  "kiro pro+": "Pro+",
  "kiro max": "Max",
  "kiro power": "Power",
  "kiro student": "Student",
};
const KIRO_PLAN_TONES: Record<string, string> = {
  "kiro free": "bg-zinc-100 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-400 dark:ring-zinc-500/20",
  "kiro pro": "bg-purple-50 text-purple-700 ring-purple-600/20 dark:bg-purple-950 dark:text-purple-300 dark:ring-purple-400/20",
  "kiro pro+": "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20",
  "kiro max": "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20",
  "kiro power": "bg-fuchsia-50 text-fuchsia-700 ring-fuchsia-600/20 dark:bg-fuchsia-950 dark:text-fuchsia-300 dark:ring-fuchsia-400/20",
  "kiro student": "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20",
};
function kiroPlanBadge(plan: string | undefined, title: string | undefined): { label: string; tone: string } | null {
  const key = (plan || "").trim().toLowerCase();
  if (KIRO_PLAN_LABELS[key]) return { label: KIRO_PLAN_LABELS[key], tone: KIRO_PLAN_TONES[key] };
  const raw = (title || "").replace(/^kiro\s+/i, "").trim();
  return raw ? { label: raw, tone: KIRO_PLAN_TONES["kiro free"] } : null;
}

const KIRO_AUTH_KINDS = ["social", "idc", "external_idp", "api_key"] as const;
type KiroAuthKind = (typeof KIRO_AUTH_KINDS)[number];

// kiroAuthLabel 凭据形态：Social 显示登录来源（Google / GitHub），IdC 区分 Builder ID 与 Enterprise。
function kiroAuthLabel(acc: AccountRow, t: (k: string) => string): string {
  const source = (acc.kiro_login_source || "").trim();
  switch (acc.kiro_auth_kind) {
    case "social":
      return source || t("kiro.authKind.social");
    case "idc":
      return source.toLowerCase() === "enterprise" ? "Enterprise" : "Builder ID";
    case "external_idp":
      return t("kiro.authKind.external_idp");
    case "api_key":
      return t("kiro.authKind.api_key");
    default:
      return source || "-";
  }
}

type KiroStatusFilter = "all" | "normal" | "scheduling" | "rate_limited" | "abnormal" | "banned" | "error" | "disabled" | "locked";
type HealthTier = "healthy" | "warm" | "risky" | "banned";

// 排序键：Kiro 没有 5h/7d 用量窗口，不提供按用量百分比排序。
type SortKey = "group" | "priority" | "requests" | "today" | "importTime";
type SortDir = "asc" | "desc";
const SORT_FIELD: Record<SortKey, NonNullable<AccountsPageParams["sort"]>> = {
  group: "group",
  priority: "scheduler_priority",
  requests: "requests",
  today: "today",
  importTime: "created_at",
};
const SORT_DEFAULT_DIR: Record<SortKey, SortDir> = {
  group: "asc",
  priority: "desc",
  requests: "desc",
  today: "desc",
  importTime: "desc",
};

const KIRO_TOGGLE_COLUMNS = [
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
type KiroCol = (typeof KIRO_TOGGLE_COLUMNS)[number];
type KiroColVisibility = Record<KiroCol, boolean>;
const KIRO_COLS_KEY = "codex2api:kiro-accounts:visible-columns";

function defaultKiroCols(): KiroColVisibility {
  return Object.fromEntries(KIRO_TOGGLE_COLUMNS.map((c) => [c, c !== "tags"])) as KiroColVisibility;
}

function loadKiroCols(): KiroColVisibility {
  const fallback = defaultKiroCols();
  try {
    const raw = window.localStorage.getItem(KIRO_COLS_KEY);
    if (!raw) return fallback;
    const parsed = JSON.parse(raw) as Partial<KiroColVisibility>;
    return Object.fromEntries(
      KIRO_TOGGLE_COLUMNS.map((c) => [c, typeof parsed[c] === "boolean" ? parsed[c] : fallback[c]]),
    ) as KiroColVisibility;
  } catch {
    return fallback;
  }
}

// KiroQuota credit 额度：基础额度按月重置，赠送 / 试用额度到期作废。
function KiroQuota({ usage }: { usage?: KiroUsageSnapshot }) {
  const { t } = useTranslation();
  if (!usage || !usage.fetched_at || usage.limit <= 0) {
    return <span className="text-xs text-muted-foreground">{t("kiro.quotaUnknown")}</span>;
  }
  const extraUsed = usage.bonus_used + usage.trial_used;
  const extraLimit = usage.bonus_limit + usage.trial_limit;
  return (
    <div className="space-y-1">
      <AccountUsageBar
        label={t("kiro.quotaBase")}
        pct={percent(usage.base_used, usage.base_limit)}
        resetAt={usage.next_reset_at}
        detailText={`${formatCredits(usage.base_used)} / ${formatCredits(usage.base_limit)}`}
      />
      {extraLimit > 0 ? (
        <AccountUsageBar
          label={t("kiro.quotaBonus")}
          pct={percent(extraUsed, extraLimit)}
          resetAt={usage.credits_expire_at}
          detailText={`${formatCredits(extraUsed)} / ${formatCredits(extraLimit)}`}
        />
      ) : null}
    </div>
  );
}

// LiveCountdown 冷却剩余时间，每秒刷新（与 Codex CooldownTimer 同款琥珀沙漏胶囊）。
function LiveCountdown({ until, label }: { until?: string; label: string }) {
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

function KiroConcurrencyBadge({ acc }: { acc: AccountRow }) {
  const { t } = useTranslation();
  const active = Math.max(0, acc.active_requests ?? 0);
  const occupied = Math.max(active, acc.occupied_requests ?? active);
  if (occupied === 0) return null;
  const buffered = occupied - active;
  const showOccupied = acc.session_slot_buffer_enabled === true;
  return (
    <span
      className="inline-flex shrink-0 items-center gap-1 whitespace-nowrap rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium tabular-nums text-blue-700 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-950 dark:text-blue-300"
      title={showOccupied
        ? t("accounts.occupiedRequestsTooltip", { active, occupied, buffered })
        : t("accounts.activeRequestsTooltip", { count: active })}
    >
      <span className="size-1.5 animate-pulse rounded-full bg-blue-500" aria-hidden />
      {showOccupied ? `${active}/${occupied}` : active}
    </span>
  );
}

function ImportModal({ show, onClose, onDone }: { show: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [content, setContent] = useState("");
  const [proxyURL, setProxyURL] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const readFile = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (file) setContent(await file.text());
  };

  const submit = async () => {
    setSubmitting(true);
    try {
      const result = await api.importKiroAccounts({ content, proxy_url: proxyURL.trim() || undefined });
      const firstError = result.results.find((item) => item.error)?.error;
      showToast(
        t("kiro.importResult", { imported: result.imported, failed: result.failed }) + (firstError ? ` · ${firstError}` : ""),
        result.failed > 0 ? "error" : "success",
      );
      if (result.imported > 0) {
        setContent("");
        onDone();
        onClose();
      }
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("kiro.importTitle")}
      onClose={onClose}
      contentClassName="sm:max-w-[640px]"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void submit()} disabled={submitting || !content.trim()}>
            {submitting ? <RefreshCw className="size-3.5 animate-spin" /> : <Upload className="size-3.5" />}
            {t("kiro.importSubmit")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p className="text-xs text-muted-foreground">{t("kiro.importHint")}</p>
        <textarea
          value={content}
          onChange={(event) => setContent(event.target.value)}
          placeholder={t("kiro.importPlaceholder")}
          className={TEXTAREA_CLASS}
          spellCheck={false}
        />
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => fileRef.current?.click()}>
            <Upload className="size-3.5" />
            {t("kiro.importFile")}
          </Button>
          <input ref={fileRef} type="file" accept=".json,application/json" className="hidden" onChange={(event) => void readFile(event)} />
        </div>
        <label className="block space-y-1 text-xs font-medium text-muted-foreground">
          <span>{t("kiro.proxyLabel")}</span>
          <Input value={proxyURL} onChange={(event) => setProxyURL(event.target.value)} placeholder="socks5://host:port" />
        </label>
      </div>
    </Modal>
  );
}

function SocialLoginModal({ show, onClose, onDone }: { show: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [session, setSession] = useState<{ auth_url: string; session: string } | null>(null);
  const [callbackURL, setCallbackURL] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!show) {
      setSession(null);
      setCallbackURL("");
    }
  }, [show]);

  const start = async () => {
    setBusy(true);
    try {
      const next = await api.startKiroSocialLogin({});
      setSession(next);
      window.open(next.auth_url, "_blank", "noopener");
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  };

  const finish = async () => {
    if (!session) return;
    setBusy(true);
    try {
      const created = await api.finishKiroSocialLogin({ session: session.session, callback_url: callbackURL.trim() });
      showToast(t("kiro.loginDone", { account: created.email || `#${created.id}` }), "success");
      onDone();
      onClose();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("kiro.socialTitle")}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>{t("common.cancel")}</Button>
          {session ? (
            <Button onClick={() => void finish()} disabled={busy || !callbackURL.trim()}>
              {busy ? <RefreshCw className="size-3.5 animate-spin" /> : <LogIn className="size-3.5" />}
              {t("kiro.socialFinish")}
            </Button>
          ) : (
            <Button onClick={() => void start()} disabled={busy}>
              {busy ? <RefreshCw className="size-3.5 animate-spin" /> : <ExternalLink className="size-3.5" />}
              {t("kiro.socialStart")}
            </Button>
          )}
        </>
      }
    >
      <div className="space-y-3 text-sm">
        <p className="text-xs text-muted-foreground">{t("kiro.socialHint")}</p>
        {session ? (
          <>
            <a href={session.auth_url} target="_blank" rel="noopener noreferrer" className="block break-all font-mono text-xs text-primary underline">
              {session.auth_url}
            </a>
            <label className="block space-y-1 text-xs font-medium text-muted-foreground">
              <span>{t("kiro.socialCallback")}</span>
              <Input value={callbackURL} onChange={(event) => setCallbackURL(event.target.value)} placeholder="http://127.0.0.1:3128/oauth/callback?code=…&state=…" />
            </label>
          </>
        ) : null}
      </div>
    </Modal>
  );
}

function DeviceLoginModal({ show, onClose, onDone }: { show: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [kind, setKind] = useState<"builder" | "enterprise">("builder");
  const [startURL, setStartURL] = useState("");
  const [region, setRegion] = useState("us-east-1");
  const [pending, setPending] = useState<KiroDeviceLoginStart | null>(null);
  const [busy, setBusy] = useState(false);
  // 父组件每次渲染都会新建回调；用 ref 持有，避免轮询循环被反复重启。
  const callbacks = useRef({ onClose, onDone });
  callbacks.current = { onClose, onDone };

  useEffect(() => {
    if (!show) setPending(null);
  }, [show]);

  useEffect(() => {
    if (!pending) return;
    let stopped = false;
    const poll = async () => {
      while (!stopped && Date.now() < Date.parse(pending.expires_at)) {
        await new Promise((resolve) => window.setTimeout(resolve, Math.max(pending.interval, 1) * 1000));
        if (stopped) return;
        try {
          const result = await api.pollKiroDeviceLogin(pending.session);
          if (result.status === "completed") {
            showToast(t("kiro.loginDone", { account: result.email || `#${result.id}` }), "success");
            callbacks.current.onDone();
            callbacks.current.onClose();
            return;
          }
        } catch (error) {
          showToast(getErrorMessage(error), "error");
          setPending(null);
          return;
        }
      }
    };
    void poll();
    return () => {
      stopped = true;
    };
  }, [pending, showToast, t]);

  const start = async () => {
    setBusy(true);
    try {
      const next = await api.startKiroDeviceLogin(kind === "enterprise" ? { start_url: startURL.trim(), region: region.trim() } : {});
      setPending(next);
      window.open(next.verification_uri_complete, "_blank", "noopener");
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("kiro.deviceTitle")}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>{t("common.cancel")}</Button>
          {!pending ? (
            <Button onClick={() => void start()} disabled={busy || (kind === "enterprise" && !startURL.trim())}>
              {busy ? <RefreshCw className="size-3.5 animate-spin" /> : <KeyRound className="size-3.5" />}
              {t("kiro.deviceStart")}
            </Button>
          ) : null}
        </>
      }
    >
      <div className="space-y-3 text-sm">
        {pending ? (
          <>
            <p className="text-xs text-muted-foreground">{t("kiro.deviceWaiting")}</p>
            <div className="rounded-lg border border-dashed border-border px-4 py-3 text-center font-mono text-2xl font-semibold tracking-[0.3em]">{pending.user_code}</div>
            <a href={pending.verification_uri_complete} target="_blank" rel="noopener noreferrer" className="block break-all font-mono text-xs text-primary underline">
              {pending.verification_uri_complete}
            </a>
            <p className="flex items-center gap-1.5 text-xs text-muted-foreground"><RefreshCw className="size-3 animate-spin" />{t("kiro.devicePolling")}</p>
          </>
        ) : (
          <>
            <Select
              value={kind}
              onValueChange={(value) => setKind(value as "builder" | "enterprise")}
              options={[
                { value: "builder", label: t("kiro.deviceBuilder") },
                { value: "enterprise", label: t("kiro.deviceEnterprise") },
              ]}
            />
            {kind === "enterprise" ? (
              <div className="grid gap-2 sm:grid-cols-[2fr_1fr]">
                <Input value={startURL} onChange={(event) => setStartURL(event.target.value)} placeholder="https://d-xxxxxxxxxx.awsapps.com/start" />
                <Input value={region} onChange={(event) => setRegion(event.target.value)} placeholder="us-east-1" />
              </div>
            ) : null}
            <p className="text-xs text-muted-foreground">{t("kiro.deviceHint")}</p>
          </>
        )}
      </div>
    </Modal>
  );
}

export default function KiroAccounts({ headerSlot }: { headerSlot?: ReactNode } = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [summary, setSummary] = useState<AccountListSummary | null>(null);
  const [tags, setTags] = useState<string[]>([]);
  const [domains, setDomains] = useState<AccountEmailDomainFacet[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [statsState, setStatsState] = useState<"ready" | "stale" | "warming">("warming");
  const [disabledSorts, setDisabledSorts] = useState<string[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  const [proxyPoolEnabled, setProxyPoolEnabled] = useState(false);
  const [globalProxyURL, setGlobalProxyURL] = useState("");
  const [quickProxyAccount, setQuickProxyAccount] = useState<AccountRow | null>(null);
  const [groups, setGroups] = useState<AccountGroup[]>([]);
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([]);

  const [modal, setModal] = useState<"import" | "social" | "device" | null>(null);
  const [exporting, setExporting] = useState(false);
  const [syncingAll, setSyncingAll] = useState(false);
  const [busyIDs, setBusyIDs] = useState<Set<number>>(new Set());
  const [showManageGroups, setShowManageGroups] = useState(false);
  const [assignTarget, setAssignTarget] = useState<AccountRow | null>(null);
  const [usageTarget, setUsageTarget] = useState<AccountRow | null>(null);
  const [editTarget, setEditTarget] = useState<AccountRow | null>(null);
  const [detailTarget, setDetailTarget] = useState<AccountRow | null>(null);
  const [testingTarget, setTestingTarget] = useState<AccountRow | null>(null);
  const detailAbortRef = useRef<AbortController | null>(null);
  const detailRequestSeqRef = useRef(0);
  useEffect(() => () => detailAbortRef.current?.abort(), []);
  const [pageStats, setPageStats] = useState<Record<string, AccountPageStatsItem>>({});
  const [pageStatsToken, setPageStatsToken] = useState(0);
  const [liveState, setLiveState] = useState<Record<string, { active_requests: number; occupied_requests: number }>>({});
  const [liveSessionSlotBufferEnabled, setLiveSessionSlotBufferEnabled] = useState(false);
  const [healthBars, setHealthBars] = useState<Record<string, AccountHealthBucket[]>>({});

  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<KiroStatusFilter>("all");
  const [healthTier, setHealthTier] = useState<HealthTier | null>(null);
  const [planFilter, setPlanFilter] = useState<string>("all");
  const [authFilter, setAuthFilter] = useState<"all" | KiroAuthKind>("all");
  const [tagFilter, setTagFilter] = useState<string>("all");
  const [domainFilter, setDomainFilter] = useState<string>("all");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(EMPTY_ACCOUNT_GROUP_FILTER);
  const [sortKey, setSortKey] = useState<SortKey | null>(null);
  const [sortDir, setSortDir] = useState<SortDir>("desc");
  const [showFilters, setShowFilters] = useState(false);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [hideDomainTags, setHideDomainTags] = useState(false);
  const [visibleCols, setVisibleCols] = useState<KiroColVisibility>(loadKiroCols);
  useEffect(() => {
    try {
      window.localStorage.setItem(KIRO_COLS_KEY, JSON.stringify(visibleCols));
    } catch {
      /* localStorage 不可用时忽略 */
    }
  }, [visibleCols]);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [batchGroupOpen, setBatchGroupOpen] = useState(false);
  const reloadAbortRef = useRef<AbortController | null>(null);
  const reloadGenerationRef = useRef(0);

  useEffect(() => {
    const id = window.setTimeout(() => setDebouncedSearch(search.trim()), 300);
    return () => window.clearTimeout(id);
  }, [search]);

  useEffect(() => {
    setPage(1);
  }, [debouncedSearch, statusFilter, healthTier, planFilter, authFilter, tagFilter, domainFilter, groupFilter, sortKey, sortDir, pageSize]);

  const kiroGroups = useMemo(() => groups.filter((g) => g.channel === "kiro"), [groups]);
  const groupMap = useMemo(() => new Map(kiroGroups.map((g) => [g.id, g])), [kiroGroups]);

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
          channel: "kiro",
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
      setStatsState(res.stats_state ?? "ready");
      setDisabledSorts(res.disabled_sorts ?? []);
      if (res.page && res.page !== page) setPage(res.page);
    } catch (error) {
      if (!controller.signal.aborted && generation === reloadGenerationRef.current) {
        const message = getErrorMessage(error);
        setLoadError(message);
        if (!options?.silent) showToast(message, "error");
      }
    } finally {
      // 由最新一次请求收尾 loading：静默请求顶替了可见刷新时，也要把它清掉。
      if (!controller.signal.aborted && generation === reloadGenerationRef.current) setLoading(false);
    }
  }, [page, pageSize, debouncedSearch, statusFilter, healthTier, planFilter, authFilter, tagFilter, domainFilter, groupFilter, sortKey, sortDir, showToast]);

  useEffect(() => {
    void reload();
    return () => reloadAbortRef.current?.abort();
  }, [reload]);

  // 请求数胶囊由后台扫描 usage_logs 填充；stats_state 未就绪时有限次静默轮询（与 Codex / Claude 页同口径）。
  const statsStaleRetriesRef = useRef(0);
  const statsReloadRef = useRef(reload);
  useEffect(() => {
    if (statsReloadRef.current !== reload) {
      statsReloadRef.current = reload;
      statsStaleRetriesRef.current = 0;
    }
    if (loading) return undefined;
    if (statsState === "ready") {
      statsStaleRetriesRef.current = 0;
      return undefined;
    }
    const maxStatsRetries = disabledSorts.includes("requests") ? 60 : 20;
    if (statsStaleRetriesRef.current >= maxStatsRetries) return undefined;
    return scheduleAccountStatsPoll(3000, () => {
      statsStaleRetriesRef.current += 1;
      void reload({ silent: true });
    });
  }, [accounts, disabledSorts, loadError, loading, reload, statsState]);

  // 操作完成时按当前筛选刷新：异步操作捕获的是发起时的 reload，期间改了筛选 / 页码会用旧条件覆盖列表。
  const reloadRef = useRef(reload);
  useEffect(() => {
    reloadRef.current = reload;
  }, [reload]);
  const reloadLatest = useCallback((options?: { silent?: boolean }) => reloadRef.current(options), []);

  const mergeLiveStateIntoAccount = useCallback((account: AccountRow): AccountRow => {
    const live = liveState[String(account.id)];
    return live
      ? { ...account, active_requests: live.active_requests, occupied_requests: live.occupied_requests, session_slot_buffer_enabled: liveSessionSlotBufferEnabled }
      : account;
  }, [liveSessionSlotBufferEnabled, liveState]);

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
      // 详情刷新失败时以列表为准。
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

  const handleSaveDetailCooldownPolicy = useCallback(async (account: AccountRow, data: {
    mode: "off" | "fixed" | "adaptive" | null;
    seconds: number | null;
    backoff_enabled: boolean | null;
  }) => {
    try {
      await api.updateAccountModelCooldownPolicy(account.id, data);
      showToast(t("accounts.modelCooldownPolicySaved"), "success");
      await refreshOpenDetail(account.id);
      void reloadLatest();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [refreshOpenDetail, reloadLatest, showToast, t]);

  const handleClearDetailCooldown = useCallback(async (account: AccountRow, model: string) => {
    try {
      await api.clearAccountModelCooldown(account.id, model);
      showToast(t("accounts.modelCooldownCleared", { model }), "success");
      await refreshOpenDetail(account.id);
      void reloadLatest();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [refreshOpenDetail, reloadLatest, showToast, t]);

  const handleClearAllDetailCooldowns = useCallback(async (account: AccountRow) => {
    try {
      const result = await api.clearAllAccountModelCooldowns(account.id);
      showToast(t("accounts.allModelCooldownsCleared", { count: result.cleared }), "success");
      await refreshOpenDetail(account.id);
      void reloadLatest();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [refreshOpenDetail, reloadLatest, showToast, t]);

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

  const { bars: intelligenceBars, error: intelligenceBarsError } = useIntelligenceBars(accountIDsKey, pageStatsToken);

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
        if (!merged.usage_7d_detail && stats.usage_7d_detail) merged.usage_7d_detail = stats.usage_7d_detail;
        if (!merged.usage_today_detail && stats.usage_today_detail) merged.usage_today_detail = stats.usage_today_detail;
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
    void api
      .getAPIKeys({ view: "lite" })
      .then((res) => {
        if (!cancelled) setAPIKeys(res.keys ?? []);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [reloadGroups]);

  // 组代理解析不看渠道，按全量 groups 构建，避免跨渠道成员被误判成"无组代理"。
  const proxyBindingCtx = useMemo<ProxyBindingContext>(
    () => buildProxyBindingContext({ proxies: proxyPool, groups, poolEnabled: proxyPoolEnabled, globalProxy: globalProxyURL }),
    [proxyPool, groups, proxyPoolEnabled, globalProxyURL],
  );

  useEffect(() => {
    setGroupFilter((current) => pruneAccountGroupFilter(current, kiroGroups));
  }, [kiroGroups]);

  // ── 账号操作 ──────────────────────────────────────────────
  const runAccountAction = useCallback(
    async (acc: AccountRow, action: () => Promise<unknown>, success?: string) => {
      setBusyIDs((prev) => new Set(prev).add(acc.id));
      try {
        await action();
        if (success) showToast(success, "success");
        await refreshOpenDetail(acc.id);
        setPageStatsToken((v) => v + 1);
        void reloadLatest({ silent: true });
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      } finally {
        setBusyIDs((prev) => {
          const next = new Set(prev);
          next.delete(acc.id);
          return next;
        });
      }
    },
    [refreshOpenDetail, reloadLatest, showToast],
  );

  const handleDelete = useCallback(async (acc: AccountRow) => {
    const ok = await confirm({
      title: t("kiro.deleteTitle"),
      description: t("kiro.deleteDescription", { account: accountLabel(acc) }),
      confirmText: t("common.delete"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!ok) return;
    try {
      await api.deleteAccount(acc.id);
      showToast(t("kiro.deleted"), "success");
      void reloadLatest();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [confirm, reloadLatest, showToast, t]);

  const handleRefreshToken = (acc: AccountRow) => runAccountAction(acc, () => api.refreshAccount(acc.id), t("kiro.tokenRefreshed"));
  const handleSync = (acc: AccountRow) => runAccountAction(acc, () => api.syncKiroAccount(acc.id), t("kiro.synced"));
  const handleToggleEnabled = (acc: AccountRow) => {
    const next = acc.enabled === false;
    return runAccountAction(acc, () => api.toggleAccountEnabled(acc.id, next), next ? t("kiro.enabled") : t("kiro.disabled"));
  };
  const handleToggleLock = (acc: AccountRow) => {
    const next = !acc.locked;
    return runAccountAction(acc, () => api.toggleAccountLock(acc.id, next), next ? t("kiro.lockedToast") : t("kiro.unlockedToast"));
  };
  const handleResetStatus = (acc: AccountRow) => runAccountAction(acc, () => api.resetAccountStatus(acc.id), t("kiro.statusReset"));

  // 用量列刷新：后端对 Kiro 账号执行额度与模型目录同步。
  const handleRefreshUsage = useCallback(async (acc: AccountRow) => {
    try {
      await api.refreshAccountUsage(acc.id);
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
    setPageStatsToken((v) => v + 1);
    void reloadLatest({ silent: true });
  }, [reloadLatest, showToast]);

  const syncAll = useCallback(async () => {
    setSyncingAll(true);
    try {
      const result = await api.syncAllKiroAccounts();
      showToast(t("kiro.syncAllResult", { refreshed: result.refreshed, failed: result.failed }), result.failed > 0 ? "error" : "success");
      void reloadLatest({ silent: true });
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setSyncingAll(false);
    }
  }, [reloadLatest, showToast, t]);

  // ── 批量与导出 ──────────────────────────────────────────────
  const selectedIds = useMemo(() => Array.from(selected), [selected]);

  const exportAccounts = useCallback(async (format: "kam" | "kiro-rs", ids?: number[]) => {
    setExporting(true);
    try {
      downloadNamedBlob(await api.exportKiroAccounts(format, ids), `kiro-${format}.json`);
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setExporting(false);
    }
  }, [showToast]);

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

  const runBatch = useCallback(async (patch: { enabled?: boolean; locked?: boolean }) => {
    if (selectedIds.length === 0) return;
    try {
      await api.batchUpdateAccounts({ ids: selectedIds, ...patch });
      setSelected(new Set());
      void reloadLatest();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [selectedIds, reloadLatest, showToast]);

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
      void reloadLatest();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  }, [confirm, reloadLatest, selectedIds, showToast, t]);

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
    const chips: Array<{ id: KiroStatusFilter; count: number }> = [
      { id: "all", count: s?.total ?? total },
      { id: "normal", count: s?.normal ?? 0 },
      { id: "scheduling", count: s?.active ?? 0 },
      { id: "rate_limited", count: s?.rate_limited ?? 0 },
      { id: "abnormal", count: s?.abnormal ?? 0 },
      { id: "banned", count: s?.banned ?? 0 },
      { id: "error", count: s?.error ?? 0 },
      { id: "disabled", count: s?.disabled ?? 0 },
      { id: "locked", count: s?.locked ?? 0 },
    ];
    return chips.map((chip) => ({ ...chip, label: t(`kiro.stat.${chip.id}`) }));
  }, [summary, total, t]);
  const statusLabelFor = (id: KiroStatusFilter) => statChips.find((chip) => chip.id === id)?.label ?? id;

  const healthChips = useMemo(() => {
    const s = summary;
    return [
      { id: "healthy" as HealthTier, count: s?.healthy ?? 0, tone: "success" as const },
      { id: "warm" as HealthTier, count: s?.warm ?? 0, tone: "warning" as const },
      { id: "risky" as HealthTier, count: s?.risky ?? 0, tone: "danger" as const },
      // 与 health_tier=banned 筛选同口径（按健康档计数，不是按 unauthorized 状态）。
      { id: "banned" as HealthTier, count: s?.health_banned ?? 0, tone: "neutral" as const },
    ].map((chip) => ({ ...chip, label: t(`kiro.health.${chip.id}`) }));
  }, [summary, t]);

  const planOrder = Object.keys(KIRO_PLAN_LABELS);
  // 套餐筛选固定列出全部档位，不依赖当前页出现过哪些套餐。
  const planTabs = ["all", ...planOrder];
  const planLabel = (plan: string) => (plan === "all" ? t("accounts.filterAll") : KIRO_PLAN_LABELS[plan] ?? plan);

  const authCounts = summary?.kiro_auth_kinds ?? {};
  const authTabs: Array<{ id: "all" | KiroAuthKind; label: string; count?: number }> = [
    { id: "all", label: t("accounts.filterAll") },
    ...KIRO_AUTH_KINDS.filter((kind) => (authCounts[kind] ?? 0) > 0 || authFilter === kind).map((kind) => ({
      id: kind,
      label: t(`kiro.authKind.${kind}`),
      count: authCounts[kind] ?? 0,
    })),
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

  const manageSections: HeaderActionMenuSection[] = [
    {
      key: "maintenance",
      label: t("accounts.maintenanceActions"),
      items: [
        {
          key: "sync-all",
          label: t("kiro.syncAll"),
          icon: <RefreshCw className={cn("size-3.5", syncingAll && "animate-spin")} />,
          disabled: syncingAll || total === 0,
          onSelect: () => void syncAll(),
        },
      ],
    },
    {
      key: "data",
      label: t("accounts.dataActions"),
      items: [
        {
          key: "export-kiro-rs",
          label: t("kiro.exportKiroRs"),
          icon: <Download className="size-3.5" />,
          disabled: exporting || total === 0,
          onSelect: () => void exportAccounts("kiro-rs", selectedIds.length > 0 ? selectedIds : undefined),
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
    { value: "default", label: t("kiro.sort.default") },
    { value: "group", label: t("kiro.sort.group") },
    { value: "priority", label: t("kiro.sort.priority") },
    { value: "requests", label: t("kiro.sort.requests") },
    { value: "today", label: t("kiro.sort.today") },
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
  const exportLabel = selectedIds.length > 0 ? t("kiro.exportSelected") : t("kiro.exportAll");

  return (
    <div className="relative @container/kiro-accounts">
      <PageHeader
        title={t("kiro.pageTitle")}
        description={t("kiro.pageSubtitle")}
        hideTitle={Boolean(headerSlot)}
        actionsBelow
        titleAdornment={headerSlot}
        actions={
          <div className="flex w-full flex-wrap items-center gap-1.5 sm:gap-2 [&>button]:h-8">
            <Button size="sm" className="min-w-0 sm:flex-none" onClick={() => setModal("import")}>
              <Upload className="size-3.5" />
              {t("kiro.import")}
            </Button>
            <HeaderActionMenu
              label={t("kiro.login")}
              icon={<LogIn className="size-3.5" />}
              items={[
                { key: "social", label: t("kiro.socialTitle"), icon: <LogIn className="size-3.5" />, onSelect: () => setModal("social") },
                { key: "device", label: t("kiro.deviceTitle"), icon: <KeyRound className="size-3.5" />, onSelect: () => setModal("device") },
              ]}
            />
            <Button
              variant="outline"
              size="sm"
              className="size-8 p-0 md:w-auto md:px-2.5"
              title={`${exportLabel} · ${t("kiro.exportKam")}`}
              aria-label={exportLabel}
              disabled={exporting || total === 0}
              onClick={() => void exportAccounts("kam", selectedIds.length > 0 ? selectedIds : undefined)}
            >
              <Download className="size-3.5" />
              <span className="hidden md:inline">{exporting ? t("kiro.exporting") : exportLabel}</span>
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
              onClick={() => {
                // 分页列表不带今日 / 7 天明细，显式刷新时一并重拉。
                setPageStatsToken((v) => v + 1);
                void reloadLatest();
              }}
            >
              <RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
            </Button>
          </div>
        }
      />

      {loadError && accounts.length > 0 ? (
        <div className="mb-2 flex items-center justify-between gap-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive" role="alert">
          <span className="truncate">{loadError}</span>
          <Button variant="outline" size="sm" onClick={() => void reloadLatest()}>{t("common.retry")}</Button>
        </div>
      ) : null}
      {loading && accounts.length > 0 ? (
        <div className="mb-2 flex items-center justify-end gap-1.5 text-xs text-muted-foreground" role="status">
          <Loader2 className="size-3 animate-spin" />
          {t("common.loading")}
        </div>
      ) : null}

      <div className="mb-4 flex snap-x gap-2 overflow-x-auto pb-1 @4xl/kiro-accounts:grid @4xl/kiro-accounts:grid-cols-5 @4xl/kiro-accounts:gap-3 [&>button]:w-40 [&>button]:shrink-0 [&>button]:snap-start @4xl/kiro-accounts:[&>button]:w-full">
        <CompactStat label={t("accounts.totalAccounts")} chipLabel={t("kiro.stat.all")} value={summary?.total ?? total} tone="neutral" active={statusFilter === "all"} onClick={() => setStatusFilter("all")} />
        <CompactStat label={t("accounts.normalAccounts")} chipLabel={t("kiro.stat.normal")} value={summary?.normal ?? 0} tone="success" active={statusFilter === "normal"} onClick={() => setStatusFilter("normal")} />
        <CompactStat label={t("accounts.schedulingAccounts")} chipLabel={t("kiro.stat.scheduling")} value={summary?.active ?? 0} tone="warning" active={statusFilter === "scheduling"} onClick={() => setStatusFilter("scheduling")} />
        <CompactStat label={t("accounts.rateLimited")} chipLabel={t("kiro.stat.rate_limited")} value={summary?.rate_limited ?? 0} tone="warning" active={statusFilter === "rate_limited"} onClick={() => setStatusFilter("rate_limited")} />
        <CompactStat
          label={t("accounts.abnormalAccounts")}
          chipLabel={t("kiro.stat.abnormal")}
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

      <div className="toolbar-surface mb-3 flex flex-col gap-2 px-3 py-2.5">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
          <div className="relative min-w-0 flex-1 @4xl/kiro-accounts:w-56 @4xl/kiro-accounts:flex-none @6xl/kiro-accounts:w-64">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground/70" />
            <Input
              className="h-8 rounded-lg bg-background/70 pl-9 pr-8 text-[13px] shadow-none"
              placeholder={t("kiro.searchPlaceholder")}
              aria-label={t("kiro.searchPlaceholder")}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            {search ? (
              <button type="button" className="absolute right-1.5 top-1/2 flex size-6 -translate-y-1/2 items-center justify-center rounded-md text-muted-foreground hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50" aria-label={t("kiro.clearSearch")} onClick={() => setSearch("")}>
                <X className="size-3.5" />
              </button>
            ) : null}
          </div>
          <div className="order-last flex w-full min-w-0 items-center gap-0.5 overflow-x-auto @4xl/kiro-accounts:order-none @4xl/kiro-accounts:w-auto @4xl/kiro-accounts:flex-1">
            {statChips.map((chip) => (
              <button
                key={chip.id}
                type="button"
                aria-pressed={statusFilter === chip.id}
                onClick={() => setStatusFilter(chip.id)}
                className={cn(
                  "inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-lg px-2 py-1 text-xs font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50",
                  statusFilter === chip.id ? "bg-primary/10 text-primary" : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
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
            aria-controls="kiro-account-filters"
            onClick={() => setShowFilters((current) => !current)}
            className={cn("h-8 @4xl/kiro-accounts:hidden", (showFilters || filtersActive) && "border-primary/25 bg-primary/5 text-primary")}
          >
            <SlidersHorizontal className="size-3.5" />
            {t("accounts.filter")}
          </Button>
          <div className="ml-auto shrink-0">
            <KiroColumnSettingsMenu
              columns={visibleCols}
              onToggle={(column) => setVisibleCols((current) => ({ ...current, [column]: !current[column] }))}
              onReset={() => setVisibleCols(defaultKiroCols())}
            />
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 border-t border-border/50 pt-2">
          <div className="flex min-h-8 min-w-0 max-w-full items-center gap-1 overflow-x-auto py-0.5">
            <span className="mr-1 shrink-0 text-[11px] font-medium text-muted-foreground">{t("kiro.schedulingView")}</span>
            {healthChips.map((h) => (
              <KiroSchedulerChip key={h.id} label={h.label} value={h.count} tone={h.tone} active={healthTier === h.id} onClick={() => setHealthTier(healthTier === h.id ? null : h.id)} />
            ))}
          </div>

          <div id="kiro-account-filters" className={cn("w-full flex-col gap-1.5 @4xl/kiro-accounts:contents", showFilters ? "flex" : "hidden")}>
            <div className="flex max-w-full flex-wrap items-center gap-1.5 @4xl/kiro-accounts:ml-1">
              {planTabs.length > 1 ? (
                <div className={segmentGroupClass}>
                  {planTabs.map((p) => (
                    <button key={p} type="button" aria-pressed={planFilter === p} onClick={() => setPlanFilter(p)} className={segmentButtonClass(planFilter === p)}>{planLabel(p)}</button>
                  ))}
                </div>
              ) : null}
              {authTabs.length > 1 ? (
                <div className={segmentGroupClass}>
                  {authTabs.map((a) => (
                    <button key={a.id} type="button" aria-pressed={authFilter === a.id} onClick={() => setAuthFilter(a.id)} className={segmentButtonClass(authFilter === a.id)}>
                      {typeof a.count === "number" ? `${a.label} ${a.count}` : a.label}
                    </button>
                  ))}
                </div>
              ) : null}
            </div>

            <div className="grid grid-cols-2 gap-1.5 @4xl/kiro-accounts:contents">
              <Select className="min-w-0 @4xl/kiro-accounts:w-28" compact value={tagFilter} onValueChange={setTagFilter} options={[{ value: "all", label: t("accounts.tagsFilter") }, ...tags.map((tag) => ({ value: tag, label: tag }))]} />
              <Select
                className="min-w-0 @4xl/kiro-accounts:w-36"
                compact
                value={domainFilter}
                onValueChange={setDomainFilter}
                options={[
                  { value: "all", label: t("accounts.emailDomainFilter") },
                  ...domains.map((d) => ({ value: d.domain, triggerLabel: d.domain, label: t("accounts.emailDomainFilterOption", { domain: d.domain, banned: d.banned, total: d.total }) })),
                ]}
              />
              <AccountGroupFilterSelect className="min-w-0 @4xl/kiro-accounts:w-32" groups={kiroGroups} value={groupFilter} onChange={setGroupFilter} />
              <div className="flex min-w-0 items-center gap-1 @4xl/kiro-accounts:w-36">
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
                  <Button variant="ghost" size="icon-sm" className="text-primary" title={sortDir === "desc" ? t("kiro.sortAscending") : t("kiro.sortDescending")} aria-label={sortDir === "desc" ? t("kiro.sortAscending") : t("kiro.sortDescending")} onClick={() => setSortDir((current) => current === "desc" ? "asc" : "desc")}>
                    {sortDir === "desc" ? <ArrowDown className="size-3.5" /> : <ArrowUp className="size-3.5" />}
                  </Button>
                ) : null}
              </div>
              <Button type="button" variant="ghost" size="sm" className="justify-start px-2 text-xs text-muted-foreground @4xl/kiro-accounts:ml-auto" aria-pressed={!hideDomainTags} onClick={() => setHideDomainTags((v) => !v)}>
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
              <button type="button" onClick={() => { setSortKey(null); setSortDir("desc"); }} className={filterPillClass} title={t("kiro.sort.default")}>
                {sortDir === "desc" ? <ArrowDown className="size-3" /> : <ArrowUp className="size-3" />}
                {sortOptions.find((option) => option.value === sortKey)?.label}<X className="size-3" />
              </button>
            ) : null}
            {statusFilter !== "all" ? (
              <button type="button" onClick={() => setStatusFilter("all")} className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2.5 py-1 text-[11px] font-medium text-primary transition-colors hover:bg-primary/15">
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
                {t(`kiro.authKind.${authFilter}`)}
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
            <button type="button" onClick={clearFilters} className="ml-auto text-[11px] font-medium text-muted-foreground transition-colors hover:text-foreground">
              {t("accounts.clearFilters")}
            </button>
          </div>
        ) : null}
      </div>

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
            <Button variant="outline" size="sm" disabled={exporting} onClick={() => void exportAccounts("kam", selectedIds)}>
              <Download className="size-3.5" />
              <span className="hidden sm:inline">{t("kiro.exportSelected")}</span>
            </Button>
            <Button variant="outline" size="sm" onClick={() => void runBatch({ enabled: true })}>
              <Power className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.enable")}</span>
            </Button>
            <Button variant="outline" size="sm" onClick={() => void runBatch({ enabled: false })}>
              <PowerOff className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.disable")}</span>
            </Button>
            <Button variant="outline" size="sm" onClick={() => setBatchGroupOpen(true)}>
              <FolderOpen className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.batchGroupEdit")}</span>
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

      <Card>
        <CardContent className="p-3 sm:p-4">
          <StateShell
            variant="section"
            loading={loading && accounts.length === 0}
            error={accounts.length === 0 ? loadError : null}
            onRetry={() => void reloadLatest()}
            isEmpty={accounts.length === 0}
            emptyIcon={<ChannelLogo channel="kiro" size={30} />}
            emptyTitle={poolEmpty ? t("kiro.empty") : t("kiro.noMatchesTitle")}
            emptyDescription={poolEmpty ? t("kiro.emptyHint") : t("kiro.noMatchesDescription")}
            action={
              poolEmpty ? (
                <Button onClick={() => setModal("import")}>
                  <Upload className="size-4" />
                  {t("kiro.import")}
                </Button>
              ) : (
                <Button variant="outline" onClick={clearFilters}>
                  {t("accounts.clearFilters")}
                </Button>
              )
            }
          >
            <div className={cn("data-table-shell kiro-account-table", accounts.length <= pageSize && "account-table-shell-fit-content")}>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-10">
                      <input type="checkbox" className="size-4 cursor-pointer accent-primary" checked={allPageSelected} onChange={toggleSelectAll} aria-label={t("accounts.selectAll")} />
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("accounts.sequence")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("accounts.email")}</TableHead>
                    {visibleCols.tags ? <TableHead className="text-[13px] font-semibold">{t("accounts.tagsLabel")}</TableHead> : null}
                    {visibleCols.groups ? renderSortHead("group", t("accounts.groupsLabel")) : null}
                    {visibleCols.proxy ? <TableHead className="text-[13px] font-semibold">{t("accounts.proxyColumn")}</TableHead> : null}
                    {visibleCols.priority ? renderSortHead("priority", t("accounts.schedulerPriorityColumn")) : null}
                    {visibleCols.plan ? <TableHead className="text-[13px] font-semibold">{t("accounts.plan")}</TableHead> : null}
                    {visibleCols.status ? <TableHead className="text-[13px] font-semibold">{t("accounts.status")}</TableHead> : null}
                    {visibleCols.today ? renderSortHead("today", t("accounts.todayStats"), t("accounts.todayStatsHint")) : null}
                    {visibleCols.requests ? renderSortHead("requests", t("accounts.requests")) : null}
                    {visibleCols.usage ? <TableHead className="text-[13px] font-semibold">{t("kiro.colQuota")}</TableHead> : null}
                    {visibleCols.cost ? <TableHead className="text-[13px] font-semibold">{t("accounts.billed")}</TableHead> : null}
                    {visibleCols.importTime ? renderSortHead("importTime", t("accounts.importTime")) : null}
                    {visibleCols.updatedAt ? <TableHead className="text-[13px] font-semibold">{t("accounts.updatedAt")}</TableHead> : null}
                    <TableHead data-account-actions className="text-right text-[13px] font-semibold">{t("accounts.actions")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {displayRows.map((acc, idx) => (
                    <KiroAccountRow
                      key={acc.id}
                      acc={acc}
                      no={(page - 1) * pageSize + idx + 1}
                      selected={selected.has(acc.id)}
                      busy={busyIDs.has(acc.id)}
                      detailOpen={detailTarget?.id === acc.id}
                      onToggleSelect={() => toggleSelect(acc.id)}
                      groupMap={groupMap}
                      healthBuckets={healthBars[String(acc.id)]}
                      intelligenceBar={intelligenceBars[String(acc.id)]}
                      intelligenceError={intelligenceBarsError}
                      showDomainTags={!hideDomainTags}
                      columns={visibleCols}
                      proxyCtx={proxyBindingCtx}
                      onEditProxy={() => setQuickProxyAccount(acc)}
                      onRefreshToken={() => void handleRefreshToken(acc)}
                      onSync={() => void handleSync(acc)}
                      onExportOne={() => void exportAccounts("kam", [acc.id])}
                      onToggleEnabled={() => void handleToggleEnabled(acc)}
                      onToggleLock={() => void handleToggleLock(acc)}
                      onResetStatus={() => void handleResetStatus(acc)}
                      onAssignGroups={() => setAssignTarget(acc)}
                      onUsage={() => setUsageTarget(acc)}
                      onUsageRefreshed={() => handleRefreshUsage(acc)}
                      onOpenDetail={() => void openDetail(acc)}
                      onTest={() => setTestingTarget(acc)}
                      onEdit={() => setEditTarget(acc)}
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

      <ImportModal show={modal === "import"} onClose={() => setModal(null)} onDone={() => void reloadLatest()} />
      <SocialLoginModal show={modal === "social"} onClose={() => setModal(null)} onDone={() => void reloadLatest()} />
      <DeviceLoginModal show={modal === "device"} onClose={() => setModal(null)} onDone={() => void reloadLatest()} />

      {showManageGroups ? (
        <AccountGroupManagerModal
          channel="kiro"
          groups={kiroGroups}
          title={t("kiro.manageGroups")}
          onClose={() => setShowManageGroups(false)}
          onChanged={() => {
            void reloadGroups();
            void reloadLatest();
          }}
        />
      ) : null}

      <BatchAccountGroupModal
        show={batchGroupOpen}
        ids={selectedIds}
        channel="kiro"
        groups={kiroGroups}
        onClose={() => setBatchGroupOpen(false)}
        onSaved={() => {
          setBatchGroupOpen(false);
          setSelected(new Set());
          void reloadGroups();
          void reloadLatest();
        }}
        onGroupsChanged={reloadGroups}
      />

      {assignTarget ? (
        <AssignGroupsModal
          account={assignTarget}
          groups={kiroGroups}
          onGroupsChanged={reloadGroups}
          onClose={() => setAssignTarget(null)}
          onSaved={() => {
            setAssignTarget(null);
            void reloadGroups();
            void reloadLatest();
          }}
        />
      ) : null}

      {usageTarget ? (
        <AccountUsageModal account={usageTarget} onClose={() => setUsageTarget(null)} showCreditSettings={false} officialUsage={false} />
      ) : null}

      {editTarget ? (
        <KiroEditAccountModal
          account={editTarget}
          proxies={proxyPool}
          apiKeys={apiKeys}
          tagOptions={tags}
          onClose={() => setEditTarget(null)}
          onSaved={() => {
            const id = editTarget.id;
            setEditTarget(null);
            void refreshOpenDetail(id);
            void reloadLatest();
          }}
        />
      ) : null}

      {detailTarget ? (
        <AccountDetailSheet
          account={detailTarget}
          groups={(detailTarget.group_ids ?? []).map((id) => groupMap.get(id)).filter(Boolean) as AccountGroup[]}
          healthBuckets={healthBars[String(detailTarget.id)]}
          intelligenceBar={intelligenceBars[String(detailTarget.id)]}
          intelligenceError={intelligenceBarsError}
          refreshing={busyIDs.has(detailTarget.id)}
          usageSlot={
            <div className="space-y-1.5 rounded-xl border border-border bg-card p-3">
              <KiroQuota usage={detailTarget.kiro_usage} />
              <Kiro7dDetail acc={detailTarget} />
            </div>
          }
          providerSlot={<KiroProviderSection acc={detailTarget} busy={busyIDs.has(detailTarget.id)} onSync={() => void handleSync(detailTarget)} />}
          onClose={closeDetail}
          onEdit={() => { setEditTarget(detailTarget); closeDetail(); }}
          onUsage={() => { setUsageTarget(detailTarget); closeDetail(); }}
          onTest={() => { closeDetail(); setTestingTarget(detailTarget); }}
          onRefresh={() => void handleRefreshToken(detailTarget)}
          onGenerateAuthJson={() => void exportAccounts("kam", [detailTarget.id])}
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
        <TestConnectionModal
          account={testingTarget}
          onClose={() => setTestingTarget(null)}
          onSettled={() => {
            setPageStatsToken((v) => v + 1);
            void reloadLatest({ silent: true });
          }}
        />
      ) : null}

      <AccountProxyQuickEditor
        account={quickProxyAccount}
        accountLabel={quickProxyAccount ? accountLabel(quickProxyAccount) : ""}
        proxies={proxyPool}
        ctx={proxyBindingCtx}
        onClose={() => setQuickProxyAccount(null)}
        onSaved={() => reloadLatest({ silent: true })}
      />

      {confirmDialog}
    </div>
  );
}

const ROW_INTERACTIVE_SELECTOR =
  'button, a, input, label, [role="menuitem"], [role="menu"], [data-slot="button"], [data-slot="select-trigger"]';

function KiroAccountRow({
  acc,
  no,
  selected,
  busy,
  detailOpen,
  onToggleSelect,
  groupMap,
  healthBuckets,
  intelligenceBar,
  intelligenceError,
  showDomainTags,
  columns,
  proxyCtx,
  onEditProxy,
  onRefreshToken,
  onSync,
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
  onDelete,
}: {
  acc: AccountRow;
  no: number;
  selected: boolean;
  busy: boolean;
  detailOpen: boolean;
  onToggleSelect: () => void;
  groupMap: Map<number, AccountGroup>;
  healthBuckets?: AccountHealthBucket[];
  intelligenceBar?: IntelligenceBar;
  intelligenceError?: boolean;
  showDomainTags: boolean;
  columns: KiroColVisibility;
  proxyCtx: ProxyBindingContext;
  onEditProxy: () => void;
  onRefreshToken: () => void;
  onSync: () => void;
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
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const isAPIKey = acc.kiro_auth_kind === "api_key";
  const disabled = acc.enabled === false;
  const cooldownReason = (acc.status || "").toLowerCase().includes("rate") ? acc.error_message : "";
  const accGroups = (acc.group_ids || []).map((id) => groupMap.get(id)).filter(Boolean) as AccountGroup[];
  const tableOverlayKind = resolveAccountOverlayKind(acc);
  const tableOverlay = renderAccountStateOverlay(acc, t, { compact: true, markerOnly: true, onRecover: onResetStatus });
  const plan = kiroPlanBadge(acc.kiro_plan, acc.kiro_usage?.subscription_title || acc.plan_type);

  const rowMenuItems: HeaderActionMenuItem[] = [
    { key: "edit", label: t("kiro.editTitle"), icon: <Pencil className="size-3.5" />, onSelect: onEdit },
    { key: "usage", label: t("accounts.usageDetail"), icon: <BarChart3 className="size-3.5" />, onSelect: onUsage },
    { key: "test", label: t("accounts.testConnection"), icon: <Zap className="size-3.5" />, onSelect: onTest },
    { key: "sync", label: t("kiro.sync"), icon: <RefreshCw className="size-3.5" />, disabled: busy, onSelect: onSync },
    ...(!isAPIKey ? [{ key: "refresh", label: t("accounts.refreshAccessToken"), icon: <KeyRound className="size-3.5" />, disabled: busy, onSelect: onRefreshToken }] : []),
    { key: "export", label: t("kiro.exportOne"), icon: <FileJson className="size-3.5" />, onSelect: onExportOne },
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
    { key: "delete", label: t("accounts.deleteAccount"), icon: <Trash2 className="size-3.5" />, destructive: true, onSelect: onDelete },
  ];

  return (
    <TableRow
      data-state={selected ? "selected" : undefined}
      className={cn("cursor-pointer", detailOpen ? "bg-primary/8" : selected ? "bg-primary/5" : "", accountStateTableRowClass(acc))}
      onClick={(event) => {
        const target = event.target as HTMLElement | null;
        if (target?.closest(ROW_INTERACTIVE_SELECTOR)) return;
        onOpenDetail();
      }}
    >
      <TableCell>
        <div className="flex items-center gap-1">
          <input
            type="checkbox"
            className="size-4 cursor-pointer accent-primary"
            checked={selected}
            onChange={onToggleSelect}
            onClick={(event) => event.stopPropagation()}
            aria-label={accountLabel(acc)}
          />
          {!columns.status && tableOverlayKind ? (
            <span className="sr-only">{tableOverlayKind === "disabled" ? t("accounts.disabledOverlay") : t("accounts.overloadOverlay")}</span>
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
      <TableCell className="text-[14px] font-mono text-muted-foreground" title={`ID ${acc.id}`}>
        {no}
      </TableCell>
      <TableCell className="min-w-[220px] whitespace-normal text-[14px] text-muted-foreground">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="flex size-8 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-card ring-1 ring-border shadow-sm">
            <ChannelLogo channel="kiro" size={32} className="rounded-lg" />
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
              {accountLabel(acc)}
            </button>
            {showDomainTags && acc.email_domain ? (
              <span
                className="inline-flex max-w-full items-center break-all rounded-md bg-muted px-1.5 py-0.5 text-left text-[10px] font-medium leading-tight text-muted-foreground ring-1 ring-inset ring-border/80"
                title={`${t("accounts.emailDomainSystemTag")}: @${acc.email_domain}`}
              >
                @{acc.email_domain}
              </span>
            ) : null}
            <div className="flex flex-wrap items-center gap-1">
              {acc.locked ? (
                <span className="inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                  <Lock className="mr-0.5 size-2.5" />
                  {t("accounts.lock")}
                </span>
              ) : null}
              <span className="inline-flex items-center rounded-md bg-violet-50 px-1.5 py-0.5 text-[10px] font-medium text-violet-700 ring-1 ring-inset ring-violet-500/20 dark:bg-violet-950/40 dark:text-violet-300 dark:ring-violet-400/20">
                {kiroAuthLabel(acc, t)}
              </span>
              {acc.models?.length ? (
                <span className="inline-flex items-center rounded-md bg-orange-50 px-1.5 py-0.5 text-[10px] font-medium text-orange-700 ring-1 ring-inset ring-orange-600/20 dark:bg-orange-950 dark:text-orange-300 dark:ring-orange-400/20">
                  {t("kiro.modelCount", { count: acc.models.length })}
                </span>
              ) : null}
              {acc.last_used_at ? (
                <span className="text-[10px] text-muted-foreground/70" title={formatBeijingTime(acc.last_used_at)}>
                  {t("kiro.lastUsed")}: {formatRelativeShort(acc.last_used_at, t)}
                </span>
              ) : null}
            </div>
          </div>
        </div>
      </TableCell>
      {columns.tags ? (
        <TableCell className="min-w-[120px]">
          <KiroChipList items={acc.tags ?? []} />
        </TableCell>
      ) : null}
      {columns.groups ? (
        <TableCell className="min-w-[140px]">
          <KiroGroupChipList groups={accGroups} onClick={onAssignGroups} emptyLabel={t("accounts.groupQuickEdit")} />
        </TableCell>
      ) : null}
      {columns.proxy ? (
        <TableCell className="min-w-[120px] max-w-[180px]">
          <AccountProxyBadge account={acc} ctx={proxyCtx} onClick={onEditProxy} />
        </TableCell>
      ) : null}
      {columns.priority ? (
        <TableCell>
          <KiroPriorityBadge acc={acc} />
        </TableCell>
      ) : null}
      {columns.plan ? (
        <TableCell>
          {plan ? (
            <span
              className={cn("inline-flex min-w-0 max-w-full items-center truncate rounded-md px-2.5 py-1 text-[13px] font-semibold ring-1 ring-inset", plan.tone)}
              title={acc.kiro_usage?.subscription_title || acc.plan_type}
            >
              {plan.label}
            </span>
          ) : (
            <span className="text-[12px] text-muted-foreground">-</span>
          )}
        </TableCell>
      ) : null}
      {columns.status ? (
        <TableCell data-account-state-cell="status">
          {tableOverlay ?? (
            <div className="min-w-[168px] max-w-[240px] space-y-1.5">
              <div className="flex min-h-6 flex-nowrap items-center gap-1.5 whitespace-nowrap">
                <div className="inline-flex shrink-0 items-center gap-1.5">
                  <StatusBadge status={getAccountStatusBadgeStatus(acc)} errorMessage={acc.error_message} detail={cooldownReason} />
                  <AccountIntelligenceActions bar={intelligenceBar} accountID={acc.id} accountName={accountLabel(acc)} />
                </div>
                <LiveCountdown until={acc.cooldown_until} label={t("kiro.resetIn")} />
                <KiroConcurrencyBadge acc={acc} />
              </div>
              <AccountHealthBar buckets={healthBuckets} />
              <AccountIntelligenceBar bar={intelligenceBar} accountID={acc.id} accountName={accountLabel(acc)} error={intelligenceError} />
            </div>
          )}
        </TableCell>
      ) : null}
      {columns.today ? (
        <TableCell>
          <KiroTodayStatsCell acc={acc} />
        </TableCell>
      ) : null}
      {columns.requests ? (
        <TableCell>
          <RequestCountPills account={acc} compact />
        </TableCell>
      ) : null}
      {columns.usage ? (
        <TableCell>
          <div className="flex min-w-[232px] items-center gap-1.5">
            <div className="min-w-0 flex-1 space-y-1">
              <KiroQuota usage={acc.kiro_usage} />
              <Kiro7dDetail acc={acc} />
            </div>
            <UsageRefreshButton onRefresh={onUsageRefreshed} title={t("kiro.sync")} />
          </div>
        </TableCell>
      ) : null}
      {columns.cost ? (
        <TableCell className="whitespace-nowrap text-[13px] text-muted-foreground">
          <KiroBilledCell acc={acc} />
        </TableCell>
      ) : null}
      {columns.importTime ? (
        <TableCell className="whitespace-nowrap text-[13px] tabular-nums text-muted-foreground">{formatBeijingTime(acc.created_at)}</TableCell>
      ) : null}
      {columns.updatedAt ? (
        <TableCell className="whitespace-nowrap text-[13px] tabular-nums text-muted-foreground">{formatRelativeTime(acc.updated_at)}</TableCell>
      ) : null}
      <TableCell data-account-actions className="text-right">
        <div className="flex items-center justify-end gap-0.5">
          <div className="hidden items-center gap-0.5 @4xl/kiro-accounts:flex">
            <Button variant="ghost" size="icon-sm" className="size-8" onClick={onEdit} title={t("kiro.editTitle")} aria-label={t("kiro.editTitle")}>
              <Pencil className="size-3.5" />
            </Button>
            <Button variant="ghost" size="icon-sm" className="size-8" onClick={onUsage} title={t("accounts.usageDetail")} aria-label={t("accounts.usageDetail")}>
              <BarChart3 className="size-3.5" />
            </Button>
            <Button variant="ghost" size="icon-sm" className="size-8" onClick={onTest} title={t("accounts.testConnection")} aria-label={t("accounts.testConnection")}>
              <Zap className="size-3.5" />
            </Button>
          </div>
          <HeaderActionMenu label={t("accounts.rowActions")} icon={<MoreHorizontal className="size-3.5" />} align="end" compact items={rowMenuItems} triggerVariant="ghost" />
        </div>
      </TableCell>
    </TableRow>
  );
}

// Kiro7dDetail 近 7 天网关侧请求与 token 明细（与 Codex 用量列 7d 下方的 req / tok 同口径）。
function Kiro7dDetail({ acc }: { acc: AccountRow }) {
  const { t } = useTranslation();
  const detail = acc.usage_7d_detail;
  if (!detail || ((detail.requests ?? 0) === 0 && (detail.tokens ?? 0) === 0)) return null;
  return (
    <div className="text-[11px] font-medium text-muted-foreground">
      7d · {formatCompactNum(detail.requests)} {t("accounts.usageReqUnit")} / {formatCompactNum(detail.tokens)} {t("accounts.usageTokUnit")}
    </div>
  );
}

// KiroProviderSection 详情侧栏的 Kiro 账号信息：凭据形态、订阅与 credit 生命周期、模型目录与最近同步。
function KiroProviderSection({ acc, busy, onSync }: { acc: AccountRow; busy: boolean; onSync: () => void }) {
  const { t } = useTranslation();
  const usage = acc.kiro_usage;
  const plan = kiroPlanBadge(acc.kiro_plan, usage?.subscription_title || acc.plan_type);
  const row = (label: string, value: ReactNode) => (
    <div className="flex justify-between gap-3">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className="min-w-0 break-all text-right">{value}</span>
    </div>
  );
  const time = (iso?: string) => (iso ? formatBeijingTime(iso) : t("kiro.provider.unknown"));
  return (
    <section className="space-y-2.5">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{t("kiro.provider.title")}</h3>
        <Button type="button" variant="ghost" size="xs" className="h-7 text-[11px]" disabled={busy} onClick={onSync}>
          <RefreshCw className={cn("size-3", busy && "animate-spin")} />
          {t("kiro.sync")}
        </Button>
      </div>
      <div className="space-y-2 rounded-xl border border-violet-200/70 bg-violet-50/40 p-3 text-xs dark:border-violet-900/60 dark:bg-violet-950/20">
        {row(t("kiro.provider.authKind"), kiroAuthLabel(acc, t))}
        {row(t("kiro.provider.plan"), plan ? <span className={cn("inline-flex items-center rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset", plan.tone)}>{plan.label}</span> : "-")}
        {usage?.subscription_type ? row(t("kiro.provider.subscriptionType"), <span className="font-mono text-[10px]">{usage.subscription_type}</span>) : null}
        {row(t("kiro.provider.resetAt"), time(usage?.next_reset_at))}
        {usage && usage.bonus_limit + usage.trial_limit > 0 ? row(t("kiro.provider.creditsExpire"), time(usage.credits_expire_at)) : null}
        {usage?.overage_status ? row(t("kiro.provider.overage"), usage.overage_status) : null}
        {row(t("kiro.provider.models"), acc.models?.length ? t("kiro.modelCount", { count: acc.models.length }) : t("kiro.provider.unknown"))}
        {row(t("kiro.provider.lastSync"), usage?.fetched_at ? formatRelativeShort(usage.fetched_at, t) : t("kiro.quotaUnknown"))}
      </div>
    </section>
  );
}

function KiroSchedulerChip({
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
  const dotStyle = { neutral: "bg-slate-400", success: "bg-emerald-500", warning: "bg-amber-500", danger: "bg-red-500" }[tone];
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn("inline-flex h-6 shrink-0 items-center gap-1.5 rounded-full px-2 text-[11px] font-medium transition-shadow", toneStyle, active ? "ring-2 ring-primary/40" : "hover:ring-1 hover:ring-border")}
    >
      <span className={cn("size-1.5 rounded-full", dotStyle)} />
      <span>{label}</span>
      <span className="tabular-nums">{value}</span>
    </button>
  );
}

function KiroPriorityBadge({ acc }: { acc: AccountRow }) {
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
    <span className={cn("inline-flex shrink-0 items-center rounded-md border px-1.5 py-0.5 text-[10px] font-semibold tabular-nums", tone)} title={t("accounts.schedulerPriorityBadgeTitle", { value })}>
      P {value}
    </span>
  );
}

function KiroChipList({ items }: { items: string[] }) {
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
      {hidden > 0 ? <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">+{hidden}</span> : null}
    </div>
  );
}

function KiroGroupChipList({ groups, onClick, emptyLabel }: { groups: AccountGroup[]; onClick: () => void; emptyLabel: string }) {
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
      {hidden > 0 ? <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">+{hidden}</span> : null}
      {groups.length > 0 ? <Pencil className="mt-0.5 size-3 text-muted-foreground opacity-60 transition-opacity group-hover:opacity-100" /> : null}
    </button>
  );
}

function KiroTodayStatsCell({ acc }: { acc: AccountRow }) {
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
      <span
        className={cn(
          "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[11px] tabular-nums ring-1 ring-inset",
          billed > 0 ? "bg-emerald-500/10 font-medium text-emerald-700 ring-emerald-500/20 dark:text-emerald-400" : "bg-slate-500/10 text-slate-500 ring-slate-500/20 dark:text-slate-400",
        )}
        title={t("accounts.accountBilledLabel")}
      >
        <Coins className={cn("size-3 shrink-0", billed > 0 ? "text-emerald-500" : "opacity-50")} aria-hidden />
        {billed > 0 ? `$${billed < 0.01 ? "<0.01" : billed.toFixed(2)}` : "$0.00"}
      </span>
    </div>
  );
}

// KiroBilledCell 成本列：近 7 天网关侧按模型定价估算的账号成本（与 Codex 成本列 7d 胶囊同口径）。
function KiroBilledCell({ acc }: { acc: AccountRow }) {
  const { t } = useTranslation();
  const billed = acc.usage_7d_detail?.account_billed;
  if (typeof billed !== "number") return <span className="text-[12px] text-muted-foreground">-</span>;
  return (
    <span
      className="inline-flex items-center gap-1 whitespace-nowrap rounded-md bg-slate-500/10 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-slate-700 ring-1 ring-inset ring-slate-500/20 dark:text-slate-300"
      title={t("accounts.billedGatewayHint")}
    >
      <Wallet className="size-3 shrink-0" aria-hidden />
      7d: ${billed.toFixed(2)}
    </span>
  );
}

function KiroColumnSettingsMenu({ columns, onToggle, onReset }: { columns: KiroColVisibility; onToggle: (column: KiroCol) => void; onReset: () => void }) {
  const { t } = useTranslation();
  const labelFor: Record<KiroCol, string> = {
    tags: t("accounts.tagsLabel"),
    groups: t("accounts.groupsLabel"),
    proxy: t("accounts.proxyColumn"),
    priority: t("accounts.schedulerPriorityColumn"),
    plan: t("accounts.plan"),
    status: t("accounts.status"),
    today: t("accounts.todayStats"),
    requests: t("accounts.requests"),
    usage: t("kiro.colQuota"),
    cost: t("accounts.billed"),
    importTime: t("accounts.importTime"),
    updatedAt: t("accounts.updatedAt"),
  };
  return (
    <ColumnSettingsMenu
      columns={columns}
      columnOrder={KIRO_TOGGLE_COLUMNS}
      labels={labelFor}
      onToggle={onToggle}
      onReset={onReset}
      title={t("accounts.columnSettings")}
      resetTitle={t("accounts.columnReset")}
    />
  );
}

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

  const createGroupInline = useCallback(async (name: string): Promise<number | null> => {
    try {
      const color = ACCOUNT_GROUP_COLORS[groups.length % ACCOUNT_GROUP_COLORS.length];
      const res = await api.createAccountGroup({ name: name.trim(), channel: "kiro", color });
      await onGroupsChanged?.();
      return res.id ?? null;
    } catch (error) {
      showToast(getErrorMessage(error), "error");
      return null;
    }
  }, [groups.length, onGroupsChanged, showToast]);

  const save = useCallback(async () => {
    setBusy(true);
    try {
      await api.batchUpdateAccounts({ ids: [account.id], group_ids: selected });
      showToast(t("kiro.groupsUpdated"), "success");
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
      title={t("kiro.assignGroupsTitle")}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void save()} disabled={busy}>{t("common.save")}</Button>
        </div>
      }
    >
      <div className="space-y-2">
        <p className="text-xs text-muted-foreground">{accountLabel(account)}</p>
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

// KiroEditAccountModal 编辑账号配置：只放对 Kiro 账号真实生效的通用调度项。
// Kiro 按积分计费、没有 5h/7d 窗口，因此不提供按窗口的自动暂停与请求次数限流。
function KiroEditAccountModal({
  account,
  proxies,
  apiKeys,
  tagOptions,
  onClose,
  onSaved,
}: {
  account: AccountRow;
  proxies: ProxyRow[];
  apiKeys: APIKeyRow[];
  tagOptions: string[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [proxyUrl, setProxyUrl] = useState(account.proxy_url ?? "");
  const [tags, setTags] = useState<string[]>(account.tags ?? []);
  const [priority, setPriority] = useState(account.scheduler_priority != null ? String(account.scheduler_priority) : "");
  const [scoreBias, setScoreBias] = useState(account.score_bias_override != null ? String(account.score_bias_override) : "");
  const [concurrency, setConcurrency] = useState(account.base_concurrency_override != null ? String(account.base_concurrency_override) : "");
  const [skipWarmTier, setSkipWarmTier] = useState(account.skip_warm_tier === true);
  const [keepConcurrency, setKeepConcurrency] = useState(account.keep_concurrency_on_degrade === true);
  const [allowedKeys, setAllowedKeys] = useState<number[]>(() => {
    const existing = new Set(apiKeys.map((key) => key.id));
    return (account.allowed_api_key_ids ?? []).filter((id) => apiKeys.length === 0 || existing.has(id));
  });
  const [busy, setBusy] = useState(false);

  const parseInteger = (value: string, min: number, max: number): number | null | undefined => {
    const s = value.trim();
    if (!s) return null;
    const n = Number(s);
    return Number.isInteger(n) && n >= min && n <= max ? n : undefined;
  };

  const save = useCallback(async () => {
    const parsedPriority = parseInteger(priority, -100, 100);
    const parsedScore = parseInteger(scoreBias, -200, 200);
    const parsedConcurrency = parseInteger(concurrency, 1, 1_000_000);
    if (parsedPriority === undefined) return showToast(t("accounts.schedulerPriorityRange"), "error");
    if (parsedScore === undefined) return showToast(t("accounts.schedulerScoreRange"), "error");
    if (parsedConcurrency === undefined) return showToast(t("accounts.schedulerConcurrencyRange"), "error");
    setBusy(true);
    try {
      await api.updateAccountScheduler(account.id, {
        proxy_url: proxyUrl.trim() || null,
        tags,
        scheduler_priority: parsedPriority,
        score_bias_override: parsedScore,
        base_concurrency_override: parsedConcurrency,
        skip_warm_tier: skipWarmTier,
        keep_concurrency_on_degrade: keepConcurrency,
        allowed_api_key_ids: allowedKeys,
      });
      showToast(t("kiro.saved"), "success");
      onSaved();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  }, [account.id, allowedKeys, concurrency, keepConcurrency, onSaved, priority, proxyUrl, scoreBias, showToast, skipWarmTier, t, tags]);

  const field = (label: string, node: ReactNode, hint?: string) => (
    <div className="space-y-1">
      <span className="text-xs font-semibold text-muted-foreground">{label}</span>
      {node}
      {hint ? <p className="text-[10px] leading-tight text-muted-foreground/70">{hint}</p> : null}
    </div>
  );
  const toggle = (label: string, hint: string, checked: boolean, onChange: (next: boolean) => void) => (
    <label className="flex cursor-pointer items-start gap-2.5">
      <input type="checkbox" className="mt-0.5 size-4 shrink-0 cursor-pointer accent-primary" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      <span className="space-y-0.5">
        <span className="block text-xs font-semibold text-foreground">{label}</span>
        <span className="block text-[10px] leading-tight text-muted-foreground/70">{hint}</span>
      </span>
    </label>
  );
  const section = (title: string, children: ReactNode) => (
    <div className="space-y-3 rounded-lg border border-border/60 p-3">
      <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{title}</div>
      {children}
    </div>
  );

  return (
    <Modal
      show
      onClose={onClose}
      title={t("kiro.editTitle")}
      contentClassName="sm:max-w-[640px]"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void save()} disabled={busy}>
            {busy ? <Loader2 className="size-3.5 animate-spin" /> : null}
            {t("common.save")}
          </Button>
        </div>
      }
    >
      <div className="max-h-[70vh] space-y-4 overflow-y-auto pr-1">
        <p className="text-xs text-muted-foreground">
          {accountLabel(account)} · {kiroAuthLabel(account, t)}
          {kiroPlanBadge(account.kiro_plan, account.kiro_usage?.subscription_title || account.plan_type) ? ` · ${kiroPlanBadge(account.kiro_plan, account.kiro_usage?.subscription_title || account.plan_type)?.label}` : ""}
        </p>
        {section(t("accounts.categoryDispatch"), (
          <div className="grid gap-3 sm:grid-cols-3">
            {field(t("accounts.schedulerScoreLabel"), <Input value={scoreBias} onChange={(e) => setScoreBias(e.target.value)} placeholder="0" inputMode="numeric" />, t("kiro.scoreBiasHint"))}
            {field(t("accounts.schedulerConcurrencyLabel"), <Input value={concurrency} onChange={(e) => setConcurrency(e.target.value)} placeholder={t("accounts.schedulerConcurrencyAuto")} inputMode="numeric" />, t("accounts.schedulerConcurrencyHint"))}
            {field(t("accounts.schedulerPriorityTitle"), <Input value={priority} onChange={(e) => setPriority(e.target.value)} placeholder="0" inputMode="numeric" />, t("accounts.schedulerPriorityDefault"))}
          </div>
        ))}
        {section(t("accounts.categorySafety"), (
          <div className="space-y-3">
            {toggle(t("accounts.schedulerSkipWarmLabel"), t("accounts.schedulerSkipWarmHint"), skipWarmTier, setSkipWarmTier)}
            {toggle(t("accounts.schedulerKeepConcurrencyLabel"), t("kiro.keepConcurrencyHint"), keepConcurrency, setKeepConcurrency)}
          </div>
        ))}
        {section(t("accounts.categoryNetwork"), (
          <div className="space-y-3">
            {field(
              t("accounts.allowedAPIKeysLabel"),
              <APIKeyMultiSelect
                options={apiKeys}
                value={allowedKeys}
                disabled={apiKeys.length === 0}
                onChange={setAllowedKeys}
                allLabel={t("accounts.allowedAPIKeysAll")}
                selectedLabel={t("accounts.allowedAPIKeysSelected", { count: allowedKeys.length })}
                placeholder={t("accounts.allowedAPIKeysPlaceholder")}
                emptyLabel={t("accounts.allowedAPIKeysNoOptions")}
                emptyHint={t("accounts.allowedAPIKeysNoOptionsHint")}
              />,
              t("accounts.allowedAPIKeysHint"),
            )}
            {field(t("kiro.proxyField"), <ProxyField value={proxyUrl} onChange={setProxyUrl} proxies={proxies} label="" />, t("kiro.proxyHint"))}
          </div>
        ))}
        {field(t("accounts.tagsLabel"), <ChipInput value={tags} onChange={setTags} options={tagOptions} placeholder={t("kiro.tagsPlaceholder")} maxVisible={8} />)}
      </div>
    </Modal>
  );
}
