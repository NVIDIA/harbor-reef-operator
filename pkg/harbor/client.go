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

type projectEntry struct {
	ProjectID int    `json:"project_id"`
	Name      string `json:"name"`
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
	return id, nil
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
