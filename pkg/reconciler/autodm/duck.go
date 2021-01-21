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
	"strings"

	"go.uber.org/zap"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/logging"
)

type Reconciler struct {
	addressableLister cache.GenericLister

	gvr schema.GroupVersionResource
}

const (
	annotation = "sugar.knative.dev/domainmapping"
)

func (r *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logging.FromContext(ctx).Errorw("invalid resource key", zap.String("key", key))
		return nil
	}

	// Get the Addressable resource with this namespace/name
	runtimeObj, err := r.addressableLister.ByNamespace(namespace).Get(name)

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

	// Look for new annotations
	for _, m := range mappings(addr.Annotations) {
		logging.FromContext(ctx).Info("===> map ", addr.Kind, " ", addr.Name, " to ", m)
	}

	// Look for deleted annotations.
	// TODO implement

	return nil
}

// todo: rename to something smart
func mappings(annotations map[string]string) []string {

	// TODO: this gets complicated real quick, we need to navigate up the owner graph and let the parent resource
	// make the map if it is annotated with the same annotation.

	var dms []string
	for k, v := range annotations {
		if strings.HasPrefix(k, annotation) {
			// TODO: we could split off the index from the annotation key, or confirm it is suffixed with only a number, for now YOLO.
			dms = append(dms, v)
		}
	}
	return dms
}
