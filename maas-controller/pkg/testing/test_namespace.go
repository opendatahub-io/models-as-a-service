package testing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestNamespace is a namespace owned by the current spec.
type TestNamespace struct {
	Name string
}

// TestNamespaceOption customizes a TestNamespace.
type TestNamespaceOption func(*testNamespaceConfig)

type testNamespaceConfig struct {
	suffix string
}

// WithNameSuffix distinguishes several namespaces created by one spec.
func WithNameSuffix(suffix string) TestNamespaceOption {
	return func(cfg *testNamespaceConfig) {
		cfg.suffix = suffix
	}
}

// NewTestNamespace creates a namespace named after the current spec. envtest runs no
// namespace controller, so a deleted namespace never finishes terminating; specs get
// unique names instead of cleanup.
func NewTestNamespace(ctx context.Context, c *Client, opts ...TestNamespaceOption) *TestNamespace {
	cfg := &testNamespaceConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	spec := ginkgo.CurrentSpecReport()
	parts := append(append([]string{"test"}, spec.ContainerHierarchyTexts...), spec.LeafNodeText, cfg.suffix)
	name := namespaceName(strings.Join(parts, "-"))

	gomega.Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(gomega.Succeed())
	return &TestNamespace{Name: name}
}

var nonDNSChars = regexp.MustCompile(`[^a-z0-9]+`)

// namespaceName turns text into a DNS label, keeping it unique with a hash suffix
// when it has to be shortened.
func namespaceName(text string) string {
	name := strings.Trim(nonDNSChars.ReplaceAllString(strings.ToLower(text), "-"), "-")
	const maxLen = 63
	if len(name) <= maxLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(sum[:4])
	return strings.TrimRight(name[:maxLen-len(hash)-1], "-") + "-" + hash
}
