# HiFiBerry Beocreate 4-Channel Amplifier pinout

Reference notes for using a Beocreate board as a Slopify device output. They
summarize HiFiBerry's published documentation. Slopify has no Beocreate-specific
support. Check pin 1 with a multimeter before wiring anything.

Sources:

- [Beocreate – GPIOs](https://hifiberry.com/beocreate/beocreate-doc/beocreate-gpios)
- [Beocreate – Hardware documentation](https://www.hifiberry.com/beocreate/beocreate-doc/beocreate-hardware-documentation/)
- [Beocreate product page](https://www.hifiberry.com/shop/boards/beocreate/)

## Board overview

```
            Raspberry Pi (40-pin header)
   I2S 18-21 │  SPI 7-11 │  I2C 2,3 │ RESET 17 │ SELFBOOT 22 │ MUTE 27
             ▼           ▼          ▼          ▼             ▼          ▼
 Toslink ┌──────┐   ┌──────────────────────────────┐   ┌─────────────────┐
 in ────►│ ASRC ├──►│ DSP (ADAU145x family, 294 MHz│──►│ Class-D stages  │──► 2×60 W (bottom, 2 Ω ok)
         └──────┘   │ 16 kB data / 8 kB prog RAM,  │   │ (MUTE jumper /  │──► 2×30 W (top)
 Pi I2S ──► ASRC ──►│ fixed 48/96/192 kHz core,    │   │  GPIO 27)       │
                    │ EEPROM selfboot)             │   └─────────────────┘
                    └───────┬──────────────────────┘
                            ├──► Toslink out (daisy chain)
                            └──► 34-pin GPIO connector (ADC0-2, MP6/7, I2S OUT2/3, IN1/2, MCLK)
Power: 12–20 V DC, barrel jack 5.5×2.1 mm (centre +) **or** screw terminal, never both.
```

HiFiBerry's pages refer only to "ADAU145x" register settings. The community
commonly identifies the chip as the ADAU1451, but the published pages do not name
the exact part, the DAC or the amplifier ICs. The DSP core runs at a fixed
sample rate selected by jumper, 48 kHz by default. Raspberry Pi and S/PDIF
inputs pass through asynchronous sample-rate converters, so the Beocreate does
**not** preserve Slopify's source sample rate end to end. The board's DSP
profiles program it over SPI/SigmaTCP.

The board has an onboard step-down converter. It powers the Pi from the 12–20 V
input through the 40-pin header, so do not also power the Pi over USB.
HiFiBerry does not publish the converter's current rating. Power large add-ons
such as a VFD or motor from a separate 5 V buck converter on the same input.

## Raspberry Pi GPIOs (BCM numbering)

```
                 3V3  (1) (2)  5V
 I2C SDA ■ GPIO2      (3) (4)  5V
 I2C SCL ■ GPIO3      (5) (6)  GND
       □ GPIO4        (7) (8)  GPIO14 ◆ UART TX  → conn. pin 24
                 GND  (9) (10) GPIO15 ◆ UART RX  → conn. pin 25
 RESET   ■ GPIO17    (11) (12) GPIO18 ■ I2S BCLK
 MUTE    ■ GPIO27    (13) (14) GND
 SELFBOOT■ GPIO22    (15) (16) GPIO23 ● → conn. pin 26
                 3V3 (17) (18) GPIO24 ● → conn. pin 27
 SPI MOSI■ GPIO10    (19) (20) GND
 SPI MISO■ GPIO9     (21) (22) GPIO25 ● → conn. pin 28
 SPI SCLK■ GPIO11    (23) (24) GPIO8  ■ SPI CE0
                 GND (25) (26) GPIO7  ■ SPI CE1
 HAT ID  ✕ GPIO0     (27) (28) GPIO1  ✕ HAT ID
       ● GPIO5       (29) (30) GND               (GPIO5 → conn. pin 29)
       ● GPIO6       (31) (32) GPIO12 ●          (GPIO6 → conn. pin 31, GPIO12 → conn. pin 30)
       □ GPIO13      (33) (34) GND
 I2S LRCLK■ GPIO19   (35) (36) GPIO16 ●          (GPIO16 → conn. pin 32)
       □ GPIO26      (37) (38) GPIO20 ■ I2S DIN (to board)
                 GND (39) (40) GPIO21 ■ I2S DOUT (from Pi)

 ■ used by Beocreate        ● free, broken out on the 34-pin connector
 ◆ free if the serial console/UART is disabled; broken out
 □ free, not broken out (Pi header only)   ✕ reserved for HAT ID EEPROM
```

Used by the board and unavailable for anything else:

| BCM GPIO | Function | Notes |
|---|---|---|
| 2, 3 | I2C SDA/SCL | Also routed to conn. pins 3/4 and the extra-GPIO jumper; can be shared with I2C peripherals at distinct addresses |
| 7, 8, 9, 10, 11 | SPI CE1/CE0/MISO/MOSI/SCLK | DSP control and programming |
| 17 | RESET | 1 = reset, 0 = run |
| 18, 19, 20, 21 | I2S BCLK/LRCLK/DIN/DOUT | Audio to and from the DSP |
| 22 | SELFBOOT | Boot the DSP from its EEPROM after reset |
| 27 | MUTE | 1 = muted, 0 = playing (power stages) |

Free GPIOs:

| BCM GPIO | Header pin | 34-pin connector pin | Notes |
|---|---|---|---|
| 5 | 29 | 29 | |
| 6 | 31 | 31 | |
| 12 | 32 | 30 | PWM0-capable |
| 14 | 8 | 24 | UART TX: disable the serial console first |
| 15 | 10 | 25 | UART RX: disable the serial console first |
| 16 | 36 | 32 | |
| 23 | 16 | 26 | |
| 24 | 18 | 27 | |
| 25 | 22 | 28 | |
| 4 | 7 | — | Pi header only |
| 13 | 33 | — | Pi header only |
| 26 | 37 | — | Pi header only |

The Pi's GPIOs use 3.3 V logic. Never connect the 5 V pins to a GPIO. Ground
is available on connector pins 2, 9, 22 and 34.

GPIO 4, 13 and 26 are reachable only if the Pi header is accessible, for example
through a stacking header or an interposer board. The board does not route them.

## 34-pin DSP/GPIO connector

Pin 1 is the upper-right pin, near the "3 from P3" marking, when the board faces
you with the speaker terminals on the right. Odd pins are in one row and even
pins in the other.

```
          ┌─────────────────────────────┬──────────────────────────────┐
  pin 1   │ +5V                       1 │ 2  GND                       │
          │ RPI I2C SDA (GPIO2)       3 │ 4  RPI I2C SCL (GPIO3)       │
          │ DSP ADC0                  5 │ 6  DSP ADC1                  │
          │ DSP ADC2                  7 │ 8  —                         │
          │ GND                       9 │ 10 +3.3V ADC (ref for pots)  │
          │ DSP MP6                  11 │ 12 DSP MP7                   │
          │ —                        13 │ 14 —                         │
          │ DSP LRCLK OUT2           15 │ 16 DSP BCLK OUT2             │
          │ DSP SDATA OUT2           17 │ 18 DSP SDATA OUT3            │
          │ DSP SDATA IN1            19 │ 20 DSP SDATA IN2             │
          │ DSP MCLK                 21 │ 22 GND                       │
          │ —                        23 │ 24 RPI GPIO14 (UART TX)      │
          │ RPI GPIO15 (UART RX)     25 │ 26 RPI GPIO23                │
          │ RPI GPIO24               27 │ 28 RPI GPIO25                │
          │ RPI GPIO5                29 │ 30 RPI GPIO12                │
          │ RPI GPIO6                31 │ 32 RPI GPIO16                │
          │ +5V                      33 │ 34 GND                       │
          └─────────────────────────────┴──────────────────────────────┘
```

DSP-side signals:

- **ADC0–ADC2**: the DSP's auxiliary ADC inputs, intended for potentiometers.
  Wire each pot between GND (pin 9) and +3.3V ADC (pin 10), with the wiper on
  ADCn. The DSP program reads them, not the Pi.
- **MP6, MP7**: DSP multipurpose pins, usable as GPIO for DSP-side buttons or
  LEDs once the DSP program configures them.
- **I2S OUT2/OUT3, IN1/IN2, MCLK**: extra DSP serial ports for digital audio
  expansion, such as more DACs or another digital source. Only OUT2 has its own
  LRCLK/BCLK pins, so the other data lines presumably use those clocks too.
  HiFiBerry does not document this.

HiFiBerry does not specify ADC input ranges, MP pin drive limits or logic
levels for the DSP pins. Treat them as 3.3 V and check the ADAU1451 datasheet
before relying on them.

## Extra I2S inputs

The Pi's I2S/PCM peripheral is only available on GPIO 18–21 on the 40-pin
header, and the board already uses those. None of the free Pi GPIOs can carry
I2S, and bit-banging it is not practical.

An external I2S source can feed the DSP through connector pins 19/20
(SDATA IN1/IN2), with these conditions:

- Only LRCLK/BCLK OUT2 (pins 15/16) are broken out. The DSP's input clock pins
  are not. The simplest setup clocks serial input 1 from port 2's clocks and
  makes the external device a clock slave to them, at the DSP's fixed rate.
  Making the external device clock master would require reconfiguring OUT2's
  clock pins as inputs, which is unverified. HiFiBerry does not document this
  wiring.
- Stock Beocreate DSP profiles do not use IN1/IN2. A custom SigmaStudio program
  must configure the serial input port and route it.
- Use 3.3 V logic and keep the wires short.

Without DSP reprogramming, the supported second digital input is Toslink. It
passes through the board's ASRC. With DSP reprogramming, see variant C in
[`README.md`](README.md). An interposer that switches the Pi's I2S lines
(variant A) avoids DSP changes to the audio routing.

## Other jumpers

| # | Jumper | Notes |
|---|---|---|
| 1 | Selfboot | DSP loads from its EEPROM; unused on release 2.x |
| 3 | 48/96/192 kHz | DSP core sample rate; default 48 kHz |
| 4 | MUTE | Hardware mute; HiFiBerry recommends removing it |
| 5 | Output voltage limiter | Limits the 30 W channels; removed on release 2.x |
| 6 / 8 | Power switch bypass / switch | VIN–VSW; fit 6 if no switch is wired to 8 |
| 7 | Extra GPIO | Top: I2C SCL/SDA; bottom: GPIO15/14 |
