#!/usr/bin/env python3
"""Regression tests for the shared email validator.

Exercises `email_lint` directly (the structured `Finding` API) and the
legacy string-shape shims re-exported by `sendgrid_server` and
`mailbux` so PR #195's token-finder behavior keeps holding.

Assertions check rule codes (EH001, EC001, …), not substrings of the
human message — that way copy edits to the agent-facing text don't
break the suite. Failure history that motivated each rule lives in the
docstrings of `email_lint._check_*` and in the module-level rule
catalog (`email_lint.RULES`).

Run directly (`python3 server/mcp/test_template_validation.py`) or via
`python3 -m unittest server.mcp.test_template_validation`. CI runs the
same suite via `make test-py` (which calls `unittest discover`).
"""

from __future__ import annotations

import importlib.util
import sys
import types
import unittest
from pathlib import Path

_MCP_DIR = Path(__file__).resolve().parent


def _stub_module(name: str, **attrs) -> types.ModuleType:
    """Install a fake top-level module so the MCP files can import their
    heavy deps (sendgrid, fastmcp, aiofiles, httpx) without those wheels
    being present in CI.

    Only stubs when the real wheel is unavailable: in the consolidated
    fleet suite the real wheels ARE installed and are shared with sibling
    tests (e.g. ``aioboto3``-backed ses_s3_email needs the real
    ``httpx``), so clobbering ``sys.modules`` with an empty fake would
    poison the shared session. The fake path only triggers in the
    wheel-less CI this test was originally written for."""
    try:
        return importlib.import_module(name)
    except Exception:
        pass
    mod = types.ModuleType(name)
    for key, value in attrs.items():
        setattr(mod, key, value)
    sys.modules[name] = mod
    return mod


def _stub_callable(*_args, **_kwargs):
    return None


class _FakeMCP:
    def __init__(self, *_a, **_kw):
        pass

    def tool(self, *_a, **_kw):
        def decorator(fn):
            return fn

        return decorator


_stub_module("mcp")
_stub_module("mcp.server")
_stub_module("mcp.server.fastmcp", FastMCP=_FakeMCP)
_stub_module("sendgrid", SendGridAPIClient=_stub_callable)
_stub_module("sendgrid.helpers")
_stub_module(
    "sendgrid.helpers.mail",
    **dict.fromkeys(("Attachment", "Content", "ContentId", "Disposition", "Email", "FileContent", "FileName", "FileType", "Mail", "Personalization"), _stub_callable),
)
_stub_module("aiofiles")
_stub_module("httpx")


def _load(name: str):
    spec = importlib.util.spec_from_file_location(name, _MCP_DIR / f"{name}.py")
    assert spec and spec.loader, f"could not locate {name}.py next to this test"
    module = importlib.util.module_from_spec(spec)
    # Register under `name` only for the duration of exec_module so the
    # module's own self-references resolve, then restore the prior entry.
    # In the consolidated fleet suite sibling pytest tests import these
    # same modules normally (`from sendgrid_server import send_email`);
    # permanently clobbering sys.modules with this stubbed instance would
    # break their monkeypatch targets (the imported closure would point at
    # a different module object than the one being patched).
    previous = sys.modules.get(name)
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    finally:
        if previous is not None:
            sys.modules[name] = previous
        else:
            sys.modules.pop(name, None)
    return module


# email_lint must import cleanly with zero stubs — it's the contract.
email_lint = _load("email_lint")
sendgrid_server = _load("sendgrid_server")
mailbux = _load("mailbux")


# ── Rule catalog sanity ─────────────────────────────────────────────────


class RuleCatalogTests(unittest.TestCase):
    def test_every_rule_has_a_severity(self):
        for code, (severity, _title) in email_lint.RULES.items():
            self.assertIn(severity, ("error", "warning"), f"{code} has bad severity {severity!r}")

    def test_finding_severity_defaults_from_catalog(self):
        self.assertEqual(email_lint.Finding(rule="EH001", message="x").severity, "error")
        self.assertEqual(email_lint.Finding(rule="EC002", message="y").severity, "warning")

    def test_unknown_rule_code_rejected(self):
        with self.assertRaises(ValueError):
            email_lint.Finding(rule="EX999", message="nope")


# ── PR #195 token finder regression ─────────────────────────────────────


class _SharedTokenFinderTests:
    """Shared across sendgrid_server, mailbux, and email_lint to prove
    all three return the same shape. PR #195 history: the single-brace
    pattern used to also match inside `{{…}}` / `${…}` and surface a
    misleading single-brace fragment — agents stripped valid
    `{{handlebars}}` thinking they were the problem."""

    finder = staticmethod(lambda _content: [])  # overridden

    def test_double_brace_returned_whole(self):
        self.assertEqual(self.finder("Hi {{badge_label}}!"), ["{{badge_label}}"])

    def test_dollar_brace_returned_whole(self):
        self.assertEqual(
            self.finder("subject=${tool:run_python.vars.subject}"),
            ["${tool:run_python.vars.subject}"],
        )

    def test_single_brace_still_detected(self):
        self.assertEqual(self.finder("Hi {first_name}!"), ["{first_name}"])

    def test_single_brace_with_format_spec(self):
        self.assertEqual(self.finder("revenue: {amount:,.2f}"), ["{amount:,.2f}"])

    def test_mixed_returns_distinct_full_tokens(self):
        tokens = self.finder("Hi {first_name}, your badge is {{badge_label}}.")
        self.assertEqual(tokens, ["{first_name}", "{{badge_label}}"])

    def test_clean_html_returns_empty(self):
        self.assertEqual(self.finder("<p>Hello Brad, your report is attached.</p>"), [])

    def test_inner_brace_of_double_brace_not_reported(self):
        self.assertNotIn("{badge_label}", self.finder("Hi {{badge_label}}!"))

    def test_inner_brace_of_dollar_brace_not_reported(self):
        self.assertNotIn("{run_python}", self.finder("${run_python}"))


class EmailLintFinderTests(_SharedTokenFinderTests, unittest.TestCase):
    finder = staticmethod(email_lint.find_unresolved_template_tokens)


class SendgridFinderTests(_SharedTokenFinderTests, unittest.TestCase):
    finder = staticmethod(sendgrid_server._find_unresolved_template_tokens)


class MailbuxFinderTests(_SharedTokenFinderTests, unittest.TestCase):
    finder = staticmethod(mailbux._find_unresolved_template_tokens)


# ── Legacy validator-integration regressions ────────────────────────────


class ValidatorIntegrationTests(unittest.TestCase):
    """The legacy `_validate_email_body` shim must keep returning the
    full `{{…}}` form in its error message so agents fix the right thing."""

    def test_sendgrid_body_error_surfaces_double_brace(self):
        err = sendgrid_server._validate_email_body("<p>Hi {{badge_label}}, here's your report.</p>", "text/html")
        self.assertIsNotNone(err)
        self.assertIn("{{badge_label}}", err)

    def test_sendgrid_subject_error_surfaces_dollar_brace(self):
        err = sendgrid_server._validate_email_subject("${tool:run_python.vars.subject}")
        self.assertIsNotNone(err)
        self.assertIn("${tool:run_python.vars.subject}", err)

    def test_sendgrid_clean_body_passes(self):
        clean = "<p>Hi Brad, your weekly report is attached.</p>"
        self.assertIsNone(sendgrid_server._validate_email_body(clean, "text/html"))

    def test_mailbux_body_error_surfaces_double_brace(self):
        err = mailbux._validate_email_body("<p>Hi {{badge_label}}, here's your report.</p>", "text/html")
        self.assertIsNotNone(err)
        self.assertIn("{{badge_label}}", err)


# ── Foster-parent (EH001 / EH002) and EC001 rgba ────────────────────────


class _SharedStructureTests:
    """Run the structural checks via the legacy `_validate_html` shim
    (so the shim's wiring is exercised) AND via the structured
    `email_lint.validate` (so the rule codes are the stable contract).
    Both should agree."""

    validate = staticmethod(lambda _content: ([], []))  # overridden

    SHELL_OPEN = '<html><body><table class="email-container">'
    SHELL_CLOSE = "</table></body></html>"
    HEADER_ROW = "<tr><td>Header</td></tr>"
    FOOTER_ROW = "<tr><td>Footer</td></tr>"

    def _findings(self, inner: str):
        return email_lint.validate(self.SHELL_OPEN + inner + self.SHELL_CLOSE, content_type="text/html")

    def test_div_direct_child_of_table_is_EH001(self):
        # OMUS Comfluence regression — agent appended raw <div> prose
        # between status_bar and footer.
        findings = self._findings(
            self.HEADER_ROW + '<div style="padding:20px;"><p>Hey Vlad, …</p></div>' + self.FOOTER_ROW
        )
        eh001 = [f for f in findings if f.rule == "EH001"]
        self.assertEqual(len(eh001), 1, [f.format() for f in findings])
        # Legacy shim returns the same error formatted as a string.
        errors, _ = self.validate(self.SHELL_OPEN + self.HEADER_ROW + "<div>x</div>" + self.FOOTER_ROW + self.SHELL_CLOSE)
        self.assertTrue(any("EH001" in e for e in errors), errors)

    def test_heading_direct_child_of_table_is_EH001(self):
        findings = self._findings(self.HEADER_ROW + "<h2>Pricing</h2>" + self.FOOTER_ROW)
        self.assertIn("EH001", [f.rule for f in findings])

    def test_raw_inner_table_direct_child_of_table_is_EH001(self):
        findings = self._findings(self.HEADER_ROW + "<table><tr><td>Deal</td></tr></table>" + self.FOOTER_ROW)
        self.assertTrue(any(f.rule == "EH001" and "<table>" in f.message for f in findings))

    def test_p_direct_child_of_tbody_is_EH001(self):
        findings = self._findings(
            "<tr><td><table><tbody>"
            "<tr><td>row 1</td></tr><p>orphan</p><tr><td>row 2</td></tr>"
            "</tbody></table></td></tr>"
        )
        self.assertTrue(any(f.rule == "EH001" and "<tbody>" in f.message for f in findings))

    def test_loose_text_between_rows_is_EH002(self):
        # Jules #202 — prose between rows foster-parents above the header.
        findings = self._findings(self.HEADER_ROW + "Hey Vlad and Mark, got your email." + self.FOOTER_ROW)
        eh002 = [f for f in findings if f.rule == "EH002"]
        self.assertEqual(len(eh002), 1)

    def test_single_violation_reported_once_for_many_children(self):
        children = "".join(f"<p>line {i}</p>" for i in range(20))
        findings = self._findings(self.HEADER_ROW + f"<div>{children}</div>" + self.FOOTER_ROW)
        eh001 = [f for f in findings if f.rule == "EH001"]
        self.assertEqual(len(eh001), 1, f"got {len(eh001)} dedup violations")

    def test_valid_canonical_passes_cleanly(self):
        findings = self._findings(
            "<tr><td>header</td></tr>"
            "<tr><td>status_bar</td></tr>"
            "<tr><td><table><tbody><tr><td>data row</td></tr></tbody></table></td></tr>"
            "<tr><td>footer</td></tr>"
        )
        structural = [f for f in findings if f.rule in ("EH001", "EH002")]
        self.assertEqual(structural, [], f"unexpectedly flagged: {[f.format() for f in structural]}")

    def test_prose_inside_td_passes(self):
        # Wrapped prose is the correct shape — must NOT flag.
        findings = self._findings(
            "<tr><td>header</td></tr>"
            "<tr><td style=\"padding:20px;\">"
            "<p>Hi.</p><h3>Root</h3><div>Body.</div>"
            "</td></tr>"
            "<tr><td>footer</td></tr>"
        )
        foster = [f for f in findings if f.rule in ("EH001", "EH002")]
        self.assertEqual(foster, [], f"prose-in-<td> unexpectedly flagged: {foster}")


class SendgridStructureTests(_SharedStructureTests, unittest.TestCase):
    validate = staticmethod(sendgrid_server._validate_html)


class MailbuxStructureTests(_SharedStructureTests, unittest.TestCase):
    validate = staticmethod(mailbux._validate_html)


class _SharedRgbaTests:
    """rgba() — Outlook strips the alpha and an overlay becomes opaque."""

    validate = staticmethod(lambda _content: ([], []))  # overridden

    SHELL = '<html><body><table class="email-container"><tr><td>{body}</td></tr></table></body></html>'

    def _validate(self, inner: str):
        return self.validate(self.SHELL.format(body=inner))

    def test_rgba_in_style_is_EC001(self):
        errors, _ = self._validate('<p style="background:rgba(0,0,0,0.6);">overlay</p>')
        self.assertTrue(any("EC001" in e for e in errors), errors)

    def test_rgba_with_space_after_keyword_is_EC001(self):
        errors, _ = self._validate('<p style="color:rgba (255,255,255,0.5);">a</p>')
        self.assertTrue(any("EC001" in e for e in errors), errors)

    def test_uppercase_rgba_is_EC001(self):
        errors, _ = self._validate('<p style="color:RGBA(0,0,0,0.5);">a</p>')
        self.assertTrue(any("EC001" in e for e in errors), errors)

    def test_plain_rgb_passes(self):
        errors, _ = self._validate('<p style="color:rgb(255,0,0);">a</p>')
        self.assertFalse(any("EC001" in e for e in errors), errors)

    def test_hex_color_passes(self):
        errors, _ = self._validate('<p style="color:#FF0000;">a</p>')
        self.assertFalse(any("EC001" in e for e in errors), errors)


class SendgridRgbaTests(_SharedRgbaTests, unittest.TestCase):
    validate = staticmethod(sendgrid_server._validate_html)


class MailbuxRgbaTests(_SharedRgbaTests, unittest.TestCase):
    validate = staticmethod(mailbux._validate_html)


# ── EP001 — pad-cell nesting (TWC Campaign Health Scan regression) ─────


class PadNestingTests(unittest.TestCase):
    """TWC Campaign Health Scan regression (cutlass 33e4468, 2026-05-21).

    Victoria emitted <td class="status-pad"> with an inner badge <table>,
    closed the inner <tr>, but forgot </table></td></tr>. The next body
    section ended up nested inside the badge table, squeezing the rest
    of the email through a 2-column cell. Ported into the shared
    validator so chat picks up the check too."""

    def _findings(self, inner: str):
        shell = '<html><body><table class="email-container">{body}</table></body></html>'
        return email_lint.validate(shell.format(body=inner), content_type="text/html")

    def test_body_pad_nested_in_status_pad_is_EP001(self):
        findings = self._findings(
            '<tr><td class="status-pad"><table>'
            "<tr><td>BADGE</td><td>Status: ok</td></tr>"
            # MISSING </table></td></tr> here
            '<tr><td class="body-pad"><table><tr><td>body</td></tr></table></td></tr>'
        )
        ep001 = [f for f in findings if f.rule == "EP001"]
        self.assertEqual(len(ep001), 1, [f.format() for f in findings])

    def test_pads_at_top_level_pass(self):
        findings = self._findings(
            '<tr><td class="header-pad">header</td></tr>'
            '<tr><td class="status-pad">'
            "<table><tr><td>BADGE</td><td>ok</td></tr></table>"
            "</td></tr>"
            '<tr><td class="body-pad">body</td></tr>'
            '<tr><td class="footer-pad">footer</td></tr>'
        )
        self.assertNotIn("EP001", [f.rule for f in findings])

    def test_multiple_nested_pads_dedup_to_one_finding(self):
        findings = self._findings(
            '<tr><td class="status-pad"><table>'
            "<tr><td>BADGE</td></tr>"
            '<tr><td class="body-pad">section 1</td></tr>'
            '<tr><td class="body-pad">section 2</td></tr>'
            "</table></td></tr>"
        )
        ep001 = [f for f in findings if f.rule == "EP001"]
        self.assertEqual(len(ep001), 1)

    def test_unclassed_inner_td_inside_pad_passes(self):
        findings = self._findings(
            '<tr><td class="body-pad">'
            "<table><tr><td>Metric</td><td>Value</td></tr></table>"
            "</td></tr>"
        )
        self.assertNotIn("EP001", [f.rule for f in findings])


# ── Real-world regression fixtures ──────────────────────────────────────
# `mcp/testdata/*.html` are high-fidelity reproductions of actual bugs
# that hit production. If a future refactor breaks detection on any of
# these, that's a regression.


class RealWorldFixtureTests(unittest.TestCase):
    """The TWC and OMUS fixtures live identically in both repos so the
    drift check covers them too. The structural bugs they contain are
    the originating reason each rule exists."""

    FIXTURE_DIR = _MCP_DIR / "testdata"

    def _load(self, name: str) -> str:
        return (self.FIXTURE_DIR / name).read_text(encoding="utf-8")

    def test_twc_health_scan_real_fixture_fires_EP001(self):
        # The exact failure that prompted EP001 (cutlass commit 33e4468,
        # 2026-05-21): status-pad with a forgotten `</table></td></tr>`
        # swallowed every subsequent body row. Picked up by chat as part
        # of the email_lint extraction.
        findings = email_lint.validate(
            self._load("twc_health_scan_broken.html"),
            content_type="text/html",
        )
        ep001 = [f for f in findings if f.rule == "EP001"]
        codes = [f.rule for f in findings]
        self.assertGreaterEqual(
            len(ep001), 1, f"TWC regression must fire EP001 — got codes: {codes}"
        )

    def test_omus_comfluence_real_fixture_fires_EH001(self):
        # The OMUS / Kyle regression (chat#201): bare `<div>` between
        # component `<tr>`s rendered above the dark header because
        # HTML5 foster-parented it out of the table.
        findings = email_lint.validate(
            self._load("omus_comfluence_div_orphan.html"),
            content_type="text/html",
        )
        eh001 = [f for f in findings if f.rule == "EH001"]
        codes = [f.rule for f in findings]
        self.assertGreaterEqual(
            len(eh001), 1, f"OMUS regression must fire EH001 — got codes: {codes}"
        )
        self.assertTrue(
            any("<div>" in f.message for f in eh001),
            f"EH001 message must mention <div>: {[f.message for f in eh001]}",
        )


if __name__ == "__main__":
    unittest.main()
