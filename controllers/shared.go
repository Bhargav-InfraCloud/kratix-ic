package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/resourceutil"
	"github.com/syntasso/kratix/lib/writers"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	promiseVersionLabel            = v1alpha1.KratixPrefix + "promise-version"
	promiseReleaseNameLabel        = v1alpha1.KratixPrefix + "promise-release-name"
	removeAllWorkflowJobsFinalizer = v1alpha1.KratixPrefix + "workflows-cleanup"
	runDeleteWorkflowsFinalizer    = v1alpha1.KratixPrefix + "delete-workflows"
)

type StateStore interface {
	client.Object
	GetSecretRef() *v1.SecretReference
}

type opts struct {
	ctx    context.Context
	client client.Client
	logger logr.Logger
}

// pass in nil resourceLabels to delete all resources of the GVK
func deleteAllResourcesWithKindMatchingLabel(o opts, gvk schema.GroupVersionKind, resourceLabels map[string]string) (bool, error) {
	resourceList := &unstructured.UnstructuredList{}
	resourceList.SetGroupVersionKind(gvk)
	listOptions := client.ListOptions{LabelSelector: labels.SelectorFromSet(resourceLabels)}
	err := o.client.List(o.ctx, resourceList, &listOptions)
	if err != nil {
		return true, err
	}

	o.logger.Info("deleting resources", "gvk", resourceList.GroupVersionKind(), "withLabels", resourceLabels, "resources", resourceutil.GetResourceNames(resourceList.Items))

	for _, resource := range resourceList.Items {
		err = o.client.Delete(o.ctx, &resource, client.PropagationPolicy(metav1.DeletePropagationBackground))
		o.logger.Info("deleting resource", "res", resource.GetName(), "gvk", resource.GroupVersionKind())
		if err != nil && !errors.IsNotFound(err) {
			o.logger.Error(err, "Error deleting resource, will try again in 5 seconds", "name", resource.GetName(), "kind", resource.GetKind())
			return true, err
		}
		o.logger.Info("successfully triggered deletion of resource", "name", resource.GetName(), "kind", resource.GetKind())
	}

	return len(resourceList.Items) != 0, nil
}

// finalizers must be less than 64 characters
func addFinalizers(o opts, resource client.Object, finalizers []string) (ctrl.Result, error) {
	o.logger.Info("Adding missing finalizers",
		"expectedFinalizers", finalizers,
		"existingFinalizers", resource.GetFinalizers(),
	)
	for _, finalizer := range finalizers {
		controllerutil.AddFinalizer(resource, finalizer)
	}
	if err := o.client.Update(o.ctx, resource); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func fetchObjectAndSecret(o opts, stateStoreRef client.ObjectKey, stateStore StateStore) (*v1.Secret, error) {
	if err := o.client.Get(o.ctx, stateStoreRef, stateStore); err != nil {
		o.logger.Error(err, "unable to fetch resource", "resourceKind", stateStore.GetObjectKind(), "stateStoreRef", stateStoreRef)
		return nil, err
	}

	if stateStore.GetSecretRef() == nil {
		return nil, nil
	}

	namespace := stateStore.GetSecretRef().Namespace
	if namespace == "" {
		namespace = v1alpha1.SystemNamespace
	}

	secret := &v1.Secret{}
	secretRef := types.NamespacedName{
		Name:      stateStore.GetSecretRef().Name,
		Namespace: namespace,
	}

	if err := o.client.Get(o.ctx, secretRef, secret); err != nil {
		o.logger.Error(err, "unable to fetch resource", "resourceKind", stateStore.GetObjectKind(), "secretRef", secretRef)
		return nil, err
	}

	return secret, nil
}

func newWriter(o opts, destination v1alpha1.Destination) (writers.StateStoreWriter, error) {
	stateStoreRef := client.ObjectKey{
		Name:      destination.Spec.StateStoreRef.Name,
		Namespace: destination.Namespace,
	}

	var writer writers.StateStoreWriter
	var err error
	switch destination.Spec.StateStoreRef.Kind {
	case "BucketStateStore":
		stateStore := &v1alpha1.BucketStateStore{}
		secret, fetchErr := fetchObjectAndSecret(o, stateStoreRef, stateStore)
		if fetchErr != nil {
			return nil, fetchErr
		}
		var data map[string][]byte = nil
		if secret != nil {
			data = secret.Data
		}

		writer, err = writers.NewS3Writer(o.logger.WithName("writers").WithName("BucketStateStoreWriter"), stateStore.Spec, destination, data)
	case "GitStateStore":
		stateStore := &v1alpha1.GitStateStore{}
		secret, fetchErr := fetchObjectAndSecret(o, stateStoreRef, stateStore)
		if fetchErr != nil {
			return nil, fetchErr
		}

		writer, err = writers.NewGitWriter(o.logger.WithName("writers").WithName("GitStateStoreWriter"), stateStore.Spec, destination, secret.Data)
	default:
		return nil, fmt.Errorf("unsupported kind %s", destination.Spec.StateStoreRef.Kind)
	}

	if err != nil {
		//TODO: should this be a retryable error?
		o.logger.Error(err, "unable to create StateStoreWriter")
		return nil, err
	}
	return writer, nil
}

func getJobsWithLabels(o opts, jobLabels map[string]string, namespace string) ([]batchv1.Job, error) {
	selectorLabels := labels.FormatLabels(jobLabels)
	selector, err := labels.Parse(selectorLabels)

	if err != nil {
		return nil, fmt.Errorf("error parsing labels %v: %w", jobLabels, err)
	}

	listOps := &client.ListOptions{
		LabelSelector: selector,
		Namespace:     namespace,
	}

	jobs := &batchv1.JobList{}
	err = o.client.List(o.ctx, jobs, listOps)
	if err != nil {
		o.logger.Error(err, "error listing jobs", "selectors", selector.String())
		return nil, err
	}
	return jobs.Items, nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func shortID(id string) string {
	return id[0:5]
}
