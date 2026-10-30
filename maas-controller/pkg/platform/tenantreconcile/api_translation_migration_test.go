package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func apiTranslationTenant() *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "rhoai",
	}}
}

func TestPersistAPITranslationMigrationIntent(t *testing.T) {
	tests := []struct {
		name           string
		persistDefault bool
		setting        *bool
		wantExplicit   bool
		wantEnabled    bool
	}{
		{name: "pure default stays omitted", persistDefault: false},
		{name: "response baseline persists true", persistDefault: true, wantEnabled: true},
		{name: "explicit false wins", persistDefault: true, setting: migrationBoolPtr(false), wantExplicit: true},
		{name: "explicit true remains true", persistDefault: false, setting: migrationBoolPtr(true), wantExplicit: true, wantEnabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tenant := apiTranslationTenant()
			if test.setting != nil {
				tenant.Spec.APITranslation = &maasv1alpha1.TenantAPITranslationConfig{Enabled: test.setting}
			}
			cl := fake.NewClientBuilder().WithScheme(pluginsMigrationScheme(t)).WithObjects(tenant).Build()
			got, err := persistAPITranslationMigrationIntent(context.Background(), cl, cl, tenant, test.persistDefault)
			require.NoError(t, err)
			require.Equal(t, test.wantExplicit, got.Explicit)
			require.Equal(t, test.wantEnabled, got.Enabled)

			stored := &maasv1alpha1.MaasTenantConfig{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
			if test.persistDefault || test.setting != nil {
				require.NotNil(t, stored.Spec.APITranslation)
				require.NotNil(t, stored.Spec.APITranslation.Enabled)
				require.Equal(t, test.wantEnabled, *stored.Spec.APITranslation.Enabled)
			} else {
				require.Nil(t, stored.Spec.APITranslation)
			}
		})
	}
}

func TestPersistAPITranslationMigrationIntentIsRestartIdempotent(t *testing.T) {
	tenant := apiTranslationTenant()
	cl := fake.NewClientBuilder().WithScheme(pluginsMigrationScheme(t)).WithObjects(tenant).Build()
	first, err := persistAPITranslationMigrationIntent(context.Background(), cl, cl, tenant, true)
	require.NoError(t, err)
	require.Equal(t, apiTranslationMigrationIntent{Enabled: true}, first)

	// A controller restart sees the durable true value as an existing decision
	// and does not rewrite it or infer a different value from a missing ConfigMap.
	second, err := persistAPITranslationMigrationIntent(context.Background(), cl, cl, tenant, false)
	require.NoError(t, err)
	require.Equal(t, apiTranslationMigrationIntent{Explicit: true, Enabled: true}, second)
}

func TestPersistAPITranslationMigrationIntentConflictPreservesOperatorChoice(t *testing.T) {
	tenant := apiTranslationTenant()
	base := fake.NewClientBuilder().WithScheme(pluginsMigrationScheme(t)).WithObjects(tenant).Build()
	cl := &apiTranslationConflictClient{Client: base}
	got, err := persistAPITranslationMigrationIntent(context.Background(), cl, cl, tenant, true)
	require.NoError(t, err)
	require.True(t, got.Explicit)
	require.False(t, got.Enabled)

	stored := &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
	require.NotNil(t, stored.Spec.APITranslation)
	require.NotNil(t, stored.Spec.APITranslation.Enabled)
	require.False(t, *stored.Spec.APITranslation.Enabled)
}

func TestPersistAPITranslationMigrationIntentUpdateFailureBlocksCleanupCaller(t *testing.T) {
	tenant := apiTranslationTenant()
	base := fake.NewClientBuilder().WithScheme(pluginsMigrationScheme(t)).WithObjects(tenant).Build()
	cl := &apiTranslationFailingUpdateClient{Client: base}
	_, err := persistAPITranslationMigrationIntent(context.Background(), cl, cl, tenant, true)
	require.ErrorContains(t, err, "write denied")

	stored := &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
	require.Nil(t, stored.Spec.APITranslation)
}

type apiTranslationConflictClient struct {
	client.Client
	injected bool
}

func (c *apiTranslationConflictClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if !c.injected {
		if tenant, ok := obj.(*maasv1alpha1.MaasTenantConfig); ok && tenant.Spec.APITranslation != nil && tenant.Spec.APITranslation.Enabled != nil && *tenant.Spec.APITranslation.Enabled {
			c.injected = true
			latest := &maasv1alpha1.MaasTenantConfig{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(tenant), latest); err != nil {
				return err
			}
			latest.Spec.APITranslation = &maasv1alpha1.TenantAPITranslationConfig{Enabled: migrationBoolPtr(false)}
			if err := c.Client.Update(ctx, latest); err != nil {
				return err
			}
			return apierrors.NewConflict(schema.GroupResource{Group: "maas.opendatahub.io", Resource: "maastenantconfigs"}, tenant.Name, errors.New("operator set apiTranslation.enabled=false"))
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

type apiTranslationFailingUpdateClient struct{ client.Client }

func (c *apiTranslationFailingUpdateClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errors.New("write denied")
}

func migrationBoolPtr(v bool) *bool { return &v }
