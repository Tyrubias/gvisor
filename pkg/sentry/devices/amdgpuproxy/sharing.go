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
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// ipcHandle records an IPC object's size and its allocation references.
// Exporting the same allocation again does not create another reference.
type ipcHandle struct {
	size uint64
	refs uint64
}

func (p *proxy) trackIPCHandle(a *allocation, handle amdgpu.KFDIPCShareHandle) {
	if a.ipcHandle != nil {
		return
	}
	p.mu.Lock()
	h := p.ipcHandles[handle]
	h.size = a.size
	h.refs++
	p.ipcHandles[handle] = h
	p.mu.Unlock()
	a.ipcHandle = &handle
}

func (p *proxy) retainIPCHandle(handle amdgpu.KFDIPCShareHandle) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.ipcHandles[handle]
	if !ok {
		return 0, false
	}
	h.refs++
	p.ipcHandles[handle] = h
	return h.size, true
}

func (p *proxy) releaseIPCHandle(handle amdgpu.KFDIPCShareHandle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.ipcHandles[handle]
	if h.refs <= 1 {
		delete(p.ipcHandles, handle)
	} else {
		h.refs--
		p.ipcHandles[handle] = h
	}
}

func (p *proxy) releaseAllocation(a *allocation) {
	if a.userptr != nil {
		a.userptr.release()
	}
	if a.ipcHandle != nil {
		p.releaseIPCHandle(*a.ipcHandle)
	}
}

func (fd *deviceFD) exportDMABuf(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.AMDKFD_IOC_EXPORT_DMABUF
	var v amdgpu.KFDExportDMABuf
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if v.Flags & ^uint32(linux.O_ACCMODE|linux.O_CLOEXEC) != 0 {
		return linuxerr.EINVAL
	}
	if err := fd.call(t, cmd, &v); err != nil {
		return err
	}
	guestFD, err := fd.dev.proxy.newDMABufFD(t, int32(v.DMABufFD), v.Flags)
	if err != nil {
		return err
	}
	v.DMABufFD = uint32(guestFD)
	// KFD installs the descriptor before copying the arguments out. It
	// remains open if that copy fails.
	_, err = v.CopyOut(t, addr)
	return err
}

func (fd *deviceFD) importDMABuf(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.AMDKFD_IOC_IMPORT_DMABUF
	var v amdgpu.KFDImportDMABuf
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if !fd.dev.proxy.gpus[v.GPUID] {
		return linuxerr.EINVAL
	}
	f, hostFD, err := fd.dev.proxy.getDMABufFD(t, int32(v.DMABufFD))
	if err != nil {
		return err
	}
	defer f.DecRef(t)
	size, err := f.Seek(t, 0, linux.SEEK_END)
	if err != nil {
		return err
	}
	guestFD := v.DMABufFD
	v.DMABufFD = uint32(hostFD)
	c := fd.context
	c.mu.Lock()
	err = fd.call(t, cmd, &v)
	if err == nil {
		c.allocations[v.Handle] = &allocation{size: uint64(size)}
	}
	c.mu.Unlock()
	v.DMABufFD = guestFD
	return copyOut(t, cmd, addr, &v, err)
}

func (fd *deviceFD) getDMABufInfo(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.AMDKFD_IOC_GET_DMABUF_INFO
	var v amdgpu.KFDGetDMABufInfo
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if v.MetadataSize > maxDRMBuffer {
		return linuxerr.EINVAL
	}
	f, hostFD, err := fd.dev.proxy.getDMABufFD(t, int32(v.DMABufFD))
	if err != nil {
		return err
	}
	defer f.DecRef(t)
	guestFD, ptr := v.DMABufFD, v.MetadataPtr
	var metadata []byte
	if ptr != 0 {
		metadata = make([]byte, v.MetadataSize)
	}
	v.DMABufFD = uint32(hostFD)
	v.MetadataPtr = hostBufferPointer(metadata)
	err = fd.call(t, cmd, &v, platform.HostIoctlBuffer{PointerOffset: 8, Data: metadata})
	v.DMABufFD, v.MetadataPtr = guestFD, ptr
	if err == nil && len(metadata) != 0 {
		if _, err := t.CopyOutBytes(hostarch.Addr(ptr), metadata[:min(uint32(len(metadata)), v.MetadataSize)]); err != nil {
			return err
		}
	}
	return copyOut(t, cmd, addr, &v, err)
}

func (fd *deviceFD) exportIPC(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.AMDKFD_IOC_IPC_EXPORT_HANDLE
	var v amdgpu.KFDIPCExportHandle
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if !fd.dev.proxy.gpus[v.GPUID] {
		return linuxerr.EINVAL
	}
	c := fd.context
	c.mu.Lock()
	a := c.allocations[v.Handle]
	if a == nil {
		c.mu.Unlock()
		return linuxerr.EINVAL
	}
	err := fd.call(t, cmd, &v)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	p := fd.dev.proxy
	p.trackIPCHandle(a, v.ShareHandle)
	c.mu.Unlock()
	return copyOut(t, cmd, addr, &v, nil)
}

func (fd *deviceFD) importIPC(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.AMDKFD_IOC_IPC_IMPORT_HANDLE
	var v amdgpu.KFDIPCImportHandle
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	p := fd.dev.proxy
	if !p.gpus[v.GPUID] {
		return linuxerr.EINVAL
	}
	// Keep authorization alive across the host import, including when the
	// last allocation reference is freed concurrently.
	handle := v.ShareHandle
	size, ok := p.retainIPCHandle(handle)
	if !ok {
		return linuxerr.EINVAL
	}
	c := fd.context
	c.mu.Lock()
	err := fd.call(t, cmd, &v)
	if err == nil {
		c.allocations[v.Handle] = &allocation{size: size, ipcHandle: &handle}
	}
	c.mu.Unlock()
	if err != nil {
		p.releaseIPCHandle(handle)
	}
	return copyOut(t, cmd, addr, &v, err)
}
