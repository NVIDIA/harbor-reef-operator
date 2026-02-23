// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	annotationKeyUpstreams = "harbor.rewrite/original-upstreams"
	patchedContainersKey   = "harbor-reef/patched-containers"
	lastPatchedTimeKey     = "harbor-reef/patched"
)

var (
	podsPatchedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pods_upstream_patched_total",
			Help: "Total number of pod containers patched to use original upstream images",
		},
		[]string{"patched_kube_namespace", "patched_pod_name", "patched_container_name", "patched_image"},
	)

	reconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "reconcile_errors_total",
			Help: "Total number of reconciliation errors",
		},
		[]string{"patched_kube_namespace", "patched_pod_name", "error_type"},
	)

	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "reconcile_duration_seconds",
			Help:    "Duration of reconciliation operations in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"patched_kube_namespace", "result"},
	)
)

func init() {
	metrics.Registry.MustRegister(podsPatchedTotal)
	metrics.Registry.MustRegister(reconcileErrorsTotal)
	metrics.Registry.MustRegister(reconcileDuration)
}

type patchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// Reconciler watches Pods for ImagePullBackOff and reverts images to upstream.
type Reconciler struct {
	client client.Client
}

// NewReconciler creates a Reconciler with the given controller-runtime client.
func NewReconciler(c client.Client) *Reconciler {
	return &Reconciler{client: c}
}

// SetupWithManager registers the pod controller with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	pred := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			if !ok {
				return false
			}
			return isImagePullBackOff(pod)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okNew {
				return false
			}
			return isImagePullBackOff(newPod)
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(pred)).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	startTime := time.Now()
	defer func() {
		reconcileDuration.WithLabelValues(req.Namespace, "completed").Observe(time.Since(startTime).Seconds())
	}()

	var pod corev1.Pod
	if err := r.client.Get(ctx, req.NamespacedName, &pod); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if pod.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}

	if pod.Annotations == nil {
		return reconcile.Result{}, nil
	}

	if !isImagePullBackOff(&pod) {
		return reconcile.Result{}, nil
	}

	waiting := waitingContainersSet(&pod)
	logger := ctrl.Log.WithValues("namespace", pod.Namespace, "pod", pod.Name)
	if len(waiting) > 0 {
		waitingNames := make([]string, 0, len(waiting))
		for name := range waiting {
			waitingNames = append(waitingNames, name)
		}
		logger.Info("Detected image pull backoff", "waitingContainers", waitingNames)
	} else {
		logger.Info("Detected image pull backoff")
	}

	upstreams := getOriginalUpstreams(&pod)
	alreadyPatched := getPatchedContainers(&pod)
	patchOps := make([]patchOp, 0)
	newlyPatched := make([]string, 0)

	for idx, c := range pod.Spec.Containers {
		if _, isWaiting := waiting[c.Name]; !isWaiting {
			continue
		}
		if _, wasPatched := alreadyPatched[c.Name]; wasPatched {
			continue
		}
		if upstream, ok := upstreams[c.Name]; ok && upstream != "" {
			patchOps = append(patchOps, patchOp{
				Op:    "replace",
				Path:  fmt.Sprintf("/spec/containers/%d/image", idx),
				Value: upstream,
			})
			newlyPatched = append(newlyPatched, c.Name)
		}
	}
	for idx, c := range pod.Spec.InitContainers {
		if _, isWaiting := waiting[c.Name]; !isWaiting {
			continue
		}
		if _, wasPatched := alreadyPatched[c.Name]; wasPatched {
			continue
		}
		if upstream, ok := upstreams[c.Name]; ok && upstream != "" {
			patchOps = append(patchOps, patchOp{
				Op:    "replace",
				Path:  fmt.Sprintf("/spec/initContainers/%d/image", idx),
				Value: upstream,
			})
			newlyPatched = append(newlyPatched, c.Name)
		}
	}

	if len(patchOps) == 0 {
		return reconcile.Result{}, nil
	}

	allPatched := make([]string, 0, len(alreadyPatched)+len(newlyPatched))
	for name := range alreadyPatched {
		allPatched = append(allPatched, name)
	}
	allPatched = append(allPatched, newlyPatched...)

	patchedContainersJSON, _ := json.Marshal(allPatched)
	patchedContainersOp := "add"
	if pod.Annotations[patchedContainersKey] != "" {
		patchedContainersOp = "replace"
	}
	patchOps = append(patchOps, patchOp{
		Op:    patchedContainersOp,
		Path:  "/metadata/annotations/" + escapePatchPath(patchedContainersKey),
		Value: string(patchedContainersJSON),
	})

	timestampOp := "add"
	if pod.Annotations[lastPatchedTimeKey] != "" {
		timestampOp = "replace"
	}
	patchOps = append(patchOps, patchOp{
		Op:    timestampOp,
		Path:  "/metadata/annotations/" + escapePatchPath(lastPatchedTimeKey),
		Value: time.Now().UTC().Format(time.RFC3339),
	})

	waiting = make(map[string]struct{})
	for _, name := range newlyPatched {
		waiting[name] = struct{}{}
	}

	patchBytes, err := json.Marshal(patchOps)
	if err != nil {
		reconcileErrorsTotal.WithLabelValues(pod.Namespace, pod.Name, "marshal_error").Inc()
		return reconcile.Result{}, err
	}
	if err := r.client.Patch(ctx, &pod, client.RawPatch(types.JSONPatchType, patchBytes)); err != nil {
		reconcileErrorsTotal.WithLabelValues(pod.Namespace, pod.Name, "patch_error").Inc()
		return reconcile.Result{RequeueAfter: 15 * time.Second}, err
	}

	patched := patchedContainersFromUpstreams(&pod, upstreams, waiting)
	if len(patched) > 0 {
		logger.Info("Successfully patched pod images to original upstream", "patched", patched)
	} else {
		logger.Info("Successfully patched pod images to original upstream")
	}

	for _, c := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		if _, isWaiting := waiting[c.Name]; !isWaiting {
			continue
		}
		if upstream, ok := upstreams[c.Name]; ok && upstream != "" {
			podsPatchedTotal.WithLabelValues(pod.Namespace, pod.Name, c.Name, upstream).Inc()
		}
	}

	return reconcile.Result{}, nil
}

func getOriginalUpstreams(pod *corev1.Pod) map[string]string {
	upstreams := make(map[string]string)

	if pod.Annotations == nil {
		return upstreams
	}

	jsonData, ok := pod.Annotations[annotationKeyUpstreams]
	if !ok || jsonData == "" {
		return upstreams
	}

	if err := json.Unmarshal([]byte(jsonData), &upstreams); err != nil {
		ctrl.Log.Error(err, "Failed to parse original-upstreams annotation",
			"annotation", jsonData)
		return make(map[string]string)
	}

	return upstreams
}

func getPatchedContainers(pod *corev1.Pod) map[string]struct{} {
	patched := make(map[string]struct{})

	if pod.Annotations == nil {
		return patched
	}

	jsonData, ok := pod.Annotations[patchedContainersKey]
	if !ok || jsonData == "" {
		return patched
	}

	var names []string
	if err := json.Unmarshal([]byte(jsonData), &names); err != nil {
		ctrl.Log.Error(err, "Failed to parse patched-containers annotation",
			"annotation", jsonData)
		return make(map[string]struct{})
	}

	for _, name := range names {
		patched[name] = struct{}{}
	}
	return patched
}

func isImagePullBackOff(p *corev1.Pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			if cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull" {
				return true
			}
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.State.Waiting != nil {
			if cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull" {
				return true
			}
		}
	}
	return false
}

func waitingContainersSet(p *corev1.Pod) map[string]struct{} {
	names := make(map[string]struct{})
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			if cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull" {
				names[cs.Name] = struct{}{}
			}
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.State.Waiting != nil {
			if cs.State.Waiting.Reason == "ImagePullBackOff" || cs.State.Waiting.Reason == "ErrImagePull" {
				names[cs.Name] = struct{}{}
			}
		}
	}
	return names
}

func patchedContainersFromUpstreams(p *corev1.Pod, upstreams map[string]string, waiting map[string]struct{}) map[string]string {
	m := make(map[string]string)
	for _, c := range p.Spec.Containers {
		if _, isWaiting := waiting[c.Name]; !isWaiting {
			continue
		}
		if upstream, ok := upstreams[c.Name]; ok && upstream != "" {
			m[c.Name] = upstream
		}
	}
	for _, c := range p.Spec.InitContainers {
		if _, isWaiting := waiting[c.Name]; !isWaiting {
			continue
		}
		if upstream, ok := upstreams[c.Name]; ok && upstream != "" {
			m[c.Name] = upstream
		}
	}
	return m
}

func escapePatchPath(key string) string {
	escaped := strings.ReplaceAll(key, "~", "~0")
	escaped = strings.ReplaceAll(escaped, "/", "~1")
	return escaped
}
