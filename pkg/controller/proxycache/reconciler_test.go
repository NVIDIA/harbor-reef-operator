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

// newFakeHarborServerForDelete returns a Harbor stub for finalizer tests.
// repos seeds the repository list returned for the project's first
// /repositories GET; a second GET returns empty to terminate the page loop.
// The handler tracks every DELETE on /repositories/* in deletedRepos.
func newFakeHarborServerForDelete(t *testing.T, repos []string, deletedRepos *[]string) *httptest.Server {
	t.Helper()
	repoListCalls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries":
			json.NewEncoder(w).Encode([]registryEntry{{ID: 42, Name: r.URL.Query().Get("name")}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repositories"):
			repoListCalls++
			if repoListCalls == 1 {
				entries := make([]map[string]any, 0, len(repos))
				for _, name := range repos {
					entries = append(entries, map[string]any{"id": 1, "name": name})
				}
				json.NewEncoder(w).Encode(entries)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/repositories/"):
			if deletedRepos != nil {
				raw := r.URL.RawPath
				if raw == "" {
					raw = r.URL.Path
				}
				*deletedRepos = append(*deletedRepos, raw)
			}
			w.WriteHeader(http.StatusOK)
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
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-private"},
		Spec: v1alpha1.ProxyCacheSpec{
			Type: "private", Name: "proxy-private", URL: "https://private.registry.example.com",
			Credentials: &v1alpha1.CredentialSpec{SecretName: "private-registry-credentials", UsernameKey: "username", PasswordKey: "password"},
		},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "private-registry-credentials", Namespace: "harbor"},
		Data:       map[string][]byte{"username": []byte("test-user"), "password": []byte("test-pass")},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret, credSecret).WithStatusSubresource(pc).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-private"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	var updated v1alpha1.ProxyCache
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-private"}, &updated); err != nil {
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
			ECR: &v1alpha1.ECRSpec{AccountID: "123456789012", Region: "us-west-2", StaticCredentialsSecretName: "ecr-static-credentials"},
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
	if err != nil {
		t.Fatalf("expected nil error so RequeueAfter is honored, got: %v", err)
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

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-unknown"}})
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if result.RequeueAfter != requeueOnError {
		t.Errorf("expected requeue after %v, got %v", requeueOnError, result.RequeueAfter)
	}

	var updated v1alpha1.ProxyCache
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-unknown"}, &updated); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != "Error" {
		t.Errorf("expected phase=Error, got %q", updated.Status.Phase)
	}
}

func TestReconciler_DeletionTimestamp(t *testing.T) {
	srv := newFakeHarborServerForDelete(t, nil, nil)
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

// Verify that deletion cascades repository cleanup before the project delete,
// using a stub that simulates a project with cached images.
func TestReconciler_DeletionCascadesRepositories(t *testing.T) {
	deleted := []string{}
	repos := []string{"proxy-gcr/google-containers/pause", "proxy-gcr/datadoghq/agent"}
	srv := newFakeHarborServerForDelete(t, repos, &deleted)
	defer srv.Close()

	now := metav1.Now()
	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "proxy-gcr",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
		Spec: v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-gcr", URL: "https://gcr.io"},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"},
		Data:       map[string][]byte{"HARBOR_ADMIN_PASSWORD": []byte("admin123")},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc, adminSecret).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-gcr"}}); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	if len(deleted) != 2 {
		t.Fatalf("expected 2 repository deletions, got %d: %v", len(deleted), deleted)
	}
	for _, want := range []string{"google-containers%2Fpause", "datadoghq%2Fagent"} {
		found := false
		for _, got := range deleted {
			if strings.HasSuffix(got, "/"+want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a DELETE on repo %q, got: %v", want, deleted)
		}
	}
}

// Verify that retainOnDelete=true removes the finalizer without calling Harbor.
func TestReconciler_DeletionRetainOnDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("retainOnDelete should not call Harbor; got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	now := metav1.Now()
	pc := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "proxy-keep",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
		Spec: v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-keep", URL: "https://gcr.io"},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pc).Build()
	r := NewReconciler(cl, srv.URL, "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")
	r.RetainOnDelete = true

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "proxy-keep"}}); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	var updated v1alpha1.ProxyCache
	err := cl.Get(context.Background(), types.NamespacedName{Name: "proxy-keep"}, &updated)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected GC after finalizer removal, got err=%v", err)
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

func TestProxyCachesForSecret(t *testing.T) {
	pcA := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-a"},
		Spec: v1alpha1.ProxyCacheSpec{
			Type: "private", Name: "proxy-a", URL: "https://private.registry.example.com",
			Credentials: &v1alpha1.CredentialSpec{SecretName: "private-creds"},
		},
	}
	pcB := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-b"},
		Spec: v1alpha1.ProxyCacheSpec{
			Type: "aws-ecr-private", Name: "proxy-b",
			ECR: &v1alpha1.ECRSpec{AccountID: "1", StaticCredentialsSecretName: "ecr-creds"},
		},
	}
	pcC := &v1alpha1.ProxyCache{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-c"},
		Spec:       v1alpha1.ProxyCacheSpec{Type: "public", Name: "proxy-c", URL: "https://gcr.io"},
	}

	cl := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(pcA, pcB, pcC).Build()
	r := NewReconciler(cl, "http://h", "harbor-admin-password", "HARBOR_ADMIN_PASSWORD", "harbor")

	cases := []struct {
		name       string
		secret     *corev1.Secret
		wantNames  []string
	}{
		{
			name:      "admin secret enqueues all proxycaches",
			secret:    &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "harbor-admin-password", Namespace: "harbor"}},
			wantNames: []string{"proxy-a", "proxy-b", "proxy-c"},
		},
		{
			name:      "private credential secret enqueues only matching CR",
			secret:    &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "private-creds", Namespace: "harbor"}},
			wantNames: []string{"proxy-a"},
		},
		{
			name:      "ecr credential secret enqueues only matching CR",
			secret:    &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ecr-creds", Namespace: "harbor"}},
			wantNames: []string{"proxy-b"},
		},
		{
			name:      "unrelated secret enqueues nothing",
			secret:    &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "harbor"}},
			wantNames: nil,
		},
		{
			name:      "secret in wrong namespace enqueues nothing",
			secret:    &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "private-creds", Namespace: "elsewhere"}},
			wantNames: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.proxyCachesForSecret(context.Background(), tc.secret)
			gotNames := make([]string, 0, len(got))
			for _, req := range got {
				gotNames = append(gotNames, req.Name)
			}
			if len(gotNames) != len(tc.wantNames) {
				t.Fatalf("got %d requests, want %d: got=%v want=%v", len(gotNames), len(tc.wantNames), gotNames, tc.wantNames)
			}
			gotSet := map[string]bool{}
			for _, n := range gotNames {
				gotSet[n] = true
			}
			for _, want := range tc.wantNames {
				if !gotSet[want] {
					t.Errorf("missing expected request %q in %v", want, gotNames)
				}
			}
		})
	}
}

// Ensure harbor package types are accessible (compile-time check).
var _ *harbor.RegistryCredential
