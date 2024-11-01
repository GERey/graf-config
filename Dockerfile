# Use the official Golang image to build the app
FROM golang:1.23 AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application code
COPY . .

# Build the Go app
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o georgetestapp main.go

# Use a minimal base image to run the application
FROM alpine:3.17

# Install CA certificates to handle HTTPS requests
RUN apk add --no-cache ca-certificates

# Set the working directory
WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /app/georgetestapp .

# Create a startup script to print "Hello, World!" and then run the app
RUN echo -e '#!/bin/sh\n\necho "Hello, World!"\n\n./georgetestapp' > /app/startup.sh && \
    chmod +x /app/startup.sh

# Expose port 8080
EXPOSE 8080

# Run the startup script
CMD ["/app/startup.sh"]