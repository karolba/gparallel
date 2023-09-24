package main

import (
	"log"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"syscall"
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

var dataDir = sync.OnceValue(func() (dir string) {
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
	if err != nil && currentUser.Username != "" {
		return filepath.Join(dir, ".gparallel")
	} else {
		return filepath.Join(dir, ".gparallel-"+currentUser.Username)
	}
})

// stdoutAndStderrAreTheSame tells us if stdout and stderr point to the same file/pipe/stream, for the sole purpose
// of conserving pty/tty pairs - which are a very limited resource on most unix systems (linux default max: usually
// from 512 to 4096, macOS default max: from 127 to 512)
var stdoutAndStderrAreTheSame = sync.OnceValue(func() bool {
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
