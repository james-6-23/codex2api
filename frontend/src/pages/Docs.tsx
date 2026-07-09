import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  Copy,
  Check,
  ClipboardCheck,
  ExternalLink,
  Sparkles,
  Terminal,
  KeyRound,
  Wand2,
  Server,
} from "lucide-react";
import { api, getAdminKey } from "../api";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { useToast } from "../hooks/useToast";
import { CodeBlock, EndpointDoc } from "./docs/EndpointDoc";
import DocsTOC, { type DocsTOCItem } from "./docs/DocsTOC";
import {
  buildQuickTools,
  resolveTemplate,
  type DocsLocale,
  type QuickTool,
} from "./docs/quickStartTools";
import {
  buildAdminSpecs,
  buildDocsMarkdown,
  buildEndpointSpecs,
} from "./docs/docsContent";
import { DEFAULT_CLAUDE_MODEL_MAP } from "../lib/modelMapping";
import type { SystemSettings } from "../types";

const FALLBACK_MODELS = [
  "gpt-5.5",
  "gpt-5.4",
  "gpt-5.4-mini",
  "gpt-5.3-codex",
  "claude-sonnet-4-5-20250514",
];
type CCSwitchApp = "claude" | "codex" | "gemini";
type QuickToolTab = "codex-cli" | "claude-code" | "cc-switch" | "cherry-studio";
type QuickServiceTier = "default" | "fast";
type QuickReasoningEffort = "none" | "minimal" | "low" | "medium" | "high" | "xhigh" | "ultra";

const CC_SWITCH_LOGO = "https://ccswitch.io/assets/cc-switch-logo-BPrI77SG.png";
const LOBE_ICON_BASE =
  "https://unpkg.com/@lobehub/icons-static-svg@latest/icons";
const CLIENT_ICON_SRC: Record<QuickToolTab, string> = {
  "codex-cli": `${LOBE_ICON_BASE}/codex-color.svg`,
  "claude-code": `${LOBE_ICON_BASE}/claudecode-color.svg`,
  "cc-switch": CC_SWITCH_LOGO,
  "cherry-studio": `${LOBE_ICON_BASE}/cherrystudio-color.svg`,
};

const CC_SWITCH_APPS: Record<
  CCSwitchApp,
  {
    label: string;
    suffix: string;
    endpoint: (baseUrl: string) => string;
    fields: { key: string }[];
  }
> = {
  claude: {
    label: "Claude Code",
    suffix: "Claude",
    endpoint: (baseUrl) => baseUrl,
    fields: [
      { key: "model" },
      { key: "haikuModel" },
      { key: "sonnetModel" },
      { key: "opusModel" },
    ],
  },
  codex: {
    label: "Codex CLI",
    suffix: "Codex",
    endpoint: (baseUrl) => `${baseUrl}/v1`,
    fields: [{ key: "model" }],
  },
  gemini: {
    label: "Gemini CLI",
    suffix: "Gemini",
    endpoint: (baseUrl) => baseUrl,
    fields: [{ key: "model" }],
  },
};

const SECTION_ICON: Record<string, ReactNode> = {
  "quick-start": <Sparkles className="size-4" />,
  "client-config": <Terminal className="size-4" />,
  authentication: <KeyRound className="size-4" />,
  "model-api": <Wand2 className="size-4" />,
  "admin-api": <Server className="size-4" />,
};

const SECTION_TONE: Record<string, { text: string; bg: string; ring: string }> =
  {
    "quick-start": {
      text: "text-amber-600 dark:text-amber-400",
      bg: "bg-amber-500/10 dark:bg-amber-500/15",
      ring: "ring-amber-500/20",
    },
    "client-config": {
      text: "text-emerald-600 dark:text-emerald-400",
      bg: "bg-emerald-500/10 dark:bg-emerald-500/15",
      ring: "ring-emerald-500/20",
    },
    authentication: {
      text: "text-fuchsia-600 dark:text-fuchsia-400",
      bg: "bg-fuchsia-500/10 dark:bg-fuchsia-500/15",
      ring: "ring-fuchsia-500/20",
    },
    "model-api": {
      text: "text-sky-600 dark:text-sky-400",
      bg: "bg-sky-500/10 dark:bg-sky-500/15",
      ring: "ring-sky-500/20",
    },
    "admin-api": {
      text: "text-rose-600 dark:text-rose-400",
      bg: "bg-rose-500/10 dark:bg-rose-500/15",
      ring: "ring-rose-500/20",
    },
  };

function OsTabs({
  active,
  onChange,
}: {
  active: "unix" | "windows";
  onChange: (v: "unix" | "windows") => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="border-b border-border mb-4">
      <nav className="-mb-px flex space-x-4">
        <button
          onClick={() => onChange("unix")}
          className={`whitespace-nowrap py-2.5 px-1 border-b-2 font-medium text-sm transition-colors flex items-center gap-2 ${
            active === "unix"
              ? "border-primary text-primary"
              : "border-transparent text-muted-foreground hover:text-foreground hover:border-border"
          }`}
        >
          macOS / Linux
        </button>
        <button
          onClick={() => onChange("windows")}
          className={`whitespace-nowrap py-2.5 px-1 border-b-2 font-medium text-sm transition-colors flex items-center gap-2 ${
            active === "windows"
              ? "border-primary text-primary"
              : "border-transparent text-muted-foreground hover:text-foreground hover:border-border"
          }`}
        >
          Windows
        </button>
      </nav>
    </div>
  );
}

const FIELD_LABEL = "mb-1.5 block text-[11px] font-bold text-muted-foreground";
const FIELD_INPUT =
  "h-8 w-full rounded-lg border border-input bg-background px-2.5 text-[13px] font-medium text-foreground shadow-xs outline-none transition-[border-color,box-shadow,background-color] placeholder:text-muted-foreground hover:border-primary/30 hover:bg-accent/50 focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/20";
const CONFIG_PANEL =
  "grid gap-3 rounded-lg border border-border bg-muted/25 p-3 shadow-[inset_0_1px_0_hsl(0_0%_100%/0.35)] dark:shadow-none";

function FieldBox({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <label className={FIELD_LABEL}>{label}</label>
      {children}
    </div>
  );
}

async function copyToClipboard(text: string) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.cssText = "position:fixed;left:-9999px";
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
  }
}

function ClientIcon({ id, size = 18 }: { id: QuickToolTab; size?: number }) {
  return (
    <img
      src={CLIENT_ICON_SRC[id]}
      alt=""
      width={size}
      height={size}
      className="rounded-[4px] object-contain"
      loading="lazy"
      decoding="async"
    />
  );
}

function ImportPreviewCard({
  title,
  description,
  icon,
  link,
  disabled,
  onLaunch,
  onCopied,
}: {
  title: string;
  description: string;
  icon: ReactNode;
  link: string;
  disabled?: boolean;
  onLaunch: () => void;
  onCopied: () => void;
}) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const [copied, setCopied] = useState(false);

  const handleCopy = async () => {
    await copyToClipboard(link);
    setCopied(true);
    onCopied();
    window.setTimeout(() => setCopied(false), 1200);
  };

  return (
    <div className="rounded-xl border border-border bg-card/80 p-3.5 shadow-sm transition-[border-color,box-shadow] duration-200 hover:border-primary/25 hover:shadow-md">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex items-start gap-3">
          <span className="inline-flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/15">
            {icon}
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <h4 className="text-[14px] font-bold text-foreground">{title}</h4>
              <Badge
                variant="outline"
                className="h-5 px-1.5 text-[10px] font-bold"
              >
                {t("docs.quickStart.deeplinkBadge")}
              </Badge>
            </div>
            <p className="mt-0.5 text-[12.5px] leading-relaxed text-muted-foreground">
              {description}
            </p>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => void handleCopy()}
            className="h-8 gap-1.5"
          >
            {copied ? (
              <Check className="size-3.5 text-emerald-500" />
            ) : (
              <Copy className="size-3.5" />
            )}
            {copied
              ? t("docs.quickStart.copied")
              : t("docs.quickStart.copyLink")}
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={disabled}
            onClick={onLaunch}
            className="h-8 gap-1.5"
          >
            <ExternalLink className="size-3.5" />
            {t("docs.quickStart.openClient")}
          </Button>
        </div>
      </div>
      <button
        type="button"
        onClick={() => setExpanded((current) => !current)}
        className="mt-2 inline-flex items-center gap-1 text-[12px] font-semibold text-muted-foreground hover:text-foreground"
      >
        {expanded
          ? t("docs.quickStart.collapseFullLink")
          : t("docs.quickStart.viewFullLink")}
      </button>
      {expanded && (
        <div className="mt-2 rounded-lg border border-border bg-muted/25 p-2.5">
          <code className="block max-h-28 overflow-auto break-all font-mono text-[12px] leading-relaxed text-foreground">
            {link}
          </code>
        </div>
      )}
    </div>
  );
}

function ToolTabButton({
  selected,
  id,
  label,
  onClick,
}: {
  selected: boolean;
  id: QuickToolTab;
  label: string;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`inline-flex h-8 items-center justify-center gap-2 rounded-lg px-3 text-[13px] font-semibold transition-[background-color,color,box-shadow] ${
        selected
          ? "bg-background text-foreground shadow-sm ring-1 ring-border"
          : "text-muted-foreground hover:bg-background/60 hover:text-foreground"
      }`}
    >
      <span className="inline-flex size-5 items-center justify-center">
        <ClientIcon id={id} size={18} />
      </span>
      {label}
    </button>
  );
}

function UnderlineTabs<T extends string>({
  tabs,
  active,
  onChange,
}: {
  tabs: { value: T; label: string; hint?: string }[];
  active: T;
  onChange: (value: T) => void;
}) {
  return (
    <div className="border-b border-border">
      <nav className="-mb-px flex space-x-4">
        {tabs.map((tab) => {
          const selected = active === tab.value;
          return (
            <button
              key={tab.value}
              type="button"
              onClick={() => onChange(tab.value)}
              className={`whitespace-nowrap border-b-2 px-1 py-2.5 text-sm font-medium transition-colors ${
                selected
                  ? "border-primary text-primary"
                  : "border-transparent text-muted-foreground hover:border-border hover:text-foreground"
              }`}
              title={tab.hint}
            >
              {tab.label}
            </button>
          );
        })}
      </nav>
    </div>
  );
}

function QuickToolCard({
  tool,
  baseUrl,
  apiKey,
  onCopied,
  onLaunched,
}: {
  tool: QuickTool;
  baseUrl: string;
  apiKey: string;
  onCopied: (name: string) => void;
  onLaunched: (name: string) => void;
}) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  const isProtocol = tool.kind === "protocol";
  const hasKey = Boolean(apiKey);
  const previewKey = hasKey ? apiKey : "YOUR_API_KEY";
  const resolved = resolveTemplate(tool, baseUrl, previewKey);

  const handleClick = async () => {
    if (isProtocol) {
      if (!hasKey) return;
      window.open(resolved, "_blank");
      onLaunched(tool.name);
      return;
    }
    try {
      await navigator.clipboard.writeText(resolved);
    } catch {
      const ta = document.createElement("textarea");
      ta.value = resolved;
      ta.style.cssText = "position:fixed;left:-9999px";
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
    }
    setCopied(true);
    onCopied(tool.name);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className="group relative flex flex-col gap-2.5 rounded-xl border border-border bg-card/70 p-4 transition-all hover:-translate-y-0.5 hover:border-primary/30 hover:shadow-md">
      <div className="flex items-start gap-3">
        <div
          className={`inline-flex size-10 shrink-0 items-center justify-center rounded-lg text-sm font-bold ${tool.iconHue}`}
        >
          {tool.glyph}
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5">
            <h4 className="truncate text-[14px] font-bold text-foreground">
              {tool.name}
            </h4>
            <Badge
              variant="outline"
              className="shrink-0 px-1.5 py-0 text-[10px] font-bold"
            >
              {tool.badge}
            </Badge>
          </div>
          <p className="mt-1 line-clamp-2 text-[12px] leading-snug text-muted-foreground">
            {tool.blurb}
          </p>
        </div>
      </div>
      <Button
        variant={isProtocol ? "default" : "outline"}
        size="sm"
        disabled={isProtocol && !hasKey}
        onClick={() => void handleClick()}
        className="mt-1 w-full justify-center gap-1.5"
      >
        {isProtocol ? (
          <>
            <ExternalLink className="size-3.5" />
            {hasKey
              ? t("docs.quickStart.launch")
              : t("docs.quickStart.needKey")}
          </>
        ) : copied ? (
          <>
            <ClipboardCheck className="size-3.5 text-emerald-500" />
            {t("docs.quickStart.copied")}
          </>
        ) : (
          <>
            <Copy className="size-3.5" />
            {t("docs.quickStart.copyConfig")}
          </>
        )}
      </Button>
    </div>
  );
}

function buildCCSwitchImportUrl({
  app,
  name,
  endpoint,
  apiKey,
  models,
  homepage,
  serviceTier,
  reasoningEffort,
}: {
  app: CCSwitchApp;
  name: string;
  endpoint: string;
  apiKey: string;
  models: Record<string, string>;
  homepage: string;
  serviceTier?: QuickServiceTier;
  reasoningEffort?: QuickReasoningEffort;
}) {
  const params = new URLSearchParams();
  params.set("resource", "provider");
  params.set("app", app);
  params.set("name", name);
  params.set("endpoint", endpoint);
  params.set("apiKey", apiKey);
  Object.entries(models).forEach(([key, value]) => {
    if (value) params.set(key, value);
  });
  if (app === "codex" && serviceTier === "fast") {
    params.set("service_tier", "fast");
  }
  if (app === "codex" && reasoningEffort) {
    params.set("model_reasoning_effort", reasoningEffort);
  }
  params.set("homepage", homepage);
  params.set("enabled", "true");
  return `ccswitch://v1/import?${params.toString()}`;
}

function parseModelMapping(
  settings?: SystemSettings | null,
): Record<string, string> {
  if (!settings?.model_mapping) return {};
  try {
    const parsed = JSON.parse(settings.model_mapping);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
      return {};
    const entries = Object.entries(parsed).filter(
      ([, value]) => typeof value === "string",
    ) as [string, string][];
    return entries.length > 0 ? Object.fromEntries(entries) : {};
  } catch {
    return {};
  }
}

function slugProviderId(name: string) {
  const slug = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return slug || "codexproxy";
}

function modelIncludes(models: string[], model?: string) {
  return Boolean(model && models.includes(model));
}

function preferredMappedClaudeModel(
  mappedClaudeModels: string[],
  keyword: string,
  fallback: string,
) {
  return (
    mappedClaudeModels.find((model) => model.toLowerCase().includes(keyword)) ??
    fallback
  );
}

function encodeBase64(text: string): string {
  return btoa(unescape(encodeURIComponent(text)));
}

function SectionHeader({
  id,
  icon,
  tone,
  eyebrow,
  title,
  description,
}: {
  id: string;
  icon: ReactNode;
  tone: { text: string; bg: string; ring: string };
  eyebrow?: string;
  title: string;
  description?: string;
}) {
  return (
    <div id={id} className="scroll-mt-20 mt-4 mb-3 first:mt-0">
      <div className="flex items-start gap-3">
        <span
          className={`mt-0.5 inline-flex size-9 shrink-0 items-center justify-center rounded-xl ring-1 ${tone.bg} ${tone.text} ${tone.ring}`}
        >
          {icon}
        </span>
        <div className="min-w-0 flex-1">
          {eyebrow ? (
            <div
              className={`text-[10.5px] font-bold uppercase tracking-[0.14em] ${tone.text}`}
            >
              {eyebrow}
            </div>
          ) : null}
          <h2 className="mt-0.5 text-[22px] font-bold leading-tight text-foreground">
            {title}
          </h2>
          {description ? (
            <p className="mt-1 max-w-[640px] text-[13px] leading-relaxed text-muted-foreground">
              {description}
            </p>
          ) : null}
        </div>
      </div>
    </div>
  );
}

export default function Docs() {
  const { t, i18n } = useTranslation();
  const baseUrl = useMemo(() => window.location.origin, []);
  const adminSeed = useMemo(() => getAdminKey(), []);
  const docsLocale = useMemo<DocsLocale>(
    () => ((i18n.language || "zh").startsWith("zh") ? "zh" : "en"),
    [i18n.language],
  );
  const [quickBaseUrl, setQuickBaseUrl] = useState(baseUrl);
  const [codexOs, setCodexOs] = useState<"unix" | "windows">("unix");
  const [claudeOs, setClaudeOs] = useState<"unix" | "windows">("unix");
  const [firstKey, setFirstKey] = useState("");
  const [allKeys, setAllKeys] = useState<{ name: string; key: string }[]>([]);
  const [copyingMd, setCopyingMd] = useState(false);
  const { toast, showToast } = useToast();
  const [selectedKey, setSelectedKey] = useState("");
  const [activeToolTab, setActiveToolTab] = useState<QuickToolTab>("codex-cli");
  const [quickStartModel, setQuickStartModel] = useState("gpt-5.4");
  const [quickServiceTier, setQuickServiceTier] =
    useState<QuickServiceTier>("default");
  const [quickReasoningEffort, setQuickReasoningEffort] =
    useState<QuickReasoningEffort>("xhigh");
  const [settings, setSettings] = useState<SystemSettings | null>(null);
  const [ccSwitchApp, setCcSwitchApp] = useState<CCSwitchApp>("codex");
  const [ccSwitchName, setCcSwitchName] = useState("");
  const [ccSwitchNameEdited, setCcSwitchNameEdited] = useState(false);
  const [ccSwitchModels, setCcSwitchModels] = useState<Record<string, string>>({
    model: "gpt-5.4",
  });
  const [cherryProviderId, setCherryProviderId] = useState("");
  const [cherryProviderEdited, setCherryProviderEdited] = useState(false);
  const [activeCurl, setActiveCurl] = useState<
    "responses" | "chat" | "messages"
  >("responses");
  const [curlModel, setCurlModel] = useState("gpt-5.4");
  const [models, setModels] = useState(FALLBACK_MODELS);

  useEffect(() => {
    api
      .getAPIKeys()
      .then((res) => {
        const keys = (res.keys ?? []).map((k) => ({
          name: k.name,
          key: k.raw_key || k.key,
        }));
        setAllKeys(keys);
        if (keys.length > 0) {
          setFirstKey(keys[0].key);
          setSelectedKey(keys[0].key);
        }
      })
      .catch(() => {});
  }, []);

  useEffect(() => {
    Promise.all([
      api.getModels().catch(() => ({ models: [], items: [] })),
      api.getSettings().catch(() => null),
    ])
      .then(([res, nextSettings]) => {
        setSettings(nextSettings);
        const next = [
          ...(res.models ?? []),
          ...(res.items ?? []).map((item) => item.id),
        ].filter(Boolean);
        const unique = Array.from(new Set(next));
        if (unique.length === 0) return;
        setModels(unique);
        const configuredModel = nextSettings?.test_model;
        const preferred =
          configuredModel && unique.includes(configuredModel)
            ? configuredModel
            : unique.includes("gpt-5.4")
              ? "gpt-5.4"
              : unique[0];
        setQuickStartModel((current) =>
          unique.includes(current) ? current : preferred,
        );
        setCurlModel((current) =>
          unique.includes(current) ? current : preferred,
        );
        setCcSwitchModels((current) => ({
          ...current,
          model: unique.includes(current.model) ? current.model : preferred,
        }));
      })
      .catch(() => {});
  }, []);

  const modelMapping = useMemo(() => {
    const configured = parseModelMapping(settings);
    return Object.keys(configured).length > 0
      ? configured
      : DEFAULT_CLAUDE_MODEL_MAP;
  }, [settings]);
  const mappedClaudeModels = useMemo(
    () => Object.keys(modelMapping),
    [modelMapping],
  );
  const modelOptions = useMemo(
    () => models.map((model) => ({ label: model, value: model })),
    [models],
  );
  const claudeModelOptions = useMemo(() => {
    const merged = Array.from(
      new Set([
        ...mappedClaudeModels,
        ...models.filter((model) => model.startsWith("claude-")),
        "claude-sonnet-4-5-20250514",
      ]),
    );
    return merged.map((model) => ({ label: model, value: model }));
  }, [mappedClaudeModels, models]);
  const ccSwitchModelOptions =
    ccSwitchApp === "claude" ? claudeModelOptions : modelOptions;
  const quickTools = useMemo(() => buildQuickTools(docsLocale), [docsLocale]);
  const ccSwitchConfig = CC_SWITCH_APPS[ccSwitchApp];
  const siteName = settings?.site_name?.trim() || "CodexProxy";
  const defaultCcSwitchName = `${siteName} ${ccSwitchConfig.suffix}`;
  const defaultCherryProviderId = slugProviderId(siteName);
  const showQuickCodexOptions =
    activeToolTab === "codex-cli" ||
    (activeToolTab === "cc-switch" && ccSwitchApp === "codex");

  useEffect(() => {
    if (!ccSwitchNameEdited) setCcSwitchName(defaultCcSwitchName);
  }, [ccSwitchNameEdited, defaultCcSwitchName]);

  useEffect(() => {
    if (!cherryProviderEdited) setCherryProviderId(defaultCherryProviderId);
  }, [cherryProviderEdited, defaultCherryProviderId]);

  useEffect(() => {
    const config = CC_SWITCH_APPS[ccSwitchApp];
    if (!ccSwitchNameEdited) setCcSwitchName(`${siteName} ${config.suffix}`);
    setCcSwitchModels((current) => {
      const preferredClaude = preferredMappedClaudeModel(
        mappedClaudeModels,
        "sonnet",
        "claude-sonnet-4-5-20250514",
      );
      const preferredCodex = models.includes(quickStartModel)
        ? quickStartModel
        : (models[0] ?? FALLBACK_MODELS[0]);
      const next: Record<string, string> = {};
      config.fields.forEach((field) => {
        if (ccSwitchApp === "claude") {
          if (field.key === "haikuModel")
            next[field.key] = modelIncludes(
              mappedClaudeModels,
              current[field.key],
            )
              ? current[field.key]
              : preferredMappedClaudeModel(
                  mappedClaudeModels,
                  "haiku",
                  preferredClaude,
                );
          else if (field.key === "opusModel")
            next[field.key] = modelIncludes(
              mappedClaudeModels,
              current[field.key],
            )
              ? current[field.key]
              : preferredMappedClaudeModel(
                  mappedClaudeModels,
                  "opus",
                  preferredClaude,
                );
          else if (field.key === "sonnetModel")
            next[field.key] = modelIncludes(
              mappedClaudeModels,
              current[field.key],
            )
              ? current[field.key]
              : preferredMappedClaudeModel(
                  mappedClaudeModels,
                  "sonnet",
                  preferredClaude,
                );
          else
            next[field.key] = modelIncludes(
              mappedClaudeModels,
              current[field.key],
            )
              ? current[field.key]
              : preferredClaude;
          return;
        }
        next[field.key] = modelIncludes(models, current[field.key])
          ? current[field.key]
          : preferredCodex;
      });
      return next;
    });
  }, [
    ccSwitchApp,
    ccSwitchNameEdited,
    mappedClaudeModels,
    models,
    quickStartModel,
    siteName,
  ]);

  const modelEndpoints = useMemo(
    () => buildEndpointSpecs(baseUrl, docsLocale),
    [baseUrl, docsLocale],
  );
  const adminEndpoints = useMemo(
    () => buildAdminSpecs(baseUrl, docsLocale),
    [baseUrl, docsLocale],
  );

  const tocItems: DocsTOCItem[] = useMemo(
    () => [
      {
        id: "quick-start",
        label: t("docs.toc.quickStart"),
        children: [
          { id: "qs-tools", label: t("docs.toc.qsTools") },
          { id: "qs-curl", label: t("docs.toc.qsCurl") },
        ],
      },
      {
        id: "client-config",
        label: t("docs.toc.clientConfig"),
        children: [
          { id: "client-codex", label: "Codex CLI" },
          { id: "client-claude", label: "Claude Code" },
          { id: "client-mapping", label: t("docs.toc.modelMapping") },
        ],
      },
      {
        id: "authentication",
        label: t("docs.toc.authentication"),
      },
      {
        id: "model-api",
        label: t("docs.toc.modelApi"),
        children: modelEndpoints.map((e) => ({
          id: e.id,
          label: e.path,
          method: e.method,
        })),
      },
      {
        id: "admin-api",
        label: t("docs.toc.adminApi"),
        children: adminEndpoints.map((e) => ({
          id: e.id,
          label: e.path,
          method: e.method,
        })),
      },
    ],
    [t, modelEndpoints, adminEndpoints],
  );

  const handleCopyMarkdown = async () => {
    setCopyingMd(true);
    const md = buildDocsMarkdown({
      baseUrl: quickBaseUrl,
      quickTools,
      apiKeyExample: firstKey || "YOUR_API_KEY",
      locale: docsLocale,
    });
    try {
      await navigator.clipboard.writeText(md);
      showToast(t("docs.markdownCopied"), "success");
    } catch {
      const ta = document.createElement("textarea");
      ta.value = md;
      ta.style.cssText = "position:fixed;left:-9999px";
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
      showToast(t("docs.markdownCopied"), "success");
    } finally {
      setTimeout(() => setCopyingMd(false), 1200);
    }
  };

  const codexConfigDir =
    codexOs === "windows" ? "%userprofile%\\.codex" : "~/.codex";
  const claudeConfigDir =
    claudeOs === "windows" ? "%userprofile%\\.claude" : "~/.claude";
  const codexConfigPath =
    codexOs === "windows"
      ? `${codexConfigDir}\\config.toml`
      : `${codexConfigDir}/config.toml`;
  const codexAuthPath =
    codexOs === "windows"
      ? `${codexConfigDir}\\auth.json`
      : `${codexConfigDir}/auth.json`;
  const claudeSettingsPath =
    claudeOs === "windows"
      ? `${claudeConfigDir}\\settings.json`
      : `${claudeConfigDir}/settings.json`;
  const activeKey = selectedKey || firstKey || "YOUR_API_KEY";

  const codexServiceTierLine =
    quickServiceTier === "fast" ? '\nservice_tier = "fast"' : "";
  const codexConfigToml = `model_provider = "OpenAI"
model = "${quickStartModel}"
review_model = "${quickStartModel}"
model_reasoning_effort = "${quickReasoningEffort}"${codexServiceTierLine}
disable_response_storage = true
network_access = "enabled"

[model_providers.OpenAI]
name = "OpenAI"
base_url = "${quickBaseUrl}"
wire_api = "responses"
requires_openai_auth = true

[features]
goals = true`;

  const codexAuthJson = `{
  "OPENAI_API_KEY": "${activeKey}"
}`;

  const claudeSettingsJson = `{
  "env": {
    "ANTHROPIC_BASE_URL": "${quickBaseUrl}",
    "ANTHROPIC_AUTH_TOKEN": "${activeKey}",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }
}`;

  const claudeEnvUnix = `export ANTHROPIC_BASE_URL="${quickBaseUrl}"
export ANTHROPIC_AUTH_TOKEN="${activeKey}"
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`;

  const claudeEnvWindows = `set ANTHROPIC_BASE_URL=${quickBaseUrl}
set ANTHROPIC_AUTH_TOKEN=${activeKey}
set CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`;

  const responsesCurl = `curl -X POST ${quickBaseUrl}/v1/responses \\
  -H "Authorization: Bearer ${activeKey}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${curlModel}",
    "input": [{"role": "user", "content": [{"type": "input_text", "text": "Hello"}]}],
    "stream": true
  }'`;
  const chatCurl = `curl -X POST ${quickBaseUrl}/v1/chat/completions \\
  -H "Authorization: Bearer ${activeKey}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${curlModel}",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'`;
  const messagesCurl = `curl -X POST ${quickBaseUrl}/v1/messages \\
  -H "x-api-key: ${activeKey}" \\
  -H "Content-Type: application/json" \\
  -H "anthropic-version: 2023-06-01" \\
  -d '{
    "model": "${curlModel.startsWith("claude-") ? curlModel : "claude-sonnet-4-5-20250514"}",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'`;
  const curlExamples = {
    responses: responsesCurl,
    chat: chatCurl,
    messages: messagesCurl,
  };
  const ccSwitchUrl = buildCCSwitchImportUrl({
    app: ccSwitchApp,
    name: ccSwitchName,
    endpoint: ccSwitchConfig.endpoint(quickBaseUrl),
    apiKey: activeKey,
    models: ccSwitchModels,
    homepage: quickBaseUrl,
    serviceTier: ccSwitchApp === "codex" ? quickServiceTier : "default",
    reasoningEffort:
      ccSwitchApp === "codex" ? quickReasoningEffort : undefined,
  });
  const cherryConfig = `cherrystudio://providers/api-keys?v=1&data=${encodeURIComponent(
    encodeBase64(
      JSON.stringify({
        id: cherryProviderId || defaultCherryProviderId,
        baseUrl: quickBaseUrl,
        apiKey: activeKey,
      }),
    ),
  )}`;
  const hasUsableKey = Boolean(selectedKey || firstKey);
  const handleImportLinkCopied = (name: string) => {
    showToast(t("docs.quickStart.copiedToast", { name }), "success");
  };
  return (
    <>
      <div className="mb-4 max-w-[760px]">
        <div className="max-w-[760px]">
          <h2 className="text-2xl font-semibold leading-tight text-foreground sm:text-[28px]">
            {t("docs.title")}
          </h2>
          <p className="mt-2 max-w-[640px] text-sm leading-relaxed text-muted-foreground">
            {t("docs.description")}
          </p>
        </div>
        <div className="mt-3 xl:hidden">
          <Button
            variant="outline"
            size="sm"
            onClick={() => void handleCopyMarkdown()}
            disabled={copyingMd}
            className="gap-1.5"
          >
            {copyingMd ? (
              <Check className="size-3.5 text-emerald-500" />
            ) : (
              <Copy className="size-3.5" />
            )}
            {t("docs.copyMarkdown")}
          </Button>
        </div>
      </div>


      <div className="xl:hidden mb-3 -mx-2 overflow-x-auto px-2">
        <div className="flex gap-1.5 pb-1">
          {tocItems.map((parent) => (
            <a
              key={parent.id}
              href={`#${parent.id}`}
              className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-border bg-background px-2.5 py-1.5 text-[12px] font-semibold text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
            >
              {parent.label}
            </a>
          ))}
        </div>
      </div>

      <SectionHeader
        id="quick-start"
        icon={SECTION_ICON["quick-start"]}
        tone={SECTION_TONE["quick-start"]}
        eyebrow={t("docs.section1Eyebrow")}
        title={t("docs.quickStart.title")}
        description={t("docs.quickStart.description")}
      />

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_260px]">
        <div className="min-w-0">
          <Card id="qs-tools" className="mb-4 scroll-mt-20 py-0">
            <CardContent className="p-5">
              <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
                <div className="min-w-0">
                  <h3 className="text-[15px] font-semibold text-foreground">
                    {t("docs.quickStart.toolsTitle")}
                  </h3>
                  <p className="mt-0.5 text-[12.5px] text-muted-foreground">
                    {t("docs.quickStart.toolsDesc")}
                  </p>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  {allKeys.length > 0 ? (
                    <>
                      <span className="text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
                        {t("docs.quickStart.useKey")}
                      </span>
                      <Select
                        compact
                        className="w-44"
                        value={selectedKey}
                        onValueChange={setSelectedKey}
                        options={allKeys.map((k) => ({
                          label: k.name
                            ? `${k.name} · ${k.key.slice(0, 6)}…${k.key.slice(-4)}`
                            : k.key,
                          value: k.key,
                        }))}
                      />
                    </>
                  ) : (
                    <a
                      href="/admin/api-keys"
                      className="inline-flex items-center gap-1 rounded-md border border-amber-500/30 bg-amber-500/10 px-2.5 py-1 text-[11px] font-bold text-amber-600 dark:text-amber-400"
                    >
                      {t("docs.quickStart.createKeyFirst")}
                    </a>
                  )}
                </div>
              </div>
              <div className="mb-3 rounded-xl border border-border bg-muted/25 p-1">
                <div className="grid gap-1 sm:grid-cols-4">
                  {[
                    { value: "codex-cli", label: "Codex CLI" },
                    { value: "claude-code", label: "Claude Code" },
                    { value: "cc-switch", label: "CC Switch" },
                    { value: "cherry-studio", label: "Cherry Studio" },
                  ].map((tab) => (
                    <ToolTabButton
                      key={tab.value}
                      selected={activeToolTab === tab.value}
                      id={tab.value as QuickToolTab}
                      label={tab.label}
                      onClick={() =>
                        setActiveToolTab(tab.value as QuickToolTab)
                      }
                    />
                  ))}
                </div>
              </div>
              <div
                className={`mb-4 ${CONFIG_PANEL} ${
                  showQuickCodexOptions
                    ? "md:grid-cols-2 xl:grid-cols-[minmax(0,1fr)_190px_170px_150px]"
                    : "md:grid-cols-[minmax(0,1fr)_260px]"
                }`}
              >
                <FieldBox label={t("docs.clientConfig.endpointLabel")}>
                  <input
                    className={`${FIELD_INPUT} font-mono`}
                    value={
                      activeToolTab === "cc-switch"
                        ? ccSwitchConfig.endpoint(quickBaseUrl)
                        : quickBaseUrl
                    }
                    onChange={(event) => {
                      const value = event.target.value;
                      if (
                        activeToolTab === "cc-switch" &&
                        ccSwitchApp === "codex" &&
                        value.endsWith("/v1")
                      ) {
                        setQuickBaseUrl(value.slice(0, -3));
                      } else {
                        setQuickBaseUrl(value);
                      }
                    }}
                  />
                </FieldBox>
                <FieldBox label={t("docs.clientConfig.defaultModel")}>
                  <Select
                    compact
                    value={quickStartModel}
                    onValueChange={setQuickStartModel}
                    options={modelOptions}
                  />
                </FieldBox>
                {showQuickCodexOptions ? (
                  <FieldBox label={t("docs.clientConfig.reasoningEffort")}>
                    <Select
                      compact
                      value={quickReasoningEffort}
                      onValueChange={(value) =>
                        setQuickReasoningEffort(value as QuickReasoningEffort)
                      }
                      options={[
                        { label: "None", value: "none" },
                        { label: "Minimal", value: "minimal" },
                        { label: "Low", value: "low" },
                        { label: "Medium", value: "medium" },
                        { label: "High", value: "high" },
                        { label: "xhigh", value: "xhigh" },
                        { label: "ultra", value: "ultra" },
                      ]}
                    />
                  </FieldBox>
                ) : null}
                {showQuickCodexOptions ? (
                  <FieldBox label={t("docs.clientConfig.fastMode")}>
                    <Select
                      compact
                      value={quickServiceTier}
                      onValueChange={(value) =>
                        setQuickServiceTier(value as QuickServiceTier)
                      }
                      options={[
                        {
                          label: t("docs.clientConfig.fastModeDefault"),
                          value: "default",
                        },
                        {
                          label: t("docs.clientConfig.fastModeEnabled"),
                          value: "fast",
                        },
                      ]}
                    />
                  </FieldBox>
                ) : null}
              </div>
              <div className="space-y-4">
                {activeToolTab === "codex-cli" && (
                  <>
                    <OsTabs active={codexOs} onChange={setCodexOs} />
                    <CodeBlock
                      label={codexConfigPath}
                      content={codexConfigToml}
                      lang="toml"
                    />
                    <CodeBlock
                      label={codexAuthPath}
                      content={codexAuthJson}
                      lang="json"
                    />
                  </>
                )}
                {activeToolTab === "claude-code" && (
                  <>
                    <OsTabs active={claudeOs} onChange={setClaudeOs} />
                    <CodeBlock
                      label={
                        claudeOs === "unix"
                          ? t("docs.clientConfig.unixTerminal")
                          : t("docs.clientConfig.windowsTerminal")
                      }
                      content={
                        claudeOs === "unix" ? claudeEnvUnix : claudeEnvWindows
                      }
                      lang="bash"
                    />
                    <CodeBlock
                      label={claudeSettingsPath}
                      content={claudeSettingsJson}
                      lang="json"
                    />
                  </>
                )}
                {activeToolTab === "cc-switch" && (
                  <>
                    <div className={`${CONFIG_PANEL} md:grid-cols-2`}>
                      <FieldBox label={t("docs.clientConfig.importTarget")}>
                        <Select
                          compact
                          value={ccSwitchApp}
                          onValueChange={(value) =>
                            setCcSwitchApp(value as CCSwitchApp)
                          }
                          options={(
                            Object.keys(CC_SWITCH_APPS) as CCSwitchApp[]
                          ).map((app) => ({
                            label: CC_SWITCH_APPS[app].label,
                            value: app,
                          }))}
                        />
                      </FieldBox>
                      <FieldBox label={t("docs.clientConfig.configName")}>
                        <input
                          className={FIELD_INPUT}
                          value={ccSwitchName}
                          onChange={(event) => {
                            setCcSwitchNameEdited(true);
                            setCcSwitchName(event.target.value);
                          }}
                        />
                      </FieldBox>
                      {ccSwitchConfig.fields.map((field) => (
                        <FieldBox
                          key={field.key}
                          label={t(
                            `docs.clientConfig.ccSwitchFields.${field.key}`,
                          )}
                        >
                          <div className="space-y-1.5">
                            <Select
                              compact
                              value={ccSwitchModels[field.key] || ""}
                              onValueChange={(value) =>
                                setCcSwitchModels((current) => ({
                                  ...current,
                                  [field.key]: value,
                                }))
                              }
                              options={ccSwitchModelOptions}
                            />
                            {ccSwitchApp === "claude" &&
                            ccSwitchModels[field.key] ? (
                              <div className="truncate text-[11px] font-medium text-muted-foreground">
                                {t("docs.clientConfig.mappedTo")}{" "}
                                <code className="font-mono text-foreground">
                                  {modelMapping[ccSwitchModels[field.key]] ??
                                    t("docs.clientConfig.backendDefaultModel")}
                                </code>
                              </div>
                            ) : null}
                          </div>
                        </FieldBox>
                      ))}
                    </div>
                    <ImportPreviewCard
                      title={t("docs.clientConfig.ccSwitchPreviewTitle")}
                      description={t("docs.clientConfig.ccSwitchPreviewDesc")}
                      icon={<ClientIcon id="cc-switch" size={20} />}
                      link={ccSwitchUrl}
                      disabled={!hasUsableKey}
                      onCopied={() => handleImportLinkCopied("CC Switch")}
                      onLaunch={() => {
                        window.open(ccSwitchUrl, "_blank");
                        showToast(
                          t("docs.quickStart.launchedToast", {
                            name: "CC Switch",
                          }),
                          "success",
                        );
                      }}
                    />
                  </>
                )}
                {activeToolTab === "cherry-studio" && (
                  <>
                    <div className={`${CONFIG_PANEL} md:grid-cols-2`}>
                      <FieldBox label={t("docs.clientConfig.importTarget")}>
                        <code
                          className={`${FIELD_INPUT} flex items-center truncate font-mono`}
                        >
                          Cherry Studio
                        </code>
                      </FieldBox>
                      <FieldBox label={t("docs.clientConfig.providerId")}>
                        <input
                          className={`${FIELD_INPUT} font-mono`}
                          value={cherryProviderId}
                          onChange={(event) => {
                            setCherryProviderEdited(true);
                            setCherryProviderId(event.target.value);
                          }}
                        />
                      </FieldBox>
                    </div>
                    <ImportPreviewCard
                      title={t("docs.clientConfig.cherryPreviewTitle")}
                      description={t("docs.clientConfig.cherryPreviewDesc")}
                      icon={<ClientIcon id="cherry-studio" size={20} />}
                      link={cherryConfig}
                      disabled={!hasUsableKey}
                      onCopied={() => handleImportLinkCopied("Cherry Studio")}
                      onLaunch={() => {
                        window.open(cherryConfig, "_blank");
                        showToast(
                          t("docs.quickStart.launchedToast", {
                            name: "Cherry Studio",
                          }),
                          "success",
                        );
                      }}
                    />
                  </>
                )}
              </div>
            </CardContent>
          </Card>

          <Card id="qs-curl" className="mb-4 scroll-mt-20 py-0">
            <CardContent className="p-6">
              <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
                <div className="min-w-0">
                  <h3 className="text-base font-semibold text-foreground mb-1">
                    {t("docs.quickStart.curlTitle")}
                  </h3>
                  <p className="text-sm text-muted-foreground">
                    {t("docs.quickStart.curlDesc")}
                  </p>
                </div>
                <Select
                  compact
                  className="w-52"
                  value={curlModel}
                  onValueChange={setCurlModel}
                  options={[
                    ...modelOptions,
                    ...claudeModelOptions.filter(
                      (option) => !models.includes(option.value),
                    ),
                  ]}
                />
              </div>
              <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
                <UnderlineTabs
                  active={activeCurl}
                  onChange={setActiveCurl}
                  tabs={[
                    {
                      value: "responses",
                      label: "Responses",
                      hint: "/v1/responses",
                    },
                    {
                      value: "chat",
                      label: "Chat",
                      hint: "/v1/chat/completions",
                    },
                    {
                      value: "messages",
                      label: "Messages",
                      hint: "/v1/messages",
                    },
                  ]}
                />
                <code className="code-inline text-[11px]">
                  {activeCurl === "responses"
                    ? "/v1/responses"
                    : activeCurl === "chat"
                      ? "/v1/chat/completions"
                      : "/v1/messages"}
                </code>
              </div>
              <CodeBlock
                label="cURL"
                content={curlExamples[activeCurl]}
                lang="bash"
              />
            </CardContent>
          </Card>

          {/* Section 2: Client Config */}
          <SectionHeader
            id="client-config"
            icon={SECTION_ICON["client-config"]}
            tone={SECTION_TONE["client-config"]}
            eyebrow={t("docs.section2Eyebrow")}
            title={t("docs.clientConfig.title")}
            description={t("docs.clientConfig.description")}
          />

          <Card id="client-codex" className="mb-4 scroll-mt-20 py-0">
            <CardContent className="p-6">
              <h3 className="text-base font-semibold text-foreground mb-1">
                Codex CLI
              </h3>
              <p className="mb-4 text-sm text-muted-foreground">
                {t("docs.clientConfig.codexDesc")}
              </p>
              <OsTabs active={codexOs} onChange={setCodexOs} />
              <p className="mb-3 flex items-center gap-1 text-xs text-amber-600 dark:text-amber-400">
                ⓘ {t("docs.clientConfig.codexConfigHint")}
              </p>
              <div className="space-y-4">
                <CodeBlock
                  label={codexConfigPath}
                  content={codexConfigToml}
                  lang="toml"
                />
                <CodeBlock
                  label={codexAuthPath}
                  content={codexAuthJson}
                  lang="json"
                />
              </div>
              <p className="mt-3 text-xs text-muted-foreground">
                {codexOs === "windows"
                  ? t("docs.clientConfig.codexNoteWindows")
                  : t("docs.clientConfig.codexNoteUnix")}
              </p>
            </CardContent>
          </Card>

          <Card id="client-claude" className="mb-4 scroll-mt-20 py-0">
            <CardContent className="p-6">
              <h3 className="text-base font-semibold text-foreground mb-1">
                Claude Code
              </h3>
              <p className="mb-4 text-sm text-muted-foreground">
                {t("docs.clientConfig.claudeDesc")}
              </p>
              <OsTabs active={claudeOs} onChange={setClaudeOs} />
              <div className="space-y-4">
                <CodeBlock
                  label={
                    claudeOs === "unix"
                      ? t("docs.clientConfig.unixTerminal")
                      : t("docs.clientConfig.windowsTerminal")
                  }
                  content={
                    claudeOs === "unix" ? claudeEnvUnix : claudeEnvWindows
                  }
                  lang="bash"
                />
                <p className="text-xs text-muted-foreground">
                  {t("docs.clientConfig.claudeEnvNote")}
                </p>
                <CodeBlock
                  label={claudeSettingsPath}
                  content={claudeSettingsJson}
                  lang="json"
                />
                <p className="text-xs text-muted-foreground">
                  {t("docs.clientConfig.claudeSettingsNote")}
                </p>
              </div>
            </CardContent>
          </Card>

          <Card id="client-mapping" className="mb-4 scroll-mt-20 py-0">
            <CardContent className="p-6">
              <h3 className="text-base font-semibold text-foreground mb-1">
                {t("docs.clientConfig.mappingTitle")}
              </h3>
              <p className="text-sm text-muted-foreground">
                {t("docs.clientConfig.mappingDesc")}
              </p>
            </CardContent>
          </Card>

          {/* Section 3: Authentication */}
          <SectionHeader
            id="authentication"
            icon={SECTION_ICON["authentication"]}
            tone={SECTION_TONE["authentication"]}
            eyebrow={t("docs.section3Eyebrow")}
            title={t("docs.authentication.title")}
            description={t("docs.authentication.description")}
          />

          <Card className="mb-6 scroll-mt-20 py-0">
            <CardContent className="p-6">
              <div className="space-y-2.5">
                <div className="flex items-center gap-2.5 px-3.5 py-2.5 rounded-xl bg-muted/40 border border-border">
                  <Badge
                    variant="outline"
                    className="text-[10px] font-bold shrink-0"
                  >
                    Header
                  </Badge>
                  <code className="code-inline">
                    Authorization: Bearer{" "}
                    <span className="text-muted-foreground italic">
                      &lt;key&gt;
                    </span>
                  </code>
                  <span className="ml-auto text-xs text-muted-foreground">
                    {t("docs.authentication.bearerNote")}
                  </span>
                </div>
                <div className="flex items-center gap-2.5 px-3.5 py-2.5 rounded-xl bg-muted/40 border border-border">
                  <Badge
                    variant="outline"
                    className="text-[10px] font-bold shrink-0"
                  >
                    Header
                  </Badge>
                  <code className="code-inline">
                    x-api-key:{" "}
                    <span className="text-muted-foreground italic">
                      &lt;key&gt;
                    </span>
                  </code>
                  <span className="ml-auto text-xs text-muted-foreground">
                    {t("docs.authentication.xApiKeyNote")}
                  </span>
                </div>
                <div className="flex items-center gap-2.5 px-3.5 py-2.5 rounded-xl bg-muted/40 border border-border">
                  <Badge
                    variant="outline"
                    className="text-[10px] font-bold shrink-0"
                  >
                    Header
                  </Badge>
                  <code className="code-inline">
                    X-Admin-Key:{" "}
                    <span className="text-muted-foreground italic">
                      &lt;admin_secret&gt;
                    </span>
                  </code>
                  <span className="ml-auto text-xs text-muted-foreground">
                    {t("docs.authentication.adminNote")}
                  </span>
                </div>
              </div>
            </CardContent>
          </Card>

          {/* Section 4: Model API */}
          <SectionHeader
            id="model-api"
            icon={SECTION_ICON["model-api"]}
            tone={SECTION_TONE["model-api"]}
            eyebrow={t("docs.section4Eyebrow")}
            title={t("docs.modelApi.title")}
            description={t("docs.modelApi.description")}
          />

          {modelEndpoints.map((endpoint) => (
            <EndpointDoc
              key={endpoint.id}
              id={endpoint.id}
              method={endpoint.method}
              path={endpoint.path}
              title={endpoint.title}
              description={endpoint.description}
              curlExample={endpoint.curl}
              defaultBody={endpoint.defaultBody}
              responseExamples={endpoint.responses}
              apiKey={activeKey}
              baseUrl={baseUrl}
              allKeys={allKeys}
            />
          ))}

          {/* Section 5: Admin API */}
          <SectionHeader
            id="admin-api"
            icon={SECTION_ICON["admin-api"]}
            tone={SECTION_TONE["admin-api"]}
            eyebrow={t("docs.section5Eyebrow")}
            title={t("docs.adminApi.title")}
            description={t("docs.adminApi.description")}
          />

          {adminEndpoints.map((endpoint) => (
            <EndpointDoc
              key={endpoint.id}
              id={endpoint.id}
              method={endpoint.method}
              path={endpoint.path}
              title={endpoint.title}
              description={endpoint.description}
              curlExample={endpoint.curl}
              defaultBody={endpoint.defaultBody}
              responseExamples={endpoint.responses}
              apiKey={adminSeed}
              baseUrl={baseUrl}
              allKeys={[]}
            />
          ))}
        </div>

        {/*
          sticky + self-start on the grid item:
          - self-start: aside height = content (not stretched), required for sticky to pin
          - sticky top-4: stays in the viewport while the left column scrolls to the end
        */}
        <aside className="sticky top-4 z-10 hidden max-h-[calc(100dvh-2rem)] self-start xl:block">
          <div className="flex h-full max-h-[calc(100dvh-2rem)] flex-col gap-3">
            <div className="flex shrink-0 justify-end">
              <Button
                variant="outline"
                size="sm"
                onClick={() => void handleCopyMarkdown()}
                disabled={copyingMd}
                className="gap-1.5"
              >
                {copyingMd ? (
                  <Check className="size-3.5 text-emerald-500" />
                ) : (
                  <Copy className="size-3.5" />
                )}
                {t("docs.copyMarkdown")}
              </Button>
            </div>
            <div className="min-h-0 flex-1 overflow-hidden">
              <DocsTOC items={tocItems} title={t("docs.tocTitle")} />
            </div>
          </div>
        </aside>
      </div>
    </>
  );
}
