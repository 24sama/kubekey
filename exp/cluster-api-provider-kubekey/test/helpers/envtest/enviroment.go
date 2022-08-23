/*
 Copyright 2022 The KubeSphere Authors.

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

package envtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/version"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	infrav1 "github.com/kubesphere/kubekey/exp/cluster-api-provider-kubekey/api/v1beta1"
	"github.com/kubesphere/kubekey/exp/cluster-api-provider-kubekey/test/helpers/builder"
)

func init() {
	klog.InitFlags(nil)
	logger := klogr.New()
	// Additionally force all of the controllers to use the Ginkgo logger.
	ctrl.SetLogger(logger)
	// Add logger for ginkgo.
	klog.SetOutput(ginkgo.GinkgoWriter)

	// Calculate the scheme.
	utilruntime.Must(clusterv1.AddToScheme(scheme.Scheme))
	utilruntime.Must(bootstrapv1.AddToScheme(scheme.Scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme.Scheme))
}

// RunInput is the input for Run.
type RunInput struct {
	M                   *testing.M
	ManagerUncachedObjs []client.Object
	SetupIndexes        func(ctx context.Context, mgr ctrl.Manager)
	SetupReconcilers    func(ctx context.Context, mgr ctrl.Manager)
	SetupEnv            func(e *Environment)
	MinK8sVersion       string
}

// Run executes the tests of the given testing.M in a test environment.
// Note: The environment will be created in this func and should not be created before. This func takes a *Environment
//
//	because our tests require access to the *Environment. We use this field to make the created Environment available
//	to the consumer.
//
// Note: Test environment creation can be skipped by setting the environment variable `CAPI_DISABLE_TEST_ENV`. This only
//
//	makes sense when executing tests which don't require the test environment, e.g. tests using only the fake client.
func Run(ctx context.Context, input RunInput) int {
	if os.Getenv("CAPKK_DISABLE_TEST_ENV") != "" {
		return input.M.Run()
	}

	// Bootstrapping test environment
	env := newEnvironment(input.ManagerUncachedObjs...)

	if input.SetupIndexes != nil {
		input.SetupIndexes(ctx, env.Manager)
	}
	if input.SetupReconcilers != nil {
		input.SetupReconcilers(ctx, env.Manager)
	}

	// Start the environment.
	env.start(ctx)

	if input.MinK8sVersion != "" {
		if err := version.CheckKubernetesVersion(env.Config, input.MinK8sVersion); err != nil {
			fmt.Printf("[IMPORTANT] skipping tests after failing version check: %v\n", err)
			if err := env.stop(); err != nil {
				fmt.Println("[WARNING] Failed to stop the test environment")
			}
			return 0
		}
	}

	// Expose the environment.
	input.SetupEnv(env)

	// Run tests
	code := input.M.Run()

	// Tearing down the test environment
	if err := env.stop(); err != nil {
		panic(fmt.Sprintf("Failed to stop the test environment: %v", err))
	}
	return code
}

// Environment encapsulates a Kubernetes local test environment.
type Environment struct {
	manager.Manager
	client.Client
	Config *rest.Config

	env           *envtest.Environment
	cancelManager context.CancelFunc
}

func newEnvironment(uncachedObjs ...client.Object) *Environment {
	// Get the root of the current file to use in CRD paths.
	_, filename, _, _ := goruntime.Caller(0) //nolint:dogsled
	root := path.Join(path.Dir(filename), "..", "..", "..")

	// Create the test environment.
	env := &envtest.Environment{
		ErrorIfCRDPathMissing: true,
		CRDDirectoryPaths: []string{
			filepath.Join(root, "config", "crd", "bases"),
		},
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			builder.TestClusterCRD.DeepCopy(),
			builder.TestMachineCRD.DeepCopy(),
		},
		// initialize webhook here to be able to test the envtest install via webhookOptions
		// This should set LocalServingCertDir and LocalServingPort that are used below.
		WebhookInstallOptions: initWebhookInstallOptions(),
	}

	if _, err := env.Start(); err != nil {
		err = kerrors.NewAggregate([]error{err, env.Stop()})
		panic(err)
	}

	objs := []client.Object{}
	if len(uncachedObjs) > 0 {
		objs = append(objs, uncachedObjs...)
	}

	// Localhost is used on MacOS to avoid Firewall warning popups.
	host := "localhost"
	if strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
		// 0.0.0.0 is required on Linux when using kind because otherwise the kube-apiserver running in kind
		// is unable to reach the webhook, because the webhook would be only listening on 127.0.0.1.
		// Somehow that's not an issue on MacOS.
		if goruntime.GOOS == "linux" {
			host = "0.0.0.0"
		}
	}

	options := manager.Options{
		Scheme:                scheme.Scheme,
		MetricsBindAddress:    "0",
		CertDir:               env.WebhookInstallOptions.LocalServingCertDir,
		Port:                  env.WebhookInstallOptions.LocalServingPort,
		ClientDisableCacheFor: objs,
		Host:                  host,
	}

	mgr, err := ctrl.NewManager(env.Config, options)
	if err != nil {
		klog.Fatalf("Failed to start testenv manager: %v", err)
	}

	// Set minNodeStartupTimeout for Test, so it does not need to be at least 30s
	clusterv1.SetMinNodeStartupTimeout(metav1.Duration{Duration: 1 * time.Millisecond})

	if err := (&infrav1.KKCluster{}).SetupWebhookWithManager(mgr); err != nil {
		klog.Fatalf("unable to create webhook: %+v", err)
	}

	return &Environment{
		Manager: mgr,
		Client:  mgr.GetClient(),
		Config:  mgr.GetConfig(),
		env:     env,
	}
}

// start starts the manager.
func (e *Environment) start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	e.cancelManager = cancel

	go func() {
		fmt.Println("Starting the test environment manager")
		if err := e.Manager.Start(ctx); err != nil {
			panic(fmt.Sprintf("Failed to start the test environment manager: %v", err))
		}
	}()
	<-e.Manager.Elected()
	e.waitForWebhooks()
}

// stop stops the test environment.
func (e *Environment) stop() error {
	fmt.Println("Stopping the test environment")
	e.cancelManager()
	return e.env.Stop()
}

// waitForWebhooks waits for the webhook server to be available.
func (e *Environment) waitForWebhooks() {
	port := e.env.WebhookInstallOptions.LocalServingPort

	klog.V(2).Infof("Waiting for webhook port %d to be open prior to running tests", port)
	timeout := 1 * time.Second
	for {
		time.Sleep(1 * time.Second)
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), timeout)
		if err != nil {
			klog.V(2).Infof("Webhook port is not ready, will retry in %v: %s", timeout, err)
			continue
		}
		if err := conn.Close(); err != nil {
			klog.V(2).Infof("Closing connection when testing if webhook port is ready failed: %v", err)
		}
		klog.V(2).Info("Webhook port is now open. Continuing with tests...")
		return
	}
}
