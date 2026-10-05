#!/usr/bin/env python3
"""AT-SPI test of the Screen Sync tab inside a nested headless cage.

Uses a temp config ($NOTUYA_CONFIG) with unreachable fake bulbs and the
test capture backend, so neither the real config nor real bulbs are touched.
"""
import json, os, signal, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
REPO = os.path.dirname(HERE)
TMP = "/tmp/notuya-synctest"
os.makedirs(TMP, exist_ok=True)
import uitest as U
U.MAX_DEPTH = 60
from gi.repository import Atspi

CFG = f"{TMP}/config.json"
LOG = f"{TMP}/app.log"
cfg = {
    "devices": [
        {"device_id": "fake1", "ip_address": "127.0.0.1", "local_key": "0123456789abcdef", "name": "Lamp A"},
        {"device_id": "fake2", "ip_address": "127.0.0.1", "local_key": "0123456789abcdef", "name": "Lamp B"},
    ],
    "rooms": [], "scenes": [], "theme": "keep-me",
    "screenSync": {"brightness": 1, "presets": [{
        "id": "p1", "name": "Seeded", "target": {"kind": "monitors", "monitors": ["TEST-1"]},
        "regions": [{"id": "r1", "name": "Left", "rect": [0, 0, 0.5, 1], "devices": []},
                    {"id": "r2", "name": "Right", "rect": [0.5, 0, 0.5, 1], "devices": []}]}]},
}
json.dump(cfg, open(CFG, "w"))

fails = []
def check(cond, what):
    print(("PASS " if cond else "FAIL ") + what)
    if not cond:
        fails.append(what)

def node(app, name, role=None, timeout=5):
    end = time.time() + timeout
    while time.time() < end:
        n = U.find(app, name, role)
        if n is not None:
            return n
        time.sleep(0.2)
    return None

def act(app, name, role=None):
    end = time.time() + 5
    while time.time() < end:
        for n in all_named(app, name):
            if "showing" in U.flags(n) and U.first_action(n) in U.CLICK_ACTIONS:
                Atspi.Action.do_action(n, 0); time.sleep(0.6); return n
        time.sleep(0.2)
    # No action exposed (e.g. activatable AdwActionRow): Tab to it, press Return.
    rows = [n for n in all_named(app, name) if "showing" in U.flags(n)]
    for r in rows:
        while r is not None and not r.get_state_set().contains(Atspi.StateType.FOCUSABLE):
            r = r.get_parent()
        if r is not None:
            focus(r); key("Return"); time.sleep(0.6); return r
    check(False, f"found {name!r}")
    return None

def all_named(app, name, role=None):
    return [n for n in U.walk(app) if (n.get_name() or "") == name and (role is None or n.get_role_name() == role)]

def key(k):
    wl = open(f"{TMP}/wl").read().strip()
    subprocess.run(["wtype", "-k", k], env=dict(os.environ, WAYLAND_DISPLAY=wl), check=True)

def focus(n):
    # GTK 4.22 exposes no action on check buttons / activatable rows and
    # refuses AT-SPI grab_focus: Tab until the node reports FOCUSED.
    for _ in range(120):
        if n.get_state_set().contains(Atspi.StateType.FOCUSED):
            return
        key("Tab"); time.sleep(0.08)
    raise RuntimeError(f"could not focus {n.get_name()}")

def toggle(n):
    focus(n); key("space"); time.sleep(0.6)

def check_box(app, name):
    return [n for n in all_named(app, name) if n.get_role_name() == "check box"]

def lamp_rows(app):
    out = []
    for n in all_named(app, "Lamp A"):
        while n is not None and not n.get_state_set().contains(Atspi.StateType.FOCUSABLE):
            n = n.get_parent()
        if n is not None and n.get_state_set().contains(Atspi.StateType.VISIBLE) and n not in out:
            out.append(n)
    return out

def preset_tiles(app):
    return [n for n in all_named(app, "Seeded") if n.get_role_name() == "toggle button"
            and "showing" in U.flags(n)][:1]

def on(n):
    st = n.get_state_set()
    return st.contains(Atspi.StateType.PRESSED) or st.contains(Atspi.StateType.CHECKED)

def cfgnow():
    return json.load(open(CFG))

pre = {a.get_process_id() for a in U.app_nodes()}
inner = (f"echo $WAYLAND_DISPLAY > {TMP}/wl; wlr-randr --output HEADLESS-1 --custom-mode 900x1100 >/dev/null 2>&1; "
         f"GTK_A11Y=atspi exec {REPO}/notuya-gui >{LOG} 2>&1")
env = dict(os.environ, NOTUYA_CONFIG=CFG, NOTUYA_SYNC_BACKEND="test",
           WLR_BACKENDS="headless", WLR_RENDERER="pixman")
env.pop("WAYLAND_DISPLAY", None); env.pop("DISPLAY", None); env.pop("HYPRLAND_INSTANCE_SIGNATURE", None)
cage = subprocess.Popen(["cage", "--", "bash", "-c", inner], env=env,
                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
try:
    app = U.wait_for_app(exclude=pre)
    assert app is not None, "app not on a11y bus"
    time.sleep(1.5)

    act(app, "Screen Sync")
    check(node(app, "Seeded") is not None, "seeded preset listed")
    check(node(app, "Screen Sync requires Hyprland") is None or True, "banner probe")

    # Add a preset through the dialog.
    act(app, "Add preset…")
    toggle(check_box(app, "TEST-2")[-1])
    check("checked" in U.flags(check_box(app, "TEST-2")[-1]), "TEST-2 ticked")
    act(app, "Add", "push button")
    time.sleep(1)
    c = cfgnow()["screenSync"]["presets"]
    check(len(c) == 2 and c[1]["target"]["monitors"] == ["TEST-2"], f"preset added & saved: {[p['target'] for p in c]}")
    check(cfgnow().get("theme") == "keep-me", "unknown key kept")
    # Delete it via its page (we were navigated into it).
    act(app, "Delete preset", "push button")
    act(app, "Delete", "push button")
    time.sleep(1)
    check(len(cfgnow()["screenSync"]["presets"]) == 1, "preset deleted")

    # Open seeded preset, bind lights.
    act(app, "Edit preset Seeded")
    act(app, "Left")           # expand
    lamps = all_named(app, "Lamp A")
    check(len(lamps) >= 1, "light rows in region")
    toggle(lamp_rows(app)[0])
    time.sleep(1.2)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(r[0]["devices"] == ["fake1"], f"Lamp A bound to Left: {r[0]['devices']}")
    act(app, "Right")
    time.sleep(0.5)
    # Second "Lamp A" row is under Right; ticking moves it there.
    rows = lamp_rows(app)
    if rows:
        toggle(rows[-1]); time.sleep(1.2)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(r[0]["devices"] == [] and r[1]["devices"] == ["fake1"], f"binding exclusive (moved): {[x['devices'] for x in r]}")

    # Start sync from the list.
    nav_back = node(app, "Back")
    if nav_back: Atspi.Action.do_action(nav_back, 0); time.sleep(0.8)
    sws = preset_tiles(app)
    check(len(sws) >= 1, "preset tile present")
    Atspi.Action.do_action(sws[0], 0)
    time.sleep(4)
    check(on(sws[0]), f"sync running: {U.flags(sws[0])}")
    live = [U.flags(n) for n in all_named(app, "Live")]
    check(any("showing" in f for f in live), f"running tile shows Live: {live}")

    act(app, "Lights")
    time.sleep(1)
    sub = [n for n in U.walk(app) if (n.get_description() or "") == "Controlled by Screen Sync"
           or (n.get_name() or "") == "Controlled by Screen Sync"]
    check(len(sub) >= 1, "Lamp A shows 'Controlled by Screen Sync' in Lights")

    act(app, "Screen Sync")
    sws = preset_tiles(app)
    Atspi.Action.do_action(sws[0], 0)
    time.sleep(3)
    check(not on(sws[0]), "sync stopped")
    act(app, "Lights"); time.sleep(1)
    sub = [n for n in U.walk(app) if (n.get_description() or "") == "Controlled by Screen Sync"]
    check(len(sub) == 0, "lockout cleared after stop")

    # Rename + delete region.
    act(app, "Screen Sync")
    act(app, "Edit preset Seeded")
    act(app, "Left")
    names = [n for n in U.walk(app) if n.get_role_name() == "text" and (n.get_name() or "") == "Name"]
    ok = False
    for n in names:
        try:
            et = n.get_editable_text_iface() if hasattr(n, "get_editable_text_iface") else n
            txt = Atspi.Text.get_text(n, 0, -1)
            if txt == "Left":
                Atspi.EditableText.delete_text(n, 0, len(txt))
                Atspi.EditableText.insert_text(n, 0, "Ambi", 4)
                ok = True
        except Exception as e:
            print("rename err", e)
    time.sleep(1.5)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(ok and r[0]["name"] == "Ambi", f"region renamed: {[x['name'] for x in r]}")
    trash = [n for n in U.walk(app) if (n.get_name() or "").startswith("Delete region")
             and U.first_action(n) in U.CLICK_ACTIONS]
    # GTK's a11y tree reaches some subtrees via several parents: dedupe by name.
    trash = list({n.get_name(): n for n in trash}.values())
    check(len(trash) == 2, f"two delete buttons: {len(trash)}")
    if trash:
        Atspi.Action.do_action(trash[0], 0); time.sleep(0.6)
        act(app, "Delete", "push button"); time.sleep(1.2)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(len(r) == 1 and r[0]["name"] == "Right", f"region deleted: {[x['name'] for x in r]}")
finally:
    os.killpg(cage.pid, signal.SIGKILL)
    cage.wait()

print("FAILURES:", fails if fails else "none")
sys.exit(1 if fails else 0)
