package main

import (
	"log"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/mattn/go-isatty"
)

func max(a, b int) int {
	if a > b {
		return a
	} else {
		return b
	}
}
func min(a, b int) int {
	if a < b {
		return a
	} else {
		return b
	}
}

var stdoutIsTty = onceValue(func() bool {
	return isatty.IsTerminal(uintptr(syscall.Stdout))
})

var dataDir = onceValue(func() (dir string) {
	if _, err := os.Stat("/dev/shm"); !os.IsNotExist(err) {
		dir = "/dev/shm"
	} else if _, err := os.Stat(os.TempDir()); !os.IsNotExist(err) {
		dir = os.TempDir()
	} else if _, err := os.Stat("/tmp"); !os.IsNotExist(err) {
		dir = "/tmp"
	} else {
		dir, err = os.UserHomeDir()
		if err != nil {
			log.Fatalln("Could not get current user's home directory:", err)
		}
	}

	currentUser, err := user.Current()
	if err == nil || currentUser.Username == "" {
		return filepath.Join(dir, ".gparallel")
	} else {
		return filepath.Join(dir, ".gparallel-"+currentUser.Username)
	}
})

// stdoutAndStderrAreTheSame tells us if stdout and stderr point to the same file/pipe/stream, for the sole purpose
// of conserving pty/tty pairs - which are a very limited resource on most unix systems (linux default max: usually
// from 512 to 4096, macOS default max: from 127 to 512)
var stdoutAndStderrAreTheSame = onceValue(func() bool {
	stdoutStat, err := os.Stdout.Stat()
	if err != nil {
		log.Fatalln("Cannot stat stdout:", err)
	}
	stdout, ok := stdoutStat.Sys().(*syscall.Stat_t)
	if !ok {
		// We probably aren't on a Unix - assume stdout and stderr aren't the same
		return false
	}

	stderrStat, err := os.Stderr.Stat()
	if err != nil {
		log.Fatalln("Cannot stat stderr:", err)
	}
	stderr, ok := stderrStat.Sys().(*syscall.Stat_t)
	if !ok {
		// We probably aren't on a Unix - assume stdout and stderr aren't the same
		return false
	}

	return stdout.Dev == stderr.Dev &&
		stdout.Ino == stderr.Ino &&
		stdout.Mode == stderr.Mode &&
		stdout.Nlink == stderr.Nlink &&
		stdout.Rdev == stderr.Rdev
})

func mustSetenv(key, value string) {
	err := os.Setenv(key, value)
	if err != nil {
		log.Fatalln("Couldn't set the value of the", key, "environment variable: ", err)
	}
}

func assert(msg string, condition bool) {
	if !condition {
		log.Panicln("Failed assert:", msg)
	}
}

// an exact copy of sync.OnceValue to support pre-1.21 versions of Go
func onceValue[T any](f func() T) func() T {
	var (
		once   sync.Once
		valid  bool
		p      any
		result T
	)
	g := func() {
		defer func() {
			p = recover()
			if !valid {
				panic(p)
			}
		}()
		result = f()
		valid = true
	}
	return func() T {
		once.Do(g)
		if !valid {
			panic(p)
		}
		return result
	}
}

func channel(f func()) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		f()
		ch <- struct{}{}
	}()
	return ch
}

func toChannel[T any](f func() T) <-chan T {
	ch := make(chan T)
	go func() { ch <- f() }()
	return ch
}

type withError[T any] struct {
	value T
	err   error
}

func toChannelWithError[T any](f func() (T, error)) <-chan withError[T] {
	ch := make(chan withError[T])
	go func() {
		val, err := f()
		ch <- withError[T]{
			value: val,
			err:   err,
		}
	}()
	return ch
}

func getOrDefault[T any](slice []T, index int) (result T) {
	if index < len(slice) {
		result = slice[index]
	}
	return result
}
