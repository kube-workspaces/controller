package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestImageRepositoryFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"images/base.yaml", "images/other.yml", "images/README.md", "images/kasm/core/core.yaml", "images/kasm/desktop/app.yaml", "images/kasm/desktop/nested/ignored.yaml"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("kind: Image\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"direct files only", []string{"images"}, []string{"images/base.yaml", "images/other.yml"}},
		{"optional trailing slash", []string{"images/"}, []string{"images/base.yaml", "images/other.yml"}},
		{"explicit subfolders", []string{"images", "images/kasm/core/", "images/kasm/desktop"}, []string{"images/base.yaml", "images/other.yml", "images/kasm/core/core.yaml", "images/kasm/desktop/app.yaml"}},
		{"duplicate paths", []string{"images", "images/"}, []string{"images/base.yaml", "images/other.yml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := imageRepositoryFiles(root, test.paths)
			if err != nil {
				t.Fatal(err)
			}
			for i := range got {
				got[i], err = filepath.Rel(root, got[i])
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
	t.Run("missing folder fails rather than returning a partial inventory", func(t *testing.T) {
		files, err := imageRepositoryFiles(root, []string{"images", "does-not-exist"})
		if err == nil || files != nil {
			t.Fatalf("got files=%v err=%v; expected an error without partial inventory", files, err)
		}
	})
}

const repositoryImageManifest = `apiVersion: kubeworkspaces.io/v1alpha1
kind: Image
metadata:
  name: code
  labels:
    catalog-label: from-repository
  annotations:
    catalog-annotation: from-repository
spec:
  displayName: Repository Code
  image: example.com/code:new
  defaultPort: 8080
`

func repositoryFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "images", "code.yaml")
	if err := os.WriteFile(path, []byte(repositoryImageManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestImageRepositoryAdoptsExistingImages(t *testing.T) {
	for _, source := range []string{"new", "manual", "helm", "other-repository"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			root, path := repositoryFixture(t)
			repo := &workspacev1.ImageRepository{ObjectMeta: metav1.ObjectMeta{Name: "catalog"}, Spec: workspacev1.ImageRepositorySpec{URL: "https://example.com/catalog.git", Ref: "main"}}
			builder := fake.NewClientBuilder().WithScheme(resetTestScheme(t))
			if source != "new" {
				existing := &workspacev1.Image{ObjectMeta: metav1.ObjectMeta{
					Name: "code", UID: "original-image-uid", Finalizers: []string{"example.com/retain"},
					Labels: map[string]string{"installation": source}, Annotations: map[string]string{"example.com/note": "keep"},
				}, Spec: workspacev1.ImageSpec{Image: "example.com/code:old", DefaultPort: 9000, Description: "old spec field must be removed"}}
				if source == "helm" {
					existing.Labels["app.kubernetes.io/managed-by"] = "Helm"
					existing.Annotations["meta.helm.sh/release-name"] = "kube-workspaces"
				}
				if source == "other-repository" {
					existing.Labels[imageRepositoryLabelKey] = "previous-source"
					existing.Annotations[imageRepositorySourceURL] = "https://example.com/old.git"
				}
				builder.WithObjects(existing)
			}
			r := &ImageRepositoryReconciler{Client: builder.Build()}
			if name, err := r.syncImageFile(ctx, repo, "commit-123", root, path); err != nil || name != "code" {
				t.Fatalf("sync: name=%q err=%v", name, err)
			}
			var got workspacev1.Image
			if err := r.Get(ctx, client.ObjectKey{Name: "code"}, &got); err != nil {
				t.Fatal(err)
			}
			if got.Spec.Image != "example.com/code:new" || got.Spec.DefaultPort != 8080 || got.Spec.Description != "" {
				t.Fatalf("repository must replace the entire spec: %+v", got.Spec)
			}
			if got.Labels[imageRepositoryLabelKey] != repo.Name || got.Labels[imageRepositoryManagedBy] != imageRepositoryManagedVal || got.Labels["catalog-label"] != "from-repository" {
				t.Fatalf("missing repository ownership: %v", got.Labels)
			}
			for key, want := range map[string]string{imageRepositorySourceURL: repo.Spec.URL, imageRepositorySourceRef: "main", imageRepositorySourceSHA: "commit-123", imageRepositorySourcePath: "images/code.yaml", "catalog-annotation": "from-repository"} {
				if got.Annotations[key] != want {
					t.Errorf("annotation %s = %q, want %q", key, got.Annotations[key], want)
				}
			}
			if source != "new" && (got.UID != "original-image-uid" || !reflect.DeepEqual(got.Finalizers, []string{"example.com/retain"}) || got.Labels["installation"] != source || got.Annotations["example.com/note"] != "keep") {
				t.Fatalf("adoption discarded live identity/metadata: %+v", got.ObjectMeta)
			}
			if source == "helm" && got.Annotations["meta.helm.sh/release-name"] != "kube-workspaces" {
				t.Fatal("adoption discarded Helm release metadata")
			}
			version := got.ResourceVersion
			if _, err := r.syncImageFile(ctx, repo, "commit-123", root, path); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKey{Name: "code"}, &got); err != nil {
				t.Fatal(err)
			}
			if got.ResourceVersion != version {
				t.Fatal("unchanged sync should not write the image again")
			}
		})
	}
}

func TestImageRepositoryAdoptionRetriesConflicts(t *testing.T) {
	root, path := repositoryFixture(t)
	repo := &workspacev1.ImageRepository{ObjectMeta: metav1.ObjectMeta{Name: "catalog"}}
	existing := &workspacev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "code"}}
	attempts := 0
	cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithObjects(existing).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			attempts++
			if attempts == 1 {
				return apierrors.NewConflict(schema.GroupResource{Group: "kubeworkspaces.io", Resource: "images"}, obj.GetName(), fmt.Errorf("concurrent edit"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
	r := &ImageRepositoryReconciler{Client: cl}
	if _, err := r.syncImageFile(context.Background(), repo, "commit", root, path); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("got %d update attempts, want 2", attempts)
	}
}

func localGitImageRepository(t *testing.T) *workspacev1.ImageRepository {
	t.Helper()
	root, _ := repositoryFixture(t)
	repository, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("images/code.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("image catalog", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	return &workspacev1.ImageRepository{ObjectMeta: metav1.ObjectMeta{Name: "catalog", Generation: 2}, Spec: workspacev1.ImageRepositorySpec{URL: root}}
}

func TestImageRepositoryReconcileBackfillsProvenance(t *testing.T) {
	for _, reason := range []string{"new-install", "manual-sync", "spec-changed", "failed-sync", "periodic-sync"} {
		t.Run(reason, func(t *testing.T) {
			repo := localGitImageRepository(t)
			if reason != "new-install" {
				repo.Status = workspacev1.ImageRepositoryStatus{ObservedGeneration: repo.Generation, LastSyncTime: &metav1.Time{Time: time.Now()}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced", LastTransitionTime: metav1.Now()}}}
			}
			switch reason {
			case "manual-sync":
				repo.Annotations = map[string]string{imageRepositorySyncAnno: "now"}
			case "spec-changed":
				repo.Status.ObservedGeneration--
			case "failed-sync":
				repo.Status.Conditions[0].Status = metav1.ConditionFalse
			case "periodic-sync":
				repo.Status.LastSyncTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
			}
			var objects []client.Object
			objects = append(objects, repo)
			if reason != "new-install" {
				objects = append(objects, &workspacev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "code"}, Spec: workspacev1.ImageSpec{Image: "example.com/old"}})
			}
			cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithStatusSubresource(repo).WithObjects(objects...).Build()
			r := &ImageRepositoryReconciler{Client: cl}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(repo)}); err != nil {
				t.Fatal(err)
			}
			var image workspacev1.Image
			if err := cl.Get(context.Background(), client.ObjectKey{Name: "code"}, &image); err != nil {
				t.Fatal(err)
			}
			if image.Labels[imageRepositoryLabelKey] != repo.Name || image.Annotations[imageRepositorySourceSHA] == "" {
				t.Fatalf("missing backfilled provenance: %+v", image.ObjectMeta)
			}
			var synced workspacev1.ImageRepository
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(repo), &synced); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(synced.Status.ImportedImages, []string{"code"}) || synced.Status.ObservedGeneration != repo.Generation || synced.Status.LastSyncCommit == "" {
				t.Fatalf("sync status not recorded: %+v", synced.Status)
			}
			if _, present := synced.Annotations[imageRepositorySyncAnno]; present {
				t.Fatal("manual trigger was not cleared")
			}
		})
	}
}

func TestImageRepositoryPrunesOnlyOwnedImages(t *testing.T) {
	repo := localGitImageRepository(t)
	cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithObjects(
		&workspacev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "removed", Labels: map[string]string{imageRepositoryLabelKey: repo.Name}}},
		&workspacev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "transferred", Labels: map[string]string{imageRepositoryLabelKey: "new-owner"}}},
		&workspacev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "manual"}},
	).Build()
	r := &ImageRepositoryReconciler{Client: cl}
	if _, err := r.syncRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"transferred", "manual", "code"} {
		if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, &workspacev1.Image{}); err != nil {
			t.Fatalf("image %s must remain: %v", name, err)
		}
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "removed"}, &workspacev1.Image{}); !apierrors.IsNotFound(err) {
		t.Fatalf("removed image was not pruned: %v", err)
	}
}
