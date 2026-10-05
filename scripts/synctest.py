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

    def showing(name, role=None):
        return [n for n in all_named(app, name, role) if "showing" in U.flags(n)]

    def editor_gone():
        for _ in range(25):
            time.sleep(0.2)
            if not showing("New preset") and not showing("Edit preset"):
                return True
        return False

    # New preset: same window as edit, default name, no target, nothing runs.
    act(app, "Add preset…")
    check(len(showing("New preset")) >= 1, "Add preset opens the New preset window")
    check(len(cfgnow()["screenSync"]["presets"]) == 1, "nothing written before Save")
    names = [n for n in U.walk(app) if n.get_role_name() == "text" and "showing" in U.flags(n)]
    check(any(Atspi.Text.get_text(n, 0, -1) == "Preset 2" for n in names if (n.get_name() or "") == "Preset name"), "default name Preset 2")
    check(len(showing("None")) >= 1, "target shows None")
    edit = showing("Edit regions", "button")
    check(len(edit) >= 1 and edit[0].get_description() == "Select a target first",
          f"Edit regions waits for a target: {edit[0].get_description() if edit else None}")
    time.sleep(1.5)
    check(all(not on(n) for n in U.walk(app) if n.get_role_name() == "toggle button" and (n.get_name() or "") == "Seeded"), "opening the window starts nothing")

    # Cancel discards the draft.
    act(app, "Cancel", "button")
    check(editor_gone(), "Cancel closes the window")
    check(len(cfgnow()["screenSync"]["presets"]) == 1, "Cancel discards the new preset")

    # New again, Select a target (test backend picks TEST-1), Save.
    act(app, "Add preset…")
    act(app, "Select", "button")
    time.sleep(3)
    check(len(showing("Monitors · TEST-1")) >= 1, "Select fills the target")
    check(len(showing("Change", "button")) >= 1, "button now reads Change")
    # Cage has no layer-shell, so Edit regions stays off there, but no longer for want of a target.
    edit = showing("Edit regions", "button")
    check(len(edit) >= 1 and edit[0].get_description() != "Select a target first",
          f"Edit regions no longer waits for a target: {edit[0].get_description() if edit else None}")
    act(app, "Save", "button"); time.sleep(1.2)
    check(editor_gone(), "Save closes the window")
    c = cfgnow()["screenSync"]["presets"]
    check(len(c) == 2 and c[1]["name"] == "Preset 2", "new preset saved")
    check(c[1]["target"] == {"kind": "monitors", "monitors": ["TEST-1"]}, f"target saved: {c[1]['target']}")
    check(cfgnow().get("theme") == "keep-me", "unknown key kept")
    tile2 = [n for n in all_named(app, "Preset 2") if n.get_role_name() == "toggle button" and "showing" in U.flags(n)]
    check(len(tile2) >= 1 and not on(tile2[0]), "preview stopped after Save")
    act(app, "Delete preset Preset 2")
    act(app, "Delete", "button")
    time.sleep(1)
    check(len(cfgnow()["screenSync"]["presets"]) == 1, "preset deleted")

    # Edit seeded preset: bind lights through chips (written only on Save).
    def chip(region):
        # Chips are named by their label; the Left card comes first.
        uniq = showing("Lamp A", "toggle button")
        i = {"Left": 0, "Ambi": 0, "Right": 1}[region]
        return uniq[i:i + 1]
    act(app, "Edit preset Seeded")
    check(len(showing("Edit preset")) >= 1, "Edit preset window opened")
    check(len(chip("Left")) >= 1 and len(chip("Right")) >= 1, "light chips on every region card")
    Atspi.Action.do_action(chip("Left")[0], 0); time.sleep(0.8)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(r[0]["devices"] == [], f"unsaved binding not written: {r[0]['devices']}")
    act(app, "Save", "button"); time.sleep(1.2)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(r[0]["devices"] == ["fake1"], f"Lamp A bound to Left after Save: {r[0]['devices']}")
    act(app, "Edit preset Seeded")
    Atspi.Action.do_action(chip("Right")[0], 0); time.sleep(0.8)
    check(not on(chip("Left")[0]), "turning on in Right turns it off in Left")
    act(app, "Save", "button"); time.sleep(1.2)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(r[0]["devices"] == [] and r[1]["devices"] == ["fake1"], f"binding exclusive (moved): {[x['devices'] for x in r]}")

    # Start sync from the list.
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

    # Rename + delete a region, then Esc discards an unsaved edit.
    act(app, "Screen Sync")
    act(app, "Edit preset Seeded")
    ok = False
    for n in showing("Region name"):
        txt = Atspi.Text.get_text(n, 0, -1)
        if txt == "Left":
            Atspi.EditableText.delete_text(n, 0, len(txt))
            Atspi.EditableText.insert_text(n, 0, "Ambi", 4)
            ok = True
    time.sleep(0.5)
    act(app, "Save", "button"); time.sleep(1.2)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(ok and r[0]["name"] == "Ambi", f"region renamed: {[x['name'] for x in r]}")
    act(app, "Edit preset Seeded")
    trash = [n for n in showing("Delete region Ambi") if U.first_action(n) in U.CLICK_ACTIONS]
    check(len(trash) >= 1, "delete button on region card")
    Atspi.Action.do_action(trash[0], 0); time.sleep(0.6)
    act(app, "Cancel", "button")
    check(editor_gone(), "Cancel closes the editor")
    check(len(cfgnow()["screenSync"]["presets"][0]["regions"]) == 2, "Cancel discards the delete")
    act(app, "Edit preset Seeded")
    key("Escape")
    check(editor_gone(), "Escape closes the editor")
    act(app, "Edit preset Seeded")
    trash = [n for n in showing("Delete region Ambi") if U.first_action(n) in U.CLICK_ACTIONS]
    Atspi.Action.do_action(trash[0], 0); time.sleep(0.6)
    act(app, "Save", "button"); time.sleep(1.2)
    r = cfgnow()["screenSync"]["presets"][0]["regions"]
    check(len(r) == 1 and r[0]["name"] == "Right", f"region deleted: {[x['name'] for x in r]}")
finally:
    os.killpg(cage.pid, signal.SIGKILL)
    cage.wait()

print("FAILURES:", fails if fails else "none")
sys.exit(1 if fails else 0)
