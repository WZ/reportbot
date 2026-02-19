FROM golang:1.23-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=1 go build -o reportbot ./cmd/reportbot

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/reportbot .
RUN mkdir -p /app/reports

ENTRYPOINT ["./reportbot"]
