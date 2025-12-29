/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
