package pipeline

import (
	"github.com/syntasso/kratix/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const kratixActionDelete = "delete"

func NewDeleteResource(rr *unstructured.Unstructured, pipelines []v1alpha1.Pipeline, resourceRequestIdentifier, promiseIdentifier, crdPlural string) []client.Object {
	return NewDelete(rr, pipelines, resourceRequestIdentifier, promiseIdentifier, crdPlural)
}

func NewDeletePromise(promise *unstructured.Unstructured, pipelines []v1alpha1.Pipeline) []client.Object {
	return NewDelete(promise, pipelines, "", promise.GetName(), v1alpha1.PromisePlural)
}

func NewDelete(obj *unstructured.Unstructured, pipelines []v1alpha1.Pipeline, resourceRequestIdentifier, promiseIdentifier, objPlural string) []client.Object {
	isPromise := resourceRequestIdentifier == ""
	namespace := obj.GetNamespace()
	if isPromise {
		namespace = v1alpha1.SystemNamespace
	}

	args := NewPipelineArgs(promiseIdentifier, resourceRequestIdentifier, namespace)

	containers, pipelineVolumes := deletePipelineContainers(obj, isPromise, pipelines)

	var imagePullSecrets []v1.LocalObjectReference
	if len(pipelines) > 0 {
		imagePullSecrets = pipelines[0].Spec.ImagePullSecrets
	}

	resources := []client.Object{
		serviceAccount(args),
		role(obj, objPlural, args),
		roleBinding(args),
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      args.DeletePipelineName(),
				Namespace: args.Namespace(),
				Labels:    args.DeletePipelinePodLabels(),
			},
			Spec: batchv1.JobSpec{
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: args.DeletePipelinePodLabels(),
					},
					Spec: v1.PodSpec{
						RestartPolicy:      v1.RestartPolicyOnFailure,
						ServiceAccountName: args.ServiceAccountName(),
						Containers:         []v1.Container{containers[len(containers)-1]},
						InitContainers:     containers[0 : len(containers)-1],
						Volumes:            pipelineVolumes,
						ImagePullSecrets:   imagePullSecrets,
					},
				},
			},
		},
	}

	return resources
}

func deletePipelineContainers(obj *unstructured.Unstructured, isPromise bool, pipelines []v1alpha1.Pipeline) ([]v1.Container, []v1.Volume) {
	volumes, volumeMounts := pipelineVolumes()

	//TODO: Does this get called for promises too? If so, change the parameter name and dynamically set input below
	workflowType := v1alpha1.WorkflowTypeResource
	if isPromise {
		workflowType = v1alpha1.WorkflowTypePromise
	}

	kratixEnvVars := []v1.EnvVar{
		{
			Name:  kratixActionEnvVar,
			Value: kratixActionDelete,
		},
	}

	readerContainer := readerContainer(obj, workflowType, "shared-input")
	containers := []v1.Container{
		readerContainer,
	}

	if len(pipelines) > 0 {
		if len(pipelines[0].Spec.Volumes) > 0 {
			volumes = append(volumes, pipelines[0].Spec.Volumes...)
		}
		for _, c := range pipelines[0].Spec.Containers {
			if len(c.VolumeMounts) > 0 {
				volumeMounts = append(volumeMounts, c.VolumeMounts...)
			}
			containers = append(containers, v1.Container{
				Name:            c.Name,
				Image:           c.Image,
				VolumeMounts:    volumeMounts,
				Args:            c.Args,
				Command:         c.Command,
				Env:             append(kratixEnvVars, c.Env...),
				EnvFrom:         c.EnvFrom,
				ImagePullPolicy: c.ImagePullPolicy,
			})
		}
	}

	return containers, volumes
}
