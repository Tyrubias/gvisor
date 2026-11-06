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

// Package amdgpuproxy implements /dev/kfd and /dev/dri/renderD* for AMD GPU
// compute (ROCm) on top of the host's AMDGPU/KFD driver.
//
// Device operations execute in the application's host address space on systrap.
// KFD descriptors retain the identity of the opening application process.
package amdgpuproxy

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/amdgpu"
	hostamdgpu "gvisor.dev/gvisor/pkg/amdgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
)

// Register registers the granted devices. gpuIDs maps DRM render minors to
// their KFD GPU IDs.
func Register(vfsObj *vfs.VirtualFilesystem, devices []hostamdgpu.Device, gpuIDs map[uint32]uint32) error {
	p := &proxy{gpus: make(map[uint32]bool), contexts: make(map[platform.AddressSpace]*deviceContext)}
	for _, d := range devices {
		dev := &device{proxy: p, path: strings.TrimPrefix(d.Path, "/dev/"), kfd: d.Path == "/dev/kfd", gpuID: gpuIDs[d.Minor]}
		if !dev.kfd {
			if dev.gpuID == 0 {
				return fmt.Errorf("no KFD GPU ID for %s", d.Path)
			}
			p.gpus[dev.gpuID] = true
		}
		if err := vfsObj.RegisterDevice(vfs.CharDevice, d.Major, d.Minor, dev, &vfs.RegisterDeviceOptions{GroupName: "amdgpuproxy"}); err != nil {
			return err
		}
	}
	return nil
}

// +stateify savable
type proxy struct {
	gpus map[uint32]bool `state:"nosave"`

	mu sync.Mutex `state:"nosave"`
	// +checklocks:mu
	contexts map[platform.AddressSpace]*deviceContext `state:"nosave"`
}

type deviceContext struct {
	proxy *proxy
	owner *mm.MemoryManager
	as    platform.AddressSpace

	mu sync.Mutex
	// +checklocks:mu
	group *kernel.ThreadGroup
	// +checklocks:mu
	allocations map[uint64]*allocation
}

func (p *proxy) context(t *kernel.Task) (*deviceContext, error) {
	as := t.MemoryManager().AddressSpace()
	io, ok := as.(platform.HostDeviceIO)
	if !ok {
		return nil, linuxerr.EOPNOTSUPP
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.contexts[as]; c != nil {
		return c, nil
	}
	c := &deviceContext{proxy: p, owner: t.MemoryManager(), as: as, allocations: make(map[uint64]*allocation)}
	if err := io.OnHostDeviceRelease(c.release); err != nil {
		return nil, err
	}
	p.contexts[as] = c
	return c, nil
}

func (c *deviceContext) release() {
	c.proxy.mu.Lock()
	delete(c.proxy.contexts, c.as)
	c.proxy.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.allocations {
		if a.userptr != nil {
			a.userptr.release()
		}
	}
	c.allocations = nil
}

func (p *proxy) beforeSave() {
	panic("amdgpuproxy: checkpoint is not supported")
}

type allocation struct {
	size    uint64
	userptr *userMemory
}

func (fd *deviceFD) checkOwner(t *kernel.Task) error {
	if !fd.dev.kfd {
		return nil
	}
	c := fd.context
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner != t.MemoryManager() || c.group != t.ThreadGroup() {
		return linuxerr.EBADF
	}
	return nil
}

// +stateify savable
type device struct {
	proxy *proxy
	// path is relative to /dev.
	path string
	kfd  bool
	// gpuID is the KFD GPU ID of a render node.
	gpuID uint32
}

// Open implements vfs.Device.Open.
func (d *device) Open(ctx context.Context, mnt *vfs.Mount, dent *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		return nil, linuxerr.EINVAL
	}
	c, err := d.proxy.context(t)
	if err != nil {
		return nil, err
	}
	var hostFD int
	if d.kfd {
		c.mu.Lock()
		if c.group == nil {
			c.group = t.ThreadGroup()
		}
		io := c.as.(platform.HostDeviceIO)
		var opened int32
		opened, err = io.OpenHostDevice("/dev/kfd", opts.Flags&(unix.O_ACCMODE|unix.O_NONBLOCK)|unix.O_CLOEXEC)
		if err == nil {
			// Retry faults must not resolve against platform-private VMAs.
			var mode [4]byte
			if _, err = io.IoctlHostDevice(opened, amdgpu.AMDKFD_IOC_SET_XNACK_MODE, mode[:]); err != nil {
				_ = unix.Close(int(opened))
			}
		}
		c.mu.Unlock()
		hostFD = int(opened)
	} else {
		client := devutil.GoferClientFromContext(ctx)
		if client == nil {
			return nil, linuxerr.ENOENT
		}
		hostFD, err = client.OpenAt(ctx, d.path, opts.Flags&unix.O_ACCMODE)
	}
	if err != nil {
		return nil, err
	}
	fd := &deviceFD{dev: d, context: c, hostFD: int32(hostFD)}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, dent, &vfs.FileDescriptionOptions{UseDentryMetadata: true, SpecialFile: true}); err != nil {
		unix.Close(hostFD)
		return nil, err
	}
	fd.memmapFile.SetFD(hostFD)
	fd.memmapFile.SetMemType(hostarch.MemoryTypeUncached)
	return &fd.vfsfd, nil
}

// +stateify savable
type deviceFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD
	memmap.MappableNoTrackMappings
	memmap.MappableNoHugepages

	dev        *device
	context    *deviceContext `state:"nosave"`
	hostFD     int32          `state:"nosave"`
	memmapFile fsutil.MmapPreciseFile
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *deviceFD) Release(ctx context.Context) {
	fd.memmapFile.MappableRelease()
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (fd *deviceFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	if err := fd.checkOwner(t); err != nil {
		return 0, err
	}
	cmd, addr := args[1].Uint(), args[2].Pointer()
	var err error
	if fd.dev.kfd {
		err = fd.kfdIoctl(t, cmd, addr)
	} else {
		err = fd.drmIoctl(t, cmd, addr)
	}
	if err != nil {
		ctx.Debugf("amdgpuproxy: %s ioctl %#x: %v", fd.dev.path, cmd, err)
	}
	return 0, err
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *deviceFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		return linuxerr.EINVAL
	}
	if err := fd.checkOwner(t); err != nil {
		return err
	}
	if gpu := uint32((opts.Offset & amdgpu.KFD_MMAP_GPU_ID_MASK) >> amdgpu.KFD_MMAP_GPU_ID_SHIFT); fd.dev.kfd && gpu != 0 && !fd.dev.proxy.gpus[gpu] {
		return linuxerr.EINVAL
	}
	// As vfs.GenericProxyDeviceConfigureMMap, but without its file offset
	// limit: KFD mmap offsets carry the mapping type in bits 62-63.
	if opts.PlatformEffect < memmap.PlatformEffectPopulate {
		opts.PlatformEffect = memmap.PlatformEffectPopulate
	}
	opts.RequirePlatformEffect = true
	opts.DontFork = fd.dev.kfd
	opts.Mappable = fd
	opts.MappingIdentity = &fd.vfsfd
	fd.vfsfd.IncRef()
	return nil
}

// Translate implements memmap.Mappable.Translate.
func (fd *deviceFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	return []memmap.Translation{{Source: optional, File: &fd.memmapFile, Offset: optional.Start, Perms: hostarch.AnyAccess}}, nil
}
