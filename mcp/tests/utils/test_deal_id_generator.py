import os
import re
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from utils.deal_id_generator import generate_external_deal_id


def test_generate_external_deal_id_default_format_and_constraints() -> None:
    deal_id = generate_external_deal_id()

    assert re.fullmatch(r"IX\d{18}", deal_id)
    assert len(deal_id) <= 64
    assert " " not in deal_id
    assert not deal_id.startswith("0")
    assert re.fullmatch(r"[A-Za-z0-9._-]+", deal_id)


def test_generate_external_deal_id_total_length_matches_ix_ui() -> None:
    """The IX UI generates 20-character deal IDs (e.g. IX777567008013364251).
    The MCP must produce the same total length so deals look consistent across
    both surfaces. Regression: a 17-digit numeric component shipped as
    'one character short' next to UI-created deals."""
    deal_id = generate_external_deal_id()
    assert len(deal_id) == 20  # "IX" (2) + 18 numeric digits


def test_generate_external_deal_id_is_unique() -> None:
    generated = {generate_external_deal_id() for _ in range(500)}

    assert len(generated) == 500


def test_generate_external_deal_id_long_custom_prefix_is_trimmed() -> None:
    deal_id = generate_external_deal_id(prefix="A" * 100)

    assert len(deal_id) <= 64
    assert re.fullmatch(r"[A-Za-z0-9._-]+", deal_id)


def test_generate_external_deal_id_sanitizes_invalid_prefix_chars() -> None:
    deal_id = generate_external_deal_id(prefix="Brand Name*&$")

    assert re.fullmatch(r"BrandName\d{18}", deal_id)
    assert re.fullmatch(r"[A-Za-z0-9._-]+", deal_id)


def test_generate_external_deal_id_prefix_cannot_start_with_zero() -> None:
    deal_id = generate_external_deal_id(prefix="0ABC")

    assert not deal_id.startswith("0")
    assert re.fullmatch(r"X0ABC\d{18}", deal_id)
