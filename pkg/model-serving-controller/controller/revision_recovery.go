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
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// syncRoleReplicaCounts checkpoints desired capacity before changing child
// resources. A deleted Role has no current spec to consult during recovery;
// surviving Pods cannot tell us how many instances were intended to exist.
// Keep its last desired count until no retained snapshot contains that Role.
func (c *ModelServingController) syncRoleReplicaCounts(ctx context.Context, ms *workload.ModelServing) (*workload.ModelServing, error) {
	history, err := c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{utils.ControllerRevisionLabelKey: ms.Name}).String(),
	})
	if err != nil {
		return nil, err
	}
	historicalNames := make(map[string]struct{})
	for i := range history.Items {
		cr := &history.Items[i]
		if !metav1.IsControlledBy(cr, ms) {
			continue
		}
		roles, err := utils.GetRolesFromControllerRevision(cr)
		if err != nil {
			return nil, fmt.Errorf("read Role membership from revision %s: %w", cr.Name, err)
		}
		for _, role := range roles {
			historicalNames[role.Name] = struct{}{}
		}
	}
	var result *workload.ModelServing
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if latest.UID != ms.UID || latest.Generation != ms.Generation ||
			!reflect.DeepEqual(latest.Spec, ms.Spec) || !reflect.DeepEqual(latest.OwnerReferences, ms.OwnerReferences) {
			return fmt.Errorf("ModelServing changed while recording recovery capacity")
		}
		counts := make(map[string]int32)
		for name, replicas := range latest.Status.RoleReplicaCounts {
			if _, retained := historicalNames[name]; retained {
				counts[name] = replicas
			}
		}
		for _, role := range ms.Spec.Template.Roles {
			counts[role.Name] = int32(roleReplicas(role))
		}
		result = latest
		if reflect.DeepEqual(counts, latest.Status.RoleReplicaCounts) {
			return nil
		}
		copy := latest.DeepCopy()
		copy.Status.RoleReplicaCounts = counts
		result, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
		return err
	})
	return result, err
}
