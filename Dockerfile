FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o controller .

FROM gcr.io/distroless/static:nonroot
WORKDIR /app
EXPOSE 8080
COPY --from=builder /app/controller /app/controller
USER nonroot:nonroot
ENTRYPOINT ["/app/controller"]