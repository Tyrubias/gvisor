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

#include <dlfcn.h>
#include <unistd.h>

#include <cstddef>
#include <cstdint>
#include <cstring>
#include <initializer_list>
#include <thread>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/cleanup/cleanup.h"
#include "test/util/file_descriptor.h"
#include "test/util/test_util.h"

struct ibv_device;
struct ibv_context;
struct ibv_pd;
struct ibv_cq;
struct ibv_mr;
struct ibv_comp_channel;

namespace gvisor {
namespace testing {
namespace {

template <typename T>
T Symbol(void* library, const char* name) {
  return reinterpret_cast<T>(dlsym(library, name));
}

TEST(Rdma, PensandoGpuDmaBuf) {
  // Libraries and device access are optional on syscall-test hosts.
  void* verbs = dlopen("libibverbs.so.1", RTLD_NOW | RTLD_NODELETE);
  if (verbs == nullptr) {
    GTEST_SKIP() << "libibverbs is unavailable";
  }
  absl::Cleanup close_verbs = [verbs] { dlclose(verbs); };
  auto get_devices = Symbol<ibv_device** (*)(int*)>(verbs, "ibv_get_device_list");
  auto free_devices =
      Symbol<void (*)(ibv_device**)>(verbs, "ibv_free_device_list");
  auto get_name = Symbol<const char* (*)(ibv_device*)>(verbs,
                                                       "ibv_get_device_name");
  auto open_device =
      Symbol<ibv_context* (*)(ibv_device*)>(verbs, "ibv_open_device");
  auto close_device =
      Symbol<int (*)(ibv_context*)>(verbs, "ibv_close_device");
  auto alloc_pd = Symbol<ibv_pd* (*)(ibv_context*)>(verbs, "ibv_alloc_pd");
  auto dealloc_pd = Symbol<int (*)(ibv_pd*)>(verbs, "ibv_dealloc_pd");
  auto reg_host = Symbol<ibv_mr* (*)(ibv_pd*, void*, size_t, int)>(
      verbs, "ibv_reg_mr");
  auto create_cq = Symbol<ibv_cq* (*)(ibv_context*, int, void*, ibv_comp_channel*,
                                    int)>(verbs, "ibv_create_cq");
  auto destroy_cq = Symbol<int (*)(ibv_cq*)>(verbs, "ibv_destroy_cq");
  auto reg_dmabuf = Symbol<ibv_mr* (*)(ibv_pd*, uint64_t, size_t, uint64_t, int,
                                     int)>(verbs, "ibv_reg_dmabuf_mr");
  auto dereg_mr = Symbol<int (*)(ibv_mr*)>(verbs, "ibv_dereg_mr");
  ASSERT_NE(get_devices, nullptr);
  ASSERT_NE(free_devices, nullptr);
  ASSERT_NE(get_name, nullptr);
  ASSERT_NE(open_device, nullptr);
  ASSERT_NE(close_device, nullptr);
  ASSERT_NE(alloc_pd, nullptr);
  ASSERT_NE(dealloc_pd, nullptr);
  ASSERT_NE(reg_host, nullptr);
  ASSERT_NE(create_cq, nullptr);
  ASSERT_NE(destroy_cq, nullptr);
  if (reg_dmabuf == nullptr) {
    GTEST_SKIP() << "libibverbs has no dma-buf support";
  }
  ASSERT_NE(dereg_mr, nullptr);
  int count = 0;
  ibv_device** devices = get_devices(&count);
  ASSERT_NE(devices, nullptr);
  absl::Cleanup release_devices = [&] { free_devices(devices); };
  bool have_ionic = false;
  for (int i = 0; i < count; ++i) {
    have_ionic |= strncmp(get_name(devices[i]), "ionic_", 6) == 0;
  }
  if (!have_ionic) {
    GTEST_SKIP() << "No Pensando RDMA device";
  }

  void* hip = dlopen("libamdhip64.so.7", RTLD_NOW | RTLD_GLOBAL | RTLD_NODELETE);
  if (hip == nullptr) {
    hip = dlopen("libamdhip64.so.6", RTLD_NOW | RTLD_GLOBAL | RTLD_NODELETE);
  }
  void* hsa = dlopen("libhsa-runtime64.so.1", RTLD_NOW | RTLD_NODELETE);
  if (hip == nullptr || hsa == nullptr) {
    GTEST_SKIP() << "AMD HIP/HSA libraries are unavailable";
  }
  absl::Cleanup close_hip = [hip] { dlclose(hip); };
  absl::Cleanup close_hsa = [hsa] { dlclose(hsa); };
  auto device_count = Symbol<int (*)(int*)>(hip, "hipGetDeviceCount");
  auto set_device = Symbol<int (*)(int)>(hip, "hipSetDevice");
  auto allocate = Symbol<int (*)(void**, size_t)>(hip, "hipMalloc");
  auto allocate_fine = Symbol<int (*)(void**, size_t, unsigned)>(
      hip, "hipExtMallocWithFlags");
  auto free = Symbol<int (*)(void*)>(hip, "hipFree");
  auto export_dmabuf = Symbol<int (*)(const void*, size_t, int*, uint64_t*,
                                     uint64_t)>(
      hsa, "hsa_amd_portable_export_dmabuf_v2");
  ASSERT_NE(device_count, nullptr);
  ASSERT_NE(set_device, nullptr);
  ASSERT_NE(allocate, nullptr);
  ASSERT_NE(allocate_fine, nullptr);
  ASSERT_NE(free, nullptr);
  if (export_dmabuf == nullptr) {
    GTEST_SKIP() << "HSA has no PCIe dma-buf export support";
  }
  int gpus = 0;
  if (device_count(&gpus) != 0 || gpus == 0) {
    GTEST_SKIP() << "No accessible AMD GPU";
  }
  ASSERT_EQ(set_device(0), 0);
  constexpr size_t kSize = 2 * 1024 * 1024;
  void* memory = nullptr;
  ASSERT_EQ(allocate(&memory, kSize), 0);
  absl::Cleanup release_memory = [&] { EXPECT_EQ(free(memory), 0); };
  void* fine_memory = nullptr;
  ASSERT_EQ(allocate_fine(&fine_memory, kSize, 1), 0);
  absl::Cleanup release_fine_memory = [&] { EXPECT_EQ(free(fine_memory), 0); };

  for (int i = 0; i < count; ++i) {
    const char* name = get_name(devices[i]);
    if (strncmp(name, "ionic_", 6) != 0) {
      continue;
    }
    SCOPED_TRACE(name);
    ibv_context* context = open_device(devices[i]);
    ASSERT_NE(context, nullptr);
    absl::Cleanup release_context = [&] { EXPECT_EQ(close_device(context), 0); };
    ibv_pd* pd = alloc_pd(context);
    ASSERT_NE(pd, nullptr);
    absl::Cleanup release_pd = [&] { EXPECT_EQ(dealloc_pd(pd), 0); };
    std::vector<uint8_t> host_memory(65536);
    ibv_mr* host_mr = reg_host(pd, host_memory.data(), host_memory.size(), 7);
    ASSERT_NE(host_mr, nullptr);
    absl::Cleanup release_host_mr = [&] { EXPECT_EQ(dereg_mr(host_mr), 0); };
    // Fine-grained GPU memory also backs RCCL's CPU-accessible control buffers.
    ibv_mr* fine_mr = reg_host(pd, fine_memory, kSize, 15);
    ASSERT_NE(fine_mr, nullptr);
    absl::Cleanup release_fine_mr = [&] { EXPECT_EQ(dereg_mr(fine_mr), 0); };
    ibv_cq* cq = create_cq(context, 128, nullptr, nullptr, 0);
    ASSERT_NE(cq, nullptr);
    absl::Cleanup release_cq = [&] { EXPECT_EQ(destroy_cq(cq), 0); };
    // Exercise registration and handle reuse on one shared uverbs context.
    std::vector<std::thread> workers;
    for (int worker = 0; worker < 2; ++worker) {
      workers.emplace_back([&] {
        for (int iteration = 0; iteration < 16; ++iteration) {
          ibv_mr* mr = reg_host(pd, host_memory.data(), host_memory.size(), 7);
          ASSERT_NE(mr, nullptr);
          EXPECT_EQ(dereg_mr(mr), 0);
        }
      });
    }
    for (auto& worker : workers) {
      worker.join();
    }
    // HSA mapping types NONE and PCIE.
    for (uint64_t flags : {0, 1}) {
      SCOPED_TRACE(flags);
      int fd = -1;
      uint64_t offset = 0;
      ASSERT_EQ(export_dmabuf(memory, kSize, &fd, &offset, flags), 0);
      FileDescriptor dmabuf(fd);
      constexpr int kAccess = 1 | 2 | 4;  // Local write, remote write/read.
      ibv_mr* mr = reg_dmabuf(pd, offset, kSize,
                            reinterpret_cast<uint64_t>(memory), dmabuf.get(),
                            kAccess);
      ASSERT_NE(mr, nullptr);
      absl::Cleanup release_mr = [&] { EXPECT_EQ(dereg_mr(mr), 0); };
      // The memory registration retains the exported object after fd close.
      ASSERT_THAT(close(dmabuf.release()), SyscallSucceeds());
    }
  }
}

}  // namespace
}  // namespace testing
}  // namespace gvisor
