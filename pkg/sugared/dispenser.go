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

package sugared

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	discoverylistersv1alpha1 "knative.dev/discovery/pkg/client/listers/discovery/v1alpha1"
	"knative.dev/pkg/apis/duck"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	sugarreconciler "knative.dev/sugar/pkg/reconciler"
)

type Dispenser struct {
	prefix          string
	owner           string
	informerFactory duck.InformerFactory
	listers         map[string]cache.GenericLister
	duckTypeLister  discoverylistersv1alpha1.ClusterDuckTypeLister
	duckTypeName    string
	duckTypeVersion string
}

func (d *Dispenser) getLister(ctx context.Context, gvk schema.GroupVersionKind) (cache.GenericLister, error) {
	lister, found := d.listers[gvk.String()]
	if !found {
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		_, l, err := d.informerFactory.Get(ctx, gvr)
		if err != nil {
			return nil, err
		}
		lister = l
		d.listers[gvk.String()] = l
	}
	return lister, nil
}

func (d *Dispenser) List(ctx context.Context, namespace string, gvks []schema.GroupVersionKind) ([]*Turbinado, error) {
	all := make([]*Turbinado, 0)
	for _, gvk := range gvks {
		s, err := d.list(ctx, namespace, gvk)
		if err != nil {
			return nil, err
		}
		all = append(all, s...)
	}
	return all, nil
}

func (d *Dispenser) list(ctx context.Context, namespace string, gvk schema.GroupVersionKind) ([]*Turbinado, error) {
	selector, err := labels.Parse(fmt.Sprintf("%s=%s", sugarreconciler.SugarOwnerLabelKey, sugarreconciler.AutoDomainMappingLabel))
	if err != nil {
		return nil, fmt.Errorf("failed to produce label selector: %w", err)
	}

	lister, err := d.getLister(ctx, gvk)
	if err != nil {
		return nil, err
	}

	runtimeObjs, err := lister.ByNamespace(namespace).List(selector)
	if err != nil {
		logging.FromContext(ctx).Errorw("unable to list duck type",
			zap.String("gvk", gvk.String()), zap.String("namespace", namespace))
		return nil, err
	}

	s := make([]*Turbinado, 0)
	for _, runtimeObj := range runtimeObjs {
		var ok bool
		var resource kmeta.OwnerRefable
		if resource, ok = runtimeObj.DeepCopyObject().(kmeta.OwnerRefable); !ok {
			return nil, errors.New("runtime object is not convertible to kmeta.OwnerRefable type")
		}

		s = append(s, &Turbinado{
			Resource: resource,
			Prefix:   d.prefix,
		})
	}
	return s, nil
}

func (d *Dispenser) Get(ctx context.Context, namespace, name string, gvk schema.GroupVersionKind) (*Turbinado, error) {
	lister, err := d.getLister(ctx, gvk)
	if err != nil {
		return nil, err
	}

	// Get the duck resource with this namespace/name
	runtimeObj, err := lister.ByNamespace(namespace).Get(name)
	if err != nil {
		logging.FromContext(ctx).Errorw("unable to get duck type", zap.String("gvk", gvk.String()), zap.String("key", namespace+"/"+name))
		return nil, err
	}

	var ok bool
	var resource kmeta.OwnerRefable
	if resource, ok = runtimeObj.DeepCopyObject().(kmeta.OwnerRefable); !ok {
		return nil, errors.New("runtime object is not convertible to kmeta.OwnerRefable type")
	}

	return &Turbinado{
		Resource: resource,
		Prefix:   d.prefix,
	}, nil
}

func (d *Dispenser) IsDuck(ctx context.Context, gvk schema.GroupVersionKind) bool {
	key := fmt.Sprintf("%s.%s", gvk.Kind, gvk.Group)

	if dt, err := d.duckTypeLister.Get(d.duckTypeName); err != nil {
		logging.FromContext(ctx).Errorw("failed to get cluster duck type for "+d.duckTypeName, zap.Error(err))
		return false
	} else {
		// Build a lookup table without version.
		for _, v := range dt.Status.Ducks[d.duckTypeVersion] {
			if key == fmt.Sprintf("%s.%s", v.Kind, v.Group()) {
				return true
			}
		}
	}

	return false
}

// OwnerSugaredDuckConfig will return the config of the owner of the sugared resource.
// For now, we will only look up one level in the owners graph.
// TODO: rename?
func (d *Dispenser) OwnerSugaredDuckConfig(ctx context.Context, sugared *Turbinado) (*Config, bool) {
	// TODO: we could filter by Owner Controller.
	for _, owner := range sugared.Resource.GetObjectMeta().GetOwnerReferences() {
		gvk := schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)
		// First, check if the owner is a duck.
		if !d.IsDuck(ctx, gvk) {
			// If the owner is not a duck, we can skip looking at it further.
			continue
		}

		// Second, check if that owner is sugared.
		t, err := d.Get(ctx, sugared.Resource.GetObjectMeta().GetNamespace(), owner.Name, gvk)
		if err != nil {
			logging.FromContext(ctx).Errorw("failed to get sugared owner ", owner.Name,
				zap.String("gvk", gvk.String()), zap.Error(err))
			return nil, false
		}
		cfg := t.Config()
		if cfg.Sugared() {
			return cfg, true
		}
		// TODO: here we could recurse on the sugared owner's owner and keep searching,
		// but this will require a curricular loop detection.
	}
	return nil, false
}
