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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
		return ctrl.Result{RequeueAfter: defaultSyncInterval}, nil
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
	if !forceSync && repo.Status.LastSyncTime != nil {
		since := time.Since(repo.Status.LastSyncTime.Time)
		if since < interval {
			return ctrl.Result{RequeueAfter: interval - since + time.Minute}, nil
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
		return ctrl.Result{RequeueAfter: time.Minute}, nil
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
	if err := r.Status().Update(ctx, &repo); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

type syncResult struct {
	commit   string
	imported []string
}

func (r *ImageRepositoryReconciler) syncRepo(ctx context.Context, repo *kubeworkspacesiov1alpha1.ImageRepository) (*syncResult, error) {
	tmpDir, err := os.MkdirTemp("", "imagerepo-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	cloneOpts := &git.CloneOptions{
		URL:      repo.Spec.URL,
		Progress: nil,
		Depth:    1,
	}
	if repo.Spec.Ref != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(repo.Spec.Ref)
		if strings.Contains(repo.Spec.Ref, "/") || strings.HasPrefix(repo.Spec.Ref, "refs/") {
			cloneOpts.ReferenceName = plumbing.ReferenceName(repo.Spec.Ref)
		}
	}

	var auth transport.AuthMethod
	if repo.Spec.Auth != nil && repo.Spec.Auth.SecretRef != nil {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: repo.Spec.Auth.SecretRef.Name, Namespace: "kube-workspaces-system"}, &secret); err != nil {
			// try default namespace
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
		auth = &http.BasicAuth{Username: username, Password: password}
		cloneOpts.Auth = auth
	}

	repoClone, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts)
	if err != nil {
		// try without branch ref
		cloneOpts.ReferenceName = ""
		cloneOpts.Depth = 1
		repoClone, err = git.PlainCloneContext(ctx, tmpDir, false, cloneOpts)
		if err != nil {
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
		paths = []string{"images/"}
	}

	var imported []string
	desired := map[string]struct{}{}

	for _, scanPath := range paths {
		fullPath := filepath.Join(tmpDir, scanPath)
		_ = filepath.Walk(fullPath, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			// try to parse as unstructured
			var u map[string]interface{}
			if err := yaml.Unmarshal(data, &u); err != nil {
				return nil
			}
			kind, _ := u["kind"].(string)
			apiVersion, _ := u["apiVersion"].(string)
			if kind != "Image" || !strings.Contains(apiVersion, "kubeworkspaces.io") {
				return nil
			}
			metaObj, _ := u["metadata"].(map[string]interface{})
			name, _ := metaObj["name"].(string)
			if name == "" {
				return nil
			}

			// build object
			obj := &unstructured.Unstructured{Object: u}
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
			ann[imageRepositorySourcePath] = strings.TrimPrefix(p, tmpDir+"/")
			obj.SetAnnotations(ann)

			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubeworkspaces.io", Version: "v1alpha1", Kind: "Image"})
			err = r.Get(ctx, types.NamespacedName{Name: name}, existing)
			if apierrors.IsNotFound(err) {
				if err := r.Create(ctx, obj); err != nil {
					return nil
				}
			} else if err == nil {
				obj.SetResourceVersion(existing.GetResourceVersion())
				if err := r.Update(ctx, obj); err != nil {
					return nil
				}
			}
			imported = append(imported, name)
			desired[name] = struct{}{}
			return nil
		})
	}

	// GC
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubeworkspaces.io", Version: "v1alpha1", Kind: "ImageList"})
	if err := r.List(ctx, list, client.MatchingLabels{imageRepositoryLabelKey: repo.Name}); err == nil {
		for i := range list.Items {
			n := list.Items[i].GetName()
			if _, ok := desired[n]; !ok {
				_ = r.Delete(ctx, &list.Items[i])
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

	res := &syncResult{commit: commit, imported: uniq}
	_, _ = json.Marshal(res)
	_ = sha256.Sum256([]byte(commit))
	_ = hex.EncodeToString([]byte{})
	_ = controllerutil.AddFinalizer
	return res, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeworkspacesiov1alpha1.ImageRepository{}).
		Complete(r)
}
