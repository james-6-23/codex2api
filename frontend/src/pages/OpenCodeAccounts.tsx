import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  Check,
  ChevronDown,
  Download,
  Eye,
  EyeOff,
  FolderOpen,
  Globe,
  KeyRound,
  Loader2,
  Pencil,
  Plus,
  Power,
  PowerOff,
  RefreshCw,
  Search,
  Sliders,
  SlidersHorizontal,
  Trash2,
  Upload,
  X,
  Zap,
} from "lucide-react";

import { api } from "../api";
import type { NamedBlob, ProxyRow } from "../api";
import type {
  APIKeyRow,
  AccountGroup,
  AccountListSummary,
  AccountRow,
  AccountsPageParams,
  OpenCodeMode,
  OpenCodeNativeProtocol,
  OpenCodeProtocolRule,
  OpenCodeUsageSnapshot,
  OpenCodeUsageWindow,
} from "../types";
import AccountDetailSheet from "../components/AccountDetailSheet";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  isAccountGroupFilterEmpty,
  pruneAccountGroupFilter,
} from "../components/AccountGroupFilterSelect";
import type { AccountGroupFilterValue } from "../components/AccountGroupFilterSelect";
import { AccountGroupManagerModal } from "../components/AccountGroupManagerModal";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import AccountProxyBadge from "../components/AccountProxyBadge";
import AccountProxyQuickEditor from "../components/AccountProxyQuickEditor";
import { accountStateTableRowClass } from "../components/AccountStateOverlay";
import APIKeyMultiSelect from "../components/APIKeyMultiSelect";
import BatchAccountGroupModal from "../components/BatchAccountGroupModal";
import ChannelLogo from "../components/ChannelLogo";
import ChipInput from "../components/ChipInput";
import { CompactStat } from "../components/CompactStat";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import { ProxyField } from "../components/ProxyField";
import { SegmentedTabs } from "../components/SegmentedTabs";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import TestConnectionModal from "../components/TestConnectionModal";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { FilterChipBar } from "@/components/ui/beautiful";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from "../hooks/usePersistedPageSize";
import { useToast } from "../hooks/useToast";
import { buildProxyBindingContext } from "../lib/accountProxyBinding";
import { getAccountStatusBadgeStatus } from "../lib/usageFormat";
import { cn } from "@/lib/utils";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";

const TEXTAREA_CLASS =
  "flex min-h-[168px] w-full resize-y rounded-lg border border-input bg-background px-3 py-2.5 font-mono text-xs leading-relaxed text-foreground shadow-2xs transition-colors placeholder:text-muted-foreground/80 focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30";

const DEFAULT_GO_BASE_URL = "https://opencode.ai/zen/go/v1";
const DEFAULT_GO_ANTHROPIC_URL = "https://opencode.ai/zen/go";
const DEFAULT_ZEN_BASE_URL = "https://opencode.ai/zen/v1";
const DEFAULT_ZEN_ANTHROPIC_URL = "https://opencode.ai/zen";

const DEFAULT_GO_RULES: OpenCodeProtocolRule[] = [
  { pattern: "grok-*", protocol: "responses" },
  { pattern: "gpt-*", protocol: "responses" },
  { pattern: "muse-spark-*", protocol: "responses" },
  { pattern: "minimax-*", protocol: "anthropic" },
  { pattern: "qwen*", protocol: "anthropic" },
];

const DEFAULT_ZEN_RULES: OpenCodeProtocolRule[] = [
  { pattern: "grok-*", protocol: "responses" },
  { pattern: "gpt-*", protocol: "responses" },
  { pattern: "muse-spark-*", protocol: "responses" },
  { pattern: "claude-*", protocol: "anthropic" },
  { pattern: "qwen3.8-max", protocol: "chat_completions" },
  { pattern: "qwen*", protocol: "anthropic" },
];

const PROTOCOL_OPTIONS = [
  { value: "chat_completions", label: "Chat Completions" },
  { value: "responses", label: "Responses" },
  { value: "anthropic", label: "Anthropic Messages" },
];

const MODE_CARD_OPTIONS: Array<{
  value: OpenCodeMode;
  title: string;
  badge: string;
  badgeClassName: string;
  description: string;
}> = [
  {
    value: "go",
    title: "OpenCode Go",
    badge: "订阅套餐",
    badgeClassName: "bg-emerald-500/12 text-emerald-700 ring-emerald-500/20 dark:text-emerald-300",
    description: "支持 5h / 周 / 月三窗口额度自动同步",
  },
  {
    value: "zen",
    title: "OpenCode Zen",
    badge: "按量付费",
    badgeClassName: "bg-sky-500/12 text-sky-700 ring-sky-500/20 dark:text-sky-300",
    description: "按量计费网关，无需额度窗口同步",
  },
];

function OpenCodeModeCards({
  value,
  onChange,
  disabled = false,
}: {
  value: OpenCodeMode;
  onChange: (next: OpenCodeMode) => void;
  disabled?: boolean;
}) {
  return (
    <div role="radiogroup" className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
      {MODE_CARD_OPTIONS.map((option) => {
        const active = value === option.value;
        return (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={active}
            disabled={disabled}
            onClick={() => onChange(option.value)}
            className={cn(
              "group relative flex flex-col items-start gap-1 rounded-xl border px-3.5 py-2.5 text-left transition-all outline-none focus-visible:ring-2 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-60",
              active
                ? "border-primary/70 bg-primary/[0.045] shadow-2xs ring-1 ring-primary/15"
                : "border-border/80 bg-background hover:border-border hover:bg-muted/30",
            )}
          >
            <div className="flex w-full items-center justify-between gap-2">
              <div className="flex min-w-0 items-center gap-2">
                <span className="text-xs font-semibold text-foreground">{option.title}</span>
                <span
                  className={cn(
                    "inline-flex shrink-0 items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
                    option.badgeClassName,
                  )}
                >
                  {option.badge}
                </span>
              </div>
              <span
                className={cn(
                  "flex size-4 shrink-0 items-center justify-center rounded-full border transition-colors",
                  active
                    ? "border-primary bg-primary text-primary-foreground"
                    : "border-muted-foreground/30 bg-background group-hover:border-muted-foreground/50",
                )}
              >
                {active ? <Check className="size-2.5 stroke-[3]" /> : null}
              </span>
            </div>
            <p className="text-[11px] leading-snug text-muted-foreground">{option.description}</p>
          </button>
        );
      })}
    </div>
  );
}

function FieldLabel({
  children,
  required,
  optional,
  hint,
}: {
  children: ReactNode;
  required?: boolean;
  optional?: boolean;
  hint?: string;
}) {
  return (
    <div className="mb-1.5 flex flex-wrap items-baseline justify-between gap-x-2 gap-y-0.5">
      <label className="inline-flex items-center gap-1.5 text-xs font-medium text-foreground">
        <span>{children}</span>
        {required ? <span className="text-destructive">*</span> : null}
        {optional ? <span className="font-normal text-muted-foreground/80">可选</span> : null}
      </label>
      {hint ? <span className="text-[11px] text-muted-foreground">{hint}</span> : null}
    </div>
  );
}

function detectImportSummary(content: string): string | null {
  const trimmed = content.trim();
  if (!trimmed) return null;
  if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
    try {
      const parsed = JSON.parse(trimmed);
      if (Array.isArray(parsed)) return `JSON 数组 · ${parsed.length} 项`;
      if (parsed && typeof parsed === "object") {
        const accounts = (parsed as { accounts?: unknown; data?: unknown }).accounts ?? (parsed as { data?: unknown }).data;
        if (Array.isArray(accounts)) return `JSON 对象 · ${accounts.length} 条记录`;
        return "JSON 对象";
      }
    } catch {
      return "JSON 格式未闭合";
    }
  }
  const lines = trimmed.split(/\r?\n/).filter((line) => line.trim() !== "");
  return `文本列表 · ${lines.length} 行`;
}

const STATUS_OPTIONS = [
  { value: "all", label: "全部状态" },
  { value: "normal", label: "可用" },
  { value: "rate_limited", label: "限流" },
  { value: "abnormal", label: "异常" },
  { value: "disabled", label: "已暂停" },
];

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
  return account.name || account.email || `#${account.id}`;
}

function barColor(pct: number): string {
  if (pct >= 90) return "bg-destructive";
  if (pct >= 70) return "bg-amber-500";
  return "bg-emerald-500";
}

function UsageWindowRow({ label, win }: { label: string; win?: OpenCodeUsageWindow }) {
  if (!win) return null;
  const pct = Math.max(0, Math.min(100, Number.isFinite(win.percent) ? win.percent : 0));
  const resetHint = win.resets_at ? formatRelativeTime(win.resets_at) : "";
  return (
    <div
      className="flex items-center gap-2 text-[11px] leading-none"
      title={win.resets_at ? `${label}: ${pct.toFixed(1)}% · 重置 ${formatBeijingTime(win.resets_at)}` : `${label}: ${pct.toFixed(1)}%`}
    >
      <span className="w-6 shrink-0 text-muted-foreground">{label}</span>
      <div className="h-1.5 w-16 overflow-hidden rounded-full bg-muted">
        <div className={cn("h-full rounded-full transition-all", barColor(pct))} style={{ width: `${pct}%` }} />
      </div>
      <span className="w-9 shrink-0 text-right font-mono tabular-nums text-foreground/90">{pct.toFixed(0)}%</span>
      {resetHint ? <span className="truncate text-[10px] text-muted-foreground">{resetHint}</span> : null}
    </div>
  );
}

function OpenCodeUsageCell({ account }: { account: AccountRow }) {
  const mode: OpenCodeMode = account.opencode_mode === "zen" ? "zen" : "go";
  if (mode === "zen") {
    return (
      <span className="inline-flex items-center rounded-md bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
        按量计费 (Zen)
      </span>
    );
  }
  const usage: OpenCodeUsageSnapshot | undefined = account.opencode_usage;
  if (!usage) {
    return <span className="text-xs text-muted-foreground">未同步</span>;
  }
  if (usage.status !== "ok") {
    return (
      <span
        className="inline-flex items-center rounded-md bg-destructive/10 px-2 py-0.5 text-[11px] text-destructive"
        title={usage.last_error || usage.status}
      >
        {usage.status === "unauthorized" ? "认证失效 (401)" : `同步失败${usage.http_status ? ` (${usage.http_status})` : ""}`}
      </span>
    );
  }
  return (
    <div className="space-y-1">
      <UsageWindowRow label="5h" win={usage.rolling} />
      <UsageWindowRow label="周" win={usage.weekly} />
      <UsageWindowRow label="月" win={usage.monthly} />
    </div>
  );
}

function ProtocolRulesEditor({
  mode,
  rules,
  onChange,
}: {
  mode: OpenCodeMode;
  rules: OpenCodeProtocolRule[];
  onChange: (next: OpenCodeProtocolRule[]) => void;
}) {
  const defaults = mode === "zen" ? DEFAULT_ZEN_RULES : DEFAULT_GO_RULES;
  return (
    <div className="space-y-2.5 rounded-xl border border-border/80 bg-muted/20 p-3.5">
      <div className="space-y-1.5">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <span className="text-xs font-semibold text-foreground">协议路由规则</span>
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
              protocol_rules
            </code>
          </div>
          <div className="flex flex-wrap items-center gap-1.5">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="h-7 px-2.5 text-[11px]"
              onClick={() => onChange(defaults.map((r) => ({ ...r })))}
            >
              载入 {mode === "zen" ? "Zen" : "Go"} 默认
            </Button>
            {rules.length > 0 && (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-7 px-2 text-[11px] text-muted-foreground hover:text-foreground"
                onClick={() => onChange([])}
              >
                恢复内置
              </Button>
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="h-7 px-2.5 text-[11px]"
              onClick={() => onChange([...rules, { pattern: "", protocol: "chat_completions" }])}
            >
              <Plus className="mr-1 size-3" /> 添加规则
            </Button>
          </div>
        </div>
        <p className="text-[11px] leading-relaxed text-muted-foreground">
          留空时使用 {mode === "zen" ? "Zen" : "Go"} 内置规则；按顺序首条命中，支持精确名或末尾{" "}
          <code className="rounded bg-muted px-1 font-mono text-[10px]">*</code>，未命中回落 Chat Completions。
        </p>
      </div>

      {rules.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border/80 bg-background/60 p-2.5">
          <div className="mb-1.5 text-[10px] font-medium uppercase tracking-wider text-muted-foreground">
            当前生效内置规则
          </div>
          <div className="flex flex-wrap gap-1.5">
            {defaults.map((r) => (
              <span
                key={`${r.pattern}-${r.protocol}`}
                className="inline-flex items-center gap-1 rounded-md border border-border/70 bg-background px-2 py-0.5 font-mono text-[11px] shadow-2xs"
              >
                <span className="text-foreground/90">{r.pattern}</span>
                <span className="text-muted-foreground/70">→</span>
                <span className="text-primary">{r.protocol}</span>
              </span>
            ))}
            <span className="inline-flex items-center gap-1 rounded-md border border-dashed border-border/70 bg-muted/40 px-2 py-0.5 font-mono text-[11px] text-muted-foreground">
              <span>其余</span>
              <span>→</span>
              <span>chat_completions</span>
            </span>
          </div>
        </div>
      ) : (
        <div className="space-y-2">
          {rules.map((rule, idx) => (
            <div key={idx} className="flex items-center gap-2">
              <span className="w-5 shrink-0 text-center font-mono text-[11px] text-muted-foreground/70">
                {idx + 1}
              </span>
              <Input
                value={rule.pattern}
                onChange={(e) => {
                  const next = [...rules];
                  next[idx] = { ...rule, pattern: e.target.value };
                  onChange(next);
                }}
                placeholder="例如 grok-* / claude-* / *"
                className="h-8 min-w-0 flex-1 font-mono text-xs"
              />
              <Select
                value={rule.protocol}
                onValueChange={(value) => {
                  const next = [...rules];
                  next[idx] = { ...rule, protocol: value as OpenCodeNativeProtocol };
                  onChange(next);
                }}
                options={PROTOCOL_OPTIONS}
                compact
                className="w-40 shrink-0 sm:w-44"
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="size-8 shrink-0 text-muted-foreground hover:text-destructive"
                aria-label="删除规则"
                onClick={() => onChange(rules.filter((_, i) => i !== idx))}
              >
                <X className="size-3.5" />
              </Button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function parseHeadersText(text: string): { headers: Record<string, string>; error: string | null } {
  const trimmed = text.trim();
  if (!trimmed) return { headers: {}, error: null };
  try {
    const parsed = JSON.parse(trimmed);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { headers: {}, error: '自定义请求头必须是 JSON 对象，例如 {"User-Agent":"opencode/1.19.6"}' };
    }
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (k.trim()) headers[k.trim()] = String(v ?? "");
    }
    return { headers, error: null };
  } catch {
    return { headers: {}, error: "自定义请求头 JSON 格式无效" };
  }
}

export default function OpenCodeAccounts({ headerSlot }: { headerSlot?: ReactNode }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm } = useConfirmDialog();

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [summary, setSummary] = useState<AccountListSummary | null>(null);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize("opencode-accounts", 20, DEFAULT_PAGE_SIZE_OPTIONS);

  const [searchInput, setSearchInput] = useState("");
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("all");
  const [modeFilter, setModeFilter] = useState<"all" | OpenCodeMode>("all");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(EMPTY_ACCOUNT_GROUP_FILTER);

  const [groups, setGroups] = useState<AccountGroup[]>([]);
  const [proxies, setProxies] = useState<ProxyRow[]>([]);
  const [apiKeys, setApiKeys] = useState<APIKeyRow[]>([]);
  const [selectedIds, setSelectedIds] = useState<Set<number>>(new Set());

  const [syncingAll, setSyncingAll] = useState(false);
  const [syncingId, setSyncingId] = useState<number | null>(null);
  const [exporting, setExporting] = useState(false);

  const [showAddModal, setShowAddModal] = useState(false);
  const [addTab, setAddTab] = useState<"single" | "import">("single");
  const [addSubmitting, setAddSubmitting] = useState(false);
  const [addName, setAddName] = useState("");
  const [addEmail, setAddEmail] = useState("");
  const [addAPIKey, setAddAPIKey] = useState("");
  const [addMode, setAddMode] = useState<OpenCodeMode>("go");
  const [addBaseURL, setAddBaseURL] = useState("");
  const [addAnthropicBaseURL, setAddAnthropicBaseURL] = useState("");
  const [addProxyURL, setAddProxyURL] = useState("");
  const [addModels, setAddModels] = useState<string[]>([]);
  const [addModelMapping, setAddModelMapping] = useState("");
  const [addProtocolRules, setAddProtocolRules] = useState<OpenCodeProtocolRule[]>([]);
  const [addCustomHeadersText, setAddCustomHeadersText] = useState("");
  const [addAutoSync, setAddAutoSync] = useState(true);
  const [showAddAPIKey, setShowAddAPIKey] = useState(false);
  const [showAddAdvanced, setShowAddAdvanced] = useState(false);

  const [importContent, setImportContent] = useState("");
  const [importMode, setImportMode] = useState<OpenCodeMode>("go");
  const [importBaseURL, setImportBaseURL] = useState("");
  const [importProxyURL, setImportProxyURL] = useState("");
  const [importAutoSync, setImportAutoSync] = useState(true);
  const importFileRef = useRef<HTMLInputElement | null>(null);

  const [editingAccount, setEditingAccount] = useState<AccountRow | null>(null);
  const [editLoading, setEditLoading] = useState(false);
  const [editSaving, setEditSaving] = useState(false);
  const [editName, setEditName] = useState("");
  const [editEmail, setEditEmail] = useState("");
  const [editAPIKey, setEditAPIKey] = useState("");
  const [showEditAPIKey, setShowEditAPIKey] = useState(false);
  const [editMode, setEditMode] = useState<OpenCodeMode>("go");
  const [editBaseURL, setEditBaseURL] = useState("");
  const [editAnthropicBaseURL, setEditAnthropicBaseURL] = useState("");
  const [editProxyURL, setEditProxyURL] = useState("");
  const [editModels, setEditModels] = useState<string[]>([]);
  const [editModelMapping, setEditModelMapping] = useState("");
  const [editProtocolRules, setEditProtocolRules] = useState<OpenCodeProtocolRule[]>([]);
  const [editCustomHeadersText, setEditCustomHeadersText] = useState("");
  const [editGroupIDs, setEditGroupIDs] = useState<number[]>([]);
  const [editAllowedKeyIDs, setEditAllowedKeyIDs] = useState<number[]>([]);

  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null);
  const [detailAccount, setDetailAccount] = useState<AccountRow | null>(null);
  const [quickProxyAccount, setQuickProxyAccount] = useState<AccountRow | null>(null);
  const [showGroupManager, setShowGroupManager] = useState(false);
  const [showBatchGroupModal, setShowBatchGroupModal] = useState(false);

  const openCodeGroups = useMemo(
    () => groups.filter((g) => (g.channel || "codex") === "opencode"),
    [groups],
  );
  const groupMap = useMemo(() => new Map(openCodeGroups.map((g) => [g.id, g])), [openCodeGroups]);
  const proxyBindingContext = useMemo(
    () => buildProxyBindingContext({ proxies, groups }),
    [proxies, groups],
  );

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setSearch(searchInput.trim());
      setPage(1);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [searchInput]);

  const loadAux = useCallback(async () => {
    try {
      const [gRes, pRes, kRes] = await Promise.all([
        api.listAccountGroups().catch(() => ({ groups: [] as AccountGroup[] })),
        api.listProxies().catch(() => ({ proxies: [] as ProxyRow[] })),
        api.getAPIKeys().catch(() => ({ keys: [] as APIKeyRow[] })),
      ]);
      setGroups(gRes.groups || []);
      setProxies(pRes.proxies || []);
      setApiKeys(kRes.keys || []);
    } catch {
      // ignore aux errors
    }
  }, []);

  const loadAccounts = useCallback(async () => {
    setLoading(true);
    try {
      const params: AccountsPageParams = {
        channel: "opencode",
        page,
        pageSize,
      };
      if (search) params.search = search;
      if (statusFilter !== "all") params.status = statusFilter;
      if (modeFilter !== "all") params.authKind = modeFilter;
      if (!isAccountGroupFilterEmpty(groupFilter)) {
        if (groupFilter.ungrouped) params.ungrouped = true;
        else if (groupFilter.include.length > 0) params.groupInclude = groupFilter.include;
        if (groupFilter.exclude.length > 0) params.groupExclude = groupFilter.exclude;
      }
      const res = await api.getAccountsPage(params);
      setAccounts(res.accounts || []);
      setTotal(res.total || 0);
      setSummary(res.summary || null);
      setSelectedIds(new Set());
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setLoading(false);
    }
  }, [page, pageSize, search, statusFilter, modeFilter, groupFilter, showToast]);

  useEffect(() => {
    void loadAux();
  }, [loadAux]);

  useEffect(() => {
    void loadAccounts();
  }, [loadAccounts]);

  useEffect(() => {
    setGroupFilter((prev) => pruneAccountGroupFilter(prev, openCodeGroups));
  }, [openCodeGroups]);

  const resetAddForm = () => {
    setAddName("");
    setAddEmail("");
    setAddAPIKey("");
    setAddMode("go");
    setAddBaseURL("");
    setAddAnthropicBaseURL("");
    setAddProxyURL("");
    setAddModels([]);
    setAddModelMapping("");
    setAddProtocolRules([]);
    setAddCustomHeadersText("");
    setAddAutoSync(true);
    setShowAddAPIKey(false);
    setShowAddAdvanced(false);
    setImportContent("");
    setImportMode("go");
    setImportBaseURL("");
    setImportProxyURL("");
    setImportAutoSync(true);
  };

  const handleAddSingle = async () => {
    if (!addAPIKey.trim()) {
      showToast("请输入 OpenCode API Key", "warning");
      return;
    }
    const { headers, error: headerErr } = parseHeadersText(addCustomHeadersText);
    if (headerErr) {
      showToast(headerErr, "error");
      return;
    }
    setAddSubmitting(true);
    try {
      const cleanedRules = addProtocolRules.filter((r) => r.pattern.trim() !== "");
      const res = await api.addOpenCodeAccount({
        name: addName.trim() || undefined,
        email: addEmail.trim() || undefined,
        api_key: addAPIKey.trim(),
        mode: addMode,
        base_url: addBaseURL.trim() || undefined,
        anthropic_base_url: addAnthropicBaseURL.trim() || undefined,
        proxy_url: addProxyURL.trim() || undefined,
        models: addModels.length > 0 ? addModels : undefined,
        model_mapping: addModelMapping.trim() || undefined,
        protocol_rules: cleanedRules.length > 0 ? cleanedRules : undefined,
        custom_headers: Object.keys(headers).length > 0 ? headers : undefined,
        auto_sync: addAutoSync,
      });
      const warn = res.warnings && res.warnings.length > 0 ? `（提示: ${res.warnings.join("; ")}）` : "";
      showToast(`已添加 OpenCode 账号 ${res.name}${res.models > 0 ? `，同步 ${res.models} 个模型` : ""}${warn}`, "success");
      setShowAddModal(false);
      resetAddForm();
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setAddSubmitting(false);
    }
  };

  const handleBatchImport = async () => {
    if (!importContent.trim()) {
      showToast("请粘贴 API Key 列表或 JSON 导入内容", "warning");
      return;
    }
    setAddSubmitting(true);
    try {
      const res = await api.importOpenCodeAccounts({
        content: importContent,
        mode: importMode,
        base_url: importBaseURL.trim() || undefined,
        proxy_url: importProxyURL.trim() || undefined,
        auto_sync: importAutoSync,
      });
      showToast(
        `导入完成：成功 ${res.imported}，重复 ${res.duplicates}，失败 ${res.failed}`,
        res.failed > 0 && res.imported === 0 ? "error" : "success",
      );
      if (res.imported > 0) {
        setShowAddModal(false);
        resetAddForm();
        void loadAccounts();
      }
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setAddSubmitting(false);
    }
  };

  const handleImportFileChange = async (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    try {
      const text = await file.text();
      setImportContent(text);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      e.target.value = "";
    }
  };

  const openEditModal = async (account: AccountRow) => {
    setEditingAccount(account);
    setEditLoading(true);
    setEditAPIKey("");
    setShowEditAPIKey(false);
    try {
      const detail = await api.getAccount(account.id);
      const src = detail || account;
      setEditName(src.name || "");
      setEditEmail(src.email || "");
      setEditMode(src.opencode_mode === "zen" ? "zen" : "go");
      setEditBaseURL(src.base_url || "");
      setEditAnthropicBaseURL(src.opencode_anthropic_base_url || "");
      setEditProxyURL(src.proxy_url || "");
      setEditModels(src.models || []);
      setEditModelMapping(src.model_mapping || "");
      setEditProtocolRules(src.opencode_protocol_rules || []);
      setEditCustomHeadersText(
        src.custom_headers && Object.keys(src.custom_headers).length > 0
          ? JSON.stringify(src.custom_headers, null, 2)
          : "",
      );
      setEditGroupIDs(src.group_ids || []);
      setEditAllowedKeyIDs(src.allowed_api_key_ids || []);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
      setEditingAccount(null);
    } finally {
      setEditLoading(false);
    }
  };

  const handleSaveEdit = async () => {
    if (!editingAccount) return;
    const { headers, error: headerErr } = parseHeadersText(editCustomHeadersText);
    if (headerErr) {
      showToast(headerErr, "error");
      return;
    }
    setEditSaving(true);
    try {
      const cleanedRules = editProtocolRules.filter((r) => r.pattern.trim() !== "");
      await api.updateOpenCodeAccount(editingAccount.id, {
        name: editName.trim() || undefined,
        email: editEmail.trim(),
        api_key: editAPIKey.trim() || undefined,
        mode: editMode,
        base_url: editBaseURL.trim(),
        anthropic_base_url: editAnthropicBaseURL.trim(),
        proxy_url: editProxyURL.trim(),
        models: editModels,
        model_mapping: editModelMapping.trim(),
        protocol_rules: cleanedRules,
        clear_protocol_rules: cleanedRules.length === 0,
        custom_headers: headers,
      });
      await api.updateAccountScheduler(editingAccount.id, {
        group_ids: editGroupIDs,
        allowed_api_key_ids: editAllowedKeyIDs,
      });
      showToast("已保存 OpenCode 账号配置", "success");
      setEditingAccount(null);
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setEditSaving(false);
    }
  };

  const handleSyncOne = async (account: AccountRow) => {
    setSyncingId(account.id);
    try {
      const res = await api.syncOpenCodeAccount(account.id);
      const warn = res.warnings && res.warnings.length > 0 ? `（${res.warnings.join("; ")}）` : "";
      showToast(`同步完成：${res.models?.length ?? 0} 个模型${warn}`, "success");
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSyncingId(null);
    }
  };

  const handleSyncAll = async () => {
    setSyncingAll(true);
    try {
      const res = await api.syncAllOpenCodeAccounts();
      showToast(`批量同步完成：成功 ${res.succeeded}，失败 ${res.failed}`, res.failed > 0 && res.succeeded === 0 ? "error" : "success");
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSyncingAll(false);
    }
  };

  const handleExport = async (ids?: number[]) => {
    setExporting(true);
    try {
      const blob = await api.exportOpenCodeAccounts(ids);
      downloadNamedBlob(blob, "opencode-accounts.json");
      showToast("已导出 OpenCode 账号", "success");
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setExporting(false);
    }
  };

  const handleToggleEnabled = async (account: AccountRow) => {
    try {
      await api.toggleAccountEnabled(account.id, !account.enabled);
      showToast(account.enabled ? "已暂停调度" : "已恢复调度", "success");
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    }
  };

  const handleDeleteOne = async (account: AccountRow) => {
    const ok = await confirm({
      title: "删除 OpenCode 账号",
      description: `确定删除账号「${accountLabel(account)}」吗？`,
      confirmText: "删除",
      confirmVariant: "destructive",
    });
    if (!ok) return;
    try {
      await api.deleteAccount(account.id);
      showToast("已删除账号", "success");
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    }
  };

  const handleBatchDelete = async () => {
    const ids = Array.from(selectedIds);
    if (ids.length === 0) return;
    const ok = await confirm({
      title: "批量删除 OpenCode 账号",
      description: `确定删除已选中的 ${ids.length} 个 OpenCode 账号吗？`,
      confirmText: "删除",
      confirmVariant: "destructive",
    });
    if (!ok) return;
    try {
      await api.batchDeleteAccounts(ids);
      showToast(`已删除 ${ids.length} 个账号`, "success");
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    }
  };

  const handleBatchEnabled = async (enabled: boolean) => {
    const ids = Array.from(selectedIds);
    if (ids.length === 0) return;
    try {
      await api.batchUpdateAccounts({ ids, enabled });
      showToast(enabled ? `已启用 ${ids.length} 个账号` : `已暂停 ${ids.length} 个账号`, "success");
      void loadAccounts();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    }
  };

  const allSelected = accounts.length > 0 && accounts.every((a) => selectedIds.has(a.id));
  const toggleSelectAll = () => {
    if (allSelected) {
      setSelectedIds(new Set());
    } else {
      setSelectedIds(new Set(accounts.map((a) => a.id)));
    }
  };

  const goCount = summary?.opencode_modes?.go ?? 0;
  const zenCount = summary?.opencode_modes?.zen ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / Math.max(1, pageSize)));

  const addAdvancedCount = useMemo(() => {
    let count = 0;
    if (addBaseURL.trim() || addAnthropicBaseURL.trim()) count += 1;
    if (addModels.length > 0) count += 1;
    if (addProtocolRules.some((r) => r.pattern.trim() !== "")) count += 1;
    if (addCustomHeadersText.trim()) count += 1;
    return count;
  }, [addBaseURL, addAnthropicBaseURL, addModels, addProtocolRules, addCustomHeadersText]);

  const addHeaderError = useMemo(
    () => (addCustomHeadersText.trim() ? parseHeadersText(addCustomHeadersText).error : null),
    [addCustomHeadersText],
  );
  const editHeaderError = useMemo(
    () => (editCustomHeadersText.trim() ? parseHeadersText(editCustomHeadersText).error : null),
    [editCustomHeadersText],
  );
  const importSummary = useMemo(() => detectImportSummary(importContent), [importContent]);

  return (
    <div className="space-y-4">
      <PageHeader
        title="OpenCode 账号池"
        description="支持 OpenCode Go（订阅三窗口额度）与 Zen（按量），同一 API Key 自动按模型分流 Responses / Chat / Anthropic 三协议"
        titleAdornment={headerSlot}
        hideTitle={Boolean(headerSlot)}
        actionsBelow={Boolean(headerSlot)}
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" onClick={() => setShowGroupManager(true)}>
              <Sliders className="mr-1.5 size-3.5" /> 分组管理
            </Button>
            <Button variant="outline" size="sm" disabled={syncingAll} onClick={() => void handleSyncAll()}>
              {syncingAll ? <Loader2 className="mr-1.5 size-3.5 animate-spin" /> : <RefreshCw className="mr-1.5 size-3.5" />}
              同步全部额度/模型
            </Button>
            <Button variant="outline" size="sm" disabled={exporting} onClick={() => void handleExport()}>
              <Download className="mr-1.5 size-3.5" /> 导出
            </Button>
            <Button
              size="sm"
              onClick={() => {
                setAddTab("single");
                setShowAddModal(true);
              }}
            >
              <Plus className="mr-1.5 size-3.5" /> 添加账号
            </Button>
          </div>
        }
      />

      {summary && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
          <CompactStat
            label="总账号"
            value={summary.total}
            tone="neutral"
            active={statusFilter === "all" && modeFilter === "all"}
            onClick={() => {
              setStatusFilter("all");
              setModeFilter("all");
              setPage(1);
            }}
          />
          <CompactStat
            label="可用"
            value={summary.normal}
            tone="success"
            active={statusFilter === "normal"}
            onClick={() => {
              setStatusFilter((prev) => (prev === "normal" ? "all" : "normal"));
              setPage(1);
            }}
          />
          <CompactStat
            label="限流"
            value={summary.rate_limited}
            tone="warning"
            active={statusFilter === "rate_limited"}
            onClick={() => {
              setStatusFilter((prev) => (prev === "rate_limited" ? "all" : "rate_limited"));
              setPage(1);
            }}
          />
          <CompactStat
            label="异常"
            value={summary.abnormal}
            tone="danger"
            active={statusFilter === "abnormal"}
            onClick={() => {
              setStatusFilter((prev) => (prev === "abnormal" ? "all" : "abnormal"));
              setPage(1);
            }}
          />
          <CompactStat
            label="Go 订阅"
            value={goCount}
            tone="neutral"
            active={modeFilter === "go"}
            onClick={() => {
              setModeFilter((prev) => (prev === "go" ? "all" : "go"));
              setPage(1);
            }}
          />
          <CompactStat
            label="Zen 按量"
            value={zenCount}
            tone="neutral"
            active={modeFilter === "zen"}
            onClick={() => {
              setModeFilter((prev) => (prev === "zen" ? "all" : "zen"));
              setPage(1);
            }}
          />
        </div>
      )}

      <Card>
        <CardContent className="space-y-3 p-4">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative flex-1 min-w-[200px] max-w-sm">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={searchInput}
                onChange={(e) => setSearchInput(e.target.value)}
                placeholder="搜索名称 / 邮箱 / 备注…"
                className="h-8 pl-8 text-xs"
              />
            </div>

            <FilterChipBar
              value={modeFilter}
              onChange={(next) => {
                setModeFilter(next);
                setPage(1);
              }}
              options={[
                { value: "all", label: "全部", count: summary?.total ?? total },
                { value: "go", label: "Go 订阅", count: goCount, dotClassName: "bg-emerald-500" },
                { value: "zen", label: "Zen 按量", count: zenCount, dotClassName: "bg-sky-500" },
              ]}
            />

            <Select
              value={statusFilter}
              onValueChange={(value) => {
                setStatusFilter(value);
                setPage(1);
              }}
              options={STATUS_OPTIONS}
              compact
              className="w-32"
            />

            <AccountGroupFilterSelect
              groups={openCodeGroups}
              value={groupFilter}
              onChange={(next) => {
                setGroupFilter(next);
                setPage(1);
              }}
            />

            <Button variant="ghost" size="sm" className="h-8 px-2" onClick={() => void loadAccounts()} title={t("common.refresh")}>
              <RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
            </Button>
          </div>

          {selectedIds.size > 0 && (
            <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-primary/20 bg-primary/5 px-3 py-2 text-xs">
              <span className="font-medium">已选中 {selectedIds.size} 个账号</span>
              <div className="flex flex-wrap items-center gap-1.5">
                <Button size="sm" variant="outline" className="h-7 text-xs" onClick={() => void handleBatchEnabled(true)}>
                  <Power className="mr-1 size-3" /> 启用
                </Button>
                <Button size="sm" variant="outline" className="h-7 text-xs" onClick={() => void handleBatchEnabled(false)}>
                  <PowerOff className="mr-1 size-3" /> 暂停
                </Button>
                <Button size="sm" variant="outline" className="h-7 text-xs" onClick={() => setShowBatchGroupModal(true)}>
                  <Sliders className="mr-1 size-3" /> 分配分组
                </Button>
                <Button size="sm" variant="outline" className="h-7 text-xs" onClick={() => void handleExport(Array.from(selectedIds))}>
                  <Download className="mr-1 size-3" /> 导出选中
                </Button>
                <Button size="sm" variant="destructive" className="h-7 text-xs" onClick={() => void handleBatchDelete()}>
                  <Trash2 className="mr-1 size-3" /> 删除
                </Button>
              </div>
            </div>
          )}

          <StateShell
            loading={loading && accounts.length === 0}
            isEmpty={!loading && accounts.length === 0}
            emptyIcon={<ChannelLogo channel="opencode" size={28} />}
            emptyTitle="暂无 OpenCode 账号"
            emptyDescription="添加 OpenCode Go（订阅）或 Zen（按量）API Key，支持从 Sub2API 导出 JSON 一键导入。"
            action={
              <Button
                size="sm"
                onClick={() => {
                  setAddTab("single");
                  setShowAddModal(true);
                }}
              >
                <Plus className="mr-1.5 size-3.5" /> 添加 OpenCode 账号
              </Button>
            }
          >
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-9">
                      <input type="checkbox" checked={allSelected} onChange={toggleSelectAll} aria-label="全选" />
                    </TableHead>
                    <TableHead>账号 / 模式</TableHead>
                    <TableHead>状态</TableHead>
                    <TableHead>额度窗口 (Go)</TableHead>
                    <TableHead>模型 / 路由</TableHead>
                    <TableHead>代理</TableHead>
                    <TableHead className="text-right">操作</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {accounts.map((acc) => {
                    const checked = selectedIds.has(acc.id);
                    const mode: OpenCodeMode = acc.opencode_mode === "zen" ? "zen" : "go";
                    const explicitModels = acc.models?.length ?? 0;
                    const catalogModels = acc.opencode_catalog_models?.length ?? 0;
                    const customRuleCount = acc.opencode_protocol_rules?.length ?? 0;
                    return (
                      <TableRow key={acc.id} className={cn("relative", accountStateTableRowClass(acc))}>
                        <TableCell>
                          <input
                            type="checkbox"
                            checked={checked}
                            onChange={() => {
                              setSelectedIds((prev) => {
                                const next = new Set(prev);
                                if (next.has(acc.id)) next.delete(acc.id);
                                else next.add(acc.id);
                                return next;
                              });
                            }}
                          />
                        </TableCell>
                        <TableCell>
                          <div className="flex items-start gap-2">
                            <ChannelLogo channel="opencode" size={16} className="mt-0.5 shrink-0" />
                            <div className="min-w-0">
                              <div className="flex flex-wrap items-center gap-1.5">
                                <button
                                  type="button"
                                  onClick={() => setDetailAccount(acc)}
                                  className="truncate font-medium text-foreground hover:underline"
                                >
                                  {accountLabel(acc)}
                                </button>
                                <span
                                  className={cn(
                                    "inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase",
                                    mode === "go"
                                      ? "bg-emerald-500/15 text-emerald-700 dark:text-emerald-300"
                                      : "bg-sky-500/15 text-sky-700 dark:text-sky-300",
                                  )}
                                >
                                  {mode === "go" ? "Go 订阅" : "Zen 按量"}
                                </span>
                              </div>
                              {acc.base_url ? (
                                <div className="truncate font-mono text-[11px] text-muted-foreground" title={acc.base_url}>
                                  {acc.base_url}
                                </div>
                              ) : null}
                            </div>
                          </div>
                        </TableCell>
                        <TableCell>
                          <StatusBadge status={getAccountStatusBadgeStatus(acc)} />
                        </TableCell>
                        <TableCell>
                          <OpenCodeUsageCell account={acc} />
                        </TableCell>
                        <TableCell>
                          <div className="space-y-0.5 text-xs">
                            <div className="text-foreground/90">
                              {explicitModels > 0
                                ? `白名单 ${explicitModels} 个`
                                : catalogModels > 0
                                  ? `目录 ${catalogModels} 个`
                                  : "默认目录"}
                            </div>
                            <div className="text-[11px] text-muted-foreground">
                              {customRuleCount > 0 ? `自定义规则 ${customRuleCount} 条` : `内置 ${mode === "zen" ? "Zen" : "Go"} 协议分流`}
                            </div>
                          </div>
                        </TableCell>
                        <TableCell>
                          <AccountProxyBadge
                            account={acc}
                            ctx={proxyBindingContext}
                            onClick={() => setQuickProxyAccount(acc)}
                          />
                        </TableCell>
                        <TableCell className="text-right">
                          <div className="inline-flex items-center justify-end gap-1">
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7"
                              disabled={syncingId === acc.id}
                              title="同步模型与额度"
                              onClick={() => void handleSyncOne(acc)}
                            >
                              <RefreshCw className={cn("size-3.5", syncingId === acc.id && "animate-spin")} />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7"
                              title="测试连通性"
                              onClick={() => setTestingAccount(acc)}
                            >
                              <Zap className="size-3.5" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7"
                              title="编辑配置"
                              onClick={() => void openEditModal(acc)}
                            >
                              <Pencil className="size-3.5" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7"
                              title={acc.enabled ? "暂停调度" : "恢复调度"}
                              onClick={() => void handleToggleEnabled(acc)}
                            >
                              {acc.enabled ? <PowerOff className="size-3.5 text-amber-600" /> : <Power className="size-3.5 text-emerald-600" />}
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7 text-muted-foreground hover:text-destructive"
                              title="删除"
                              onClick={() => void handleDeleteOne(acc)}
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
          </StateShell>

          {total > 0 && (
            <Pagination
              page={page}
              totalPages={totalPages}
              totalItems={total}
              pageSize={pageSize}
              pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
              onPageChange={setPage}
              onPageSizeChange={(size) => {
                setPageSize(size);
                setPage(1);
              }}
            />
          )}
        </CardContent>
      </Card>

      {/* 添加 / 批量导入弹窗 */}
      <Modal
        show={showAddModal}
        onClose={() => {
          if (!addSubmitting) setShowAddModal(false);
        }}
        title={
          <span className="inline-flex items-center gap-2.5">
            <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-lg border border-border/80 bg-muted/50 shadow-2xs">
              <ChannelLogo channel="opencode" size={16} />
            </span>
            <span>添加 OpenCode 账号</span>
          </span>
        }
        contentClassName="sm:max-w-[680px]"
        bodyClassName="space-y-4 py-4"
        footer={
          <div className="flex w-full flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <label className="flex cursor-pointer items-center gap-2.5 select-none">
              <Switch
                checked={addTab === "single" ? addAutoSync : importAutoSync}
                onCheckedChange={(checked) => {
                  if (addTab === "single") setAddAutoSync(checked);
                  else setImportAutoSync(checked);
                }}
                disabled={addSubmitting}
              />
              <span className="text-xs text-muted-foreground">
                {addTab === "single"
                  ? `添加时立即同步模型${addMode === "go" ? "与 Go 额度" : ""}`
                  : "导入时立即同步模型与 Go 额度"}
              </span>
            </label>
            <div className="flex items-center justify-end gap-2">
              <Button variant="outline" size="sm" disabled={addSubmitting} onClick={() => setShowAddModal(false)}>
                取消
              </Button>
              {addTab === "single" ? (
                <Button size="sm" disabled={addSubmitting} onClick={() => void handleAddSingle()}>
                  {addSubmitting ? <Loader2 className="mr-1.5 size-3.5 animate-spin" /> : null}
                  保存并添加
                </Button>
              ) : (
                <Button size="sm" disabled={addSubmitting} onClick={() => void handleBatchImport()}>
                  {addSubmitting ? <Loader2 className="mr-1.5 size-3.5 animate-spin" /> : null}
                  开始导入
                </Button>
              )}
            </div>
          </div>
        }
      >
        <SegmentedTabs
          size="sm"
          className="w-full"
          value={addTab}
          onValueChange={(value) => setAddTab(value as "single" | "import")}
          tabs={[
            {
              value: "single",
              label: (
                <>
                  <KeyRound className="size-3.5" />
                  <span>单个添加</span>
                </>
              ),
            },
            {
              value: "import",
              label: (
                <>
                  <Upload className="size-3.5" />
                  <span>批量导入 / Sub2API JSON</span>
                </>
              ),
            },
          ]}
        />

        {addTab === "single" ? (
          <div className="space-y-4">
            {/* 基础凭据 */}
            <section className="space-y-3.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <div>
                <FieldLabel>接入模式</FieldLabel>
                <OpenCodeModeCards value={addMode} onChange={setAddMode} disabled={addSubmitting} />
              </div>

              <div>
                <FieldLabel required hint="支持 OpenCode Go / Zen Key">
                  API Key
                </FieldLabel>
                <div className="relative">
                  <Input
                    type={showAddAPIKey ? "text" : "password"}
                    value={addAPIKey}
                    onChange={(e) => setAddAPIKey(e.target.value)}
                    placeholder="sk-..."
                    autoComplete="off"
                    className="h-9 pr-9 font-mono text-xs"
                  />
                  <button
                    type="button"
                    tabIndex={-1}
                    onClick={() => setShowAddAPIKey((prev) => !prev)}
                    aria-label={showAddAPIKey ? "隐藏 API Key" : "显示 API Key"}
                    className="absolute inset-y-0 right-0 flex w-9 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                  >
                    {showAddAPIKey ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
                  </button>
                </div>
              </div>

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <FieldLabel optional>名称</FieldLabel>
                  <Input
                    value={addName}
                    onChange={(e) => setAddName(e.target.value)}
                    placeholder="默认按模式与 Key 后缀生成"
                    className="h-9 text-xs"
                  />
                </div>
                <div>
                  <FieldLabel optional>备注邮箱</FieldLabel>
                  <Input
                    value={addEmail}
                    onChange={(e) => setAddEmail(e.target.value)}
                    placeholder="user@example.com"
                    className="h-9 text-xs"
                  />
                </div>
              </div>
            </section>

            {/* 网络代理 */}
            <section className="space-y-2.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-2 text-xs font-semibold text-foreground">
                  <Globe className="size-3.5 text-muted-foreground" />
                  <span>网络代理</span>
                  <span className="font-normal text-muted-foreground/80">可选</span>
                </div>
                <span className="text-[11px] text-muted-foreground">可手填测试或直接从代理池绑定</span>
              </div>
              <ProxyField value={addProxyURL} onChange={setAddProxyURL} proxies={proxies} label="" />
            </section>

            {/* 高级配置（可折叠） */}
            <section className="overflow-hidden rounded-xl border border-border/75 bg-card shadow-2xs">
              <button
                type="button"
                onClick={() => setShowAddAdvanced((prev) => !prev)}
                aria-expanded={showAddAdvanced}
                className="flex w-full items-center justify-between gap-3 px-4 py-3 text-left transition-colors hover:bg-muted/30"
              >
                <div className="flex min-w-0 items-center gap-2.5">
                  <SlidersHorizontal className="size-3.5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-xs font-semibold text-foreground">高级配置</span>
                      {addAdvancedCount > 0 ? (
                        <span className="inline-flex items-center rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-medium text-primary">
                          已自定义 {addAdvancedCount} 项
                        </span>
                      ) : (
                        <span className="text-[11px] text-muted-foreground">
                          Base URL、模型白名单、协议路由、自定义请求头
                        </span>
                      )}
                    </div>
                  </div>
                </div>
                <ChevronDown
                  className={cn(
                    "size-4 shrink-0 text-muted-foreground transition-transform duration-200",
                    showAddAdvanced && "rotate-180",
                  )}
                />
              </button>

              {showAddAdvanced && (
                <div className="space-y-4 border-t border-border/70 bg-muted/10 px-4 py-4">
                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                    <div>
                      <FieldLabel optional>自定义 Base URL</FieldLabel>
                      <Input
                        value={addBaseURL}
                        onChange={(e) => setAddBaseURL(e.target.value)}
                        placeholder={addMode === "zen" ? DEFAULT_ZEN_BASE_URL : DEFAULT_GO_BASE_URL}
                        className="h-9 font-mono text-xs"
                      />
                    </div>
                    <div>
                      <FieldLabel optional>Anthropic Base URL</FieldLabel>
                      <Input
                        value={addAnthropicBaseURL}
                        onChange={(e) => setAddAnthropicBaseURL(e.target.value)}
                        placeholder={addMode === "zen" ? DEFAULT_ZEN_ANTHROPIC_URL : DEFAULT_GO_ANTHROPIC_URL}
                        className="h-9 font-mono text-xs"
                      />
                    </div>
                  </div>

                  <div>
                    <FieldLabel optional hint="留空则使用同步目录/默认目录">
                      模型白名单
                    </FieldLabel>
                    <ChipInput value={addModels} onChange={setAddModels} placeholder="输入模型名后回车，例如 glm-5.3" />
                  </div>

                  <ProtocolRulesEditor mode={addMode} rules={addProtocolRules} onChange={setAddProtocolRules} />

                  <div>
                    <FieldLabel optional hint="JSON 对象，支持覆盖 User-Agent">
                      自定义请求头
                    </FieldLabel>
                    <Input
                      value={addCustomHeadersText}
                      onChange={(e) => setAddCustomHeadersText(e.target.value)}
                      placeholder='{"User-Agent": "opencode/1.19.6"}'
                      aria-invalid={Boolean(addHeaderError)}
                      className="h-9 font-mono text-xs"
                    />
                    {addHeaderError ? (
                      <p className="mt-1 text-[11px] text-destructive">{addHeaderError}</p>
                    ) : null}
                  </div>
                </div>
              )}
            </section>
          </div>
        ) : (
          <div className="space-y-4">
            {/* 默认接入模式 */}
            <section className="space-y-2.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <FieldLabel hint="JSON 条目未指定模式时生效">默认接入模式</FieldLabel>
              <OpenCodeModeCards value={importMode} onChange={setImportMode} disabled={addSubmitting} />
            </section>

            {/* 导入内容 */}
            <section className="space-y-2.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <label className="text-xs font-semibold text-foreground">导入内容</label>
                  {importSummary ? (
                    <span className="inline-flex items-center rounded-md bg-muted px-2 py-0.5 font-mono text-[10px] font-medium text-muted-foreground">
                      {importSummary}
                    </span>
                  ) : null}
                </div>
                <div className="flex items-center gap-1.5">
                  <input
                    ref={importFileRef}
                    type="file"
                    accept=".json,.txt"
                    className="hidden"
                    onChange={(e) => void handleImportFileChange(e)}
                  />
                  {importContent.trim() ? (
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-7 px-2 text-[11px] text-muted-foreground hover:text-foreground"
                      onClick={() => setImportContent("")}
                    >
                      清空
                    </Button>
                  ) : null}
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="h-7 px-2.5 text-xs"
                    onClick={() => importFileRef.current?.click()}
                  >
                    <FolderOpen className="mr-1.5 size-3.5" /> 选择文件
                  </Button>
                </div>
              </div>

              <textarea
                value={importContent}
                onChange={(e) => setImportContent(e.target.value)}
                className={TEXTAREA_CLASS}
                placeholder={`粘贴 API Key 列表或导出的 JSON 内容…\n\n示例：\nsk-xxxxxxxx\n账号A,sk-yyyyyyyy`}
              />

              <div className="flex flex-wrap items-center gap-1.5 text-[11px] text-muted-foreground">
                <span className="text-muted-foreground/80">支持格式：</span>
                <span className="rounded-md border border-border/60 bg-muted/40 px-2 py-0.5">每行 Key 或 名称,Key</span>
                <span className="rounded-md border border-border/60 bg-muted/40 px-2 py-0.5">OpenCode 导出 JSON</span>
                <span className="rounded-md border border-border/60 bg-muted/40 px-2 py-0.5">
                  Sub2API JSON (<code className="font-mono text-[10px]">opencode_go</code>)
                </span>
              </div>
            </section>

            {/* 默认代理与端点 */}
            <section className="space-y-3.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-2 text-xs font-semibold text-foreground">
                  <Globe className="size-3.5 text-muted-foreground" />
                  <span>默认代理与端点</span>
                  <span className="font-normal text-muted-foreground/80">可选</span>
                </div>
                <span className="text-[11px] text-muted-foreground">导入项未单独指定时回落</span>
              </div>
              <ProxyField value={importProxyURL} onChange={setImportProxyURL} proxies={proxies} label="" />
              <div>
                <FieldLabel optional>默认 Base URL</FieldLabel>
                <Input
                  value={importBaseURL}
                  onChange={(e) => setImportBaseURL(e.target.value)}
                  placeholder={importMode === "zen" ? DEFAULT_ZEN_BASE_URL : DEFAULT_GO_BASE_URL}
                  className="h-9 font-mono text-xs"
                />
              </div>
            </section>
          </div>
        )}
      </Modal>

      {/* 编辑弹窗 */}
      <Modal
        show={Boolean(editingAccount)}
        onClose={() => {
          if (!editSaving) setEditingAccount(null);
        }}
        title={
          <span className="inline-flex items-center gap-2.5">
            <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-lg border border-border/80 bg-muted/50 shadow-2xs">
              <ChannelLogo channel="opencode" size={16} />
            </span>
            <span className="truncate">
              {editingAccount ? `编辑 OpenCode 账号：${accountLabel(editingAccount)}` : "编辑账号"}
            </span>
          </span>
        }
        contentClassName="sm:max-w-[680px]"
        bodyClassName="space-y-4 py-4"
        footer={
          !editLoading ? (
            <div className="flex w-full justify-end gap-2">
              <Button variant="outline" size="sm" disabled={editSaving} onClick={() => setEditingAccount(null)}>
                取消
              </Button>
              <Button size="sm" disabled={editSaving} onClick={() => void handleSaveEdit()}>
                {editSaving ? <Loader2 className="mr-1.5 size-3.5 animate-spin" /> : null}
                保存修改
              </Button>
            </div>
          ) : undefined
        }
      >
        {editLoading ? (
          <div className="flex items-center justify-center py-12 text-sm text-muted-foreground">
            <Loader2 className="mr-2 size-4 animate-spin" /> 正在加载配置…
          </div>
        ) : (
          <div className="space-y-4">
            <section className="space-y-3.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <div>
                <FieldLabel>接入模式</FieldLabel>
                <OpenCodeModeCards value={editMode} onChange={setEditMode} disabled={editSaving} />
              </div>

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <FieldLabel>名称</FieldLabel>
                  <Input value={editName} onChange={(e) => setEditName(e.target.value)} className="h-9 text-xs" />
                </div>
                <div>
                  <FieldLabel optional>备注邮箱</FieldLabel>
                  <Input value={editEmail} onChange={(e) => setEditEmail(e.target.value)} className="h-9 text-xs" />
                </div>
              </div>

              <div>
                <FieldLabel optional hint="留空保持当前 Key 不变">
                  更换 API Key
                </FieldLabel>
                <div className="relative">
                  <Input
                    type={showEditAPIKey ? "text" : "password"}
                    value={editAPIKey}
                    onChange={(e) => setEditAPIKey(e.target.value)}
                    placeholder="留空不修改"
                    autoComplete="off"
                    className="h-9 pr-9 font-mono text-xs"
                  />
                  <button
                    type="button"
                    tabIndex={-1}
                    onClick={() => setShowEditAPIKey((prev) => !prev)}
                    aria-label={showEditAPIKey ? "隐藏 API Key" : "显示 API Key"}
                    className="absolute inset-y-0 right-0 flex w-9 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                  >
                    {showEditAPIKey ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
                  </button>
                </div>
              </div>
            </section>

            <section className="space-y-3.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <div className="flex items-center gap-2 text-xs font-semibold text-foreground">
                <Globe className="size-3.5 text-muted-foreground" />
                <span>端点与代理</span>
              </div>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <FieldLabel optional hint="留空使用官方默认">
                    Base URL
                  </FieldLabel>
                  <Input
                    value={editBaseURL}
                    onChange={(e) => setEditBaseURL(e.target.value)}
                    placeholder={editMode === "zen" ? DEFAULT_ZEN_BASE_URL : DEFAULT_GO_BASE_URL}
                    className="h-9 font-mono text-xs"
                  />
                </div>
                <div>
                  <FieldLabel optional>Anthropic Base URL</FieldLabel>
                  <Input
                    value={editAnthropicBaseURL}
                    onChange={(e) => setEditAnthropicBaseURL(e.target.value)}
                    placeholder={editMode === "zen" ? DEFAULT_ZEN_ANTHROPIC_URL : DEFAULT_GO_ANTHROPIC_URL}
                    className="h-9 font-mono text-xs"
                  />
                </div>
              </div>
              <ProxyField value={editProxyURL} onChange={setEditProxyURL} proxies={proxies} label="" />
            </section>

            <section className="space-y-3.5 rounded-xl border border-border/75 bg-card p-4 shadow-2xs">
              <div className="flex items-center gap-2 text-xs font-semibold text-foreground">
                <SlidersHorizontal className="size-3.5 text-muted-foreground" />
                <span>模型、路由与请求头</span>
              </div>

              <div>
                <FieldLabel optional hint="留空则按同步目录放行">
                  模型白名单
                </FieldLabel>
                <ChipInput value={editModels} onChange={setEditModels} placeholder="输入模型名后回车" />
              </div>

              <div>
                <FieldLabel optional hint="JSON 对象">
                  账号级模型映射
                </FieldLabel>
                <Input
                  value={editModelMapping}
                  onChange={(e) => setEditModelMapping(e.target.value)}
                  placeholder='{"gpt-4o": "glm-5.3"}'
                  className="h-9 font-mono text-xs"
                />
              </div>

              <ProtocolRulesEditor mode={editMode} rules={editProtocolRules} onChange={setEditProtocolRules} />

              <div>
                <FieldLabel optional hint="JSON 对象">
                  自定义请求头
                </FieldLabel>
                <textarea
                  value={editCustomHeadersText}
                  onChange={(e) => setEditCustomHeadersText(e.target.value)}
                  aria-invalid={Boolean(editHeaderError)}
                  className="flex min-h-[76px] w-full resize-y rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs leading-relaxed shadow-2xs transition-colors focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30"
                  placeholder='{"User-Agent": "opencode/1.19.6"}'
                />
                {editHeaderError ? <p className="mt-1 text-[11px] text-destructive">{editHeaderError}</p> : null}
              </div>
            </section>

            <section className="grid grid-cols-1 gap-3 rounded-xl border border-border/75 bg-card p-4 shadow-2xs sm:grid-cols-2">
              <div>
                <FieldLabel>所属分组</FieldLabel>
                <AccountGroupMultiSelect
                  groups={openCodeGroups}
                  value={editGroupIDs}
                  onChange={setEditGroupIDs}
                  placeholder="选择分组"
                  emptyLabel="暂无 OpenCode 分组"
                  selectedLabel={`已选 ${editGroupIDs.length} 个分组`}
                />
              </div>
              <div>
                <FieldLabel optional hint="留空不限">
                  限定 API Keys
                </FieldLabel>
                <APIKeyMultiSelect
                  options={apiKeys}
                  value={editAllowedKeyIDs}
                  disabled={false}
                  onChange={setEditAllowedKeyIDs}
                  allLabel="全部 API Key"
                  selectedLabel={`已选 ${editAllowedKeyIDs.length} 个 Key`}
                  placeholder="选择允许使用的 API Key"
                  emptyLabel="暂无 API Key"
                  emptyHint="可先在 API Keys 页面创建"
                />
              </div>
            </section>
          </div>
        )}
      </Modal>

      {testingAccount && (
        <TestConnectionModal
          account={testingAccount}
          onClose={() => setTestingAccount(null)}
          onSettled={() => void loadAccounts()}
        />
      )}

      {detailAccount && (
        <AccountDetailSheet
          account={detailAccount}
          groups={(detailAccount.group_ids ?? []).map((id) => groupMap.get(id)).filter(Boolean) as AccountGroup[]}
          refreshing={syncingId === detailAccount.id}
          onClose={() => setDetailAccount(null)}
          onEdit={() => {
            const target = detailAccount;
            setDetailAccount(null);
            void openEditModal(target);
          }}
          onUsage={() => void handleSyncOne(detailAccount)}
          onTest={() => {
            setTestingAccount(detailAccount);
            setDetailAccount(null);
          }}
          onRefresh={() => void handleSyncOne(detailAccount)}
          onGenerateAuthJson={() => void handleExport([detailAccount.id])}
          onToggleEnabled={() => void handleToggleEnabled(detailAccount)}
          onToggleLock={() => {
            void api
              .toggleAccountLock(detailAccount.id, !detailAccount.locked)
              .then(() => {
                showToast(detailAccount.locked ? "已解锁账号" : "已锁定账号", "success");
                void loadAccounts();
              })
              .catch((err) => showToast(getErrorMessage(err), "error"));
          }}
          onResetStatus={() => {
            void api
              .resetAccountStatus(detailAccount.id)
              .then(() => {
                showToast("已重置账号状态", "success");
                void loadAccounts();
              })
              .catch((err) => showToast(getErrorMessage(err), "error"));
          }}
          onSaveModelCooldownPolicy={(data) => {
            void api
              .updateAccountModelCooldownPolicy(detailAccount.id, data)
              .then(() => {
                showToast("已保存模型冷却策略", "success");
                void loadAccounts();
              })
              .catch((err) => showToast(getErrorMessage(err), "error"));
          }}
          onClearModelCooldown={(model) => {
            void api
              .clearAccountModelCooldown(detailAccount.id, model)
              .then(() => {
                showToast(`已清除模型 ${model} 的冷却`, "success");
                void loadAccounts();
              })
              .catch((err) => showToast(getErrorMessage(err), "error"));
          }}
          onClearAllModelCooldowns={() => {
            void api
              .clearAllAccountModelCooldowns(detailAccount.id)
              .then((res) => {
                showToast(`已清除 ${res.cleared} 个模型冷却`, "success");
                void loadAccounts();
              })
              .catch((err) => showToast(getErrorMessage(err), "error"));
          }}
          onResetCredits={() => undefined}
          onDelete={() => {
            const target = detailAccount;
            setDetailAccount(null);
            void handleDeleteOne(target);
          }}
        />
      )}

      <AccountProxyQuickEditor
        account={quickProxyAccount}
        accountLabel={quickProxyAccount ? accountLabel(quickProxyAccount) : ""}
        proxies={proxies}
        ctx={proxyBindingContext}
        onClose={() => setQuickProxyAccount(null)}
        onSaved={() => {
          setQuickProxyAccount(null);
          void loadAccounts();
        }}
      />

      {showGroupManager ? (
        <AccountGroupManagerModal
          channel="opencode"
          groups={openCodeGroups}
          title="OpenCode 分组管理"
          onClose={() => setShowGroupManager(false)}
          onChanged={() => {
            void loadAux();
            void loadAccounts();
          }}
        />
      ) : null}

      <BatchAccountGroupModal
        show={showBatchGroupModal}
        ids={Array.from(selectedIds)}
        channel="opencode"
        groups={openCodeGroups}
        onClose={() => setShowBatchGroupModal(false)}
        onSaved={() => {
          setShowBatchGroupModal(false);
          void loadAccounts();
        }}
        onGroupsChanged={() => void loadAux()}
      />
    </div>
  );
}
