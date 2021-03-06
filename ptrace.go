// Package ptrace provides an interface to the ptrace system call.
package ptrace

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"syscall"
)

var (
	// TraceeExited is returned when a command is executed on a tracee
	// that has already exited.
	TraceeExited = errors.New("tracee exited")
)

// An Event is sent on a Tracee's event channel whenever it changes state.
type Event interface{}

// A Tracee is a process that is being traced.
type Tracee struct {
	proc   *os.Process
	events chan Event
	err    chan error
	cmds   chan func()
}

func (t *Tracee) PID() int { return t.proc.Pid }

// Events returns the events channel for the tracee.
func (t *Tracee) Events() <-chan Event {
	return t.events
}

// Exec executes a process with tracing enabled, returning the Tracee
// or an error if an error occurs while executing the process.
func Exec(name string, argv []string) (*Tracee, error) {
	t := &Tracee{
		events: make(chan Event, 1),
		err:    make(chan error, 1),
		cmds:   make(chan func()),
	}

	err := make(chan error)
	proc := make(chan *os.Process)
	go func() {
		runtime.LockOSThread()
		p, e := os.StartProcess(name, argv, &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			Sys: &syscall.SysProcAttr{
				Ptrace:    true,
				Pdeathsig: syscall.SIGCHLD,
			},
		})
		proc <- p
		err <- e
		if e != nil {
			return
		}
		go t.wait()
		t.trace()
	}()
	t.proc = <-proc
	return t, <-err
}

// Attaches to the given process.
func Attach(pid int) (*Tracee, error) {
	t := &Tracee{
		events: make(chan Event, 1),
		err:    make(chan error, 1),
		cmds:   make(chan func()),
	}

	err := make(chan error, 1)
	proc := make(chan *os.Process)
	go func() {
		runtime.LockOSThread()
		err <- syscall.PtraceAttach(pid)
		p, e := os.FindProcess(pid)
		proc <- p
		err <- e
		if e != nil {
			return
		}
		go t.wait()
		t.trace()
	}()
	t.proc = <-proc
	return t, <-err
}

// Detach detaches the tracee, allowing it to continue its execution normally.
// No more tracing is performed, and no events are sent on the event channel
// until the tracee exits.
func (t *Tracee) Detach() error {
	err := make(chan error, 1)
	if t.do(func() { err <- syscall.PtraceDetach(t.proc.Pid) }) {
		return <-err
	}
	return TraceeExited
}

// SingleStep continues the tracee for one instruction.
func (t *Tracee) SingleStep() error {
	err := make(chan error, 1)
	if t.do(func() { err <- syscall.PtraceSingleStep(t.proc.Pid) }) {
		return <-err
	}
	return TraceeExited
}

// Makes the tracee execute unmanaged by the tracer.  Most commands are not
// possible in this state, with the notable exception of sending a
// syscall.SIGSTOP signal.
func (t *Tracee) Continue() error {
	err := make(chan error, 1)
	sig := 0
	if t.do(func() { err <- syscall.PtraceCont(t.proc.Pid, sig) }) {
		return <-err
	}
	return TraceeExited
}

// Executes the inferior until it hits, or returns from, a system call.
func (t *Tracee) Syscall() error {
	if t.cmds == nil {
		return TraceeExited
	}
	errchan := make(chan error, 1)
	t.cmds <- func() {
		err := syscall.PtraceSyscall(t.proc.Pid, 0)
		errchan <- err
	}
	return <- errchan
}

// Sends the given signal to the tracee.
func (t *Tracee) SendSignal(sig syscall.Signal) error {
	err := make(chan error, 1)
	if t.do(func() { err <- syscall.Kill(t.proc.Pid, sig) }) {
		return <-err
	}
	return TraceeExited
}

// grabs a word at the given address.
func peek(pid int, address uintptr) (uint64, error) {
	word := make([]byte, 8 /* 8 should really be sizeof(uintptr)... */)
	nbytes, err := syscall.PtracePeekData(pid, address, word)
	if err != nil || nbytes != 8/*sizeof(uintptr)*/ {
		return 0, err
	}
	v := uint64(0x2Bc0ffee)
	err = binary.Read(bytes.NewReader(word), binary.LittleEndian, &v)
	return v, err
}

// Reads the given word from the inferior's address space.
func (t *Tracee) ReadWord(address uintptr) (uint64, error) {
	err := make(chan error, 1)
	value := make(chan uint64, 1)
	if t.do(func() {
		v, e := peek(t.proc.Pid, address);
		value <- v
		err <- e
	}) {
		return <-value, <-err
	}
	return 0, errors.New("unreachable.")
}

// grabs a word at the given address.
func poke(pid int, address uintptr, word uint64) (error) {
	/* convert the word into the byte array that PtracePokeData needs. */
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.LittleEndian, word)
	if err != nil { return err }

	nbytes, err := syscall.PtracePokeData(pid, address, buf.Bytes())
	if err != nil || nbytes != 8/*sizeof(uint64)*/ {
		return err
	}
	return nil
}

// Writes the given word into the inferior's address space.
func (t *Tracee) WriteWord(address uintptr, word uint64) (error) {
	err := make(chan error, 1)
	if t.do(func() { err <- poke(t.proc.Pid, address, word) }) {
		return <-err
	}
	return TraceeExited
}

func (t *Tracee) Write(address uintptr, data []byte) (error) {
	err := make(chan error, 1)
	if t.do(func() {
	  _, e := syscall.PtracePokeData(t.proc.Pid, address, data)
	  err <- e
	}) {
	  return <-err
	}
	return TraceeExited
}

// Read grabs memory starting at the given address, for len(data) bytes.
func (t *Tracee) Read(address uintptr, data []byte) error {
  err := make(chan error, 1)
  if t.do(func() {
    _, e := syscall.PtracePeekData(t.proc.Pid, address, data)
    err <- e
  }) {
    return <-err
  }
  return TraceeExited
}

// read the registers from the inferior.
func (t* Tracee) GetRegs() (syscall.PtraceRegs, error) {
	errchan := make(chan error, 1)
	value := make(chan syscall.PtraceRegs, 1)
	if t.do(func() {
		var regs syscall.PtraceRegs
		err := syscall.PtraceGetRegs(t.proc.Pid, &regs)
		value <- regs
		errchan <- err
	}) {
		return <-value, <-errchan
	}
	return syscall.PtraceRegs{}, errors.New("unreachable.")
}

// reads the instruction pointer from the inferior and returns it.
func (t* Tracee) GetIPtr() (uintptr, error) {
	errchan := make(chan error, 1)
	value := make(chan uintptr, 1)
	if t.do(func() {
		var regs syscall.PtraceRegs
		regs.Rip = 0
		err := syscall.PtraceGetRegs(t.proc.Pid, &regs)
		value <- uintptr(regs.Rip)
		errchan <- err
	}) {
		return <-value, <-errchan
	}
	return 0, TraceeExited
}

func (t* Tracee) SetIPtr(addr uintptr) error {
	errchan := make(chan error, 1)
	if t.do(func() {
		var regs syscall.PtraceRegs
		err := syscall.PtraceGetRegs(t.proc.Pid, &regs)
		if err != nil { errchan <- err; return }
		regs.Rip = uint64(addr)
		err = syscall.PtraceSetRegs(t.proc.Pid, &regs)
		errchan <- err
	}) {
		return <-errchan
	}
	return TraceeExited
}

func (t* Tracee) SetRegs(regs syscall.PtraceRegs) error {
	errchan := make(chan error, 1)
	if t.do(func() {
		err := syscall.PtraceSetRegs(t.proc.Pid, &regs)
		errchan <- err
	}) {
		return <-errchan
	}
	return TraceeExited
}

// Reads the signal information for the signal that stopped the inferior.  Only
// valid if the inferior is stopped due to a signal.
func (t *Tracee) GetSiginfo() (Siginfo, error) {
	errchan := make(chan error, 1)
	value := make(chan Siginfo, 1)
	if t.do(func() {
		si, err := get_siginfo(t.proc.Pid)
		errchan <- err
		value <- si
	}) {
		return <-value, <-errchan
	}
	return Siginfo{}, TraceeExited
}

// Clears the last signal the inferior received.  This could allow the inferior
// to continue after a segfault, for example.
func (t *Tracee) ClearSignal() error {
	errchan := make(chan error, 1)
	if t.do(func() {
		errchan <- clear_signals(int(t.proc.Pid))
	}) {
		return <-errchan
	}
	return TraceeExited
}

// Sends the command to the tracer go routine.	Returns whether the command
// was sent or not. The command may not have been sent if the tracee exited.
func (t *Tracee) do(f func()) bool {
	if t.cmds != nil {
		t.cmds <- f
		return true
	}
	return false
}

func (t *Tracee) Close() error {
	var err error
	select {
	case err = <-t.err:
	default:
		err = nil
	}
	close(t.cmds)
	t.cmds = nil

	syscall.Kill(t.proc.Pid, syscall.SIGKILL)
	return err
}

func (t *Tracee) wait() {
	defer close(t.err)
	for {
		state, err := t.proc.Wait()
		if err != nil {
			t.err <- err
			close(t.events)
			return
		}
		if state.Exited() {
			t.events <- Event(state.Sys().(syscall.WaitStatus))
			close(t.events)
			return
		}
		t.events <- Event(state.Sys().(syscall.WaitStatus))
	}
}

func (t *Tracee) trace() {
	for cmd := range t.cmds {
		cmd()
	}
}
