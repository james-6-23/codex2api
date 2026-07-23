import { Fragment, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { useToast } from "../hooks/useToast";
import type { APIKeyAccountStat, APIKeyTokenStat } from "../types";
import { formatCompactEmail } from "../lib/utils";
import { formatUsageNumber } from "../lib/usageFormat";
import { getErrorMessage } from "../utils/error";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  ChevronDown,
  ChevronRight,
  Loader2,
  RefreshCw,
  Search,
  Server,
} from "lucide-react";

// 与 AccountUsageModal 的分布色板保持一致,展开明细用色点区分上游账号。
const ACCOUNT_COLORS = [
  "#0f766e",
  "#2563eb",
  "#d97706",
  "#7c3aed",
  "#dc2626",
  "#059669",
  "#0891b2",
  "#ea580c",
  "#4f46e5",
  "#db2777",
];

type RangeKey = "today" | "week" | "month" | "custom";

type SortKey =
  | "label"
  | "requests"
  | "input_tokens"
  | "output_tokens"
  | "cached_tokens"
  | "total_tokens"
  | "error_count"
  | "user_billed";

type SortDir = "asc" | "desc";

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

function toLocalRFC3339(date: Date): string {
  const offset = date.getTimezoneOffset();
  const sign = offset <= 0 ? "+" : "-";
  const abs = Math.abs(offset);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

function startOfToday(): Date {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate());
}

function startOfWeek(): Date {
  const now = new Date();
  const d = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  // ISO 周：周一为第一天
  const day = d.getDay() || 7; // Sunday → 7
  d.setDate(d.getDate() - (day - 1));
  return d;
}

function startOfMonth(): Date {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), 1);
}

function dateToLocalInputValue(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function localInputValueToDate(value: string): Date | null {
  if (!value) return null;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}

function formatUSD(value: number): string {
  if (value === 0) return "$0.00";
  if (value < 0.01) return `$${value.toFixed(6)}`;
  if (value < 1) return `$${value.toFixed(4)}`;
  return `$${value.toFixed(2)}`;
}

function SortIcon({ active, dir }: { active: boolean; dir: SortDir }) {
  if (!active) return <ArrowUpDown className="size-3 opacity-50" />;
  return dir === "asc" ? <ArrowUp className="size-3" /> : <ArrowDown className="size-3" />;
}

interface APIKeyTokenUsagePanelProps {
  // 父页已拉取的系统设置；传入后优先使用，避免面板独立请求滞后/失败导致开关无效。
  showFullUsageNumbers?: boolean;
}

export default function APIKeyTokenUsagePanel({
  showFullUsageNumbers: showFullUsageNumbersProp,
}: APIKeyTokenUsagePanelProps = {}) {
  const { t, i18n } = useTranslation();
  const { showToast } = useToast();
  const locale = i18n.language;

  const [rangeKey, setRangeKey] = useState<RangeKey>("today");
  const [customStart, setCustomStart] = useState<string>(() =>
    dateToLocalInputValue(startOfToday()),
  );
  const [customEnd, setCustomEnd] = useState<string>(() =>
    dateToLocalInputValue(new Date()),
  );

  const [items, setItems] = useState<APIKeyTokenStat[]>([]);
  const [loading, setLoading] = useState(false);
  const [search, setSearch] = useState("");
  const [sortKey, setSortKey] = useState<SortKey>("total_tokens");
  const [sortDir, setSortDir] = useState<SortDir>("desc");
  // 本地兜底：父页未传 prop 时自行读设置；父页传入后以 prop 为准。
  const [showFullUsageNumbersLocal, setShowFullUsageNumbersLocal] = useState(false);
  const showFullUsageNumbers =
    typeof showFullUsageNumbersProp === "boolean"
      ? showFullUsageNumbersProp
      : showFullUsageNumbersLocal;

  // 展开 Key 行时按需拉取上游账号明细；支持多行同时展开。
  // 缓存按 api_key_id,切换时间范围时整体清空(下方 reload 内重置)。
  const [expandedIds, setExpandedIds] = useState<Set<number>>(() => new Set());
  const [accountData, setAccountData] = useState<Record<number, APIKeyAccountStat[]>>({});
  const [accountLoadingIds, setAccountLoadingIds] = useState<Set<number>>(() => new Set());
  const [accountError, setAccountError] = useState<Record<number, string>>({});

  const range = useMemo(() => {
    const now = new Date();
    if (rangeKey === "today") {
      return { start: toLocalRFC3339(startOfToday()), end: toLocalRFC3339(now) };
    }
    if (rangeKey === "week") {
      return { start: toLocalRFC3339(startOfWeek()), end: toLocalRFC3339(now) };
    }
    if (rangeKey === "month") {
      return { start: toLocalRFC3339(startOfMonth()), end: toLocalRFC3339(now) };
    }
    // custom
    const s = localInputValueToDate(customStart);
    const e = localInputValueToDate(customEnd);
    if (!s || !e) return null;
    if (!(e.getTime() > s.getTime())) return null;
    return { start: toLocalRFC3339(s), end: toLocalRFC3339(e) };
  }, [rangeKey, customStart, customEnd]);

  const refreshFullUsageSetting = async () => {
    // 父页已传 prop 时无需再拉；刷新按钮/切回前台时仍可同步兜底状态。
    if (typeof showFullUsageNumbersProp === "boolean") return;
    try {
      const settings = await api.getSettings();
      setShowFullUsageNumbersLocal(Boolean(settings.show_full_usage_numbers));
    } catch {
      setShowFullUsageNumbersLocal(false);
    }
  };

  const reload = async () => {
    if (!range) return;
    setLoading(true);
    // 范围变化或手动刷新：已展开的账号明细缓存作废。
    setExpandedIds(new Set());
    setAccountLoadingIds(new Set());
    setAccountData({});
    setAccountError({});
    try {
      const [data] = await Promise.all([
        api.getAPIKeyTokenStats(range),
        refreshFullUsageSetting(),
      ]);
      setItems(data.items ?? []);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setLoading(false);
    }
  };

  const toggleExpand = async (item: APIKeyTokenStat) => {
    const id = item.api_key_id;
    // 已展开 → 收起；其它行保持展开状态。
    if (expandedIds.has(id)) {
      setExpandedIds((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
      return;
    }
    setExpandedIds((prev) => {
      const next = new Set(prev);
      next.add(id);
      return next;
    });
    // 已缓存则直接展开，不重复请求。
    if (accountData[id] || !range) return;
    setAccountLoadingIds((prev) => {
      const next = new Set(prev);
      next.add(id);
      return next;
    });
    setAccountError((prev) => {
      const next = { ...prev };
      delete next[id];
      return next;
    });
    try {
      const data = await api.getAPIKeyAccountStats(id, range);
      setAccountData((prev) => ({ ...prev, [id]: data.items ?? [] }));
    } catch (err) {
      setAccountError((prev) => ({ ...prev, [id]: getErrorMessage(err) }));
    } finally {
      setAccountLoadingIds((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    }
  };

  // 切换 range 自动加载
  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range?.start, range?.end]);

  useEffect(() => {
    if (typeof showFullUsageNumbersProp === "boolean") {
      setShowFullUsageNumbersLocal(showFullUsageNumbersProp);
      return;
    }
    let active = true;
    void (async () => {
      try {
        const settings = await api.getSettings();
        if (active) setShowFullUsageNumbersLocal(Boolean(settings.show_full_usage_numbers));
      } catch {
        if (active) setShowFullUsageNumbersLocal(false);
      }
    })();
    return () => {
      active = false;
    };
  }, [showFullUsageNumbersProp]);

  // 从设置页改完开关再切回本页时，若组件仍挂载则靠可见性同步一次。
  useEffect(() => {
    if (typeof showFullUsageNumbersProp === "boolean") return;
    const onVisible = () => {
      if (document.visibilityState === "visible") {
        void refreshFullUsageSetting();
      }
    };
    document.addEventListener("visibilitychange", onVisible);
    window.addEventListener("focus", onVisible);
    return () => {
      document.removeEventListener("visibilitychange", onVisible);
      window.removeEventListener("focus", onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [showFullUsageNumbersProp]);

  const filteredItems = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return items;
    return items.filter((it) => {
      const label = it.label?.toLowerCase() ?? "";
      const masked = it.api_key_masked?.toLowerCase() ?? "";
      const name = it.api_key_name?.toLowerCase() ?? "";
      return label.includes(q) || masked.includes(q) || name.includes(q);
    });
  }, [items, search]);

  const sortedItems = useMemo(() => {
    const copy = [...filteredItems];
    copy.sort((a, b) => {
      let diff = 0;
      switch (sortKey) {
        case "label":
          diff = (a.label || "").localeCompare(b.label || "");
          break;
        case "requests":
          diff = a.requests - b.requests;
          break;
        case "input_tokens":
          diff = a.input_tokens - b.input_tokens;
          break;
        case "output_tokens":
          diff = a.output_tokens - b.output_tokens;
          break;
        case "cached_tokens":
          diff = a.cached_tokens - b.cached_tokens;
          break;
        case "total_tokens":
          diff = a.total_tokens - b.total_tokens;
          break;
        case "error_count":
          diff = a.error_count - b.error_count;
          break;
        case "user_billed":
          diff = a.user_billed - b.user_billed;
          break;
      }
      return sortDir === "asc" ? diff : -diff;
    });
    return copy;
  }, [filteredItems, sortKey, sortDir]);

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    } else {
      setSortKey(key);
      // 字符串列默认升序，数值列默认降序
      setSortDir(key === "label" ? "asc" : "desc");
    }
  };

  const rangeChips: { key: RangeKey; label: string }[] = [
    { key: "today", label: t("apiKeys.tokenUsageRangeToday") },
    { key: "week", label: t("apiKeys.tokenUsageRangeWeek") },
    { key: "month", label: t("apiKeys.tokenUsageRangeMonth") },
    { key: "custom", label: t("apiKeys.tokenUsageRangeCustom") },
  ];

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="text-base font-semibold tracking-tight text-foreground">
            {t("apiKeys.tokenUsageTitle")}
          </h3>
          <p className="mt-1 max-w-2xl text-sm leading-relaxed text-muted-foreground">
            {t("apiKeys.tokenUsageDesc")}
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          className="shrink-0"
          onClick={() => void reload()}
          disabled={loading}
          title={t("common.refresh")}
        >
          <RefreshCw className={`size-3.5 ${loading ? "animate-spin" : ""}`} />
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-2.5">
        <div className="inline-flex items-center gap-0.5 rounded-xl border border-border/80 bg-muted/25 p-1 shadow-sm">
          {rangeChips.map((chip) => (
            <button
              key={chip.key}
              type="button"
              onClick={() => setRangeKey(chip.key)}
              className={`shrink-0 whitespace-nowrap rounded-lg px-2.5 py-1.5 text-xs font-semibold transition-all ${
                rangeKey === chip.key
                  ? "bg-primary text-primary-foreground shadow-sm"
                  : "text-muted-foreground hover:bg-background/80 hover:text-foreground"
              }`}
            >
              {chip.label}
            </button>
          ))}
        </div>

        {rangeKey === "custom" && (
          <div className="flex flex-wrap items-center gap-2 rounded-xl border border-border/80 bg-muted/15 px-2.5 py-1.5">
            <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
              {t("apiKeys.tokenUsageStartLabel")}
              <Input
                type="datetime-local"
                value={customStart}
                onChange={(e) => setCustomStart(e.target.value)}
                className="h-8 w-auto text-[12px]"
              />
            </label>
            <span className="text-muted-foreground/50">→</span>
            <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
              {t("apiKeys.tokenUsageEndLabel")}
              <Input
                type="datetime-local"
                value={customEnd}
                onChange={(e) => setCustomEnd(e.target.value)}
                className="h-8 w-auto text-[12px]"
              />
            </label>
          </div>
        )}

        <div className="relative ml-auto w-64 max-sm:ml-0 max-sm:w-full">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t("apiKeys.tokenUsageSearchPlaceholder")}
            className="h-9 rounded-xl border-border/80 bg-background pl-9 text-[13px] shadow-sm"
          />
        </div>
      </div>

      <div className="flex items-center justify-between gap-2">
        <span className="inline-flex items-center rounded-full bg-muted/60 px-2.5 py-0.5 text-xs font-medium tabular-nums text-muted-foreground">
          {t("apiKeys.tokenUsageRowCount", { count: sortedItems.length })}
        </span>
        <span className="hidden text-[11px] text-muted-foreground sm:inline">
          {t("apiKeys.keyAccountsHint")}
        </span>
      </div>

      <div className="data-table-shell">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("label")}
                >
                  {t("common.name")}
                  <SortIcon active={sortKey === "label"} dir={sortDir} />
                </button>
              </TableHead>
              <TableHead className="text-right">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("requests")}
                >
                  {t("apiKeys.tokenUsageColRequests")}
                  <SortIcon active={sortKey === "requests"} dir={sortDir} />
                </button>
              </TableHead>
              <TableHead className="text-right">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("input_tokens")}
                >
                  {t("apiKeys.tokenUsageColInput")}
                  <SortIcon active={sortKey === "input_tokens"} dir={sortDir} />
                </button>
              </TableHead>
              <TableHead className="text-right">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("output_tokens")}
                >
                  {t("apiKeys.tokenUsageColOutput")}
                  <SortIcon active={sortKey === "output_tokens"} dir={sortDir} />
                </button>
              </TableHead>
              <TableHead className="text-right">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("cached_tokens")}
                >
                  {t("apiKeys.tokenUsageColCached")}
                  <SortIcon active={sortKey === "cached_tokens"} dir={sortDir} />
                </button>
              </TableHead>
              <TableHead className="text-right">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("total_tokens")}
                >
                  {t("apiKeys.tokenUsageColTotal")}
                  <SortIcon active={sortKey === "total_tokens"} dir={sortDir} />
                </button>
              </TableHead>
              <TableHead className="text-right">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("error_count")}
                >
                  {t("apiKeys.tokenUsageColErrors")}
                  <SortIcon active={sortKey === "error_count"} dir={sortDir} />
                </button>
              </TableHead>
              <TableHead className="text-right">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-semibold hover:text-foreground"
                  onClick={() => toggleSort("user_billed")}
                >
                  {t("apiKeys.tokenUsageColCost")}
                  <SortIcon active={sortKey === "user_billed"} dir={sortDir} />
                </button>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sortedItems.length === 0 ? (
              <TableRow>
                <TableCell colSpan={8} className="text-center text-sm text-muted-foreground">
                  {t("apiKeys.tokenUsageEmpty")}
                </TableCell>
              </TableRow>
            ) : (
              sortedItems.map((item) => {
                const expanded = expandedIds.has(item.api_key_id);
                const isLoadingAccounts = accountLoadingIds.has(item.api_key_id);
                return (
                  <Fragment key={`${item.api_key_id}-${item.label}`}>
                    <TableRow
                      className={
                        expanded
                          ? "border-b-0 bg-primary/[0.04] hover:bg-primary/[0.06]"
                          : "hover:bg-muted/40"
                      }
                    >
                      <TableCell>
                        <button
                          type="button"
                          onClick={() => void toggleExpand(item)}
                          className="group flex max-w-full items-start gap-2 text-left"
                          title={t("apiKeys.keyAccountsToggle")}
                          aria-expanded={expanded}
                        >
                          <span
                            className={`mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-md border transition-colors ${
                              expanded
                                ? "border-primary/30 bg-primary/10 text-primary"
                                : "border-border bg-muted/40 text-muted-foreground group-hover:border-primary/25 group-hover:bg-primary/5 group-hover:text-foreground"
                            }`}
                          >
                            {isLoadingAccounts ? (
                              <Loader2 className="size-3.5 animate-spin" />
                            ) : expanded ? (
                              <ChevronDown className="size-3.5" />
                            ) : (
                              <ChevronRight className="size-3.5" />
                            )}
                          </span>
                          <span className="flex min-w-0 flex-col gap-1">
                            <span className="truncate font-medium text-foreground transition-colors group-hover:text-primary">
                              {formatCompactEmail(item.label) || item.label || "—"}
                            </span>
                            {item.api_key_masked && (
                              <Badge
                                variant="secondary"
                                className="w-fit font-mono text-[10px] tracking-wide"
                              >
                                {item.api_key_masked}
                              </Badge>
                            )}
                          </span>
                        </button>
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatUsageNumber(item.requests, showFullUsageNumbers, locale)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatUsageNumber(item.input_tokens, showFullUsageNumbers, locale)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatUsageNumber(item.output_tokens, showFullUsageNumbers, locale)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatUsageNumber(item.cached_tokens, showFullUsageNumbers, locale)}
                      </TableCell>
                      <TableCell className="text-right font-semibold tabular-nums">
                        {formatUsageNumber(item.total_tokens, showFullUsageNumbers, locale)}
                      </TableCell>
                      <TableCell
                        className={`text-right tabular-nums ${
                          item.error_count > 0 ? "text-red-600 dark:text-red-400" : ""
                        }`}
                      >
                        {formatUsageNumber(item.error_count, showFullUsageNumbers, locale)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums text-emerald-700 dark:text-emerald-400">
                        {formatUSD(item.user_billed)}
                      </TableCell>
                    </TableRow>
                    {expanded && (
                      <TableRow className="hover:bg-transparent">
                        <TableCell colSpan={8} className="border-t-0 bg-muted/15 p-0">
                          <div className="border-l-2 border-primary/40 ml-3 sm:ml-4">
                            <KeyAccountBreakdown
                              loading={isLoadingAccounts}
                              error={accountError[item.api_key_id]}
                              rows={accountData[item.api_key_id]}
                              showFull={showFullUsageNumbers}
                              locale={locale}
                            />
                          </div>
                        </TableCell>
                      </TableRow>
                    )}
                  </Fragment>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

function accountPrimaryLabel(a: APIKeyAccountStat): string {
  // 优先邮箱；无邮箱时再退回 name / id。不额外展示 name 副标题（常为 account-1 这类占位名）。
  return a.account_email?.trim() || a.account_name?.trim() || `#${a.account_id}`;
}

function formatPercent(value: number): string {
  const n = Number(value || 0);
  return `${n.toFixed(n >= 10 ? 1 : 2)}%`;
}

// KeyAccountBreakdown 渲染某个 Key 展开后的"各上游账号"明细：
// 卡片网格 + 色点占比条,视觉对齐账号用量里的 Key 分布。
function KeyAccountBreakdown({
  loading,
  error,
  rows,
  showFull,
  locale,
}: {
  loading: boolean;
  error?: string;
  rows?: APIKeyAccountStat[];
  showFull: boolean;
  locale: string;
}) {
  const { t } = useTranslation();

  if (loading) {
    return (
      <div className="space-y-3 px-3 py-4 sm:px-4">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" />
          {t("common.loading")}
        </div>
        <div className="grid gap-2 sm:grid-cols-2">
          {[0, 1].map((i) => (
            <div
              key={i}
              className="h-[76px] animate-pulse rounded-xl border border-border/60 bg-muted/40"
            />
          ))}
        </div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="mx-3 my-3 rounded-xl border border-red-200 bg-red-50 px-3 py-3 text-xs text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400 sm:mx-4">
        {error}
      </div>
    );
  }

  if (!rows || rows.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center gap-1.5 px-4 py-6 text-center">
        <span className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <Server className="size-4" />
        </span>
        <p className="text-xs text-muted-foreground">{t("apiKeys.keyAccountsEmpty")}</p>
      </div>
    );
  }

  const totalRequests = rows.reduce((sum, a) => sum + a.requests, 0);
  const totalTokens = rows.reduce((sum, a) => sum + a.total_tokens, 0);
  const totalBilled = rows.reduce((sum, a) => sum + a.user_billed, 0);

  return (
    <div className="space-y-3 px-3 py-3.5 sm:px-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="flex size-7 items-center justify-center rounded-lg bg-primary/10 text-primary">
            <Server className="size-3.5" />
          </span>
          <div>
            <div className="text-xs font-semibold text-foreground">
              {t("apiKeys.keyAccountsTitle")}
            </div>
            <div className="text-[11px] text-muted-foreground">
              {t("apiKeys.keyAccountsCount", { count: rows.length })}
            </div>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="rounded-full bg-muted px-2.5 py-0.5 text-[11px] font-medium tabular-nums text-muted-foreground">
            {formatUsageNumber(totalRequests, showFull, locale)} {t("apiKeys.keyAccountsReqUnit")}
          </span>
          <span className="rounded-full bg-muted px-2.5 py-0.5 text-[11px] font-medium tabular-nums text-muted-foreground">
            {formatUsageNumber(totalTokens, showFull, locale)} {t("apiKeys.keyAccountsTokUnit")}
          </span>
          <span className="rounded-full bg-emerald-500/10 px-2.5 py-0.5 text-[11px] font-semibold tabular-nums text-emerald-700 dark:text-emerald-400">
            {formatUSD(totalBilled)}
          </span>
        </div>
      </div>

      <div className="grid gap-2 sm:grid-cols-2">
        {rows.map((a, i) => {
          const share =
            totalRequests > 0 ? Math.min(100, (a.requests / totalRequests) * 100) : 0;
          const color = ACCOUNT_COLORS[i % ACCOUNT_COLORS.length];
          return (
            <div
              key={a.account_id}
              className="rounded-xl border border-border/80 bg-background/80 px-3 py-2.5 shadow-sm backdrop-blur-sm transition-colors hover:border-border"
            >
              <div className="mb-2 grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2">
                <span
                  className="size-2.5 shrink-0 rounded-full ring-2 ring-background"
                  style={{ background: color }}
                />
                <div className="min-w-0 truncate text-sm font-medium text-foreground">
                  {accountPrimaryLabel(a)}
                </div>
                <span className="shrink-0 tabular-nums text-xs font-semibold text-muted-foreground">
                  {formatPercent(share)}
                </span>
              </div>
              <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                <div
                  className="h-full rounded-full transition-[width] duration-300"
                  style={{ width: `${share}%`, background: color }}
                />
              </div>
              <div className="mt-2 flex flex-wrap items-center justify-between gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
                <span className="tabular-nums">
                  <span className="font-semibold text-foreground">
                    {formatUsageNumber(a.requests, showFull, locale)}
                  </span>{" "}
                  {t("apiKeys.keyAccountsReqUnit")}
                  <span className="mx-1 text-border">·</span>
                  <span className="font-semibold text-foreground">
                    {formatUsageNumber(a.total_tokens, showFull, locale)}
                  </span>{" "}
                  {t("apiKeys.keyAccountsTokUnit")}
                </span>
                <span className="flex items-center gap-1.5 tabular-nums">
                  {a.error_count > 0 && (
                    <span className="rounded-md bg-red-500/10 px-1.5 py-0.5 font-medium text-red-600 dark:text-red-400">
                      {formatUsageNumber(a.error_count, showFull, locale)}{" "}
                      {t("apiKeys.keyAccountsErrUnit")}
                    </span>
                  )}
                  <span className="font-semibold text-emerald-700 dark:text-emerald-400">
                    {formatUSD(a.user_billed)}
                  </span>
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
