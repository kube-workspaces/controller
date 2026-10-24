package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/yaml"
)

const annotationAgentGeneration = "kubeworkspaces.io/agent-generation"
const remoteDesktopProtocolAgent = "agent"

func linuxAgentImage(img *workspacev1.Image) bool {
	return img != nil && img.Spec.VMProfile == "" && img.Spec.RemoteDesktop != nil && img.Spec.RemoteDesktop.Protocol == remoteDesktopProtocolAgent
}

// A root incarnation has one stable binding. Reset removes this generation;
// clones receive their own Workspace UID and a fresh generation.
func (r *WorkspaceReconciler) ensureLinuxAgentGeneration(ctx context.Context, ws *workspacev1.Workspace, img *workspacev1.Image) error {
	if !linuxAgentImage(img) {
		return nil
	}
	if !img.Spec.DefaultCloudInit || !img.Spec.PersistentRootDisk || img.Spec.DefaultUser == "" {
		return fmt.Errorf("linux agent images require cloud-init, a persistent root and defaultUser")
	}
	if ws.Annotations[annotationAgentGeneration] != "" {
		return nil
	}
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[annotationAgentGeneration] = string(uuid.NewUUID())
	return r.Update(ctx, ws)
}

func linuxAgentCloudInit(ws *workspacev1.Workspace, img *workspacev1.Image, data string) (string, error) {
	if !linuxAgentImage(img) {
		return data, nil
	}
	if ws.UID == "" || ws.Annotations[annotationAgentGeneration] == "" {
		return "", fmt.Errorf("linux agent workspace binding has not been initialized")
	}
	body := map[string]interface{}{}
	if !strings.HasPrefix(strings.TrimSpace(data), "#cloud-config") {
		return "", fmt.Errorf("linux agent requires cloud-config user-data")
	}
	if err := yaml.Unmarshal([]byte(data), &body); err != nil {
		return "", fmt.Errorf("linux agent requires cloud-config user-data: %w", err)
	}
	if body == nil {
		body = map[string]interface{}{}
	}
	binding, err := json.Marshal(map[string]string{"workspaceUid": string(ws.UID), "workspaceGeneration": ws.Annotations[annotationAgentGeneration]})
	if err != nil {
		return "", err
	}
	files, _ := body["write_files"].([]interface{})
	const bindingPath = "/etc/workspace-agent/public/binding.json"
	for _, file := range files {
		if entry, ok := file.(map[string]interface{}); ok && entry["path"] == bindingPath {
			return "", fmt.Errorf("linux agent binding path is reserved for the controller")
		}
	}
	body["write_files"] = append(files, map[string]interface{}{"path": bindingPath, "owner": "root:root", "permissions": "0444", "content": string(binding)})
	encoded, err := yaml.Marshal(body)
	return "#cloud-config\n" + string(encoded), err
}
