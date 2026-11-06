FROM gcr.io/distroless/static-debian12:nonroot

COPY farvardin /usr/bin/farvardin
COPY configs/config.example.toml /etc/farvardin/config.example.toml

USER nonroot:nonroot
VOLUME ["/var/lib/farvardin"]
EXPOSE 8000

ENV FARVARDIN_SERVER__LISTEN=":8000"

ENTRYPOINT ["/usr/bin/farvardin"]
CMD ["serve"]
