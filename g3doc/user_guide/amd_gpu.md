# AMD GPU Support

[TOC]

gVisor can run ROCm compute workloads, such as HIP applications and PyTorch, on
AMD Instinct GPUs. To achieve this, gVisor implements a proxy driver inside the
sandbox, henceforth referred to as `amdgpuproxy`. `amdgpuproxy` proxies the
application's interactions with the host's AMDGPU/KFD driver through the
compute device `/dev/kfd` and the DRM render nodes `/dev/dri/renderD*` granted
to the container. ROCm userspace runs unmodified inside the sandbox. gVisor also
reproduces the host's KFD topology under `/sys`, so ROCm's device discovery
behaves the same inside the sandbox as on the host.

AMD GPU support is **experimental**. The proxy admits MI300X (gfx942) and
MI350X/MI355X (gfx950) GPUs. Multi-process HIP, IPC, PyTorch, and SGLang serving
have been validated on an MI350X and on eight MI355X GPUs. For NVIDIA GPUs, see
[NVIDIA GPU Support](gpu.md).

## Enabling AMD GPU support

Pass `--amdgpuproxy=compute` to `runsc`. The [systrap platform](platforms.md) is
required (see [Limitations](#limitations)). The container's OCI spec must list
`/dev/kfd` and the render node of every GPU the container may use; GPUs whose
render node is not listed are not exposed, and `runsc` verifies that each
listed device is the host's KFD device or an `amdgpu` render node. Display
nodes (`/dev/dri/card*`) are never exposed.

ROCm has no equivalent of the NVIDIA container toolkit's library injection, so
the container image must contain the ROCm userspace, or bind-mount the host's
installation. Workloads that request shared virtual memory must select ROCm's
non-SVM fallback (see [Limitations](#limitations)):

```
HSA_USE_SVM=0
HSA_XNACK=0
```

With Docker, register a runtime whose `runsc` is invoked with
`--platform=systrap --amdgpuproxy=compute`, then grant the devices with `--device`:

```
$ docker run --runtime=runsc --rm \
    --device=/dev/kfd --device=/dev/dri/renderD128 \
    -e HSA_USE_SVM=0 -e HSA_XNACK=0 \
    rocm/pytorch python3 -c 'import torch; print(torch.cuda.get_device_name())'
AMD Instinct MI300X
```

Granting several render nodes exposes several GPUs to the same application;
the proxy validates each GPU in mapping and sharing requests.

## Interprocess sharing

Applications can export and import GPU memory through dma-buf descriptors or
legacy KFD IPC handles. Unix socket descriptor passing retains each dma-buf's
ownership and lifetime. Legacy IPC imports accept only handles exported inside
the sandbox. Neither interface permits importing an unrelated host allocation.

## SGLang

SGLang `v0.5.23-rocm10-mi35x` can serve `Qwen/Qwen2.5-7B-Instruct` with the
ROCm environment above. Grant the KFD device and the GPU's render node, provide
a writable temporary directory and shared memory, and run:

```
python3 -m sglang.launch_server \
    --model-path /models/Qwen2.5-7B-Instruct \
    --attention-backend triton \
    --mem-fraction-static 0.2 --context-length 4096 --max-running-requests 4
```

The model directory must contain the downloaded weights. This configuration
has been checked with concurrent requests to `/v1/chat/completions`.

### RCCL symmetric memory

RCCL can use HIP virtual memory management (VMM) and symmetric memory for
collectives between GPUs. VMM is separate from shared virtual memory (SVM).
Keep `HSA_USE_SVM=0` and `HSA_XNACK=0` when you enable VMM.

Grant all participating GPUs and enable RCCL's VMM allocations and window
registration:

```
NCCL_CUMEM_ENABLE=1
NCCL_WIN_ENABLE=1
```

Add `--enable-symm-mem` to the SGLang server command. SGLang can otherwise
disable RCCL VMM allocations. Add `--disable-custom-all-reduce` to route
all-reduce operations through RCCL when you validate this path. Keep RCCL
peer-to-peer transport enabled for GPUs in the same host. This configuration
has been validated with `DeepSeek-V4-Flash-0731` and tensor parallelism across
eight MI355X GPUs.

Select VMM explicitly. RCCL's automatic selection can depend on the kernel
version reported by the sandbox. To verify the path, check for `P2P/CUMEM`
channels and successful window registrations with non-null window handles.
The environment variables alone do not prove that application buffers use
symmetric memory.

SGLang builds its symmetric-memory allocator at startup, so the image must
contain a C++ compiler and ROCm development headers and libraries. Some ROCm
images provide only `librccl.so.1`, while the allocator links with `-lnccl`.
In that case, create a separate build-library directory with a `libnccl.so`
symlink to the same `librccl.so.1` that the application loads, and add that
directory to `LIBRARY_PATH`. The directory can be mounted read-only in the
container. This supplies the link name without changing the RCCL runtime.

## Limitations

*   **Systrap platform only.** KFD opens, ioctls, and device mappings execute
    in the application's host address space. Multiple processes can use the
    GPU concurrently. Forked children must open their own KFD descriptor.
*   **Unsupported ROCm features.** Shared virtual memory (SVM) and XNACK retry
    faults, the runtime debugger, and checkpoint/restore are not supported. `hipMallocManaged()`
    works through ROCm's non-SVM path, that is without GPU page migration.
*   **Supported GPUs.** Only gfx942 (MI300) and gfx950 (MI355) GPUs are
    admitted, on the AMDGPU/KFD driver versions they were validated with. Other
    models fail at sandbox start.

## Security

`amdgpuproxy` follows the same model as `nvproxy`; the
[discussion of GPU security](gpu.md#security) applies. The proxy forwards a
fixed set of KFD and DRM `ioctl`s, mirrored in the sandbox's seccomp filter,
and the host driver is trusted to handle those requests as it would for any
unprivileged native process. The proxy's own job is to keep the application's
pointers and file descriptors out of the sentry's address space: `ioctl`
arguments and their nested buffers are copied through the sentry, registered
application memory is pinned, and XNACK is kept off so that GPU retry faults
cannot access the platform's private mappings. Vulnerabilities in the host
driver's handling of the forwarded requests are not mitigated, so **AMD driver
updates must be part of any security plan with or without gVisor**. The
[design notes](https://github.com/google/gvisor/blob/master/pkg/sentry/devices/amdgpuproxy/README.md)
describe the proxy in more detail.
