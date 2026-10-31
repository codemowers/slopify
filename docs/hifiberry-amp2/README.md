# HiFiBerry Amp2 and rotary knob

Pinout and wiring reference for the Raspberry Pi + HiFiBerry Amp2 device with
a bare rotary encoder and push button. Installation steps, the systemd unit and
boot staging live in [`deploy/raspi/README.md`](../../deploy/raspi/README.md).
Physical encoder operation has not yet been verified.

Sources:

- [Datasheet Amp2](https://www.hifiberry.com/docs/data-sheets/datasheet-amp2/)
- [GPIO usage of HiFiBerry boards](https://hifiberry.com/?p=64904)

## Board overview

```
                    Raspberry Pi (40-pin header)
     I2C 2,3 (codec config) │ I2S 18-21 (audio) │ GPIO4 (MUTE)
                            ▼                   ▼             ▼
 12–20 V DC ──► ┌───────────────────────────────────────────────────┐
 (abs. max 24 V)│ TAS5756M DAC + class-D stereo amplifier           │──► L speaker
 also powers    │ ALSA card "sndrpihifiberry", mixer "Digital"      │──► R speaker
 the Pi         │ 44.1–192 kHz, 24-bit; dtoverlay=hifiberry-dacplus*│   (no bridging)
                └───────────────────────────────────────────────────┘
```

Typical output per channel at THD+N < 0.1 % is 14/18/20 W into 4 Ω at
12/18/24 V, and 8/17/28 W into 8 Ω. HiFiBerry recommends 12–20 V. Do not also
power the Pi over USB. The Slopify boot profile uses `hifiberry-dacplus-std`.

## Raspberry Pi GPIOs (BCM numbering)

```
                 3V3  (1) (2)  5V   ◄ backpowered by Amp2
 I2C SDA ■ GPIO2      (3) (4)  5V
 I2C SCL ■ GPIO3      (5) (6)  GND
 MUTE    ■ GPIO4      (7) (8)  GPIO14 □
                 GND  (9) (10) GPIO15 □
       □ GPIO17      (11) (12) GPIO18 ■ I2S BCLK
       □ GPIO27      (13) (14) GND    ◆ encoder common / button
       □ GPIO22      (15) (16) GPIO23 ◆ encoder A
                 3V3 (17) (18) GPIO24 ◆ encoder B
       □ GPIO10      (19) (20) GND
       □ GPIO9       (21) (22) GPIO25 ◆ push button
       □ GPIO11      (23) (24) GPIO8  □
                 GND (25) (26) GPIO7  □
 HAT ID  ✕ GPIO0     (27) (28) GPIO1  ✕ HAT ID
       □ GPIO5       (29) (30) GND
       □ GPIO6       (31) (32) GPIO12 □
       □ GPIO13      (33) (34) GND
 I2S LRCLK■ GPIO19   (35) (36) GPIO16 □
       □ GPIO26      (37) (38) GPIO20 ■ I2S DIN
                 GND (39) (40) GPIO21 ■ I2S DOUT

 ■ used by Amp2   ◆ Slopify rotary knob   □ free   ✕ reserved for HAT ID EEPROM
```

| BCM GPIO | Header pin | Used by | Function |
|---|---|---|---|
| 2, 3 | 3, 5 | Amp2 | I2C codec configuration; extra I2C devices only at non-conflicting addresses |
| 4 | 7 | Amp2 | Power-stage MUTE, low = muted |
| 18, 19, 20, 21 | 12, 35, 38, 40 | Amp2 | I2S audio |
| 0, 1 | 27, 28 | reserved | HAT ID EEPROM |
| 23 | 16 | knob | Encoder A |
| 24 | 18 | knob | Encoder B |
| 25 | 22 | knob | Push button |

Everything else is free: GPIO 5–17, 22, 26 and 27, with 14/15 serving as the UART
when the serial console is enabled. Draw only a few mA from the 3.3 V rail.

## Rotary knob wiring

Disconnect the Amp2 supply before wiring. A bare mechanical encoder needs no
3.3 V or 5 V connection, because the kernel overlays enable internal pull-ups.

```
     EC11-style encoder (typical layout)   Pi header
    ┌──────────────────────────┐
    │   A     C     B          │   A ───────── pin 16 (GPIO23)
    │   │     │     │          │   C ───┬───── pin 14 (GND)
    │                          │        │
    │        SW1   SW2         │   SW1 ─┘
    │         │     │          │   SW2 ─────── pin 22 (GPIO25)
    └──────────────────────────┘   B ───────── pin 18 (GPIO24)
```

| Contact | BCM GPIO | Header pin |
|---|---|---|
| Rotation A | 23 | 16 |
| Rotation B | 24 | 18 |
| Rotation common (usually the middle of three) | GND | 14 |
| Push switch, one side | 25 | 22 |
| Push switch, other side | GND | 14 or any GND |

Confirm the common with a continuity tester. Swap A and B if the direction is
reversed.

## Software configuration

Boot overlays (`deploy/raspi/boot-config.txt`, staged by `configure-boot.py`):

```
dtparam=audio=off
dtoverlay=hifiberry-dacplus-std
dtoverlay=rotary-encoder,pin_a=23,pin_b=24,relative_axis=1,linux_axis=0
dtoverlay=gpio-key,gpio=25,active_low=1,gpio_pull=up,keycode=164,label=SlopifyPlayPause
```

The rotary overlay reports REL_X, one step per detent. The key overlay
reports KEY_PLAYPAUSE (164). The udev rules in `deploy/raspi/70-slopify-knob.rules`
match the kernel input names `rotary@17` and `button@19`, which are the GPIO
numbers in hex. The rules create `/dev/input/slopify-rotary` and
`/dev/input/slopify-button`, readable only by the `slopify-player` group.

Player flags (`deploy/raspi/hifiberry-amp2.conf` systemd drop-in):

```
slopify-player -volume-control Digital -volume-card sndrpihifiberry \
  -volume-min 0 -volume-max 40 -volume-initial 20 \
  -rotary-input /dev/input/slopify-rotary -button-input /dev/input/slopify-button
```

Volume levels use alsamixer's perceptual 0–100 display scale. On the Amp2,
20 ≈ −42 dB and 40 ≈ −24 dB. The initial level is set once per process start,
and local and web changes are clamped to 0–40. One detent moves one displayed
level. A press toggles play/pause for the queue currently selecting this
device. Rotation is applied locally and never waits for the server.

## Checks after boot

```sh
cat /proc/asound/cards                     # sndrpihifiberry present
cat /proc/bus/input/devices                # rotary@17, button@19
ls -l /dev/input/slopify-rotary /dev/input/slopify-button
amixer -c sndrpihifiberry cget 'name=Digital Playback Volume'
journalctl -u slopify-player -n 30
```
