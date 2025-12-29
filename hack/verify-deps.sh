#!/bin/bash
set -e

# Verify dependencies
echo "Verifying Go dependencies..."
go mod verify

# Download dependencies
echo "Downloading Go dependencies..."
go mod download

# Tidy up
echo "Tidying go.mod and go.sum..."
go mod tidy

echo "Dependencies verified and updated!"
