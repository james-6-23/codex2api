import { useTranslation } from "react-i18next";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";

export function normalizePlanType(planType?: string): string {
  const raw = (planType || "").toLowerCase().trim();
  if (raw === "prolite" || raw === "pro_lite" || raw === "pro-lite")
    return "pro";
  if (raw === "promax" || raw === "pro_max" || raw === "pro-max")
    return "pro";
  if (
    raw === "ent26" ||
    raw === "enterprise_cbp_usage_based" ||
    raw === "enterprise_cbp_automation"
  )
    return "enterprise";
  if (raw === "edu_plus" || raw === "edu_pro") return "edu";
  return raw;
}

function formatPlanLabel(planType?: string): string {
  const raw = (planType || "").trim();
  if (!raw) return "-";
  const lower = raw.toLowerCase();
  if (lower === "prolite" || lower === "pro_lite" || lower === "pro-lite")
    return "ProLite";
  if (lower === "promax" || lower === "pro_max" || lower === "pro-max")
    return "ProMax";
  if (lower === "self_serve_business_prolite") return "team5x";
  if (lower === "self_serve_business_usage_based") return "Business";
  if (lower === "ent26" || lower === "enterprise_cbp_usage_based")
    return "Enterprise";
  if (lower === "enterprise_cbp_automation") return "Enterprise (Automation)";
  if (lower === "edu_plus") return "Edu Plus";
  if (lower === "edu_pro") return "Edu Pro";
  return raw;
}

function isWorkspacePlan(planType?: string): boolean {
  const normalized = normalizePlanType(planType);
  return (
    normalized === "team" ||
    normalized === "teamplus" ||
    normalized === "k12" ||
    normalized === "edu" ||
    normalized === "education"
  );
}

export default function PlanBadge({
  planType,
  workspaceId,
}: {
  planType?: string;
  workspaceId?: string;
}) {
  const { t } = useTranslation();
  const label = formatPlanLabel(planType);
  if (label === "-")
    return <span className="text-[12px] text-muted-foreground">-</span>;

  const style: Record<string, string> = {
    pro: "bg-violet-100 text-violet-700 ring-violet-500/30 dark:bg-violet-500/20 dark:text-violet-300 dark:ring-violet-400/30",
    prolite:
      "bg-purple-50 text-purple-600 ring-purple-400/25 dark:bg-purple-500/15 dark:text-purple-300 dark:ring-purple-400/25",
    promax:
      "bg-fuchsia-100 text-fuchsia-700 ring-fuchsia-500/35 dark:bg-fuchsia-500/20 dark:text-fuchsia-300 dark:ring-fuchsia-400/35",
    plus: "bg-blue-100 text-blue-700 ring-blue-500/30 dark:bg-blue-500/20 dark:text-blue-300 dark:ring-blue-400/30",
    team: "bg-amber-100 text-amber-700 ring-amber-500/30 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/30",
    k12: "bg-emerald-100 text-emerald-700 ring-emerald-500/30 dark:bg-emerald-500/20 dark:text-emerald-300 dark:ring-emerald-400/30",
    free: "bg-zinc-100 text-zinc-500 ring-zinc-400/20 dark:bg-zinc-500/10 dark:text-zinc-400 dark:ring-zinc-400/15",
  };

  const normalized = normalizePlanType(planType);
  const key =
    normalized === "pro" && label === "ProLite"
      ? "prolite"
      : normalized === "pro" && label === "ProMax"
        ? "promax"
        : label === "team5x"
        ? "team"
        : normalized;
  const cls =
    style[key] ||
    "bg-slate-100 text-slate-600 ring-slate-400/20 dark:bg-slate-500/15 dark:text-slate-300 dark:ring-slate-400/20";
  const trimmedWorkspaceId = workspaceId?.trim() ?? "";
  const badge = (
    <span
      className={`inline-flex min-w-0 max-w-full items-center truncate rounded-md px-2.5 py-1 text-[13px] font-semibold ring-1 ring-inset ${cls}`}
    >
      {label}
    </span>
  );
  if (!isWorkspacePlan(planType) || !trimmedWorkspaceId) {
    return badge;
  }

  return (
    <TooltipProvider delayDuration={0} skipDelayDuration={0}>
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex min-w-0 max-w-full cursor-help">
            {badge}
          </span>
        </TooltipTrigger>
        <TooltipContent
          side="top"
          sideOffset={6}
          className="max-w-[360px] font-mono text-[11px]"
        >
          {t("accounts.planWorkspaceId", { id: trimmedWorkspaceId })}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
