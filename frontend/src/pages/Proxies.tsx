import { useState, useEffect, useCallback, useMemo } from "react";
import { useTranslation } from "react-i18next";
import {
  Globe,
  Plus,
  Trash2,
  Play,
  MapPin,
  Loader2,
  Zap,
  ChevronLeft,
  ChevronRight,
  Eye,
  EyeOff,
  AlertTriangle,
  Pencil,
} from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, type ProxyRow, type ProxyTestResult } from "../api";
import Modal from "../components/Modal";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";

const PAGE_SIZE = 10;
const TEST_ALL_CONCURRENCY = 4;

const PROXY_SCHEMES = ["http:", "https:", "socks5:", "socks5h:"];

function validateProxyInput(url: string): boolean {
  const trimmed = url.trim();
  if (!trimmed) return false;
  // 优先用 URL 严格解析（覆盖 IPv6 等）。
  try {
    const parsed = new URL(trimmed);
    if (Boolean(parsed.hostname) && PROXY_SCHEMES.includes(parsed.protocol)) {
      return true;
    }
  } catch {
    /* 落到下方宽松校验 */
  }
  // 宽松结构校验：密码可能含 # / ? @ 等特殊字符，会让 new URL 抛错（issue #293）。
  // 形如 scheme://[user[:pass]@]host[:port]，userinfo 贪婪匹配到最后一个 '@'。
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

export default function Proxies() {
  const { t, i18n } = useTranslation();
  const { toast, showToast } = useToast();
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
  const [page, setPage] = useState(1);
  const [revealedIds, setRevealedIds] = useState<Set<number>>(new Set());
  const [editingProxy, setEditingProxy] = useState<ProxyRow | null>(null);
  const [editUrl, setEditUrl] = useState("");
  const [editLabel, setEditLabel] = useState("");
  const [editSaving, setEditSaving] = useState(false);
  const [editError, setEditError] = useState("");

  const ipApiLang = i18n.language?.startsWith("zh") ? "zh-CN" : "en";

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

  const totalPages = Math.max(1, Math.ceil(proxies.length / PAGE_SIZE));
  const pagedProxies = proxies.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE);

  useEffect(() => {
    if (page > totalPages) setPage(totalPages);
  }, [page, totalPages]);

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
    setTestingIds((prev) => new Set(prev).add(p.id));
    try {
      const result = await api.testProxy(p.url, p.id, ipApiLang);
      if (!result.success) {
        setProxies((prev) =>
          prev.map((px) =>
            px.id === p.id
              ? { ...px, test_ip: "", test_location: "", test_latency_ms: 0 }
              : px,
          ),
        );
        showToast(
          t("proxies.testFailed", {
            error: result.error || t("proxies.testFailedUnknown"),
          }),
          "error",
        );
      } else {
        setProxies((prev) =>
          prev.map((px) =>
            px.id === p.id
              ? {
                  ...px,
                  test_ip: result.ip || "",
                  test_location: result.location || "",
                  test_latency_ms: result.latency_ms || 0,
                }
              : px,
          ),
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
    setTestAllLoading(true);
    setTestAllDone(0);
    setTestAllFailed(0);
    let failedCount = 0;
    let firstError = "";
    let nextIndex = 0;
    const queue = [...proxies];
    const testOne = async (p: ProxyRow) => {
      setTestingIds((prev) => new Set(prev).add(p.id));
      try {
        const result = await api.testProxy(p.url, p.id, ipApiLang);
        if (!result.success) {
          failedCount += 1;
          setTestAllFailed(failedCount);
          if (!firstError)
            firstError = result.error || t("proxies.testFailedUnknown");
          setProxies((prev) =>
            prev.map((px) =>
              px.id === p.id
                ? { ...px, test_ip: "", test_location: "", test_latency_ms: 0 }
                : px,
            ),
          );
        } else {
          setProxies((prev) =>
            prev.map((px) =>
              px.id === p.id
                ? {
                    ...px,
                    test_ip: result.ip || "",
                    test_location: result.location || "",
                    test_latency_ms: result.latency_ms || 0,
                  }
                : px,
            ),
          );
        }
      } catch (error) {
        failedCount += 1;
        setTestAllFailed(failedCount);
        if (!firstError) firstError = getErrorMessage(error);
      } finally {
        setTestAllDone((prev) => prev + 1);
        setTestingIds((prev) => {
          const next = new Set(prev);
          next.delete(p.id);
          return next;
        });
      }
    };

    const worker = async () => {
      for (;;) {
        const current = nextIndex;
        nextIndex += 1;
        const proxy = queue[current];
        if (!proxy) return;
        await testOne(proxy);
      }
    };

    await Promise.all(
      Array.from(
        { length: Math.min(TEST_ALL_CONCURRENCY, queue.length) },
        worker,
      ),
    );
    if (failedCount > 0) {
      showToast(
        t("proxies.testAllFailed", { count: failedCount, error: firstError }),
        "error",
      );
    }
    setTestAllLoading(false);
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

  const enabledCount = proxies.filter((p) => p.enabled).length;
  const canEnable = enabledCount > 0;

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <h2 className="text-2xl font-bold text-foreground flex items-center gap-2.5">
            <Globe className="size-6 text-primary" />
            {t("nav.proxies")}
          </h2>
          <p className="mt-1 text-sm text-muted-foreground">
            {t("proxies.description")}
          </p>
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          {/* Pool Toggle Switch */}
          <div
            className="flex items-center gap-3"
            title={
              !canEnable && !poolEnabled
                ? t("proxies.addFirstProxy")
                : undefined
            }
          >
            <span
              className={`text-sm font-medium ${poolEnabled ? "text-emerald-600 dark:text-emerald-400" : "text-muted-foreground"}`}
            >
              {poolEnabled
                ? t("proxies.poolEnabled")
                : t("proxies.poolDisabled")}
            </span>
            <button
              role="switch"
              aria-checked={poolEnabled}
              disabled={!canEnable && !poolEnabled}
              onClick={handleTogglePool}
              className={`relative inline-flex h-6 w-11 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/50 disabled:cursor-not-allowed disabled:opacity-40 ${
                poolEnabled ? "bg-emerald-500" : "bg-muted-foreground/30"
              }`}
            >
              <span
                className={`pointer-events-none inline-block size-5 transform rounded-full bg-white shadow-lg ring-0 transition-transform duration-200 ${poolEnabled ? "translate-x-5" : "translate-x-0"}`}
              />
            </button>
          </div>

          {selected.size > 0 && (
            <button
              onClick={handleBatchDelete}
              className="flex items-center gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm font-semibold text-destructive transition-colors hover:bg-destructive/20"
            >
              <Trash2 className="size-4" />
              {t("proxies.deleteSelected", { count: selected.size })}
            </button>
          )}

          {proxies.length > 0 && (
            <button
              onClick={handleTestAll}
              disabled={testAllLoading}
              className="flex items-center gap-2 rounded-md border border-border px-3 py-2 text-sm font-semibold text-foreground transition-colors hover:bg-muted/50 disabled:opacity-50"
            >
              {testAllLoading ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <Zap className="size-4" />
              )}
              {testAllLoading
                ? t("proxies.testingAllProgress", {
                    current: testAllDone,
                    total: proxies.length,
                    failed: testAllFailed,
                  })
                : t("proxies.testAll")}
            </button>
          )}

          <button
            onClick={() => setShowAdd(!showAdd)}
            className="flex items-center gap-2 rounded-md bg-primary px-3 py-2 text-sm font-semibold text-primary-foreground shadow-sm transition-colors hover:bg-primary/90"
          >
            <Plus className="size-4" />
            {t("proxies.addProxy")}
          </button>
        </div>
      </div>

      {/* Add Panel */}
      {showAdd && (
        <Card className="py-0">
          <CardContent className="p-6 space-y-4">
            <h4 className="text-base font-semibold text-foreground">
              {t("proxies.addProxyTitle")}
            </h4>
            <p className="text-sm text-muted-foreground">
              {t("proxies.addProxyDesc")}
            </p>
            <textarea
              value={addInput}
              onChange={(e) => setAddInput(e.target.value)}
              placeholder={"http://user:pass@ip:port\nsocks5://ip:port"}
              className="w-full h-32 px-3 py-2 text-sm rounded-md border border-border bg-background text-foreground placeholder:text-muted-foreground resize-none outline-none focus:ring-2 focus:ring-primary/30 font-mono"
            />
            <div className="flex items-center gap-3">
              <input
                type="text"
                value={addLabel}
                onChange={(e) => setAddLabel(e.target.value)}
                placeholder={t("proxies.labelPlaceholder")}
                className="flex-1 px-3 py-2 text-sm rounded-md border border-border bg-background text-foreground placeholder:text-muted-foreground outline-none focus:ring-2 focus:ring-primary/30"
              />
              <button
                onClick={handleAdd}
                disabled={addLoading || !addInput.trim()}
                className="px-5 py-2 rounded-md text-sm font-semibold bg-primary text-primary-foreground hover:bg-primary/90 transition-colors disabled:opacity-50 shadow-sm"
              >
                {addLoading ? t("proxies.adding") : t("proxies.confirmAdd")}
              </button>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Stats */}
      <div className="grid grid-cols-1 gap-3 min-[420px]:grid-cols-3 sm:gap-4">
        <Card className="py-0">
          <CardContent className="p-4 text-center">
            <div className="text-2xl font-bold tabular-nums text-foreground">
              {proxies.length}
            </div>
            <div className="text-xs text-muted-foreground mt-1">
              {t("proxies.totalProxies")}
            </div>
          </CardContent>
        </Card>
        <Card className="py-0">
          <CardContent className="p-4 text-center">
            <div className="text-2xl font-bold tabular-nums text-emerald-600 dark:text-emerald-400">
              {enabledCount}
            </div>
            <div className="text-xs text-muted-foreground mt-1">
              {t("proxies.enabledCount")}
            </div>
          </CardContent>
        </Card>
        <Card className="py-0">
          <CardContent className="p-4 text-center">
            <div
              className={`text-2xl font-bold ${poolEnabled ? "text-emerald-600 dark:text-emerald-400" : "text-muted-foreground"}`}
            >
              {poolEnabled ? t("proxies.roundRobin") : t("proxies.off")}
            </div>
            <div className="text-xs text-muted-foreground mt-1">
              {t("proxies.poolStatus")}
            </div>
          </CardContent>
        </Card>
      </div>

      {/* Table */}
      <Card className="py-0">
        <CardContent className="p-0">
          {loading ? (
            <div className="flex justify-center items-center py-16">
              <Loader2 className="size-6 animate-spin text-primary" />
            </div>
          ) : proxies.length === 0 ? (
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
                            <button
                              onClick={() => {
                                setRevealedIds((prev) => {
                                  const next = new Set(prev);
                                  if (next.has(p.id)) next.delete(p.id);
                                  else next.add(p.id);
                                  return next;
                                });
                              }}
                              className="flex size-9 shrink-0 items-center justify-center rounded-lg text-muted-foreground hover:bg-muted/50 hover:text-foreground"
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
                            </button>
                            <span className="min-w-0 flex-1 break-all font-mono text-[12px] font-medium leading-relaxed text-foreground">
                              {revealedIds.has(p.id) ? p.url : maskUrl(p.url)}
                            </span>
                          </div>

                          <div className="mt-2.5 flex flex-wrap items-center gap-2">
                            <button
                              onClick={() => handleToggle(p)}
                              className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-semibold transition-all ${
                                p.enabled
                                  ? "border-emerald-500/20 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
                                  : "border-border bg-muted/50 text-muted-foreground"
                              }`}
                            >
                              <span
                                className={`size-1.5 rounded-full ${p.enabled ? "bg-emerald-500" : "bg-muted-foreground/50"}`}
                              />
                              {p.enabled
                                ? t("proxies.enabled")
                                : t("proxies.disabled")}
                            </button>
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
                            <button
                              onClick={() => startEdit(p)}
                              className="inline-flex min-h-9 flex-1 items-center justify-center gap-1.5 rounded-lg border border-border px-2.5 text-xs font-medium text-foreground hover:bg-muted/50"
                            >
                              <Pencil className="size-3.5" />
                              {t("proxies.editProxy")}
                            </button>
                            <button
                              onClick={() => handleTest(p)}
                              disabled={isTesting}
                              className="inline-flex min-h-9 flex-1 items-center justify-center gap-1.5 rounded-lg border border-border px-2.5 text-xs font-medium text-foreground hover:bg-muted/50 disabled:opacity-50"
                            >
                              {isTesting ? (
                                <Loader2 className="size-3.5 animate-spin" />
                              ) : (
                                <Play className="size-3.5" />
                              )}
                              {t("proxies.test")}
                            </button>
                            <button
                              onClick={() => handleDelete(p.id)}
                              className="inline-flex min-h-9 items-center justify-center rounded-lg px-2.5 text-destructive hover:bg-destructive/10"
                              title={t("common.delete")}
                            >
                              <Trash2 className="size-3.5" />
                            </button>
                          </div>
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>

              {/* Desktop table */}
              <div className="data-table-shell hidden lg:block">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border text-left text-muted-foreground">
                      <th className="p-3 w-10">
                        <input
                          type="checkbox"
                          checked={allSelected}
                          onChange={toggleSelectAll}
                          className="size-4 rounded"
                        />
                      </th>
                      <th className="p-3 font-semibold">
                        {t("proxies.colUrl")}
                      </th>
                      <th className="p-3 font-semibold">
                        {t("proxies.colStatus")}
                      </th>
                      <th className="p-3 font-semibold">
                        {t("proxies.colLocation")}
                      </th>
                      <th className="p-3 font-semibold">
                        {t("proxies.colIp")}
                      </th>
                      <th className="p-3 font-semibold">
                        {t("proxies.colLatency")}
                      </th>
                      <th className="p-3 font-semibold text-right">
                        {t("proxies.colActions")}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {pagedProxies.map((p) => {
                      const isTesting = testingIds.has(p.id);
                      return (
                        <tr
                          key={p.id}
                          className="border-b border-border/50 hover:bg-muted/30 transition-colors"
                        >
                          <td className="p-3">
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
                          </td>
                          <td className="p-3 max-w-[380px]">
                            <div className="flex items-center gap-2">
                              <button
                                onClick={() => {
                                  setRevealedIds((prev) => {
                                    const next = new Set(prev);
                                    if (next.has(p.id)) next.delete(p.id);
                                    else next.add(p.id);
                                    return next;
                                  });
                                }}
                                className="shrink-0 flex items-center justify-center size-6 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-all"
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
                              </button>
                              <span className="font-mono text-[13px] font-medium break-all text-foreground">
                                {revealedIds.has(p.id) ? p.url : maskUrl(p.url)}
                              </span>
                            </div>
                          </td>
                          <td className="p-3">
                            <button
                              onClick={() => handleToggle(p)}
                              className={`inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-semibold transition-all ${
                                p.enabled
                                  ? "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border border-emerald-500/20"
                                  : "bg-muted/50 text-muted-foreground border border-border"
                              }`}
                            >
                              <span
                                className={`size-1.5 rounded-full ${p.enabled ? "bg-emerald-500" : "bg-muted-foreground/50"}`}
                              />
                              {p.enabled
                                ? t("proxies.enabled")
                                : t("proxies.disabled")}
                            </button>
                          </td>
                          {/* Location */}
                          <td className="p-3">
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
                          </td>
                          {/* IP */}
                          <td className="p-3">
                            {p.test_ip ? (
                              <span className="text-[13px] font-mono font-medium text-foreground whitespace-nowrap">
                                {p.test_ip}
                              </span>
                            ) : (
                              <span className="text-xs text-muted-foreground">
                                -
                              </span>
                            )}
                          </td>
                          {/* Latency */}
                          <td className="p-3">
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
                          </td>
                          <td className="p-3">
                            <div className="flex items-center gap-1.5 justify-end">
                              <button
                                onClick={() => startEdit(p)}
                                className="flex items-center justify-center size-7 rounded-lg text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-all"
                                title={t("proxies.editProxy")}
                              >
                                <Pencil className="size-3.5" />
                              </button>
                              <button
                                onClick={() => handleTest(p)}
                                disabled={isTesting}
                                className="flex items-center gap-1 px-2.5 py-1.5 rounded-lg text-xs font-medium border border-border text-foreground hover:bg-muted/50 transition-all disabled:opacity-50"
                                title={t("proxies.testProxy")}
                              >
                                {isTesting ? (
                                  <Loader2 className="size-3.5 animate-spin" />
                                ) : (
                                  <Play className="size-3.5" />
                                )}
                                {t("proxies.test")}
                              </button>
                              <button
                                onClick={() => handleDelete(p.id)}
                                className="flex items-center justify-center size-7 rounded-lg text-destructive hover:bg-destructive/10 transition-all"
                                title={t("common.delete")}
                              >
                                <Trash2 className="size-3.5" />
                              </button>
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>

              {/* Pagination */}
              {totalPages > 1 && (
                <div className="flex flex-col gap-2 px-4 py-3 border-t border-border sm:flex-row sm:items-center sm:justify-between">
                  <span className="text-xs text-muted-foreground">
                    {t("proxies.pagination", {
                      total: proxies.length,
                      page,
                      totalPages,
                    })}
                  </span>
                  <div className="flex items-center gap-1">
                    <button
                      onClick={() => setPage((p) => Math.max(1, p - 1))}
                      disabled={page <= 1}
                      className="flex items-center justify-center size-9 rounded-lg border border-border text-foreground hover:bg-muted/50 transition-all disabled:opacity-30 disabled:cursor-not-allowed"
                    >
                      <ChevronLeft className="size-4" />
                    </button>
                    {Array.from({ length: totalPages }, (_, i) => i + 1).map(
                      (n) => (
                        <button
                          key={n}
                          onClick={() => setPage(n)}
                          className={`flex items-center justify-center size-9 rounded-lg text-xs font-medium transition-all ${
                            n === page
                              ? "bg-primary text-primary-foreground shadow-sm"
                              : "border border-border text-foreground hover:bg-muted/50"
                          }`}
                        >
                          {n}
                        </button>
                      ),
                    )}
                    <button
                      onClick={() =>
                        setPage((p) => Math.min(totalPages, p + 1))
                      }
                      disabled={page >= totalPages}
                      className="flex items-center justify-center size-9 rounded-lg border border-border text-foreground hover:bg-muted/50 transition-all disabled:opacity-30 disabled:cursor-not-allowed"
                    >
                      <ChevronRight className="size-4" />
                    </button>
                  </div>
                </div>
              )}
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

    </div>
  );
}
