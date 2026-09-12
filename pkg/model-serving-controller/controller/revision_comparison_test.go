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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestRoleRollingUpdateMissingRevisionHistoryDoesNotPromoteRole(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-history", Namespace: "default", UID: types.UID("ms-uid")},
		Spec: workloadv1alpha1.ModelServingSpec{
			SchedulerName: "new-scheduler",
			Plugins: []workloadv1alpha1.PluginSpec{{
				Name:   "scheduler-sensitive",
				Type:   workloadv1alpha1.PluginTypeBuiltIn,
				Config: &apiextensionsv1.JSON{Raw: []byte(`{"mode":"new"}`)},
			}},
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name:     "decode",
				Replicas: ptr.To[int32](1),
				EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "image:v1"}},
				}},
			}}},
		},
	}

	store := datastore.New()
	nsn := utils.GetNamespaceName(ms)
	groupName := utils.GenerateServingGroupName(ms.Name, 0)
	oldRevision := "missing-revision"
	store.AddServingGroup(nsn, 0, oldRevision)
	store.AddRole(nsn, groupName, "decode", "decode-0", oldRevision, utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0]))
	require.NoError(t, store.UpdateRoleStatus(nsn, groupName, "decode", "decode-0", datastore.RoleRunning))

	controller := &ModelServingController{
		kubeClientSet: kubefake.NewSimpleClientset(),
		store:         store,
	}
	rolesToDelete, hasOutdated, err := controller.rolesToDeleteForRoleRollingUpdate(
		ctx,
		ms,
		datastore.ServingGroup{Name: groupName, Revision: oldRevision, Status: datastore.ServingGroupRunning},
	)
	require.NoError(t, err)
	require.True(t, hasOutdated, "a Role with missing revision history must remain outdated")
	require.Equal(t, []roleToDelete{{roleName: "decode", roleID: "decode-0"}}, rolesToDelete)

	currentRevision, ok := store.GetServingGroupRevision(nsn, groupName)
	require.True(t, ok)
	require.Equal(t, oldRevision, currentRevision, "the ServingGroup must not be promoted before replacing the Role")
}

func TestPartitionProtectedRecoveryFailsWhenRevisionIsMissing(t *testing.T) {
	ctx := context.Background()
	ms := recoveryModelServing()
	controller := &ModelServingController{kubeClientSet: kubefake.NewSimpleClientset()}

	_, _, _, _, err := controller.roleTemplateForReplica(
		ctx,
		ms,
		ms.Spec.Template.Roles[0],
		datastore.Role{Name: "decode-0", Revision: "missing-revision"},
		"new-revision",
		true,
	)
	require.ErrorIs(t, err, errControllerRevisionNotFound)
}
