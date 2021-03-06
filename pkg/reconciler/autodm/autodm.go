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

package autodm

import (
	"context"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/logging"
	servingv1alpha1 "knative.dev/serving/pkg/apis/serving/v1alpha1"
	"knative.dev/sugar/pkg/reconciler/autodm/resources"
	"knative.dev/sugar/pkg/sugared"
)

func NewAutoDM() *AutoDM {
	return &AutoDM{}
}

type AutoDM struct{}

func (a *AutoDM) Do(ctx context.Context, cfg *sugared.Config, fn sugared.RealizeFn) error {
	dms := make([]runtime.Object, 0)

	if cfg.Sugared() {
		apiVersion, kind := cfg.Raw.Resource.GetGroupVersionKind().ToAPIVersionAndKind()
		for key, value := range cfg.Annotations {

			logging.FromContext(ctx).Info("working a sugared resource with k=v", key, "=", value)

			dm := resources.MakeDomainMapping(&resources.DomainMappingArgs{
				Name:  value,
				Owner: cfg.Raw.Resource,
				Ref: duckv1.KReference{
					Kind:       kind,
					Namespace:  cfg.Raw.Resource.GetObjectMeta().GetNamespace(),
					Name:       cfg.Raw.Resource.GetObjectMeta().GetName(),
					APIVersion: apiVersion,
				},
			})
			// Apply the hint so we can find the index again.
			cfg.ApplyHint(dm, key)
			dms = append(dms, dm)
		}
	}

	if err := fn(dms); err != nil {
		logging.FromContext(ctx).Errorw("failed to realize domain maps",
			zap.String("key", cfg.Raw.Resource.GetObjectMeta().GetNamespace()+"/"+cfg.Raw.Resource.GetObjectMeta().GetName()),
			zap.String("gvk", cfg.Raw.Resource.GetGroupVersionKind().String()),
			zap.Error(err))
		return err
	}

	return nil
}

func (a *AutoDM) Kinds() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		servingv1alpha1.SchemeGroupVersion.WithKind("DomainMapping"),
	}
}
