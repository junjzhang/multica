"use client";

import type { ReactNode } from "react";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

export function SessionChoice({ value, title, details, available, reason, selected, name, onSelect }: {
  value: string;
  title: string;
  details: ReactNode;
  available: boolean;
  reason?: string | null;
  selected: boolean;
  name: string;
  onSelect: () => void;
}) {
  const { t } = useT("issues");
  return (
    <label className={cn(
      "flex w-full min-w-0 cursor-pointer flex-col gap-0.5 rounded-md border p-2 text-left text-caption transition-colors focus-within:ring-2 focus-within:ring-ring/50",
      selected ? "border-primary bg-primary/5" : "border-border/70 hover:bg-muted/50",
      !available && "cursor-not-allowed opacity-60 hover:bg-transparent",
    )}>
      <input type="radio" name={name} value={value} checked={selected} disabled={!available} onChange={onSelect} className="sr-only" />
      <span className="flex min-w-0 items-center gap-1.5">
        <span className="min-w-0 truncate font-medium">{title}</span>
        {selected && <span className="ml-auto shrink-0 text-micro font-medium text-primary">{t(($) => $.detail.external_binding_selected)}</span>}
      </span>
      <span className="min-w-0 break-words text-micro text-muted-foreground">{details}</span>
      {reason && <span className="text-micro text-destructive">{reason}</span>}
    </label>
  );
}
