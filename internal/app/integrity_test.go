package app

import (
	"bytes"
	"errors"
	"testing"
)

func TestProbeCodecDataDump(t *testing.T) {
	text := "\n00000000: 1200 1200 0002 dd00 0552 0bb8 02f0 0015  .........R......\n00000010: f900 cd80 f149 a061 059c 7e54 3dca 8df5  .....I.a..~T=...\n00000020: 09af                                     ..\n"
	data, err := parseProbeHex(text)
	if err != nil || len(data) != 34 || !bytes.Equal(data[32:], []byte{9, 175}) {
		t.Fatalf("codec data: %x, %v", data, err)
	}
	for _, text := range []string{
		"00000010: 0000  ..",
		"00000000: 0x00  ..",
		"00000000: 0000",
		"00000000: 00  .\n00000000: 01  .",
		"00000000: 0000000000000000000000000000000000 00  .",
	} {
		if _, err := parseProbeHex(text); err == nil {
			t.Fatalf("accepted malformed codec data %q", text)
		}
	}
}

func TestErrorDiagnosticCannotBeSuccessful(t *testing.T) {
	stderr := tailBuffer{Limit: 1024}
	stderr.Write([]byte("invalid compressed frame\n"))
	if err := strictToolError("decoder", nil, &stderr); err == nil {
		t.Fatal("decoder error with zero exit status was accepted")
	}
	if err := strictToolError("decoder", errors.New("exit 1"), &tailBuffer{}); err == nil {
		t.Fatal("failed decoder was accepted")
	}
	if err := strictToolError("decoder", nil, &tailBuffer{}); err != nil {
		t.Fatal(err)
	}
}
