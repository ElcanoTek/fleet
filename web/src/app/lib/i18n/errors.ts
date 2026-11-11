import messages from "./backend-messages.json";
import displayMessages from "./server-display-messages.json";
import { getLanguage, translate, type Language } from "./index";

function prepare(catalog: typeof messages) {
  return {
    exact: new Set(catalog.exact),
    formatted: catalog.formatted.map(({ message, fields }) => ({
      message,
      fields,
      parts: message.split(/\{\d+\}/g),
    })).sort((a, b) => b.parts.join("").length - a.parts.join("").length),
  };
}
const errors = prepare(messages);
const display = prepare(displayMessages);

// Only templates extracted from Fleet's HTTP error surfaces are recognized.
// Matching uses bounded string scans, not regexes over server/user values.
// Captures are interpolated once: filenames, tool errors and IDs stay literal.
export function localizeError(source: string, language: Language = getLanguage()): string {
  return localizeKnownMessage(source, language, errors);
}

/** Only use for owned doctor/forecast fields, never user text or tool errors. */
export function localizeServerMessage(source: string, language: Language = getLanguage()): string {
  return localizeKnownMessage(source, language, display);
}

function localizeKnownMessage(source: string, language: Language, { exact, formatted }: ReturnType<typeof prepare>): string {
  if (language === "en" || typeof source !== "string") return source;
  if (exact.has(source)) return translate(language, source);
  if (source.length > 4096) return source;
  for (const { message, parts, fields } of formatted) {
    if (!source.startsWith(parts[0])) continue;
    const values: string[] = [];
    let offset = parts[0].length;
    let matched = true;
    for (let index = 0; index < fields.length; index += 1) {
      const delimiter = parts[index + 1];
      const last = index === fields.length - 1;
      const end = last
        ? (source.endsWith(delimiter) ? source.length - delimiter.length : -1)
        : (delimiter ? source.indexOf(delimiter, offset) : -1);
      if (end < offset) { matched = false; break; }
      const value = source.slice(offset, end);
      if (/d$/.test(fields[index]) && !/^-?\d+$/.test(value)) { matched = false; break; }
      values.push(value);
      offset = end + delimiter.length;
    }
    if (matched && offset === source.length) return translate(language, message, values);
  }
  return source;
}
