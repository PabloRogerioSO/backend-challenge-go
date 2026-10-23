FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/app

FROM alpine:3.19

# Install CA certificates for HTTPS calls (e.g., Keycloak)
RUN apk add --no-cache ca-certificates

# Copy the compiled binary to the system's local bin folder
COPY --from=build /out/app /usr/local/bin/app

EXPOSE 8080

# Run as a non-root user for security
USER nobody:nobody

ENTRYPOINT ["/usr/local/bin/app"]