// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package storageclass

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	infrav1 "github.com/vmware-tanzu/vm-operator/external/infra/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkglog "github.com/vmware-tanzu/vm-operator/pkg/log"
	"github.com/vmware-tanzu/vm-operator/pkg/record"
	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
)

// AddToManager adds this package's controller to the provided manager.
func AddToManager(ctx *pkgctx.ControllerManagerContext, mgr manager.Manager) error {
	var (
		controlledType     = &storagev1.StorageClass{}
		controlledTypeName = reflect.TypeOf(controlledType).Elem().Name()

		controllerNameShort = fmt.Sprintf("%s-controller", strings.ToLower(controlledTypeName))
		controllerNameLong  = fmt.Sprintf("%s/%s/%s", ctx.Namespace, ctx.Name, controllerNameShort)
	)

	separateCache, err := cache.New(mgr.GetConfig(), newSeparateCacheOptions(mgr.GetScheme()))
	if err != nil {
		return fmt.Errorf("creating separate cache: %w", err)
	}
	if err := mgr.Add(separateCache); err != nil {
		return fmt.Errorf("adding separate cache to manager: %w", err)
	}

	r := NewReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("controllers").WithName(controlledTypeName),
		record.New(mgr.GetEventRecorderFor(controllerNameLong)),
		separateCache,
		nil,
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(controlledType).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: ctx.GetMaxConcurrentReconciles(controllerNameShort, 0),
			LogConstructor:          pkglog.ControllerLogConstructor(controllerNameShort, controlledType, mgr.GetScheme()),
		}).
		Complete(r)
}

func NewReconciler(
	ctx context.Context,
	client client.Client,
	logger logr.Logger,
	recorder record.Recorder,
	separateCache cache.Cache) *Reconciler {

	return &Reconciler{
		Context:       ctx,
		Client:        client,
		Logger:        logger,
		Recorder:      recorder,
		separateCache: separateCache,
	}
}

// Reconciler reconciles a StorageClass object.
type Reconciler struct {
	client.Client
	Context       context.Context
	Logger        logr.Logger
	Recorder      record.Recorder
	separateCache cache.Cache
}

// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.vmware.com,resources=storagepolicies,verbs=get;list;watch;create;update;patch;delete

func (r *Reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request) (_ ctrl.Result, reterr error) {

	ctx = pkgcfg.JoinContext(ctx, r.Context)

	// make a list of 100 different label key value pairs
	labels := make([]map[string]string, 100)
	for i := 0; i < 100; i++ {
		labels[i] = map[string]string{
			fmt.Sprintf("key%d", i): fmt.Sprintf("value%d", i),
		}
	}

	logger := pkglog.FromContextOrDefault(ctx)

	logger.Info("Fetching configmaps from cache", "request", req)
	// now list configmap with above 100 label key value pairs separately for each label key value pair
	for _, label := range labels {
		configMapList := &corev1.ConfigMapList{}
		err := r.separateCache.List(ctx, configMapList, ctrlclient.InNamespace(req.Namespace), ctrlclient.MatchingLabels(label))
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.Info("Configmaps fetched from cache", "request", req)

	logger.Info("Fetching configmaps from api server", "request", req)
	// now list configmap with above 100 label key value pairs separately for each label key value pair
	for _, label := range labels {
		configMapList := &corev1.ConfigMapList{}
		err := r.Client.List(ctx, configMapList, ctrlclient.InNamespace(req.Namespace), ctrlclient.MatchingLabels(label))
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.Info("Configmaps fetched from api server", "request", req)

	var obj storagev1.StorageClass
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !obj.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.ReconcileNormal(ctx, &obj)
}

// StripDataTransform returns a cache transform that removes .Data and .StringData
// from ConfigMaps and Secrets to reduce memory usage.
func StripDataTransform() toolscache.TransformFunc {
	return func(obj interface{}) (interface{}, error) {
		switch t := obj.(type) {
		case *corev1.ConfigMap:
			t.Data = nil
			t.BinaryData = nil
			if t.ObjectMeta.Annotations != nil {
				delete(t.ObjectMeta.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
			}
			return t, nil
		case *corev1.Secret:
			t.Data = nil
			t.StringData = nil
			if t.ObjectMeta.Annotations != nil {
				delete(t.ObjectMeta.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
			}
			return t, nil
		default:
			return obj, nil
		}
	}
}

func newSeparateCacheOptions(scheme *runtime.Scheme) cache.Options {
	return cache.Options{
		Scheme: scheme,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {
				Transform: StripDataTransform(),
			},
			&corev1.Secret{}: {
				Transform: StripDataTransform(),
			},
		},
		ReaderFailOnMissingInformer: false,
	}
}

func (r *Reconciler) ReconcileNormal(
	ctx context.Context,
	obj *storagev1.StorageClass) error {

	logger := pkglog.FromContextOrDefault(ctx)

	policyID, err := kubeutil.GetStoragePolicyIDFromStorageClass(*obj)
	if err != nil || policyID == "" {
		logger.Error(
			err,
			"failed to get storage policy ID",
			"storageClass",
			obj.Name)

		// Don't return an error: an update to the StorageClass will cause a
		// reconcile.
		return nil
	}

	polName := kubeutil.GetStoragePolicyObjectName(policyID)
	if polName == "" {
		return fmt.Errorf("invalid profile ID %q", policyID)
	}

	pol := infrav1.StoragePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pkgcfg.FromContext(ctx).PodNamespace,
			Name:      polName,
		},
	}
	res, err := controllerutil.CreateOrPatch(ctx, r, &pol, func() error {
		pol.Spec.ID = policyID
		return controllerutil.SetOwnerReference(obj, &pol, r.Scheme())
	})
	if err != nil {
		return fmt.Errorf("failed to create or patch storage policy %s/%s: %w",
			pol.Namespace, pol.Name, err)
	}

	logger.Info("Created or patched storage policy object",
		"namespace", pol.Namespace,
		"name", pol.Name,
		"result", res)

	return nil
}
