FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /mailrelay ./cmd/mailrelay

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /mailrelay /usr/local/bin/mailrelay

RUN addgroup -S mailrelay && adduser -S mailrelay -G mailrelay \
    && mkdir -p /data/emails \
    && chown -R mailrelay:mailrelay /data

VOLUME /data

ENV MAILRELAY_WEBUI__ENABLED=true
ENV MAILRELAY_WEBUI__DB_PATH=/data/mailrelay.db
ENV MAILRELAY_WEBUI__RAW_EMAIL_DIR=/data/emails
ENV MAILRELAY_HTTP__ADDR=0.0.0.0:2623

USER mailrelay

EXPOSE 25 2623

ENTRYPOINT ["mailrelay"]
CMD ["-config", "/data/config.yaml"]
