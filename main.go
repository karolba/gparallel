package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alessio/shellescape"
	"github.com/fatih/color"
	"github.com/karolba/gparallel/chann"
	"github.com/pkg/term/termios"
	"golang.org/x/exp/slices"
	"golang.org/x/term"
)

var standardFdToFile = []*os.File{
	0: os.Stdin,
	1: os.Stdout,
	2: os.Stderr,
}

var noLongerSpawnChildren = atomic.Bool{}

var bold = color.New(color.Bold).SprintFunc()
var yellow = color.New(color.FgYellow).SprintFunc()

func writeOut(out *Output) {
	var clearedOutBytes int64

	offset := 0
	for {
		fd, content, ok := out.getNextChunk(&offset)
		if !ok {
			break
		}

		_, _ = standardFdToFile[fd].Write(content)

		clearedOutBytes += chunkSizeWithHeader(content)
	}

	out.allocator.mustFree(out.parts)
	out.allocator.mustClose()
	out.parts = nil

	// Just deallocated a lot due to a child process dying, let's also hint Go to do the same
	debug.FreeOSMemory()

	mem.childDiedFreeingMemory.L.Lock()
	defer mem.childDiedFreeingMemory.L.Unlock()

	mem.currentlyStored.Add(-clearedOutBytes)
	mem.currentlyInTheForeground = out
	mem.childDiedFreeingMemory.Broadcast()
}

func toForeground(proc *ProcessResult) (exitCode int) {
	proc.output.partsMutex.Lock()

	proc.output.shouldPassToParent.value = true
	proc.output.shouldPassToParent.becameTrue <- struct{}{}
	if !stdoutAndStderrAreTheSame() {
		proc.output.shouldPassToParent.becameTrue <- struct{}{}
	}

	writeOut(proc.output)

	proc.output.partsMutex.Unlock()

	return <-proc.exitCode // block until the process exits
}

func tryToIncreaseNoFile() {
	var rLimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return
	}
	rLimit.Cur = rLimit.Max
	_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
}

func waitForChildrenAfterAFailedOne(processes <-chan *ProcessResult) {
	wg := sync.WaitGroup{}

	for processResult := range processes {
		processResult := processResult

		_ = processResult.cmd.Process.Signal(syscall.SIGTERM)

		wg.Add(1)
		go func() {
			<-processResult.exitCode
			wg.Done()
		}()
	}

	wg.Wait()
}

func instantiateCommandString(command []string, argument string) []string {
	if *flTemplate == "" {
		return append(command, argument)
	}

	replacedIn := 0

	for i, word := range command {
		if !strings.Contains(word, *flTemplate) {
			continue
		}

		command[i] = strings.ReplaceAll(command[i], *flTemplate, argument)
		replacedIn += 1
	}

	if replacedIn == 0 {
		// If there's no {}-template anywhere, let's just append the argument at the end
		return append(command, argument)
	} else {
		return command
	}
}

func resetTermStateBeforeExit(originalTermState *term.State) {
	if originalTermState != nil {
		err := term.Restore(syscall.Stdout, originalTermState)
		if err != nil {
			log.Printf("Warning: could not restore terminal state on exit: %v\n", err)
		}
	}
}

func startProcessesFromCliArguments(args Args, result chan<- *ProcessResult) {
	for _, argument := range args.data {
		if noLongerSpawnChildren.Load() {
			break
		}

		result <- run(instantiateCommandString(slices.Clone(args.command), argument))
	}
}

func startProcessesFromStdin(args Args, result chan<- *ProcessResult) {
	stdinReader := bufio.NewReader(os.Stdin)

	for {
		line, err := stdinReader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")

		if noLongerSpawnChildren.Load() {
			break
		}
		if len(line) > 0 {
			result <- run(instantiateCommandString(slices.Clone(args.command), line))
		}

		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatalf("Failed reading: %v\n", err)
		}
	}
}

func displaySequentially(processes <-chan *ProcessResult) (exitCode int) {
	tryToIncreaseNoFile()

	var originalTermState *term.State
	var err error

	if stdoutIsTty() {
		originalTermState, err = term.GetState(syscall.Stdout)
		if err != nil {
			log.Printf("Warning: could get terminal state for stdout: %v\n", err)
		}
	}

	if originalTermState != nil {
		defer resetTermStateBeforeExit(originalTermState)

		signalledToExit := make(chan os.Signal, 1)
		signal.Notify(signalledToExit, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-signalledToExit
			resetTermStateBeforeExit(originalTermState)
			os.Exit(1)
		}()
	}

	firstProcess := true
	for processResult := range processes {
		if *flVerbose {
			quotedCommand := shellescape.QuoteCommand(processResult.originalCommand)

			if firstProcess || !stdoutIsTty() {
				_, _ = fmt.Fprintf(os.Stderr, bold("+ %s")+"\n", quotedCommand)
			} else if !processResult.isAlive() {
				_, _ = fmt.Fprintf(os.Stderr,
					bold("+ %s")+yellow(" (already finished, reporting saved output)")+"\n",
					quotedCommand)
			} else if -time.Until(processResult.startedAt) > 1*time.Second {
				_, _ = fmt.Fprintf(os.Stderr,
					bold("+ %s")+yellow(" (resumed output, already runnning for %v)")+"\n",
					quotedCommand,
					-time.Until(processResult.startedAt).Round(time.Second))
			} else {
				_, _ = fmt.Fprintf(os.Stderr, bold("+ %s")+"\n", quotedCommand)
			}
		}

		exitCode = max(exitCode, toForeground(processResult))

		if !*flKeepGoingOnError {
			if exitCode != 0 {
				noLongerSpawnChildren.Store(true)

				waitForChildrenAfterAFailedOne(processes)
				break
			}
		}

		firstProcess = false
	}

	return exitCode
}

func executeAndFlushTty(command []string) (exitCode int) {
	if originalGomaxprocs := os.Getenv("_GPARALLEL_ORIGINAL_GOMAXPROCS"); originalGomaxprocs != "" {
		_ = os.Unsetenv("_GPARALLEL_ORIGINAL_GOMAXPROCS")
		_ = os.Setenv("GOMAXPROCS", originalGomaxprocs)
	} else {
		_ = os.Unsetenv("GOMAXPROCS")
	}

	path, err := exec.LookPath(command[0])
	if err != nil {
		log.Fatalf("Could not find executable %s: %v\n", command[0], err)
	}

	process, err := os.StartProcess(path, command, &os.ProcAttr{
		Files: standardFdToFile,
	})
	if err != nil {
		log.Fatalf("Could not displaySequentially %s: %v\n", shellescape.QuoteCommand(command), err)
	}

	// this process won't be used for anything much more, let's cap memory usage a bit
	// this reduces memory usage by a couple of megabytes when running a lot of executeAndFlushTtys
	debug.SetMemoryLimit(0)
	debug.FreeOSMemory()

	processState, err := process.Wait()
	if err != nil {
		log.Fatalf("Could not wait for process %v, %v\n", shellescape.QuoteCommand(command), err)
	}

	_ = termios.Tcdrain(uintptr(syscall.Stdout))
	_ = termios.Tcdrain(uintptr(syscall.Stderr))

	return processState.ExitCode()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(fmt.Sprintf("%s: ", os.Args[0]))

	args := parseArgs()

	switch {
	case *flExecuteAndFlushTty:
		os.Exit(executeAndFlushTty(args.command))
	case *flQueueCommandAncestor != "":
		queueCommandForAncestor(args.command, *flQueueCommandAncestor)
		os.Exit(0)
	case *flQueueCommandPid != -1:
		queueCommand(args.command, *flQueueCommandPid)
		os.Exit(0)
	case *flQueueCommandParent:
		queueCommandForParent(args.command)
		os.Exit(0)
	case *flShowQueue:
		showGlobalQueue()
		os.Exit(0)
	}

	if !*flRecursiveProcessLimit {
		_ = os.Unsetenv(EnvGparallelChildLimitSocket)
	}
	if _, hasMasterLimitServer := os.LookupEnv(EnvGparallelChildLimitSocket); !hasMasterLimitServer {
		createLimitServer()
	}

	processes := chann.New[*ProcessResult]()
	go func() {
		defer processes.Close()

		if *flQueueWait {
			startProcessesFromQueue(processes.In())
			return
		}

		if args.hasTripleColon {
			startProcessesFromCliArguments(args, processes.In())
		}
		if *flFromStdin {
			startProcessesFromStdin(args, processes.In())
		}
	}()

	os.Exit(displaySequentially(processes.Out()))
}
