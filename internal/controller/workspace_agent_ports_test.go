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
	"testing"

	corev1 "k8s.io/api/core/v1"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

func testImageWithAgentPort(port int32) *kubeworkspacesiov1alpha1.Image {
	return &kubeworkspacesiov1alpha1.Image{
		Spec: kubeworkspacesiov1alpha1.ImageSpec{
			ProxyConfig: &kubeworkspacesiov1alpha1.ImageProxyConfig{AgentPort: port},
		},
	}
}

func TestAgentPortForImage(t *testing.T) {
	if got := agentPortForImage(nil); got != 0 {
		t.Errorf("nil image must disable the agent plane, got %d", got)
	}
	if got := agentPortForImage(&kubeworkspacesiov1alpha1.Image{}); got != 0 {
		t.Errorf("missing proxyConfig must disable the agent plane, got %d", got)
	}
	for _, port := range []int32{0, -1, 65536, 100000} {
		if got := agentPortForImage(testImageWithAgentPort(port)); got != 0 {
			t.Errorf("out-of-range port %d must disable the agent plane, got %d", port, got)
		}
	}
	for _, port := range []int32{1, 44787, 65535} {
		if got := agentPortForImage(testImageWithAgentPort(port)); got != port {
			t.Errorf("port %d must pass through, got %d", port, got)
		}
	}
}

func servicePortNumbers(svcPorts []corev1.ServicePort) map[int32]bool {
	ports := map[int32]bool{}
	for _, p := range svcPorts {
		ports[p.Port] = true
	}
	return ports
}

func TestGenerateServiceAgentPort(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)

	// Absent by default: no behavior change for existing images.
	plain := generateService(ws, WorkspaceTypeVM, 0)
	if ports := servicePortNumbers(plain.Spec.Ports); ports[44787] {
		t.Errorf("agent port must be absent by default: %v", plain.Spec.Ports)
	}

	// Declared: exposed on its own number alongside the app ports.
	withAgent := generateService(ws, WorkspaceTypeVM, 44787)
	ports := servicePortNumbers(withAgent.Spec.Ports)
	if !ports[44787] {
		t.Fatalf("agent port missing: %v", withAgent.Spec.Ports)
	}
	if !ports[DefaultServingPort] {
		t.Errorf("app port 80 must be preserved: %v", withAgent.Spec.Ports)
	}
	for _, p := range withAgent.Spec.Ports {
		if p.Port == 44787 && (p.Name != "agent" || p.TargetPort.IntValue() != 44787 || p.Protocol != "TCP") {
			t.Errorf("agent ServicePort malformed: %+v", p)
		}
	}

	// No duplicate when a container already declares the port.
	wsDup := testWorkspace(WorkspaceTypeVM, nil)
	wsDup.Spec.Template.Spec.Containers[0].Ports = append(
		wsDup.Spec.Template.Spec.Containers[0].Ports,
		corev1.ContainerPort{ContainerPort: 44787, Name: "agent"},
	)
	deduped := generateService(wsDup, WorkspaceTypeVM, 44787)
	count := 0
	for _, p := range deduped.Spec.Ports {
		if p.Port == 44787 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("agent port must appear exactly once, got %d: %v", count, deduped.Spec.Ports)
	}
}

func masqueradePorts(t *testing.T, podSpec corev1.PodSpec, agentPort int32) []int64 {
	t.Helper()
	interfaces, _ := vmDefaultNetwork("my-ws", podSpec, agentPort)
	if len(interfaces) != 1 {
		t.Fatalf("expected one masquerade interface, got %d", len(interfaces))
	}
	iface, ok := interfaces[0].(map[string]interface{})
	if !ok {
		t.Fatalf("interface has unexpected shape: %T", interfaces[0])
	}
	raw, ok := iface["ports"].([]interface{})
	if !ok {
		t.Fatalf("interface has no ports list: %v", iface)
	}
	var ports = make([]int64, 0, len(raw))
	for _, entry := range raw {
		forward, ok := entry.(map[string]interface{})
		if !ok {
			t.Fatalf("forward has unexpected shape: %T", entry)
		}
		port, ok := forward["port"].(int64)
		if !ok {
			t.Fatalf("forward has no int64 port: %v", forward)
		}
		ports = append(ports, port)
	}
	return ports
}

func TestVMDefaultNetworkAgentPort(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	full := ws.Spec.Template.Spec
	// Absent by default.
	plain := masqueradePorts(t, full, 0)
	for _, p := range plain {
		if p == 44787 {
			t.Fatalf("agent forward must be absent by default: %v", plain)
		}
	}
	// Declared: forwarded once.
	withAgent := masqueradePorts(t, full, 44787)
	found := 0
	for _, p := range withAgent {
		if p == 44787 {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("agent forward must appear exactly once, got %d: %v", found, withAgent)
	}
	// Already declared as a container port: no duplicate forward.
	dup := full.DeepCopy()
	dup.Containers[0].Ports = append(dup.Containers[0].Ports,
		corev1.ContainerPort{ContainerPort: 44787, Name: "agent"})
	again := masqueradePorts(t, *dup, 44787)
	found = 0
	for _, p := range again {
		if p == 44787 {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("declared agent port must not duplicate the forward, got %d: %v", found, again)
	}
}
