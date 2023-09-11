package main

import (
	"encoding/binary"
	"log"
	"sync"
	"sync/atomic"
	"unsafe"

	memoryStats "github.com/pbnjay/memory"
	"modernc.org/memory"
)

// make sure we don't use too much RAM for storing command output
var mem = struct {
	max                      int64
	freedMemory              *sync.Cond
	currentlyInTheForeground *Output
	currentlyStored          atomic.Int64
}{
	int64(memoryStats.TotalMemory() / 32),
	sync.NewCond(&sync.Mutex{}),
	nil,
	atomic.Int64{},
}

type chunkAllocator struct{ memory.Allocator }

func (allocator *chunkAllocator) mustMalloc(size int) []byte {
	r, err := allocator.Malloc(size)
	if err != nil {
		log.Fatalf("Could not allocate memory: %v\n", err)
	}
	return r
}

func (allocator *chunkAllocator) mustRealloc(mem []byte, size int) []byte {
	r, err := allocator.Realloc(mem, size)
	if err != nil {
		log.Fatalf("Could not allocate memory: %v\n", err)
	}
	return r
}

func (allocator *chunkAllocator) mustFree(mem []byte) {
	if err := allocator.Free(mem); err != nil {
		log.Fatalf("Could not free memory: %v\n", err)
	}
}

func (allocator *chunkAllocator) mustClose() {
	if err := allocator.Close(); err != nil {
		log.Fatalf("Could not close allocator: %v\n", err)
	}
}

func (out *Output) appendChunk(dataFromFd byte, data []byte) {
	chunk := out.newChunk(len(data) + 1) // +1 for dataFromFd

	chunk[0] = dataFromFd
	copy(chunk[1:], data)
}

const chunkHeaderSize = unsafe.Sizeof(uint32(0))

func (out *Output) newChunk(chunkSize int) []byte {
	chunkSize += int(chunkHeaderSize) // + reserve bytes for the size itself

	if len(out.parts) == 0 {
		out.parts = out.allocator.mustMalloc(chunkSize)
	}

	lenBefore := len(out.parts)
	lenAfter := lenBefore + chunkSize

	if lenAfter > cap(out.parts) {
		newAtLeastCap := lenAfter * 2
		out.parts = out.allocator.mustRealloc(out.parts, newAtLeastCap)[:lenBefore]
	}

	chunkWithLength := out.parts[lenBefore:lenAfter]

	binary.NativeEndian.PutUint32(chunkWithLength, uint32(chunkSize))

	return chunkWithLength[chunkHeaderSize:]
}

func (out *Output) nextChunk(start *int) ([]byte, bool) {
	if *start >= len(out.parts) {
		return nil, false
	}

	chunkSize := int(binary.NativeEndian.Uint32(out.parts[*start:]))
	*start += int(chunkHeaderSize)

	chunk := out.parts[*start : *start+chunkSize]

	*start += chunkSize
	return chunk, true
}
