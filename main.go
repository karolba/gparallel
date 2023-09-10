package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alessio/shellescape"
	"github.com/fatih/color"
	"golang.org/x/term"
)

var args Args

var bold = color.New(color.Bold).SprintFunc()
var yellow = color.New(color.FgYellow).SprintFunc()

func writeOut(out *Output) {
	for _, part := range out.parts {
		_, _ = syscall.Write(part.fromFd, part.content)
	}
	clear(out.parts)
}

func toForeground(proc ProcessResult) (exitCode int) {
	proc.output.mutex.Lock()
	writeOut(proc.output)
	proc.output.shouldPassToParent = true
	proc.output.mutex.Unlock()

	err := proc.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
		return
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

func waitForChildrenAfterAFailedOne(processes []ProcessResult) {
	for _, processResult := range processes {
		_ = processResult.cmd.Process.Signal(syscall.SIGTERM)
	}

	for _, processResult := range processes {
		_ = processResult.Wait()
	}
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

func main() {
	originalTermState, _ := term.GetState(syscall.Stdout)

	tryToIncreaseNoFile()

	args = parseArgs()

	exitCode := 0

	processCommands := make([][]string, len(args.data))
	processes := make([]ProcessResult, len(args.data))

	for i, argument := range args.data {
		processCommands[i] = instantiateCommandString(slices.Clone(args.command), argument)
	}

	for i, command := range processCommands {
		processes[i] = run(command)
	}

	for i, processResult := range processes {
		if *flVerbose {
			quotedCommand := shellescape.QuoteCommand(processCommands[i])

			if i == 0 {
				_, _ = fmt.Fprintf(os.Stderr, bold("+ %s")+"\n", quotedCommand)
			} else {
				_, _ = fmt.Fprintf(os.Stderr,
					bold("+ %s")+yellow(" (started %v ago, continuing output)")+"\n",
					quotedCommand,
					-time.Until(processResult.startedAt))
			}
		}

		exitCode = max(exitCode, toForeground(processResult))

		if !*flKeepGoingOnError {
			if exitCode != 0 && i < len(processes)-1 {
				waitForChildrenAfterAFailedOne(processes[i+1:])
				break
			}
		}
	}

	if originalTermState != nil {
		_ = term.Restore(syscall.Stdout, originalTermState)
	}

	os.Exit(exitCode)
}
