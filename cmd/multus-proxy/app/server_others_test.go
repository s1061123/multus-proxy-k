// +build !windows

/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package app

import (
	"fmt"
	"testing"
)

type fakeIPSetVersioner struct {
	version string // what to return
	err     error  // what to return
}

func (fake *fakeIPSetVersioner) GetVersion() (string, error) {
	return fake.version, fake.err
}

type fakeKernelCompatTester struct {
	ok bool
}

func (fake *fakeKernelCompatTester) IsCompatible() error {
	if !fake.ok {
		return fmt.Errorf("error")
	}
	return nil
}

// fakeKernelHandler implements KernelHandler.
type fakeKernelHandler struct {
	modules       []string
	kernelVersion string
}

func (fake *fakeKernelHandler) GetModules() ([]string, error) {
	return fake.modules, nil
}

func (fake *fakeKernelHandler) GetKernelVersion() (string, error) {
	return fake.kernelVersion, nil
}

func Test_getProxyMode(t *testing.T) {
	var cases = []struct {
		flag          string
		ipsetVersion  string
		kmods         []string
		kernelVersion string
		kernelCompat  bool
		ipsetError    error
		expected      string
	}{
		{ // flag says iptables, kernel not compatible
			flag:         "iptables",
			kernelCompat: false,
			expected:     proxyModeUserspace,
		},
		{ // flag says iptables, kernel is compatible
			flag:         "iptables",
			kernelCompat: true,
			expected:     proxyModeIPTables,
		},
		{ // detect, kernel not compatible
			flag:         "",
			kernelCompat: false,
			expected:     proxyModeUserspace,
		},
		{ // detect, kernel is compatible
			flag:         "",
			kernelCompat: true,
			expected:     proxyModeIPTables,
		},
	}
	for i, c := range cases {
		kcompater := &fakeKernelCompatTester{c.kernelCompat}
		r := getProxyMode(c.flag, kcompater)
		if r != c.expected {
			t.Errorf("Case[%d] Expected %q, got %q", i, c.expected, r)
		}
	}
}
