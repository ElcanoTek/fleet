package tools

// browserPreamble is the Python that defines the persistent browser session
// helpers (#503). It is idempotent — re-running it across tool calls redefines
// the helpers but leaves the module-level _fleet_page/_fleet_browser alone, so
// the session survives. Kept in a separate file for readability.
const browserPreamble = `# fleet browser session (#503) — Playwright(sync) in the persistent kernel.
import json as _json, sys as _sys, time as _time
_fleet_browser_result = {"ok": False, "action": "?", "error": "no action ran"}

def _fleet_ensure():
    """Lazily create a module-level browser + page; reused across tool calls."""
    global _fleet_pw, _fleet_browser, _fleet_page
    try:
        _fleet_page  # noqa: F821
        # Touch the page to detect a dead handle (kernel bounced mid-session).
        _fleet_page.title()
        return
    except NameError:
        pass
    except Exception:
        # Stale/broken handle — tear down and recreate.
        try:
            _fleet_browser.close()
        except Exception:
            pass
    from playwright.sync_api import sync_playwright
    _fleet_pw = sync_playwright().start()
    _fleet_browser = _fleet_pw.chromium.launch(args=["--no-sandbox", "--disable-dev-shm-usage"])
    _fleet_page = _fleet_browser.new_page()

# Interactive-element extraction: a numbered, stable-ordered list of the
# clickable/typeable elements, so the model refers to them by number (Ref).
_FLEET_SEL = "a[href], button, input:not([type=hidden]), textarea, select, [role=button], [role=link]"

def _fleet_elements():
    els = _fleet_page.query_selector_all(_FLEET_SEL)
    out, refs = [], []
    for el in els:
        try:
            if not el.is_visible():
                continue
        except Exception:
            continue
        tag = (el.evaluate("e => e.tagName") or "").lower()
        kind = {"a": "link", "button": "button", "input": "input", "textarea": "input", "select": "select"}.get(tag, "control")
        label = ""
        try:
            label = (el.inner_text() or "").strip()
            if not label:
                label = (el.get_attribute("aria-label") or el.get_attribute("placeholder") or el.get_attribute("value") or "").strip()
        except Exception:
            pass
        refs.append(el)
        out.append({"ref": len(refs), "kind": kind, "text": label[:200]})
    global _fleet_refs
    _fleet_refs = refs
    return out

def _fleet_nav(url):
    global _fleet_browser_result
    try:
        _fleet_ensure()
        _fleet_page.goto(url, wait_until="domcontentloaded", timeout=60000)
        _fleet_browser_result = {"ok": True, "action": "navigate", "url": _fleet_page.url, "title": _fleet_page.title()}
    except Exception as e:
        _fleet_browser_result = {"ok": False, "action": "navigate", "error": str(e)}

def _fleet_read():
    global _fleet_browser_result
    try:
        _fleet_ensure()
        try:
            text = _fleet_page.inner_text("body")
        except Exception:
            text = ""
        _fleet_browser_result = {"ok": True, "action": "read", "url": _fleet_page.url, "title": _fleet_page.title(), "text": text, "elements": _fleet_elements()}
    except Exception as e:
        _fleet_browser_result = {"ok": False, "action": "read", "error": str(e)}

def _fleet_click(ref):
    global _fleet_browser_result
    try:
        _fleet_ensure()
        el = _fleet_refs[ref - 1]
        el.click(timeout=15000)
        _fleet_page.wait_for_load_state("domcontentloaded", timeout=30000)
        _fleet_browser_result = {"ok": True, "action": "click", "url": _fleet_page.url, "title": _fleet_page.title()}
    except IndexError:
        _fleet_browser_result = {"ok": False, "action": "click", "error": "no element with that number; run read again"}
    except NameError:
        _fleet_browser_result = {"ok": False, "action": "click", "error": "no elements loaded; run read first"}
    except Exception as e:
        _fleet_browser_result = {"ok": False, "action": "click", "error": str(e)}

def _fleet_type(ref, text):
    global _fleet_browser_result
    try:
        _fleet_ensure()
        el = _fleet_refs[ref - 1]
        el.fill(text, timeout=15000)
        _fleet_browser_result = {"ok": True, "action": "type", "url": _fleet_page.url, "title": _fleet_page.title()}
    except IndexError:
        _fleet_browser_result = {"ok": False, "action": "type", "error": "no element with that number; run read again"}
    except NameError:
        _fleet_browser_result = {"ok": False, "action": "type", "error": "no elements loaded; run read first"}
    except Exception as e:
        _fleet_browser_result = {"ok": False, "action": "type", "error": str(e)}

def _fleet_screenshot():
    global _fleet_browser_result
    try:
        _fleet_ensure()
        name = "browser-%d.png" % int(_time.time() * 1000)
        _fleet_page.screenshot(path=name, full_page=False)
        _fleet_browser_result = {"ok": True, "action": "screenshot", "url": _fleet_page.url, "screenshot": name}
    except Exception as e:
        _fleet_browser_result = {"ok": False, "action": "screenshot", "error": str(e)}

`
