# The store directory, owned by distroless's nonroot user (65532): a new
# named volume takes this ownership, so farvardin can write its store.
FROM busybox:1.37 AS dirs
RUN mkdir -p /out/var/lib/farvardin

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=dirs --chown=65532:65532 /out/var/lib/farvardin /var/lib/farvardin
COPY farvardin /usr/bin/farvardin
COPY configs/config.example.toml /etc/farvardin/config.example.toml

USER nonroot:nonroot
VOLUME ["/var/lib/farvardin"]
EXPOSE 8000

ENV FARVARDIN_SERVER__LISTEN=":8000"

ENTRYPOINT ["/usr/bin/farvardin"]
CMD ["serve"]
