package main

import (
	"context"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	helmv1alpha1 "watchpod/api/v1alpha1"
	"watchpod/internal/controller"
	"watchpod/internal/helm"
	"watchpod/pkg/podwatch"
	"watchpod/pkg/policy"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(helmv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "watchpod-helm-operator",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	k8sClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes client")
		os.Exit(1)
	}

	watcher := podwatch.NewGlobalPodWatcher(k8sClient, 5*time.Minute, ":9090")
	if err := watcher.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "unable to start global pod watcher")
		os.Exit(1)
	}

	policyEngine := policy.NewEngine(
		func(ctx context.Context, releaseName, namespace string, revision int) (string, error) {
			helmMgr := helm.NewManager()
			if err := helmMgr.Rollback(releaseName, namespace, revision, 5*time.Minute); err != nil {
				return "", err
			}
			rel, err := helmMgr.GetRelease(releaseName, namespace)
			if err != nil {
				return "", err
			}
			if rel.Chart != nil && rel.Chart.Metadata != nil {
				return rel.Chart.Metadata.Version, nil
			}
			return "", nil
		},
		func(ctx context.Context, releaseName, message string) error {
			setupLog.Info("Policy triggered", "release", releaseName, "message", message)
			return nil
		},
		func(ctx context.Context, releaseName, namespace, newVersion string) error {
			hrList := &helmv1alpha1.HelmReleaseList{}
			if err := mgr.GetClient().List(ctx, hrList); err != nil {
				return err
			}
			for i := range hrList.Items {
				if hrList.Items[i].GetReleaseName() == releaseName {
					fresh := hrList.Items[i].DeepCopy()
					fresh.Spec.Chart.Version = newVersion
					fresh.Spec.TargetRevision = ""
					return mgr.GetClient().Update(ctx, fresh)
				}
			}
			return nil
		},
	)
	watcher.SetPolicyEngine(policyEngine)

	reconciler := controller.NewHelmReleaseReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		k8sClient,
		watcher,
	)

	if err = reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HelmRelease")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}