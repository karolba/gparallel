package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/exp/slices"
)

const EnvGparallelChildLimitSocket = "_GPARALLEL_CHILD_LIMIT_SOCKET"

func readOneByte(reader io.Reader) error {
	var b [1]byte
	_, err := reader.Read(b[:])
	return err
}

func writeOneByte(writer io.Writer) error {
	_, err := writer.Write([]byte{1})
	return err
}

func serveClients(listener net.Listener, acceptNewTasks bool) {
	for {
		conn, err := listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			break
		}
		if err != nil {
			log.Fatalf("Error accepting connection on the %s unix socket: %v\n", os.Getenv(EnvGparallelChildLimitSocket), err)
		}

		if acceptNewTasks {
			_ = writeOneByte(conn)
		}

		err = readOneByte(conn)
		if errors.Is(err, net.ErrClosed) {
			break
		}

		_ = conn.Close()
	}
}

func createLimitServer() {
	listenPath := filepath.Join(dataDir(), strconv.Itoa(os.Getpid()), "processlimit")
	if err := os.MkdirAll(filepath.Dir(listenPath), fs.ModePerm); err != nil {
		log.Fatalf("Couldn't create directory '%s': %v\n", filepath.Dir(listenPath), err)
	}

	// if we've previously crashed (or exited unexpectedly) there could be an old socket file
	// left over at the same location if PID rollover happens. Let's try to remove it then to
	// be safe in case it exists
	_ = os.Remove(listenPath)

	mustSetenv(EnvGparallelChildLimitSocket, listenPath)

	listener, err := net.Listen("unix", listenPath)
	if err != nil {
		log.Fatalf("Couldn't listen on unix socket '%s': %v\n", listenPath, err)
	}

	// Every process has the ability to spawn 1 child of its own, and as many other children
	// as there are active serveClients goroutines. That's why we spawn (*flMaxProcesses-1)
	// of them.
	for i := 0; i < *flMaxProcesses-1; i++ {
		go serveClients(listener, true)
	}

	// If only foreground processes are allowed, still respond to task limit queries, but
	// never allow anything, to be able to use the same logic in clients
	if *flMaxProcesses == 1 {
		go serveClients(listener, false)
	}
}

var recursiveTaskLimitClient = onceValue(func() (client struct {
	// waits before we're allowed to start a new process
	addWait func(result *ProcessResult)

	// called when a process dies, to make room for a new one
	del func(result *ProcessResult)
}) {
	type ProcessQueueData struct {
		processResult *ProcessResult
		cancel        context.CancelFunc
		conn          net.Conn
	}
	queue := make([]*ProcessQueueData, 0, *flMaxProcesses)
	findInQueue := func(result *ProcessResult) (index int, queueData *ProcessQueueData) {
		for i, processQueueData := range queue {
			if processQueueData.processResult == result {
				return i, processQueueData
			}
		}
		log.Panicf("Could not find process %+v in recursiveTaskLimitClient.queue\n", result)
		return 0, nil
	}
	mutex := &sync.Mutex{}

	serverSocketPath := os.Getenv(EnvGparallelChildLimitSocket)
	if serverSocketPath == "" {
		log.Panicln("Wanted to connect to the master gparallel instance, but", EnvGparallelChildLimitSocket, "is empty")
	}

	// addWait should not be called concurrently with other addWaits, as that would make child startup order
	// essentially random
	client.addWait = func(result *ProcessResult) {
		mutex.Lock()
		defer mutex.Unlock()

		processQueueData := &ProcessQueueData{processResult: result}
		queue = append(queue, processQueueData)

		if len(queue) == 1 {
			return
		}

		dialer := net.Dialer{}
		ctx, cancel := context.WithCancel(context.Background())
		processQueueData.cancel = cancel

		mutex.Unlock()
		conn, err := dialer.DialContext(ctx, "unix", serverSocketPath)
		mutex.Lock()

		processQueueData.conn = conn

		if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
			return
		} else if err != nil {
			log.Fatalf("Couldn't connect to Unix socket '%s': %v\n", serverSocketPath, err)
		}

		mutex.Unlock()
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case err = <-toChannel(func() error { return readOneByte(conn) }):
		}
		mutex.Lock()

		if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
			return
		} else if err != nil {
			log.Fatalf("Couldn't read from unix socket '%s': %v\n", serverSocketPath, err)
		}
	}
	client.del = func(zombieProcess *ProcessResult) {
		mutex.Lock()
		defer mutex.Unlock()

		assert("the foreground process does not have a connection the the master gparallel server attached", queue[0].conn == nil)

		var toClose *ProcessQueueData
		if zombieProcess == queue[0].processResult && len(queue) >= 2 {
			toClose = queue[1]
		} else {
			_, toClose = findInQueue(zombieProcess)
		}

		if toClose != nil && toClose.conn != nil {
			_ = writeOneByte(toClose.conn)
			haveToClose("connection to master gparallel", toClose.conn)
			toClose.conn = nil
		} else if toClose != nil && toClose.cancel != nil {
			toClose.cancel()
		}

		idx, _ := findInQueue(zombieProcess)
		queue[idx] = nil
		queue = slices.Delete(queue, idx, idx+1)
	}
	return client
})
