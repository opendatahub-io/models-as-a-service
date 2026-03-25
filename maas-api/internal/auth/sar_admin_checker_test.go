package auth_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/auth"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func TestSARAdminChecker_IsAdmin(t *testing.T) {
	const testNamespace = "models-as-a-service"

	t.Run("AdminUserAllowed", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar, ok := action.(k8stesting.CreateAction).GetObject().(*authv1.SubjectAccessReview)
			require.True(t, ok)
			sar.Status.Allowed = true
			return true, sar, nil
		})

		checker := auth.NewSARAdminChecker(client, testNamespace)
		user := &token.UserContext{Username: "admin-user", Groups: []string{"admin-group"}}

		assert.True(t, checker.IsAdmin(context.Background(), user))
	})

	t.Run("RegularUserDenied", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar, ok := action.(k8stesting.CreateAction).GetObject().(*authv1.SubjectAccessReview)
			require.True(t, ok)
			sar.Status.Allowed = false
			return true, sar, nil
		})

		checker := auth.NewSARAdminChecker(client, testNamespace)
		user := &token.UserContext{Username: "regular-user", Groups: []string{"users"}}

		assert.False(t, checker.IsAdmin(context.Background(), user))
	})

	t.Run("NilUserReturnsFalse", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		checker := auth.NewSARAdminChecker(client, testNamespace)

		assert.False(t, checker.IsAdmin(context.Background(), nil))
	})

	t.Run("EmptyUsernameReturnsFalse", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		checker := auth.NewSARAdminChecker(client, testNamespace)
		user := &token.UserContext{Username: "", Groups: []string{"admin-group"}}

		assert.False(t, checker.IsAdmin(context.Background(), user))
	})

	t.Run("NilCheckerReturnsFalse", func(t *testing.T) {
		var checker *auth.SARAdminChecker
		user := &token.UserContext{Username: "admin-user", Groups: []string{"admin-group"}}

		assert.False(t, checker.IsAdmin(context.Background(), user))
	})

	t.Run("NilClientPanics", func(t *testing.T) {
		assert.Panics(t, func() {
			auth.NewSARAdminChecker(nil, testNamespace)
		})
	})

	t.Run("EmptyNamespacePanics", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		assert.Panics(t, func() {
			auth.NewSARAdminChecker(client, "")
		})
	})

	t.Run("APIErrorReturnsFalse_FailClosed", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, assert.AnError
		})

		checker := auth.NewSARAdminChecker(client, testNamespace)
		user := &token.UserContext{Username: "admin-user", Groups: []string{"admin-group"}}

		assert.False(t, checker.IsAdmin(context.Background(), user), "should fail-closed on API error")
	})

	t.Run("VerifiesSARParameters", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		var capturedSAR *authv1.SubjectAccessReview
		client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			var ok bool
			capturedSAR, ok = action.(k8stesting.CreateAction).GetObject().(*authv1.SubjectAccessReview)
			require.True(t, ok)
			capturedSAR.Status.Allowed = true
			return true, capturedSAR, nil
		})

		checker := auth.NewSARAdminChecker(client, testNamespace)
		user := &token.UserContext{Username: "alice", Groups: []string{"group1", "group2"}}

		checker.IsAdmin(context.Background(), user)

		require.NotNil(t, capturedSAR)
		assert.Equal(t, "alice", capturedSAR.Spec.User)
		assert.Equal(t, []string{"group1", "group2"}, capturedSAR.Spec.Groups)
		assert.Equal(t, testNamespace, capturedSAR.Spec.ResourceAttributes.Namespace)
		assert.Equal(t, "create", capturedSAR.Spec.ResourceAttributes.Verb)
		assert.Equal(t, "maas.opendatahub.io", capturedSAR.Spec.ResourceAttributes.Group)
		assert.Equal(t, "maasauthpolicies", capturedSAR.Spec.ResourceAttributes.Resource)
	})

	t.Run("CustomNamespace", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		var capturedSAR *authv1.SubjectAccessReview
		client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			var ok bool
			capturedSAR, ok = action.(k8stesting.CreateAction).GetObject().(*authv1.SubjectAccessReview)
			require.True(t, ok)
			capturedSAR.Status.Allowed = true
			return true, capturedSAR, nil
		})

		checker := auth.NewSARAdminChecker(client, "custom-namespace")
		user := &token.UserContext{Username: "alice", Groups: []string{"users"}}

		checker.IsAdmin(context.Background(), user)

		require.NotNil(t, capturedSAR)
		assert.Equal(t, "custom-namespace", capturedSAR.Spec.ResourceAttributes.Namespace)
		assert.Equal(t, "create", capturedSAR.Spec.ResourceAttributes.Verb)
		assert.Equal(t, "maas.opendatahub.io", capturedSAR.Spec.ResourceAttributes.Group)
		assert.Equal(t, "maasauthpolicies", capturedSAR.Spec.ResourceAttributes.Resource)
	})
}
