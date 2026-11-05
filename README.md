# Korai private probe

A small agent that runs [Korai](https://korai.tech) uptime checks inside a private network (on-premises, a VPC, a
Kubernetes cluster) and sends the results to Korai over outbound HTTPS. Nothing listens and nothing needs to be opened
inbound. It is a single static Go binary with no dependencies beyond the standard library.

User guide: [Uptime checks › Private probes](https://docs.korai.tech/observability/uptime-probes/).

## Install

In Korai, open **Observability › Reliability › Uptime checks › Private probes › Add private probe**. Korai shows the
install command with a one-time install token already in it. There is nothing else to configure.

```sh
# Docker, on any Linux machine
docker run -d --name korai-probe --restart unless-stopped \
  -v korai-probe:/var/lib/korai-probe \
  -e KORAI_PROBE_TOKEN='<install token>' \
  ghcr.io/koraitech/korai-probe:1

# Kubernetes
helm upgrade --install korai-probe oci://ghcr.io/koraitech/charts/korai-probe \
  --namespace korai-probe --create-namespace --set token='<install token>'
```

Binaries for Linux (amd64, arm64) are attached to each [release](https://github.com/koraitech/korai-probe/releases),
with their SHA-256 checksums.

## How it works

- **One setting.** `KORAI_PROBE_TOKEN` is the install token. It carries Korai's address, the tenant and the probe id
  (`kpe1.<url>.<tenant>.<id>.<secret>`), valid for 24 hours and usable once.
- **Enrollment.** On its first start the probe trades the token for its own credential and keeps it in
  `KORAI_PROBE_STATE_DIR` (default `/var/lib/korai-probe`, file `credential.json`, mode 0600). Restarts reuse it. A
  different token (a reinstall) enrolls again and replaces the old credential in Korai. Keep this folder across
  upgrades.
- **Checks.** Downloaded from Korai every minute (with an ETag) and cached in `checks.json`, so the probe keeps checking
  while Korai cannot be reached. Each check runs once a minute, a second after the minute starts: HTTP(S), TCP or an
  ADMS terminal handshake, with up to two attempts within 25 seconds.
- **Results.** Appended to `results.jsonl` and sent in order. While Korai cannot be reached they wait on disk, at most
  seven days. A minute the probe did not run (stopped, restarting) is not reported, and Korai shows it as no data,
  never as down.

## Security

- Outbound only: the probe calls Korai and the endpoints it checks. It opens no port.
- Link-local and cloud metadata addresses (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`, ...) are refused after the
  name is resolved, whatever the check says. Private addresses are allowed: reaching them is the point.
- A check's header token is sent only to the check's own origin (scheme, host and port): never across a redirect to
  another origin, and never from `https` down to `http`.
- Only the result of each check is sent to Korai (up or down, status code, time, the kind of error), never a response
  body.
- `HTTPS_PROXY` / `NO_PROXY` apply to the connection to Korai only, never to the checks.
- The image is distroless, runs as a non-root user and works with a read-only root filesystem.

Report a vulnerability privately through **Security › Report a vulnerability** in this repository, not in an issue.

## Configuration

| Variable | Default | |
|---|---|---|
| `KORAI_PROBE_TOKEN` | | The install token from Korai. Needed on the first start only. |
| `KORAI_PROBE_STATE_DIR` | `/var/lib/korai-probe` | Credential, cached checks and results waiting to be sent. |
| `HTTPS_PROXY`, `NO_PROXY` | | A proxy for reaching Korai. |

## Develop

```sh
go vet ./... && go test -race ./...
KORAI_PROBE_STATE_DIR=/tmp/kp KORAI_PROBE_TOKEN='<token>' go run .
```

## Release

Bump `version` in `main.go` and `appVersion` in `charts/korai-probe/Chart.yaml`, add a `## vX.Y.Z` section to
`CHANGELOG.md` (it becomes the release notes), then push a tag `vX.Y.Z`. The release
workflow publishes the multi-arch image `ghcr.io/koraitech/korai-probe:{X.Y.Z,X.Y,X}`, the chart
`oci://ghcr.io/koraitech/charts/korai-probe` and a GitHub release with the binaries.

## License

Apache License 2.0, see [LICENSE](LICENSE).
