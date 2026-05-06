FROM alpine:3.20
RUN apk add --no-cache bash curl jq findutils coreutils && adduser -D -H -u 65532 connector
WORKDIR /app
COPY run.sh /app/run.sh
RUN chmod +x /app/run.sh
USER 65532
ENTRYPOINT ["/app/run.sh"]
