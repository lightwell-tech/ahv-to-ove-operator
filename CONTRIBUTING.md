# Contributing

Thanks for your interest in improving the AHV → OVE Migration Operator! This project
migrates VMs from Nutanix AHV to OpenShift Virtualization (KubeVirt). Contributions of
bug reports, docs, and code are welcome.

## Ground rules

- Be respectful and constructive.
- By contributing you agree your work is licensed under the project's
  [Apache 2.0 License](LICENSE).
- Keep changes focused; one logical change per pull request.

## Development setup

Requirements: Go 1.23+, `controller-gen` (`sigs.k8s.io/controller-tools`), access to an
OpenShift cluster with OpenShift Virtualization and CDI for end-to-end testing.

```bash
# build (also runs fmt + vet)
make build

# unit tests
go test ./...

# regenerate CRDs after editing api/v1alpha1/*_types.go  (REQUIRED — see note below)
controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases

# build image, push to the cluster registry, and roll the operator
make docker-login
make deploy-image
```

> **CRD regeneration is mandatory** after any change to `api/v1alpha1/*_types.go`.
> If you skip it, applying a CR will silently drop the new/renamed fields.

## Testing changes

- Unit tests for pure logic (region merge, convergence, helpers) live in
  `controllers/*_test.go`. Add tests alongside new logic.
- For behavior changes, validate end-to-end against a lab cluster. Sample CRs are in
  `config/samples/`. The warm CBT path has a dedicated sample
  (`config/samples/ahvmigration_rhel8_warm_cbt.yaml`).
- Confirm the migrated VM actually boots (IP assigned, guest agent connected) — a green
  reconcile alone is not sufficient proof of disk consistency.

## Coding conventions

- Match the surrounding style; run `make fmt vet` before committing.
- Keep comments meaningful and, where the surrounding code does, in Japanese — match the
  file you are editing.
- Prefer small, well-named helpers over inline complexity in the reconciler.

## Submitting changes

1. Fork and create a topic branch.
2. Make your change with tests and regenerated CRDs as needed.
3. Ensure `make build` and `go test ./...` pass.
4. Open a pull request describing the problem, the approach, and how you validated it
   (include E2E evidence for behavior changes).

## Reporting bugs

Open an issue with: operator version, AOS/Prism version, OpenShift/CNV version, the
`AHVMigration` spec (redacted), `status.conditions`, and relevant operator + Job logs.

## Documentation

Design and specifications live in [`docs/`](docs/). Update the relevant doc when you change
behavior — especially [`docs/warm-migration-cbt-spec.md`](docs/warm-migration-cbt-spec.md)
for the warm/CBT path and the phase/state-machine table in the README.
