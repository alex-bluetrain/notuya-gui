#!/usr/bin/env python3
"""AT-SPI harness for driving and verifying notuya-gui.

The correct, non-flaky way to test this GTK4/libadwaita app: query the live
accessibility tree over org.a11y.Bus, invoke each widget's own action (clicks
can't miss), and read real widget state for assertions. No screenshots, no
synthetic mouse/keyboard input.

Launch the app first with the a11y bridge on:

    GTK_A11Y=atspi setsid ./notuya-gui >/tmp/notuya.log 2>&1 < /dev/null &

Then:

    GTK_A11Y=atspi python3 scripts/uitest.py dump
    GTK_A11Y=atspi python3 scripts/uitest.py click White
    GTK_A11Y=atspi python3 scripts/uitest.py state Colour White

Exit status is non-zero when a target widget is not found, so it composes in
shell verification chains.
"""

import sys
import time

import gi

gi.require_version("Atspi", "2.0")
from gi.repository import Atspi  # noqa: E402

APP_NAME = "notuya-gui"
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


def app_root():
    """Return the notuya-gui application node, or None."""
    desktop = Atspi.get_desktop(0)
    for i in range(desktop.get_child_count()):
        a = desktop.get_child_at_index(i)
        if a and (a.get_name() or "") == APP_NAME:
            return a
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


def find(app, name, role=None):
    """First widget matching name (and role, if given)."""
    for n in walk(app):
        if (n.get_name() or "") != name:
            continue
        if role is not None and n.get_role_name() != role:
            continue
        return n
    return None


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


COMMANDS = {"dump": cmd_dump, "click": cmd_click, "state": cmd_state}


def main(argv):
    if len(argv) < 2 or argv[1] not in COMMANDS:
        print(__doc__)
        print("commands:", ", ".join(COMMANDS))
        return 2
    Atspi.init()
    app = app_root()
    if app is None:
        print(
            f"error: {APP_NAME!r} not on the a11y bus. "
            "Launch it with GTK_A11Y=atspi and try again.",
            file=sys.stderr,
        )
        return 3
    return COMMANDS[argv[1]](app, argv[2:])


if __name__ == "__main__":
    sys.exit(main(sys.argv))
