# Korai private probe: a static binary on a distroless base (CA certificates, no shell), running as non-root.
# Built for linux/amd64 and linux/arm64 by .github/workflows/release.yml.
FROM --platform=$BUILDPLATFORM golang:1.22 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/korai-probe . \
 && mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/korai-probe /korai-probe
# The credential and the results waiting to be sent. A named volume here keeps them across upgrades.
COPY --from=build --chown=65532:65532 /out/state /var/lib/korai-probe
VOLUME /var/lib/korai-probe
USER 65532:65532
ENTRYPOINT ["/korai-probe"]
