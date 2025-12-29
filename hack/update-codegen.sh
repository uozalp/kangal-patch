#!/bin/bash
set -e

echo "Generating DeepCopy methods..."

controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

echo "Code generation complete!"
