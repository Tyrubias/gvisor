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
#include <signal.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <unistd.h>

#include <atomic>
#include <cstdint>
#include <tuple>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/time/clock.h"
#include "absl/time/time.h"
#include "test/util/cleanup.h"
#include "test/util/file_descriptor.h"
#include "test/util/logging.h"
#include "test/util/multiprocess_util.h"
#include "test/util/posix_error.h"
#include "test/util/signal_util.h"
#include "test/util/test_util.h"
#include "test/util/thread_util.h"

namespace gvisor {
namespace testing {
namespace {

struct KFDVersion {
  uint32_t major;
  uint32_t minor;
};

constexpr unsigned long kGetVersion = _IOR('K', 1, KFDVersion);

struct KFDCreateEvent {
  uint64_t event_page_offset;
  uint32_t event_trigger_data;
  uint32_t event_type;
  uint32_t auto_reset;
  uint32_t node_id;
  uint32_t event_id;
  uint32_t event_slot_index;
};

struct KFDEvent {
  uint32_t event_id;
  uint32_t pad;
};

struct KFDEventData {
  uint8_t data[32];
  uint64_t event_data_ext;
  uint32_t event_id;
  uint32_t pad;
};

struct KFDWaitEvents {
  uint64_t events_ptr;
  uint32_t num_events;
  uint32_t wait_for_all;
  uint32_t timeout;
  uint32_t wait_result;
};

constexpr unsigned long kCreateEvent = _IOWR('K', 8, KFDCreateEvent);
constexpr unsigned long kDestroyEvent = _IOW('K', 9, KFDEvent);
constexpr unsigned long kWaitEvents = _IOWR('K', 12, KFDWaitEvents);
constexpr uint32_t kEventMemory = 8;
constexpr uint32_t kTimeoutInfinite = UINT32_MAX;
constexpr uint32_t kWaitResultTimeout = 1;
constexpr uint32_t kWaitResultFail = 2;

TEST(KFDTest, ProcessOwnership) {
  const FileDescriptor fd(open("/dev/kfd", O_RDWR | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  KFDVersion version = {};
  ASSERT_THAT(ioctl(fd.get(), kGetVersion, &version), SyscallSucceeds());

  ScopedThread thread([&] {
    KFDVersion other = {};
    EXPECT_THAT(ioctl(fd.get(), kGetVersion, &other), SyscallSucceeds());
    EXPECT_EQ(other.major, version.major);
    EXPECT_EQ(other.minor, version.minor);
  });
  thread.Join();

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

using KFDWaitEventsTest = ::testing::TestWithParam<std::tuple<uint32_t, bool>>;

TEST_P(KFDWaitEventsTest, Interrupted) {
  const auto [timeout, restart] = GetParam();
  const FileDescriptor fd(open("/dev/kfd", O_RDWR | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());

  KFDCreateEvent create = {};
  // Memory events do not require a GPU signal page.
  create.event_type = kEventMemory;
  ASSERT_THAT(ioctl(fd.get(), kCreateEvent, &create), SyscallSucceeds());
  KFDEvent event = {create.event_id, 0};
  const auto destroy_event = Cleanup([&] {
    EXPECT_THAT(ioctl(fd.get(), kDestroyEvent, &event), SyscallSucceeds());
  });

  struct sigaction sa = {};
  sa.sa_handler = +[](int) {};
  sa.sa_flags = restart ? SA_RESTART : 0;
  sigemptyset(&sa.sa_mask);
  const auto restore_action =
      ASSERT_NO_ERRNO_AND_VALUE(ScopedSigaction(SIGUSR1, sa));
  const auto restore_mask =
      ASSERT_NO_ERRNO_AND_VALUE(ScopedSignalMask(SIG_UNBLOCK, SIGUSR1));

  KFDEventData data = {};
  data.event_id = event.event_id;
  KFDWaitEvents wait = {};
  wait.events_ptr = reinterpret_cast<uintptr_t>(&data);
  wait.num_events = 1;
  wait.timeout = timeout;
  const pid_t pid = getpid();
  const pid_t tid = gettid();
  std::atomic<bool> done = false;
  ScopedThread signal([&] {
    absl::SleepFor(absl::Milliseconds(100));
    while (!done.load(std::memory_order_relaxed)) {
      EXPECT_THAT(tgkill(pid, tid, SIGUSR1), SyscallSucceeds());
      absl::SleepFor(absl::Milliseconds(100));
    }
  });

  if (restart) {
    EXPECT_THAT(ioctl(fd.get(), kWaitEvents, &wait), SyscallSucceeds());
    EXPECT_EQ(wait.wait_result, kWaitResultTimeout);
  } else {
    EXPECT_THAT(ioctl(fd.get(), kWaitEvents, &wait),
                SyscallFailsWithErrno(EINTR));
    EXPECT_EQ(wait.wait_result, kWaitResultFail);
  }
  if (timeout == kTimeoutInfinite) {
    EXPECT_EQ(wait.timeout, timeout);
  } else {
    if (!restart) {
      EXPECT_GT(wait.timeout, 0);
    }
    EXPECT_LT(wait.timeout, timeout);
  }
  done.store(true, std::memory_order_relaxed);
  signal.Join();
}

INSTANTIATE_TEST_SUITE_P(KFDTest, KFDWaitEventsTest,
                         ::testing::Values(std::make_tuple(10000u, false),
                                           std::make_tuple(kTimeoutInfinite,
                                                           false),
                                           std::make_tuple(1000u, true)));

}  // namespace
}  // namespace testing
}  // namespace gvisor
