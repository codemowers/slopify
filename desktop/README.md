# Slopify desktop remote

A GNOME Shell extension for controlling a Slopify speaker from the desktop
panel, media keys and volume keys. Ubuntu GNOME 46 is the validated target;
other GNOME versions still need testing.

## Features

- Follow the selected PulseAudio/PipeWire output, with a manual speaker override.
- Show track information, transport controls, hardware volume and mute.
- Select a speaker input or return to automatic input selection.
- Control the chosen speaker with media keys and GNOME's volume OSD.
- Volume keys skip duplicate hardware gain steps so adjustment cannot stall.
- Expose an MPRIS endpoint for desktop integration.

The remote is a compact controller. Use the browser UI for catalog browsing,
adding tracks and queue editing. Speaker discovery and control require a
trusted LAN.

## Install

Run from the repository root:

```sh
gjs -m desktop/install.js
```

Log out and back in on Wayland after first installation. For updates, use the
reload procedure below or log out and back in. The companion follows GNOME's
selected audio output: a discovered Slopify PulseAudio or AirPlay output controls that speaker; other outputs use normal laptop shortcuts.
Choose a player manually to override this, or choose Follow system audio output
to resume following. Selecting Laptop controls, or disabling the extension,
restores normal desktop shortcuts.

```sh
gnome-extensions disable slopify@codemowers.io
```

## Reload an update without logging out

GNOME caches extension JavaScript for the session; disabling and enabling the
extension alone does not load changed code. On GNOME 46, an already loaded
Slopify companion can be replaced through Looking Glass without restarting
GNOME. This development workaround uses GNOME's internal extension manager;
other GNOME versions may require logging out.

Run the installer, then prepare a fresh copy of the modules from the repository
root. A new directory on each update avoids GNOME's module cache:

```sh
gjs -m desktop/install.js
reload_dir=$(mktemp -d /tmp/slopify-gnome-reload.XXXXXX)
cp desktop/extension/*.js "$reload_dir/"
printf "await (async () => { const m = Main.extensionManager; const e = m.lookup('slopify@codemowers.io'); const mod = await import('file://%s/extension.js'); await m._callExtensionDisable(e.uuid); e.stateObj = new mod.default({...e.metadata, dir: e.dir, path: e.path}); m.enableExtension(e.uuid); await m._callExtensionEnable(e.uuid); return 'Slopify reloaded'; })()\n" "$reload_dir"
```

Press **Alt+F2**, enter **`lg`**, and press Enter. Paste the single command printed
by the terminal into Looking Glass's Evaluator and press Enter. It should return
`Slopify reloaded`; press **Esc** to close the console. With **Follow system audio
output** selected, the companion follows the current PulseAudio/PipeWire output.
This replaces only the companion; it does not restart audio playback. The
installed extension loads normally at the next login. The temporary copy must
remain available for the rest of the current session.

See the [desktop guide](docs/implementation.md) for key handling,
control behavior, limits and troubleshooting.
