package controller

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

// URL roots use the same image reference as catalog lookup and workspace
// cloning. Only persistent, VM-only catalog entries can opt into CDI HTTP.
func validateVMRootImage(ws *workspacev1.Workspace, img *workspacev1.Image) error {
	if windowsWorkspace(ws) || len(ws.Spec.Template.Spec.Containers) == 0 {
		return nil
	}
	ref := ws.Spec.Template.Spec.Containers[0].Image
	if !strings.Contains(ref, "://") {
		return nil
	}
	u, err := url.Parse(ref)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.IndexFunc(ref, unicode.IsSpace) >= 0 {
		return fmt.Errorf("VM cloud image must be an absolute HTTP(S) URL without credentials, whitespace or a fragment")
	}
	if img == nil || !img.Spec.PersistentRootDisk || img.Spec.VMProfile != "" || len(img.Spec.WorkspaceTypes) != 1 || img.Spec.WorkspaceTypes[0] != WorkspaceTypeVM {
		return fmt.Errorf("HTTP(S) VM roots require a VM-only Image with persistentRootDisk enabled")
	}
	return nil
}

func vmRootDiskSource(image string) map[string]interface{} {
	lower := strings.ToLower(image)
	if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") {
		return map[string]interface{}{"http": map[string]interface{}{"url": image}}
	}
	return map[string]interface{}{"registry": map[string]interface{}{"url": "docker://" + image}}
}
