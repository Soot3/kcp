/*
Copyright 2022 The KCP Authors.

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

package framework

import (
	"errors"
	"flag"
	"path/filepath"
	"strings"

	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"

	corev1alpha1 "github.com/kcp-dev/kcp/sdk/apis/core/v1alpha1"
	frameworkhelpers "github.com/kcp-dev/kcp/test/e2e/framework/helpers"
)

func init() {
	klog.InitFlags(flag.CommandLine)
	if err := flag.Lookup("v").Value.Set("4"); err != nil {
		panic(err)
	}
}

type testConfig struct {
	kcpKubeconfig       string
	shardKubeconfigs    map[string]string
	useDefaultKCPServer bool
	suites              string
}

var TestConfig *testConfig

func (c *testConfig) KCPKubeconfig() string {
	// TODO(marun) How to validate before use given that the testing package is calling flags.Parse()?
	if c.useDefaultKCPServer && len(c.kcpKubeconfig) > 0 {
		panic(errors.New("only one of --use-default-kcp-server and --kcp-kubeconfig should be set"))
	}

	if c.useDefaultKCPServer {
		return filepath.Join(frameworkhelpers.RepositoryDir(), ".kcp", "admin.kubeconfig")
	}
	return c.kcpKubeconfig
}

func (c *testConfig) ShardKubeconfig() map[string]string {
	if len(c.shardKubeconfigs) == 0 {
		return map[string]string{corev1alpha1.RootShard: c.KCPKubeconfig()}
	}

	return c.shardKubeconfigs
}

func (c *testConfig) Suites() []string {
	return strings.Split(c.suites, ",")
}

func init() {
	TestConfig = &testConfig{}
	registerFlags(TestConfig)
	// The testing package will call flags.Parse()
}

func registerFlags(c *testConfig) {
	flag.StringVar(&c.kcpKubeconfig, "kcp-kubeconfig", "", "Path to the kubeconfig for a kcp server.")
	flag.Var(cliflag.NewMapStringString(&c.shardKubeconfigs), "shard-kubeconfigs", "Paths to the kubeconfigs for a kcp shard server in the format <shard-name>=<kubeconfig-path>. If unset, kcp-kubeconfig is used.")
	flag.BoolVar(&c.useDefaultKCPServer, "use-default-kcp-server", false, "Whether to use server configuration from .kcp/admin.kubeconfig.")
	flag.StringVar(&c.suites, "suites", "control-plane", "A comma-delimited list of suites to run.")
}
