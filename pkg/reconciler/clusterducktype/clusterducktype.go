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

package clusterducktype

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	discoveryv1alpha1 "knative.dev/discovery/pkg/apis/discovery/v1alpha1"
	netclientset "knative.dev/networking/pkg/client/clientset/versioned"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/reconciler"
	sugarreconciler "knative.dev/sugar/pkg/reconciler"
	"knative.dev/sugar/pkg/reconciler/autodm"
)

type runningController struct {
	gvr        schema.GroupVersionResource
	controller *controller.Impl
	cancel     context.CancelFunc
}

type Reconciler struct {
	netclient netclientset.Interface

	ogctx context.Context
	ogcmw configmap.Watcher

	// Local state

	controllers map[string]runningController
	lock        sync.Mutex
}

// ReconcileKind implements Interface.ReconcileKind.
func (r *Reconciler) ReconcileKind(ctx context.Context, dt *discoveryv1alpha1.ClusterDuckType) reconciler.Event {
	if dt.GetName() == sugarreconciler.Addressables {
		return r.reconcileAddressables(ctx, dt)
	}
	return nil
}

func (r *Reconciler) reconcileAddressables(ctx context.Context, dt *discoveryv1alpha1.ClusterDuckType) reconciler.Event {
	if r.controllers == nil {
		r.controllers = make(map[string]runningController)
	}

	for _, v := range dt.Status.Ducks[sugarreconciler.AddressablesVersion] {
		key := fmt.Sprintf("%s.%s", v.Kind, v.Group())
		logging.FromContext(ctx).Info("going to use addressable - ", key)

		if rc, found := r.controllers[key]; !found {
			gvk := schema.GroupVersionKind{
				Group:   v.Group(),
				Version: v.Version(),
				Kind:    v.Kind,
			}
			gvr, _ := meta.UnsafeGuessKindToResource(gvk)
			cc := autodm.NewController(gvr)

			atctx, cancel := context.WithCancel(r.ogctx)
			// Auto Trigger
			impl := cc(atctx, r.ogcmw)

			rc = runningController{
				gvr:        gvr,
				controller: impl,
				cancel:     cancel,
			}

			r.lock.Lock()
			r.controllers[key] = rc
			r.lock.Unlock()

			logging.FromContext(ctx).Infof("starting auto domain mapping reconciler for gvr %q", rc.gvr.String())
			go func(c *controller.Impl) {
				if err := c.Run(2, atctx.Done()); err != nil {
					logging.FromContext(ctx).Errorf("unable to start auto domain mapping reconciler for gvr %q", rc.gvr.String())
				}
			}(rc.controller)
		}
	}

	logging.FromContext(ctx).Infof("-----Auto Domain Mapping-------")
	for k, _ := range r.controllers {
		logging.FromContext(ctx).Infof(" - %q", k)
	}

	logging.FromContext(ctx).Infof("==========================")
	return nil
}
