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

package systrap

import (
	"math/rand/v2"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/bpf"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/pkg/seccomp/precompiledseccomp"
	"gvisor.dev/gvisor/pkg/sentry/platform/systrap/stubfilter"
)

func TestHostDeviceFilters(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		info := (&Systrap{hostDeviceIO: enabled}).SeccompInfo()
		if enabled && info.ConfigKey() == (&Systrap{}).SeccompInfo().ConfigKey() {
			t.Fatal("host device filters share the default seccomp cache key")
		}
		program := seccomp.Program{
			RuleSets: []seccomp.RuleSet{{Rules: info.SyscallFilters(info.Variables()), Action: seccomp.Allow}},
			Options:  seccomp.ProgramOptions{DefaultAction: seccomp.KillProcess},
		}
		insns, _, err := program.Build()
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := bpf.Compile(insns, true /* optimize */)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			args    [6]uint64
			allowed bool
		}{
			{[6]uint64{unix.PTRACE_ATTACH, 123}, true},
			{[6]uint64{unix.PTRACE_SEIZE, 123, 0, unix.PTRACE_O_EXITKILL}, enabled},
			{[6]uint64{unix.PTRACE_INTERRUPT, 123}, enabled},
			{[6]uint64{unix.PTRACE_SEIZE, 123, 0, unix.PTRACE_O_TRACECLONE}, false},
			{[6]uint64{unix.PTRACE_INTERRUPT, 123, 1}, false},
		} {
			data := linux.SeccompData{Nr: unix.SYS_PTRACE, Arch: seccomp.LINUX_AUDIT_ARCH, Args: tc.args}
			got, err := bpf.Exec[bpf.NativeEndian](compiled, seccomp.DataAsBPFInput(&data, make([]byte, data.SizeBytes())))
			if err != nil {
				t.Fatal(err)
			}
			if allowed := got == uint32(linux.SECCOMP_RET_ALLOW); allowed != tc.allowed {
				t.Errorf("HostDeviceIO=%t args=%#v: action=%#x, want allowed=%t", enabled, tc.args, got, tc.allowed)
			}
		}
	}
}

// TestStubFilterPrecompiled checks that rendering the precompiled stub
// programs yields the same bytecode as compiling them with the same values,
// for randomized stub addresses as chosen by stubInit.
func TestStubFilterPrecompiled(t *testing.T) {
	const stubROMapLen = 0x9000
	rendered := map[string][]bpf.Instruction{}
	for range 16 {
		stubStart := rand.Uint64N(1<<47) &^ uint64(hostarch.PageMask)
		stubSysmsgStart := stubStart + 0x5000
		for _, test := range []struct {
			name string
			vars precompiledseccomp.Values
			fn   func(precompiledseccomp.Values) *seccomp.Program
		}{
			{stubfilter.StubProcessName, stubfilter.StubProcessVars(stubStart, stubROMapLen), stubfilter.StubProcess},
			{stubfilter.SysmsgThreadName, stubfilter.SysmsgThreadVars(stubStart, stubSysmsgStart), stubfilter.SysmsgThread},
			{stubfilter.SyscallThreadName, nil, stubfilter.SyscallThread},
		} {
			want, _, err := test.fn(test.vars).Build()
			if err != nil {
				t.Fatalf("%s (stubStart=%#x): cannot build program: %v", test.name, stubStart, err)
			}
			got := renderStubFilter(test.name, test.vars)
			if !slices.Equal(got, want) {
				t.Errorf("%s (stubStart=%#x): rendered precompiled program differs from compiled program:\ngot:  %v\nwant: %v", test.name, stubStart, got, want)
			}
			if prev, ok := rendered[test.name]; ok && test.vars != nil && slices.Equal(got, prev) {
				t.Errorf("%s: rendered program does not depend on stubStart=%#x", test.name, stubStart)
			}
			rendered[test.name] = got
		}
	}
}
