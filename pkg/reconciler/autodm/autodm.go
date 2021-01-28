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
		apiVersion, kind := cfg.Sugar.Resource.GetGroupVersionKind().ToAPIVersionAndKind()
		for key, value := range cfg.Annotations {
			dms = append(dms, resources.MakeDomainMapping(&resources.DomainMappingArgs{
				Name:  value,
				Hint:  key,
				Owner: cfg.Sugar.Resource,
				Ref: duckv1.KReference{
					Kind:       kind,
					Namespace:  cfg.Sugar.Resource.GetObjectMeta().GetNamespace(),
					Name:       cfg.Sugar.Resource.GetObjectMeta().GetName(),
					APIVersion: apiVersion,
				},
			}))
		}
	}

	if err := fn(dms); err != nil {
		logging.FromContext(ctx).Errorw("failed to realize domain maps",
			zap.String("key", cfg.Sugar.Resource.GetObjectMeta().GetNamespace()+"/"+cfg.Sugar.Resource.GetObjectMeta().GetName()),
			zap.String("gvk", cfg.Sugar.Resource.GetGroupVersionKind().String()),
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
