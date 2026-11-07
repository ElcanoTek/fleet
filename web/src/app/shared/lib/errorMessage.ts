// errorMessage — the one way a catch block turns what it caught into words
// for a banner or toast (#2005).
//
// A catch binding is `unknown`: a thrown string, a rejected non-Error, or an
// Error built with no message all reach it. `(err as Error).message` showed
// "undefined" for the first two, and `err instanceof Error ? err.message : x`
// showed an empty banner for the last. errorMessage answers the Error's
// message when there is one and the caller's fallback otherwise.

export function errorMessage(err: unknown, fallback: string): string {
  if (err instanceof Error && err.message) return err.message;
  return fallback;
}
