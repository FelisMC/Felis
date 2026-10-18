import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

// Use ESLint for hooks and effect dependencies; TypeScript handles types.
// CI runs it with --max-warnings 0; a deliberate gap in a dependency list carries a disable comment that says why.
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
