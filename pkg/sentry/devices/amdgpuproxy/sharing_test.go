// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package amdgpuproxy

import (
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

type queueDeviceIO struct {
	platform.AddressSpace
	platform.HostDeviceIO
	results map[uint32]error
	calls   int
}

func (io *queueDeviceIO) IoctlHostDevice(_ int32, _ uint32, buf []byte, _ ...platform.HostIoctlBuffer) (uintptr, error) {
	var v amdgpu.KFDDestroyQueue
	v.UnmarshalBytes(buf)
	io.calls++
	return 0, io.results[v.QueueID]
}

func TestQueueQuiesce(t *testing.T) {
	io := &queueDeviceIO{results: map[uint32]error{2: unix.EINVAL, 3: linuxerr.EINVAL, 4: unix.EIO}}
	c := &deviceContext{as: io, queues: map[uint32]struct{}{1: {}, 2: {}, 3: {}, 4: {}}}
	if err := c.quiesce(); err == nil {
		t.Fatal("quiesce succeeded despite EIO")
	}
	if io.calls != 4 {
		t.Fatalf("attempted %d queue destroys, want 4", io.calls)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, failed := c.queues[4]; len(c.queues) != 1 || !failed {
		t.Fatalf("retained queue IDs %v, want only 4", c.queues)
	}
}

func TestIPCHandleReferences(t *testing.T) {
	p := &proxy{ipcHandles: make(map[amdgpu.KFDIPCShareHandle]ipcHandle)}
	count := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.ipcHandles)
	}
	for i := uint32(0); i < 32; i++ {
		handle := amdgpu.KFDIPCShareHandle{i, 1, 2, 3}
		original := &allocation{size: 4096}
		p.trackIPCHandle(original, handle)
		p.trackIPCHandle(original, handle)
		size, ok := p.retainIPCHandle(handle)
		if !ok || size != original.size {
			t.Fatalf("retainIPCHandle() = %d, %t", size, ok)
		}
		imported := &allocation{size: size, ipcHandle: &handle}
		if i%2 == 0 {
			c := &deviceContext{proxy: p, allocations: map[uint64]*allocation{1: original}, kfd: -1}
			c.release()
		} else {
			p.releaseAllocation(original)
		}
		if got := count(); got != 1 {
			t.Fatalf("imported allocation lost authorization: %d handles", got)
		}
		p.trackIPCHandle(imported, handle)
		if _, ok := p.retainIPCHandle(handle); !ok {
			t.Fatal("live imported handle was rejected")
		}
		p.releaseAllocation(imported)
		if got := count(); got != 1 {
			t.Fatalf("pending import lost authorization: %d handles", got)
		}
		// A failed import drops its temporary reference.
		p.releaseIPCHandle(handle)
		if got := count(); got != 0 {
			t.Fatalf("last release retained %d handles", got)
		}
		if _, ok := p.retainIPCHandle(handle); ok {
			t.Fatal("expired handle retained authorization")
		}
	}
}
