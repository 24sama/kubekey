/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	infrav1 "github.com/kubesphere/kubekey/exp/cluster-api-provider-kubekey/api/v1beta1"
	"github.com/kubesphere/kubekey/exp/cluster-api-provider-kubekey/test/helpers/envtest"
)

const (
	timeout = time.Second * 30
)

var (
	env        *envtest.Environment
	ctx        = ctrl.SetupSignalHandler()
	fakeScheme = runtime.NewScheme()
)

func init() {
	_ = clientgoscheme.AddToScheme(fakeScheme)
	_ = clusterv1.AddToScheme(fakeScheme)
	_ = infrav1.AddToScheme(fakeScheme)
}

func TestMain(m *testing.M) {
	setupReconcilers := func(ctx context.Context, mgr ctrl.Manager) {
		log := ctrl.Log.WithName("remote").WithName("ClusterCacheTracker")
		tracker, err := remote.NewClusterCacheTracker(
			mgr,
			remote.ClusterCacheTrackerOptions{
				Log:     &log,
				Indexes: remote.DefaultIndexes,
			},
		)
		if err != nil {
			panic(fmt.Sprintf("unable to create cluster cache tracker: %v", err))
		}

		if err := (&KKClusterReconciler{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorderFor("kkcluster-controller"),
			Scheme:   mgr.GetScheme(),
		}).SetupWithManager(ctx, mgr, controller.Options{MaxConcurrentReconciles: 1}); err != nil {
			panic(fmt.Sprintf("Failed to start ClusterReconciler: %v", err))
		}
		if err := (&KKMachineReconciler{
			Client:   mgr.GetClient(),
			Recorder: mgr.GetEventRecorderFor("kkmachine-controller"),
			Scheme:   mgr.GetScheme(),
			Tracker:  tracker,
		}).SetupWithManager(ctx, mgr, controller.Options{MaxConcurrentReconciles: 1}); err != nil {
			panic(fmt.Sprintf("Failed to start MachineReconciler: %v", err))
		}
	}

	SetDefaultEventuallyPollingInterval(100 * time.Millisecond)
	SetDefaultEventuallyTimeout(timeout)

	os.Exit(envtest.Run(ctx, envtest.RunInput{
		M:                m,
		SetupEnv:         func(e *envtest.Environment) { env = e },
		SetupReconcilers: setupReconcilers,
	}))
}
