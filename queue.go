package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alessio/shellescape"
	"github.com/shirou/gopsutil/v3/process"
)

type QueuedCommand struct {
	QueuedFrom struct {
		Pid     int
		Command []string
	}
	QueuedFor struct {
		Pid       int
		StartedAt int64
	}
	Command []string
}

func queueDataPath(pid int) string {
	var dir string

	if _, err := os.Stat("/dev/shm"); !os.IsNotExist(err) {
		dir = "/dev/shm"
	} else if _, err := os.Stat(os.TempDir()); !os.IsNotExist(err) {
		dir = os.TempDir()
	} else if _, err := os.Stat("/tmp"); !os.IsNotExist(err) {
		dir = "/tmp"
	} else {
		dir = "."
	}

	return filepath.Join(dir, ".gparallel", strconv.Itoa(pid), "queue")
}

func appendableQueueDataFile(pid int) *os.File {
	path := queueDataPath(pid)

	if err := os.MkdirAll(filepath.Dir(path), fs.ModePerm); err != nil {
		log.Fatalln("Cannot create directory", filepath.Dir(path), ":", err)
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, fs.ModePerm)
	if err != nil {
		log.Fatalln("Could not create queue file", file, ":", err)
	}

	return file
}

func readQueueDataFile(pid int) (file *os.File, exists bool) {
	path := queueDataPath(pid)

	if err := os.MkdirAll(filepath.Dir(path), fs.ModePerm); err != nil {
		log.Fatalln("Cannot create directory", filepath.Dir(path), ":", err)
	}

	file, err := os.OpenFile(path, os.O_RDONLY, fs.ModePerm)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		log.Fatalln("Could not open queue file", file, ":", err)
	}

	return file, true
}

func queueCommand(command []string, forPid int) {
	proc, err := process.NewProcess(int32(forPid))
	if err != nil {
		log.Fatalf("Did not queue command %s - couldn't find process with pid %d: %v\n", shellescape.QuoteCommand(command), forPid, err)
	}

	createTime, err := proc.CreateTime()
	if err != nil {
		name, _ := proc.Name()
		log.Fatalf("Did not queue command %s - couldn't get pid %d (%s) creation time: %v\n", shellescape.QuoteCommand(command), forPid, name, err)
	}

	qc := QueuedCommand{}
	qc.Command = command
	qc.QueuedFrom.Pid = os.Getpid()
	qc.QueuedFrom.Command = os.Args
	qc.QueuedFor.Pid = forPid
	qc.QueuedFor.StartedAt = createTime

	// Write the command to the queue file
	queue := appendableQueueDataFile(forPid)
	defer haveToClose("queue file", queue)

	err = json.NewEncoder(queue).Encode(qc)
	if err != nil {
		log.Fatalf("Could not write to queue file (%v): %v\n", queue.Name(), err)
	}
}

func queueCommandForAncestor(command []string, ancestorName string) {
	parent, err := process.NewProcess(int32(os.Getppid()))
	if err != nil {
		log.Fatalf("Error getting process info for parent (pid %v): %v\n", parent.Pid, err)
	}

	for {
		name, err := parent.Name()
		if err != nil {
			log.Fatalf("Error getting process name for process with pid %v: %v\n", parent.Pid, err)
		}

		if strings.Contains(name, ancestorName) {
			break
		}

		if parent.Pid <= 1 {
			log.Fatalf("Could not find an ancestor process with the name \"%s\"\n", ancestorName)
		}

		grandParent, err := parent.Parent()
		if err != nil {
			log.Fatalf("Error getting parent process for parent of %v: %v\n", parent.Pid, err)
		}
		parent = grandParent
	}

	queueCommand(command, int(parent.Pid))
}

func queueCommandForParent(command []string) {
	queueCommand(command, os.Getppid())
}

func startProcessesFromQueue(result chan<- ProcessResult) {
	// start from our pid, not ppid, in case `gparallel --wait` is placed at the end of a shellscript, which would
	// automatically turn it into `exec gparallel --wait` as an optimisation
	procWithQueue, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		log.Fatalf("Could not get proces information about ourselves (pid %d) %v\n", os.Getpid(), err)
	}

	ourCreateTime, err := procWithQueue.CreateTime()
	if err != nil {
		log.Fatalln("Couldn't get the creation time of this process:", err)
	}

	var queueFile *os.File
	var exists bool
	for {
		queueFile, exists = readQueueDataFile(int(procWithQueue.Pid))
		if exists {
			break
		}
		procWithQueue, err = procWithQueue.Parent()
		if err != nil {
			// Don't make this an explicit error, rather, just a warning
			_, _ = fmt.Fprintf(os.Stderr, "%s: Could not find any parent process with an active queue\n", os.Args[0])
			return
		}
	}

	reader := bufio.NewReader(queueFile)
	for {
		line, err := reader.ReadBytes('\n')

		if len(line) > 0 {
			qc := QueuedCommand{}
			if err := json.Unmarshal(line, &qc); err != nil {
				log.Fatalf("Could not parse queue line '%s' from file '%s': %v\n", string(line), queueFile.Name(), err)
			}

			// make sure we don't start processes queued from before we (this process) were born. This ensures we don't start
			// any unnecessary commands in case of PID rollover
			if qc.QueuedFor.StartedAt > ourCreateTime {
				continue
			}

			if noLongerSpawnChildren.Load() {
				break
			}
			result <- run(qc.Command)
		}

		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatalf("Failed reading: %v\n", err)
		}
	}

	// Remove the queue file when we're successfully done with it
	// not doing that isn't the end of the world (due to the StartedAt check), so don't error out
	// if it's not successful
	if err := os.Remove(queueFile.Name()); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s: Warning: could not remove the queue file(%s): %v\n", os.Args[0], queueFile.Name(), err)
	}
}
