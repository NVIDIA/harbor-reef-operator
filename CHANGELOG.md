# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

Given a version number MAJOR.MINOR.PATCH, increment the:

MAJOR version when you make incompatible API changes,
MINOR version when you add functionality in a backwards compatible manner, and
PATCH version when you make backwards compatible bug fixes.

## [Unreleased]

## [1.2.0] - 2026-06-03

### Added

- ProxyCache controller now reads Harbor's registry endpoint health (`GET /api/v2.0/registries/{id}`) and surfaces it as a new `status.health` field (`healthy`/`unhealthy`/`unknown`), a `Healthy` status condition, and a `Health` printer column (`kubectl get hpc`).
- `proxycache_healthy` Prometheus gauge (labels: `proxycache`, `registry_type`) exposing per-cache health for alerting — Harbor's own exporter does not surface proxy-cache endpoint health. Appears in Datadog as `harbor_reef_operator.proxycache_healthy`.
- Periodic 5-minute requeue on successful ProxyCache reconciles so pushed credentials and reported health stay fresh (Harbor health is time-driven, not Kubernetes-event-driven).
- Chainsaw end-to-end test suite in `test/chainsaw/` covering CRD schema enforcement, cascade-delete with a cached repository, Secret-watch reconcile timing, and Pod-fallback patching on `ImagePullBackOff`. Targets seams that the Go unit suite cannot exercise (CRD YAML, RBAC, controller-runtime watch wiring, real Harbor API contract, JSON6902 patch against a live kubelet).
- Top-level `Makefile` with `chainsaw` and `chainsaw-install` targets.

### Fixed

- ProxyCache controller now re-pushes credentials to an existing Harbor registry endpoint (`PUT /api/v2.0/registries/{id}`, followed by `POST /api/v2.0/registries/ping`) when Harbor reports it **unhealthy**, then leaves healthy endpoints untouched. Previously a **rotated upstream credential never reached Harbor** — `EnsureRegistryEndpoint` returned early when the endpoint existed — leaving the proxy cache authenticating with stale credentials and stuck `unhealthy` even after the Secret was updated. Writes are health-gated so steady-state reconciles perform no writes.

## [1.1.0] - 2026-05-04

### Added

- `ProxyCache` custom resource (`harbor-reef.nvidia.com/v1alpha1`, cluster-scoped, short name `hpc`) and reconciler that declaratively manages Harbor registry endpoints and proxy-cache projects. Supports `public`, `private`, and `aws-ecr-private` upstreams.
- Deletion finalizer that cascade-removes cached repositories, the proxy project, and the registry endpoint when a ProxyCache CR is deleted from Kubernetes.
- `harbor.retainOnDelete` helm value (env: `HARBOR_RETAIN_ON_DELETE`) opt-out that preserves Harbor state when a ProxyCache CR is deleted.
- Watch on Secrets in the operator namespace so creating or updating a referenced credential secret (or the Harbor admin secret) auto-reconciles affected ProxyCaches.

### Changed

- Pod fallback controller moved into `pkg/controller/pod/`; behavior unchanged.
- Helm chart `imagePullSecrets` template now accepts both bare-string lists (`- foo`) and the Kubernetes-native form (`- name: foo`).

### Fixed

- ClusterRole now grants `update` on `proxycaches`, allowing the operator to add and remove the finalizer.
- ProxyCache reconcile errors honor the configured 30-second requeue instead of falling through to controller-runtime's exponential backoff (and the noisy "non-zero result and non-nil error" warning that came with it).

## [1.0.1] - 2026-02-11

### Changed

- Update helm chart default values and use public ngc image

## [1.0.0] - 2026-01-26

### Added

- First release

## [0.0.17] - 2026-01-22

### Added

- Initial commit of pre-release
