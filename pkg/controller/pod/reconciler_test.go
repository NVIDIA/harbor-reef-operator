// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var testScheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = corev1.AddToScheme(testScheme)
}

func newFakeClient(objs ...runtime.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme).
		WithRuntimeObjects(objs...).
		Build()
}

func TestEscapePatchPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no special characters", "simple-key", "simple-key"},
		{"contains slash", "harbor-reef/patched", "harbor-reef~1patched"},
		{"contains tilde", "key~value", "key~0value"},
		{"contains both tilde and slash", "key~with/both", "key~0with~1both"},
		{"multiple slashes", "a/b/c", "a~1b~1c"},
		{"tilde before slash (order matters)", "~/path", "~0~1path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapePatchPath(tt.input)
			if got != tt.want {
				t.Errorf("escapePatchPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetOriginalUpstreams(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[string]string
	}{
		{"nil annotations", nil, map[string]string{}},
		{"empty annotations", map[string]string{}, map[string]string{}},
		{"missing annotation key", map[string]string{"other-key": "value"}, map[string]string{}},
		{"empty annotation value", map[string]string{annotationKeyUpstreams: ""}, map[string]string{}},
		{"valid single container", map[string]string{annotationKeyUpstreams: `{"nginx":"docker.io/nginx:latest"}`}, map[string]string{"nginx": "docker.io/nginx:latest"}},
		{"valid multiple containers", map[string]string{annotationKeyUpstreams: `{"nginx":"docker.io/nginx:latest","redis":"docker.io/redis:7"}`}, map[string]string{"nginx": "docker.io/nginx:latest", "redis": "docker.io/redis:7"}},
		{"invalid JSON", map[string]string{annotationKeyUpstreams: `{invalid json}`}, map[string]string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			got := getOriginalUpstreams(pod)
			if len(got) != len(tt.want) {
				t.Errorf("getOriginalUpstreams() returned %d items, want %d", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("getOriginalUpstreams()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestGetPatchedContainers(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[string]struct{}
	}{
		{"nil annotations", nil, map[string]struct{}{}},
		{"empty annotations", map[string]string{}, map[string]struct{}{}},
		{"missing key", map[string]string{"other-key": "value"}, map[string]struct{}{}},
		{"empty value", map[string]string{patchedContainersKey: ""}, map[string]struct{}{}},
		{"valid single", map[string]string{patchedContainersKey: `["nginx"]`}, map[string]struct{}{"nginx": {}}},
		{"valid multiple", map[string]string{patchedContainersKey: `["nginx","redis","app"]`}, map[string]struct{}{"nginx": {}, "redis": {}, "app": {}}},
		{"invalid JSON", map[string]string{patchedContainersKey: `[invalid]`}, map[string]struct{}{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			got := getPatchedContainers(pod)
			if len(got) != len(tt.want) {
				t.Errorf("getPatchedContainers() returned %d items, want %d", len(got), len(tt.want))
			}
			for k := range tt.want {
				if _, exists := got[k]; !exists {
					t.Errorf("getPatchedContainers() missing key %q", k)
				}
			}
		})
	}
}

func TestIsImagePullBackOff(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"no container statuses", &corev1.Pod{Status: corev1.PodStatus{}}, false},
		{"container running", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}, false},
		{"container in ImagePullBackOff", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}, true},
		{"container in ErrImagePull", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}}}}}, true},
		{"init container in ImagePullBackOff", &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}, true},
		{"container waiting for other reason", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isImagePullBackOff(tt.pod)
			if got != tt.want {
				t.Errorf("isImagePullBackOff() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWaitingContainersSet(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want map[string]struct{}
	}{
		{"no waiting containers", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}, map[string]struct{}{}},
		{"one container in backoff", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}, map[string]struct{}{"app": {}}},
		{"init container in backoff", &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}, map[string]struct{}{"init": {}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := waitingContainersSet(tt.pod)
			if len(got) != len(tt.want) {
				t.Errorf("waitingContainersSet() returned %d items, want %d", len(got), len(tt.want))
			}
			for k := range tt.want {
				if _, exists := got[k]; !exists {
					t.Errorf("waitingContainersSet() missing key %q", k)
				}
			}
		})
	}
}

func TestReconcile_PodNotFound(t *testing.T) {
	r := NewReconciler(newFakeClient())
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "nonexistent-pod"}})
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_PodBeingDeleted(t *testing.T) {
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deleting-pod", DeletionTimestamp: &now, Finalizers: []string{"test-finalizer"}, Annotations: map[string]string{annotationKeyUpstreams: `{"app":"docker.io/nginx:latest"}`}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "harbor.local/nginx:latest"}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	r := NewReconciler(newFakeClient(pod))
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "deleting-pod"}})
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_PodWithoutAnnotations(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "no-annotations-pod"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "harbor.local/nginx:latest"}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	r := NewReconciler(newFakeClient(pod))
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "no-annotations-pod"}})
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_SuccessfulPatch(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "patch-me-pod", Annotations: map[string]string{annotationKeyUpstreams: `{"app":"docker.io/nginx:latest"}`}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "harbor.local/nginx:latest"}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	fakeClient := newFakeClient(pod)
	r := NewReconciler(fakeClient)
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "patch-me-pod"}})
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue on success", result)
	}

	var patchedPod corev1.Pod
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "patch-me-pod"}, &patchedPod); err != nil {
		t.Fatalf("Failed to get patched pod: %v", err)
	}
	if patchedPod.Spec.Containers[0].Image != "docker.io/nginx:latest" {
		t.Errorf("Container image = %q, want %q", patchedPod.Spec.Containers[0].Image, "docker.io/nginx:latest")
	}
	patchedAnnotation := patchedPod.Annotations[patchedContainersKey]
	if patchedAnnotation == "" {
		t.Error("Expected patched-containers annotation to be set")
	}
	var patchedContainers []string
	if err := json.Unmarshal([]byte(patchedAnnotation), &patchedContainers); err != nil {
		t.Errorf("Failed to unmarshal patched-containers annotation: %v", err)
	}
	if len(patchedContainers) != 1 || patchedContainers[0] != "app" {
		t.Errorf("patched-containers = %v, want [\"app\"]", patchedContainers)
	}
	if patchedPod.Annotations[lastPatchedTimeKey] == "" {
		t.Error("Expected last-patched-time annotation to be set")
	}
}

func TestReconcile_SuccessfulPatchInitContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "init-patch-pod", Annotations: map[string]string{annotationKeyUpstreams: `{"init":"docker.io/busybox:latest"}`}},
		Spec:       corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init", Image: "harbor.local/busybox:latest"}}, Containers: []corev1.Container{{Name: "app", Image: "harbor.local/nginx:latest"}}},
		Status:     corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	fakeClient := newFakeClient(pod)
	r := NewReconciler(fakeClient)
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "init-patch-pod"}})
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue on success", result)
	}
	var patchedPod corev1.Pod
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "init-patch-pod"}, &patchedPod); err != nil {
		t.Fatalf("Failed to get patched pod: %v", err)
	}
	if patchedPod.Spec.InitContainers[0].Image != "docker.io/busybox:latest" {
		t.Errorf("Init container image = %q, want %q", patchedPod.Spec.InitContainers[0].Image, "docker.io/busybox:latest")
	}
}

func TestReconcile_PartialPatch(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "partial-patch-pod", Annotations: map[string]string{annotationKeyUpstreams: `{"app1":"docker.io/nginx:latest","app2":"docker.io/redis:7"}`, patchedContainersKey: `["app1"]`}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app1", Image: "docker.io/nginx:latest"}, {Name: "app2", Image: "harbor.local/redis:7"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "app1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "app2", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
		}},
	}
	fakeClient := newFakeClient(pod)
	r := NewReconciler(fakeClient)
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "partial-patch-pod"}})
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue on success", result)
	}
	var patchedPod corev1.Pod
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "partial-patch-pod"}, &patchedPod); err != nil {
		t.Fatalf("Failed to get patched pod: %v", err)
	}
	if patchedPod.Spec.Containers[1].Image != "docker.io/redis:7" {
		t.Errorf("Container 2 image = %q, want %q", patchedPod.Spec.Containers[1].Image, "docker.io/redis:7")
	}
	var pcs []string
	if err := json.Unmarshal([]byte(patchedPod.Annotations[patchedContainersKey]), &pcs); err != nil {
		t.Errorf("Failed to unmarshal patched-containers: %v", err)
	}
	if len(pcs) != 2 {
		t.Errorf("Expected 2 patched containers, got %d: %v", len(pcs), pcs)
	}
}
