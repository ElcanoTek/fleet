// The grammars fleet highlights, registered once into one table. Keep the
// set in sync with syntaxSupportedLanguages in ../../ToolChips.tsx and
// languageForPath in orchestrator/logTimeline.ts.

import type { Grammar, PrismLike } from "../prism";
import { bash } from "./bash";
import { json } from "./json";
import { markup } from "./markup";
import { python } from "./python";
import { yaml } from "./yaml";

const prism: PrismLike = { languages: {} };
for (const register of [markup, python, bash, json, yaml]) {
  register(prism);
}

/** The grammar for a language name or alias, or undefined. */
export function grammarFor(language: string): Grammar | undefined {
  return Object.prototype.hasOwnProperty.call(prism.languages, language) ? prism.languages[language] : undefined;
}
