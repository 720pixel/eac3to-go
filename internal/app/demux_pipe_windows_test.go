package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDemuxPipeContextCancelsConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := newDemuxPipe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	done := make(chan error, 1)
	go func() { var b [1]byte; _, err := p.Read(b[:]); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled connection: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not release the pipe connection")
	}
}
