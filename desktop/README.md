# Slopify desktop remote

A GNOME Shell extension for controlling a Slopify speaker from the desktop
panel, media keys and volume keys. Ubuntu GNOME 46 is the validated target;
other GNOME versions still need testing.

## Features

- Discover speakers on the LAN and choose a control target.
- Show track information, transport controls, hardware volume and mute.
- Select a speaker input or return to automatic input selection.
- Control the chosen speaker with media keys and GNOME's volume OSD.
- Expose an MPRIS endpoint for desktop integration.

The remote is a compact controller. Use the browser UI for catalog browsing,
adding tracks and queue editing. Speaker discovery and control require a
trusted LAN.

## Install

Run from the repository root:

```sh
gjs -m desktop/install.js
```

Log out and back in on Wayland after the first installation. Choose a speaker
from the Slopify panel menu. Selecting Laptop controls, or disabling the
extension, restores normal desktop shortcuts.

```sh
gnome-extensions disable slopify@codemowers.io
```

See the [desktop guide](docs/implementation.md) for key handling,
control behavior, limits and troubleshooting.
