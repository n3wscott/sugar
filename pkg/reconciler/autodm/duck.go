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
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/labels"
	"knative.dev/pkg/kmp"
	"strings"

	"go.uber.org/zap"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	discoverylistersv1alpha1 "knative.dev/discovery/pkg/client/listers/discovery/v1alpha1"
	"knative.dev/pkg/apis/duck"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/logging"
	servingv1alpha1 "knative.dev/serving/pkg/apis/serving/v1alpha1"
	clientset "knative.dev/serving/pkg/client/clientset/versioned"
	servinglistersv1alpha1 "knative.dev/serving/pkg/client/listers/serving/v1alpha1"
	sugarreconciler "knative.dev/sugar/pkg/reconciler"
	"knative.dev/sugar/pkg/reconciler/autodm/resources"
)

type Reconciler struct {
	addressableDuckInformer duck.InformerFactory
	addressableLister       cache.GenericLister

	cdtLister discoverylistersv1alpha1.ClusterDuckTypeLister

	domainMappingLister servinglistersv1alpha1.DomainMappingLister

	gvr schema.GroupVersionResource

	client clientset.Interface

	ownerListers map[string]cache.GenericLister
}

func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logging.FromContext(ctx).Errorw("invalid resource key", zap.String("key", key))
		return nil
	}

	// Get the Addressable resource with this namespace/name
	runtimeObj, err := r.addressableLister.ByNamespace(namespace).Get(name)
	if err != nil {
		logging.FromContext(ctx).Errorw("unable to get addressable", zap.String("key", key))
		return nil
	}

	var ok bool
	var original *duckv1.AddressableType
	if original, ok = runtimeObj.(*duckv1.AddressableType); !ok {
		logging.FromContext(ctx).Errorw("runtime object is not convertible to Addressable duck type: ", zap.Any("runtimeObj", runtimeObj))
		// Avoid re-enqueuing.
		return nil
	}

	if apierrs.IsNotFound(err) {
		// The resource may no longer exist, in which case we stop processing.
		logging.FromContext(ctx).Error("Addressable in work queue no longer exists")
		return nil
	} else if err != nil {
		return err
	}

	// Don't modify the informers copy
	orig := original.DeepCopy()
	// Reconcile this copy of the Addressable. We do not control the Addressable, so do not update status.
	return r.reconcile(ctx, orig)
}

func (r *Reconciler) reconcile(ctx context.Context, addr *duckv1.AddressableType) error {
	logging.FromContext(ctx).Info("reconcile an addressable ", addr.Kind, " ", addr.Name)

	addressables := make(map[string]bool)

	if dt, err := r.cdtLister.Get(sugarreconciler.Addressables); err != nil {
		logging.FromContext(ctx).Debug("failed to get cluster duck type for "+sugarreconciler.Addressables, zap.Error(err))
	} else {
		for _, v := range dt.Status.Ducks[sugarreconciler.AddressablesVersion] {
			key := fmt.Sprintf("%s.%s", v.Kind, v.Group())
			addressables[key] = true
		}
	}

	_ = addressables

	//owner domain maps
	odm := make(map[string]string)
	for _, owner := range addr.GetOwnerReferences() {
		gvk := schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)
		key := fmt.Sprintf("%s.%s", gvk.Kind, gvk.Group)
		// if the owner is also an addressable, go get the owners domainMapping labels.
		if addressables[key] {
			if dt, err := r.getAddressable(ctx, addr.Namespace, owner.Name, gvk); err != nil {
				logging.FromContext(ctx).Debug("failed to get addressable owner", zap.Error(err))
			} else {
				joinMap(odm, domainMapping(dt.Annotations))
			}
		}
	}

	foundAnnotations := domainMapping(addr.Annotations)
	// Look for new annotations
	for k, v := range foundAnnotations {
		if _, found := odm[k]; !found {
			logging.FromContext(ctx).Info("===> map ", addr.Kind, " ", addr.Name, " to ", v)
			if _, err := r.ensureDomainMapping(ctx, addr, k, v); err != nil {
				logging.FromContext(ctx).Error("failed to ensure domain mapping", zap.Error(err))
			}
		} else {
			logging.FromContext(ctx).Info("~~~> owned ", addr.Kind, " ", addr.Name, " to ", v)
		}
	}

	// Look for deleted annotations.
	if dms, err := r.findDomainMappingsForOwner(addr); err != nil {
		logging.FromContext(ctx).Debug("failed to get list domain mappings for addressable", zap.Error(err))
	} else {
		// Compare the annotations we know are on the addressable to the existing domain mappings that are owned by
		// this addressable. We want to delete the ones that exist and are no longer annotated.
		for _, dm := range dms {
			found := false
			for k, v := range foundAnnotations {
				if hasHint(ctx, dm, k) {
					if dm.Name == v {
						found = true
					}
					break
				}
			}
			if !found {
				logging.FromContext(ctx).Info("did not find the domain map in the annotations, deleting.", zap.String("domainmapping", dm.Namespace+"/"+dm.Name))
				if err := r.client.ServingV1alpha1().DomainMappings(dm.Namespace).Delete(ctx, dm.Name, metav1.DeleteOptions{}); err != nil {
					logging.FromContext(ctx).Info("failed to delete a domain mapping", zap.String("domainmapping", dm.Namespace+"/"+dm.Name), zap.Error(err))
				}
			}
		}
	}
	return nil
}

func hasHint(ctx context.Context, dm *servingv1alpha1.DomainMapping, expected string) bool {
	value, found := dm.Annotations[sugarreconciler.DomainMappingHintAnnotationKey]

	// TODO: remove debug.
	logging.FromContext(ctx).Info("hasHint -> ", value, expected, found)

	if !found {
		return false
	}
	return value == expected
}

func (r *Reconciler) ensureDomainMapping(ctx context.Context, addr *duckv1.AddressableType, key, value string) (*servingv1alpha1.DomainMapping, error) {
	//recorder := controller.GetEventRecorder(ctx)

	// TODO: the domain mapping hint needs to match the key we are passed here, or it is the wrong domain map.

	dm := resources.MakeDomainMapping(&resources.DomainMappingArgs{
		Name:  value,
		Hint:  key,
		Owner: addr,
		Ref: duckv1.KReference{
			Kind:       addr.Kind,
			Namespace:  addr.Namespace,
			Name:       addr.Name,
			APIVersion: addr.APIVersion,
		},
	})
	existing, err := r.domainMappingLister.DomainMappings(dm.Namespace).Get(dm.Name)
	if apierrs.IsNotFound(err) {
		dm, err = r.createDomainMapping(ctx, dm)
		if err != nil {
			//recorder.Eventf(addr, corev1.EventTypeWarning, "CreationFailed", "Failed to create DomainMapping %q: %v", value, err)
			return nil, fmt.Errorf("failed to create DomainMapping: %w", err)
		}
		//recorder.Eventf(addr, corev1.EventTypeNormal, "Created", "Created DomainMapping %q", value)
	} else if err != nil {
		return nil, fmt.Errorf("failed to get DomainMapping: %w", err)
	} else if !metav1.IsControlledBy(existing, addr) {
		return nil, fmt.Errorf("addressable[%s]: %q does not own domain mapping: %q", addr.Kind, addr.Name, value)
	} else if dm, err = r.reconcileDomainMapping(ctx, dm, existing); err != nil {
		return nil, fmt.Errorf("failed to reconcile DomainMapping: %w", err)
	}
	return dm, nil
}

func (r *Reconciler) findDomainMappingsForOwner(addr *duckv1.AddressableType) ([]*servingv1alpha1.DomainMapping, error) {
	selector, err := labels.Parse(fmt.Sprintf("%s=%s", sugarreconciler.SugarOwnerLabelKey, sugarreconciler.AutoDomainMappingLabel))
	if err != nil {
		return nil, fmt.Errorf("failed to produce label selector: %w", err)
	}

	dms, err := r.domainMappingLister.DomainMappings(addr.Namespace).List(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to list DomainMapping: %w", err)
	}

	// Filter for the owner.
	ownedDMs := make([]*servingv1alpha1.DomainMapping, 0)
	for _, dm := range dms {
		owner := metav1.GetControllerOf(dm)
		if owner != nil &&
			owner.APIVersion == addr.APIVersion &&
			owner.Kind == addr.Kind &&
			owner.Name == addr.Name &&
			dm.Namespace == addr.Namespace {
			ownedDMs = append(ownedDMs, dm)
		}

	}
	return ownedDMs, nil
}

func (r *Reconciler) reconcileDomainMapping(ctx context.Context, desired, existing *servingv1alpha1.DomainMapping) (*servingv1alpha1.DomainMapping, error) {
	existing = existing.DeepCopy()
	// In the case of an upgrade, there can be default values set that don't exist pre-upgrade.
	// We are setting the up-to-date default values here so an update won't be triggered if the only
	// diff is the new default values.
	existing.SetDefaults(ctx)
	equals, err := domainMappingSemanticEquals(ctx, desired, existing)
	if err != nil {
		return nil, err
	}
	if equals {
		return existing, nil
	}

	// Preserve the rest of the object (e.g. ObjectMeta except for labels and annotations).
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	existing.Annotations = desired.Annotations
	return r.client.ServingV1alpha1().DomainMappings(existing.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
}

func domainMappingSemanticEquals(ctx context.Context, desired, observed *servingv1alpha1.DomainMapping) (bool, error) {
	logger := logging.FromContext(ctx)
	specDiff, err := kmp.SafeDiff(desired.Spec, observed.Spec)
	if err != nil {
		logger.Errorw("Error diffing domain mapping spec", zap.Error(err))
		return false, fmt.Errorf("failed to diff DomanMapping: %w", err)
	} else if specDiff != "" {
		logger.Info("Reconciling domain mapping diff (-desired, +observed):\n", specDiff)
	}
	return equality.Semantic.DeepEqual(desired.Spec, observed.Spec) &&
		equality.Semantic.DeepEqual(desired.Labels, observed.Labels) &&
		equality.Semantic.DeepEqual(desired.Annotations, observed.Annotations) &&
		specDiff == "", nil
}

func (r *Reconciler) createDomainMapping(ctx context.Context, dm *servingv1alpha1.DomainMapping) (*servingv1alpha1.DomainMapping, error) {
	return r.client.ServingV1alpha1().DomainMappings(dm.Namespace).Create(
		ctx, dm, metav1.CreateOptions{})
}

func (r *Reconciler) getAddressable(ctx context.Context, namespace, name string, gvk schema.GroupVersionKind) (*duckv1.AddressableType, error) {
	lister, found := r.ownerListers[gvk.String()]
	if !found {
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		_, l, err := r.addressableDuckInformer.Get(ctx, gvr)
		if err != nil {
			return nil, err
		}
		lister = l
		r.ownerListers[gvk.String()] = l
	}

	// Get the Addressable resource with this namespace/name
	runtimeObj, err := lister.ByNamespace(namespace).Get(name)
	if err != nil {
		logging.FromContext(ctx).Errorw("unable to get addressable", zap.String("gvk", gvk.String()), zap.String("key", namespace+"/"+name))
		return nil, err
	}

	var ok bool
	var original *duckv1.AddressableType
	if original, ok = runtimeObj.(*duckv1.AddressableType); !ok {
		logging.FromContext(ctx).Errorw("runtime object is not convertible to Addressable duck type: ", zap.Any("runtimeObj", runtimeObj))
		// Avoid re-enqueuing.
		return nil, errors.New("not an addressable duck type")
	}
	return original, nil
}

func joinMap(main, add map[string]string) {
	for k, v := range add {
		main[k] = v
	}
}

// todo: rename to something smart
func domainMapping(annotations map[string]string) map[string]string {

	// TODO: this gets complicated real quick, we need to navigate up the owner graph and let the parent resource
	// make the map if it is annotated with the same annotation.

	dms := make(map[string]string)
	for k, v := range annotations {
		if strings.HasPrefix(k, sugarreconciler.DomainMappingAnnotationKey) {
			// TODO: we could split off the index from the annotation key, or confirm it is suffixed with only a number, for now YOLO.
			dms[k] = v
		}
	}
	return dms
}
