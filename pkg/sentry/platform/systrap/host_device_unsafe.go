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

package systrap

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/hostsyscall"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

const hostDeviceScratchSize = 1 << 20

type hostDeviceRequest struct {
	path    string
	flags   uint32
	hostFD  int32
	request uint32
	buf     []byte
	nested  []platform.HostIoctlBuffer
	done    chan hostDeviceReply
}

type hostDeviceReply struct {
	ret uintptr
	err error
}

// OpenHostDevice implements platform.HostDeviceIO.
func (s *subprocess) OpenHostDevice(path string, flags uint32) (int32, error) {
	if !strings.HasPrefix(path, "/dev/") || strings.IndexByte(path, 0) >= 0 || len(path) >= hostDeviceScratchSize {
		return -1, unix.EINVAL
	}
	r := s.hostDeviceRequest(&hostDeviceRequest{path: path, flags: flags})
	if r.err != nil {
		return -1, r.err
	}
	return int32(r.ret), nil
}

// IoctlHostDevice implements platform.HostDeviceIO.
func (s *subprocess) IoctlHostDevice(hostFD int32, request uint32, buf []byte, nested ...platform.HostIoctlBuffer) (uintptr, error) {
	if hostFD < 0 {
		return 0, unix.EBADF
	}
	if len(buf) > hostDeviceScratchSize {
		return 0, unix.E2BIG
	}
	size := (len(buf) + 7) &^ 7
	for _, b := range nested {
		if b.PointerOffset < 0 || b.PointerOffset > len(buf)-8 {
			return 0, unix.EINVAL
		}
		if len(b.Data) > hostDeviceScratchSize-size {
			return 0, unix.E2BIG
		}
		size += (len(b.Data) + 7) &^ 7
	}
	r := s.hostDeviceRequest(&hostDeviceRequest{hostFD: hostFD, request: request, buf: buf, nested: nested})
	return r.ret, r.err
}

// OnHostDeviceRelease implements platform.HostDeviceIO.
func (s *subprocess) OnHostDeviceRelease(cleanup func()) error {
	return s.withAliveRLock(func() error {
		s.syscallThreadMu.Lock()
		defer s.syscallThreadMu.Unlock()
		s.hostMMBound.Store(true)
		s.hostDeviceCleanups = append(s.hostDeviceCleanups, cleanup)
		return nil
	})
}

func (s *subprocess) hostDeviceRequest(r *hostDeviceRequest) hostDeviceReply {
	r.done = make(chan hostDeviceReply, 1)
	var reply hostDeviceReply
	err := s.withAliveRLock(func() error {
		// Include the complete sysmsg thread initialization in this exclusion:
		// a child not yet in sysmsgThreads can already share this mm.
		s.hostDeviceMu.Lock()
		defer s.hostDeviceMu.Unlock()
		s.requests <- r
		reply = <-r.done
		return nil
	})
	if err != nil {
		return hostDeviceReply{err: err}
	}
	return reply
}

// executeHostDeviceRequest runs on the ptrace request goroutine with the
// canonical syscall thread attached and syscallThreadMu held.
//
// +checklocks:s.syscallThreadMu
func (s *subprocess) executeHostDeviceRequest(r *hostDeviceRequest) hostDeviceReply {
	if s.hostMMUnsafe {
		return hostDeviceReply{err: unix.EIO}
	}
	// Even an operation reporting failure can leave a reference to this mm.
	s.hostMMBound.Store(true)

	type stoppedThread struct {
		t   *thread
		sig uintptr
	}
	stoppedThreads := make([]stoppedThread, 0, len(s.sysmsgThreads))
	defer func() {
		for _, st := range stoppedThreads {
			if errno := hostsyscall.RawSyscallErrno6(unix.SYS_PTRACE, unix.PTRACE_DETACH, uintptr(st.t.tid), 0, st.sig, 0, 0); errno != 0 {
				panic(fmt.Sprintf("cannot resume device address space thread %d: %v", st.t.tid, errno))
			}
		}
	}()
	for _, p := range s.sysmsgThreads {
		t := p.thread
		// SEIZE/INTERRUPT preserves signal and group-stop state without
		// injecting SIGSTOP into application execution.
		if errno := hostsyscall.RawSyscallErrno6(unix.SYS_PTRACE, unix.PTRACE_SEIZE, uintptr(t.tid), 0, unix.PTRACE_O_EXITKILL, 0, 0); errno != 0 {
			panic(fmt.Sprintf("cannot seize device address space thread %d: %v", t.tid, errno))
		}
		if errno := hostsyscall.RawSyscallErrno6(unix.SYS_PTRACE, unix.PTRACE_INTERRUPT, uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
			panic(fmt.Sprintf("cannot interrupt device address space thread %d: %v", t.tid, errno))
		}
		sig, err := t.wait(stopped)
		if err != nil {
			panic(fmt.Sprintf("cannot stop device address space thread %d: %v", t.tid, err))
		}
		var deliver uintptr
		if sig < 256 && sig != unix.SIGSTOP {
			deliver = uintptr(sig)
		}
		stoppedThreads = append(stoppedThreads, stoppedThread{t: t, sig: deliver})
	}
	if err := s.initHostDeviceScratch(); err != nil {
		return hostDeviceReply{err: err}
	}
	scratch := s.hostDeviceScratch
	used := len(r.path) + 1
	if r.path == "" {
		used = (len(r.buf) + 7) &^ 7
		for _, b := range r.nested {
			used += (len(b.Data) + 7) &^ 7
		}
	}
	defer clear(scratch[:used])
	t := s.syscallThread.thread
	if r.path != "" {
		copy(scratch, r.path)
		dirFD := int32(unix.AT_FDCWD)
		ret, err := t.syscallIgnoreInterrupt(&t.initRegs, unix.SYS_OPENAT,
			arch.SyscallArgument{Value: uintptr(dirFD)},
			arch.SyscallArgument{Value: stubHostDeviceScratch},
			arch.SyscallArgument{Value: uintptr(r.flags)})
		return hostDeviceReply{ret: ret, err: err}
	}
	copy(scratch, r.buf)
	offset := (len(r.buf) + 7) &^ 7
	for _, b := range r.nested {
		var ptr uint64
		if len(b.Data) != 0 {
			copy(scratch[offset:], b.Data)
			ptr = uint64(stubHostDeviceScratch + uintptr(offset))
		}
		hostarch.ByteOrder.PutUint64(scratch[b.PointerOffset:], ptr)
		offset += (len(b.Data) + 7) &^ 7
	}
	ret, err := t.syscallIgnoreInterrupt(&t.initRegs, unix.SYS_IOCTL,
		arch.SyscallArgument{Value: uintptr(r.hostFD)},
		arch.SyscallArgument{Value: uintptr(r.request)},
		arch.SyscallArgument{Value: stubHostDeviceScratch})
	// Restore the Sentry-side pointer fields before copying the argument back.
	// Nested buffers and status are captured while all application threads
	// remain stopped, including when the driver reports an error.
	for _, b := range r.nested {
		copy(scratch[b.PointerOffset:b.PointerOffset+8], r.buf[b.PointerOffset:b.PointerOffset+8])
	}
	copy(r.buf, scratch)
	offset = (len(r.buf) + 7) &^ 7
	for _, b := range r.nested {
		copy(b.Data, scratch[offset:offset+len(b.Data)])
		offset += (len(b.Data) + 7) &^ 7
	}
	return hostDeviceReply{ret: ret, err: err}
}

// +checklocks:s.syscallThreadMu
func (s *subprocess) initHostDeviceScratch() error {
	if len(s.hostDeviceScratch) != 0 {
		return nil
	}
	fr, err := s.memoryFile.Allocate(hostDeviceScratchSize, pgalloc.AllocOpts{Kind: usage.System, Dir: pgalloc.TopDown})
	if err != nil {
		return err
	}
	scratch, err := unix.Mmap(s.memoryFile.FD(), int64(fr.Start), hostDeviceScratchSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		s.memoryFile.DecRef(fr)
		return err
	}
	t := s.syscallThread.thread
	if _, err := t.syscallIgnoreInterrupt(&t.initRegs, unix.SYS_MMAP,
		arch.SyscallArgument{Value: stubHostDeviceScratch},
		arch.SyscallArgument{Value: hostDeviceScratchSize},
		arch.SyscallArgument{Value: unix.PROT_READ | unix.PROT_WRITE},
		arch.SyscallArgument{Value: unix.MAP_SHARED | unix.MAP_FIXED},
		arch.SyscallArgument{Value: uintptr(s.memoryFile.FD())},
		arch.SyscallArgument{Value: uintptr(fr.Start)}); err != nil {
		_ = unix.Munmap(scratch)
		s.memoryFile.DecRef(fr)
		return err
	}
	s.hostDeviceScratch = scratch
	s.hostDeviceScratchRange = fr
	return nil
}

func (s *subprocess) releaseHostDeviceScratch() {
	s.syscallThreadMu.Lock()
	defer s.syscallThreadMu.Unlock()
	for _, cleanup := range s.hostDeviceCleanups {
		cleanup()
	}
	s.hostDeviceCleanups = nil
	if len(s.hostDeviceScratch) == 0 {
		return
	}
	if err := unix.Munmap(s.hostDeviceScratch); err != nil {
		panic(fmt.Sprintf("cannot unmap host device scratch: %v", err))
	}
	s.hostDeviceScratch = nil
	s.memoryFile.DecRef(s.hostDeviceScratchRange)
	s.hostDeviceScratchRange = memmap.FileRange{}
}
