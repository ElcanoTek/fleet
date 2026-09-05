// plural renders "1 row" / "2 rows" — the orchestrator's one count formatter,
// replacing the "row(s)" / "run(s)" / "bucket(s)" hedges that used to sit in
// the datasets, usage and upcoming panels. `many` defaults to `one` + "s";
// pass it for irregular nouns.
export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}
