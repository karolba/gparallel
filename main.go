package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alessio/shellescape"
	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
	"golang.org/x/term"
)

var args Args

var noLongerSpawnChildren = atomic.Bool{}

var bold = color.New(color.Bold).SprintFunc()
var yellow = color.New(color.FgYellow).SprintFunc()

var stdoutIsTty = isatty.IsTerminal(uintptr(syscall.Stdout))

func writeOut(out *Output) {
	var clearedOutBytes int64

	var offset int
	for chunk, ok := out.nextChunk(&offset); ok; {
		fd, content := chunk[0], chunk[1:]
		_, _ = syscall.Write(int(fd), content)

		clearedOutBytes += int64(len(content))
	}

	out.allocator.mustFree(out.parts)
	out.parts = nil
	out.allocator.mustClose()

	// just freed a bit of memory, so let's hope running a gc cycle does something
	runtime.GC()

	mem.freedMemory.L.Lock()
	defer mem.freedMemory.L.Unlock()

	mem.currentlyStored.Store(-clearedOutBytes)
	mem.currentlyInTheForeground = out
	mem.freedMemory.Broadcast()
}

func toForeground(proc ProcessResult) (exitCode int) {
	proc.output.partsMutex.Lock()
	writeOut(proc.output)
	proc.output.shouldPassToParent = true
	proc.output.partsMutex.Unlock()

	err := proc.wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err != nil {
		log.Fatal(err)
	}
	return 0
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

func waitForChildrenAfterAFailedOne(processes <-chan ProcessResult) {
	wg := sync.WaitGroup{}

	for processResult := range processes {
		processResult := processResult

		_ = processResult.cmd.Process.Signal(syscall.SIGTERM)

		wg.Add(1)
		go func() {
			_ = processResult.wait()
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

	// additionally, restore the cursor (TODO: and other things) to be visible again just in case it has been hidden
	if stdoutIsTty {
		// TODO: restore a bit more.

		fmt.Print("\x1b[?25h") // make the cursor visible
		fmt.Print("\x1b[?0c")  // restore the cursor to its default shape
	}
}

func startProcessesFromCliArguments(args Args, result chan<- ProcessResult) {
	for _, argument := range args.data {
		if noLongerSpawnChildren.Load() {
			break
		}

		processCommand := instantiateCommandString(slices.Clone(args.command), argument)
		result <- run(processCommand)
	}

	close(result)
}

func startProcessesFromStdin(args Args, result chan<- ProcessResult) {
	stdinReader := bufio.NewReader(os.Stdin)

	for {
		line, err := stdinReader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")

		if len(line) > 0 {
			processCommand := instantiateCommandString(slices.Clone(args.command), line)
			result <- run(processCommand)
		}

		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatalf("Failed reading: %v\n", err)
		}
	}

	close(result)
}

func start(args Args) (exitCode int) {
	var originalTermState *term.State
	var err error

	if stdoutIsTty {
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

	processes := make(chan ProcessResult, *flMaxProcesses)
	if args.hasTripleColon {
		go startProcessesFromCliArguments(args, processes)
	} else {
		go startProcessesFromStdin(args, processes)
	}

	firstProcess := true
	for processResult := range processes {
		if *flVerbose {
			quotedCommand := shellescape.QuoteCommand(processResult.cmd.Args)

			if firstProcess || !stdoutIsTty {
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

func main() {
	log.SetFlags(0)
	log.SetPrefix(fmt.Sprintf("%s: ", os.Args[0]))

	tryToIncreaseNoFile()

	args = parseArgs()

	exitCode := start(args)

	os.Exit(exitCode)
}
