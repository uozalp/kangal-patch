#!/bin/bash
set -e

echo "Generating Kubernetes manifests..."

# Generate CRDs
controller-gen crd:maxDescLen=0 \
    rbac:roleName=manager-role \
    webhook \
    paths="./..." \
    output:crd:artifacts:config=config/crd/bases

# Generate RBAC
controller-gen rbac:roleName=manager-role \
    paths="./..." \
    output:rbac:artifacts:config=config/rbac

echo "Manifests generated successfully!"
