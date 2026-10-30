package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var registry = struct {
	sync.RWMutex
	engines map[uint64]*engine
}{engines: make(map[uint64]*engine)}
var nextID atomic.Uint64

func lookup(id C.uint64_t) *engine {
	registry.RLock()
	defer registry.RUnlock()
	return registry.engines[uint64(id)]
}

//export H264EngineStart
func H264EngineStart(config *C.char, key *C.uint8_t, keyLength C.int, errorOut **C.char) C.uint64_t {
	if errorOut != nil {
		*errorOut = nil
	}
	fail := func(err error) C.uint64_t {
		if errorOut != nil {
			*errorOut = C.CString(err.Error())
		}
		return 0
	}
	if config == nil || key == nil || keyLength != 32 {
		return fail(fmt.Errorf("invalid engine configuration or shared key length"))
	}
	e, err := startEngine([]byte(C.GoString(config)), C.GoBytes(unsafe.Pointer(key), keyLength))
	if err != nil {
		return fail(err)
	}
	id := nextID.Add(1)
	registry.Lock()
	registry.engines[id] = e
	registry.Unlock()
	return C.uint64_t(id)
}

//export H264EngineWaitReady
func H264EngineWaitReady(id C.uint64_t, timeoutMillis C.int, errorOut **C.char) C.int {
	if errorOut != nil {
		*errorOut = nil
	}
	e := lookup(id)
	fail := func(message string) C.int {
		if errorOut != nil {
			*errorOut = C.CString(message)
		}
		return -1
	}
	if e == nil {
		return fail("engine stopped")
	}
	wait := time.Duration(timeoutMillis) * time.Millisecond
	if wait <= 0 || wait > 30*time.Second {
		wait = 30 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-e.ready:
		if e.initErr != nil {
			return fail(e.initErr.Error())
		}
		if e.ctx.Err() != nil {
			return fail("engine stopped")
		}
		return 0
	case <-e.ctx.Done():
		return fail("engine startup canceled")
	case <-timer.C:
		return fail("engine startup timed out")
	}
}

//export H264EngineInject
func H264EngineInject(id C.uint64_t, data unsafe.Pointer, length C.int) C.int {
	e := lookup(id)
	if e == nil || data == nil || length < 1 || length > mtu {
		return -1
	}
	if err := e.inject(C.GoBytes(data, length)); err != nil {
		return -1
	}
	return 0
}

//export H264EngineRead
func H264EngineRead(id C.uint64_t, dst unsafe.Pointer, capacity C.int, timeoutMillis C.int) C.int {
	e := lookup(id)
	if e == nil || dst == nil || capacity < mtu {
		return -1
	}
	timeout := time.Duration(timeoutMillis) * time.Millisecond
	if timeout <= 0 || timeout > time.Second {
		timeout = time.Second
	}
	b := e.read(timeout)
	if len(b) == 0 {
		select {
		case <-e.ctx.Done():
			return -1
		default:
			return 0
		}
	}
	if len(b) > int(capacity) {
		return -2
	}
	copy(unsafe.Slice((*byte)(dst), int(capacity)), b)
	return C.int(len(b))
}

//export H264EngineStatus
func H264EngineStatus(id C.uint64_t) *C.char {
	e := lookup(id)
	if e == nil {
		return C.CString(`{"state":"stopped"}`)
	}
	return C.CString(string(e.status()))
}

//export H264EngineStop
func H264EngineStop(id C.uint64_t) {
	registry.Lock()
	e := registry.engines[uint64(id)]
	delete(registry.engines, uint64(id))
	registry.Unlock()
	if e != nil {
		e.close()
	}
}

//export H264EngineFree
func H264EngineFree(p unsafe.Pointer) { C.free(p) }
func main()                           {}
