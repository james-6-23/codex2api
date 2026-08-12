import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import type { ReactNode } from "react";
import {
  Copy,
  ExternalLink,
  Link2,
  Loader2,
  Pencil,
  Plus,
  Power,
  PowerOff,
  RefreshCw,
  RotateCcw,
  Search,
  Trash2,
  Upload,
  X,
} from "lucide-react";
import { api } from "../api";
import type { ProxyRow } from "../api";
import type {
  AccountListSummary,
  AccountRow,
  AccountHealthBucket,
  ClaudeImportItem,
} from "../types";
import { ProxyPoolSelect } from "../components/ProxyPoolSelect";
import AccountHealthBar from "../components/AccountHealthBar";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import { CompactStat } from "../components/CompactStat";
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
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import {
  DEFAULT_PAGE_SIZE_OPTIONS,
  usePersistedPageSize,
} from "../hooks/usePersistedPageSize";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import {
  CLAUDE_KNOWN_PLAN_KEYS,
  resolveAccountClaudePlan,
  type ClaudePlanFilter,
} from "../lib/claudePlan";
import { cn } from "@/lib/utils";

// 官方默认上游；base_url 等于默认值时列表不展示（无信息量），只有自定义中转才显示。
const CLAUDE_DEFAULT_HOSTS = new Set(["api.anthropic.com"]);

// Anthropic 的托管回调页：授权完成后 code 显示在这个页面上，用户复制粘回管理台。
const CLAUDE_CALLBACK_HINT_HOST = "platform.claude.com";

type StatusFilter = "all" | "active" | "rate_limited" | "disabled" | "error";

// 与前端状态徽章一致的限流状态集合。
const CLAUDE_LIMITED_STATUSES = new Set([
  "usage_limited",
  "usage_exhausted",
  "rate_limited",
  "rate_limited_5h",
  "rate_limited_7d",
]);

function shortHost(raw?: string | null): string {
  const value = (raw ?? "").trim();
  if (!value) return "";
  let host = "";
  try {
    const url = new URL(value.includes("://") ? value : `https://${value}`);
    host =
      url.host +
      (url.pathname && url.pathname !== "/"
        ? url.pathname.replace(/\/$/, "")
        : "");
  } catch {
    host = value.replace(/^https?:\/\//, "").replace(/\/$/, "");
  }
  return CLAUDE_DEFAULT_HOSTS.has(host) ? "" : host;
}

function parseModelTokens(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

function mergeModels(existing: string[], incoming: string[]): string[] {
  const seen = new Set(existing.map((m) => m.toLowerCase()));
  const merged = [...existing];
  for (const token of incoming) {
    if (!seen.has(token.toLowerCase())) {
      seen.add(token.toLowerCase());
      merged.push(token);
    }
  }
  return merged;
}

function accountLabel(account: AccountRow): string {
  return account.name || account.email || `#${account.id}`;
}

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

// 套餐徽章：付费档（Pro / Max / Team / Enterprise）琥珀，Free 绿色，未知留占位。
function ClaudePlanBadge({ account }: { account: AccountRow }) {
  const plan = resolveAccountClaudePlan(account);
  if (!plan) {
    return <span className="text-[12px] text-muted-foreground">—</span>;
  }
  const tone = plan.paid
    ? "bg-amber-500/10 text-amber-700 ring-amber-600/20 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/20"
    : "bg-emerald-500/10 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/20 dark:text-emerald-300 dark:ring-emerald-400/20";
  return (
    <span
      className={cn(
        "inline-flex items-center whitespace-nowrap rounded-md px-2 py-1 text-xs font-semibold ring-1 ring-inset",
        tone,
      )}
      title={account.claude_rate_limit_tier || undefined}
    >
      {plan.display}
    </span>
  );
}

function ModelChips({
  models,
  emptyLabel,
}: {
  models?: string[];
  emptyLabel: string;
}) {
  if (!models?.length) {
    return (
      <span className="text-[12px] text-muted-foreground">{emptyLabel}</span>
    );
  }
  const visible = models.slice(0, 3);
  const hidden = models.length - visible.length;
  return (
    <div className="flex flex-wrap items-center gap-1" title={models.join(", ")}>
      {visible.map((model) => (
        <span
          key={model}
          className="inline-flex max-w-[10rem] items-center truncate rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground"
        >
          {model}
        </span>
      ))}
      {hidden > 0 ? (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-semibold text-muted-foreground">
          +{hidden}
        </span>
      ) : null}
    </div>
  );
}

interface EditFormState {
  models: string[];
  base_url: string;
  model_mapping: string;
  proxy_url: string;
}

const EMPTY_EDIT_FORM: EditFormState = {
  models: [],
  base_url: "",
  model_mapping: "",
  proxy_url: "",
};

function ClaudeAccounts({
  headerSlot,
}: {
  // headerSlot 由账号管理页注入 Codex/Claude/Grok 顶部切换器，渲染在标题旁。
  headerSlot?: ReactNode;
} = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [healthBars, setHealthBars] = useState<
    Record<string, AccountHealthBucket[]>
  >({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [totalAccounts, setTotalAccounts] = useState(0);
  const [serverSummary, setServerSummary] =
    useState<AccountListSummary | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);
  const requestAbortRef = useRef<AbortController | null>(null);

  const [searchQuery, setSearchQuery] = useState("");
  const [debouncedSearchQuery, setDebouncedSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [planFilter, setPlanFilter] = useState<ClaudePlanFilter>("all");
  const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS;
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize(
    "claude-accounts",
    20,
    pageSizeOptions,
  );

  // 代理池条目：账号表单「从代理池选择」下拉的数据源；加载失败静默留空。
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

  // 添加账号：授权链接 → 用户在浏览器完成授权 → 粘回回调页显示的 code。
  // session 只在后端内存里活 15 分钟（code_verifier 随它一起），过期必须重新生成链接。
  const [showAdd, setShowAdd] = useState(false);
  const [addForm, setAddForm] = useState({
    name: "",
    base_url: "",
    proxy_url: "",
    models: [] as string[],
  });
  const [addModelDraft, setAddModelDraft] = useState("");
  const [authSession, setAuthSession] = useState<{
    session_id: string;
    auth_url: string;
    expires_in: number;
  } | null>(null);
  const [authCode, setAuthCode] = useState("");
  const [generating, setGenerating] = useState(false);
  const [exchanging, setExchanging] = useState(false);

  // 凭据文件导入：粘贴 ~/.claude/.credentials.json 的内容，或直接选文件（可多选）。
  // 后端会逐个用 refresh_token 换新 AT 再探针身份，所以这里只等结果、不做解析。
  const [showImport, setShowImport] = useState(false);
  const [importContent, setImportContent] = useState("");
  const [importProxy, setImportProxy] = useState("");
  const [importBusy, setImportBusy] = useState(false);
  const [importResult, setImportResult] = useState<{
    total: number;
    imported: number;
    failed: number;
    items: ClaudeImportItem[];
  } | null>(null);
  const importFileInputRef = useRef<HTMLInputElement | null>(null);

  const [editAccount, setEditAccount] = useState<AccountRow | null>(null);
  const [editForm, setEditForm] = useState<EditFormState>(EMPTY_EDIT_FORM);
  const [editModelDraft, setEditModelDraft] = useState("");
  const [editSubmitting, setEditSubmitting] = useState(false);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDebouncedSearchQuery(searchQuery);
      setPage(1);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [searchQuery]);

  const reload = useCallback(async () => {
    requestAbortRef.current?.abort();
    const controller = new AbortController();
    requestAbortRef.current = controller;
    setLoading(true);
    try {
      const res = await api.getAccountsPage(
        {
          channel: "claude",
          page,
          pageSize,
          search: debouncedSearchQuery,
          status: statusFilter,
        },
        controller.signal,
      );
      if (controller.signal.aborted) return;
      // 服务端已按渠道过滤，claude_api 再兜一层：旧后端会忽略未认识的 channel 值。
      const claudeAccounts = (res.accounts ?? []).filter((a) => a.claude_api);
      setAccounts(claudeAccounts);
      setTotalAccounts(res.total ?? 0);
      setServerSummary(res.summary ?? null);
      if (res.page !== page) setPage(res.page);
      setError(null);
      setLoading(false);
      const pageIDs = claudeAccounts.map((account) => account.id);
      void api
        .getAccountHealthBars(pageIDs)
        .then((bars) => {
          if (!controller.signal.aborted) setHealthBars(bars.buckets ?? {});
        })
        .catch(() => undefined);
      void api
        .getAccountPageStats(pageIDs, controller.signal)
        .then((response) => {
          if (controller.signal.aborted) return;
          setAccounts((current) =>
            current.map((account) => {
              const stats = response.stats[String(account.id)];
              return stats ? { ...account, ...stats } : account;
            }),
          );
        })
        .catch(() => undefined);
    } catch (err) {
      if (controller.signal.aborted) return;
      const message = getErrorMessage(err);
      setError(message);
      showToast(message, "error");
      setLoading(false);
    }
  }, [debouncedSearchQuery, page, pageSize, showToast, statusFilter]);

  useEffect(() => {
    void reload();
  }, [reload]);

  useEffect(() => () => requestAbortRef.current?.abort(), []);

  // 套餐筛选在本地做：后端的 plan 过滤按 Codex/Grok 的档位键设计，不认 Claude 的键。
  const visibleAccounts = useMemo(() => {
    if (planFilter === "all") return accounts;
    return accounts.filter((account) => {
      const plan = resolveAccountClaudePlan(account);
      if (planFilter === "other") {
        return (
          !plan ||
          !CLAUDE_KNOWN_PLAN_KEYS.includes(
            plan.key as (typeof CLAUDE_KNOWN_PLAN_KEYS)[number],
          )
        );
      }
      return plan?.key === planFilter;
    });
  }, [accounts, planFilter]);

  const stats = {
    total: serverSummary?.total ?? totalAccounts,
    active: serverSummary?.active ?? 0,
    rateLimited: serverSummary?.rate_limited ?? 0,
    errorOnly: serverSummary?.error ?? 0,
    disabled: serverSummary?.disabled ?? 0,
  };

  const totalPages = Math.max(1, Math.ceil(totalAccounts / pageSize));
  const currentPage = Math.min(page, totalPages);
  const hasActiveFilters = Boolean(
    debouncedSearchQuery || statusFilter !== "all" || planFilter !== "all",
  );

  const resetAddFlow = useCallback(() => {
    setAddForm({ name: "", base_url: "", proxy_url: "", models: [] });
    setAddModelDraft("");
    setAuthSession(null);
    setAuthCode("");
    setGenerating(false);
    setExchanging(false);
  }, []);

  const handleGenerateAuthURL = async () => {
    setGenerating(true);
    try {
      const res = await api.generateClaudeAuthURL({
        name: addForm.name.trim() || undefined,
        base_url: addForm.base_url.trim() || undefined,
        models: addForm.models.length ? addForm.models : undefined,
        proxy_url: addForm.proxy_url.trim() || undefined,
      });
      setAuthSession({
        session_id: res.session_id,
        auth_url: res.auth_url,
        expires_in: res.expires_in,
      });
      // 直接开新标签：授权要在用户自己的浏览器会话里完成，管理台代不了。
      window.open(res.auth_url, "_blank", "noopener,noreferrer");
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setGenerating(false);
    }
  };

  const handleExchangeCode = async () => {
    if (!authSession || !authCode.trim()) return;
    setExchanging(true);
    try {
      const res = await api.exchangeClaudeOAuthCode({
        session_id: authSession.session_id,
        code: authCode.trim(),
        name: addForm.name.trim() || undefined,
        proxy_url: addForm.proxy_url.trim() || undefined,
      });
      showToast(
        res.email
          ? t("claude.addSuccessWithEmail", { email: res.email })
          : t("claude.addSuccess"),
      );
      setShowAdd(false);
      resetAddFlow();
      void reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setExchanging(false);
    }
  };

  const runImport = async (files: string[], content: string) => {
    setImportBusy(true);
    setImportResult(null);
    try {
      const res = await api.importClaudeAccounts({
        files: files.length ? files : undefined,
        content: content.trim() || undefined,
        proxy_url: importProxy.trim() || undefined,
      });
      setImportResult(res);
      if (res.imported > 0) {
        showToast(
          t("claude.importDone", { imported: res.imported, total: res.total }),
        );
        setImportContent("");
        void reload();
      } else {
        showToast(t("claude.importNothing"), "error");
      }
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setImportBusy(false);
    }
  };

  const handleImportFiles = async (fileList: FileList | null) => {
    if (!fileList?.length) return;
    const contents = await Promise.all(
      Array.from(fileList).map((file) => file.text()),
    );
    await runImport(contents, "");
  };

  const openEdit = (account: AccountRow) => {
    const populate = (loaded: AccountRow) => {
      setEditAccount(loaded);
      setEditForm({
        models: loaded.models ?? [],
        base_url: loaded.base_url ?? "",
        model_mapping: loaded.model_mapping ?? "",
        proxy_url: loaded.proxy_url ?? "",
      });
      setEditModelDraft("");
    };
    // 列表行是精简投影，model_mapping 等详情字段只在详情接口里；不补一次会把它清空。
    if (account.detail_loaded) {
      populate(account);
      return;
    }
    void api
      .getAccount(account.id)
      .then(populate)
      .catch((err) => showToast(getErrorMessage(err), "error"));
  };

  const handleSaveEdit = async () => {
    if (!editAccount) return;
    setEditSubmitting(true);
    try {
      await api.updateClaudeAccount(editAccount.id, {
        base_url: editForm.base_url.trim(),
        models: editForm.models,
        model_mapping: editForm.model_mapping.trim(),
        proxy_url: editForm.proxy_url.trim(),
      });
      showToast(t("claude.editSaved"));
      setEditAccount(null);
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setEditSubmitting(false);
    }
  };

  const handleRefresh = async (account: AccountRow) => {
    setBusyId(account.id);
    try {
      await api.refreshAccount(account.id);
      showToast(t("claude.refreshDone"));
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleToggleEnabled = async (account: AccountRow) => {
    setBusyId(account.id);
    try {
      await api.toggleAccountEnabled(account.id, account.enabled === false);
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleResetStatus = async (account: AccountRow) => {
    setBusyId(account.id);
    try {
      await api.resetAccountStatus(account.id);
      showToast(t("claude.resetStatusDone"));
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleDelete = async (account: AccountRow) => {
    const ok = await confirm({
      title: t("claude.deleteTitle"),
      description: t("claude.deleteDesc", { account: accountLabel(account) }),
      confirmText: t("claude.deleteConfirm"),
      tone: "destructive",
    });
    if (!ok) return;
    setBusyId(account.id);
    try {
      await api.deleteAccount(account.id);
      showToast(t("claude.deleteDone"));
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const clearFilters = () => {
    setSearchQuery("");
    setStatusFilter("all");
    setPlanFilter("all");
    setPage(1);
  };

  return (
    <div className="@container/claude-accounts">
      {confirmDialog}
      <StateShell
        variant="page"
        loading={loading && accounts.length === 0}
        error={accounts.length === 0 ? error : null}
        onRetry={() => void reload()}
        loadingTitle={t("claude.loadingTitle")}
        loadingDescription={t("claude.loadingDesc")}
        errorTitle={t("claude.errorTitle")}
      >
        <PageHeader
          title={t("claude.pageTitle")}
          description={t("claude.pageSubtitle")}
          onRefresh={() => void reload()}
          hideTitle
          titleAdornment={headerSlot}
          actions={
            <div className="flex flex-wrap items-center gap-1.5">
              <Button
                size="sm"
                onClick={() => {
                  resetAddFlow();
                  setShowAdd(true);
                }}
              >
                <Plus className="size-3.5" />
                {t("claude.addAccount")}
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={importBusy}
                onClick={() => {
                  setImportResult(null);
                  setShowImport(true);
                }}
              >
                {importBusy ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <Upload className="size-3.5" />
                )}
                {t("claude.importAccounts")}
              </Button>
            </div>
          }
        />

        <div className="grid grid-cols-2 gap-2 sm:gap-3 @2xl/claude-accounts:grid-cols-4">
          <CompactStat
            label={t("claude.statTotal")}
            value={stats.total}
            tone="neutral"
          />
          <CompactStat
            label={t("claude.statActive")}
            value={stats.active}
            tone="success"
          />
          <CompactStat
            label={t("claude.statRateLimited")}
            value={stats.rateLimited}
            tone="warning"
          />
          <CompactStat
            label={t("claude.statError")}
            value={stats.errorOnly}
            tone="danger"
            details={[
              { label: t("claude.statDisabled"), value: stats.disabled },
            ]}
          />
        </div>

        <div className="mt-4 flex flex-wrap items-center gap-2">
          <div className="relative min-w-[13rem] flex-1">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input
              className="pl-8"
              value={searchQuery}
              placeholder={t("claude.searchPlaceholder")}
              onChange={(event) => setSearchQuery(event.target.value)}
            />
          </div>
          <Select
            className="w-36"
            compact
            value={statusFilter}
            onValueChange={(value) => {
              setStatusFilter(value as StatusFilter);
              setPage(1);
            }}
            options={[
              { value: "all", label: t("claude.filterStatusAll") },
              { value: "active", label: t("claude.filterStatusActive") },
              {
                value: "rate_limited",
                label: t("claude.filterStatusRateLimited"),
              },
              { value: "error", label: t("claude.filterStatusError") },
              { value: "disabled", label: t("claude.filterStatusDisabled") },
            ]}
          />
          <Select
            className="w-36"
            compact
            value={planFilter}
            onValueChange={(value) => setPlanFilter(value as ClaudePlanFilter)}
            options={[
              { value: "all", label: t("claude.filterPlanAll") },
              { value: "max20", label: t("claude.planMax20") },
              { value: "max5", label: t("claude.planMax5") },
              { value: "pro", label: t("claude.planPro") },
              { value: "team", label: t("claude.planTeam") },
              { value: "enterprise", label: t("claude.planEnterprise") },
              { value: "free", label: t("claude.planFree") },
              { value: "other", label: t("claude.planOther") },
            ]}
          />
          {hasActiveFilters ? (
            <Button variant="ghost" size="sm" onClick={clearFilters}>
              <X className="size-3.5" />
              {t("claude.clearFilters")}
            </Button>
          ) : null}
        </div>

        {visibleAccounts.length === 0 ? (
          <div className="mt-6 rounded-xl border border-dashed border-border px-6 py-12 text-center">
            <p className="text-base font-semibold text-foreground">
              {hasActiveFilters
                ? t("claude.noMatchTitle")
                : t("claude.emptyTitle")}
            </p>
            <p className="mt-1 text-sm text-muted-foreground">
              {hasActiveFilters
                ? t("claude.noMatchDesc")
                : t("claude.emptyDesc")}
            </p>
          </div>
        ) : (
          <div className="mt-4 overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("claude.colAccount")}</TableHead>
                  <TableHead>{t("claude.colPlan")}</TableHead>
                  <TableHead>{t("claude.colStatus")}</TableHead>
                  <TableHead>{t("claude.colModels")}</TableHead>
                  <TableHead>{t("claude.colHealth")}</TableHead>
                  <TableHead>{t("claude.colUpdated")}</TableHead>
                  <TableHead className="text-right">
                    {t("claude.colActions")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visibleAccounts.map((account) => {
                  const busy = busyId === account.id;
                  const customHost = shortHost(account.base_url);
                  const rateLimited =
                    CLAUDE_LIMITED_STATUSES.has(
                      (account.status ?? "").toLowerCase(),
                    ) ||
                    CLAUDE_LIMITED_STATUSES.has(
                      (account.cooldown_reason ?? "").toLowerCase(),
                    );
                  return (
                    <TableRow
                      key={account.id}
                      className={cn(account.enabled === false && "opacity-60")}
                    >
                      <TableCell>
                        <div className="flex flex-col gap-0.5">
                          <span className="font-semibold text-foreground">
                            {accountLabel(account)}
                          </span>
                          {account.email ? (
                            <span className="text-[12px] text-muted-foreground">
                              {account.email}
                            </span>
                          ) : null}
                          {account.claude_organization_name ? (
                            <span className="text-[11px] text-muted-foreground">
                              {account.claude_organization_name}
                            </span>
                          ) : null}
                          {customHost ? (
                            <span className="text-[11px] text-muted-foreground">
                              {customHost}
                            </span>
                          ) : null}
                        </div>
                      </TableCell>
                      <TableCell>
                        <ClaudePlanBadge account={account} />
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-col items-start gap-1">
                          <StatusBadge
                            status={account.status}
                            errorMessage={account.error_message}
                          />
                          {rateLimited && account.cooldown_until ? (
                            <span className="text-[11px] text-muted-foreground">
                              {t("claude.cooldownUntil", {
                                time: formatRelativeTime(
                                  account.cooldown_until,
                                ),
                              })}
                            </span>
                          ) : null}
                        </div>
                      </TableCell>
                      <TableCell>
                        <ModelChips
                          models={account.models}
                          emptyLabel={t("claude.modelsAll")}
                        />
                      </TableCell>
                      <TableCell className="min-w-[10rem]">
                        <AccountHealthBar
                          buckets={healthBars[String(account.id)]}
                        />
                      </TableCell>
                      <TableCell>
                        <span
                          className="whitespace-nowrap text-[12px] text-muted-foreground"
                          title={formatBeijingTime(account.updated_at)}
                        >
                          {formatRelativeTime(account.updated_at)}
                        </span>
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center justify-end gap-1">
                          <Button
                            variant="ghost"
                            size="icon"
                            disabled={busy}
                            title={t("claude.actionRefresh")}
                            onClick={() => void handleRefresh(account)}
                          >
                            {busy ? (
                              <Loader2 className="size-3.5 animate-spin" />
                            ) : (
                              <RefreshCw className="size-3.5" />
                            )}
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            disabled={busy}
                            title={t("claude.actionEdit")}
                            onClick={() => openEdit(account)}
                          >
                            <Pencil className="size-3.5" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            disabled={busy}
                            title={t("claude.actionResetStatus")}
                            onClick={() => void handleResetStatus(account)}
                          >
                            <RotateCcw className="size-3.5" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            disabled={busy}
                            title={
                              account.enabled === false
                                ? t("claude.actionEnable")
                                : t("claude.actionDisable")
                            }
                            onClick={() => void handleToggleEnabled(account)}
                          >
                            {account.enabled === false ? (
                              <PowerOff className="size-3.5" />
                            ) : (
                              <Power className="size-3.5" />
                            )}
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            disabled={busy}
                            title={t("claude.actionDelete")}
                            onClick={() => void handleDelete(account)}
                          >
                            <Trash2 className="size-3.5 text-destructive" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
        )}

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

      <Modal
        show={showAdd}
        title={t("claude.addTitle")}
        onClose={() => {
          setShowAdd(false);
          resetAddFlow();
        }}
        footer={
          <div className="flex items-center justify-end gap-2">
            <Button
              variant="outline"
              onClick={() => {
                setShowAdd(false);
                resetAddFlow();
              }}
            >
              {t("claude.cancel")}
            </Button>
            {authSession ? (
              <Button
                disabled={exchanging || !authCode.trim()}
                onClick={() => void handleExchangeCode()}
              >
                {exchanging ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : null}
                {t("claude.exchangeSubmit")}
              </Button>
            ) : (
              <Button
                disabled={generating}
                onClick={() => void handleGenerateAuthURL()}
              >
                {generating ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <Link2 className="size-3.5" />
                )}
                {t("claude.generateAuthURL")}
              </Button>
            )}
          </div>
        }
      >
        <div className="space-y-4">
          <p className="rounded-lg border border-border bg-muted/40 px-3 py-2 text-[13px] text-muted-foreground">
            {t("claude.addHint", { host: CLAUDE_CALLBACK_HINT_HOST })}
          </p>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.nameLabel")}
            </label>
            <Input
              value={addForm.name}
              placeholder={t("claude.namePlaceholder")}
              onChange={(event) =>
                setAddForm((f) => ({ ...f, name: event.target.value }))
              }
            />
          </div>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.proxyLabel")}
            </label>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                className="min-w-[14rem] flex-1"
                value={addForm.proxy_url}
                placeholder="http://user:pass@host:port"
                onChange={(event) =>
                  setAddForm((f) => ({ ...f, proxy_url: event.target.value }))
                }
              />
              <ProxyPoolSelect
                proxies={proxyPool}
                onSelect={(url) => setAddForm((f) => ({ ...f, proxy_url: url }))}
              />
            </div>
            <p className="text-[12px] text-muted-foreground">
              {t("claude.proxyHint")}
            </p>
          </div>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.baseURLLabel")}
            </label>
            <Input
              value={addForm.base_url}
              placeholder="https://api.anthropic.com"
              onChange={(event) =>
                setAddForm((f) => ({ ...f, base_url: event.target.value }))
              }
            />
          </div>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.modelsLabel")}
            </label>
            <Input
              value={addModelDraft}
              placeholder="claude-sonnet-4-5, claude-opus-4-1"
              onChange={(event) => setAddModelDraft(event.target.value)}
              onKeyDown={(event) => {
                if (event.key !== "Enter") return;
                event.preventDefault();
                const tokens = parseModelTokens(addModelDraft);
                if (tokens.length === 0) return;
                setAddForm((f) => ({
                  ...f,
                  models: mergeModels(f.models, tokens),
                }));
                setAddModelDraft("");
              }}
            />
            {addForm.models.length > 0 ? (
              <div className="flex flex-wrap gap-1.5">
                {addForm.models.map((model) => (
                  <button
                    key={model}
                    type="button"
                    className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-0.5 text-[12px] font-medium text-muted-foreground hover:text-foreground"
                    onClick={() =>
                      setAddForm((f) => ({
                        ...f,
                        models: f.models.filter((m) => m !== model),
                      }))
                    }
                  >
                    {model}
                    <X className="size-3" />
                  </button>
                ))}
              </div>
            ) : null}
            <p className="text-[12px] text-muted-foreground">
              {t("claude.modelsHint")}
            </p>
          </div>

          {authSession ? (
            <div className="space-y-2 rounded-lg border border-border bg-muted/30 px-3 py-3">
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() =>
                    window.open(
                      authSession.auth_url,
                      "_blank",
                      "noopener,noreferrer",
                    )
                  }
                >
                  <ExternalLink className="size-3.5" />
                  {t("claude.openAuthURL")}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    void copyTextToClipboard(authSession.auth_url)
                      .then(() => showToast(t("claude.authURLCopied")))
                      .catch(() =>
                        showToast(t("claude.authURLCopyFailed"), "error"),
                      );
                  }}
                >
                  <Copy className="size-3.5" />
                  {t("claude.copyAuthURL")}
                </Button>
                <span className="text-[12px] text-muted-foreground">
                  {t("claude.sessionExpiresIn", {
                    minutes: Math.max(
                      1,
                      Math.round(authSession.expires_in / 60),
                    ),
                  })}
                </span>
              </div>
              <div className="space-y-1.5">
                <label className="text-[13px] font-semibold text-foreground">
                  {t("claude.codeLabel")}
                </label>
                <Input
                  value={authCode}
                  placeholder={t("claude.codePlaceholder")}
                  onChange={(event) => setAuthCode(event.target.value)}
                />
                <p className="text-[12px] text-muted-foreground">
                  {t("claude.codeHint")}
                </p>
              </div>
            </div>
          ) : null}
        </div>
      </Modal>

      <Modal
        show={showImport}
        title={t("claude.importTitle")}
        onClose={() => {
          setShowImport(false);
          setImportResult(null);
        }}
        footer={
          <div className="flex items-center justify-end gap-2">
            <Button
              variant="outline"
              onClick={() => {
                setShowImport(false);
                setImportResult(null);
              }}
            >
              {t("claude.cancel")}
            </Button>
            <Button
              variant="outline"
              disabled={importBusy}
              onClick={() => importFileInputRef.current?.click()}
            >
              <Upload className="size-3.5" />
              {t("claude.importPickFiles")}
            </Button>
            <Button
              disabled={importBusy || !importContent.trim()}
              onClick={() => void runImport([], importContent)}
            >
              {importBusy ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : null}
              {t("claude.importSubmit")}
            </Button>
          </div>
        }
      >
        <div className="space-y-4">
          <p className="rounded-lg border border-border bg-muted/40 px-3 py-2 text-[13px] text-muted-foreground">
            {t("claude.importHint")}
          </p>
          <p className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-[13px] text-amber-700 dark:text-amber-300">
            {t("claude.importScopeWarning")}
          </p>

          <input
            ref={importFileInputRef}
            type="file"
            accept=".json,application/json"
            multiple
            className="hidden"
            onChange={(event) => {
              void handleImportFiles(event.target.files);
              event.target.value = "";
            }}
          />

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.importContentLabel")}
            </label>
            <textarea
              className="min-h-[10rem] w-full rounded-lg border border-border bg-background px-3 py-2 font-mono text-[12px] text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={importContent}
              spellCheck={false}
              placeholder={'{"claudeAiOauth":{"accessToken":"…","refreshToken":"…","expiresAt":1786548694331}}'}
              onChange={(event) => setImportContent(event.target.value)}
            />
            <p className="text-[12px] text-muted-foreground">
              {t("claude.importContentHint")}
            </p>
          </div>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.proxyLabel")}
            </label>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                className="min-w-[14rem] flex-1"
                value={importProxy}
                placeholder="http://user:pass@host:port"
                onChange={(event) => setImportProxy(event.target.value)}
              />
              <ProxyPoolSelect
                proxies={proxyPool}
                onSelect={(url) => setImportProxy(url)}
              />
            </div>
          </div>

          {importResult ? (
            <div className="space-y-2 rounded-lg border border-border bg-muted/30 px-3 py-3">
              <p className="text-[13px] font-semibold text-foreground">
                {t("claude.importSummary", {
                  imported: importResult.imported,
                  failed: importResult.failed,
                  total: importResult.total,
                })}
              </p>
              <div className="max-h-48 space-y-1 overflow-y-auto">
                {importResult.items.map((item, index) => (
                  <div
                    key={`${item.id ?? "err"}-${index}`}
                    className="flex flex-wrap items-center gap-2 text-[12px]"
                  >
                    <span
                      className={cn(
                        "inline-flex items-center rounded-md px-1.5 py-0.5 font-semibold",
                        item.ok
                          ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
                          : "bg-destructive/10 text-destructive",
                      )}
                    >
                      {item.ok ? t("claude.importItemOK") : t("claude.importItemFailed")}
                    </span>
                    <span className="text-foreground">
                      {item.email || (item.id ? `#${item.id}` : "—")}
                    </span>
                    {item.error ? (
                      <span className="text-destructive">{item.error}</span>
                    ) : null}
                    {item.warning ? (
                      <span className="text-amber-700 dark:text-amber-300">
                        {item.warning}
                      </span>
                    ) : null}
                  </div>
                ))}
              </div>
            </div>
          ) : null}
        </div>
      </Modal>

      <Modal
        show={editAccount != null}
        title={t("claude.editTitle")}
        onClose={() => setEditAccount(null)}
        footer={
          <div className="flex items-center justify-end gap-2">
            <Button variant="outline" onClick={() => setEditAccount(null)}>
              {t("claude.cancel")}
            </Button>
            <Button
              disabled={editSubmitting}
              onClick={() => void handleSaveEdit()}
            >
              {editSubmitting ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : null}
              {t("claude.editSubmit")}
            </Button>
          </div>
        }
      >
        <div className="space-y-4">
          <p className="rounded-lg border border-border bg-muted/40 px-3 py-2 text-[13px] text-muted-foreground">
            {t("claude.editHint")}
          </p>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.baseURLLabel")}
            </label>
            <Input
              value={editForm.base_url}
              placeholder="https://api.anthropic.com"
              onChange={(event) =>
                setEditForm((f) => ({ ...f, base_url: event.target.value }))
              }
            />
          </div>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.proxyLabel")}
            </label>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                className="min-w-[14rem] flex-1"
                value={editForm.proxy_url}
                placeholder="http://user:pass@host:port"
                onChange={(event) =>
                  setEditForm((f) => ({ ...f, proxy_url: event.target.value }))
                }
              />
              <ProxyPoolSelect
                proxies={proxyPool}
                onSelect={(url) =>
                  setEditForm((f) => ({ ...f, proxy_url: url }))
                }
              />
            </div>
          </div>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.modelsLabel")}
            </label>
            <Input
              value={editModelDraft}
              placeholder="claude-sonnet-4-5, claude-opus-4-1"
              onChange={(event) => setEditModelDraft(event.target.value)}
              onKeyDown={(event) => {
                if (event.key !== "Enter") return;
                event.preventDefault();
                const tokens = parseModelTokens(editModelDraft);
                if (tokens.length === 0) return;
                setEditForm((f) => ({
                  ...f,
                  models: mergeModels(f.models, tokens),
                }));
                setEditModelDraft("");
              }}
            />
            {editForm.models.length > 0 ? (
              <div className="flex flex-wrap gap-1.5">
                {editForm.models.map((model) => (
                  <button
                    key={model}
                    type="button"
                    className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-0.5 text-[12px] font-medium text-muted-foreground hover:text-foreground"
                    onClick={() =>
                      setEditForm((f) => ({
                        ...f,
                        models: f.models.filter((m) => m !== model),
                      }))
                    }
                  >
                    {model}
                    <X className="size-3" />
                  </button>
                ))}
              </div>
            ) : null}
            <p className="text-[12px] text-muted-foreground">
              {t("claude.modelsHint")}
            </p>
          </div>

          <div className="space-y-1.5">
            <label className="text-[13px] font-semibold text-foreground">
              {t("claude.modelMappingLabel")}
            </label>
            <Input
              value={editForm.model_mapping}
              placeholder="claude-3-5-sonnet=claude-sonnet-4-5"
              onChange={(event) =>
                setEditForm((f) => ({
                  ...f,
                  model_mapping: event.target.value,
                }))
              }
            />
            <p className="text-[12px] text-muted-foreground">
              {t("claude.modelMappingHint")}
            </p>
          </div>
        </div>
      </Modal>
    </div>
  );
}

export default memo(ClaudeAccounts);
