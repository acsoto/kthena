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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestRolePartitionRecoveryUsesHistoricalPodCount(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		oldWorkers, newWorkers int32
		deletedPodIndex        int
	}{
		{name: "increased workers", oldWorkers: 0, newWorkers: 2, deletedPodIndex: 0},
		{name: "decreased workers", oldWorkers: 2, newWorkers: 0, deletedPodIndex: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			kubeClient := kubefake.NewSimpleClientset()
			c, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
			require.NoError(t, err)
			partition := intstr.FromInt(1)
			template := workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "old-image"}}}}
			oldMS := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "role-recovery", Namespace: "default", UID: "role-recovery-uid"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1), SchedulerName: "volcano", RecoveryPolicy: workloadv1alpha1.NoneRestartPolicy,
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate},
					Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
						Name: "decode", Replicas: ptr.To[int32](1), WorkerReplicas: tt.oldWorkers,
						EntryTemplate: template, WorkerTemplate: template.DeepCopy(),
						RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{Partition: &partition},
					}}},
				},
			}
			data, err := utils.BuildRevisionData(oldMS)
			require.NoError(t, err)
			revision, _, err := utils.RecordModelServingRevision(ctx, kubeClient, oldMS, data)
			require.NoError(t, err)
			oldRevision := revision.Labels[utils.ControllerRevisionRevisionLabelKey]
			oldMS.Status.CurrentRevision = oldRevision
			oldMS.Status.UpdateRevision = oldRevision
			key := utils.GetNamespaceName(oldMS)
			groupName := utils.GenerateServingGroupName(oldMS.Name, 0)
			roleID := utils.GenerateRoleID("decode", 0)
			c.store.AddServingGroup(key, 0, oldRevision)
			require.NoError(t, c.CreatePodsForServingGroup(ctx, oldMS, 0, oldRevision))
			pods, err := kubeClient.CoreV1().Pods(oldMS.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			for i := range pods.Items {
				pod := &pods.Items[i]
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
				require.NoError(t, c.handleReadyPod(oldMS, groupName, pod))
			}
			require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, groupName))
			ms := oldMS.DeepCopy()
			ms.Spec.Template.Roles[0].WorkerReplicas = tt.newWorkers
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "new-image"
			ms.Status.UpdateRevision = "new-revision"
			deletedName := utils.GeneratePodName(groupName, roleID, tt.deletedPodIndex)
			deleted, err := c.podsLister.Pods(ms.Namespace).Get(deletedName)
			require.NoError(t, err)
			require.NoError(t, kubeClient.CoreV1().Pods(ms.Namespace).Delete(ctx, deletedName, metav1.DeleteOptions{}))
			require.NoError(t, c.podsInformer.GetIndexer().Delete(deleted))
			c.store.DeleteRunningPodFromServingGroup(key, groupName, deletedName)
			require.NoError(t, c.handleDeletedPod(ms, groupName, deleted))
			require.NoError(t, c.syncRoleReplicas(ctx, ms, ms.Status.UpdateRevision, c.newRoleUpdateCheck(ms)))
			pods, err = kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1+int(tt.oldWorkers))
			for i := range pods.Items {
				pod := &pods.Items[i]
				assert.Equal(t, oldRevision, utils.ObjectRevision(pod))
				assert.Equal(t, "old-image", pod.Spec.Containers[0].Image)
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
				require.NoError(t, c.handleReadyPod(ms, groupName, pod))
			}
			assert.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, groupName, "decode", roleID))
			assert.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, groupName))
		})
	}
}

func TestRoleUpdateCheckReusesHistoryWithinReconciliation(t *testing.T) {
	ctx := context.Background()
	kubeClient := kubefake.NewSimpleClientset()
	c := &ModelServingController{kubeClientSet: kubeClient}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "comparison", Namespace: "default", UID: "comparison-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{
			{Name: "decode", EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "old"}}}}},
			{Name: "prefill", EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "old"}}}}},
		}}},
	}
	data, err := utils.BuildRevisionData(ms)
	require.NoError(t, err)
	history, _, err := utils.RecordModelServingRevision(ctx, kubeClient, ms, data)
	require.NoError(t, err)
	oldRevision := history.Labels[utils.ControllerRevisionRevisionLabelKey]
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "new"
	data, err = utils.BuildRevisionData(ms)
	require.NoError(t, err)
	history, _, err = utils.RecordModelServingRevision(ctx, kubeClient, ms, data)
	require.NoError(t, err)
	newRevision := history.Labels[utils.ControllerRevisionRevisionLabelKey]
	kubeClient.ClearActions()

	needsUpdate := c.newRoleUpdateCheck(ms)
	for _, revision := range []string{oldRevision, newRevision} {
		for _, role := range ms.Spec.Template.Roles {
			for i := 0; i < 10; i++ {
				outdated, err := needsUpdate(ctx, datastore.ServingGroup{}, role, datastore.Role{
					Name: utils.GenerateRoleID(role.Name, i), Revision: revision,
				})
				require.NoError(t, err)
				assert.Equal(t, revision == oldRevision && role.Name == "decode", outdated)
			}
		}
	}
	// Multiple Roles and replicas share one GET per historical revision.
	require.Len(t, kubeClient.Actions(), 2)
	for _, action := range kubeClient.Actions() {
		assert.True(t, action.Matches("get", "controllerrevisions"))
	}

	// A later reconciliation must see spec changes instead of reusing old results.
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
	outdated, err := c.newRoleUpdateCheck(ms)(ctx, datastore.ServingGroup{}, ms.Spec.Template.Roles[0], datastore.Role{Revision: oldRevision})
	require.NoError(t, err)
	assert.False(t, outdated)
	require.Len(t, kubeClient.Actions(), 3)
}
