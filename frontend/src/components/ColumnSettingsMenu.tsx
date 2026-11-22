import { useEffect, useRef } from "react";
import { Check, Columns3, RotateCcw } from "lucide-react";
import { DropdownMenu } from "radix-ui";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { applyManualScroll } from "@/lib/selectScroll";

interface ColumnSettingsMenuProps<Column extends string> {
  columns: Record<Column, boolean>;
  columnOrder: readonly Column[];
  labels: Record<Column, string>;
  onToggle: (column: Column) => void;
  onReset: () => void;
  title: string;
  resetTitle: string;
}

export default function ColumnSettingsMenu<Column extends string>({
  columns,
  columnOrder,
  labels,
  onToggle,
  onReset,
  title,
  resetTitle,
}: ColumnSettingsMenuProps<Column>) {
  const visibleCount = columnOrder.filter((column) => columns[column]).length;

  // Radix DropdownMenu 通过 Portal 挂到 body，处于 Dialog 内时 remove-scroll
  // 会拦截原生 wheel/touchmove，这里手动驱动 Content 滚动（手机端）。
  const contentRef = useRef<HTMLDivElement>(null);
  const touchStartRef = useRef<{ x: number; y: number } | null>(null);

  useEffect(() => {
    const handleWheel = (event: WheelEvent) => {
      const el = contentRef.current;
      const target = event.target as Node | null;
      if (!el || !target || !el.contains(target)) return;
      if (applyManualScroll(el, event.deltaX, event.deltaY)) {
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
    <DropdownMenu.Root modal={false}>
      <DropdownMenu.Trigger asChild>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="shrink-0 data-[state=open]:bg-accent"
          title={title}
          aria-label={title}
        >
          <Columns3 className="size-3.5" />
          <span className="hidden sm:inline">{title}</span>
          <span className="hidden rounded bg-muted px-1.5 text-[10px] tabular-nums text-muted-foreground sm:inline">
            {visibleCount}/{columnOrder.length}
          </span>
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          ref={contentRef}
          align="end"
          sideOffset={8}
          collisionPadding={12}
          className="action-menu-surface z-[200] w-72 max-w-[calc(100vw-1.5rem)] overflow-y-auto overscroll-contain rounded-xl border border-border/80 bg-popover p-1.5 shadow-[0_12px_40px_-12px_hsl(222_30%_12%/0.3)] outline-none"
        >
          <div className="flex items-center justify-between px-2.5 py-2">
            <DropdownMenu.Label className="text-xs font-semibold text-foreground">
              {title}
            </DropdownMenu.Label>
            <span className="text-[10px] tabular-nums text-muted-foreground">
              {visibleCount}/{columnOrder.length}
            </span>
          </div>
          <div className="grid grid-cols-2 gap-0.5">
            {columnOrder.map((column) => (
              <DropdownMenu.CheckboxItem
                key={column}
                checked={columns[column]}
                onCheckedChange={() => onToggle(column)}
                onSelect={(event) => event.preventDefault()}
                className="flex min-h-9 cursor-default select-none items-center gap-2 rounded-lg px-2.5 py-2 text-xs text-foreground outline-none transition-colors data-[highlighted]:bg-accent"
              >
                <span
                  className={cn(
                    "flex size-3.5 shrink-0 items-center justify-center rounded border transition-colors",
                    columns[column]
                      ? "border-primary bg-primary text-primary-foreground"
                      : "border-border bg-background",
                  )}
                >
                  <DropdownMenu.ItemIndicator>
                    <Check className="size-2.5" />
                  </DropdownMenu.ItemIndicator>
                </span>
                <span className="min-w-0 leading-4">{labels[column]}</span>
              </DropdownMenu.CheckboxItem>
            ))}
          </div>
          <DropdownMenu.Separator className="mx-1 my-1.5 h-px bg-border/70" />
          <DropdownMenu.Item
            onSelect={(event) => {
              event.preventDefault();
              onReset();
            }}
            className="flex min-h-9 cursor-default items-center justify-center gap-2 rounded-lg px-2.5 py-2 text-xs font-medium text-muted-foreground outline-none data-[highlighted]:bg-accent data-[highlighted]:text-foreground"
          >
            <RotateCcw className="size-3.5" />
            {resetTitle}
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}
