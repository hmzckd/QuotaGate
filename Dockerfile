FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY client ./client
COPY examples ./examples
RUN CGO_ENABLED=0 go build -trimpath -o /out/quotagate ./cmd/quotagate
RUN CGO_ENABLED=0 go build -trimpath -o /out/example-api ./examples/api

FROM alpine:3.22 AS runtime
RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
USER app

FROM runtime AS example-api
COPY --from=build /out/example-api /app/example-api
EXPOSE 8081
ENTRYPOINT ["/app/example-api"]

FROM runtime AS quotagate
COPY --from=build /out/quotagate /app/quotagate
COPY migrations /app/migrations
EXPOSE 8080
ENTRYPOINT ["/app/quotagate"]
