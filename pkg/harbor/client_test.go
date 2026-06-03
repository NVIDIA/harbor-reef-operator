// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeRegistryURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"docker.io with https", "https://docker.io", "https://registry-1.docker.io"},
		{"docker.io without scheme", "docker.io", "https://registry-1.docker.io"},
		{"public registry unchanged", "https://registry.example.com", "https://registry.example.com"},
		{"private registry unchanged", "https://private.registry.example.com", "https://private.registry.example.com"},
		{"empty string unchanged", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeRegistryURL(tt.url)
			if got != tt.want {
				t.Errorf("NormalizeRegistryURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestEnsureRegistryEndpoint_ExistingHealthy_NoWrite(t *testing.T) {
	// A healthy existing endpoint must be left untouched: no PUT, no ping.
	var wrote bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries":
			_ = json.NewEncoder(w).Encode([]registryEntry{{ID: 42, Name: "proxy-k8s"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "status": "healthy"})
		case r.Method == http.MethodPut || r.Method == http.MethodPost:
			wrote = true
			http.Error(w, "should not write to a healthy endpoint", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected call", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	id, err := hc.EnsureRegistryEndpoint("proxy-k8s", "https://registry.k8s.io", "docker-registry",
		&RegistryCredential{Type: "basic", AccessKey: "$oauthtoken", AccessSecret: "tok"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 42 {
		t.Errorf("expected registry id=42, got %d", id)
	}
	if wrote {
		t.Error("expected no write (PUT/ping) against a healthy existing endpoint")
	}
}

func TestEnsureRegistryEndpoint_ExistingUnhealthy_PushesAndPings(t *testing.T) {
	// An unhealthy existing endpoint must be re-pushed (PUT) so a rotated
	// credential reaches Harbor, then pinged to refresh health.
	var updated, pinged bool
	var updatePayload registryUpdatePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries":
			_ = json.NewEncoder(w).Encode([]registryEntry{{ID: 42, Name: "proxy-k8s"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "status": "unhealthy"})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v2.0/registries/42":
			updated = true
			_ = json.NewDecoder(r.Body).Decode(&updatePayload)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/registries/ping":
			pinged = true
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected call", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	id, err := hc.EnsureRegistryEndpoint("proxy-k8s", "https://registry.k8s.io", "docker-registry",
		&RegistryCredential{Type: "basic", AccessKey: "$oauthtoken", AccessSecret: "rotated-token"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 42 {
		t.Errorf("expected registry id=42, got %d", id)
	}
	if !updated {
		t.Fatal("expected unhealthy endpoint to be updated (PUT), but it was not")
	}
	if !pinged {
		t.Error("expected a ping after updating an unhealthy endpoint")
	}
	if updatePayload.AccessSecret == nil || *updatePayload.AccessSecret != "rotated-token" {
		t.Errorf("expected rotated access_secret to be pushed, got %v", updatePayload.AccessSecret)
	}
	if updatePayload.AccessKey == nil || *updatePayload.AccessKey != "$oauthtoken" {
		t.Errorf("expected access_key '$oauthtoken' to be pushed, got %v", updatePayload.AccessKey)
	}
}

func TestUpdateRegistryEndpoint_PublicLeavesCredentialsUntouched(t *testing.T) {
	var payload registryUpdatePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/v2.0/registries/7" {
			_ = json.NewDecoder(r.Body).Decode(&payload)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	if err := hc.UpdateRegistryEndpoint(7, "https://registry.k8s.io", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload.URL == nil || *payload.URL != "https://registry.k8s.io" {
		t.Errorf("expected url to be reconciled, got %v", payload.URL)
	}
	if payload.CredentialType != nil || payload.AccessKey != nil || payload.AccessSecret != nil {
		t.Error("expected credential fields to be omitted for a public (nil-cred) update")
	}
}

func TestGetRegistryHealth(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status string
	}{
		{"healthy", "healthy"},
		{"unhealthy", "unhealthy"},
		{"unknown empty", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries/5" {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "status": tt.status})
					return
				}
				http.Error(w, "unexpected call", http.StatusInternalServerError)
			}))
			defer srv.Close()

			hc := NewClient(srv.URL, "admin", "pass")
			got, err := hc.GetRegistryHealth(5)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.status {
				t.Errorf("GetRegistryHealth = %q, want %q", got, tt.status)
			}
		})
	}
}

func TestGetRegistryHealth_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	if _, err := hc.GetRegistryHealth(5); err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}

func TestEnsureRegistryEndpoint_Creates(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries" {
			callCount++
			if callCount == 1 {
				_ = json.NewEncoder(w).Encode([]registryEntry{})
				return
			}
			_ = json.NewEncoder(w).Encode([]registryEntry{{ID: 7, Name: "proxy-private"}})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/registries" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	id, err := hc.EnsureRegistryEndpoint("proxy-private", "https://private.registry.example.com", "docker-registry",
		&RegistryCredential{Type: "basic", AccessKey: "user", AccessSecret: "pw"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 7 {
		t.Errorf("expected registry id=7, got %d", id)
	}
}

func TestEnsureRegistryEndpoint_Conflict(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/registries" {
			callCount++
			if callCount == 1 {
				_ = json.NewEncoder(w).Encode([]registryEntry{})
				return
			}
			_ = json.NewEncoder(w).Encode([]registryEntry{{ID: 3, Name: "proxy-dup"}})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/registries" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	id, err := hc.EnsureRegistryEndpoint("proxy-dup", "https://example.com", "docker-registry", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 3 {
		t.Errorf("expected registry id=3, got %d", id)
	}
}

func TestEnsureRegistryEndpoint_CreateFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]registryEntry{})
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"errors":[{"message":"internal"}]}`))
			return
		}
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	_, err := hc.EnsureRegistryEndpoint("fail-reg", "https://example.com", "docker-registry", nil, "")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestEnsureProxyProject_AlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects" {
			_ = json.NewEncoder(w).Encode([]projectEntry{{ProjectID: 10, Name: "proxy-k8s"}})
			return
		}
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	err := hc.EnsureProxyProject("proxy-k8s", 42, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureProxyProject_FuzzyMatchIgnored(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects" {
			_ = json.NewEncoder(w).Encode([]projectEntry{{ProjectID: 10, Name: "proxy-k8s-extra"}})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/projects" {
			created = true
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	err := hc.EnsureProxyProject("proxy-k8s", 42, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !created {
		t.Error("expected project to be created when only a fuzzy match exists")
	}
}

func TestEnsureProxyProject_Creates(t *testing.T) {
	var receivedPayload projectPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects" {
			_ = json.NewEncoder(w).Encode([]projectEntry{})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/projects" {
			_ = json.NewDecoder(r.Body).Decode(&receivedPayload)
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	err := hc.EnsureProxyProject("proxy-private", 7, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedPayload.ProjectName != "proxy-private" {
		t.Errorf("expected project name 'proxy-private', got %q", receivedPayload.ProjectName)
	}
	if receivedPayload.RegistryID != 7 {
		t.Errorf("expected registry_id=7, got %d", receivedPayload.RegistryID)
	}
	if receivedPayload.Public != false {
		t.Error("expected public=false")
	}
}

func TestEnsureRegistryEndpoint_WithRegion(t *testing.T) {
	var receivedPayload registryPayload
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			callCount++
			if callCount == 1 {
				_ = json.NewEncoder(w).Encode([]registryEntry{})
				return
			}
			_ = json.NewEncoder(w).Encode([]registryEntry{{ID: 99, Name: "proxy-ecr"}})
			return
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&receivedPayload)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	cred := &RegistryCredential{Type: "basic", AccessKey: "AKID", AccessSecret: "SKEY"}
	id, err := hc.EnsureRegistryEndpoint("proxy-ecr", "https://123.dkr.ecr.us-west-2.amazonaws.com", "aws-ecr", cred, "us-west-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 99 {
		t.Errorf("expected id=99, got %d", id)
	}
	if receivedPayload.Type != "aws-ecr" {
		t.Errorf("expected type 'aws-ecr', got %q", receivedPayload.Type)
	}
	if receivedPayload.Region != "us-west-2" {
		t.Errorf("expected region 'us-west-2', got %q", receivedPayload.Region)
	}
}

func TestDeleteAllRepositoriesInProject(t *testing.T) {
	listCalls := 0
	deleted := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects/proxy-gcr/repositories":
			listCalls++
			if listCalls == 1 {
				_ = json.NewEncoder(w).Encode([]repositoryEntry{
					{ID: 1, Name: "proxy-gcr/google-containers/pause"},
					{ID: 2, Name: "proxy-gcr/datadoghq/agent"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode([]repositoryEntry{})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2.0/projects/proxy-gcr/repositories/"):
			// RawPath preserves the on-the-wire %2F encoding that Harbor
			// requires; r.URL.Path is the decoded form.
			raw := r.URL.RawPath
			if raw == "" {
				raw = r.URL.Path
			}
			deleted = append(deleted, strings.TrimPrefix(raw, "/api/v2.0/projects/proxy-gcr/repositories/"))
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	if err := hc.DeleteAllRepositoriesInProject("proxy-gcr"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]bool{
		"google-containers%2Fpause": false,
		"datadoghq%2Fagent":         false,
	}
	for _, d := range deleted {
		if _, ok := want[d]; !ok {
			t.Errorf("unexpected DELETE on %q", d)
			continue
		}
		want[d] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("expected DELETE on %q, did not see it; got %v", k, deleted)
		}
	}
}

func TestDeleteAllRepositoriesInProject_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repositories") {
			_ = json.NewEncoder(w).Encode([]repositoryEntry{})
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	hc := NewClient(srv.URL, "admin", "pass")
	if err := hc.DeleteAllRepositoriesInProject("proxy-empty"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
