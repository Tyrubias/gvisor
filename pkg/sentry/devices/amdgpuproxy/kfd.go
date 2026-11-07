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
	"math/bits"
	"time"

	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// maxGPUs bounds per-GPU arrays copied for an ioctl.
const maxGPUs = 256

type ioctlArgs interface {
	marshal.Marshallable
	CopyIn(marshal.CopyContext, hostarch.Addr) (int, error)
	CopyOut(marshal.CopyContext, hostarch.Addr) (int, error)
}

// call forwards cmd with argument v, updating v with the driver's output.
func (fd *deviceFD) call(t *kernel.Task, cmd uint32, v marshal.Marshallable, nested ...platform.HostIoctlBuffer) error {
	b := make([]byte, v.SizeBytes())
	v.MarshalBytes(b)
	err := fd.hostIoctl(t, cmd, b, nested...)
	v.UnmarshalBytes(b)
	return err
}

// copyOut writes v back to the application if cmd has output and returns
// err, the driver's result. Like kfd_ioctl, output is copied even for a
// failed request.
func copyOut(t *kernel.Task, cmd uint32, addr hostarch.Addr, v ioctlArgs, err error) error {
	if cmd&(linux.IOC_READ<<linux.IOC_DIRSHIFT) != 0 {
		if _, cerr := v.CopyOut(t, addr); cerr != nil {
			return cerr
		}
	}
	return err
}

// gpuIoctl forwards an ioctl whose argument v names the GPU *gpuID.
func (fd *deviceFD) gpuIoctl(t *kernel.Task, cmd uint32, addr hostarch.Addr, v ioctlArgs, gpuID *uint32) error {
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if !fd.dev.proxy.gpus[*gpuID] {
		return linuxerr.EINVAL
	}
	return copyOut(t, cmd, addr, v, fd.call(t, cmd, v))
}

// passthrough forwards an ioctl whose argument needs no translation.
func (fd *deviceFD) passthrough(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	b := make([]byte, linux.IOC_SIZE(cmd))
	if cmd&(linux.IOC_WRITE<<linux.IOC_DIRSHIFT) != 0 {
		if _, err := t.CopyInBytes(addr, b); err != nil {
			return err
		}
	}
	err := fd.hostIoctl(t, cmd, b)
	if cmd&(linux.IOC_READ<<linux.IOC_DIRSHIFT) != 0 {
		if _, cerr := t.CopyOutBytes(addr, b); cerr != nil {
			return cerr
		}
	}
	return err
}

func (fd *deviceFD) kfdIoctl(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	switch cmd {
	case amdgpu.AMDKFD_IOC_GET_VERSION,
		amdgpu.AMDKFD_IOC_DESTROY_EVENT,
		amdgpu.AMDKFD_IOC_SET_EVENT,
		amdgpu.AMDKFD_IOC_RUNTIME_ENABLE:
		// These arguments hold handles and scalars only. RUNTIME_ENABLE's
		// r_debug is a cookie kept for debuggers; the driver never
		// dereferences it.
		return fd.passthrough(t, cmd, addr)
	case amdgpu.AMDKFD_IOC_GET_CLOCK_COUNTERS:
		var v amdgpu.KFDClockCounters
		if _, err := v.CopyIn(t, addr); err != nil {
			return err
		}
		// GPU ID zero requests only the CPU and system counters.
		if v.GPUID != 0 && !fd.dev.proxy.gpus[v.GPUID] {
			return linuxerr.EINVAL
		}
		return copyOut(t, cmd, addr, &v, fd.call(t, cmd, &v))
	case amdgpu.AMDKFD_IOC_SET_MEMORY_POLICY:
		var v amdgpu.KFDSetMemoryPolicy
		return fd.gpuIoctl(t, cmd, addr, &v, &v.GPUID)
	case amdgpu.AMDKFD_IOC_SET_SCRATCH_BACKING_VA:
		var v amdgpu.KFDSetScratchBackingVA
		return fd.gpuIoctl(t, cmd, addr, &v, &v.GPUID)
	case amdgpu.AMDKFD_IOC_SET_TRAP_HANDLER:
		var v amdgpu.KFDSetTrapHandler
		return fd.gpuIoctl(t, cmd, addr, &v, &v.GPUID)
	case amdgpu.AMDKFD_IOC_AVAILABLE_MEMORY:
		var v amdgpu.KFDAvailableMemory
		return fd.gpuIoctl(t, cmd, addr, &v, &v.GPUID)
	case amdgpu.AMDKFD_IOC_CREATE_QUEUE:
		return fd.createQueue(t, addr)
	case amdgpu.AMDKFD_IOC_DESTROY_QUEUE:
		return fd.destroyQueue(t, addr)
	case amdgpu.AMDKFD_IOC_CREATE_EVENT:
		return fd.createEvent(t, addr)
	case amdgpu.AMDKFD_IOC_SET_XNACK_MODE:
		return fd.setXNACKMode(t, addr)
	case amdgpu.AMDKFD_IOC_GET_PROCESS_APERTURES_NEW:
		return fd.getProcessApertures(t, addr)
	case amdgpu.AMDKFD_IOC_ACQUIRE_VM:
		return fd.acquireVM(t, addr)
	case amdgpu.AMDKFD_IOC_ALLOC_MEMORY_OF_GPU:
		return fd.allocMemory(t, addr)
	case amdgpu.AMDKFD_IOC_FREE_MEMORY_OF_GPU:
		return fd.freeMemory(t, addr)
	case amdgpu.AMDKFD_IOC_MAP_MEMORY_TO_GPU, amdgpu.AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU:
		return fd.mapMemory(t, cmd, addr)
	case amdgpu.AMDKFD_IOC_EXPORT_DMABUF:
		return fd.exportDMABuf(t, addr)
	case amdgpu.AMDKFD_IOC_IMPORT_DMABUF:
		return fd.importDMABuf(t, addr)
	case amdgpu.AMDKFD_IOC_GET_DMABUF_INFO:
		return fd.getDMABufInfo(t, addr)
	case amdgpu.AMDKFD_IOC_IPC_EXPORT_HANDLE:
		return fd.exportIPC(t, addr)
	case amdgpu.AMDKFD_IOC_IPC_IMPORT_HANDLE:
		return fd.importIPC(t, addr)
	case amdgpu.AMDKFD_IOC_WAIT_EVENTS:
		return fd.waitEvents(t, addr)
	default:
		return linuxerr.ENOTTY
	}
}

func (fd *deviceFD) createQueue(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.KFDCreateQueue
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	switch v.QueueType {
	case amdgpu.KFD_IOC_QUEUE_TYPE_COMPUTE,
		amdgpu.KFD_IOC_QUEUE_TYPE_COMPUTE_AQL,
		amdgpu.KFD_IOC_QUEUE_TYPE_SDMA,
		amdgpu.KFD_IOC_QUEUE_TYPE_SDMA_XGMI,
		amdgpu.KFD_IOC_QUEUE_TYPE_SDMA_BY_ENG_ID:
	default:
		return linuxerr.EINVAL
	}
	if !fd.dev.proxy.gpus[v.GPUID] {
		return linuxerr.EINVAL
	}
	// Queue loading and SDMA accounting read these addresses with the CPU.
	// Exclude platform-private memory and populate ordinary application pages
	// without replacing device mappings that are already installed.
	for _, ptr := range []uint64{v.ReadPointerAddress, v.WritePointerAddress} {
		ar, ok := hostarch.Addr(ptr).ToRange(8)
		if !ok {
			return linuxerr.EFAULT
		}
		ar.Start = ar.Start.RoundDown()
		if ar.End, ok = ar.End.RoundUp(); !ok {
			return linuxerr.EFAULT
		}
		if err := t.MemoryManager().EnsureAddressSpaceMappingsForCPU(t, ar); err != nil {
			return err
		}
	}
	c := fd.context
	c.mu.Lock()
	err := fd.call(t, amdgpu.AMDKFD_IOC_CREATE_QUEUE, &v)
	if err == nil {
		c.queues[v.QueueID] = struct{}{}
	}
	c.mu.Unlock()
	return copyOut(t, amdgpu.AMDKFD_IOC_CREATE_QUEUE, addr, &v, err)
}

func (fd *deviceFD) destroyQueue(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.KFDDestroyQueue
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	c := fd.context
	c.mu.Lock()
	err := fd.call(t, amdgpu.AMDKFD_IOC_DESTROY_QUEUE, &v)
	if err == nil {
		delete(c.queues, v.QueueID)
	}
	c.mu.Unlock()
	return copyOut(t, amdgpu.AMDKFD_IOC_DESTROY_QUEUE, addr, &v, err)
}

// createEvent checks that an application-supplied signal page holds the
// KFD_SIGNAL_EVENT_LIMIT slots the driver initializes in it; not every driver
// release checks this itself.
func (fd *deviceFD) createEvent(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.AMDKFD_IOC_CREATE_EVENT
	var v amdgpu.KFDCreateEvent
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	p := fd.context
	p.mu.Lock()
	if v.EventPageOffset != 0 {
		a := p.allocations[v.EventPageOffset]
		if a == nil || a.size < amdgpu.KFD_SIGNAL_EVENT_LIMIT*8 {
			p.mu.Unlock()
			return linuxerr.EINVAL
		}
	}
	err := fd.call(t, cmd, &v)
	p.mu.Unlock()
	return copyOut(t, cmd, addr, &v, err)
}

func (fd *deviceFD) setXNACKMode(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.KFDSetXNACKMode
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	// Retry faults require SVM support. Negative values query the current mode.
	if v.XNACKEnabled > 0 {
		return linuxerr.EPERM
	}
	return copyOut(t, amdgpu.AMDKFD_IOC_SET_XNACK_MODE, addr, &v, fd.call(t, amdgpu.AMDKFD_IOC_SET_XNACK_MODE, &v))
}

// getProcessApertures returns the apertures of the granted GPUs. A request
// for zero nodes returns their number, as in the driver.
func (fd *deviceFD) getProcessApertures(t *kernel.Task, addr hostarch.Addr) error {
	const cmd = amdgpu.AMDKFD_IOC_GET_PROCESS_APERTURES_NEW
	var v amdgpu.KFDGetProcessApertures
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	var all amdgpu.KFDGetProcessApertures
	if err := fd.call(t, cmd, &all); err != nil {
		return err
	}
	if all.NumOfNodes > maxGPUs {
		return linuxerr.EIO
	}
	var entry amdgpu.KFDProcessDeviceApertures
	size := entry.SizeBytes()
	buf := make([]byte, int(all.NumOfNodes)*size)
	if len(buf) != 0 {
		all.AperturesPtr = hostBufferPointer(buf)
		if err := fd.call(t, cmd, &all, platform.HostIoctlBuffer{PointerOffset: 0, Data: buf}); err != nil {
			return err
		}
	}
	var out []byte
	for i := 0; i < int(all.NumOfNodes) && (i+1)*size <= len(buf); i++ {
		entry.UnmarshalBytes(buf[i*size:])
		if fd.dev.proxy.gpus[entry.GPUID] {
			out = append(out, buf[i*size:(i+1)*size]...)
		}
	}
	n := uint32(len(out) / size)
	if v.NumOfNodes != 0 {
		n = min(n, v.NumOfNodes)
		if _, err := t.CopyOutBytes(hostarch.Addr(v.AperturesPtr), out[:int(n)*size]); err != nil {
			return err
		}
	}
	v.NumOfNodes = n
	return copyOut(t, cmd, addr, &v, nil)
}

func (fd *deviceFD) acquireVM(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.KFDAcquireVM
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	f := t.GetFile(int32(v.DRMFD))
	if f == nil {
		return linuxerr.EINVAL
	}
	defer f.DecRef(t)
	render, ok := f.Impl().(*deviceFD)
	if !ok || render.dev.kfd || render.dev.proxy != fd.dev.proxy || render.dev.gpuID != v.GPUID {
		return linuxerr.EINVAL
	}
	v.DRMFD = uint32(render.hostFD)
	return fd.call(t, amdgpu.AMDKFD_IOC_ACQUIRE_VM, &v)
}

func (fd *deviceFD) allocMemory(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.KFDAllocMemory
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if !fd.dev.proxy.gpus[v.GPUID] {
		return linuxerr.EINVAL
	}
	const allocTypes = (1 << 5) - 1
	if bits.OnesCount32(v.Flags&allocTypes) != 1 {
		return linuxerr.EINVAL
	}
	a := &allocation{size: v.Size}
	if v.Flags&amdgpu.KFD_IOC_ALLOC_MEM_FLAGS_AQL_QUEUE_MEM != 0 {
		// The driver maps an AQL queue buffer twice over half the size.
		a.size >>= 1
	}
	userAddr := v.MmapOffset
	if v.Flags&amdgpu.KFD_IOC_ALLOC_MEM_FLAGS_USERPTR != 0 {
		// MmapOffset is the application address to register. The driver
		// retains the address through its MMU notifier. Keep gVisor backing
		// referenced and populate the application's host mappings.
		um, err := pinUserMemory(t, v.MmapOffset, v.Size)
		if err != nil {
			return err
		}
		a.userptr = um
		v.MmapOffset = uint64(um.hostAddr)
	}
	p := fd.context
	p.mu.Lock()
	err := fd.call(t, amdgpu.AMDKFD_IOC_ALLOC_MEMORY_OF_GPU, &v)
	if err != nil {
		if a.userptr != nil {
			a.userptr.release()
			v.MmapOffset = userAddr
		}
	} else {
		p.allocations[v.Handle] = a
	}
	p.mu.Unlock()
	return copyOut(t, amdgpu.AMDKFD_IOC_ALLOC_MEMORY_OF_GPU, addr, &v, err)
}

func (fd *deviceFD) freeMemory(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.KFDFreeMemory
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	p := fd.context
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := fd.call(t, amdgpu.AMDKFD_IOC_FREE_MEMORY_OF_GPU, &v); err != nil {
		return err
	}
	if a := p.allocations[v.Handle]; a != nil {
		if a.userptr != nil {
			a.userptr.release()
		}
		delete(p.allocations, v.Handle)
	}
	return nil
}

func (fd *deviceFD) mapMemory(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var v amdgpu.KFDMapMemory
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if v.NDevices > maxGPUs {
		return linuxerr.EINVAL
	}
	ids := make([]byte, 4*int(v.NDevices))
	ptr := v.DeviceIDsArrayPtr
	if _, err := t.CopyInBytes(hostarch.Addr(ptr), ids); err != nil {
		return err
	}
	for i := 0; i < len(ids); i += 4 {
		if !fd.dev.proxy.gpus[hostarch.ByteOrder.Uint32(ids[i:])] {
			return linuxerr.EINVAL
		}
	}
	v.DeviceIDsArrayPtr = hostBufferPointer(ids)
	err := fd.call(t, cmd, &v, platform.HostIoctlBuffer{PointerOffset: 8, Data: ids})
	v.DeviceIDsArrayPtr = ptr
	return copyOut(t, cmd, addr, &v, err)
}

// waitEvents polls between interruptible sleeps. The flat transport allows
// application threads to run during waits; neither the event data nor the
// returned status establishes ownership of a host resource.
func (fd *deviceFD) waitEvents(t *kernel.Task, addr hostarch.Addr) error {
	var v amdgpu.KFDWaitEvents
	if _, err := v.CopyIn(t, addr); err != nil {
		return err
	}
	if v.NumEvents > amdgpu.KFD_SIGNAL_EVENT_LIMIT {
		return linuxerr.EINVAL
	}
	// Polling consumes pending auto-reset signals, so waiting for all of
	// several events cannot be split up.
	if v.WaitForAll != 0 && v.NumEvents > 1 {
		return linuxerr.EINVAL
	}
	as, ok := t.MemoryManager().AddressSpace().(platform.HostMMIO)
	if !ok {
		return linuxerr.EOPNOTSUPP
	}
	if v.NumEvents != 0 {
		var event amdgpu.KFDEventData
		ar, ok := hostarch.Addr(v.EventsPtr).ToRange(uint64(v.NumEvents) * uint64(event.SizeBytes()))
		if !ok {
			return linuxerr.EFAULT
		}
		ar.Start = ar.Start.RoundDown()
		if ar.End, ok = ar.End.RoundUp(); !ok {
			return linuxerr.EFAULT
		}
		if err := t.MemoryManager().EnsureAddressSpaceMappingsForCPU(t, ar); err != nil {
			return err
		}
	}
	ptr, timeout := v.EventsPtr, v.Timeout
	var deadline time.Time
	if timeout != amdgpu.KFD_EVENT_TIMEOUT_INFINITE {
		deadline = time.Now().Add(time.Duration(min(timeout, 0x7fffffff)) * time.Millisecond)
	}
	buf := make([]byte, v.SizeBytes())
	wait := 100 * time.Microsecond
	var err error
	for {
		v.EventsPtr, v.Timeout = ptr, 0
		v.MarshalBytes(buf)
		// No translated descriptors or Sentry addresses enter writable
		// scratch. Event buffers are ordinary application memory, and all
		// returned values affect only this application's wait result.
		result, transportErr := as.IoctlFlat(fd.hostFD, amdgpu.AMDKFD_IOC_WAIT_EVENTS, buf)
		if transportErr != nil {
			err = transportErr
			break
		}
		var output amdgpu.KFDWaitEvents
		output.UnmarshalBytes(buf)
		v.WaitResult = output.WaitResult
		if result.Errno != 0 {
			err = result.Errno
		}
		if err != nil || v.WaitResult != amdgpu.KFD_IOC_WAIT_RESULT_TIMEOUT || (!deadline.IsZero() && !time.Now().Before(deadline)) {
			break
		}
		pause := wait
		if !deadline.IsZero() {
			pause = max(min(pause, time.Until(deadline)), 0)
		}
		if _, blocked := t.BlockWithTimeout(nil, true, pause); blocked != nil && !linuxerr.Equals(linuxerr.ETIMEDOUT, blocked) {
			err = linuxerr.ERESTARTSYS
			break
		}
		wait = min(wait*2, 10*time.Millisecond)
	}
	v.EventsPtr, v.Timeout = ptr, timeout
	if err != nil {
		v.WaitResult = amdgpu.KFD_IOC_WAIT_RESULT_FAIL
		// A restarted finite wait gets its remaining timeout.
		if linuxerr.Equals(linuxerr.ERESTARTSYS, err) && !deadline.IsZero() {
			v.Timeout = uint32(max(time.Until(deadline), 0) / time.Millisecond)
		}
	}
	return copyOut(t, amdgpu.AMDKFD_IOC_WAIT_EVENTS, addr, &v, err)
}
