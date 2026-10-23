import { lazy, Suspense, useLayoutEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { SegmentedControl } from "@/components/SegmentedControl";
import { Loading } from "@/components/States";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { PropertiesParseError, readProperties, setProperty } from "./properties";
import { PROPERTY_FIELDS, PROPERTY_GROUPS, propertyManaged, propertyProblem } from "./serverPropertiesFields";

const TextFileEditor = lazy(() => import("./TextFileEditor").then((m) => ({ default: m.TextFileEditor })));

function parsed(text: string): ReadonlyMap<string, string> | null {
  try {
    return readProperties(text);
  } catch (error) {
    if (error instanceof PropertiesParseError) return null;
    throw error;
  }
}

export function ServerPropertiesEditor({ server, value, original, onChange, onSave, onValidityChange, disabled = false }: {
  server: string;
  value: string;
  original: string;
  onChange: (text: string) => void;
  onSave: () => void;
  onValidityChange: (valid: boolean) => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation("files");
  const [mode, setMode] = useState<"form" | "text">("form");
  const values = useMemo(() => mode === "form" ? parsed(value) : null, [mode, value]);
  const baseline = useMemo(() => mode === "form" ? parsed(original) : null, [mode, original]);
  const fields = PROPERTY_FIELDS.filter((field) => (!field.loginOnly || server === "login") && (!field.existingOnly || values?.has(field.key)));
  const errors = new Map<string, string>(fields.flatMap((field) => {
    const current = values?.get(field.key);
    const problem = current !== undefined && current !== baseline?.get(field.key) && !propertyManaged(field, server)
      ? propertyProblem(field, current) : undefined;
    return problem ? [[field.key, t(`properties_invalid_${problem}`, field.type === "integer" ? { min: field.min, max: field.max } : {})] as const] : [];
  }));
  const valid = mode === "text" || (values !== null && errors.size === 0);
  useLayoutEffect(() => onValidityChange(valid), [valid, onValidityChange]);

  function update(key: string, next: string) {
    if (!disabled) onChange(setProperty(value, key, next));
  }

  return (
    <div className="grid min-w-0 gap-3" onKeyDown={(event) => {
      if (event.defaultPrevented || !(event.ctrlKey || event.metaKey) || event.key.toLowerCase() !== "s") return;
      event.preventDefault();
      if (valid && !disabled) onSave();
    }}>
      <SegmentedControl label={t("properties_mode")} value={mode}
        segments={[{ key: "form", label: t("properties_form") }, { key: "text", label: t("properties_text") }]}
        onChange={(next) => { if (!disabled) setMode(next); }} />
      {mode === "text" ? (
        <Suspense fallback={<Loading />}>
          <TextFileEditor path="server.properties" value={value} label={t("file_content")} onChange={onChange} onSave={onSave} />
        </Suspense>
      ) : values === null ? (
        <p role="alert" className="rounded-md border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">{t("properties_parse_error")}</p>
      ) : (
        <div className="max-h-[50vh] min-w-0 space-y-5 overflow-y-auto rounded-md border border-border p-3 sm:p-4">
          <p className="text-xs leading-relaxed text-muted-foreground">{t("properties_form_hint")}</p>
          {PROPERTY_GROUPS.map((group) => (
            <fieldset key={group} disabled={disabled} className="min-w-0 border-t border-border pt-3">
              <legend className="pr-3 text-sm font-semibold">{t(`properties_group_${group}`)}</legend>
              <div className="grid min-w-0 gap-4 pt-2 sm:grid-cols-2">
                {fields.filter((field) => field.group === group).map((field) => {
                  const token = field.key.replace(/[.-]/g, "_");
                  const id = `property-${token}`;
                  const current = values.get(field.key);
                  const managed = propertyManaged(field, server);
                  const error = errors.get(field.key);
                  const options = field.type === "boolean" ? ["true", "false"] : field.type === "enum" ? field.options : null;
                  const label = t(`properties_field_${token}`);
                  const attributes = { id, "aria-invalid": !!error, "aria-describedby": `${id}-hint${error ? ` ${id}-error` : ""}` };
                  return (
                    <div key={field.key} className="grid min-w-0 content-start gap-1.5">
                      <div className="grid min-w-0 gap-0.5">
                        <Label htmlFor={id}>{label}</Label>
                        <code className="break-all text-[11px] text-muted-foreground">{field.key}</code>
                      </div>
                      {managed ? (
                        <Input {...attributes} value={current ?? ""} placeholder={t("properties_unset")} readOnly />
                      ) : options ? (
                        <Select value={current === undefined ? "unset" : `value:${current}`} onValueChange={(next) => update(field.key, next.slice(6))} disabled={disabled}>
                          <SelectTrigger {...attributes}><SelectValue /></SelectTrigger>
                          <SelectContent>
                            <SelectItem value="unset" disabled>{t("properties_unset")}</SelectItem>
                            {current !== undefined && !options.includes(current) && (
                              <SelectItem value={`value:${current}`}>{t("properties_current_value", { value: current || t("properties_empty_value") })}</SelectItem>
                            )}
                            {options.map((option) => <SelectItem key={option} value={`value:${option}`}>
                              {/^[0-4]$/.test(option) ? option : t(`properties_option_${option}`)}
                            </SelectItem>)}
                          </SelectContent>
                        </Select>
                      ) : field.type === "text" && field.multiline ? (
                        <Textarea {...attributes} value={current ?? ""} placeholder={t("properties_unset")}
                          onChange={(event) => update(field.key, event.target.value)} rows={3} />
                      ) : (
                        <Input {...attributes} value={current ?? ""} placeholder={t("properties_unset")}
                          inputMode={field.type === "integer" ? "numeric" : undefined}
                          onChange={(event) => update(field.key, event.target.value)} />
                      )}
                      <p id={`${id}-hint`} className="break-words text-xs leading-relaxed text-muted-foreground">
                        {t(`properties_hint_${token}`)}
                        {managed && <> {t(field.managed === "login" ? "properties_managed_login" : "properties_managed_platform")}</>}
                      </p>
                      {error && <p id={`${id}-error`} role="alert" className="text-xs text-destructive">{error}</p>}
                    </div>
                  );
                })}
              </div>
            </fieldset>
          ))}
        </div>
      )}
    </div>
  );
}
