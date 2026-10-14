package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

func pipePattern(index int) []byte {
	b := make([]byte, 2<<20)
	for i := range b {
		b[i] = byte(i*31 + index*17)
	}
	return b
}

func TestDemuxPipeChild(t *testing.T) {
	mode := os.Getenv("EAC3TO_PIPE_TEST")
	if mode == "" {
		return
	}
	if mode == "exit" {
		os.Exit(7)
	}
	var wg sync.WaitGroup
	for index := 0; index < 2; index++ {
		path := os.Getenv(fmt.Sprintf("EAC3TO_PIPE_%d", index))
		if path == "" {
			continue
		}
		wg.Add(1)
		go func(index int, path string) {
			defer wg.Done()
			var f *os.File
			var err error
			if runtime.GOOS == "windows" {
				f, err = os.OpenFile(path, os.O_WRONLY, 0)
			} else {
				f = os.NewFile(uintptr(3+index), path)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(8)
			}
			defer f.Close()
			data := pipePattern(index)
			if mode == "hold" {
				data = data[:1]
			}
			for len(data) > 0 {
				n := min(len(data), 7919)
				if _, err := f.Write(data[:n]); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(9)
				}
				data = data[n:]
			}
			if mode == "hold" {
				time.Sleep(30 * time.Second)
			}
		}(index, path)
	}
	wg.Wait()
	os.Exit(0)
}

func pipeChild(t *testing.T, ctx context.Context, mode string, pipes ...*demuxPipe) *exec.Cmd {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, program, "-test.run=^TestDemuxPipeChild$")
	cmd.Env = append(os.Environ(), "EAC3TO_PIPE_TEST="+mode)
	cmd.Stderr = os.Stderr
	for i, p := range pipes {
		cmd.Env = append(cmd.Env, "EAC3TO_PIPE_"+strconv.Itoa(i)+"="+p.path)
		p.bind(cmd)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, p := range pipes {
		p.started()
	}
	return cmd
}

func TestDemuxPipeRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var pipes []*demuxPipe
	for i := 0; i < 2; i++ {
		p, err := newDemuxPipe(ctx, i)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		pipes = append(pipes, p)
	}
	cmd := pipeChild(t, ctx, "data", pipes...)
	results := make(chan error, len(pipes))
	for i, p := range pipes {
		go func(i int, p *demuxPipe) {
			data, err := io.ReadAll(p)
			if err == nil && !bytes.Equal(data, pipePattern(i)) {
				err = fmt.Errorf("pipe %d changed its byte stream", i)
			}
			results <- err
		}(i, p)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, p := range pipes {
		p.processDone()
	}
	for range pipes {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestDemuxPipeProcessFailsBeforeConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := newDemuxPipe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cmd := pipeChild(t, ctx, "exit", p)
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, p); done <- err }()
	if cmd.Wait() == nil {
		t.Fatal("child unexpectedly succeeded")
	}
	p.processDone()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipe reader hung after the child failed")
	}
}

func TestDemuxPipeCloseInterruptsRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := newDemuxPipe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cmd := pipeChild(t, ctx, "hold", p)
	defer func() { cancel(); cmd.Wait(); p.processDone() }()
	var first [1]byte
	if _, err := io.ReadFull(p, first[:]); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.Read(first[:]); done <- err }()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed pipe returned success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not interrupt the reader")
	}
}

func TestDemuxPipeCloseWithoutChild(t *testing.T) {
	p, err := newDemuxPipe(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
