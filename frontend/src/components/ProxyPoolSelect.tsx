import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ChevronDown, MapPin, Check } from "lucide-react";

import type { ProxyRow } from "../api";
import { cn } from "@/lib/utils";

interface ProxyPoolSelectProps {
  proxies: ProxyRow[];
  onSelect: (url: string) => void;
  /** 当前已选代理 URL(受控回显);与某条代理 URL 一致时该项高亮并在触发器回显。 */
  value?: string;
  disabled?: boolean;
  className?: string;
}

// ProxyPoolSelect 是账号表单里"从代理池选一条代理"的下拉。
// 自定义渲染:每项用徽章展示 📍地点 + 空闲(绿)/已绑定 N(琥珀),空闲优先置顶;
// 选中后触发器回显所选代理(label/url + 地点),让用户明确知道选了哪条。
export function ProxyPoolSelect({
  proxies,
  onSelect,
  value,
  disabled = false,
  className,
}: ProxyPoolSelectProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onEsc);
    };
  }, [open]);

  // 空闲(bound_count=0)优先,其余按绑定数升序,负载最轻的排前面。
  const sorted = useMemo(
    () => [...proxies].sort((a, b) => (a.bound_count ?? 0) - (b.bound_count ?? 0)),
    [proxies],
  );
  const selected = useMemo(
    () => (value ? proxies.find((p) => p.url === value.trim()) : undefined),
    [proxies, value],
  );

  if (proxies.length === 0) return null;

  const IdleBadge = () => (
    <span className="inline-flex shrink-0 items-center rounded-full bg-emerald-500/12 px-1.5 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400">
      {t("proxies.idle")}
    </span>
  );
  const BoundBadge = ({ count }: { count: number }) => (
    <span className="inline-flex shrink-0 items-center rounded-full bg-amber-500/12 px-1.5 py-0.5 text-[10px] font-medium text-amber-600 dark:text-amber-400">
      {t("proxies.boundCount", { count })}
    </span>
  );
  const LocationTag = ({ loc }: { loc: string }) => (
    <span className="inline-flex shrink-0 items-center gap-0.5 text-[10px] text-muted-foreground">
      <MapPin className="size-2.5" />
      {loc}
    </span>
  );

  const selectedLoc = selected?.test_location?.trim();

  return (
    <div ref={rootRef} className={cn("relative", className)}>
      <button
        type="button"
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className={cn(
          "flex h-9 w-full items-center justify-between gap-2 rounded-md border border-input bg-background px-2.5 text-left text-sm outline-none transition-colors focus-visible:border-ring disabled:opacity-50",
          open && "border-ring",
        )}
      >
        <span className="flex min-w-0 items-center gap-1.5">
          {selected ? (
            <>
              <span className="truncate text-foreground">{selected.label?.trim() || selected.url}</span>
              {selectedLoc ? <LocationTag loc={selectedLoc} /> : null}
              {(selected.bound_count ?? 0) === 0 ? <IdleBadge /> : <BoundBadge count={selected.bound_count ?? 0} />}
            </>
          ) : (
            <span className="truncate text-muted-foreground">{t("proxies.selectFromPool")}</span>
          )}
        </span>
        <ChevronDown className={cn("size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-180")} />
      </button>

      {open ? (
        <div className="absolute left-0 right-0 top-full z-50 mt-1 max-h-64 overflow-y-auto rounded-lg border border-border bg-popover p-1 shadow-lg">
          {sorted.map((proxy) => {
            const count = proxy.bound_count ?? 0;
            const loc = proxy.test_location?.trim();
            const name = proxy.label?.trim() || proxy.url;
            const active = selected?.url === proxy.url;
            return (
              <button
                key={proxy.url}
                type="button"
                onClick={() => {
                  onSelect(proxy.url);
                  setOpen(false);
                }}
                className={cn(
                  "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left transition-colors hover:bg-muted",
                  active && "bg-primary/8",
                )}
              >
                <span className="flex w-4 shrink-0 justify-center">
                  {active ? <Check className="size-3.5 text-primary" /> : null}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="flex items-center gap-1.5">
                    <span className="truncate text-[13px] font-medium text-foreground">{name}</span>
                    {count === 0 ? <IdleBadge /> : <BoundBadge count={count} />}
                  </span>
                  <span className="flex items-center gap-2 text-[11px] text-muted-foreground">
                    {loc ? <LocationTag loc={loc} /> : null}
                    <span className="truncate font-mono opacity-80">{proxy.url}</span>
                  </span>
                </span>
              </button>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}
