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

package amdgpu

import "structs"

// DRM ioctl requests, for 64-bit applications.
const (
	DRM_IOCTL_VERSION     = 0xc0406400
	DRM_IOCTL_AMDGPU_INFO = 0x40206445
)

// DRMVersion is struct drm_version.
//
// +marshal
type DRMVersion struct {
	_          structs.HostLayout
	Major      int32
	Minor      int32
	PatchLevel int32
	Pad        uint32
	NameLen    uint64
	Name       uint64
	DateLen    uint64
	Date       uint64
	DescLen    uint64
	Desc       uint64
}

// DRMAMDGPUInfo is struct drm_amdgpu_info. QueryData is the query-specific
// input union, none of whose members are pointers.
//
// +marshal
type DRMAMDGPUInfo struct {
	_             structs.HostLayout
	ReturnPointer uint64
	ReturnSize    uint32
	Query         uint32
	QueryData     [4]uint32
}

// DRM sharing and buffer management ioctl requests. GEM_OP and GEM_VA use
// the original argument sizes, which remain supported by current drivers.
const (
	DRM_IOCTL_GEM_CLOSE               = 0x40086409
	DRM_IOCTL_PRIME_HANDLE_TO_FD      = 0xc00c642d
	DRM_IOCTL_PRIME_FD_TO_HANDLE      = 0xc00c642e
	DRM_IOCTL_SYNCOBJ_CREATE          = 0xc00864bf
	DRM_IOCTL_SYNCOBJ_DESTROY         = 0xc00864c0
	DRM_IOCTL_AMDGPU_GEM_MMAP         = 0xc0086441
	DRM_IOCTL_AMDGPU_GEM_METADATA     = 0xc1206446
	DRM_IOCTL_AMDGPU_GEM_VA           = 0x40286448
	DRM_IOCTL_AMDGPU_GEM_OP           = 0xc0106450
	AMDGPU_GEM_OP_GET_GEM_CREATE_INFO = 0
	AMDGPU_GEM_OP_SET_PLACEMENT       = 1
)

// DRMPrimeHandle is struct drm_prime_handle.
//
// +marshal
type DRMPrimeHandle struct {
	_      structs.HostLayout
	Handle uint32
	Flags  uint32
	FD     int32
}

// DRMGEMOp is the original struct drm_amdgpu_gem_op.
//
// +marshal
type DRMGEMOp struct {
	_      structs.HostLayout
	Handle uint32
	Op     uint32
	Value  uint64
}

// Extended GEM_VA requests include VM timeline synchronization. Both direction
// encodings are used by AMD driver and userspace releases.
const (
	DRM_IOCTL_AMDGPU_GEM_VA_EXT    = 0x40406448
	DRM_IOCTL_AMDGPU_GEM_VA_EXT_RW = 0xc0406448
)

// DRMGEMVAExt is the extended struct drm_amdgpu_gem_va.
//
// +marshal
type DRMGEMVAExt struct {
	_                        structs.HostLayout
	Handle                   uint32
	Pad                      uint32
	Operation                uint32
	Flags                    uint32
	VAAddress                uint64
	OffsetInBO               uint64
	MapSize                  uint64
	VMTimelinePoint          uint64
	VMTimelineSyncobjOut     uint32
	NumSyncobjHandles        uint32
	InputFenceSyncobjHandles uint64
}

// Timeline wait request versions, with and without the deadline hint.
const (
	DRM_IOCTL_SYNCOBJ_TIMELINE_WAIT    = 0xc03064ca
	DRM_IOCTL_SYNCOBJ_TIMELINE_WAIT_V1 = 0xc02864ca
)

// DRMSyncobjTimelineWait is struct drm_syncobj_timeline_wait.
//
// +marshal
type DRMSyncobjTimelineWait struct {
	_             structs.HostLayout
	Handles       uint64
	Points        uint64
	TimeoutNsec   int64
	CountHandles  uint32
	Flags         uint32
	FirstSignaled uint32
	Pad           uint32
	DeadlineNsec  uint64
}
