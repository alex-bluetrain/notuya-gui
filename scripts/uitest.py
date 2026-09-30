#!/usr/bin/env python3
"""AT-SPI harness for driving and verifying notuya-gui.

The correct, non-flaky way to test this GTK4/libadwaita app: query the live
accessibility tree over org.a11y.Bus, invoke each widget's own action (clicks
can't miss), and read real widget state for assertions. No synthetic mouse or
keyboard input, no pixel matching.

Full loop (all proven on Hyprland 0.56.2):

    python3 scripts/uitest.py launch          # start app on the a11y bus, wait for window
    python3 scripts/uitest.py dump            # list interactive widgets + state
    python3 scripts/uitest.py click White     # invoke a widget's own action
    python3 scripts/uitest.py state Colour White
    python3 scripts/uitest.py shot /tmp/x.png # capture the real window (grim, address-based)
    python3 scripts/uitest.py close           # clean exit (no zombie)
    python3 scripts/uitest.py cage /tmp/x.png White  # full-height headless capture
                                              # (nested cage, pixman renderer;
                                              #  extra args = widgets to click first)

State/behaviour commands (dump, click, state) need the app on the a11y bus, so
launch it with launch (or with GTK_A11Y=atspi yourself). The window commands
(launch, shot, close) talk to Hyprland via hyprctl + grim and resolve the
window by PID/address, never by guessed geometry.

Exit status is non-zero on failure, so commands compose in shell chains.
"""

import json
import os
import signal
import subprocess
import sys
import time

import gi

gi.require_version("Atspi", "2.0")
from gi.repository import Atspi  # noqa: E402

APP_NAME = "notuya-gui"
APP_CLASS = "ar.averstraeten.tuyawheel.app"
BINARY = "./notuya-gui"
PIDFILE = "/tmp/notuya-gui.uitest.pid"
LOGFILE = "/tmp/notuya-gui.uitest.log"
MAX_DEPTH = 25
INTERACTIVE = {
    "page tab",
    "check box",
    "switch",
    "radio button",
    "toggle button",
    "push button",
    "slider",
    "combo box",
    "entry",
}
STATE_FLAGS = [
    (Atspi.StateType.CHECKED, "checked"),
    (Atspi.StateType.ACTIVE, "active"),
    (Atspi.StateType.SELECTED, "selected"),
    (Atspi.StateType.SHOWING, "showing"),
]


# ---------------------------------------------------------------------------
# Hyprland helpers (window resolved by PID/address, never guessed geometry)
# ---------------------------------------------------------------------------
def hypr_clients():
    out = subprocess.run(
        ["hyprctl", "clients", "-j"], capture_output=True, text=True, check=True
    ).stdout
    return json.loads(out)


def window_for_pid(pid):
    for c in hypr_clients():
        if c.get("pid") == pid:
            return c
    return None


def window_for_class():
    for c in hypr_clients():
        if c.get("class") == APP_CLASS:
            return c
    return None


def wait_for_window(pid, timeout=12.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        w = window_for_pid(pid)
        if w and w.get("address"):
            return w
        time.sleep(0.2)
    return None


# ---------------------------------------------------------------------------
# AT-SPI helpers
# ---------------------------------------------------------------------------
def app_nodes():
    """All notuya-gui application nodes on the a11y bus."""
    desktop = Atspi.get_desktop(0)
    out = []
    for i in range(desktop.get_child_count()):
        a = desktop.get_child_at_index(i)
        if a and (a.get_name() or "") == APP_NAME:
            out.append(a)
    return out


def app_root(exclude=()):
    """The notuya-gui app node to drive, or None.

    Prefers the instance started by `launch` (pidfile); otherwise the first
    one whose pid is not in `exclude`. Other notuya-gui instances (e.g. the
    user's own) are on the same bus, so matching by name alone is ambiguous.
    """
    nodes = [a for a in app_nodes() if a.get_process_id() not in exclude]
    want = read_pid()
    for a in nodes:
        if a.get_process_id() == want:
            return a
    return nodes[0] if nodes else None


def wait_for_app(exclude=(), timeout=12.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        a = app_root(exclude)
        if a is not None:
            return a
        time.sleep(0.2)
    return None


def walk(node, depth=0):
    if node is None or depth > MAX_DEPTH:
        return
    yield node
    for i in range(node.get_child_count()):
        yield from walk(node.get_child_at_index(i), depth + 1)


def flags(node):
    st = node.get_state_set()
    return [label for s, label in STATE_FLAGS if st.contains(s)]


def n_actions(node):
    try:
        return Atspi.Action.get_n_actions(node)
    except Exception:
        return 0


# Action names GTK exposes for "activate this widget" (buttons, tabs, toggles).
CLICK_ACTIONS = {"click", "press", "activate", "toggle"}


def first_action(node):
    try:
        return Atspi.Action.get_action_name(node, 0)
    except Exception:
        return None


def find(app, name, role=None):
    """Widget matching name (and role, if given).

    Several nodes can share a name: a button and its tooltip, or the window
    frame, whose title follows the visible tab and whose first action is
    window.close. Prefer a node whose first action is a real click; failing
    that, return the first match so state reads still work.
    """
    fallback = None
    for n in walk(app):
        if (n.get_name() or "") != name:
            continue
        if role is not None and n.get_role_name() != role:
            continue
        if first_action(n) in CLICK_ACTIONS:
            return n
        fallback = fallback or n
    return fallback


# ---------------------------------------------------------------------------
# Commands that talk to the compositor / process (no a11y app node needed)
# ---------------------------------------------------------------------------
def cmd_launch(_app, _args):
    """Start the app on the a11y bus, detached, and wait for its window."""
    if not os.path.exists(BINARY):
        print(f"launch: {BINARY} not found (build it first)", file=sys.stderr)
        return 1
    env = dict(os.environ, GTK_A11Y="atspi")
    log = open(LOGFILE, "wb")
    proc = subprocess.Popen(
        [BINARY],
        env=env,
        stdout=log,
        stderr=log,
        stdin=subprocess.DEVNULL,
        start_new_session=True,  # detach: own session, survives this script
    )
    with open(PIDFILE, "w") as f:
        f.write(str(proc.pid))
    w = wait_for_window(proc.pid)
    if w is None:
        print(f"launch: window never appeared (pid {proc.pid}, see {LOGFILE})",
              file=sys.stderr)
        return 1
    print(f"launched pid={proc.pid} addr={w['address']} title={w.get('title')!r}")
    return 0


def read_pid():
    """Pid recorded by `launch`, or None."""
    try:
        with open(PIDFILE) as f:
            return int(f.read().strip())
    except (FileNotFoundError, ValueError):
        return None


def resolve_target_window():
    """Window dict for the launched pid, else any app-class window."""
    pid = read_pid()
    if pid is not None:
        w = window_for_pid(pid)
        if w:
            return w, pid
    w = window_for_class()
    return (w, w["pid"]) if w else (None, None)


def cmd_shot(_app, args):
    """grim-capture the real window by its Hyprland geometry (address-resolved)."""
    path = args[0] if args else "/tmp/notuya-gui.png"
    w, _pid = resolve_target_window()
    if w is None:
        print("shot: no notuya-gui window found", file=sys.stderr)
        return 1
    x, y = w["at"]
    cw, ch = w["size"]
    geo = f"{x},{y} {cw}x{ch}"
    r = subprocess.run(["grim", "-g", geo, path], capture_output=True, text=True)
    if r.returncode != 0:
        print(f"shot: grim failed: {r.stderr.strip()}", file=sys.stderr)
        return 1
    print(f"saved {path} ({geo})")
    return 0


def cmd_close(_app, _args):
    """Clean exit via Hyprland's Lua dispatcher (legacy closewindow is dead on 0.56)."""
    w, pid = resolve_target_window()
    if w is None:
        print("close: no notuya-gui window found (already gone?)")
        return 0
    addr = w["address"]
    subprocess.run(
        ["hyprctl", "dispatch", f'hl.dsp.window.close({{ window = "address:{addr}" }})'],
        capture_output=True, text=True,
    )
    # The fixed app quits when its window closes; wait for the process to reap.
    if pid:
        for _ in range(30):
            if not _pid_alive(pid):
                print(f"closed {addr} -> pid {pid} reaped")
                try:
                    os.remove(PIDFILE)
                except OSError:
                    pass
                return 0
            time.sleep(0.1)
        print(f"close: dispatched but pid {pid} still alive", file=sys.stderr)
        return 1
    print(f"closed {addr}")
    return 0


def _pid_alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


CAGE_MARKER = "/tmp/notuya-gui.uitest.cage-shoot"
CAGE_SIZE = "800x1100"
CAGE_SETTLE = 6.0  # seconds: covers a bulb command plus the read-back repaint


def cmd_cage(_app, args):
    """Full-height capture in a nested headless cage compositor.

    usage: uitest.py cage [path] [widget-name...]

    Runs cage with the wlroots headless backend and the pixman (software)
    renderer -- the GPU path renders black on NVIDIA headless outputs. The
    output is sized 800x1100 via wlr-randr so nothing is clipped. The app
    inside cage joins the session a11y bus, so any widget names given are
    clicked via AT-SPI before the frame is grabbed.

    Instances already on the a11y bus (e.g. the user's own) are ignored, so
    clicks only ever hit the app inside cage. Cage is always torn down, even
    if a click fails.
    """
    preexisting = {a.get_process_id() for a in app_nodes()}
    path = args[0] if args and args[0].endswith(".png") else "/tmp/notuya-gui-cage.png"
    clicks = args[1:] if args and args[0].endswith(".png") else args
    try:
        os.remove(CAGE_MARKER)
    except OSError:
        pass
    inner = (
        f"wlr-randr --output HEADLESS-1 --custom-mode {CAGE_SIZE} >/dev/null 2>&1; "
        f"GTK_A11Y=atspi {os.path.abspath(BINARY)} >{LOGFILE} 2>&1 & APP=$!; "
        f"while [ ! -f {CAGE_MARKER} ]; do sleep 0.2; done; "
        f"grim {path} && echo GRIM_OK; kill $APP"
    )
    env = dict(os.environ)
    env.pop("WAYLAND_DISPLAY", None)
    env.pop("DISPLAY", None)
    env.update(WLR_BACKENDS="headless", WLR_RENDERER="pixman")
    cage = subprocess.Popen(
        ["cage", "--", "bash", "-c", inner],
        env=env, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
        start_new_session=True,  # own process group: teardown reaps bash + app too
    )
    out = ""
    try:
        app = wait_for_app(exclude=preexisting)
        if app is None:
            print("cage: app never appeared on the a11y bus", file=sys.stderr)
            return 1
        time.sleep(1.0)  # let the first frame paint
        for name in clicks:
            n = find(app, name)
            if n is None:
                print(f"cage: widget not found: {name!r}", file=sys.stderr)
            else:
                before = flags(n)
                Atspi.Action.do_action(n, 0)
                time.sleep(0.4)
                after = flags(n)
                note = "" if before != after else "  (UNCHANGED)"
                print(f"clicked {name!r} -> [{','.join(after)}]{note}")
        # Commands talk to real bulbs and repaint from the reply, so the grab
        # has to outwait a device round-trip, not just a frame.
        time.sleep(CAGE_SETTLE)
        open(CAGE_MARKER, "w").close()
        out, _ = cage.communicate(timeout=20)
    except subprocess.TimeoutExpired:
        print("cage: timed out", file=sys.stderr)
        return 1
    finally:
        if cage.poll() is None:
            try:
                os.killpg(cage.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            cage.wait()
        try:
            os.remove(CAGE_MARKER)
        except OSError:
            pass
    # The app's own stderr is the only place device errors surface; a silent
    # capture that looks fine is exactly how a failing command hides.
    try:
        with open(LOGFILE) as f:
            app_log = [l for l in f.read().splitlines() if "notuya-gui:" in l]
        for line in app_log[-10:]:
            print(f"app: {line}")
    except OSError:
        pass
    if "GRIM_OK" not in (out or "") or not os.path.exists(path):
        print("cage: capture failed (no GRIM_OK / missing file)", file=sys.stderr)
        return 1
    print(f"saved {path} ({CAGE_SIZE})")
    return 0


# ---------------------------------------------------------------------------
# Commands that need the app on the a11y bus
# ---------------------------------------------------------------------------
def cmd_dump(app, _args):
    for n in walk(app):
        role = n.get_role_name()
        if role not in INTERACTIVE:
            continue
        name = n.get_name() or ""
        fl = ",".join(flags(n))
        print(f"{role:14} {name!r:22} actions={n_actions(n)} [{fl}]")
    return 0


def cmd_click(app, args):
    if not args:
        print("usage: uitest.py click <name> [name...]", file=sys.stderr)
        return 2
    rc = 0
    for name in args:
        n = find(app, name)
        if n is None:
            print(f"click: not found: {name!r}", file=sys.stderr)
            rc = 1
            continue
        Atspi.Action.do_action(n, 0)
        time.sleep(0.4)
        print(f"clicked {name!r} -> [{','.join(flags(n))}]")
    return rc


def cmd_state(app, args):
    if not args:
        print("usage: uitest.py state <name> [name...]", file=sys.stderr)
        return 2
    rc = 0
    for name in args:
        n = find(app, name)
        if n is None:
            print(f"state: not found: {name!r}", file=sys.stderr)
            rc = 1
            continue
        print(f"{name!r}: [{','.join(flags(n))}]")
    return rc


# launch/shot/close/cage don't need a pre-existing a11y app node; the rest do.
NO_APP = {"launch", "shot", "close", "cage"}
COMMANDS = {
    "launch": cmd_launch,
    "shot": cmd_shot,
    "close": cmd_close,
    "cage": cmd_cage,
    "dump": cmd_dump,
    "click": cmd_click,
    "state": cmd_state,
}


def main(argv):
    if len(argv) < 2 or argv[1] not in COMMANDS:
        print(__doc__)
        print("commands:", ", ".join(COMMANDS))
        return 2
    cmd = argv[1]
    Atspi.init()
    if cmd in NO_APP:
        return COMMANDS[cmd](None, argv[2:])
    app = wait_for_app()
    if app is None:
        print(
            f"error: {APP_NAME!r} not on the a11y bus. "
            "Run 'uitest.py launch' (or start it with GTK_A11Y=atspi) first.",
            file=sys.stderr,
        )
        return 3
    return COMMANDS[cmd](app, argv[2:])


if __name__ == "__main__":
    sys.exit(main(sys.argv))
