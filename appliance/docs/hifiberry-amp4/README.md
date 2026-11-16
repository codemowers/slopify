# HiFiBerry Amp4 with an IR receiver

This example uses a Raspberry Pi 3 with an Amp4 and a separate GPIO IR
receiver. The Amp4 is the audio output; Linux RC-core and a BPF decoder turn
the receiver's pulses into standard media keys. The Go player does not decode
or transmit IR.

## Hardware and overlays

The tested receiver uses **BCM25, physical pin 22**. Use a receiver suitable
for 3.3 V GPIO: power it from 3.3 V (for example physical pin 17), connect its
ground to a Pi ground pin, and connect its output to GPIO25. Check the exact
receiver or breakout pinout before connecting it; connector order varies.
Disconnect power while wiring. A GPIO demodulator's carrier frequency is fixed
by its hardware and cannot be retuned in LuCI.

The audio and receiver overlays are separate:

```ini
dtoverlay=hifiberry-dacplus-std
dtoverlay=gpio-ir,gpio_pin=25
```

Use the explicit audio overlay when generic DAC+ detection chooses Pro clocking
incorrectly. Set the IR pin explicitly: GPIO18 conflicts with I2S audio.
GPIO25 here is also the push-button pin in the [Amp2 rotary example](../hifiberry-amp2/README.md);
these are separate setups. A combined knob/IR setup needs a different checked
pin assignment. The DSP add-on is outside this example; confirm its reservations
before combining it with additional GPIO controls.

## Build and install an IR-capable image

Official Image Builder release kernels lack the GPIO IR support needed here.
The IR receive tab is disabled on those images. A boot overlay alone cannot
supply missing kernel support, and kernel modules must match the running image.

The experimental Pi 3 source build is separate from release CI:

```sh
appliance/source-build.sh rpi-3
```

It supplies built-in RC-core, LIRC, GPIO reception and NEC/RC5/RC6 modules.
BPF decoding additionally requires `CONFIG_BPF_LIRC_MODE2=y` and the upstream
v4l-utils BPF objects; general BPF support alone is insufficient. See the
[OpenWrt build guide](../openwrt.md#experimental-arm64-pi-source-build)
for configuration checks, output paths and installation details. Enable the
checked overlays in `/boot/config.txt` and reboot.

## Configure the remote in LuCI

1. Open **Services → Slopify player**. Select `hifiberry-dacplus-std` as the
   audio HAT when needed; audio-overlay changes require a reboot.
2. Under **Volume control**, choose the card's `Digital` hardware control and
   conservative volume limits.
3. Under **IR receive**, choose the receiver and the remote's protocol, then
   **Save & Apply**. An empty keymap is allowed during initial setup.
4. Click **Learn** for an action, press and release its remote button, and
   **Save & Apply** again. Repeat for volume, mute and transport buttons.

Receiver identity is stored by physical path, so changing `rcN` or `eventN`
numbers do not invalidate the choice. Learning has a five-second limit and
captures the full LIRC scancode. It grabs only that input device, preventing
the learning press from also changing playback. Volume keys can repeat;
transport and mute act once per press.

The separate `slopify-ir` helper and `ir-keytable` configure decoding and the
kernel keymap. The player receives the resulting Linux media-key events.

## Verify reception

```sh
ir-keytable
slopify-ir ir-info
logread | grep -i slopify
```

The Amp4 setup has exposed `gpio_ir_recv`, an input event device and
`/dev/lirc0`, with `pulse_distance` attached. Saved captures include TechniSat
100 TS 009 RECS80 toggle variants and a Denon PMA-60 Kaseikyo frame with vendor
`0x3254` and valid parity. These establish reception and decoder operation;
each remote's button mappings still need learning and testing.
