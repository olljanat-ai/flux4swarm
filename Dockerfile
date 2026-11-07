# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/flux4swarm ./cmd/flux4swarm

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/flux4swarm /usr/local/bin/flux4swarm
# The Docker socket is owned by root; run as root or add the docker group.
USER 0:0
ENTRYPOINT ["/usr/local/bin/flux4swarm"]
CMD ["controller", "--config", "/etc/flux4swarm/gotk-sync.yaml"]
