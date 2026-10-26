package controller

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("ImageRepository schema validation", func() {
	It("imports valid manifests, reports invalid ones and withholds pruning until repaired", func() {
		root, err := os.MkdirTemp("", "image-repository-validation-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(os.RemoveAll(root)).To(Succeed()) })
		Expect(os.Mkdir(filepath.Join(root, "images"), 0o755)).To(Succeed())
		valid := strings.ReplaceAll(repositoryImageManifest, "name: code", "name: repository-valid")
		invalid := strings.ReplaceAll(valid, "name: repository-valid", "name: repository-invalid")
		invalid = strings.ReplaceAll(invalid, "defaultPort: 8080", "defaultPort: not-an-integer")
		Expect(os.WriteFile(filepath.Join(root, "images", "valid.yaml"), []byte(valid), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "images", "invalid.yaml"), []byte(invalid), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "images", "malformed.yaml"), []byte("spec: ["), 0o600)).To(Succeed())
		repository, err := git.PlainInit(root, false)
		Expect(err).NotTo(HaveOccurred())
		worktree, err := repository.Worktree()
		Expect(err).NotTo(HaveOccurred())
		commit := func() {
			Expect(worktree.AddGlob("images/*")).To(Succeed())
			_, err := worktree.Commit("catalog update", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}})
			Expect(err).NotTo(HaveOccurred())
		}
		commit()
		repo := &workspacev1.ImageRepository{ObjectMeta: metav1.ObjectMeta{Name: "validation-catalog"}, Spec: workspacev1.ImageRepositorySpec{URL: root}}
		Expect(k8sClient.Create(ctx, repo)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repo)).To(Succeed()) })
		stale := &workspacev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "repository-stale", Labels: map[string]string{imageRepositoryLabelKey: repo.Name}}, Spec: workspacev1.ImageSpec{Image: "example.com/stale", DefaultPort: 8080}}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())
		r := &ImageRepositoryReconciler{Client: k8sClient}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(repo)}
		result, err := r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">=", defaultSyncInterval))
		Expect(result.RequeueAfter).To(BeNumerically("<", defaultSyncInterval*12/10))
		Expect(k8sClient.Get(ctx, request.NamespacedName, repo)).To(Succeed())
		Expect(repo.Status.ImportedImages).To(ConsistOf("repository-valid"))
		Expect(repo.Status.Conditions[0].Reason).To(Equal("InvalidManifests"))
		Expect(repo.Status.Conditions[0].Message).To(ContainSubstring("2 invalid manifest(s)"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), stale)).To(Succeed())
		image := &workspacev1.Image{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "repository-valid"}, image)).To(Succeed())
		validImage := image.DeepCopy()
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, validImage)).To(Succeed()) })
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "repository-invalid"}, &workspacev1.Image{})).NotTo(Succeed())

		Expect(os.WriteFile(filepath.Join(root, "images", "invalid.yaml"), []byte(strings.ReplaceAll(invalid, "not-an-integer", "8080")), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "images", "malformed.yaml"), []byte("kind: ConfigMap\n"), 0o600)).To(Succeed())
		commit()
		_, err = r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, request.NamespacedName, repo)).To(Succeed())
		Expect(repo.Status.Conditions[0].Reason).To(Equal("Synced"))
		Expect(repo.Status.ImportedImages).To(ConsistOf("repository-valid", "repository-invalid"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), stale)).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "repository-invalid"}, image)).To(Succeed())
		Expect(k8sClient.Delete(ctx, image)).To(Succeed())
	})
})
