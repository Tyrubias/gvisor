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

package ib

import "structs"

// IonicABIVersion is IONIC_ABI_VERSION in the Pensando ionic-abi.h UAPI.
const IonicABIVersion = 1

// IonicQueueDesc is struct ionic_qdesc.
//
// +marshal
type IonicQueueDesc struct {
	_          structs.HostLayout
	Addr       uint64
	Size       uint32
	Mask       uint16
	DepthLog2  uint8
	StrideLog2 uint8
}

// IonicCQReq is struct ionic_cq_req.
//
// +marshal
type IonicCQReq struct {
	_        structs.HostLayout
	CQ       [2]IonicQueueDesc
	UDMAMask uint8
	Reserved [7]uint8
}

// IonicQPReq is struct ionic_qp_req.
//
// +marshal
type IonicQPReq struct {
	_          structs.HostLayout
	SQ         IonicQueueDesc
	RQ         IonicQueueDesc
	SQSpec     uint8
	RQSpec     uint8
	SQCMB      uint8
	RQCMB      uint8
	UDMAMask   uint8
	Reserved   [3]uint8
	IonicFlags uint32
	_          uint32
}

// IonicSRQReq is struct ionic_srq_req.
//
// +marshal
type IonicSRQReq struct {
	_        structs.HostLayout
	RQ       IonicQueueDesc
	RQSpec   uint8
	RQCMB    uint8
	UDMAMask uint8
	Reserved [5]uint8
}
