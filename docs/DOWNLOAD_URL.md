# download_url: streamed downloads and HTML interstitials

`download_url` is the native URL → workspace fetch. The HTTP request runs
host-side under the SSRF guard (ADR-0036's brokered-fetch class); every byte
lands in the conversation workspace through the sandbox FileOp seam.

## What shipped

- **Streamed writes.** The body moves into the sandbox in 16 MB chunks through
  the `stream` FileOp: each chunk is appended, at a checked offset, to a hidden
  `.fleet-download-<random>.partial` file beside the destination; the last
  chunk renames it into place with link + unlink, never replacing an existing
  file (on a filesystem without hard links, `renameat2(RENAME_NOREPLACE)`;
  with neither, the download fails rather than risk a replacing rename).
  Every chunk checks the partial file holds exactly the bytes written so far,
  in the executor and again in Fleet, and each step (append or commit) passes
  the ctime the step before reported: if anything else wrote to, truncated or
  touched the partial file in between, even overwriting the same length, the
  executor refuses to go on. That costs a `fstat`, not a re-hash of the file,
  so a 512 MB commit stays well inside the file-op time limit. A process in
  the sandbox can still change the file after the commit, as it can any
  workspace file; `sha256` describes the body Fleet received and wrote.
  Host memory stays at about one chunk whatever the file size.
- **512 MB cap** (was 100 MB, when the whole body was buffered). A body that
  declares more is refused before anything is written; one that streams past
  the cap is aborted and its partial file removed. The cap covers the largest
  files an email archive's link cache keeps (450 MB in Elcano's deployment).
- **Failure leaves nothing behind.** Any error aborts the partial file. If the
  abort itself cannot run (a cancelled turn retires the sandbox first), the
  first chunk of the next streamed write into that directory removes partial
  files untouched for an hour, and the hourly maintenance pass
  (`store.SweepStaleDownloadPartials`) removes them from every conversation
  workspace even if no later download comes.
- **Downloads cannot fill the disk.** Before each chunk the free space of the
  filesystem holding the destination is measured (`diskguard.Usage`); a
  download that would leave less than 5% of it (the default
  `FLEET_DISK_MIN_FREE_PERCENT` floor) or 2 GiB, whichever is larger, is
  refused or aborted with its partial file removed. A body that declares its
  length is checked whole before anything is written. A chunk that passes the
  check stays reserved until it is on disk, so concurrent downloads cannot all
  pass against the same free space. The check fails open:
  a disk that cannot be measured does not block downloads.
- **Re-downloads do not pile up.** Files are named `<stem>__<url hash><ext>`; a
  re-download of the same URL whose bytes match an earlier copy reuses that
  file (`deduplicated: true`) instead of writing another; the search for one
  runs past deleted copies (it ends after 8 missing names in a row) and hashes
  only files no larger than the body, under a time limit scaled to its size.
  Only changed content gets a new name: the first free one of `_1`, `_2`, …. Completed downloads otherwise live and go with
  their conversation's workspace (conversation TTL, deletion, the admin
  cleanup). `.fleet-download-<24 hex>.partial` is a name
  Fleet reserves for this; nothing else should create files with it.
- **HTML interstitials are followed to the file**, up to 3 hops: a meta refresh,
  or a page whose only link names a data or attachment file (`.csv`, `.xlsx`,
  `.zip`, `.pdf`, `.doc`, `.png`, …).
  The page is tokenized (`golang.org/x/net/html`), so markup inside a comment,
  a script or an attribute value is never followed. Hops use the same SSRF-guarded client and a
  per-call cookie jar (a gateway that sets a session cookie on the page gets
  it back on the file request), and are listed in `html_chase`. A page naming
  several files, or none, is saved as it is.
- **Result fields:** `sha256` of the saved file (hashed while streaming), and a
  `warning` when a link naming a data file (the requested URL, an interstitial
  target, or the final URL) returned HTML — usually a login page or an expired link.
- **Timeout** covers the whole download: 300 s by default, 1800 s at most.

## Deviations

- The stream op is restricted to `.fleet-download-*.partial` names, in both the
  Go request check and the in-sandbox executor, so it cannot extend or delete a
  file the agent made. ADR-0036 records it.
- Email-specific guidance (which connector hands out the URL) stays in client
  bundles; the tool description speaks of "an email connector".

## Deferred

- Resuming an interrupted download with HTTP Range requests.
- Following JavaScript-driven interstitials (only meta refresh and plain links
  are followed).
