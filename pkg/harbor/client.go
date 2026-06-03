// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client communicates with the Harbor v2.0 REST API.
type Client struct {
	BaseURL    string
	AdminUser  string
	AdminPass  string
	HTTPClient *http.Client
}

// NewClient creates a Client pointing at the given Harbor Core URL.
func NewClient(baseURL, adminUser, adminPass string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		AdminUser: adminUser,
		AdminPass: adminPass,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RegistryCredential holds optional basic-auth credentials for a registry endpoint.
type RegistryCredential struct {
	AccessKey    string `json:"access_key"`
	AccessSecret string `json:"access_secret"`
	Type         string `json:"type"` // "basic"
}

type registryPayload struct {
	Name       string              `json:"name"`
	Type       string              `json:"type"` // "docker-registry" or "aws-ecr"
	URL        string              `json:"url"`
	Insecure   bool                `json:"insecure"`
	Credential *RegistryCredential `json:"credential,omitempty"`
	Region     string              `json:"region,omitempty"`
}

type registryEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// registryUpdatePayload is Harbor's RegistryUpdate body for
// PUT /api/v2.0/registries/{id}. Fields are pointers so only the values we
// intend to change are serialized.
//
// IMPORTANT: the credential here is FLAT (credential_type/access_key/
// access_secret), which is intentional and required. Harbor's RegistryUpdate
// model differs from the create Registry model: create nests credentials under
// a `credential` object, update flattens them as top-level fields. Do not
// "align" this with registryPayload by nesting under `credential` — Harbor
// silently ignores the unknown nested key on update, the PUT still returns 200,
// and the rotated credential never lands (the exact stale-credential bug this
// reconcile path exists to fix). Verified against the Harbor v2.0 swagger
// (RegistryUpdate) and empirically against live Harbor (flat PUT flips a
// 401/unhealthy proxy cache back to healthy).
type registryUpdatePayload struct {
	URL            *string `json:"url,omitempty"`
	CredentialType *string `json:"credential_type,omitempty"`
	AccessKey      *string `json:"access_key,omitempty"`
	AccessSecret   *string `json:"access_secret,omitempty"`
	Insecure       *bool   `json:"insecure,omitempty"`
}

// registryStatusHealthy is the status string Harbor reports for a registry
// endpoint whose upstream is reachable and authenticating.
const registryStatusHealthy = "healthy"

// registryDetail is the subset of the Harbor registry object returned by
// GET /api/v2.0/registries/{id}. Status is the health Harbor derives from its
// periodic ping of the upstream ("healthy", "unhealthy", or "" when unknown).
type registryDetail struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type projectEntry struct {
	ProjectID int    `json:"project_id"`
	Name      string `json:"name"`
}

type repositoryEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"` // formatted as "<project>/<repository>"
}

type projectPayload struct {
	ProjectName string `json:"project_name"`
	Public      bool   `json:"public"`
	RegistryID  int    `json:"registry_id"`
}

// NormalizeRegistryURL rewrites docker.io to the actual Docker Hub endpoint.
func NormalizeRegistryURL(rawURL string) string {
	if strings.Contains(rawURL, "docker.io") {
		return "https://registry-1.docker.io"
	}
	return rawURL
}

// EnsureRegistryEndpoint creates the Harbor registry endpoint if it does not
// already exist. Returns the Harbor-assigned registry ID.
func (h *Client) EnsureRegistryEndpoint(name, registryURL, registryType string, cred *RegistryCredential, region string) (int, error) {
	id, err := h.findRegistry(name)
	if err != nil {
		return 0, fmt.Errorf("looking up registry %q: %w", name, err)
	}
	if id > 0 {
		// The endpoint already exists. Re-push the URL/credentials only when
		// Harbor reports it unhealthy — e.g. after a credential rotation that
		// left Harbor holding a stale secret. A healthy endpoint is left
		// untouched, so steady-state reconciles perform no writes.
		health, err := h.GetRegistryHealth(id)
		if err != nil {
			return 0, fmt.Errorf("checking health of registry %q (id=%d): %w", name, id, err)
		}
		if health != registryStatusHealthy {
			if err := h.UpdateRegistryEndpoint(id, registryURL, cred); err != nil {
				return 0, fmt.Errorf("updating registry %q (id=%d): %w", name, id, err)
			}
			// Nudge Harbor to re-evaluate health now instead of waiting for its
			// next scheduled ping (best-effort: the credential is already
			// pushed, and the reconciler re-reads health regardless).
			_ = h.PingRegistry(id)
		}
		return id, nil
	}

	payload := registryPayload{
		Name:     name,
		Type:     registryType,
		URL:      registryURL,
		Insecure: false,
	}
	if cred != nil {
		payload.Credential = cred
	}
	if region != "" {
		payload.Region = region
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshalling registry payload: %w", err)
	}

	code, respBody, err := h.doRequest(http.MethodPost, "/api/v2.0/registries", body)
	if err != nil {
		return 0, fmt.Errorf("creating registry %q: %w", name, err)
	}
	if code != http.StatusCreated && code != http.StatusConflict {
		return 0, fmt.Errorf("creating registry %q: HTTP %d: %s", name, code, string(respBody))
	}

	id, err = h.findRegistry(name)
	if err != nil {
		return 0, fmt.Errorf("re-fetching registry %q after create: %w", name, err)
	}
	if id == 0 {
		return 0, fmt.Errorf("registry %q not found after creation", name)
	}
	// Nudge Harbor to evaluate health now so a freshly created endpoint reports
	// healthy promptly rather than sitting "unknown" until the next ping cycle
	// (best-effort).
	_ = h.PingRegistry(id)
	return id, nil
}

// UpdateRegistryEndpoint pushes the desired URL and credentials to an existing
// Harbor registry endpoint via PUT /api/v2.0/registries/{id}. This is how a
// rotated credential propagates to Harbor. When cred is nil (public upstreams)
// only the URL is reconciled; the credential fields are left untouched.
func (h *Client) UpdateRegistryEndpoint(id int, registryURL string, cred *RegistryCredential) error {
	insecure := false
	payload := registryUpdatePayload{
		URL:      &registryURL,
		Insecure: &insecure,
	}
	if cred != nil {
		credType := cred.Type
		accessKey := cred.AccessKey
		accessSecret := cred.AccessSecret
		payload.CredentialType = &credType
		payload.AccessKey = &accessKey
		payload.AccessSecret = &accessSecret
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling registry update payload: %w", err)
	}

	code, respBody, err := h.doRequest(http.MethodPut, fmt.Sprintf("/api/v2.0/registries/%d", id), body)
	if err != nil {
		return fmt.Errorf("updating registry id=%d: %w", id, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("updating registry id=%d: HTTP %d: %s", id, code, string(respBody))
	}
	return nil
}

// GetRegistryHealth returns the health status Harbor reports for a registry
// endpoint ("healthy", "unhealthy", or "" when Harbor has not yet determined
// it). Harbor computes this from a periodic ping of the upstream, so a stale or
// rejected credential surfaces here as "unhealthy".
func (h *Client) GetRegistryHealth(id int) (string, error) {
	code, body, err := h.doRequest(http.MethodGet, fmt.Sprintf("/api/v2.0/registries/%d", id), nil)
	if err != nil {
		return "", fmt.Errorf("getting registry id=%d: %w", id, err)
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("getting registry id=%d: HTTP %d: %s", id, code, string(body))
	}

	var detail registryDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return "", fmt.Errorf("decoding registry detail for id=%d: %w", id, err)
	}
	return detail.Status, nil
}

// PingRegistry asks Harbor to test connectivity and authentication to an
// existing registry endpoint (POST /api/v2.0/registries/ping). A 200 means the
// upstream is reachable and the stored credentials authenticate; the call also
// refreshes the endpoint's reported health so recovery is reflected promptly
// after a credential update.
func (h *Client) PingRegistry(id int) error {
	body, err := json.Marshal(map[string]int{"id": id})
	if err != nil {
		return fmt.Errorf("marshalling ping payload: %w", err)
	}
	code, respBody, err := h.doRequest(http.MethodPost, "/api/v2.0/registries/ping", body)
	if err != nil {
		return fmt.Errorf("pinging registry id=%d: %w", id, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("pinging registry id=%d: HTTP %d: %s", id, code, string(respBody))
	}
	return nil
}

// EnsureProxyProject creates the Harbor proxy-cache project linked to the given
// registry endpoint if it does not already exist.
func (h *Client) EnsureProxyProject(name string, registryID int, public bool) error {
	exists, err := h.projectExists(name)
	if err != nil {
		return fmt.Errorf("checking project %q: %w", name, err)
	}
	if exists {
		return nil
	}

	payload := projectPayload{
		ProjectName: name,
		Public:      public,
		RegistryID:  registryID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling project payload: %w", err)
	}

	code, respBody, err := h.doRequest(http.MethodPost, "/api/v2.0/projects", body)
	if err != nil {
		return fmt.Errorf("creating project %q: %w", name, err)
	}
	if code != http.StatusCreated && code != http.StatusConflict {
		return fmt.Errorf("creating project %q: HTTP %d: %s", name, code, string(respBody))
	}
	return nil
}

// DeleteAllRepositoriesInProject removes every repository (and all its
// artifacts) from the named project. Harbor refuses to delete a project that
// still contains repositories, so this must be called before DeleteProject for
// any project that has been pulled through.
//
// Idempotent: empty/missing projects return nil.
func (h *Client) DeleteAllRepositoriesInProject(projectName string) error {
	page := 1
	const pageSize = 100
	prefix := projectName + "/"

	var all []repositoryEntry
	for {
		path := fmt.Sprintf("/api/v2.0/projects/%s/repositories?page=%d&page_size=%d",
			url.PathEscape(projectName), page, pageSize)
		code, body, err := h.doRequest(http.MethodGet, path, nil)
		if err != nil {
			return fmt.Errorf("listing repositories in %q: %w", projectName, err)
		}
		if code == http.StatusNotFound {
			return nil
		}
		if code != http.StatusOK {
			return fmt.Errorf("listing repositories in %q: HTTP %d: %s", projectName, code, string(body))
		}

		var repos []repositoryEntry
		if err := json.Unmarshal(body, &repos); err != nil {
			return fmt.Errorf("decoding repository list for %q: %w", projectName, err)
		}
		all = append(all, repos...)
		if len(repos) < pageSize {
			break
		}
		page++
	}

	for _, r := range all {
		// Harbor returns names as "<project>/<repo>"; the DELETE path takes the
		// repo portion with internal slashes percent-encoded.
		repoName := strings.TrimPrefix(r.Name, prefix)
		encoded := strings.ReplaceAll(repoName, "/", "%2F")
		delPath := fmt.Sprintf("/api/v2.0/projects/%s/repositories/%s",
			url.PathEscape(projectName), encoded)
		code, body, err := h.doRequest(http.MethodDelete, delPath, nil)
		if err != nil {
			return fmt.Errorf("deleting repository %q: %w", r.Name, err)
		}
		if code != http.StatusOK && code != http.StatusNotFound {
			return fmt.Errorf("deleting repository %q: HTTP %d: %s", r.Name, code, string(body))
		}
	}
	return nil
}

// DeleteProject deletes a Harbor project by name.
// Returns nil if the project does not exist (idempotent).
func (h *Client) DeleteProject(name string) error {
	path := fmt.Sprintf("/api/v2.0/projects/%s", url.PathEscape(name))
	code, body, err := h.doRequest(http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("deleting project %q: %w", name, err)
	}
	if code == http.StatusOK || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("deleting project %q: HTTP %d: %s", name, code, string(body))
}

// DeleteRegistryEndpoint deletes a Harbor registry endpoint by name.
// Returns nil if the registry does not exist (idempotent).
func (h *Client) DeleteRegistryEndpoint(name string) error {
	id, err := h.findRegistry(name)
	if err != nil {
		return fmt.Errorf("looking up registry %q for deletion: %w", name, err)
	}
	if id == 0 {
		return nil
	}
	path := fmt.Sprintf("/api/v2.0/registries/%d", id)
	code, body, err := h.doRequest(http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("deleting registry %q (id=%d): %w", name, id, err)
	}
	if code == http.StatusOK || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("deleting registry %q (id=%d): HTTP %d: %s", name, id, code, string(body))
}

func (h *Client) findRegistry(name string) (int, error) {
	path := fmt.Sprintf("/api/v2.0/registries?name=%s&page_size=100", url.QueryEscape(name))
	code, body, err := h.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d listing registries: %s", code, string(body))
	}

	var registries []registryEntry
	if err := json.Unmarshal(body, &registries); err != nil {
		return 0, fmt.Errorf("decoding registry list: %w", err)
	}

	for _, r := range registries {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, nil
}

func (h *Client) projectExists(name string) (bool, error) {
	path := fmt.Sprintf("/api/v2.0/projects?name=%s", url.QueryEscape(name))
	code, body, err := h.doRequest(http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d listing projects: %s", code, string(body))
	}

	var projects []projectEntry
	if err := json.Unmarshal(body, &projects); err != nil {
		return false, fmt.Errorf("decoding project list: %w", err)
	}

	for _, p := range projects {
		if p.ProjectID > 0 && p.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (h *Client) doRequest(method, path string, body []byte) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = strings.NewReader(string(body))
	}

	req, err := http.NewRequest(method, h.BaseURL+path, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(h.AdminUser, h.AdminPass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}
