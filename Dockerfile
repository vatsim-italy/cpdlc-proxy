# Built by .github/workflows/image.yml; locally: docker build -t cpdlc-proxy:latest .
# Cross-compiles on the build machine's platform: no emulation for the arm64 image.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/cpdlc-proxy . \
 && mkdir -p /out/data

# distroless static: CA certificates (for https://www.hoppie.nl), no shell, uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cpdlc-proxy /cpdlc-proxy
# A new named volume copies this directory's owner, so nonroot can write state.
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/cpdlc-proxy"]
CMD ["-config", "/config/config.json", "-listen", ":8080", "-state", "/data/state.json"]
