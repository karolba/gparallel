package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	memoryStats "github.com/pbnjay/memory"
	flag "github.com/spf13/pflag"
	"golang.org/x/exp/slices"
)

type Args struct {
	command        []string
	data           []string
	hasTripleColon bool
}

func max(a, b int) int {
	if a > b {
		return a
	} else {
		return b
	}
}

var (
	flFromStdin            = flag.BoolP("from-stdin", "s", false, "Get input from stdin")
	flVersion              = flag.Bool("version", false, "Show program version")
	flVerbose              = flag.BoolP("verbose", "v", false, "Print the full command line before each execution")
	flTemplate             = flag.StringP("replacement", "I", "{}", "The `replacement` string")
	flKeepGoingOnError     = flag.Bool("keep-going-on-error", false, "Don't exit on error, keep going")
	flMaxProcesses         = flag.IntP("max-concurrent", "P", max(runtime.NumCPU(), 2), "How many concurrent `children` to execute at once at maximum (minimum 2, default based on the amount of cores)")
	flMaxMemory            = flag.String("max-mem", "5%", "How much system `memory` can be used for storing command outputs before we start blocking. Set to 'inf' to disable the limit.")
	flExecuteAndFlushTty   = flag.Bool("_execute-and-flush-tty", false, "Execute a given command and flush attached ttys afterwards. Used internally by gparallel.")
	flQueueCommandParent   = flag.Bool("queue-command", false, "Queue a command for parent of gparellel to later execute with --wait")
	flQueueCommandAncestor = flag.String("queue-command-ancestor", "", "Queue a command for a specific ancestor process with a `name` to later execute with --wait")
	flQueueCommandPid      = flag.Int("queue-command-pid", -1, "Queue a command for a specific ancestor `pid` to let it later execute it with --wait")
	flShowQueue            = flag.Bool("show-queue", false, "Show every queued command for every process - useful for debugging missing --wait calls")
	flQueueWait            = flag.Bool("wait", false, "Execute and wait for commands queued using --queue-*")

	parsedFlMaxMemory int64
)

func showVersion() {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		_, _ = fmt.Println("gparallel - version unknown")
		return
	}

	vcs, revision, modified := "", "(unknown)", false
	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs":
			vcs = setting.Value
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}

	if len(revision) == 40 {
		// if we have a long hash, get the shorter one
		revision = revision[0:7]
	}

	gitRev := ""
	if vcs == "git" && !modified {
		gitRev = fmt.Sprintf(", git rev: %s", revision)
	} else if vcs == "git" && modified {
		gitRev = fmt.Sprintf(", git rev: %s (with local changes)", revision)
	}

	_, _ = fmt.Printf("gparallel %s%s, %s\n", buildInfo.Main.Version, gitRev, buildInfo.GoVersion)
}

func countTrue(vals ...bool) (i int) {
	for _, v := range vals {
		if v {
			i += 1
		}
	}
	return i
}

func usage() {
	_, _ = fmt.Fprintf(os.Stderr, "Usage: %s    [-v] [-P proc] [-I replacement] command [arguments] ::: arguments\n", os.Args[0])
	_, _ = fmt.Fprintf(os.Stderr, "       %s -s [-v] [-P proc] [-I replacement] command [arguments] < arguments-in-lines\n", os.Args[0])
	_, _ = fmt.Fprintf(os.Stderr, "       %s --wait\n", os.Args[0])
	_, _ = fmt.Fprintf(os.Stderr, "       %s --queue-command command [arguments]\n", os.Args[0])
	_, _ = fmt.Fprintf(os.Stderr, "       %s --queue-command-pid pid command [arguments]\n", os.Args[0])
	_, _ = fmt.Fprintf(os.Stderr, "       %s --queue-command-ancestor process-name command [arguments]\n\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(1)
}

func parseArgs() Args {
	flag.Usage = usage
	flag.SetInterspersed(false)
	_ = flag.CommandLine.MarkHidden("_execute-and-flush-tty")
	// This one is not yet implemented:
	_ = flag.CommandLine.MarkHidden("show-queue")
	flag.Parse()

	if *flVersion {
		showVersion()
		os.Exit(0)
	}

	parsedFlMaxMemory = maxMemoryFromFlag()

	args := flag.Args()

	if len(args) == 0 && !*flQueueWait {
		usage()
	}

	if *flMaxProcesses <= 1 {
		_, _ = fmt.Fprintf(os.Stderr, "%s: Error: -P (--max-concurrent) cannot be less than 2\n", os.Args[0])
		usage()
	}

	behaviourModifyingFlags := countTrue(
		*flFromStdin,
		*flExecuteAndFlushTty,
		*flQueueWait,
		*flShowQueue,
		*flQueueCommandParent || *flQueueCommandAncestor != "" || *flQueueCommandPid != -1,
	)

	if behaviourModifyingFlags > 1 {
		_, _ = fmt.Fprintf(os.Stderr, "%s: Error: Cannot specify %v, %v, %v, %v, and %v (or %v, or %v) at the same time", os.Args[0],
			"--from-stdin",
			"--_execute-and-flush-tty",
			"--wait",
			"--show-queue",
			"--queue-command", "--queue-command-ancestor", "--queue-command-pid")
		usage()
	}

	subcommandSupportsTripleColon := behaviourModifyingFlags < 1

	if subcommandSupportsTripleColon {
		threeColons := slices.Index(args, ":::")

		if !*flFromStdin && threeColons == -1 {
			_, _ = fmt.Fprintf(os.Stderr, "%s: Error: don't know where to get arguments from: neither -s (--from-stdin) nor \":::\" specified in the arguments\n", os.Args[0])
			usage()
		}

		if threeColons == -1 {
			return Args{
				command:        args,
				data:           []string{},
				hasTripleColon: false,
			}
		} else {
			return Args{
				command:        args[0:threeColons],
				data:           args[threeColons+1:],
				hasTripleColon: true,
			}
		}
	} else {
		return Args{
			command: args,
			data:    []string{},
		}
	}

}

func maxMemoryFromFlag() int64 {
	totalMemory := memoryStats.TotalMemory()

	if *flMaxMemory == "inf" {
		return int64(totalMemory)
	}

	if !strings.HasSuffix(*flMaxMemory, "%") {
		_, _ = fmt.Fprintf(os.Stderr, "%s: Error: the [--max-mem memory] flag only accepts 'number%%' and 'inf' as values, but got '%s'\n", os.Args[0], *flMaxMemory)
		usage()
	}

	percentage, err := strconv.ParseFloat(strings.TrimSuffix(*flMaxMemory, "%"), 64)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s: Invalid value of the --max-mem flag: %v\n", os.Args[0], err)
		usage()
	}

	if percentage < 0 {
		_, _ = fmt.Fprintf(os.Stderr, "%s: Invalid value of the --max-mem flag - the value cannot be negative\n", os.Args[0])
		usage()
	}

	// decrease by a little bit to cover for Go's overhead. determined by observation
	percentage *= 0.98

	return int64(float64(totalMemory) * percentage / 100.0)
}
