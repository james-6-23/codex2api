import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  Download,
  FolderOpen,
  KeyRound,
  Loader2,
  Pencil,
  Plus,
  Power,
  PowerOff,
  RefreshCw,
  Search,
  Sliders,
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
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import TestConnectionModal from "../components/TestConnectionModal";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from "../hooks/usePersistedPageSize";
import { useToast } from "../hooks/useToast";
import { buildProxyBindingContext } from "../lib/accountProxyBinding";
import { getAccountStatusBadgeStatus } from "../lib/usageFormat";
import { cn } from "@/lib/utils";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";

const TEXTAREA_CLASS =
  "flex min-h-[140px] w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50";

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

const MODE_OPTIONS = [
  { value: "go", label: "OpenCode Go（订阅套餐，支持三窗口额度）" },
  { value: "zen", label: "OpenCode Zen（按量付费网关）" },
];

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
    <div className="space-y-2 rounded-lg border border-border bg-muted/20 p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <div className="text-xs font-medium text-foreground">协议路由规则 (protocol_rules)</div>
          <div className="text-[11px] text-muted-foreground">
            留空时使用 {mode === "zen" ? "Zen" : "Go"} 内置规则；按顺序首条命中，支持精确名或末尾 <code>*</code>，未命中回落 Chat Completions。
          </div>
        </div>
        <div className="flex items-center gap-1.5">
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={() => onChange(defaults.map((r) => ({ ...r })))}
          >
            载入 {mode === "zen" ? "Zen" : "Go"} 默认
          </Button>
          {rules.length > 0 && (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-7 px-2 text-xs text-muted-foreground"
              onClick={() => onChange([])}
            >
              清空（用内置）
            </Button>
          )}
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={() => onChange([...rules, { pattern: "", protocol: "chat_completions" }])}
          >
            <Plus className="mr-1 size-3" /> 添加规则
          </Button>
        </div>
      </div>

      {rules.length === 0 ? (
        <div className="rounded-md border border-dashed border-border/70 px-3 py-2 text-[11px] text-muted-foreground">
          当前使用内置规则：{defaults.map((r) => `${r.pattern} → ${r.protocol}`).join("，")}；其余 → chat_completions。
        </div>
      ) : (
        <div className="space-y-2">
          {rules.map((rule, idx) => (
            <div key={idx} className="flex flex-wrap items-center gap-2">
              <Input
                value={rule.pattern}
                onChange={(e) => {
                  const next = [...rules];
                  next[idx] = { ...rule, pattern: e.target.value };
                  onChange(next);
                }}
                placeholder="例如 grok-* / claude-* / *"
                className="h-8 flex-1 min-w-[140px] font-mono text-xs"
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
                className="w-44"
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="h-8 w-8 text-muted-foreground hover:text-destructive"
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
          <CompactStat label="总账号" value={summary.total} tone="neutral" />
          <CompactStat label="可用" value={summary.normal} tone="success" />
          <CompactStat label="限流" value={summary.rate_limited} tone="warning" />
          <CompactStat label="异常" value={summary.abnormal} tone="danger" />
          <CompactStat label="Go 订阅" value={goCount} tone="neutral" />
          <CompactStat label="Zen 按量" value={zenCount} tone="neutral" />
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

            <div className="inline-flex rounded-md border border-border bg-muted/40 p-0.5 text-xs">
              {(
                [
                  ["all", `全部 (${summary?.total ?? total})`],
                  ["go", `Go 订阅 (${goCount})`],
                  ["zen", `Zen 按量 (${zenCount})`],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => {
                    setModeFilter(key);
                    setPage(1);
                  }}
                  className={cn(
                    "rounded px-2.5 py-1 font-medium transition-colors",
                    modeFilter === key ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {label}
                </button>
              ))}
            </div>

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
        title="添加 OpenCode 账号"
        contentClassName="sm:max-w-2xl"
      >
        <div className="space-y-4">
          <div className="inline-flex rounded-lg border border-border bg-muted/40 p-0.5 text-xs">
            <button
              type="button"
              onClick={() => setAddTab("single")}
              className={cn(
                "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 font-medium",
                addTab === "single" ? "bg-background text-foreground shadow-xs" : "text-muted-foreground",
              )}
            >
              <KeyRound className="size-3.5" /> 单个添加
            </button>
            <button
              type="button"
              onClick={() => setAddTab("import")}
              className={cn(
                "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 font-medium",
                addTab === "import" ? "bg-background text-foreground shadow-xs" : "text-muted-foreground",
              )}
            >
              <Upload className="size-3.5" /> 批量导入 / Sub2API JSON
            </button>
          </div>

          {addTab === "single" ? (
            <div className="space-y-3">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label className="mb-1 block text-xs font-medium">接入模式</label>
                  <Select value={addMode} onValueChange={(v) => setAddMode(v as OpenCodeMode)} options={MODE_OPTIONS} />
                </div>
                <div>
                  <label className="mb-1 block text-xs font-medium">名称（可选）</label>
                  <Input value={addName} onChange={(e) => setAddName(e.target.value)} placeholder="默认按模式与 Key 后缀生成" className="h-9 text-xs" />
                </div>
              </div>

              <div>
                <label className="mb-1 block text-xs font-medium">API Key *</label>
                <Input
                  type="password"
                  value={addAPIKey}
                  onChange={(e) => setAddAPIKey(e.target.value)}
                  placeholder="sk-..."
                  className="h-9 font-mono text-xs"
                />
              </div>

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label className="mb-1 block text-xs font-medium">自定义 Base URL（可选）</label>
                  <Input
                    value={addBaseURL}
                    onChange={(e) => setAddBaseURL(e.target.value)}
                    placeholder={addMode === "zen" ? DEFAULT_ZEN_BASE_URL : DEFAULT_GO_BASE_URL}
                    className="h-9 font-mono text-xs"
                  />
                </div>
                <div>
                  <label className="mb-1 block text-xs font-medium">Anthropic Base URL（可选）</label>
                  <Input
                    value={addAnthropicBaseURL}
                    onChange={(e) => setAddAnthropicBaseURL(e.target.value)}
                    placeholder={addMode === "zen" ? DEFAULT_ZEN_ANTHROPIC_URL : DEFAULT_GO_ANTHROPIC_URL}
                    className="h-9 font-mono text-xs"
                  />
                </div>
              </div>

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label className="mb-1 block text-xs font-medium">备注邮箱（可选）</label>
                  <Input value={addEmail} onChange={(e) => setAddEmail(e.target.value)} placeholder="user@example.com" className="h-9 text-xs" />
                </div>
                <div>
                  <label className="mb-1 block text-xs font-medium">代理（可选）</label>
                  <ProxyField value={addProxyURL} onChange={setAddProxyURL} proxies={proxies} />
                </div>
              </div>

              <div>
                <label className="mb-1 block text-xs font-medium">模型白名单（留空则使用同步目录/默认目录）</label>
                <ChipInput value={addModels} onChange={setAddModels} placeholder="输入模型名后回车，例如 glm-5.3" />
              </div>

              <ProtocolRulesEditor mode={addMode} rules={addProtocolRules} onChange={setAddProtocolRules} />

              <div>
                <label className="mb-1 block text-xs font-medium">自定义请求头 JSON（可选，支持覆盖 User-Agent）</label>
                <Input
                  value={addCustomHeadersText}
                  onChange={(e) => setAddCustomHeadersText(e.target.value)}
                  placeholder='{"User-Agent": "opencode/1.19.6"}'
                  className="h-9 font-mono text-xs"
                />
              </div>

              <div className="flex items-center justify-between rounded-md border border-border px-3 py-2">
                <span className="text-xs text-muted-foreground">添加时立即拉取模型列表{addMode === "go" ? "与 Go 额度快照" : ""}</span>
                <Switch checked={addAutoSync} onCheckedChange={setAddAutoSync} />
              </div>

              <div className="flex justify-end gap-2 pt-2">
                <Button variant="outline" size="sm" disabled={addSubmitting} onClick={() => setShowAddModal(false)}>
                  取消
                </Button>
                <Button size="sm" disabled={addSubmitting} onClick={() => void handleAddSingle()}>
                  {addSubmitting ? <Loader2 className="mr-1.5 size-3.5 animate-spin" /> : null}
                  保存并添加
                </Button>
              </div>
            </div>
          ) : (
            <div className="space-y-3">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label className="mb-1 block text-xs font-medium">默认模式（JSON 未指定时）</label>
                  <Select value={importMode} onValueChange={(v) => setImportMode(v as OpenCodeMode)} options={MODE_OPTIONS} />
                </div>
                <div>
                  <label className="mb-1 block text-xs font-medium">默认代理（可选）</label>
                  <ProxyField value={importProxyURL} onChange={setImportProxyURL} proxies={proxies} />
                </div>
              </div>

              <div>
                <label className="mb-1 block text-xs font-medium">默认 Base URL（可选）</label>
                <Input
                  value={importBaseURL}
                  onChange={(e) => setImportBaseURL(e.target.value)}
                  placeholder={importMode === "zen" ? DEFAULT_ZEN_BASE_URL : DEFAULT_GO_BASE_URL}
                  className="h-9 font-mono text-xs"
                />
              </div>

              <div>
                <div className="mb-1 flex items-center justify-between">
                  <label className="text-xs font-medium">导入内容</label>
                  <div>
                    <input
                      ref={importFileRef}
                      type="file"
                      accept=".json,.txt"
                      className="hidden"
                      onChange={(e) => void handleImportFileChange(e)}
                    />
                    <Button type="button" variant="outline" size="sm" className="h-7 px-2 text-xs" onClick={() => importFileRef.current?.click()}>
                      <FolderOpen className="mr-1 size-3" /> 选择文件
                    </Button>
                  </div>
                </div>
                <textarea
                  value={importContent}
                  onChange={(e) => setImportContent(e.target.value)}
                  className={TEXTAREA_CLASS}
                  placeholder={`支持以下格式：\n1. 每行一个 API Key 或 "名称,API Key"\n2. OpenCode 导出的 JSON 数组或对象\n3. Sub2API 导出的 JSON（自动提取 platform=opencode_go 条目）`}
                />
              </div>

              <div className="flex items-center justify-between rounded-md border border-border px-3 py-2">
                <span className="text-xs text-muted-foreground">导入时立即同步模型与 Go 额度</span>
                <Switch checked={importAutoSync} onCheckedChange={setImportAutoSync} />
              </div>

              <div className="flex justify-end gap-2 pt-2">
                <Button variant="outline" size="sm" disabled={addSubmitting} onClick={() => setShowAddModal(false)}>
                  取消
                </Button>
                <Button size="sm" disabled={addSubmitting} onClick={() => void handleBatchImport()}>
                  {addSubmitting ? <Loader2 className="mr-1.5 size-3.5 animate-spin" /> : null}
                  开始导入
                </Button>
              </div>
            </div>
          )}
        </div>
      </Modal>

      {/* 编辑弹窗 */}
      <Modal
        show={Boolean(editingAccount)}
        onClose={() => {
          if (!editSaving) setEditingAccount(null);
        }}
        title={editingAccount ? `编辑 OpenCode 账号：${accountLabel(editingAccount)}` : "编辑账号"}
        contentClassName="sm:max-w-2xl"
      >
        {editLoading ? (
          <div className="flex items-center justify-center py-10 text-sm text-muted-foreground">
            <Loader2 className="mr-2 size-4 animate-spin" /> 正在加载配置…
          </div>
        ) : (
          <div className="space-y-3">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label className="mb-1 block text-xs font-medium">接入模式</label>
                <Select value={editMode} onValueChange={(v) => setEditMode(v as OpenCodeMode)} options={MODE_OPTIONS} />
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium">名称</label>
                <Input value={editName} onChange={(e) => setEditName(e.target.value)} className="h-9 text-xs" />
              </div>
            </div>

            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label className="mb-1 block text-xs font-medium">更换 API Key（留空保持不变）</label>
                <Input
                  type="password"
                  value={editAPIKey}
                  onChange={(e) => setEditAPIKey(e.target.value)}
                  placeholder="留空不修改"
                  className="h-9 font-mono text-xs"
                />
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium">备注邮箱</label>
                <Input value={editEmail} onChange={(e) => setEditEmail(e.target.value)} className="h-9 text-xs" />
              </div>
            </div>

            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label className="mb-1 block text-xs font-medium">Base URL（留空使用官方默认）</label>
                <Input
                  value={editBaseURL}
                  onChange={(e) => setEditBaseURL(e.target.value)}
                  placeholder={editMode === "zen" ? DEFAULT_ZEN_BASE_URL : DEFAULT_GO_BASE_URL}
                  className="h-9 font-mono text-xs"
                />
              </div>
              <div>
                <label className="mb-1 block text-xs font-medium">Anthropic Base URL（可选）</label>
                <Input
                  value={editAnthropicBaseURL}
                  onChange={(e) => setEditAnthropicBaseURL(e.target.value)}
                  placeholder={editMode === "zen" ? DEFAULT_ZEN_ANTHROPIC_URL : DEFAULT_GO_ANTHROPIC_URL}
                  className="h-9 font-mono text-xs"
                />
              </div>
            </div>

            <div>
              <label className="mb-1 block text-xs font-medium">代理</label>
              <ProxyField value={editProxyURL} onChange={setEditProxyURL} proxies={proxies} />
            </div>

            <div>
              <label className="mb-1 block text-xs font-medium">模型白名单（留空则按同步目录放行）</label>
              <ChipInput value={editModels} onChange={setEditModels} placeholder="输入模型名后回车" />
            </div>

            <div>
              <label className="mb-1 block text-xs font-medium">账号级模型映射 JSON（可选）</label>
              <Input
                value={editModelMapping}
                onChange={(e) => setEditModelMapping(e.target.value)}
                placeholder='{"gpt-4o": "glm-5.3"}'
                className="h-9 font-mono text-xs"
              />
            </div>

            <ProtocolRulesEditor mode={editMode} rules={editProtocolRules} onChange={setEditProtocolRules} />

            <div>
              <label className="mb-1 block text-xs font-medium">自定义请求头 JSON</label>
              <textarea
                value={editCustomHeadersText}
                onChange={(e) => setEditCustomHeadersText(e.target.value)}
                className="flex min-h-[72px] w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs"
                placeholder='{"User-Agent": "opencode/1.19.6"}'
              />
            </div>

            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label className="mb-1 block text-xs font-medium">所属分组</label>
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
                <label className="mb-1 block text-xs font-medium">限定 API Keys（留空不限）</label>
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
            </div>

            <div className="flex justify-end gap-2 pt-2">
              <Button variant="outline" size="sm" disabled={editSaving} onClick={() => setEditingAccount(null)}>
                取消
              </Button>
              <Button size="sm" disabled={editSaving} onClick={() => void handleSaveEdit()}>
                {editSaving ? <Loader2 className="mr-1.5 size-3.5 animate-spin" /> : null}
                保存修改
              </Button>
            </div>
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
