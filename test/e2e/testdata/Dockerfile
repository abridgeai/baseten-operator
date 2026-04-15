FROM golang:1.24 AS builder
WORKDIR /app
COPY mock_server.go .
RUN CGO_ENABLED=0 GOOS=linux go build -o /mock-server mock_server.go

FROM alpine:3.21
RUN apk add --no-cache wget
COPY --from=builder /mock-server /mock-server
USER 65532:65532
ENTRYPOINT ["/mock-server"]
