import { lazy, Suspense, useEffect, useLayoutEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { SegmentedControl } from "@/components/SegmentedControl";
import { Loading } from "@/components/States";
import { ConfigField } from "./ConfigField";
import { configProblem, configScalar, yamlCompatible } from "./configFields";
import { YAML_FIELDS } from "./yamlFields";
import { getYamlScalar, readYamlConfig, setYamlScalar, YamlConfigParseError } from "./yamlConfig";

const TextFileEditor = lazy(() => import("./TextFileEditor").then((module) => ({ default: module.TextFileEditor })));

function parsed(text: string) {
  try {
    return readYamlConfig(text);
  } catch (error) {
    if (error instanceof YamlConfigParseError) return null;
    throw error;
  }
}

export function YamlConfigEditor({ path, value, original, onChange, onSave, onValidityChange, disabled = false }: {
  path: string;
  value: string;
  original: string;
  onChange: (text: string) => void;
  onSave: () => void;
  onValidityChange: (valid: boolean) => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation("files");
  const [mode, setMode] = useState<"form" | "text">("form");
  const [edited, setEdited] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => setEdited(new Set()), [original]);
  const document = useMemo(() => mode === "form" ? parsed(value) : null, [mode, value]);
  const baseline = useMemo(() => mode === "form" ? parsed(original) : null, [mode, original]);
  const fields = (YAML_FIELDS[path] ?? []).filter((field) => {
    const current = document && getYamlScalar(document, field.path);
    return current !== undefined && current !== null && (yamlCompatible(field, current) || edited.has(field.id));
  });
  const errors = new Map(fields.flatMap((field) => {
    const current = document && getYamlScalar(document, field.path);
    const before = baseline && getYamlScalar(baseline, field.path);
    const problem = !Object.is(current, before) ? configProblem(field, String(current)) : undefined;
    return problem ? [[field.id, problem] as const] : [];
  }));
  const valid = mode === "text" || (document !== null && errors.size === 0);
  useLayoutEffect(() => onValidityChange(valid), [valid, onValidityChange]);

  return <div className="grid min-w-0 gap-3" onKeyDown={(event) => {
    if (event.defaultPrevented || !(event.ctrlKey || event.metaKey) || event.key.toLowerCase() !== "s") return;
    event.preventDefault();
    if (valid && !disabled) onSave();
  }}>
    <SegmentedControl label={t("properties_mode")} value={mode}
      segments={[{ key: "form", label: t("properties_form") }, { key: "text", label: t("properties_text") }]}
      onChange={(next) => { if (!disabled) setMode(next); }} />
    {mode === "text" ? (
      <Suspense fallback={<Loading />}>
        <TextFileEditor path={path} value={value} label={t("file_content")}
          onChange={(text) => { if (!disabled) onChange(text); }} onSave={() => { if (!disabled) onSave(); }} />
      </Suspense>
    ) : document === null ? (
      <p role="alert" className="rounded-md border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">{t("yaml_parse_error")}</p>
    ) : (
      <div className="max-h-[50vh] min-w-0 space-y-4 overflow-y-auto rounded-md border border-border p-3 sm:p-4">
        <p className="text-xs leading-relaxed text-muted-foreground">{t("yaml_form_hint")}</p>
        {path !== "bukkit.yml" && <p className="text-xs leading-relaxed text-muted-foreground">{t("yaml_world_defaults_hint")}</p>}
        {path === "bukkit.yml" && <p className="text-xs leading-relaxed text-muted-foreground">{t("yaml_bukkit_hint")}</p>}
        {fields.length === 0 && <p className="text-sm text-muted-foreground">{t("yaml_no_fields")}</p>}
        <fieldset disabled={disabled} className="grid min-w-0 gap-4 sm:grid-cols-2">
          {fields.map((field) => <ConfigField key={field.id} field={field}
            value={String(getYamlScalar(document, field.path))} problem={errors.get(field.id)} disabled={disabled}
            onChange={(next) => {
              if (disabled) return;
              setEdited((previous) => new Set([...previous, field.id]));
              onChange(setYamlScalar(value, field.path, configScalar(field, next)));
            }} />)}
        </fieldset>
      </div>
    )}
  </div>;
}
