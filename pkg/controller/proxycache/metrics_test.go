// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proxycache

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHealthMetricSetAndDelete(t *testing.T) {
	const name, regType = "metrics-test-pc", "public"

	setHealthMetric(name, regType, true)
	if v := testutil.ToFloat64(proxyCacheHealthy.WithLabelValues(name, regType)); v != 1 {
		t.Fatalf("expected gauge 1 after healthy, got %v", v)
	}

	setHealthMetric(name, regType, false)
	if v := testutil.ToFloat64(proxyCacheHealthy.WithLabelValues(name, regType)); v != 0 {
		t.Fatalf("expected gauge 0 after unhealthy, got %v", v)
	}

	// deleteHealthMetric must drop the series; deleting an absent series
	// returns false, so a second delete confirms the first one removed it.
	deleteHealthMetric(name, regType)
	if proxyCacheHealthy.DeleteLabelValues(name, regType) {
		t.Error("expected series to be gone after deleteHealthMetric")
	}
}
