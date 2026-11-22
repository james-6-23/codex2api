import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Download,
  Eraser,
  FlaskConical,
  FolderOpen,
  Layers,
  Link2,
  ListChecks,
  Loader2,
  Lock,
  MoreHorizontal,
  Plus,
  Power,
  PowerOff,
  RefreshCw,
  RotateCcw,
  RotateCw,
  Search,
  SlidersHorizontal,
  Trash2,
  Unlock,
  Upload,
  X,
} from "lucide-react";
import { api } from "../api";
import type { ProxyRow } from "../api";
import type {
  AccountGroup,
  AccountHealthBucket,
  AccountListSummary,
  AccountOperationSelector,
  AccountPageStatsItem,
  AccountRow,
  AntigravityAccountState,
} from "../types";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  isAccountGroupFilterEmpty,
  pruneAccountGroupFilter,
  type AccountGroupFilterValue,
} from "../components/AccountGroupFilterSelect";
import { AccountGroupManagerModal } from "../components/AccountGroupManagerModal";
import AccountDetailSheet from "../components/AccountDetailSheet";
import AccountProxyQuickEditor from "../components/AccountProxyQuickEditor";
import AccountUsageModal from "../components/AccountUsageModal";
import BatchAccountGroupModal from "../components/BatchAccountGroupModal";
import ChannelLogo from "../components/ChannelLogo";
import ColumnSettingsMenu from "../components/ColumnSettingsMenu";
import { CompactStat } from "../components/CompactStat";
import {
  HeaderActionMenu,
  type HeaderActionMenuSection,
} from "../components/HeaderActionMenu";
import OperationProgressToast from "../components/OperationProgressToast";
import OperationResultsModal from "../components/OperationResultsModal";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import TestConnectionModal from "../components/TestConnectionModal";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { useAccountTableColumns } from "../hooks/useAccountTableColumns";
import { useMediaQuery } from "../hooks/useMediaQuery";
import { useOperationProgress } from "../hooks/useOperationProgress";
import {
  DEFAULT_PAGE_SIZE_OPTIONS,
  usePersistedPageSize,
} from "../hooks/usePersistedPageSize";
import { useToast } from "../hooks/useToast";
import {
  buildProxyBindingContext,
  type ProxyBindingContext,
} from "../lib/accountProxyBinding";
import {
  isLargePoolSortDisabled,
  resolveDisabledAccountSorts,
} from "../lib/accountListSort";
import {
  readAntigravityAccountSort,
  writeAntigravityAccountSort,
  type AntigravityAccountSortKey,
} from "../lib/antigravityAccountSort";
import { accountLabel, isAntigravityAPIKeyAccount } from "../lib/antigravityQuota";
import { cn } from "@/lib/utils";
import { getErrorMessage } from "../utils/error";
import {
  ANTIGRAVITY_TABLE_COLUMNS,
  AntigravityAccountCard,
  AntigravityAccountRow,
  type AntigravityRowBusyAction,
  type AntigravityRowProps,
} from "./antigravity/AntigravityAccountRow";
import AntigravityBatchModelsModal from "./antigravity/AntigravityBatchModelsModal";
import AntigravityEditModal from "./antigravity/AntigravityEditModal";
import AntigravityAddModal from "./antigravity/AntigravityAddModal";
import AntigravityImportModal from "./antigravity/AntigravityImportModal";
import AntigravityOAuthModal from "./antigravity/AntigravityOAuthModal";
import AntigravityQuotaDetailModal from "./antigravity/AntigravityQuotaDetailModal";
import { AntigravityQuotaOverview } from "./antigravity/AntigravityQuotaCells";
import { AntigravityProviderPanel } from "./antigravity/AntigravityStatePanel";

type StatusFilter = "all" | "active" | "rate_limited" | "abnormal" | "disabled" | "locked";
type AuthFilter = "all" | "oauth" | "api_key";
type SortKey = AntigravityAccountSortKey;
type SortDir = "asc" | "desc";
type BatchOperation = "sync" | "test" | "delete" | "update" | "reset" | "clean" | "quota-clear";

const DEFAULT_SORT: SortKey = "updated_at";
const SORT_DEFAULT_DIR: Record<SortKey, SortDir> = {
  updated_at: "desc",
  usage: "desc",
  today: "desc",
  requests: "desc",
  created_at: "desc",
  group: "asc",
  id: "desc",
};

// 列配置 v2:项目与权限合并成"订阅与权限"一列并新增今日/请求列,换新 key 避免旧配置错位。
const COLUMNS_STORAGE_KEY = "codex2api:antigravity-accounts:columns-v2";

const filterPillClass =
  "inline-flex max-w-full items-center gap-1.5 rounded-md border border-border/60 bg-background/70 px-2 py-1 text-[11px] font-medium text-muted-foreground transition-colors hover:border-primary/30 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50";
const segmentGroupClass =
  "flex h-8 max-w-full shrink-0 items-center gap-0.5 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden";
const segmentButtonClass = (active: boolean) =>
  cn(
    "flex h-6 shrink-0 items-center whitespace-nowrap rounded-md px-2 text-xs font-medium transition-colors",
    active ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
  );

function downloadAntigravityBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

function AntigravityAccounts({
  headerSlot,
  showOperationResults = false,
}: {
  headerSlot?: ReactNode;
  showOperationResults?: boolean;
} = {}) {
  const { t } = useTranslation();
  const isTableViewport = useMediaQuery("(min-width: 768px)");
  const { columns: visibleColumns, toggleColumn, resetColumns } = useAccountTableColumns(
    COLUMNS_STORAGE_KEY,
    ANTIGRAVITY_TABLE_COLUMNS,
  );
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const {
    operationProgress,
    operationResults,
    runStreamingOperation,
    closeOperationProgress,
    closeOperationResults,
  } = useOperationProgress(showOperationResults);
  const requestAbortRef = useRef<AbortController | null>(null);

  // ---------- 列表数据 ----------
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [allGroups, setAllGroups] = useState<AccountGroup[]>([]);
  const antigravityGroups = useMemo(
    () => allGroups.filter((group) => group.channel === "antigravity"),
    [allGroups],
  );
  const [serverSummary, setServerSummary] = useState<AccountListSummary | null>(null);
  const [totalAccounts, setTotalAccounts] = useState(0);
  const [disabledSorts, setDisabledSorts] = useState<string[] | undefined>(undefined);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [page, setPage] = useState(1);
  const [loadedPage, setLoadedPage] = useState(0);
  const [pageSize, setPageSize] = usePersistedPageSize(
    "antigravity-accounts",
    20,
    DEFAULT_PAGE_SIZE_OPTIONS,
  );
  const [searchQuery, setSearchQuery] = useState("");
  const [debouncedSearchQuery, setDebouncedSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [authFilter, setAuthFilter] = useState<AuthFilter>("all");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(
    EMPTY_ACCOUNT_GROUP_FILTER,
  );
  // 排序按标签页记住（sessionStorage），与 Codex 账号页一致：切换菜单再回来不丢，新标签页恢复默认。
  const [initialSort] = useState(readAntigravityAccountSort);
  const [sortKey, setSortKey] = useState<SortKey>(initialSort?.key ?? DEFAULT_SORT);
  const [sortDir, setSortDir] = useState<SortDir>(initialSort?.dir ?? SORT_DEFAULT_DIR[DEFAULT_SORT]);
  useEffect(() => {
    const isDefault = sortKey === DEFAULT_SORT && sortDir === SORT_DEFAULT_DIR[DEFAULT_SORT];
    writeAntigravityAccountSort(isDefault ? null : { key: sortKey, dir: sortDir });
  }, [sortKey, sortDir]);
  const [showFilters, setShowFilters] = useState(false);
  const [pageStats, setPageStats] = useState<Record<string, AccountPageStatsItem>>({});
  const [healthBars, setHealthBars] = useState<Record<string, AccountHealthBucket[]>>({});
  // 跨页勾选的账号只知道 id;这里记住见过的行,用来判断勾选里有没有 API Key 号。
  const knownAccountsRef = useRef<Map<number, AccountRow>>(new Map());

  // ---------- 选择 / 批量 ----------
  const [selected, setSelected] = useState<Set<number>>(() => new Set());
  const [batchOperation, setBatchOperation] = useState<BatchOperation | null>(null);
  const [batchGroupOpen, setBatchGroupOpen] = useState(false);
  const [batchModelsOpen, setBatchModelsOpen] = useState(false);
  const [manageGroupsOpen, setManageGroupsOpen] = useState(false);
  const batchBusy = batchOperation !== null;

  // ---------- 行操作与弹窗 ----------
  const [busy, setBusy] = useState<{ id: number; action: AntigravityRowBusyAction } | null>(null);
  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null);
  const [usageAccount, setUsageAccount] = useState<AccountRow | null>(null);
  const [editTarget, setEditTarget] = useState<AccountRow | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [showImport, setShowImport] = useState(false);
  const [showOAuth, setShowOAuth] = useState(false);
  const [exporting, setExporting] = useState(false);

  // 代理池 + 代理池开关 + 全局代理:代理徽章的判定输入(拉取失败静默留空,不影响手填)。
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  const [proxyPoolEnabled, setProxyPoolEnabled] = useState(false);
  const [globalProxyURL, setGlobalProxyURL] = useState("");
  const [quickProxyAccount, setQuickProxyAccount] = useState<AccountRow | null>(null);
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
  // 分组用全量而非 antigravityGroups:后端解析组代理不看渠道,按渠道过滤会把
  // 跨渠道的存量成员误报成"无组代理"。
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

  // ---------- 详情抽屉 ----------
  const [detailAccountId, setDetailAccountId] = useState<number | null>(null);
  // 同一个详情账号有两种视图:管理抽屉(sheet)与"额度与权限"宽版窗口(quota),共用管理状态。
  const [detailView, setDetailView] = useState<"sheet" | "quota">("sheet");
  const [managementState, setManagementState] = useState<AntigravityAccountState | null>(null);
  const [managementLoading, setManagementLoading] = useState(false);
  const [managementError, setManagementError] = useState<string | null>(null);
  const [managementAction, setManagementAction] = useState<"sync" | "probe" | null>(null);
  const detailAccountIdRef = useRef<number | null>(null);
  const detailGenerationRef = useRef(0);
  const managementAbortRef = useRef<AbortController | null>(null);
  const detailNavigationTargetRef = useRef<"first" | "last" | null>(null);

  const isCurrentDetailRequest = useCallback(
    (accountId: number, generation: number) =>
      detailAccountIdRef.current === accountId &&
      detailGenerationRef.current === generation,
    [],
  );

  const openDetailAccount = useCallback((accountId: number, view: "sheet" | "quota" = "sheet") => {
    setDetailView(view);
    if (detailAccountIdRef.current === accountId) {
      // 同一账号只切换视图,不重拉管理状态。
      return;
    }
    managementAbortRef.current?.abort();
    detailGenerationRef.current += 1;
    detailAccountIdRef.current = accountId;
    setManagementState(null);
    setManagementError(null);
    setManagementAction(null);
    setManagementLoading(true);
    setDetailAccountId(accountId);
  }, []);

  const closeDetailAccount = useCallback(() => {
    managementAbortRef.current?.abort();
    detailGenerationRef.current += 1;
    detailAccountIdRef.current = null;
    detailNavigationTargetRef.current = null;
    setDetailView("sheet");
    setDetailAccountId(null);
    setManagementState(null);
    setManagementError(null);
    setManagementAction(null);
    setManagementLoading(false);
  }, []);

  useEffect(() => {
    if (detailAccountId === null) {
      setManagementState(null);
      setManagementError(null);
      setManagementAction(null);
      setManagementLoading(false);
      return;
    }
    const accountId = detailAccountId;
    const generation = detailGenerationRef.current;
    const controller = new AbortController();
    managementAbortRef.current = controller;
    setManagementLoading(true);
    setManagementError(null);
    void api.getAntigravityAccountState(accountId, controller.signal)
      .then((state) => {
        if (isCurrentDetailRequest(accountId, generation)) {
          setManagementState(state);
        }
      })
      .catch((stateError) => {
        if (
          !controller.signal.aborted &&
          isCurrentDetailRequest(accountId, generation)
        ) {
          setManagementError(getErrorMessage(stateError));
        }
      })
      .finally(() => {
        if (
          !controller.signal.aborted &&
          isCurrentDetailRequest(accountId, generation)
        ) {
          setManagementLoading(false);
        }
      });
    return () => {
      controller.abort();
      if (managementAbortRef.current === controller) {
        managementAbortRef.current = null;
      }
    };
  }, [detailAccountId, isCurrentDetailRequest]);

  // ---------- 加载 ----------
  const reloadGroups = useCallback(async () => {
    const response = await api.listAccountGroups();
    const groups = response.groups ?? [];
    setAllGroups(groups);
    setGroupFilter((current) => pruneAccountGroupFilter(current, groups));
    return groups;
  }, []);

  useEffect(() => {
    void reloadGroups().catch(() => undefined);
  }, [reloadGroups]);

  const reload = useCallback(
    async (options?: { silent?: boolean }) => {
      requestAbortRef.current?.abort();
      const controller = new AbortController();
      requestAbortRef.current = controller;
      if (!options?.silent) setLoading(true);
      try {
        const response = await api.getAccountsPage(
          {
            channel: "antigravity",
            page,
            pageSize,
            search: debouncedSearchQuery,
            status: statusFilter,
            authKind: authFilter,
            groupInclude: groupFilter.include,
            groupExclude: groupFilter.exclude,
            ungrouped: groupFilter.ungrouped,
            sort: sortKey,
            order: sortDir,
          },
          controller.signal,
        );
        if (controller.signal.aborted) return;
        const rows = (response.accounts ?? []).filter(
          (account) => account.antigravity_api !== false,
        );
        for (const row of rows) knownAccountsRef.current.set(row.id, row);
        setAccounts(rows);
        setTotalAccounts(response.total ?? 0);
        setServerSummary(response.summary ?? null);
        setDisabledSorts(response.disabled_sorts);
        setLoadedPage(response.page ?? page);
        if (response.page && response.page !== page) setPage(response.page);
        setError(null);
      } catch (loadError) {
        if (controller.signal.aborted) return;
        const message = getErrorMessage(loadError);
        setError(message);
        showToast(message, "error");
      } finally {
        if (requestAbortRef.current === controller) setLoading(false);
      }
    },
    [
      authFilter,
      debouncedSearchQuery,
      groupFilter.exclude,
      groupFilter.include,
      groupFilter.ungrouped,
      page,
      pageSize,
      showToast,
      sortDir,
      sortKey,
      statusFilter,
    ],
  );

  useEffect(() => {
    void reload();
  }, [reload]);

  useEffect(() => () => requestAbortRef.current?.abort(), []);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDebouncedSearchQuery(searchQuery.trim());
      setPage(1);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [searchQuery]);

  useEffect(() => {
    setPage(1);
  }, [authFilter, groupFilter, sortDir, sortKey, statusFilter]);

  // 当前页的网关侧用量明细(今日 / 5h / 7d)与健康条;失败不阻断列表。
  const accountIDsKey = useMemo(() => accounts.map((account) => account.id).join(","), [accounts]);
  useEffect(() => {
    if (!accountIDsKey) {
      setPageStats({});
      setHealthBars({});
      return;
    }
    const ids = accountIDsKey.split(",").map(Number);
    const controller = new AbortController();
    void api
      .getAccountPageStats(ids, controller.signal)
      .then((res) => {
        if (!controller.signal.aborted) setPageStats(res.stats ?? {});
      })
      .catch(() => undefined);
    let cancelled = false;
    void api
      .getAccountHealthBars(ids)
      .then((res) => {
        if (!cancelled) setHealthBars(res.buckets ?? {});
      })
      .catch(() => undefined);
    return () => {
      controller.abort();
      cancelled = true;
    };
  }, [accountIDsKey]);

  // 渲染行 = 基础行 + page-stats 补齐(只补缺失字段,基础行已有的以基础行为准)。
  const displayRows = useMemo(
    () =>
      accounts.map((account) => {
        const stats = pageStats[String(account.id)];
        if (!stats) return account;
        const merged = { ...account };
        if (!merged.usage_today_detail && stats.usage_today_detail) merged.usage_today_detail = stats.usage_today_detail;
        if (!merged.usage_5h_detail && stats.usage_5h_detail) merged.usage_5h_detail = stats.usage_5h_detail;
        if (!merged.usage_7d_detail && stats.usage_7d_detail) merged.usage_7d_detail = stats.usage_7d_detail;
        return merged;
      }),
    [accounts, pageStats],
  );

  // 跨页切换详情时旧页行先消失、新页行随后到达;保留上一次解析到的行,避免抽屉闪关。
  const lastDetailAccountRef = useRef<AccountRow | null>(null);
  const detailAccount = useMemo(() => {
    if (detailAccountId == null) return null;
    const found = displayRows.find((account) => account.id === detailAccountId);
    if (found) {
      lastDetailAccountRef.current = found;
      return found;
    }
    return lastDetailAccountRef.current?.id === detailAccountId ? lastDetailAccountRef.current : null;
  }, [displayRows, detailAccountId]);
  const detailNavIndex = useMemo(
    () => (detailAccountId == null ? -1 : displayRows.findIndex((account) => account.id === detailAccountId)),
    [detailAccountId, displayRows],
  );
  const totalPages = Math.max(1, Math.ceil(totalAccounts / pageSize));

  const goDetailPrev = useCallback(() => {
    if (detailNavIndex > 0) {
      openDetailAccount(displayRows[detailNavIndex - 1].id);
      return;
    }
    if (page > 1) {
      detailNavigationTargetRef.current = "last";
      setPage(page - 1);
    }
  }, [detailNavIndex, displayRows, openDetailAccount, page]);

  const goDetailNext = useCallback(() => {
    if (detailNavIndex >= 0 && detailNavIndex < displayRows.length - 1) {
      openDetailAccount(displayRows[detailNavIndex + 1].id);
      return;
    }
    if (page < totalPages) {
      detailNavigationTargetRef.current = "first";
      setPage(page + 1);
    }
  }, [detailNavIndex, displayRows, openDetailAccount, page, totalPages]);

  useEffect(() => {
    const target = detailNavigationTargetRef.current;
    if (!target || loadedPage !== page || accounts.length === 0) return;
    const account = target === "first" ? accounts[0] : accounts[accounts.length - 1];
    detailNavigationTargetRef.current = null;
    if (account) openDetailAccount(account.id);
  }, [accounts, loadedPage, openDetailAccount, page]);

  // ---------- 选择 ----------
  const selectedIds = useMemo(() => Array.from(selected), [selected]);
  const allPageSelected =
    accounts.length > 0 && accounts.every((account) => selected.has(account.id));
  const toggleSelect = useCallback((id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);
  // 勾选跨页保留,"全选"只作用于当前页(与其他渠道页一致)。
  const toggleSelectAll = useCallback(() => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (accounts.every((account) => prev.has(account.id))) {
        for (const account of accounts) next.delete(account.id);
      } else {
        for (const account of accounts) next.add(account.id);
      }
      return next;
    });
  }, [accounts]);
  const clearSelection = useCallback(() => setSelected(new Set()), []);

  const selectedAPIKeyIds = useMemo(
    () =>
      selectedIds.filter((id) => {
        const account = knownAccountsRef.current.get(id);
        return account ? isAntigravityAPIKeyAccount(account) : false;
      }),
    // knownAccountsRef 随 accounts 刷新,accounts 作为依赖保证重算。
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [selectedIds, accounts],
  );

  const filtersActive = Boolean(
    debouncedSearchQuery ||
      statusFilter !== "all" ||
      authFilter !== "all" ||
      !isAccountGroupFilterEmpty(groupFilter),
  );

  const currentSelector = useMemo<AccountOperationSelector>(
    () => ({
      channel: "antigravity",
      search: debouncedSearchQuery || undefined,
      status: statusFilter !== "all" ? statusFilter : undefined,
      auth_kind: authFilter !== "all" ? authFilter : undefined,
      group_include: groupFilter.include.length > 0 ? groupFilter.include : undefined,
      group_exclude: groupFilter.exclude.length > 0 ? groupFilter.exclude : undefined,
      ungrouped: groupFilter.ungrouped || undefined,
    }),
    [authFilter, debouncedSearchQuery, groupFilter, statusFilter],
  );

  const clearFilters = () => {
    setSearchQuery("");
    setDebouncedSearchQuery("");
    setStatusFilter("all");
    setAuthFilter("all");
    setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
    setPage(1);
  };

  const resolvedDisabledSorts = useMemo(
    () => resolveDisabledAccountSorts(disabledSorts, serverSummary?.total ?? totalAccounts),
    [disabledSorts, serverSummary?.total, totalAccounts],
  );
  const applySort = useCallback(
    (key: SortKey, dir?: SortDir) => {
      if (isLargePoolSortDisabled(key, resolvedDisabledSorts)) {
        showToast(t("accounts.largePoolSortDisabled"), "warning");
        return;
      }
      setSortKey(key);
      setSortDir(dir ?? SORT_DEFAULT_DIR[key]);
    },
    [resolvedDisabledSorts, showToast, t],
  );
  const toggleSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortDir((current) => (current === "desc" ? "asc" : "desc"));
      return;
    }
    applySort(key);
  };

  // ---------- 分组 ----------
  const createAntigravityGroup = useCallback(
    async (name: string): Promise<number | null> => {
      try {
        const response = await api.createAccountGroup({ name: name.trim(), channel: "antigravity" });
        await reloadGroups();
        showToast(t("accounts.groupCreated"), "success");
        return response.id;
      } catch (createError) {
        showToast(getErrorMessage(createError), "error");
        return null;
      }
    },
    [reloadGroups, showToast, t],
  );

  // ---------- 单号操作 ----------
  const runAccountAction = useCallback(
    async (
      account: AccountRow,
      action: AntigravityRowBusyAction,
      operation: () => Promise<{ warning?: string }>,
      successMessage: string,
    ) => {
      setBusy({ id: account.id, action });
      try {
        const result = await operation();
        showToast(result.warning || successMessage, result.warning ? "warning" : "success");
      } catch (actionError) {
        showToast(getErrorMessage(actionError), "error");
      } finally {
        await reload({ silent: true });
        setBusy(null);
      }
    },
    [reload, showToast],
  );

  const handleSync = useCallback(
    (account: AccountRow) =>
      void runAccountAction(
        account,
        "sync",
        () => api.refreshAntigravityAccount(account.id),
        t("antigravity.syncSuccess"),
      ),
    [runAccountAction, t],
  );

  const handleToggleEnabled = useCallback(
    (account: AccountRow) =>
      void runAccountAction(
        account,
        "toggle",
        () => api.toggleAccountEnabled(account.id, account.enabled === false),
        account.enabled === false ? t("antigravity.enableSuccess") : t("antigravity.disableSuccess"),
      ),
    [runAccountAction, t],
  );

  const handleToggleLock = useCallback(
    (account: AccountRow) =>
      void runAccountAction(
        account,
        "lock",
        () => api.toggleAccountLock(account.id, !account.locked),
        account.locked ? t("accounts.unlockSuccess") : t("accounts.lockSuccess"),
      ),
    [runAccountAction, t],
  );

  const handleResetStatus = useCallback(
    (account: AccountRow) =>
      void runAccountAction(
        account,
        "reset",
        () => api.resetAccountStatus(account.id),
        t("accounts.resetStatusSuccess"),
      ),
    [runAccountAction, t],
  );

  const handleDelete = useCallback(
    async (account: AccountRow) => {
      const confirmed = await confirm({
        title: t("antigravity.deleteTitle"),
        description: t("antigravity.deleteDescription", { account: accountLabel(account) }),
        confirmText: t("common.delete"),
        tone: "destructive",
        confirmVariant: "destructive",
      });
      if (!confirmed) return;
      if (detailAccountIdRef.current === account.id) closeDetailAccount();
      setSelected((prev) => {
        if (!prev.has(account.id)) return prev;
        const next = new Set(prev);
        next.delete(account.id);
        return next;
      });
      await runAccountAction(
        account,
        "delete",
        () => api.deleteAccount(account.id),
        t("antigravity.deleteSuccess"),
      );
    },
    [closeDetailAccount, confirm, runAccountAction, t],
  );

  const handleExport = useCallback(async (ids?: number[]) => {
    if (exporting) return;
    const exportingAll = !ids || ids.length === 0;
    if (exportingAll) {
      const confirmed = await confirm({
        title: t("antigravity.exportAllConfirmTitle"),
        description: filtersActive
          ? t("antigravity.exportAllConfirmFiltered")
          : t("antigravity.exportAllConfirmDescription"),
        confirmText: t("antigravity.exportAllConfirmAction"),
        tone: "warning",
      });
      if (!confirmed) return;
    }
    setExporting(true);
    try {
      const { blob, filename, count: responseCount } =
        await api.exportAntigravityAccounts(ids);
      const count = responseCount ?? (ids?.length || undefined);
      // 导出始终是单个 JSON 文件(多账号为数组,与 Codex 导出一致),服务端文件名缺失时按同样格式兜底。
      const fallback = `codex2api-antigravity-${new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19)}-${count ?? 0}.json`;
      downloadAntigravityBlob(blob, filename || fallback);
      showToast(
        count === undefined
          ? t("antigravity.exportSuccessUnknownCount")
          : t("antigravity.exportSuccess", { count }),
        "success",
      );
    } catch (exportError) {
      showToast(t("antigravity.exportFailed", { error: getErrorMessage(exportError) }), "error");
    } finally {
      setExporting(false);
    }
  }, [confirm, exporting, filtersActive, showToast, t]);

  const handleStateSync = useCallback(async () => {
    const accountId = detailAccountIdRef.current;
    if (accountId === null || managementAction !== null) return;
    const generation = detailGenerationRef.current;
    setManagementAction("sync");
    setManagementError(null);
    try {
      const result = await api.syncAntigravityAccountState(accountId);
      if (!isCurrentDetailRequest(accountId, generation)) return;
      setManagementState(result.state);
      showToast(result.message, result.verified ? "success" : "warning");
      await reload({ silent: true });
    } catch (syncError) {
      if (!isCurrentDetailRequest(accountId, generation)) return;
      const message = getErrorMessage(syncError);
      setManagementError(message);
      showToast(message, "error");
    } finally {
      if (isCurrentDetailRequest(accountId, generation)) {
        setManagementAction(null);
      }
    }
  }, [isCurrentDetailRequest, managementAction, reload, showToast]);

  const handleCapabilityProbe = useCallback(async () => {
    const accountId = detailAccountIdRef.current;
    if (accountId === null || managementAction !== null) return;
    const generation = detailGenerationRef.current;
    setManagementAction("probe");
    setManagementError(null);
    try {
      const result = await api.probeAntigravityAccountCapabilities(accountId);
      if (!isCurrentDetailRequest(accountId, generation)) return;
      setManagementState(result.state);
      showToast(result.warning || result.message, result.result.verified ? "success" : "warning");
    } catch (probeError) {
      if (!isCurrentDetailRequest(accountId, generation)) return;
      const message = getErrorMessage(probeError);
      setManagementError(message);
      showToast(message, "error");
    } finally {
      if (isCurrentDetailRequest(accountId, generation)) {
        setManagementAction(null);
      }
    }
  }, [isCurrentDetailRequest, managementAction, showToast]);

  // ---------- 批量操作 ----------
  const finishBatch = useCallback(
    async (clear: boolean) => {
      if (clear) clearSelection();
      await reload({ silent: true });
    },
    [clearSelection, reload],
  );

  const handleBatchUpdate = async (
    patch: { enabled?: boolean; locked?: boolean },
    doneKey: string,
  ) => {
    if (selectedIds.length === 0 || batchBusy) return;
    setBatchOperation("update");
    try {
      const result = await api.batchUpdateAccounts({ ids: selectedIds, ...patch });
      showToast(
        t(doneKey, { success: result.success ?? 0, fail: result.failed ?? 0 }),
        (result.failed ?? 0) > 0 ? "warning" : "success",
      );
      await finishBatch(true);
    } catch (batchError) {
      showToast(getErrorMessage(batchError), "error");
    } finally {
      setBatchOperation(null);
    }
  };

  const handleBatchResetStatus = async () => {
    if (selectedIds.length === 0 || batchBusy) return;
    setBatchOperation("reset");
    try {
      const result = await api.batchResetStatus(selectedIds);
      showToast(t("accounts.batchResetStatusDone", { success: result.success ?? 0, fail: result.failed ?? 0 }));
      await finishBatch(true);
    } catch (batchError) {
      showToast(getErrorMessage(batchError), "error");
    } finally {
      setBatchOperation(null);
    }
  };

  // 运维止血:清除选中账号已存储的配额事实并递增配额纪元(误封/契约异常时使用),
  // 后端同事务清空内存门禁状态,凭据与模型目录不受影响。
  const handleBatchClearQuota = async () => {
    if (selectedIds.length === 0 || batchBusy) return;
    const confirmed = await confirm({
      title: t("antigravityQuota.clearTitle"),
      description: t("antigravityQuota.clearConfirm"),
      confirmText: t("antigravityQuota.clearAction"),
      tone: "warning",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchOperation("quota-clear");
    try {
      const result = await api.clearAntigravityQuota(selectedIds);
      showToast(
        (result.failed ?? 0) > 0
          ? t("antigravityQuota.clearPartial", { cleared: result.cleared ?? 0, failed: result.failed ?? 0 })
          : t("antigravityQuota.clearDone", { cleared: result.cleared ?? 0 }),
        (result.failed ?? 0) > 0 ? "warning" : "success",
      );
      await finishBatch(true);
    } catch (batchError) {
      showToast(t("antigravityQuota.clearFailed", { error: getErrorMessage(batchError) }), "error");
    } finally {
      setBatchOperation(null);
    }
  };

  const handleBatchDelete = async () => {
    if (selectedIds.length === 0 || batchBusy) return;
    const confirmed = await confirm({
      title: t("accounts.batchDeleteTitle"),
      description: t("antigravity.batchDeleteDescription", { count: selectedIds.length }),
      confirmText: t("accounts.deleteConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchOperation("delete");
    try {
      const result = await runStreamingOperation(
        "/accounts/batch-delete?stream=true",
        { ids: selectedIds },
        t("accounts.batchDeleteProgressTitle"),
      );
      showToast(
        t("accounts.batchDeleteDone", {
          success: result?.deleted ?? result?.success ?? 0,
          fail: result?.failed ?? 0,
        }),
        (result?.failed ?? 0) > 0 ? "warning" : "success",
      );
      if (detailAccountIdRef.current !== null && selected.has(detailAccountIdRef.current)) {
        closeDetailAccount();
      }
      await finishBatch(true);
    } catch (batchError) {
      showToast(t("accounts.batchDeleteFailed", { error: getErrorMessage(batchError) }), "error");
    } finally {
      setBatchOperation(null);
    }
  };

  // 范围:勾选 → 当前筛选(确认数量)。必须显式传 ids 或 selector,否则后端会跨渠道。
  const handleBatchTest = async (scope: "selected" | "filtered") => {
    if (batchBusy) return;
    let body: { ids: number[] } | { selector: AccountOperationSelector };
    if (scope === "selected") {
      if (selectedIds.length === 0) return;
      body = { ids: selectedIds };
    } else {
      if (totalAccounts === 0) return;
      const confirmed = await confirm({
        title: t("antigravity.batchTestFilteredTitle"),
        description: t("antigravity.batchTestFilteredDescription", { count: totalAccounts }),
        confirmText: t("accounts.batchTest"),
        tone: "warning",
      });
      if (!confirmed) return;
      body = { selector: currentSelector };
    }
    setBatchOperation("test");
    try {
      const result = await runStreamingOperation(
        "/accounts/batch-test?stream=true",
        body,
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
      await finishBatch(false);
    } catch (batchError) {
      showToast(t("accounts.batchTestFailed", { error: getErrorMessage(batchError) }), "error");
    } finally {
      setBatchOperation(null);
    }
  };

  // 同步 = 控制面完整同步(换票 + 项目 + 权限 + 配额)。API Key 号没有控制面,跳过。
  const handleBatchSync = async (scope: "selected" | "filtered") => {
    if (batchBusy) return;
    let body: { ids: number[] } | { selector: AccountOperationSelector };
    if (scope === "selected") {
      const ids = selectedIds.filter((id) => !selectedAPIKeyIds.includes(id));
      if (ids.length === 0) {
        showToast(t("antigravity.batchSyncNoOAuth"), "warning");
        return;
      }
      body = { ids };
    } else {
      if (authFilter === "api_key") {
        showToast(t("antigravity.batchSyncNoOAuth"), "warning");
        return;
      }
      // 只有"仅 OAuth"或未筛选时才能给出确切的 OAuth 数量;其余情况提示 API Key 会被跳过。
      const oauthCount =
        authFilter === "oauth" ? totalAccounts : !filtersActive && serverSummary ? serverSummary.oauth : null;
      const confirmed = await confirm({
        title: t("antigravity.batchSyncFilteredTitle"),
        description:
          oauthCount !== null
            ? t("antigravity.batchSyncFilteredDescription", { count: oauthCount })
            : t("antigravity.batchSyncFilteredDescriptionMixed", { count: totalAccounts }),
        confirmText: t("antigravity.batchSync"),
        tone: "warning",
      });
      if (!confirmed) return;
      body = { selector: { ...currentSelector, auth_kind: "oauth" } };
    }
    setBatchOperation("sync");
    try {
      const result = await runStreamingOperation(
        "/accounts/batch-refresh?stream=true",
        body,
        t("antigravity.batchSyncProgressTitle"),
      );
      showToast(
        t("antigravity.batchSyncDone", { success: result?.success ?? 0, fail: result?.failed ?? 0 }),
        (result?.failed ?? 0) > 0 ? "warning" : "success",
      );
      await finishBatch(scope === "selected");
    } catch (batchError) {
      showToast(t("antigravity.batchSyncFailed", { error: getErrorMessage(batchError) }), "error");
    } finally {
      setBatchOperation(null);
    }
  };

  // 清理异常 = 按列表口径(封禁 + 错误)选号后批量删除,删除进回收站可恢复。
  const handleCleanAbnormal = async () => {
    const count = serverSummary?.abnormal ?? 0;
    if (batchBusy || count === 0) return;
    const confirmed = await confirm({
      title: t("antigravity.cleanAbnormalTitle"),
      description: t("antigravity.cleanAbnormalDescription", {
        count,
        banned: serverSummary?.banned ?? 0,
        error: serverSummary?.error ?? 0,
      }),
      confirmText: t("antigravity.cleanAbnormalConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchOperation("clean");
    try {
      const result = await runStreamingOperation(
        "/accounts/batch-delete?stream=true",
        { selector: { channel: "antigravity", status: "abnormal" } },
        t("antigravity.cleanAbnormalProgressTitle"),
      );
      showToast(
        t("accounts.batchDeleteDone", {
          success: result?.deleted ?? result?.success ?? 0,
          fail: result?.failed ?? 0,
        }),
        (result?.failed ?? 0) > 0 ? "warning" : "success",
      );
      closeDetailAccount();
      await finishBatch(true);
    } catch (batchError) {
      showToast(t("accounts.batchDeleteFailed", { error: getErrorMessage(batchError) }), "error");
    } finally {
      setBatchOperation(null);
    }
  };

  // ---------- 派生展示 ----------
  const stats = {
    total: serverSummary?.total ?? totalAccounts,
    active: serverSummary?.active ?? 0,
    rateLimited: serverSummary?.rate_limited ?? 0,
    abnormal: serverSummary?.abnormal ?? 0,
    banned: serverSummary?.banned ?? 0,
    error: serverSummary?.error ?? 0,
    disabled: serverSummary?.disabled ?? 0,
    locked: serverSummary?.locked ?? 0,
    oauth: serverSummary?.oauth ?? 0,
    apiKey: serverSummary?.api_key ?? 0,
  };
  const poolEmpty = stats.total === 0 && !filtersActive;

  const statusChips: Array<{ id: StatusFilter; label: string; count: number }> = [
    { id: "all", label: t("antigravity.statAll"), count: stats.total },
    { id: "active", label: t("antigravity.statSchedulable"), count: stats.active },
    { id: "rate_limited", label: t("antigravity.statRateLimited"), count: stats.rateLimited },
    { id: "abnormal", label: t("antigravity.statAbnormal"), count: stats.abnormal },
    { id: "disabled", label: t("antigravity.statDisabled"), count: stats.disabled },
    { id: "locked", label: t("antigravity.statLocked"), count: stats.locked },
  ];
  const authTabs: Array<{ id: AuthFilter; label: string; count?: number }> = [
    { id: "all", label: t("antigravity.authAll") },
    { id: "oauth", label: t("antigravity.authKindOAuth"), count: stats.oauth },
    { id: "api_key", label: t("antigravity.authKindApiKey"), count: stats.apiKey },
  ];
  const sortOptions: Array<{ value: SortKey; label: string }> = [
    { value: "updated_at", label: t("antigravity.sortUpdated") },
    { value: "usage", label: t("antigravity.sortQuotaUsage") },
    { value: "today", label: t("antigravity.sortToday") },
    { value: "requests", label: t("antigravity.sortRequests") },
    { value: "created_at", label: t("accounts.importTime") },
    { value: "id", label: t("accounts.idColumn") },
    { value: "group", label: t("antigravity.sortGroup") },
  ];
  const sortLabel = sortOptions.find((option) => option.value === sortKey)?.label ?? sortKey;
  const statusLabel = statusChips.find((chip) => chip.id === statusFilter)?.label ?? statusFilter;

  const manageSections: HeaderActionMenuSection[] = [
    {
      key: "maintenance",
      label: t("accounts.maintenanceActions"),
      items: [
        {
          key: "sync-filtered",
          label: filtersActive ? t("antigravity.syncFiltered") : t("antigravity.syncAll"),
          icon: <RotateCw className="size-3.5" />,
          disabled: batchBusy || totalAccounts === 0,
          onSelect: () => void handleBatchSync("filtered"),
        },
        {
          key: "test-filtered",
          label: filtersActive ? t("antigravity.testFiltered") : t("antigravity.testAll"),
          icon: <FlaskConical className="size-3.5" />,
          disabled: batchBusy || totalAccounts === 0,
          onSelect: () => void handleBatchTest("filtered"),
        },
        {
          key: "clean-abnormal",
          label: t("antigravity.cleanAbnormal", { count: stats.abnormal }),
          icon: <Trash2 className="size-3.5" />,
          destructive: true,
          disabled: batchBusy || stats.abnormal === 0,
          onSelect: () => void handleCleanAbnormal(),
        },
      ],
    },
    {
      key: "data",
      label: t("accounts.dataActions"),
      items: [
        {
          key: "groups",
          label: t("accounts.groupManage"),
          icon: <FolderOpen className="size-3.5" />,
          onSelect: () => setManageGroupsOpen(true),
        },
        {
          key: "export-all",
          label: t("antigravity.exportAll"),
          icon: <Download className="size-3.5" />,
          disabled: exporting || stats.total === 0,
          onSelect: () => void handleExport(),
        },
      ],
    },
  ];

  const renderSortHead = (key: SortKey, label: string, title?: string) => (
    <TableHead
      aria-sort={sortKey === key ? (sortDir === "desc" ? "descending" : "ascending") : "none"}
      title={title}
    >
      <button
        type="button"
        onClick={() => toggleSort(key)}
        className={cn(
          "group inline-flex items-center gap-1.5 rounded py-1 text-[13px] font-semibold outline-none transition-colors hover:text-primary focus-visible:ring-2 focus-visible:ring-ring/50",
          sortKey === key && "text-primary",
        )}
      >
        {label}
        {sortKey === key ? (
          sortDir === "desc" ? <ArrowDown className="size-3.5" /> : <ArrowUp className="size-3.5" />
        ) : (
          <ArrowUpDown className="size-3 text-muted-foreground/35 group-hover:text-primary/70" />
        )}
      </button>
    </TableHead>
  );

  const rowProps = (account: AccountRow, index: number): AntigravityRowProps => ({
    account,
    sequence: (page - 1) * pageSize + index + 1,
    selected: selected.has(account.id),
    detailOpen: detailAccountId === account.id,
    columns: visibleColumns,
    groups: allGroups,
    healthBuckets: healthBars[String(account.id)],
    proxyCtx: proxyBindingCtx,
    busyAction: busy?.id === account.id ? busy.action : null,
    exporting,
    onToggleSelect: toggleSelect,
    onOpenDetail: (target) => openDetailAccount(target.id),
    onOpenQuota: (target) =>
      openDetailAccount(target.id, isAntigravityAPIKeyAccount(target) ? "sheet" : "quota"),
    onTest: setTestingAccount,
    onSync: handleSync,
    onEdit: setEditTarget,
    onUsage: setUsageAccount,
    onExport: (target) => void handleExport([target.id]),
    onToggleEnabled: handleToggleEnabled,
    onToggleLock: handleToggleLock,
    onResetStatus: handleResetStatus,
    onEditProxy: setQuickProxyAccount,
    onDelete: (target) => void handleDelete(target),
  });

  const refreshDetailAndList = useCallback(async () => {
    await reload({ silent: true });
  }, [reload]);

  return (
    <div className="relative @container/antigravity-accounts">
      <PageHeader
        title={t("antigravity.pageTitle")}
        description={t("antigravity.pageSubtitle")}
        hideTitle={Boolean(headerSlot)}
        actionsBelow
        titleAdornment={headerSlot}
        actions={
          <div className="flex w-full flex-wrap items-center gap-1.5 sm:gap-2 [&>button]:h-8">
            <Button size="sm" onClick={() => setShowAdd(true)}>
              <Plus className="size-3.5" />
              {t("antigravity.addAccount")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="size-8 p-0 md:w-auto md:px-2.5"
              title={t("antigravity.oauthAddAccount")}
              aria-label={t("antigravity.oauthAddAccount")}
              onClick={() => setShowOAuth(true)}
            >
              <Link2 className="size-3.5" />
              <span className="hidden md:inline">{t("antigravity.oauthAddAccount")}</span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="size-8 p-0 md:w-auto md:px-2.5"
              title={t("antigravity.importFiles")}
              aria-label={t("antigravity.importFiles")}
              onClick={() => setShowImport(true)}
            >
              <Upload className="size-3.5" />
              <span className="hidden md:inline">{t("antigravity.importFiles")}</span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="size-8 p-0 md:w-auto md:px-2.5"
              title={selectedIds.length > 0 ? t("antigravity.exportSelected") : t("antigravity.exportAll")}
              aria-label={selectedIds.length > 0 ? t("antigravity.exportSelected") : t("antigravity.exportAll")}
              disabled={exporting || stats.total === 0}
              onClick={() => void handleExport(selectedIds.length > 0 ? selectedIds : undefined)}
            >
              {exporting ? <Loader2 className="size-3.5 animate-spin" /> : <Download className="size-3.5" />}
              <span className="hidden md:inline">
                {exporting
                  ? t("antigravity.exporting")
                  : selectedIds.length > 0
                    ? t("antigravity.exportSelectedCount", { count: selectedIds.length })
                    : t("antigravity.exportAll")}
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
              onClick={() => void reload()}
            >
              <RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
            </Button>
          </div>
        }
      />

      {error && accounts.length > 0 ? (
        <div className="mb-2 flex items-center justify-between gap-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive" role="alert">
          <span className="truncate">{error}</span>
          <Button variant="outline" size="sm" onClick={() => void reload()}>{t("common.retry")}</Button>
        </div>
      ) : null}

      {/* 统计卡:点击即按状态筛选;异常卡的明细列拆成封禁 / 错误。 */}
      <div className="mb-4 flex snap-x gap-2 overflow-x-auto pb-1 @4xl/antigravity-accounts:grid @4xl/antigravity-accounts:grid-cols-5 @4xl/antigravity-accounts:gap-3 [&>button]:w-40 [&>button]:shrink-0 [&>button]:snap-start @4xl/antigravity-accounts:[&>button]:w-full">
        <CompactStat
          label={t("antigravity.statTotal")}
          chipLabel={t("antigravity.statAll")}
          value={stats.total}
          tone="neutral"
          active={statusFilter === "all"}
          details={[
            { label: "OAuth", value: stats.oauth },
            { label: "Key", value: stats.apiKey },
          ]}
          onClick={() => setStatusFilter("all")}
        />
        <CompactStat
          label={t("antigravity.statActiveLong")}
          chipLabel={t("antigravity.statSchedulable")}
          value={stats.active}
          tone="success"
          active={statusFilter === "active"}
          onClick={() => setStatusFilter("active")}
        />
        <CompactStat
          label={t("antigravity.statRateLimitedLong")}
          chipLabel={t("antigravity.statRateLimited")}
          value={stats.rateLimited}
          tone="warning"
          active={statusFilter === "rate_limited"}
          onClick={() => setStatusFilter("rate_limited")}
        />
        <CompactStat
          label={t("antigravity.statAbnormalLong")}
          chipLabel={t("antigravity.statAbnormal")}
          value={stats.abnormal}
          tone="danger"
          active={statusFilter === "abnormal"}
          details={[
            { label: t("accounts.abnormalBannedShort"), value: stats.banned },
            { label: t("accounts.abnormalErrorShort"), value: stats.error },
          ]}
          onClick={() => setStatusFilter("abnormal")}
        />
        <CompactStat
          label={t("antigravity.statDisabledLong")}
          chipLabel={t("antigravity.statDisabled")}
          value={stats.disabled}
          tone="neutral"
          active={statusFilter === "disabled"}
          onClick={() => setStatusFilter("disabled")}
        />
      </div>

      {/* 工具栏:搜索 + 状态胶囊一行;认证方式 / 分组 / 排序一行(窄屏折叠);活动筛选药丸一行。 */}
      <div className="toolbar-surface mb-3 flex flex-col gap-2 px-3 py-2.5">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
          <div className="relative min-w-0 flex-1 @4xl/antigravity-accounts:w-60 @4xl/antigravity-accounts:flex-none @6xl/antigravity-accounts:w-72">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground/70" />
            <Input
              className="h-8 rounded-lg bg-background/70 pl-9 pr-8 text-[13px] shadow-none"
              placeholder={t("antigravity.searchPlaceholder")}
              aria-label={t("antigravity.searchPlaceholder")}
              value={searchQuery}
              onChange={(event) => setSearchQuery(event.target.value)}
            />
            {searchQuery ? (
              <button
                type="button"
                className="absolute right-1.5 top-1/2 flex size-6 -translate-y-1/2 items-center justify-center rounded-md text-muted-foreground hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                aria-label={t("antigravity.clearSearch")}
                onClick={() => setSearchQuery("")}
              >
                <X className="size-3.5" />
              </button>
            ) : null}
          </div>
          <div className="order-last flex w-full min-w-0 items-center gap-0.5 overflow-x-auto @4xl/antigravity-accounts:order-none @4xl/antigravity-accounts:w-auto @4xl/antigravity-accounts:flex-1">
            {statusChips.map((chip) => (
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
            aria-controls="antigravity-account-filters"
            onClick={() => setShowFilters((current) => !current)}
            className={cn("h-8 @4xl/antigravity-accounts:hidden", (showFilters || filtersActive) && "border-primary/25 bg-primary/5 text-primary")}
          >
            <SlidersHorizontal className="size-3.5" />
            {t("accounts.filter")}
          </Button>
          {isTableViewport ? (
            <div className="ml-auto shrink-0">
              <ColumnSettingsMenu
                columns={visibleColumns}
                columnOrder={ANTIGRAVITY_TABLE_COLUMNS}
                onToggle={toggleColumn}
                onReset={resetColumns}
                title={t("accounts.columnSettings")}
                resetTitle={t("accounts.columnReset")}
                labels={{
                  id: t("accounts.idColumn"),
                  tier: t("antigravity.columnTier"),
                  quota: t("antigravity.columnQuota"),
                  proxy: t("accounts.proxyColumn"),
                  status: t("antigravity.columnStatus"),
                  today: t("accounts.todayStats"),
                  requests: t("accounts.requests"),
                  updatedAt: t("antigravity.columnUpdated"),
                }}
              />
            </div>
          ) : null}
        </div>

        <div
          id="antigravity-account-filters"
          className={cn(
            "flex-wrap items-center gap-x-2 gap-y-1.5 border-t border-border/50 pt-2 @4xl/antigravity-accounts:flex",
            showFilters ? "flex" : "hidden",
          )}
        >
          <div className={segmentGroupClass} role="group" aria-label={t("antigravity.authKind")}>
            {authTabs.map((tab) => (
              <button
                key={tab.id}
                type="button"
                aria-pressed={authFilter === tab.id}
                onClick={() => setAuthFilter(tab.id)}
                className={segmentButtonClass(authFilter === tab.id)}
              >
                {typeof tab.count === "number" ? `${tab.label} ${tab.count}` : tab.label}
              </button>
            ))}
          </div>
          <AccountGroupFilterSelect
            className="min-w-0 w-full sm:w-44"
            groups={antigravityGroups}
            value={groupFilter}
            onChange={setGroupFilter}
          />
          <div className="flex min-w-0 items-center gap-1 sm:ml-auto">
            <span className="shrink-0 text-[11px] font-medium text-muted-foreground">{t("antigravity.sortLabel")}</span>
            <Select
              className="min-w-0 w-40"
              compact
              value={sortKey}
              onValueChange={(value) => applySort(value as SortKey)}
              options={sortOptions}
            />
            <Button
              variant="ghost"
              size="icon-sm"
              className="text-primary"
              title={sortDir === "desc" ? t("antigravity.sortAscending") : t("antigravity.sortDescending")}
              aria-label={sortDir === "desc" ? t("antigravity.sortAscending") : t("antigravity.sortDescending")}
              onClick={() => setSortDir((current) => (current === "desc" ? "asc" : "desc"))}
            >
              {sortDir === "desc" ? <ArrowDown className="size-3.5" /> : <ArrowUp className="size-3.5" />}
            </Button>
          </div>
        </div>

        {filtersActive || sortKey !== DEFAULT_SORT ? (
          <div className="flex flex-wrap items-center gap-1.5 border-t border-border/60 pt-2">
            {debouncedSearchQuery ? (
              <button type="button" onClick={() => setSearchQuery("")} className={filterPillClass} title={debouncedSearchQuery}>
                <Search className="size-3 shrink-0" />
                <span className="max-w-40 truncate">{debouncedSearchQuery}</span>
                <X className="size-3 shrink-0" />
              </button>
            ) : null}
            {statusFilter !== "all" ? (
              <button
                type="button"
                onClick={() => setStatusFilter("all")}
                className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2.5 py-1 text-[11px] font-medium text-primary transition-colors hover:bg-primary/15"
              >
                {statusLabel}
                <X className="size-3" />
              </button>
            ) : null}
            {authFilter !== "all" ? (
              <button type="button" onClick={() => setAuthFilter("all")} className={filterPillClass}>
                {authTabs.find((tab) => tab.id === authFilter)?.label ?? authFilter}
                <X className="size-3" />
              </button>
            ) : null}
            {!isAccountGroupFilterEmpty(groupFilter) ? (
              <button type="button" onClick={() => setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER)} className={filterPillClass}>
                {t("accounts.groupsLabel")}
                <X className="size-3" />
              </button>
            ) : null}
            {sortKey !== DEFAULT_SORT ? (
              <button
                type="button"
                onClick={() => applySort(DEFAULT_SORT)}
                className={filterPillClass}
                title={t("antigravity.sortReset")}
              >
                {sortDir === "desc" ? <ArrowDown className="size-3" /> : <ArrowUp className="size-3" />}
                {sortLabel}
                <X className="size-3" />
              </button>
            ) : null}
            {filtersActive ? (
              <button
                type="button"
                onClick={clearFilters}
                className="ml-auto text-[11px] font-medium text-muted-foreground transition-colors hover:text-foreground"
              >
                {t("accounts.clearFilters")}
              </button>
            ) : null}
          </div>
        ) : null}
      </div>

      {/* 批量操作条(与其他渠道同款 sticky 玻璃条) */}
      {selectedIds.length > 0 ? (
        <div className="sticky top-2 z-20 mb-4 flex items-center justify-between gap-3 rounded-xl border border-primary/20 bg-card/95 px-3 py-2 text-sm shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
          <span className="inline-flex items-center gap-2 font-semibold text-primary">
            {batchBusy ? <Loader2 className="size-3.5 animate-spin" /> : null}
            {t("common.selected", { count: selectedIds.length })}
          </span>
          <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
            {!allPageSelected ? (
              <Button variant="outline" size="sm" disabled={batchBusy} onClick={toggleSelectAll}>
                <ListChecks className="size-3.5" />
                <span>{t("accounts.selectCurrentPage")}</span>
              </Button>
            ) : null}
            <Button variant="outline" size="sm" disabled={batchBusy} onClick={() => void handleBatchSync("selected")}>
              <RotateCw className={cn("size-3.5", batchOperation === "sync" && "animate-spin")} />
              <span className="hidden sm:inline">{t("antigravity.batchSync")}</span>
            </Button>
            <Button variant="outline" size="sm" disabled={batchBusy} onClick={() => void handleBatchTest("selected")}>
              <FlaskConical className={cn("size-3.5", batchOperation === "test" && "animate-pulse")} />
              <span className="hidden sm:inline">
                {batchOperation === "test" ? t("accounts.batchTesting") : t("accounts.batchTest")}
              </span>
            </Button>
            <Button variant="outline" size="sm" disabled={batchBusy} onClick={() => setBatchGroupOpen(true)}>
              <FolderOpen className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.batchGroupEdit")}</span>
            </Button>
            <Button variant="outline" size="sm" disabled={batchBusy} onClick={() => void handleBatchUpdate({ enabled: true }, "accounts.batchEnableDone")}>
              <Power className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.enable")}</span>
            </Button>
            <Button variant="outline" size="sm" disabled={batchBusy} onClick={() => void handleBatchUpdate({ enabled: false }, "accounts.batchDisableDone")}>
              <PowerOff className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.disable")}</span>
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="text-destructive hover:bg-destructive/10 hover:text-destructive"
              disabled={batchBusy}
              onClick={() => void handleBatchDelete()}
            >
              <Trash2 className="size-3.5" />
              <span className="hidden sm:inline">{t("accounts.batchDelete")}</span>
            </Button>
            <HeaderActionMenu
              label={t("accounts.batchMore")}
              icon={<MoreHorizontal className="size-3.5" />}
              align="end"
              compact
              items={[
                {
                  key: "export",
                  label: t("antigravity.exportSelectedCount", { count: selectedIds.length }),
                  icon: <Download className="size-3.5" />,
                  disabled: exporting || batchBusy,
                  onSelect: () => void handleExport(selectedIds),
                },
                {
                  key: "lock",
                  label: t("accounts.lock"),
                  icon: <Lock className="size-3.5" />,
                  disabled: batchBusy,
                  onSelect: () => void handleBatchUpdate({ locked: true }, "accounts.batchLockDone"),
                },
                {
                  key: "unlock",
                  label: t("accounts.unlock"),
                  icon: <Unlock className="size-3.5" />,
                  disabled: batchBusy,
                  onSelect: () => void handleBatchUpdate({ locked: false }, "accounts.batchUnlockDone"),
                },
                {
                  key: "reset",
                  label: t("accounts.batchResetStatus"),
                  icon: <RotateCcw className="size-3.5" />,
                  disabled: batchBusy,
                  onSelect: () => void handleBatchResetStatus(),
                },
                {
                  key: "quota-clear",
                  label: t("antigravityQuota.clearAction"),
                  icon: <Eraser className="size-3.5" />,
                  disabled: batchBusy,
                  onSelect: () => void handleBatchClearQuota(),
                },
                {
                  key: "models",
                  label: t("antigravity.batchModels"),
                  icon: <Layers className="size-3.5" />,
                  disabled: batchBusy || selectedAPIKeyIds.length === 0,
                  title: selectedAPIKeyIds.length === 0 ? t("antigravity.batchModelsNeedsApiKey") : undefined,
                  onSelect: () => setBatchModelsOpen(true),
                },
              ]}
            />
            <Button variant="ghost" size="sm" disabled={batchBusy} onClick={clearSelection}>
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
            error={accounts.length === 0 ? error : null}
            onRetry={() => void reload()}
            isEmpty={!loading && !error && accounts.length === 0}
            emptyIcon={<ChannelLogo channel="antigravity" size={30} />}
            loadingTitle={t("antigravity.loadingTitle")}
            loadingDescription={t("antigravity.loadingDescription")}
            errorTitle={t("antigravity.errorTitle")}
            emptyTitle={poolEmpty ? t("antigravity.emptyTitle") : t("antigravity.noMatchesTitle")}
            emptyDescription={poolEmpty ? t("antigravity.emptyDescription") : t("antigravity.noMatchesDescription")}
            action={
              poolEmpty ? (
                <div className="flex flex-wrap items-center justify-center gap-2">
                  <Button onClick={() => setShowAdd(true)}>
                    <Plus className="size-4" />
                    {t("antigravity.addAccount")}
                  </Button>
                  <Button variant="outline" onClick={() => setShowOAuth(true)}>
                    <Link2 className="size-4" />
                    {t("antigravity.oauthAddAccount")}
                  </Button>
                  <Button variant="outline" onClick={() => setShowImport(true)}>
                    <Upload className="size-4" />
                    {t("antigravity.importFiles")}
                  </Button>
                </div>
              ) : (
                <Button variant="outline" onClick={clearFilters}>
                  {t("antigravity.clearFilters")}
                </Button>
              )
            }
          >
            {isTableViewport ? (
              <div className={cn("data-table-shell", accounts.length <= pageSize && "account-table-shell-fit-content")}>
                <Table className="[&_td]:px-2.5 [&_th]:px-2.5 [&_td]:py-3">
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
                      <TableHead className="w-12 text-[13px] font-semibold">{t("accounts.sequence")}</TableHead>
                      {visibleColumns.id ? renderSortHead("id", t("accounts.idColumn")) : null}
                      <TableHead className="text-[13px] font-semibold">{t("antigravity.columnAccount")}</TableHead>
                      {visibleColumns.tier ? (
                        <TableHead className="text-[13px] font-semibold">{t("antigravity.columnTier")}</TableHead>
                      ) : null}
                      {visibleColumns.quota ? renderSortHead("usage", t("antigravity.columnQuota"), t("antigravity.sortQuotaUsageHint")) : null}
                      {visibleColumns.proxy ? (
                        <TableHead className="text-[13px] font-semibold">{t("accounts.proxyColumn")}</TableHead>
                      ) : null}
                      {visibleColumns.status ? (
                        <TableHead className="text-[13px] font-semibold">{t("antigravity.columnStatus")}</TableHead>
                      ) : null}
                      {visibleColumns.today ? renderSortHead("today", t("accounts.todayStats"), t("accounts.todayStatsHint")) : null}
                      {visibleColumns.requests ? renderSortHead("requests", t("accounts.requests")) : null}
                      {visibleColumns.updatedAt ? renderSortHead("updated_at", t("antigravity.columnUpdated")) : null}
                      <TableHead data-account-actions className="text-right text-[13px] font-semibold">
                        {t("antigravity.columnActions")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {displayRows.map((account, index) => (
                      <AntigravityAccountRow key={account.id} {...rowProps(account, index)} />
                    ))}
                  </TableBody>
                </Table>
              </div>
            ) : (
              <div className="grid gap-2">
                {displayRows.map((account, index) => (
                  <AntigravityAccountCard key={account.id} {...rowProps(account, index)} />
                ))}
              </div>
            )}

            <Pagination
              page={page}
              totalPages={totalPages}
              onPageChange={setPage}
              totalItems={totalAccounts}
              pageSize={pageSize}
              onPageSizeChange={(next) => {
                setPageSize(next);
                setPage(1);
              }}
              pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
            />
          </StateShell>
        </CardContent>
      </Card>

      <AntigravityAddModal
        show={showAdd}
        proxies={proxyPool}
        groups={antigravityGroups}
        onCreateGroup={createAntigravityGroup}
        onClose={() => setShowAdd(false)}
        onAdded={async () => {
          setPage(1);
          await reload({ silent: true });
        }}
      />

      <AntigravityImportModal
        show={showImport}
        proxies={proxyPool}
        groups={antigravityGroups}
        onCreateGroup={createAntigravityGroup}
        onClose={() => setShowImport(false)}
        onImported={async () => {
          setPage(1);
          await reload({ silent: true });
        }}
      />

      {showOAuth ? (
        <AntigravityOAuthModal
          proxies={proxyPool}
          groups={antigravityGroups}
          onCreateGroup={createAntigravityGroup}
          onClose={() => setShowOAuth(false)}
          onCompleted={async () => {
            setPage(1);
            await reload({ silent: true });
          }}
        />
      ) : null}

      <AntigravityEditModal
        account={editTarget}
        proxies={proxyPool}
        groups={antigravityGroups}
        onCreateGroup={createAntigravityGroup}
        onClose={() => setEditTarget(null)}
        onSaved={refreshDetailAndList}
      />

      <AntigravityBatchModelsModal
        show={batchModelsOpen}
        ids={selectedAPIKeyIds}
        skipped={selectedIds.length - selectedAPIKeyIds.length}
        onClose={() => setBatchModelsOpen(false)}
        onSaved={() => finishBatch(true)}
      />

      <BatchAccountGroupModal
        show={batchGroupOpen}
        ids={selectedIds}
        channel="antigravity"
        groups={antigravityGroups}
        onClose={() => setBatchGroupOpen(false)}
        onSaved={async () => {
          setBatchGroupOpen(false);
          clearSelection();
          await Promise.all([reload(), reloadGroups()]);
        }}
        onGroupsChanged={reloadGroups}
      />

      {manageGroupsOpen ? (
        <AccountGroupManagerModal
          channel="antigravity"
          groups={antigravityGroups}
          title={t("antigravity.manageGroups")}
          onClose={() => setManageGroupsOpen(false)}
          onChanged={() => {
            void reloadGroups();
            void reload({ silent: true });
          }}
        />
      ) : null}

      {/* 代理徽章直达的快速绑定弹窗:与其他渠道账号页共用组件 */}
      <AccountProxyQuickEditor
        account={quickProxyAccount}
        accountLabel={quickProxyAccount ? accountLabel(quickProxyAccount) : ""}
        proxies={proxyPool}
        ctx={proxyBindingCtx}
        onClose={() => setQuickProxyAccount(null)}
        onSaved={() => reload({ silent: true })}
      />

      {testingAccount ? (
        <TestConnectionModal
          account={testingAccount}
          onClose={() => setTestingAccount(null)}
          onSettled={() => void reload({ silent: true })}
        />
      ) : null}

      {usageAccount ? (
        <AccountUsageModal
          account={usageAccount}
          onClose={() => setUsageAccount(null)}
          showCreditSettings={false}
          officialUsage={false}
        />
      ) : null}

      <AntigravityQuotaDetailModal
        account={detailView === "quota" ? detailAccount : null}
        state={managementState}
        loading={managementLoading}
        error={managementError}
        action={managementAction}
        onSync={() => void handleStateSync()}
        onProbe={() => void handleCapabilityProbe()}
        onClose={closeDetailAccount}
        onOpenManage={() => setDetailView("sheet")}
      />

      <AccountDetailSheet
        account={detailView === "sheet" ? detailAccount : null}
        groups={
          detailAccount
            ? ((detailAccount.group_ids ?? [])
                .map((id) => allGroups.find((group) => group.id === id))
                .filter(Boolean) as AccountGroup[])
            : []
        }
        healthBuckets={detailAccount ? healthBars[String(detailAccount.id)] : undefined}
        sequence={detailNavIndex >= 0 ? (page - 1) * pageSize + detailNavIndex + 1 : undefined}
        usageSlot={detailAccount ? <AntigravityQuotaOverview account={detailAccount} /> : null}
        providerSlot={
          detailAccount ? (
            <AntigravityProviderPanel
              account={detailAccount}
              state={managementState}
              loading={managementLoading}
              error={managementError}
              action={managementAction}
              onSync={() => void handleStateSync()}
              onProbe={() => void handleCapabilityProbe()}
              onOpenFullDetail={() => setDetailView("quota")}
            />
          ) : null
        }
        canGoPrev={detailNavIndex > 0 || page > 1}
        canGoNext={(detailNavIndex >= 0 && detailNavIndex < displayRows.length - 1) || page < totalPages}
        refreshing={detailAccount ? busy?.id === detailAccount.id && busy.action === "sync" : false}
        onClose={closeDetailAccount}
        onPrev={goDetailPrev}
        onNext={goDetailNext}
        onEdit={() => {
          if (detailAccount) setEditTarget(detailAccount);
        }}
        onUsage={() => {
          if (detailAccount) setUsageAccount(detailAccount);
        }}
        onTest={() => {
          if (detailAccount) setTestingAccount(detailAccount);
        }}
        onRefresh={() => {
          if (detailAccount) handleSync(detailAccount);
        }}
        onGenerateAuthJson={() => {
          if (detailAccount) void handleExport([detailAccount.id]);
        }}
        onToggleEnabled={() => {
          if (detailAccount) handleToggleEnabled(detailAccount);
        }}
        onToggleLock={() => {
          if (detailAccount) handleToggleLock(detailAccount);
        }}
        onResetStatus={() => {
          if (detailAccount) handleResetStatus(detailAccount);
        }}
        onSaveModelCooldownPolicy={(data) => {
          if (!detailAccount) return;
          void api
            .updateAccountModelCooldownPolicy(detailAccount.id, data)
            .then(() => {
              showToast(t("accounts.modelCooldownPolicySaved"));
              return reload({ silent: true });
            })
            .catch((saveError) =>
              showToast(t("accounts.modelCooldownPolicySaveFailed", { error: getErrorMessage(saveError) }), "error"),
            );
        }}
        onClearModelCooldown={(model) => {
          if (!detailAccount) return;
          void api
            .clearAccountModelCooldown(detailAccount.id, model)
            .then(() => {
              showToast(t("accounts.modelCooldownCleared", { model }));
              return reload({ silent: true });
            })
            .catch((clearError) => showToast(getErrorMessage(clearError), "error"));
        }}
        onClearAllModelCooldowns={() => {
          if (!detailAccount) return;
          void api
            .clearAllAccountModelCooldowns(detailAccount.id)
            .then((result) => {
              showToast(t("accounts.allModelCooldownsCleared", { count: result.cleared ?? 0 }));
              return reload({ silent: true });
            })
            .catch((clearError) => showToast(getErrorMessage(clearError), "error"));
        }}
        onResetCredits={() => undefined}
        onDelete={() => {
          if (detailAccount) void handleDelete(detailAccount);
        }}
      />

      <OperationProgressToast progress={operationProgress} onClose={closeOperationProgress} />
      <OperationResultsModal
        state={showOperationResults ? operationResults : null}
        accounts={displayRows}
        channel="antigravity"
        onClose={closeOperationResults}
      />

      {confirmDialog}
    </div>
  );
}

export default memo(AntigravityAccounts);
