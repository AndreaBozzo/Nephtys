# ---------- Build ----------
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /nephtys ./cmd/nephtys

# ---------- Runtime ----------
FROM gcr.io/distroless/static-debian12

COPY --from=builder /nephtys /nephtys

EXPOSE 3002
ENTRYPOINT ["/nephtys"]
