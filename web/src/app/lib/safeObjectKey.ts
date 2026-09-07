// Guard object-property writes keyed by a conversation slot id. CodeQL's
// js/remote-property-injection query flags a write whose property name
// traces to a value it considers user-controlled (`__proto__` /
// `constructor` / `prototype` overwrite). Conversation ids are
// server-minted UUIDs, so this is defense-in-depth; the comparisons are
// the sanitizer the query models.

export function isSafeObjectKey(key: string): boolean {
  return key !== "__proto__" && key !== "constructor" && key !== "prototype";
}

export function setOwn<T>(obj: Record<string, T>, key: string, value: T): void {
  if (key === "__proto__" || key === "constructor" || key === "prototype") return;
  obj[key] = value;
}

export function deleteOwn<T>(obj: Record<string, T>, key: string): void {
  if (key === "__proto__" || key === "constructor" || key === "prototype") return;
  delete obj[key];
}
