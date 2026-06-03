// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proxycache

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "harbor-reef-operator/pkg/apis/v1alpha1"
	"harbor-reef-operator/pkg/harbor"
)

const (
	requeueOnError = 30 * time.Second
	// requeueHealthy re-runs a successful reconcile on a slow cadence so that
	// (a) a rotated upstream credential is re-pushed to Harbor and (b) the
	// reported health stays fresh. Harbor's endpoint health is derived from a
	// periodic ping and is not driven by Kubernetes events, so without this the
	// status would only refresh on spec/secret changes.
	requeueHealthy = 5 * time.Minute
	finalizerName  = "harbor-reef.nvidia.com/proxycache-finalizer"

	// healthHealthy / healthUnhealthy are the definitive status strings Harbor
	// reports for an endpoint. Anything else (including "" when Harbor has not
	// yet determined health, or when the health read failed) is treated as
	// unknown and must not flip the health gauge — otherwise a transient Harbor
	// API blip would page on-call for a cache that is actually fine.
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
)

// Reconciler watches ProxyCache CRs and ensures the corresponding
// registry endpoints and proxy projects exist in Harbor.
type Reconciler struct {
	client client.Client

	HarborURL            string
	HarborAdminSecret    string
	HarborAdminSecretKey string
	SecretNamespace      string
	// RetainOnDelete, when true, makes the finalizer skip Harbor cleanup
	// (registry endpoint, project, and cached repositories) when a ProxyCache
	// CR is deleted. The finalizer is still removed so the CR can be garbage
	// collected; Harbor state is preserved for manual cleanup.
	RetainOnDelete bool
}

// NewReconciler creates a ProxyCache Reconciler.
func NewReconciler(c client.Client, harborURL, adminSecret, adminSecretKey, secretNamespace string) *Reconciler {
	return &Reconciler{
		client:               c,
		HarborURL:            harborURL,
		HarborAdminSecret:    adminSecret,
		HarborAdminSecretKey: adminSecretKey,
		SecretNamespace:      secretNamespace,
	}
}

// SetupWithManager registers the proxycache controller with the manager. It
// also watches Secrets in the operator's namespace so that creating or
// updating a referenced credential secret (or the Harbor admin secret) will
// auto-reconcile the affected ProxyCache(s) without waiting for the next
// requeue.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ProxyCache{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.proxyCachesForSecret),
		).
		Complete(r)
}

// proxyCachesForSecret maps a Secret event to the set of ProxyCache reconcile
// requests it affects. Updates to the Harbor admin secret enqueue every
// ProxyCache; updates to a private/ECR credential secret enqueue only the
// ProxyCaches that reference it.
func (r *Reconciler) proxyCachesForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Namespace != r.SecretNamespace {
		return nil
	}

	var list v1alpha1.ProxyCacheList
	if err := r.client.List(ctx, &list); err != nil {
		ctrl.Log.Error(err, "Listing ProxyCaches for secret event", "secret", secret.Name)
		return nil
	}

	isAdmin := secret.Name == r.HarborAdminSecret
	var requests []reconcile.Request
	for i := range list.Items {
		pc := &list.Items[i]
		if isAdmin || referencesSecret(pc, secret.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: pc.Name},
			})
		}
	}
	return requests
}

func referencesSecret(pc *v1alpha1.ProxyCache, secretName string) bool {
	if pc.Spec.Credentials != nil && pc.Spec.Credentials.SecretName == secretName {
		return true
	}
	if pc.Spec.ECR != nil && pc.Spec.ECR.StaticCredentialsSecretName == secretName {
		return true
	}
	return false
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := ctrl.Log.WithValues("proxycache", req.Name)

	var pc v1alpha1.ProxyCache
	if err := r.client.Get(ctx, req.NamespacedName, &pc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: clean up Harbor resources before allowing GC.
	if pc.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(&pc, finalizerName) {
			return reconcile.Result{}, nil
		}
		if r.RetainOnDelete {
			logger.Info("ProxyCache being deleted; retainOnDelete=true, preserving Harbor resources")
		} else {
			logger.Info("ProxyCache being deleted, cleaning up Harbor resources")
			adminPass, err := r.readSecretKey(ctx, r.HarborAdminSecret, r.HarborAdminSecretKey)
			if err != nil {
				return reconcile.Result{RequeueAfter: requeueOnError},
					fmt.Errorf("reading harbor admin secret for cleanup: %w", err)
			}
			hc := harbor.NewClient(r.HarborURL, "admin", adminPass)
			if err := deleteHarborResources(ctx, &pc, hc); err != nil {
				return reconcile.Result{RequeueAfter: requeueOnError},
					fmt.Errorf("deleting harbor resources for %q: %w", pc.Spec.Name, err)
			}
		}
		// Drop the health gauge series so a deleted ProxyCache does not linger
		// as a stale timeseries until the operator restarts.
		deleteHealthMetric(pc.Name, pc.Spec.Type)
		controllerutil.RemoveFinalizer(&pc, finalizerName)
		if err := r.client.Update(ctx, &pc); err != nil {
			return reconcile.Result{}, err
		}
		logger.Info("Finalizer removed")
		return reconcile.Result{}, nil
	}

	// Ensure our finalizer is present before proceeding.
	if !controllerutil.ContainsFinalizer(&pc, finalizerName) {
		controllerutil.AddFinalizer(&pc, finalizerName)
		if err := r.client.Update(ctx, &pc); err != nil {
			return reconcile.Result{}, err
		}
	}

	adminPass, err := r.readSecretKey(ctx, r.HarborAdminSecret, r.HarborAdminSecretKey)
	if err != nil {
		return r.setErrorStatus(ctx, &pc, fmt.Sprintf("reading harbor admin secret: %v", err))
	}

	if r.HarborURL == "" {
		return r.setErrorStatus(ctx, &pc, "HARBOR_URL not configured")
	}

	hc := harbor.NewClient(r.HarborURL, "admin", adminPass)
	logger.Info("Reconciling proxy cache", "type", pc.Spec.Type, "name", pc.Spec.Name)

	var registryID int

	switch pc.Spec.Type {
	case "public":
		registryID, err = r.reconcilePublic(ctx, hc, &pc)
	case "private":
		registryID, err = r.reconcilePrivate(ctx, hc, &pc)
	case "aws-ecr-private":
		registryID, err = r.reconcileECR(ctx, hc, &pc)
	default:
		return r.setErrorStatus(ctx, &pc, fmt.Sprintf("unknown proxy cache type: %q", pc.Spec.Type))
	}

	if err != nil {
		return r.setErrorStatus(ctx, &pc, err.Error())
	}

	// The configuration is reconciled; now read the health Harbor derives from
	// its periodic upstream ping. A failure to read health is not fatal to the
	// reconcile (the config is correct) — we record it as unknown and continue.
	health, herr := hc.GetRegistryHealth(registryID)
	if herr != nil {
		logger.Error(herr, "Failed to read registry health from Harbor", "registryId", registryID)
		health = ""
	}

	logger.Info("Proxy cache reconciled successfully", "registryId", registryID, "health", health)
	return r.setReadyStatus(ctx, &pc, registryID, health)
}

func (r *Reconciler) reconcilePublic(_ context.Context, hc *harbor.Client, pc *v1alpha1.ProxyCache) (int, error) {
	registryURL := harbor.NormalizeRegistryURL(pc.Spec.URL)
	registryID, err := hc.EnsureRegistryEndpoint(pc.Spec.Name, registryURL, "docker-registry", nil, "")
	if err != nil {
		return 0, fmt.Errorf("ensuring registry endpoint: %w", err)
	}
	if err := hc.EnsureProxyProject(pc.Spec.Name, registryID, true); err != nil {
		return 0, fmt.Errorf("ensuring proxy project: %w", err)
	}
	return registryID, nil
}

func (r *Reconciler) reconcilePrivate(ctx context.Context, hc *harbor.Client, pc *v1alpha1.ProxyCache) (int, error) {
	if pc.Spec.Credentials == nil {
		return 0, fmt.Errorf("credentials required for private proxy cache %q", pc.Spec.Name)
	}

	usernameKey := pc.Spec.Credentials.UsernameKey
	if usernameKey == "" {
		usernameKey = "username"
	}
	passwordKey := pc.Spec.Credentials.PasswordKey
	if passwordKey == "" {
		passwordKey = "password"
	}

	username, err := r.readSecretKey(ctx, pc.Spec.Credentials.SecretName, usernameKey)
	if err != nil {
		return 0, fmt.Errorf("reading credential username: %w", err)
	}
	password, err := r.readSecretKey(ctx, pc.Spec.Credentials.SecretName, passwordKey)
	if err != nil {
		return 0, fmt.Errorf("reading credential password: %w", err)
	}

	registryURL := harbor.NormalizeRegistryURL(pc.Spec.URL)
	cred := &harbor.RegistryCredential{
		Type:         "basic",
		AccessKey:    username,
		AccessSecret: password,
	}

	registryID, err := hc.EnsureRegistryEndpoint(pc.Spec.Name, registryURL, "docker-registry", cred, "")
	if err != nil {
		return 0, fmt.Errorf("ensuring registry endpoint: %w", err)
	}
	if err := hc.EnsureProxyProject(pc.Spec.Name, registryID, false); err != nil {
		return 0, fmt.Errorf("ensuring proxy project: %w", err)
	}
	return registryID, nil
}

func (r *Reconciler) reconcileECR(ctx context.Context, hc *harbor.Client, pc *v1alpha1.ProxyCache) (int, error) {
	if pc.Spec.ECR == nil {
		return 0, fmt.Errorf("ecr config required for aws-ecr-private proxy cache %q", pc.Spec.Name)
	}

	region := pc.Spec.ECR.Region
	if region == "" {
		region = "us-west-2"
	}

	accessKeyID, err := r.readSecretKey(ctx, pc.Spec.ECR.StaticCredentialsSecretName, "access_key_id")
	if err != nil {
		return 0, fmt.Errorf("reading ECR access key: %w", err)
	}
	secretAccessKey, err := r.readSecretKey(ctx, pc.Spec.ECR.StaticCredentialsSecretName, "secret_access_key")
	if err != nil {
		return 0, fmt.Errorf("reading ECR secret key: %w", err)
	}

	registryURL := fmt.Sprintf("https://%s.dkr.ecr.%s.amazonaws.com", pc.Spec.ECR.AccountID, region)
	cred := &harbor.RegistryCredential{
		Type:         "basic",
		AccessKey:    accessKeyID,
		AccessSecret: secretAccessKey,
	}

	registryID, err := hc.EnsureRegistryEndpoint(pc.Spec.Name, registryURL, "aws-ecr", cred, region)
	if err != nil {
		return 0, fmt.Errorf("ensuring registry endpoint: %w", err)
	}
	if err := hc.EnsureProxyProject(pc.Spec.Name, registryID, false); err != nil {
		return 0, fmt.Errorf("ensuring proxy project: %w", err)
	}
	return registryID, nil
}

func (r *Reconciler) readSecretKey(ctx context.Context, secretName, key string) (string, error) {
	var secret corev1.Secret
	nn := types.NamespacedName{Name: secretName, Namespace: r.SecretNamespace}
	if err := r.client.Get(ctx, nn, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("secret %q not found in namespace %q", secretName, r.SecretNamespace)
		}
		return "", err
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %q", key, secretName)
	}
	return string(val), nil
}

func (r *Reconciler) setReadyStatus(ctx context.Context, pc *v1alpha1.ProxyCache, registryID int, health string) (reconcile.Result, error) {
	now := metav1.Now()
	pc.Status.Phase = "Ready"
	pc.Status.RegistryID = registryID
	pc.Status.ProjectCreated = true
	pc.Status.Message = ""
	pc.Status.LastReconciled = &now

	setCondition(pc, "Ready", metav1.ConditionTrue, "Reconciled", "Registry endpoint and proxy project are configured")

	// The health gauge mirrors Harbor's reported endpoint health and only flips
	// on a definitive reading. An unknown/empty reading (Harbor not yet decided,
	// or a transient read failure) leaves the gauge at its previous value so it
	// does not produce spurious "unhealthy" alerts.
	switch health {
	case healthHealthy:
		pc.Status.Health = healthHealthy
		setCondition(pc, "Healthy", metav1.ConditionTrue, "RegistryHealthy", "Harbor reports the registry endpoint as healthy")
		setHealthMetric(pc.Name, pc.Spec.Type, true)
	case healthUnhealthy:
		pc.Status.Health = healthUnhealthy
		setCondition(pc, "Healthy", metav1.ConditionFalse, "RegistryUnhealthy",
			"Harbor reports the registry endpoint as unhealthy (often a rejected or stale upstream credential)")
		setHealthMetric(pc.Name, pc.Spec.Type, false)
	default:
		pc.Status.Health = "unknown"
		setCondition(pc, "Healthy", metav1.ConditionUnknown, "HealthUnknown",
			"Harbor health for the registry endpoint is unknown (not yet determined or could not be read)")
		// gauge intentionally left unchanged
	}

	if err := r.client.Status().Update(ctx, pc); err != nil {
		return reconcile.Result{RequeueAfter: requeueOnError}, err
	}
	// Requeue on a slow cadence to re-push credentials and refresh health even
	// when nothing in Kubernetes changes.
	return reconcile.Result{RequeueAfter: requeueHealthy}, nil
}

func (r *Reconciler) setErrorStatus(ctx context.Context, pc *v1alpha1.ProxyCache, message string) (reconcile.Result, error) {
	pc.Status.Phase = "Error"
	pc.Status.Message = message
	pc.Status.Health = "unknown"

	// A reconcile error (e.g. a transient secret/Harbor read failure) does not
	// mean Harbor's endpoint is unhealthy — the cache may still be serving from
	// its existing config. Leave the health gauge untouched so operator-side
	// hiccups do not page on-call; the Error phase + Ready=False condition carry
	// that signal instead.
	setCondition(pc, "Ready", metav1.ConditionFalse, "ReconcileError", message)
	setCondition(pc, "Healthy", metav1.ConditionUnknown, "ReconcileError", message)

	ctrl.Log.Info("ProxyCache reconcile error", "proxycache", pc.Name, "error", message)

	if err := r.client.Status().Update(ctx, pc); err != nil {
		ctrl.Log.Error(err, "Failed to update ProxyCache error status", "name", pc.Name)
	}
	// Returning nil error so controller-runtime honors RequeueAfter instead of
	// applying exponential backoff. The watch on referenced Secrets means most
	// errors caused by missing/bad credentials will be retried as soon as the
	// secret is created or updated, without waiting for this requeue.
	return reconcile.Result{RequeueAfter: requeueOnError}, nil
}

// deleteHarborResources removes cached repositories, the proxy project, and
// the registry endpoint from Harbor. Repositories are removed first because
// Harbor refuses to delete a project that still contains them. All calls are
// idempotent: already-absent resources are not errors.
func deleteHarborResources(_ context.Context, pc *v1alpha1.ProxyCache, hc *harbor.Client) error {
	if err := hc.DeleteAllRepositoriesInProject(pc.Spec.Name); err != nil {
		return fmt.Errorf("emptying proxy project %q: %w", pc.Spec.Name, err)
	}
	if err := hc.DeleteProject(pc.Spec.Name); err != nil {
		return fmt.Errorf("deleting proxy project %q: %w", pc.Spec.Name, err)
	}
	if err := hc.DeleteRegistryEndpoint(pc.Spec.Name); err != nil {
		return fmt.Errorf("deleting registry endpoint %q: %w", pc.Spec.Name, err)
	}
	return nil
}

func setCondition(pc *v1alpha1.ProxyCache, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range pc.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				pc.Status.Conditions[i].LastTransitionTime = now
			}
			pc.Status.Conditions[i].Status = status
			pc.Status.Conditions[i].Reason = reason
			pc.Status.Conditions[i].Message = message
			pc.Status.Conditions[i].ObservedGeneration = pc.Generation
			return
		}
	}
	pc.Status.Conditions = append(pc.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: pc.Generation,
	})
}
