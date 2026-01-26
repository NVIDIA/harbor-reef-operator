// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ============================================================================
// Helper function tests
// ============================================================================

func TestGetWatchNamespace(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		envSet   bool
		want     string
	}{
		{
			name:   "not set",
			envSet: false,
			want:   "",
		},
		{
			name:     "single namespace",
			envValue: "default",
			envSet:   true,
			want:     "default",
		},
		{
			name:     "multiple namespaces",
			envValue: "ns1,ns2,ns3",
			envSet:   true,
			want:     "ns1,ns2,ns3",
		},
		{
			name:     "empty string",
			envValue: "",
			envSet:   true,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up env before each test
			os.Unsetenv("WATCH_NAMESPACE")

			if tt.envSet {
				os.Setenv("WATCH_NAMESPACE", tt.envValue)
			}

			got, err := getWatchNamespace()
			if err != nil {
				t.Errorf("getWatchNamespace() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("getWatchNamespace() = %v, want %v", got, tt.want)
			}

			// Clean up
			os.Unsetenv("WATCH_NAMESPACE")
		})
	}
}

func TestGetEnvDuration(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		envValue   string
		envSet     bool
		defaultVal time.Duration
		want       time.Duration
	}{
		{
			name:       "not set - uses default",
			key:        "TEST_DURATION",
			envSet:     false,
			defaultVal: 30 * time.Second,
			want:       30 * time.Second,
		},
		{
			name:       "valid duration",
			key:        "TEST_DURATION",
			envValue:   "45s",
			envSet:     true,
			defaultVal: 30 * time.Second,
			want:       45 * time.Second,
		},
		{
			name:       "valid duration with minutes",
			key:        "TEST_DURATION",
			envValue:   "2m",
			envSet:     true,
			defaultVal: 30 * time.Second,
			want:       2 * time.Minute,
		},
		{
			name:       "invalid duration - uses default",
			key:        "TEST_DURATION",
			envValue:   "invalid",
			envSet:     true,
			defaultVal: 30 * time.Second,
			want:       30 * time.Second,
		},
		{
			name:       "empty string - uses default",
			key:        "TEST_DURATION",
			envValue:   "",
			envSet:     true,
			defaultVal: 30 * time.Second,
			want:       30 * time.Second,
		},
		{
			name:       "whitespace only - uses default",
			key:        "TEST_DURATION",
			envValue:   "   ",
			envSet:     true,
			defaultVal: 30 * time.Second,
			want:       30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Unsetenv(tt.key)

			if tt.envSet {
				os.Setenv(tt.key, tt.envValue)
			}

			got := getEnvDuration(tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getEnvDuration() = %v, want %v", got, tt.want)
			}

			os.Unsetenv(tt.key)
		})
	}
}

func TestEscapePatchPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no special characters",
			input: "simple-key",
			want:  "simple-key",
		},
		{
			name:  "contains slash",
			input: "harbor-reef/patched",
			want:  "harbor-reef~1patched",
		},
		{
			name:  "contains tilde",
			input: "key~value",
			want:  "key~0value",
		},
		{
			name:  "contains both tilde and slash",
			input: "key~with/both",
			want:  "key~0with~1both",
		},
		{
			name:  "multiple slashes",
			input: "a/b/c",
			want:  "a~1b~1c",
		},
		{
			name:  "tilde before slash (order matters)",
			input: "~/path",
			want:  "~0~1path",
		},
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

// ============================================================================
// Pod annotation parsing tests
// ============================================================================

func TestGetOriginalUpstreams(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[string]string
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			want:        map[string]string{},
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			want:        map[string]string{},
		},
		{
			name: "missing annotation key",
			annotations: map[string]string{
				"other-key": "value",
			},
			want: map[string]string{},
		},
		{
			name: "empty annotation value",
			annotations: map[string]string{
				annotationKeyUpstreams: "",
			},
			want: map[string]string{},
		},
		{
			name: "valid single container",
			annotations: map[string]string{
				annotationKeyUpstreams: `{"nginx":"docker.io/nginx:latest"}`,
			},
			want: map[string]string{
				"nginx": "docker.io/nginx:latest",
			},
		},
		{
			name: "valid multiple containers",
			annotations: map[string]string{
				annotationKeyUpstreams: `{"nginx":"docker.io/nginx:latest","redis":"docker.io/redis:7"}`,
			},
			want: map[string]string{
				"nginx": "docker.io/nginx:latest",
				"redis": "docker.io/redis:7",
			},
		},
		{
			name: "invalid JSON",
			annotations: map[string]string{
				annotationKeyUpstreams: `{invalid json}`,
			},
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

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
		{
			name:        "nil annotations",
			annotations: nil,
			want:        map[string]struct{}{},
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			want:        map[string]struct{}{},
		},
		{
			name: "missing patched-containers key",
			annotations: map[string]string{
				"other-key": "value",
			},
			want: map[string]struct{}{},
		},
		{
			name: "empty patched-containers value",
			annotations: map[string]string{
				patchedContainersKey: "",
			},
			want: map[string]struct{}{},
		},
		{
			name: "valid single container",
			annotations: map[string]string{
				patchedContainersKey: `["nginx"]`,
			},
			want: map[string]struct{}{
				"nginx": {},
			},
		},
		{
			name: "valid multiple containers",
			annotations: map[string]string{
				patchedContainersKey: `["nginx","redis","app"]`,
			},
			want: map[string]struct{}{
				"nginx": {},
				"redis": {},
				"app":   {},
			},
		},
		{
			name: "invalid JSON",
			annotations: map[string]string{
				patchedContainersKey: `[invalid]`,
			},
			want: map[string]struct{}{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

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

// ============================================================================
// Image pull backoff detection tests
// ============================================================================

func TestIsImagePullBackOff(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "no container statuses",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{},
			},
			want: false,
		},
		{
			name: "container running",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "container in ImagePullBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "container in ErrImagePull",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ErrImagePull",
								},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "init container in ImagePullBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "init",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "init container in ErrImagePull",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "init",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ErrImagePull",
								},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "container waiting for other reason",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ContainerCreating",
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "mixed states - one in backoff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app1",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
						{
							Name: "app2",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
				},
			},
			want: true,
		},
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
		{
			name: "no waiting containers",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			want: map[string]struct{}{},
		},
		{
			name: "one container in ImagePullBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
				},
			},
			want: map[string]struct{}{
				"app": {},
			},
		},
		{
			name: "multiple containers in backoff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app1",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
						{
							Name: "app2",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ErrImagePull",
								},
							},
						},
					},
				},
			},
			want: map[string]struct{}{
				"app1": {},
				"app2": {},
			},
		},
		{
			name: "init container in backoff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "init",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
				},
			},
			want: map[string]struct{}{
				"init": {},
			},
		},
		{
			name: "mixed containers and init containers",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ImagePullBackOff",
								},
							},
						},
					},
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "init",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ErrImagePull",
								},
							},
						},
					},
				},
			},
			want: map[string]struct{}{
				"app":  {},
				"init": {},
			},
		},
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

func TestPatchedContainersFromUpstreams(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		upstreams map[string]string
		waiting   map[string]struct{}
		want      map[string]string
	}{
		{
			name: "no waiting containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "harbor.local/nginx:latest"},
					},
				},
			},
			upstreams: map[string]string{
				"app": "docker.io/nginx:latest",
			},
			waiting: map[string]struct{}{},
			want:    map[string]string{},
		},
		{
			name: "waiting container with upstream",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "harbor.local/nginx:latest"},
					},
				},
			},
			upstreams: map[string]string{
				"app": "docker.io/nginx:latest",
			},
			waiting: map[string]struct{}{
				"app": {},
			},
			want: map[string]string{
				"app": "docker.io/nginx:latest",
			},
		},
		{
			name: "waiting container without upstream",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "harbor.local/nginx:latest"},
					},
				},
			},
			upstreams: map[string]string{},
			waiting: map[string]struct{}{
				"app": {},
			},
			want: map[string]string{},
		},
		{
			name: "init container with upstream",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "init", Image: "harbor.local/busybox:latest"},
					},
				},
			},
			upstreams: map[string]string{
				"init": "docker.io/busybox:latest",
			},
			waiting: map[string]struct{}{
				"init": {},
			},
			want: map[string]string{
				"init": "docker.io/busybox:latest",
			},
		},
		{
			name: "mixed containers and init containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "harbor.local/nginx:latest"},
					},
					InitContainers: []corev1.Container{
						{Name: "init", Image: "harbor.local/busybox:latest"},
					},
				},
			},
			upstreams: map[string]string{
				"app":  "docker.io/nginx:latest",
				"init": "docker.io/busybox:latest",
			},
			waiting: map[string]struct{}{
				"app":  {},
				"init": {},
			},
			want: map[string]string{
				"app":  "docker.io/nginx:latest",
				"init": "docker.io/busybox:latest",
			},
		},
		{
			name: "empty upstream value is skipped",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "harbor.local/nginx:latest"},
					},
				},
			},
			upstreams: map[string]string{
				"app": "",
			},
			waiting: map[string]struct{}{
				"app": {},
			},
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := patchedContainersFromUpstreams(tt.pod, tt.upstreams, tt.waiting)

			if len(got) != len(tt.want) {
				t.Errorf("patchedContainersFromUpstreams() returned %d items, want %d", len(got), len(tt.want))
			}

			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("patchedContainersFromUpstreams()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// ============================================================================
// Reconcile tests
// ============================================================================

func newFakeClient(objs ...runtime.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()
}

func TestReconcile_PodNotFound(t *testing.T) {
	// Test that reconcile handles a missing pod gracefully
	r := &podReconciler{
		client: newFakeClient(),
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "nonexistent-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil (not found should be ignored)", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_PodBeingDeleted(t *testing.T) {
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "deleting-pod",
			DeletionTimestamp: &now,
			Finalizers:        []string{"test-finalizer"}, // Required by fake client when deletionTimestamp is set
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"app":"docker.io/nginx:latest"}`,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "harbor.local/nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
			},
		},
	}

	r := &podReconciler{
		client: newFakeClient(pod),
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "deleting-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue for deleting pod", result)
	}
}

func TestReconcile_PodWithoutAnnotations(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "no-annotations-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "harbor.local/nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
			},
		},
	}

	r := &podReconciler{
		client: newFakeClient(pod),
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "no-annotations-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_PodNotInBackoff(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "running-pod",
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"app":"docker.io/nginx:latest"}`,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "harbor.local/nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				},
			},
		},
	}

	r := &podReconciler{
		client: newFakeClient(pod),
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "running-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_AlreadyPatchedContainer(t *testing.T) {
	// Container is in backoff but was already patched - should not patch again
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "already-patched-pod",
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"app":"docker.io/nginx:latest"}`,
				patchedContainersKey:   `["app"]`,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "docker.io/nginx:latest"}, // Already patched to upstream
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff", // Still in backoff but already patched
						},
					},
				},
			},
		},
	}

	r := &podReconciler{
		client: newFakeClient(pod),
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "already-patched-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_NoUpstreamForWaitingContainer(t *testing.T) {
	// Container is in backoff but has no upstream defined
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "no-upstream-pod",
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"other":"docker.io/other:latest"}`, // Different container
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "harbor.local/nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
			},
		},
	}

	r := &podReconciler{
		client: newFakeClient(pod),
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "no-upstream-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue", result)
	}
}

func TestReconcile_SuccessfulPatch(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "patch-me-pod",
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"app":"docker.io/nginx:latest"}`,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "harbor.local/nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(pod)
	r := &podReconciler{
		client: fakeClient,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "patch-me-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue on success", result)
	}

	// Verify the pod was patched
	var patchedPod corev1.Pod
	if err := fakeClient.Get(context.Background(), req.NamespacedName, &patchedPod); err != nil {
		t.Fatalf("Failed to get patched pod: %v", err)
	}

	// Check image was updated
	if patchedPod.Spec.Containers[0].Image != "docker.io/nginx:latest" {
		t.Errorf("Container image = %q, want %q", patchedPod.Spec.Containers[0].Image, "docker.io/nginx:latest")
	}

	// Check patched-containers annotation was added
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

	// Check timestamp annotation was added
	if patchedPod.Annotations[lastPatchedTimeKey] == "" {
		t.Error("Expected last-patched-time annotation to be set")
	}
}

func TestReconcile_SuccessfulPatchInitContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "init-patch-pod",
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"init":"docker.io/busybox:latest"}`,
			},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init", Image: "harbor.local/busybox:latest"},
			},
			Containers: []corev1.Container{
				{Name: "app", Image: "harbor.local/nginx:latest"},
			},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "init",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(pod)
	r := &podReconciler{
		client: fakeClient,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "init-patch-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue on success", result)
	}

	// Verify the init container was patched
	var patchedPod corev1.Pod
	if err := fakeClient.Get(context.Background(), req.NamespacedName, &patchedPod); err != nil {
		t.Fatalf("Failed to get patched pod: %v", err)
	}

	if patchedPod.Spec.InitContainers[0].Image != "docker.io/busybox:latest" {
		t.Errorf("Init container image = %q, want %q", patchedPod.Spec.InitContainers[0].Image, "docker.io/busybox:latest")
	}
}

func TestReconcile_MultipleContainers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "multi-container-pod",
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"app1":"docker.io/nginx:latest","app2":"docker.io/redis:7"}`,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app1", Image: "harbor.local/nginx:latest"},
				{Name: "app2", Image: "harbor.local/redis:7"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app1",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
				{
					Name: "app2",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ErrImagePull",
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(pod)
	r := &podReconciler{
		client: fakeClient,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "multi-container-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue on success", result)
	}

	// Verify both containers were patched
	var patchedPod corev1.Pod
	if err := fakeClient.Get(context.Background(), req.NamespacedName, &patchedPod); err != nil {
		t.Fatalf("Failed to get patched pod: %v", err)
	}

	if patchedPod.Spec.Containers[0].Image != "docker.io/nginx:latest" {
		t.Errorf("Container 1 image = %q, want %q", patchedPod.Spec.Containers[0].Image, "docker.io/nginx:latest")
	}
	if patchedPod.Spec.Containers[1].Image != "docker.io/redis:7" {
		t.Errorf("Container 2 image = %q, want %q", patchedPod.Spec.Containers[1].Image, "docker.io/redis:7")
	}
}

func TestReconcile_PartialPatch(t *testing.T) {
	// One container already patched, another one newly in backoff
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "partial-patch-pod",
			Annotations: map[string]string{
				annotationKeyUpstreams: `{"app1":"docker.io/nginx:latest","app2":"docker.io/redis:7"}`,
				patchedContainersKey:   `["app1"]`, // app1 already patched
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app1", Image: "docker.io/nginx:latest"}, // Already patched
				{Name: "app2", Image: "harbor.local/redis:7"},   // Needs patching
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app1",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{}, // Now running
					},
				},
				{
					Name: "app2",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				},
			},
		},
	}

	fakeClient := newFakeClient(pod)
	r := &podReconciler{
		client: fakeClient,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "partial-patch-pod",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("Reconcile() result = %v, want no requeue on success", result)
	}

	// Verify only app2 was patched
	var patchedPod corev1.Pod
	if err := fakeClient.Get(context.Background(), req.NamespacedName, &patchedPod); err != nil {
		t.Fatalf("Failed to get patched pod: %v", err)
	}

	// app1 should remain unchanged
	if patchedPod.Spec.Containers[0].Image != "docker.io/nginx:latest" {
		t.Errorf("Container 1 image changed unexpectedly: %q", patchedPod.Spec.Containers[0].Image)
	}
	// app2 should be patched
	if patchedPod.Spec.Containers[1].Image != "docker.io/redis:7" {
		t.Errorf("Container 2 image = %q, want %q", patchedPod.Spec.Containers[1].Image, "docker.io/redis:7")
	}

	// Check that patched-containers now includes both
	var patchedContainers []string
	if err := json.Unmarshal([]byte(patchedPod.Annotations[patchedContainersKey]), &patchedContainers); err != nil {
		t.Errorf("Failed to unmarshal patched-containers annotation: %v", err)
	}
	if len(patchedContainers) != 2 {
		t.Errorf("Expected 2 patched containers, got %d: %v", len(patchedContainers), patchedContainers)
	}
}
