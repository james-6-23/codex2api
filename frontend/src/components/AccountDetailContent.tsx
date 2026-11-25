import type { ReactNode } from "react";
import {
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetBody,
} from "@/components/ui/sheet";

// Data and actions belong to the caller. This view can render a key-scoped
// account without constructing a management AccountRow or loading admin data.
export function AccountDetailContent({
  id,
  sequence,
  name,
  plan,
  icon,
  titleAction,
  description,
  navigation,
  children,
}: {
  id: number;
  sequence?: number;
  name: string;
  plan?: string;
  icon?: ReactNode;
  titleAction?: ReactNode;
  description: ReactNode;
  navigation?: ReactNode;
  children: ReactNode;
}) {
  return (
    <SheetContent
      side="right"
      className="w-[min(calc(100%-1.5rem),460px)] max-w-[min(calc(100%-1.5rem),460px)] sm:w-[min(calc(100%-2rem),460px)] sm:max-w-[min(calc(100%-2rem),460px)]"
    >
      <SheetHeader>
        <div className="flex items-start justify-between gap-3 pr-2">
          <div className="flex min-w-0 flex-1 items-start gap-3">
            {icon && (
              <span className="mt-0.5 flex size-10 shrink-0 items-center justify-center overflow-hidden rounded-xl bg-card ring-1 ring-border shadow-sm">
                {icon}
              </span>
            )}
            <div className="min-w-0 flex-1">
              <div className="mb-1.5 flex flex-wrap items-center gap-1.5">
                {sequence != null && (
                  <span className="rounded-md bg-muted px-1.5 py-0.5 font-mono text-[11px] font-semibold text-muted-foreground">
                    #{sequence}
                  </span>
                )}
                <span className="rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">
                  ID {id}
                </span>
                {plan && (
                  <span className="rounded-md bg-primary/10 px-1.5 py-0.5 text-[11px] font-semibold text-primary">
                    {plan}
                  </span>
                )}
              </div>
              <div className="flex items-start gap-1.5">
                <SheetTitle className="break-all text-[17px] leading-snug">
                  {name}
                </SheetTitle>
                {titleAction}
              </div>
              {description}
            </div>
          </div>
          {navigation}
        </div>
      </SheetHeader>

      {children}
    </SheetContent>
  );
}

export function AccountDetailBody({ children }: { children: ReactNode }) {
  return <SheetBody className="space-y-5">{children}</SheetBody>;
}

export function AccountDetailSection({
  title,
  children,
  action,
}: {
  title: string;
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <section className="space-y-2.5">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
          {title}
        </h3>
        {action}
      </div>
      {children}
    </section>
  );
}

export function AccountDetailMetric({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="min-w-0 rounded-lg border border-border bg-muted/20 px-3 py-2.5">
      <div className="mb-1 text-[11px] font-medium text-muted-foreground">
        {label}
      </div>
      <div className="min-w-0 text-sm text-foreground">{children}</div>
    </div>
  );
}
