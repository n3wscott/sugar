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
	"knative.dev/pkg/injection/clients/dynamicclient"
	"knative.dev/sugar/pkg/reconciler"
	"knative.dev/sugar/pkg/reconciler/sugarduck"
	"knative.dev/sugar/pkg/sugared"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/logging"

	addressaleinformer "knative.dev/pkg/client/injection/ducks/duck/v1/addressable"
	domainmappinginformer "knative.dev/serving/pkg/client/injection/informers/serving/v1alpha1/domainmapping"
)

const (
	// ReconcilerName is the name of the reconciler.
	ReconcilerName = "SugarDuck"
)

// NewController returns a function that initializes the controller and
// Registers event handlers to enqueue events
func NewController(gvk schema.GroupVersionKind) injection.ControllerConstructor {
	return func(ctx context.Context,
		cmw configmap.Watcher,
	) *controller.Impl {
		logger := logging.FromContext(ctx)
		addressableduckInformer := addressaleinformer.Get(ctx)
		domainMapInformer := domainmappinginformer.Get(ctx)
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		addressableInformer, _, err := addressableduckInformer.Get(ctx, gvr)
		if err != nil {
			logger.Errorw("Error getting source informer", zap.String("GVR", gvr.String()), zap.Error(err))
			return nil
		}

		sugarDispenser := sugared.NewDispenser(ctx,
			reconciler.Addressables, reconciler.AddressablesVersion, // Duck type
			reconciler.DomainMappingAnnotationKey, reconciler.AutoDomainMappingLabel, // Sugar
			addressableduckInformer)

		r := &sugarduck.Reconciler{
			GVK:            gvk,
			SugarDispenser: sugarDispenser,
			Confectioner:   NewAutoDM(),
			Dynamic:        dynamicclient.Get(ctx),
		}
		impl := controller.NewImplFull(r, controller.ControllerOptions{WorkQueueName: ReconcilerName + gvr.String(), Logger: logger})

		logger.Info("Setting up event handlers")
		// Watch for all updates for the addressable.
		addressableInformer.AddEventHandler(controller.HandleAll(impl.Enqueue))

		// Also enqueue changes that are owned by this addressable.
		handleControllerOf := cache.FilteringResourceEventHandler{
			FilterFunc: controller.FilterControllerGK(gvk.GroupKind()),
			Handler:    controller.HandleAll(impl.EnqueueControllerOf),
		}

		// For all domain mappings that are owned by this addressable, enqueue the owner.
		domainMapInformer.Informer().AddEventHandler(handleControllerOf)

		return impl
	}
}
