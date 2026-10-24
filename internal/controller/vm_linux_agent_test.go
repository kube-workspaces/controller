package controller

import (
	"context"
	"encoding/json"
	"testing"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func TestLinuxAgentBindingIsStableAndCloneSpecific(t *testing.T) {
	img := &workspacev1.Image{Spec: workspacev1.ImageSpec{DefaultCloudInit: true, DefaultUser: "arch", PersistentRootDisk: true,
		RemoteDesktop:   &workspacev1.ImageRemoteDesktop{Protocol: "agent", Port: 44787},
		DefaultUserData: "#cloud-config\npackages: [gnome]\n"}}
	scheme := resetTestScheme(t)
	ws := testWorkspace(WorkspaceTypeVM, nil)
	ws.UID = "00000000-0000-4000-8000-000000000001"
	clone := ws.DeepCopy()
	clone.Name = "clone"
	clone.UID = "00000000-0000-4000-8000-000000000002"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, clone).Build()
	r := &WorkspaceReconciler{Client: cl, Scheme: scheme}
	seen := map[string]bool{}
	for _, guest := range []*workspacev1.Workspace{ws, clone} {
		if err := r.ensureLinuxAgentGeneration(context.Background(), guest, img); err != nil {
			t.Fatal(err)
		}
		generation := guest.Annotations[annotationAgentGeneration]
		if generation == "" || seen[generation] {
			t.Fatal("missing or shared agent root generation")
		}
		seen[generation] = true
		if err := r.ensureLinuxAgentGeneration(context.Background(), guest, img); err != nil {
			t.Fatal(err)
		}
		if guest.Annotations[annotationAgentGeneration] != generation {
			t.Fatal("ordinary reconcile rotated the binding")
		}
		data, err := vmCloudInitUserData(guest, img, []string{"ssh-ed25519 test"})
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Packages []string `json:"packages"`
			Files    []struct {
				Path        string `json:"path"`
				Permissions string `json:"permissions"`
				Content     string `json:"content"`
			} `json:"write_files"`
		}
		if err := yaml.Unmarshal([]byte(data), &config); err != nil {
			t.Fatal(err)
		}
		if len(config.Packages) != 1 || config.Packages[0] != "gnome" || len(config.Files) != 1 || config.Files[0].Permissions != "0444" {
			t.Fatalf("bootstrap config lost: %s", data)
		}
		binding := map[string]string{}
		if err := json.Unmarshal([]byte(config.Files[0].Content), &binding); err != nil {
			t.Fatal(err)
		}
		if binding["workspaceUid"] != string(guest.UID) || binding["workspaceGeneration"] != generation || len(binding) != 2 {
			t.Fatal("incorrect public binding")
		}
	}
	ws.Annotations[AnnotationReset] = "requested"
	if err := cl.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if _, handled, err := r.handleReset(context.Background(), ws); err != nil || !handled {
		t.Fatalf("reset: %v", err)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, ws); err != nil {
		t.Fatal(err)
	}
	if ws.Annotations[annotationAgentGeneration] != "" {
		t.Fatal("reset retained previous agent generation")
	}
	if err := r.ensureLinuxAgentGeneration(context.Background(), ws, img); err != nil {
		t.Fatal(err)
	}
	if seen[ws.Annotations[annotationAgentGeneration]] {
		t.Fatal("reset reused an old agent generation")
	}
}
