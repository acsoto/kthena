/*
Copyright The Volcano Authors.
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

package plugins

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestLWSLabelsPluginUsesCurrentModelServingOwner(t *testing.T) {
	plugin := &LWSLabelsPlugin{name: LWSLabelsPluginName}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
			{Kind: "LeaderWorkerSet", Name: "current-lws", UID: "current"},
		}},
	}
	pod := &corev1.Pod{}
	request := &HookRequest{
		ModelServing: ms,
		ServingGroup: "model-serving-2",
		IsEntry:      true,
		Pod:          pod,
	}

	if err := plugin.OnPodCreate(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := pod.Labels[leaderworkerset.SetNameLabelKey]; got != "current-lws" {
		t.Fatalf("set-name label = %q, want current-lws", got)
	}
}
