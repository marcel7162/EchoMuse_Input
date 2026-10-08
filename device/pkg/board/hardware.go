package board

import (
	"fmt"
	"sync"
)

// Hardware says where a board's parts are, by NAME (#541). The numbers the
// kernel hands out are enumeration order: event2 is the volume keys on
// biscuit and something else on every other board measured, and opening the
// wrong one succeeds and reads nothing.
//
// Each part also carries the number it has had on every unit of this board
// measured so far. It is used only when the name is not found, and says so in
// the log: a kernel build that names a part differently then behaves as the
// firmware always has, and tells us. A part with no fallback is not opened by
// number at all.
type Hardware struct {
	// DotKeys carries the action button and, on biscuit, the mute button
	// too. VolumeKeys the volume pair. Input device names, as
	// /proc/bus/input/devices gives them.
	DotKeys    Input
	VolumeKeys Input
	// MuteKeys carries the mute button when it is on a separate input
	// device from DotKeys. Zero value means mute is on DotKeys (biscuit)
	// or absent.
	MuteKeys Input
	// LEDRing is the ring driver's i2c client, by its sysfs `name`.
	LEDRing I2C
	// MuteLEDGPIO is the sysfs GPIO number of the LED under the mute button,
	// "" when the board has none we may drive. The same number is a
	// different SoC pin on a board with another gpiochip base, so this is
	// never assumed for a board that has not stated it.
	MuteLEDGPIO string
	// Capture is the mic array's PCM and Playback the speaker's, by the id
	// /proc/asound/pcm lists.
	Capture  PCM
	Playback PCM
	// LightSensor is the ambient light sensor. A zero value means the board
	// has none.
	LightSensor LightSensor
	// HCI is the Bluetooth controller's raw HCI character device, "" when
	// the board has none the firmware can drive. A node of the same name
	// can exist on a board whose radio is not behind it, so this is stated
	// per board and never assumed.
	HCI string
	// MicArray describes the capture layout: channel count, sample format,
	// mic geometry and echo reference. A zero value means the capture
	// pipeline will not start (needs hardware probing).
	MicArray MicArray
	// CodecRoutes are the DAPM routes EnsureRoutes must close for audio.
	CodecRoutes []CodecRoute
	// AdcMuteCtls are the mixer control names for muting capture channels.
	AdcMuteCtls []string
	// AdcVolumeCtls are the mixer control names for ADC digital volume.
	AdcVolumeCtls []string
	// AdcMicpgaCtls are the mixer control names for ADC MICPGA volume.
	AdcMicpgaCtls []string
}

// MicPosition is one microphone in a perimeter array.
type MicPosition struct {
	Ch    int     // ALSA channel number
	Angle float64 // degrees, clockwise from 12 o'clock
}

// MicArray describes a board's microphone capture configuration. A zero
// value (Channels == 0) means the capture pipeline will not start on this
// board — the layout needs hardware probing first.
type MicArray struct {
	Channels    int           // total ALSA channels (mics + any loopback)
	SampleBytes int           // bytes per sample: 2=S16_LE, 3=S24_3LE, 4=S32_LE
	SampleRate  int           // Hz
	PeriodSize  int           // frames per ALSA period
	PeriodCount int           // periods per ALSA ring buffer
	Perimeter   []MicPosition // perimeter mics for direction estimation
	CentreCh    int           // omnidirectional centre mic, -1 when none
	EchoRefCh   int           // hardware echo reference channel, -1 when none
}

// CodecRoute is a DAPM switch that must be closed for audio to flow.
type CodecRoute struct {
	Control string
	Value   string
}

// LightSensor is an ambient light sensor on the i2c bus.
type LightSensor struct {
	Driver string // the client's sysfs `name`
	Attr   string // the attribute, in the client's directory, that reads lux
}

// Input is an evdev device.
type Input struct {
	Name     string // exact device name
	Fallback string // /dev/input/eventN, "" for none
}

// I2C is an i2c client.
type I2C struct {
	Driver   string // the client's sysfs `name`
	Fallback string // sysfs directory, "" for none
}

// PCM is an ALSA PCM device.
type PCM struct {
	Name     string   // stream name: the id up to the DAI name
	Fallback *PCMAddr // nil for none
}

// PCMAddr is a PCM's card and device number.
type PCMAddr struct{ Card, Device int }

// biscuitHardware: names read off VVV (FireOS 5.5.5.4) on 2026-10-04, numbers
// as every build before this one opened them.
var biscuitHardware = &Hardware{
	DotKeys:     Input{Name: "mtk-kpd", Fallback: "/dev/input/event1"},
	VolumeKeys:  Input{Name: "keys", Fallback: "/dev/input/event2"},
	LEDRing:     I2C{Driver: "is31fl3236", Fallback: "/sys/devices/soc/11007000.i2c/i2c-0/0-003f"},
	MuteLEDGPIO: "444",
	Capture:     PCM{Name: "TLV320AIC3101 Capture", Fallback: &PCMAddr{0, 24}},
	Playback:    PCM{Name: "TLV320AIC3204 Playback", Fallback: &PCMAddr{0, 23}},
	LightSensor: LightSensor{Driver: "tsl2540", Attr: "als_lux"},
	// MediaTek's combo-chip (WMT) Bluetooth node.
	HCI: "/dev/stpbt",
	MicArray: MicArray{
		Channels:    9,
		SampleBytes: 3, // S24_3LE
		SampleRate:  16000,
		PeriodSize:  512,
		PeriodCount: 5,
		Perimeter: []MicPosition{
			{0, 330}, {1, 30}, {2, 90}, {3, 150}, {4, 210}, {5, 270},
		},
		CentreCh:  6,
		EchoRefCh: 8,
	},
	CodecRoutes: []CodecRoute{
		{"ADC_D Right Ip Select ADC_D DIF1_R switch", "1"},
		{"ADC_D Left Ip Select ADC_D DIF1_L switch", "1"},
		{"ADC_C Right Ip Select ADC_C DIF1_R switch", "1"},
		{"ADC_C Left Ip Select ADC_C DIF1_L switch", "1"},
		{"ADC_B Right Ip Select ADC_B DIF1_R switch", "1"},
		{"ADC_B Left Ip Select ADC_B DIF1_L switch", "1"},
		{"ADC_A Right Ip Select ADC_A DIF1_R switch", "1"},
		{"ADC_A Left Ip Select ADC_A DIF1_L switch", "1"},
		{"HPR Output Mixer R_DAC Switch", "1"},
		{"HPL Output Mixer L_DAC Switch", "1"},
	},
	AdcMuteCtls: []string{
		"ADC_A Left Mute", "ADC_A Right Mute",
		"ADC_B Left Mute", "ADC_B Right Mute",
		"ADC_C Left Mute", "ADC_C Right Mute",
		"ADC_D Left Mute", "ADC_D Right Mute",
	},
	AdcVolumeCtls: []string{
		"ADC_A Digital Volume Control",
		"ADC_B Digital Volume Control",
		"ADC_C Digital Volume Control",
		"ADC_D Digital Volume Control",
	},
	AdcMicpgaCtls: []string{
		"ADC_A MICPGA Volume Ctrl",
		"ADC_B MICPGA Volume Ctrl",
		"ADC_C MICPGA Volume Ctrl",
		"ADC_D MICPGA Volume Ctrl",
	},
}

// cupcakeHardware: names read by marcel7162 off a replacement-eMMC unit on
// 2026-10-08. No fallback values — new boards do not get any.
var cupcakeHardware = &Hardware{
	DotKeys:  Input{Name: "gpio-keys"},
	MuteKeys: Input{Name: "gpio-privacy"},
	// No volume buttons on the Echo Input.
	LEDRing: I2C{Driver: "lp5562"},
	// No MuteLEDGPIO: mute is handled by the gpio-privacy input driver.
	Capture:  PCM{Name: "TDM_Capture"},
	Playback: PCM{Name: "DL1_Playback"},
	// tsl2572 light sensor uses IIO; not readable by the current als binding.
	HCI: "/dev/stpbt",
}

// Layout is a board's Hardware resolved on the running device: what the
// bindings open.
type Layout struct {
	// Board is nil on a device nothing identified. Its layout is then
	// biscuit's, which is what the firmware assumed of every device before
	// boards were told apart.
	Board *Board

	DotKeys    string // /dev/input/eventN, "" when not found
	VolumeKeys string
	MuteKeys   string // separate mute device, "" when mute is on DotKeys
	LEDRing    string // sysfs directory, "" when not found
	// MuteLEDGPIO is "" when the board has no mute LED GPIO to drive.
	MuteLEDGPIO string
	Capture     *PCMAddr // nil when not found
	Playback    *PCMAddr
	// LightSensor and HCI are the board's own statements, not resolved here:
	// the sensor is looked for by internal/bindings/als, which retries, and
	// the HCI device is opened by internal/bluetooth only when the proxy is
	// switched on.
	LightSensor LightSensor
	HCI         string

	// MicArray, CodecRoutes and the ADC control lists are static board
	// properties, copied from Hardware so consumers read one struct.
	MicArray      MicArray
	CodecRoutes   []CodecRoute
	AdcMuteCtls   []string
	AdcVolumeCtls []string
	AdcMicpgaCtls []string

	// Notes has one line per part saying how it was found, for the log.
	Notes []string
	// Problems is the subset of Notes for parts NOT found by name: opened by
	// their old number, or not available. Sent to the controller, because the
	// device's own log is RAM-backed and nobody reads it on a working Echo.
	Problems []string
}

// Resolve finds each part of hw beneath root ("" on a device).
func Resolve(root string, b *Board) *Layout {
	hw := biscuitHardware
	if b != nil && b.Hardware != nil {
		hw = b.Hardware
	}
	l := &Layout{
		Board: b, MuteLEDGPIO: hw.MuteLEDGPIO, LightSensor: hw.LightSensor, HCI: hw.HCI,
		MicArray: hw.MicArray, CodecRoutes: hw.CodecRoutes, AdcMuteCtls: hw.AdcMuteCtls,
		AdcVolumeCtls: hw.AdcVolumeCtls, AdcMicpgaCtls: hw.AdcMicpgaCtls,
	}
	note := func(part, want, got string, err error) {
		var line string
		switch {
		case err == nil:
			l.Notes = append(l.Notes, fmt.Sprintf("%s: %q at %s", part, want, got))
			return
		case got != "":
			line = fmt.Sprintf("%s: %v; using %s as on every unit measured", part, err, got)
		default:
			line = fmt.Sprintf("%s: %v; not available", part, err)
		}
		l.Notes = append(l.Notes, line)
		l.Problems = append(l.Problems, line)
	}
	input := func(part string, in Input) string {
		p, err := InputEvent(root, in.Name)
		if err != nil {
			p = in.Fallback
		}
		note(part, in.Name, p, err)
		return p
	}
	pcm := func(part string, want PCM, capture bool) *PCMAddr {
		a, err := PCMDevice(root, want.Name, capture)
		if err != nil {
			a = want.Fallback
		}
		got := ""
		if a != nil {
			got = fmt.Sprintf("card %d device %d", a.Card, a.Device)
		}
		note(part, want.Name, got, err)
		return a
	}
	l.DotKeys = input("dot keys", hw.DotKeys)
	l.VolumeKeys = input("volume keys", hw.VolumeKeys)
	if hw.MuteKeys.Name != "" {
		l.MuteKeys = input("mute keys", hw.MuteKeys)
	}
	dir, err := I2CDevice(root, hw.LEDRing.Driver)
	if err != nil {
		dir = hw.LEDRing.Fallback
	}
	note("led ring", hw.LEDRing.Driver, dir, err)
	l.LEDRing = dir
	l.Capture = pcm("capture", hw.Capture, true)
	l.Playback = pcm("playback", hw.Playback, false)
	return l
}

var (
	currentOnce   sync.Once
	current       *Board
	currentLayout *Layout
)

func resolveCurrent() {
	currentOnce.Do(func() {
		current = Detect("")
		currentLayout = Resolve("", current)
	})
}

// Current is the board the firmware is running on, detected once. Nil when
// nothing identified it.
func Current() *Board {
	resolveCurrent()
	return current
}

// CurrentLayout is Current's hardware as found on this device, resolved once.
func CurrentLayout() *Layout {
	resolveCurrent()
	return currentLayout
}
