# Distroless runtime image for the static binary (non-root).
FROM gcr.io/distroless/static-debian12:nonroot
COPY cms /usr/local/bin/cms
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cms"]
CMD ["serve", "--with-worker", "--addr", "0.0.0.0:8080"]
