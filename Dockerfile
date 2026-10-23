# registry-mirrors-relay 的镜像。
#
# 二进制在镜像外编译好，放在构建上下文的 <平台>/registry-mirrors-relay（GoReleaser 发布时自动这样放；
# 自己构建用 scripts/build-image.sh）。这里只复制文件、不编译，最后一层没有任何 RUN，
# 所以构建 arm64、arm/v7 的镜像也不需要 QEMU。基础镜像按 digest 固定，由 Dependabot 更新。

# 配置和数据目录在这一阶段准备好，权限和属主一并定下来，再整个复制过去（COPY --from 保留属主和权限）。
# 数据目录属于 nonroot（65532）：挂一个新的命名卷时，Docker 按镜像里这个目录的属主初始化卷。
# 这一阶段在构建机本身的平台上运行，产物只是几个文件和目录。
FROM --platform=$BUILDPLATFORM docker.io/library/busybox:1.38@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e AS files
COPY deploy/docker/config.yaml /out/etc/registry-mirrors-relay/config.yaml
RUN mkdir -p /out/var/lib/registry-mirrors-relay \
 && chmod 0755 /out/etc/registry-mirrors-relay \
 && chmod 0644 /out/etc/registry-mirrors-relay/config.yaml \
 && chown 65532:65532 /out/var/lib/registry-mirrors-relay \
 && chmod 0700 /out/var/lib/registry-mirrors-relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
ARG TARGETPLATFORM
COPY --from=files /out/ /
COPY --chmod=0755 $TARGETPLATFORM/registry-mirrors-relay /usr/local/bin/registry-mirrors-relay
USER 65532:65532
# 8080 拉取入口；9090 管理后台（宿主机上只映射到 127.0.0.1）；9180 指标
EXPOSE 8080 9090 9180
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/registry-mirrors-relay", "probe"]
ENTRYPOINT ["/usr/local/bin/registry-mirrors-relay"]
CMD ["serve"]
