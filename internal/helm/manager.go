package helm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Manager struct {
	settings *cli.EnvSettings
}

// ErrReleaseNotFound is returned when a Helm release does not exist. The Helm
// driver surfaces this as a plain "release: not found" error, which does NOT
// satisfy k8s.io/apimachinery's errors.IsNotFound (that only matches Kubernetes
// StatusError). Callers must match against this sentinel instead.
var ErrReleaseNotFound = errors.New("release: not found")

func NewManager() *Manager {
	return &Manager{
		settings: cli.New(),
	}
}

func (m *Manager) newActionConfig(namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)

	configFlags := &genericclioptions.ConfigFlags{
		Namespace: &namespace,
	}

	err := cfg.Init(configFlags, namespace, "secrets", func(format string, v ...interface{}) {
		log.Log.Info(fmt.Sprintf(format, v...))
	})
	if err != nil {
		return nil, fmt.Errorf("failed to init helm action config: %w", err)
	}

	return cfg, nil
}

func (m *Manager) InstallOrUpgrade(ctx context.Context, releaseName, namespace, repoURL, chartName, chartVersion string, values map[string]interface{}, waitTimeout time.Duration, force bool, atomic bool, wait bool) (*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	existing, err := m.GetRelease(releaseName, namespace)
	if err != nil && !errors.Is(err, ErrReleaseNotFound) {
		return nil, fmt.Errorf("failed to check existing release: %w", err)
	}

	chartPath, err := m.downloadChart(repoURL, chartName, chartVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to download chart: %w", err)
	}
	defer os.RemoveAll(filepath.Dir(chartPath))

	chart, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	if existing != nil {
		return m.upgrade(cfg, releaseName, chart, values, waitTimeout, force, atomic, wait)
	}
	return m.install(cfg, releaseName, namespace, chart, values, waitTimeout, wait)
}

func (m *Manager) InstallFromLocal(ctx context.Context, releaseName, namespace, localPath string, values map[string]interface{}, waitTimeout time.Duration, force bool, atomic bool, wait bool) (*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	existing, err := m.GetRelease(releaseName, namespace)
	if err != nil && !errors.Is(err, ErrReleaseNotFound) {
		return nil, fmt.Errorf("failed to check existing release: %w", err)
	}

	chart, err := loader.Load(localPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart from %s: %w", localPath, err)
	}

	if existing != nil {
		return m.upgrade(cfg, releaseName, chart, values, waitTimeout, force, atomic, wait)
	}
	return m.install(cfg, releaseName, namespace, chart, values, waitTimeout, wait)
}

func (m *Manager) install(cfg *action.Configuration, releaseName, namespace string, ch *chart.Chart, values map[string]interface{}, waitTimeout time.Duration, wait bool) (*release.Release, error) {
	client := action.NewInstall(cfg)
	client.ReleaseName = releaseName
	client.Namespace = namespace
	client.CreateNamespace = true
	client.Wait = wait
	client.Timeout = waitTimeout

	return client.Run(ch, values)
}

func (m *Manager) upgrade(cfg *action.Configuration, releaseName string, ch *chart.Chart, values map[string]interface{}, waitTimeout time.Duration, force bool, atomic bool, wait bool) (*release.Release, error) {
	client := action.NewUpgrade(cfg)
	client.Wait = wait
	client.Timeout = waitTimeout
	client.Force = force
	client.Atomic = atomic

	return client.Run(releaseName, ch, values)
}

func (m *Manager) Uninstall(releaseName, namespace string) error {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return err
	}

	client := action.NewUninstall(cfg)
	_, err = client.Run(releaseName)
	return err
}

func (m *Manager) GetRelease(releaseName, namespace string) (*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewGet(cfg)
	rel, err := client.Run(releaseName)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return nil, ErrReleaseNotFound
	}
	return rel, err
}

func (m *Manager) Rollback(releaseName, namespace string, version int, waitTimeout time.Duration) error {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return err
	}

	client := action.NewRollback(cfg)
	client.Wait = true
	client.Timeout = waitTimeout

	if version > 0 {
		client.Version = version
	}

	return client.Run(releaseName)
}

func (m *Manager) downloadChart(repoURL, chartName, chartVersion string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "helm-chart-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	repoFile := filepath.Join(tmpDir, "repositories.yaml")
	repoCache := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(repoCache, 0755); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}

	// Write a throwaway repo file so the Helm repo machinery can resolve the
	// index relative to repoURL.
	r := repo.NewFile()
	r.Add(&repo.Entry{Name: "inferguard-repo", URL: repoURL})
	if err := r.WriteFile(repoFile, 0644); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}

	chartRepo, err := repo.NewChartRepository(
		&repo.Entry{Name: "inferguard-repo", URL: repoURL},
		getter.All(m.settings),
	)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}
	chartRepo.CachePath = repoCache

	indexPath, err := chartRepo.DownloadIndexFile()
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to download index: %w", err)
	}

	idx, err := repo.LoadIndexFile(indexPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to load index: %w", err)
	}

	cv, err := idx.Get(chartName, chartVersion)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("chart %s not found in repository index: %w", chartName, err)
	}
	if len(cv.URLs) == 0 {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("chart %s has no downloadable URL", chartName)
	}

	// Index URLs are usually relative to the repository root; resolve them.
	chartURL := cv.URLs[0]
	if parsed, err := url.Parse(chartURL); err != nil || !parsed.IsAbs() {
		chartURL = strings.TrimSuffix(repoURL, "/") + "/" + strings.TrimPrefix(chartURL, "/")
	}

	scheme := "http"
	if parsed, err := url.Parse(chartURL); err == nil && parsed.Scheme != "" {
		scheme = parsed.Scheme
	}
	g, err := getter.All(m.settings).ByScheme(scheme)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("no getter for scheme %q: %w", scheme, err)
	}

	buf, err := g.Get(chartURL)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to download chart archive: %w", err)
	}

	dest := filepath.Join(tmpDir, fmt.Sprintf("%s-%s.tgz", chartName, cv.Version))
	if err := os.WriteFile(dest, buf.Bytes(), 0644); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}

	return dest, nil
}

