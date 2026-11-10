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

// Package ionicproxy implements RDMA queue-buffer translation for AMD Pensando
// adapters. Init must be called before the devices are registered.
package ionicproxy

import (
	"gvisor.dev/gvisor/pkg/abi/ib"
	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/devices/rdmaproxy"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

type ionicDriver struct{}

// Name implements rdmaproxy.Driver.Name.
func (ionicDriver) Name() string { return "ionic" }

// ABIVersions implements rdmaproxy.Driver.ABIVersions.
func (ionicDriver) ABIVersions() []uint32 { return []uint32{ib.IonicABIVersion} }

// PrepareCreateDMA implements rdmaproxy.Driver.PrepareCreateDMA.
func (ionicDriver) PrepareCreateDMA(t *kernel.Task, action rdmaproxy.DmaKind, uhwIn []byte) (*rdmaproxy.PinnedDMABufs, error) {
	var size, queues int
	switch action {
	case rdmaproxy.DmaCQCreate:
		size, queues = (*ib.IonicCQReq)(nil).SizeBytes(), 2
	case rdmaproxy.DmaQPCreate:
		size, queues = (*ib.IonicQPReq)(nil).SizeBytes(), 2
	case rdmaproxy.DmaSRQCreate:
		size, queues = (*ib.IonicSRQReq)(nil).SizeBytes(), 1
	default:
		return nil, linuxerr.EINVAL
	}
	if len(uhwIn) != size {
		return nil, linuxerr.EINVAL
	}

	var bufs rdmaproxy.PinnedDMABufs
	cu := cleanup.Make(func() { bufs.Release(t) })
	defer cu.Clean()
	var desc ib.IonicQueueDesc
	for i := 0; i < queues; i++ {
		data := uhwIn[i*desc.SizeBytes():]
		desc.UnmarshalBytes(data)
		if desc.Addr == 0 {
			continue
		}
		mp, sentryVA, err := rdmaproxy.MirrorAppPages(t, desc.Addr, uint64(desc.Size))
		if err != nil {
			return nil, err
		}
		bufs.Buffers[i] = mp
		desc.Addr = uint64(sentryVA)
		desc.MarshalBytes(data)
	}
	cu.Release()
	return &bufs, nil
}

// Schemas implements rdmaproxy.Driver.Schemas.
func (ionicDriver) Schemas() map[uint32]*rdmaproxy.MethodSchema { return nil }

// Init registers the Pensando driver.
func Init() { rdmaproxy.RegisterDriver(ionicDriver{}) }
