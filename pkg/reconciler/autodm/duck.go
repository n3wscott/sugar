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

package autodm

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
	discoverylistersv1alpha1 "knative.dev/discovery/pkg/client/listers/discovery/v1alpha1"
	"knative.dev/pkg/apis/duck"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/kmp"
	"knative.dev/pkg/logging"
	servingv1alpha1 "knative.dev/serving/pkg/apis/serving/v1alpha1"
	clientset "knative.dev/serving/pkg/client/clientset/versioned"
	servinglistersv1alpha1 "knative.dev/serving/pkg/client/listers/serving/v1alpha1"
	sugarreconciler "knative.dev/sugar/pkg/reconciler"
	"knative.dev/sugar/pkg/sugared"
)

type Reconciler struct {
	addressableDuckInformer duck.InformerFactory
	addressableLister       cache.GenericLister

	cdtLister discoverylistersv1alpha1.ClusterDuckTypeLister

	domainMappingLister servinglistersv1alpha1.DomainMappingLister

	gvk schema.GroupVersionKind
	gvr schema.GroupVersionResource

	client clientset.Interface

	ownerListers map[string]cache.GenericLister

	sugarDispenser *sugared.SugarDispenser
	dc             dynamic.Interface

	confectioner sugared.Confectioner
}

func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logging.FromContext(ctx).Errorw("invalid resource key", zap.String("key", key))
		return nil
	}

	s, err := r.sugarDispenser.Get(ctx, namespace, name, r.gvk)

	if apierrs.IsNotFound(err) {
		// The resource may no longer exist, in which case we stop processing.
		logging.FromContext(ctx).Error("Resource in work queue no longer exists")
		return nil
	} else if err != nil {
		return err
	}

	return r.ReconcileSugar(ctx, s)
}

func (r *Reconciler) ReconcileSugar(ctx context.Context, s *sugared.Sugared) error {
	cfg := s.Config()
	ownerCfg, ownerSugared := r.sugarDispenser.OwnerSugaredDuckConfig(ctx, s)
	if ownerSugared {
		cfg.Subtract(ownerCfg)
	}

	tctx, cancel := context.WithTimeout(ctx, time.Second*3) // TODO: idk, let's try out a static timeout.
	defer cancel()

	if err := r.confectioner.Do(tctx, cfg, func(objects []runtime.Object) error {
		// TODO: test all objects from the confectioner to confirm we have type meta.
		// For any requested resource, create or update it.
		for _, o := range objects {
			if err := r.ensureResource(ctx, s.Resource, o); err != nil {
				logging.FromContext(ctx).Error("failed to ensure resource", zap.Error(err))
			}
		}
		// For any resource that exists but not requested, delete it.
		if dms, err := r.findKindsForOwner(ctx, s.Resource); err != nil {
			logging.FromContext(ctx).Errorw("failed to get list domain mappings for addressable", zap.Error(err)) // TODO: fix comment.
		} else {
			// Compare the annotations we know are on the addressable to the existing domain mappings that are owned by
			// this addressable. We want to delete the ones that exist and are no longer annotated.
			// TODO: fix comment.

			logging.FromContext(ctx).Info("looking for lost domain maps --> ", len(dms)) // TODO: debug

			for _, dm := range dms {
				found := false
				for k, v := range cfg.Annotations {
					if hasHint(ctx, dm, k) {
						if dm.GetObjectMeta().GetName() == v {
							found = true
						}
						break
					}
				}
				if !found {
					logging.FromContext(ctx).Info("did not find the domain map in the annotations, deleting.", zap.String("domainmapping", dm.GetObjectMeta().GetNamespace()+"/"+dm.GetObjectMeta().GetName())) // TODO: fix key
					gvr, _ := meta.UnsafeGuessKindToResource(dm.GetGroupVersionKind())
					if err := r.dc.Resource(gvr).Namespace(dm.GetObjectMeta().GetNamespace()).Delete(ctx, dm.GetObjectMeta().GetName(), metav1.DeleteOptions{}); err != nil {
						logging.FromContext(ctx).Info("failed to delete a domain mapping", zap.String("domainmapping", dm.GetObjectMeta().GetNamespace()+"/"+dm.GetObjectMeta().GetName())) // TODO: fix key
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

func hasHint(ctx context.Context, resource kmeta.OwnerRefable, expected string) bool {
	value, found := resource.GetObjectMeta().GetAnnotations()[sugarreconciler.DomainMappingHintAnnotationKey] // TODO: this is hardcoded to domain mapping... fix.

	// TODO: remove debug.
	logging.FromContext(ctx).Info("hasHint -> ", value, expected, found)

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
	existing, err := r.dc.Resource(gvr).Namespace(resource.GetNamespace()).Get(ctx, resource.GetName(), metav1.GetOptions{})
	if apierrs.IsNotFound(err) {
		_, err = r.dc.Resource(gvr).Namespace(resource.GetNamespace()).Create(ctx, resource, metav1.CreateOptions{})
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

//func (r *Reconciler) ensureDomainMapping(ctx context.Context, owner kmeta.OwnerRefable, dm *servingv1alpha1.DomainMapping) (*servingv1alpha1.DomainMapping, error) {
//	//recorder := controller.GetEventRecorder(ctx)
//
//	// TODO: remove sugar hint from the resource creation and add it here.
//
//	existing, err := r.domainMappingLister.DomainMappings(dm.Namespace).Get(dm.Name)
//	if apierrs.IsNotFound(err) {
//		dm, err = r.createDomainMapping(ctx, dm)
//		if err != nil {
//			//recorder.Eventf(addr, corev1.EventTypeWarning, "CreationFailed", "Failed to create DomainMapping %q: %v", value, err)
//			return nil, fmt.Errorf("failed to create DomainMapping: %w", err)
//		}
//		//recorder.Eventf(addr, corev1.EventTypeNormal, "Created", "Created DomainMapping %q", value)
//	} else if err != nil {
//		return nil, fmt.Errorf("failed to get DomainMapping: %w", err)
//	} else if !metav1.IsControlledBy(existing, owner.GetObjectMeta()) {
//		return nil, fmt.Errorf("addressable[%s]: %q does not own domain mapping: %q", owner.GetGroupVersionKind().Kind, owner.GetObjectMeta().GetName(), existing.Name)
//	} else if dm, err = r.reconcileDomainMapping(ctx, dm, existing); err != nil {
//		return nil, fmt.Errorf("failed to reconcile DomainMapping: %w", err)
//	}
//	return dm, nil
//}

func (r *Reconciler) findKindsForOwner(ctx context.Context, owner kmeta.OwnerRefable) ([]kmeta.OwnerRefable, error) {
	s, err := r.sugarDispenser.List(ctx, owner.GetObjectMeta().GetNamespace(), r.confectioner.Kinds())
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

	_, err = r.dc.Resource(gvr).Namespace(existing.GetNamespace()).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

//func (r *Reconciler) reconcileDomainMapping(ctx context.Context, desired, existing *servingv1alpha1.DomainMapping) (*servingv1alpha1.DomainMapping, error) {
//	existing = existing.DeepCopy()
//	// In the case of an upgrade, there can be default values set that don't exist pre-upgrade.
//	// We are setting the up-to-date default values here so an update won't be triggered if the only
//	// diff is the new default values.
//	existing.SetDefaults(ctx)
//
//	// merge the labels and annotations.
//	joinNewKeys(desired.Labels, existing.Labels)
//	joinNewKeys(desired.Annotations, existing.Annotations)
//
//	equals, err := domainMappingSemanticEquals(ctx, desired, existing)
//	if err != nil {
//		return nil, err
//	}
//	if equals {
//		return existing, nil
//	}
//
//	// Preserve the rest of the object (e.g. ObjectMeta except for labels and annotations).
//	existing.Spec = desired.Spec
//
//	return r.client.ServingV1alpha1().DomainMappings(existing.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
//}
//
//func domainMappingSemanticEquals(ctx context.Context, desired, observed *servingv1alpha1.DomainMapping) (bool, error) {
//	logger := logging.FromContext(ctx)
//	specDiff, err := kmp.SafeDiff(desired.Spec, observed.Spec)
//	if err != nil {
//		logger.Errorw("Error diffing domain mapping spec", zap.Error(err))
//		return false, fmt.Errorf("failed to diff DomanMapping: %w", err)
//	} else if specDiff != "" {
//		logger.Info("Reconciling domain mapping diff (-desired, +observed):\n", specDiff)
//	}
//	return equality.Semantic.DeepEqual(desired.Spec, observed.Spec) &&
//		equality.Semantic.DeepEqual(desired.Labels, observed.Labels) &&
//		equality.Semantic.DeepEqual(desired.Annotations, observed.Annotations) &&
//		specDiff == "", nil
//}
//
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

func (r *Reconciler) createDomainMapping(ctx context.Context, dm *servingv1alpha1.DomainMapping) (*servingv1alpha1.DomainMapping, error) {
	return r.client.ServingV1alpha1().DomainMappings(dm.Namespace).Create(
		ctx, dm, metav1.CreateOptions{})
}

//
//func (r *Reconciler) getAddressable(ctx context.Context, namespace, name string, gvk schema.GroupVersionKind) (*duckv1.AddressableType, error) {
//	lister, found := r.ownerListers[gvk.String()]
//	if !found {
//		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
//		_, l, err := r.addressableDuckInformer.Get(ctx, gvr)
//		if err != nil {
//			return nil, err
//		}
//		lister = l
//		r.ownerListers[gvk.String()] = l
//	}
//
//	// Get the Addressable resource with this namespace/name
//	runtimeObj, err := lister.ByNamespace(namespace).Get(name)
//	if err != nil {
//		logging.FromContext(ctx).Errorw("unable to get addressable", zap.String("gvk", gvk.String()), zap.String("key", namespace+"/"+name))
//		return nil, err
//	}
//
//	var ok bool
//	var original *duckv1.AddressableType
//	if original, ok = runtimeObj.(*duckv1.AddressableType); !ok {
//		logging.FromContext(ctx).Errorw("runtime object is not convertible to Addressable duck type: ", zap.Any("runtimeObj", runtimeObj))
//		// Avoid re-enqueuing.
//		return nil, errors.New("not an addressable duck type")
//	}
//	return original, nil
//}
//
func joinNewKeys(main, add map[string]string) map[string]string {
	for k, v := range add {
		if _, found := main[k]; !found {
			main[k] = v
		}
	}
	return main
}

//
//// todo: rename to something smart
//func domainMapping(annotations map[string]string) map[string]string {
//
//	// TODO: this gets complicated real quick, we need to navigate up the owner graph and let the parent resource
//	// make the map if it is annotated with the same annotation.
//
//	dms := make(map[string]string)
//	for k, v := range annotations {
//		if strings.HasPrefix(k, sugarreconciler.DomainMappingAnnotationKey) {
//			// TODO: we could split off the index from the annotation key, or confirm it is suffixed with only a number, for now YOLO.
//			dms[k] = v
//		}
//	}
//	return dms
//}
