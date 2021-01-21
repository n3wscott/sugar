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

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/injection"
	"knative.dev/pkg/logging"

	addressaleinformer "knative.dev/pkg/client/injection/ducks/duck/v1/addressable"
)

const (
	// ReconcilerName is the name of the reconciler.
	ReconcilerName = "Addressables"
)

// NewController returns a function that initializes the controller and
// Registers event handlers to enqueue events
func NewController(gvr schema.GroupVersionResource) injection.ControllerConstructor {
	return func(ctx context.Context,
		cmw configmap.Watcher,
	) *controller.Impl {
		logger := logging.FromContext(ctx)
		addressableduckInformer := addressaleinformer.Get(ctx)

		addressableInformer, addressableLister, err := addressableduckInformer.Get(ctx, gvr)
		if err != nil {
			logger.Errorw("Error getting source informer", zap.String("GVR", gvr.String()), zap.Error(err))
			return nil
		}

		r := &Reconciler{
			addressableLister: addressableLister,
			gvr:               gvr,
		}
		impl := controller.NewImplFull(r, controller.ControllerOptions{WorkQueueName: ReconcilerName + gvr.String(), Logger: logger})

		logger.Info("Setting up event handlers")
		addressableInformer.AddEventHandler(controller.HandleAll(impl.Enqueue))

		// TODO: be informed on DomainMappings mutations.

		return impl
	}
}
