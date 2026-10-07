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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func windowsAgentTestImage(digest string, port int32) *kubeworkspacesiov1alpha1.Image {
	img := testImageWithAgentPort(port)
	img.ObjectMeta = metav1.ObjectMeta{Name: "windows11-pro-private"}
	img.Spec.Image = digest
	return img
}

func windowsMasqueradePorts(t *testing.T, vm *unstructured.Unstructured) []int64 {
	t.Helper()
	ifaces, _, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	if err != nil || len(ifaces) != 1 {
		t.Fatalf("expected one windows interface, got %v: %v", ifaces, err)
	}
	iface, ok := ifaces[0].(map[string]interface{})
	if !ok {
		t.Fatalf("interface has unexpected shape: %T", ifaces[0])
	}
	raw, _, _ := unstructured.NestedSlice(iface, "ports")
	ports := make([]int64, 0, len(raw))
	for _, entry := range raw {
		forward, ok := entry.(map[string]interface{})
		if !ok {
			t.Fatalf("forward has unexpected shape: %T", entry)
		}
		port, ok := forward["port"].(int64)
		if !ok {
			t.Fatalf("forward has no int64 port: %v", forward)
		}
		if proto, _, _ := unstructured.NestedString(forward, "protocol"); proto != "TCP" {
			t.Fatalf("forward must be TCP, got %q", proto)
		}
		ports = append(ports, port)
	}
	return ports
}

// Regression: Windows vmProfile workspaces skip lookupVMImage, but the agent
// port must still resolve from a digest-pinned Image CR (live: windows11-pro
// reconciled with agentPort 0 despite the patched Image).
func TestAgentImageForWorkspaceResolvesVmProfileDigest(t *testing.T) {
	ws := windowsTestWorkspace()
	digest := ws.Spec.Template.Spec.Containers[0].Image
	cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).
		WithObjects(windowsAgentTestImage(digest, 44787)).Build()
	found := agentImageForWorkspace(context.Background(), cl, ws)
	if found == nil {
		t.Fatal("vmProfile workspace must resolve its digest-pinned Image for the agent port")
	}
	if got := agentPortForImage(found); got != 44787 {
		t.Fatalf("resolved Image must carry agentPort 44787, got %d", got)
	}
}

func TestAgentImageForWorkspaceNilCases(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).Build()
	// No Image CRs at all: plane disabled, never an error.
	if found := agentImageForWorkspace(context.Background(), cl, windowsTestWorkspace()); found != nil {
		t.Fatalf("absent Image must disable the agent plane, got %v", found.Name)
	}
	// No containers: nothing to match on.
	ws := testWorkspace(WorkspaceTypeVM, nil)
	ws.Spec.Template.Spec.Containers = nil
	if found := agentImageForWorkspace(context.Background(), cl, ws); found != nil {
		t.Fatalf("imageless workspace must disable the agent plane, got %v", found.Name)
	}
}

func TestWindowsVirtualMachineAgentPort(t *testing.T) {
	ws := windowsTestWorkspace()
	// Absent by default: no behavior change for existing Windows guests.
	plain := windowsMasqueradePorts(t, generateWindowsVirtualMachine(ws, 0))
	for _, p := range plain {
		if p == 44787 {
			t.Fatalf("agent forward must be absent by default: %v", plain)
		}
	}
	// Declared: forwarded once over TCP on the masquerade interface.
	withAgent := windowsMasqueradePorts(t, generateWindowsVirtualMachine(ws, 44787))
	found := 0
	for _, p := range withAgent {
		if p == 44787 {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("agent forward must appear exactly once, got %d: %v", found, withAgent)
	}
	// Threaded through the shared generator with a nil provisioning image,
	// exactly the live Windows shape (lookupVMImage returns nil there).
	threaded := windowsMasqueradePorts(t, generateVirtualMachine(ws, nil, nil, 44787))
	found = 0
	for _, p := range threaded {
		if p == 44787 {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("generateVirtualMachine must forward the explicit agent port for Windows, got %d: %v", found, threaded)
	}
}
