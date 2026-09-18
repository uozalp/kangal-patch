package patchutil

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PatchStatus applies a status patch to a Kubernetes object, only updating changed fields.
func PatchStatus[T client.Object](ctx context.Context, statusWriter client.StatusWriter, original, modified T) error {
	logger := log.FromContext(ctx)

	patch := client.MergeFrom(original)
	if err := statusWriter.Patch(ctx, modified, patch); err != nil {
		logger.Error(err, "unable to patch status")
		return err
	}

	return nil
}
