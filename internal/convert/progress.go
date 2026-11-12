package convert

import (
	"context"
	"strconv"
	"strings"
)

// ProgressUpdate is a single, throttle-ready progress observation for one
// job: a clamped percent complete and an optional ETA. ETA is nil until an
// estimate is derivable (D-04, etaEstimator's warm-up window) -- callers
// must treat a nil ETA as "not yet known," never as zero seconds remaining.
type ProgressUpdate struct {
	// Pct is 0-100, clamped and monotonic within a job (D-02a).
	Pct int
	// ETA is the estimated remaining seconds, or nil before the EMA
	// warm-up completes (D-04).
	ETA *int
}

// ProgressSink is implemented by the worker (Plan 03) and passed to
// ProgressConverter.ConvertWithProgress. SourceDuration is called once,
// early, with the probed source duration (feeds PROG-02's persisted
// source_duration_seconds); Update is called for every throttled tick.
// Neither method carries any engine-internal text (stderr, paths) -- only
// integers -- so no diagnostic detail can leak into a client-facing field
// (T-41-03).
type ProgressSink interface {
	// SourceDuration reports the source's probed duration in seconds,
	// called at most once per conversion, before the first Update.
	SourceDuration(seconds float64)
	// Update reports a new progress observation. Implementations are
	// expected to throttle/persist; ProgressConverter implementations
	// must NOT throttle on their own -- that responsibility belongs to
	// the sink (D-03).
	Update(u ProgressUpdate)
}

// ProgressConverter is an OPTIONAL capability a Converter may additionally
// implement (D-01) -- added ALONGSIDE Converter, never as a change to it.
// Converters with no native machine-readable progress signal (libvips,
// LibreOffice, Chromium, whisper-cli's transcribe stage) do not implement
// this; the worker type-asserts for it and falls back to the plain
// Convert(ctx, in, out, opts) path when absent.
type ProgressConverter interface {
	// ConvertWithProgress behaves like Convert but additionally reports
	// progress to sink as the conversion runs.
	ConvertWithProgress(ctx context.Context, inPath, outPath string, opts map[string]any, sink ProgressSink) error
}

type StageUpdate struct {
	Stage string
	Index int
	Count int
}

type StageSink interface {
	Stage(u StageUpdate)
}

type StageConverter interface {
	ConvertWithStages(ctx context.Context, inPath, outPath string, opts map[string]any, sink StageSink) error
}

// progressTick is one parsed ffmpeg -progress block. Unexported: this is the
// parser's internal currency, not a seam contract (ProgressUpdate is the
// exported shape callers receive).
type progressTick struct {
	// outTimeSeconds is out_time_us (or out_time_ms, an identical
	// microsecond value despite the "_ms" name) divided by 1e6. Zero and
	// hasData=false when the block reported "N/A" (warm-up / a
	// transient mid-stream gap).
	outTimeSeconds float64
	// speed is ffmpeg's speed= field with the trailing "x" stripped.
	// Zero when "N/A" or unparseable -- callers must not treat a zero
	// speed as "stalled at 0x," only as "not yet known."
	speed float64
	// hasData is false when out_time was "N/A" -- "not yet derivable,"
	// never a garbage/zero value passed off as real (Pitfall 2,
	// 41-RESEARCH.md).
	hasData bool
	// done is true when the block's terminator was "progress=end"
	// rather than "progress=continue".
	done bool
}

// progressBlockParser accumulates ffmpeg -progress "key=value" lines into
// blocks, each terminated by a "progress=continue"/"progress=end" line. It
// is a bounded, per-line consumer: a malformed/truncated/non-numeric line
// is guarded before any strconv call, never panics, and never allocates
// unbounded state (T-41-01) -- one map entry per accumulated key, reset on
// every terminator.
type progressBlockParser struct {
	cur map[string]string
}

// newProgressBlockParser returns a parser ready to consume lines via
// feedLine, one ffmpeg -progress stream at a time.
func newProgressBlockParser() *progressBlockParser {
	return &progressBlockParser{cur: make(map[string]string)}
}

// feedLine consumes one raw line from the ffmpeg -progress stream. It
// returns (tick, true) when the line was the block's "progress=" terminator
// (a completed tick is available), else (progressTick{}, false). Blank
// lines and lines with no "=" are ignored rather than erroring -- ffmpeg's
// own output is server-generated/trusted, but a defensive parser never
// panics on an unexpected shape (T-41-01).
func (p *progressBlockParser) feedLine(line string) (progressTick, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return progressTick{}, false
	}
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return progressTick{}, false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	if key != "progress" {
		p.cur[key] = value
		return progressTick{}, false
	}

	tick := buildProgressTick(p.cur, value)
	p.cur = make(map[string]string) // reset for the next block
	return tick, true
}

// buildProgressTick assembles a completed tick from one block's accumulated
// fields plus the terminator's value ("continue" or "end").
func buildProgressTick(fields map[string]string, terminator string) progressTick {
	tick := progressTick{done: terminator == "end"}

	outStr, ok := fields["out_time_us"]
	if !ok || outStr == "N/A" {
		outStr, ok = fields["out_time_ms"]
	}
	if ok {
		if seconds, ok := parseProgressMicroseconds(outStr); ok {
			tick.outTimeSeconds = seconds
			tick.hasData = true
		}
	}

	if speedStr, ok := fields["speed"]; ok {
		if speed, ok := parseProgressSpeed(speedStr); ok {
			tick.speed = speed
		}
	}

	return tick
}

// parseProgressMicroseconds parses an out_time_us/out_time_ms field value,
// explicitly guarding the literal "N/A" BEFORE any strconv call (Pitfall 2,
// 41-RESEARCH.md) -- ffmpeg emits "N/A" for every numeric field during its
// warm-up block(s), and this codebase's own microsecond wart means the
// division by 1e6 must always follow a successful parse, never precede a
// guard check.
func parseProgressMicroseconds(raw string) (seconds float64, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "N/A" {
		return 0, false
	}
	us, err := parseProgressFloat(raw)
	if err != nil {
		return 0, false
	}
	return us / 1e6, true
}

// parseProgressSpeed parses ffmpeg's speed= field, which carries a trailing
// "x" (e.g. "2.69x", or leading-space-padded "   3x") and reports "N/A"
// during warm-up -- both guarded before strconv, mirroring
// parseProgressMicroseconds's discipline.
func parseProgressSpeed(raw string) (speed float64, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "N/A" {
		return 0, false
	}
	raw = strings.TrimSuffix(raw, "x")
	raw = strings.TrimSpace(raw)
	v, err := parseProgressFloat(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseProgressFloat is the single strconv.ParseFloat call site for the
// block parser -- every caller guards "N/A"/empty BEFORE reaching here, so
// this never needs its own N/A special case (Pitfall 2, 41-RESEARCH.md).
func parseProgressFloat(raw string) (float64, error) {
	return strconv.ParseFloat(raw, 64)
}

// computeProgressPct converts an out_time/source-duration pair into a
// clamped [0,100] percent. ok is false when sourceDurationSeconds <= 0 --
// no usable denominator, PROG-01's null-percent policy -- so callers never
// divide by zero or a negative/garbage value. A raw ratio above 100%
// (encoder lookahead/B-frame reorder can transiently overshoot the source
// duration) clamps to exactly 100 rather than being reported as-is (D-02a).
func computeProgressPct(outTimeSeconds, sourceDurationSeconds float64) (pct int, ok bool) {
	if sourceDurationSeconds <= 0 {
		return 0, false
	}
	raw := outTimeSeconds / sourceDurationSeconds * 100
	if raw < 0 {
		raw = 0
	}
	if raw > 100 {
		raw = 100
	}
	return int(raw), true
}

// progressEMAAlpha and progressWarmupSamples are compile-time constants,
// NOT env vars (41-RESEARCH.md Open Question 2 / 41-CONTEXT.md Claude's
// Discretion) -- only the throttle gates (Plan 03's PROGRESS_* knobs) carry
// env-tunable ceremony; these are shipped as reasonable, explicitly
// [ASSUMED] starting defaults, mirroring avDiskSafetyFactorDefault's own
// "[ASSUMED], no measured ratio existed yet" precedent (av.go:336-344) --
// there is no measured production ETA-accuracy data to derive them from at
// this point in the milestone.
const (
	// progressEMAAlpha weights each new speed= sample against the
	// running EMA. [ASSUMED] 0.3: higher is more responsive to real
	// speed changes, lower is smoother/less reactive to transient noise.
	progressEMAAlpha = 0.3
	// progressWarmupSamples is how many positive speed samples must be
	// folded in before etaSeconds returns a value instead of nil.
	// [ASSUMED] 3: a few seconds at ffmpeg's typical multi-Hz tick rate,
	// enough to smooth past the first sample(s)' start-up bias
	// (Pitfall 4, 41-RESEARCH.md).
	progressWarmupSamples = 3
)

// etaEstimator EMA-smooths ffmpeg's speed= samples and derives a
// remaining-seconds ETA gated behind a short warm-up window (D-04). The
// zero value is ready to use.
type etaEstimator struct {
	emaSpeed float64
	samples  int
}

// update folds a new speed sample into the EMA. Non-positive samples (an
// unparseable/"N/A"-derived speed of 0, per parseProgressSpeed, or any
// other non-positive value) are skipped entirely -- they must never pollute
// the running average or reset the warm-up counter (Pitfall 4).
func (e *etaEstimator) update(speed float64) {
	if speed <= 0 {
		return
	}
	if e.samples == 0 {
		e.emaSpeed = speed
	} else {
		e.emaSpeed = progressEMAAlpha*speed + (1-progressEMAAlpha)*e.emaSpeed
	}
	e.samples++
}

// etaSeconds returns the estimated remaining seconds given remainingSeconds
// of source left to encode, or nil before progressWarmupSamples positive
// samples have been observed (D-04) -- never a value derived from the
// noisy first sample(s).
func (e *etaEstimator) etaSeconds(remainingSeconds float64) *int {
	if e.samples < progressWarmupSamples || e.emaSpeed <= 0 {
		return nil
	}
	eta := int(remainingSeconds / e.emaSpeed)
	if eta < 0 {
		eta = 0
	}
	return &eta
}

// buildProgressUpdate is the single composition point Plans 02/03 call: it
// folds one parsed tick's speed into est, computes the clamped percent
// against sourceDurationSeconds, and derives the ETA -- callers (the
// AVConverter.ConvertWithProgress implementation, Plan 02) never need to
// duplicate this composition logic.
func buildProgressUpdate(tick progressTick, sourceDurationSeconds float64, est *etaEstimator) ProgressUpdate {
	pct, ok := computeProgressPct(tick.outTimeSeconds, sourceDurationSeconds)

	if tick.hasData {
		est.update(tick.speed)
	}

	var eta *int
	if ok {
		remaining := sourceDurationSeconds - tick.outTimeSeconds
		if remaining < 0 {
			remaining = 0
		}
		eta = est.etaSeconds(remaining)
	}

	return ProgressUpdate{Pct: pct, ETA: eta}
}
