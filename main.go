// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
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
	scheme = runtime.NewScheme()

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

type patchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

type podReconciler struct {
	client client.Client
}

func init() {
	// Register API types with the scheme
	_ = clientgoscheme.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Register Prometheus metrics
	metrics.Registry.MustRegister(podsPatchedTotal)
	metrics.Registry.MustRegister(reconcileErrorsTotal)
	metrics.Registry.MustRegister(reconcileDuration)
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	watchNamespace, err := getWatchNamespace()
	if err != nil {
		ctrl.Log.Error(err, "unable to get WATCH_NAMESPACE, "+
			"the manager will watch and manage resources in all Namespaces")
	}

	mgrOpts := ctrl.Options{
		Scheme: scheme,
	}

	if watchNamespace != "" {
		watchNamespaces := strings.Split(watchNamespace, ",")
		defaultNamespaces := make(map[string]cache.Config)
		for _, ns := range watchNamespaces {
			trimmedNs := strings.TrimSpace(ns)
			if trimmedNs != "" {
				defaultNamespaces[trimmedNs] = cache.Config{}
			}
		}
		mgrOpts.Cache = cache.Options{
			DefaultNamespaces: defaultNamespaces,
		}
		ctrl.Log.Info("Manager configured to watch namespaces", "namespaces", watchNamespaces)
	} else {
		ctrl.Log.Info("Manager configured to watch all namespaces (cluster-scoped)")
	}

	// Leader election configuration (enabled by default to allow >1 replica safely)
	leaderEnabled := true
	if v, ok := os.LookupEnv("LEADER_ELECTION_ENABLED"); ok {
		leaderEnabled = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	// Always default Lease to the operator's own namespace (via downward API POD_NAMESPACE)
	podNamespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	leaseDuration := getEnvDuration("LEASE_DURATION", 30*time.Second)
	renewDeadline := getEnvDuration("RENEW_DEADLINE", 15*time.Second)
	retryPeriod := getEnvDuration("RETRY_PERIOD", 5*time.Second)

	mgrOpts.LeaderElection = leaderEnabled
	mgrOpts.LeaderElectionID = "harbor-reef-operator-leader-election"
	mgrOpts.LeaderElectionReleaseOnCancel = true
	if podNamespace != "" {
		mgrOpts.LeaderElectionNamespace = podNamespace
	}
	ld := leaseDuration
	rd := renewDeadline
	rp := retryPeriod
	mgrOpts.LeaseDuration = &ld
	mgrOpts.RenewDeadline = &rd
	mgrOpts.RetryPeriod = &rp

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	r := &podReconciler{client: mgr.GetClient()}

	// Predicate to enqueue:
	// - existing Pods already in ImagePullBackOff/ErrImagePull at startup (Create events from informer sync)
	// - any Update while the Pod remains in that state
	podPred := predicate.Funcs{
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

	// Watches the primary resource (Pods) for create, update, delete events with predicates
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(podPred)).
		Complete(r); err != nil {
		ctrl.Log.Error(err, "Unable to create controller")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

// getWatchNamespace returns the namespace(s) the operator should watch.
func getWatchNamespace() (string, error) {
	const watchNamespaceEnvVar = "WATCH_NAMESPACE"

	ns, found := os.LookupEnv(watchNamespaceEnvVar)
	if !found {
		return "", nil
	}
	return ns, nil
}

// getEnvDuration parses a duration from env or returns default when unset/invalid.
func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		ctrl.Log.Info("Invalid duration in env; using default", "key", key, "value", v, "default", defaultVal.String())
		return defaultVal
	}
	return d
}

// getOriginalUpstreams extracts container->image mappings from the pod's
// harbor.rewrite/original-upstreams JSON annotation.
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

// getPatchedContainers returns the set of container names that have already been patched.
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

// Reconcile attempts to resolve image pull back-off errors for pods by patching
// containers whose images failed to pull, redirecting them to fallback images when necessary.
// Only containers currently experiencing ImagePullBackOff or ErrImagePull states that have not
// already been patched are handled. The function also ensures idempotency by using annotations
// to track containers that have been patched previously.
//
// Returns a reconcile.Result indicating requeue behavior and an error if reconciliation fails.
func (r *podReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
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

	// Only patch containers that are:
	// 1. Currently failing to pull images (in ImagePullBackOff/ErrImagePull)
	// 2. Not already patched in a previous reconciliation
	// This prevents restarting containers that have already started successfully,
	// allows re-processing pods when new containers enter ImagePullBackOff,
	// and ensures each container is only patched once (no loops).
	for idx, c := range pod.Spec.Containers {
		if _, isWaiting := waiting[c.Name]; !isWaiting {
			continue // Skip containers that aren't in ImagePullBackOff
		}
		if _, wasPatched := alreadyPatched[c.Name]; wasPatched {
			continue // Skip containers that were already patched
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
			continue // Skip init containers that aren't in ImagePullBackOff
		}
		if _, wasPatched := alreadyPatched[c.Name]; wasPatched {
			continue // Skip containers that were already patched
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

	// Build the combined list of all patched containers (existing + newly patched)
	allPatched := make([]string, 0, len(alreadyPatched)+len(newlyPatched))
	for name := range alreadyPatched {
		allPatched = append(allPatched, name)
	}
	allPatched = append(allPatched, newlyPatched...)

	// Update the patched-containers annotation
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

	// Update the timestamp annotation (for audit/logging purposes)
	timestampOp := "add"
	if pod.Annotations[lastPatchedTimeKey] != "" {
		timestampOp = "replace"
	}
	patchOps = append(patchOps, patchOp{
		Op:    timestampOp,
		Path:  "/metadata/annotations/" + escapePatchPath(lastPatchedTimeKey),
		Value: time.Now().UTC().Format(time.RFC3339),
	})

	// Update the waiting set to only include newly patched containers for logging/metrics
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

	// Log successful patch with details - only include containers that were actually patched
	patched := patchedContainersFromUpstreams(&pod, upstreams, waiting)
	if len(patched) > 0 {
		logger.Info("Successfully patched pod images to original upstream", "patched", patched)
	} else {
		logger.Info("Successfully patched pod images to original upstream")
	}

	// Emit Prometheus metrics for containers that were actually patched
	for _, c := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		if _, isWaiting := waiting[c.Name]; !isWaiting {
			continue // Only emit metrics for containers we actually patched
		}
		if upstream, ok := upstreams[c.Name]; ok && upstream != "" {
			podsPatchedTotal.WithLabelValues(pod.Namespace, pod.Name, c.Name, upstream).Inc()
		}
	}

	return reconcile.Result{}, nil
}

// isImagePullBackOff determines if any container or init container in the given Pod
// is currently in the "ImagePullBackOff" or "ErrImagePull" waiting state.
// Returns true if at least one container or init container is in one of these states.

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

// waitingContainersSet returns a set of container names currently waiting with pull errors
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

// patchedContainersFromUpstreams returns container->upstream image pairs that were actually patched
// Only includes containers that were in the waiting (ImagePullBackOff) state
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

// escapePatchPath escapes JSON Patch path separators ('/' and '~') according to RFC6901
func escapePatchPath(key string) string {
	escaped := strings.ReplaceAll(key, "~", "~0")
	escaped = strings.ReplaceAll(escaped, "/", "~1")
	return escaped
}

// Ensure the binary links in k8s env
var _ = metav1.LabelSelector{}
