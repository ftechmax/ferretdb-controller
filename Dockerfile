FROM golang:1.25-alpine AS builder
WORKDIR /app
ENV CGO_ENABLED=0
COPY . .
RUN go build -o controller -ldflags "-s -w" .

FROM gcr.io/distroless/static:nonroot
WORKDIR /app
EXPOSE 8080
COPY --from=builder /app/controller /app/controller
USER nonroot:nonroot
ENTRYPOINT ["/app/controller"]
