# Builds the synthetic ubuntu-respawn.jsonl from the captured
# ubuntu-bind-rejected.jsonl by editing one line, its last (line 37): worker
# 911 is replaced by a worker 990 of the same master that started 250 ticks
# later, as if 911 had crashed and the master had respawned it. Every other
# line, and every other process on line 37, is the capture's.
# Run from this directory: python3 -I scripts/make-respawn.py
import json

lines = open("ubuntu-bind-rejected.jsonl").read().split("\n")
edited = 37
scan = json.loads(lines[edited - 1])
for p in scan["procs"]:
    if p["pid"] == 911:
        f = p["stat"].split(" ")
        assert f[0] == "911" and f[3] == "909", p["stat"]
        f[0] = "990"
        f[21] = str(int(f[21]) + 250)  # field 22, starttime
        p["pid"] = 990
        p["stat"] = " ".join(f)
scan["procs"].sort(key=lambda p: p["pid"])
lines[edited - 1] = json.dumps(scan, separators=(",", ":"), ensure_ascii=False)
open("ubuntu-respawn.jsonl", "w").write("\n".join(lines))
