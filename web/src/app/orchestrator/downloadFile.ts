// downloadFile fetches a report and hands it to the browser as a file, for the
// admin CSV exports (Usage, Adoption).
//
// Those buttons used to set window.location.href to the CSV endpoint. That
// works only when the response is a 200 with a Content-Disposition attachment;
// a 401 (expired session), 403 or 500 is a NAVIGATION to a raw JSON/HTML error
// page — the whole dashboard is replaced by the server's error body, and the
// operator has to use the back button to recover. Fetching first lets the
// caller check `ok` and show the failure in place; only a real body becomes an
// object URL clicked through a throwaway anchor (the LogViewer JSON download's
// pattern).
//
// The server's filename wins when it names one (Content-Disposition
// `filename=`); `fallbackName` covers a proxy that drops the header.
export async function downloadFile(url: string, fallbackName: string): Promise<void> {
  const res = await fetch(url, { credentials: "same-origin" });
  if (!res.ok) {
    throw new Error(await describeFailure(res));
  }
  const blob = await res.blob();
  const objectUrl = URL.createObjectURL(blob);
  try {
    const a = document.createElement("a");
    a.href = objectUrl;
    a.download = filenameFrom(res.headers.get("content-disposition")) ?? fallbackName;
    document.body.appendChild(a);
    a.click();
    a.remove();
  } finally {
    URL.revokeObjectURL(objectUrl);
  }
}

// describeFailure prefers the API's own `{error}` message; a non-JSON body
// (a proxy's HTML page) falls back to the status line so the toast still says
// something an operator can act on ("401 Unauthorized" → sign in again).
async function describeFailure(res: Response): Promise<string> {
  const body = (await res.json().catch(() => null)) as { error?: string } | null;
  if (body?.error) return body.error;
  return `HTTP ${res.status}${res.statusText ? ` ${res.statusText}` : ""}`;
}

function filenameFrom(disposition: string | null): string | null {
  if (!disposition) return null;
  // filename*= (RFC 5987) first, then the plain quoted/unquoted form.
  const star = /filename\*=(?:UTF-8'')?([^;]+)/i.exec(disposition);
  if (star) {
    try {
      return decodeURIComponent(star[1].trim().replace(/^"|"$/g, ""));
    } catch {
      /* fall through to the plain form */
    }
  }
  const plain = /filename="?([^";]+)"?/i.exec(disposition);
  return plain ? plain[1].trim() : null;
}
