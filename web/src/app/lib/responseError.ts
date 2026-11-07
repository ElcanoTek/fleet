// responseErrorText — the one way the UI turns a failed fetch Response into a
// message for an error banner (#1956).
//
// The Go handlers answer a refused request with a short plain-text reason
// ("name already taken", "over the cap"), and those words are what the user
// should see. Two things can go wrong with showing the body as-is: it can be
// empty (a bare 500, a 204-less proxy error), which used to render an empty
// banner, and it can be a whole HTML error page from a proxy in front of
// fleet. So the body is trimmed and capped, and it is replaced by the
// caller's fallback — with the status, so the banner still says which failure
// it was — when it is empty or is an HTML page (by Content-Type or by shape). Reading the body never throws: a stream error or
// an already-consumed body yields the fallback too.

/** Longest body shown verbatim; a longer one is cut and ends in "…". */
export const RESPONSE_ERROR_MAX_CHARS = 300;

// An HTML document or fragment: a proxy's error page, never a handler reason.
const HTML_BODY = /^<(?:!doctype|html|head|body|\?xml|[a-z][a-z0-9]*[\s>])/i;

function isHTML(res: Response, text: string): boolean {
  const type = res.headers?.get?.("content-type") ?? "";
  return type.toLowerCase().includes("text/html") || HTML_BODY.test(text);
}

export async function responseErrorText(res: Response, fallback: string): Promise<string> {
  let text = "";
  try {
    text = (await res.text()).trim();
  } catch {
    text = "";
  }
  if (!text || isHTML(res, text)) return `${fallback} (${res.status})`;
  if (text.length > RESPONSE_ERROR_MAX_CHARS) {
    return `${text.slice(0, RESPONSE_ERROR_MAX_CHARS).trimEnd()}…`;
  }
  return text;
}
