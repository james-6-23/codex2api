import { useEffect, useMemo, useRef, useState } from "react";
import { Ban, Check, ChevronDown, Eye } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { AccountGroup } from "../types";
import { applyManualScroll } from "../lib/selectScroll";

const FALLBACK_GROUP_COLOR = "#2563eb";

function normalizeGroupColor(color?: string): string {
  const value = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(value) ? value : FALLBACK_GROUP_COLOR;
}

export interface AccountGroupFilterValue {
  ungrouped: boolean;
  include: number[];
  exclude: number[];
}

export const EMPTY_ACCOUNT_GROUP_FILTER: AccountGroupFilterValue = {
  ungrouped: false,
  include: [],
  exclude: [],
};

export function isAccountGroupFilterEmpty(
  value: AccountGroupFilterValue,
): boolean {
  return (
    !value.ungrouped && value.include.length === 0 && value.exclude.length === 0
  );
}

export function accountMatchesGroupFilter(
  groupIds: number[],
  filter: AccountGroupFilterValue,
): boolean {
  if (filter.ungrouped) return groupIds.length === 0;
  if (
    filter.include.length > 0 &&
    !filter.include.some((id) => groupIds.includes(id))
  )
    return false;
  if (filter.exclude.some((id) => groupIds.includes(id))) return false;
  return true;
}

// 分组被删除或列表刷新后，清理筛选中已不存在的分组 ID。
export function pruneAccountGroupFilter(
  value: AccountGroupFilterValue,
  groups: AccountGroup[],
): AccountGroupFilterValue {
  const valid = new Set(groups.map((group) => group.id));
  const include = value.include.filter((id) => valid.has(id));
  const exclude = value.exclude.filter((id) => valid.has(id));
  if (
    include.length === value.include.length &&
    exclude.length === value.exclude.length
  )
    return value;
  return { ...value, include, exclude };
}

type GroupState = "off" | "include" | "exclude";

function groupStateOf(
  value: AccountGroupFilterValue,
  id: number,
): GroupState {
  if (value.include.includes(id)) return "include";
  if (value.exclude.includes(id)) return "exclude";
  return "off";
}

export interface AccountGroupFilterSelectProps {
  groups: AccountGroup[];
  value: AccountGroupFilterValue;
  onChange: (value: AccountGroupFilterValue) => void;
  className?: string;
}

export default function AccountGroupFilterSelect({
  groups,
  value,
  onChange,
  className,
}: AccountGroupFilterSelectProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const touchStartRef = useRef<{ x: number; y: number } | null>(null);

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

    // 手机端在弹窗内下拉列表可能因 remove-scroll 无法原生滚动，手动驱动。
    const handleWheel = (event: WheelEvent) => {
      const list = listRef.current;
      const target = event.target as Node | null;
      if (!list || !target || !list.contains(target)) return;
      if (applyManualScroll(list, event.deltaX, event.deltaY)) {
        event.preventDefault();
      }
    };
    const handleTouchStart = (event: TouchEvent) => {
      const list = listRef.current;
      const target = event.target as Node | null;
      if (!list || !target || !list.contains(target)) return;
      const touch = event.touches[0];
      if (!touch) return;
      touchStartRef.current = { x: touch.clientX, y: touch.clientY };
    };
    const handleTouchMove = (event: TouchEvent) => {
      const list = listRef.current;
      const start = touchStartRef.current;
      if (!list || !start) return;
      const target = event.target as Node | null;
      if (!target || !list.contains(target)) return;
      const touch = event.touches[0];
      if (!touch) return;
      const deltaX = start.x - touch.clientX;
      const deltaY = start.y - touch.clientY;
      touchStartRef.current = { x: touch.clientX, y: touch.clientY };
      if (applyManualScroll(list, deltaX, deltaY)) {
        event.preventDefault();
      }
    };

    document.addEventListener("mousedown", handlePointerDown);
    document.addEventListener("keydown", handleEscape);
    document.addEventListener("wheel", handleWheel, { passive: false, capture: true });
    document.addEventListener("touchstart", handleTouchStart, { passive: true, capture: true });
    document.addEventListener("touchmove", handleTouchMove, { passive: false, capture: true });
    return () => {
      document.removeEventListener("mousedown", handlePointerDown);
      document.removeEventListener("keydown", handleEscape);
      document.removeEventListener("wheel", handleWheel, true);
      document.removeEventListener("touchstart", handleTouchStart, true);
      document.removeEventListener("touchmove", handleTouchMove, true);
      touchStartRef.current = null;
    };
  }, [open]);

  const isEmpty = isAccountGroupFilterEmpty(value);

  const summary = useMemo(() => {
    if (isEmpty) return t("accounts.groupsFilter");
    if (value.ungrouped) return t("accounts.groupFilterUngrouped");
    if (value.include.length === 1 && value.exclude.length === 0) {
      const group = groups.find((item) => item.id === value.include[0]);
      if (group) return group.name;
    }
    return t("accounts.groupFilterSummary", {
      count: value.include.length + value.exclude.length,
    });
  }, [groups, isEmpty, t, value]);

  const cycleGroup = (id: number) => {
    const state = groupStateOf(value, id);
    const include = value.include.filter((item) => item !== id);
    const exclude = value.exclude.filter((item) => item !== id);
    if (state === "off") include.push(id);
    else if (state === "include") exclude.push(id);
    onChange({ ungrouped: false, include, exclude });
  };

  return (
    <div ref={rootRef} className={`relative ${className ?? ""}`}>
      <button
        type="button"
        className={`flex h-8 w-full items-center justify-between gap-1.5 rounded-lg border border-input bg-background px-2.5 text-left text-[13px] shadow-xs transition-[border-color,box-shadow] hover:border-primary/30 hover:bg-accent/40 ${
          open ? "border-primary/35 ring-[3px] ring-primary/10" : ""
        } ${isEmpty ? "text-foreground" : "text-primary"}`}
        onClick={() => setOpen((current) => !current)}
      >
        <span className="truncate">{summary}</span>
        <ChevronDown
          className={`size-3.5 shrink-0 text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`}
        />
      </button>

      {open ? (
        <div className="absolute left-0 top-[calc(100%+0.5rem)] z-50 w-64 max-w-[calc(100vw-2.5rem)] overflow-hidden rounded-lg border border-border bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm max-sm:right-0 max-sm:left-auto">
          <div className="space-y-0.5 p-1.5">
            <button
              type="button"
              className={`flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-left text-[13px] transition-colors ${
                isEmpty
                  ? "bg-primary/10 font-medium text-primary"
                  : "text-foreground hover:bg-accent/70"
              }`}
              onClick={() => {
                onChange(EMPTY_ACCOUNT_GROUP_FILTER);
                setOpen(false);
              }}
            >
              <span className="flex size-3.5 shrink-0 items-center justify-center">
                {isEmpty ? <Check className="size-3.5" /> : null}
              </span>
              {t("accounts.groupsFilter")}
            </button>
            <button
              type="button"
              className={`flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-left text-[13px] transition-colors ${
                value.ungrouped
                  ? "bg-primary/10 font-medium text-primary"
                  : "text-foreground hover:bg-accent/70"
              }`}
              onClick={() => {
                onChange(
                  value.ungrouped
                    ? EMPTY_ACCOUNT_GROUP_FILTER
                    : { ungrouped: true, include: [], exclude: [] },
                );
                setOpen(false);
              }}
            >
              <span className="flex size-3.5 shrink-0 items-center justify-center">
                {value.ungrouped ? <Check className="size-3.5" /> : null}
              </span>
              {t("accounts.groupFilterUngrouped")}
            </button>
          </div>

          {groups.length > 0 ? (
            <>
              <div className="border-t border-border" />
              <div ref={listRef} className="max-h-64 space-y-0.5 overflow-auto p-1.5">
                {groups.map((group) => {
                  const state = groupStateOf(value, group.id);
                  const color = normalizeGroupColor(group.color);
                  return (
                    <button
                      key={group.id}
                      type="button"
                      className={`flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-left text-[13px] transition-colors ${
                        state === "include"
                          ? "bg-primary/10 text-primary"
                          : state === "exclude"
                            ? "bg-destructive/10 text-destructive"
                            : "text-foreground hover:bg-accent/70"
                      }`}
                      onClick={() => cycleGroup(group.id)}
                    >
                      <span
                        className="size-2.5 shrink-0 rounded-full"
                        style={{ backgroundColor: color }}
                      />
                      <span
                        className={`min-w-0 flex-1 truncate font-medium ${
                          state === "exclude" ? "line-through opacity-80" : ""
                        }`}
                      >
                        {group.name}
                      </span>
                      {state === "include" ? (
                        <span className="inline-flex shrink-0 items-center gap-1 text-[11px] font-semibold">
                          <Eye className="size-3" />
                          {t("accounts.groupFilterOnly")}
                        </span>
                      ) : null}
                      {state === "exclude" ? (
                        <span className="inline-flex shrink-0 items-center gap-1 text-[11px] font-semibold">
                          <Ban className="size-3" />
                          {t("accounts.groupFilterHide")}
                        </span>
                      ) : null}
                    </button>
                  );
                })}
              </div>
              <div className="border-t border-border px-3 py-1.5 text-[11px] text-muted-foreground">
                {t("accounts.groupFilterHint")}
              </div>
            </>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
