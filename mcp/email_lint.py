"""Email HTML linter for Elcano agents.

Single source of truth for HTML / subject / body validation across the
Python MCP servers that can send mail (sendgrid_server.py, mailbux.py).
This file is kept BYTE-IDENTICAL in cutlass and chat — `make check`
in either repo diffs them and fails on drift, so any edit here must be
mirrored before either PR can land.

Design rules:

* **One non-stdlib dep: html5lib.** Spec-correct HTML5 parsing means
  foster-parenting and unclosed-table detection come from a real
  parser, not a hand-coded state machine. The library is pure-Python,
  has no transitive deps that aren't already pinned, and is stable
  enough that we can ride it without version drama.

* **One rule per function.** Every check is its own small named helper
  that yields zero or more `Finding`s. Adding a new failure mode is one
  function plus one test, never surgery on a 500-line class.

* **Stable rule codes.** Every `Finding` has a 2-letter prefix + 3-digit
  code (`EH001` foster-parent, `EC001` rgba, etc.). Agents reference
  codes in retries; downstream tooling can grep for them. NEVER reuse a
  retired code — append a deprecation note in `RULES` and pick the next
  number.

* **Severity = error | warning.** Errors block sending. Warnings are
  surfaced in the tool response but do not block. No third tier today;
  promote a warning to error rather than inventing `info`.

* **Pure functions, no I/O.** Path-reading, file-loading, and SendGrid
  wiring all live in the caller. This module only walks strings.

Public API:

    from email_lint import (
        validate,                     # html [+ optional subject] → list[Finding]
        Finding,                      # dataclass: rule, severity, message, hint, line
        detect_html_content,          # str → bool (HTML auto-detection)
        resolve_content_type,         # (content, explicit) → (resolved, was_corrected)
        extract_cid_references,       # html → set[str]
        find_unresolved_template_tokens,  # str → sorted list[str]
        format_findings,              # list[Finding] → "  - EH001 (error): ..."
        partition_findings,           # list[Finding] → (errors, warnings)
        RULES,                        # {code: (severity, short title)} for docs / tooling
    )

Backward-compat helpers (kept for the existing callers' single-pass swap):

    validate_html_legacy(content)         → (errors_list[str], warnings_list[str])
    validate_email_subject_legacy(s)      → str | None
    validate_email_body_legacy(s, ctype)  → str | None
    check_template_leakage_legacy(s)      → (errors_list[str], warnings_list[str])

These wrap `validate()` with the old string-list shape; new code should
use the structured `Finding` API.
"""

from __future__ import annotations

import re
from collections import Counter
from collections.abc import Iterable, Iterator
from dataclasses import dataclass, field
from typing import Any, Final

import html5lib

# ── Rule catalog ────────────────────────────────────────────────────────────
# Stable codes; never reuse a retired number. Two-letter prefix groups by
# concern so a glance at a finding tells you where to look:
#   EH  email HTML structure          (foster-parent, unclosed, deep nest)
#   EI  email image references        (data: URIs, file://, missing cid)
#   EC  email CSS                     (rgba, position:fixed/absolute)
#   ET  email template tokens         (unresolved {{...}} / ${...} / {...})
#   EL  email leakage / placeholders  (canonical_template demo data)
#   EP  email pad-cell heuristics     (cutlass canonical_template specific)
RULES: Final[dict[str, tuple[str, str]]] = {
    # Structure (EH)
    "EH001": ("error", "Foster-parented element (invalid table child)"),
    "EH002": ("error", "Loose text foster-parented out of table"),
    "EH003": ("error", "Unclosed HTML tags"),
    "EH004": ("error", "Closing tag with no matching opening tag"),
    "EH005": ("warning", "Tag closed before nested tags"),
    "EH006": ("warning", "Table with inconsistent column counts"),
    "EH007": ("warning", "Table without any <tr> rows"),
    "EH008": ("warning", "Table without any <td>/<th> cells"),
    "EH009": ("warning", "No common content tags found"),
    "EH010": ("warning", "Very deep nesting"),
    "EH011": ("error", "HTML parsing error"),
    "EH012": ("error", "HTML content empty"),
    "EH013": ("error", "Content-type is text/html but no HTML tags found"),
    # Images (EI)
    "EI001": ("error", "Image with empty src"),
    "EI002": ("error", "Image src is a local file:// path"),
    "EI003": ("error", "Image src is a data: URI (fabricated inline)"),
    "EI004": ("error", "Image src must be cid: or http(s)://"),
    "EI005": ("error", "Inline cid: references with no inline_attachments"),
    "EI006": ("error", "Inline cid: references missing from inline_attachments"),
    # CSS (EC)
    "EC001": ("error", "CSS rgba() colors (alpha stripped by Outlook)"),
    "EC002": ("warning", "CSS position:fixed not supported in email"),
    "EC003": ("warning", "CSS position:absolute limited support in email"),
    # Tokens (ET)
    "ET001": ("error", "Unresolved template tokens in body"),
    "ET002": ("error", "Unresolved template tokens in subject"),
    # Leakage / placeholders (EL)
    "EL001": ("error", "Body is a placeholder value"),
    "EL002": ("error", "Body is too short or empty"),
    "EL003": ("error", "Subject is empty"),
    "EL101": ("error", "Template leakage: data-preview attributes survived"),
    "EL102": ("error", "Template leakage: demo prose survived"),
    "EL103": ("error", "Template leakage: ≥3 demo markers co-occur"),
    "EL104": ("warning", "Possible template leakage: 1-2 demo markers found"),
    # Pad-cell heuristics (EP) — cutlass canonical_template, but the rule
    # is harmless in chat too (no false positives on hand-rolled HTML).
    "EP001": ("error", "Pad-class <td> nested inside another pad-class <td>"),
}


@dataclass(frozen=True)
class Finding:
    """One validator hit. Findings compose into the full validate() result.

    `rule` MUST be a key in RULES. `severity` is derived from RULES so the
    caller can't accidentally drift severity from the catalog.
    """

    rule: str
    message: str
    hint: str = ""
    line: int | None = None
    # `severity` defaults from the catalog — set explicitly only in tests.
    severity: str = field(default="")

    def __post_init__(self) -> None:
        if self.rule not in RULES:
            raise ValueError(f"Unknown rule code: {self.rule}")
        if not self.severity:
            object.__setattr__(self, "severity", RULES[self.rule][0])

    def format(self) -> str:
        """One-line human form: 'EH001 (error): message — hint'."""
        head = f"{self.rule} ({self.severity}): {self.message}"
        if self.hint:
            return f"{head} — {self.hint}"
        return head


# ── Constants used by checks ────────────────────────────────────────────────

DEFAULT_CONTENT_TYPE = "text/html"

_HTML_TAG_PATTERN = re.compile(
    r"<\s*(?:html|head|body|div|span|p|br|hr|table|tr|td|th|ul|ol|li|"
    r"h[1-6]|a|img|strong|em|b|i|u|style|script|link|meta|header|footer|"
    r"nav|section|article|aside|main|form|input|button|label|select|"
    r"textarea|iframe|video|audio|canvas|svg|!DOCTYPE)\b[^>]*>",
    re.IGNORECASE,
)
_HTML_ENTITY_PATTERN = re.compile(r"&(?:nbsp|amp|lt|gt|quot|apos|#\d+|#x[0-9a-fA-F]+);")

_UNRESOLVED_TEMPLATE_PATTERNS = (
    re.compile(r"\$\{[^{}]+\}"),
    re.compile(r"\{\{[^{}]+\}\}"),
    re.compile(r"(?<![\$\{])\{[A-Za-z_][A-Za-z0-9_]*(?::[A-Za-z0-9_.,%+\-<>^]+)?\}(?!\})"),
)

_IMG_SRC_PATTERN = re.compile(r'<img[^>]*src\s*=\s*["\']([^"\']*)["\']', re.IGNORECASE)
_CID_PATTERN = re.compile(r"cid:([A-Za-z0-9_.@\-]+)", re.IGNORECASE)

_PLACEHOLDER_VALUES = frozenset({"html_content", "content", "BODY", "text_content", "PUT_CONTENT_HERE"})

# ── Template leakage detection ──────────────────────────────────────────────
# protocols/email-style.yaml#canonical_template carries demo content that
# must not reach recipients. Three tiers by false-positive risk:
#   1. `data-preview="..."` attributes live ONLY on demo rows — survivors
#      mean the agent copy-pasted, not regenerated.
#   2. Full demo-prose sentences are long enough that real analyst output
#      won't collide. Hard block.
#   3. Brand names and specific dollar figures CAN legitimately recur in
#      real reporting. Warn on 1–2 hits; block only if ≥3 co-occur (real
#      data won't collocate "Amazon US OLV" + the exact demo amount +
#      "Pepsi Display" in one email).
_DATA_PREVIEW_ATTR_PATTERN = re.compile(r'\bdata-preview\s*=\s*"([^"]*)"')

_TIER2_DEMO_PROSE_MARKERS: tuple[str, ...] = (
    "driven primarily by Amazon OLV pacing cuts that began on Apr 3",
    "biggest production risk is continued weekend underdelivery",
    "immediate opportunity is recovering volume in stable Canada inventory",
    "weekend spend fell to roughly $755/day after pacing above $104,000/day",
    "Canada display inventory stayed efficient, creating room to shift spend",
    "Hold the dark header plus white-body structure, keep all critical colors",
)

_TIER3_DEMO_MARKERS: tuple[str, ...] = (
    "Amazon US OLV",
    "Amazon CA Display",
    "Pepsi Display",
    "$589,075.45",
    "63,400,529",
    "$311,402",
    "$405,928",
    "$72,119",
    "$68,004",
    "$54,882",
    "$58,131",
    "$9.29",
)

_TIER3_BLOCK_THRESHOLD = 3

# ── HTML structure constants ────────────────────────────────────────────────

# Section-wrapper classes used on the canonical_template's top-level <td>s.
# A pad cell nested inside another pad cell is the smoking-gun signal that
# an inner badge/utility <table> was never closed. Cutlass-specific but
# harmless in chat (no false positives on hand-rolled HTML).
_PAD_CLASSES = frozenset({"header-pad", "status-pad", "body-pad", "footer-pad", "table-pad"})

# Table-family element names — anything that the foster-parent algorithm
# treats as a "this element belongs inside a table" container. When an
# offending start tag is reported by html5lib, its parent context (for
# our EH001 message) is the deepest of these on the open-elements stack.
_TABLE_FAMILY = frozenset({"table", "tbody", "thead", "tfoot", "tr", "colgroup"})

# html5lib error codes that mean "an element was opened where it couldn't
# legally live, so the parser foster-parented it OUT of the table".
_FOSTER_PARENT_START_CODES: Final[frozenset[str]] = frozenset(
    {
        "unexpected-start-tag-implies-table-voodoo",
    }
)
# Nested `<table>` directly inside `<table>` (no `<td>` wrapper) doesn't
# trigger the voodoo code; html5lib emits this code with start==end==table
# instead. Treated as the same EH001 failure.
_NESTED_TABLE_CODE: Final[str] = "unexpected-start-tag-implies-end-tag"
# `<table>` (or other table-context) left open at EOF.
_EOF_IN_TABLE_CODE: Final[str] = "eof-in-table"


# ── HTML5 parser subclass that records error context ─────────────────────────


class _TrackingHTMLParser(html5lib.HTMLParser):
    """html5lib parser subclass that records the context we need for our
    structural rules.

    Two hooks:

    * `parseError` is the html5lib-provided notification hook. We record
      every tree-construction error along with the open-elements stack
      at the moment of error. The stack tells us the table-family
      ancestor the offending tag was trying to live inside, which is
      what our EH001 message needs ("<div> appears as a direct child
      of <tbody>"). html5lib doesn't put this in the error tuple, but
      `self.tree.openElements` IS the live insertion-mode stack at the
      moment `parseError` runs.

    * `processCharacters` on the foster-parenting-prone phases
      (`inTable`, `inTableBody`, `inRow`) is wrapped so we can record
      every non-whitespace character token that lands in a table
      context. html5lib silently foster-parents these — no parseError
      fires — so the wrapping is the only way to detect EH002 against
      the spec algorithm. Wrapping is done by replacing the phase
      instance with a subclass instance (phases use `__slots__`, so we
      can't monkey-patch the bound method directly).
    """

    def __init__(self) -> None:
        super().__init__(
            strict=False,
            tree=html5lib.treebuilders.getTreeBuilder("etree"),
        )
        # (pos, code, datavars, open_elements_stack)
        self.recorded_errors: list[tuple[tuple[int, int], str, dict, list[str]]] = []
        # (pos, text_data) — non-whitespace character tokens that landed
        # in a table-context phase. The same source text often fires the
        # hook on multiple phases as the parser transitions (e.g. inTable
        # then inTableBody for the same character) — caller dedups by pos.
        self.loose_text_events: list[tuple[tuple[int, int], str]] = []
        for phase_name in ("inTable", "inTableBody", "inRow"):
            self._wrap_text_handler(phase_name)

    def _wrap_text_handler(self, phase_name: str) -> None:
        original = self.phases[phase_name]
        phase_cls = type(original)
        parser_ref = self

        class WrappedPhase(phase_cls):  # type: ignore[valid-type,misc]
            def processCharacters(self, token: Any) -> Any:
                data = token.get("data", "") if isinstance(token, dict) else ""
                if data and data.strip():
                    parser_ref.loose_text_events.append((parser_ref.tokenizer.stream.position(), data))
                return super().processCharacters(token)

        # html5lib phases use __slots__, so we have to construct a fresh
        # instance of the subclass rather than reassigning a method on
        # the existing one. Phase.__init__(parser, tree) is the canonical
        # signature.
        self.phases[phase_name] = WrappedPhase(self, self.tree)

    def parseError(self, errorcode: str = "?", datavars: Any = None) -> None:
        if datavars is None:
            datavars = {}
        pos = self.tokenizer.stream.position()
        stack = [_strip_ns(el.name) for el in self.tree.openElements]
        self.recorded_errors.append((pos, errorcode, datavars, stack))
        super().parseError(errorcode, datavars)


def _strip_ns(tag: Any) -> str:
    """html5lib's etree treebuilder namespaces every tag as
    `{http://www.w3.org/1999/xhtml}div`. Strip that for our purposes.

    Returns the empty string for non-element nodes (etree represents
    `<!--comment-->` as a Comment factory callable rather than a tag
    name string, and processing instructions get similar treatment;
    none of our checks care about those).
    """
    if not isinstance(tag, str):
        return ""
    return tag.split("}", 1)[-1] if "}" in tag else tag


def _pad_classes(class_attr: str) -> set[str]:
    if not class_attr:
        return set()
    return {token for token in class_attr.split() if token in _PAD_CLASSES}


def _table_parent(stack: list[str]) -> str | None:
    """Walk the open-elements stack from deepest to shallowest and return
    the first table-family element. That's the parent context our EH001
    message references ("<div> appears as a direct child of <{this}>")."""
    for tag in reversed(stack):
        if tag in _TABLE_FAMILY:
            return tag
    return None


def _rule_text_for_parent(parent: str) -> str:
    """Human-readable HTML5 rule the offending child violates. Lets the
    agent map our message back to the spec rule without us pointing at
    URLs that might rot."""
    if parent == "table":
        return "Only <tr>, <thead>, <tbody>, <tfoot>, <caption>, or <colgroup> are valid as direct children of <table>."
    if parent in ("thead", "tbody", "tfoot"):
        return f"Only <tr> rows are valid as direct children of <{parent}>."
    if parent == "tr":
        return "Only <td> or <th> cells are valid as direct children of <tr>."
    if parent == "colgroup":
        return "Only <col> elements are valid as direct children of <colgroup>."
    return ""


# ── Foster-parent detection (EH001) ──────────────────────────────────────────


def _check_foster_parents(parser: _TrackingHTMLParser, dedup: set[tuple[str, str]]) -> Iterator[Finding]:
    """EH001 — element foster-parented out of a table container.

    The OMUS Comfluence regression (chat#201 / cutlass#469): agent
    appended a `<div>` between component `<tr>` rows; html5lib's
    foster-parent algorithm moves it OUT of the table (typically to a
    sibling of the table) and it renders above the dark Victoria header.

    Two error codes map to EH001:

    * `unexpected-start-tag-implies-table-voodoo` — fires for <div>/<p>/
      <h*>/<span>/etc. opened in a table context.
    * `unexpected-start-tag-implies-end-tag` with start==end==table —
      fires when a `<table>` is opened directly inside another `<table>`
      without a `<td>` wrapper. html5lib treats this as an implicit
      `</table>` plus a sibling `<table>`.

    Three layers of dedup keep the agent's error budget sane. Each
    layer was audited against an adversarial case (see commit history
    for the audit script); removing any one of them re-introduces
    spurious findings:

    1. **Top-of-stack must be table-family.** When a foster-parented
       `<div>` contains a `<p>`, html5lib reports both. Layer 1 drops
       the `<p>` because its source-context parent is the already-
       fostered `<div>` (top of stack), not a table-family element.
       The fix for the outer `<div>` resolves the inner `<p>` for free.

       Adversarial case (would over-report without this layer):
         `<table><div><p>0</p><p>1</p></div></table>`
         Without: 2 findings (`div in tbody`, `p in tbody`).
         With:    1 finding  (`div in tbody`). Fix the div, the p is fine.

       Legitimate cases still fire (e.g. `<table><tr><div></div></tr>`
       reports `div in tr` because the `<tr>` IS table-family).

    2. **(parent, child)** — same-shape repeated foster-parents collapse.

       Adversarial case:
         `<table>` + `<p>x</p>` × 5 + `</table>`
         Without: 5 findings (all `p in table`).
         With:    1 finding. Same fix; one error is enough signal.

       Doesn't over-collapse: distinct child tags stay distinct.
         `<table>` + `<p>...</p><div>...</div>` × 3 → 2 findings.

    3. **(child, source-position)** — the implicit-close cascade for a
       nested `<table>` fires `voodoo` + `implies-end-tag` events at
       the SAME source offset. Same source bug; one finding is enough.

       Adversarial case:
         `<table><tr><td>x</td></tr><table>...</table></table>`
         Without: 2 findings (`table in tbody`, `table in table`).
         With:    1 finding. Same source offset, same fix.
    """
    for pos, code, data, stack in parser.recorded_errors:
        child_tag: str | None = None
        if code in _FOSTER_PARENT_START_CODES:
            child_tag = data.get("name")
        elif code == _NESTED_TABLE_CODE and data.get("startName") == "table" and data.get("endName") == "table":
            child_tag = "table"
        if child_tag is None:
            continue
        if not stack or stack[-1] not in _TABLE_FAMILY:
            continue
        parent = _table_parent(stack)
        if parent is None:
            continue
        key = ("EH001", f"{parent}>{child_tag}")
        pos_key = ("EH001", f"{child_tag}@{pos[0]}:{pos[1]}")
        if key in dedup or pos_key in dedup:
            continue
        dedup.add(key)
        dedup.add(pos_key)
        rule_text = _rule_text_for_parent(parent)
        yield Finding(
            rule="EH001",
            message=(
                f"<{child_tag}> appears as a direct child of <{parent}>. "
                f"HTML5 foster-parents invalid table children OUTSIDE the table "
                f"(typically before it), so the content will NOT render where you "
                f"placed it in source — in the Elcano canonical_template this means "
                f"a <div>/<p>/<h1>/etc. you appended between component <tr> rows "
                f"will jump above the dark Victoria header. {rule_text}"
            ),
            hint=(
                "Wrap free-form prose or custom tables in `<tr><td>...</td></tr>` "
                "before inserting them into the email-container table."
            ),
            line=pos[0],
        )


# ── Loose text in table (EH002) ──────────────────────────────────────────────


def _check_loose_text(parser: _TrackingHTMLParser, dedup: set[tuple[str, str]]) -> Iterator[Finding]:
    """EH002 — non-whitespace text directly inside a `<table*>` context.

    Jules #202 (OMUS): "Hey Vlad and Mark, …" appeared between two
    component `<tr>` rows; html5lib foster-parents text OUT of the table
    just like elements, and the prose ended up above the dark Victoria
    header.

    Source: `_TrackingHTMLParser.loose_text_events` — every non-whitespace
    character token the parser processed while in a foster-parenting-
    prone phase (`inTable`, `inTableBody`, `inRow`). Spec-correct: if
    html5lib reaches one of those phases with character data, that data
    IS being foster-parented. No regex; no false positives on text
    inside `<style>` or `<script>` or attribute values.

    One finding per parse — once the agent sees there's loose text in
    the table, that's enough signal to fix it. The same source text
    often fires the hook on multiple phases as the parser transitions,
    so we dedup by source position before reporting.
    """
    if not parser.loose_text_events:
        return
    # The first event by source position is the user-actionable one.
    pos, _data = min(parser.loose_text_events, key=lambda evt: (evt[0][0], evt[0][1]))
    key = ("EH002", "table")
    if key in dedup:
        return
    dedup.add(key)
    yield Finding(
        rule="EH002",
        message=(
            "Loose text content appears as a direct child of <table>. "
            "HTML5 foster-parents text-between-rows OUTSIDE the table, so "
            "any prose written between two component rows will render above "
            "the dark Victoria header."
        ),
        hint="Wrap it in `<tr><td>...</td></tr>`.",
        line=pos[0],
    )


# ── Unclosed structural containers (EH003) ───────────────────────────────────


def _check_unclosed(parser: _TrackingHTMLParser) -> Iterator[Finding]:
    """EH003 — `<table>` (or other table-context) left open at EOF.

    html5lib's `eof-in-table` is the cleanest signal. It doesn't tell us
    *which* element specifically was unclosed, but in practice it always
    means a missing `</table>` (or a missing `</tr>` / `</td>` that
    cascades to a missing `</table>`). The fix is always the same: close
    the open table.
    """
    if any(code == _EOF_IN_TABLE_CODE for _pos, code, _data, _stack in parser.recorded_errors):
        yield Finding(
            rule="EH003",
            message="Unclosed HTML tags: <table>",
            hint="Add the missing </table> (and any missing </tr>/</td>) at the end of the email body.",
        )


# ── Bad close tags (EH004) ───────────────────────────────────────────────────

# html5lib codes that indicate a closing tag with no opener — the agent
# wrote `</div>` (or whatever) more times than they wrote `<div>`, or
# wrote `</span>` where they meant `</div>`. NOT the same as
# `unexpected-end-tag-implies-table-voodoo` (that's foster-parent noise
# we already drop in _check_foster_parents).
_BAD_CLOSE_CODES: Final[frozenset[str]] = frozenset(
    {
        "unexpected-end-tag",
        "end-tag-too-early",
        "expected-one-end-tag-but-got-another",
    }
)


def _check_bad_close(
    parser: _TrackingHTMLParser,
    foster_parent_or_unclosed_fired: bool,
    dedup: set[tuple[str, str]],
) -> Iterator[Finding]:
    """EH004 — closing tag with no matching opener.

    Catches genuine `<div>x</div></div>` and `</span>` typos. NOT
    emitted when EH001 or EH003 already fired in the same parse: those
    are root-cause issues and html5lib cascades 1-3 spurious end-tag
    errors as a consequence of foster-parenting or implicit `</table>`
    insertion. Suppressing here means the agent fixes the structural
    bug first; if a genuine extra-close survives that fix, the next
    validation pass surfaces it cleanly.

    Trade-off accepted: an agent with BOTH a foster-parent AND an
    unrelated extra-close tag in the same draft will see only the
    foster-parent on first pass. They iterate on the structural bug,
    re-validate, and the extra-close surfaces. One bug at a time is
    fine; spurious cascade noise is not.

    Dedup by `(rule, tag)` so a chain of `</td></tr></table>` typos
    doesn't produce three separate findings for the same fix.
    """
    if foster_parent_or_unclosed_fired:
        return
    for pos, code, data, _stack in parser.recorded_errors:
        if code not in _BAD_CLOSE_CODES:
            continue
        tag = data.get("name") or data.get("gotName") or "?"
        key = ("EH004", tag)
        if key in dedup:
            continue
        dedup.add(key)
        yield Finding(
            rule="EH004",
            message=f"Closing tag </{tag}> has no matching opening tag",
            hint=("Either remove the extra closing tag or add the missing opener earlier in the document."),
            line=pos[0],
        )


# ── Pad-cell nesting heuristic (EP001) ───────────────────────────────────────


def _walk_etree(elem: Any, parent_chain: list[tuple[str, dict]]) -> Iterator[tuple[str, dict, list[tuple[str, dict]]]]:
    """Depth-first walk of an html5lib etree. Yields (tag, attrib,
    parent_chain) for each element, with namespace stripped."""
    for child in elem:
        tag = _strip_ns(child.tag)
        attrib = child.attrib
        yield tag, attrib, parent_chain
        yield from _walk_etree(child, [*parent_chain, (tag, attrib)])


def _check_pad_nesting(tree: Any, dedup: set[tuple[str, str]]) -> Iterator[Finding]:
    """EP001 — `*-pad` `<td>` nested inside another `*-pad` `<td>`.

    The TWC Campaign Health Scan regression (cutlass 33e4468,
    2026-05-21): Victoria emitted `<td class="status-pad">` with an
    inner badge `<table>`, closed the inner `<tr>` but forgot
    `</table></td></tr>`. The next `<tr><td class="body-pad">…` body
    section ended up nested inside the badge table — structurally
    valid HTML5, but every subsequent row rendered squeezed inside a
    2-column badge cell.

    Not something html5lib catches (the input IS valid HTML5). Pure
    domain heuristic on the canonical_template's class convention.
    """
    for tag, attrib, chain in _walk_etree(tree, []):
        if tag not in ("td", "th"):
            continue
        own_pads = _pad_classes(attrib.get("class", ""))
        if not own_pads:
            continue
        for ancestor_tag, ancestor_attrib in chain:
            if ancestor_tag not in ("td", "th"):
                continue
            ancestor_pads = _pad_classes(ancestor_attrib.get("class", ""))
            if not ancestor_pads:
                continue
            own_label = " ".join(sorted(own_pads))
            ancestor_label = " ".join(sorted(ancestor_pads))
            key = ("EP001", f"{ancestor_label}>{own_label}")
            if key in dedup:
                break
            dedup.add(key)
            yield Finding(
                rule="EP001",
                message=(
                    f'<td class="{own_label}"> is nested inside another '
                    f'<td class="{ancestor_label}">. The canonical_template\'s *-pad '
                    f"cells (header-pad / status-pad / body-pad / footer-pad / "
                    f"table-pad) are section wrappers that sit DIRECTLY in the "
                    f'<tr> rows of <table class="email-container">. Nesting one '
                    f"inside another almost always means an inner badge or utility "
                    f"<table> inside a *-pad cell was never closed, so subsequent "
                    f"body sections render squeezed inside it instead of at the "
                    f"top level."
                ),
                hint=(
                    "Confirm every inner <table> inside a *-pad <td> is closed with "
                    '</table></td></tr> before the next <tr><td class="body-pad">…'
                    " row begins."
                ),
            )
            break


# ── Column-count consistency (EH006) ─────────────────────────────────────────


def _check_column_consistency(tree: Any) -> Iterator[Finding]:
    """EH006 — every row in a multi-row table should have the same column
    count (respecting colspan). A mismatch is a common signal of a
    forgotten `</td>` or stray cell.
    """

    def find_tables(elem: Any) -> Iterator[Any]:
        for child in elem:
            if _strip_ns(child.tag) == "table":
                yield child
            yield from find_tables(child)

    def collect_rows(elem: Any, out: list[Any]) -> None:
        for child in elem:
            tag = _strip_ns(child.tag)
            if tag == "tr":
                out.append(child)
            elif tag in ("thead", "tbody", "tfoot"):
                collect_rows(child, out)

    for table in find_tables(tree):
        rows: list[Any] = []
        collect_rows(table, rows)
        if len(rows) < 2:
            continue
        cell_counts: list[int] = []
        for row in rows:
            count = 0
            for cell in row:
                if _strip_ns(cell.tag) in ("td", "th"):
                    try:
                        count += max(1, int(cell.attrib.get("colspan", "1")))
                    except ValueError:
                        count += 1
            cell_counts.append(count)
        first = cell_counts[0]
        if first <= 1:
            continue
        mismatched = [c for c in cell_counts[1:] if c != first]
        if mismatched:
            yield Finding(
                rule="EH006",
                message=(
                    f"Table has rows with inconsistent column counts "
                    f"(first row has {first} columns, but later rows "
                    f"have {', '.join(str(c) for c in sorted(set(mismatched)))})"
                ),
                hint="Possible nesting error — check colspan and closing tags.",
            )


# ── Empty-table / missing-content warnings (EH007, EH008, EH009, EH010) ──────


def _check_tag_warnings(tree: Any) -> Iterator[Finding]:
    """EH007/EH008/EH009/EH010 — bulk warnings derived from the
    constructed tree after parsing.

    These are advisory: a table without `<tr>` rows or an email with no
    visible content tags is almost certainly broken, but we don't block
    on them — the agent might be sending a 1x1 tracking pixel or a
    text-only `<pre>` block.
    """
    tag_counts: Counter[str] = Counter()
    max_depth = 0

    def visit(elem: Any, depth: int) -> None:
        nonlocal max_depth
        max_depth = max(max_depth, depth)
        for child in elem:
            tag_counts[_strip_ns(child.tag)] += 1
            visit(child, depth + 1)

    visit(tree, 0)

    visible = {"div", "p", "span", "table", "h1", "h2", "h3", "h4", "h5", "h6", "td", "th", "li"}
    if not any(tag_counts[t] > 0 for t in visible):
        yield Finding(
            rule="EH009",
            message="No common content tags found (div, p, table, etc.) - email may appear empty",
        )

    if tag_counts["table"] > 0:
        if tag_counts["tr"] == 0:
            yield Finding(rule="EH007", message="Table found without any <tr> rows")
        if tag_counts["td"] == 0 and tag_counts["th"] == 0:
            yield Finding(rule="EH008", message="Table found without any <td> or <th> cells")

    if max_depth > 50:
        yield Finding(
            rule="EH010",
            message=f"Very deep nesting detected ({max_depth} levels) - may cause rendering issues",
        )


# ── Helper functions used by multiple checks ────────────────────────────────


def detect_html_content(content: str) -> bool:
    """Return True if `content` looks like HTML.

    Used by callers to auto-correct a `content_type="text/plain"` arg
    when the body contains HTML tags.
    """
    if not content or not isinstance(content, str):
        return False
    if content.strip().lower().startswith("<!doctype"):
        return True
    if _HTML_TAG_PATTERN.search(content):
        return True
    return bool(_HTML_ENTITY_PATTERN.search(content) and "<" in content and ">" in content)


def resolve_content_type(content: str, explicit_type: str | None) -> tuple[str, bool]:
    """Return `(resolved_content_type, was_auto_corrected)`.

    Auto-corrects `text/plain` → `text/html` when HTML is detected, so a
    caller that forgot to set the type can't accidentally send raw HTML
    source as plain text. The reverse (plain text declared as HTML) is
    allowed because HTML can legitimately contain only text.
    """
    is_html = detect_html_content(content)
    if not explicit_type:
        return ("text/html" if is_html else DEFAULT_CONTENT_TYPE, False)
    explicit_is_html = explicit_type.lower() == "text/html"
    if is_html and not explicit_is_html:
        return ("text/html", True)
    return (explicit_type, False)


def extract_cid_references(html_content: str) -> set[str]:
    """Return the set of cid: identifiers referenced in `<img src="cid:…">`.

    Callers cross-check this against the `inline_attachments` they were
    given; mismatches surface as EI005 / EI006.
    """
    return {m.group(1).strip() for m in _CID_PATTERN.finditer(html_content) if m.group(1).strip()}


def find_unresolved_template_tokens(content: str) -> list[str]:
    """Return sorted list of `${…}`, `{{…}}`, `{name[:fmt]}` survivors.

    PR #195 history: the single-brace pattern used to also match inside
    `{{…}}` / `${…}` and surface a misleading single-brace fragment, so
    agents stripped valid `{{handlebars}}` thinking they were the
    problem. The patterns above are negative-lookbehind / negative-
    lookahead anchored to avoid that.
    """
    matches: set[str] = set()
    for pattern in _UNRESOLVED_TEMPLATE_PATTERNS:
        for match in pattern.finditer(content):
            matches.add(match.group(0))
    return sorted(matches)


# ── Subject checks ──────────────────────────────────────────────────────────


def _check_subject(subject: str) -> Iterator[Finding]:
    if not subject or not subject.strip():
        yield Finding(rule="EL003", message="Email subject is empty.")
        return
    tokens = find_unresolved_template_tokens(subject)
    if tokens:
        sample, suffix = _sample_and_suffix(tokens)
        yield Finding(
            rule="ET002",
            message=f"Unresolved template tokens in subject: {sample}{suffix}",
            hint="Resolve every placeholder before sending; tokens like ${tool:...} or {{name}} are programmatic, not literal text.",
        )


# ── Body checks ─────────────────────────────────────────────────────────────


def _check_body_placeholders(content: str) -> Iterator[Finding]:
    """EL001 — body is literally `html_content` / `BODY` / etc.

    Catches agents that called send_email with the placeholder string
    they were shown in the tool docs instead of real HTML.
    """
    if content.strip() in _PLACEHOLDER_VALUES:
        yield Finding(
            rule="EL001",
            message=f"Email content appears to be a placeholder: '{content.strip()}'",
            hint="Pass the rendered HTML body, not the variable name from the docs.",
        )


def _check_body_length(content: str) -> Iterator[Finding]:
    if not content or len(content.strip()) < 5:
        yield Finding(rule="EL002", message="Email content is too short or empty.")


def _check_body_tokens(content: str) -> Iterator[Finding]:
    tokens = find_unresolved_template_tokens(content)
    if tokens:
        sample, suffix = _sample_and_suffix(tokens)
        yield Finding(
            rule="ET001",
            message=f"Unresolved template tokens found: {sample}{suffix}",
            hint="Resolve every placeholder before sending; recipients should never see raw `{{…}}` or `${…}`.",
        )


def _check_body_html_marker(content: str, content_type: str | None) -> Iterator[Finding]:
    """EH013 — caller said it's HTML but no HTML tags are present.

    Usually means a plaintext blob was passed to a code path that demands
    HTML. The caller (sendgrid/mailbux) is expected to have already run
    `resolve_content_type` first, so this only fires on explicit lies.
    """
    if (
        content_type == "text/html"
        and not _HTML_TAG_PATTERN.search(content)
        and not content.strip().lower().startswith("<!doctype")
    ):
        yield Finding(
            rule="EH013",
            message="Content type is 'text/html' but no HTML tags were found.",
            hint="Either wrap the prose in HTML tags or pass content_type='text/plain'.",
        )


# ── Image checks ────────────────────────────────────────────────────────────


def _check_images(content: str) -> Iterator[Finding]:
    """Image src must be cid: (with matching inline_attachments) or http(s)://.

    Blocks fabricated `data:` URIs and local `file://` paths, which
    render as broken icons in every real client. Caller is responsible
    for the cid<->inline_attachments cross-check (EI005 / EI006).
    """
    for match in _IMG_SRC_PATTERN.finditer(content):
        src = match.group(1)
        src_stripped = src.strip()
        src_lower = src_stripped.lower()
        if not src_stripped:
            yield Finding(rule="EI001", message="Image tag found with empty src attribute")
        elif src_lower.startswith("file://"):
            yield Finding(
                rule="EI002",
                message=f"Image uses local file path (won't work in email): {src_stripped[:50]}...",
                hint="Local paths are not reachable from recipients' clients. Use cid: or https://.",
            )
        elif src_lower.startswith("data:"):
            yield Finding(
                rule="EI003",
                message=("Image uses a data: URI — fabricated inline images are not allowed."),
                hint=("Reference a real asset via cid: (with a matching inline_attachments entry) or an https:// URL."),
            )
        elif not src_lower.startswith(("cid:", "http://", "https://")):
            yield Finding(
                rule="EI004",
                message=f"Image src must be cid: or http(s):// — got: {src_stripped[:50]}",
                hint="Replace with cid:<inline-id> or an https:// URL.",
            )


# ── CSS checks ──────────────────────────────────────────────────────────────


def _check_css(content: str) -> Iterator[Finding]:
    """Block / warn on CSS properties known to misbehave in email clients.

    Kept hand-rolled rather than data-driven (caniemail JSON) because the
    list is short and stable. Add to this function when a new client
    quirk surfaces — one helper, one rule code, one test.
    """
    lower = content.lower()
    if "rgba(" in lower or "rgba (" in lower:
        yield Finding(
            rule="EC001",
            message=(
                "CSS 'rgba()' colors found — alpha channels are not supported in most "
                "email clients (Outlook strips the alpha and clients render the "
                "underlying color as fully opaque)."
            ),
            hint="Use solid hex (#RRGGBB) or rgb() instead.",
        )
    if "position:fixed" in lower or "position: fixed" in lower:
        yield Finding(
            rule="EC002",
            message="CSS 'position:fixed' found — not supported in most email clients.",
        )
    if "position:absolute" in lower or "position: absolute" in lower:
        yield Finding(
            rule="EC003",
            message="CSS 'position:absolute' found — limited support in email clients.",
        )


# ── Template leakage ─────────────────────────────────────────────────────────


def _check_template_leakage(content: str) -> Iterator[Finding]:
    """Detect canonical_template demo data the agent failed to replace.

    See `_DATA_PREVIEW_ATTR_PATTERN`, `_TIER2_DEMO_PROSE_MARKERS`,
    `_TIER3_DEMO_MARKERS` for the three tiers and their thresholds.
    """
    preview_tokens = sorted({m.group(1) for m in _DATA_PREVIEW_ATTR_PATTERN.finditer(content)})
    if preview_tokens:
        sample, suffix = _sample_and_suffix(preview_tokens)
        yield Finding(
            rule="EL101",
            message=(
                "Template example data detected — `data-preview` attributes from "
                f"canonical_template survived: {sample}{suffix}."
            ),
            hint="Rebuild the email body without copying demo sections.",
        )

    tier2_hits = [m for m in _TIER2_DEMO_PROSE_MARKERS if m in content]
    if tier2_hits:
        sample = "; ".join(f'"{hit[:60]}…"' if len(hit) > 60 else f'"{hit}"' for hit in tier2_hits[:3])
        yield Finding(
            rule="EL102",
            message=(f"Template example data detected — canonical_template demo prose is still in the body: {sample}."),
            hint="Remove the Executive Summary, Risk, Opportunity, and Closing example sections.",
        )

    tier3_hits = [m for m in _TIER3_DEMO_MARKERS if m in content]
    if len(tier3_hits) >= _TIER3_BLOCK_THRESHOLD:
        sample, suffix = _sample_and_suffix(tier3_hits)
        yield Finding(
            rule="EL103",
            message=(
                f"Template example data detected — {len(tier3_hits)} demo markers "
                f"co-occur in the body ({sample}{suffix})."
            ),
            hint="Real data would not collocate these values — regenerate from real reporting.",
        )
    elif tier3_hits:
        sample, _suffix = _sample_and_suffix(tier3_hits, head=5)
        yield Finding(
            rule="EL104",
            message=f"Possible template example data: {sample}.",
            hint="Verify these are real campaign values — if copied from canonical_template, replace them.",
        )


def _sample_and_suffix(items: Iterable[str], head: int = 5) -> tuple[str, str]:
    """Render '..., …, …' + ' (+N more)' suffix for a list of finding samples."""
    seq = list(items)
    sample = ", ".join(seq[:head])
    remainder = len(seq) - min(len(seq), head)
    suffix = f" (+{remainder} more)" if remainder > 0 else ""
    return sample, suffix


# ── HTML aggregate checks ────────────────────────────────────────────────────


def _check_html_structure(content: str) -> Iterator[Finding]:
    """Run all parser-based structural checks.

    Order:

    1. EH001 foster-parent (from html5lib parser.errors)
    2. EH002 loose text (from html5lib tokenizer-phase hook)
    3. EH003 unclosed `<table>` (from html5lib's `eof-in-table`)
    4. EH004 extra/mismatched close tag — ONLY when EH001/EH003 didn't
       fire (they cascade spurious end-tag errors as a consequence; the
       agent fixes the root cause and any genuine EH004 surfaces on the
       next pass)
    5. EP001 pad-nesting (from etree walk — domain heuristic)
    6. EH006 column consistency (from etree walk)
    7. EH007/8/9/10 tag-count warnings (from etree walk)

    Cascade-suppression for EH004 is the only inter-check dependency;
    everything else is independent and order-only-affects-output-order.
    """
    if not content or not content.strip():
        yield Finding(rule="EH012", message="HTML content is empty")
        return

    parser = _TrackingHTMLParser()
    try:
        tree = parser.parse(content)
    except Exception as exc:  # noqa: BLE001 — surface as a finding, not a crash
        yield Finding(rule="EH011", message=f"HTML parsing error: {exc}")
        return

    dedup: set[tuple[str, str]] = set()
    # EH001 / EH002 / EH003 first so we know whether EH004 should be
    # suppressed as cascade noise. Buffer them so we can keep the
    # documented output order if everything fires.
    structural = list(_check_foster_parents(parser, dedup))
    structural.extend(_check_loose_text(parser, dedup))
    unclosed = list(_check_unclosed(parser))
    root_cause_fired = bool(structural) or bool(unclosed)

    yield from structural
    yield from unclosed
    yield from _check_bad_close(parser, root_cause_fired, dedup)
    yield from _check_pad_nesting(tree, dedup)
    yield from _check_column_consistency(tree)
    yield from _check_tag_warnings(tree)


# ── Public API ───────────────────────────────────────────────────────────────


def validate(
    content: str,
    *,
    subject: str | None = None,
    content_type: str = "text/html",
) -> list[Finding]:
    """Run every check applicable to `content` (+ `subject` if given).

    Order: subject → body length/placeholders/tokens/html-marker → CSS
    → images → HTML structure (parser walk) → template leakage. Order
    only affects the order of returned findings; severity decisions are
    made per-finding via `RULES`.

    `content_type` defaults to `text/html` so callers that want the full
    HTML pass don't need to pass it. Pass `text/plain` to skip the
    HTML-structure checks for plaintext bodies (still runs token,
    placeholder, leakage checks).
    """
    findings: list[Finding] = []

    if subject is not None:
        findings.extend(_check_subject(subject))

    findings.extend(_check_body_placeholders(content))
    findings.extend(_check_body_length(content))
    findings.extend(_check_body_tokens(content))
    findings.extend(_check_body_html_marker(content, content_type))

    if content_type == "text/html":
        findings.extend(_check_css(content))
        findings.extend(_check_images(content))
        findings.extend(_check_html_structure(content))
        findings.extend(_check_template_leakage(content))

    return findings


def partition_findings(findings: list[Finding]) -> tuple[list[Finding], list[Finding]]:
    """Split a finding list into `(errors, warnings)` preserving order."""
    errors = [f for f in findings if f.severity == "error"]
    warnings = [f for f in findings if f.severity == "warning"]
    return errors, warnings


def format_findings(findings: list[Finding]) -> str:
    """Render findings as a bulleted multi-line block for tool responses."""
    if not findings:
        return ""
    return "\n  - ".join(f.format() for f in findings)


# ── Backward-compat shims for the existing callers ──────────────────────────
#
# Old API was `_validate_html(content) -> (errors_list[str], warnings_list[str])`.
# These let sendgrid_server.py / mailbux.py keep their current control flow
# while switching to the structured backend. New code should call
# `validate()` and consume `Finding`s directly.


def validate_html_legacy(content: str) -> tuple[list[str], list[str]]:
    """Old `_validate_html` shape: html-only, returns (errors, warnings) of strings."""
    findings: list[Finding] = []
    if content and content.strip():
        findings.extend(_check_css(content))
        findings.extend(_check_images(content))
        findings.extend(_check_html_structure(content))
    else:
        findings.append(Finding(rule="EH012", message="HTML content is empty"))
    errors, warnings = partition_findings(findings)
    return [f.format() for f in errors], [f.format() for f in warnings]


def validate_email_subject_legacy(subject: str) -> str | None:
    """Old `_validate_email_subject` shape: returns first error string, else None."""
    for finding in _check_subject(subject):
        if finding.severity == "error":
            return finding.format()
    return None


def validate_email_body_legacy(content: str, content_type: str | None) -> str | None:
    """Old `_validate_email_body` shape: returns first error string, else None."""
    for finding in _check_body_placeholders(content):
        if finding.severity == "error":
            return finding.format()
    for finding in _check_body_length(content):
        if finding.severity == "error":
            return finding.format()
    for finding in _check_body_tokens(content):
        if finding.severity == "error":
            return finding.format()
    for finding in _check_body_html_marker(content, content_type):
        if finding.severity == "error":
            return finding.format()
    return None


def check_template_leakage_legacy(content: str) -> tuple[list[str], list[str]]:
    """Old `_check_template_leakage` shape: (errors_list, warnings_list) of strings."""
    findings = list(_check_template_leakage(content))
    errors, warnings = partition_findings(findings)
    return [f.format() for f in errors], [f.format() for f in warnings]
