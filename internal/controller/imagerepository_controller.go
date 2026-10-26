/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

const (
	imageRepositoryLabelKey   = "kubeworkspaces.io/image-repository"
	imageRepositoryManagedBy  = "kubeworkspaces.io/managed-by"
	imageRepositoryManagedVal = "imagerepository-controller"
	imageRepositorySyncAnno   = "kubeworkspaces.io/sync"
	imageRepositorySourceURL  = "kubeworkspaces.io/source-url"
	imageRepositorySourceRef  = "kubeworkspaces.io/source-ref"
	imageRepositorySourceSHA  = "kubeworkspaces.io/source-commit"
	imageRepositorySourcePath = "kubeworkspaces.io/source-path"
	defaultSyncInterval       = 10 * time.Minute
)

// ImageRepositoryReconciler reconciles a ImageRepository object
type ImageRepositoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=imagerepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=imagerepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=imagerepositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=images,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *ImageRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var repo kubeworkspacesiov1alpha1.ImageRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if repo.Spec.Suspend {
		meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "Suspended",
			Message:            "Syncing is suspended",
			LastTransitionTime: metav1.Now(),
		})
		_ = r.Status().Update(ctx, &repo)
		return ctrl.Result{}, nil
	}

	// Manual sync trigger via annotation
	forceSync := false
	if repo.Annotations != nil {
		if _, ok := repo.Annotations[imageRepositorySyncAnno]; ok {
			forceSync = true
		}
	}

	interval := defaultSyncInterval
	if repo.Spec.SyncInterval != nil && repo.Spec.SyncInterval.Duration > 0 {
		interval = repo.Spec.SyncInterval.Duration
	}

	// If we synced recently and not forced, requeue
	if !forceSync && repo.Status.ObservedGeneration == repo.Generation && repo.Status.LastSyncTime != nil {
		since := time.Since(repo.Status.LastSyncTime.Time)
		if since < interval && meta.IsStatusConditionTrue(repo.Status.Conditions, "Ready") {
			return ctrl.Result{RequeueAfter: interval - since}, nil
		}
	}

	result, err := r.syncRepo(ctx, &repo)
	if err != nil {
		meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "SyncFailed",
			Message:            err.Error(),
			LastTransitionTime: metav1.Now(),
		})
		repo.Status.ObservedGeneration = repo.Generation
		_ = r.Status().Update(ctx, &repo)
		// Returning the error uses controller-runtime's exponential retry backoff.
		return ctrl.Result{}, err
	}

	// Clear sync annotation if present
	if repo.Annotations != nil {
		if _, ok := repo.Annotations[imageRepositorySyncAnno]; ok {
			delete(repo.Annotations, imageRepositorySyncAnno)
			if err := r.Update(ctx, &repo); err != nil {
				logger.Error(err, "failed to clear sync annotation")
			}
		}
	}

	repo.Status.LastSyncTime = &metav1.Time{Time: time.Now()}
	repo.Status.LastSyncCommit = result.commit
	repo.Status.ImportedImages = result.imported
	repo.Status.ObservedGeneration = repo.Generation
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Synced",
		Message:            fmt.Sprintf("Synced %d image(s)", len(result.imported)),
		LastTransitionTime: metav1.Now(),
	})
	if len(result.invalid) > 0 {
		// Keep diagnostics bounded. Raw API errors may contain manifest values.
		meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "InvalidManifests",
			Message:            fmt.Sprintf("Skipped %d invalid manifest(s); pruning withheld. First invalid file: %.200s", len(result.invalid), result.invalid[0]),
			LastTransitionTime: metav1.Now(),
		})
	}
	if err := r.Status().Update(ctx, &repo); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: wait.Jitter(interval, 0.2)}, nil
}

type syncResult struct {
	commit   string
	imported []string
	invalid  []string
}

// invalidImageManifest distinguishes malformed source files from cluster failures.
type invalidImageManifest struct{ err error }

func (e *invalidImageManifest) Error() string { return e.err.Error() }
func (e *invalidImageManifest) Unwrap() error { return e.err }

func (r *ImageRepositoryReconciler) syncRepo(ctx context.Context, repo *kubeworkspacesiov1alpha1.ImageRepository) (*syncResult, error) {
	tmpDir, err := os.MkdirTemp("", "imagerepo-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			log.FromContext(ctx).Error(err, "failed to remove temporary image repository")
		}
	}()

	cloneOpts := &git.CloneOptions{
		URL:      repo.Spec.URL,
		Progress: nil,
		Depth:    1,
	}
	if repo.Spec.Ref != "" {
		// Fetch history/refs when a branch, tag or commit is explicitly requested.
		// Never silently sync the default branch when a requested ref is absent.
		cloneOpts.Depth = 0
	}

	cloneOpts.Auth, err = r.imageRepositoryAuth(ctx, repo)
	if err != nil {
		return nil, err
	}

	repoClone, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts)
	if err != nil {
		// Git transport errors can include credentials from the remote URL.
		return nil, fmt.Errorf("failed to clone repository; check URL and credentials")
	}
	if repo.Spec.Ref != "" {
		if err := checkoutImageRepositoryRef(repoClone, repo.Spec.Ref); err != nil {
			return nil, err
		}
	}

	head, err := repoClone.Head()
	if err != nil {
		return nil, err
	}
	commit := head.Hash().String()

	paths := repo.Spec.Paths
	if len(paths) == 0 {
		paths = []string{"images"}
	}
	files, err := imageRepositoryFiles(tmpDir, paths)
	if err != nil {
		return nil, err
	}

	var imported []string
	var invalid []string
	desired := map[string]struct{}{}

	for _, p := range files {
		name, err := r.syncImageFile(ctx, repo, commit, tmpDir, p)
		if err != nil {
			var manifestErr *invalidImageManifest
			if errors.As(err, &manifestErr) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
				invalid = append(invalid, strings.TrimPrefix(p, tmpDir+"/"))
				continue
			}
			return nil, fmt.Errorf("sync %s: %w", strings.TrimPrefix(p, tmpDir+"/"), err)
		}
		if name != "" {
			imported = append(imported, name)
			desired[name] = struct{}{}
		}
	}

	// Prune only images currently assigned to this repository. Adoption by a
	// different repository changes this label, so the former source cannot prune it.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubeworkspaces.io", Version: "v1alpha1", Kind: "ImageList"})
	if err := r.List(ctx, list, client.MatchingLabels{imageRepositoryLabelKey: repo.Name}); err != nil {
		return nil, fmt.Errorf("list images for pruning: %w", err)
	}
	for i := range list.Items {
		if len(invalid) > 0 {
			break // An incomplete inventory must never remove existing images.
		}
		image := &list.Items[i]
		if _, ok := desired[image.GetName()]; !ok {
			// A concurrent adoption/update must not be deleted using a stale list.
			uid, version := image.GetUID(), image.GetResourceVersion()
			if err := r.Delete(ctx, image, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("prune image %s: %w", image.GetName(), err)
			}
		}
	}

	// deduplicate
	seen := map[string]bool{}
	var uniq []string
	for _, v := range imported {
		if !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}

	return &syncResult{commit: commit, imported: uniq, invalid: invalid}, nil
}

func (r *ImageRepositoryReconciler) imageRepositoryAuth(ctx context.Context, repo *kubeworkspacesiov1alpha1.ImageRepository) (transport.AuthMethod, error) {
	if repo.Spec.Auth == nil {
		return nil, nil
	}
	if authType := repo.Spec.Auth.Type; authType != "" && authType != "basic" && authType != "https-basic" {
		return nil, fmt.Errorf("unsupported authentication type; use HTTPS basic authentication")
	}
	if repo.Spec.Auth.SecretRef == nil {
		return nil, nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: repo.Spec.Auth.SecretRef.Name, Namespace: "kube-workspaces-system"}, &secret); err != nil {
		// Preserve the historical default-namespace fallback.
		if err2 := r.Get(ctx, types.NamespacedName{Name: repo.Spec.Auth.SecretRef.Name, Namespace: "default"}, &secret); err2 != nil {
			return nil, fmt.Errorf("failed to get auth secret: %w", err)
		}
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if password == "" {
		password = string(secret.Data["token"])
	}
	if username == "" {
		username = "git"
	}
	return &http.BasicAuth{Username: username, Password: password}, nil
}

func checkoutImageRepositoryRef(repository *git.Repository, ref string) error {
	hash, err := repository.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		// Non-default branches of a clone live under refs/remotes/origin.
		hash, err = repository.ResolveRevision(plumbing.Revision("refs/remotes/origin/" + ref))
	}
	if err != nil {
		return fmt.Errorf("requested git ref could not be resolved")
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("failed to open repository worktree")
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Hash: *hash}); err != nil {
		return fmt.Errorf("failed to check out requested git ref")
	}
	return nil
}

// syncImageFile imports one manifest and returns its name, or an empty name for
// documents that are not named Images.
func (r *ImageRepositoryReconciler) syncImageFile(ctx context.Context, repo *kubeworkspacesiov1alpha1.ImageRepository, commit, root, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return "", &invalidImageManifest{err: err}
	}
	// Use Kubernetes' JSON decoder to preserve integer fields. Generic YAML/JSON
	// decoding turns numbers into float64 and makes unchanged specs look modified.
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(jsonData); err != nil {
		return "", &invalidImageManifest{err: err}
	}
	if obj.GetKind() != "Image" {
		return "", nil
	}
	if obj.GetAPIVersion() != "kubeworkspaces.io/v1alpha1" {
		return "", &invalidImageManifest{err: fmt.Errorf("unsupported Image apiVersion")}
	}
	name := obj.GetName()
	if name == "" {
		return "", &invalidImageManifest{err: fmt.Errorf("image name is required")}
	}
	if _, ok := obj.Object["spec"].(map[string]interface{}); !ok {
		return "", &invalidImageManifest{err: fmt.Errorf("image %s is missing a spec object", name)}
	}
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[imageRepositoryLabelKey] = repo.Name
	labels[imageRepositoryManagedBy] = imageRepositoryManagedVal
	obj.SetLabels(labels)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[imageRepositorySourceURL] = repo.Spec.URL
	ann[imageRepositorySourceRef] = repo.Spec.Ref
	ann[imageRepositorySourceSHA] = commit
	ann[imageRepositorySourcePath] = strings.TrimPrefix(path, root+"/")
	obj.SetAnnotations(ann)

	err = retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}, func() error {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GroupVersionKind())
		if err := r.Get(ctx, types.NamespacedName{Name: name}, existing); err != nil {
			if apierrors.IsNotFound(err) {
				return r.Create(ctx, obj.DeepCopy())
			}
			return err
		}

		// The repository is authoritative for the complete spec, including when
		// adopting a preinstalled/manual Image. Keep the live object's identity,
		// finalizers and unrelated metadata instead of replacing its ObjectMeta.
		updated := existing.DeepCopy()
		updated.Object["spec"] = runtime.DeepCopyJSONValue(obj.Object["spec"])
		mergedLabels := updated.GetLabels()
		if mergedLabels == nil {
			mergedLabels = map[string]string{}
		}
		for key, value := range obj.GetLabels() {
			mergedLabels[key] = value
		}
		updated.SetLabels(mergedLabels)
		mergedAnnotations := updated.GetAnnotations()
		if mergedAnnotations == nil {
			mergedAnnotations = map[string]string{}
		}
		for key, value := range obj.GetAnnotations() {
			mergedAnnotations[key] = value
		}
		updated.SetAnnotations(mergedAnnotations)
		if equality.Semantic.DeepEqual(existing.Object, updated.Object) {
			return nil
		}
		return r.Update(ctx, updated)
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

// imageRepositoryFiles lists only direct YAML children of each configured
// directory. ReadDir accepts paths with or without a trailing slash. Resolve all
// directories before importing so a missing path fails the sync without pruning.
func imageRepositoryFiles(root string, paths []string) ([]string, error) {
	var files []string
	seen := map[string]bool{}
	for _, scanPath := range paths {
		scanPath = strings.TrimSpace(scanPath)
		if scanPath == "" || filepath.IsAbs(scanPath) {
			return nil, fmt.Errorf("image directory must be relative to the repository")
		}
		fullPath, err := containedImageRepositoryPath(root, filepath.Join(root, scanPath))
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			return nil, fmt.Errorf("read image directory %q: %w", scanPath, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
				continue
			}
			path := filepath.Join(fullPath, entry.Name())
			if _, err := containedImageRepositoryPath(root, path); err != nil {
				return nil, err
			}
			if !seen[path] {
				files = append(files, path)
				seen[path] = true
			}
		}
	}
	return files, nil
}

func containedImageRepositoryPath(root, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve image repository path: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image path escapes the repository")
	}
	return path, nil
}

func imageRepositoryPredicate() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			(e.ObjectNew.GetAnnotations()[imageRepositorySyncAnno] != "" &&
				e.ObjectOld.GetAnnotations()[imageRepositorySyncAnno] != e.ObjectNew.GetAnnotations()[imageRepositorySyncAnno])
	}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeworkspacesiov1alpha1.ImageRepository{}).
		WithEventFilter(imageRepositoryPredicate()).
		Complete(r)
}
