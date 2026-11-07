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
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
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
	// PRIME imports can check current->mm when opening a USERPTR GEM object.
	// Keep descriptor transfer in the application mm. Other admitted DRM
	// ioctls use per-file state and Sentry-owned argument buffers.
	if !fd.dev.kfd && cmd != amdgpu.DRM_IOCTL_PRIME_FD_TO_HANDLE && cmd != amdgpu.DRM_IOCTL_PRIME_HANDLE_TO_FD {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd.hostFD), uintptr(cmd), uintptr(unsafe.Pointer(unsafe.SliceData(arg))))
		runtime.KeepAlive(nested)
		if errno != 0 {
			return errno
		}
		return nil
	}
	io, ok := t.MemoryManager().AddressSpace().(platform.HostDeviceIO)
	if !ok {
		return linuxerr.EOPNOTSUPP
	}
	_, err := io.IoctlHostDevice(fd.hostFD, cmd, arg, nested...)
	return err
}

// hostIoctlFlat forwards arguments whose contents and result need not be
// protected from application writes. It must not receive translated host FDs.
func (fd *deviceFD) hostIoctlFlat(t *kernel.Task, cmd uint32, arg []byte, nested ...platform.HostIoctlBuffer) error {
	io, ok := t.MemoryManager().AddressSpace().(platform.HostDeviceIO)
	if !ok {
		return linuxerr.EOPNOTSUPP
	}
	result, err := io.IoctlHostDeviceFlat(fd.hostFD, cmd, arg, nested...)
	if err == unix.E2BIG {
		// Large query arrays can exceed the flat transport's capacity.
		return fd.hostIoctl(t, cmd, arg, nested...)
	}
	if err != nil {
		return err
	}
	if result.Errno != 0 {
		return result.Errno
	}
	return nil
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
