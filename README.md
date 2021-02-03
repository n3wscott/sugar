# Sugar controller

This repo is a proof of concept of a reconciler focused around reconciling duck
type kinds and a simple delegate that will request some set of resources as a
result of labels and annotations on instances of duck type kinds.

## Demo

Given a ClusterDuckType

```
$ kubectl get clusterducktype addressables.duck.knative.dev -oyaml
apiVersion: discovery.knative.dev/v1alpha1
kind: ClusterDuckType
metadata:
  name: addressables.duck.knative.dev
spec:
  group: duck.knative.dev
  names:
    name: Addressable
    plural: addressables
    singular: addressable
...
status:
  conditions:
  - lastTransitionTime: "2021-01-20T20:54:49Z"
    status: "True"
    type: Ready
  duckCount: 8
  ducks:
    v1:
    - apiVersion: eventing.knative.dev/v1
      kind: Broker
      scope: Namespaced
    - apiVersion: eventing.knative.dev/v1beta1
      kind: Broker
      scope: Namespaced
    - apiVersion: flows.knative.dev/v1
      kind: Parallel
      scope: Namespaced
    - apiVersion: flows.knative.dev/v1
      kind: Sequence
      scope: Namespaced
    - apiVersion: flows.knative.dev/v1beta1
      kind: Parallel
      scope: Namespaced
    - apiVersion: flows.knative.dev/v1beta1
      kind: Sequence
      scope: Namespaced
    - apiVersion: messaging.knative.dev/v1
      kind: Channel
      scope: Namespaced
    - apiVersion: messaging.knative.dev/v1
      kind: InMemoryChannel
      scope: Namespaced
    - apiVersion: messaging.knative.dev/v1beta1
      kind: Channel
      scope: Namespaced
    - apiVersion: messaging.knative.dev/v1beta1
      kind: InMemoryChannel
      scope: Namespaced
    - apiVersion: serving.knative.dev/v1
      kind: Route
      scope: Namespaced
    - apiVersion: serving.knative.dev/v1
      kind: Service
      scope: Namespaced
    - apiVersion: v1
      kind: Service
      scope: Namespaced
  observedGeneration: 2
```

We will watch these instances of ducks in the cluster for a special annotation:

```
metadata:
  annotations:
    sugar.knative.dev/domainmapping: helloworld.getrekt.dev
```

The annotations and labels are filtered and collected and candidates are passed
to the AutoDM instance's `Do` method:

```
// (simplified a little)
func (a *AutoDM) Do(ctx context.Context, cfg *sugared.Config, fn sugared.RealizeFn) error {
	dms := make([]runtime.Object, 0)
	if cfg.Sugared() {
		apiVersion, kind := cfg.Raw.Resource.GetGroupVersionKind().ToAPIVersionAndKind()
		for key, value := range cfg.Annotations {
			dm := resources.MakeDomainMapping(&resources.DomainMappingArgs{
				Name:  value,
				Owner: cfg.Raw.Resource,
				Ref: duckv1.KReference{
					Kind:       kind,
					Namespace:  cfg.Raw.Resource.GetObjectMeta().GetNamespace(),
					Name:       cfg.Raw.Resource.GetObjectMeta().GetName(),
					APIVersion: apiVersion,
				},
			})
			// Apply the hint so we can find the index again.
			cfg.ApplyHint(dm, key)
			dms = append(dms, dm)
		}
	}

	if err := fn(dms) {
    return err
	}

	return nil
}
```

This works a lot like metacontroller, in that the returned list of objects are
the source of truth from what we would like to exist as a result of the sugar
and nothing more. So then for each sugared (or non-sugared) duck instance we
create, update, or delete owned resulting resources (in this case
DomainMappings).

A demo can be seen here: [demo-autodm](https://github.com/n3wscott/demo-autodm).

By labeling an addressable, in this case a Knative Service:

```
kubectl patch ksvc hello --type=json --patch '[{"op": "add", "path": "/metadata/annotations/sugar.knative.dev~1domainmapping", "value": "getrekt.dev"}]'
```

We get a resulting domain mapping:

```
$ kubectl get domainmapping
NAME                     URL                              READY   REASON
getrekt.dev              https://getrekt.dev              True
```

And the domainmapping is owned by the labeled resource:

```
$ kubectl get domainmapping getrekt.dev -oyaml
apiVersion: serving.knative.dev/v1alpha1
kind: DomainMapping
metadata:
  annotations:
    sugar.knative.dev/domainmapping.hint: ""
  labels:
    sugar.knative.dev/owner: domainmapping
  name: getrekt.dev
  namespace: default
  ownerReferences:
  - apiVersion: serving.knative.dev/v1
    blockOwnerDeletion: true
    controller: true
    kind: Service
    name: hello
```

There are some hints and labels added as well to aid in the PoC finding
resources it should be healing.
