// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
	"time"
)

func TestGetWatchNamespace(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		envSet   bool
		want     string
	}{
		{"not set", "", false, ""},
		{"single namespace", "default", true, "default"},
		{"multiple namespaces", "ns1,ns2,ns3", true, "ns1,ns2,ns3"},
		{"empty string", "", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Unsetenv("WATCH_NAMESPACE")
			if tt.envSet {
				os.Setenv("WATCH_NAMESPACE", tt.envValue)
			}
			got, err := getWatchNamespace()
			if err != nil {
				t.Errorf("getWatchNamespace() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("getWatchNamespace() = %v, want %v", got, tt.want)
			}
			os.Unsetenv("WATCH_NAMESPACE")
		})
	}
}

func TestGetEnvDuration(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		envValue   string
		envSet     bool
		defaultVal time.Duration
		want       time.Duration
	}{
		{"not set - uses default", "TEST_DURATION", "", false, 30 * time.Second, 30 * time.Second},
		{"valid duration", "TEST_DURATION", "45s", true, 30 * time.Second, 45 * time.Second},
		{"valid duration with minutes", "TEST_DURATION", "2m", true, 30 * time.Second, 2 * time.Minute},
		{"invalid duration - uses default", "TEST_DURATION", "invalid", true, 30 * time.Second, 30 * time.Second},
		{"empty string - uses default", "TEST_DURATION", "", true, 30 * time.Second, 30 * time.Second},
		{"whitespace only - uses default", "TEST_DURATION", "   ", true, 30 * time.Second, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Unsetenv(tt.key)
			if tt.envSet {
				os.Setenv(tt.key, tt.envValue)
			}
			got := getEnvDuration(tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getEnvDuration() = %v, want %v", got, tt.want)
			}
			os.Unsetenv(tt.key)
		})
	}
}
