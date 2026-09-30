package tenantreconcile

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestKindNotServed(t *testing.T) {
	noMatch := &meta.NoKindMatchError{GroupKind: GVKEnvoyFilter.GroupKind(), SearchedVersions: []string{GVKEnvoyFilter.Version}}

	t.Run("records a kind the REST mapper does not know", func(t *testing.T) {
		ctx, unserved := WithUnservedKinds(t.Context())

		assert.True(t, KindNotServed(ctx, GVKEnvoyFilter, noMatch))
		assert.True(t, KindNotServed(ctx, GVKEnvoyFilter, noMatch))
		assert.Equal(t, []schema.GroupVersionKind{GVKEnvoyFilter}, unserved.Kinds())
	})

	t.Run("ignores other errors", func(t *testing.T) {
		ctx, unserved := WithUnservedKinds(t.Context())

		assert.False(t, KindNotServed(ctx, GVKEnvoyFilter, errors.New("connection refused")))
		assert.False(t, KindNotServed(ctx, GVKEnvoyFilter, nil))
		assert.Empty(t, unserved.Kinds())
	})

	t.Run("works without a recorder in the context", func(t *testing.T) {
		assert.True(t, KindNotServed(t.Context(), GVKEnvoyFilter, noMatch))
	})
}
