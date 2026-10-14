//go:build !windows

package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

type demuxPipe struct {
	reader, writer *os.File
	path           string
}

func newDemuxPipe(_ context.Context, index int) (*demuxPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &demuxPipe{reader: r, writer: w, path: fmt.Sprintf("pipe:%d", 3+index)}, nil
}

func (p *demuxPipe) Read(b []byte) (int, error) { return p.reader.Read(b) }
func (p *demuxPipe) bind(cmd *exec.Cmd)         { cmd.ExtraFiles = append(cmd.ExtraFiles, p.writer) }
func (p *demuxPipe) started()                   { p.writer.Close() }
func (p *demuxPipe) processDone()               {}
func (p *demuxPipe) Close() error {
	p.writer.Close()
	return p.reader.Close()
}
