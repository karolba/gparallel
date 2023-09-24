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

var (
	flExecuteAndFlushTty     = flag.Bool("_execute-and-flush-tty", false, "Execute a given command and flush attached ttys afterwards. Used internally by gparallel.")
	flFromStdin              = flag.BoolP("from-stdin", "s", false, "Get input from stdin.")
	flHelp                   = flag.BoolP("help", "h", false, "Show this help message.")
	flKeepGoingOnError       = flag.Bool("keep-going-on-error", false, "Don't exit on error, keep going.")
	flMaxMemory              = flag.String("max-mem", "5%", "How much system `memory` can be used for storing command outputs before we start blocking.\nSet to 'inf' to disable the limit.")
	flMaxProcesses           = flag.IntP("max-concurrent", "P", max(runtime.NumCPU(), 2), "How many concurrent `children` to execute at once at maximum.\n(minimum 2, default based on the amount of cores)")
	flMaxProcessesUpperLimit = flag.Int("max-concurrent-upper-limit", max(runtime.NumCPU(), 2), "The upper limit of maximum processes when inferring them from the number of CPUs.")
	flQueueCommandAncestor   = flag.String("queue-command-ancestor", "", "Queue a command for a specific ancestor process with a `name` to later execute with --wait.")
	flQueueCommandParent     = flag.Bool("queue-command", false, "Queue a command for parent of gparellel to later execute with --wait.")
	flQueueCommandPid        = flag.Int("queue-command-pid", -1, "Queue a command for a specific ancestor `pid` to let it later execute it with --wait.")
	flQueueWait              = flag.Bool("wait", false, "Execute and wait for commands queued using --queue-*.")
	flRecursiveProcessLimit  = flag.Bool("recursive-max-concurrent", true, "Whether to apply the one -P children limit to all gparallel subprocesses as well as a shared\nresource.")
	flShowQueue              = flag.Bool("show-queue", false, "Show every queued command for every process - useful for debugging missing --wait calls.")
	flSlurpStdin             = flag.Bool("slurp-stdin", false, "Read all available stdin and pass it onto the command - only works in the --queue-command-* mode.\n(as otherwise it would send everything to the first command).")
	flTemplate               = flag.StringP("replacement", "I", "{}", "The `replacement` string.")
	flVerbose                = flag.BoolP("verbose", "v", false, "Print the full command line before each execution.")
	flVersion                = flag.Bool("version", false, "Show the program version.")

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

func countTrue(values ...bool) (i int) {
	for _, v := range values {
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
}

func exitWithUsage(exitCode int) {
	usage()
	os.Exit(exitCode)
}

func errorWithUsage(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "%s: Argument error: "+format+"\n\n", append([]any{os.Args[0]}, args...))
	exitWithUsage(1)
}

func parseArgs() Args {
	flag.Usage = usage
	flag.SetInterspersed(false)
	_ = flag.CommandLine.MarkHidden("_execute-and-flush-tty")
	flag.Parse()

	if *flVersion {
		showVersion()
		os.Exit(0)
	}

	if *flHelp {
		exitWithUsage(0)
	}

	parsedFlMaxMemory = maxMemoryFromFlag()
	*flMaxProcesses = min(*flMaxProcesses, *flMaxProcessesUpperLimit)

	args := flag.Args()

	queueModeEnabled := *flQueueCommandParent || *flQueueCommandAncestor != "" || *flQueueCommandPid != -1

	flagsPreventingFurtherArguments := countTrue(
		*flQueueWait,
		*flShowQueue,
	)

	exclusiveFlags := flagsPreventingFurtherArguments + countTrue(
		*flFromStdin,
		*flExecuteAndFlushTty,
		queueModeEnabled,
	)

	if len(args) == 0 && flagsPreventingFurtherArguments == 0 {
		exitWithUsage(1)
	}

	if *flMaxProcesses <= 1 {
		errorWithUsage("-P (--max-concurrent) cannot be less than 2")
	}

	if exclusiveFlags > 1 {
		errorWithUsage("Cannot specify %v, %v, %v, %v, and %v (or %v, or %v) at the same time",
			"--from-stdin",
			"--_execute-and-flush-tty",
			"--wait",
			"--show-queue",
			"--queue-command",
			"--queue-command-ancestor",
			"--queue-command-pid")
	}

	if *flSlurpStdin && !queueModeEnabled {
		errorWithUsage("The --slurp-stdin flag can only be specified with %s, %s, or %s",
			"--queue-command",
			"--queue-command-pid",
			"--queue-command-ancestor")
	}

	subcommandSupportsTripleColon := exclusiveFlags < 1

	if subcommandSupportsTripleColon {
		threeColons := slices.Index(args, ":::")
		foundTripleColon := threeColons != -1

		if !*flFromStdin && !foundTripleColon {
			errorWithUsage("don't know where to get arguments from: neither -s (--from-stdin) nor \":::\" specified in the arguments")
		}

		if foundTripleColon {
			return Args{
				command:        args[0:threeColons],
				data:           args[threeColons+1:],
				hasTripleColon: true,
			}
		}
	}

	return Args{
		command: args,
		data:    []string{},
	}
}

func maxMemoryFromFlag() int64 {
	totalMemory := memoryStats.TotalMemory()

	if *flMaxMemory == "inf" {
		return int64(totalMemory)
	}

	if !strings.HasSuffix(*flMaxMemory, "%") {
		errorWithUsage("the [--max-mem memory] flag only accepts 'number%%' and 'inf' as values, but got '%s'\n", *flMaxMemory)
	}

	percentage, err := strconv.ParseFloat(strings.TrimSuffix(*flMaxMemory, "%"), 64)
	if err != nil {
		errorWithUsage("Invalid value of the --max-mem flag: %v", err)
	}

	if percentage < 0 {
		errorWithUsage("Invalid value of the --max-mem flag - the value cannot be negative")
	}

	// decrease by a little bit to cover for Go's overhead. determined by experimentation and observation
	percentage *= 0.98

	return int64(float64(totalMemory) * percentage / 100.0)
}
