package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	helmrelease "helm.sh/helm/v3/pkg/release"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "watchpod/api/v1alpha1"
	"watchpod/internal/helm"
	watchpodlog "watchpod/pkg/log"
	"watchpod/pkg/podwatch"
)

const (
	helmReleaseFinalizer = "helm.watchpod.io/finalizer"
	maxBackoff           = 10 * time.Minute
)

type HelmReleaseReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	HelmManager *helm.Manager
	K8sClient   kubernetes.Interface

	Watcher      *podwatch.GlobalPodWatcher
	releaseNames map[string]string
}

func NewHelmReleaseReconciler(client client.Client, scheme *runtime.Scheme, k8sClient kubernetes.Interface, watcher *podwatch.GlobalPodWatcher) *HelmReleaseReconciler {
	return &HelmReleaseReconciler{
		Client:       client,
		Scheme:       scheme,
		HelmManager:  helm.NewManager(),
		K8sClient:    k8sClient,
		Watcher:      watcher,
		releaseNames: make(map[string]string),
	}
}

func (r *HelmReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = ctrlLog.FromContext(ctx)

	hr := &helmv1alpha1.HelmRelease{}
	if err := r.Get(ctx, req.NamespacedName, hr); err != nil {
		if errors.IsNotFound(err) {
			key := req.NamespacedName.String()
			if releaseName, ok := r.releaseNames[key]; ok {
				r.Watcher.UnregisterRelease(releaseName)
				delete(r.releaseNames, key)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion.
	if !hr.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(hr, helmReleaseFinalizer) {
			if err := r.finalizeHelmRelease(ctx, hr); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(hr, helmReleaseFinalizer)
			if err := r.Update(ctx, hr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Stable: spec unchanged and already Running — nothing to do.
	if hr.IsStable() {
		return ctrl.Result{}, nil
	}

	// Retries exhausted: permanent failure or too many transient failures.
	if hr.HasRetriesExhausted() {
		watchpodlog.Info("Retries exhausted, giving up",
			"name", hr.Name,
			"retryCount", hr.Status.RetryCount,
			"lastError", hr.Status.LastFailureMessage)
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present.
	if !controllerutil.ContainsFinalizer(hr, helmReleaseFinalizer) {
		controllerutil.AddFinalizer(hr, helmReleaseFinalizer)
		if err := r.Update(ctx, hr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Attempt reconciliation.
	if err := r.reconcileHelmRelease(ctx, hr); err != nil {
		backoff := time.Duration(1<<hr.Status.RetryCount) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		watchpodlog.Info("Reconciliation failed, retrying with backoff",
			"name", hr.Name,
			"retryCount", hr.Status.RetryCount,
			"backoff", backoff)
		return ctrl.Result{RequeueAfter: backoff}, nil
	}

	// Success — no periodic requeue. Wait for spec change.
	return ctrl.Result{}, nil
}

func (r *HelmReleaseReconciler) reconcileHelmRelease(ctx context.Context, hr *helmv1alpha1.HelmRelease) error {
	if hr.ShouldRollback() {
		return r.performRollback(ctx, hr)
	}
	return r.performInstallOrUpgrade(ctx, hr)
}

func (r *HelmReleaseReconciler) performInstallOrUpgrade(ctx context.Context, hr *helmv1alpha1.HelmRelease) error {
	releaseName := hr.GetReleaseName()
	releaseNs := hr.GetReleaseNamespace()

	values, err := r.getValues(hr)
	if err != nil {
		r.updateStatusFailed(ctx, hr, "values parsing failed: "+err.Error())
		return err
	}

	existing, err := r.HelmManager.GetRelease(releaseName, releaseNs)
	if err != nil && !errors.IsNotFound(err) {
		r.updateStatusFailed(ctx, hr, "failed to query release: "+err.Error())
		return err
	}

	if existing == nil {
		r.updateStatusPhase(ctx, hr, helmv1alpha1.PhaseInstalling)
	} else {
		r.updateStatusPhase(ctx, hr, helmv1alpha1.PhaseUpgrading)
	}

	var rel *helmrelease.Release
	if hr.Spec.Chart.LocalPath != "" {
		rel, err = r.HelmManager.InstallFromLocal(ctx, releaseName, releaseNs, hr.Spec.Chart.LocalPath, values, hr.GetWaitTimeout(), hr.ShouldForceUpgrade())
	} else {
		rel, err = r.HelmManager.InstallOrUpgrade(ctx, releaseName, releaseNs, hr.Spec.Chart.Repository, hr.Spec.Chart.Name, hr.Spec.Chart.Version, values, hr.GetWaitTimeout(), hr.ShouldForceUpgrade())
	}
	if err != nil {
		if hr.ShouldAtomic() && existing != nil {
			watchpodlog.Info("Atomic upgrade failed, rolling back",
				"release", releaseName, "namespace", releaseNs, "error", err.Error())
			if rollbackErr := r.HelmManager.Rollback(releaseName, releaseNs, 0, hr.GetWaitTimeout()); rollbackErr != nil {
				watchpodlog.ErrorE(rollbackErr, "Failed to rollback after atomic upgrade failure",
					"release", releaseName, "namespace", releaseNs)
			}
		}
		r.updateStatusFailed(ctx, hr, err.Error())

		if !isRetryable(err) {
			watchpodlog.Info("Permanent failure detected, giving up",
				"release", releaseName, "error", err.Error())
			updated := hr.DeepCopy()
			updated.Status.RetryCount = helmv1alpha1.MaxTransientRetries
			updated.Status.LastFailureMessage = err.Error()
			r.Status().Update(ctx, updated)
		}

		return err
	}

	if err := r.updateStatusSuccess(ctx, hr, rel); err != nil {
		return err
	}

	return r.managePodMonitor(ctx, hr)
}

func (r *HelmReleaseReconciler) performRollback(ctx context.Context, hr *helmv1alpha1.HelmRelease) error {
	releaseName := hr.GetReleaseName()
	releaseNs := hr.GetReleaseNamespace()
	targetRevision := hr.GetTargetRevision()

	watchpodlog.Info("Performing rollback",
		"release", releaseName, "namespace", releaseNs, "targetRevision", targetRevision)

	r.updateStatusPhase(ctx, hr, helmv1alpha1.PhaseInstalling)

	if err := r.HelmManager.Rollback(releaseName, releaseNs, targetRevision, hr.GetWaitTimeout()); err != nil {
		r.updateStatusFailed(ctx, hr, err.Error())
		return err
	}

	rel, err := r.HelmManager.GetRelease(releaseName, releaseNs)
	if err != nil {
		r.updateStatusFailed(ctx, hr, "failed to get release after rollback: "+err.Error())
		return err
	}

	if err := r.updateStatusSuccess(ctx, hr, rel); err != nil {
		return err
	}

	// Align CR spec with the rolled-back state so the next reconciliation
	// won't upgrade back to the previous chart version.
	updated := hr.DeepCopy()
	updated.Spec.TargetRevision = ""
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		updated.Spec.Chart.Version = rel.Chart.Metadata.Version
	}
	if err := r.Update(ctx, updated); err != nil {
		watchpodlog.ErrorE(err, "Failed to update spec after rollback",
			"release", releaseName)
	}

	return r.managePodMonitor(ctx, hr)
}

func (r *HelmReleaseReconciler) managePodMonitor(ctx context.Context, hr *helmv1alpha1.HelmRelease) error {
	releaseName := hr.GetReleaseName()

	key := types.NamespacedName{Namespace: hr.Namespace, Name: hr.Name}.String()

	if !hr.IsPodMonitorEnabled() {
		r.Watcher.UnregisterRelease(releaseName)
		delete(r.releaseNames, key)
		return nil
	}

	r.Watcher.RegisterRelease(releaseName, &podwatch.ReleaseConfig{
		EventSender: podwatch.NewEventSenderWithConfig(
			hr.GetPodMonitorEndpoint(),
			hr.GetPodMonitorMethod(),
			hr.GetPodMonitorHeaders(),
		),
		StatusUpdater: &crStatusUpdater{
			client:      r.Client,
			crNamespace: hr.Namespace,
			crName:      hr.Name,
		},
	})

	r.releaseNames[key] = releaseName
	return nil
}

func (r *HelmReleaseReconciler) unmanagePodMonitor(releaseName string) {
	r.Watcher.UnregisterRelease(releaseName)
}

func (r *HelmReleaseReconciler) getValues(hr *helmv1alpha1.HelmRelease) (map[string]interface{}, error) {
	if hr.Spec.Values == nil {
		return map[string]interface{}{}, nil
	}

	raw := hr.Spec.Values.Raw
	if len(raw) == 0 {
		return map[string]interface{}{}, nil
	}

	var values map[string]interface{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("failed to unmarshal values: %w", err)
	}

	return values, nil
}

// isRetryable checks whether a Helm operation error is transient (retry) or permanent (give up immediately).
func isRetryable(err error) bool {
	msg := strings.ToLower(err.Error())

	permanent := []string{
		"values parsing failed",
		"failed to load chart",
		"failed to unmarshal",
		"chart not found",
		"no chart version found",
		"failed to render template",
		"imagepullbackoff",
		"errimagepull",
	}
	for _, p := range permanent {
		if strings.Contains(msg, p) {
			return false
		}
	}

	transient := []string{
		"connection refused",
		"timeout",
		"context deadline exceeded",
		"dial tcp",
		"connection reset",
		"too many requests",
		"tls handshake timeout",
	}
	for _, t := range transient {
		if strings.Contains(msg, t) {
			return true
		}
	}

	return true
}

func (r *HelmReleaseReconciler) finalizeHelmRelease(ctx context.Context, hr *helmv1alpha1.HelmRelease) error {
	releaseName := hr.GetReleaseName()
	releaseNs := hr.GetReleaseNamespace()

	key := types.NamespacedName{Namespace: hr.Namespace, Name: hr.Name}.String()
	r.Watcher.UnregisterRelease(releaseName)
	delete(r.releaseNames, key)

	_, err := r.HelmManager.GetRelease(releaseName, releaseNs)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
	}

	if err := r.HelmManager.Uninstall(releaseName, releaseNs); err != nil {
		return fmt.Errorf("failed to uninstall release: %w", err)
	}

	return nil
}

func (r *HelmReleaseReconciler) updateStatusPhase(ctx context.Context, hr *helmv1alpha1.HelmRelease, phase string) {
	updated := hr.DeepCopy()
	updated.Status.Phase = phase
	if err := r.Status().Update(ctx, updated); err != nil {
		watchpodlog.ErrorE(err, "Failed to update status phase", "phase", phase)
	}
}

func (r *HelmReleaseReconciler) updateStatusSuccess(ctx context.Context, hr *helmv1alpha1.HelmRelease, rel interface{}) error {
	updated := hr.DeepCopy()
	updated.Status.Phase = helmv1alpha1.PhaseRunning
	updated.Status.ReleaseName = hr.GetReleaseName()
	updated.Status.ReleaseStatus = "deployed"
	updated.Status.ObservedGeneration = hr.Generation
	updated.Status.LastAttemptedGeneration = hr.Generation
	updated.Status.RetryCount = 0
	updated.Status.LastFailureMessage = ""
	now := metav1.Now()
	updated.Status.LastAppliedTime = &now

	if release, ok := rel.(*helmrelease.Release); ok {
		updated.Status.Revision = release.Version
		updated.Status.ReleaseVersion = release.Version
		updated.Status.LastTargetRevision = fmt.Sprintf("%d", release.Version)
		if release.Info != nil {
			updated.Status.Notes = release.Info.Notes
		}
	}

	if hr.IsPodMonitorEnabled() {
		updated.Status.PodMonitorReady = true
	}

	if err := r.Status().Update(ctx, updated); err != nil {
		return err
	}
	return nil
}

func (r *HelmReleaseReconciler) updateStatusFailed(ctx context.Context, hr *helmv1alpha1.HelmRelease, message string) {
	updated := hr.DeepCopy()
	updated.Status.Phase = helmv1alpha1.PhaseFailed
	updated.Status.LastAttemptedGeneration = hr.Generation
	updated.Status.RetryCount = hr.Status.RetryCount + 1
	updated.Status.LastFailureMessage = message
	if err := r.Status().Update(ctx, updated); err != nil {
		watchpodlog.ErrorE(err, "Failed to update status")
	}
}

func (r *HelmReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&helmv1alpha1.HelmRelease{}).
		Complete(r)
}

// crStatusUpdater implements podwatch.StatusUpdater to write pod status back to the CR
type crStatusUpdater struct {
	client      client.Client
	crNamespace string
	crName      string
}

func (u *crStatusUpdater) UpdatePodStatus(ctx context.Context, releaseName string, podInfo podwatch.PodInfo, eventType podwatch.PodEventType) error {
	hr := &helmv1alpha1.HelmRelease{}
	key := types.NamespacedName{Namespace: u.crNamespace, Name: u.crName}
	if err := u.client.Get(ctx, key, hr); err != nil {
		if errors.IsNotFound(err) {
			watchpodlog.Info("HelmRelease not found, skipping status update",
				"namespace", u.crNamespace, "name", u.crName)
			return nil
		}
		return fmt.Errorf("failed to get HelmRelease: %w", err)
	}

	updated := hr.DeepCopy()

	podStatus := helmv1alpha1.PodRuntimeStatus{
		Namespace: podInfo.Namespace,
		Name:      podInfo.Name,
		UID:       podInfo.UID,
		Phase:     podInfo.Phase,
		NodeName:  podInfo.NodeName,
		PodIP:     podInfo.PodIP,
		Ready:     podInfo.Ready,
		Restart:   podInfo.Restart,
	}

	if eventType == podwatch.PodEventDeleted {
		filtered := make([]helmv1alpha1.PodRuntimeStatus, 0, len(updated.Status.PodStatuses))
		for _, ps := range updated.Status.PodStatuses {
			if ps.Namespace == podInfo.Namespace && ps.Name == podInfo.Name {
				continue
			}
			filtered = append(filtered, ps)
		}
		updated.Status.PodStatuses = filtered
	} else {
		found := false
		for i, ps := range updated.Status.PodStatuses {
			if ps.Namespace == podInfo.Namespace && ps.Name == podInfo.Name {
				updated.Status.PodStatuses[i] = podStatus
				found = true
				break
			}
		}
		if !found {
			updated.Status.PodStatuses = append(updated.Status.PodStatuses, podStatus)
		}
	}

	if err := u.client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("failed to update CR pod statuses: %w", err)
	}

	return nil
}