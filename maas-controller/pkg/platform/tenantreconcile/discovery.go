package tenantreconcile

import (
	"context"
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// UnservedKinds collects the kinds a reconcile skipped because the REST mapper did not
// know them. A CRD turns Established before discovery lists its kind, and nothing reports
// discovery catching up, so the caller rechecks these kinds once the reconcile returns.
type UnservedKinds struct {
	mu    sync.Mutex
	kinds []schema.GroupVersionKind
}

type unservedKindsKey struct{}

// WithUnservedKinds returns a context whose reconcile records the kinds it skips.
func WithUnservedKinds(ctx context.Context) (context.Context, *UnservedKinds) {
	u := &UnservedKinds{}
	return context.WithValue(ctx, unservedKindsKey{}, u), u
}

// Kinds returns the recorded kinds.
func (u *UnservedKinds) Kinds() []schema.GroupVersionKind {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.kinds)
}

// KindNotServed reports whether err is the REST mapper not knowing gvk, recording gvk for
// the reconcile in ctx when it is. Every place that skips an optional kind on that error
// goes through here, so the skip is retried once discovery serves the kind.
func KindNotServed(ctx context.Context, gvk schema.GroupVersionKind, err error) bool {
	if !meta.IsNoMatchError(err) {
		return false
	}
	if u, ok := ctx.Value(unservedKindsKey{}).(*UnservedKinds); ok {
		u.mu.Lock()
		if !slices.Contains(u.kinds, gvk) {
			u.kinds = append(u.kinds, gvk)
		}
		u.mu.Unlock()
	}
	return true
}
