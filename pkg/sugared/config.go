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
	"knative.dev/pkg/kmeta"
)

type Config struct {
	Raw         *Turbinado
	Annotations map[string]string
	Labels      map[string]string
}

func (c *Config) Hint() string {
	return c.Raw.Prefix + ".hint"
}

func (c *Config) ApplyHint(resource kmeta.OwnerRefable, hint string) {
	a := resource.GetObjectMeta().GetAnnotations()
	a[c.Hint()] = hint
	resource.GetObjectMeta().SetAnnotations(a)
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
