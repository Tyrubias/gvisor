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
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// dmaBufFD owns a dma-buf exported inside this sandbox. Guest descriptor
// passing shares this file object, including its sandbox provenance.
//
// +stateify savable
type dmaBufFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD
	memmap.MappableNoTrackMappings
	memmap.MappableNoHugepages

	proxy      *proxy
	hostFD     int32 `state:"nosave"`
	memmapFile fsutil.MmapPreciseFile
}

func (fd *dmaBufFD) beforeSave() {
	panic("amdgpuproxy: checkpoint is not supported")
}

// HostFD implements vfs.HostFDProvider.HostFD.
func (fd *dmaBufFD) HostFD() int {
	return int(fd.hostFD)
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *dmaBufFD) Release(ctx context.Context) {
	fd.memmapFile.MappableRelease()
}

// Seek implements vfs.FileDescriptionImpl.Seek.
func (fd *dmaBufFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	return unix.Seek(int(fd.hostFD), offset, int(whence))
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *dmaBufFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	return vfs.GenericProxyDeviceConfigureMMap(&fd.vfsfd, fd, opts)
}

// Translate implements memmap.Mappable.Translate.
func (fd *dmaBufFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	return []memmap.Translation{{Source: optional, File: &fd.memmapFile, Offset: optional.Start, Perms: hostarch.AnyAccess}}, nil
}

// newDMABufFD takes ownership of hostFD and installs a guest descriptor.
func (p *proxy) newDMABufFD(t *kernel.Task, hostFD int32, flags uint32) (int32, error) {
	vd := t.Kernel().VFS().NewAnonVirtualDentry("[amdgpu-dmabuf]")
	defer vd.DecRef(t)
	fd := &dmaBufFD{proxy: p, hostFD: hostFD}
	if err := fd.vfsfd.Init(fd, flags&linux.O_ACCMODE, t.Credentials(), vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{UseDentryMetadata: true}); err != nil {
		unix.Close(int(hostFD))
		return -1, err
	}
	fd.memmapFile.SetFD(int(hostFD))
	defer fd.vfsfd.DecRef(t)
	return t.NewFDFrom(0, &fd.vfsfd, kernel.FDFlags{CloseOnExec: flags&linux.O_CLOEXEC != 0})
}

// getDMABufFD returns a referenced sandbox export and its host descriptor.
func (p *proxy) getDMABufFD(t *kernel.Task, guestFD int32) (*vfs.FileDescription, int32, error) {
	f := t.GetFile(guestFD)
	if f == nil {
		return nil, -1, linuxerr.EBADF
	}
	fd, ok := f.Impl().(*dmaBufFD)
	if !ok || fd.proxy != p {
		f.DecRef(t)
		return nil, -1, linuxerr.EINVAL
	}
	return f, fd.hostFD, nil
}

func removeFD(t *kernel.Task, guestFD int32) {
	if f := t.FDTable().Remove(t, guestFD); f != nil {
		f.DecRef(t)
	}
}
