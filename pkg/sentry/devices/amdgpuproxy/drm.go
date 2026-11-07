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
	"time"

	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// maxDRMBuffer bounds the nested buffers of DRM queries.
const maxDRMBuffer = 4096

// maxDRMHandles bounds arrays of per-file DRM handles and timeline points.
const maxDRMHandles = 4096

func (fd *deviceFD) drmIoctl(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	switch cmd {
	case amdgpu.DRM_IOCTL_VERSION:
		return fd.drmVersion(t, addr)
	case amdgpu.DRM_IOCTL_GEM_CLOSE,
		amdgpu.DRM_IOCTL_SYNCOBJ_CREATE,
		amdgpu.DRM_IOCTL_SYNCOBJ_DESTROY,
		amdgpu.DRM_IOCTL_AMDGPU_GEM_MMAP,
		amdgpu.DRM_IOCTL_AMDGPU_GEM_METADATA,
		amdgpu.DRM_IOCTL_AMDGPU_GEM_VA:
		// These argument versions contain per-file handles and scalars.
		return fd.passthrough(t, cmd, addr)
	case amdgpu.DRM_IOCTL_SYNCOBJ_TIMELINE_WAIT, amdgpu.DRM_IOCTL_SYNCOBJ_TIMELINE_WAIT_V1:
		return fd.timelineWait(t, cmd, addr)
	case amdgpu.DRM_IOCTL_AMDGPU_GEM_VA_EXT, amdgpu.DRM_IOCTL_AMDGPU_GEM_VA_EXT_RW:
		return fd.gemVA(t, cmd, addr)
	case amdgpu.DRM_IOCTL_PRIME_HANDLE_TO_FD:
		return fd.primeHandleToFD(t, addr)
	case amdgpu.DRM_IOCTL_PRIME_FD_TO_HANDLE:
		return fd.primeFDToHandle(t, addr)
	case amdgpu.DRM_IOCTL_AMDGPU_GEM_OP:
		return fd.gemOp(t, addr)
	case amdgpu.DRM_IOCTL_AMDGPU_INFO:
		return fd.drmInfo(t, addr)
	default:
		return linuxerr.ENOTTY
	}
}

func (fd *deviceFD) drmVersion(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.DRMVersion
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	fields := []struct{ ptr, length *uint64 }{{&v.Name, &v.NameLen}, {&v.Date, &v.DateLen}, {&v.Desc, &v.DescLen}}
	var ptrs [3]uint64
	var bufs [3][]byte
	for i, f := range fields {
		if *f.length > maxDRMBuffer {
			return linuxerr.EINVAL
		}
		// drm_copy_field only reports a field's length to a NULL buffer.
		if ptrs[i] = *f.ptr; ptrs[i] != 0 {
			bufs[i] = make([]byte, *f.length)
		}
		*f.ptr = hostBufferPointer(bufs[i])
	}
	err := fd.call(t, amdgpu.DRM_IOCTL_VERSION, &v, platform.HostIoctlBuffer{PointerOffset: 24, Data: bufs[0]}, platform.HostIoctlBuffer{PointerOffset: 40, Data: bufs[1]}, platform.HostIoctlBuffer{PointerOffset: 56, Data: bufs[2]})
	for i, f := range fields {
		*f.ptr = ptrs[i]
		if err == nil && len(bufs[i]) != 0 {
			if _, err = t.CopyOutBytes(hostarch.Addr(ptrs[i]), bufs[i][:min(uint64(len(bufs[i])), *f.length)]); err != nil {
				return err
			}
		}
	}
	return copyOut(t, amdgpu.DRM_IOCTL_VERSION, addr, &v, err)
}

func (fd *deviceFD) drmInfo(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.DRMAMDGPUInfo
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if v.ReturnSize > maxDRMBuffer {
		return linuxerr.EINVAL
	}
	// amdgpu_info_ioctl writes min(return_size, sizeof(result)) bytes without
	// reporting how many, so start from the application's buffer contents.
	out := make([]byte, v.ReturnSize)
	ptr := hostarch.Addr(v.ReturnPointer)
	if _, err := t.CopyInBytes(ptr, out); err != nil {
		return err
	}
	v.ReturnPointer = hostBufferPointer(out)
	if err := fd.call(t, amdgpu.DRM_IOCTL_AMDGPU_INFO, &v, platform.HostIoctlBuffer{PointerOffset: 0, Data: out}); err != nil {
		return err
	}
	_, err := t.CopyOutBytes(ptr, out)
	return err
}

func (fd *deviceFD) primeHandleToFD(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.DRM_IOCTL_PRIME_HANDLE_TO_FD
	var v amdgpu.DRMPrimeHandle
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if v.Flags & ^uint32(linux.O_CLOEXEC|linux.O_RDWR) != 0 {
		return linuxerr.EINVAL
	}
	if err := fd.call(t, cmd, &v); err != nil {
		return err
	}
	guestFD, err := fd.dev.proxy.newDMABufFD(t, v.FD, v.Flags)
	if err != nil {
		return err
	}
	v.FD = guestFD
	if _, err := v.CopyOut(t, addr); err != nil {
		removeFD(t, guestFD)
		return err
	}
	return nil
}

func (fd *deviceFD) primeFDToHandle(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.DRM_IOCTL_PRIME_FD_TO_HANDLE
	var v amdgpu.DRMPrimeHandle
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	f, hostFD, err := fd.dev.proxy.getDMABufFD(t, v.FD)
	if err != nil {
		return err
	}
	defer f.DecRef(t)
	guestFD := v.FD
	v.FD = hostFD
	err = fd.call(t, cmd, &v)
	v.FD = guestFD
	return copyOut(t, cmd, addr, &v, err)
}

func (fd *deviceFD) gemOp(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.DRM_IOCTL_AMDGPU_GEM_OP
	var v amdgpu.DRMGEMOp
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	switch v.Op {
	case amdgpu.AMDGPU_GEM_OP_GET_GEM_CREATE_INFO:
		// The output is union drm_amdgpu_gem_create: four 64-bit scalars.
		out := make([]byte, 32)
		ptr := v.Value
		v.Value = hostBufferPointer(out)
		if err := fd.call(t, cmd, &v, platform.HostIoctlBuffer{PointerOffset: 8, Data: out}); err != nil {
			return err
		}
		v.Value = ptr
		if _, err := t.CopyOutBytes(hostarch.Addr(ptr), out); err != nil {
			return err
		}
		return copyOut(t, cmd, addr, &v, nil)
	case amdgpu.AMDGPU_GEM_OP_SET_PLACEMENT:
		return copyOut(t, cmd, addr, &v, fd.call(t, cmd, &v))
	default:
		return linuxerr.EINVAL
	}
}

func (fd *deviceFD) gemVA(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var v amdgpu.DRMGEMVAExt
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if v.NumSyncobjHandles > maxDRMHandles {
		return linuxerr.EINVAL
	}
	handles := make([]byte, int(v.NumSyncobjHandles)*4)
	ptr := v.InputFenceSyncobjHandles
	if _, err := t.CopyInBytes(hostarch.Addr(ptr), handles); err != nil {
		return err
	}
	v.InputFenceSyncobjHandles = hostBufferPointer(handles)
	err := fd.call(t, cmd, &v, platform.HostIoctlBuffer{PointerOffset: 56, Data: handles})
	v.InputFenceSyncobjHandles = ptr
	return copyOut(t, cmd, addr, &v, err)
}

func (fd *deviceFD) timelineWait(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var v amdgpu.DRMSyncobjTimelineWait
	buf := make([]byte, v.SizeBytes())
	size := linux.IOC_SIZE(cmd)
	if _, err := t.CopyInBytes(addr, buf[:size]); err != nil {
		return err
	}
	v.UnmarshalBytes(buf)
	if v.CountHandles > maxDRMHandles {
		return linuxerr.EINVAL
	}
	handles, points := make([]byte, int(v.CountHandles)*4), make([]byte, int(v.CountHandles)*8)
	hptr, pptr, timeout := v.Handles, v.Points, v.TimeoutNsec
	if _, err := t.CopyInBytes(hostarch.Addr(hptr), handles); err != nil {
		return err
	}
	if _, err := t.CopyInBytes(hostarch.Addr(pptr), points); err != nil {
		return err
	}
	v.Handles, v.Points, v.TimeoutNsec = hostBufferPointer(handles), hostBufferPointer(points), 0
	var err error
	for {
		err = fd.call(t, cmd, &v, platform.HostIoctlBuffer{PointerOffset: 0, Data: handles}, platform.HostIoctlBuffer{PointerOffset: 8, Data: points})
		if !linuxerr.Equals(linuxerr.ETIME, err) {
			break
		}
		now := t.Kernel().MonotonicClock().Now().Nanoseconds()
		// The timeout is an absolute deadline, including negative values.
		// Compare before subtracting to avoid overflow for expired deadlines.
		if timeout <= now {
			break
		}
		pause := min(100*time.Microsecond, time.Duration(timeout-now))
		if _, blocked := t.BlockWithTimeout(nil, true, pause); blocked != nil && !linuxerr.Equals(linuxerr.ETIMEDOUT, blocked) {
			err = linuxerr.ERESTARTSYS
			break
		}
	}
	v.Handles, v.Points, v.TimeoutNsec = hptr, pptr, timeout
	v.MarshalBytes(buf)
	if _, outErr := t.CopyOutBytes(addr, buf[:size]); outErr != nil {
		return outErr
	}
	return err
}
