# Distroless runtime image for the static binary (non-root). GoReleaser's
# dockers_v2 puts each platform's binary under <os>/<arch>/ in the context.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/cms /usr/local/bin/cms
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cms"]
CMD ["serve", "--with-worker", "--addr", "0.0.0.0:8080"]
