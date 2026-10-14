# One image, three binaries: the proxy, the simulated provider and the load
# generator. The build stage cross-compiles, so multi-arch images need no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/proxy ./cmd/fakellm ./cmd/loadgen

# distroless/static ships CA certificates (needed for provider HTTPS) and runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
EXPOSE 8080
ENTRYPOINT ["/proxy"]
