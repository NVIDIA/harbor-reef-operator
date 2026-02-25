// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proxycache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1alpha1 "harbor-reef-operator/pkg/apis/v1alpha1"
	"harbor-reef-operator/pkg/harbor"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var testScheme = runtime.NewScheme()

type registryEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type projectEntry struct {
	ProjectID int `json:"project_id"`
}

func init() {
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = corev1.AddToScheme(testScheme)
	_ = v1alpha1.AddToScheme(testScheme)
}

func newFakeHarborServer(t *testing.T) *httptest.Server {
	t.Helper()
	callCount := map[string]int{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		callCount[key]++

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries":
			if callCount[key] == 1 {
				json.NewEncoder(w).Encode([]registryEntry{})
				return
			}
			json.NewEncoder(w).Encode([]registryEntry{{ID: 1, Name: r.URL.Query().Get("name")}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/registries":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects":
			json.NewEncoder(w).Encode([]projectEntry{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/projects":
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
}

func newFakeHarborServerForDelete(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries":
			json.NewEncoder(w).Encode([]registryEntry{{ID: 42, Name: r.URL.Query().Get("name")}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2.0/projects/"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2.0/registries/"):
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
}

func TestReconciler_PublicCache(t *testing.T) {
	srv := newFakeHarborServer(t)
	defer srv.Close()

	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-k8s"},
		Spec:       v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-k8s", URL: "https://registry.k8s.io"},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret).WithStatusSubresource(pc).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-k8s"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	var updated v1alpha1.ProxyCache
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-k8s"}, &updated); err != nil {
		t.Fatalf("failed to get updated ProxyCache: %v", err)
	}
	if updated.Status.Phase != "Ready" {
		t.Errorf("expected phase=Ready, got %q", updated.Status.Phase)
	}
	if updated.Status.RegistryID != 1 {
		t.Errorf("expected registryId=1, got %d", updated.Status.RegistryID)
	}
	if !updated.Status.ProjectCreated {
		t.Error("expected projectCreated=true")
	}
}

func TestReconciler_PrivateCache(t *testing.T) {
	srv := newFakeHarborServer(t)
	defer srv.Close()

	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-nvcr"},
		Spec: v1alpha1.ProxyCacheSpec{
			Type: "private", Name: "proxy-nvcr", URL: "https://nvcr.io",
			Credentials: &v1alpha1.CredentialSpec{SecretName: "ngc-api-secret", UsernameKey: "username", PasswordKey: "password"},
		},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ngc-api-secret", Namespace: "harbor"},
		Data:       map[string][]byte{"username": []byte("$oauthtoken"), "password": []byte("nvapi-key")},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret, credSecret).WithStatusSubresource(pc).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-nvcr"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	var updated v1alpha1.ProxyCache
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-nvcr"}, &updated); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != "Ready" {
		t.Errorf("expected phase=Ready, got %q", updated.Status.Phase)
	}
}

func TestReconciler_ECRCache(t *testing.T) {
	srv := newFakeHarborServer(t)
	defer srv.Close()

	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-ecr-private"},
		Spec: v1alpha1.ProxyCacheSpec{
			Type: "aws-ecr-private", Name: "proxy-ecr-private",
			ECR: &v1alpha1.ECRSpec{AccountID: "563805952193", Region: "us-west-2", StaticCredentialsSecretName: "ecr-static-credentials"},
		},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}
	ecrSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ecr-static-credentials", Namespace: "harbor"},
		Data:       map[string][]byte{"access_key_id": []byte("AKIAIOSFODNN7EXAMPLE"), "secret_access_key": []byte("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret, ecrSecret).WithStatusSubresource(pc).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-ecr-private"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	var updated v1alpha1.ProxyCache
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-ecr-private"}, &updated); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != "Ready" {
		t.Errorf("expected phase=Ready, got %q", updated.Status.Phase)
	}
}

func TestReconciler_NotFound(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme).Build()
	r := NewReconciler(cl, "http://localhost", "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "does-not-exist"}})
	if err != nil {
		t.Fatalf("expected nil error for not-found, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestReconciler_MissingAdminSecret(t *testing.T) {
	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-test"},
		Spec:       v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-test", URL: "https://registry.k8s.io"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc).WithStatusSubresource(pc).Build()
	r := NewReconciler(cl, "http://localhost:8080", "missing-secret", "HARBOR_ADMIN_PASSWORD", "harbor")

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-test"}})
	if err == nil {
		t.Fatal("expected error for missing admin secret, got nil")
	}
	if result.RequeueAfter != requeueOnError {
		t.Errorf("expected requeue after %v, got %v", requeueOnError, result.RequeueAfter)
	}

	var updated v1alpha1.ProxyCache
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-test"}, &updated); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != "Error" {
		t.Errorf("expected phase=Error, got %q", updated.Status.Phase)
	}
}

func TestReconciler_UnknownType(t *testing.T) {
	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-unknown"},
		Spec:       v1alpha1.ProxyCacheSpec{Type: "unsupported", Name: "proxy-unknown"},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret).WithStatusSubresource(pc).Build()
	r := NewReconciler(cl, "http://localhost:8080", "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-unknown"}})
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
}

func TestReconciler_DeletionTimestamp(t *testing.T) {
	srv := newFakeHarborServerForDelete(t)
	defer srv.Close()

	now := metav1.Now()
	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "proxy-deleting",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
		Spec: v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-deleting", URL: "https://registry.k8s.io"},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-deleting"}})
	if err != nil {
		t.Fatalf("expected nil error for deleting resource, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	// The fake client simulates K8s GC: once the last finalizer is removed
	// from an object with a DeletionTimestamp, the object is deleted.
	var updated v1alpha1.ProxyCache
	err = cl.Get(context.Background(), types.NamespacedName{Name: "proxy-deleting"}, &updated)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected object to be garbage-collected after finalizer removal, got err=%v", err)
	}
}

func TestReconciler_DeletionWithoutFinalizer(t *testing.T) {
	now := metav1.Now()
	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "proxy-no-fin",
			DeletionTimestamp: &now,
			Finalizers:        []string{"some-other-finalizer"},
		},
		Spec: v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-no-fin", URL: "https://registry.k8s.io"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc).Build()
	r := NewReconciler(cl, "http://localhost", "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-no-fin"}})
	if err != nil {
		t.Fatalf("expected nil error when finalizer not present, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestReconciler_AddsFinalizer(t *testing.T) {
	srv := newFakeHarborServer(t)
	defer srv.Close()

	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-fin"},
		Spec:       v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-fin", URL: "https://registry.k8s.io"},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret).WithStatusSubresource(pc).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-fin"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.ProxyCache
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-fin"}, &updated); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	found := false
	for _, f := range updated.Finalizers {
		if f == finalizerName {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected finalizer to be added to ProxyCache")
	}
}

// Ensure harbor package types are accessible (compile-time check).
var _ *harbor.RegistryCredential
