# Image for GoReleaser releases: packages the prebuilt binary for each
# platform (GoReleaser puts it in the build context under $TARGETPLATFORM).
# The Dockerfile next to it builds from source for local use.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/flux4swarm /usr/local/bin/flux4swarm
# The Docker socket is owned by root; run as root or add the docker group.
USER 0:0
ENTRYPOINT ["/usr/local/bin/flux4swarm"]
CMD ["controller", "--config", "/etc/flux4swarm/gotk-sync.yaml"]
