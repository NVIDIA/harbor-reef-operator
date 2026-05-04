// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	v1alpha1 "harbor-reef-operator/pkg/apis/v1alpha1"
	podctrl "harbor-reef-operator/pkg/controller/pod"
	pcctrl "harbor-reef-operator/pkg/controller/proxycache"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	watchNamespace, err := getWatchNamespace()
	if err != nil {
		ctrl.Log.Error(err, "unable to get WATCH_NAMESPACE, "+
			"the manager will watch and manage resources in all Namespaces")
	}

	mgrOpts := ctrl.Options{
		Scheme: scheme,
	}

	if watchNamespace != "" {
		watchNamespaces := strings.Split(watchNamespace, ",")
		defaultNamespaces := make(map[string]cache.Config)
		for _, ns := range watchNamespaces {
			trimmedNs := strings.TrimSpace(ns)
			if trimmedNs != "" {
				defaultNamespaces[trimmedNs] = cache.Config{}
			}
		}
		mgrOpts.Cache = cache.Options{
			DefaultNamespaces: defaultNamespaces,
		}
		ctrl.Log.Info("Manager configured to watch namespaces", "namespaces", watchNamespaces)
	} else {
		ctrl.Log.Info("Manager configured to watch all namespaces (cluster-scoped)")
	}

	leaderEnabled := true
	if v, ok := os.LookupEnv("LEADER_ELECTION_ENABLED"); ok {
		leaderEnabled = strings.ToLower(strings.TrimSpace(v)) == "true"
	}
	podNamespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	leaseDuration := getEnvDuration("LEASE_DURATION", 30*time.Second)
	renewDeadline := getEnvDuration("RENEW_DEADLINE", 15*time.Second)
	retryPeriod := getEnvDuration("RETRY_PERIOD", 5*time.Second)

	mgrOpts.LeaderElection = leaderEnabled
	mgrOpts.LeaderElectionID = "harbor-reef-operator-leader-election"
	mgrOpts.LeaderElectionReleaseOnCancel = true
	if podNamespace != "" {
		mgrOpts.LeaderElectionNamespace = podNamespace
	}
	ld := leaseDuration
	rd := renewDeadline
	rp := retryPeriod
	mgrOpts.LeaseDuration = &ld
	mgrOpts.RenewDeadline = &rd
	mgrOpts.RetryPeriod = &rp

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Pod fallback controller
	podReconciler := podctrl.NewReconciler(mgr.GetClient())
	if err := podReconciler.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create pod controller")
		os.Exit(1)
	}

	// ProxyCache controller (opt-in via HARBOR_URL)
	harborURL := strings.TrimSpace(os.Getenv("HARBOR_URL"))
	harborAdminSecret := strings.TrimSpace(os.Getenv("HARBOR_ADMIN_SECRET"))
	if harborAdminSecret == "" {
		harborAdminSecret = "harbor-admin-password"
	}
	harborAdminSecretKey := strings.TrimSpace(os.Getenv("HARBOR_ADMIN_SECRET_KEY"))
	if harborAdminSecretKey == "" {
		harborAdminSecretKey = "HARBOR_ADMIN_PASSWORD"
	}

	if harborURL != "" {
		retainOnDelete := strings.EqualFold(strings.TrimSpace(os.Getenv("HARBOR_RETAIN_ON_DELETE")), "true")
		pcReconciler := pcctrl.NewReconciler(
			mgr.GetClient(), harborURL, harborAdminSecret, harborAdminSecretKey, podNamespace,
		)
		pcReconciler.RetainOnDelete = retainOnDelete
		if err := pcReconciler.SetupWithManager(mgr); err != nil {
			ctrl.Log.Error(err, "unable to create proxycache controller")
			os.Exit(1)
		}
		ctrl.Log.Info("ProxyCache controller enabled", "harborURL", harborURL, "retainOnDelete", retainOnDelete)
	} else {
		ctrl.Log.Info("ProxyCache controller disabled (HARBOR_URL not set)")
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func getWatchNamespace() (string, error) {
	const watchNamespaceEnvVar = "WATCH_NAMESPACE"
	ns, found := os.LookupEnv(watchNamespaceEnvVar)
	if !found {
		return "", nil
	}
	return ns, nil
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		ctrl.Log.Info("Invalid duration in env; using default", "key", key, "value", v, "default", defaultVal.String())
		return defaultVal
	}
	return d
}
