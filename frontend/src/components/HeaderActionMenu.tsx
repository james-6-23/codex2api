import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ChevronDown } from "lucide-react";
import { DropdownMenu } from "radix-ui";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { applyManualScroll, applyWheelScroll } from "@/lib/selectScroll";

export interface HeaderActionMenuItem {
  key: string;
  label: string;
  icon: ReactNode;
  disabled?: boolean;
  title?: string;
  destructive?: boolean;
  onSelect: () => void;
}

export interface HeaderActionMenuSection {
  key: string;
  label?: string;
  items: HeaderActionMenuItem[];
}

// 页头和行内菜单共用 portal、视口避让与键盘焦点管理。
export function HeaderActionMenu({
  label,
  icon,
  items,
  sections,
  align = "end",
  compact = false,
  triggerVariant = "outline",
}: {
  label: string;
  icon: ReactNode;
  items?: HeaderActionMenuItem[];
  sections?: HeaderActionMenuSection[];
  align?: "start" | "end";
  compact?: boolean;
  triggerVariant?: "outline" | "default" | "ghost" | "secondary" | "destructive";
}) {
  const [open, setOpen] = useState(false);
  const resolvedSections: HeaderActionMenuSection[] = sections?.length
    ? sections.filter((section) => section.items.length > 0)
    : items?.length
      ? [{ key: "default", items }]
      : [];

  // Radix DropdownMenu 通过 Portal 挂到 body，处于 Dialog 内时 remove-scroll
  // 会拦截原生 wheel/touchmove，这里手动驱动 Content 滚动（手机端）。
  const contentRef = useRef<HTMLDivElement>(null);
  const touchStartRef = useRef<{ x: number; y: number } | null>(null);

  useEffect(() => {
    const handleWheel = (event: WheelEvent) => {
      const el = contentRef.current;
      const target = event.target as Node | null;
      if (!el || !target || !el.contains(target)) return;
      if (applyWheelScroll(el, event)) {
        event.preventDefault();
      }
    };
    const handleTouchStart = (event: TouchEvent) => {
      const el = contentRef.current;
      const target = event.target as Node | null;
      if (!el || !target || !el.contains(target)) return;
      const touch = event.touches[0];
      if (!touch) return;
      touchStartRef.current = { x: touch.clientX, y: touch.clientY };
    };
    const handleTouchMove = (event: TouchEvent) => {
      const el = contentRef.current;
      const start = touchStartRef.current;
      if (!el || !start) return;
      const target = event.target as Node | null;
      if (!target || !el.contains(target)) return;
      const touch = event.touches[0];
      if (!touch) return;
      const deltaX = start.x - touch.clientX;
      const deltaY = start.y - touch.clientY;
      touchStartRef.current = { x: touch.clientX, y: touch.clientY };
      if (applyManualScroll(el, deltaX, deltaY)) {
        event.preventDefault();
      }
    };
    document.addEventListener("wheel", handleWheel, { passive: false, capture: true });
    document.addEventListener("touchstart", handleTouchStart, { passive: true, capture: true });
    document.addEventListener("touchmove", handleTouchMove, { passive: false, capture: true });
    return () => {
      document.removeEventListener("wheel", handleWheel, true);
      document.removeEventListener("touchstart", handleTouchStart, true);
      document.removeEventListener("touchmove", handleTouchMove, true);
      touchStartRef.current = null;
    };
  }, []);

  return (
    <DropdownMenu.Root open={open} onOpenChange={setOpen} modal={false}>
      <DropdownMenu.Trigger asChild>
        <Button
          type="button"
          variant={triggerVariant}
          size={compact ? "icon-sm" : "sm"}
          aria-label={label}
          title={label}
          className={cn("shrink-0", open && "border-primary/25 bg-accent text-accent-foreground")}
        >
          {icon}
          {!compact ? (
            <>
              {label}
              <ChevronDown className={cn("size-3.5 text-muted-foreground transition-transform", open && "rotate-180")} />
            </>
          ) : null}
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          ref={contentRef}
          data-slot="action-menu-popover"
          align={align}
          sideOffset={8}
          collisionPadding={12}
          className="action-menu-surface z-[200] w-64 max-w-[calc(100vw-1.5rem)] overflow-y-auto overscroll-contain rounded-xl border border-border/80 bg-popover p-1.5 text-popover-foreground shadow-[0_12px_40px_-12px_hsl(222_30%_12%/0.3)] outline-none"
        >
          {resolvedSections.map((section, sectionIndex) => (
            <DropdownMenu.Group key={section.key}>
              {sectionIndex > 0 ? <DropdownMenu.Separator className="mx-1 my-1.5 h-px bg-border/70" /> : null}
              {section.label ? (
                <DropdownMenu.Label className="px-2.5 pb-1 pt-1.5 text-[10px] font-semibold tracking-wide text-muted-foreground">
                  {section.label}
                </DropdownMenu.Label>
              ) : null}
              {section.items.map((item) => (
                <DropdownMenu.Item
                  key={item.key}
                  disabled={item.disabled}
                  title={item.title}
                  onSelect={item.onSelect}
                  className={cn(
                    "group flex min-h-9 cursor-default select-none items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] outline-none transition-colors data-[disabled]:pointer-events-none data-[disabled]:opacity-40",
                    item.destructive
                      ? "text-destructive data-[highlighted]:bg-destructive/10"
                      : "text-foreground data-[highlighted]:bg-accent",
                  )}
                >
                  <span className={cn("flex size-6 shrink-0 items-center justify-center rounded-md", item.destructive ? "bg-destructive/8" : "bg-muted/70 text-muted-foreground group-data-[highlighted]:text-foreground")}>
                    {item.icon}
                  </span>
                  <span className="min-w-0 flex-1 leading-5">{item.label}</span>
                </DropdownMenu.Item>
              ))}
            </DropdownMenu.Group>
          ))}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}
