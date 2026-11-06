"""Read-only remote collector profile; never print targets, auth, or message bodies.

python3 tests/profile-activity.py --host your-host --targets /path/targets.json
Targets are metadata from a real bridge argv, not prompts. Source is sent on stdin;
no remote installation or files are needed. Warm run tests cross-exec metrics and
shell caches. Counts include received JSON bytes, not TLS/HTTP framing.
"""

import argparse
import json
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--host", required=True)
parser.add_argument("--targets", type=Path, required=True)
parser.add_argument("--source", type=Path, default=Path(__file__).parents[1] / "mods/trin-agents/activity.py")
args = parser.parse_args()
source = args.source.read_text()
targets = json.loads(args.targets.read_text())
script = "import types,json,time,cProfile,pstats,io,copy\n"
script += "m=types.ModuleType('activity')\nexec(" + repr(source) + ",m.__dict__)\n"
script += "targets=json.loads(" + repr(json.dumps(targets)) + ")\n"
script += '''
calls=[]
original=m.API.get
def get(self,path,**query):
    start=time.monotonic()
    value=original(self,path,**query)
    # No paths/session IDs/query values or response bodies enter the report.
    route='message' if '/message' in path else ('session' if '/session' in path else path)
    calls.append((route,len(json.dumps(value).encode()),time.monotonic()-start))
    return value
m.API.get=get
shell_cache=None
for phase in ('cold','warm'):
    api=m.API()
    if phase=='warm' and shell_cache:
        api.shell_cache=shell_cache
    calls.clear()
    profile=cProfile.Profile()
    start=time.monotonic(); cpu=time.process_time(); profile.enable()
    result=m.collect(api,targets)
    profile.disable()
    print(json.dumps(dict(phase=phase,wall=time.monotonic()-start,cpu=time.process_time()-cpu,
        calls=len(calls),bytes=sum(c[1] for c in calls),targets=len(targets),
        errors=sum('error' in x for x in result.values()),
        contextErrors=sum('contextError' in (x.get('meta') or {}) for x in result.values()),
        durationErrors=sum('durationError' in (x.get('meta') or {}) for x in result.values()))))
    for route in sorted(set(c[0] for c in calls)):
        items=[c for c in calls if c[0]==route]
        print(json.dumps(dict(route=route,calls=len(items),bytes=sum(c[1] for c in items))))
    buf=io.StringIO(); pstats.Stats(profile,stream=buf).sort_stats('tottime').print_stats(10)
    print(buf.getvalue())
    shell_cache=getattr(api,'shell_cache',None)
    for target in targets:
        meta=result.get(target['address'],{}).get('meta') or {}
        for key in ('durationState','metricsCache'):
            if key in meta: target[key]=meta[key]
'''
result = subprocess.run(["ssh", args.host, "python3 -I -"], input=script, text=True, capture_output=True)
print(result.stdout, end="")
if result.returncode:
    # Tracebacks can include raw URLs and credentials; keep failure reporting fixed.
    raise SystemExit("Remote profiling failed (details suppressed for privacy)")
