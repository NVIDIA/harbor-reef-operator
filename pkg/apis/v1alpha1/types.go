// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProxyCache represents a Harbor proxy-cache project backed by an external
// registry endpoint. The operator reconciles each ProxyCache resource by
// ensuring the corresponding registry endpoint and project exist in Harbor.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ProxyCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxyCacheSpec   `json:"spec"`
	Status ProxyCacheStatus `json:"status,omitempty"`
}

// ProxyCacheSpec defines the desired state for a Harbor proxy cache.
type ProxyCacheSpec struct {
	// Type of upstream registry: "public", "private", or "aws-ecr-private".
	Type string `json:"type"`
	// Name used for both the Harbor registry endpoint and the proxy project.
	Name string `json:"name"`
	// URL of the upstream registry (not required for aws-ecr-private).
	URL string `json:"url,omitempty"`
	// Credentials for private registry authentication.
	Credentials *CredentialSpec `json:"credentials,omitempty"`
	// ECR-specific configuration for aws-ecr-private type.
	ECR *ECRSpec `json:"ecr,omitempty"`
}

// CredentialSpec references a Kubernetes Secret containing registry credentials.
type CredentialSpec struct {
	// Name of the Secret containing the registry credentials.
	SecretName string `json:"secretName"`
	// Key within the Secret for the username. Defaults to "username".
	UsernameKey string `json:"usernameKey,omitempty"`
	// Key within the Secret for the password. Defaults to "password".
	PasswordKey string `json:"passwordKey,omitempty"`
}

// ECRSpec holds AWS ECR-specific configuration.
type ECRSpec struct {
	// AWS account ID that owns the ECR registry.
	AccountID string `json:"accountId"`
	// AWS region for the ECR registry. Falls back to operator default if empty.
	Region string `json:"region,omitempty"`
	// Name of the Secret containing ECR static credentials
	// (keys: access_key_id, secret_access_key).
	StaticCredentialsSecretName string `json:"staticCredentialsSecretName"`
}

// ProxyCacheStatus reports the observed state of the proxy cache in Harbor.
type ProxyCacheStatus struct {
	// Phase of the proxy cache: Pending, Ready, or Error. Reflects whether the
	// operator successfully reconciled the Harbor configuration, independent of
	// upstream health.
	Phase string `json:"phase,omitempty"`
	// Health is the status Harbor reports for the registry endpoint: "healthy",
	// "unhealthy", or "unknown". Unlike Phase, this reflects whether Harbor can
	// actually reach and authenticate to the upstream registry.
	Health string `json:"health,omitempty"`
	// Harbor registry endpoint ID created for this proxy cache.
	RegistryID int `json:"registryId,omitempty"`
	// Whether the Harbor proxy project has been created.
	ProjectCreated bool `json:"projectCreated,omitempty"`
	// Human-readable message with details about the current phase.
	Message string `json:"message,omitempty"`
	// Timestamp of the last successful reconciliation.
	LastReconciled *metav1.Time `json:"lastReconciled,omitempty"`
	// Standard Kubernetes conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ProxyCacheList is a list of ProxyCache resources.
//
// +kubebuilder:object:root=true
type ProxyCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProxyCache `json:"items"`
}
