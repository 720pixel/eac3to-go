package app

import (
	"testing"
)

func TestOptionScopes(t *testing.T) {
	o, e := ParseOptions([]string{"input.mkv", "-keepDialnorm", "2:", "left.flac", "+3dB", "3:right.wav", "-mono"})
	if e != nil {
		t.Fatal(e)
	}
	if len(o.Outputs) != 2 {
		t.Fatal(o.Outputs)
	}
	if o.Outputs[0].Options.Gain != 3 || o.Outputs[1].Options.Gain != 0 || o.Outputs[1].Options.Mix != "mono" {
		t.Fatal(o.Outputs)
	}
	if !o.Outputs[0].Options.KeepDialnorm || !o.Outputs[1].Options.KeepDialnorm {
		t.Fatal(o.Outputs)
	}
}
func TestInvalidOptions(t *testing.T) {
	for _, args := range [][]string{{"a.wav", "b.flac", "-unknown"}, {"a.wav", "2:"}, {"a.wav", "b.flac", "-resampleTo0"}, {"a.wav", "-minDuration=NaN"}, {"a.wav", "-destpath="}, {"a.wav", "b.flac", "-demux"}, {"disc", "--language=en"}, {"disc", "--language=e/n"}, {"disc", "--playlist=1", "--language=eng"}, {"disc", "--explain", "-demux"}} {
		if _, e := ParseOptions(args); e == nil {
			t.Errorf("accepted %q", args)
		}
	}
}
func TestInputAndTitleParsing(t *testing.T) {
	o, e := ParseOptions([]string{"a.m2ts+b.m2ts", "1)", "2:", "out.flac", "-slowdown"})
	if e != nil {
		t.Fatal(e)
	}
	if len(o.Inputs) != 2 || o.Title != 0 || o.Outputs[0].Options.Speed != 24000.0/25025.0 {
		t.Fatal(o)
	}
}

func TestTimestampRollover(t *testing.T) {
	if got := timestamp(59.9999); got != "00:01:00.000" {
		t.Fatal(got)
	}
}

func TestDialnormSettingIsGlobal(t *testing.T) {
	o, e := ParseOptions([]string{"input.mkv", "2:", "a.ac3", "-keepDialnorm", "3:", "b.ac3"})
	if e != nil {
		t.Fatal(e)
	}
	if !o.Outputs[0].Options.KeepDialnorm || !o.Outputs[1].Options.KeepDialnorm {
		t.Fatal(o.Outputs)
	}
}

func TestRankingOptionsRejectSinglePlaylist(t *testing.T) {
	for _, option := range []string{"--language=eng", "--explain"} {
		if _, err := ParseOptions([]string{"00001.mpls", option}); err == nil {
			t.Fatalf("accepted %s for a single playlist", option)
		}
	}
}
