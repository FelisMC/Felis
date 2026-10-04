import { useEffect, useRef } from "react";
import { basicSetup } from "codemirror";
import { EditorView, keymap } from "@codemirror/view";
import { EditorState, Compartment } from "@codemirror/state";
import { StreamLanguage, syntaxHighlighting, HighlightStyle } from "@codemirror/language";
import { tags } from "@lezer/highlight";
import { yaml } from "@codemirror/legacy-modes/mode/yaml";
import { properties } from "@codemirror/legacy-modes/mode/properties";
import { toml } from "@codemirror/legacy-modes/mode/toml";
import { json, javascript } from "@codemirror/legacy-modes/mode/javascript";
import { xml } from "@codemirror/legacy-modes/mode/xml";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { useTheme } from "@/lib/theme";

const languages = { yaml, yml: yaml, properties, toml, json, js: javascript, xml, sh: shell };

export function TextFileEditor({ path, value, onChange, onSave, label }: {
  path: string;
  value: string;
  onChange: (value: string) => void;
  onSave: () => void;
  label: string;
}) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const themeConfig = useRef(new Compartment());
  const callbacks = useRef({ onChange, onSave });
  const { theme } = useTheme();
  useEffect(() => { callbacks.current = { onChange, onSave }; }, [onChange, onSave]);

  useEffect(() => {
    const extension = path.split(".").pop()?.toLowerCase() ?? "";
    const language = languages[extension as keyof typeof languages];
    const lineBreak = value.includes("\r\n") ? "\r\n" : value.includes("\r") && !value.includes("\n") ? "\r" : "\n";
    const editor = new EditorView({
      parent: host.current!,
      state: EditorState.create({
        doc: value,
        extensions: [
          basicSetup,
          language ? StreamLanguage.define(language) : [],
          EditorState.tabSize.of(2),
          EditorState.lineSeparator.of(lineBreak),
          EditorView.contentAttributes.of({ "aria-label": label }),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) callbacks.current.onChange(update.state.sliceDoc());
          }),
          keymap.of([{ key: "Mod-s", run: () => { callbacks.current.onSave(); return true; } }]),
          themeConfig.current.of(editorTheme(theme === "dark")),
        ],
      }),
    });
    view.current = editor;
    editor.focus();
    return () => { editor.destroy(); view.current = null; };
    // A file gets one editor; text and theme changes update it without losing history.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, label]);

  useEffect(() => {
    const editor = view.current;
    if (editor && editor.state.sliceDoc() !== value) {
      editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: value } });
    }
  }, [value]);
  useEffect(() => {
    view.current?.dispatch({ effects: themeConfig.current.reconfigure(editorTheme(theme === "dark")) });
  }, [theme]);

  return <div ref={host} className="min-w-0 overflow-hidden rounded-md border border-input" />;
}

function editorTheme(dark: boolean) {
  return [EditorView.theme({
    "&": { height: "50vh", color: "hsl(var(--foreground))", backgroundColor: "hsl(var(--background))", fontSize: "13px" },
    "&.cm-focused": { outline: "2px solid hsl(var(--ring) / .5)", outlineOffset: "-2px" },
    ".cm-scroller": { fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", lineHeight: "1.7" },
    ".cm-content": { padding: "12px 0", caretColor: "hsl(var(--foreground))" },
    ".cm-gutters": { backgroundColor: "hsl(var(--card))", color: "hsl(var(--muted-foreground))", borderColor: "hsl(var(--border))" },
    ".cm-activeLine, .cm-activeLineGutter": { backgroundColor: "hsl(var(--muted) / .5)" },
    "&.cm-focused .cm-selectionBackground, .cm-selectionBackground": { backgroundColor: "hsl(var(--primary) / .2)" },
  }, { dark }), syntaxHighlighting(HighlightStyle.define([
    { tag: [tags.keyword, tags.bool, tags.null], color: dark ? "#c4b5fd" : "#7c3aed" },
    { tag: [tags.propertyName, tags.variableName, tags.atom, tags.meta], color: dark ? "#7dd3fc" : "#0369a1" },
    { tag: [tags.string, tags.quote], color: dark ? "#86efac" : "#15803d" },
    { tag: tags.number, color: dark ? "#fdba74" : "#c2410c" },
    { tag: tags.comment, color: dark ? "#94a3b8" : "#64748b", fontStyle: "italic" },
  ]))];
}
