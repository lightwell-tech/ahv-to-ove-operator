# Release & OperatorHub Submission

How to publish a release image and submit the operator to the community OperatorHub
catalog. The bundle in `bundle/` targets **GHCR** (`ghcr.io/lightwell-tech`) and ships the
operator only (the Console Plugin is not included in v0.1.x — it can be added in a later
release).

## 1. Build & push the operator image (GHCR)

> **Easiest path:** push a `v*` tag — the `Release` workflow builds and pushes
> `ghcr.io/lightwell-tech/ahv-to-ove-operator` to GHCR automatically using `GITHUB_TOKEN`
> (no registry secrets needed) and creates the GitHub Release:
> `git tag v0.1.0 && git push origin v0.1.0`
>
> Manual alternative:

```bash
export VERSION=v0.1.0
export IMG=ghcr.io/lightwell-tech/ahv-to-ove-operator:${VERSION}

# login (use a GitHub PAT with write:packages, or GITHUB_TOKEN in CI)
echo "$GHCR_TOKEN" | docker login ghcr.io -u <github-user> --password-stdin

docker buildx build --provenance=false --push -t "$IMG" .
```

Then **make the GHCR package public**: GitHub → your `lightwell` org →
Packages → `ahv-to-ove-operator` → Package settings → Change visibility → Public.
(community-operators CI must be able to pull it anonymously.)

## 2. Pin the image by digest (recommended)

community-operators prefers digest-pinned images for reproducible / disconnected installs.
After pushing, resolve the digest and update the bundle CSV in **three** places
(`spec.install...containers[].image`, `metadata.annotations.containerImage`, and
`spec.relatedImages[].image`):

```bash
DIGEST=$(docker buildx imagetools inspect "$IMG" --format '{{.Manifest.Digest}}')
echo "ghcr.io/lightwell-tech/ahv-to-ove-operator@${DIGEST}"
# replace the three ':v0.1.0' references in bundle/manifests/*.clusterserviceversion.yaml
```

## 3. Validate the bundle

```bash
operator-sdk bundle validate ./bundle
# optional: build/push the bundle image
make bundle-build bundle-push
```

## 4. Submit to community-operators (OpenShift OperatorHub)

The OpenShift in-cluster OperatorHub "Community" catalog is fed by
[`redhat-openshift-ecosystem/community-operators-prod`](https://github.com/redhat-openshift-ecosystem/community-operators-prod).
(For the vendor-neutral OperatorHub.io, use
[`k8s-operatorhub/community-operators`](https://github.com/k8s-operatorhub/community-operators)
— same layout.)

1. Fork the repo.
2. Copy the bundle into the required layout:

   ```
   operators/ahv-to-ove-operator/
   ├── ci.yaml                     # reviewer / update-graph config
   └── 0.1.0/
       ├── manifests/              # = bundle/manifests/  (CSV + CRD)
       └── metadata/               # = bundle/metadata/   (annotations.yaml)
   ```

3. Add `operators/ahv-to-ove-operator/ci.yaml`, e.g.:

   ```yaml
   ---
   reviewers:
     - <your-github-handle>
   updateGraph: replaces-mode
   ```

4. Commit **with DCO sign-off** and open the PR:

   ```bash
   git commit -s -m "operator ahv-to-ove-operator (0.1.0)"
   ```

5. CI deploys the bundle on OpenShift 4 and validates it. Fix any findings, then a
   maintainer reviews and merges. The operator then appears in the cluster OperatorHub.

## Prerequisites recap (for users installing the operator)

- OpenShift Virtualization (KubeVirt) + CDI
- A `Secret` with Prism credentials
- **Only for CBT delta sync** (`source.cbt.enabled: true`): the `ahv-delta-sync`
  ServiceAccount with the `hostmount-anyuid` SCC in the target namespace, and the Nutanix
  storage container's NFS whitelist opened to the OpenShift node subnet — see
  [docs/warm-migration-cbt-spec.md](docs/warm-migration-cbt-spec.md).
