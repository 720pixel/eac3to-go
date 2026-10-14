package truehd

import "testing"

func TestLosslessTrueHDDelayFilename(t *testing.T) {
	for _, row := range []struct {
		path  string
		delay int64
		want  string
	}{
		{"out.thd", 0, "out.thd"},
		{"out.thd", 32, "out DELAY 32ms.thd"},
		{"out.thd", -32, "out DELAY -32ms.thd"},
		{"/tmp/a.b/track 2.THD", -1, "/tmp/a.b/track 2 DELAY -1ms.THD"},
		{"out.thd", 7, "out DELAY 7ms.thd"},
	} {
		got, err := DelayPath(row.path, row.delay)
		if err != nil || got != row.want {
			t.Fatalf("%q %d => %q %v", row.path, row.delay, got, err)
		}
	}
	for _, path := range []string{"out.ac3", "out.wav", "out", ""} {
		if _, err := DelayPath(path, 32); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
