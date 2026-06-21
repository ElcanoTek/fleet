"""Regression tests for the unresolved-template-token regex.

`sendgrid_server.py` hard-stops sends that still contain unfilled `{name}`,
`{{name}}`, or `${name}` tokens. The single-brace pattern used to also match
*inside* `{{name}}` / `${name}` and surface a misleading single-brace fragment
in the error — agents drafting emails would then strip out valid
`{{handlebars}}` placeholders thinking those were the problem and the email
rendered blank. Mirrors the fix already landed in ElcanoTek/chat#195.
"""

from __future__ import annotations

import pytest
from sendgrid_server import (
    _find_unresolved_template_tokens,
    _validate_email_body,
    _validate_email_subject,
)


@pytest.mark.parametrize(
    "content,expected",
    [
        # The actual regression: must return the full `{{...}}` form, never
        # the inner `{badge_label}` fragment.
        ("Hi {{badge_label}}!", ["{{badge_label}}"]),
        # `${...}` is handled by a dedicated pattern; the single-brace
        # pattern must not double-fire on its inner `{...}`.
        ("subject=${tool:run_python.vars.subject}", ["${tool:run_python.vars.subject}"]),
        # Real single-brace placeholders still get caught.
        ("Hi {first_name}!", ["{first_name}"]),
        ("revenue: {amount:,.2f}", ["{amount:,.2f}"]),
        # Mixed input returns the full forms of both tokens.
        (
            "Hi {first_name}, your badge is {{badge_label}}.",
            ["{first_name}", "{{badge_label}}"],
        ),
        # Clean HTML has nothing to flag.
        ("<p>Hello Brad, your report is attached.</p>", []),
    ],
)
def test_find_unresolved_template_tokens(content: str, expected: list[str]) -> None:
    assert _find_unresolved_template_tokens(content) == expected


def test_inner_brace_of_double_brace_not_reported() -> None:
    # The exact regression that motivated the fix. A bare `{badge_label}`
    # fragment must NOT appear in the output when the source has `{{badge_label}}`.
    assert "{badge_label}" not in _find_unresolved_template_tokens("Hi {{badge_label}}!")


def test_inner_brace_of_dollar_brace_not_reported() -> None:
    assert "{run_python}" not in _find_unresolved_template_tokens("${run_python}")


def test_validate_email_body_surfaces_double_brace_form() -> None:
    # User-visible error message must show the full `{{...}}` form so the
    # agent can fix the right thing.
    err = _validate_email_body("<p>Hi {{badge_label}}, here's your report.</p>", "text/html")
    assert err is not None
    assert "{{badge_label}}" in err
    assert "{badge_label}," not in err  # no bare single-brace fragment


def test_validate_email_subject_surfaces_dollar_brace_form() -> None:
    err = _validate_email_subject("${tool:run_python.vars.subject}")
    assert err is not None
    assert "${tool:run_python.vars.subject}" in err


def test_validate_email_body_passes_clean_html() -> None:
    clean = "<p>Hi Brad, your weekly report is attached.</p>"
    assert _validate_email_body(clean, "text/html") is None
