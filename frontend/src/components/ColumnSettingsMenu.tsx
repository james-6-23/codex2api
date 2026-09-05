import { Check, Columns3, RotateCcw } from "lucide-react";
import { DropdownMenu } from "radix-ui";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

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
