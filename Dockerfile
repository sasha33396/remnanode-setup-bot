# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/deployer ./cmd/deployer

FROM alpine:3.22
RUN addgroup -S deployer && adduser -S -G deployer deployer

COPY --from=build /out/deployer /usr/local/bin/deployer

USER deployer
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/deployer"]
