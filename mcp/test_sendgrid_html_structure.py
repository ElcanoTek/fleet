"""Structural HTML-validator regression tests.

Lives next to `email_lint.py` so the structured validator can be
exercised standalone (no sendgrid / fastmcp / aiofiles import needed).
Assertions check rule codes, not substring matches on the human
message, so future copy edits don't break the suite.

Coverage origin:

1. **EH001 / EH002** — foster-parented elements and loose text as
   direct children of `<table>`/`<tbody>`. The OMUS Comfluence
   regression where Kyle's appended `<div>` jumped above the dark
   Victoria header (chat#201).

2. **EP001** — `*-pad` `<td>` nested inside another `*-pad` `<td>`.
   The TWC Campaign Health Scan regression (cutlass 33e4468,
   2026-05-21): unclosed inner badge `<table>` squeezed every
   subsequent body row into a 2-column cell.

3. **EC001** — `rgba()` CSS. Outlook strips the alpha, so a
   `rgba(0,0,0,0.6)` overlay becomes solid black.
"""

from __future__ import annotations

import pathlib

import pytest
from email_lint import RULES, Finding, partition_findings, validate

_FIXTURE_DIR = pathlib.Path(__file__).parent / "testdata"

SHELL_OPEN = '<html><body><table class="email-container">'
SHELL_CLOSE = "</table></body></html>"
HEADER_ROW = "<tr><td>Header</td></tr>"
FOOTER_ROW = "<tr><td>Footer</td></tr>"


def _findings(inner: str) -> list[Finding]:
    return validate(SHELL_OPEN + inner + SHELL_CLOSE, content_type="text/html")


def _codes(findings: list[Finding]) -> list[str]:
    return [f.rule for f in findings]


# ── Rule catalog sanity ─────────────────────────────────────────────────


def test_every_rule_has_a_severity() -> None:
    for code, (severity, _title) in RULES.items():
        assert severity in ("error", "warning"), f"{code} has bad severity {severity!r}"


def test_finding_severity_matches_catalog() -> None:
    # Constructing a Finding without an explicit severity must read from RULES.
    f = Finding(rule="EH001", message="x")
    assert f.severity == "error"
    f2 = Finding(rule="EC002", message="y")
    assert f2.severity == "warning"


def test_unknown_rule_code_rejected() -> None:
    with pytest.raises(ValueError):
        Finding(rule="EX999", message="nope")


# ── Foster-parent checks (EH001 / EH002) ────────────────────────────────


def test_div_direct_child_of_table_is_EH001() -> None:
    # Source-written: `<table><div>...</div></table>`. html5lib's spec-correct
    # parser inserts an implicit `<tbody>` between `<table>` and the
    # contents, then foster-parents the `<div>` out of that `<tbody>`. The
    # reported parent context is `<tbody>` (the actual immediate parent in
    # the constructed tree), which is more accurate than calling it
    # "child of <table>" — the agent's mental model "I wrote it as a
    # table child" still holds.
    findings = _findings(HEADER_ROW + '<div style="padding:20px;"><p>Hey Vlad, …</p></div>' + FOOTER_ROW)
    eh001 = [f for f in findings if f.rule == "EH001"]
    assert len(eh001) == 1, f"expected one EH001, got: {_codes(findings)}"
    assert "<div>" in eh001[0].message
    assert "direct child of <tbody>" in eh001[0].message or "direct child of <table>" in eh001[0].message


def test_heading_direct_child_of_table_is_EH001() -> None:
    findings = _findings(HEADER_ROW + "<h2>Pricing Inefficiency</h2>" + FOOTER_ROW)
    assert "EH001" in _codes(findings)


def test_raw_inner_table_direct_child_of_table_is_EH001() -> None:
    # Bare <table> inside <table> with no <td> wrapper — what Victoria did
    # with the deal-breakdown table on the OMUS thread.
    findings = _findings(HEADER_ROW + "<table><tr><td>Deal</td></tr></table>" + FOOTER_ROW)
    assert any(f.rule == "EH001" and "<table>" in f.message for f in findings)


def test_p_direct_child_of_tbody_is_EH001() -> None:
    findings = _findings(
        "<tr><td><table><tbody>"
        "<tr><td>row 1</td></tr>"
        "<p>orphan paragraph</p>"
        "<tr><td>row 2</td></tr>"
        "</tbody></table></td></tr>"
    )
    assert any(f.rule == "EH001" and "<p>" in f.message and "<tbody>" in f.message for f in findings)


def test_div_direct_child_of_tr_is_EH001() -> None:
    findings = _findings("<tr><div>orphan</div><td>cell</td></tr>")
    assert any(f.rule == "EH001" and "<div>" in f.message and "direct child of <tr>" in f.message for f in findings)


def test_loose_text_between_rows_is_EH002() -> None:
    findings = _findings(
        HEADER_ROW + "Hey Vlad and Mark, got your email request and wanted to share something." + FOOTER_ROW
    )
    eh002 = [f for f in findings if f.rule == "EH002"]
    assert len(eh002) == 1, f"expected one EH002, got: {_codes(findings)}"


def test_single_violation_reported_once_even_with_many_children() -> None:
    # A misplaced <div> wrapping 20 child elements shouldn't produce 20
    # errors — that floods the agent and obscures the fix.
    children = "".join(f"<p>line {i}</p>" for i in range(20))
    findings = _findings(HEADER_ROW + f"<div>{children}</div>" + FOOTER_ROW)
    eh001 = [f for f in findings if f.rule == "EH001"]
    assert len(eh001) == 1, f"expected one EH001, got {len(eh001)}: {[f.message for f in eh001]}"


def test_valid_canonical_structure_passes_cleanly() -> None:
    findings = _findings(
        "<tr><td>header</td></tr>"
        + "<tr><td>status_bar</td></tr>"
        + "<tr><td><table><tbody><tr><td>data row</td></tr></tbody></table></td></tr>"
        + "<tr><td>footer</td></tr>"
    )
    structural = [f for f in findings if f.rule in ("EH001", "EH002", "EH011")]
    assert structural == [], f"valid composition unexpectedly flagged: {structural}"


def test_prose_inside_td_is_fine() -> None:
    # The right way to add free-form prose: wrap in <tr><td>...</td></tr>.
    findings = _findings(
        "<tr><td>header</td></tr>" + '<tr><td style="padding:20px;">'
        "<p>Hey Vlad and Mark, here is the analysis.</p>"
        "<h3>Root Cause</h3>"
        "<div>Pricing inflation drove the scale gap.</div>"
        "</td></tr>" + "<tr><td>footer</td></tr>"
    )
    foster = [f for f in findings if f.rule in ("EH001", "EH002")]
    assert foster == [], f"prose-in-<td> unexpectedly flagged: {foster}"


# ── EP001 — nested *-pad cells ──────────────────────────────────────────


def test_body_pad_nested_inside_status_pad_is_EP001() -> None:
    """TWC Campaign Health Scan regression (2026-05-21).

    Victoria emitted <td class="status-pad"> with an inner badge <table>,
    closed the inner <tr>, but forgot </table></td></tr>. The next
    <tr><td class="body-pad">…</td></tr> body section ended up nested
    inside the badge table, squeezing the rest of the email through a
    2-column badge cell.
    """
    findings = _findings(
        '<tr><td class="status-pad"><table>'
        "<tr><td>HEALTH SCAN</td><td>Status: ok</td></tr>"
        # MISSING </table></td></tr> here
        '<tr><td class="body-pad">'
        "<table><tr><td>WoW Analysis</td></tr></table>"
        "</td></tr>"
    )
    ep001 = [f for f in findings if f.rule == "EP001"]
    assert len(ep001) == 1, f"expected one EP001, got: {_codes(findings)}"
    assert "body-pad" in ep001[0].message
    assert "status-pad" in ep001[0].message


def test_pad_cells_at_top_level_pass() -> None:
    findings = _findings(
        '<tr><td class="header-pad">header</td></tr>'
        '<tr><td class="status-pad">'
        "<table><tr><td>BADGE</td><td>Status: ok</td></tr></table>"
        "</td></tr>"
        '<tr><td class="body-pad">body</td></tr>'
        '<tr><td class="table-pad">table-wrapper</td></tr>'
        '<tr><td class="footer-pad">footer</td></tr>'
    )
    assert "EP001" not in _codes(findings), f"unexpectedly flagged: {_codes(findings)}"


def test_multiple_body_pads_nested_in_status_pad_reported_once() -> None:
    findings = _findings(
        '<tr><td class="status-pad"><table>'
        "<tr><td>BADGE</td></tr>"
        '<tr><td class="body-pad">section 1</td></tr>'
        '<tr><td class="body-pad">section 2</td></tr>'
        '<tr><td class="body-pad">section 3</td></tr>'
        "</table></td></tr>"
    )
    ep001 = [f for f in findings if f.rule == "EP001"]
    assert len(ep001) == 1, f"expected one EP001, got {len(ep001)}"


def test_unclassed_td_inside_pad_is_fine() -> None:
    findings = _findings(
        '<tr><td class="body-pad">'
        "<table>"
        "<tr><td>Metric</td><td>Value</td></tr>"
        "<tr><td>Spend</td><td>$130,991</td></tr>"
        "</table>"
        "</td></tr>"
    )
    assert "EP001" not in _codes(findings)


def test_body_pad_nested_inside_body_pad_is_EP001() -> None:
    findings = _findings('<tr><td class="body-pad"><table><tr><td class="body-pad">inner</td></tr></table></td></tr>')
    ep001 = [f for f in findings if f.rule == "EP001"]
    assert len(ep001) == 1


# ── EC001 — rgba() ─────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "style",
    [
        "background:rgba(0,0,0,0.6);",
        "color:rgba (255,255,255,0.5);",  # whitespace variant
        "color:RGBA(0,0,0,0.5);",  # uppercase
    ],
)
def test_rgba_in_style_attribute_is_EC001(style: str) -> None:
    findings = _findings(f'<tr><td style="{style}">overlay</td></tr>')
    assert "EC001" in _codes(findings), f"expected EC001, got: {_codes(findings)}"


@pytest.mark.parametrize(
    "style",
    [
        "color:rgb(255,0,0);",
        "color:#FF0000;",
    ],
)
def test_non_rgba_colors_pass(style: str) -> None:
    findings = _findings(f'<tr><td style="{style}">solid</td></tr>')
    assert "EC001" not in _codes(findings)


# ── partition_findings + severity ──────────────────────────────────────


def test_partition_separates_errors_and_warnings() -> None:
    findings = _findings(HEADER_ROW + '<div style="position:fixed;color:rgba(0,0,0,0.5);">x</div>' + FOOTER_ROW)
    errors, warnings = partition_findings(findings)
    assert any(f.rule == "EH001" for f in errors)  # foster-parent
    assert any(f.rule == "EC001" for f in errors)  # rgba
    assert any(f.rule == "EC002" for f in warnings)  # position:fixed


# ── html5lib spec-correctness regressions ───────────────────────────────
#
# These cases all rely on the html5lib backend being a spec-correct HTML5
# parser. The stdlib `html.parser` backend (used pre-2026-05) would either
# silently accept the input or report a different parent context.


def test_implicit_tbody_inserted_for_eh001_parent_context() -> None:
    # HTML5 spec: when `<table>` has a `<tr>` child without an explicit
    # `<tbody>` wrapper, the parser inserts an implicit `<tbody>`. So a
    # foster-parented `<div>` between rows is reported as a child of that
    # implicit `<tbody>`, not the `<table>`. The stdlib walker can't
    # model this; html5lib does it for free.
    findings = _findings(HEADER_ROW + "<div>oops</div>" + FOOTER_ROW)
    eh001 = [f for f in findings if f.rule == "EH001"]
    assert len(eh001) == 1
    assert "direct child of <tbody>" in eh001[0].message


def test_eof_in_table_is_EH003() -> None:
    # `<table>` left open at end of input. html5lib emits `eof-in-table`;
    # we map it to EH003 with a concrete fix hint pointing at `</table>`.
    findings = list(validate("<table><tr><td>x</td></tr>", content_type="text/html"))
    assert "EH003" in _codes(findings)
    eh003 = next(f for f in findings if f.rule == "EH003")
    assert "</table>" in eh003.hint


def test_loose_text_before_first_row_is_EH002() -> None:
    # The other EH002 source pattern: text directly inside `<table>` BEFORE
    # any `<tr>`. Also foster-parented by html5lib; the stdlib walker
    # caught this via handle_data; we catch it via source regex.
    findings = _findings("<tr><td>row</td></tr>")
    # Insert before the first row: <table>{text}<tr>...</tr></table>
    findings = list(
        validate(
            '<html><body><table class="email-container">Preamble<tr><td>row</td></tr></table></body></html>',
            content_type="text/html",
        )
    )
    assert "EH002" in _codes(findings)


def test_character_references_dont_break_validator() -> None:
    # Stdlib's HTMLParser can choke on certain entity-reference-adjacent
    # tag boundaries. html5lib handles the full HTML5 entity reference
    # algorithm correctly. Verify clean HTML with entities passes.
    findings = _findings("<tr><td>Price&nbsp;&pound;9.29 &amp; tax</td></tr><tr><td>Q&amp;A</td></tr>")
    structural = [f for f in findings if f.rule in ("EH001", "EH002", "EH011")]
    assert structural == [], f"clean HTML with entities flagged: {structural}"


def test_attribute_values_with_angle_brackets_dont_confuse_parser() -> None:
    # `<a href="?x=1&y=2">` etc. — html5lib parses attributes per spec,
    # so unescaped `&` and other tricky chars in attribute values don't
    # produce spurious foster-parent findings.
    findings = _findings('<tr><td><a href="https://example.com/?a=1&b=2">link</a></td></tr>')
    structural = [f for f in findings if f.rule in ("EH001", "EH002", "EH011")]
    assert structural == [], f"attribute with & flagged: {structural}"


def test_eh002_loose_text_with_no_following_row_tag_is_detected() -> None:
    # Old regex-based EH002 required a following `<tr|tbody|...>` tag to
    # match. Tokenizer-based detection catches text that's foster-parented
    # regardless of what follows it. `<table><tr><td>x</td></tr>orphan</table>`
    # — the "orphan" text is still foster-parented even though no row tag
    # comes after it.
    findings = list(
        validate(
            '<html><body><table class="email-container"><tr><td>x</td></tr>orphan</table></body></html>',
            content_type="text/html",
        )
    )
    assert "EH002" in _codes(findings), f"text-before-table-close missed: {_codes(findings)}"


def test_eh002_has_line_number() -> None:
    # All other findings include a `line` field for source navigation.
    # EH002 must too, sourced from the parser tokenizer's position
    # (was missing on the old regex-based detection).
    findings = list(
        validate(
            '<html><body><table class="email-container">'
            "<tr><td>x</td></tr>foster-me-please<tr><td>y</td></tr>"
            "</table></body></html>",
            content_type="text/html",
        )
    )
    eh002 = next(f for f in findings if f.rule == "EH002")
    assert eh002.line is not None and eh002.line >= 1, f"EH002 has no line: {eh002}"


def test_eh002_not_triggered_by_text_in_td() -> None:
    # Sanity: text inside `<td>` is NOT foster-parented (parser is in
    # `inCell` phase, not `inTable`/`inTableBody`/`inRow`). Must NOT fire.
    findings = list(
        validate(
            '<html><body><table class="email-container">'
            "<tr><td>Real text inside a cell — totally valid.</td></tr>"
            "</table></body></html>",
            content_type="text/html",
        )
    )
    assert "EH002" not in _codes(findings), f"text in <td> false-positived: {_codes(findings)}"


# ── EH004 (smart cascade suppression) ───────────────────────────────────


def test_eh004_extra_close_div_fires() -> None:
    # Real bug: agent wrote one </div> too many. No foster-parent or
    # unclosed-table to mask it, so EH004 must surface.
    findings = list(
        validate(
            "<html><body><div>hi</div></div></body></html>",
            content_type="text/html",
        )
    )
    assert "EH004" in _codes(findings), f"extra </div> not caught: {_codes(findings)}"


def test_eh004_extra_close_span_fires() -> None:
    # Wrong tag name in the close — agent typed `</span>` where they
    # meant `</div>`. The lone `<div>` remains open; `</span>` has no opener.
    findings = list(
        validate(
            "<html><body><div>hi</span></body></html>",
            content_type="text/html",
        )
    )
    assert "EH004" in _codes(findings), f"mismatched close not caught: {_codes(findings)}"


def test_eh004_suppressed_during_foster_parent_cascade() -> None:
    # When EH001 fires, html5lib cascades spurious EH004-style errors
    # from its implicit-close machinery. Those are NOT real bugs — they're
    # downstream effects. Suppress them so the agent isn't flooded.
    findings = list(
        validate(
            '<html><body><table class="email-container">'
            "<tr><td>x</td></tr>"
            "<table><tr><td>nested no wrapper</td></tr></table>"
            "<tr><td>y</td></tr></table></body></html>",
            content_type="text/html",
        )
    )
    # EH001 fires for the foster-parent; EH004 must NOT
    assert "EH001" in _codes(findings)
    assert "EH004" not in _codes(findings), f"EH004 cascade not suppressed: {_codes(findings)}"


def test_eh004_suppressed_during_unclosed_table() -> None:
    # Same idea for EH003 — unclosed <table> cascades end-tag noise.
    findings = list(validate("<table><tr><td>x</td></tr>", content_type="text/html"))
    assert "EH003" in _codes(findings)
    assert "EH004" not in _codes(findings), f"EH004 cascade not suppressed: {_codes(findings)}"


def test_eh004_dedup_by_tag() -> None:
    # `<div>x</div></div></div></div>` should produce ONE EH004 for
    # </div>, not four. The agent's fix is the same regardless of count.
    findings = list(
        validate(
            "<html><body><div>x</div></div></div></div></body></html>",
            content_type="text/html",
        )
    )
    eh004 = [f for f in findings if f.rule == "EH004"]
    assert len(eh004) == 1, f"expected one EH004 (deduped), got {len(eh004)}: {[f.message for f in eh004]}"


# ── Real-world regression fixtures ──────────────────────────────────────
# Files under `mcp/testdata/` are high-fidelity reproductions of actual
# bugs that hit production. If a future refactor breaks detection on
# any of these, that's a regression — the fix-hint at the top of each
# fixture explains the original failure mode.


def _load_fixture(name: str) -> str:
    return (_FIXTURE_DIR / name).read_text(encoding="utf-8")


def test_twc_health_scan_real_fixture_fires_EP001() -> None:
    # The exact failure that prompted the EP001 check (commit 33e4468,
    # 2026-05-21). Status-pad with a forgotten `</table></td></tr>`
    # swallowed every subsequent body row. EP001 must catch it.
    findings = validate(_load_fixture("twc_health_scan_broken.html"), content_type="text/html")
    ep001 = [f for f in findings if f.rule == "EP001"]
    assert len(ep001) >= 1, f"TWC regression must fire EP001 — broken email codes: {_codes(findings)}"


def test_omus_comfluence_real_fixture_fires_EH001() -> None:
    # The OMUS / Kyle regression (chat#201): bare `<div>` between
    # component `<tr>`s rendered above the dark header because HTML5
    # foster-parented it out of the table. EH001 must catch it.
    findings = validate(_load_fixture("omus_comfluence_div_orphan.html"), content_type="text/html")
    eh001 = [f for f in findings if f.rule == "EH001"]
    assert len(eh001) >= 1, f"OMUS regression must fire EH001 — broken email codes: {_codes(findings)}"
    # And the message should mention <div> specifically (root cause)
    assert any("<div>" in f.message for f in eh001), [f.message for f in eh001]
