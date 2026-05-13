import { memo, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Copy, Check, Play, Loader2 } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { useHighlightedHtml } from "../../hooks/useHighlighter";

export const CodeBlock = memo(function CodeBlock({
  label,
  content,
  lang,
}: {
  label?: string;
  content: string;
  lang?: string;
}) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [shouldHighlight, setShouldHighlight] = useState(false);
  const blockRef = useRef<HTMLDivElement>(null);
  const resolvedLang =
    lang ||
    (label?.endsWith(".toml")
      ? "toml"
      : label?.endsWith(".json")
        ? "json"
        : "bash");
  const highlightedHtml = useHighlightedHtml(
    shouldHighlight ? content : "",
    resolvedLang,
  );

  useEffect(() => {
    const el = blockRef.current;
    if (!el || shouldHighlight) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) {
          setShouldHighlight(true);
          observer.disconnect();
        }
      },
      { rootMargin: "360px 0px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [shouldHighlight]);

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(content);
    } catch {
      const ta = document.createElement("textarea");
      ta.value = content;
      ta.style.cssText = "position:fixed;left:-9999px";
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div ref={blockRef} className="code-panel relative">
      {label && (
        <div className="code-panel-header">
          <span className="code-panel-label">{label}</span>
          <button
            onClick={() => void handleCopy()}
            className={`code-panel-copy ${
              copied ? "bg-emerald-500/20 text-emerald-300" : ""
            }`}
            aria-label={copied ? t("common.copied") : t("common.copy")}
          >
            {copied ? (
              <Check className="size-3.5" />
            ) : (
              <Copy className="size-3.5" />
            )}
          </button>
        </div>
      )}
      {!label && (
        <div className="absolute top-2 right-2 z-10">
          <button
            onClick={() => void handleCopy()}
            className={`code-panel-copy ${copied ? "bg-emerald-500/20 text-emerald-300" : ""}`}
            aria-label={copied ? t("common.copied") : t("common.copy")}
          >
            {copied ? (
              <Check className="size-3" />
            ) : (
              <Copy className="size-3" />
            )}
          </button>
        </div>
      )}
      {highlightedHtml ? (
        <div
          className={`code-panel-pre shiki-wrapper ${lang === "json" ? "text-[13px]" : "text-sm"}`}
          dangerouslySetInnerHTML={{ __html: highlightedHtml }}
        />
      ) : (
        <pre
          className={`code-panel-pre ${lang === "json" ? "text-[13px]" : "text-sm"}`}
        >
          <code>{content}</code>
        </pre>
      )}
    </div>
  );
});

export function MethodBadge({ method, sm }: { method: string; sm?: boolean }) {
  const colors: Record<string, string> = {
    GET: "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400 border-emerald-200 dark:border-emerald-800",
    POST: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400 border-blue-200 dark:border-blue-800",
    PUT: "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400 border-amber-200 dark:border-amber-800",
    DELETE:
      "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400 border-red-200 dark:border-red-800",
  };
  const size = sm
    ? "px-1.5 py-0.5 rounded text-[10px]"
    : "px-2.5 py-1 rounded-lg text-xs";
  return (
    <span
      className={`inline-flex items-center font-bold border ${size} ${colors[method] || "bg-muted text-foreground border-border"}`}
    >
      {method}
    </span>
  );
}

function StatusTabs({
  tabs,
  active,
  onChange,
}: {
  tabs: { code: number; label?: string }[];
  active: number;
  onChange: (c: number) => void;
}) {
  return (
    <div className="flex items-center gap-0.5 border-b border-border mb-0">
      {tabs.map((tab) => {
        const isActive = active === tab.code;
        const codeColor =
          tab.code < 300
            ? "text-emerald-600 dark:text-emerald-400"
            : tab.code < 400
              ? "text-amber-600 dark:text-amber-400"
              : "text-red-500 dark:text-red-400";
        return (
          <button
            key={tab.code}
            onClick={() => onChange(tab.code)}
            className={`px-3 py-2 text-sm font-semibold border-b-2 transition-colors ${
              isActive
                ? `border-foreground ${codeColor}`
                : "border-transparent text-muted-foreground/60 hover:text-muted-foreground"
            }`}
          >
            {tab.code}
          </button>
        );
      })}
    </div>
  );
}

function TryItDialog({
  open,
  onClose,
  method,
  path,
  defaultBody,
  apiKey,
  baseUrl,
  allKeys,
}: {
  open: boolean;
  onClose: () => void;
  method: string;
  path: string;
  defaultBody: string;
  apiKey: string;
  baseUrl: string;
  allKeys: { name: string; key: string }[];
}) {
  const { t } = useTranslation();
  const [body, setBody] = useState(defaultBody);
  const [token, setToken] = useState(apiKey);
  const [response, setResponse] = useState("");
  const [status, setStatus] = useState<number | null>(null);
  const [loading, setLoading] = useState(false);
  const [duration, setDuration] = useState<number | null>(null);

  useEffect(() => {
    if (open) {
      setBody(defaultBody);
      setToken(apiKey);
      setResponse("");
      setStatus(null);
      setDuration(null);
    }
  }, [open, defaultBody, apiKey]);

  const handleSend = async () => {
    setLoading(true);
    setResponse("");
    setStatus(null);
    setDuration(null);
    const start = performance.now();
    try {
      const isAdmin = path.startsWith("/api/admin");
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
      };
      if (isAdmin) {
        headers["X-Admin-Key"] = token;
      } else if (path === "/v1/messages") {
        headers["x-api-key"] = token;
        headers["anthropic-version"] = "2023-06-01";
      } else {
        headers["Authorization"] = `Bearer ${token}`;
      }

      const isGet = method === "GET";
      const url = baseUrl + path;
      const res = await fetch(url, {
        method,
        headers,
        body: isGet ? undefined : body.trim() || undefined,
      });
      setStatus(res.status);
      setDuration(Math.round(performance.now() - start));
      const text = await res.text();
      try {
        setResponse(JSON.stringify(JSON.parse(text), null, 2));
      } catch {
        setResponse(text);
      }
    } catch (e) {
      setDuration(Math.round(performance.now() - start));
      setResponse(`Error: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setLoading(false);
    }
  };

  const statusColor =
    status === null
      ? ""
      : status < 300
        ? "text-emerald-600"
        : status < 400
          ? "text-amber-600"
          : "text-red-500";
  const statusBg =
    status === null
      ? ""
      : status < 300
        ? "bg-emerald-50 dark:bg-emerald-900/20"
        : status < 400
          ? "bg-amber-50 dark:bg-amber-900/20"
          : "bg-red-50 dark:bg-red-900/20";

  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
    >
      <DialogContent
        className="sm:max-w-3xl max-h-[90vh] overflow-visible flex flex-col gap-0 p-0"
        showCloseButton={false}
      >
        <div className="flex items-center gap-3 px-5 py-3.5 border-b border-border bg-muted/30">
          <div className="flex items-center gap-2.5 flex-1 px-3 py-2 rounded-xl border border-border bg-background">
            <MethodBadge method={method} />
            <code className="code-inline">{path}</code>
          </div>
          <Button
            onClick={() => void handleSend()}
            disabled={loading}
            className="gap-2 bg-emerald-600 px-5 text-white shrink-0 hover:bg-emerald-600/90 dark:bg-emerald-500/90 dark:hover:bg-emerald-500"
          >
            {loading ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Play className="size-4" />
            )}
            {loading ? t("apiRef.tryIt.sending") : t("apiRef.tryIt.send")}
          </Button>
        </div>

        <div className="flex flex-1 min-h-0 overflow-hidden">
          <div className="flex-1 overflow-visible p-5 space-y-4 border-r border-border">
            <div className="rounded-xl border border-border overflow-visible">
              <div className="px-4 py-2.5 bg-muted/30 border-b border-border">
                <span className="text-sm font-semibold text-foreground">
                  {t("apiRef.tryIt.authTitle")}
                </span>
              </div>
              <div className="p-4 space-y-3">
                <div className="flex items-center justify-between gap-3">
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-semibold text-foreground">
                        {path === "/v1/messages"
                          ? "x-api-key"
                          : path.startsWith("/api/admin")
                            ? "X-Admin-Key"
                            : "Authorization"}
                      </span>
                      <span className="code-inline text-[11px]">string</span>
                    </div>
                    <Badge
                      variant="destructive"
                      className="mt-1 text-[10px] px-1.5 py-0"
                    >
                      {t("apiRef.tryIt.required")}
                    </Badge>
                  </div>
                  <input
                    className="w-52 px-3 py-1.5 rounded-lg border border-border bg-background text-sm font-medium focus:outline-none focus:ring-2 focus:ring-primary/30"
                    placeholder={t("apiRef.tryIt.keyPlaceholder")}
                    value={token}
                    onChange={(e) => setToken(e.target.value)}
                  />
                </div>
                {allKeys.length > 0 && (
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground shrink-0">
                      {t("apiRef.tryIt.selectKey")}
                    </span>
                    <Select
                      value={token}
                      onValueChange={(v) => setToken(v)}
                      options={allKeys.map((k) => ({
                        label: `${k.name} — ${k.key.length > 20 ? k.key.slice(0, 8) + "..." + k.key.slice(-4) : k.key}`,
                        value: k.key,
                      }))}
                    />
                  </div>
                )}
              </div>
            </div>

            {method !== "GET" && method !== "DELETE" && (
              <div className="rounded-xl border border-border overflow-hidden">
                <div className="px-4 py-2.5 bg-muted/30 border-b border-border">
                  <span className="text-sm font-semibold text-foreground">
                    {t("apiRef.tryIt.requestBody")}
                  </span>
                </div>
                <textarea
                  className="w-full h-56 resize-none border-0 bg-background p-4 text-[15px] leading-relaxed outline-none"
                  style={{ fontFamily: "var(--font-mono)" }}
                  value={body}
                  onChange={(e) => setBody(e.target.value)}
                  spellCheck={false}
                />
              </div>
            )}
          </div>

          <div className="flex-1 overflow-auto p-5">
            <div className="rounded-xl border border-border overflow-hidden h-full flex flex-col">
              <div className="px-4 py-2.5 bg-muted/30 border-b border-border flex items-center justify-between">
                <span className="text-sm font-semibold text-foreground">
                  {t("apiRef.tryIt.responseTitle")}
                </span>
                {status !== null && (
                  <div className="flex items-center gap-2.5">
                    <span
                      className={`px-2 py-0.5 rounded-md text-xs font-bold ${statusColor} ${statusBg}`}
                    >
                      {status}
                    </span>
                    {duration !== null && (
                      <span className="text-xs text-muted-foreground">
                        {duration}ms
                      </span>
                    )}
                  </div>
                )}
              </div>
              <div className="flex-1 overflow-auto">
                {response ? (
                  <pre
                    className="p-4 text-[15px] text-foreground leading-relaxed whitespace-pre-wrap"
                    style={{ fontFamily: "var(--font-mono)" }}
                  >
                    <code>{response}</code>
                  </pre>
                ) : (
                  <div className="flex items-center justify-center h-full min-h-[200px] text-sm text-muted-foreground">
                    {loading ? (
                      <div className="flex items-center gap-2">
                        <Loader2 className="size-4 animate-spin" />
                        <span>{t("apiRef.tryIt.sending")}</span>
                      </div>
                    ) : (
                      <span>{t("apiRef.tryIt.placeholder")}</span>
                    )}
                  </div>
                )}
              </div>
            </div>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export const EndpointDoc = memo(function EndpointDoc({
  id,
  method,
  path,
  title,
  description,
  curlExample,
  responseExamples,
  defaultBody,
  apiKey,
  baseUrl,
  allKeys,
}: {
  id?: string;
  method: string;
  path: string;
  title: string;
  description: string;
  curlExample: string;
  responseExamples: { code: number; body: string }[];
  defaultBody?: string;
  apiKey?: string;
  baseUrl?: string;
  allKeys?: { name: string; key: string }[];
}) {
  const { t } = useTranslation();
  const [activeStatus, setActiveStatus] = useState(
    responseExamples[0]?.code ?? 200,
  );
  const activeBody =
    responseExamples.find((r) => r.code === activeStatus)?.body ?? "";
  const [tryOpen, setTryOpen] = useState(false);
  const supportsJsonBody =
    method === "GET" ||
    method === "DELETE" ||
    !defaultBody ||
    (() => {
      try {
        JSON.parse(defaultBody);
        return true;
      } catch {
        return false;
      }
    })();
  const supportsTryIt =
    !path.includes(":") &&
    path !== "/api/admin/accounts/import" &&
    supportsJsonBody;

  return (
    <Card id={id} className="mb-4 scroll-mt-20 py-0">
      <CardContent className="p-4">
        <div className="mb-3 flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="text-[17px] font-bold text-foreground">{title}</h3>
            <p className="mt-0.5 text-[13px] text-muted-foreground">
              {description}
            </p>
          </div>
        </div>

        <div className="mb-4 flex items-center gap-2 rounded-lg border border-border bg-muted/30 p-2.5">
          <MethodBadge method={method} />
          <code className="code-inline min-w-0 flex-1 truncate">{path}</code>
          <Button
            size="sm"
            onClick={() => setTryOpen(true)}
            disabled={!supportsTryIt}
            className="h-8 gap-1.5 bg-emerald-600 text-white shrink-0 hover:bg-emerald-600/90 dark:bg-emerald-500/90 dark:hover:bg-emerald-500"
          >
            <Play className="size-3.5" />
            {t("apiRef.tryIt.button")}
          </Button>
        </div>

        {supportsTryIt && tryOpen && (
          <TryItDialog
            open={tryOpen}
            onClose={() => setTryOpen(false)}
            method={method}
            path={path}
            defaultBody={defaultBody || ""}
            apiKey={apiKey || ""}
            baseUrl={baseUrl || ""}
            allKeys={allKeys || []}
          />
        )}

        <div className="mb-4">
          <CodeBlock label="cURL" content={curlExample} lang="bash" />
        </div>

        <div className="code-panel">
          <div className="code-panel-header px-4 pt-1.5 pb-0">
            <StatusTabs
              tabs={responseExamples.map((r) => ({ code: r.code }))}
              active={activeStatus}
              onChange={setActiveStatus}
            />
          </div>
          <pre className="code-panel-pre max-h-[340px] bg-transparent text-[13px]">
            <code>{activeBody}</code>
          </pre>
        </div>
      </CardContent>
    </Card>
  );
});
