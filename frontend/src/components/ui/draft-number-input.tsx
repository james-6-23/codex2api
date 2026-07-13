import { useState } from "react";
import type { ComponentProps, FocusEvent } from "react";

import {
  commitDraftNumber,
  updateDraftNumber,
} from "@/lib/numberInputDraft";
import { Input } from "@/components/ui/input";

export interface DraftNumberInputProps
  extends Omit<
    ComponentProps<"input">,
    "type" | "value" | "onChange" | "onBlur"
  > {
  value: number;
  onValueChange: (value: number) => void;
  onValueCommit?: (value: number) => void;
  integer?: boolean;
  emptyValue?: number;
  formatValue?: (value: number) => string;
}

function DraftNumberInput({
  value,
  onValueChange,
  onValueCommit,
  integer = true,
  emptyValue,
  formatValue = String,
  min,
  max,
  ...props
}: DraftNumberInputProps) {
  const [draft, setDraft] = useState<string | null>(null);
  const options = {
    integer,
    min: typeof min === "number" ? min : undefined,
    max: typeof max === "number" ? max : undefined,
    emptyValue,
  };

  const handleBlur = (_event: FocusEvent<HTMLInputElement>) => {
    const committed = commitDraftNumber(
      draft ?? formatValue(value),
      value,
      options,
    );
    setDraft(null);
    if (committed !== value) onValueChange(committed);
    onValueCommit?.(committed);
  };

  return (
    <Input
      {...props}
      type="number"
      min={min}
      max={max}
      value={draft ?? formatValue(value)}
      onChange={(event) => {
        const next = updateDraftNumber(event.currentTarget.value, value, options);
        setDraft(next.draft);
        if (next.changed) onValueChange(next.value);
      }}
      onBlur={handleBlur}
    />
  );
}

export { DraftNumberInput };
