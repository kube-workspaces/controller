# --platform=$BUILDPLATFORM keeps the toolchain running natively; the target
# architecture is passed to the compiler instead. Without this, a multi-arch
# build emulates the whole Go compile under QEMU, which is an order of magnitude
# slower and occasionally flaky.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o manager ./cmd/main.go

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/kube-workspaces/controller"
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
