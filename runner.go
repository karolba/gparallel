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

	"github.com/alessio/shellescape"
	"github.com/creack/pty"
	"github.com/shirou/gopsutil/v3/process"
)

const MAXBUF = 32 * 1024

type OutputPart struct {
	fromFd  int
	content []byte
}

type Output struct {
	mutex              sync.Mutex
	shouldPassToParent bool
	parts              []OutputPart
	stdoutPipeOrPty    *os.File
	stderrPipeOrPty    *os.File
	winchSignal        chan os.Signal
	streamClosed       chan struct{}
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

	// I wonder if this actually does anything, but it doesn't seem like it would hurt
	_ = proc.output.stdoutPipeOrPty.Sync()
	_ = proc.output.stderrPipeOrPty.Sync()

	// this looks weird but makes stream closing a bit faster
	_, _ = proc.output.stdoutPipeOrPty.Write([]byte{})
	_, _ = proc.output.stderrPipeOrPty.Write([]byte{})

	haveToClose("the read side of the stdout pipe", proc.output.stdoutPipeOrPty)
	haveToClose("the read side of the stderr pipe", proc.output.stderrPipeOrPty)

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

func haveToClose(name string, closer io.Closer) {
	err := closer.Close()
	if err != nil {
		log.Fatalf("Could not close %s: %v\n", name, err)
	}
}

func runInteractive(cmd *exec.Cmd) *Output {
	out := &Output{}
	var stdoutTty, stderrTty *os.File

	size, err := pty.GetsizeFull(os.Stdout)
	if err != nil {
		log.Fatalf("Could not get terminal size: %v\n", err)
	}

	out.stdoutPipeOrPty, stdoutTty, err = pty.Open()
	if err != nil {
		log.Fatalf("Couldn't open a pty for %v's stdout: %v\n", cmd.Args, err)
	}
	defer haveToClose("stdout tty", stdoutTty)
	err = pty.Setsize(out.stdoutPipeOrPty, size)
	if err != nil {
		log.Fatalf("Could not set stdout terminal size for command %v: %v\n", cmd.Args, err)
	}

	out.stderrPipeOrPty, stderrTty, err = pty.Open()
	if err != nil {
		log.Fatalf("Couldn't open a pty for %v's stderr: %v\n", cmd.Args, err)
	}
	defer haveToClose("stderr tty", stderrTty)
	err = pty.Setsize(out.stderrPipeOrPty, size)
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

			err = pty.Setsize(out.stdoutPipeOrPty, size)
			if err != nil {
				log.Fatalf("Could not set stdout terminal size for command %v on sigwinch: %v\n", cmd.Args, err)
			}

			err = pty.Setsize(out.stderrPipeOrPty, size)
			if err != nil {
				log.Fatalf("Could not set stderr terminal size for command %v on sigwinch: %v\n", cmd.Args, err)
			}
		}
	}()

	cmd.Stdin = stdoutTty
	cmd.Stdout = stdoutTty
	cmd.Stderr = stderrTty

	err = cmd.Start()
	if err != nil {
		log.Fatalf("Could not start %v: %v\n", shellescape.QuoteCommand(cmd.Args), err)
	}

	out.streamClosed = make(chan struct{}, 2)
	go readContinuouslyTo(out.stdoutPipeOrPty, out, syscall.Stdout)
	go readContinuouslyTo(out.stderrPipeOrPty, out, syscall.Stderr)

	return out
}

func runNonInteractive(cmd *exec.Cmd) *Output {
	var err error
	var stdoutWritePipe, stderrWritePipe *os.File
	out := &Output{}

	out.stdoutPipeOrPty, stdoutWritePipe, err = os.Pipe()
	if err != nil {
		log.Fatalf("Could not create a pipe for %v's stdout: %v\n", cmd.Args, err)
	}
	defer haveToClose("stdout pipe", stdoutWritePipe)

	out.stderrPipeOrPty, stderrWritePipe, err = os.Pipe()
	if err != nil {
		log.Fatalf("Could not create a pipe for %v's stderr: %v\n", cmd.Args, err)
	}
	defer haveToClose("stderr pipe", stderrWritePipe)

	cmd.Stdout = stdoutWritePipe
	cmd.Stderr = stderrWritePipe
	err = cmd.Start()
	if err != nil {
		log.Fatalf("Could not start %v: %v\n", shellescape.QuoteCommand(cmd.Args), err)
	}

	out.streamClosed = make(chan struct{}, 2)
	go readContinuouslyTo(out.stdoutPipeOrPty, out, syscall.Stdout)
	go readContinuouslyTo(out.stderrPipeOrPty, out, syscall.Stderr)

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
