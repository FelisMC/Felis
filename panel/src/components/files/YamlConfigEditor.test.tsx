// @vitest-environment jsdom
import { useState } from "react";
import { beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { YamlConfigEditor } from "./YamlConfigEditor";
import { getYamlScalar, readYamlConfig } from "./yamlConfig";

vi.mock("./TextFileEditor", () => ({
  TextFileEditor: ({ value, onChange, label }: { value: string; onChange: (text: string) => void; label: string }) =>
    <textarea aria-label={label} value={value} onChange={(event) => onChange(event.target.value)} />,
}));

beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const t = (key: string) => i18next.t(`files:${key}`);
const input = (id: string) => screen.getByRole("textbox", { name: t(`config_field_${id}`) }) as HTMLInputElement;
const save = () => screen.getByRole("button", { name: t("save") }) as HTMLButtonElement;
const mode = (next: "form" | "text") => userEvent.click(screen.getByRole("radio", { name: t(`properties_${next}`) }));

function editor(path: string, source: string, disabled = false) {
  const changed = vi.fn();
  const saved = vi.fn();
  function Harness() {
    const [text, setText] = useState(source);
    const [valid, setValid] = useState(true);
    return <>
      <YamlConfigEditor path={path} value={text} original={source} onChange={(next) => { changed(next); setText(next); }}
        onSave={() => saved(text)} onValidityChange={setValid} disabled={disabled} />
      <button disabled={disabled || !valid || text === source} onClick={() => saved(text)}>{t("save")}</button>
    </>;
  }
  render(<Harness />);
  return { changed, saved };
}

describe("YAML configuration form", () => {
  it("keeps comments, unknown data and CRLF source unchanged until an explicit edit", async () => {
    const source = "# retained\r\nsettings:\r\n  allow-end: yes # legacy boolean\r\nspawn-limits:\r\n  monsters: 70\r\nunknown:\r\n  list: [one, two]\r\n";
    const { changed, saved } = editor("bukkit.yml", source);
    expect(save().disabled).toBe(true);
    expect(screen.getByRole("combobox", { name: t("config_field_bukkit_allow_end") }).textContent).toBe(t("properties_option_true"));
    await mode("text");
    await mode("form");
    expect(changed).not.toHaveBeenCalled();
    expect(save().disabled).toBe(true);
    fireEvent.change(input("bukkit_monsters"), { target: { value: "9007199254740993" } });
    await userEvent.click(save());
    const result = saved.mock.calls[0][0] as string;
    expect(getYamlScalar(readYamlConfig(result), ["spawn-limits", "monsters"])).toBe(9007199254740993n);
    expect(result).toContain("# retained\r\n");
    expect(result).toContain("# legacy boolean\r\n");
    expect(readYamlConfig(result).getIn(["unknown", "list", 0])).toBe("one");
    expect(readYamlConfig(result).getIn(["unknown", "list", 1])).toBe("two");
    expect(getYamlScalar(readYamlConfig(result), ["settings", "allow-end"])).toBe(true);
  });

  it.each([
    ["bukkit.yml", "ticks-per:\n  autosave: 6000\n", "bukkit_autosave", ["ticks-per", "autosave"], "-1"],
    ["spigot.yml", "world-settings:\n  default:\n    hopper-amount: 1\n    ticks-per:\n      hopper-amount: 99\n", "spigot_hopper_amount", ["world-settings", "default", "hopper-amount"], "-2147483649"],
    ["config/paper-world-defaults.yml", "chunks:\n  max-auto-save-chunks-per-tick: 24\nworld-settings:\n  default:\n    chunks:\n      max-auto-save-chunks-per-tick: 99\n", "paper_chunk_saves", ["chunks", "max-auto-save-chunks-per-tick"], "123"],
    ["purpur.yml", "world-settings:\n  default:\n    gameplay-mechanics:\n      player:\n        exp-dropped-on-death:\n          maximum: 100\n  my.world:\n    custom: retained\n  'minecraft:overworld':\n    custom: also retained\n", "purpur_death_exp", ["world-settings", "default", "gameplay-mechanics", "player", "exp-dropped-on-death", "maximum"], "-9"],
  ] as const)("edits an existing scalar at the exact %s path", async (path, source, id, target, next) => {
    const { saved } = editor(path, source);
    fireEvent.change(input(id), { target: { value: next } });
    await userEvent.click(save());
    const document = readYamlConfig(saved.mock.calls[0][0]);
    expect(getYamlScalar(document, target)).toBe(BigInt(next));
    if (path === "spigot.yml") expect(getYamlScalar(document, ["world-settings", "default", "ticks-per", "hopper-amount"])).toBe(99n);
    if (path === "config/paper-world-defaults.yml") expect(getYamlScalar(document, ["world-settings", "default", "chunks", "max-auto-save-chunks-per-tick"])).toBe(99n);
    if (path === "purpur.yml") {
      expect(getYamlScalar(document, ["world-settings", "my.world", "custom"])).toBe("retained");
      expect(getYamlScalar(document, ["world-settings", "minecraft:overworld", "custom"])).toBe("also retained");
    }
  });

  it("omits missing values, incompatible types and unknown enum values without adding defaults", async () => {
    const source = "chunks:\n  max-auto-save-chunks-per-tick: '12'\nenvironment:\n  disable-thunder: true\nmisc:\n  redstone-implementation: FUTURE_ENGINE\nanticheat:\n  anti-xray:\n    engine-mode: 9\n";
    const { changed, saved } = editor("config/paper-world-defaults.yml", source);
    expect(screen.queryByRole("textbox", { name: t("config_field_paper_chunk_saves") })).toBeNull();
    expect(screen.queryByRole("combobox", { name: t("config_field_paper_redstone") })).toBeNull();
    expect(screen.queryByRole("combobox", { name: t("config_field_paper_xray_engine") })).toBeNull();
    expect(screen.queryByRole("textbox", { name: t("config_field_paper_portal_create") })).toBeNull();
    await mode("text");
    await mode("form");
    expect(changed).not.toHaveBeenCalled();
    expect(saved).not.toHaveBeenCalled();
  });

  it("shares invalid numeric drafts with Text, blocks form saves and restores the integer type after correction", async () => {
    const { changed, saved } = editor("bukkit.yml", "spawn-limits:\n  monsters: 70\n");
    fireEvent.change(input("bukkit_monsters"), { target: { value: "1.5" } });
    expect(screen.getByRole("alert").textContent).toBe(t("config_invalid_integer"));
    expect(save().disabled).toBe(true);
    fireEvent.keyDown(input("bukkit_monsters"), { key: "s", ctrlKey: true });
    expect(saved).not.toHaveBeenCalled();
    await mode("text");
    expect(getYamlScalar(readYamlConfig(changed.mock.lastCall![0]), ["spawn-limits", "monsters"])).toBe("1.5");
    expect(save().disabled).toBe(false);
    await mode("form");
    expect(input("bukkit_monsters").value).toBe("1.5");
    expect(save().disabled).toBe(true);
    fireEvent.change(input("bukkit_monsters"), { target: { value: "42" } });
    fireEvent.keyDown(input("bukkit_monsters"), { key: "s", metaKey: true });
    expect(getYamlScalar(readYamlConfig(saved.mock.lastCall![0]), ["spawn-limits", "monsters"])).toBe(42n);
  });

  it("validates only edited values and applies the documented multiple-of-16 rule", async () => {
    const { saved } = editor("config/paper-world-defaults.yml", "anticheat:\n  anti-xray:\n    max-block-height: 70\nenvironment:\n  disable-thunder: false\n");
    await userEvent.click(screen.getByRole("combobox", { name: t("config_field_paper_thunder") }));
    await userEvent.click(screen.getByRole("option", { name: t("properties_option_true") }));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(save().disabled).toBe(false);
    fireEvent.change(input("paper_xray_height"), { target: { value: "72" } });
    expect(screen.getByRole("alert").textContent).toBe(i18next.t("files:config_invalid_multiple", { multiple: 16 }));
    expect(save().disabled).toBe(true);
    fireEvent.change(input("paper_xray_height"), { target: { value: "-16" } });
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(save());
    expect(getYamlScalar(readYamlConfig(saved.mock.lastCall![0]), ["anticheat", "anti-xray", "max-block-height"])).toBe(-16n);
  });

  it("uses the Purpur message path and preserves multiline placeholders shared with Text", async () => {
    const { saved } = editor("purpur.yml", "settings:\n  messages:\n    afk-broadcast-away: '%s idle'\n    afk-broadcast-use-display-name: true\n  afk-broadcast-away: wrong path\n");
    fireEvent.change(input("purpur_afk_away"), { target: { value: "%s first\nsecond" } });
    await mode("text");
    await mode("form");
    expect(input("purpur_afk_away").value).toBe("%s first\nsecond");
    await userEvent.click(save());
    const document = readYamlConfig(saved.mock.lastCall![0]);
    expect(getYamlScalar(document, ["settings", "messages", "afk-broadcast-away"])).toBe("%s first\nsecond");
    expect(getYamlScalar(document, ["settings", "afk-broadcast-away"])).toBe("wrong path");
  });

  it("offers Text repair for unsupported structure and retains the original draft", async () => {
    const source = "settings: &shared\n  allow-end: true\ncopy: *shared\n";
    const { changed, saved } = editor("bukkit.yml", source);
    expect(screen.getByRole("alert").textContent).toBe(t("yaml_parse_error"));
    expect(save().disabled).toBe(true);
    expect(changed).not.toHaveBeenCalled();
    await mode("text");
    fireEvent.change(await screen.findByRole("textbox", { name: t("file_content") }), { target: { value: "settings:\n  allow-end: false\n" } });
    await mode("form");
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(save());
    expect(saved).toHaveBeenCalledWith("settings:\n  allow-end: false\n");
  });

  it("prevents field edits, mode changes and shortcuts while disabled", async () => {
    const { changed, saved } = editor("bukkit.yml", "spawn-limits:\n  monsters: 70\n", true);
    fireEvent.change(input("bukkit_monsters"), { target: { value: "30" } });
    fireEvent.keyDown(input("bukkit_monsters"), { key: "s", ctrlKey: true });
    await mode("text");
    expect(screen.getByRole("radio", { name: t("properties_form") }).getAttribute("aria-checked")).toBe("true");
    expect(changed).not.toHaveBeenCalled();
    expect(saved).not.toHaveBeenCalled();
  });
});
