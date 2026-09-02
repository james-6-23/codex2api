import { useState, useEffect, useCallback, useMemo, useRef, memo } from "react";
import { useTranslation } from "react-i18next";
import {
  Globe,
  Plus,
  Trash2,
  Play,
  MapPin,
  Loader2,
  Zap,
  Eye,
  EyeOff,
  AlertTriangle,
  Pencil,
  Link2,
  Unlink,
  Scale,
  Search,
  Users,
  Power,
  ShieldCheck,
  RotateCcw,
} from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, type ProxyRow } from "../api";
import type { AccountRow } from "../types";
import ChannelLogo from "../components/ChannelLogo";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import { StatTile } from "../components/StatTile";
import StatusBadge from "../components/StatusBadge";
import {
  DEFAULT_PAGE_SIZE_OPTIONS,
  usePersistedPageSize,
} from "../hooks/usePersistedPageSize";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { postAdminSSE } from "../hooks/useOperationProgress";
import {
  applyProxyTestResult,
  chunkProxyTestIDs,
  getProxyStatusBadgeKind,
  readProxyBatchTestSSE,
} from "../lib/proxyTestState";
import { getErrorMessage } from "../utils/error";
import { cn } from "@/lib/utils";

const PROXY_SCHEMES = ["http:", "https:", "socks5:", "socks5h:"];

type BindFilter = "all" | "unbound" | "this" | "other";
type BindKindFilter = "all" | "codex" | "grok" | "claude";
type StatusFilter = "all" | "enabled" | "disabled" | "error" | "untested";

function accountDisplayName(account: AccountRow): string {
  if (account.openai_responses_api) {
    return account.name || account.email || `#${account.id}`;
  }
  return account.email || account.name || `#${account.id}`;
}

function accountKindKey(account: AccountRow): string {
  if (account.claude_api) return "claude";
  if (account.grok_api) return "grok";
  if (account.openai_responses_api) return "openai";
  if (account.agent_identity) return "agent";
  if (account.at_only) return "at";
  return "codex";
}

function normalizeProxyUrl(url: string | null | undefined): string {
  return (url ?? "").trim();
}

function isAccountBoundToProxy(
  account: AccountRow,
  proxyUrl: string,
): boolean {
  const bound = normalizeProxyUrl(account.proxy_url);
  const target = normalizeProxyUrl(proxyUrl);
  return Boolean(bound) && bound === target;
}

function validateProxyInput(url: string): boolean {
  const trimmed = url.trim();
  if (!trimmed) return false;
  try {
    const parsed = new URL(trimmed);
    if (Boolean(parsed.hostname) && PROXY_SCHEMES.includes(parsed.protocol)) {
      return true;
    }
  } catch {
    /* fallback */
  }
  const m = /^([a-z0-9+.-]+):\/\/(?:.*@)?([^@\s:/?#]+)(?::(\d{1,5}))?\/?$/i.exec(
    trimmed,
  );
  if (!m) return false;
  if (!PROXY_SCHEMES.includes(`${m[1].toLowerCase()}:`)) return false;
  if (m[3] !== undefined) {
    const port = Number(m[3]);
    if (port < 1 || port > 65535) return false;
  }
  return true;
}

function getProxyScheme(url: string): string {
  try {
    const parsed = new URL(url.trim());
    return parsed.protocol.replace(":", "").toUpperCase();
  } catch {
    if (url.toLowerCase().startsWith("socks5")) return "SOCKS5";
    if (url.toLowerCase().startsWith("https")) return "HTTPS";
    if (url.toLowerCase().startsWith("http")) return "HTTP";
    return "PROXY";
  }
}

function latencyColor(ms: number): string {
  if (ms <= 0) return "text-muted-foreground";
  if (ms < 500) return "text-emerald-600 dark:text-emerald-400";
  if (ms < 1500) return "text-amber-600 dark:text-amber-400";
  return "text-red-600 dark:text-red-400";
}

function latencyBg(ms: number): string {
  if (ms <= 0) return "";
  if (ms < 500) return "bg-emerald-500/10";
  if (ms < 1500) return "bg-amber-500/10";
  return "bg-red-500/10";
}

function maskUrl(url: string): string {
  try {
    const u = new URL(url);
    const host = u.hostname;
    const masked =
      host.length > 6 ? host.slice(0, 3) + "***" + host.slice(-3) : "***";
    return `${u.protocol}//${u.username ? "***:***@" : ""}${masked}${u.port ? ":" + u.port : ""}`;
  } catch {
    return url.slice(0, 10) + "******";
  }
}

function SchemeBadge({ scheme }: { scheme: string }) {
  const isSocks = scheme.includes("SOCKS");
  const isHttps = scheme === "HTTPS";
  return (
    <span
      className={cn(
        "inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-bold font-mono tracking-wide uppercase border",
        isSocks
          ? "border-purple-500/25 bg-purple-500/10 text-purple-600 dark:text-purple-400"
          : isHttps
            ? "border-emerald-500/25 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
            : "border-sky-500/25 bg-sky-500/10 text-sky-600 dark:text-sky-400"
      )}
    >
      {scheme}
    </span>
  );
}

function ProxyStatusBadge({ proxy }: { proxy: ProxyRow }) {
  const { t } = useTranslation();
  const kind = getProxyStatusBadgeKind(proxy);
  const styles =
    kind === "error"
      ? "border-destructive/25 bg-destructive/10 text-destructive"
      : kind === "untested"
        ? "border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-400"
        : kind === "enabled"
          ? "border-emerald-500/20 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
          : "border-border bg-muted/50 text-muted-foreground";
  const dot =
    kind === "error"
      ? "bg-destructive"
      : kind === "untested"
        ? "bg-amber-500"
        : kind === "enabled"
          ? "bg-emerald-500"
          : "bg-muted-foreground/50";
  const label =
    kind === "error"
      ? t("proxies.testStatusError")
      : kind === "untested"
        ? t("proxies.testStatusUntested")
        : kind === "enabled"
          ? t("proxies.enabled")
          : t("proxies.disabled");

  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-semibold transition-all ${styles}`}>
      <span className={`size-1.5 rounded-full ${dot}`} />
      {label}
    </span>
  );
}

const BindAccountRow = memo(function BindAccountRow({
  account,
  checked,
  isThis,
  onToggle,
}: {
  account: AccountRow;
  checked: boolean;
  isThis: boolean;
  onToggle: (id: number) => void;
}) {
  const { t } = useTranslation();
  const boundUrl = normalizeProxyUrl(account.proxy_url);
  const kind = accountKindKey(account);
  return (
    <li>
      <label
        className={`flex cursor-pointer items-start gap-3 px-5 py-3 transition-colors sm:px-6 ${
          checked ? "bg-primary/5" : "hover:bg-muted/30"
        }`}
      >
        <input
          type="checkbox"
          checked={checked}
          onChange={() => onToggle(account.id)}
          className="mt-1 size-4 shrink-0 rounded"
        />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="truncate text-sm font-semibold text-foreground">
              {accountDisplayName(account)}
            </span>
            <span className="rounded-md border border-border bg-muted/40 px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">
              {t(`proxies.accountKind.${kind}`, { defaultValue: kind })}
            </span>
            <StatusBadge status={account.status} />
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
            <span className="tabular-nums">#{account.id}</span>
            {account.name && account.email ? (
              <span className="truncate">{account.name}</span>
            ) : null}
            {boundUrl ? (
              <span
                className={`inline-flex max-w-full items-center gap-1 truncate ${
                  isThis
                    ? "font-medium text-primary"
                    : "text-amber-600 dark:text-amber-400"
                }`}
                title={boundUrl}
              >
                <Link2 className="size-3 shrink-0" />
                {isThis
                  ? t("proxies.bindStatusThis")
                  : t("proxies.bindStatusOther", { proxy: maskUrl(boundUrl) })}
              </span>
            ) : (
              <span className="text-muted-foreground/80">
                {t("proxies.bindStatusNone")}
              </span>
            )}
          </div>
        </div>
      </label>
    </li>
  );
});

export default function Proxies() {
  const { t, i18n } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const [proxies, setProxies] = useState<ProxyRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [poolEnabled, setPoolEnabled] = useState(false);
  const [showAdd, setShowAdd] = useState(false);
  const [addInput, setAddInput] = useState("");
  const [addLabel, setAddLabel] = useState("");
  const [addLoading, setAddLoading] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [testingIds, setTestingIds] = useState<Set<number>>(new Set());
  const [testAllLoading, setTestAllLoading] = useState(false);
  const [testAllDone, setTestAllDone] = useState(0);
  const [testAllFailed, setTestAllFailed] = useState(0);
  const [testAllTotal, setTestAllTotal] = useState(0);
  const [cleaningErrors, setCleaningErrors] = useState(false);
  const [page, setPage] = useState(1);
  const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS;
  const [pageSize, setPageSize] = usePersistedPageSize(
    "proxies",
    10,
    pageSizeOptions,
  );
  const [revealedIds, setRevealedIds] = useState<Set<number>>(new Set());
  const [editingProxy, setEditingProxy] = useState<ProxyRow | null>(null);
  const [editUrl, setEditUrl] = useState("");
  const [editLabel, setEditLabel] = useState("");
  const [editSaving, setEditSaving] = useState(false);
  const [editError, setEditError] = useState("");

  const [query, setQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [accountsLoading, setAccountsLoading] = useState(false);
  const [bindingProxy, setBindingProxy] = useState<ProxyRow | null>(null);
  const [bindSelected, setBindSelected] = useState<Set<number>>(new Set());
  const [bindFilter, setBindFilter] = useState<BindFilter>("all");
  const [bindKindFilter, setBindKindFilter] = useState<BindKindFilter>("all");
  const [bindQuery, setBindQuery] = useState("");
  const [debouncedBindQuery, setDebouncedBindQuery] = useState("");
  const [bindPage, setBindPage] = useState(1);
  const [bindTotal, setBindTotal] = useState(0);
  const bindPageSize = 50;
  const bindAbortRef = useRef<AbortController | null>(null);
  const [bindSubmitting, setBindSubmitting] = useState(false);

  const [showBalance, setShowBalance] = useState(false);
  const [balanceChannel, setBalanceChannel] = useState<"" | "codex" | "grok" | "claude">("grok");
  const [balanceMode, setBalanceMode] = useState<"unbound" | "all">("unbound");
  const [balanceMaxPerProxy, setBalanceMaxPerProxy] = useState("");
  const [balanceSubmitting, setBalanceSubmitting] = useState(false);

  const ipApiLang = i18n.language?.startsWith("zh") ? "zh-CN" : "en";

  const reloadAccounts = useCallback(async () => {
    if (!bindingProxy) return;
    bindAbortRef.current?.abort();
    const controller = new AbortController();
    bindAbortRef.current = controller;
    setAccountsLoading(true);
    try {
      const res = await api.getAccountsPage({
        channel: bindKindFilter === "all" ? undefined : bindKindFilter,
        page: bindPage,
        pageSize: bindPageSize,
        search: debouncedBindQuery,
        proxyUrl: bindingProxy.url,
        proxyFilter: bindFilter,
      }, controller.signal);
      setAccounts(res.accounts ?? []);
      setBindTotal(res.total ?? 0);
      setBindSelected((current) => {
        const next = new Set(current);
        for (const account of res.accounts ?? []) {
          if (isAccountBoundToProxy(account, bindingProxy.url)) next.add(account.id);
        }
        return next;
      });
    } catch (error) {
      if (controller.signal.aborted) return;
      showToast(
        t("proxies.bindLoadAccountsFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      if (!controller.signal.aborted) setAccountsLoading(false);
    }
  }, [bindFilter, bindKindFilter, bindPage, bindingProxy, debouncedBindQuery, showToast, t]);

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedBindQuery(bindQuery), 250);
    return () => window.clearTimeout(timer);
  }, [bindQuery]);

  useEffect(() => {
    if (!bindingProxy) return;
    void reloadAccounts();
    return () => bindAbortRef.current?.abort();
  }, [bindingProxy, reloadAccounts]);

  useEffect(() => {
    setBindPage(1);
  }, [bindFilter, bindKindFilter, debouncedBindQuery]);

  const reload = useCallback(async () => {
    try {
      const [proxyRes, settingsRes] = await Promise.all([
        api.listProxies(),
        api.getSettings(),
      ]);
      setProxies(proxyRes.proxies);
      setPoolEnabled(settingsRes.proxy_pool_enabled);
    } catch (error) {
      showToast(
        t("proxies.loadFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
    setLoading(false);
  }, [showToast, t]);

  useEffect(() => {
    reload();
  }, [reload]);

  const counts = useMemo(() => {
    let enabled = 0;
    let disabled = 0;
    let error = 0;
    let untested = 0;
    for (const p of proxies) {
      if (p.test_status === "error") error += 1;
      else if (!p.test_status || p.test_status === "untested") untested += 1;

      if (p.enabled) enabled += 1;
      else disabled += 1;
    }
    return { total: proxies.length, enabled, disabled, error, untested };
  }, [proxies]);

  const filteredProxies = useMemo(() => {
    const q = query.trim().toLowerCase();
    return proxies.filter((p) => {
      if (statusFilter === "enabled" && !p.enabled) return false;
      if (statusFilter === "disabled" && p.enabled) return false;
      if (statusFilter === "error" && p.test_status !== "error") return false;
      if (statusFilter === "untested" && p.test_status && p.test_status !== "untested") return false;

      if (q) {
        const matchUrl = p.url.toLowerCase().includes(q);
        const matchLabel = p.label?.toLowerCase().includes(q) ?? false;
        const matchIp = p.test_ip?.toLowerCase().includes(q) ?? false;
        const matchLoc = p.test_location?.toLowerCase().includes(q) ?? false;
        if (!matchUrl && !matchLabel && !matchIp && !matchLoc) return false;
      }
      return true;
    });
  }, [proxies, query, statusFilter]);

  const totalPages = Math.max(1, Math.ceil(filteredProxies.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pagedProxies = filteredProxies.slice(
    (currentPage - 1) * pageSize,
    currentPage * pageSize,
  );

  const boundCountForProxy = useCallback(
    (proxy: ProxyRow): number => proxy.bound_count ?? 0,
    [],
  );

  const totalBoundAccounts = useMemo(
    () => proxies.reduce((sum, p) => sum + (p.bound_count ?? 0), 0),
    [proxies],
  );

  const bindFilteredAccounts = accounts;
  const bindRenderedAccounts = accounts;
  const bindHiddenCount = 0;

  const bindVisibleAllSelected =
    bindFilteredAccounts.length > 0 &&
    bindFilteredAccounts.every((a) => bindSelected.has(a.id));

  const toggleBindAccount = useCallback((id: number) => {
    setBindSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const openBindModal = (proxy: ProxyRow) => {
    setBindingProxy(proxy);
    setBindFilter("all");
    setBindKindFilter("all");
    setBindQuery("");
    setDebouncedBindQuery("");
    setBindPage(1);
    setBindSelected(new Set());
  };

  const closeBindModal = () => {
    if (bindSubmitting) return;
    setBindingProxy(null);
    setBindSelected(new Set());
    setBindQuery("");
    setBindFilter("all");
    setBindKindFilter("all");
    setBindTotal(0);
    setAccounts([]);
  };

  const toggleBindSelectAll = () => {
    if (bindVisibleAllSelected) {
      setBindSelected((prev) => {
        const next = new Set(prev);
        bindFilteredAccounts.forEach((a) => next.delete(a.id));
        return next;
      });
    } else {
      setBindSelected((prev) => {
        const next = new Set(prev);
        bindFilteredAccounts.forEach((a) => next.add(a.id));
        return next;
      });
    }
  };

  const handleBindAccounts = async (mode: "bind" | "unbind") => {
    if (!bindingProxy || bindSelected.size === 0) return;
    const ids = Array.from(bindSelected);
    setBindSubmitting(true);
    try {
      const result = await api.batchUpdateAccounts({
        ids,
        proxy_url: mode === "bind" ? bindingProxy.url : "",
      });
      showToast(
        mode === "bind"
          ? t("proxies.bindDone", {
              success: result.success,
              fail: result.failed,
            })
          : t("proxies.unbindDone", {
              success: result.success,
              fail: result.failed,
            }),
      );
      await Promise.all([reloadAccounts(), reload()]);
      if (mode === "unbind") {
        setBindSelected(new Set());
      }
    } catch (error) {
      showToast(
        t("proxies.bindFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBindSubmitting(false);
    }
  };

  useEffect(() => {
    if (page !== currentPage) setPage(currentPage);
  }, [currentPage, page]);

  const handleTogglePool = async () => {
    const next = !poolEnabled;
    setPoolEnabled(next);
    try {
      await api.updateSettings({ proxy_pool_enabled: next });
    } catch {
      setPoolEnabled(!next);
    }
  };

  const handleAdd = async () => {
    const urls = addInput
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean);
    if (urls.length === 0) return;
    const invalidUrl = urls.find((url) => !validateProxyInput(url));
    if (invalidUrl) {
      showToast(t("proxies.invalidProxyUrl"), "error");
      return;
    }
    setAddLoading(true);
    try {
      await api.addProxies({ urls, label: addLabel });
      setAddInput("");
      setAddLabel("");
      setShowAdd(false);
      await reload();
    } catch (error) {
      showToast(
        t("proxies.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
    setAddLoading(false);
  };

  const handleDelete = async (id: number) => {
    try {
      await api.deleteProxy(id);
      await reload();
    } catch (error) {
      showToast(
        t("proxies.deleteFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleBatchDelete = async () => {
    if (selected.size === 0) return;
    try {
      await api.batchDeleteProxies([...selected]);
      setSelected(new Set());
      await reload();
    } catch (error) {
      showToast(
        t("proxies.batchDeleteFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const startEdit = (p: ProxyRow) => {
    setEditingProxy(p);
    setEditUrl(p.url);
    setEditLabel(p.label || "");
    setEditError("");
  };

  const handleEditSave = async () => {
    if (!editingProxy) return;
    const trimmedUrl = editUrl.trim();
    if (!trimmedUrl || !validateProxyInput(trimmedUrl)) {
      setEditError(t("proxies.invalidProxyUrl"));
      return;
    }
    setEditSaving(true);
    setEditError("");
    try {
      await api.updateProxy(editingProxy.id, {
        url: trimmedUrl,
        label: editLabel.trim(),
      });
      setEditingProxy(null);
      await reload();
      showToast(t("proxies.proxyUpdated"));
    } catch (error) {
      setEditError(getErrorMessage(error));
    } finally {
      setEditSaving(false);
    }
  };

  const handleToggle = async (p: ProxyRow) => {
    try {
      await api.updateProxy(p.id, { enabled: !p.enabled });
      await reload();
    } catch {
      /* ignore */
    }
  };

  const handleTest = async (p: ProxyRow) => {
    if (cleaningErrors) return;
    setTestingIds((prev) => new Set(prev).add(p.id));
    try {
      const result = await api.testProxy(p.url, p.id, ipApiLang);
      setProxies((prev) =>
        prev.map((px) =>
          px.id === p.id ? applyProxyTestResult(px, result) : px,
        ),
      );
      if (!result.success) {
        showToast(
          t("proxies.testFailed", {
            error: result.error || t("proxies.testFailedUnknown"),
          }),
          "error",
        );
      }
    } catch (error) {
      showToast(
        t("proxies.testFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
    setTestingIds((prev) => {
      const next = new Set(prev);
      next.delete(p.id);
      return next;
    });
  };

  const handleTestAll = async () => {
    if (cleaningErrors || testAllLoading || testingIds.size > 0) return;
    const queue = [...proxies];
    if (queue.length === 0) return;
    setTestAllLoading(true);
    setTestAllDone(0);
    setTestAllFailed(0);
    setTestAllTotal(queue.length);
    let completedCount = 0;
    let failedCount = 0;
    let firstError = "";
    let completionError = "";
    setTestingIds(new Set(queue.map((proxy) => proxy.id)));

    try {
      for (const batchIDs of chunkProxyTestIDs(
        queue.map((proxy) => proxy.id),
      )) {
        const response = await postAdminSSE("/proxies/test-all", {
          ids: batchIDs,
          lang: ipApiLang,
        });
        const completeEvent = await readProxyBatchTestSSE(response, (event) => {
          if (event.type === "complete") {
            if (!completionError && event.error) {
              completionError = event.error;
            }
            return;
          }
          if (event.type !== "progress" || event.proxy_id === undefined) {
            return;
          }

          const proxyID = event.proxy_id;
          const result = event.result;
          if (result) {
            setProxies((prev) =>
              prev.map((proxy) =>
                proxy.id === proxyID
                  ? applyProxyTestResult(proxy, result)
                  : proxy,
              ),
            );
            if (!result.success && !firstError) {
              firstError = result.error || t("proxies.testFailedUnknown");
            }
          }
          setTestAllDone(completedCount + (event.current ?? 0));
          setTestAllFailed(failedCount + (event.failed ?? 0));
          setTestingIds((prev) => {
            const next = new Set(prev);
            next.delete(proxyID);
            return next;
          });
        });
        const batchCompleted = completeEvent?.current ?? 0;
        if (!completeEvent || batchCompleted !== batchIDs.length) {
          throw new Error(t("proxies.testAllInterrupted"));
        }
        completedCount += batchCompleted;
        failedCount += completeEvent.failed ?? 0;
        setTestAllDone(completedCount);
        setTestAllFailed(failedCount);
      }
      await reload();
      if (completionError) {
        showToast(completionError, "error");
      } else if (failedCount > 0) {
        showToast(
          t("proxies.testAllFailed", {
            count: failedCount,
            error: firstError,
          }),
          "error",
        );
      }
    } catch (error) {
      await reload();
      showToast(
        t("proxies.testFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setTestingIds(new Set());
      setTestAllLoading(false);
    }
  };

  const handleAutoBalance = async () => {
    const maxPerProxy = Number(balanceMaxPerProxy.trim());
    setBalanceSubmitting(true);
    try {
      const result = await api.autoBalanceProxies({
        channel: balanceChannel || undefined,
        mode: balanceMode,
        max_per_proxy:
          Number.isInteger(maxPerProxy) && maxPerProxy > 0 ? maxPerProxy : 0,
      });
      showToast(
        t("proxies.balanceDone", {
          assigned: result.assigned,
          kept: result.kept,
          skipped: result.skipped,
        }),
        result.skipped > 0 ? "error" : "success",
      );
      setShowBalance(false);
      await Promise.all([
        reload(),
        bindingProxy ? reloadAccounts() : Promise.resolve(),
      ]);
    } catch (error) {
      showToast(
        t("proxies.balanceFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBalanceSubmitting(false);
    }
  };

  const errorCount = counts.error;
  const testsRunning = testAllLoading || testingIds.size > 0;

  const handleCleanErrors = async () => {
    if (errorCount === 0 || testsRunning || cleaningErrors) return;
    const confirmed = await confirm({
      title: t("proxies.cleanErrorTitle"),
      description: t("proxies.cleanErrorDesc", { count: errorCount }),
      confirmText: t("proxies.cleanErrorConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;

    setCleaningErrors(true);
    try {
      const result = await api.cleanErrorProxies();
      setSelected(new Set());
      showToast(
        t("proxies.cleanErrorSuccess", {
          count: result.cleaned,
          unbound: result.unbound,
        }),
      );
      await reload();
    } catch (error) {
      showToast(
        t("proxies.cleanErrorFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningErrors(false);
    }
  };

  const allSelected =
    pagedProxies.length > 0 && pagedProxies.every((p) => selected.has(p.id));
  const toggleSelectAll = () => {
    if (allSelected) {
      setSelected((prev) => {
        const next = new Set(prev);
        pagedProxies.forEach((p) => next.delete(p.id));
        return next;
      });
    } else {
      setSelected((prev) => {
        const next = new Set(prev);
        pagedProxies.forEach((p) => next.add(p.id));
        return next;
      });
    }
  };

  const canEnable = proxies.some(
    (p) => p.enabled && p.test_status !== "error",
  );

  const statusFilterItems: Array<{ id: StatusFilter; label: string; count: number }> = [
    { id: "all", label: t("proxies.filterAll"), count: counts.total },
    { id: "enabled", label: t("proxies.filterEnabled"), count: counts.enabled },
    { id: "disabled", label: t("proxies.filterDisabled"), count: counts.disabled },
    { id: "error", label: t("proxies.filterError"), count: counts.error },
    { id: "untested", label: t("proxies.filterUntested"), count: counts.untested },
  ];

  return (
    <div className="space-y-5 sm:space-y-6">
      {confirmDialog}
      {/* Header */}
      <PageHeader
        title={t("nav.proxies")}
        description={t("proxies.description")}
        className="mb-0 sm:mb-0"
        actions={
          <>
            <div
              className="flex h-9 shrink-0 items-center gap-2.5 rounded-lg border border-border/60 bg-muted/20 px-3"
              title={
                !canEnable && !poolEnabled
                  ? t("proxies.addFirstProxy")
                  : t("proxies.poolFailClosedHint")
              }
            >
              <span
                className={`text-[13px] font-medium ${poolEnabled ? "text-foreground" : "text-muted-foreground"}`}
              >
                {poolEnabled
                  ? t("proxies.poolEnabled")
                  : t("proxies.poolDisabled")}
              </span>
              <Switch
                checked={poolEnabled}
                onCheckedChange={handleTogglePool}
                disabled={!canEnable && !poolEnabled}
              />
            </div>

            {selected.size > 0 && (
              <Button variant="destructive" onClick={handleBatchDelete}>
                <Trash2 className="size-4" />
                {t("proxies.deleteSelected", { count: selected.size })}
              </Button>
            )}

            <Button
              variant="outline"
              onClick={handleCleanErrors}
              disabled={errorCount === 0 || cleaningErrors || testsRunning}
              className="text-destructive hover:bg-destructive/10 hover:text-destructive dark:hover:bg-destructive/10"
            >
              {cleaningErrors ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <AlertTriangle className="size-4" />
              )}
              {cleaningErrors
                ? t("proxies.cleaningErrors")
                : t("proxies.cleanErrors", { count: errorCount })}
            </Button>

            <Button
              variant="outline"
              onClick={() => setShowBalance(true)}
              disabled={
                balanceSubmitting ||
                !proxies.some((p) => p.enabled && p.test_status !== "error")
              }
            >
              <Scale className="size-4" />
              {t("proxies.autoBalance")}
            </Button>

            <Button
              variant="outline"
              onClick={handleTestAll}
              disabled={proxies.length === 0 || testsRunning || cleaningErrors}
            >
              {testAllLoading ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <Zap className="size-4" />
              )}
              {testAllLoading
                ? t("proxies.testingAllProgress", {
                    current: testAllDone,
                    total: testAllTotal,
                    failed: testAllFailed,
                  })
                : t("proxies.testAll")}
            </Button>

            <Button onClick={() => setShowAdd(!showAdd)}>
              <Plus className="size-4" />
              {t("proxies.addProxy")}
            </Button>
          </>
        }
      />

      {/* 批量测试进度条 */}
      {testAllLoading && testAllTotal > 0 ? (
        <div className="rounded-xl border border-primary/20 bg-primary/5 p-3.5 space-y-2">
          <div className="flex items-center justify-between text-xs font-semibold">
            <span className="flex items-center gap-1.5 text-primary">
              <Loader2 className="size-3.5 animate-spin" />
              {t("proxies.testingAllProgress", {
                current: testAllDone,
                total: testAllTotal,
                failed: testAllFailed,
              })}
            </span>
            <span className="tabular-nums text-muted-foreground">
              {Math.round((testAllDone / testAllTotal) * 100)}%
            </span>
          </div>
          <div className="h-1.5 w-full overflow-hidden rounded-full bg-primary/10">
            <div
              className="h-full bg-primary transition-all duration-300 rounded-full"
              style={{ width: `${(testAllDone / testAllTotal) * 100}%` }}
            />
          </div>
        </div>
      ) : null}

      {/* Add Panel */}
      {showAdd && (
        <Card className="py-0">
          <CardContent className="p-5 space-y-4">
            <div className="flex items-center justify-between">
              <div>
                <h4 className="text-base font-semibold text-foreground">
                  {t("proxies.addProxyTitle")}
                </h4>
                <p className="mt-0.5 text-xs text-muted-foreground">
                  {t("proxies.addProxyDesc")}
                </p>
              </div>
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="h-8 text-xs text-muted-foreground"
                  onClick={() => setAddInput("socks5://127.0.0.1:1080\nhttp://user:pass@1.2.3.4:8080")}
                >
                  <RotateCcw className="size-3 mr-1" />
                  {t("proxies.insertTemplate")}
                </Button>
                {addInput ? (
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="h-8 text-xs text-muted-foreground"
                    onClick={() => setAddInput("")}
                  >
                    {t("proxies.clearAll")}
                  </Button>
                ) : null}
              </div>
            </div>
            <textarea
              value={addInput}
              onChange={(e) => setAddInput(e.target.value)}
              placeholder={"http://user:pass@ip:port\nsocks5://ip:port"}
              className="w-full h-32 px-3.5 py-2.5 text-xs rounded-xl border border-border bg-background text-foreground placeholder:text-muted-foreground resize-none outline-none focus:ring-2 focus:ring-primary/30 font-mono"
            />
            <div className="flex items-center gap-3">
              <Input
                type="text"
                value={addLabel}
                onChange={(e) => setAddLabel(e.target.value)}
                placeholder={t("proxies.labelPlaceholder")}
                className="flex-1 rounded-xl h-10"
              />
              <Button
                onClick={handleAdd}
                disabled={addLoading || !addInput.trim()}
                className="rounded-xl h-10 px-5"
              >
                {addLoading ? t("proxies.adding") : t("proxies.confirmAdd")}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Stats （点击快捷筛选） */}
      {loading ? (
        <div
          className="grid grid-cols-2 gap-3 min-[520px]:grid-cols-4 sm:gap-4"
          aria-busy="true"
        >
          {[0, 1, 2, 3].map((i) => (
            <div
              key={i}
              className="h-[76px] animate-pulse rounded-lg border border-border bg-muted/40"
            />
          ))}
        </div>
      ) : (
        <div className="grid grid-cols-2 gap-3 min-[520px]:grid-cols-4 sm:gap-4">
          <StatTile
            label={t("proxies.totalProxies")}
            value={counts.total}
            icon={<Globe className="size-4" />}
            active={statusFilter === "all"}
            onClick={() => setStatusFilter("all")}
          />
          <StatTile
            label={t("proxies.enabledCount")}
            value={counts.enabled}
            icon={<Power className="size-4" />}
            tone="success"
            active={statusFilter === "enabled"}
            onClick={() => setStatusFilter("enabled")}
          />
          <StatTile
            label={t("proxies.boundAccounts")}
            value={totalBoundAccounts}
            icon={<Users className="size-4" />}
            tone="info"
          />
          <StatTile
            label={t("proxies.untestedCount")}
            value={counts.untested}
            icon={<Zap className="size-4" />}
            tone="warning"
            active={statusFilter === "untested"}
            onClick={() => setStatusFilter("untested")}
          />
        </div>
      )}

      {/* Sticky toolbar */}
      <div className="sticky top-2 z-20 -mx-1 px-1">
        <div className="flex flex-col gap-3 rounded-xl border border-border/80 bg-card/95 p-2.5 shadow-sm backdrop-blur-xl sm:flex-row sm:items-center sm:justify-between sm:p-2 sm:pl-3">
          <div className="relative min-w-0 flex-1 sm:max-w-xs">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input
              className="h-9 border-transparent bg-muted/40 pl-9 text-sm shadow-none focus-visible:bg-background"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={t("proxies.searchPlaceholder")}
            />
          </div>

          <div
            className="flex max-w-full gap-0.5 overflow-x-auto rounded-xl bg-muted/50 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
            role="tablist"
          >
            {statusFilterItems.map((item) => {
              const active = statusFilter === item.id;
              return (
                <button
                  key={item.id}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  onClick={() => setStatusFilter(item.id)}
                  className={cn(
                    "inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-2.5 text-xs font-semibold transition-all",
                    active
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {item.label}
                  <span
                    className={cn(
                      "tabular-nums rounded-md px-1 py-px text-[10px] font-bold",
                      active
                        ? item.id === "error" && item.count > 0
                          ? "bg-destructive/20 text-destructive"
                          : "bg-primary/10 text-primary"
                        : item.id === "error" && item.count > 0
                          ? "bg-destructive/10 text-destructive"
                          : "bg-background/60 text-muted-foreground",
                    )}
                  >
                    {item.count}
                  </span>
                </button>
              );
            })}
          </div>
        </div>
      </div>

      {/* Table */}
      <Card className="py-0">
        <CardContent className="p-0">
          {loading ? (
            <div className="space-y-2 p-4" aria-busy="true">
              <div className="h-9 w-full animate-pulse rounded-md bg-muted/60" />
              {[0, 1, 2, 3, 4].map((i) => (
                <div
                  key={i}
                  className="h-12 w-full animate-pulse rounded-md bg-muted/40"
                />
              ))}
            </div>
          ) : filteredProxies.length === 0 ? (
            <div className="text-center py-16 text-muted-foreground">
              <Globe className="size-12 mx-auto mb-3 opacity-30" />
              <p className="text-sm font-medium">{t("proxies.noProxies")}</p>
              <p className="text-xs mt-1">{t("proxies.noProxiesDesc")}</p>
            </div>
          ) : (
            <>
              {/* Mobile cards */}
              <div className="grid gap-3 p-3 lg:hidden">
                {pagedProxies.map((p) => {
                  const isTesting = testingIds.has(p.id);
                  const scheme = getProxyScheme(p.url);
                  return (
                    <div
                      key={p.id}
                      className="rounded-xl border border-border bg-background/70 p-3.5 shadow-sm"
                    >
                      <div className="flex items-start gap-2.5">
                        <input
                          type="checkbox"
                          checked={selected.has(p.id)}
                          onChange={() => {
                            const next = new Set(selected);
                            if (next.has(p.id)) next.delete(p.id);
                            else next.add(p.id);
                            setSelected(next);
                          }}
                          className="mt-1 size-4 rounded"
                        />
                        <div className="min-w-0 flex-1">
                          <div className="flex items-start gap-2">
                            <SchemeBadge scheme={scheme} />
                            {p.label ? (
                              <span className="inline-flex items-center rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-semibold text-primary">
                                {p.label}
                              </span>
                            ) : null}
                            <Button
                              variant="ghost"
                              size="icon"
                              onClick={() => {
                                setRevealedIds((prev) => {
                                  const next = new Set(prev);
                                  if (next.has(p.id)) next.delete(p.id);
                                  else next.add(p.id);
                                  return next;
                                });
                              }}
                              className="shrink-0 text-muted-foreground hover:text-foreground ml-auto"
                              title={
                                revealedIds.has(p.id)
                                  ? t("proxies.hideProxyUrl")
                                  : t("proxies.showProxyUrl")
                              }
                            >
                              {revealedIds.has(p.id) ? (
                                <EyeOff className="size-3.5" />
                              ) : (
                                <Eye className="size-3.5" />
                              )}
                            </Button>
                          </div>

                          <div className="mt-1.5 break-all font-mono text-[12px] font-medium leading-relaxed text-foreground">
                            {revealedIds.has(p.id) ? p.url : maskUrl(p.url)}
                          </div>

                          <div className="mt-2.5 flex flex-wrap items-center gap-2">
                            <ProxyStatusBadge proxy={p} />
                            <span className="inline-flex items-center gap-1 rounded-full border border-border bg-muted/40 px-2 py-0.5 text-xs font-medium text-muted-foreground">
                              <Users className="size-3" />
                              {t("proxies.boundCount", {
                                count: boundCountForProxy(p),
                              })}
                            </span>
                            {p.test_latency_ms > 0 ? (
                              <span
                                className={`inline-flex rounded-full px-2 py-0.5 text-xs font-bold ${latencyColor(p.test_latency_ms)} ${latencyBg(p.test_latency_ms)}`}
                              >
                                {p.test_latency_ms}ms
                              </span>
                            ) : null}
                            {isTesting ? (
                              <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
                            ) : p.test_location ? (
                              <span className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground">
                                <MapPin className="size-3 text-primary" />
                                {p.test_location}
                                {p.test_ip ? ` · ${p.test_ip}` : ""}
                              </span>
                            ) : null}
                          </div>

                          <div className="mt-3 flex flex-wrap gap-1.5">
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => openBindModal(p)}
                              className="min-h-9 flex-1 border-primary/25 bg-primary/5 text-primary hover:bg-primary/10 hover:text-primary dark:border-primary/25 dark:bg-primary/10 dark:hover:bg-primary/15"
                            >
                              <Link2 className="size-3.5" />
                              {t("proxies.bindAccounts")}
                            </Button>
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => startEdit(p)}
                              className="min-h-9 flex-1"
                            >
                              <Pencil className="size-3.5" />
                              {t("proxies.editProxy")}
                            </Button>
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => handleTest(p)}
                              disabled={isTesting || cleaningErrors}
                              className="min-h-9 flex-1"
                            >
                              {isTesting ? (
                                <Loader2 className="size-3.5 animate-spin" />
                              ) : (
                                <Play className="size-3.5" />
                              )}
                              {t("proxies.test")}
                            </Button>
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => handleToggle(p)}
                              className={`min-h-9 flex-1 ${
                                p.enabled
                                  ? "border-amber-500/25 text-amber-600 hover:bg-amber-500/10 hover:text-amber-600 dark:border-amber-500/25 dark:text-amber-400 dark:hover:bg-amber-500/10 dark:hover:text-amber-400"
                                  : "border-emerald-500/25 text-emerald-600 hover:bg-emerald-500/10 hover:text-emerald-600 dark:border-emerald-500/25 dark:text-emerald-400 dark:hover:bg-emerald-500/10 dark:hover:text-emerald-400"
                              }`}
                            >
                              <Power className="size-3.5" />
                              {p.enabled
                                ? t("proxies.disableAction")
                                : t("proxies.enableAction")}
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              onClick={() => handleDelete(p.id)}
                              className="min-h-9 text-destructive hover:bg-destructive/10 hover:text-destructive dark:hover:bg-destructive/10"
                              title={t("common.delete")}
                            >
                              <Trash2 className="size-3.5" />
                            </Button>
                          </div>
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>

              {/* Desktop table */}
              <div className="data-table-shell hidden lg:block">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-10">
                        <input
                          type="checkbox"
                          checked={allSelected}
                          onChange={toggleSelectAll}
                          className="size-4 rounded"
                        />
                      </TableHead>
                      <TableHead>{t("proxies.colUrl")}</TableHead>
                      <TableHead>{t("proxies.colStatus")}</TableHead>
                      <TableHead>{t("proxies.colBound")}</TableHead>
                      <TableHead>{t("proxies.colLocation")}</TableHead>
                      <TableHead>{t("proxies.colIp")}</TableHead>
                      <TableHead>{t("proxies.colLatency")}</TableHead>
                      <TableHead className="text-right">
                        {t("proxies.colActions")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {pagedProxies.map((p) => {
                      const isTesting = testingIds.has(p.id);
                      const scheme = getProxyScheme(p.url);
                      return (
                        <TableRow key={p.id}>
                          <TableCell>
                            <input
                              type="checkbox"
                              checked={selected.has(p.id)}
                              onChange={() => {
                                const next = new Set(selected);
                                if (next.has(p.id)) next.delete(p.id);
                                else next.add(p.id);
                                setSelected(next);
                              }}
                              className="size-4 rounded"
                            />
                          </TableCell>
                          <TableCell className="max-w-[380px] whitespace-normal">
                            <div className="flex items-center gap-2">
                              <SchemeBadge scheme={scheme} />
                              {p.label ? (
                                <span className="inline-flex rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-semibold text-primary">
                                  {p.label}
                                </span>
                              ) : null}
                              <Button
                                variant="ghost"
                                size="icon-xs"
                                onClick={() => {
                                  setRevealedIds((prev) => {
                                    const next = new Set(prev);
                                    if (next.has(p.id)) next.delete(p.id);
                                    else next.add(p.id);
                                    return next;
                                  });
                                }}
                                className="shrink-0 text-muted-foreground hover:text-foreground"
                                title={
                                  revealedIds.has(p.id)
                                    ? t("proxies.hideProxyUrl")
                                    : t("proxies.showProxyUrl")
                                }
                              >
                                {revealedIds.has(p.id) ? (
                                  <EyeOff className="size-3.5" />
                                ) : (
                                  <Eye className="size-3.5" />
                                )}
                              </Button>
                              <span className="font-mono text-[13px] font-medium break-all text-foreground">
                                {revealedIds.has(p.id) ? p.url : maskUrl(p.url)}
                              </span>
                            </div>
                          </TableCell>
                          <TableCell>
                            <ProxyStatusBadge proxy={p} />
                          </TableCell>
                          {/* Bound accounts */}
                          <TableCell>
                            <button
                              type="button"
                              onClick={() => openBindModal(p)}
                              className="inline-flex items-center gap-1.5 rounded-full border border-border bg-muted/30 px-2.5 py-1 text-xs font-semibold text-foreground transition-colors hover:border-primary/30 hover:bg-primary/5 hover:text-primary"
                              title={t("proxies.bindAccounts")}
                            >
                              <Users className="size-3" />
                              <span className="tabular-nums">
                                {boundCountForProxy(p)}
                              </span>
                            </button>
                          </TableCell>
                          {/* Location */}
                          <TableCell>
                            {isTesting ? (
                              <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
                            ) : p.test_location ? (
                              <div className="flex items-center gap-1 text-xs font-medium text-foreground whitespace-nowrap">
                                <MapPin className="size-3 text-primary shrink-0" />
                                {p.test_location}
                              </div>
                            ) : (
                              <span className="text-xs text-muted-foreground">
                                -
                              </span>
                            )}
                          </TableCell>
                          {/* IP */}
                          <TableCell>
                            {p.test_ip ? (
                              <span className="text-[13px] font-mono font-medium text-foreground whitespace-nowrap">
                                {p.test_ip}
                              </span>
                            ) : (
                              <span className="text-xs text-muted-foreground">
                                -
                              </span>
                            )}
                          </TableCell>
                          {/* Latency */}
                          <TableCell>
                            {p.test_latency_ms > 0 ? (
                              <span
                                className={`inline-flex px-2 py-0.5 rounded-full text-xs font-bold ${latencyColor(p.test_latency_ms)} ${latencyBg(p.test_latency_ms)}`}
                              >
                                {p.test_latency_ms}ms
                              </span>
                            ) : (
                              <span className="text-xs text-muted-foreground">
                                -
                              </span>
                            )}
                          </TableCell>
                          <TableCell>
                            <div className="flex items-center gap-1 justify-end">
                              <Button
                                variant="outline"
                                size="sm"
                                onClick={() => openBindModal(p)}
                                className="border-primary/20 bg-primary/5 text-primary hover:bg-primary/10 hover:text-primary dark:border-primary/25 dark:bg-primary/10 dark:hover:bg-primary/15"
                                title={t("proxies.bindAccounts")}
                              >
                                <Link2 className="size-3.5" />
                                {t("proxies.bind")}
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                onClick={() => startEdit(p)}
                                className="text-muted-foreground hover:text-foreground"
                                title={t("proxies.editProxy")}
                              >
                                <Pencil className="size-3.5" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                onClick={() => handleTest(p)}
                                disabled={isTesting || cleaningErrors}
                                className="text-muted-foreground hover:text-foreground"
                                title={t("proxies.testProxy")}
                              >
                                {isTesting ? (
                                  <Loader2 className="size-3.5 animate-spin" />
                                ) : (
                                  <Play className="size-3.5" />
                                )}
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                onClick={() => handleToggle(p)}
                                className={
                                  p.enabled
                                    ? "text-amber-600 hover:bg-amber-500/10 hover:text-amber-600 dark:text-amber-400 dark:hover:bg-amber-500/10 dark:hover:text-amber-400"
                                    : "text-emerald-600 hover:bg-emerald-500/10 hover:text-emerald-600 dark:text-emerald-400 dark:hover:bg-emerald-500/10 dark:hover:text-emerald-400"
                                }
                                title={
                                  p.enabled
                                    ? t("proxies.disableAction")
                                    : t("proxies.enableAction")
                                }
                              >
                                <Power className="size-3.5" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                onClick={() => handleDelete(p.id)}
                                className="text-destructive hover:bg-destructive/10 hover:text-destructive dark:hover:bg-destructive/10"
                                title={t("common.delete")}
                              >
                                <Trash2 className="size-3.5" />
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>

              <div className="px-4 pb-3">
                <Pagination
                  page={currentPage}
                  totalPages={totalPages}
                  onPageChange={setPage}
                  totalItems={filteredProxies.length}
                  pageSize={pageSize}
                  pageSizeOptions={pageSizeOptions}
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

      <Modal
        show={Boolean(editingProxy)}
        title={t("proxies.editProxyTitle")}
        onClose={() => setEditingProxy(null)}
        contentClassName="sm:max-w-[520px]"
        footer={
          <>
            <Button
              type="button"
              variant="outline"
              onClick={() => setEditingProxy(null)}
              disabled={editSaving}
            >
              {t("common.cancel")}
            </Button>
            <Button
              type="button"
              onClick={() => void handleEditSave()}
              disabled={editSaving || !editUrl.trim()}
            >
              {editSaving ? t("common.saving") : t("common.save")}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <label className="block space-y-1.5">
            <span className="text-xs font-semibold text-muted-foreground">
              {t("proxies.editUrlLabel")}
            </span>
            <Input
              type="text"
              value={editUrl}
              onChange={(e) => {
                setEditUrl(e.target.value);
                setEditError("");
              }}
              className="font-mono"
              placeholder="http://user:pass@ip:port"
            />
          </label>
          <label className="block space-y-1.5">
            <span className="text-xs font-semibold text-muted-foreground">
              {t("proxies.editLabelLabel")}
            </span>
            <Input
              type="text"
              value={editLabel}
              onChange={(e) => setEditLabel(e.target.value)}
              placeholder={t("proxies.labelPlaceholder")}
            />
          </label>
          {editError && (
            <div className="flex items-center gap-2 rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm font-medium text-destructive">
              <AlertTriangle className="size-4 shrink-0" />
              {editError}
            </div>
          )}
        </div>
      </Modal>

      {/* 一键均衡绑定 */}
      <Modal
        show={showBalance}
        title={t("proxies.balanceModalTitle")}
        onClose={() => {
          if (!balanceSubmitting) setShowBalance(false);
        }}
        contentClassName="sm:max-w-[520px]"
        footer={
          <>
            <Button
              type="button"
              variant="outline"
              onClick={() => setShowBalance(false)}
              disabled={balanceSubmitting}
            >
              {t("common.cancel")}
            </Button>
            <Button
              type="button"
              className="gap-1.5"
              onClick={() => void handleAutoBalance()}
              disabled={balanceSubmitting}
            >
              {balanceSubmitting ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <Scale className="size-3.5" />
              )}
              {t("proxies.balanceConfirm")}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            {t("proxies.balanceDesc")}
          </p>
          <div className="space-y-1.5">
            <span className="text-xs font-semibold text-muted-foreground">
              {t("proxies.balanceChannel")}
            </span>
            <div className="flex flex-wrap gap-1.5">
              {(
                [
                  ["grok", t("proxies.bindKindGrok")],
                  ["codex", t("proxies.bindKindCodex")],
                  ["claude", t("proxies.bindKindClaude")],
                  ["", t("proxies.bindKindAll")],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key || "all"}
                  type="button"
                  onClick={() => setBalanceChannel(key)}
                  className={`rounded-full border px-3 py-1.5 text-xs font-semibold transition-colors ${
                    balanceChannel === key
                      ? "border-primary/30 bg-primary/10 text-primary"
                      : "border-border text-muted-foreground hover:bg-muted/50 hover:text-foreground"
                  }`}
                >
                  {label}
                </button>
              ))}
            </div>
          </div>
          <div className="space-y-1.5">
            <span className="text-xs font-semibold text-muted-foreground">
              {t("proxies.balanceMode")}
            </span>
            <div className="flex flex-wrap gap-1.5">
              {(
                [
                  ["unbound", t("proxies.balanceModeUnbound")],
                  ["all", t("proxies.balanceModeAll")],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => setBalanceMode(key)}
                  className={`rounded-full border px-3 py-1.5 text-xs font-semibold transition-colors ${
                    balanceMode === key
                      ? "border-primary/30 bg-primary/10 text-primary"
                      : "border-border text-muted-foreground hover:bg-muted/50 hover:text-foreground"
                  }`}
                >
                  {label}
                </button>
              ))}
            </div>
            <p className="text-[11px] leading-relaxed text-muted-foreground">
              {balanceMode === "all"
                ? t("proxies.balanceModeAllHint")
                : t("proxies.balanceModeUnboundHint")}
            </p>
          </div>
          <label className="block space-y-1.5">
            <span className="text-xs font-semibold text-muted-foreground">
              {t("proxies.balanceMaxPerProxy")}
            </span>
            <Input
              type="number"
              min={0}
              value={balanceMaxPerProxy}
              onChange={(e) => setBalanceMaxPerProxy(e.target.value)}
              placeholder={t("proxies.balanceMaxPerProxyPlaceholder")}
            />
          </label>
        </div>
      </Modal>

      {/* 绑定账号到代理 */}
      <Modal
        show={Boolean(bindingProxy)}
        title={t("proxies.bindModalTitle")}
        onClose={closeBindModal}
        contentClassName="sm:max-w-[720px]"
        bodyClassName="!p-0"
        footer={
          <>
            <Button
              type="button"
              variant="outline"
              onClick={closeBindModal}
              disabled={bindSubmitting}
            >
              {t("common.cancel")}
            </Button>
            <Button
              type="button"
              variant="outline"
              className="gap-1.5 text-destructive hover:bg-destructive/10 hover:text-destructive"
              disabled={bindSubmitting || bindSelected.size === 0}
              onClick={() => void handleBindAccounts("unbind")}
            >
              {bindSubmitting ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <Unlink className="size-3.5" />
              )}
              {t("proxies.unbindSelected", { count: bindSelected.size })}
            </Button>
            <Button
              type="button"
              className="gap-1.5"
              disabled={bindSubmitting || bindSelected.size === 0}
              onClick={() => void handleBindAccounts("bind")}
            >
              {bindSubmitting ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <Link2 className="size-3.5" />
              )}
              {t("proxies.bindSelected", { count: bindSelected.size })}
            </Button>
          </>
        }
      >
        {bindingProxy ? (
          <div className="flex flex-col">
            <div className="border-b border-border bg-muted/20 px-5 py-3.5 sm:px-6">
              <div className="text-xs font-semibold text-muted-foreground">
                {t("proxies.bindTargetProxy")}
              </div>
              <div className="mt-1.5 flex flex-wrap items-center gap-2">
                {bindingProxy.label ? (
                  <span className="inline-flex rounded-md bg-primary/10 px-2 py-0.5 text-xs font-semibold text-primary">
                    {bindingProxy.label}
                  </span>
                ) : null}
                <span className="min-w-0 break-all font-mono text-[13px] font-medium text-foreground">
                  {maskUrl(bindingProxy.url)}
                </span>
                <span className="inline-flex items-center gap-1 rounded-full border border-border px-2 py-0.5 text-xs text-muted-foreground">
                  <Users className="size-3" />
                  {t("proxies.boundCount", {
                    count: boundCountForProxy(bindingProxy),
                  })}
                </span>
              </div>
              <p className="mt-2 text-xs text-muted-foreground">
                {t("proxies.bindHint")}
              </p>
            </div>

            <div className="space-y-3 border-b border-border px-5 py-3 sm:px-6">
              <div className="relative">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  value={bindQuery}
                  onChange={(e) => setBindQuery(e.target.value)}
                  placeholder={t("proxies.bindSearchPlaceholder")}
                  className="pl-9"
                />
              </div>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap gap-1.5">
                  {(
                    [
                      ["all", t("proxies.bindFilterAll")],
                      ["unbound", t("proxies.bindFilterUnbound")],
                      ["this", t("proxies.bindFilterThis")],
                      ["other", t("proxies.bindFilterOther")],
                    ] as const
                  ).map(([key, label]) => (
                    <button
                      key={key}
                      type="button"
                      onClick={() => setBindFilter(key)}
                      className={`rounded-full border px-2.5 py-1 text-xs font-semibold transition-colors ${
                        bindFilter === key
                          ? "border-primary/30 bg-primary/10 text-primary"
                          : "border-border text-muted-foreground hover:bg-muted/50 hover:text-foreground"
                      }`}
                    >
                      {label}
                    </button>
                  ))}
                </div>
                <div
                  className="inline-flex items-center rounded-full border border-border bg-muted/30 p-0.5"
                  role="group"
                  aria-label={t("proxies.bindKindGroupLabel")}
                >
                  {(
                    [
                      ["all", t("proxies.bindKindAll")],
                      ["codex", t("proxies.bindKindCodex")],
                      ["grok", t("proxies.bindKindGrok")],
                      ["claude", t("proxies.bindKindClaude")],
                    ] as const
                  ).map(([key, label]) => (
                    <button
                      key={key}
                      type="button"
                      onClick={() => setBindKindFilter(key)}
                      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-semibold transition-colors ${
                        bindKindFilter === key
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      {key === "codex" || key === "grok" || key === "claude" ? (
                        <ChannelLogo channel={key} size={14} />
                      ) : null}
                      {label}
                    </button>
                  ))}
                </div>
              </div>
              <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
                <label className="inline-flex cursor-pointer items-center gap-2">
                  <input
                    type="checkbox"
                    checked={bindVisibleAllSelected}
                    onChange={toggleBindSelectAll}
                    disabled={bindFilteredAccounts.length === 0}
                    className="size-3.5 rounded"
                  />
                  {t("proxies.bindSelectVisible")}
                </label>
                <span>
                  {t("proxies.bindSelectionSummary", {
                    selected: bindSelected.size,
                    shown: bindFilteredAccounts.length,
                    total: bindTotal,
                  })}
                </span>
              </div>
            </div>

            <div className="max-h-[min(420px,50dvh)] overflow-y-auto">
              {accountsLoading ? (
                <div className="flex items-center justify-center gap-2 py-16 text-sm text-muted-foreground">
                  <Loader2 className="size-4 animate-spin" />
                  {t("proxies.bindLoadingAccounts")}
                </div>
              ) : bindFilteredAccounts.length === 0 ? (
                <div className="px-5 py-14 text-center text-sm text-muted-foreground sm:px-6">
                  {accounts.length === 0
                    ? t("proxies.bindNoAccounts")
                    : t("proxies.bindNoMatch")}
                </div>
              ) : (
                <>
                  <ul className="divide-y divide-border/60">
                    {bindRenderedAccounts.map((account) => (
                      <BindAccountRow
                        key={account.id}
                        account={account}
                        checked={bindSelected.has(account.id)}
                        isThis={isAccountBoundToProxy(
                          account,
                          bindingProxy.url,
                        )}
                        onToggle={toggleBindAccount}
                      />
                    ))}
                  </ul>
                  {bindHiddenCount > 0 ? (
                    <div className="border-t border-border/60 px-5 py-3 text-center text-xs text-muted-foreground sm:px-6">
                      {t("proxies.bindListTruncated", {
                        hidden: bindHiddenCount,
                        shown: bindRenderedAccounts.length,
                      })}
                    </div>
                  ) : null}
                  {bindTotal > bindPageSize ? (
                    <div className="border-t border-border/60 px-5 py-3 sm:px-6">
                      <Pagination
                        page={bindPage}
                        totalPages={Math.max(1, Math.ceil(bindTotal / bindPageSize))}
                        totalItems={bindTotal}
                        pageSize={bindPageSize}
                        onPageChange={setBindPage}
                      />
                    </div>
                  ) : null}
                </>
              )}
            </div>
          </div>
        ) : null}
      </Modal>
    </div>
  );
}
