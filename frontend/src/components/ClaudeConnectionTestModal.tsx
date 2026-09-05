import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Check, CheckCircle2, ChevronRight, Copy, Loader2, RefreshCw, XCircle } from "lucide-react";
import { getAdminKey } from "../api";
import type { AccountRow } from "../types";
import { claudeTestTokenMetrics, readClaudeTestEvents } from "../lib/claudeConnectionTest";
import type { ClaudeTestDiagnostics } from "../lib/claudeConnectionTest";
import { cn } from "../lib/utils";
import { useToast } from "../hooks/useToast";
import Modal from "./Modal";
import { Button } from "./ui/button";
import { Select } from "./ui/select";

type TestStatus = "connecting" | "streaming" | "success" | "error";

export default function ClaudeConnectionTestModal({ account, onClose, onSettled }: {
  account: AccountRow;
  onClose: () => void;
  onSettled: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [status, setStatus] = useState<TestStatus>("connecting");
  const [running, setRunning] = useState(true);
  const [output, setOutput] = useState("");
  const [errorMessage, setErrorMessage] = useState("");
  const [diagnostics, setDiagnostics] = useState<ClaudeTestDiagnostics | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [copied, setCopied] = useState(false);
  const settledRef = useRef(false);
  const onSettledRef = useRef(onSettled);
  const translationRef = useRef(t);
  onSettledRef.current = onSettled;
  translationRef.current = t;

  const modelOptions = useMemo(() => {
    const blocked = new Set((account.model_cooldowns ?? [])
      .filter((cooldown) => (cooldown.reason || "").toLowerCase().includes("credit"))
      .map((cooldown) => cooldown.model.toLowerCase()));
    const configured = (account.models ?? []).map((item) => item.trim()).filter((item) => item.toLowerCase().startsWith("claude-") && !blocked.has(item.toLowerCase()));
    return configured.length > 0 ? configured : ["claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"].filter((item) => !blocked.has(item));
  }, [account.model_cooldowns, account.models]);
  const [model, setModel] = useState(modelOptions[0] || "");
  useEffect(() => {
    if (!modelOptions.includes(model)) setModel(modelOptions[0] || "");
  }, [model, modelOptions]);

  const markSettled = useCallback(() => {
    if (settledRef.current) return;
    settledRef.current = true;
    onSettledRef.current();
  }, []);

  useEffect(() => {
    if (!model) {
      setStatus("error");
      setRunning(false);
      setErrorMessage(translationRef.current("claude.testNoModel"));
      return;
    }
    const controller = new AbortController();
    settledRef.current = false;
    setStatus("connecting");
    setRunning(true);
    setOutput("");
    setErrorMessage("");
    setDiagnostics(null);
    setCopied(false);

    const run = async () => {
      const translate = translationRef.current;
      try {
        const query = new URLSearchParams({ model });
        const key = getAdminKey();
        const response = await fetch(`/api/admin/accounts/${account.id}/test?${query.toString()}`, {
          signal: controller.signal,
          headers: key ? { "X-Admin-Key": key } : {},
        });
        if (!response.ok) {
          const body = await response.text();
          let message = `HTTP ${response.status}`;
          try {
            const parsed = JSON.parse(body) as { error?: string | { message?: string } };
            if (typeof parsed.error === "string") message = parsed.error;
            else if (parsed.error?.message) message = parsed.error.message;
          } catch {
            if (body.trim()) message = body.trim().slice(0, 500);
          }
          throw new Error(message);
        }
        if (!response.body) throw new Error(translate("accounts.browserStreamingUnsupported"));
        const receivedTerminalEvent = await readClaudeTestEvents(response.body, (event) => {
          if (controller.signal.aborted) return;
          if (event.diagnostics) setDiagnostics(event.diagnostics);
          if (event.type === "diagnostics" && event.diagnostics?.duration_ms === undefined) setStatus("streaming");
          if (event.type === "content" && event.text) {
            setStatus("streaming");
            setOutput((current) => current + event.text);
          }
          if (event.type === "test_complete") {
            setStatus(event.success ? "success" : "error");
            if (!event.success) setErrorMessage(event.error || translate("accounts.testFailed"));
          }
          if (event.type === "error") {
            setStatus("error");
            setErrorMessage(event.error || translate("accounts.unknownError"));
          }
        });
        if (controller.signal.aborted) return;
        // Refresh only once the SSE stream has closed: final diagnostics and
        // account snapshot invalidation can follow the terminal result.
        if (!receivedTerminalEvent) throw new Error(translate("accounts.connectionEndedUnexpectedly"));
        markSettled();
      } catch (error) {
        if (controller.signal.aborted) return;
        setStatus("error");
        setErrorMessage(error instanceof Error ? error.message : translationRef.current("accounts.connectionFailed"));
        markSettled();
      } finally {
        if (!controller.signal.aborted) setRunning(false);
      }
    };
    void run();
    return () => controller.abort();
  }, [account.id, model, attempt, markSettled]);

  const metrics = claudeTestTokenMetrics(diagnostics?.usage);
  const tokenLabels = {
    input_tokens: t("claude.testInputTokens"),
    output_tokens: t("claude.testOutputTokens"),
    cache_read_input_tokens: t("claude.testCacheRead"),
    cache_creation_input_tokens: t("claude.testCacheWrite"),
  };
  const tokenColors = ["bg-sky-500", "bg-emerald-500", "bg-violet-500", "bg-amber-500"];
  const formatMS = (value?: number) => typeof value === "number" ? `${value.toLocaleString()} ms` : "—";
  const timing = [
    { label: t("claude.testHTTPStatus"), value: diagnostics?.http_status || "—" },
    { label: t("claude.testHeadersTime"), value: formatMS(diagnostics?.headers_ms) },
    { label: t("claude.testFirstContent"), value: formatMS(diagnostics?.first_content_ms) },
    { label: t("claude.testDuration"), value: formatMS(diagnostics?.duration_ms) },
  ];
  const identity = [
    { label: "Request ID", value: diagnostics?.request_id },
    { label: "Organization ID", value: diagnostics?.organization_id },
    { label: "Message ID", value: diagnostics?.message_id },
    { label: t("claude.testStopReason"), value: diagnostics?.stop_reason },
    { label: t("claude.testErrorType"), value: diagnostics?.error_type },
  ].filter((item) => item.value);
  const modeLabel = diagnostics?.fingerprint_mode === "force" ? t("claude.fpForce")
    : diagnostics?.fingerprint_mode === "preserve" ? t("claude.fpPreserve") : "—";
  const statusLabel = status === "connecting" ? t("accounts.connecting") : status === "streaming" ? t("accounts.receivingResponse") : status === "success" ? t("accounts.testSuccess") : t("accounts.testFailed");
  const StatusIcon = running ? Loader2 : status === "success" ? CheckCircle2 : XCircle;
  const copyDiagnostics = async () => {
    try {
      await navigator.clipboard.writeText(JSON.stringify({ status, error: errorMessage || undefined, output, diagnostics }, null, 2));
      setCopied(true);
    } catch {
      showToast(t("claude.testCopyFailed"), "error");
    }
  };

  return (
    <Modal
      show
      title={t("accounts.testConnectionTitle", { account: account.email || account.name || `#${account.id}` })}
      titleClassName="text-base sm:text-lg"
      contentClassName="sm:max-w-[760px]"
      onClose={onClose}
      footer={(
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          <Button variant="ghost" size="sm" className="mr-auto" disabled={running || !diagnostics} onClick={() => void copyDiagnostics()}>
            {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
            {copied ? t("claude.testCopied") : t("claude.testCopy")}
          </Button>
          <Button variant="outline" size="sm" onClick={onClose}>{t("common.close")}</Button>
          <Button size="sm" disabled={running || !model} onClick={() => setAttempt((value) => value + 1)}>
            <RefreshCw className={cn("size-3.5", running && "animate-spin")} />{t("claude.testRetry")}
          </Button>
        </div>
      )}
    >
      <div className="space-y-4">
        <div className={cn("flex flex-wrap items-center gap-3 rounded-xl border px-3 py-3", running ? "border-primary/20 bg-primary/5" : status === "success" ? "border-emerald-500/20 bg-emerald-500/5" : "border-destructive/20 bg-destructive/5")}>
          <StatusIcon className={cn("size-5 shrink-0", running ? "animate-spin text-primary" : status === "success" ? "text-emerald-500" : "text-destructive")} />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-semibold text-foreground" role="status">{statusLabel}</p>
            <p className="mt-0.5 text-[11px] text-muted-foreground">Claude · Messages API</p>
          </div>
          <Select compact className="w-full min-w-0 sm:w-60" value={model} onValueChange={setModel} disabled={running} options={modelOptions.map((item) => ({ value: item, label: item }))} />
        </div>

        {errorMessage ? <div role="alert" className="break-words rounded-lg border border-destructive/20 bg-destructive/5 px-3 py-2.5 text-xs leading-relaxed text-destructive">{errorMessage}</div> : null}

        <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          {timing.map((item) => (
            <div key={item.label} className="rounded-xl border border-border/70 bg-muted/20 px-3 py-2.5">
              <p className="text-[11px] text-muted-foreground">{item.label}</p>
              <p className="mt-1 font-mono text-sm font-semibold tabular-nums text-foreground">{item.value}</p>
            </div>
          ))}
        </div>

        <dl className="grid grid-cols-1 gap-x-4 gap-y-2 text-xs sm:grid-cols-2">
          <div className="min-w-0"><dt className="text-muted-foreground">{t("claude.testResponseModel")}</dt><dd className="mt-1 break-all font-mono">{diagnostics?.response_model || "—"}</dd></div>
          <div className="min-w-0"><dt className="text-muted-foreground">{t("claude.fingerprintModeLabel")}</dt><dd className="mt-1">{modeLabel}</dd></div>
          {identity.map((item) => (
            <div key={item.label} className="min-w-0"><dt className="text-muted-foreground">{item.label}</dt><dd className="mt-1 break-all font-mono text-[11px] leading-relaxed">{item.value}</dd></div>
          ))}
        </dl>

        {diagnostics?.usage ? (
          <section aria-label={t("claude.testTokens")} className="space-y-2">
            <h3 className="text-xs font-semibold">{t("claude.testTokens")}</h3>
            <div className="grid grid-cols-2 gap-2">
              {metrics.map((metric, index) => (
                <div key={metric.key} className="min-w-0 rounded-lg border border-border/70 px-3 py-2.5">
                  <div className="flex flex-wrap items-baseline justify-between gap-1 text-xs">
                    <span className="text-muted-foreground">{tokenLabels[metric.key]}</span>
                    <span className="font-mono font-semibold tabular-nums">{metric.value?.toLocaleString() ?? "—"}</span>
                  </div>
                  <div aria-hidden className="mt-2 h-1 overflow-hidden rounded-full bg-muted"><div className={cn("h-full rounded-full transition-[width]", tokenColors[index])} style={{ width: `${metric.percent}%` }} /></div>
                </div>
              ))}
            </div>
            {diagnostics.usage.cache_creation ? (
              <p className="text-[11px] text-muted-foreground">
                {t("claude.testCacheWrite")} · 5m: {diagnostics.usage.cache_creation.ephemeral_5m_input_tokens?.toLocaleString() ?? "—"} · 1h: {diagnostics.usage.cache_creation.ephemeral_1h_input_tokens?.toLocaleString() ?? "—"}
              </p>
            ) : null}
          </section>
        ) : null}

        <section className="space-y-2">
          <h3 className="text-xs font-semibold">{t("claude.testReply")}</h3>
          <pre className="max-h-52 overflow-auto whitespace-pre-wrap break-words rounded-xl border border-border/70 bg-muted/20 p-3 font-mono text-xs leading-relaxed">{output || (running ? t("common.loading") : t("claude.testNoText"))}</pre>
        </section>

        {diagnostics?.response_headers?.length ? (
          <details className="group rounded-xl border border-border/70">
            <summary className="flex cursor-pointer list-none items-center gap-2 rounded-xl px-3 py-2.5 text-xs font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 [&::-webkit-details-marker]:hidden">
              <ChevronRight className="size-3.5 transition-transform group-open:rotate-90" />
              {t("claude.testResponseHeaders")}<span className="ml-auto rounded bg-muted px-1.5 text-[10px] tabular-nums text-muted-foreground">{diagnostics.response_headers.length}</span>
            </summary>
            <div className="max-h-60 overflow-auto border-t border-border/70">
              <table className="w-full table-fixed text-left text-[11px]">
                <tbody>
                  {diagnostics.response_headers.map((header, index) => (
                    <tr key={`${header.name}-${index}`} className="border-b border-border/40 last:border-0">
                      <th scope="row" className="w-[48%] break-all px-3 py-2 align-top font-mono font-normal text-muted-foreground">{header.name}</th>
                      <td className="break-all px-3 py-2 align-top font-mono">{header.value}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </details>
        ) : null}

        {diagnostics?.response_body ? (
          <details className="group rounded-xl border border-border/70">
            <summary className="flex cursor-pointer list-none items-center gap-2 rounded-xl px-3 py-2.5 text-xs font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 [&::-webkit-details-marker]:hidden">
              <ChevronRight className="size-3.5 transition-transform group-open:rotate-90" />{t("claude.testRawResponse")}
            </summary>
            <div className="border-t border-border/70 px-3 py-2.5">
              <p className="mb-2 text-[11px] leading-relaxed text-muted-foreground">{t("claude.testRawHint")}{diagnostics.body_truncated ? ` · ${t("claude.testBodyTruncated")}` : ""}</p>
              <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all font-mono text-[11px] leading-relaxed">{diagnostics.response_body}</pre>
            </div>
          </details>
        ) : null}
      </div>
    </Modal>
  );
}
