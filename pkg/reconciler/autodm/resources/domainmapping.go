/*
Copyright 2021 The Knative Authors

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

package resources

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/kmeta"
	servingv1alpha1 "knative.dev/serving/pkg/apis/serving/v1alpha1"
	"knative.dev/sugar/pkg/reconciler"
)

// DomainMappingArgs
type DomainMappingArgs struct {
	Name  string
	Hint  string
	Owner kmeta.OwnerRefable
	Ref   duckv1.KReference
}

// MakeDomainMapping
func MakeDomainMapping(args *DomainMappingArgs) *servingv1alpha1.DomainMapping {
	return &servingv1alpha1.DomainMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: args.Owner.GetObjectMeta().GetNamespace(),
			Name:      args.Name,
			OwnerReferences: []metav1.OwnerReference{
				*kmeta.NewControllerRef(args.Owner),
			},
			Labels:      DomainMappingLabels(),
			Annotations: DomainMappingAnnotations(args.Hint),
		},
		Spec: servingv1alpha1.DomainMappingSpec{
			Ref: args.Ref,
		},
	}
}

// DomainMappingLabels
func DomainMappingLabels() map[string]string {
	return map[string]string{
		reconciler.SugarOwnerLabelKey: reconciler.AutoDomainMappingLabel,
	}
}

// DomainMappingAnnotations
func DomainMappingAnnotations(hint string) map[string]string {
	return map[string]string{
		reconciler.DomainMappingHintAnnotationKey: hint,
	}
}
