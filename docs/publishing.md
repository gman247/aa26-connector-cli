# Publishing your connector

Three places a connector image can live, and three matching ways the framework discovers it:

| Where the image lives | How to register it |
|---|---|
| Local Docker daemon, side-loaded into k3s | Drop `connector.yaml` in `/var/lib/aa26/connectors/<name>/` |
| OCI registry the cluster can pull from (ghcr, ECR, internal) | Same — drop `connector.yaml`. Set `spec.image.repository` to the registry URL. |
| GitHub repo (Phase 3, marketplace) | Webapp **Marketplace → Import from GitHub**, paste the repo URL |

The sections below cover the first two, which work today.

## Side-loading into k3s

When you're iterating fast, you don't want to push to a registry every build. Build locally, then save+import into k3s's containerd:

```bash
docker build -t localhost/my-connector:dev .
sudo docker save localhost/my-connector:dev | sudo k3s ctr images import -
```

Confirm it landed:

```bash
sudo k3s ctr images ls | grep my-connector
```

In your `connector.yaml`:

```yaml
spec:
  image:
    repository: localhost/my-connector
    pullPolicy: Never                 # important — don't try to pull
```

`Never` tells the kubelet not to look at any registry. If you forget it, you'll get `ImagePullBackOff` because `localhost/...` doesn't resolve to anything.

## Pulling from a registry

For a connector you actually ship, build and push:

```bash
docker build -t ghcr.io/myorg/connectors/my-connector:1.2.0 .
docker push ghcr.io/myorg/connectors/my-connector:1.2.0
```

If the registry needs auth, the cluster needs a pull secret. Talk to whoever owns the AA26 install — they'll attach the secret to the connector pod's service account.

In your `connector.yaml`:

```yaml
spec:
  image:
    repository: ghcr.io/myorg/connectors/my-connector
    digest: sha256:abc123...                    # required for signed installs
    pullPolicy: IfNotPresent                    # default
    signing:
      cosign:
        certificateIdentity: https://github.com/myorg/connectors/.github/workflows/release.yaml@refs/tags/v1.2.0
        certificateOidcIssuer: https://token.actions.githubusercontent.com
```

The image tag is derived from `metadata.version` — push your image under that same tag (e.g. `ghcr.io/myorg/connectors/my-connector:1.2.0` when `metadata.version: 1.2.0`).

For unsigned community connectors the cluster operator has to opt in via `allowUnverifiedConnectors: true`. First-party / vendor connectors are expected to be signed.

## Registering the manifest

Drop the manifest file (and any assets — icon, etc.) into the watch directory:

```bash
sudo mkdir -p /var/lib/aa26/connectors/my-connector
sudo cp connector.yaml /var/lib/aa26/connectors/my-connector/
sudo cp -r assets /var/lib/aa26/connectors/my-connector/   # if you have any
```

The registry polls the directory every 10 seconds. Within ~30 seconds your connector should show up:

```bash
kubectl -n connector-prototype port-forward svc/connector-registry 8090:8090 &
curl -s http://localhost:8090/status | jq '.connectors[] | select(.name=="my-connector")'
```

Look for `"state": "Ready"`. If you see `"InvalidManifest"`, the `reason` field tells you what's wrong.

## Updating

Edit the `connector.yaml` in place. Bump `metadata.version` if your binary's behavior changed. The registry detects the change on its next poll and re-validates.

## Removing

Delete the directory:

```bash
sudo rm -rf /var/lib/aa26/connectors/my-connector
```

The registry notices the manifest is gone and removes the row. Existing scan history is preserved — only the connector's discoverability is affected.

## What's coming (Phase 3)

- **GitHub-import in the webapp**: paste a repo URL, the framework clones it, builds the image with kaniko, signs it, and registers — all without you touching the host.
- **Curated marketplace**: a published catalog of vetted connectors maintainers can browse and install in one click.
- **Per-tenant scope**: in multi-tenant installs, each tenant manages its own connector list.

None of this changes the manifest format or the runtime contract — the work above will keep working.
