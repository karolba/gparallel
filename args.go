package main

import (
	"fmt"
	"os"
	"slices"

	flag "github.com/spf13/pflag"
)

type Args struct {
	command []string
	data    []string
}

var (
	flVerbose          = flag.BoolP("verbose", "v", false, "print the full command line before each execution")
	flTemplate         = flag.StringP("replacement", "I", "{}", "the `replacement` string")
	flKeepGoingOnError = flag.Bool("keep-going-on-error", false, "don't exit on error, keep going")
)

func usage() {
	_, _ = fmt.Fprintf(os.Stderr, "Usage: %s [-v] [-I [replacement]] command [arguments] ::: arguments\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(1)
}

func parseArgs() Args {
	flag.Usage = usage
	flag.SetInterspersed(false)
	flag.Parse()

	args := flag.Args()

	threeColons := slices.Index(args, ":::")
	if threeColons == -1 {
		usage()
	}

	return Args{
		command: args[0:threeColons],
		data:    args[threeColons+1:],
	}
}
