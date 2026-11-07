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

#include <drm/drm.h>
#include <errno.h>
#include <fcntl.h>
#include <glob.h>
#include <linux/kfd_ioctl.h>
#include <sched.h>
#include <signal.h>
#include <stddef.h>
#include <stdio.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#include <atomic>
#include <cstdint>
#include <tuple>
#include <utility>
#include <vector>

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

// KFDCreateQueue is the 96-byte queue creation ABI. Older system headers omit
// the SDMA engine and metadata ring fields.
struct KFDCreateQueue {
  uint64_t ring_base_address;
  uint64_t write_pointer_address;
  uint64_t read_pointer_address;
  uint64_t doorbell_offset;
  uint32_t ring_size;
  uint32_t gpu_id;
  uint32_t queue_type;
  uint32_t queue_percentage;
  uint32_t queue_priority;
  uint32_t queue_id;
  uint64_t eop_buffer_address;
  uint64_t eop_buffer_size;
  uint64_t ctx_save_restore_address;
  uint32_t ctx_save_restore_size;
  uint32_t ctl_stack_size;
  uint32_t sdma_engine_id;
  uint32_t metadata_ring_size;
};

constexpr unsigned long kCreateQueue = _IOWR('K', 2, KFDCreateQueue);

struct KFDExportDMABuf {
  uint64_t handle;
  uint32_t flags;
  uint32_t dmabuf_fd;
};

constexpr unsigned long kExportDMABuf = _IOWR('K', 0x24, KFDExportDMABuf);

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
constexpr uint32_t kWaitResultComplete = 0;
constexpr uint32_t kWaitResultTimeout = 1;
constexpr uint32_t kWaitResultFail = 2;

struct DRMSyncobj {
  uint32_t handle;
  uint32_t flags;
};

struct DRMSyncobjTimelineWait {
  uint64_t handles;
  uint64_t points;
  int64_t timeout_nsec;
  uint32_t count_handles;
  uint32_t flags;
  uint32_t first_signaled;
  uint32_t pad;
  uint64_t deadline_nsec;
};

constexpr unsigned long kSyncobjCreate = _IOWR('d', 0xbf, DRMSyncobj);
constexpr unsigned long kSyncobjDestroy = _IOWR('d', 0xc0, DRMSyncobj);
constexpr unsigned long kTimelineWait =
    _IOWR('d', 0xca, DRMSyncobjTimelineWait);
constexpr unsigned long kTimelineWaitV1 =
    _IOC(_IOC_READ | _IOC_WRITE, 'd', 0xca, 40);
constexpr uint32_t kSyncobjCreateSignaled = 1;
constexpr uint32_t kWaitForSubmit = 2;

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

PosixErrorOr<FileDescriptor> AcquireKFDVM(int fd, uint32_t* gpu_id) {
  kfd_ioctl_get_process_apertures_new_args apertures = {};
  if (ioctl(fd, AMDKFD_IOC_GET_PROCESS_APERTURES_NEW, &apertures) < 0) {
    return PosixError(errno, "GET_PROCESS_APERTURES_NEW count");
  }
  std::vector<kfd_process_device_apertures> devices(apertures.num_of_nodes);
  apertures.kfd_process_device_apertures_ptr =
      reinterpret_cast<uintptr_t>(devices.data());
  if (ioctl(fd, AMDKFD_IOC_GET_PROCESS_APERTURES_NEW, &apertures) < 0) {
    return PosixError(errno, "GET_PROCESS_APERTURES_NEW");
  }

  glob_t nodes = {};
  const int found = glob("/dev/dri/renderD*", 0, nullptr, &nodes);
  const auto free_nodes = Cleanup([&] { globfree(&nodes); });
  if (found != 0) {
    return PosixError(found == GLOB_NOMATCH ? ENODEV : EIO,
                      "Cannot enumerate render nodes");
  }
  for (const auto& device : devices) {
    for (size_t i = 0; i < nodes.gl_pathc; ++i) {
      FileDescriptor render(open(nodes.gl_pathv[i], O_RDWR | O_CLOEXEC));
      if (render.get() < 0 &&
          (errno == EACCES || errno == ENODEV || errno == ENOENT)) {
        continue;
      }
      if (render.get() < 0) {
        return PosixError(errno, "open render node");
      }
      kfd_ioctl_acquire_vm_args vm = {};
      vm.drm_fd = render.get();
      vm.gpu_id = device.gpu_id;
      const int ret = ioctl(fd, AMDKFD_IOC_ACQUIRE_VM, &vm);
      if (ret < 0 && errno == EINVAL) {
        continue;
      }
      if (ret < 0) {
        return PosixError(errno, "ACQUIRE_VM");
      }
      *gpu_id = device.gpu_id;
      return std::move(render);
    }
  }
  return PosixError(ENODEV, "No accessible KFD GPU");
}

TEST(KFDTest, ReopenWithQueue) {
  const FileDescriptor fd(open("/dev/kfd", O_RDWR | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  uint32_t gpu_id = 0;
  auto render_or = AcquireKFDVM(fd.get(), &gpu_id);
  SKIP_IF(!render_or.ok() && render_or.error().errno_value() == ENODEV);
  const FileDescriptor render = ASSERT_NO_ERRNO_AND_VALUE(std::move(render_or));

  const size_t size = getpagesize();
  void* pages[3];
  Cleanup unmap_cpu[3], free_memory[3], unmap_gpu[3];
  for (int i = 0; i < 3; ++i) {
    pages[i] =
        mmap(nullptr, size, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    ASSERT_NE(pages[i], MAP_FAILED);
    unmap_cpu[i] = Cleanup(
        [&, i] { EXPECT_THAT(munmap(pages[i], size), SyscallSucceeds()); });
    kfd_ioctl_alloc_memory_of_gpu_args alloc = {};
    alloc.va_addr = reinterpret_cast<uintptr_t>(pages[i]);
    alloc.size = size;
    alloc.gpu_id = gpu_id;
    alloc.flags =
        KFD_IOC_ALLOC_MEM_FLAGS_GTT | KFD_IOC_ALLOC_MEM_FLAGS_WRITABLE |
        KFD_IOC_ALLOC_MEM_FLAGS_COHERENT | KFD_IOC_ALLOC_MEM_FLAGS_UNCACHED;
    ASSERT_THAT(ioctl(fd.get(), AMDKFD_IOC_ALLOC_MEMORY_OF_GPU, &alloc),
                SyscallSucceeds());
    free_memory[i] = Cleanup([&, handle = alloc.handle] {
      kfd_ioctl_free_memory_of_gpu_args args = {handle};
      EXPECT_THAT(ioctl(fd.get(), AMDKFD_IOC_FREE_MEMORY_OF_GPU, &args),
                  SyscallSucceeds());
    });
    ASSERT_EQ(mmap(pages[i], size, PROT_READ | PROT_WRITE,
                   MAP_SHARED | MAP_FIXED, render.get(), alloc.mmap_offset),
              pages[i]);
    *static_cast<uint64_t*>(pages[i]) = 0;
    kfd_ioctl_map_memory_to_gpu_args map = {};
    map.handle = alloc.handle;
    map.device_ids_array_ptr = reinterpret_cast<uintptr_t>(&gpu_id);
    map.n_devices = 1;
    ASSERT_THAT(ioctl(fd.get(), AMDKFD_IOC_MAP_MEMORY_TO_GPU, &map),
                SyscallSucceeds());
    unmap_gpu[i] = Cleanup([&, handle = alloc.handle] {
      kfd_ioctl_unmap_memory_from_gpu_args args = {};
      args.handle = handle;
      args.device_ids_array_ptr = reinterpret_cast<uintptr_t>(&gpu_id);
      args.n_devices = 1;
      EXPECT_THAT(ioctl(fd.get(), AMDKFD_IOC_UNMAP_MEMORY_FROM_GPU, &args),
                  SyscallSucceeds());
    });
  }
  // Keep an empty SDMA queue alive without submitting commands or mapping its
  // doorbell. Destroy it before releasing the ring and pointer buffers.
  KFDCreateQueue queue = {};
  queue.ring_base_address = reinterpret_cast<uintptr_t>(pages[0]);
  queue.write_pointer_address = reinterpret_cast<uintptr_t>(pages[1]);
  queue.read_pointer_address = reinterpret_cast<uintptr_t>(pages[2]);
  queue.ring_size = size;
  queue.gpu_id = gpu_id;
  queue.queue_type = KFD_IOC_QUEUE_TYPE_SDMA;
  queue.queue_percentage = 100;
  ASSERT_THAT(ioctl(fd.get(), kCreateQueue, &queue), SyscallSucceeds());
  const auto destroy = Cleanup([&] {
    kfd_ioctl_destroy_queue_args args = {queue.queue_id, 0};
    EXPECT_THAT(ioctl(fd.get(), AMDKFD_IOC_DESTROY_QUEUE, &args),
                SyscallSucceeds());
  });

  const auto reopen = [&] {
    const FileDescriptor other(open("/dev/kfd", O_RDWR | O_CLOEXEC));
    ASSERT_THAT(other.get(), SyscallSucceeds());
    KFDVersion version = {};
    EXPECT_THAT(ioctl(other.get(), kGetVersion, &version), SyscallSucceeds());
  };
  reopen();
  ScopedThread thread(reopen);
  reopen();
  thread.Join();
}

TEST(KFDTest, ExportCopyOutFault) {
  const FileDescriptor fd(open("/dev/kfd", O_RDWR | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  uint32_t gpu_id = 0;
  auto render_or = AcquireKFDVM(fd.get(), &gpu_id);
  SKIP_IF(!render_or.ok() && render_or.error().errno_value() == ENODEV);
  const FileDescriptor render = ASSERT_NO_ERRNO_AND_VALUE(std::move(render_or));

  const size_t size = getpagesize();
  void* memory =
      mmap(nullptr, size, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  ASSERT_NE(memory, MAP_FAILED);
  const auto unmap_memory =
      Cleanup([&] { EXPECT_THAT(munmap(memory, size), SyscallSucceeds()); });
  kfd_ioctl_alloc_memory_of_gpu_args alloc = {};
  alloc.va_addr = reinterpret_cast<uintptr_t>(memory);
  alloc.size = size;
  alloc.gpu_id = gpu_id;
  alloc.flags = KFD_IOC_ALLOC_MEM_FLAGS_GTT | KFD_IOC_ALLOC_MEM_FLAGS_WRITABLE;
  ASSERT_THAT(ioctl(fd.get(), AMDKFD_IOC_ALLOC_MEMORY_OF_GPU, &alloc),
              SyscallSucceeds());
  const auto free_memory = Cleanup([&] {
    kfd_ioctl_free_memory_of_gpu_args args = {alloc.handle};
    EXPECT_THAT(ioctl(fd.get(), AMDKFD_IOC_FREE_MEMORY_OF_GPU, &args),
                SyscallSucceeds());
  });

  KFDExportDMABuf exported = {alloc.handle, O_RDWR | O_CLOEXEC, 0};
  ASSERT_THAT(ioctl(fd.get(), kExportDMABuf, &exported), SyscallSucceeds());
  FileDescriptor dmabuf(exported.dmabuf_fd);
  drm_prime_handle imported = {};
  imported.fd = dmabuf.get();
  ASSERT_THAT(ioctl(render.get(), DRM_IOCTL_PRIME_FD_TO_HANDLE, &imported),
              SyscallSucceeds());
  const auto close_handle = Cleanup([&] {
    drm_gem_close args = {imported.handle, 0};
    EXPECT_THAT(ioctl(render.get(), DRM_IOCTL_GEM_CLOSE, &args),
                SyscallSucceeds());
  });
  dmabuf.reset();

  void* page = mmap(nullptr, size, PROT_READ | PROT_WRITE,
                    MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  ASSERT_NE(page, MAP_FAILED);
  const auto unmap_page =
      Cleanup([&] { EXPECT_THAT(munmap(page, size), SyscallSucceeds()); });
  for (bool prime : {false, true}) {
    SCOPED_TRACE(prime);
    ASSERT_THAT(mprotect(page, size, PROT_READ | PROT_WRITE),
                SyscallSucceeds());
    if (prime) {
      *static_cast<drm_prime_handle*>(page) = {imported.handle,
                                               O_RDWR | O_CLOEXEC, -1};
    } else {
      *static_cast<KFDExportDMABuf*>(page) = {alloc.handle, O_RDWR | O_CLOEXEC,
                                              UINT32_MAX};
    }
    ASSERT_THAT(mprotect(page, size, PROT_READ), SyscallSucceeds());
    const int expected = dup(fd.get());
    ASSERT_THAT(expected, SyscallSucceeds());
    ASSERT_THAT(close(expected), SyscallSucceeds());
    EXPECT_THAT(
        ioctl(prime ? render.get() : fd.get(),
              prime ? DRM_IOCTL_PRIME_HANDLE_TO_FD : kExportDMABuf, page),
        SyscallFailsWithErrno(EFAULT));
    // The descriptor is installed before the argument copy-out fails.
    const int flags = fcntl(expected, F_GETFD);
    const FileDescriptor result(flags >= 0 ? expected : -1);
    EXPECT_EQ(flags, FD_CLOEXEC);
    if (result.get() >= 0) {
      EXPECT_THAT(lseek(result.get(), 0, SEEK_END),
                  SyscallSucceedsWithValue(size));
    }
  }
}

TEST(KFDTest, FailedOpenDoesNotOwnSharedAddressSpace) {
  const FileDescriptor fd(open("/dev/kfd", O_RDONLY | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  constexpr size_t kStackSize = 1 << 20;
  void* stack = mmap(nullptr, kStackSize, PROT_READ | PROT_WRITE,
                     MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  ASSERT_NE(stack, MAP_FAILED);
  const auto unmap = Cleanup(
      [&] { EXPECT_THAT(munmap(stack, kStackSize), SyscallSucceeds()); });
  // A fresh mm has no KFD owner. The CLONE_VM child has a different thread
  // group, so its failed open must not claim the parent's device context.
  const int status = ASSERT_NO_ERRNO_AND_VALUE(InForkedProcess([&] {
    int error = 0;
    const pid_t child = clone(
        +[](void* arg) {
          const int fd = open("/dev/kfd", O_WRONLY | O_CLOEXEC);
          *static_cast<int*>(arg) = fd < 0 ? errno : 0;
          if (fd >= 0) {
            TEST_CHECK_SUCCESS(close(fd));
          }
          return 0;
        },
        static_cast<char*>(stack) + kStackSize, CLONE_VM | SIGCHLD, &error);
    TEST_CHECK_SUCCESS(child);
    int status;
    TEST_CHECK(waitpid(child, &status, 0) == child);
    TEST_CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0);
    if (error == 0) {
      _exit(77);
    }
    TEST_CHECK(error == EACCES);
    const int own = open("/dev/kfd", O_RDWR | O_CLOEXEC);
    TEST_CHECK_SUCCESS(own);
    KFDVersion version = {};
    TEST_CHECK_SUCCESS(ioctl(own, kGetVersion, &version));
    TEST_CHECK_SUCCESS(close(own));
  }));
  // Exercise this case with host-side denial of write-only KFD opens.
  SKIP_IF(WIFEXITED(status) && WEXITSTATUS(status) == 77);
  EXPECT_EQ(status, 0);
}

TEST(KFDTest, RenderTimelineWaitExpired) {
  glob_t nodes = {};
  const int found = glob("/dev/dri/renderD*", 0, nullptr, &nodes);
  const auto free_nodes = Cleanup([&] { globfree(&nodes); });
  SKIP_IF(found == GLOB_NOMATCH);
  ASSERT_EQ(found, 0);

  struct sigaction sa = {};
  sa.sa_handler = +[](int) {};
  sigemptyset(&sa.sa_mask);
  const auto restore_action =
      ASSERT_NO_ERRNO_AND_VALUE(ScopedSigaction(SIGUSR1, sa));
  const auto restore_mask =
      ASSERT_NO_ERRNO_AND_VALUE(ScopedSignalMask(SIG_UNBLOCK, SIGUSR1));
  const pid_t pid = getpid();
  const pid_t tid = gettid();
  bool tested = false;
  for (size_t i = 0; i < nodes.gl_pathc; ++i) {
    SCOPED_TRACE(nodes.gl_pathv[i]);
    const FileDescriptor fd(open(nodes.gl_pathv[i], O_RDWR | O_CLOEXEC));
    if (fd.get() < 0 &&
        (errno == EACCES || errno == ENODEV || errno == ENOENT)) {
      continue;
    }
    ASSERT_THAT(fd.get(), SyscallSucceeds());
    for (uint32_t flags : {0u, kSyncobjCreateSignaled}) {
      DRMSyncobj object = {0, flags};
      const int ret = ioctl(fd.get(), kSyncobjCreate, &object);
      if (ret < 0 && (errno == EOPNOTSUPP || errno == ENOTTY)) {
        break;
      }
      ASSERT_THAT(ret, SyscallSucceeds());
      const auto destroy = Cleanup([&] {
        DRMSyncobj args = {object.handle, 0};
        EXPECT_THAT(ioctl(fd.get(), kSyncobjDestroy, &args), SyscallSucceeds());
      });
      tested = true;
      for (unsigned long cmd : {kTimelineWaitV1, kTimelineWait}) {
        for (int64_t timeout : {int64_t{-1}, INT64_MIN}) {
          SCOPED_TRACE(::testing::Message() << "cmd=" << cmd << " flags="
                                            << flags << " timeout=" << timeout);
          uint64_t point = 0;
          DRMSyncobjTimelineWait wait = {};
          wait.handles = reinterpret_cast<uintptr_t>(&object.handle);
          wait.points = reinterpret_cast<uintptr_t>(&point);
          wait.timeout_nsec = timeout;
          wait.count_handles = 1;
          wait.flags = kWaitForSubmit;
          // Bound the test if an expired deadline incorrectly blocks.
          std::atomic<bool> done = false;
          ScopedThread signal([&] {
            absl::SleepFor(absl::Milliseconds(100));
            while (!done.load(std::memory_order_relaxed)) {
              EXPECT_THAT(tgkill(pid, tid, SIGUSR1), SyscallSucceeds());
              absl::SleepFor(absl::Milliseconds(100));
            }
          });
          if (flags == kSyncobjCreateSignaled) {
            EXPECT_THAT(ioctl(fd.get(), cmd, &wait), SyscallSucceeds());
          } else {
            EXPECT_THAT(ioctl(fd.get(), cmd, &wait),
                        SyscallFailsWithErrno(ETIME));
          }
          done.store(true, std::memory_order_relaxed);
          signal.Join();
          EXPECT_EQ(wait.timeout_nsec, timeout);
        }
      }
    }
  }
  SKIP_IF(!tested);
}

TEST(KFDTest, WaitEventsBounds) {
  const FileDescriptor fd(open("/dev/kfd", O_RDWR | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());

  for (uint32_t count : {0u, 4097u, UINT32_MAX}) {
    for (uint32_t timeout :
         {0u, 1u, uint32_t{INT32_MAX}, UINT32_MAX - 1, kTimeoutInfinite}) {
      SCOPED_TRACE(::testing::Message()
                   << "count=" << count << " timeout=" << timeout);
      KFDWaitEvents wait = {};
      // These counts do not require access to the event array.
      wait.events_ptr = UINT64_MAX;
      wait.num_events = count;
      wait.timeout = timeout;
      wait.wait_result = 77;
      if (count == 0) {
        EXPECT_THAT(ioctl(fd.get(), kWaitEvents, &wait), SyscallSucceeds());
        EXPECT_EQ(wait.wait_result, kWaitResultComplete);
      } else {
        EXPECT_THAT(ioctl(fd.get(), kWaitEvents, &wait),
                    SyscallFailsWithErrno(ENOMEM));
        EXPECT_EQ(wait.wait_result, kWaitResultFail);
      }
      EXPECT_EQ(wait.timeout, timeout);
      EXPECT_EQ(wait.events_ptr, UINT64_MAX);
    }
  }
}

TEST(KFDTest, WaitEventsBadMemory) {
  const FileDescriptor fd(open("/dev/kfd", O_RDWR | O_CLOEXEC));
  SKIP_IF(fd.get() < 0 && (errno == ENOENT || errno == ENODEV));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  const size_t size = getpagesize();
  void* page =
      mmap(nullptr, size, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  ASSERT_NE(page, MAP_FAILED);
  const auto unmap =
      Cleanup([&] { EXPECT_THAT(munmap(page, size), SyscallSucceeds()); });
  for (uint64_t ptr : {uint64_t{0}, uint64_t{1}, UINT64_MAX - 15,
                       uint64_t{reinterpret_cast<uintptr_t>(page)}}) {
    SCOPED_TRACE(ptr);
    KFDWaitEvents wait = {};
    wait.events_ptr = ptr;
    wait.num_events = 1;
    wait.timeout = kTimeoutInfinite;
    wait.wait_result = 77;
    EXPECT_THAT(ioctl(fd.get(), kWaitEvents, &wait),
                SyscallFailsWithErrno(EFAULT));
    EXPECT_EQ(wait.wait_result, kWaitResultFail);
    EXPECT_EQ(wait.timeout, kTimeoutInfinite);
    EXPECT_EQ(wait.events_ptr, ptr);
  }
  ASSERT_THAT(mprotect(page, size, PROT_READ | PROT_WRITE), SyscallSucceeds());
  auto* wait = static_cast<KFDWaitEvents*>(page);
  *wait = {};
  wait->num_events = UINT32_MAX;
  wait->timeout = kTimeoutInfinite;
  wait->wait_result = 77;
  ASSERT_THAT(mprotect(page, size, PROT_READ), SyscallSucceeds());
  EXPECT_THAT(ioctl(fd.get(), kWaitEvents, wait),
              SyscallFailsWithErrno(EFAULT));
  EXPECT_EQ(wait->wait_result, 77);
  EXPECT_EQ(wait->timeout, kTimeoutInfinite);
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
      EXPECT_LE(wait.timeout, uint32_t{INT32_MAX});
    }
    EXPECT_LT(wait.timeout, timeout);
  }
  done.store(true, std::memory_order_relaxed);
  signal.Join();
}

INSTANTIATE_TEST_SUITE_P(
    KFDTest, KFDWaitEventsTest,
    ::testing::Values(std::make_tuple(10000u, false),
                      std::make_tuple(kTimeoutInfinite, false),
                      std::make_tuple(1000u, true),
                      std::make_tuple(uint32_t{INT32_MAX}, false),
                      std::make_tuple(uint32_t{INT32_MAX} + 1, false),
                      std::make_tuple(UINT32_MAX - 1, false)));

}  // namespace
}  // namespace testing
}  // namespace gvisor
