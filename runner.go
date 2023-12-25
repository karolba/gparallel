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
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/alessio/shellescape"
	ptyPkg "github.com/creack/pty"
	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

const MAXBUF = 32 * 1024

type Output struct {
	parts              []byte
	partsMutex         sync.Mutex
	shouldPassToParent struct {
		value      bool
		becameTrue chan struct{}
	}
	stdoutPipeOrPty     *os.File
	stderrPipeOrPty     *os.File
	stdoutVirtualScreen *Screen
	stderrVirtualScreen *Screen
	winchSignal         chan os.Signal
	streamClosed        chan struct{}
	allocator           chunkAllocator
}

func NewOutput() *Output {
	o := &Output{}
	o.shouldPassToParent.becameTrue = make(chan struct{}, 2)
	return o
}

type ProcessResult struct {
	startedAt       time.Time
	output          *Output
	originalCommand []string
	cmd             *exec.Cmd
	exitCode        chan int
}

func (proc *ProcessResult) isAlive() bool {
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

func (proc *ProcessResult) wait() error {
	defer recursiveTaskLimitClient().del(proc)

	// wait for both stdout and stderr if we opened two readers
	<-proc.output.streamClosed
	if !stdoutAndStderrAreTheSame() {
		<-proc.output.streamClosed
	}

	signal.Stop(proc.output.winchSignal)

	return proc.cmd.Wait()
}

func (out *Output) appendOrWrite(buf []byte, dataFromFd int, assumedShouldPassToParent bool, screen *Screen) {
	out.partsMutex.Lock()
	defer out.partsMutex.Unlock()

	if out.shouldPassToParent.value != assumedShouldPassToParent && screen != nil {
		// We know for sure (due to using the mutex) this is now a foreground process that should pass its data
		// directly to stdout/stderr - but our calling function doesn't know that yet - oops.
		//
		// The chunk storage for this process no longer exists, so can't do out.appendChunk()
		//
		// This wouldn't be a problem (just write to stdout/stderr!) if not for the virtual terminal screen emulation
		// present in interactive mode. Writing child's output to stdout/stderr before the screen dumps all its contents
		// is bound to produce out-of-order output.
		//
		// Let's just pass it along to the virtual screen for later processing then.
		//
		// Could probably get rid of this with some heavy refactoring, to make this function less bloated.
		// (if only golang supported using sync.RWMutexes in sync.Cond, this whole situation wouldn't have happened)
		screen.Advance(buf)
		return
	}

	if out.shouldPassToParent.value {
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

func readContinuouslyTo(stream io.ReadCloser, throughVirtualScreen *Screen, out *Output, fileDescriptor int) {
	// Track out.shouldPassToParent ourselves (and update via a channel) to avoid race conditions
	shouldPassToParent := false

	buffer := make([]byte, MAXBUF)

	for {
		var count int
		var err error

		select {
		case countWithError := <-toChannelWithError[int](func() (int, error) { return stream.Read(buffer) }):
			count = countWithError.value
			err = countWithError.err
		case <-out.shouldPassToParent.becameTrue:
			shouldPassToParent = true
			if throughVirtualScreen != nil {
				// We became the foreground process ourselves - dump the last visible screen lines (non-scrollback)
				// to the output.
				throughVirtualScreen.End()
			}
		}

		if count > 0 {
			if throughVirtualScreen == nil || shouldPassToParent {
				waitIfUsingTooMuchMemory(chunkSizeWithHeader(buffer[:count]), out)
				out.appendOrWrite(buffer[:count], fileDescriptor, shouldPassToParent, throughVirtualScreen)
			} else {
				throughVirtualScreen.Advance(buffer[:count])
			}
		}

		if throughVirtualScreen != nil && len(throughVirtualScreen.queuedScrollbackOutput) > 0 {
			waitIfUsingTooMuchMemory(chunkSizeWithHeader(throughVirtualScreen.queuedScrollbackOutput), out)
			out.appendOrWrite(throughVirtualScreen.queuedScrollbackOutput, fileDescriptor, shouldPassToParent, throughVirtualScreen)
			throughVirtualScreen.queuedScrollbackOutput = []byte{}
		}

		if err != nil {
			if err == io.EOF {
				haveToClose("child stdout/stderr after EOF", stream)
				break
			}
			if errors.Is(err, fs.ErrClosed) {
				break
			}
			var pathError *os.PathError
			if errors.As(err, &pathError) && pathError.Err == syscall.EIO {
				// Returning EIO is Linux's way of saying the other end is closed when reading from a ptmx:
				// https://github.com/creack/pty/issues/21
				// On BSDs (and macOS) this is signaled by a simple EOF
				haveToClose("child stdout/stderr after EIO", stream)
				break
			}
			log.Fatalf("error from read: %v\n", err)
		}
	}

	// The process died (or at least closed its output) so need to dump the last visible screen lines into the output
	if throughVirtualScreen != nil {
		throughVirtualScreen.End()
		waitIfUsingTooMuchMemory(chunkSizeWithHeader(throughVirtualScreen.queuedScrollbackOutput), out)
		out.appendOrWrite(throughVirtualScreen.queuedScrollbackOutput, fileDescriptor, shouldPassToParent, throughVirtualScreen)
		throughVirtualScreen.queuedScrollbackOutput = []byte{}
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
	// when reading from lots of ptys in goroutines. Let's work around that by duping the file descriptor
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
	// set GOMAXPROCS to 1 to make the process running executeAndFlushTty a bit lighter - it's a really lightweight
	// job, so it shouldn't consume much resources at all
	cmd.Env = os.Environ()
	if originalGoMaxProcs, exists := os.LookupEnv("GOMAXPROCS"); exists {
		cmd.Env = append(cmd.Env, fmt.Sprintf("_GPARALLEL_ORIGINAL_GOMAXPROCS=%s", originalGoMaxProcs))
	}
	cmd.Env = append(cmd.Env, "GOMAXPROCS=1")

	out := NewOutput()
	var stdoutTty, stderrTty *os.File

	size, err := ptyPkg.GetsizeFull(os.Stdout)
	if err != nil {
		log.Fatalf("Could not get terminal size: %v\n", err)
	}

	out.stdoutVirtualScreen = NewScreen(size.Cols, size.Rows)
	if stdoutAndStderrAreTheSame() {
		out.stderrVirtualScreen = out.stdoutVirtualScreen
	} else {
		out.stderrVirtualScreen = NewScreen(size.Cols, size.Rows)
	}

	out.stdoutPipeOrPty, stdoutTty, err = createPty(size)
	if err != nil {
		log.Fatalf("Couldn't create a pty for %v's stdout: %v\n", cmd.Args, err)
	}
	defer haveToClose("stdout tty", stdoutTty)

	if stdoutAndStderrAreTheSame() {
		out.stderrPipeOrPty, stderrTty = out.stdoutPipeOrPty, stdoutTty
	} else {
		out.stderrPipeOrPty, stderrTty, err = createPty(size)
		if err != nil {
			log.Fatalf("Couldn't create a pty for %v's stderr: %v\n", cmd.Args, err)
		}
		defer haveToClose("stderr tty", stderrTty)
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
			// TODO: this should handle just one of stderr/stdout being closed and the other resizing

			size, err := ptyPkg.GetsizeFull(os.Stdout)
			if err != nil {
				log.Fatalf("Could not get terminal size on sigwinch: %v\n", err)
			}

			// Resize the in-kernel virtual pty (to propagate the resize)
			_ = ptyPkg.Setsize(out.stdoutPipeOrPty, size)
			if !stdoutAndStderrAreTheSame() {
				_ = ptyPkg.Setsize(out.stderrPipeOrPty, size)
			}

			// Resize our own in-process terminal screen representation
			out.stdoutVirtualScreen.Resize(size.Rows, size.Cols)
			if !stdoutAndStderrAreTheSame() {
				out.stderrVirtualScreen.Resize(size.Rows, size.Cols)
			}
		}
	}()

	if cmd.Stdin == nil {
		cmd.Stdin = stdoutTty
	}
	cmd.Stdout = stdoutTty
	cmd.Stderr = stderrTty

	err = cmd.Start()
	if err != nil {
		// TODO: take the :2 in the error message only if --_execute-and-flush-tty is used - if not using it is even implemented
		log.Fatalf("Could not start %v: %v\n", shellescape.QuoteCommand(cmd.Args[2:]), err)
	}

	return out
}

func runNonInteractive(cmd *exec.Cmd) *Output {
	var err error
	var stdoutWritePipe, stderrWritePipe *os.File

	out := NewOutput()
	out.stdoutPipeOrPty, stdoutWritePipe, err = os.Pipe()
	if err != nil {
		log.Fatalf("Could not create a pipe for %v's stdout: %v\n", cmd.Args, err)
	}
	defer haveToClose("stdout pipe", stdoutWritePipe)

	if stdoutAndStderrAreTheSame() {
		out.stderrPipeOrPty, stderrWritePipe = out.stdoutPipeOrPty, stdoutWritePipe
	} else {
		out.stderrPipeOrPty, stderrWritePipe, err = os.Pipe()
		if err != nil {
			log.Fatalf("Could not create a pipe for %v's stderr: %v\n", cmd.Args, err)
		}
		defer haveToClose("stderr pipe", stderrWritePipe)
	}

	cmd.Stdout = stdoutWritePipe
	cmd.Stderr = stderrWritePipe
	err = cmd.Start()
	if err != nil {
		log.Fatalf("Could not start %v: %v\n", shellescape.QuoteCommand(cmd.Args), err)
	}

	return out
}

// executable behaves like os.Executable(), but doesn't needlessly readlink the path, which is not necessary
// if we don't care where the executable is located at
func executable() string {
	switch runtime.GOOS {
	case "linux", "android":
		return "/proc/self/exe"
	case "netbsd":
		return "/proc/curproc/exe"
	default:
		path, err := os.Executable()
		if err != nil {
			log.Fatalln("Could not locate argv[0] location:", err)
		}
		return path
	}
}

func runWithStdin(command []string, stdin io.Reader) (result *ProcessResult) {
	result = &ProcessResult{}
	result.originalCommand = command
	result.exitCode = make(chan int)

	recursiveTaskLimitClient().addWait(result)

	if stdoutIsTty() {
		command = append([]string{executable(), "--_execute-and-flush-tty"}, command...)
	}

	result.cmd = exec.Command(command[0], command[1:]...)
	result.cmd.Stdin = stdin

	if stdoutIsTty() {
		result.output = runInteractive(result.cmd)
	} else {
		result.output = runNonInteractive(result.cmd)
	}

	result.output.streamClosed = make(chan struct{}, 2)
	go readContinuouslyTo(result.output.stdoutPipeOrPty, result.output.stdoutVirtualScreen, result.output, syscall.Stdout)
	if !stdoutAndStderrAreTheSame() {
		go readContinuouslyTo(result.output.stderrPipeOrPty, result.output.stderrVirtualScreen, result.output, syscall.Stderr)
	}

	result.startedAt = time.Now()

	go func() {
		err := result.wait()

		// Check if our child exited unsuccessfully
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.exitCode <- exitErr.ExitCode()
			return
		}
		if err != nil {
			log.Fatalf("Failed to wait for command %s: %v\n", shellescape.QuoteCommand(command), err)
		}
		result.exitCode <- 0
	}()

	return result
}

func run(command []string) (result *ProcessResult) {
	return runWithStdin(command, nil)
}
