# Build: static Go binary (stdlib only, no network needed besides the base images)
FROM registry.access.redhat.com/ubi9/go-toolset:latest AS build
USER 0
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /ibench . \
 && mkdir -p /out/data && chmod 0775 /out/data

# Runtime: ~25 MB, runs as any non-root UID (OpenShift restricted-v2)
FROM registry.access.redhat.com/ubi9/ubi-micro:latest
COPY --from=build /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/pki/tls/certs/ca-bundle.crt
COPY --from=build /ibench /usr/local/bin/ibench
COPY --from=build --chown=1001:0 --chmod=775 /out/data /data
ENV DATA_DIR=/data
EXPOSE 8080
USER 1001
ENTRYPOINT ["/usr/local/bin/ibench"]
CMD ["serve"]
