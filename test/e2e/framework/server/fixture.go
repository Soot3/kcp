/*
Copyright 2025 The KCP Authors.

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

package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/egymgmbh/go-prefix-writer/prefixer"
	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubernetesscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/component-base/cli/flag"
	"sigs.k8s.io/yaml"

	kcpoptions "github.com/kcp-dev/kcp/cmd/kcp/options"
	"github.com/kcp-dev/kcp/pkg/embeddedetcd"
	"github.com/kcp-dev/kcp/pkg/server"
	corev1alpha1 "github.com/kcp-dev/kcp/sdk/apis/core/v1alpha1"
	kcpscheme "github.com/kcp-dev/kcp/sdk/client/clientset/versioned/scheme"
	"github.com/kcp-dev/kcp/test/e2e/framework/env"
	frameworkhelpers "github.com/kcp-dev/kcp/test/e2e/framework/helpers"
)

// Fixture manages the lifecycle of a set of kcp servers.
//
// Deprecated for use outside this package. Prefer PrivateKcpServer().
type Fixture struct {
	Servers map[string]RunningServer
}

// NewFixture returns a new kcp server fixture.
func NewFixture(t *testing.T, cfgs ...Config) *Fixture {
	t.Helper()

	f := &Fixture{}

	// Initialize servers from the provided configuration
	servers := make([]*kcpServer, 0, len(cfgs))
	f.Servers = make(map[string]RunningServer, len(cfgs))
	for _, cfg := range cfgs {
		if len(cfg.ArtifactDir) == 0 {
			panic(fmt.Sprintf("provided kcpConfig for %s is incorrect, missing ArtifactDir", cfg.Name))
		}
		if len(cfg.DataDir) == 0 {
			panic(fmt.Sprintf("provided kcpConfig for %s is incorrect, missing DataDir", cfg.Name))
		}
		srv, err := newKcpServer(t, cfg, cfg.ArtifactDir, cfg.DataDir, cfg.ClientCADir)
		require.NoError(t, err)

		servers = append(servers, srv)
		f.Servers[srv.name] = srv
	}

	// Launch kcp servers and ensure they are ready before starting the test
	start := time.Now()
	t.Log("Starting kcp servers...")
	wg := sync.WaitGroup{}
	wg.Add(len(servers))
	for i, srv := range servers {
		var opts []RunOption
		if env.LogToConsoleEnvSet() || cfgs[i].LogToConsole {
			opts = append(opts, WithLogStreaming)
		}
		if env.InProcessEnvSet() || cfgs[i].RunInProcess {
			opts = append(opts, RunInProcess)
		}
		err := srv.Run(opts...)
		require.NoError(t, err)

		// Wait for the server to become ready
		go func(s *kcpServer, i int) {
			defer wg.Done()

			err := s.loadCfg()
			require.NoError(t, err, "error loading config")

			err = WaitForReady(s.ctx, t, s.RootShardSystemMasterBaseConfig(t), !cfgs[i].RunInProcess)
			require.NoError(t, err, "kcp server %s never became ready: %v", s.name, err)
		}(srv, i)
	}
	wg.Wait()

	for _, s := range servers {
		scrapeMetricsForServer(t, s)
	}

	if t.Failed() {
		t.Fatal("Fixture setup failed: one or more servers did not become ready")
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), wait.ForeverTestTimeout)
		defer cancel()

		for _, s := range servers {
			gatherMetrics(ctx, t, s, s.artifactDir)
		}
	})

	t.Logf("Started kcp servers after %s", time.Since(start))

	return f
}

// kcpServer exposes a kcp invocation to a test and
// ensures the following semantics:
//   - the server will run only until the test deadline
//   - all ports and data directories are unique to support
//     concurrent execution within a test case and across tests
type kcpServer struct {
	name        string
	args        []string
	ctx         context.Context //nolint:containedctx
	dataDir     string
	artifactDir string
	clientCADir string

	lock           *sync.Mutex
	cfg            clientcmd.ClientConfig
	kubeconfigPath string

	t *testing.T
}

func newKcpServer(t *testing.T, cfg Config, artifactDir, dataDir, clientCADir string) (*kcpServer, error) {
	t.Helper()

	kcpListenPort, err := GetFreePort(t)
	if err != nil {
		return nil, err
	}
	etcdClientPort, err := GetFreePort(t)
	if err != nil {
		return nil, err
	}
	etcdPeerPort, err := GetFreePort(t)
	if err != nil {
		return nil, err
	}
	artifactDir = filepath.Join(artifactDir, "kcp", cfg.Name)
	if err := os.MkdirAll(artifactDir, 0755); err != nil {
		return nil, fmt.Errorf("could not create artifact dir: %w", err)
	}
	dataDir = filepath.Join(dataDir, "kcp", cfg.Name)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("could not create data dir: %w", err)
	}

	return &kcpServer{
		name: cfg.Name,
		args: append([]string{
			"--root-directory",
			dataDir,
			"--secure-port=" + kcpListenPort,
			"--embedded-etcd-client-port=" + etcdClientPort,
			"--embedded-etcd-peer-port=" + etcdPeerPort,
			"--embedded-etcd-wal-size-bytes=" + strconv.Itoa(5*1000), // 5KB
			"--kubeconfig-path=" + filepath.Join(dataDir, "admin.kubeconfig"),
			"--feature-gates=" + fmt.Sprintf("%s", utilfeature.DefaultFeatureGate),
			"--audit-log-path", filepath.Join(artifactDir, "kcp.audit"),
		},
			cfg.Args...),
		dataDir:     dataDir,
		artifactDir: artifactDir,
		clientCADir: clientCADir,
		t:           t,
		lock:        &sync.Mutex{},
	}, nil
}

type runOptions struct {
	runInProcess bool
	streamLogs   bool
}

type RunOption func(o *runOptions)

func RunInProcess(o *runOptions) {
	o.runInProcess = true
}

func WithLogStreaming(o *runOptions) {
	o.streamLogs = true
}

// StartKcpCommand returns the string tokens required to start kcp in
// the currently configured mode (direct or via `go run`).
func StartKcpCommand(identity string) []string {
	command := Command("kcp", identity)
	return append(command, "start")
}

// Command returns the string tokens required to start
// the given executable in the currently configured mode (direct or
// via `go run`).
func Command(executableName, identity string) []string {
	if env.RunDelveEnvSet() {
		cmdPath := filepath.Join(frameworkhelpers.RepositoryDir(), "cmd", executableName)
		return []string{"dlv", "debug", "--api-version=2", "--headless", fmt.Sprintf("--listen=unix:dlv-%s.sock", identity), cmdPath, "--"}
	}
	if env.NoGoRunEnvSet() {
		cmdPath := filepath.Join(frameworkhelpers.RepositoryBinDir(), executableName)
		return []string{cmdPath}
	}
	cmdPath := filepath.Join(frameworkhelpers.RepositoryDir(), "cmd", executableName)
	return []string{"go", "run", cmdPath}
}

// Run runs the kcp server while the parent context is active. This call is not blocking,
// callers should ensure that the server is Ready() before using it.
func (c *kcpServer) Run(opts ...RunOption) error {
	runOpts := runOptions{}
	for _, opt := range opts {
		opt(&runOpts)
	}

	// We close this channel when the kcp server has stopped
	shutdownComplete := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())

	cleanup := func() {
		cancel()
		close(shutdownComplete)
	}

	c.t.Cleanup(func() {
		c.t.Log("cleanup: canceling context")
		cancel()

		// Wait for the kcp server to stop
		c.t.Log("cleanup: waiting for shutdownComplete")

		<-shutdownComplete

		c.t.Log("cleanup: received shutdownComplete")
	})
	c.ctx = ctx

	commandLine := append(StartKcpCommand("KCP"), c.args...)
	c.t.Logf("running: %v", strings.Join(commandLine, " "))

	// run kcp start in-process for easier debugging
	if runOpts.runInProcess {
		rootDir := ".kcp"
		if c.dataDir != "" {
			rootDir = c.dataDir
		}
		serverOptions := kcpoptions.NewOptions(rootDir)
		fss := flag.NamedFlagSets{}
		serverOptions.AddFlags(&fss)
		all := pflag.NewFlagSet("kcp", pflag.ContinueOnError)
		for _, fs := range fss.FlagSets {
			all.AddFlagSet(fs)
		}
		if err := all.Parse(c.args); err != nil {
			cleanup()
			return err
		}

		completed, err := serverOptions.Complete()
		if err != nil {
			cleanup()
			return err
		}
		if errs := completed.Validate(); len(errs) > 0 {
			cleanup()
			return utilerrors.NewAggregate(errs)
		}

		config, err := server.NewConfig(ctx, completed.Server)
		if err != nil {
			cleanup()
			return err
		}

		completedConfig, err := config.Complete()
		if err != nil {
			cleanup()
			return err
		}

		// the etcd server must be up before NewServer because storage decorators access it right away
		if completedConfig.EmbeddedEtcd.Config != nil {
			if err := embeddedetcd.NewServer(completedConfig.EmbeddedEtcd).Run(ctx); err != nil {
				return err
			}
		}

		s, err := server.NewServer(completedConfig)
		if err != nil {
			cleanup()
			return err
		}
		go func() {
			defer cleanup()

			if err := s.Run(ctx); err != nil && ctx.Err() == nil {
				c.t.Errorf("`kcp` failed: %v", err)
			}
		}()

		return nil
	}

	// NOTE: do not use exec.CommandContext here. That method issues a SIGKILL when the context is done, and we
	// want to issue SIGTERM instead, to give the server a chance to shut down cleanly.
	cmd := exec.Command(commandLine[0], commandLine[1:]...)

	// Create a new process group for the child/forked process (which is either 'go run ...' or just 'kcp
	// ...'). This is necessary so the SIGTERM we send to terminate the kcp server works even with the
	// 'go run' variant - we have to work around this issue: https://github.com/golang/go/issues/40467.
	// Thanks to
	// https://medium.com/@felixge/killing-a-child-process-and-all-of-its-children-in-go-54079af94773 for
	// the idea!
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logFile, err := os.Create(filepath.Join(c.artifactDir, "kcp.log"))
	if err != nil {
		cleanup()
		return fmt.Errorf("could not create log file: %w", err)
	}

	// Closing the logfile is necessary so the cmd.Wait() call in the goroutine below can finish (it only finishes
	// waiting when the internal io.Copy goroutines for stdin/stdout/stderr are done, and that doesn't happen if
	// the log file remains open.
	c.t.Cleanup(func() {
		logFile.Close()
	})

	log := bytes.Buffer{}

	writers := []io.Writer{&log, logFile}

	if runOpts.streamLogs {
		prefix := fmt.Sprintf("%s: ", c.name)
		writers = append(writers, prefixer.New(os.Stdout, func() string { return prefix }))
	}

	mw := io.MultiWriter(writers...)
	cmd.Stdout = mw
	cmd.Stderr = mw

	if err := cmd.Start(); err != nil {
		cleanup()
		return err
	}

	c.t.Cleanup(func() {
		// Ensure child process is killed on cleanup - send the negative of the pid, which is the process group id.
		// See https://medium.com/@felixge/killing-a-child-process-and-all-of-its-children-in-go-54079af94773 for details.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			c.t.Errorf("Saw an error trying to kill `kcp`: %v", err)
		}
	})

	go func() {
		defer cleanup()

		err := cmd.Wait()

		if err != nil && ctx.Err() == nil {
			// we care about errors in the process that did not result from the
			// context expiring and us ending the process
			data := c.filterKcpLogs(&log)
			c.t.Errorf("`kcp` failed: %v logs:\n%v", err, data)
			c.t.Errorf("`kcp` failed: %v", err)
		}
	}()

	return nil
}

// filterKcpLogs is a silly hack to get rid of the nonsense output that
// currently plagues kcp. Yes, in the future we want to actually fix these
// issues but until we do, there's no reason to force awful UX onto users.
func (c *kcpServer) filterKcpLogs(logs *bytes.Buffer) string {
	output := strings.Builder{}
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		line := scanner.Bytes()
		ignored := false
		for _, ignore := range [][]byte{
			// TODO: some careful thought on context cancellation might fix the following error
			[]byte(`clientconn.go:1326] [core] grpc: addrConn.createTransport failed to connect to`),
		} {
			if bytes.Contains(line, ignore) {
				ignored = true
				continue
			}
		}
		if ignored {
			continue
		}
		_, err := output.Write(append(line, []byte("\n")...))
		if err != nil {
			c.t.Logf("failed to write log line: %v", err)
		}
	}
	return output.String()
}

// Name exposes the name of this kcp server.
func (c *kcpServer) Name() string {
	return c.name
}

// Name exposes the path of the kubeconfig file of this kcp server.
func (c *kcpServer) KubeconfigPath() string {
	return c.kubeconfigPath
}

// Config exposes a copy of the base client config for this server. Client-side throttling is disabled (QPS=-1).
func (c *kcpServer) config(context string) (*rest.Config, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.cfg == nil {
		return nil, fmt.Errorf("programmer error: kcpServer.Config() called before load succeeded. Stack: %s", string(debug.Stack()))
	}
	raw, err := c.cfg.RawConfig()
	if err != nil {
		return nil, err
	}

	config := clientcmd.NewNonInteractiveClientConfig(raw, context, nil, nil)

	restConfig, err := config.ClientConfig()
	if err != nil {
		return nil, err
	}

	restConfig.QPS = -1

	return restConfig, nil
}

func (c *kcpServer) ClientCAUserConfig(t *testing.T, config *rest.Config, name string, groups ...string) *rest.Config {
	return clientCAUserConfig(t, config, c.clientCADir, name, groups...)
}

// BaseConfig returns a rest.Config for the "base" context. Client-side throttling is disabled (QPS=-1).
func (c *kcpServer) BaseConfig(t *testing.T) *rest.Config {
	t.Helper()

	cfg, err := c.config("base")
	require.NoError(t, err)
	cfg = rest.CopyConfig(cfg)
	return rest.AddUserAgent(cfg, t.Name())
}

// RootShardSystemMasterBaseConfig returns a rest.Config for the "shard-base" context. Client-side throttling is disabled (QPS=-1).
func (c *kcpServer) RootShardSystemMasterBaseConfig(t *testing.T) *rest.Config {
	t.Helper()

	cfg, err := c.config("shard-base")
	require.NoError(t, err)
	cfg = rest.CopyConfig(cfg)

	return rest.AddUserAgent(cfg, t.Name())
}

// ShardSystemMasterBaseConfig returns a rest.Config for the "shard-base" context of a given shard. Client-side throttling is disabled (QPS=-1).
func (c *kcpServer) ShardSystemMasterBaseConfig(t *testing.T, shard string) *rest.Config {
	t.Helper()

	if shard != corev1alpha1.RootShard {
		t.Fatalf("only root shard is supported for now")
	}
	return c.RootShardSystemMasterBaseConfig(t)
}

func (c *kcpServer) ShardNames() []string {
	return []string{corev1alpha1.RootShard}
}

// RawConfig exposes a copy of the client config for this server.
func (c *kcpServer) RawConfig() (clientcmdapi.Config, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.cfg == nil {
		return clientcmdapi.Config{}, fmt.Errorf("programmer error: kcpServer.RawConfig() called before load succeeded. Stack: %s", string(debug.Stack()))
	}
	return c.cfg.RawConfig()
}

func (c *kcpServer) loadCfg() error {
	var lastError error
	if err := wait.PollUntilContextTimeout(c.ctx, 100*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		c.kubeconfigPath = filepath.Join(c.dataDir, "admin.kubeconfig")
		config, err := loadKubeConfig(c.kubeconfigPath, "base")
		if err != nil {
			// A missing file is likely caused by the server not
			// having started up yet. Ignore these errors for the
			// purposes of logging.
			if !os.IsNotExist(err) {
				lastError = err
			}

			return false, nil
		}

		c.lock.Lock()
		c.cfg = config
		c.lock.Unlock()

		return true, nil
	}); err != nil && lastError != nil {
		return fmt.Errorf("failed to load admin kubeconfig: %w", lastError)
	} else if err != nil {
		// should never happen
		return fmt.Errorf("failed to load admin kubeconfig: %w", err)
	}
	return nil
}

func (c *kcpServer) CADirectory() string {
	return c.dataDir
}

func (c *kcpServer) Artifact(t *testing.T, producer func() (runtime.Object, error)) {
	t.Helper()
	artifact(t, c, producer)
}

// artifact registers the data-producing function to run and dump the YAML-formatted output
// to the artifact directory for the test before the kcp process is terminated.
func artifact(t *testing.T, server RunningServer, producer func() (runtime.Object, error)) {
	t.Helper()

	subDir := filepath.Join("artifacts", "kcp", server.Name())
	artifactDir, err := CreateTempDirForTest(t, subDir)
	require.NoError(t, err, "could not create artifacts dir")
	// Using t.Cleanup ensures that artifact collection is local to
	// the test requesting retention regardless of server's scope.
	t.Cleanup(func() {
		data, err := producer()
		require.NoError(t, err, "error fetching artifact")

		accessor, ok := data.(metav1.Object)
		require.True(t, ok, "artifact has no object meta: %#v", data)

		dir := path.Join(artifactDir, logicalcluster.From(accessor).String())
		dir = strings.ReplaceAll(dir, ":", "_") // github actions don't like colon because NTFS is unhappy with it in path names
		if accessor.GetNamespace() != "" {
			dir = path.Join(dir, accessor.GetNamespace())
		}
		err = os.MkdirAll(dir, 0755)
		require.NoError(t, err, "could not create dir")

		gvks, _, err := kubernetesscheme.Scheme.ObjectKinds(data)
		if err != nil {
			gvks, _, err = kcpscheme.Scheme.ObjectKinds(data)
		}
		require.NoError(t, err, "error finding gvk for artifact")
		require.NotEmpty(t, gvks, "found no gvk for artifact: %T", data)
		gvk := gvks[0]
		data.GetObjectKind().SetGroupVersionKind(gvk)

		group := gvk.Group
		if group == "" {
			group = "core"
		}

		gvkForFilename := fmt.Sprintf("%s_%s", group, gvk.Kind)

		file := path.Join(dir, fmt.Sprintf("%s-%s.yaml", gvkForFilename, accessor.GetName()))
		file = strings.ReplaceAll(file, ":", "_") // github actions don't like colon because NTFS is unhappy with it in path names

		bs, err := yaml.Marshal(data)
		require.NoError(t, err, "error marshalling artifact")

		err = os.WriteFile(file, bs, 0644)
		require.NoError(t, err, "error writing artifact")
	})
}
