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
	"unsafe"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

func hostBufferPointer(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&b[0])))
}

func (fd *deviceFD) hostIoctl(t *kernel.Task, cmd uint32, arg []byte, nested ...platform.HostIoctlBuffer) error {
	io, ok := t.MemoryManager().AddressSpace().(platform.HostDeviceIO)
	if !ok {
		return linuxerr.EOPNOTSUPP
	}
	_, err := io.IoctlHostDevice(fd.hostFD, cmd, arg, nested...)
	return err
}

type userMemory struct {
	pinned   []mm.PinnedRange
	hostAddr uintptr
}

func pinUserMemory(t *kernel.Task, addr, size uint64) (*userMemory, error) {
	ar, ok := hostarch.Addr(addr).ToRange(size)
	if !ok || size == 0 || !ar.IsPageAligned() {
		return nil, linuxerr.EINVAL
	}
	pinned, err := t.MemoryManager().Pin(t, ar, hostarch.ReadWrite, false)
	if err != nil {
		mm.Unpin(pinned)
		return nil, err
	}
	if err := t.MemoryManager().EnsureAddressSpaceMappings(t, ar); err != nil {
		mm.Unpin(pinned)
		return nil, err
	}
	return &userMemory{pinned: pinned, hostAddr: uintptr(addr)}, nil
}

func (um *userMemory) release() {
	mm.Unpin(um.pinned)
}
