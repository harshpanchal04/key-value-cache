# Multi-stage build
# --- Builder Stage ---
    FROM golang:1.24-alpine AS builder

    # Set necessary environment variables
    ENV GO111MODULE=on
    ENV CGO_ENABLED=0
    ENV GOOS=linux
    ENV GOARCH=amd64
    
    # Working directory
    WORKDIR /app
    
    # Copy go mod and sum files
    COPY go.mod go.sum ./
    
    # Download dependencies
    RUN go mod download
    RUN go mod tidy # Add this line to ensure go.sum is up-to-date
    
    # Copy the source code
    COPY . .
    
    # Build the application
    RUN go build -o cache_app ./key_value_cache.go # Specify the file explicitly
    
    # --- Final Stage ---
    FROM alpine:latest
    
    # Copy built binary
    COPY --from=builder /app/cache_app /cache_app
    
    # Expose port
    EXPOSE 7171
    
    # Run the application
    CMD ["/cache_app"]
    