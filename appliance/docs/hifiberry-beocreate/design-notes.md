# Design notes and alternatives

Decisions, rejected options and parked ideas behind the speaker build in
[`README.md`](README.md). These are design reasoning only. Nothing here is
verified on hardware.

## Getting USB audio into the Beocreate

Ranked from least to most invasive:

1. **Pico → S/PDIF → Toslink input.** Requires no DSP reprogramming.
   - The Pico's PIO generates S/PDIF (`pico-extras` `pico_audio_spdif`), and a
     Toslink transmitter module feeds the Beocreate's Toslink input.
   - The input ASRC absorbs the clock mismatch. The Pico stays clock master
     with asynchronous USB feedback against its own crystal, and the host can
     use 44.1/48/96 kHz.
   - Optical isolation removes the host-to-amplifier ground loop.
   - The cost is that the Beocreate's Toslink input is used up.
2. **Interposer mux on the Pi's I2S lines** (variant A). See the interposer
   section below.
3. **I2S into DSP SDATA IN1 on the connector** (pin 19), with a custom
   SigmaStudio program. This is now the preferred route; see variant C in
   [`README.md`](README.md). The connector breaks out only LRCLK/BCLK OUT2
   (pins 15/16), so there are two clocking choices:
   - **DSP master (preferred):** serial input 1 uses output port 2's clocks, and
     the Pico follows them. The USB device offers only the DSP's fixed rate (48
     kHz by default), and the host resamples. The Pico measures LRCLK against
     the USB 1 kHz SOF to produce asynchronous feedback. OUT2 data (pin 17)
     shares the clocks, so the same ribbon also carries the VU tap.
   - **Pico master (unverified):** reconfigure OUT2's clock pins as inputs and
     clock serial input 1 from them through an ASRC, so the Pico can offer
     44.1/48/96 kHz natively. Check this against the ADAU145x serial-port and ASRC
     register documentation before relying on it.
   - Use a short ribbon (≤ 10 cm) with ground next to the clocks and 33 Ω series
     resistors at the drivers, routed away from the class-D outputs.
4. **Tapping the DSP's own input clock pins on the PCB**, so the Pico can drive
   its own clocks into an ASRC-backed serial input. Analog Devices' datasheet
   could not be fetched to check which ADAU1451 clock pins share pads with MP
   pins. The LFCSP pins are hard to solder to. Unverified, not recommended.
5. **Pi 4 as a USB audio gadget** (UAC2 over USB-C) instead of a Pico. The Pi's
   existing I2S then carries it. This needs a data-only cable because the
   Beocreate already powers the Pi, and it ties the gadget to the Slopify Pi. A Pi 3
   has no USB device mode.

## Interposer (variant A) details

- **Switched lines:** BCLK (GPIO18), LRCLK (GPIO19) and Pi→board data (GPIO21).
  Board→Pi data (GPIO20) passes through. Use a 3.3 V bus switch (74CBTLV3257) or a
  74LVC157, with short, length-matched traces.
- **Select line:** GPIO 4, 13 or 26. The interposer sees all 40 pins, including
  those the Beocreate does not break out. The Pico can also assert a "streaming"
  pin instead.
- **Switching:** assert MUTE (GPIO27), switch, let the ASRC relock, then unmute.
- **Pico power:** 5 V rail → Schottky → VSYS. The Pico's onboard VBUS diode then
  keeps host VBUS and board 5 V apart. Do not power the Pico from the Pi's 3.3 V,
  which is limited to a few mA.
- **Isolation:** the Pico is a full-speed (12 Mbit/s) USB device, so an ADuM3160
  isolator can break the host-to-amplifier ground loop.
- **5 V passthrough:** about 2.5 A for the Pi. Use wide copper or several pins in
  parallel. Use longer standoffs and a stacking header.
- **Precondition:** scope GPIO18 while playing 44.1 vs 48 kHz. If BCLK changes, the
  Pi is clock master and the Pico can replace it. If the board drives the clocks,
  the Pico would have to slave and the design falls back to option 3 above.

## Pico USB connector

The Pico and Pico 2 expose USB D− on TP2, D+ on TP3 and GND on TP1, all on the
underside. For a panel USB-B socket, either fly-wire them as a short twisted
pair, or surface-mount the Pico on the carrier with matching copper pads under
TP1–TP3. Feed VBUS to Pico pin 40, through the isolator if used. Check the Pico
schematic for whether the 27 Ω series resistors sit before or after the test
pads.

The full-speed isochronous limit is 1023 bytes per 1 ms frame. 24-bit stereo
at 192 kHz needs 1152 bytes, so the USB DAC tops out around 96 kHz. A
high-speed USB device would be needed for more, not additional Picos.

## One Pico or several

**Decision:** one Pico 2 (RP2350).

- **Core 0:** USB audio and I2S, the timing-critical work.
- **Core 1:** UART, VFD drawing, VU/FFT and the knob loop.
- **Shared data:** lock-free level and text buffers between the cores.
- **PIO:** I2S out and the I2S monitor use 2 PIO state machines out of 12.
- **Clocks:** run `clk_peri` from the USB PLL so UART/SPI baud rates don't
  change when the system clock is retuned for the 44.1 kHz vs 48 kHz rate family.
- **Debug:** bring SWD and reset to a header so the Pico can be reflashed without
  unstacking.

Two Picos make sense only if the USB DAC should be a separately reusable unit,
the display must stay up while DAC firmware is under test, or the USB socket is
physically far from the display.

**Alternative: no display Pico.** The Pi's hardware SPI is used by the
Beocreate, but the `spi-gpio` overlay provides a kernel bit-banged SPI on free
pins such as GPIO 5/6/12/13. The Slopify player could then drive the VFD
directly. The VU meter would lead the audio by the ALSA/PipeWire buffer.

## VU metering options

- **Pico taps the I2S it can see** (mux output in variant A, DSP OUT2 in variant
  B). Recommended. It is sample-accurate in time, independent of the player, and
  costs no UART traffic.
- **Toslink in variant A without DSP changes:** add a Toslink receiver plus a
  CS8416 (hardware mode, ≤ 192 kHz) as another mux input. It outputs I2S
  with the clock recovered from the S/PDIF stream, so the existing tap meters it.
  The Beocreate's own Toslink input is then unused. Do not decode S/PDIF on the
  Pico and re-emit I2S, because that needs a software clock-tracking loop.
- **Pi computes levels and sends them over UART.** These lead the sound by the
  output buffer. Rejected.
- **Pi reads ADAU145x level-detector cells over SPI.** This competes with
  SigmaTCP for the DSP control bus. Rejected.

### FFT on the Pico vs a DSP filter bank

- **Pico FFT (recommended):**
  - On a Pico 2, CMSIS-DSP does a 1024-point real FFT at 48 kHz in about
    1–2 ms. That is about 21 ms of audio per window, enough for 30–60 updates
    per second with overlapping windows.
  - On an RP2040, use a fixed-point 512-point FFT instead.
  - Bands and styling are firmware-only changes.
- **DSP filter bank:**
  - SigmaStudio, as far as known, has no general FFT block for this chip, but
    8–16 band-pass filters, each with an RMS detector, give a classic constant-Q
    analyser.
  - Export the band envelopes as "audio" in TDM slots on OUT2, so the Pico just
    draws slot values with no maths.
  - Any band change requires a SigmaStudio rebuild, and the slots used for
    envelopes cannot also carry plain audio.

## Parked: more I2S sources on the mux

A wider mux takes more sources, one at a time. Two dual 4:1 bus switches
(74CBTLV3253) handle 4 sources, and a CPLD more. Each selected source must be
I2S clock master, or be clocked from a fixed 48 kHz BCLK/LRCLK that the Pico
generates.

Candidate tuners with I2S output, listed from memory without checking datasheets:

| Chip | Bands | Notes |
|---|---|---|
| Skyworks (ex-Silicon Labs) Si4684/Si4688/Si4689 | FM + DAB/DAB+ (Si4689 adds HD Radio) | Host uploads firmware at each boot; redistribution terms apply |
| NXP TEF6686 / TEF668x | AM/FM | I2C control, I2S out |
| Si4705 / Si4735 | FM (Si4735 adds AM/SW) | Check whether they can be I2S clock master |

The Beocreate uses the Pi's whole SPI bus, so the Pico would control tuners on
its own SPI/I2C. Keep Band III RF (174–240 MHz) on a shielded module with its own
antenna connector, away from class-D stages. Parked in favour of the Pico
acting only as the USB DAC.

## Pi ↔ Pico link choice

UART over I2C. Pi GPIO 2/3 are the HAT configuration bus. HiFiBerry discourages
extra devices on it, and the Pi 3 I2C controller has a clock-stretching bug that
the RP2040/RP2350 I2C target triggers. Slopify's Pi boot profile already sets
`dtoverlay=disable-bt`, which puts the PL011 UART on GPIO 14/15. Remove the
serial console from the kernel command line.
