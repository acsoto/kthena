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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
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

func TestHistoricalRoleCapacitySurvivesInstanceLoss(t *testing.T) {
	for _, survivors := range []int{2, 0} {
		t.Run(fmt.Sprint(survivors), func(t *testing.T) {
			ctx := context.Background()
			old := recoveryModelServing()
			kube := kubefake.NewSimpleClientset()
			data, err := utils.BuildRevisionData(old)
			require.NoError(t, err)
			cr, _, err := utils.RecordModelServingRevision(ctx, kube, old, data)
			require.NoError(t, err)
			current := old.DeepCopy()
			current.Spec.Template.Roles = nil
			current.Status.RoleReplicaCounts = map[string]int32{"decode": 3}
			c, err := NewModelServingController(kube, kthenafake.NewSimpleClientset(current), nil, apiextfake.NewSimpleClientset())
			require.NoError(t, err)
			key := utils.GetNamespaceName(current)
			revision := cr.Labels[utils.ControllerRevisionRevisionLabelKey]
			for i := 0; i < survivors; i++ {
				c.store.AddServingGroupAndRole(key, "recovery-0", revision, "hash", "decode", utils.GenerateRoleID("decode", i))
			}
			restored, err := c.modelServingForServingGroupRevision(ctx, current, "recovery-0", revision)
			require.NoError(t, err)
			require.EqualValues(t, 3, *restored.Spec.Template.Roles[0].Replicas)
		})
	}
}

func TestRoleReplicaCountsCheckpointAndRetention(t *testing.T) {
	ctx := context.Background()
	ms := recoveryModelServing()
	kube := kubefake.NewSimpleClientset()
	client := kthenafake.NewSimpleClientset(ms)
	c, err := NewModelServingController(kube, client, nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	data, err := utils.BuildRevisionData(ms)
	require.NoError(t, err)
	cr, _, err := utils.RecordModelServingRevision(ctx, kube, ms, data)
	require.NoError(t, err)

	saved, err := c.syncRoleReplicaCounts(ctx, ms)
	require.NoError(t, err)
	require.Equal(t, map[string]int32{"decode": 3}, saved.Status.RoleReplicaCounts)
	require.Nil(t, ms.Status.RoleReplicaCounts, "the informer object must not be mutated")

	// Operational scaling updates the checkpoint without creating a revision.
	saved.Spec.Template.Roles[0].Replicas = ptr.To[int32](5)
	saved, err = client.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, saved, metav1.UpdateOptions{})
	require.NoError(t, err)
	saved, err = c.syncRoleReplicaCounts(ctx, saved)
	require.NoError(t, err)
	require.EqualValues(t, 5, saved.Status.RoleReplicaCounts["decode"])

	// Removing the Role keeps its last desired capacity while history uses it.
	saved.Spec.Template.Roles[0].Name = "prefill"
	saved.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	saved, err = client.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, saved, metav1.UpdateOptions{})
	require.NoError(t, err)
	saved, err = c.syncRoleReplicaCounts(ctx, saved)
	require.NoError(t, err)
	require.Equal(t, map[string]int32{"decode": 5, "prefill": 2}, saved.Status.RoleReplicaCounts)

	// A fresh controller with no observed instances uses the durable checkpoint.
	restarted, err := NewModelServingController(kube, client, nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	restored, err := restarted.modelServingForServingGroupRevision(ctx, saved, "recovery-0", cr.Labels[utils.ControllerRevisionRevisionLabelKey])
	require.NoError(t, err)
	require.EqualValues(t, 5, *restored.Spec.Template.Roles[0].Replicas)

	require.NoError(t, kube.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, cr.Name, metav1.DeleteOptions{}))
	saved, err = c.syncRoleReplicaCounts(ctx, saved)
	require.NoError(t, err)
	require.Equal(t, map[string]int32{"prefill": 2}, saved.Status.RoleReplicaCounts)
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

func TestRecoveryCheckpointFailureStopsPodCreation(t *testing.T) {
	ctx := context.Background()
	ms := recoveryModelServing()
	kube := kubefake.NewSimpleClientset()
	client := kthenafake.NewSimpleClientset(ms)
	client.PrependReactor("update", "modelservings", func(action kubetesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			return true, nil, fmt.Errorf("status unavailable")
		}
		return false, nil, nil
	})
	c, err := NewModelServingController(kube, client, nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetStore().Add(ms))
	require.ErrorContains(t, c.syncModelServing(ctx, "default/recovery"), "status unavailable")
	pods, err := kube.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, pods.Items)
}

func TestRecoveryCheckpointRetriesStatusConflict(t *testing.T) {
	ctx := context.Background()
	ms := recoveryModelServing()
	client := kthenafake.NewSimpleClientset(ms)
	attempts := 0
	client.PrependReactor("update", "modelservings", func(action kubetesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		attempts++
		if attempts == 1 {
			concurrent := ms.DeepCopy()
			concurrent.Status.CurrentRevision = "concurrent-status"
			require.NoError(t, client.Tracker().Update(workload.SchemeGroupVersion.WithResource("modelservings"), concurrent, ms.Namespace))
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: workload.SchemeGroupVersion.Group, Resource: "modelservings"}, ms.Name, fmt.Errorf("conflict"))
		}
		return false, nil, nil
	})
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), client, nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	saved, err := c.syncRoleReplicaCounts(ctx, ms)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	require.Equal(t, "concurrent-status", saved.Status.CurrentRevision)
	require.EqualValues(t, 3, saved.Status.RoleReplicaCounts["decode"])
}
