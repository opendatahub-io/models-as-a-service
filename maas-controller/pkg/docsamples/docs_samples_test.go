package docsamples_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// defaultNamespace is where kubectl puts a namespaced object that declares none.
const defaultNamespace = "default"

// The table sits in a Describe so its entries are produced while the spec tree is
// built, after RegisterFailHandler: at package init pkgtest.ProjectRoot's assertion
// would panic.
var _ = Describe("Documentation samples", func() {
	DescribeTable("are admitted by the API server",
		func(ctx SpecContext, doc sampleDoc) {
			Expect(doc.err).NotTo(HaveOccurred())

			obj := doc.obj.DeepCopy()
			gvk := obj.GroupVersionKind()

			mapping, err := envTest.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
			Expect(err).NotTo(HaveOccurred(), "no API available for %s - install its CRD in sampleCRDs", gvk)

			if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
				if obj.GetNamespace() == "" {
					obj.SetNamespace(defaultNamespace)
				}
				ensureNamespace(ctx, obj.GetNamespace())
			}

			// Strict field validation is what makes this check worth running: without
			// it the API server prunes unknown fields and returns success, so a renamed
			// or retired field looks fine here while `kubectl apply` (strict since 1.25)
			// rejects it.
			strict := client.FieldValidation(metav1.FieldValidationStrict)
			Expect(envTest.Create(ctx, obj, client.DryRunAll, strict)).To(Succeed(),
				"sample rejected by the API server - it would fail the same way for anyone following the docs")
		},
		sampleEntries(filepath.Join(pkgtest.ProjectRoot(), "docs", "samples")),
	)
})

// sampleDoc is one object from a sample. A sample that could not be read or built
// carries err instead of obj, so it fails its entry rather than going missing.
type sampleDoc struct {
	source string
	obj    *unstructured.Unstructured
	err    error
}

func (d sampleDoc) String() string {
	if d.obj == nil {
		return d.source
	}

	name := d.obj.GetName()
	if ns := d.obj.GetNamespace(); ns != "" {
		name = ns + "/" + name
	}

	return fmt.Sprintf("%s (%s %s)", d.source, d.obj.GetKind(), name)
}

// sampleEntries produces a table entry for every object under dir, the two ways the
// docs apply samples: each YAML file as it is (`kubectl apply -f`) and each
// kustomization as it builds (`kustomize build`).
func sampleEntries(dir string) []TableEntry {
	var docs []sampleDoc
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			// Walked into; a kustomization is found through its file.
		case kustomizationNames[entry.Name()]:
			docs = append(docs, buildKustomization(filepath.Dir(path))...)
		case filepath.Ext(path) == ".yaml" || filepath.Ext(path) == ".yml":
			docs = append(docs, decodeFile(path)...)
		}
		return nil
	})
	if walkErr != nil {
		docs = append(docs, sampleDoc{source: repoPath(dir), err: walkErr})
	}
	if len(docs) == 0 {
		docs = append(docs, sampleDoc{source: repoPath(dir), err: errors.New("no samples found - has the directory moved?")})
	}

	entries := make([]TableEntry, 0, len(docs))
	for _, doc := range docs {
		entries = append(entries, Entry(doc.String(), doc))
	}

	return entries
}

// kustomizationNames are the file names kustomize recognises. They configure a build
// rather than describing a cluster resource; their directories are built instead.
var kustomizationNames = map[string]bool{
	"kustomization.yaml": true,
	"kustomization.yml":  true,
	"Kustomization":      true,
}

func decodeFile(path string) []sampleDoc {
	source := repoPath(path)

	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return []sampleDoc{{source: source, err: err}}
	}

	return decodeDocs(source, raw)
}

func buildKustomization(dir string) []sampleDoc {
	source := "kustomize build " + repoPath(dir)

	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return []sampleDoc{{source: source, err: err}}
	}

	// YAML, not MarshalJSON: encoding/json escapes < and >, which hides placeholders.
	out, err := resMap.AsYaml()
	if err != nil {
		return []sampleDoc{{source: source, err: err}}
	}

	return decodeDocs(source, out)
}

// decodeDocs splits a YAML stream into objects. Objects are told apart by kind and
// name in their entry; a document that fails carries its index instead.
func decodeDocs(source string, raw []byte) []sampleDoc {
	var docs []sampleDoc
	fail := func(index int, err error) {
		docs = append(docs, sampleDoc{source: fmt.Sprintf("%s document %d", source, index), err: err})
	}

	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for index := 0; ; index++ {
		chunk, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail(index, err)
			break
		}

		if chunk, err = fillPlaceholders(chunk); err != nil {
			fail(index, err)
			continue
		}

		// Strict: a duplicated key would otherwise collapse to the last occurrence
		// here, so the API server would never get the chance to notice it.
		obj := &unstructured.Unstructured{}
		if err := yaml.UnmarshalStrict(chunk, &obj.Object); err != nil {
			fail(index, err)
			continue
		}
		// A chunk holding only comments carries no fields once decoded.
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" {
			fail(index, errors.New("document declares no apiVersion/kind"))
			continue
		}

		docs = append(docs, sampleDoc{source: source, obj: obj})
	}

	return docs
}

// placeholders maps the fill-in values the docs ask users to replace to stand-ins
// that pass validation. A placeholder missing here fails its sample, so a new one
// gets a stand-in on purpose rather than turning its sample into noise.
var placeholders = strings.NewReplacer(
	"<application-namespace>", "opendatahub",
	"<cluster-domain>", "apps.example.com",
)

var placeholderPattern = regexp.MustCompile(`<[a-z][a-z0-9-]*>`)

func fillPlaceholders(raw []byte) ([]byte, error) {
	filled := placeholders.Replace(string(raw))
	if unknown := placeholderPattern.FindAllString(filled, -1); len(unknown) > 0 {
		return nil, fmt.Errorf("placeholders without a stand-in: %v", unknown)
	}

	return []byte(filled), nil
}

// repoPath names a sample by its path from the repository root in spec names.
func repoPath(path string) string {
	rel, err := filepath.Rel(pkgtest.ProjectRoot(), path)
	if err != nil {
		return path
	}

	return filepath.ToSlash(rel)
}

// ensureNamespace creates name unless it exists. The API server only admits a create
// into an existing namespace, even a dry-run one.
func ensureNamespace(ctx context.Context, name string) {
	GinkgoHelper()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(client.IgnoreAlreadyExists(envTest.Create(ctx, namespace))).To(Succeed())
}
