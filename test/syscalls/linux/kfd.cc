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

#include <errno.h>
#include <fcntl.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <unistd.h>

#include <cstdint>
#include <thread>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "test/util/file_descriptor.h"
#include "test/util/logging.h"
#include "test/util/multiprocess_util.h"
#include "test/util/posix_error.h"
#include "test/util/test_util.h"

namespace gvisor {
namespace testing {
namespace {

struct KFDVersion {
  uint32_t major;
  uint32_t minor;
};

constexpr unsigned long kGetVersion = _IOR('K', 1, KFDVersion);

TEST(KFDTest, ProcessOwnership) {
  const FileDescriptor fd(open("/dev/kfd", O_RDWR | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  KFDVersion version = {};
  ASSERT_THAT(ioctl(fd.get(), kGetVersion, &version), SyscallSucceeds());

  std::thread thread([&] {
    KFDVersion other = {};
    EXPECT_THAT(ioctl(fd.get(), kGetVersion, &other), SyscallSucceeds());
    EXPECT_EQ(other.major, version.major);
    EXPECT_EQ(other.minor, version.minor);
  });
  thread.join();

  for (int i = 0; i < 2; ++i) {
    EXPECT_THAT(InForkedProcess([&] {
                  KFDVersion other = {};
                  TEST_CHECK(ioctl(fd.get(), kGetVersion, &other) == -1);
                  TEST_CHECK(errno == EBADF);
                  TEST_CHECK(mmap(nullptr, 4096, PROT_READ | PROT_WRITE,
                                  MAP_SHARED, fd.get(), 0) == MAP_FAILED);
                  TEST_CHECK(errno == EBADF);
                  const int child = open("/dev/kfd", O_RDWR | O_CLOEXEC);
                  TEST_CHECK_SUCCESS(child);
                  TEST_CHECK_SUCCESS(ioctl(child, kGetVersion, &other));
                  TEST_CHECK(other.major == version.major);
                  TEST_CHECK_SUCCESS(close(child));
                }),
                IsPosixErrorOkAndHolds(0));
  }
  EXPECT_THAT(ioctl(fd.get(), kGetVersion, &version), SyscallSucceeds());
}

}  // namespace
}  // namespace testing
}  // namespace gvisor
