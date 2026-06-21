import gzip
import zipfile

from ses_s3_email import (
    attachment_name_matches,
    build_collision_safe_output_path,
    clean_reported_filename,
    sanitize_download_filename,
    sniff_file_metadata,
)


def test_build_collision_safe_output_path_uses_source_token(tmp_path):
    path = build_collision_safe_output_path(tmp_path, "report.csv", "emails/2026/03/31/example:report.csv")

    assert path.parent == tmp_path
    assert path.name.startswith("report__")
    assert path.suffix == ".csv"


def test_build_collision_safe_output_path_preserves_multi_suffix(tmp_path):
    path = build_collision_safe_output_path(tmp_path, "archive.csv.zip", "source-a")

    assert path.name.endswith(".csv.zip")


def test_build_collision_safe_output_path_avoids_existing_file(tmp_path):
    first = build_collision_safe_output_path(tmp_path, "report.csv", "source-a")
    first.write_text("existing")

    second = build_collision_safe_output_path(tmp_path, "report.csv", "source-a")

    assert second != first
    assert second.name.startswith(first.stem + "_")


def test_sanitize_download_filename_preserves_meaningful_segments():
    raw = "Magnite_TWC - Overview - America/Los_Angeles.csv"

    assert sanitize_download_filename(raw) == "Magnite_TWC - Overview - America_Los_Angeles.csv"


def test_attachment_name_matches_saved_path_variant():
    part_filename = "Magnite_TWC - Overview - America/Los_Angeles.csv"
    requested = "/tmp/twc_reports/Magnite_TWC - Overview - America_Los_Angeles.csv"

    assert attachment_name_matches(part_filename, requested)


# ── clean_reported_filename ──


def test_clean_reported_filename_collapses_header_folded_crlf():
    # PubMatic-style: vendor folded a long filename across multiple MIME header
    # lines. decode_email_header leaves raw CR+LF bytes in the decoded string.
    raw = "50,751-Historical\r\n Report-Reklaimdailyreport-20260325T00.csv"

    assert clean_reported_filename(raw) == "50,751-Historical Report-Reklaimdailyreport-20260325T00.csv"


def test_clean_reported_filename_survives_round_trip_with_matcher():
    # The whole point: if we report the cleaned name to the agent, and the
    # agent passes it back verbatim, attachment_name_matches still finds the
    # part on disk whose filename still has CRLF.
    part_filename_on_disk = "50,751-Historical\r\n Report-Reklaim.csv"
    reported_to_agent = clean_reported_filename(part_filename_on_disk)

    assert attachment_name_matches(part_filename_on_disk, reported_to_agent)


def test_clean_reported_filename_handles_none_and_whitespace():
    assert clean_reported_filename(None) == ""
    assert clean_reported_filename("  \t\n  ") == ""
    assert clean_reported_filename("  report.csv  ") == "report.csv"


# ── sniff_file_metadata ──


def test_sniff_file_metadata_csv_with_bom_and_quoted_delimiter(tmp_path):
    # Mimic the PubMatic file shape: UTF-8 BOM + leading "" row + CSV content.
    content = b'\xef\xbb\xbf""\r\n"Date","DSP","Spend($)"\r\n"2026-03-25","DV360","1.23"\r\n'
    p = tmp_path / "pm.csv"
    p.write_bytes(content)

    meta = sniff_file_metadata(p)

    assert meta["kind"] == "csv"
    assert meta["byte_order_mark"] == "utf-8-bom"
    assert meta["delimiter"] == ","
    # We shouldn't require a specific header layout since the leading "" row
    # confuses some sniffers — but sample_newline_count should be meaningful.
    assert meta["sample_newline_count"] >= 2
    assert meta["size_bytes"] == len(content)


def test_sniff_file_metadata_tsv(tmp_path):
    p = tmp_path / "report.tsv"
    p.write_text("col_a\tcol_b\tcol_c\n1\t2\t3\n")

    meta = sniff_file_metadata(p)

    assert meta["kind"] == "tsv"
    assert meta["delimiter"] == "\t"
    assert meta["header_sample"][:3] == ["col_a", "col_b", "col_c"]


def test_sniff_file_metadata_zip_of_csv(tmp_path):
    p = tmp_path / "ix_report.csv.zip"
    with zipfile.ZipFile(p, "w") as zf:
        zf.writestr("inner.csv", "day,deal,spend\n2026-04-01,foo,100\n")

    meta = sniff_file_metadata(p)

    assert meta["kind"] == "zip_csv"
    assert meta["inner_member"] == "inner.csv"
    assert meta["delimiter"] == ","
    assert "day" in meta["header_sample"]


def test_sniff_file_metadata_gzip_csv(tmp_path):
    p = tmp_path / "ox_report.csv.gz"
    with gzip.open(p, "wb") as gfh:
        gfh.write(b"Day,Deal Name,Spend\n2026-04-01,foo,100\n")

    meta = sniff_file_metadata(p)

    assert meta["kind"] == "gzip_csv"
    assert meta["delimiter"] == ","
    assert "Day" in meta["header_sample"]
