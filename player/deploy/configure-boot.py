#!/usr/bin/env python3
"""Stage the headless Amp2/encoder boot configuration; never reboot."""
import argparse
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("config", nargs="?", default="/boot/firmware/config.txt")
args = parser.parse_args()
path = Path(args.config)
original = path.read_text()
begin, end = "# BEGIN SLOPIFY APPLIANCE", "# END SLOPIFY APPLIANCE"
lines = []
managed = False
for line in original.splitlines():
    if line == begin:
        managed = True
        continue
    if line == end:
        managed = False
        continue
    if managed:
        continue
    if line.startswith(("dtoverlay=vc4-", "dtoverlay=hifiberry-", "dtoverlay=rotary-encoder", "dtoverlay=gpio-key")):
        line = "# Disabled by Slopify appliance setup: " + line
    if line == "dtparam=audio=on":
        line = "dtparam=audio=off"
    if line.startswith(("camera_auto_detect=", "display_auto_detect=")):
        line = line.split("=")[0] + "=0"
    lines.append(line)
if managed:
    raise SystemExit("Unterminated Slopify configuration block; leaving file unchanged")
snippet = Path(__file__).with_name("boot-config.txt").read_text().rstrip()
updated = "\n".join(lines).rstrip() + "\n\n" + begin + "\n" + snippet + "\n" + end + "\n"
backup = path.with_name(path.name + ".before-slopify")
if not backup.exists():
    backup.write_text(original)
path.write_text(updated)
print(f"Updated {path}; original backup: {backup}. Changes take effect at next boot.")
