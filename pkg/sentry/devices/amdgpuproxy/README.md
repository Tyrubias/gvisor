# AMD GPU proxy

`amdgpuproxy` implements `/dev/kfd` and AMD DRM render nodes for ROCm compute
on the systrap platform. Enable it with `--amdgpuproxy=compute` and grant
`/dev/kfd` plus the required render nodes in the OCI spec.

## Device contexts

KFD associates its primary context with the host address space that opens
`/dev/kfd`. Its ioctl and mmap paths also check the caller's thread-group
leader. The proxy opens KFD and executes its ioctls in the application's
canonical systrap syscall thread. Each application address space has its own
KFD context and allocation table. Threads share that context. A forked child
must open its own KFD descriptor; using its parent's descriptor returns `EBADF`.

Systrap shares its host descriptor table with the Sentry. Guest descriptors
refer to VFS file descriptions that own the corresponding host descriptors.
Render nodes retain their native per-file state and are opened through the
device gofer.

Device operations use `platform.HostDeviceIO`. Systrap stops all application
execution threads while it copies ioctl arguments into internal scratch,
executes the operation, and captures its results. Nested buffers are copied
and relocated into that scratch. Translated host descriptors and returned
resource identifiers cannot be changed by application threads during the
operation. Host addresses of Sentry buffers are never passed to the driver.

An address space with device context is destroyed when its application exits;
it is not returned to the systrap pool. KFD's MMU release notifier tears down
its queues. The proxy then releases its references to registered application
memory, including allocations that the application did not explicitly free.

## Memory

`USERPTR` registrations retain gVisor page references and populate their
application host mappings before the driver accesses them. Registration uses
the application virtual address. Pinning resolves software copy-on-write and
preserves the parent's registered pages across fork.

Device mappings are installed in the application's host mm. The host driver
sets their CPU memory attributes. KFD doorbell and MMIO mappings are excluded
from fork. Mapping population must not replace driver-owned VMAs.

XNACK retry faults are disabled before a KFD descriptor is exposed to an
application. Retry faults must not resolve against systrap's private VMAs.
Requests to enable XNACK are rejected.

## Admission and discovery

The proxy admits a fixed ioctl set, also installed in the source stub's
seccomp filter. It copies and bounds nested buffers, translates guest
identifiers, and checks GPU IDs against the render nodes granted to the
sandbox. Other requests return `ENOTTY`.

The sandbox's `/sys` contains a snapshot of KFD topology and the PCI identity
of the granted render nodes. ROCm filters topology nodes by opening their
render devices, as it does with native device cgroups.

## Sharing

KFD legacy IPC imports accept only handles captured from exports inside the
sandbox. The registry covers all application processes and granted GPUs.
A driver import still enforces the exported allocation's native lifetime.

KFD and DRM PRIME dma-buf exports create guest file descriptions. Imports
translate only those file descriptions from the same sandbox. Descriptor
passing over a Unix socket preserves the file object's identity and lifetime.
The host descriptor table is shared by systrap stubs, so the same host
reference is available in the receiving process without a duplicate.

## Current limitations

The compute path requires these ROCm settings:

```
HSA_ENABLE_SDMA=0 HSA_USE_SVM=0 HSA_XNACK=0
HIP_MEM_POOL_USE_VM=0 DEBUG_HIP_MEM_POOL_VMHEAP=0 HIP_VMEM_MANAGE_SUPPORT=0
```

SDMA, SVM, and checkpoint/restore are not yet supported. Event waits remain
interruptible; wait-for-all on multiple events is rejected. The application must have a writable temporary directory for
ROCm code loading.
