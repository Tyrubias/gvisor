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

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/cleanup/cleanup.h"
#include "test/util/file_descriptor.h"
#include "test/util/test_util.h"

namespace gvisor {
namespace testing {
namespace {

// HIP's public VMM ABI permits loading the runtime without SDK build
// dependencies.
struct HipMemLocation {
  int type;
  int id;
};

struct HipMemAllocationProp {
  int type;
  int requested_handle_types;
  HipMemLocation location;
  void* win32_handle_metadata;
  uint8_t compression_type;
  uint8_t gpu_direct_rdma_capable;
  uint16_t usage;
  uint32_t padding;
};

struct HipMemAccessDesc {
  HipMemLocation location;
  int flags;
};

template <typename T>
T Symbol(void* library, const char* name) {
  return reinterpret_cast<T>(dlsym(library, name));
}

TEST(AmdgpuVmm, DescriptorSharing) {
  void* hip =
      dlopen("libamdhip64.so.7", RTLD_NOW | RTLD_GLOBAL | RTLD_NODELETE);
  if (hip == nullptr) {
    hip = dlopen("libamdhip64.so.6", RTLD_NOW | RTLD_GLOBAL | RTLD_NODELETE);
  }
  if (hip == nullptr) {
    GTEST_SKIP() << "AMD HIP runtime is unavailable";
  }
  absl::Cleanup close_hip = [hip] { dlclose(hip); };
  auto device_count = Symbol<int (*)(int*)>(hip, "hipGetDeviceCount");
  auto set_device = Symbol<int (*)(int)>(hip, "hipSetDevice");
  auto granularity = Symbol<int (*)(size_t*, const HipMemAllocationProp*, int)>(
      hip, "hipMemGetAllocationGranularity");
  auto create = Symbol<int (*)(void**, size_t, const HipMemAllocationProp*,
                               unsigned long long)>(hip, "hipMemCreate");
  auto release = Symbol<int (*)(void*)>(hip, "hipMemRelease");
  auto reserve =
      Symbol<int (*)(void**, size_t, size_t, void*, unsigned long long)>(
          hip, "hipMemAddressReserve");
  auto free_address = Symbol<int (*)(void*, size_t)>(hip, "hipMemAddressFree");
  auto map = Symbol<int (*)(void*, size_t, size_t, void*, unsigned long long)>(
      hip, "hipMemMap");
  auto unmap = Symbol<int (*)(void*, size_t)>(hip, "hipMemUnmap");
  auto set_access =
      Symbol<int (*)(void*, size_t, const HipMemAccessDesc*, size_t)>(
          hip, "hipMemSetAccess");
  auto export_fd = Symbol<int (*)(void*, void*, int, unsigned long long)>(
      hip, "hipMemExportToShareableHandle");
  auto import_fd = Symbol<int (*)(void**, void*, int)>(
      hip, "hipMemImportFromShareableHandle");
  auto fill = Symbol<int (*)(void*, int, size_t)>(hip, "hipMemset");
  auto copy =
      Symbol<int (*)(void*, const void*, size_t, int)>(hip, "hipMemcpy");
  auto synchronize = Symbol<int (*)()>(hip, "hipDeviceSynchronize");
  ASSERT_NE(device_count, nullptr);
  ASSERT_NE(set_device, nullptr);
  if (create == nullptr || export_fd == nullptr || import_fd == nullptr) {
    GTEST_SKIP() << "HIP VMM descriptor sharing is unavailable";
  }
  ASSERT_NE(granularity, nullptr);
  ASSERT_NE(release, nullptr);
  ASSERT_NE(reserve, nullptr);
  ASSERT_NE(free_address, nullptr);
  ASSERT_NE(map, nullptr);
  ASSERT_NE(unmap, nullptr);
  ASSERT_NE(set_access, nullptr);
  ASSERT_NE(fill, nullptr);
  ASSERT_NE(copy, nullptr);
  ASSERT_NE(synchronize, nullptr);
  int count = 0;
  if (device_count(&count) != 0 || count == 0) {
    GTEST_SKIP() << "No accessible AMD GPU";
  }
  for (int gpu = 0; gpu < count; ++gpu) {
    SCOPED_TRACE(gpu);
    ASSERT_EQ(set_device(gpu), 0);
    HipMemAllocationProp properties = {};
    properties.type = 1;  // hipMemAllocationTypePinned.
    properties.requested_handle_types =
        1;                           // hipMemHandleTypePosixFileDescriptor.
    properties.location = {1, gpu};  // hipMemLocationTypeDevice.
    size_t size = 0;
    ASSERT_EQ(granularity(&size, &properties, 0), 0);
    ASSERT_GT(size, 0);
    void* handle = nullptr;
    ASSERT_EQ(create(&handle, size, &properties, 0), 0);
    absl::Cleanup release_handle = [&] { EXPECT_EQ(release(handle), 0); };
    void* memory = nullptr;
    ASSERT_EQ(reserve(&memory, size, 0, nullptr, 0), 0);
    absl::Cleanup free_memory = [&] {
      EXPECT_EQ(free_address(memory, size), 0);
    };
    ASSERT_EQ(map(memory, size, 0, handle, 0), 0);
    absl::Cleanup unmap_memory = [&] { EXPECT_EQ(unmap(memory, size), 0); };
    const HipMemAccessDesc access = {{1, gpu}, 3};  // Read/write on this GPU.
    ASSERT_EQ(set_access(memory, size, &access, 1), 0);
    ASSERT_EQ(fill(memory, 0x5a, size), 0);
    ASSERT_EQ(synchronize(), 0);
    int raw_fd = -1;
    ASSERT_EQ(export_fd(&raw_fd, handle, 1, 0), 0);
    FileDescriptor fd(raw_fd);
    void* imported = nullptr;
    ASSERT_EQ(
        import_fd(&imported, reinterpret_cast<void*>(intptr_t(fd.get())), 1),
        0);
    absl::Cleanup release_imported = [&] { EXPECT_EQ(release(imported), 0); };
    ASSERT_THAT(close(fd.release()), SyscallSucceeds());
    void* alias = nullptr;
    ASSERT_EQ(reserve(&alias, size, 0, nullptr, 0), 0);
    absl::Cleanup free_alias = [&] { EXPECT_EQ(free_address(alias, size), 0); };
    ASSERT_EQ(map(alias, size, 0, imported, 0), 0);
    absl::Cleanup unmap_alias = [&] { EXPECT_EQ(unmap(alias, size), 0); };
    ASSERT_EQ(set_access(alias, size, &access, 1), 0);
    std::vector<uint8_t> data(size);
    ASSERT_EQ(copy(data.data(), alias, size, 2 /* hipMemcpyDeviceToHost */), 0);
    EXPECT_TRUE(std::all_of(data.begin(), data.end(),
                            [](uint8_t value) { return value == 0x5a; }));
  }
}

}  // namespace
}  // namespace testing
}  // namespace gvisor
