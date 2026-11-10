load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "ionicproxy",
    srcs = ["ionicproxy.go"],
    visibility = [
        "//pkg/sentry:internal",
        "//runsc:__subpackages__",
    ],
    deps = [
        "//pkg/abi/ib",
        "//pkg/cleanup",
        "//pkg/errors/linuxerr",
        "//pkg/sentry/devices/rdmaproxy",
        "//pkg/sentry/kernel",
    ],
)
