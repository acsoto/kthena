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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func recoveryModelServing() *workload.ModelServing {
	template := workload.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Image: "image:v1"}},
	}}
	return &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "recovery", Namespace: "default", UID: "recovery"},
		Spec: workload.ModelServingSpec{
			Replicas: ptr.To[int32](1), SchedulerName: "volcano",
			Template: workload.ServingGroup{Roles: []workload.Role{{
				Name: "decode", Replicas: ptr.To[int32](3), EntryTemplate: template,
				WorkerReplicas: 1, WorkerTemplate: template.DeepCopy(),
			}}},
		},
	}
}

func TestHistoricalWorkerRecoveryPreservesSurvivingEntry(t *testing.T) {
	ctx := context.Background()
	ms := recoveryModelServing()
	kube := kubefake.NewSimpleClientset()
	c, err := NewModelServingController(kube, kthenafake.NewSimpleClientset(ms), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	data, err := utils.BuildRevisionData(ms)
	require.NoError(t, err)
	cr, _, err := utils.RecordModelServingRevision(ctx, kube, ms, data)
	require.NoError(t, err)
	revision := cr.Labels[utils.ControllerRevisionRevisionLabelKey]
	role := ms.Spec.Template.Roles[0]
	hash := utils.CalRoleTemplateHash(role)
	require.NoError(t, c.CreatePodsByRole(ctx, role, ms, 0, 0, revision, hash))
	pods, err := kube.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	for _, pod := range pods.Items {
		if pod.Labels[workload.EntryLabelKey] != utils.Entry {
			require.NoError(t, kube.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
		}
	}
	current := ms.DeepCopy()
	current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
	restoredRole, restoredMS, restoredRevision, restoredHash, err := c.roleTemplateForReplica(ctx, current, current.Spec.Template.Roles[0], datastore.Role{
		Name: "decode-0", Revision: revision, RoleTemplateHash: hash,
	}, "new-revision", true)
	require.NoError(t, err)
	require.NoError(t, c.CreatePodsByRole(ctx, restoredRole, restoredMS, 0, 0, restoredRevision, restoredHash))
	pods, err = kube.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 2)
	for _, pod := range pods.Items {
		require.Equal(t, revision, utils.ObjectRevision(&pod))
		require.Equal(t, hash, utils.ObjectRoleTemplateHash(&pod))
	}
}

func TestOwnerOnlyUpdateEnqueuesModelServing(t *testing.T) {
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	old := recoveryModelServing()
	current := old.DeepCopy()
	current.OwnerReferences = []metav1.OwnerReference{{Kind: "LeaderWorkerSet", Name: "owner", UID: "owner"}}
	c.updateModelServing(old, current)
	require.Equal(t, 1, c.workqueue.Len())
}
