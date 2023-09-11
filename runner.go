package main

import (
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/shirou/gopsutil/v3/process"
)

const MAXBUF = 32 * 1024

type OutputPart struct {
	fromFd  int
	content []byte
}

type Output struct {
	mutex                    sync.Mutex
	shouldPassToParent       bool
	parts                    []OutputPart
	stdoutPty                *os.File
	stderrPty                *os.File
	stdoutTty                *os.File
	stderrTty                *os.File
	stdoutNoninteractivePipe io.ReadCloser
	stderrNoninteractivePipe io.ReadCloser
	winchSignal              chan os.Signal
	streamClosed             chan struct{}
}

type ProcessResult struct {
	startedAt time.Time
	output    *Output
	cmd       *exec.Cmd
}

func (proc ProcessResult) isAlive() bool {
	p, err := process.NewProcess(int32(proc.cmd.Process.Pid))
	if err != nil {
		return false
	}

	statuses, err := p.Status()
	if err != nil {
		return false
	}

	return !slices.Contains(statuses, process.Zombie)
}

func (proc ProcessResult) wait() error {
	waitError := proc.cmd.Wait()

	signal.Stop(proc.output.winchSignal)

	// if running in non-interactive mode, Go's exec#Wait() will close pipes for us
	// as they are created with .StdoutPipe() and .StderrPipe()
	if stdoutIsTty {
		// this looks weird but makes stream closing a bit faster
		_, _ = proc.output.stdoutPty.Write([]byte{})
		_, _ = proc.output.stderrPty.Write([]byte{})

		_ = proc.output.stdoutPty.Close()
		_ = proc.output.stderrPty.Close()

		_ = proc.output.stdoutTty.Close()
		_ = proc.output.stderrTty.Close()
	}

	// wait for both stdout and stderr
	<-proc.output.streamClosed
	<-proc.output.streamClosed

	return waitError
}

func (out *Output) append(buf []byte, dataFromFd int) {
	out.mutex.Lock()
	defer out.mutex.Unlock()

	if out.shouldPassToParent {
		_, err := syscall.Write(dataFromFd, buf)
		if err != nil {
			log.Fatalf("Syscall write to fd %d: %v\n", dataFromFd, err)
		}
	} else {
		out.parts = append(out.parts, OutputPart{
			fromFd:  dataFromFd,
			content: buf,
		})
	}
}

func readContinuouslyTo(stream io.Reader, out *Output, fileDescriptor int) {
	buffer := make([]byte, MAXBUF)

	for {
		count, err := stream.Read(buffer)

		if count > 0 {
			data := make([]byte, count)
			copy(data, buffer[:count:count])

			out.append(data, fileDescriptor)
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			if errors.Is(err, fs.ErrClosed) {
				break
			}
			log.Fatalf("error from read: %v\n", err)
		}
	}

	out.streamClosed <- struct{}{}
}

func runInteractive(cmd *exec.Cmd) *Output {
	out := &Output{}

	size, err := pty.GetsizeFull(os.Stdout)
	if err != nil {
		log.Fatalf("Could not get terminal size: %v\n", err)
	}

	out.stdoutPty, out.stdoutTty, err = pty.Open()
	if err != nil {
		log.Fatalf("Couldn't open a pty for %v's stdout: %v\n", cmd.Args, err)
	}
	err = pty.Setsize(out.stdoutPty, size)
	if err != nil {
		log.Fatalf("Could not set stdout terminal size for command %v: %v\n", cmd.Args, err)
	}

	out.stderrPty, out.stderrTty, err = pty.Open()
	if err != nil {
		log.Fatalf("Couldn't open a pty for %v's stderr: %v\n", cmd.Args, err)
	}
	err = pty.Setsize(out.stderrPty, size)
	if err != nil {
		log.Fatalf("Could not set stderr terminal size for command %v: %v\n", cmd.Args, err)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    1,
	}

	out.winchSignal = make(chan os.Signal, 1)
	signal.Notify(out.winchSignal, syscall.SIGWINCH)
	go func() {
		for range out.winchSignal {
			size, err := pty.GetsizeFull(os.Stdout)
			if err != nil {
				log.Fatalf("Could not get terminal size on sigwinch: %v\n", err)
			}

			err = pty.Setsize(out.stdoutPty, size)
			if err != nil {
				log.Fatalf("Could not set stdout terminal size for command %v on sigwinch: %v\n", cmd.Args, err)
			}

			err = pty.Setsize(out.stderrPty, size)
			if err != nil {
				log.Fatalf("Could not set stderr terminal size for command %v on sigwinch: %v\n", cmd.Args, err)
			}
		}
	}()

	cmd.Stdin = out.stdoutTty
	cmd.Stdout = out.stdoutTty
	cmd.Stderr = out.stderrTty

	err = cmd.Start()
	if err != nil {
		log.Fatalf("Could not start process %v: %v\n", cmd.Args, err)
	}

	out.streamClosed = make(chan struct{}, 2)
	go readContinuouslyTo(out.stdoutPty, out, syscall.Stdout)
	go readContinuouslyTo(out.stderrPty, out, syscall.Stderr)

	return out
}

func runNonInteractive(cmd *exec.Cmd) *Output {
	var err error
	out := &Output{}

	out.stdoutNoninteractivePipe, err = cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Could not create a pipe for %v's stdout: %v\n", cmd.Args, err)
	}

	out.stderrNoninteractivePipe, err = cmd.StderrPipe()
	if err != nil {
		log.Fatalf("Could not create a pipe for %v's stderr: %v\n", cmd.Args, err)
	}

	err = cmd.Start()
	if err != nil {
		log.Fatalf("Could not start process %v: %v\n", cmd.Args, err)
	}

	out.streamClosed = make(chan struct{}, 2)
	go readContinuouslyTo(out.stdoutNoninteractivePipe, out, syscall.Stdout)
	go readContinuouslyTo(out.stderrNoninteractivePipe, out, syscall.Stderr)

	return out
}

func run(command []string) (result ProcessResult) {
	result.cmd = exec.Command(command[0], command[1:]...)

	if stdoutIsTty {
		result.output = runInteractive(result.cmd)
	} else {
		result.output = runNonInteractive(result.cmd)
	}

	result.startedAt = time.Now()
	return result
}
