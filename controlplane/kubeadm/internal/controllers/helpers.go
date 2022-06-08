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

package controllers

import (
	"context"
	"encoding/json"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	ctrl "sigs.k8s.io/controller-runtime"

	kubekeyv1 "github.com/kubesphere/kubekey/apis/kubekey/v1alpha3"
	bootstrapv1 "github.com/kubesphere/kubekey/bootstrap/kubeadm/api/v1alpha1"
	controlplanev1 "github.com/kubesphere/kubekey/controlplane/kubeadm/api/v1alpha1"
	"github.com/kubesphere/kubekey/controlplane/kubeadm/internal"
	"github.com/kubesphere/kubekey/util"
	"github.com/kubesphere/kubekey/util/conditions"
	"github.com/kubesphere/kubekey/util/patch"
	"github.com/kubesphere/kubekey/util/secret"
)

func (r *ControlPlaneReconciler) reconcileKubeconfig(ctx context.Context, cluster *kubekeyv1.Cluster, kcp *controlplanev1.ControlPlane) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	endpoint := cluster.Spec.ControlPlaneEndpoint
	if endpoint.IsZero() {
		return ctrl.Result{}, nil
	}

	controllerOwnerRef := *metav1.NewControllerRef(kcp, controlplanev1.GroupVersion.WithKind("ControlPlane"))
	clusterName := util.ObjectKey(cluster)
	configSecret, err := secret.GetFromNamespacedName(ctx, r.Client, clusterName, secret.Kubeconfig)
	switch {
	case apierrors.IsNotFound(err):
		createErr := kubeconfig.CreateSecretWithOwner(
			ctx,
			r.Client,
			clusterName,
			endpoint.String(),
			controllerOwnerRef,
		)
		if errors.Is(createErr, kubeconfig.ErrDependentCertificateNotFound) {
			return ctrl.Result{RequeueAfter: dependentCertRequeueAfter}, nil
		}
		// always return if we have just created in order to skip rotation checks
		return ctrl.Result{}, createErr
	case err != nil:
		return ctrl.Result{}, errors.Wrap(err, "failed to retrieve kubeconfig Secret")
	}

	// check if the kubeconfig secret was created by v1alpha1 controllers, and thus it has the Cluster as the owner instead of KCP;
	// if yes, adopt it.
	if util.IsOwnedByObject(configSecret, cluster) && !util.IsControlledBy(configSecret, kcp) {
		if err := r.adoptKubeconfigSecret(ctx, cluster, configSecret, controllerOwnerRef); err != nil {
			return ctrl.Result{}, err
		}
	}

	// only do rotation on owned secrets
	if !util.IsControlledBy(configSecret, kcp) {
		return ctrl.Result{}, nil
	}

	needsRotation, err := kubeconfig.NeedsClientCertRotation(configSecret, certs.ClientCertificateRenewalDuration)
	if err != nil {
		return ctrl.Result{}, err
	}

	if needsRotation {
		log.Info("rotating kubeconfig secret")
		if err := kubeconfig.RegenerateSecret(ctx, r.Client, configSecret); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to regenerate kubeconfig")
		}
	}

	return ctrl.Result{}, nil
}

func (r *ControlPlaneReconciler) adoptKubeconfigSecret(ctx context.Context, cluster *kubekeyv1.Cluster, configSecret *corev1.Secret, controllerOwnerRef metav1.OwnerReference) error {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Adopting KubeConfig secret created by v1alpha2 controllers", "Name", configSecret.Name)

	p, err := patch.NewHelper(configSecret, r.Client)
	if err != nil {
		return errors.Wrap(err, "failed to create patch helper for the kubeconfig secret")
	}
	configSecret.OwnerReferences = util.RemoveOwnerRef(configSecret.OwnerReferences, metav1.OwnerReference{
		APIVersion: kubekeyv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cluster.Name,
		UID:        cluster.UID,
	})
	configSecret.OwnerReferences = util.EnsureOwnerRef(configSecret.OwnerReferences, controllerOwnerRef)
	if err := p.Patch(ctx, configSecret); err != nil {
		return errors.Wrap(err, "failed to patch the kubeconfig secret")
	}
	return nil
}

func (r *ControlPlaneReconciler) cloneConfigsAndGenerateMachine(ctx context.Context, cluster *kubekeyv1.Cluster, kcp *controlplanev1.ControlPlane, bootstrapSpec *bootstrapv1.KubeadmConfigSpec) error {
	var errs []error

	// Since the cloned resource should eventually have a controller ref for the Machine, we create an
	// OwnerReference here without the Controller field set
	infraCloneOwner := &metav1.OwnerReference{
		APIVersion: controlplanev1.GroupVersion.String(),
		Kind:       "ControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
	}

	// Clone the infrastructure template
	infraRef, err := external.CloneTemplate(ctx, &external.CloneTemplateInput{
		Client:      r.Client,
		TemplateRef: &kcp.Spec.Machines.InfrastructureRef,
		Namespace:   kcp.Namespace,
		OwnerRef:    infraCloneOwner,
		ClusterName: cluster.Name,
		Labels:      internal.ControlPlaneMachineLabelsForCluster(kcp, cluster.Name),
		Annotations: kcp.Spec.Machines.ObjectMeta.Annotations,
	})
	if err != nil {
		// Safe to return early here since no resources have been created yet.
		conditions.MarkFalse(kcp, controlplanev1.MachinesCreatedCondition, controlplanev1.InfrastructureTemplateCloningFailedReason,
			kubekeyv1.ConditionSeverityError, err.Error())
		return errors.Wrap(err, "failed to clone infrastructure template")
	}

	// Clone the bootstrap configuration
	bootstrapRef, err := r.generateKubeadmConfig(ctx, kcp, cluster, bootstrapSpec)
	if err != nil {
		conditions.MarkFalse(kcp, controlplanev1.MachinesCreatedCondition, controlplanev1.BootstrapTemplateCloningFailedReason,
			kubekeyv1.ConditionSeverityError, err.Error())
		errs = append(errs, errors.Wrap(err, "failed to generate bootstrap config"))
	}

	// Only proceed to generating the Machine if we haven't encountered an error
	if len(errs) == 0 {
		if err := r.generateMachine(ctx, kcp, cluster, infraRef, bootstrapRef); err != nil {
			conditions.MarkFalse(kcp, controlplanev1.MachinesCreatedCondition, controlplanev1.MachineGenerationFailedReason,
				kubekeyv1.ConditionSeverityError, err.Error())
			errs = append(errs, errors.Wrap(err, "failed to create Machine"))
		}
	}

	return nil
}

func (r *ControlPlaneReconciler) generateKubeadmConfig(ctx context.Context, kcp *controlplanev1.ControlPlane, cluster *kubekeyv1.Cluster, spec *bootstrapv1.KubeadmConfigSpec) (*corev1.ObjectReference, error) {
	// Create an owner reference without a controller reference because the owning controller is the machine controller
	owner := metav1.OwnerReference{
		APIVersion: controlplanev1.GroupVersion.String(),
		Kind:       "ControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
	}

	bootstrapConfig := &bootstrapv1.KubeadmConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:            names.SimpleNameGenerator.GenerateName(kcp.Name + "-"),
			Namespace:       kcp.Namespace,
			Labels:          internal.ControlPlaneMachineLabelsForCluster(kcp, cluster.Name),
			Annotations:     kcp.Spec.Machines.ObjectMeta.Annotations,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: *spec,
	}

	if err := r.Client.Create(ctx, bootstrapConfig); err != nil {
		return nil, errors.Wrap(err, "Failed to create bootstrap configuration")
	}

	bootstrapRef := &corev1.ObjectReference{
		APIVersion: bootstrapv1.GroupVersion.String(),
		Kind:       "KubeadmConfig",
		Name:       bootstrapConfig.GetName(),
		Namespace:  bootstrapConfig.GetNamespace(),
		UID:        bootstrapConfig.GetUID(),
	}

	return bootstrapRef, nil
}

func (r *ControlPlaneReconciler) generateMachine(ctx context.Context, kcp *controlplanev1.ControlPlane, cluster *kubekeyv1.Cluster, infraRef, bootstrapRef *corev1.ObjectReference) error {
	machine := &kubekeyv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        names.SimpleNameGenerator.GenerateName(kcp.Name + "-"),
			Namespace:   kcp.Namespace,
			Labels:      internal.ControlPlaneMachineLabelsForCluster(kcp, cluster.Name),
			Annotations: map[string]string{},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kcp, controlplanev1.GroupVersion.WithKind("ControlPlane")),
			},
		},
		Spec: kubekeyv1.MachineSpec{
			ClusterName: cluster.Name,
			Version:     &kcp.Spec.Version,
			Bootstrap: kubekeyv1.Bootstrap{
				ConfigRef: bootstrapRef,
			},
			NodeDrainTimeout: kcp.Spec.Machines.NodeDrainTimeout,
		},
	}

	// Machine's bootstrap config may be missing ClusterConfiguration if it is not the first machine in the control plane.
	// We store ClusterConfiguration as annotation here to detect any changes in KCP ClusterConfiguration and rollout the machine if any.
	clusterConfig, err := json.Marshal(kcp.Spec.KubeadmConfigSpec.ClusterConfiguration)
	if err != nil {
		return errors.Wrap(err, "failed to marshal cluster configuration")
	}

	// Add the annotations from the MachineTemplate.
	// Note: we intentionally don't use the map directly to ensure we don't modify the map in KCP.
	for k, v := range kcp.Spec.Machines.ObjectMeta.Annotations {
		machine.Annotations[k] = v
	}
	machine.Annotations[controlplanev1.KubeadmClusterConfigurationAnnotation] = string(clusterConfig)

	if err := r.Client.Create(ctx, machine); err != nil {
		return errors.Wrap(err, "failed to create machine")
	}
	return nil
}
