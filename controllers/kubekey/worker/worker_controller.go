/*
Copyright 2020 The KubeSphere Authors.

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

package worker

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/cluster-api/controllers/remote"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kubekeyv1 "github.com/kubesphere/kubekey/apis/kubekey/v1alpha3"
	"github.com/kubesphere/kubekey/util/predicates"
)

//+kubebuilder:rbac:groups=kubekey.kubesphere.io,resources=workers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kubekey.kubesphere.io,resources=workers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kubekey.kubesphere.io,resources=workers/finalizers,verbs=update

// WorkerReconciler reconciles a Worker object
type WorkerReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	controller controller.Controller
	recorder   record.EventRecorder
	Tracker    *remote.ClusterCacheTracker

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkerReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&kubekeyv1.Worker{}).
		Owns(&kubekeyv1.Machine{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	err = c.Watch(
		&source.Kind{Type: &kubekeyv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(r.ClusterToWorker),
		predicates.All(ctrl.LoggerFrom(ctx),
			predicates.ResourceHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue),
			predicates.ClusterUnpausedAndInfrastructureReady(ctrl.LoggerFrom(ctx)),
		),
	)
	if err != nil {
		return errors.Wrap(err, "failed adding Watch for Clusters to controller manager")
	}

	r.controller = c
	r.recorder = mgr.GetEventRecorderFor("worker-controller")

	return nil
}

func (r *WorkerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	return ctrl.Result{}, nil
}

// ClusterToWorker is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// for worker based on updates to a Cluster.
func (r *WorkerReconciler) ClusterToWorker(o client.Object) []ctrl.Request {
	c, ok := o.(*kubekeyv1.Cluster)
	if !ok {
		panic(fmt.Sprintf("Expected a Cluster but got a %T", o))
	}

	workRefs := c.Spec.WorkerRefs
	for _, worker := range workRefs {
		if worker != nil && worker.Kind == "Worker" {
			return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: worker.Namespace, Name: worker.Name}}}
		}
	}
	return nil
}
