package auth_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/auth"
)

var errAuthCRNotFound = errors.New("auth CR not found")

// mockAuthLister implements cache.GenericLister for testing.
type mockAuthLister struct {
	authCR *unstructured.Unstructured
	err    error
}

func (m *mockAuthLister) List(selector labels.Selector) ([]runtime.Object, error) {
	return nil, nil
}

//nolint:ireturn // Mock implementation must match k8s interface signature
func (m *mockAuthLister) Get(name string) (runtime.Object, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.authCR, nil
}

//nolint:ireturn // Mock implementation must match k8s interface signature
func (m *mockAuthLister) ByNamespace(namespace string) cache.GenericNamespaceLister {
	return nil
}

// createAuthCR creates a mock Auth CR with the given admin groups.
func createAuthCR(adminGroups []string) *unstructured.Unstructured {
	// Convert []string to []interface{} for unstructured.NestedStringSlice() to work correctly
	adminGroupsInterface := make([]any, len(adminGroups))
	for i, g := range adminGroups {
		adminGroupsInterface[i] = g
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "services.opendatahub.io/v1alpha1",
			"kind":       "Auth",
			"metadata": map[string]any{
				"name": "auth",
			},
			"spec": map[string]any{
				"adminGroups": adminGroupsInterface,
			},
		},
	}
}

func TestIsAdmin(t *testing.T) {
	t.Run("UserInAdminGroup", func(t *testing.T) {
		authCR := createAuthCR([]string{"admin-group", "super-admins"})
		lister := &mockAuthLister{authCR: authCR}
		checker := auth.NewAdminChecker(lister)

		userGroups := []string{"users", "admin-group"}
		assert.True(t, checker.IsAdmin(userGroups), "user with admin-group should be admin")
	})

	t.Run("UserNotInAdminGroup", func(t *testing.T) {
		authCR := createAuthCR([]string{"admin-group", "super-admins"})
		lister := &mockAuthLister{authCR: authCR}
		checker := auth.NewAdminChecker(lister)

		userGroups := []string{"users", "developers"}
		assert.False(t, checker.IsAdmin(userGroups), "user without admin groups should not be admin")
	})

	t.Run("AuthCRNotFound", func(t *testing.T) {
		lister := &mockAuthLister{err: errAuthCRNotFound}
		checker := auth.NewAdminChecker(lister)

		userGroups := []string{"users", "admin-group"}
		assert.False(t, checker.IsAdmin(userGroups), "should fail-closed when Auth CR not found")
	})

	t.Run("MultipleAdminGroups", func(t *testing.T) {
		authCR := createAuthCR([]string{"admin-group-1", "admin-group-2", "admin-group-3"})
		lister := &mockAuthLister{authCR: authCR}
		checker := auth.NewAdminChecker(lister)

		t.Run("MatchesFirst", func(t *testing.T) {
			userGroups := []string{"admin-group-1", "users"}
			assert.True(t, checker.IsAdmin(userGroups))
		})

		t.Run("MatchesMiddle", func(t *testing.T) {
			userGroups := []string{"users", "admin-group-2"}
			assert.True(t, checker.IsAdmin(userGroups))
		})

		t.Run("MatchesLast", func(t *testing.T) {
			userGroups := []string{"users", "admin-group-3"}
			assert.True(t, checker.IsAdmin(userGroups))
		})
	})
}

func TestGetAdminGroups(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		expectedGroups := []string{"admin-group", "super-admins"}
		authCR := createAuthCR(expectedGroups)
		lister := &mockAuthLister{authCR: authCR}
		checker := auth.NewAdminChecker(lister)

		groups, err := checker.GetAdminGroups()
		require.NoError(t, err)
		assert.Equal(t, expectedGroups, groups)
	})

	t.Run("AuthCRNotFound", func(t *testing.T) {
		lister := &mockAuthLister{err: errAuthCRNotFound}
		checker := auth.NewAdminChecker(lister)

		groups, err := checker.GetAdminGroups()
		require.Error(t, err)
		assert.Nil(t, groups)
		assert.Contains(t, err.Error(), "failed to get Auth CR")
	})

	t.Run("MissingAdminGroupsField", func(t *testing.T) {
		authCR := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "services.opendatahub.io/v1alpha1",
				"kind":       "Auth",
				"metadata": map[string]any{
					"name": "auth",
				},
				"spec": map[string]any{
					// No adminGroups field
				},
			},
		}
		lister := &mockAuthLister{authCR: authCR}
		checker := auth.NewAdminChecker(lister)

		groups, err := checker.GetAdminGroups()
		require.Error(t, err)
		assert.Nil(t, groups)
		assert.Contains(t, err.Error(), "adminGroups field not found")
	})

	t.Run("EmptyAdminGroups", func(t *testing.T) {
		authCR := createAuthCR([]string{})
		lister := &mockAuthLister{authCR: authCR}
		checker := auth.NewAdminChecker(lister)

		groups, err := checker.GetAdminGroups()
		require.NoError(t, err)
		assert.Empty(t, groups, "empty admin groups should be allowed (Auth CR validation handles minimum)")
	})
}
