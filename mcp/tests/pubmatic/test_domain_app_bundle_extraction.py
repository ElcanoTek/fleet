"""Regression tests for PubMatic domain/app-bundle list extraction.

PubMatic targets web domains and app bundles through one undifferentiated
domainList, but the extractor previously validated every value against a
web-domain-only regex (DOMAIN_PATTERN). That silently dropped CTV/OTT
app-bundle lists' bare numeric store IDs (Roku/Apple/Amazon, e.g. 523428113)
and dotted bundles with numeric final labels — the same class of bug as the
Index Exchange run. These tests lock in acceptance of app-bundle shapes.
"""

import os
import sys

from openpyxl import Workbook

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from pubmatic_mcp import (
    _extract_domains_from_csv,
    _extract_domains_from_xlsx,
    _is_acceptable_domain_or_bundle,
)


class TestIsAcceptableDomainOrBundle:
    def test_accepts_web_domain(self):
        assert _is_acceptable_domain_or_bundle("example.com")

    def test_accepts_reverse_dns_bundle(self):
        assert _is_acceptable_domain_or_bundle("com.zumobi.msnbc")

    def test_accepts_dotted_numeric_final_label(self):
        # DOMAIN_PATTERN rejects a trailing numeric label; app-bundle accepts it.
        assert _is_acceptable_domain_or_bundle("com.example.app2")

    def test_accepts_bare_numeric_store_id(self):
        assert _is_acceptable_domain_or_bundle("523428113")
        assert _is_acceptable_domain_or_bundle("711586")

    def test_rejects_bare_token(self):
        # No dot, not numeric — likely a wrong-column / app-name pick.
        assert not _is_acceptable_domain_or_bundle("bloomberg")


class TestCsvExtraction:
    @staticmethod
    def _write(tmp_path, *values):
        path = tmp_path / "bundles.csv"
        path.write_text("Bundle ID\n" + "\n".join(values) + "\n", encoding="utf-8")
        return str(path)

    def test_numeric_and_reverse_dns_ids_accepted(self, tmp_path):
        path = self._write(tmp_path, "com.zumobi.msnbc", "523428113", "711586")
        result = _extract_domains_from_csv(path, column_name="Bundle ID")
        assert {"com.zumobi.msnbc", "523428113", "711586"} <= set(result["domains"])
        assert result["invalid_values"] == []

    def test_bare_token_still_rejected(self, tmp_path):
        path = self._write(tmp_path, "kbzk", "bloomberg")
        result = _extract_domains_from_csv(path, column_name="Bundle ID")
        assert result["domains"] == []
        assert set(result["invalid_values"]) == {"kbzk", "bloomberg"}


class TestXlsxExtraction:
    def test_numeric_floats_coerced_and_accepted(self, tmp_path):
        # openpyxl yields ints/floats for numeric cells; an integral float must
        # not persist a trailing ".0" (711586.0 -> "711586").
        workbook = Workbook()
        worksheet = workbook.active
        worksheet.append(["Bundle ID"])
        worksheet.append(["com.foo.bar"])
        worksheet.append([523428113])  # int
        worksheet.append([711586.0])  # float
        path = tmp_path / "bundles.xlsx"
        workbook.save(str(path))

        result = _extract_domains_from_xlsx(str(path), column_name="Bundle ID")
        assert {"com.foo.bar", "523428113", "711586"} <= set(result["domains"])
        assert "711586.0" not in result["domains"]
