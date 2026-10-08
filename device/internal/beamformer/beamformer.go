// Package beamformer implements directional mic selection for multi-mic
// arrays. The geometry, channel map, and sample format are supplied via
// Config at construction — nothing is hardcoded to a single board.
//
// # Algorithm
//
// Direction estimation always runs — smoothers update on every Process() call
// regardless of BeamformingEnabled. This keeps the baseline warm so Lock()
// gets a meaningful onset ratio the instant beamforming is turned on.
//
// Output channel is determined by lock state, not by the config flag:
//   - Unlocked: always the centre/omni channel. Covers OWW listening and any
//     turn where Lock() was a no-op (beamforming disabled).
//   - Locked: the perimeter mic selected at Lock() time, or the mic nearest
//     to BeamAngle if a fixed steering direction is configured.
//
// BeamformingEnabled only gates Lock() — if false, Lock() is a no-op and
// the device stays on the centre channel for both OWW and voice turns.
//
// # Direction estimation
//
// Two parallel smoothers run continuously:
//
//   - energySmooth (α=0.9, ~320ms): fast, tracks speech onset
//   - energyBaseline (α=0.995, ~10s): slow, tracks steady background noise
//
// At Lock() time, the direction with the highest ratio of energySmooth to
// energyBaseline is selected. This picks the direction that just had a
// sudden energy increase (speech onset) rather than the direction with the
// highest absolute energy (TV, fan, etc.).
//
// Direction is also exposed for LED ring visualisation.
package beamformer

import (
	"log"
	"math"
)

const (
	// Smoothing constants
	smoothAlpha   = 0.9   // fast smoother (~320ms time constant at 32ms/period)
	baselineAlpha = 0.995 // slow smoother (~10s time constant) — tracks background noise

	// Lock-back window. Controller-side wake detection lands 300–500ms
	// after the wake word ends, by which time the fast smoother's onset
	// spike has largely decayed — selecting on the *present* picks a mic
	// unrelated to the speaker. Instead Lock() looks back over a ring of
	// per-direction period energies covering the whole wake word plus the
	// detection latency, and scores each direction by its energy burst
	// within that window relative to its noise baseline.
	historyPeriods = 64 // ~2.0s at 32ms/period
	burstTopN      = 8  // periods averaged for a direction's burst (~256ms)
)

// Config supplies the board's mic-array layout to New.
type Config struct {
	Channels        int       // total ALSA channels
	SampleBytes     int       // bytes per sample (2, 3 or 4)
	PeriodFrames    int       // frames per ALSA period
	DirectionChs    []int     // ALSA channel of each perimeter mic
	DirectionAngles []float64 // angle of each perimeter mic (degrees CW from north)
	CentreCh        int       // omnidirectional centre mic, -1 when none
	EchoRefCh       int       // hardware echo reference channel, -1 when none
}

// Beamformer holds direction estimation state and locked mic selection.
type Beamformer struct {
	// Config-derived
	channels    int
	sampleBytes int
	frameSize   int // channels * sampleBytes
	periodFrames int
	nDirections int
	dirChs      []int
	dirAngles   []float64
	centreCh    int
	echoRefCh   int
	gainShift   uint    // 12 + (sampleBits - 16)
	sampleScale float32 // 2^(sampleBits-1) for float normalisation

	// energySmooth: fast EWMA of per-direction HF energy (~320ms time constant).
	energySmooth []float64

	// energyBaseline: slow EWMA of per-direction HF energy (~10s time constant).
	energyBaseline []float64

	// baselineReady counts periods until baseline is initialised (~3s warmup).
	baselineReady int

	// energyHistory is a ring of per-period, per-direction HF energies —
	// the lock-back window (see constants above). Written every Process()
	// period while unlocked; frozen during a locked turn so a follow-up
	// continuation lock still sees the window around the last utterance
	// rather than only what came after it.
	energyHistory [][]float64 // [historyPeriods][nDirections]
	historyIdx    int
	historyCount  int

	// wakeMic selects the unlocked output: 0 = centre, 1..N = perimeter
	// mic by direction index+1. Set from the streaming goroutine that
	// calls Process, so it needs no lock.
	wakeMic int

	// lockedChannel is the ALSA channel selected at gate open.
	// -1 means unlocked (use live best-direction selection).
	lockedChannel int

	// clippedSamples counts output samples clamped to int16 range by the
	// mic gain in extractChannel.
	clippedSamples uint64

	// Reusable per-period analysis buffers.
	chanBuf [][]float32
	hfBuf   [][]float32
}

// New creates a Beamformer from the board's mic-array configuration.
func New(cfg Config) *Beamformer {
	nd := len(cfg.DirectionChs)
	b := &Beamformer{
		channels:     cfg.Channels,
		sampleBytes:  cfg.SampleBytes,
		frameSize:    cfg.Channels * cfg.SampleBytes,
		periodFrames: cfg.PeriodFrames,
		nDirections:  nd,
		dirChs:       cfg.DirectionChs,
		dirAngles:    cfg.DirectionAngles,
		centreCh:     cfg.CentreCh,
		echoRefCh:    cfg.EchoRefCh,
		gainShift:    uint(cfg.SampleBytes*8 - 4),
		sampleScale:  float32(int64(1) << uint(cfg.SampleBytes*8-1)),
		lockedChannel: -1,
	}
	if nd > 0 {
		b.energySmooth = make([]float64, nd)
		b.energyBaseline = make([]float64, nd)
		b.energyHistory = make([][]float64, historyPeriods)
		for i := range b.energyHistory {
			b.energyHistory[i] = make([]float64, nd)
		}
		b.chanBuf = make([][]float32, nd)
		b.hfBuf = make([][]float32, nd)
		for ci := 0; ci < nd; ci++ {
			b.chanBuf[ci] = make([]float32, cfg.PeriodFrames)
			b.hfBuf[ci] = make([]float32, cfg.PeriodFrames)
		}
	}
	return b
}

// Lock selects the mic with the highest energy onset relative to its noise
// floor baseline and holds it until Unlock.
//
// enabled is the BeamformingEnabled config flag. If false, Lock() is a no-op
// and the beamformer continues outputting the centre channel. Smoothers always
// run so the baseline is warm if beamforming is later enabled.
func (b *Beamformer) Lock(enabled bool) {
	if !enabled || b.nDirections == 0 {
		if b.nDirections > 0 {
			log.Printf("[beam] Lock() called but beamforming disabled — staying on centre (omni)")
		}
		return
	}
	if b.lockedChannel >= 0 {
		return
	}

	best := 0
	var bestScore float64
	switch {
	case b.baselineReady >= 100 && b.historyCount >= burstTopN*2:
		bestScore = b.burstRatio(0)
		for di := 1; di < b.nDirections; di++ {
			r := b.burstRatio(di)
			if r > bestScore {
				bestScore = r
				best = di
			}
		}
		log.Printf("[beam] locked to ch%d (%.0f°) burst_ratio=%.2f (lock-back over %d periods)",
			b.dirChs[best], b.dirAngles[best], bestScore, b.historyCount)
	case b.baselineReady >= 100:
		bestScore = b.onsetRatio(0)
		for di := 1; di < b.nDirections; di++ {
			r := b.onsetRatio(di)
			if r > bestScore {
				bestScore = r
				best = di
			}
		}
		log.Printf("[beam] locked to ch%d (%.0f°) onset_ratio=%.2f",
			b.dirChs[best], b.dirAngles[best], bestScore)
	default:
		bestScore = b.energySmooth[0]
		for di := 1; di < b.nDirections; di++ {
			if b.energySmooth[di] > bestScore {
				bestScore = b.energySmooth[di]
				best = di
			}
		}
		log.Printf("[beam] locked to ch%d (%.0f°) energy=%.4f (baseline not ready)",
			b.dirChs[best], b.dirAngles[best], bestScore)
	}

	b.lockedChannel = b.dirChs[best]
}

func (b *Beamformer) onsetRatio(di int) float64 {
	baseline := b.energyBaseline[di]
	if baseline < 1e-10 {
		return b.energySmooth[di]
	}
	return b.energySmooth[di] / baseline
}

func (b *Beamformer) burstRatio(di int) float64 {
	n := b.historyCount
	if n > historyPeriods {
		n = historyPeriods
	}
	var top [burstTopN]float64
	for i := 0; i < n; i++ {
		v := b.energyHistory[i][di]
		for j := 0; j < burstTopN; j++ {
			if v > top[j] {
				v, top[j] = top[j], v
			}
		}
	}
	count := burstTopN
	if n < burstTopN {
		count = n
	}
	var burst float64
	for j := 0; j < count; j++ {
		burst += top[j]
	}
	burst /= float64(count)

	baseline := b.energyBaseline[di]
	if baseline < 1e-10 {
		return burst
	}
	return burst / baseline
}

// SetWakeMic picks the mic the unlocked path reads: 0 = centre, 1..N =
// perimeter mic by direction index+1. Anything else keeps the centre.
func (b *Beamformer) SetWakeMic(m int) {
	if m < 0 || m > b.nDirections {
		m = 0
	}
	b.wakeMic = m
}

func (b *Beamformer) omniChannel() int {
	if b.wakeMic >= 1 && b.wakeMic <= b.nDirections {
		return b.dirChs[b.wakeMic-1]
	}
	return b.centreCh
}

// Unlock releases the locked mic selection.
func (b *Beamformer) Unlock() {
	if b.lockedChannel >= 0 {
		log.Printf("[beam] unlocked from ch%d", b.lockedChannel)
	}
	b.lockedChannel = -1
}

// Process returns mono S16_LE audio and the estimated source angle.
//
// gain is the linear fixed mic gain applied to the full-depth samples during
// S16 extraction — see extractChannel. Direction estimation is unaffected:
// it runs on energy ratios, which are gain-invariant.
//
// Output channel is determined by lock state alone:
//   - Unlocked (lockedChannel == -1): always centre/omni.
//   - Locked, steerAngle >= 0: mic nearest to steerAngle.
//   - Locked, steerAngle < 0: the perimeter mic selected at Lock() time.
func (b *Beamformer) Process(raw []byte, steerAngle float64, gain float64) (mono []byte, angle float64) {
	if b.nDirections == 0 || len(raw) < b.periodFrames*b.frameSize {
		return b.extractChannel(raw, b.omniChannel(), gain), -1
	}

	b.decodeChannels(raw)
	b.bandDiff()

	for di := 0; di < b.nDirections; di++ {
		energy := hfEnergy(b.hfBuf, di)
		b.energySmooth[di] = smoothAlpha*b.energySmooth[di] + (1-smoothAlpha)*energy

		if b.lockedChannel < 0 {
			b.energyBaseline[di] = baselineAlpha*b.energyBaseline[di] + (1-baselineAlpha)*energy
			b.energyHistory[b.historyIdx][di] = energy
		}
	}
	if b.lockedChannel < 0 {
		b.historyIdx = (b.historyIdx + 1) % historyPeriods
		if b.historyCount < historyPeriods {
			b.historyCount++
		}
	}

	if b.baselineReady < 100 {
		b.baselineReady++
	}

	if b.lockedChannel < 0 {
		return b.extractChannel(raw, b.omniChannel(), gain), -1
	}

	bestDir := 0
	for di := 1; di < b.nDirections; di++ {
		if b.energySmooth[di] > b.energySmooth[bestDir] {
			bestDir = di
		}
	}

	var ch int
	if steerAngle >= 0 {
		fixedDir := b.nearestDirection(steerAngle)
		ch = b.dirChs[fixedDir]
		angle = b.dirAngles[fixedDir]
	} else {
		ch = b.lockedChannel
		angle = b.dirAngles[bestDir]
	}

	return b.extractChannel(raw, ch, gain), angle
}

func hfEnergy(hfChannels [][]float32, di int) float64 {
	n := len(hfChannels[0])
	var energy float64
	for _, v := range hfChannels[di] {
		energy += float64(v) * float64(v)
	}
	return energy / float64(n)
}

func (b *Beamformer) bandDiff() {
	for ci := 0; ci < b.nDirections; ci++ {
		in, out := b.chanBuf[ci], b.hfBuf[ci]
		out[0], out[1] = 0, 0
		for i := 2; i < len(in); i++ {
			out[i] = (in[i] - in[i-2]) * 0.5
		}
	}
}

// decodeChannels decodes all perimeter channels from a raw period into
// chanBuf as float32 normalised to [-1, 1].
func (b *Beamformer) decodeChannels(raw []byte) {
	for i := 0; i < b.periodFrames; i++ {
		base := i * b.frameSize
		for ci := 0; ci < b.nDirections; ci++ {
			offset := base + b.dirChs[ci]*b.sampleBytes
			b.chanBuf[ci][i] = b.decodeSample(raw, offset)
		}
	}
}

// decodeSample reads one sample as float32 in [-1, 1].
func (b *Beamformer) decodeSample(raw []byte, offset int) float32 {
	return float32(b.readSample(raw, offset)) / b.sampleScale
}

// readSample reads one sample as a sign-extended int32.
func (b *Beamformer) readSample(raw []byte, offset int) int32 {
	switch b.sampleBytes {
	case 2:
		return int32(int16(uint16(raw[offset]) | uint16(raw[offset+1])<<8))
	case 3:
		val := int32(raw[offset]) | int32(raw[offset+1])<<8 | int32(raw[offset+2])<<16
		if val&0x800000 != 0 {
			val |= ^int32(0xFFFFFF)
		}
		return val
	case 4:
		return int32(raw[offset]) | int32(raw[offset+1])<<8 | int32(raw[offset+2])<<16 | int32(raw[offset+3])<<24
	default:
		return 0
	}
}

// extractChannel extracts a single channel as S16_LE mono, applying the
// fixed mic gain to the full-depth sample before quantising to 16-bit.
//
// gain is linear (1.0 = unity). Q12 fixed point: the >>gainShift combines
// the Q12 descale with the N→16 bit reduction, so gain 1.0 at the native
// depth reproduces the upper-bytes behaviour bit-exactly.
func (b *Beamformer) extractChannel(raw []byte, ch int, gain float64) []byte {
	n := len(raw) / b.frameSize
	out := make([]byte, n*2)
	offset0 := ch * b.sampleBytes
	gainQ := int64(gain*4096.0 + 0.5)
	for i := 0; i < n; i++ {
		base := i*b.frameSize + offset0
		val := b.readSample(raw, base)
		v := (int64(val) * gainQ) >> b.gainShift
		if v > 32767 {
			v = 32767
			b.clippedSamples++
		} else if v < -32768 {
			v = -32768
			b.clippedSamples++
		}
		out[i*2] = byte(uint16(v))
		out[i*2+1] = byte(uint16(v) >> 8)
	}
	return out
}

// EchoRef extracts the hardware echo reference from the same raw period the
// mic channels come from, as 16kHz mono S16 — the AEC's far-end input.
//
// UNITY GAIN, deliberately and not negotiably. The reference is the playback
// stream at full digital scale, and applying mic gain would clip it.
//
// Returns nil when the buffer is short or the board has no echo reference
// channel.
func (b *Beamformer) EchoRef(raw []byte) []byte {
	if b.echoRefCh < 0 || len(raw) < b.frameSize {
		return nil
	}
	return b.extractChannel(raw, b.echoRefCh, 1.0)
}

// ClippedSamples returns the running count of samples clamped by the mic
// gain.
func (b *Beamformer) ClippedSamples() uint64 {
	return b.clippedSamples
}

// CandidateAngles returns the steering angles used for direction estimation.
func (b *Beamformer) CandidateAngles() []float64 {
	return b.dirAngles
}

func (b *Beamformer) nearestDirection(angleDeg float64) int {
	if b.nDirections == 0 {
		return 0
	}
	best := 0
	bestDiff := math.Abs(angleDiff(angleDeg, b.dirAngles[0]))
	for i := 1; i < b.nDirections; i++ {
		d := math.Abs(angleDiff(angleDeg, b.dirAngles[i]))
		if d < bestDiff {
			bestDiff = d
			best = i
		}
	}
	return best
}

func angleDiff(a, b float64) float64 {
	d := math.Mod(a-b+360, 360)
	if d > 180 {
		d -= 360
	}
	return d
}
