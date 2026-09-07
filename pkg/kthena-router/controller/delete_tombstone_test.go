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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// enqueueModelServer and enqueuePod are registered as DeleteFunc, so a relist
// after a dropped watch delivers a DeletedFinalStateUnknown tombstone rather
// than the object. Plain MetaNamespaceKeyFunc cannot key one, so the deletion
// would be logged and dropped.
func TestEnqueueHandlesDeletedFinalStateUnknown(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	controller, err := NewModelServerController(
		kthenaInformerFactory,
		kubeInformerFactory,
		newStoreWithMockBackend(),
	)
	require.NoError(t, err)

	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: "ms-1", Namespace: "default"},
	}
	tombstone := cache.DeletedFinalStateUnknown{Key: "default/ms-1", Obj: ms}

	controller.enqueueModelServer(tombstone)

	require.Equal(t, 1, controller.workqueue.Len(), "tombstone deletion was dropped")
	item, _ := controller.workqueue.Get()
	assert.Equal(t, QueueItem{ResourceType: ResourceTypeModelServer, Key: "default/ms-1"}, item)
}
