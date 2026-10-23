// @vitest-environment jsdom
import { useState } from "react";
import { beforeAll, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { ServerPropertiesEditor } from "./ServerPropertiesEditor";

vi.mock("./TextFileEditor", () => ({
  TextFileEditor: ({ value, onChange, label }: { value: string; onChange: (text: string) => void; label: string }) =>
    <textarea aria-label={label} value={value} onChange={(event) => onChange(event.target.value)} />,
}));

beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const t = (key: string, vars: Record<string, string | number> = {}) => i18next.t(`files:${key}`, vars);
const field = (key: string) => screen.getByRole("textbox", { name: t(`properties_field_${key}`) }) as HTMLInputElement | HTMLTextAreaElement;
const save = () => screen.getByRole("button", { name: t("save") }) as HTMLButtonElement;
async function mode(view: "form" | "text") {
  await userEvent.click(screen.getByRole("radio", { name: t(`properties_${view}`) }));
}

function editor(source: string, server = "lobby", disabled = false) {
  const changed = vi.fn();
  const saved = vi.fn();
  function Harness() {
    const [text, setText] = useState(source);
    const [valid, setValid] = useState(true);
    return <>
      <ServerPropertiesEditor server={server} value={text} original={source} disabled={disabled}
        onChange={(next) => { changed(next); setText(next); }} onSave={() => saved(text)} onValidityChange={setValid} />
      <button disabled={!valid || text === source || disabled} onClick={() => saved(text)}>{t("save")}</button>
    </>;
  }
  render(<Harness />);
  return { changed, saved };
}

describe("server.properties form", () => {
  it("keeps unedited source, missing values and custom values unchanged across mode switches", async () => {
    const source = "# retained\r\nmax-players : 20\r\ndifficulty=custom\r\nallow-flight=legacy\r\nview-distance=2\r\nunknown\\ key:keep\\nthis";
    const { changed, saved } = editor(source);
    expect(save().disabled).toBe(true);
    expect(screen.getByRole("combobox", { name: t("properties_field_difficulty") }).textContent)
      .toBe(t("properties_current_value", { value: "custom" }));
    expect(screen.getByRole("combobox", { name: t("properties_field_use_native_transport") }).textContent).toBe(t("properties_unset"));
    await mode("text");
    expect((await screen.findByRole("textbox", { name: t("file_content") }) as HTMLTextAreaElement).value).toBe(source.replaceAll("\r\n", "\n"));
    await mode("form");
    expect(save().disabled).toBe(true);
    expect(changed).not.toHaveBeenCalled();
    expect(saved).not.toHaveBeenCalled();
    fireEvent.change(field("max_players"), { target: { value: "30" } });
    expect(save().disabled).toBe(false);
    await userEvent.click(save());
    expect(saved).toHaveBeenCalledWith(source.replace("max-players : 20", "max-players : 30"));
  });

  it("shares edits between form and text and escapes multiline descriptions without replacing other lines", async () => {
    const source = "# keep\nmax-players:20\nmotd=First\\nSecond\nunknown = untouched\n";
    const { changed, saved } = editor(source);
    expect(field("motd").value).toBe("First\nSecond");
    fireEvent.change(field("max_players"), { target: { value: "42" } });
    await mode("text");
    const raw = await screen.findByRole("textbox", { name: t("file_content") });
    const edited = source.replace("max-players:20", "max-players:42").replace("unknown = untouched", "unknown = edited in text");
    fireEvent.change(raw, { target: { value: edited } });
    await mode("form");
    expect(field("max_players").value).toBe("42");
    fireEvent.change(field("motd"), { target: { value: "New first\nNew second" } });
    expect(changed).toHaveBeenLastCalledWith(edited.replace("motd=First\\nSecond", "motd=New first\\nNew second"));
    fireEvent.keyDown(field("motd"), { key: "s", metaKey: true });
    expect(saved).toHaveBeenCalledWith(edited.replace("motd=First\\nSecond", "motd=New first\\nNew second"));
  });

  it.each(["lobby", "login"])("keeps startup-managed values read-only on %s while allowlist stays editable", async (server) => {
    const { changed } = editor(`max-players=${server === "login" ? "-1" : "20"}\nwhite-list=false\nserver-port=25565\nonline-mode=false\nenable-rcon=true\nrcon.port=25575\nrcon.password=<redacted by felis>\nbungeecord=false\nbungee-guard=false\nvelocity-modern=true\nforwarding-secrets=<redacted by felis>\n`, server);
    for (const key of ["server_port", "online_mode", "enable_rcon", "rcon_port", "rcon_password"]) expect(field(key).readOnly).toBe(true);
    expect(field("max_players").readOnly).toBe(server === "login");
    if (server === "login") {
      for (const key of ["bungeecord", "bungee_guard", "velocity_modern", "forwarding_secrets"]) expect(field(key).readOnly).toBe(true);
    }
    const allowlist = screen.getByRole("combobox", { name: t("properties_field_white_list") });
    expect(allowlist).toHaveProperty("disabled", false);
    await userEvent.click(allowlist);
    await userEvent.click(await screen.findByRole("option", { name: t("properties_option_true") }));
    expect(changed).toHaveBeenLastCalledWith(expect.stringContaining("white-list=true\n"));
  });

  it.each([
    ["max_players", "-1", "0", 0, 2147483647],
    ["max_players", "2147483648", "2147483647", 0, 2147483647],
    ["player_idle_timeout", "1.5", "0", 0, 2147483647],
    ["view_distance", "2", "3", 3, 32],
    ["simulation_distance", "33", "32", 3, 32],
    ["entity_broadcast_range_percentage", "9", "1000", 10, 1000],
    ["spawn_protection", "-1", "0", 0, 2147483647],
    ["spawn_protection", "2147483648", "2147483647", 0, 2147483647],
    ["network_compression_threshold", "-2147483649", "-2", -2147483648, 2147483647],
    ["network_compression_threshold", "2147483648", "-2147483648", -2147483648, 2147483647],
    ["rate_limit", "-1", "0", 0, 2147483647],
    ["rate_limit", "2147483648", "2147483647", 0, 2147483647],
    ["max_world_size", "0", "1", 1, 29999984],
    ["max_world_size", "29999985", "29999984", 1, 29999984],
  ] as const)("blocks invalid %s=%s and accepts %s", (key, invalid, valid, min, max) => {
    const { saved } = editor("max-players=20\nview-distance=10\nsimulation-distance=10\nentity-broadcast-range-percentage=100\n");
    const input = field(key);
    fireEvent.change(input, { target: { value: invalid } });
    expect(screen.getByRole("alert").textContent).toBe(t("properties_invalid_integer", { min, max }));
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(save().disabled).toBe(true);
    fireEvent.keyDown(input, { key: "s", ctrlKey: true });
    expect(saved).not.toHaveBeenCalled();
    fireEvent.change(input, { target: { value: valid } });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(save().disabled).toBe(false);
  });

  it("validates enum and boolean changes made in text when returning to the form", async () => {
    editor("difficulty=normal\nallow-flight=false\nfunction-permission-level=2\n");
    await mode("text");
    fireEvent.change(await screen.findByRole("textbox", { name: t("file_content") }), {
      target: { value: "difficulty=impossible\nallow-flight=perhaps\nfunction-permission-level=0\n" },
    });
    await mode("form");
    expect(screen.getAllByText(t("properties_invalid_enum"))).toHaveLength(2);
    expect(screen.getByText(t("properties_invalid_boolean"))).toBeTruthy();
    expect(save().disabled).toBe(true);
    await mode("text");
    expect(save().disabled).toBe(false);
  });

  it("keeps current files free of missing legacy properties", async () => {
    const { saved } = editor("# keep\nmotd=current\n");
    for (const key of ["pvp", "enable_command_block", "allow_nether", "spawn_monsters", "spawn_animals", "spawn_npcs"]) {
      expect(screen.queryByRole("combobox", { name: t(`properties_field_${key}`) })).toBeNull();
    }
    fireEvent.change(field("motd"), { target: { value: "edited" } });
    await userEvent.click(save());
    expect(saved).toHaveBeenCalledWith("# keep\nmotd=edited\n");
  });

  it("shows existing legacy properties with version guidance and hides them after text removal", async () => {
    const keys = ["pvp", "enable-command-block", "allow-nether", "spawn-monsters", "spawn-animals", "spawn-npcs"];
    const source = "# legacy\n" + keys.map((key) => `${key}=true\n`).join("");
    const { changed } = editor(source);
    for (const key of keys) {
      const token = key.replaceAll("-", "_");
      const control = screen.getByRole("combobox", { name: t(`properties_field_${token}`) });
      expect(control.textContent).toBe(t("properties_option_true"));
      expect(document.getElementById(control.getAttribute("aria-describedby")!)?.textContent).toBe(t(`properties_hint_${token}`));
    }
    expect(save().disabled).toBe(true);
    expect(changed).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("combobox", { name: t("properties_field_enable_command_block") }));
    await userEvent.click(await screen.findByRole("option", { name: t("properties_option_false") }));
    expect(changed).toHaveBeenLastCalledWith(source.replace("enable-command-block=true", "enable-command-block=false"));
    await mode("text");
    fireEvent.change(await screen.findByRole("textbox", { name: t("file_content") }), { target: { value: "motd=modern\n" } });
    await mode("form");
    for (const key of keys) {
      expect(screen.queryByRole("combobox", { name: t(`properties_field_${key.replaceAll("-", "_")}`) })).toBeNull();
    }
  });

  it("adds resource pack and function settings through existing controls while retaining unknown properties", async () => {
    const source = "# keep\ncustom-key=retained\n";
    const { saved } = editor(source);
    for (const key of ["hide_online_players", "enforce_secure_profile", "broadcast_console_to_ops", "broadcast_rcon_to_ops", "require_resource_pack"]) {
      expect(screen.getByRole("combobox", { name: t(`properties_field_${key}`) })).toHaveProperty("disabled", false);
    }
    fireEvent.change(field("resource_pack"), { target: { value: "https://example.com/client-pack.zip?version=2" } });
    fireEvent.change(field("resource_pack_prompt"), { target: { value: '{\n  "text": "Pack ready"\n}' } });
    await userEvent.click(screen.getByRole("combobox", { name: t("properties_field_require_resource_pack") }));
    await userEvent.click(await screen.findByRole("option", { name: t("properties_option_true") }));
    await userEvent.click(screen.getByRole("combobox", { name: t("properties_field_function_permission_level") }));
    await userEvent.click(await screen.findByRole("option", { name: "4" }));
    await userEvent.click(save());
    expect(saved).toHaveBeenCalledWith(source + 'resource-pack=https://example.com/client-pack.zip?version=2\nresource-pack-prompt={\\n  "text": "Pack ready"\\n}\nrequire-resource-pack=true\nfunction-permission-level=4\n');
  });

  it.each([
    ["resource_pack_sha1", "a".repeat(39), "ABCDEF0123456789".repeat(2) + "ABCDEF01", "sha1"],
    ["resource_pack_sha1", "g".repeat(40), "0".repeat(40), "sha1"],
    ["resource_pack_sha1", "a".repeat(41), "f".repeat(40), "sha1"],
    ["resource_pack_id", "00000000000000000000000000000000", "00000000-0000-0000-0000-000000000000", "uuid"],
    ["resource_pack_id", "z0000000-0000-0000-0000-000000000000", "ABCDEF01-2345-6789-ABCD-EF0123456789", "uuid"],
  ])("validates optional %s values", (key, invalid, valid, problem) => {
    editor("motd=current\n");
    const input = field(key);
    fireEvent.change(input, { target: { value: invalid } });
    expect(screen.getByRole("alert").textContent).toBe(t(`properties_invalid_${problem}`));
    expect(save().disabled).toBe(true);
    fireEvent.change(input, { target: { value: valid } });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(save().disabled).toBe(false);
    fireEvent.change(input, { target: { value: "" } });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(save().disabled).toBe(false);
  });

  it("offers text repair for invalid Unicode and never reconstructs the file", async () => {
    const { changed, saved } = editor("# keep\nunknown=\\u12xz\n");
    expect(screen.getByRole("alert").textContent).toBe(t("properties_parse_error"));
    expect(changed).not.toHaveBeenCalled();
    expect(save().disabled).toBe(true);
    await mode("text");
    const raw = await screen.findByRole("textbox", { name: t("file_content") });
    expect(raw).toHaveProperty("value", "# keep\nunknown=\\u12xz\n");
    fireEvent.change(raw, { target: { value: "# keep\nunknown=repaired\n" } });
    await mode("form");
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(save());
    expect(saved).toHaveBeenCalledWith("# keep\nunknown=repaired\n");
  });

  it("disables form changes and shortcuts while a file operation is pending", async () => {
    const { changed, saved } = editor("max-players=20\n", "lobby", true);
    await userEvent.type(field("max_players"), "4");
    fireEvent.keyDown(field("max_players"), { key: "s", ctrlKey: true });
    await mode("text");
    expect(screen.getByRole("radio", { name: t("properties_form") }).getAttribute("aria-checked")).toBe("true");
    expect(changed).not.toHaveBeenCalled();
    expect(saved).not.toHaveBeenCalled();
  });
});
