import { useId } from "react";
import { CircleHelp } from "lucide-react";
import { Popover } from "radix-ui";
import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";

export default function ForceFastField({ force, onChange, className }: {
  force: boolean;
  onChange: (force: boolean) => void;
  className?: string;
}) {
  const { t } = useTranslation();
  const id = useId();

  return (
    <div className={cn("rounded-md border border-border p-3", className)}>
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-1">
          <label htmlFor={id} className="text-sm font-semibold">{t("forceFast.label")}</label>
          <ForceFastHelp />
        </div>
        <select
          id={id}
          aria-describedby={`${id}-hint`}
          className="h-9 rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-2 focus-visible:outline-ring"
          value={force ? "force" : "passthrough"}
          onChange={(event) => onChange(event.target.value === "force")}
        >
          <option value="passthrough">{t("forceFast.passthrough")}</option>
          <option value="force">{t("forceFast.force")}</option>
        </select>
      </div>
      <p id={`${id}-hint`} className="mt-1 text-xs text-muted-foreground">{t("forceFast.hint")}</p>
    </div>
  );
}

function ForceFastHelp() {
  const { t } = useTranslation();
  const title = t("forceFast.precedenceTitle");

  return (
    <Popover.Root>
      <Popover.Trigger
        type="button"
        aria-label={title}
        className="inline-flex size-5 shrink-0 items-center justify-center rounded-full text-muted-foreground hover:bg-muted hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring"
      >
        <CircleHelp className="size-3.5" aria-hidden="true" />
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          aria-label={title}
          side="top"
          align="start"
          sideOffset={6}
          collisionPadding={12}
          className="z-[70] w-80 max-w-[var(--radix-popover-content-available-width)] space-y-1.5 rounded-md border border-border bg-popover p-3 text-left text-xs text-popover-foreground shadow-md"
        >
          <div className="font-semibold">{title}</div>
          <p className="leading-relaxed">{t("forceFast.precedence")}</p>
          <p className="leading-relaxed">{t("forceFast.override")}</p>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  );
}
