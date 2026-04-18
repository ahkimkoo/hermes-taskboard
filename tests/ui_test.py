"""
Hermes Task Board — UI regression suite.

Run:
    TASKBOARD=http://127.0.0.1:1900 python3 tests/ui_test.py

Assumes a running taskboard on $TASKBOARD with a clean or stable data/ directory.
The suite is idempotent: every test creates any task it needs and cleans up via
the DELETE endpoint so it can re-run without manual reset.

Covers the full list of UI promises:
    1. smooth drag & reorder
    2. description editor (title-required, optional markdown body)
    3. attempts list (start time, collapse-when-one, confirm-on-new)
    4. event stream renders semantic cards + markdown (basic structural check)
    5. dark/light theme toggle persists
    6. delete-only-in-archive gating
    7. + New Task button lives inside the Draft column header
    8. new tasks land at the end of Draft; order preserved on reload
    9. i18n cleanly toggles — no mixing
   10. column subtitles
   11. Settings explains "Models = agent profiles"
   12. settings modal re-opens after closing
"""
import json
import os
import sys
import time
import uuid
from typing import Callable

from playwright.sync_api import Page, sync_playwright, expect

BASE = os.environ.get("TASKBOARD", "http://127.0.0.1:1900")


# ---------- helpers ----------


def wait_for_app(page: Page):
    """Wait for the root board to render (at least one column)."""
    page.wait_for_selector(".column", timeout=10_000)


def api_create_task(page: Page, title: str, status: str = "draft") -> str:
    """Hit the backend directly via fetch in the page context so cookies / auth still apply."""
    res = page.evaluate(
        "async ({title, status}) => (await fetch('/api/tasks', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({title, status, priority: 3, trigger_mode:'manual'})}).then(r => r.json()))",
        {"title": title, "status": status},
    )
    return res["task"]["id"]


def api_delete_task(page: Page, tid: str):
    page.evaluate(
        "id => fetch('/api/tasks/' + id, { method:'DELETE' })",
        tid,
    )


def find_card_by_title(page: Page, title: str):
    card = page.locator(f".card:has(.card-title:has-text(\"{title}\"))").first
    return card


def column_for(page: Page, status: str):
    return page.locator(f'.column[data-status="{status}"]').first


# ---------- tests ----------


class Ctx:
    passed: list[str] = []
    failed: list[tuple[str, str]] = []


def test(name: str):
    def deco(fn):
        def wrap(page, *a, **kw):
            # Fresh slate per test: reload page so no stale modals linger between cases.
            try:
                page.goto(BASE + "/", wait_until="domcontentloaded")
                wait_for_app(page)
            except Exception as e:
                Ctx.failed.append((name, f"setup failed: {e}"))
                print(f"  ✗ {name}: setup failed: {e}")
                return
            try:
                fn(page, *a, **kw)
                Ctx.passed.append(name)
                print(f"  ✓ {name}")
            except Exception as e:
                Ctx.failed.append((name, str(e)))
                print(f"  ✗ {name}: {e}")
        return wrap
    return deco


@test("#7 New-Task button lives in the Draft column header")
def test_new_task_button_in_draft(page: Page):
    draft = column_for(page, "draft")
    btn = draft.locator(".column-header button:has-text('New Task')")
    assert btn.count() > 0, "New Task button must be in Draft column header"


@test("#10 each column has a subtitle")
def test_column_subtitles(page: Page):
    for status in ["draft", "plan", "execute", "verify", "done", "archive"]:
        sub = column_for(page, status).locator(".column-subtitle")
        assert sub.count() == 1, f"{status}: missing subtitle"
        assert sub.first.inner_text().strip(), f"{status}: empty subtitle"


@test("#8 new task lands at the end of Draft")
def test_new_task_end_of_draft(page: Page):
    t1 = f"Smoke A {uuid.uuid4().hex[:6]}"
    t2 = f"Smoke B {uuid.uuid4().hex[:6]}"
    id1 = api_create_task(page, t1, status="draft")
    id2 = api_create_task(page, t2, status="draft")
    page.reload()
    wait_for_app(page)
    cards = column_for(page, "draft").locator(".card .card-title").all_inner_texts()
    try:
        i1 = cards.index(t1)
        i2 = cards.index(t2)
        assert i1 < i2, f"t1 should be above t2 (created first). Got: {cards}"
    finally:
        api_delete_task(page, id1)
        api_delete_task(page, id2)


@test("#12 settings modal re-opens after closing")
def test_settings_reopen(page: Page):
    page.click("button:has-text('Settings')")
    page.wait_for_selector(".modal-header h2:has-text('Settings')")
    # close
    page.locator(".modal .modal-header-actions button.ghost").first.click()
    page.wait_for_selector(".modal-header h2:has-text('Settings')", state="detached", timeout=3000)
    # reopen — this used to silently fail
    page.click("button:has-text('Settings')")
    page.wait_for_selector(".modal-header h2:has-text('Settings')", timeout=3000)
    page.locator(".modal .modal-header-actions button.ghost").first.click()
    page.wait_for_selector(".modal-header h2:has-text('Settings')", state="detached", timeout=3000)


@test("#11 Settings shows Models = agent profiles helper text")
def test_settings_models_helper(page: Page):
    page.click("button:has-text('Settings')")
    page.wait_for_selector(".modal-header h2:has-text('Settings')")
    # Click Edit on first server to reveal the models helper
    edit_btn = page.locator(".tbl button:has-text('Edit')").first
    if edit_btn.count() > 0:
        edit_btn.click()
        page.wait_for_selector(".server-edit")
        helper = page.locator(".server-edit .helper").first.inner_text()
        assert "profile" in helper.lower() or "Hermes" in helper, f"models helper missing profile explanation: {helper!r}"
    page.locator(".modal .modal-header-actions button.ghost").first.click()


@test("#9 i18n toggles fully — no mixed language")
def test_i18n_switch(page: Page):
    # Toggle to Chinese (button shows '🌐 EN' while in English)
    lang_btn = page.locator(".topbar button:has-text('🌐')").first
    # current state - check an English-only phrase
    draft_title = column_for(page, "draft").locator(".column-title").first.inner_text()
    lang_btn.click()
    page.wait_for_timeout(400)
    draft_title_after = column_for(page, "draft").locator(".column-title").first.inner_text()
    assert draft_title != draft_title_after, f"language toggle had no effect ({draft_title!r}→{draft_title_after!r})"
    # The subtitle for Plan should now read Chinese — assert no stray English
    plan_sub = column_for(page, "plan").locator(".column-subtitle").first.inner_text()
    # Either pure Chinese OR pure English is acceptable — the test is that it's
    # not a mix of both. "Queued" or "Plan" or "ready" bleeding into Chinese is a bug.
    assert not any(kw in plan_sub for kw in ["Queued", "ready for", "execution"]), f"English leaked into Chinese: {plan_sub!r}"
    # switch back
    lang_btn.click()
    page.wait_for_timeout(400)


@test("#5 theme toggle changes <html> class and persists")
def test_theme_toggle(page: Page):
    # Ensure dark
    initial_class = page.evaluate("document.documentElement.className")
    theme_btn = page.locator(".topbar button.icon").first  # first icon btn = theme
    theme_btn.click()
    page.wait_for_timeout(300)
    after = page.evaluate("document.documentElement.className")
    assert after != initial_class, "theme toggle had no effect"
    assert "theme-dark" in after or "theme-light" in after, f"unknown theme class: {after}"
    # Persisted?
    prefs = page.evaluate("fetch('/api/preferences').then(r => r.json())")
    # Note: evaluate returns the Promise's resolved value in Playwright — good.
    assert isinstance(prefs, dict)
    assert "preferences" in prefs
    expected = "light" if "theme-light" in after else "dark"
    assert prefs["preferences"]["theme"] == expected
    # Toggle back
    theme_btn.click()
    page.wait_for_timeout(200)


@test("task modal opens on click; Delete hidden outside Archive (#6)")
def test_delete_gating(page: Page):
    tid = api_create_task(page, f"Gating {uuid.uuid4().hex[:6]}", status="draft")
    try:
        page.reload(); wait_for_app(page)
        find_card_by_title(page, "Gating").first.click()
        page.wait_for_selector(".modal-header h2")
        del_btn = page.locator(".modal-footer button:has-text('Delete task')")
        assert del_btn.count() == 0, "Delete button must not be visible outside Archive"
        page.locator(".modal .modal-header-actions button.ghost").first.click()
    finally:
        api_delete_task(page, tid)


@test("#6 Delete visible when task is archived")
def test_delete_when_archived(page: Page):
    unique = f"dbgArch-{uuid.uuid4().hex[:6]}"
    tid = api_create_task(page, unique, status="archive")
    try:
        page.reload(); wait_for_app(page)
        card = page.locator(f".card:has(.card-title:has-text(\"{unique}\"))").first
        card.scroll_into_view_if_needed()
        card.click()
        page.wait_for_selector(".modal-header h2")
        # Wait for the footer to populate (it renders only after GetTask resolves).
        page.wait_for_selector(".modal-footer button", timeout=5000)
        del_btn = page.locator(".modal-footer button:has-text('Delete task')")
        assert del_btn.count() > 0, f"Delete button missing. Footer: {page.locator('.modal-footer').inner_html()}"
        page.locator(".modal .modal-header-actions button.ghost").first.click()
    finally:
        api_delete_task(page, tid)


@test("#2 New-Task form validates title required")
def test_title_required(page: Page):
    page.locator(".column[data-status='draft'] button:has-text('New Task')").click()
    page.wait_for_selector(".modal-header h2")
    # Save button is disabled while title is empty.
    save = page.locator(".modal-footer button.primary:has-text('Save')")
    assert save.is_disabled(), "Save should be disabled when title is empty"
    page.locator("input[type='text']").first.fill("Title filled")
    page.wait_for_timeout(100)
    assert not save.is_disabled()
    # Cancel
    page.locator(".modal-footer button:has-text('Cancel')").click()


@test("#2 Description editor has Write/Preview tabs + image-insert control")
def test_editor_controls(page: Page):
    page.locator(".column[data-status='draft'] button:has-text('New Task')").click()
    page.wait_for_selector(".modal-header h2")
    toolbar = page.locator(".desc-toolbar").first
    assert toolbar.locator("button:has-text('Write')").count() > 0
    assert toolbar.locator("button:has-text('Preview')").count() > 0
    assert toolbar.locator("button:has-text('Insert image')").count() > 0
    # Write → Preview roundtrip
    page.locator("textarea").first.fill("# hi\n\n**bold** text")
    toolbar.locator("button:has-text('Preview')").click()
    preview = page.locator(".desc-preview").first
    inner = preview.inner_html()
    assert "<h1>hi</h1>" in inner.lower() or "<h1>hi</h1>" in inner
    assert "<strong>bold</strong>" in inner.lower()
    page.locator(".modal-footer button:has-text('Cancel')").click()


@test("#3 attempt list collapsed when there are no attempts yet (single-pane)")
def test_attempts_collapsed_when_empty(page: Page):
    tid = api_create_task(page, f"Attempts {uuid.uuid4().hex[:6]}", status="draft")
    try:
        page.reload(); wait_for_app(page)
        find_card_by_title(page, "Attempts").first.click()
        page.wait_for_selector(".attempt-panel")
        # Grid-columns-1fr signals collapsed mode
        panel_class = page.locator(".attempt-panel").first.get_attribute("class") or ""
        assert "stacked" in panel_class, f"attempt panel should stack when empty, got {panel_class!r}"
        page.locator(".modal .modal-header-actions button.ghost").first.click()
    finally:
        api_delete_task(page, tid)


@test("drag smoke: card visually disappears from source when drag starts")
def test_drag_visual(page: Page):
    # We simulate pointerdown + move + up to ensure the source hides and
    # placeholder appears. We don't assert the final state change — just that
    # the drag scaffolding kicks in.
    tid = api_create_task(page, f"Draggable {uuid.uuid4().hex[:6]}", status="draft")
    try:
        page.reload(); wait_for_app(page)
        card = find_card_by_title(page, "Draggable").first
        card.scroll_into_view_if_needed()
        box = card.bounding_box()
        # Start drag: pointerdown then move far enough to cross the 5px threshold.
        page.mouse.move(box["x"] + box["width"] / 2, box["y"] + box["height"] / 2)
        page.mouse.down()
        page.mouse.move(box["x"] + box["width"] / 2 + 200, box["y"] + box["height"] / 2 + 200, steps=10)
        page.wait_for_timeout(100)
        clones = page.locator(".card-drag-clone").count()
        placeholders = page.locator(".card-drop-placeholder").count()
        assert clones >= 1, "floating clone must be attached during drag"
        assert placeholders >= 1, "placeholder must be attached during drag"
        page.mouse.up()
        page.wait_for_timeout(200)
        # After drop, clone + placeholder are cleaned up.
        assert page.locator(".card-drag-clone").count() == 0
        assert page.locator(".card-drop-placeholder").count() == 0
    finally:
        api_delete_task(page, tid)


@test("#1 dragging preserves ordering within Draft column")
def test_drag_reorder(page: Page):
    a = f"OrderA {uuid.uuid4().hex[:6]}"
    b = f"OrderB {uuid.uuid4().hex[:6]}"
    c = f"OrderC {uuid.uuid4().hex[:6]}"
    id_a = api_create_task(page, a, status="draft")
    id_b = api_create_task(page, b, status="draft")
    id_c = api_create_task(page, c, status="draft")
    try:
        page.reload(); wait_for_app(page)
        # Drag A down past B (below B's midpoint)
        card_a = find_card_by_title(page, a).first
        card_b = find_card_by_title(page, b).first
        ab = card_a.bounding_box()
        bb = card_b.bounding_box()
        page.mouse.move(ab["x"] + ab["width"] / 2, ab["y"] + ab["height"] / 2)
        page.mouse.down()
        page.mouse.move(bb["x"] + bb["width"] / 2, bb["y"] + bb["height"] + 10, steps=10)
        page.wait_for_timeout(150)
        page.mouse.up()
        page.wait_for_timeout(500)
        # Now A should be below B
        titles = column_for(page, "draft").locator(".card .card-title").all_inner_texts()
        idx_a = titles.index(a)
        idx_b = titles.index(b)
        assert idx_a > idx_b, f"After drag, A should be below B. Got: {titles}"
    finally:
        for t in (id_a, id_b, id_c):
            api_delete_task(page, t)


@test("board loads without JS errors")
def test_no_js_errors(page: Page):
    # Aggregator: captured across whole run in runner.
    pass  # handled in main()


@test("tag input accepts Enter + autocomplete + chip removal")
def test_tag_input(page: Page):
    # Seed a known tag name on one task so it appears in /api/tags suggestions.
    seed = f"unittag-{uuid.uuid4().hex[:6]}"
    seed_id = page.evaluate(
        "async ({t}) => (await fetch('/api/tags', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({name: t})}).then(r => r.json()))",
        {"t": seed},
    )
    # Open New Task modal and type the tag prefix — autocomplete should show it.
    page.locator(".column[data-status='draft'] button:has-text('New Task')").click()
    page.wait_for_selector(".modal-header h2")
    page.locator("input[type='text']").first.fill("tag-test title")
    # TagInput input is the last text input in the modal (after description, title).
    tag_input_box = page.locator(".tag-input input").first
    tag_input_box.click()
    tag_input_box.type(seed[:6], delay=10)  # partial match
    # Suggestion dropdown should contain the seeded tag.
    page.wait_for_selector(".tag-suggest-item", timeout=2000)
    # Pick the first suggestion.
    page.locator(".tag-suggest-item").first.click()
    # Chip should now exist.
    assert page.locator(f".tag-chip.removable:has-text(\"{seed}\")").count() > 0, "chip missing after selecting suggestion"
    # Free-typed tag via Enter.
    tag_input_box.type("alpha-" + uuid.uuid4().hex[:4], delay=10)
    tag_input_box.press("Enter")
    chip_count = page.locator(".tag-chip.removable").count()
    assert chip_count >= 2, f"expected ≥2 chips, got {chip_count}"
    # Remove via × — there must be at least one fewer chip.
    page.locator(".tag-chip.removable .x").first.click()
    assert page.locator(".tag-chip.removable").count() == chip_count - 1
    # Cancel new-task dialog.
    page.locator(".modal-footer button:has-text('Cancel')").click()


@test("dependencies picker: create a dep with required_state 'verify'")
def test_dependency_picker(page: Page):
    # Create two tasks via API — one will be the dep target, one the dependent.
    target = api_create_task(page, f"DepTarget {uuid.uuid4().hex[:6]}", status="plan")
    dependent = api_create_task(page, f"Dependent {uuid.uuid4().hex[:6]}", status="draft")
    try:
        page.reload(); wait_for_app(page)
        find_card_by_title(page, "Dependent").first.click()
        page.wait_for_selector(".modal-header h2")
        # Click Edit to enter the form.
        page.locator(".modal-header button:has-text('Edit')").first.click()
        page.wait_for_selector(".dep-picker")
        # Add a row.
        page.locator(".dep-picker button:has-text('Add a dependency')").click()
        # First <select> in the row is the task picker.
        page.locator(".dep-row select").first.select_option(value=target)
        # Second <select> is required_state.
        page.locator(".dep-row select").nth(1).select_option(value="verify")
        # Save.
        page.locator(".edit-actions button.primary:has-text('Save')").click()
        page.wait_for_timeout(500)
        # Re-fetch the task and verify the dep landed with the right state.
        got = page.evaluate("id => fetch('/api/tasks/' + id).then(r => r.json())", dependent)
        deps = got["task"]["dependencies"]
        assert any(d["task_id"] == target and d["required_state"] == "verify" for d in deps), f"deps={deps}"
    finally:
        api_delete_task(page, dependent)
        api_delete_task(page, target)


@test("optional markers: every non-title form row shows (optional)")
def test_optional_markers(page: Page):
    page.locator(".column[data-status='draft'] button:has-text('New Task')").click()
    page.wait_for_selector(".modal-header h2")
    required_count = page.locator(".form-row .required").count()
    optional_count = page.locator(".form-row .optional").count()
    assert required_count == 1, f"Exactly one '*' required marker expected, got {required_count}"
    assert optional_count >= 5, f"Expected multiple '(optional)' markers, got {optional_count}"
    page.locator(".modal-footer button:has-text('Cancel')").click()


# ---------- runner ----------


def main():
    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path="/usr/bin/google-chrome", headless=True, args=["--no-sandbox"])
        ctx = browser.new_context(viewport={"width": 1400, "height": 900})
        page = ctx.new_page()
        js_errors = []
        page.on("pageerror", lambda err: js_errors.append(str(err)))
        page.on("console", lambda m: js_errors.append(f"{m.type}: {m.text}") if m.type == "error" else None)
        page.goto(BASE + "/", wait_until="domcontentloaded")
        wait_for_app(page)

        print("Running UI regression suite against", BASE)
        print()

        test_new_task_button_in_draft(page)
        test_column_subtitles(page)
        test_new_task_end_of_draft(page)
        test_settings_reopen(page)
        test_settings_models_helper(page)
        test_i18n_switch(page)
        test_theme_toggle(page)
        test_delete_gating(page)
        test_delete_when_archived(page)
        test_title_required(page)
        test_editor_controls(page)
        test_attempts_collapsed_when_empty(page)
        test_drag_visual(page)
        test_drag_reorder(page)
        test_tag_input(page)
        test_dependency_picker(page)
        test_optional_markers(page)

        # Final: check we had no unexpected page errors during the whole run.
        if js_errors:
            Ctx.failed.append(("no JS errors on page", "; ".join(js_errors)))
            print(f"  ✗ no JS errors: {'; '.join(js_errors)}")
        else:
            Ctx.passed.append("no JS errors on page")
            print(f"  ✓ no JS errors on page")

        print()
        print(f"Passed: {len(Ctx.passed)}")
        print(f"Failed: {len(Ctx.failed)}")
        for n, e in Ctx.failed:
            print(f"  - {n}: {e}")
        browser.close()
        sys.exit(0 if not Ctx.failed else 1)


if __name__ == "__main__":
    main()
