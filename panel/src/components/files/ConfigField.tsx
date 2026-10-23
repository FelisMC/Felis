import { useTranslation } from "react-i18next";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import type { ConfigFieldSpec } from "./configFields";

export function ConfigField({ field, value, onChange, problem, disabled }: {
  field: ConfigFieldSpec;
  value: string;
  onChange: (value: string) => void;
  problem?: string;
  disabled?: boolean;
}) {
  const { t } = useTranslation("files");
  const id = `config-${field.id}`;
  const options = field.type === "boolean" ? ["true", "false"] : field.type === "enum" ? field.options.map(String) : null;
  const attributes = { id, disabled, "aria-invalid": !!problem, "aria-describedby": `${id}-path${field.hint ? ` ${id}-hint` : ""}${problem ? ` ${id}-error` : ""}` };
  return <div className="grid min-w-0 content-start gap-1.5">
    <Label htmlFor={id} className="leading-relaxed">{t(`config_field_${field.id}`)}</Label>
    <code id={`${id}-path`} className="break-all text-[11px] text-muted-foreground">{field.path.join(".")}</code>
    {options ? (
      <Select value={`value:${value}`} onValueChange={(next) => onChange(next.slice(6))} disabled={disabled}>
        <SelectTrigger {...attributes}><SelectValue /></SelectTrigger>
        <SelectContent>
          {!options.includes(value) && <SelectItem value={`value:${value}`}>{t("properties_current_value", { value: value || t("properties_empty_value") })}</SelectItem>}
          {options.map((option) => <SelectItem key={option} value={`value:${option}`}>
            {field.type === "boolean" ? t(`properties_option_${option}`) : option}
          </SelectItem>)}
        </SelectContent>
      </Select>
    ) : field.type === "text" ? (
      <Textarea {...attributes} value={value} onChange={(event) => onChange(event.target.value)} rows={3} />
    ) : (
      <Input {...attributes} value={value} inputMode={field.type === "number" ? "decimal" : "numeric"}
        onChange={(event) => onChange(event.target.value)} />
    )}
    {field.hint && <p id={`${id}-hint`} className="break-words text-xs leading-relaxed text-muted-foreground">{t(field.hint)}</p>}
    {problem && <p id={`${id}-error`} role="alert" className="text-xs text-destructive">
      {t(`config_invalid_${problem}`, {
        min: "min" in field ? field.min : undefined, max: "max" in field ? field.max : undefined,
        multiple: "multipleOf" in field ? field.multipleOf : undefined, bytes: "maxBytes" in field ? field.maxBytes : undefined,
      })}
    </p>}
  </div>;
}
