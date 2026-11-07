load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "amdgpuproxy",
    srcs = [
        "amdgpuproxy.go",
        "dmabuf.go",
        "drm.go",
        "ioctl_unsafe.go",
        "kfd.go",
        "seccomp_filters.go",
        "sharing.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/amdgpu",
        "//pkg/abi/linux",
        "//pkg/amdgpu",
        "//pkg/context",
        "//pkg/devutil",
        "//pkg/errors/linuxerr",
        "//pkg/hostarch",
        "//pkg/marshal",
        "//pkg/seccomp",
        "//pkg/sentry/arch",
        "//pkg/sentry/fsutil",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/ktime",
        "//pkg/sentry/memmap",
        "//pkg/sentry/mm",
        "//pkg/sentry/platform",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/usermem",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "amdgpuproxy_test",
    size = "small",
    srcs = ["sharing_test.go"],
    embed = [":amdgpuproxy"],
    deps = ["//pkg/abi/amdgpu"],
)
