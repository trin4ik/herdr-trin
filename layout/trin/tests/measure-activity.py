"""Passive /proc measurement matching the reported 60s Herdr-child CPU probe.

python3 tests/measure-activity.py --host your-host [--seconds 60] [--capture-targets /safe/targets.json]
No remote files or processes are changed. Counts all Python children of Herdr
serve, like the original probe (not just this mod). Sampling can miss short-lived
processes/final CPU ticks; initial live processes include their pre-window CPU.
Only optional target metadata is saved locally; never source, auth or messages.
"""

import argparse
import json
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--host", required=True)
parser.add_argument("--seconds", type=int, default=60)
parser.add_argument("--capture-targets", type=Path)
args = parser.parse_args()
if not 1 <= args.seconds <= 600:
    parser.error("seconds must be between 1 and 600")
script = "seconds=" + str(args.seconds) + "\n" + '''
import os,time,glob,json
hz=os.sysconf('SC_CLK_TCK'); seen={}; t0=time.monotonic(); targets=None; optimized=0
def cmd(directory):
    try: return open(directory+'/cmdline','rb').read()
    except OSError: return b''
serve=[d[6:] for d in glob.glob('/proc/[0-9]*')
       if cmd(d).split(b'\\0')[:1] and cmd(d).split(b'\\0')[0].endswith(b'/herdr-core')
       and cmd(d).split(b'\\0')[1:2] == [b'serve']]
while time.monotonic()-t0<seconds:
    for d in glob.glob('/proc/[0-9]*'):
        try:
            st=open(d+'/stat').read(); r=st.rsplit(')',1)[1].split()
            if r[1] in serve and '(python3)' in st:
                seen[d]=max(seen.get(d,0),int(r[11])+int(r[12]))
                argv=cmd(d).split(b'\\0')
                if len(argv)>4 and b'Bounded OpenCode SSE' in argv[2]:
                    targets=json.loads(argv[3])
                    if b'self.opener = urllib.request.OpenerDirector()' in argv[2]: optimized+=1
        except (OSError,ValueError,IndexError): pass
    time.sleep(.1)
cpu=sum(seen.values())/hz
print(json.dumps(dict(launches=len(seen),cpuSeconds=cpu,percentCore=cpu/seconds*100,
    seconds=seconds,optimizedObserved=optimized>0,targets=targets)))
'''
result = subprocess.run(["ssh", args.host, "python3 -I -"], input=script, text=True, capture_output=True)
if result.returncode:
    raise SystemExit("Remote measurement failed (details suppressed for privacy)")
data = json.loads(result.stdout)
targets = data.pop("targets", None)
if args.capture_targets and targets:
    args.capture_targets.write_text(json.dumps(targets, ensure_ascii=False))
print(json.dumps(data, ensure_ascii=False))
