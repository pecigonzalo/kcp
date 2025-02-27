/*
Copyright 2022 The KCP Authors.

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

package namespace

import (
	"context"
	"encoding/json"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/kcp/pkg/logging"
	"github.com/kcp-dev/kcp/pkg/syncer/shared"
)

func (c *DownstreamController) process(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	_, namespaceName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Error(err, "invalid key")
		return nil
	}

	downstreamNamespaceObj, err := c.getDownstreamNamespace(namespaceName)
	if apierrors.IsNotFound(err) {
		logger.V(4).Info("downstream namespace not found, ignoring key", "namespace", namespaceName)
		return nil
	} else if err != nil {
		logger.Error(err, "failed to get downstream namespace", "namespace", namespaceName)
		return nil
	}

	downstreamNamespace := downstreamNamespaceObj.(*unstructured.Unstructured)
	logger = logging.WithObject(logger, downstreamNamespace)

	namespaceLocatorJSON := downstreamNamespace.GetAnnotations()[shared.NamespaceLocatorAnnotation]
	if namespaceLocatorJSON == "" {
		logger.Error(nil, "downstream namespace has no namespaceLocator annotation")
		return nil
	}

	nsLocator := shared.NamespaceLocator{}
	if err := json.Unmarshal([]byte(namespaceLocatorJSON), &nsLocator); err != nil {
		logger.Error(err, "failed to unmarshal namespace locator", "namespaceLocator", namespaceLocatorJSON)
		return nil
	}
	logger = logger.WithValues("upstreamWorkspace", nsLocator.Workspace, "upstreamNamespace", nsLocator.Namespace)
	exists, err := c.upstreamNamespaceExists(nsLocator.Workspace, nsLocator.Namespace)
	if err != nil {
		logger.Error(err, "failed to check if upstream namespace exists")
		return nil
	}
	if !exists {
		logger.Info("deleting downstream namespace because the upstream namespace doesn't exist")
		return c.deleteDownstreamNamespace(ctx, namespaceName)
	}
	// The upstream namespace still exists, nothing to do.
	return nil
}
