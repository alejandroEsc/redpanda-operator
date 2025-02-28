// Copyright 2021 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

package redpanda

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"time"

	helmv2beta1 "github.com/fluxcd/helm-controller/api/v2beta1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/logger"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/go-logr/logr"
	consolepkg "github.com/redpanda-data/redpanda-operator/src/go/k8s/pkg/console"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kuberecorder "k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	v2 "sigs.k8s.io/controller-runtime/pkg/webhook/conversion/testdata/api/v2"

	"github.com/redpanda-data/redpanda-operator/src/go/k8s/api/redpanda/v1alpha1"
	vectorzied_v1alpha1 "github.com/redpanda-data/redpanda-operator/src/go/k8s/api/vectorized/v1alpha1"
)

const (
	resourceReadyStrFmt    = "%s '%s/%s' is ready"
	resourceNotReadyStrFmt = "%s '%s/%s' is not ready"

	resourceTypeHelmRepository = "HelmRepository"
	resourceTypeHelmRelease    = "HelmRelease"

	managedPath = "/managed"
)

// RedpandaReconciler reconciles a Redpanda object
type RedpandaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	kuberecorder.EventRecorder

	RequeueHelmDeps time.Duration
}

// flux resources main resources
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,namespace=default,resources=helmreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,namespace=default,resources=helmreleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,namespace=default,resources=helmreleases/finalizers,verbs=update
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=helmcharts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=helmcharts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=helmcharts/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=helmrepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=helmrepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=helmrepositories/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=gitrepository,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=gitrepository/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=gitrepository/finalizers,verbs=get;create;update;patch;delete

// flux additional resources
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=buckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,namespace=default,resources=gitrepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,namespace=default,resources=replicasets,verbs=get;list;watch;create;update;patch;delete

// any resource that Redpanda helm creates and flux controller needs to reconcile them
// +kubebuilder:rbac:groups="",namespace=default,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,namespace=default,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,namespace=default,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,namespace=default,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,namespace=default,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,namespace=default,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,namespace=default,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,namespace=default,resources=statefulsets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,namespace=default,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,namespace=default,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,namespace=default,resources=certificates,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,namespace=default,resources=issuers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="monitoring.coreos.com",namespace=default,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,namespace=default,resources=ingresses,verbs=get;list;watch;create;update;patch;delete

// for the migration purposes to disable reconciliation of cluster and console custom resources
// +kubebuilder:rbac:groups=redpanda.vectorized.io,resources=clusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=redpanda.vectorized.io,resources=consoles,verbs=get;list;watch;update;patch

// redpanda resources
// +kubebuilder:rbac:groups=cluster.redpanda.com,namespace=default,resources=redpandas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.redpanda.com,namespace=default,resources=redpandas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.redpanda.com,namespace=default,resources=redpandas/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,namespace=default,resources=events,verbs=create;patch

// SetupWithManager sets up the controller with the Manager.
func (r *RedpandaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Redpanda{}).
		Owns(&helmv2beta1.HelmRelease{}).
		Complete(r)
}

func (r *RedpandaReconciler) Reconcile(c context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, done := context.WithCancel(c)
	defer done()

	start := time.Now()
	log := ctrl.LoggerFrom(ctx).WithName("RedpandaReconciler.Reconcile")

	log.Info("Starting reconcile loop")

	rp := &v1alpha1.Redpanda{}
	if err := r.Client.Get(ctx, req.NamespacedName, rp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Examine if the object is under deletion
	if !rp.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, rp)
	}

	if !isRedpandaManaged(ctx, rp) {
		if controllerutil.ContainsFinalizer(rp, FinalizerKey) {
			// if no longer managed by us, attempt to remove the finalizer
			controllerutil.RemoveFinalizer(rp, FinalizerKey)
			if err := r.Client.Update(ctx, rp); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	// add finalizer if not exist
	if !controllerutil.ContainsFinalizer(rp, FinalizerKey) {
		patch := client.MergeFrom(rp.DeepCopy())
		controllerutil.AddFinalizer(rp, FinalizerKey)
		if err := r.Patch(ctx, rp, patch); err != nil {
			log.Error(err, "unable to register finalizer")
			return ctrl.Result{}, err
		}
	}

	if rp.Spec.Migration != nil && rp.Spec.Migration.Enabled {
		if err := r.tryMigration(ctx, log, rp); err != nil {
			log.Error(err, "migration")
		}
	}

	rp, result, err := r.reconcile(ctx, rp)

	// Update status after reconciliation.
	if updateStatusErr := r.patchRedpandaStatus(ctx, rp); updateStatusErr != nil {
		log.Error(updateStatusErr, "unable to update status after reconciliation")
		return ctrl.Result{Requeue: true}, updateStatusErr
	}

	// Log reconciliation duration
	durationMsg := fmt.Sprintf("reconciliation finished in %s", time.Since(start).String())
	if result.RequeueAfter > 0 {
		durationMsg = fmt.Sprintf("%s, next run in %s", durationMsg, result.RequeueAfter.String())
	}
	log.Info(durationMsg)

	return result, err
}

func (r *RedpandaReconciler) tryMigration(ctx context.Context, log logr.Logger, rp *v1alpha1.Redpanda) error {
	log = log.WithName("tryMigration")
	var errorResult error

	var cluster vectorzied_v1alpha1.Cluster
	namespace := rp.Spec.Migration.ClusterRef.Namespace
	if namespace == "" {
		namespace = rp.Namespace
	}
	name := rp.Spec.Migration.ClusterRef.Name
	if name == "" {
		name = rp.Name
	}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, &cluster)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("get cluster reference (%s/%s): %w", namespace, name, err), errorResult)
	} else if isRedpandaClusterManaged(log, &cluster) {
		annotatedCluster := cluster.DeepCopy()
		disableRedpandaReconciliation(annotatedCluster)

		err = r.Update(ctx, annotatedCluster)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("disabling Cluster reconciliation (%s): %w", annotatedCluster.Name, err), errorResult)
		}

		msg := "update Cluster custom resource"
		log.V(logger.DebugLevel).Info(msg, "cluster-name", annotatedCluster.Name, "annotations", annotatedCluster.Annotations, "finalizers", annotatedCluster.Finalizers)
		r.EventRecorder.AnnotatedEventf(annotatedCluster, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	var console vectorzied_v1alpha1.Console
	namespace = rp.Spec.Migration.ConsoleRef.Namespace
	if namespace == "" {
		namespace = rp.Namespace
	}
	name = rp.Spec.Migration.ConsoleRef.Name
	if name == "" {
		name = rp.Name
	}
	err = r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, &console)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("get cluster reference (%s/%s): %w", namespace, name, err), errorResult)
	} else if isConsoleManaged(log, &console) ||
		controllerutil.ContainsFinalizer(&console, consolepkg.ConsoleSAFinalizer) ||
		controllerutil.ContainsFinalizer(&console, consolepkg.ConsoleACLFinalizer) {

		annotatedConsole := console.DeepCopy()
		disableConsoleReconciliation(annotatedConsole)
		controllerutil.RemoveFinalizer(annotatedConsole, consolepkg.ConsoleSAFinalizer)
		controllerutil.RemoveFinalizer(annotatedConsole, consolepkg.ConsoleACLFinalizer)

		err = r.Update(ctx, annotatedConsole)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("disabling Cluster reconciliation (%s): %w", annotatedConsole.Name, err), errorResult)
		}

		msg := "update Console custom resource"
		log.V(logger.DebugLevel).Info(msg, "console-name", annotatedConsole.Name, "annotations", annotatedConsole.Annotations, "finalizers", annotatedConsole.Finalizers)
		r.EventRecorder.AnnotatedEventf(annotatedConsole, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	var pl v1.PodList
	err = r.List(ctx, &pl, []client.ListOption{
		client.InNamespace(rp.Namespace),
		client.MatchingLabels(map[string]string{"app.kubernetes.io/instance": rp.Name, "app.kubernetes.io/name": "redpanda"}),
	}...)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("listing pods: %w", err), errorResult)
	}

	for i := range pl.Items {
		if l, exist := pl.Items[i].Labels["app.kubernetes.io/component"]; exist && l == "redpanda-statefulset" && !controllerutil.ContainsFinalizer(&pl.Items[i], FinalizerKey) {
			continue
		}
		newPod := pl.Items[i].DeepCopy()
		if newPod.Labels == nil {
			newPod.Labels = make(map[string]string)
		}
		newPod.Labels["app.kubernetes.io/component"] = "redpanda-statefulset"

		controllerutil.RemoveFinalizer(newPod, FinalizerKey)

		err = r.Update(ctx, newPod)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("updating component Pod label (%s): %w", newPod.Name, err), errorResult)
		}

		msg := "update Redpanda Pod"
		log.V(logger.DebugLevel).Info(msg, "pod-name", newPod.Name, "labels", newPod.Labels)
		r.EventRecorder.AnnotatedEventf(newPod, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	resourcesName := rp.Name
	if rp.Spec.ClusterSpec.FullNameOverride != "" {
		resourcesName = rp.Spec.ClusterSpec.FullNameOverride
	}

	var svc v1.Service
	err = r.Get(ctx, types.NamespacedName{
		Namespace: rp.Namespace,
		Name:      resourcesName,
	}, &svc)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("get internal service (%s): %w", resourcesName, err), errorResult)
	} else if !hasLabelsAndAnnotations(&svc, rp) || !maps.Equal(svc.Spec.Selector, map[string]string{
		"app.kubernetes.io/instance": rp.Name,
		"app.kubernetes.io/name":     "redpanda",
	}) {
		internalService := svc.DeepCopy()
		setHelmLabelsAndAnnotations(internalService, rp)

		internalService.Spec.Selector = make(map[string]string)
		internalService.Spec.Selector["app.kubernetes.io/instance"] = rp.Name
		internalService.Spec.Selector["app.kubernetes.io/name"] = "redpanda"

		err = r.Update(ctx, internalService)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("updating internal service (%s): %w", internalService.Name, err), errorResult)
		}

		msg := "update internal Service"
		log.V(logger.DebugLevel).Info(msg, "service-name", internalService.Name, "labels", internalService.Labels, "annotations", internalService.Annotations, "selector", internalService.Spec.Selector)
		r.EventRecorder.AnnotatedEventf(internalService, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	externalSVCName := fmt.Sprintf("%s-external", resourcesName)
	err = r.Get(ctx, types.NamespacedName{
		Namespace: rp.Namespace,
		Name:      externalSVCName,
	}, &svc)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("get external service (%s): %w", externalSVCName, err), errorResult)
	} else if !hasLabelsAndAnnotations(&svc, rp) {
		externalService := svc.DeepCopy()
		setHelmLabelsAndAnnotations(externalService, rp)

		err = r.Update(ctx, externalService)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("updating external service (%s): %w", externalService.Name, err), errorResult)
		}

		msg := "update external Service"
		log.V(logger.DebugLevel).Info(msg, "service-account-name", externalService.Name, "labels", externalService.Labels, "annotations", externalService.Annotations)
		r.EventRecorder.AnnotatedEventf(externalService, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	var sa v1.ServiceAccount
	err = r.Get(ctx, types.NamespacedName{
		Namespace: rp.Namespace,
		Name:      resourcesName,
	}, &sa)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("get service account (%s): %w", resourcesName, err), errorResult)
	} else if !hasLabelsAndAnnotations(&sa, rp) {
		annotatedSA := sa.DeepCopy()
		setHelmLabelsAndAnnotations(annotatedSA, rp)

		err = r.Update(ctx, annotatedSA)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("updating service account (%s): %w", annotatedSA.Name, err), errorResult)
		}

		msg := "update ServiceAccount"
		log.V(logger.DebugLevel).Info(msg, "service-account-name", annotatedSA.Name, "labels", annotatedSA.Labels, "annotations", annotatedSA.Annotations)
		r.EventRecorder.AnnotatedEventf(annotatedSA, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	var pdb policyv1.PodDisruptionBudget
	err = r.Get(ctx, types.NamespacedName{
		Namespace: rp.Namespace,
		Name:      resourcesName,
	}, &pdb)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("get pod disruption budget (%s): %w", resourcesName, err), errorResult)
	} else if !hasLabelsAndAnnotations(&pdb, rp) {
		annotatedPDB := pdb.DeepCopy()
		setHelmLabelsAndAnnotations(annotatedPDB, rp)

		err = r.Update(ctx, annotatedPDB)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("updating pod disruption budget (%s): %w", annotatedPDB.Name, err), errorResult)
		}

		msg := "update PodDistributionBudget"
		log.V(logger.DebugLevel).Info(msg, "pod-distribution-budget-name", annotatedPDB.Name, "labels", annotatedPDB.Labels, "annotations", annotatedPDB.Annotations)
		r.EventRecorder.AnnotatedEventf(annotatedPDB, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	var sts appsv1.StatefulSet
	err = r.Get(ctx, types.NamespacedName{
		Namespace: rp.Namespace,
		Name:      resourcesName,
	}, &sts)
	if err != nil {
		errorResult = errors.Join(fmt.Errorf("get statefulset (%s): %w", resourcesName, err), errorResult)
	} else if !hasLabelsAndAnnotations(&sts, rp) {
		orphan := metav1.DeletePropagationOrphan
		err = r.Delete(ctx, &sts, &client.DeleteOptions{
			PropagationPolicy: &orphan,
		})
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("deleting statefulset (%s): %w", sts.Name, err), errorResult)
		}

		msg := "delete StatefulSet with orphant propagation mode"
		log.V(logger.DebugLevel).Info(msg, "stateful-set-name", sts.Name)
		r.EventRecorder.AnnotatedEventf(&sts, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
	}

	if ptr.Deref(rp.Spec.ClusterSpec.Console.Enabled, true) {
		log.V(logger.DebugLevel).Info("migrate console")
		consoleResourcesName := rp.Name
		if overwriteSAName := ptr.Deref(rp.Spec.ClusterSpec.Console.FullNameOverride, ""); overwriteSAName != "" {
			consoleResourcesName = overwriteSAName
		}
		err = r.Get(ctx, types.NamespacedName{
			Namespace: rp.Namespace,
			Name:      consoleResourcesName,
		}, &sa)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("get console service account (%s): %w", consoleResourcesName, err), errorResult)
		} else if !hasLabelsAndAnnotations(&sa, rp) {
			annotatedConsoleSA := sa.DeepCopy()
			setHelmLabelsAndAnnotations(annotatedConsoleSA, rp)

			err = r.Update(ctx, annotatedConsoleSA)
			if err != nil {
				errorResult = errors.Join(fmt.Errorf("updating console service account (%s): %w", annotatedConsoleSA.Name, err), errorResult)
			}

			msg := "update console ServiceAccount"
			log.V(logger.DebugLevel).Info(msg, "service-account-name", annotatedConsoleSA.Name, "labels", annotatedConsoleSA.Labels, "annotations", annotatedConsoleSA.Annotations)
			r.EventRecorder.AnnotatedEventf(annotatedConsoleSA, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
		}

		err = r.Get(ctx, types.NamespacedName{
			Namespace: rp.Namespace,
			Name:      consoleResourcesName,
		}, &svc)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("get console service (%s): %w", consoleResourcesName, err), errorResult)
		} else if !hasLabelsAndAnnotations(&svc, rp) || !maps.Equal(svc.Spec.Selector, map[string]string{
			"app.kubernetes.io/instance": rp.Name,
			"app.kubernetes.io/name":     "console",
		}) {
			annotatedConsoleSVC := svc.DeepCopy()
			setHelmLabelsAndAnnotations(annotatedConsoleSVC, rp)

			annotatedConsoleSVC.Spec.Selector = make(map[string]string)
			annotatedConsoleSVC.Spec.Selector["app.kubernetes.io/instance"] = rp.Name
			annotatedConsoleSVC.Spec.Selector["app.kubernetes.io/name"] = "console"

			err = r.Update(ctx, annotatedConsoleSVC)
			if err != nil {
				errorResult = errors.Join(fmt.Errorf("updating console service (%s): %w", annotatedConsoleSVC.Name, err), errorResult)
			}

			msg := "update console Service"
			log.V(logger.DebugLevel).Info(msg, "service-name", annotatedConsoleSVC.Name, "labels", annotatedConsoleSVC.Labels, "annotations", annotatedConsoleSVC.Annotations, "selector", annotatedConsoleSVC.Spec.Selector)
			r.EventRecorder.AnnotatedEventf(annotatedConsoleSVC, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
		}

		var deploy appsv1.Deployment
		err = r.Get(ctx, types.NamespacedName{
			Namespace: rp.Namespace,
			Name:      consoleResourcesName,
		}, &deploy)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("get console deployment (%s): %w", consoleResourcesName, err), errorResult)
		} else if !hasLabelsAndAnnotations(&sts, rp) {
			err = r.Delete(ctx, &deploy)
			if err != nil {
				errorResult = errors.Join(fmt.Errorf("deleting console deployment (%s): %w", deploy.Name, err), errorResult)
			}

			msg := "delete console Deployment"
			log.V(logger.DebugLevel).Info(msg, "deployment-name", deploy.Name)
			r.EventRecorder.AnnotatedEventf(&deploy, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
		}

		var ing networkingv1.Ingress
		err = r.Get(ctx, types.NamespacedName{
			Namespace: rp.Namespace,
			Name:      consoleResourcesName,
		}, &ing)
		if err != nil {
			errorResult = errors.Join(fmt.Errorf("get console ingress (%s): %w", consoleResourcesName, err), errorResult)
		} else if !hasLabelsAndAnnotations(&ing, rp) {
			annotatedIngress := ing.DeepCopy()
			setHelmLabelsAndAnnotations(annotatedIngress, rp)

			err = r.Update(ctx, annotatedIngress)
			if err != nil {
				errorResult = errors.Join(fmt.Errorf("updating console ingress (%s): %w", annotatedIngress.Name, err), errorResult)
			}

			msg := "update console Ingress"
			log.V(logger.DebugLevel).Info(msg, "ingress-name", annotatedIngress.Name, "labels", annotatedIngress.Labels, "annotations", annotatedIngress.Annotations)
			r.EventRecorder.AnnotatedEventf(annotatedIngress, map[string]string{v2.GroupVersion.Group + "/revision": rp.Status.LastAttemptedRevision}, "Normal", v1alpha1.EventSeverityInfo, msg)
		}
	}
	return errorResult
}

func hasLabelsAndAnnotations(object client.Object, rp *v1alpha1.Redpanda) bool {
	manageByLabel := false
	releaseName := false
	releaseNamespace := false
	for k, v := range object.GetLabels() {
		if k == "app.kubernetes.io/managed-by" && v == helm {
			manageByLabel = true
		}
	}

	for k, v := range object.GetAnnotations() {
		switch k {
		case "meta.helm.sh/release-name":
			releaseName = v == rp.Name
		case "meta.helm.sh/release-namespace":
			releaseNamespace = v == rp.Namespace
		}
	}

	return manageByLabel && releaseName && releaseNamespace
}

const helm = "Helm"

func setHelmLabelsAndAnnotations(object client.Object, rp *v1alpha1.Redpanda) {
	labels := make(map[string]string)
	labels["app.kubernetes.io/managed-by"] = helm
	object.SetLabels(labels)

	annotations := make(map[string]string)
	annotations["meta.helm.sh/release-name"] = rp.Name
	annotations["meta.helm.sh/release-namespace"] = rp.Namespace
	object.SetAnnotations(annotations)
}

func (r *RedpandaReconciler) reconcile(ctx context.Context, rp *v1alpha1.Redpanda) (*v1alpha1.Redpanda, ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.WithName("RedpandaReconciler.reconcile")

	// Observe HelmRelease generation.
	if rp.Status.ObservedGeneration != rp.Generation {
		rp.Status.ObservedGeneration = rp.Generation
		rp = v1alpha1.RedpandaProgressing(rp)
		if updateStatusErr := r.patchRedpandaStatus(ctx, rp); updateStatusErr != nil {
			log.Error(updateStatusErr, "unable to update status after generation update")
			return rp, ctrl.Result{Requeue: true}, updateStatusErr
		}
	}

	// Check if HelmRepository exists or create it
	rp, repo, err := r.reconcileHelmRepository(ctx, rp)
	if err != nil {
		return rp, ctrl.Result{}, err
	}

	isGenerationCurrent := repo.Generation != repo.Status.ObservedGeneration
	isStatusConditionReady := apimeta.IsStatusConditionTrue(repo.Status.Conditions, meta.ReadyCondition)
	msgNotReady := fmt.Sprintf(resourceNotReadyStrFmt, resourceTypeHelmRepository, repo.GetNamespace(), repo.GetName())
	msgReady := fmt.Sprintf(resourceReadyStrFmt, resourceTypeHelmRepository, repo.GetNamespace(), repo.GetName())
	isStatusReadyNILorTRUE := ptr.Equal(rp.Status.HelmRepositoryReady, ptr.To(true))
	isStatusReadyNILorFALSE := ptr.Equal(rp.Status.HelmRepositoryReady, ptr.To(false))

	isResourceReady := r.checkIfResourceIsReady(log, msgNotReady, msgReady, resourceTypeHelmRepository, isGenerationCurrent, isStatusConditionReady, isStatusReadyNILorTRUE, isStatusReadyNILorFALSE, rp)
	if !isResourceReady {
		// need to requeue in this case
		return v1alpha1.RedpandaNotReady(rp, "ArtifactFailed", msgNotReady), ctrl.Result{RequeueAfter: r.RequeueHelmDeps}, nil
	}

	// Check if HelmRelease exists or create it also
	rp, hr, err := r.reconcileHelmRelease(ctx, rp)
	if err != nil {
		return rp, ctrl.Result{}, err
	}
	if hr.Name == "" {
		log.Info(fmt.Sprintf("Created HelmRelease for '%s/%s', will requeue", rp.Namespace, rp.Name))
		return rp, ctrl.Result{}, err
	}

	isGenerationCurrent = hr.Generation != hr.Status.ObservedGeneration
	isStatusConditionReady = apimeta.IsStatusConditionTrue(hr.Status.Conditions, meta.ReadyCondition)
	msgNotReady = fmt.Sprintf(resourceNotReadyStrFmt, resourceTypeHelmRelease, hr.GetNamespace(), hr.GetName())
	msgReady = fmt.Sprintf(resourceReadyStrFmt, resourceTypeHelmRelease, hr.GetNamespace(), hr.GetName())
	isStatusReadyNILorTRUE = ptr.Equal(rp.Status.HelmReleaseReady, ptr.To(true))
	isStatusReadyNILorFALSE = ptr.Equal(rp.Status.HelmReleaseReady, ptr.To(false))

	isResourceReady = r.checkIfResourceIsReady(log, msgNotReady, msgReady, resourceTypeHelmRelease, isGenerationCurrent, isStatusConditionReady, isStatusReadyNILorTRUE, isStatusReadyNILorFALSE, rp)
	if !isResourceReady {
		// need to requeue in this case
		return v1alpha1.RedpandaNotReady(rp, "ArtifactFailed", msgNotReady), ctrl.Result{RequeueAfter: r.RequeueHelmDeps}, nil
	}

	return v1alpha1.RedpandaReady(rp), ctrl.Result{}, nil
}

func (r *RedpandaReconciler) checkIfResourceIsReady(log logr.Logger, msgNotReady, msgReady, kind string, isGenerationCurrent, isStatusConditionReady, isStatusReadyNILorTRUE, isStatusReadyNILorFALSE bool, rp *v1alpha1.Redpanda) bool {
	if isGenerationCurrent || !isStatusConditionReady {
		// capture event only
		if isStatusReadyNILorTRUE {
			r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityInfo, msgNotReady)
		}

		switch kind {
		case resourceTypeHelmRepository:
			rp.Status.HelmRepositoryReady = ptr.To(false)
		case resourceTypeHelmRelease:
			rp.Status.HelmReleaseReady = ptr.To(false)
		}

		log.Info(msgNotReady)
		return false
	} else if isStatusConditionReady && isStatusReadyNILorFALSE {
		// here since the condition should be true, we update the value to
		// be true, and send an event
		switch kind {
		case resourceTypeHelmRepository:
			rp.Status.HelmRepositoryReady = ptr.To(true)
		case resourceTypeHelmRelease:
			rp.Status.HelmReleaseReady = ptr.To(true)
		}

		r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityInfo, msgReady)
	}

	return true
}

func (r *RedpandaReconciler) reconcileHelmRelease(ctx context.Context, rp *v1alpha1.Redpanda) (*v1alpha1.Redpanda, *helmv2beta1.HelmRelease, error) {
	var err error

	// Check if HelmRelease exists or create it
	hr := &helmv2beta1.HelmRelease{}

	// have we recorded a helmRelease, if not assume we have not created it
	if rp.Status.HelmRelease == "" {
		// did not find helmRelease, then create it
		hr, err = r.createHelmRelease(ctx, rp)
		return rp, hr, err
	}

	// if we are not empty, then we assume at some point this existed, let's check
	key := types.NamespacedName{Namespace: rp.Namespace, Name: rp.Status.GetHelmRelease()}
	err = r.Client.Get(ctx, key, hr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			rp.Status.HelmRelease = ""
			hr, err = r.createHelmRelease(ctx, rp)
			return rp, hr, err
		}
		// if this is a not found error
		return rp, hr, fmt.Errorf("failed to get HelmRelease '%s/%s': %w", rp.Namespace, rp.Status.HelmRelease, err)
	}

	// Check if we need to update here
	hrTemplate, errTemplated := r.createHelmReleaseFromTemplate(ctx, rp)
	if errTemplated != nil {
		r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityError, errTemplated.Error())
		return rp, hr, errTemplated
	}

	if r.helmReleaseRequiresUpdate(ctx, hr, hrTemplate) {
		hr.Spec = hrTemplate.Spec
		if err = r.Client.Update(ctx, hr); err != nil {
			r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityError, err.Error())
			return rp, hr, err
		}
		r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityInfo, fmt.Sprintf("HelmRelease '%s/%s' updated", rp.Namespace, rp.GetHelmReleaseName()))
		rp.Status.HelmRelease = rp.GetHelmReleaseName()
	}

	return rp, hr, nil
}

func (r *RedpandaReconciler) reconcileHelmRepository(ctx context.Context, rp *v1alpha1.Redpanda) (*v1alpha1.Redpanda, *sourcev1.HelmRepository, error) {
	// Check if HelmRepository exists or create it
	repo := &sourcev1.HelmRepository{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: rp.Namespace, Name: rp.GetHelmRepositoryName()}, repo); err != nil {
		if apierrors.IsNotFound(err) {
			repo = r.createHelmRepositoryFromTemplate(rp)
			if errCreate := r.Client.Create(ctx, repo); errCreate != nil {
				r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityError, fmt.Sprintf("error creating HelmRepository: %s", errCreate))
				return rp, repo, fmt.Errorf("error creating HelmRepository: %w", errCreate)
			}
			r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityInfo, fmt.Sprintf("HelmRepository '%s/%s' created ", rp.Namespace, rp.GetHelmRepositoryName()))
		} else {
			r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityError, fmt.Sprintf("error getting HelmRepository: %s", err))
			return rp, repo, fmt.Errorf("error getting HelmRepository: %w", err)
		}
	}
	rp.Status.HelmRepository = rp.GetHelmRepositoryName()

	return rp, repo, nil
}

func (r *RedpandaReconciler) reconcileDelete(ctx context.Context, rp *v1alpha1.Redpanda) (ctrl.Result, error) {
	if err := r.deleteHelmRelease(ctx, rp); err != nil {
		return ctrl.Result{}, err
	}
	if controllerutil.ContainsFinalizer(rp, FinalizerKey) {
		controllerutil.RemoveFinalizer(rp, FinalizerKey)
		if err := r.Client.Update(ctx, rp); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *RedpandaReconciler) createHelmRelease(ctx context.Context, rp *v1alpha1.Redpanda) (*helmv2beta1.HelmRelease, error) {
	// create helmRelease resource from template
	hRelease, err := r.createHelmReleaseFromTemplate(ctx, rp)
	if err != nil {
		r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityError, fmt.Sprintf("could not create helm release template: %s", err))
		return hRelease, fmt.Errorf("could not create HelmRelease template: %w", err)
	}

	// create helmRelease object here
	if err := r.Client.Create(ctx, hRelease); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityError, err.Error())
			return hRelease, fmt.Errorf("failed to create HelmRelease '%s/%s': %w", rp.Namespace, rp.Status.HelmRelease, err)
		}
		// we already exist, then update the status to rp
		rp.Status.HelmRelease = rp.GetHelmReleaseName()
	}

	// we have created the resource, so we are ok to update events, and update the helmRelease name on the status object
	r.event(rp, rp.Status.LastAttemptedRevision, v1alpha1.EventSeverityInfo, fmt.Sprintf("HelmRelease '%s/%s' created ", rp.Namespace, rp.GetHelmReleaseName()))
	rp.Status.HelmRelease = rp.GetHelmReleaseName()

	return hRelease, nil
}

func (r *RedpandaReconciler) deleteHelmRelease(ctx context.Context, rp *v1alpha1.Redpanda) error {
	if rp.Status.HelmRelease == "" {
		return nil
	}

	var hr helmv2beta1.HelmRelease
	hrName := rp.Status.GetHelmRelease()
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: rp.Namespace, Name: hrName}, &hr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			rp.Status.HelmRelease = ""
			rp.Status.HelmRepository = ""
			return nil
		}
		return fmt.Errorf("failed to get HelmRelease '%s': %w", rp.Status.HelmRelease, err)
	}

	foregroundDeletePropagation := metav1.DeletePropagationForeground

	if err = r.Client.Delete(ctx, &hr, &client.DeleteOptions{
		PropagationPolicy: &foregroundDeletePropagation,
	}); err != nil {
		return fmt.Errorf("deleting helm release connected with Redpanda (%s): %w", rp.Name, err)
	}

	return errors.New("wait for helm release deletion")
}

func (r *RedpandaReconciler) createHelmReleaseFromTemplate(ctx context.Context, rp *v1alpha1.Redpanda) (*helmv2beta1.HelmRelease, error) {
	log := ctrl.LoggerFrom(ctx).WithName("RedpandaReconciler.createHelmReleaseFromTemplate")

	values, err := rp.ValuesJSON()
	if err != nil {
		return nil, fmt.Errorf("could not parse clusterSpec to json: %w", err)
	}

	hasher := sha256.New()
	hasher.Write(values.Raw)
	sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))
	// TODO possibly add the SHA to the status
	log.Info(fmt.Sprintf("SHA of values file to use: %s", sha))

	timeout := rp.Spec.ChartRef.Timeout
	if timeout == nil {
		timeout = &metav1.Duration{Duration: 15 * time.Minute}
	}

	rollBack := helmv2beta1.RemediationStrategy("rollback")

	upgrade := &helmv2beta1.Upgrade{
		Remediation: &helmv2beta1.UpgradeRemediation{
			Retries:  1,
			Strategy: &rollBack,
		},
	}

	helmUpgrade := rp.Spec.ChartRef.Upgrade
	if rp.Spec.ChartRef.Upgrade != nil {
		if helmUpgrade.Force != nil {
			upgrade.Force = ptr.Deref(helmUpgrade.Force, false)
		}
		if helmUpgrade.CleanupOnFail != nil {
			upgrade.CleanupOnFail = ptr.Deref(helmUpgrade.CleanupOnFail, false)
		}
		if helmUpgrade.PreserveValues != nil {
			upgrade.PreserveValues = ptr.Deref(helmUpgrade.PreserveValues, false)
		}
		if helmUpgrade.Remediation != nil {
			upgrade.Remediation = helmUpgrade.Remediation
		}
	}

	return &helmv2beta1.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:            rp.GetHelmReleaseName(),
			Namespace:       rp.Namespace,
			OwnerReferences: []metav1.OwnerReference{rp.OwnerShipRefObj()},
		},
		Spec: helmv2beta1.HelmReleaseSpec{
			Chart: helmv2beta1.HelmChartTemplate{
				Spec: helmv2beta1.HelmChartTemplateSpec{
					Chart:    "redpanda",
					Version:  rp.Spec.ChartRef.ChartVersion,
					Interval: &metav1.Duration{Duration: 1 * time.Minute},
					SourceRef: helmv2beta1.CrossNamespaceObjectReference{
						Kind:      "HelmRepository",
						Name:      rp.GetHelmRepositoryName(),
						Namespace: rp.Namespace,
					},
				},
			},
			Values:   values,
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  timeout,
			Upgrade:  upgrade,
		},
	}, nil
}

func (r *RedpandaReconciler) createHelmRepositoryFromTemplate(rp *v1alpha1.Redpanda) *sourcev1.HelmRepository {
	return &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:            rp.GetHelmRepositoryName(),
			Namespace:       rp.Namespace,
			OwnerReferences: []metav1.OwnerReference{rp.OwnerShipRefObj()},
		},
		Spec: sourcev1.HelmRepositorySpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			URL:      v1alpha1.RedpandaChartRepository,
		},
	}
}

func (r *RedpandaReconciler) patchRedpandaStatus(ctx context.Context, rp *v1alpha1.Redpanda) error {
	key := client.ObjectKeyFromObject(rp)
	latest := &v1alpha1.Redpanda{}
	if err := r.Client.Get(ctx, key, latest); err != nil {
		return err
	}
	return r.Client.Status().Patch(ctx, rp, client.MergeFrom(latest))
}

// event emits a Kubernetes event and forwards the event to notification controller if configured.
func (r *RedpandaReconciler) event(rp *v1alpha1.Redpanda, revision, severity, msg string) {
	var metaData map[string]string
	if revision != "" {
		metaData = map[string]string{v2.GroupVersion.Group + "/revision": revision}
	}
	eventType := "Normal"
	if severity == v1alpha1.EventSeverityError {
		eventType = "Warning"
	}
	r.EventRecorder.AnnotatedEventf(rp, metaData, eventType, severity, msg)
}

func (r *RedpandaReconciler) helmReleaseRequiresUpdate(ctx context.Context, hr, hrTemplate *helmv2beta1.HelmRelease) bool {
	log := ctrl.LoggerFrom(ctx).WithName("RedpandaReconciler.helmReleaseRequiresUpdate")

	switch {
	case !reflect.DeepEqual(hr.GetValues(), hrTemplate.GetValues()):
		log.Info("values found different")
		return true
	case helmChartRequiresUpdate(log, &hr.Spec.Chart, &hrTemplate.Spec.Chart):
		log.Info("chartTemplate found different")
		return true
	case hr.Spec.Interval != hrTemplate.Spec.Interval:
		log.Info("interval found different")
		return true
	default:
		return false
	}
}

// helmChartRequiresUpdate compares the v2beta1.HelmChartTemplate of the
// v2beta1.HelmRelease to the given v1beta2.HelmChart to determine if an
// update is required.
func helmChartRequiresUpdate(log logr.Logger, template, chart *helmv2beta1.HelmChartTemplate) bool {
	switch {
	case template.Spec.Chart != chart.Spec.Chart:
		log.Info("chart is different")
		return true
	case template.Spec.Version != "" && template.Spec.Version != chart.Spec.Version:
		log.Info("spec version is different")
		return true
	default:
		return false
	}
}

func isRedpandaManaged(ctx context.Context, redpandaCluster *v1alpha1.Redpanda) bool {
	log := ctrl.LoggerFrom(ctx).WithName("RedpandaReconciler.isRedpandaManaged")

	managedAnnotationKey := v1alpha1.GroupVersion.Group + managedPath
	if managed, exists := redpandaCluster.Annotations[managedAnnotationKey]; exists && managed == NotManaged {
		log.Info(fmt.Sprintf("management is disabled; to enable it, change the '%s' annotation to true or remove it", managedAnnotationKey))
		return false
	}
	return true
}

func disableRedpandaReconciliation(redpandaCluster *vectorzied_v1alpha1.Cluster) {
	managedAnnotationKey := vectorzied_v1alpha1.GroupVersion.Group + managedPath
	if redpandaCluster.Annotations == nil {
		redpandaCluster.Annotations = map[string]string{}
	}
	redpandaCluster.Annotations[managedAnnotationKey] = NotManaged
}

func disableConsoleReconciliation(console *vectorzied_v1alpha1.Console) {
	managedAnnotationKey := vectorzied_v1alpha1.GroupVersion.Group + managedPath
	if console.Annotations == nil {
		console.Annotations = map[string]string{}
	}
	console.Annotations[managedAnnotationKey] = NotManaged
}
