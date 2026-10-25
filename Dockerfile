# The registry-mirrors-relay image.
#
# The binary is compiled outside the image and placed at <platform>/registry-mirrors-relay in the build context
# (GoReleaser does this automatically when releasing; for your own builds use scripts/build-image.sh). This file
# only copies files and compiles nothing, and the final stage has no RUN at all, so building the arm64 and
# arm/v7 images does not need QEMU either. The base images are pinned by digest and updated by Dependabot.

# This stage prepares the configuration and data directories, fixes their permissions and ownership, and then
# everything is copied over in one go (COPY --from keeps ownership and permissions).
# The data directory belongs to nonroot (65532): when a new named volume is mounted, Docker initialises the
# volume with the ownership of this directory in the image.
# This stage runs on the build machine's own platform, and its output is just a few files and directories.
FROM --platform=$BUILDPLATFORM docker.io/library/busybox:1.38@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e AS files
COPY deploy/docker/config.yaml /out/etc/registry-mirrors-relay/config.yaml
RUN mkdir -p /out/var/lib/registry-mirrors-relay \
 && chmod 0755 /out/etc/registry-mirrors-relay \
 && chmod 0644 /out/etc/registry-mirrors-relay/config.yaml \
 && chown 65532:65532 /out/var/lib/registry-mirrors-relay \
 && chmod 0700 /out/var/lib/registry-mirrors-relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG TARGETPLATFORM
COPY --from=files /out/ /
COPY --chmod=0755 $TARGETPLATFORM/registry-mirrors-relay /usr/local/bin/registry-mirrors-relay
USER 65532:65532
# 8080: pull entrance and admin console
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/registry-mirrors-relay", "probe"]
ENTRYPOINT ["/usr/local/bin/registry-mirrors-relay"]
CMD ["serve"]
