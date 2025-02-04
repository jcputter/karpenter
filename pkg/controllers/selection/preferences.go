/*
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

package selection

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter/pkg/utils/pretty"
)

const (
	ExpirationTTL   = 5 * time.Minute
	CleanupInterval = 1 * time.Minute
)

type Preferences struct {
	cache *cache.Cache
}

func NewPreferences() *Preferences {
	return &Preferences{
		cache: cache.New(ExpirationTTL, CleanupInterval),
	}
}

// Relax removes soft preferences from pod to enable scheduling if the cloud
// provider's capacity is constrained. For example, this can be leveraged to
// prefer a specific zone, but relax the preferences if the pod cannot be
// scheduled to that zone. Preferences are removed iteratively until only hard
// constraints remain. Pods relaxation is reset (forgotten) after 5 minutes.
func (p *Preferences) Relax(ctx context.Context, pod *v1.Pod) {
	spec, ok := p.cache.Get(string(pod.UID))
	// Add to cache if we've never seen it before
	if !ok {
		// Limit cached PodSpec to only required data
		cachedSpec := v1.PodSpec{
			Affinity:    pod.Spec.Affinity,
			Tolerations: pod.Spec.Tolerations,
		}
		p.cache.SetDefault(string(pod.UID), cachedSpec)
		return
	}
	// Attempt to relax the pod and update the cache
	cachedSpec := spec.(v1.PodSpec)
	pod.Spec.Affinity = cachedSpec.Affinity
	pod.Spec.Tolerations = cachedSpec.Tolerations
	if relaxed := p.relax(ctx, pod); relaxed {
		p.cache.SetDefault(string(pod.UID), pod.Spec)
	}
}

func (p *Preferences) relax(ctx context.Context, pod *v1.Pod) bool {
	for _, relaxFunc := range []func(*v1.Pod) *string{
		func(pod *v1.Pod) *string { return p.removePreferredNodeAffinityTerm(pod) },
		func(pod *v1.Pod) *string { return p.removeRequiredNodeAffinityTerm(pod) },
		func(pod *v1.Pod) *string { return p.toleratePreferNoScheduleTaints(pod) },
	} {
		if reason := relaxFunc(pod); reason != nil {
			logging.FromContext(ctx).Debugf("Relaxing soft constraints for pod since it previously failed to schedule, %s", ptr.StringValue(reason))
			return true
		}
	}
	return false
}

func (p *Preferences) removePreferredNodeAffinityTerm(pod *v1.Pod) *string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil || len(pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		return nil
	}
	terms := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	// Remove the terms if there are any (terms are an OR semantic)
	if len(terms) > 0 {
		// Sort descending by weight to remove heaviest preferences to try lighter ones
		sort.SliceStable(terms, func(i, j int) bool { return terms[i].Weight > terms[j].Weight })
		pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = terms[1:]
		return ptr.String(fmt.Sprintf("removing: spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0]=%s", pretty.Concise(terms[0])))
	}
	return nil
}

func (p *Preferences) removeRequiredNodeAffinityTerm(pod *v1.Pod) *string {
	if pod.Spec.Affinity == nil ||
		pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		len(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		return nil
	}
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	// Remove the first term if there's more than one (terms are an OR semantic), Unlike preferred affinity, we cannot remove all terms
	if len(terms) > 1 {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = terms[1:]
		return ptr.String(fmt.Sprintf("removing: spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]=%s", pretty.Concise(terms[0])))
	}
	return nil
}

func (p *Preferences) toleratePreferNoScheduleTaints(pod *v1.Pod) *string {
	// Tolerate all Taints with PreferNoSchedule effect
	toleration := v1.Toleration{
		Operator: v1.TolerationOpExists,
		Effect:   v1.TaintEffectPreferNoSchedule,
	}
	for _, t := range pod.Spec.Tolerations {
		if t.MatchToleration(&toleration) {
			return nil
		}
	}
	tolerations := append(pod.Spec.Tolerations, toleration)
	pod.Spec.Tolerations = tolerations
	return ptr.String("adding: toleration for PreferNoSchedule taints")
}
