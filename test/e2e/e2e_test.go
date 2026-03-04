//go:build e2e

package e2e_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test directory")
	}
	return filepath.Dir(file)
}

func ensurePythonVenv(t *testing.T, dir string) string {
	t.Helper()
	venv := filepath.Join(dir, ".venv")
	python := filepath.Join(venv, "bin", "python")

	if _, err := os.Stat(python); err != nil {
		t.Log("Creating Python virtual environment...")
		cmd := exec.Command("python3", "-m", "venv", venv)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to create venv: %v\n%s", err, out)
		}
	}

	t.Log("Installing Python dependencies...")
	pip := exec.Command(python, "-m", "pip", "install", "--upgrade", "pip", "--quiet")
	if out, err := pip.CombinedOutput(); err != nil {
		t.Fatalf("pip upgrade failed: %v\n%s", err, out)
	}

	reqs := exec.Command(python, "-m", "pip", "install", "-r", filepath.Join(dir, "requirements.txt"), "--quiet")
	if out, err := reqs.CombinedOutput(); err != nil {
		t.Fatalf("pip install requirements failed: %v\n%s", err, out)
	}

	return python
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("required tool %q not found on PATH, skipping", name)
	}
}

func requireEnvOrCluster(t *testing.T) {
	t.Helper()
	if os.Getenv("MAAS_API_BASE_URL") != "" {
		return
	}
	requireTool(t, "oc")
	out, err := exec.Command("oc", "get", "ingresses.config.openshift.io", "cluster",
		"-o", "jsonpath={.spec.domain}").CombinedOutput()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Skip("MAAS_API_BASE_URL not set and cannot auto-detect cluster domain, skipping")
	}
	domain := strings.TrimSpace(string(out))
	host := "maas." + domain

	scheme := "https"
	if os.Getenv("INSECURE_HTTP") == "true" {
		scheme = "http"
	}
	url := fmt.Sprintf("%s://%s/maas-api", scheme, host)
	t.Setenv("MAAS_API_BASE_URL", url)
	t.Setenv("HOST", host)
	t.Logf("Auto-detected MAAS_API_BASE_URL=%s", url)
}

func ensureToken(t *testing.T) {
	t.Helper()
	if os.Getenv("TOKEN") != "" {
		return
	}
	if ns := os.Getenv("E2E_TEST_TOKEN_SA_NAMESPACE"); ns != "" {
		sa := os.Getenv("E2E_TEST_TOKEN_SA_NAME")
		if sa == "" {
			t.Fatal("E2E_TEST_TOKEN_SA_NAMESPACE set but E2E_TEST_TOKEN_SA_NAME is empty")
		}
		out, err := exec.Command("oc", "create", "token", sa, "-n", ns, "--duration=30m").CombinedOutput()
		if err != nil {
			t.Fatalf("failed to create SA token: %v\n%s", err, out)
		}
		t.Setenv("TOKEN", strings.TrimSpace(string(out)))
		return
	}
	out, err := exec.Command("oc", "whoami", "-t").CombinedOutput()
	if err != nil {
		t.Skip("no TOKEN set and oc whoami -t failed, skipping")
	}
	t.Setenv("TOKEN", strings.TrimSpace(string(out)))
}

func runPytest(t *testing.T, python, dir string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatalf("failed to create reports dir: %v", err)
	}

	fullArgs := append([]string{"-m", "pytest"}, args...)
	cmd := exec.Command(python, fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	t.Logf("Running: %s %s", python, strings.Join(fullArgs, " "))
	if err := cmd.Run(); err != nil {
		t.Fatalf("pytest failed: %v", err)
	}
}

// TestSmoke runs the smoke test suite.
//
//	go test -tags=e2e -run TestSmoke -timeout=10m -v ./test/e2e/
func TestSmoke(t *testing.T) {
	dir := testDir(t)
	requireTool(t, "python3")
	requireEnvOrCluster(t)
	ensureToken(t)

	python := ensurePythonVenv(t, dir)
	runPytest(t, python, dir,
		"-q", "--maxfail=1", "--disable-warnings",
		"--junitxml="+filepath.Join(dir, "reports", "smoke.xml"),
		filepath.Join(dir, "tests"),
	)
}

// TestSubscription runs the subscription controller E2E tests.
//
//	go test -tags=e2e -run TestSubscription -timeout=15m -v ./test/e2e/
func TestSubscription(t *testing.T) {
	dir := testDir(t)
	requireTool(t, "python3")
	requireTool(t, "oc")
	requireEnvOrCluster(t)
	ensureToken(t)

	if os.Getenv("GATEWAY_HOST") == "" {
		host := os.Getenv("HOST")
		if host == "" {
			t.Skip("GATEWAY_HOST not set, skipping subscription tests")
		}
		t.Setenv("GATEWAY_HOST", host)
	}

	python := ensurePythonVenv(t, dir)
	runPytest(t, python, dir,
		"-v", "--maxfail=3", "--disable-warnings",
		"--junitxml="+filepath.Join(dir, "reports", "subscription.xml"),
		filepath.Join(dir, "tests", "test_subscription.py"),
	)
}

// TestFull runs the full E2E suite: deploy + all tests (nightly CI).
//
//	go test -tags=e2e -run TestFull -timeout=30m -v ./test/e2e/
func TestFull(t *testing.T) {
	dir := testDir(t)
	requireTool(t, "oc")
	requireTool(t, "kubectl")
	requireTool(t, "kustomize")

	script := filepath.Join(dir, "scripts", "prow_run_smoke_test.sh")
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	t.Log("Running full E2E suite (deploy + test)...")
	if err := cmd.Run(); err != nil {
		t.Fatalf("full E2E failed: %v", err)
	}
}
