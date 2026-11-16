# HiFiBerry Beocreate speaker build

Hardware proposal for a Slopify speaker built from a Raspberry Pi 3, a
HiFiBerry Beocreate 4-channel amplifier, a Raspberry Pi Pico 2, a Noritake
GU128X64-800B VFD and an ALPS RK27 motorized volume pot. Nothing here is built
or verified, and Slopify has no Beocreate, display or knob support yet.

| Document | Contents |
|---|---|
| [`pinout.md`](pinout.md) | Beocreate board, Pi GPIO usage, 34-pin DSP/GPIO connector, jumpers |
| [`noritake-gu128x64-800b.md`](noritake-gu128x64-800b.md) | VFD wiring, Pico pin budget, Pi ↔ Pico UART protocol, VU meter |
| [`motorized-volume-knob.md`](motorized-volume-knob.md) | RK27 dual-gang wiring, H-bridge, Pico position loop |
| [`design-notes.md`](design-notes.md) | Alternatives and decisions: USB audio routes, interposer details, Pico USB pads, one vs several Picos, VU/FFT options, parked tuner inputs |
| [`noritake-gu128x64-800b.pdf`](noritake-gu128x64-800b.pdf), [`alps-rk168-rk271-motor-pots.pdf`](alps-rk168-rk271-motor-pots.pdf) | Datasheets |

All variants share these parts:

- **Pi ↔ Pico link:** UART on Pi GPIO14/15, which Beocreate connector pins 24/25
  also carry. The Pi sends now-playing text and knob targets. The Pico reports
  knob position.
- **VFD:** the Pico drives it over SPI through a 5 V HCT buffer.
- **Volume knob:** gang B of the RK27 feeds DSP ADC0 (connector pins 5/9/10), so
  the DSP volume follows the knob with no software in the path. Gang A goes to
  the Pico ADC, and the Pico moves the knob through an H-bridge.
- **Power:** a separate 5 V buck converter (≥ 2 A) on the 12–20 V input powers the
  VFD, motor and Pico. The Beocreate's own converter powers only the Pi. All
  grounds are common.

## Variant A: Pico inline (USB DAC + interposer)

The Pico sits on an interposer board between the Pi and the Beocreate. A 2:1 mux
selects whether the Pi's I2S or the Pico's I2S reaches the DSP. The Pico is also
a USB audio device for an external host.

```
                       host PC
                          │ USB-B (optional ADuM3160 isolator)
                          ▼
 ┌──────────┐      ┌─────────────────────────────────────────┐      ┌──────────────────────────┐
 │ RPi 3    │ 40p  │ interposer                              │ 40p  │ Beocreate                │
 │          ├─────►│ 37 pins pass through (5V, SPI, I2C,     ├─────►│                          │
 │ Slopify  │      │ RESET, SELFBOOT, MUTE, UART)            │      │                          │
 │ player   │ I2S  │   ┌─────┐  I2S                         │ I2S  │ ASRC ► DSP ► 4× class-D ─┼──► speakers
 │          ├─────►├──►│ mux ├───────────┬─────────────────►├─────►│         ▲                │
 │          │      │   └──▲──┘           │ tap (listen)     │      │         │ ADC0          │
 │          │ GPIO26 SEL  │ I2S out      ▼                  │      └─────────┼────────────────┘
 │          ├─────►├──────┤        ┌───────────┐  SPI+HCT  │                │
 │          │ UART │      └────────┤  Pico 2   ├───────────┼──► VFD         │
 │          │◄────►├──────────────►│ USB audio │  PWM      │                │
 └──────────┘      │               │ VU meter  ├───────────┼──► H-bridge ──► motor ─┐
                   │               │ knob loop │◄─ gang A ─┼────────────────────────┤ RK27
                   │               └───────────┘           │                gang B ─┘──► ADC0
                   └─────────────────────────────────────────┘
```

- **Sources:** Slopify playback through the Pi, USB audio from a host through
  the Pico, and the Beocreate's own Toslink input. The mux selects one I2S source
  at a time. Mute with GPIO 27 around each switch.
- **VU meter:** taps the mux output, which covers both I2S sources at their
  native rate with no buffer lag. Toslink is not covered unless the DSP also
  routes its mix to OUT2 (variant B's tap).
- **Requirements:** the Pi must be the I2S clock master and the Beocreate's
  input ASRC must accept the Pico's rate. Confirm by checking whether GPIO18
  BCLK changes between 44.1 and 48 kHz playback. The interposer must carry about
  2.5 A of 5 V for the Pi, and the stack grows taller.
- **USB limit:** the Pico's USB is full-speed, so the USB DAC tops out at about 96 kHz/24-bit stereo.

## Variant B: Pico passive (listener, knob driver, VFD driver)

The Pi stacks directly on the Beocreate as usual. The Pico is not in the audio
path. It connects only to the Beocreate's 34-pin connector and its own
peripherals.

```
 ┌──────────┐ 40-pin, stacked directly  ┌──────────────────────────────────────────┐
 │ RPi 3    ├──────────────────────────►│ Beocreate                                │
 │          │ I2S, SPI, I2C, control    │ Pi I2S ──► ASRC ─┐                       │
 │ Slopify  │                           │ Toslink ─► ASRC ─┴► DSP ─► 4× class-D ───┼──► speakers
 │ player   │                           │                     │  ▲                 │
 └──────────┘                           │          SigmaStudio│  │ADC0             │
                                        │          mix → OUT2 ▼  │                 │
                                        │ 34-pin: 24/25 UART  15-17 OUT2  5/9/10 ADC0
                                        └────────────┬──────────┬──────────┬───────┘
                                                     │ UART     │ I2S      │
                                               ┌─────▼──────────▼───┐      │
                                               │ Pico 2             │      │
                                               │ VU meter (FFT)     │ SPI+HCT
                                               │ VFD driver         ├──────────► VFD
                                               │ knob loop          │ PWM
                                               │                    ├──────────► H-bridge ──► motor ─┐
                                               │                    │◄── gang A ─────────────────────┤ RK27
                                               └────────────────────┘                    gang B ─────┘──► ADC0
```

- **Sources:** Slopify playback through the Pi and the Beocreate's Toslink input.
  There is no USB DAC and no interposer.
- **VU meter:** a custom SigmaStudio program routes the signal to be metered (pre-
  or post-volume mix, or per-channel) to serial output 2. The Pico listens on
  connector pins 15/16/17 (LRCLK, BCLK, SDATA OUT2). This covers every source,
  including Toslink, always at the DSP rate (48 kHz by default). Check first
  whether the stock profile drives OUT2.
- **Requirements:** a custom DSP program for the OUT2 routing and for the
  ADC0 volume curve. No board modification is needed.

## Variant C: Pico on the DSP connector (variant B + USB DAC)

Same as variant B, but the Pico also sends its USB audio as I2S into DSP
serial input 1. Everything stays on the 34-pin connector. The Pi stacks
directly, and there is no interposer or mux.

```
 Beocreate 34-pin                         Pico 2
 15 LRCLK OUT2  ────────────────────────► GP11  clock in (DSP is master)
 16 BCLK  OUT2  ────────────────────────► GP10  clock in
 17 SDATA OUT2  ────────────────────────► GP12  VU tap (DSP mix, same clocks)
 19 SDATA IN1   ◄──────────── 33 Ω ────── GP9   USB audio as I2S
 22 GND         ───────────────────────── GND   (ribbon ≤ 10 cm, GND beside clocks)
 24/25 UART, 5/9/10 ADC0 knob gang B      as in variant B
```

- **DSP program:** clocks serial input 1 from output port 2's clocks, mixes or
  selects it with the Pi and Toslink inputs, routes the metered signal to OUT2
  data, and keeps the ADC0 volume curve.
- **Clocking:** the DSP is master, so the Pico's USB device offers only the DSP
  rate (48 kHz by default) and hosts resample. The Pico derives asynchronous USB
  feedback by counting LRCLK against the 1 kHz USB SOF. A Pico-master setup at
  native rates through a DSP ASRC is possible in principle but unverified (see
  [`design-notes.md`](design-notes.md)).
- **Sources:** Slopify through the Pi, USB through the Pico, and Toslink. Unlike
  the mux, the DSP can mix sources instead of only switching between them.
- **Pin conflict:** GP9–GP12 here replace variant A's Pico-master I2S output pins.
  The Pico reads BCLK/LRCLK as inputs.

## Choosing

| | A: Pico inline | B: Pico passive | C: Pico on DSP connector |
|---|---|---|---|
| Extra hardware | interposer PCB, mux, USB-B | ribbon to the 34-pin connector | ribbon + USB-B |
| USB DAC input | yes, native rates ≤ 96 kHz/24-bit | no | yes, DSP rate (48 kHz default) |
| Toslink input | yes, via the Beocreate | yes, via the Beocreate | yes, via the Beocreate |
| Source handling | switch one I2S source | — | DSP mixes or selects |
| VU coverage | Pi + USB; Toslink only via the DSP OUT2 tap | everything the DSP routes to OUT2 | everything the DSP routes to OUT2 |
| DSP reprogramming | knob curve only | knob curve + OUT2 routing | knob curve + OUT2 routing + IN1 input |
| Failure impact | Pico or mux fault can silence Pi playback | Pico fault only loses the display and motor; the knob still sets volume | same as B; the USB input also goes |
| Pi stack | taller, custom header | stock | stock |

Build variant B first. It changes nothing in the Pi-to-DSP audio path. Variant C
then adds the USB input with one more wire (pin 19) and DSP routing, without
new boards. Choose variant A only if the USB input must avoid DSP changes or
offer native 44.1/96 kHz rates.
