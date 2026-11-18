# Changelog

Each release has a section here; the release workflow uses it as the GitHub release notes.

## v1.1.0

### Redis slowlog

- Optional: with `KORAI_REDIS_SLOWLOG` set to one or more `redis://` / `rediss://` URLs, the probe reads the `SLOWLOG`
  of those Redis and Valkey servers every 30 seconds (self-hosted, Memorystore, ElastiCache) and sends the slow
  commands to Korai (Observability › Infrastructure › Redis › Slow commands).
- Argument values, keys, client addresses and client names never leave the probe: only the command name, its duration
  and the number of arguments. `KORAI_REDIS_SLOWLOG_KEYS=1` also sends the key as a pattern (`GET cart:91822` is sent
  as `GET cart:?`); it is off by default because keys can hold personal data or tokens.
- TLS with a custom CA (`KORAI_REDIS_CA_FILE`, the Memorystore server CA), ACL users and AUTH strings.
- Each server's status shows on the probe in Korai. Entries are kept in memory and resent while Korai cannot take
  them, and a server restart (ids starting again) is handled. Best effort: a probe restart reads only the last 30 s,
  and more than 128 slow commands between two reads keeps only the newest 128.
- Helm: `redisSlowlog.urls` or `redisSlowlog.existingSecret`, `redisSlowlog.caSecret`, `redisSlowlog.keys`.

## v1.0.0

First release of the Korai private probe: run Korai uptime checks inside your own network.

### What it does

- Runs the HTTP(S), TCP and ADMS terminal checks you assign to it in Korai, once a minute, with up to two attempts
  within 25 seconds.
- Sends the results to Korai over outbound HTTPS. Nothing listens and nothing is opened inbound.
- One setting: the install token Korai shows when you add a private probe. On its first start the probe trades it for
  its own credential. There is no configuration file.
- Keeps the results on disk while Korai cannot be reached (up to seven days) and sends them later, in order.
- Keeps checking with the last checks it downloaded while Korai is away, also after a restart.

### Security

- Refuses link-local and cloud metadata addresses after resolving the name.
- Sends a check's header token only to the check's own origin, never across a redirect to another one.
- Sends only results to Korai, never a response body.
- Distroless image, non-root user, works with a read-only root filesystem.

### Install

- Docker: `ghcr.io/koraitech/korai-probe:1.0.0` (also `:1.0` and `:1`), linux/amd64 and linux/arm64.
- Kubernetes: `helm upgrade --install korai-probe oci://ghcr.io/koraitech/charts/korai-probe --version 1.0.0 --set token=...`
- Binaries: `korai-probe-linux-amd64` and `korai-probe-linux-arm64` below, with `SHA256SUMS`.

Guide: https://docs.korai.tech/observability/uptime-probes/
