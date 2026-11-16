// Copyright 2021 The gVisor Authors.
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

package systrap

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/hostsyscall"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// IoctlFlat implements platform.HostMMIO.
func (s *subprocess) IoctlFlat(hostFD int32, request uint32, buf []byte) (platform.HostIoctlResult, error) {
	return s.IoctlHostDeviceFlat(hostFD, request, buf)
}

// IoctlHostDeviceFlat implements platform.HostDeviceIO.
func (s *subprocess) IoctlHostDeviceFlat(hostFD int32, request uint32, buf []byte, nested ...platform.HostIoctlBuffer) (platform.HostIoctlResult, error) {
	if hostFD < 0 {
		return platform.HostIoctlResult{}, unix.EBADF
	}
	paramsSize := int(hostarch.PageSize) - int(unsafe.Sizeof(syscallStubMessage{}))
	if len(buf) > paramsSize {
		return platform.HostIoctlResult{}, unix.E2BIG
	}
	size := (len(buf) + 7) &^ 7
	for _, b := range nested {
		if b.PointerOffset < 0 || b.PointerOffset > len(buf)-8 {
			return platform.HostIoctlResult{}, unix.EINVAL
		}
		if len(b.Data) > paramsSize-size {
			return platform.HostIoctlResult{}, unix.E2BIG
		}
		size += (len(b.Data) + 7) &^ 7
	}
	ptrs := make([]uint64, len(nested))
	var result platform.HostIoctlResult
	err := s.withAliveRLock(func() error {
		s.syscallThreadMu.Lock()
		defer s.syscallThreadMu.Unlock()
		if s.hostMMUnsafe {
			return unix.EIO
		}
		// Serialize against failed thread creation before the driver can
		// retain this mm. It must never contain aliases of recycled backing
		// or become another application's address space.
		s.hostMMBound.Store(true)

		t := s.syscallThread
		paramsAddr := t.stubAddr + syscallStubMessageOffset + unsafe.Sizeof(syscallStubMessage{})
		// The rest of the reply page contains application-controlled data.
		// Translated host FDs and Sentry pointers must never be stored here.
		params := unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(t.stubMessage), unsafe.Sizeof(*t.stubMessage))), paramsSize)
		offset := (len(buf) + 7) &^ 7
		for i, b := range nested {
			ptrs[i] = hostarch.ByteOrder.Uint64(buf[b.PointerOffset:])
			var ptr uint64
			if len(b.Data) != 0 {
				ptr = uint64(paramsAddr) + uint64(offset)
				copy(params[offset:], b.Data)
			}
			// Relocate before publishing buf: its original pointer may name
			// Sentry memory, which must not be exposed to the application.
			hostarch.ByteOrder.PutUint64(buf[b.PointerOffset:], ptr)
			offset += (len(b.Data) + 7) &^ 7
		}
		copy(params, buf)
		// CLONE_FILES shares hostFD with the stub. The syscall request page is
		// read-only there, protecting the FD, command, and parameter address.
		ret, err := t.syscall(unix.SYS_IOCTL,
			arch.SyscallArgument{Value: uintptr(hostFD)},
			arch.SyscallArgument{Value: uintptr(request)},
			arch.SyscallArgument{Value: paramsAddr})
		copy(buf, params)
		offset = (len(buf) + 7) &^ 7
		for i, b := range nested {
			hostarch.ByteOrder.PutUint64(buf[b.PointerOffset:], ptrs[i])
			copy(b.Data, params[offset:offset+len(b.Data)])
			offset += (len(b.Data) + 7) &^ 7
		}
		// The scratch page and syscall return are guest-writable. Keep the
		// untrusted errno separate from transport and lifecycle failures.
		result.Return = ret
		if errno, ok := err.(unix.Errno); ok {
			result.Errno = errno
			return nil
		}
		return err
	})
	return result, err
}

func (t *syscallThread) initRequestReplyAddresses(sentryStackAddr uintptr) {
	// These are safe as these addresses are mmapped and never moved/gced.
	sentryMessage := (*syscallSentryMessage)(unsafe.Pointer(sentryStackAddr))
	stubMessage := (*syscallStubMessage)(unsafe.Pointer(sentryStackAddr + syscallStubMessageOffset))
	atomic.StoreUint32(&sentryMessage.state, 0)

	t.sentryMessage = sentryMessage
	t.stubMessage = stubMessage
}

// maskAllSignals blocks all signals.
func (t *syscallThread) maskAllSignalsAttached() {
	p := t.thread

	mask := ^uint64(0)
	if errno := hostsyscall.RawSyscallErrno6(unix.SYS_PTRACE, linux.PTRACE_SETSIGMASK, uintptr(p.tid), 8, uintptr(unsafe.Pointer(&mask)), 0, 0); errno != 0 {
		panic(fmt.Sprintf("unable to setmask: %v", errno))
	}
}

// unmaskAllSignals unblocks all signals.
func (t *syscallThread) unmaskAllSignalsAttached() error {
	p := t.thread
	mask := uint64(0)
	if errno := hostsyscall.RawSyscallErrno6(unix.SYS_PTRACE, linux.PTRACE_SETSIGMASK, uintptr(p.tid), 8, uintptr(unsafe.Pointer(&mask)), 0, 0); errno != 0 {
		return fmt.Errorf("unable to setmask: %w", errno)
	}
	return nil
}

func futexWakeUint32(addr *uint32) error {
	if e := hostsyscall.RawSyscallErrno(unix.SYS_FUTEX, uintptr(unsafe.Pointer(addr)), linux.FUTEX_WAKE, 1); e != 0 {
		return fmt.Errorf("failed to FUTEX_WAKE: %v", e)
	}
	return nil
}

func futexWaitForUint32(addr *uint32, targetValue uint32) error {
	for {
		val := atomic.LoadUint32(addr)
		if val == targetValue {
			break
		}

		_, _, e := unix.Syscall6(unix.SYS_FUTEX, uintptr(unsafe.Pointer(addr)), linux.FUTEX_WAIT, uintptr(val), 0, 0, 0)
		if e != 0 && e != unix.EAGAIN && e != unix.EINTR {
			return fmt.Errorf("failed to FUTEX_WAIT: %v", e)
		}
	}
	return nil
}

// futexWaitWake waits when other side will call FUTEX_WAKE. A value of the
// futex word has to be equal to futexValue and it must not be changed.
func futexWaitWake(futexAddr *uint32, futexValue uint32) error {
	for {
		_, _, e := unix.Syscall6(unix.SYS_FUTEX, uintptr(unsafe.Pointer(futexAddr)), linux.FUTEX_WAIT, uintptr(futexValue), 0, 0, 0)
		if e == 0 {
			break
		}
		if e != unix.EAGAIN && e != unix.EINTR {
			return fmt.Errorf("failed to FUTEX_WAIT: %v", e)
		}
	}

	return nil
}

func (t *syscallThread) kickSeccompNotify() unix.Errno {
	errno := hostsyscall.RawSyscallErrno(unix.SYS_IOCTL, uintptr(t.seccompNotify.Fd()),
		uintptr(linux.SECCOMP_IOCTL_NOTIF_SEND),
		uintptr(unsafe.Pointer(&t.seccompNotifyResp)))
	return errno
}

func (t *syscallThread) waitForSeccompNotify() error {
	for {
		req := linux.SeccompNotif{}
		errno := hostsyscall.RawSyscallErrno(unix.SYS_IOCTL, uintptr(t.seccompNotify.Fd()),
			uintptr(linux.SECCOMP_IOCTL_NOTIF_RECV),
			uintptr(unsafe.Pointer(&req)))
		if errno == 0 {
			t.seccompNotifyResp.ID = req.ID
			break
		}
		// ENOENT: the stub thread's notifying syscall was interrupted between
		// the wakeup and the lookup (by the cgroup freezer, say, which kicks
		// every task out of its waits). The syscall restarts and the kernel
		// issues a new notification for it, as notifySeccompThread already
		// expects for SEND: wait for that one.
		if (errno == unix.EINTR || errno == unix.ENOENT) && t.subproc.alive() {
			continue
		}
		t.kill()
		return fmt.Errorf("failed getting response from syscall thread : %w", errno)
	}
	return nil
}
