"""Eager native quota-profile rendering, without network or live overlays."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument("--temp-root", required=True)
args = parser.parse_args()
tests = Path(__file__).resolve().parent
mod = tests.parent / "mods/trin-quotas"
payload = json.loads((tests / "fixtures/quota-v1.json").read_text())
with tempfile.TemporaryDirectory(prefix="quota-profile-check-", dir=args.temp_root) as directory:
    package = Path(directory)
    for name in ["details.js", "metrics.js", "data.js"]:
        (package / name).write_text((mod / name).read_text())
    for index in range(len(payload["subscriptions"])):
        for mode in ["profile", "missing", "small"]:
            packet = json.loads(json.dumps(payload))
            record = packet["subscriptions"][index]
            record["account_email"] = None if mode == "missing" else "long-account-name-for-native-layout@example.test"
            record["plan"] = None if mode == "missing" else "Custom long subscription plan"
            source = '''import { View, svg } from "gpui-kit";
import { theme } from "herdr";
import { quotaDetails } from "./details.js";
import { normalizeSnapshot } from "./data.js";
const payload = PAYLOAD;
export default class ProfileCheck extends View {
  render() {
    const s = normalizeSnapshot(payload).subscriptions.find(s => s.id === RECORD_ID);
    return quotaDetails(s, {}, theme(), svg(`assets/trin-providers/${s.provider}.svg`).size(22), true, () => {}, VIEWPORT);
  }
}
'''.replace("PAYLOAD", json.dumps(packet)).replace("RECORD_ID", json.dumps(record["id"])).replace("VIEWPORT", "320" if mode == "small" else "900")
            (package / "main.js").write_text(source)
            subprocess.run(["herdr-app", "check", str(package)], check=True)
