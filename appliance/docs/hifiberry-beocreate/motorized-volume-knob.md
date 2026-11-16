# Motorized volume knob (Beocreate DSP + Pico)

Hardware proposal for an ALPS RK27 stereo (dual-gang) motorized potentiometer,
20 kΩ with a D shaft. One gang sets the Beocreate DSP volume directly through its
auxiliary ADC. The other gang gives the Pico position feedback, so the Pico
can move the knob with an H-bridge when volume changes elsewhere. Slopify has no
support for this yet. See [`pinout.md`](pinout.md) for
the connector pinout and [`noritake-gu128x64-800b.md`](noritake-gu128x64-800b.md)
for the rest of the Pico design.

## Part: ALPS RK27 (RK27112MC family)

From the ALPS RK168/RK271 motor-driven series catalog
([`alps-rk168-rk271-motor-pots.pdf`](alps-rk168-rk271-motor-pots.pdf)):

| Item | RK271 |
|---|---|
| Rotation | 300 ± 5°, with a slip clutch at both ends and for hand operation |
| Motor | 4–6 V DC, nominal 4.5 V |
| Motor current | 100 mA rotating, 150 mA at end slip |
| Speed | **12 ± 3 s per full 300° at 4.5 V** |
| Rotational torque | 15–45 mN·m |
| Resistance tolerance | ±20 % |
| Max signal voltage | 30 V AC |
| Life | 15,000 cycles |

The catalog lists the RK27112MC with a "15A" taper: logarithmic, 15 % of the
resistance at mid-rotation. The 20 kΩ version is not in that table. Check
yours: at mid-rotation, wiper-to-CCW-end resistance of about 3 kΩ means log
(A), about 10 kΩ means linear (B). The motor terminals are separate from both
gangs.

## Principle: the knob is the volume

```
                 ┌──────────── gang B (DSP) ─────────────┐
 hand ──► knob ──┤                                       ├──► Beocreate ADC0 ──► DSP volume
   ▲             └──────────── gang A (feedback) ────────┘
   │                                   │
 motor ◄── H-bridge ◄── Pico ◄─────────┘ (GP26 ADC0)
                        │  ▲
         UART report    ▼  │  UART "move to"
                    Raspberry Pi (Slopify player) ◄──► players service ◄──► web UI
```

- **Hand turn:** the DSP follows gang B instantly with no software in the path. The
  Pico sees gang A move while the motor is idle and reports the new position.
  The Pi then publishes the level to the web UI.
- **Web or remote change:** the Pi sends a target position. The Pico drives the
  motor until gang A reaches it, and gang B and the DSP follow mechanically.
- **Single source of truth:** there is one volume state, the physical knob
  position. Nothing in software can disagree with what the DSP hears.
- **Restart behaviour:** the Pico or Pi can restart without changing the volume.
  At startup the Pico reports where the knob is. It must never move the knob to
  an "initial level" at boot.

## Wiring

```
 Gang A (feedback)                       Gang B (volume)
 ─────────────────                       ───────────────
 end 1 ── Pico ADC_VREF / 3V3            end 1 ── Beocreate conn. pin 10 (+3.3V ADC)
 wiper ── 1 kΩ ──┬── Pico GP26 (ADC0)    wiper ── Beocreate conn. pin 5  (DSP ADC0)
                 └── 100 nF ── AGND      end 2 ── Beocreate conn. pin 9  (GND)
 end 2 ── Pico AGND (pin 33)

 Motor                                    H-bridge (DRV8833 or TB6612FNG)
 ─────                                    ───────────────────────────────
 M+ ─┬─ AOUT1                             AIN1 ── Pico GP16 (PWM0 A)
     ├─ 100 nF across motor terminals      AIN2 ── Pico GP17 (PWM0 B)
 M− ─┴─ AOUT2                             nSLEEP ── Pico GP18 (optional; low = off)
                                          VM ── 5 V motor supply, GND ── common GND
```

- **Gang isolation:** keep the gangs electrically separate. Gang B is referenced
  only to the Beocreate's ADC supply and ground. Gang A is referenced only to the
  Pico's analog supply. Do not join their ends.
- **Taper:** with the usual log (A) taper, the gang B voltage is already roughly
  proportional to amplitude. The DSP program can use the ADC value as a linear
  gain factor, or map it through a table to dB with a mute floor. Gang A follows
  the same curve, so the Pico converts its reading to rotation with the inverse
  taper table before running the position loop. The bottom half of the travel
  covers only about 15 % of the ADC range, which still leaves several counts per
  degree on the 12-bit ADC. With a linear (B) part, skip the inverse table and put
  the whole dB curve in the DSP program. Either way, the Pi uses the same table to
  report dB.
- **Load:** a 20 kΩ gang across 3.3 V draws about 165 µA. That is negligible for
  both the Pico supply and the Beocreate's +3.3V ADC pin. The wiper source impedance
  peaks at about 5 kΩ, which the RC filter and the DSP auxiliary ADC tolerate.
- **Gang tracking:** typical gang tracking error is a few percent of travel.
  Volume reported from gang A can therefore differ slightly from what the DSP
  applies, mostly near the bottom of the curve. Calibrate both gangs against the
  DSP's ADC reading once. The Pi can also read the DSP ADC cell over SPI for an
  exact value.
- **Motor supply:** take the motor supply from the same 5 V buck converter as the
  VFD, not from the Pi or Pico. 5 V is within the motor's 4–6 V range. Cap PWM
  at about 90 % to approximate the 4.5 V nominal. At 150 mA end-slip current,
  a DRV8833 (1.5 A) or TB6612FNG (1.2 A) has ample margin, and both accept
  3.3 V logic.
- **Noise:** brushed motors are noisy. Use twisted motor leads, the capacitor
  across the terminals, a separate return to the supply, and keep the motor
  wiring away from gang B, the ADC lines and I2S.

## Pico control loop

- **PWM:** 20 kHz on GP16/GP17 (inaudible). Drive one input and hold the other low.
  Both high is brake.
- **Control:** proportional control with a minimum duty to overcome
  clutch friction. Slow down within about 5 % of the target, brake at a deadband of
  about 0.5–1 % of travel, then coast so the clutch is free for a hand.
- **Filtering:** oversample GP26 and average to about 10-bit stable readings.
  Ignore readings during PWM edges if the motor couples noise into the ADC.
- **Speed:** the RK27 is slow, about 120 ms per 1 % of travel at full drive (90–150 ms within tolerance). A
  10 % web volume step takes about 1.2 s, and a full sweep 9–15 s. Size timeouts
  for that, and let the web UI show the target while the knob catches up.
- **Stall timeout:** abort if the target is not reached within travel × 200 ms per
  percent plus 1 s (about 21 s for a full sweep), or if position changes by less
  than 0.5 % within 1 s while driven. The end-stop slip clutch protects the
  mechanism, but don't keep driving into it.
- **Hand wins:** while moving, if position changes against the drive direction,
  stop at once and treat the knob as manually set. Never fight the user.
- **Manual turn:** when the motor is idle, a change beyond the deadband is
  manual. Report it at once, then coalesce further reports to 20–50 Hz.

## Pi ↔ Pico messages

These extend the line protocol on the same UART as the display:

```
Pi → Pico:  K <0..1000>      move knob to position (per-mille of travel)
Pico → Pi:  k <0..1000> m    manual position (hand turned)
            k <0..1000> r    reached commanded target
            k <0..1000> s    stalled / aborted (reports actual position)
```

The Pi converts position to dB with the DSP program's curve and reports
dB-only levels to the players service, as other Slopify volume backends do.
This needs a new opt-in player volume backend; the current one is ALSA-mixer
only.
