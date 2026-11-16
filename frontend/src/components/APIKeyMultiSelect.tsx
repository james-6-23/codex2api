import { useEffect, useRef, useState } from "react";
import { Check, ChevronDown } from "lucide-react";
import type { APIKeyRow } from "../types";

function formatAPIKeyOptionLabel(apiKey: APIKeyRow): string {
  const name = apiKey.name?.trim() || `API Key #${apiKey.id}`;
  return `${name} · ${apiKey.key}`;
}

// APIKeyMultiSelect 选择账号允许绑定的 API Key；未选任何项表示不限。
export default function APIKeyMultiSelect({
  options,
  value,
  disabled,
  onChange,
  allLabel,
  selectedLabel,
  placeholder,
  emptyLabel,
  emptyHint,
}: {
  options: APIKeyRow[];
  value: number[];
  disabled: boolean;
  onChange: (value: number[]) => void;
  allLabel: string;
  selectedLabel: string;
  placeholder: string;
  emptyLabel: string;
  emptyHint: string;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;

    const handlePointerDown = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };

    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
      }
    };

    document.addEventListener("mousedown", handlePointerDown);
    document.addEventListener("keydown", handleEscape);
    return () => {
      document.removeEventListener("mousedown", handlePointerDown);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [open]);

  const summary = value.length === 0 ? allLabel : selectedLabel;

  const toggleOption = (id: number) => {
    if (disabled) return;
    if (value.includes(id)) {
      onChange(value.filter((item) => item !== id));
      return;
    }
    onChange([...value, id].sort((a, b) => a - b));
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        disabled={disabled}
        aria-expanded={open}
        className={`flex w-full items-center justify-between gap-3 rounded-md border border-input bg-background px-3.5 py-3 text-left shadow-xs transition-[border-color,box-shadow] ${
          disabled
            ? "cursor-not-allowed opacity-70"
            : "hover:border-primary/30 hover:bg-accent/40"
        } ${open ? "border-primary/35 ring-[3px] ring-primary/10" : ""}`}
        onClick={() => {
          if (!disabled) {
            setOpen((current) => !current);
          }
        }}
      >
        <div className="min-w-0">
          <div className="truncate text-[15px] text-foreground">{summary}</div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            {disabled ? emptyHint : placeholder}
          </div>
        </div>
        <ChevronDown
          className={`size-4 shrink-0 text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`}
        />
      </button>

      {open ? (
        <div className="absolute left-0 right-0 top-[calc(100%+0.5rem)] z-50 overflow-hidden rounded-lg border border-border bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm">
          {options.length === 0 ? (
            <div className="px-4 py-3 text-sm text-muted-foreground">
              {emptyLabel}
            </div>
          ) : (
            <div className="max-h-72 space-y-1 overflow-auto p-2">
              {options.map((option) => {
                const checked = value.includes(option.id);
                return (
                  <button
                    key={option.id}
                    type="button"
                    aria-pressed={checked}
                    className={`flex w-full items-center gap-3 rounded-md px-3 py-2.5 text-left transition-colors ${
                      checked
                        ? "bg-primary/10 text-primary"
                        : "text-foreground hover:bg-accent/70"
                    }`}
                    onClick={() => toggleOption(option.id)}
                  >
                    <span
                      className={`flex size-4 items-center justify-center rounded border ${checked ? "border-primary bg-primary text-primary-foreground" : "border-border bg-background text-transparent"}`}
                    >
                      <Check className="size-3" />
                    </span>
                    <span className="min-w-0 flex-1 truncate text-sm">
                      {formatAPIKeyOptionLabel(option)}
                    </span>
                  </button>
                );
              })}
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}
