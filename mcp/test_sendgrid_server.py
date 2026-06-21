from pathlib import Path

import pytest
from sendgrid_server import send_email, validate_email_content


@pytest.mark.asyncio
async def test_send_email_rejects_missing_inline_cid(tmp_path: Path):
    html = '<html><body><div><img src="cid:weekly_chart"></div></body></html>'
    chart = tmp_path / "chart.png"
    chart.write_bytes(b"fakepng")

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
        attachments=[str(chart)],
    )

    assert "error" in result
    assert "Missing inline cids" in result["error"]


@pytest.mark.asyncio
async def test_send_email_rejects_unmapped_inline_cid(tmp_path: Path):
    html = '<html><body><div><img src="cid:weekly_chart"></div></body></html>'
    chart = tmp_path / "chart.png"
    chart.write_bytes(b"fakepng")

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
        inline_attachments=[{"path": str(chart), "cid": "some_other_cid"}],
    )

    assert "error" in result
    assert "Missing inline cids" in result["error"]


@pytest.mark.asyncio
async def test_send_email_accepts_inline_cid(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    html = '<html><body><div><img src="cid:weekly_chart"></div></body></html>'
    chart = tmp_path / "chart.png"
    chart.write_bytes(b"fakepng")

    async def fake_sendgrid_request(*_args, **_kwargs):
        return {"status_code": 202, "message_id": "test-message-id"}

    monkeypatch.setattr("sendgrid_server._sendgrid_request", fake_sendgrid_request)

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
        inline_attachments=[{"path": str(chart), "cid": "weekly_chart"}],
    )

    assert result.get("status") == "queued"
    assert result.get("inline_attachments_count") == 1
    assert result.get("inline_attachments", [{}])[0].get("cid") == "weekly_chart"


@pytest.mark.asyncio
async def test_send_email_rejects_data_uri_image():
    svg_data = (
        "data:image/svg+xml;base64,PHN2ZyB4bWxucz0naHR0cDovL3d3dy53My5vcmcvMjAw"
        "MC9zdmcnIHdpZHRoPScxMjAnIGhlaWdodD0nMjgnPjwvc3ZnPg=="
    )
    html = f'<html><body><img src="{svg_data}" alt="Elcano"></body></html>'

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
    )

    assert "error" in result
    assert "data: URI" in result["error"]


@pytest.mark.asyncio
async def test_send_email_rejects_unknown_scheme_image():
    html = '<html><body><img src="ftp://assets/logo.png"></body></html>'

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
    )

    assert "error" in result
    assert "cid: or http(s)" in result["error"]


@pytest.mark.asyncio
async def test_send_email_accepts_https_image(monkeypatch: pytest.MonkeyPatch):
    html = '<html><body><div><img src="https://assets.example.com/logo.png" alt="Logo"></div></body></html>'

    async def fake_sendgrid_request(*_args, **_kwargs):
        return {"status_code": 202, "message_id": "test-message-id"}

    monkeypatch.setattr("sendgrid_server._sendgrid_request", fake_sendgrid_request)

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
    )

    assert result.get("status") == "queued"


@pytest.mark.asyncio
async def test_send_email_rejects_python_style_template_tokens():
    html = "<html><body><div>Top 5 share: {top5_share:.1f}%</div></body></html>"

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
    )

    assert "error" in result
    assert "Unresolved template tokens found" in result["error"]
    assert "{top5_share:.1f}" in result["error"]


@pytest.mark.asyncio
async def test_send_email_rejects_jinja_style_template_tokens():
    html = "<html><body><div>Total: {{ total_spend }}</div></body></html>"

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="test",
        content=html,
    )

    assert "error" in result
    assert "Unresolved template tokens found" in result["error"]
    assert "{{ total_spend }}" in result["error"]


@pytest.mark.asyncio
async def test_send_email_rejects_shell_style_template_tokens_in_subject():
    html = "<html><body><div>Report body.</div></body></html>"

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="${tool:run_python.vars.subject}",
        content=html,
    )

    assert "error" in result
    assert "Unresolved template tokens in subject" in result["error"]
    assert "${tool:run_python.vars.subject}" in result["error"]


@pytest.mark.asyncio
async def test_send_email_rejects_jinja_style_template_tokens_in_subject():
    html = "<html><body><div>Report body.</div></body></html>"

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="{{ client_name }} weekly report",
        content=html,
    )

    assert "error" in result
    assert "Unresolved template tokens in subject" in result["error"]
    assert "{{ client_name }}" in result["error"]


@pytest.mark.asyncio
async def test_send_email_accepts_plain_subject(monkeypatch: pytest.MonkeyPatch):
    html = "<html><body><div>Daily DSP Report body.</div></body></html>"

    async def fake_sendgrid_request(*_args, **_kwargs):
        return {"status_code": 202, "message_id": "test-message-id"}

    monkeypatch.setattr("sendgrid_server._sendgrid_request", fake_sendgrid_request)

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="Daily DSP Report — 2026-04-14",
        content=html,
    )

    assert result.get("status") == "queued"


@pytest.mark.asyncio
async def test_validate_email_content_flags_subject_tokens():
    html = "<html><body><div>Body.</div></body></html>"

    result = await validate_email_content(
        content=html,
        subject="${tool:run_python.vars.subject}",
    )

    assert result["valid"] is False
    assert any("subject" in err.lower() for err in result["errors"])


@pytest.mark.asyncio
async def test_send_email_rejects_data_preview_attribute_leakage():
    html = (
        "<html><body><table><tr>"
        '<td data-preview="summary-1">Real content here but the attr survived.</td>'
        "</tr></table></body></html>"
    )

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="Daily Report — 2026-04-24",
        content=html,
    )

    assert "error" in result
    assert "data-preview" in result["error"]
    assert "summary-1" in result["error"]


@pytest.mark.asyncio
async def test_send_email_rejects_tier2_demo_prose():
    html = (
        "<html><body><p>Canada display inventory stayed efficient, creating "
        "room to shift spend without increasing blended CPM.</p></body></html>"
    )

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="Daily Report — 2026-04-24",
        content=html,
    )

    assert "error" in result
    assert "demo prose" in result["error"]


@pytest.mark.asyncio
async def test_send_email_rejects_tier3_three_markers():
    html = (
        "<html><body><p>Amazon US OLV pacing at $589,075.45.</p><p>Compare with Pepsi Display trend.</p></body></html>"
    )

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="Daily Report — 2026-04-24",
        content=html,
    )

    assert "error" in result
    assert "demo markers co-occur" in result["error"]


@pytest.mark.asyncio
async def test_send_email_allows_single_tier3_marker(monkeypatch: pytest.MonkeyPatch):
    # Real Amazon campaign name alone must not block — only warn.
    html = (
        "<html><body><p>Amazon US OLV delivered 48,203,112 impressions at an effective CPM of $8.14.</p></body></html>"
    )

    async def fake_sendgrid_request(*_args, **_kwargs):
        return {"status_code": 202, "message_id": "test-message-id"}

    monkeypatch.setattr("sendgrid_server._sendgrid_request", fake_sendgrid_request)

    result = await send_email(
        to_email="test@example.com",
        from_email="sender@example.com",
        subject="Daily Report — 2026-04-24",
        content=html,
    )

    assert result.get("status") == "queued"


@pytest.mark.asyncio
async def test_validate_email_content_warns_on_single_tier3_marker():
    html = "<html><body><p>Amazon US OLV delivered 48M impressions.</p></body></html>"

    result = await validate_email_content(content=html)

    assert result["valid"] is True
    assert any("Amazon US OLV" in w for w in result["warnings"])


@pytest.mark.asyncio
async def test_validate_email_content_blocks_data_preview_leakage():
    html = '<html><body><table><tr><td data-preview="campaign-table-body">Amazon US OLV</td></tr></table></body></html>'

    result = await validate_email_content(content=html)

    assert result["valid"] is False
    assert any("data-preview" in err for err in result["errors"])


@pytest.mark.asyncio
async def test_send_email_blocks_arbitrary_file_read():
    result = await send_email(
        to_email="test@example.com", subject="Test", from_email="sender@example.com", content_file="/etc/passwd"
    )

    assert "error" in result
    assert "SECURITY: Path is outside allowed directories" in result["error"]

    result = await send_email(
        to_email="test@example.com",
        subject="Test",
        from_email="sender@example.com",
        content="<html><body>Test content</body></html>",
        attachments=["../../../../etc/passwd"],
    )

    assert "error" in result
    assert "SECURITY: Path is outside allowed directories" in result["error"]
