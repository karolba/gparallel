package main

import (
	"fmt"
	"os"
	"runtime"
	"slices"

	flag "github.com/spf13/pflag"
)

type Args struct {
	command        []string
	data           []string
	hasTripleColon bool
}

var (
	flVerbose          = flag.BoolP("verbose", "v", false, "print the full command line before each execution")
	flTemplate         = flag.StringP("replacement", "I", "{}", "the `replacement` string")
	flKeepGoingOnError = flag.Bool("keep-going-on-error", false, "don't exit on error, keep going")
	flMaxProcesses     = flag.IntP("max-concurrent", "P", runtime.NumCPU(), "how many concurrent `children` to execute at once at maximum (defaults to the amount of cores)")
)

func usage() {
	_, _ = fmt.Fprintf(os.Stderr, "Usage: %s [-v] [-P proc] [-I replacement] command [arguments] ::: arguments\n", os.Args[0])
	_, _ = fmt.Fprintf(os.Stderr, "       %s [-v] [-P proc] [-I replacement] command [arguments] < arguments-in-lines\n\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(1)
}

func parseArgs() Args {
	flag.Usage = usage
	flag.SetInterspersed(false)
	flag.Parse()

	args := flag.Args()

	if len(args) == 0 {
		usage()
	}

	threeColons := slices.Index(args, ":::")
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
}
