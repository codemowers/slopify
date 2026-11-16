# Noritake GU128X64-800B VFD on a Raspberry Pi Pico

Wiring reference for driving a Noritake Itron GU128X64-800B 128×64 graphic VFD
from the Pico that also acts as the USB DAC. The Raspberry Pi running the Slopify
player sends now-playing text to the Pico, and the Pico renders it. This is a
hardware proposal. Slopify has no display support yet.

Datasheet: [`noritake-gu128x64-800b.pdf`](noritake-gu128x64-800b.pdf)
(spec DS-1239-0000-03).

## Module facts

| Item | Value |
|---|---|
| Supply | 5.00 V ±5 % (4.75–5.25 V); rise time ≤ 100 ms |
| Current | 0.55 A typ (all dots off), 0.70 A typ / 0.90 A max (all on); **power-up inrush can exceed 2×** |
| Logic inputs | 5 V CMOS: VIH ≥ **4.0 V**, VIL ≤ 1.0 V. A 3.3 V Pico cannot drive them directly |
| Logic outputs | VOH ≥ 4.7 V. These are not safe for Pico inputs without level shifting |
| Interfaces | Serial or two 8-bit parallel modes, selected by jumpers J1/J2 with the power off |
| Fonts | **No font ROM** on the -800B: DDRAM/character mode is unavailable, so all text is drawn into graphic RAM by the host |
| Signal cable | 30 cm or less |

The -800A variant may differ, for example by including a font ROM. Check its
own datasheet before reusing this page.

## Interface choice: synchronous serial

Short **J2** (J1 don't care) to select the serial interface. It is a write-mostly
SPI-like bus. CSS low enables it. RXD is sampled MSB first on the SCK rising
edge. C/D is sampled on the 8th clock: 1 = command, 0 = data. The minimum SCK
high/low time is 80 ns, so the absolute limit is about 6 MHz. Use 1–2 MHz over
a ribbon cable. The Pico's SPI peripheral in mode 3 (or mode 0) with CS driven
manually matches this.

TXD is only needed for read-back commands. Leave it unconnected and treat the
display as write-only. Pace commands by time: 1 ms after Display Clear.

## Wiring

```
 Pico (3.3 V)              74AHCT125 / 74HCT244 @ 5 V        GU128X64-800B CN1 (2×13)
 ────────────              ──────────────────────────        ────────────────────────
 GP2  SPI0 SCK  ─────────► buffer ──────────────────────────► 21  SCK
 GP3  SPI0 TX   ─────────► buffer ──────────────────────────► 15  RXD
 GP5  CSS       ─────────► buffer ──────────────────────────► 23  CSS   (active low)
 GP6  C/D       ─────────► buffer ──────────────────────────► 19  C/D   (1 = command)
 GP7  RESET     ─────────► buffer ──────────────────────────► 26  RESET (active low)
                                   (not connected)            13  TXD
                                   (not connected)            25  FRP
 GND ─────────────────────────────────────────────────────── 2,4,…,24  GND (all even pins 2–24)

                     5 V buck (≥ 2 A) ──► CN power  1 Vcc
                                          CN power  2 Test (factory only, leave open)
                     GND ───────────────► CN power  3 GND   (JST XH 3-pin)
```

- **Level shifting:** use an HCT/AHCT buffer powered from 5 V. Its 2.0 V input
  threshold accepts the Pico's 3.3 V outputs, and its outputs meet the module's
  4.0 V VIH. Tie unused buffer inputs to GND. Bidirectional "BSS138" shifter
  boards are slower and weaker but usually work at 1 MHz. Plain wires do not
  meet the datasheet's VIH.
- **RESET:** add a 10 kΩ pull-up to 5 V on the module side so the display stays out of
  reset while the Pico boots. The datasheet requires RESET low for > 1.5 ms after
  Vcc reaches 2.5 V. Driving it from GP7 after the Pico starts satisfies this.
- **Power:** do not take the VFD's 5 V from the Pi header or the Pico. With ~0.7 A
  steady and > 1.4 A inrush, give it its own buck converter from the 12–20 V
  amplifier supply (Beocreate or Amp2) and share GND. The DC/DC converter and
  filament inside the module produce heat and switching noise, so keep the
  ribbon short and away from the speaker wiring.
- **Pin 1:** CN1 is a 2×13 header with D7 at pin 1. Odd pins are signals and even
  pins 2–24 are GND.

## Pico pin budget

| Pico GPIO | Use |
|---|---|
| GP0 / GP1 | UART0 TX / RX to the Raspberry Pi (see below) |
| GP2, GP3, GP5, GP6, GP7 | VFD SCK, RXD, CSS, C/D, RESET |
| GP9 | I2S data to the mux board |
| GP10 / GP11 | I2S BCLK / LRCLK (consecutive, as `pico_audio_i2s` requires). Variant A: Pico drives them. Variant C: inputs from Beocreate conn. pins 16 / 15, with GP9 going to pin 19 (SDATA IN1) |
| GP12 / GP13 / GP14 | I2S monitor inputs: DATA / BCLK / LRCLK for the VU meter. Variant A: mux output. Variant B: Beocreate conn. pins 17 / 16 / 15 (DSP OUT2) |
| GP16 / GP17 / GP18 | Motor H-bridge AIN1 / AIN2 / nSLEEP ([motorized knob](motorized-volume-knob.md)) |
| GP26 (ADC0) | Motorized knob feedback gang |
| USB (TP2/TP3 or micro-USB) | USB audio from the host |

## Initialization and drawing

Power-up sequence from the datasheet: assign all 8 display-area blocks to GRAM,
because the -800B has no character RAM.

```
C/D=1 0x5F        Display Clear (both GRAM areas, reset addresses); wait 1 ms
for n in 0..7:
  C/D=1 0x62      Display Area Set
  C/D=1 n         area address
  C/D=0 0xFF      all blocks → GRAM
C/D=1 0x24 0x40   Display ON: layer 0 only (L0), GRAM on (GS)
C/D=1 0x40|b      Brightness: b = 0 (100 %) … 15 (6 %)
```

GRAM uses vertical bytes. A data byte writes 8 pixels in one column (D0 at
the top). X is 0x00–0x7F and Y is the 8-pixel row, 0x0–0x7 for layer 0.
Set the position with `0x64 X` and `0x60 Y`. Address Mode Set (`0xC0 | IGX<<2 | IGY<<1`)
selects auto-increment, so a whole 8-pixel row is one position command followed by
128 data bytes. A full frame is 1 KiB, about 4 ms at 2 MHz.

Layer 1 (Y 0x8–0xF) works as a back buffer. Draw the next frame there and
switch layers with Display ON/OFF to avoid tearing. Brightness can follow
playback state (dim while paused). Move content occasionally and avoid static
full-on areas to limit uneven phosphor wear.

## Pi → Pico link

Use **UART**, not I2C:

- **I2C problems:** GPIO 2/3 are the HAT's I2C configuration bus (Beocreate, Amp2).
  HiFiBerry discourages adding devices there. The Pi 3's I2C controller also has
  a known clock-stretching bug, and the RP2040 I2C slave stretches the clock.
- **UART is free:** Slopify's Pi boot profile already sets `dtoverlay=disable-bt`, so the PL011
  UART (`/dev/ttyAMA0`) is on GPIO 14 (TX) and 15 (RX). On a Beocreate, the
  34-pin connector also breaks these out on pins 24/25. Remove `console=serial0,…` from
  the kernel command line and disable `serial-getty@ttyAMA0`.

Wiring: Pi GPIO14 TX → Pico GP1 RX, Pi GPIO15 RX ← Pico GP0 TX, common GND. Both
sides use 3.3 V logic, so no level shifting is needed.

If I2C is unavoidable, check free addresses with `i2cdetect -y 1` first, use
writes only, keep the bus at 100 kHz and use `pico_i2c_slave`.

A minimal line protocol (UTF-8, newline-terminated, bounded line length) keeps
the Pico stateless across Pi restarts:

```
T <title>
A <artist>
S play|pause|stop
P <position-ms> <duration-ms>
V <dB or -inf>
```

The Pico needs a bitmap font covering at least Latin-1 plus Estonian/Latin
Extended-A characters (õ, š, ž), with a fallback glyph for anything else.
Sending the player's text unmodified from Slopify would need a new opt-in
player flag. That flag does not exist yet.

## VU meter

Take levels from the **I2S bus itself**, not from the Pi. On the mux board,
connect the mux *output* lines (BCLK, LRCLK, DATA going to the amplifier HAT) to
three Pico inputs, GP12–GP14. A PIO state machine clocked by that BCLK receives
whatever source is currently selected, whether the Pi or the Pico's own USB
stream. The Pico only listens on these pins, so it never drives the selected
source's bus.

Why this source:

- **Timing:** the meter matches what reaches the amplifier, with no audio-buffer
  latency. Levels computed on the Pi would run ahead of the sound by the
  ALSA/PipeWire buffer.
- **Independence:** it works for every source and needs no player changes or UART traffic.
- **Simplicity:** no DSP readback. Reading the ADAU145x level detectors over the
  Pi's SPI would be possible on a Beocreate, but it would fight SigmaTCP.

It measures the digital source level in dBFS *before* the DSP or codec volume
control, because Slopify itself never scales samples. That is the conventional
meaning of a signal meter. Volume is already shown separately in dB.

Processing on the Pico:

- **Input:** standard I2S, MSB first, one BCLK after each LRCLK edge. The slot
  width depends on the sender (32 or 64 BCLK per frame), so count BCLKs per
  LRCLK period instead of hardcoding it. The rate follows BCLK automatically,
  up to 192 kHz (BCLK 12.288 MHz with 64 BCLK per frame). At 192 kHz
  the PIO has about 10 system clocks per bit at 125 MHz, which is workable. The
  RP2350 (Pico 2, 150 MHz) has more margin. Use DMA into ping-pong buffers.
- **Per block:** per-channel peak (|x| max) and sum of squares for RMS. At 192 kHz
  stereo that is 384 k samples/s, which an RP2040 handles in integer math.
- **Ballistics:**
  - Peak bars: instant attack, about 20 dB/s release, 1.5 s peak-hold marker.
  - Optional VU-style RMS: 300 ms integration, 0 VU = −18 dBFS.
  - Clip indicator: any sample at full scale.
- **Idle:** if BCLK stops for more than 100 ms (pause, mute or mux switching), fade
  the bars out instead of freezing them.
- **Spectrum analyser (optional):** a 16–32 band FFT spectrum is possible but
  fits the RP2350 better because of its FPU/DSP instructions.

Display: the VFD can merge its two GRAM layers with OR. Draw the static
content (title, artist, dB scale) into layer 1 once per track. Redraw only the
meter rows in layer 0, for example two 8-pixel-high bars, 2 × 128 bytes per update.
At 30–60 Hz this is well under 1 % of the 2 MHz serial bus. Enable both
layers with `0x2C 0x40` (L1 + L0, OR merge, GRAM on).
