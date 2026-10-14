package audio

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func config(layout string, channels int) Config {
	return Config{InputLayout: layout, Channels: channels, SampleRate: 48000, ExtractChannel: -1}
}

func process(t *testing.T, cfg Config, samples []float64) (*Processor, []float64) {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	output, err := p.Process(samples)
	if err != nil {
		t.Fatal(err)
	}
	return p, output
}

func assertSamples(t *testing.T, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("wrong sample count: got %d, want %d", len(got), len(want))
	}
	for i, value := range got {
		if math.Float64bits(value) != math.Float64bits(want[i]) {
			t.Fatalf("sample %d = %.17g (0x%x), want %.17g (0x%x)", i, value, math.Float64bits(value), want[i], math.Float64bits(want[i]))
		}
	}
}

func TestIdentityPreservesSampleBitsAndPeak(t *testing.T) {
	source := []float64{0, math.Copysign(0, -1), math.SmallestNonzeroFloat64, -.25, 1.25, -2.5, .1, .2}
	original := append([]float64(nil), source...)
	p, output := process(t, config("stereo", 2), source)
	assertSamples(t, output, source)
	assertSamples(t, source, original)
	if p.Peak() != 2.5 || p.OutputChannels() != 2 || p.OutputLayout() != "stereo" {
		t.Fatalf("unexpected output metadata/peak: %d %s %g", p.OutputChannels(), p.OutputLayout(), p.Peak())
	}
	output[0] = 3
	if source[0] != 0 {
		t.Fatal("output aliases caller's input")
	}
}

func TestStereoChannelImpulses(t *testing.T) {
	for _, lfe := range []bool{false, true} {
		cfg := config("7.1", 8)
		cfg.Mix, cfg.MixLFE = "stereo", lfe
		rootHalf := math.Sqrt(.5)
		lfeGain := 0.0
		if lfe {
			lfeGain = .5 * math.Sqrt(10)
		}
		want := [][]float64{{1, 0}, {0, 1}, {rootHalf, rootHalf}, {lfeGain, lfeGain},
			{rootHalf, 0}, {0, rootHalf}, {rootHalf, 0}, {0, rootHalf}}
		for channel := 0; channel < 8; channel++ {
			input := make([]float64, 8)
			input[channel] = 1
			_, output := process(t, cfg, input)
			assertSamples(t, output, want[channel])
		}
		input := []float64{-1, -1, -1, -1, -1, -1, -1, -1}
		p, output := process(t, cfg, input)
		expected := -1.0
		expected += -rootHalf
		if lfe {
			expected += -lfeGain
		}
		expected += -rootHalf
		expected += -rootHalf
		assertSamples(t, output, []float64{expected, expected})
		if p.Peak() != -expected {
			t.Fatal("negative samples were omitted from peak measurement")
		}
	}
	cfg := config("6.1", 7)
	cfg.Mix = "stereo"
	_, output := process(t, cfg, []float64{0, 0, 0, 0, 1, 0, 0})
	assertSamples(t, output, []float64{.5, .5})
}

func TestGainThenCorrectionThenMix(t *testing.T) {
	cfg := config("3.0", 3)
	cfg.Mix, cfg.GainDB, cfg.Correction = "stereo", 6, .33
	p, output := process(t, cfg, []float64{.7, -.2, .1})
	gain := math.Pow(10, 6.0/20)
	l, r, c := float64(.7*gain), float64(-.2*gain), float64(.1*gain)
	lCorrected, rCorrected, cCorrected := float64(l*.33), float64(r*.33), float64(c*.33)
	center := float64(cCorrected * math.Sqrt(.5))
	left, right := lCorrected+center, rCorrected+center
	assertSamples(t, output, []float64{left, right})
	before := l + float64(c*math.Sqrt(.5))
	if p.Peak() != before {
		t.Fatalf("peak includes correction: %g, want %g", p.Peak(), before)
	}
	if output[0] == before*.33 {
		t.Fatal("fixture does not distinguish correction before/after mixing")
	}
}

func TestMonoIsCenterOrLeftExtraction(t *testing.T) {
	cases := []struct {
		layout string
		source []float64
		want   float64
	}{
		{"mono", []float64{.75}, .75},
		{"stereo", []float64{.25, .75}, .25},
		{"5.1(side)", []float64{.1, .2, .3, .4, .5, .6}, .3},
		{"quad", []float64{.1, .2, .3, .4}, .1},
	}
	for _, tc := range cases {
		cfg := config(tc.layout, len(tc.source))
		cfg.Mix = "mono"
		p, output := process(t, cfg, tc.source)
		assertSamples(t, output, []float64{tc.want})
		if p.OutputChannels() != 1 || p.OutputLayout() != "mono" {
			t.Fatal("mono extraction has incorrect output metadata")
		}
	}
}

func TestChannelMapBeforeExtraction(t *testing.T) {
	cfg := config("stereo", 2)
	cfg.ChannelMap, cfg.ExtractChannel = []int{1, 0}, 0
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ChannelMap[0] = 0 // Config storage belongs to the caller.
	output, err := p.Process([]float64{.25, .75, -.2, -.8})
	if err != nil {
		t.Fatal(err)
	}
	assertSamples(t, output, []float64{.75, -.8})
	if p.Peak() != .8 {
		t.Fatal("peak was measured on the wrong channels")
	}
}

func TestSurroundRoutes(t *testing.T) {
	cfg := config("7.1", 8)
	cfg.Mix = "5.1"
	p, output := process(t, cfg, []float64{1, 2, 3, 4, 5, 6, 7, 8})
	assertSamples(t, output, []float64{1, 2, 3, 4, 12, 14})
	if p.OutputLayout() != "5.1(side)" || p.Peak() != 14 {
		t.Fatal("incorrect 7.1 downmix metadata")
	}
	cfg = config("6.1", 7)
	cfg.Mix = "5.1"
	_, output = process(t, cfg, []float64{1, 2, 3, 4, 5, 6, 7})
	back := 5 * math.Sqrt(.5)
	assertSamples(t, output, []float64{1, 2, 3, 4, back + 6, back + 7})
	cfg.Mix = "double7"
	p, output = process(t, cfg, []float64{1, 2, 3, 4, 5, 6, 7})
	assertSamples(t, output, []float64{1, 2, 3, 4, back, back, 6, 7})
	if p.OutputChannels() != 8 || p.OutputLayout() != "7.1" {
		t.Fatal("incorrect double7 metadata")
	}
}

func TestPositiveDelayIsBoundedSilence(t *testing.T) {
	cfg := config("stereo", 2)
	cfg.SampleRate, cfg.DelayMS = 1000, 5000.5
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process([]float64{.1, .2}); err == nil {
		t.Fatal("accepted samples before initial silence was drained")
	}
	if _, err := p.Finish(); err == nil {
		t.Fatal("finished before initial silence was drained")
	}
	samples := 0
	for {
		block := p.DrainDelay()
		if block == nil {
			break
		}
		if len(block) > 4096*2 {
			t.Fatal("silence allocation exceeded a block")
		}
		for _, v := range block {
			if v != 0 {
				t.Fatal("delay contains non-silent samples")
			}
		}
		samples += len(block)
	}
	if samples != 5000*2 {
		t.Fatalf("truncated delay has %d samples, want %d", samples, 5000*2)
	}
	output, err := p.Process([]float64{.1, .2})
	if err != nil {
		t.Fatal(err)
	}
	assertSamples(t, output, []float64{.1, .2})
	if _, err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process(nil); !errors.Is(err, ErrFinished) {
		t.Fatalf("processing after Finish: %v", err)
	}
}

func TestNegativeDelayAcrossChunks(t *testing.T) {
	cfg := config("stereo", 2)
	cfg.SampleRate, cfg.DelayMS = 1000, -3.5
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out := p.DrainDelay(); out != nil {
		t.Fatal("negative delay emitted silence")
	}
	output, err := p.Process([]float64{9, -9, 8, -8})
	if err != nil || len(output) != 0 || p.Peak() != 0 {
		t.Fatalf("trimmed samples affected output/peak: %v, %g, %v", output, p.Peak(), err)
	}
	output, err = p.Process([]float64{7, -7, .1, .2, -.3, -.4})
	if err != nil {
		t.Fatal(err)
	}
	assertSamples(t, output, []float64{.1, .2, -.3, -.4})
	if p.Peak() != .4 {
		t.Fatal("peak includes removed frames")
	}
}

func TestPCMDelayUsesTimestampArithmetic(t *testing.T) {
	for _, tc := range []struct {
		delay float64
		rate  int
		want  int64
	}{
		{.5, 1000, 0}, {-.5, 1000, 0},
		{1.5, 1000, 1}, {-1.5, 1000, -1},
		{1000, 22050, 22000}, {-1000, 22050, -22000},
		{1000, 44100, 44100}, {1.5, 48000, 72},
	} {
		got, err := pcmDelayFrames(tc.delay, tc.rate)
		if err != nil || got != tc.want {
			t.Fatalf("delay %g ms at %d Hz: got %d, want %d: %v", tc.delay, tc.rate, got, tc.want, err)
		}
	}
}

func TestChunkingDoesNotChangeSamples(t *testing.T) {
	cfg := config("5.1(side)", 6)
	cfg.Mix, cfg.MixLFE = "stereo", true
	cfg.GainDB, cfg.Correction, cfg.DelayMS = 1.75, .1234, -.03
	cfg.ChannelMap = []int{1, 0, 2, 3, 5, 4}
	source := make([]float64, 600)
	for i := range source {
		source[i] = math.Sin(float64(i)) / 4
	}
	whole, want := process(t, cfg, source)
	split, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	for pos := 0; pos < len(source); {
		end := min(pos+18, len(source))
		block, err := split.Process(source[pos:end])
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, block...)
		pos = end
	}
	assertSamples(t, got, want)
	if split.Peak() != whole.Peak() {
		t.Fatal("peak depends on chunk boundaries")
	}
}

func TestBadInputLeavesStateIntact(t *testing.T) {
	p, _ := process(t, config("mono", 1), []float64{.4})
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if output, err := p.Process([]float64{.9, bad}); err == nil || output != nil || p.Peak() != .4 {
			t.Fatalf("failed chunk altered state: %v, %v", output, err)
		}
	}
	cfg := config("stereo", 2)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process([]float64{1}); err == nil {
		t.Fatal("accepted a partial frame")
	}
	cfg.GainDB = 12
	p, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process([]float64{.1, math.MaxFloat64}); err == nil || p.Peak() != 0 {
		t.Fatal("gain overflow did not fail atomically")
	}
}

func TestRejectInvalidAndUnestablishedConfigurations(t *testing.T) {
	cases := map[string]func(*Config){
		"no channels":            func(c *Config) { c.Channels = 0 },
		"no sample rate":         func(c *Config) { c.SampleRate = 0 },
		"nonfinite gain":         func(c *Config) { c.GainDB = math.NaN() },
		"overflow gain":          func(c *Config) { c.GainDB = 10000 },
		"negative correction":    func(c *Config) { c.Correction = -1 },
		"nonfinite delay":        func(c *Config) { c.DelayMS = math.Inf(1) },
		"overflow delay":         func(c *Config) { c.DelayMS = math.MaxFloat64 },
		"short map":              func(c *Config) { c.ChannelMap = []int{0} },
		"duplicate map":          func(c *Config) { c.ChannelMap = []int{0, 0} },
		"bad map index":          func(c *Config) { c.ChannelMap = []int{0, 2} },
		"bad extraction":         func(c *Config) { c.ExtractChannel = 2 },
		"extract and mix":        func(c *Config) { c.ExtractChannel, c.Mix = 0, "stereo" },
		"LFE without mix":        func(c *Config) { c.MixLFE = true },
		"wrong layout size":      func(c *Config) { c.InputLayout = "5.1" },
		"unknown mix":            func(c *Config) { c.Mix = "other" },
		"wrong down6 layout":     func(c *Config) { c.Mix = "5.1" },
		"wrong double7 layout":   func(c *Config) { c.Mix = "double7" },
		"unknown mixing layout":  func(c *Config) { c.InputLayout, c.Mix = "unassigned", "stereo" },
		"unestablished speakers": func(c *Config) { c.InputLayout, c.Mix = "FLC+FRC", "stereo" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := config("stereo", 2)
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("accepted unsupported configuration")
			}
		})
	}
}

func TestExplicitSpeakerOrder(t *testing.T) {
	cfg := config("3 channels (FC+FR+FL)", 3)
	cfg.Mix = "stereo"
	_, output := process(t, cfg, []float64{1, 2, 3})
	rootHalf := math.Sqrt(.5)
	assertSamples(t, output, []float64{rootHalf + 3, rootHalf + 2})
	cfg = config("", 3)
	p, output := process(t, cfg, []float64{1, 2, 3})
	if p.OutputLayout() != "" || !reflect.DeepEqual(output, []float64{1, 2, 3}) {
		t.Fatal("unknown layout changed during pass-through")
	}
}
