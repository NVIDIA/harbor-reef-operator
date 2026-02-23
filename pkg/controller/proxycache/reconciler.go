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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "harbor-reef-operator/pkg/apis/v1alpha1"
	"harbor-reef-operator/pkg/harbor"
)

const (
	requeueOnError = 30 * time.Second
)

// Reconciler watches ProxyCache CRs and ensures the corresponding
// registry endpoints and proxy projects exist in Harbor.
type Reconciler struct {
	client client.Client

	HarborURL            string
	HarborAdminSecret    string
	HarborAdminSecretKey string
	SecretNamespace      string
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

// SetupWithManager registers the proxycache controller with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ProxyCache{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := ctrl.Log.WithValues("proxycache", req.Name)

	var pc v1alpha1.ProxyCache
	if err := r.client.Get(ctx, req.NamespacedName, &pc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if pc.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
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
		logger.Error(err, "Failed to reconcile proxy cache")
		return r.setErrorStatus(ctx, &pc, err.Error())
	}

	logger.Info("Proxy cache reconciled successfully", "registryId", registryID)
	return r.setReadyStatus(ctx, &pc, registryID)
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

func (r *Reconciler) setReadyStatus(ctx context.Context, pc *v1alpha1.ProxyCache, registryID int) (reconcile.Result, error) {
	now := metav1.Now()
	pc.Status.Phase = "Ready"
	pc.Status.RegistryID = registryID
	pc.Status.ProjectCreated = true
	pc.Status.Message = ""
	pc.Status.LastReconciled = &now

	setCondition(pc, "Ready", metav1.ConditionTrue, "Reconciled", "Registry endpoint and proxy project are configured")

	if err := r.client.Status().Update(ctx, pc); err != nil {
		return reconcile.Result{RequeueAfter: requeueOnError}, err
	}
	return reconcile.Result{}, nil
}

func (r *Reconciler) setErrorStatus(ctx context.Context, pc *v1alpha1.ProxyCache, message string) (reconcile.Result, error) {
	pc.Status.Phase = "Error"
	pc.Status.Message = message

	setCondition(pc, "Ready", metav1.ConditionFalse, "ReconcileError", message)

	if err := r.client.Status().Update(ctx, pc); err != nil {
		ctrl.Log.Error(err, "Failed to update ProxyCache error status", "name", pc.Name)
	}
	return reconcile.Result{RequeueAfter: requeueOnError}, fmt.Errorf("%s", message)
}

func setCondition(pc *v1alpha1.ProxyCache, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range pc.Status.Conditions {
		if c.Type == condType {
			pc.Status.Conditions[i].Status = status
			pc.Status.Conditions[i].Reason = reason
			pc.Status.Conditions[i].Message = message
			pc.Status.Conditions[i].LastTransitionTime = now
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
