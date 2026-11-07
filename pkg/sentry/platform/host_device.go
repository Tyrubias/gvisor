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

package platform

// HostDeviceIO is implemented by AddressSpaces that can execute device
// operations in a consistent host process with protected arguments and results.
// Operations are serialized with address-space changes. Application execution
// cannot change the argument snapshot or results during an operation.
type HostDeviceIO interface {
	// OpenHostDevice opens path in the host process. The caller must authorize
	// the path and flags. The returned descriptor belongs to the Sentry's shared
	// host descriptor table and must be closed by the caller.
	OpenHostDevice(path string, flags uint32) (int32, error)

	// IoctlHostDevice executes an authorized ioctl. The caller must hold a
	// reference to hostFD, validate all arguments, and authorize each embedded
	// descriptor and device identifier. Nested buffers are copied through
	// protected scratch, with their pointers relocated for the host process.
	// Embedded application addresses must exclude platform-private memory.
	// The result and output buffers are captured before application execution
	// resumes, so they may establish ownership of returned host descriptors.
	IoctlHostDevice(hostFD int32, request uint32, buf []byte, nested ...HostIoctlBuffer) (uintptr, error)

	// IoctlHostDeviceFlat is like HostMMIO.IoctlFlat, with support for nested
	// buffers. The platform relocates their pointers into application-writable
	// scratch. Arguments and results are untrusted and must not establish
	// ownership of host resources. The ioctl must not consume translated host
	// descriptors or create descriptors. E2BIG indicates that the combined
	// arguments exceed the platform's scratch capacity.
	IoctlHostDeviceFlat(hostFD int32, request uint32, buf []byte, nested ...HostIoctlBuffer) (HostIoctlResult, error)

	// OnHostDeviceRelease registers two phases of address-space cleanup.
	// Quiesce runs before application mappings are removed and must stop device
	// access to them. It may call IoctlHostDevice, but must not access the
	// MemoryManager's mappings. Cleanup runs after the host device context has
	// been destroyed, before the platform releases its backing memory. It must
	// not access the AddressSpace or its MemoryManager's mappings.
	//
	// The platform invokes both callbacks without holding its locks. Callbacks
	// may run with MemoryManager locks held and must not acquire filesystem
	// locks, including by releasing VFS file descriptions.
	OnHostDeviceRelease(quiesce, cleanup func()) error
}

// HostIoctlBuffer describes an ioctl's nested buffer. PointerOffset locates
// its 64-bit pointer in the top-level argument. Data is Sentry-owned memory;
// it must not alias application-writable memory.
type HostIoctlBuffer struct {
	PointerOffset int
	Data          []byte
}
