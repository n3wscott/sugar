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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	discoverylistersv1alpha1 "knative.dev/discovery/pkg/client/listers/discovery/v1alpha1"
	"knative.dev/pkg/apis/duck"
	"knative.dev/pkg/logging"
	"strings"

	"knative.dev/pkg/kmeta"
)

type Confectioner interface {
	Do()
}

type SugarDispenser struct {
	prefix          string
	informerFactory duck.InformerFactory
	listers         map[string]cache.GenericLister
	duckTypeLister  discoverylistersv1alpha1.ClusterDuckTypeLister
	duckTypeName    string
	duckTypeVersion string
}

func (sd *SugarDispenser) Sugared(ctx context.Context, namespace, name string, gvk schema.GroupVersionKind) (*Sugared, error) {
	lister, found := sd.listers[gvk.String()]
	if !found {
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		_, l, err := sd.informerFactory.Get(ctx, gvr)
		if err != nil {
			return nil, err
		}
		lister = l
		sd.listers[gvk.String()] = l
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

	return &Sugared{
		Resource: resource,
		Prefix:   sd.prefix,
	}, nil
}

func (sd *SugarDispenser) IsDuck(ctx context.Context, gvk schema.GroupVersionKind) bool {
	key := fmt.Sprintf("%s.%s", gvk.Kind, gvk.Group)

	if dt, err := sd.duckTypeLister.Get(sd.duckTypeName); err != nil {
		logging.FromContext(ctx).Errorw("failed to get cluster duck type for "+sd.duckTypeName, zap.Error(err))
		return false
	} else {
		// Build a lookup table without version.
		for _, v := range dt.Status.Ducks[sd.duckTypeVersion] {
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
func (sd *SugarDispenser) OwnerSugaredDuckConfig(ctx context.Context, sugared *Sugared) (*Config, bool) {
	// TODO: we could filter by Owner Controller.
	for _, owner := range sugared.Resource.GetObjectMeta().GetOwnerReferences() {
		gvk := schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)
		// First, check if the owner is a duck.
		if !sd.IsDuck(ctx, gvk) {
			// If the owner is not a duck, we can skip looking at it further.
			continue
		}

		// Second, check if that owner is sugared.
		so, err := sd.Sugared(ctx, sugared.Resource.GetObjectMeta().GetNamespace(), owner.Name, gvk)
		if err != nil {
			logging.FromContext(ctx).Errorw("failed to get sugared owner ", owner.Name,
				zap.String("gvk", gvk.String()), zap.Error(err))
			return nil, false
		}
		cfg := so.Config()
		if cfg.Sugared() {
			return cfg, true
		}
		// TODO: here we could recurse on the sugared owner's owner and keep searching,
		// but this will require a curricular loop detection.
	}
	return nil, false
}

type Sugared struct {
	Resource kmeta.OwnerRefable
	Prefix   string // like sugarreconciler.DomainMappingAnnotationKey
	// TODO: needs to know the set of other children Resource might create/own?
}

type Config struct {
	Sugar       *Sugared
	Annotations map[string]string
	Labels      map[string]string
}

func (s *Sugared) Config() *Config {
	// prefix[extras] = values  ---> hint = prefix+extras

	annotations := make(map[string]string)
	for k, value := range s.Resource.GetObjectMeta().GetAnnotations() {
		if strings.HasPrefix(k, s.Prefix) {
			key := strings.TrimPrefix(k, s.Prefix)
			annotations[key] = value
		}
	}

	labels := make(map[string]string)
	for k, value := range s.Resource.GetObjectMeta().GetLabels() {
		if strings.HasPrefix(k, s.Prefix) {
			key := strings.TrimPrefix(k, s.Prefix)
			annotations[key] = value
		}
	}

	return &Config{
		Sugar:       s,
		Annotations: annotations,
		Labels:      labels,
	}
}

func (c *Config) Subtract(sub *Config) {
	for k, _ := range sub.Annotations {
		delete(c.Annotations, k)
	}
	for k, _ := range sub.Labels {
		delete(c.Labels, k)
	}
}

func (c *Config) Sugared() bool {
	return len(c.Annotations) > 0 || len(c.Labels) > 0
}
