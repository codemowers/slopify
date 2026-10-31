# Player on Ubuntu 26.04 (Raspberry Pi)

Native systemd installation on 64-bit Ubuntu 26.04. The prepared profile targets
a Raspberry Pi 3 with a HiFiBerry Amp2, Ethernet and an optional rotary encoder.
The player decodes FLAC, WAV and MP3 itself and writes directly to ALSA; Ubuntu's
`mpv` handles other formats. Laptops send audio through the player's built-in Pulse
receiver. There is no PipeWire on the speaker, and AirPlay is not available yet.

| Path | Purpose |
|---|---|
| `overlay/` | Files copied onto the root filesystem, at the same paths |
| `install.sh` | Installs runtime packages, the overlay and the player, then starts services |
| `boot-config.txt`, `configure-boot.py` | Amp2 and encoder boot settings, applied separately |
| `slopify-player.env.example` | Template for `/etc/default/slopify-player` |

## Install

Cross-compile the player (or build it on the Pi):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
  -o bin/slopify-player-arm64 ./player
```

Copy the repository to the Pi, then from its root:

```sh
cp player/deploy/slopify-player.env.example slopify-player.env
# Edit SLOPIFY_SERVER and SLOPIFY_NAME.
sudo player/deploy/install.sh --binary bin/slopify-player-arm64 \
  --env slopify-player.env --listen LAN_IP --amp2
```

`LAN_IP` is the Pi's own address: the speaker page (port 8080) and Pulse receiver
(port 4713) bind to it, with **no authentication**, for a trusted LAN only. Omit
`--amp2` for no mixer or knob flags.
The installer never changes networking or reboots. It keeps the old player as
`slopify-player.previous`, and the pairing token in `/var/lib/slopify-player`.
Adopt the device in the web app after it first connects.

For another DAC, change `SLOPIFY_OUTPUT` to its `hw:` device.

## Boot configuration

Run `sudo python3 player/deploy/configure-boot.py` to stage the Amp2 settings in
`/boot/firmware/config.txt`. The original is kept as `config.txt.before-slopify`,
and re-running is safe. Review the file before rebooting. It disables display
output, so a blank screen does not mean the Pi is off.

Bare encoder wiring, with the Amp2 power disconnected (no 3.3 V or 5 V needed):

| Contact | BCM GPIO | Header pin |
|---|---|---|
| Rotation A | 23 | 16 |
| Rotation B | 24 | 18 |
| Rotation common | GND | 14 |
| Push switch | 25 | 22 |
| Push switch, other side | GND | 14 or another GND |

Swap A and B if the direction is reversed.

## Check

```sh
systemctl status slopify-player
journalctl -u slopify-player -n 30
cat /proc/asound/cards
ls -l /dev/input/slopify-rotary /dev/input/slopify-button
```

Then open `http://LAN_IP:8080/`.

## Networking

Configure Ethernet separately, so that installing the player cannot drop SSH. Use
`netplan try` for its timeout rollback when changing networking remotely.
