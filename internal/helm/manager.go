package helm

import (
	"context"
	"fmt"
	"os"
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

func (m *Manager) InstallOrUpgrade(ctx context.Context, releaseName, namespace, repoURL, chartName, chartVersion string, values map[string]interface{}, waitTimeout time.Duration, force bool) (*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	existing, err := m.GetRelease(releaseName, namespace)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return nil, fmt.Errorf("failed to check existing release: %w", err)
	}

	chartPath, err := m.downloadChart(repoURL, chartName, chartVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to download chart: %w", err)
	}
	defer os.Remove(chartPath)

	chart, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	if existing != nil {
		return m.upgrade(cfg, releaseName, chart, values, waitTimeout, force)
	}
	return m.install(cfg, releaseName, namespace, chart, values, waitTimeout)
}

func (m *Manager) InstallFromLocal(ctx context.Context, releaseName, namespace, localPath string, values map[string]interface{}, waitTimeout time.Duration, force bool) (*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	existing, err := m.GetRelease(releaseName, namespace)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return nil, fmt.Errorf("failed to check existing release: %w", err)
	}

	chart, err := loader.Load(localPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart from %s: %w", localPath, err)
	}

	if existing != nil {
		return m.upgrade(cfg, releaseName, chart, values, waitTimeout, force)
	}
	return m.install(cfg, releaseName, namespace, chart, values, waitTimeout)
}

func (m *Manager) install(cfg *action.Configuration, releaseName, namespace string, ch *chart.Chart, values map[string]interface{}, waitTimeout time.Duration) (*release.Release, error) {
	client := action.NewInstall(cfg)
	client.ReleaseName = releaseName
	client.Namespace = namespace
	client.CreateNamespace = true
	client.Wait = true
	client.Timeout = waitTimeout

	return client.Run(ch, values)
}

func (m *Manager) upgrade(cfg *action.Configuration, releaseName string, ch *chart.Chart, values map[string]interface{}, waitTimeout time.Duration, force bool) (*release.Release, error) {
	client := action.NewUpgrade(cfg)
	client.Wait = true
	client.Timeout = waitTimeout
	client.Force = force

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
	return client.Run(releaseName)
}

func (m *Manager) ListReleases(namespace string) ([]*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewList(cfg)
	client.All = true
	client.AllNamespaces = false

	return client.Run()
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

func (m *Manager) GetHistory(releaseName, namespace string) ([]*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewHistory(cfg)
	return client.Run(releaseName)
}

func (m *Manager) GetValues(releaseName, namespace string, allValues bool) (map[string]interface{}, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewGetValues(cfg)
	client.AllValues = allValues

	return client.Run(releaseName)
}

func (m *Manager) ReleaseStatus(releaseName, namespace string) (*release.Release, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewStatus(cfg)
	return client.Run(releaseName)
}

func (m *Manager) Template(releaseName, namespace string, chart *chart.Chart, values map[string]interface{}, dryRun bool) (string, error) {
	cfg, err := m.newActionConfig(namespace)
	if err != nil {
		return "", err
	}

	client := action.NewInstall(cfg)
	client.DryRun = dryRun
	client.ReleaseName = releaseName
	client.Namespace = namespace
	client.IncludeCRDs = true
	client.SkipCRDs = false

	rel, err := client.Run(chart, values)
	if err != nil {
		return "", err
	}

	return rel.Manifest, nil
}

func (m *Manager) downloadChart(repoURL, chartName, chartVersion string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "helm-chart-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	repoFile := tmpDir + "/repositories.yaml"
	repoCache := tmpDir + "/cache"

	if err := os.MkdirAll(repoCache, 0755); err != nil {
		return "", err
	}

	r := repo.NewFile()
	r.Add(&repo.Entry{
		Name: "inferguard-repo",
		URL:  repoURL,
	})
	if err := r.WriteFile(repoFile, 0644); err != nil {
		return "", err
	}

	chartRepo, err := repo.NewChartRepository(
		&repo.Entry{Name: "inferguard-repo", URL: repoURL},
		getter.All(&cli.EnvSettings{}),
	)
	if err != nil {
		return "", err
	}
	chartRepo.CachePath = repoCache

	if _, err := chartRepo.DownloadIndexFile(); err != nil {
		return "", fmt.Errorf("failed to download index: %w", err)
	}

	pull := action.NewPull()
	pull.Settings = &cli.EnvSettings{}
	pull.Version = chartVersion
	pull.DestDir = tmpDir
	pull.Untar = true

	_, err = pull.Run(repoURL + "/" + chartName)
	if err != nil {
		return "", fmt.Errorf("failed to pull chart: %w", err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), chartName) {
			return tmpDir + "/" + entry.Name(), nil
		}
	}

	return "", fmt.Errorf("chart directory not found")
}
