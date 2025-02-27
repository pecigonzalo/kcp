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

package authorization

import (
	"context"
	"fmt"

	"github.com/kcp-dev/logicalcluster/v2"

	kaudit "k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	kubernetesinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/genericcontrolplane"
	"k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	kcpinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
	"github.com/kcp-dev/kcp/pkg/indexers"
	rbacwrapper "github.com/kcp-dev/kcp/pkg/virtual/framework/wrappers/rbac"
)

const (
	APIBindingContentAuditPrefix   = "apibinding.authorization.kcp.dev/"
	APIBindingContentAuditDecision = APIBindingContentAuditPrefix + "decision"
	APIBindingContentAuditReason   = APIBindingContentAuditPrefix + "reason"
)

// NewAPIBindingAccessAuthorizer returns an authorizer that checks if the request is for a
// bound resource or not. If the resource is bound we will check the user has RBAC access in the
// exported resource workspace. If it is not allowed we will return NoDecision, if allowed we
// will call the delegate authorizer.
func NewAPIBindingAccessAuthorizer(kubeInformers kubernetesinformers.SharedInformerFactory, kcpInformers kcpinformers.SharedInformerFactory, delegate authorizer.Authorizer) (authorizer.Authorizer, error) {
	apiBindingIndexer := kcpInformers.Apis().V1alpha1().APIBindings().Informer().GetIndexer()
	apiExportIndexer := kcpInformers.Apis().V1alpha1().APIExports().Informer().GetIndexer()

	// Make sure informer knows what to watch
	kubeInformers.Rbac().V1().Roles().Lister()
	kubeInformers.Rbac().V1().RoleBindings().Lister()
	kubeInformers.Rbac().V1().ClusterRoles().Lister()
	kubeInformers.Rbac().V1().ClusterRoleBindings().Lister()

	return &apiBindingAccessAuthorizer{
		getAPIBindingReferenceForAttributes: func(attr authorizer.Attributes, clusterName logicalcluster.Name) (*apisv1alpha1.ExportReference, bool, error) {
			return getAPIBindingReferenceForAttributes(apiBindingIndexer, attr, clusterName)
		},
		getAPIExportByReference: func(exportRef *apisv1alpha1.ExportReference) (*apisv1alpha1.APIExport, bool, error) {
			return getAPIExportByReference(apiExportIndexer, exportRef)
		},
		newAuthorizer: func(clusterName logicalcluster.Name) authorizer.Authorizer {
			clusterKubeInformer := rbacwrapper.FilterInformers(clusterName, kubeInformers.Rbac().V1())
			bootstrapInformer := rbacwrapper.FilterInformers(genericcontrolplane.LocalAdminCluster, kubeInformers.Rbac().V1())

			mergedClusterRoleLister := rbacwrapper.NewMergedClusterRoleLister(clusterKubeInformer.ClusterRoles().Lister(), bootstrapInformer.ClusterRoles().Lister())
			mergedRoleLister := rbacwrapper.NewMergedRoleLister(clusterKubeInformer.Roles().Lister(), bootstrapInformer.Roles().Lister())
			mergedClusterRoleBindingsLister := rbacwrapper.NewMergedClusterRoleBindingLister(clusterKubeInformer.ClusterRoleBindings().Lister(), bootstrapInformer.ClusterRoleBindings().Lister())

			return rbac.New(
				&rbac.RoleGetter{Lister: mergedRoleLister},
				&rbac.RoleBindingLister{Lister: clusterKubeInformer.RoleBindings().Lister()},
				&rbac.ClusterRoleGetter{Lister: mergedClusterRoleLister},
				&rbac.ClusterRoleBindingLister{Lister: mergedClusterRoleBindingsLister},
			)
		},
		delegate: delegate,
	}, nil
}

type apiBindingAccessAuthorizer struct {
	delegate authorizer.Authorizer

	getAPIBindingReferenceForAttributes func(attr authorizer.Attributes, clusterName logicalcluster.Name) (ref *apisv1alpha1.ExportReference, found bool, err error)
	getAPIExportByReference             func(exportRef *apisv1alpha1.ExportReference) (ref *apisv1alpha1.APIExport, found bool, err error)
	newAuthorizer                       func(clusterName logicalcluster.Name) authorizer.Authorizer
}

func (a *apiBindingAccessAuthorizer) Authorize(ctx context.Context, attr authorizer.Attributes) (authorizer.Decision, string, error) {
	apiBindingAccessDenied := "bound api access is not permitted"

	// get the cluster from the ctx.
	lcluster, err := genericapirequest.ClusterNameFrom(ctx)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionNoOpinion,
			APIBindingContentAuditReason, fmt.Sprintf("error getting cluster from request: %v", err),
		)
		return authorizer.DecisionNoOpinion, apiBindingAccessDenied, err
	}

	bindingLogicalCluster, bound, err := a.getAPIBindingReferenceForAttributes(attr, lcluster)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionNoOpinion,
			APIBindingContentAuditReason, fmt.Sprintf("error getting API binding reference: %v", err),
		)
		return authorizer.DecisionNoOpinion, apiBindingAccessDenied, err
	}

	if !bound {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionAllowed,
			APIBindingContentAuditReason, "no API binding bound",
		)
		return a.delegate.Authorize(ctx, attr)
	}

	apiExport, found, err := a.getAPIExportByReference(bindingLogicalCluster)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionNoOpinion,
			APIBindingContentAuditReason, fmt.Sprintf("error getting API export: %v", err),
		)
		return authorizer.DecisionNoOpinion, apiBindingAccessDenied, err
	}

	path := "unknown"
	exportName := "unknown"
	if bindingLogicalCluster.Workspace != nil {
		exportName = bindingLogicalCluster.Workspace.ExportName
		path = bindingLogicalCluster.Workspace.Path
	}

	// If we can't find the export default to close
	if !found {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionNoOpinion,
			APIBindingContentAuditReason, fmt.Sprintf("API export %q not found, path: %q", exportName, path),
		)
		return authorizer.DecisionNoOpinion, apiBindingAccessDenied, err
	}

	if apiExport.Spec.MaximalPermissionPolicy == nil {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionAllowed,
			APIBindingContentAuditReason, fmt.Sprintf("no maximal permission policy present in API export %q, path: %q, owning cluster: %q", exportName, path, logicalcluster.From(apiExport)),
		)
		return a.delegate.Authorize(ctx, attr)
	}

	if apiExport.Spec.MaximalPermissionPolicy.Local == nil {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionAllowed,
			APIBindingContentAuditReason, fmt.Sprintf("no maximal local permission policy present in API export %q, path: %q, owning cluster: %q", apiExport.Name, path, logicalcluster.From(apiExport)),
		)
		return a.delegate.Authorize(ctx, attr)
	}

	// If bound, create a rbac authorizer filtered to the cluster.
	clusterAuthorizer := a.newAuthorizer(logicalcluster.From(apiExport))
	prefixedAttr := deepCopyAttributes(attr)
	userInfo := prefixedAttr.User.(*user.DefaultInfo)
	userInfo.Name = apisv1alpha1.MaximalPermissionPolicyRBACUserGroupPrefix + userInfo.Name
	userInfo.Groups = make([]string, 0, len(attr.GetUser().GetGroups()))
	for _, g := range attr.GetUser().GetGroups() {
		userInfo.Groups = append(userInfo.Groups, apisv1alpha1.MaximalPermissionPolicyRBACUserGroupPrefix+g)
	}
	dec, reason, err := clusterAuthorizer.Authorize(ctx, prefixedAttr)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			APIBindingContentAuditDecision, DecisionNoOpinion,
			APIBindingContentAuditReason, fmt.Sprintf("error authorizing RBAC in API export cluster %q: %v", logicalcluster.From(apiExport), err),
		)
		return authorizer.DecisionNoOpinion, reason, err
	}

	kaudit.AddAuditAnnotations(
		ctx,
		APIBindingContentAuditDecision, DecisionString(dec),
		APIBindingContentAuditReason, fmt.Sprintf("API export cluster %q reason: %v", logicalcluster.From(apiExport), reason),
	)

	if dec == authorizer.DecisionAllow {
		return a.delegate.Authorize(ctx, attr)
	}

	return authorizer.DecisionNoOpinion, reason, nil
}

func getAPIBindingReferenceForAttributes(apiBindingIndexer cache.Indexer, attr authorizer.Attributes, clusterName logicalcluster.Name) (*apisv1alpha1.ExportReference, bool, error) {
	objs, err := apiBindingIndexer.ByIndex(indexers.ByLogicalCluster, clusterName.String())
	if err != nil {
		return nil, false, err
	}
	for _, obj := range objs {
		apiBinding := obj.(*apisv1alpha1.APIBinding)
		for _, br := range apiBinding.Status.BoundResources {
			if apiBinding.Status.BoundAPIExport == nil || apiBinding.Status.BoundAPIExport.Workspace == nil {
				continue
			}
			if br.Group == attr.GetAPIGroup() && br.Resource == attr.GetResource() {
				return apiBinding.Status.BoundAPIExport, true, nil
			}
		}
	}
	return nil, false, nil
}

func getAPIExportByReference(apiExportIndexer cache.Indexer, exportRef *apisv1alpha1.ExportReference) (*apisv1alpha1.APIExport, bool, error) {
	objs, err := apiExportIndexer.ByIndex(indexers.ByLogicalCluster, exportRef.Workspace.Path)
	if err != nil {
		return nil, false, err
	}
	for _, obj := range objs {
		apiExport := obj.(*apisv1alpha1.APIExport)
		if apiExport.Name == exportRef.Workspace.ExportName {
			return apiExport, true, err
		}
	}
	return nil, false, err

}
