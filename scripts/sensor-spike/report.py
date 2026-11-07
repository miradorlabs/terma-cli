"""Prints every session the sensor holds, in detail: python3 report.py <socket>."""
import json
import subprocess
import sys

sock = sys.argv[1]


def query(*args):
    cmd = ["/tmp/ts", "sensor", args[0], "--socket=" + sock, *args[1:]]
    out = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    return json.loads(out)


for s in query("sessions"):
    d = query("session", str(s["root"]))
    print(f"== {d['agent'].rsplit('/', 1)[-1]} root={d['root']} procs={d['procs']} "
          f"net tx={d['tx']:,} rx={d['rx']:,} files read={d['files_read']:,} written={d['files_written']:,} "
          f"removed={d['removed']} blocks={d['blocks']}")
    for h in d.get("hosts") or []:
        print(f"   net  {h['host']:40} tx={h['tx']:>12,} rx={h['rx']:>12,} conns={h['flows'] + h['live']}")
    for c in d.get("container_list") or []:
        print(f"   ctr  {c['name']} ({c['image']}) cgroup={c['cgroup']} via: {c['via']}")
    for p in d.get("pulls") or []:
        print(f"   pull {p['image']} size={p['size']:,} via: {p['via']}")
    for o in (d.get("ops") or [])[-12:]:
        extra = f" -> {o['to']}" if o.get("to") else (f" wrote {o['written']:,}" if o.get("written") else "")
        print(f"   file {o['op']:6} {o['comm']:10} {o['path']}{extra}")
    for b in d.get("block_list") or []:
        print(f"   BLOCK {b['kind']} {b['target']} rule={b['rule']} enforced={b['enforced']} by {b['comm']}")
