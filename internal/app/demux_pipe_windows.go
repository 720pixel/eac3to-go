package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	pipeKernel          = syscall.NewLazyDLL("kernel32.dll")
	createNamedPipe     = pipeKernel.NewProc("CreateNamedPipeW")
	connectNamedPipe    = pipeKernel.NewProc("ConnectNamedPipe")
	createPipeEvent     = pipeKernel.NewProc("CreateEventW")
	resetPipeEvent      = pipeKernel.NewProc("ResetEvent")
	getPipeResult       = pipeKernel.NewProc("GetOverlappedResult")
	convertPipeSecurity = syscall.NewLazyDLL("advapi32.dll").NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

// Each pipe permits only the current user, rejects remote connections and has
// a random name. Overlapped I/O lets cancellation interrupt both connection and
// reads; no socket, public file or inheritable handle carries track data.
type demuxPipe struct {
	path                string
	ctx                 context.Context
	handle, event       syscall.Handle
	connect             syscall.Overlapped
	connecting          bool
	done                chan struct{}
	doneOnce, closeOnce sync.Once
	closing             atomic.Bool
	mu                  sync.Mutex
}

func newDemuxPipe(ctx context.Context, _ int) (*demuxPipe, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	name := `\\.\pipe\eac3to-` + hex.EncodeToString(id[:])
	wide, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	user, err := token.GetTokenUser()
	token.Close()
	if err != nil {
		return nil, err
	}
	sid, err := user.User.Sid.String()
	if err != nil {
		return nil, err
	}
	sddl, err := syscall.UTF16PtrFromString("D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	var descriptor uintptr
	ok, _, err := convertPipeSecurity.Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	if ok == 0 {
		return nil, fmt.Errorf("named pipe security: %w", err)
	}
	defer syscall.LocalFree(syscall.Handle(descriptor))
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
	const pipeAccessInbound = 1
	const firstPipeInstance = 0x00080000
	const rejectRemoteClients = 8
	h, _, err := createNamedPipe.Call(uintptr(unsafe.Pointer(wide)), pipeAccessInbound|firstPipeInstance|syscall.FILE_FLAG_OVERLAPPED, rejectRemoteClients, 1, 0, 256<<10, 0, uintptr(unsafe.Pointer(&sa)))
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, fmt.Errorf("create named pipe: %w", err)
	}
	e, _, err := createPipeEvent.Call(0, 1, 0, 0)
	if e == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, fmt.Errorf("create pipe event: %w", err)
	}
	p := &demuxPipe{path: name, ctx: ctx, handle: syscall.Handle(h), event: syscall.Handle(e), done: make(chan struct{})}
	p.connect.HEvent = p.event
	ok, _, err = connectNamedPipe.Call(h, uintptr(unsafe.Pointer(&p.connect)))
	if ok == 0 && err != syscall.Errno(535) { // ERROR_PIPE_CONNECTED
		if err != syscall.ERROR_IO_PENDING {
			p.Close()
			return nil, fmt.Errorf("connect named pipe: %w", err)
		}
		p.connecting = true
	}
	return p, nil
}

func (p *demuxPipe) bind(_ *exec.Cmd) {}
func (p *demuxPipe) started()         {}
func (p *demuxPipe) processDone()     { p.doneOnce.Do(func() { close(p.done) }) }

func (p *demuxPipe) wait(o *syscall.Overlapped, connecting bool) (uint32, error) {
	completed := false
	defer func() {
		if !completed {
			// The OS must release the OVERLAPPED structure and read buffer before
			// they can be reused or reclaimed, even after a cancellation request.
			syscall.CancelIoEx(p.handle, o)
			var n uint32
			getPipeResult.Call(uintptr(p.handle), uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&n)), 1)
		}
	}()
	for {
		status, err := syscall.WaitForSingleObject(p.event, 50)
		if err != nil {
			return 0, err
		}
		if status == syscall.WAIT_OBJECT_0 {
			break
		}
		if status != syscall.WAIT_TIMEOUT {
			return 0, fmt.Errorf("unexpected named pipe wait result %d", status)
		}
		cancelErr := p.ctx.Err()
		if cancelErr == nil && p.closing.Load() {
			cancelErr = os.ErrClosed
		}
		if cancelErr == nil && connecting {
			select {
			case <-p.done:
				cancelErr = fmt.Errorf("FFmpeg exited before opening its track pipe")
			default:
			}
		}
		if cancelErr != nil {
			return 0, cancelErr
		}
	}
	var n uint32
	ok, _, err := getPipeResult.Call(uintptr(p.handle), uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&n)), 0)
	completed = ok != 0 || err != syscall.Errno(996) // ERROR_IO_INCOMPLETE
	if ok == 0 {
		return 0, err
	}
	return n, nil
}

func (p *demuxPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing.Load() {
		return 0, os.ErrClosed
	}
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	if p.connecting {
		_, err := p.wait(&p.connect, true)
		p.connecting = false
		if err != nil {
			return 0, err
		}
	}
	if len(b) == 0 {
		return 0, nil
	}
	if len(b) > 1<<20 {
		b = b[:1<<20]
	}
	ok, _, err := resetPipeEvent.Call(uintptr(p.event))
	if ok == 0 {
		return 0, err
	}
	o := syscall.Overlapped{HEvent: p.event}
	var n uint32
	err = syscall.ReadFile(p.handle, b, &n, &o)
	if err == syscall.ERROR_IO_PENDING {
		n, err = p.wait(&o, false)
	}
	if err == syscall.ERROR_BROKEN_PIPE || err == syscall.Errno(232) {
		err = io.EOF
	}
	if err == nil && n == 0 {
		err = io.EOF
	}
	return int(n), err
}

func (p *demuxPipe) Close() error {
	var err error
	p.closeOnce.Do(func() {
		p.closing.Store(true)
		syscall.CancelIoEx(p.handle, nil)
		p.mu.Lock()
		defer p.mu.Unlock()
		// A connection can still be pending if process creation failed.
		if p.connecting {
			var n uint32
			getPipeResult.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&p.connect)), uintptr(unsafe.Pointer(&n)), 1)
			p.connecting = false
		}
		err = syscall.CloseHandle(p.handle)
		syscall.CloseHandle(p.event)
	})
	return err
}
