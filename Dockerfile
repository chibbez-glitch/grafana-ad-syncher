FROM golang:1.21-alpine AS build

RUN apk add --no-cache build-base
WORKDIR /src
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /out/syncd ./cmd/syncd

FROM alpine:3.19

RUN adduser -D -g '' app \
  && mkdir -p /data \
  && chown -R app:app /data

WORKDIR /app
COPY --from=build /out/syncd /app/syncd
COPY web /app/web

ENV LISTEN_ADDR=:8080 \
  DATA_DIR=/data

VOLUME ["/data"]
EXPOSE 8080
USER app
ENTRYPOINT ["/app/syncd"]
