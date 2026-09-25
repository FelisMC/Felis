import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

// Lint guards what tsc cannot: the rules of hooks (a hook behind a condition
// renders fine until the branch flips) and effect dependency lists (a stale
// closure, or a memo that recomputes every render). Types are tsc's job and
// style the formatter's, so nothing else is switched on. CI runs it with
// --max-warnings 0; a deliberate gap in a dependency list carries a disable
// comment that says why.
export default tseslint.config(
  { ignores: ["dist", "node_modules", "test-results", "playwright-report", "src/lib/openapi.gen.ts"] },
  {
    files: ["src/**/*.{ts,tsx}", "dev/**/*.ts"],
    languageOptions: {
      parser: tseslint.parser,
      ecmaVersion: 2022,
      globals: { ...globals.browser, ...globals.node },
    },
    linterOptions: { reportUnusedDisableDirectives: "error" },
    plugins: { "react-hooks": reactHooks },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "warn",
    },
  },
);
