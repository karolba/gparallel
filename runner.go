package main

import (
	"errors"
	"fmt"
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
	ptyPkg "github.com/creack/pty"
	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/unix"
)

const MAXBUF = 32 * 1024

type Output struct {
	parts              []byte
	partsMutex         sync.Mutex
	shouldPassToParent bool
	stdoutPipeOrPty    *os.File
	stderrPipeOrPty    *os.File
	winchSignal        chan os.Signal
	streamClosed       chan struct{}
	allocator          chunkAllocator
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

func (out *Output) appendOrWrite(buf []byte, dataFromFd int) {
	out.partsMutex.Lock()
	defer out.partsMutex.Unlock()

	if out.shouldPassToParent {
		_, err := standardFdToFile[dataFromFd].Write(buf)
		if err != nil {
			log.Fatalf("Syscall write to fd %d: %v\n", dataFromFd, err)
		}
	} else {
		out.appendChunk(byte(dataFromFd), buf)
	}
}

func waitIfUsingTooMuchMemory(willSaveBytes int64, out *Output) {
	mem.childDiedFreeingMemory.L.Lock()
	defer mem.childDiedFreeingMemory.L.Unlock()

	if mem.currentlyInTheForeground == out {
		return
	}

	mem.currentlyStored.Add(willSaveBytes)
	for mem.currentlyStored.Load() > parsedFlMaxMemory {
		//log.Printf("Blocking because we're storing %d MiB (here: %d)\n",
		//	mem.currentlyStored.Load()/1024/1024,
		//	len(out.parts)/1024/1024)
		mem.childDiedFreeingMemory.Wait()
	}
}

func readContinuouslyTo(stream io.Reader, out *Output, fileDescriptor int) {
	buffer := make([]byte, MAXBUF)

	for {
		count, err := stream.Read(buffer)

		if count > 0 {
			waitIfUsingTooMuchMemory(chunkSizeWithHeader(buffer[:count]), out)
			out.appendOrWrite(buffer[:count], fileDescriptor)
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			if errors.Is(err, fs.ErrClosed) {
				break
			}
			var pathError *os.PathError
			if errors.As(err, &pathError) && pathError.Err == syscall.EIO {
				// Returning EIO is Linux's way of saying ErrClosed when reading from a ptmx:
				// https://github.com/creack/pty/issues/21
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

// createPty creates a pty, resizes it to winSize and marks it async
// so go doesn't schedule a thread for every read
func createPty(winSize *ptyPkg.Winsize) (pty, tty *os.File, err error) {
	pty, tty, err = ptyPkg.Open()
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = pty.Close()
			_ = tty.Close()
		}
	}()

	err = ptyPkg.Setsize(pty, winSize)
	if err != nil {
		return nil, nil, fmt.Errorf("could not set terminal size: %w", err)
	}

	err = unix.SetNonblock(int(pty.Fd()), true)
	if err != nil {
		return nil, nil, fmt.Errorf("could not set pty fd as nonblocking: %w", err)
	}

	// the pty package opens /dev/ptmx without O_NONBLOCK. This makes go spawn a lot of threads
	// when reading from lots of ptys in goroutines. Let's work around that by duping the file desctiptor
	// into a new one, and creating a new *os.File object with a new async fd
	asyncPtyFd, err := unix.FcntlInt(pty.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("could not dup pty fd (number %v): %w", pty.Fd(), err)
	}
	defer haveToClose("synchronous /dev/ptmx", pty)
	defer func() {
		if err != nil {
			_ = unix.Close(asyncPtyFd)
		}
	}()

	err = unix.SetNonblock(asyncPtyFd, true)
	if err != nil {
		return nil, nil, fmt.Errorf("could not set O_NONBLOCK to file descriptor %v duped from /dev/ptmx: %w", asyncPtyFd, err)
	}

	return os.NewFile(uintptr(asyncPtyFd), "nonblocking /dev/ptmx"), tty, err
}

func runInteractive(cmd *exec.Cmd) *Output {
	out := &Output{}
	var stdoutTty, stderrTty *os.File

	size, err := ptyPkg.GetsizeFull(os.Stdout)
	if err != nil {
		log.Fatalf("Could not get terminal size: %v\n", err)
	}

	out.stdoutPipeOrPty, stdoutTty, err = createPty(size)
	if err != nil {
		log.Fatalf("Couldn't create a pty for %v's stdout: %v\n", cmd.Args, err)
	}
	defer haveToClose("stdout tty", stdoutTty)

	out.stderrPipeOrPty, stderrTty, err = createPty(size)
	if err != nil {
		log.Fatalf("Couldn't create a pty for %v's stderr: %v\n", cmd.Args, err)
	}
	defer haveToClose("stderr tty", stderrTty)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    1,
	}

	out.winchSignal = make(chan os.Signal, 1)
	signal.Notify(out.winchSignal, syscall.SIGWINCH)
	go func() {
		for range out.winchSignal {
			// TODO: this should handle just one of stderr/stdout being closed

			size, err := ptyPkg.GetsizeFull(os.Stdout)
			if err != nil {
				log.Fatalf("Could not get terminal size on sigwinch: %v\n", err)
			}

			err = ptyPkg.Setsize(out.stdoutPipeOrPty, size)
			if err != nil {
				log.Fatalf("Could not set stdout terminal size for command %v on sigwinch: %v\n", cmd.Args, err)
			}

			err = ptyPkg.Setsize(out.stderrPipeOrPty, size)
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
