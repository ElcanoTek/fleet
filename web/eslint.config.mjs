import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
  // Override default ignores of eslint-config-next.
  globalIgnores([
    // Default ignores of eslint-config-next:
    ".next/**",
    "out/**",
    "build/**",
    "test-results/**",
    "next-env.d.ts",
  ]),
  {
    rules: {
      // Honor the standard `_`-prefix convention for intentionally
      // unused destructure bindings (e.g.
      // `const { retrying: _retrying, ...rest } = message;` to omit
      // a key from a spread). Without this, every such omit pattern
      // surfaces as a warning even though the underscore is the
      // explicit signal that the binding is unused on purpose.
      "@typescript-eslint/no-unused-vars": [
        "warn",
        {
          argsIgnorePattern: "^_",
          varsIgnorePattern: "^_",
          caughtErrorsIgnorePattern: "^_",
          destructuredArrayIgnorePattern: "^_",
          ignoreRestSiblings: true,
        },
      ],
    },
  },
]);

export default eslintConfig;
