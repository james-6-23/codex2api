import type { ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

// 与 Codex「编辑账号配置」同款的分组标题与卡片,供其他渠道的账号编辑弹窗复用。
export function ConfigCategory({
  icon: Icon,
  title,
  children,
}: {
  icon: LucideIcon;
  title: string;
  children: ReactNode;
}) {
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2 pt-1 text-xs font-bold uppercase tracking-wider text-muted-foreground/80">
        <Icon className="size-3.5 text-primary" />
        <span>{title}</span>
        <div className="h-px flex-1 bg-border/60" />
      </div>
      <div className="grid gap-4 md:grid-cols-2">{children}</div>
    </div>
  );
}

export function ConfigCard({
  icon: Icon,
  iconClassName,
  title,
  hint,
  action,
  wide = false,
  className,
  children,
}: {
  icon?: LucideIcon;
  iconClassName?: string;
  title: string;
  hint?: ReactNode;
  /** 标题右侧的控件(开关等)。 */
  action?: ReactNode;
  /** 在两列网格里占满一行。 */
  wide?: boolean;
  className?: string;
  children?: ReactNode;
}) {
  return (
    <section
      className={cn(
        "rounded-xl border border-border/70 bg-card p-4.5 shadow-2xs transition-colors hover:border-border/90",
        wide && "md:col-span-2",
        className,
      )}
    >
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0 space-y-1">
          <h3 className="flex items-center gap-2 text-sm font-semibold text-foreground">
            {Icon ? <Icon className={cn("size-4 shrink-0", iconClassName ?? "text-primary")} /> : null}
            <span>{title}</span>
          </h3>
          {hint ? <p className="text-xs leading-relaxed text-muted-foreground">{hint}</p> : null}
        </div>
        {action}
      </div>
      {children ? <div className="mt-3.5 space-y-3">{children}</div> : null}
    </section>
  );
}
