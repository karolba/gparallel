package main

import (
	"encoding/binary"
	"log"
	"sync"
	"sync/atomic"
	"unsafe"

	"modernc.org/memory"
)

// make sure we don't use too much RAM for storing command output
var mem = struct {
	childDiedFreeingMemory   *sync.Cond
	currentlyInTheForeground *Output
	currentlyStored          atomic.Int64
}{
	sync.NewCond(&sync.Mutex{}),
	nil,
	atomic.Int64{},
}

type chunkAllocator struct{ memory.Allocator }

func (allocator *chunkAllocator) mustCalloc(size int) []byte {
	r, err := allocator.Calloc(size)
	if err != nil {
		log.Fatalf("Could not allocate memory: %v\n", err)
	}
	return r
}

func (allocator *chunkAllocator) mustRealloc(mem []byte, size int) []byte {
	r, err := allocator.Realloc(mem, size)
	if err != nil {
		log.Fatalf("Could not reallocate memory: %v\n", err)
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

	chunkSizeWithHeader := chunkSize + int(chunkHeaderSize) // + reserve bytes for the size itself

	if len(out.parts) == 0 {
		out.parts = out.allocator.mustCalloc(chunkSizeWithHeader)[:0]
	}

	lenBefore := len(out.parts)
	lenAfter := lenBefore + chunkSizeWithHeader

	if lenAfter > cap(out.parts) {
		newAtLeastCap := lenAfter * 2
		out.parts = out.allocator.mustRealloc(out.parts, newAtLeastCap)[:lenBefore]
	}

	out.parts = out.parts[:lenAfter]

	chunkWithLengthHeader := out.parts[lenBefore:lenAfter]

	binary.LittleEndian.PutUint32(chunkWithLengthHeader, uint32(chunkSize))

	return chunkWithLengthHeader[chunkHeaderSize:]
}

func (out *Output) getNextChunk(start *int) (fd byte, content []byte, ok bool) {
	if *start >= len(out.parts) {
		return 0, nil, false
	}

	chunkSize := int(binary.LittleEndian.Uint32(out.parts[*start:]))
	*start += int(chunkHeaderSize)

	chunk := out.parts[*start : *start+chunkSize]

	if len(chunk) <= 0 {
		log.Panicf("Got an empty chunk from output: %+v\n", out)
	}

	*start += chunkSize
	return chunk[0], chunk[1:], true
}

func chunkSizeWithHeader(data []byte) (size int64) {
	size += int64(chunkHeaderSize)
	size += 1 // the dataFromFd byte
	size += int64(len(data))
	return size
}
