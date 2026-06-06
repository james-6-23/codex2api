import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { useToast } from "../hooks/useToast";
import type { APIKeyTokenStat } from "../types";
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
import { ArrowDown, ArrowUp, ArrowUpDown, RefreshCw, Search } from "lucide-react";

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

function formatNumber(value: number): string {
  return value.toLocaleString();
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

export default function APIKeyTokenUsagePanel() {
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
  const [showFullUsageNumbers, setShowFullUsageNumbers] = useState(false);

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

  const reload = async () => {
    if (!range) return;
    setLoading(true);
    try {
      const data = await api.getAPIKeyTokenStats(range);
      setItems(data.items ?? []);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setLoading(false);
    }
  };

  // 切换 range 自动加载
  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range?.start, range?.end]);

  useEffect(() => {
    let active = true;
    const loadSettings = async () => {
      try {
        const settings = await api.getSettings();
        if (active) setShowFullUsageNumbers(settings.show_full_usage_numbers);
      } catch {
        if (active) setShowFullUsageNumbers(false);
      }
    };
    void loadSettings();
    return () => {
      active = false;
    };
  }, []);

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
      <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
        <div>
          <h3 className="text-base font-semibold text-foreground">
            {t("apiKeys.tokenUsageTitle")}
          </h3>
          <p className="mt-1 text-sm text-muted-foreground">
            {t("apiKeys.tokenUsageDesc")}
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void reload()} disabled={loading}>
          <RefreshCw className={`size-3.5 ${loading ? "animate-spin" : ""}`} />
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <div className="inline-flex items-center gap-1 rounded-lg border border-border bg-muted/30 p-0.5">
          {rangeChips.map((chip) => (
            <button
              key={chip.key}
              type="button"
              onClick={() => setRangeKey(chip.key)}
              className={`shrink-0 whitespace-nowrap rounded-md px-2.5 py-1 text-xs font-semibold transition-colors ${
                rangeKey === chip.key
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {chip.label}
            </button>
          ))}
        </div>

        {rangeKey === "custom" && (
          <div className="flex flex-wrap items-center gap-2">
            <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
              {t("apiKeys.tokenUsageStartLabel")}
              <Input
                type="datetime-local"
                value={customStart}
                onChange={(e) => setCustomStart(e.target.value)}
                className="h-8 w-auto text-[12px]"
              />
            </label>
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

        <div className="relative ml-auto w-64 max-sm:w-full">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t("apiKeys.tokenUsageSearchPlaceholder")}
            className="h-8 rounded-lg pl-9 text-[13px]"
          />
        </div>
      </div>

      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span>{t("apiKeys.tokenUsageRowCount", { count: sortedItems.length })}</span>
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
              sortedItems.map((item) => (
                <TableRow key={`${item.api_key_id}-${item.label}`}>
                  <TableCell>
                    <div className="flex flex-col gap-0.5">
                      <span className="font-medium text-foreground">
                        {formatCompactEmail(item.label) || item.label || "—"}
                      </span>
                      {item.api_key_masked && (
                        <Badge variant="secondary" className="w-fit font-mono text-[10px]">
                          {item.api_key_masked}
                        </Badge>
                      )}
                    </div>
                  </TableCell>
                  <TableCell className="text-right tabular-nums">{formatNumber(item.requests)}</TableCell>
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
                    {formatNumber(item.error_count)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums text-emerald-700 dark:text-emerald-400">
                    {formatUSD(item.user_billed)}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}
