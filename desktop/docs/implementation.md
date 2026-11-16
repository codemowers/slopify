# Slopify Remote — GNOME Shell extension

A native GJS extension for Ubuntu GNOME 46. No Python, pip, npm dependencies,
compiled schemas or binary build. Uses GNOME's panel/menu/slider/OSD directly;
Avahi D-Bus discovers `_slopify._tcp` players and their existing HTTP APIs handle
control. One MPRIS endpoint remains available as `org.mpris.MediaPlayer2.slopify`.

```sh
gjs -m desktop/install.js
```

Log out and back in on Wayland after first installation. The installer retires the
old Python applet/autostart and restores its key bindings, preserving the selected
speaker. The extension loads from
`~/.local/share/gnome-shell/extensions/slopify@codemowers.io/` at login.

The Slopify music-note icon opens a dropdown with title, artist, album, input,
previous/play-pause/next, hardware volume slider and mute, player selection and
source selection. Open player uses its advertised `.local` hostname. Incoming RTP
metadata is shown too. Source selection pins the chosen input; Resume automatic
selection releases the pin. Transport controls are disabled unless Slopify is the
selected input. Queue commands affect its shared queue, including other outputs;
Stop pauses it. Seeking is not exposed.

Choosing a speaker redirects volume/media keys to it. Volume changes show the
native GNOME OSD with speaker name and hardware dB. MPRIS/slider volume 0–1 maps to
the speaker's configured bounds; zero uses hardware mute when available. A speaker
without a playback mute switch can only reduce to its minimum configured level.
GNOME's separate laptop volume slider is untouched. Selecting Laptop controls or
disabling the extension restores shortcuts; an offline speaker is never replaced
with a different one. Key takeover applies to normal desktop/overview modes, not
the lock screen. Binding backup and chosen speaker live in
`~/.config/slopify-remote/`; a saved backup is recovered on next enable.

Disable with `gnome-extensions disable slopify@codemowers.io`. No global unsafe mode
or extra OSD D-Bus service is used. All network operations are asynchronous and
bounded. Commands expire, are serialized and never retried; pending slider updates
coalesce. Switching target discards pending work, although an already transmitted
command may finish on the previous player. Discovery/control are trusted-LAN only,
with numeric private IPv4 HTTP endpoints, no redirects, proxy or cloud credentials.
Status polling retries every second. Missing players, discovery failures and three
consecutive HTTP failures trigger rediscovery at most once every five seconds.
Avahi cached events are subscribed before browser creation so early announcements
are not lost. Default HTTP ports are omitted from Origin to match Soup’s Host header.
Speakers need `/api/playback` support. Other GNOME versions are not yet validated.

Checks: `node desktop/test-model.mjs`, JavaScript syntax checks, GJS HTTP
read against a real speaker, and enable/MPRIS smoke test in a separate nested
GNOME 46 session. No second service or standalone now-playing window is needed.
