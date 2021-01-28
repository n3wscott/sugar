/*
Copyright 2020 The Knative Authors

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

package sugarduck

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/kmp"
	"knative.dev/pkg/logging"
	"knative.dev/sugar/pkg/sugared"
)

type Reconciler struct {
	Dynamic        dynamic.Interface
	GVK            schema.GroupVersionKind
	Confectioner   sugared.Confectioner
	SugarDispenser *sugared.SugarDispenser
}

func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logging.FromContext(ctx).Errorw("invalid resource key", zap.String("key", key))
		return nil
	}

	s, err := r.SugarDispenser.Get(ctx, namespace, name, r.GVK)

	if apierrs.IsNotFound(err) {
		// The resource may no longer exist, in which case we stop processing.
		logging.FromContext(ctx).Error("Resource in work queue no longer exists")
		return nil
	} else if err != nil {
		return err
	}

	return r.ReconcileSugar(ctx, s)
}

func (r *Reconciler) ReconcileSugar(ctx context.Context, s *sugared.Turbinado) error {
	cfg := s.Config()
	ownerCfg, ownerSugared := r.SugarDispenser.OwnerSugaredDuckConfig(ctx, s)
	if ownerSugared {
		cfg.Subtract(ownerCfg)
	}

	tctx, cancel := context.WithTimeout(ctx, time.Second*3) // TODO: idk, let's try out a static timeout.
	defer cancel()

	if err := r.Confectioner.Do(tctx, cfg, func(objects []runtime.Object) error {
		// TODO: test all objects from the confectioner to confirm we have type meta.
		// TODO: test that all objects from confectioner has hints.
		// For any requested resource, create or update it.
		for _, o := range objects {
			if err := r.ensureResource(ctx, s.Resource, o); err != nil {
				logging.FromContext(ctx).Error("failed to ensure resource", zap.Error(err))
			}
		}
		// For any resource that exists but not requested, delete it.
		if objs, err := r.findObjectsForOwner(ctx, s.Resource); err != nil {
			logging.FromContext(ctx).Errorw("failed to list kinds for owners", zap.Error(err))
		} else {
			// Compare the annotations we know are on the resource to the existing resource that are owned by
			// this sugared resource. We want to delete the ones that exist and are no longer annotated.
			for _, obj := range objs {
				found := false
				for k, v := range cfg.Annotations {
					// TODO: move hashint into config some how.
					if hasHint(obj, cfg.Hint(), k) {
						if obj.GetObjectMeta().GetName() == v {
							found = true
						}
						break
					}
				}
				if !found {
					logging.FromContext(ctx).Info("did not find the object hint in the annotations, deleting.", zap.String("resource", obj.GetObjectMeta().GetNamespace()+"/"+obj.GetObjectMeta().GetName()))
					gvr, _ := meta.UnsafeGuessKindToResource(obj.GetGroupVersionKind())
					if err := r.Dynamic.Resource(gvr).Namespace(obj.GetObjectMeta().GetNamespace()).Delete(ctx, obj.GetObjectMeta().GetName(), metav1.DeleteOptions{}); err != nil {
						logging.FromContext(ctx).Info("failed to delete a domain mapping", zap.String("resource", obj.GetObjectMeta().GetNamespace()+"/"+obj.GetObjectMeta().GetName()))
					}
				}
			}
		}
		cancel()
		return nil
	}); err != nil {
		logging.FromContext(ctx).Errorw("failed to invoke confectioner", zap.Error(err))
	}

	<-tctx.Done() // Block until context is done.

	return nil
}

func hasHint(resource kmeta.OwnerRefable, hintKey, expected string) bool {
	value, found := resource.GetObjectMeta().GetAnnotations()[hintKey]
	if !found {
		return false
	}
	return value == expected
}

func (r *Reconciler) ensureResource(ctx context.Context, owner kmeta.OwnerRefable, obj runtime.Object) error {
	gvr, _ := meta.UnsafeGuessKindToResource(obj.GetObjectKind().GroupVersionKind())
	us, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	resource := &unstructured.Unstructured{Object: us}
	existing, err := r.Dynamic.Resource(gvr).Namespace(resource.GetNamespace()).Get(ctx, resource.GetName(), metav1.GetOptions{})
	if apierrs.IsNotFound(err) {
		_, err = r.Dynamic.Resource(gvr).Namespace(resource.GetNamespace()).Create(ctx, resource, metav1.CreateOptions{})
		if err != nil {
			//recorder.Eventf(addr, corev1.EventTypeWarning, "CreationFailed", "Failed to create DomainMapping %q: %v", value, err)
			return fmt.Errorf("failed to create resource: %w", err)
		}
		//recorder.Eventf(addr, corev1.EventTypeNormal, "Created", "Created DomainMapping %q", value)
	} else if err != nil {
		return fmt.Errorf("failed to get DomainMapping: %w", err)
	} else if !metav1.IsControlledBy(existing, owner.GetObjectMeta()) {
		return fmt.Errorf("addressable[%s]: %q does not own domain mapping: %q", owner.GetGroupVersionKind().Kind, owner.GetObjectMeta().GetName(), existing.GetName())
	} else if err = r.reconcileResource(ctx, gvr, resource, existing); err != nil {
		return fmt.Errorf("failed to reconcile DomainMapping: %w", err)
	}
	return nil
}

func (r *Reconciler) findObjectsForOwner(ctx context.Context, owner kmeta.OwnerRefable) ([]kmeta.OwnerRefable, error) {
	s, err := r.SugarDispenser.List(ctx, owner.GetObjectMeta().GetNamespace(), r.Confectioner.Kinds())
	if err != nil {
		return nil, err
	}

	apiVersion, kind := owner.GetGroupVersionKind().ToAPIVersionAndKind()

	// Filter for the owner.
	owned := make([]kmeta.OwnerRefable, 0)
	for _, sugar := range s {
		resource := sugar.Resource.GetObjectMeta()
		ctr := metav1.GetControllerOf(resource)
		if owner != nil &&
			ctr.APIVersion == apiVersion &&
			ctr.Kind == kind &&
			ctr.Name == owner.GetObjectMeta().GetName() {
			owned = append(owned, sugar.Resource)
		}
	}
	return owned, nil
}

func (r *Reconciler) reconcileResource(ctx context.Context, gvr schema.GroupVersionResource, desired, existing *unstructured.Unstructured) error {
	// merge the labels and annotations.
	desired.SetLabels(joinNewKeys(desired.GetLabels(), existing.GetLabels()))
	desired.SetAnnotations(joinNewKeys(desired.GetAnnotations(), existing.GetAnnotations()))

	equals, err := unstructuredSemanticEquals(ctx, desired, existing)
	if err != nil {
		return err
	}
	if equals {
		return nil
	}

	// Preserve the rest of the object (e.g. ObjectMeta except for labels and annotations).
	unstructuredSetSpec(existing, unstructuredGetSpec(desired))
	existing.SetAnnotations(desired.GetAnnotations())
	existing.SetLabels(desired.GetLabels())

	_, err = r.Dynamic.Resource(gvr).Namespace(existing.GetNamespace()).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func unstructuredSemanticEquals(ctx context.Context, desired, observed *unstructured.Unstructured) (bool, error) {
	logger := logging.FromContext(ctx)
	specDiff, err := kmp.SafeDiff(unstructuredGetSpec(desired), unstructuredGetSpec(observed))
	if err != nil {
		logger.Errorw("Error diffing Unstructured spec", zap.Error(err))
		return false, fmt.Errorf("failed to diff Unstructured: %w", err)
	} else if specDiff != "" {
		logger.Info("Reconciling Unstructured diff (-desired, +observed):\n", specDiff)
	}
	return equality.Semantic.DeepEqual(unstructuredGetSpec(desired), unstructuredGetSpec(observed)) &&
		equality.Semantic.DeepEqual(desired.GetLabels(), observed.GetLabels()) &&
		equality.Semantic.DeepEqual(desired.GetAnnotations(), observed.GetAnnotations()) &&
		specDiff == "", nil
}

func unstructuredGetSpec(u *unstructured.Unstructured) map[string]interface{} {
	m, _, _ := unstructured.NestedMap(u.Object, "spec")
	return m
}

func unstructuredSetSpec(u *unstructured.Unstructured, spec map[string]interface{}) {
	if spec == nil {
		unstructured.RemoveNestedField(u.Object, "spec")
		return
	}
	unstructured.SetNestedMap(u.Object, spec, "spec")
}

func joinNewKeys(main, add map[string]string) map[string]string {
	for k, v := range add {
		if _, found := main[k]; !found {
			main[k] = v
		}
	}
	return main
}
