#!/usr/bin/env python3
# session_list.py — dedup helper for phase15 /new polling.
# Replaces 8 inline python3 -c blocks (4x CAND + 4x STILL) in phase15_abort_not_completed.sh.
# Usage: curl .../session/list | python3 "$SCRIPT_DIR/session_list.py" cand <pane> <before>
#        curl .../session/list | python3 "$SCRIPT_DIR/session_list.py" still <pane>
# Reads session/list JSON from stdin, filters by pane target prefix, prints matched id.
import sys
import json


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else ""
    pane = sys.argv[2] if len(sys.argv) > 2 else ""
    before = sys.argv[3] if len(sys.argv) > 3 else ""
    try:
        d = json.load(sys.stdin)
    except Exception:
        print("")
        sys.exit(0)
    cands = [
        s.get("id", "")
        for s in d.get("sessions", [])
        if s.get("target", "") == pane
        or s.get("target", "").startswith(pane.split("@")[0] + "@")
    ]
    if mode == "cand":
        out = ""
        for cid in cands:
            if cid and cid != before:
                out = cid
        if not out and cands:
            out = cands[-1]
        print(out)
    elif mode == "still":
        print(cands[-1] if cands else "")
    else:
        print("")


if __name__ == "__main__":
    main()
