package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf16"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// WindowsGuestAgent checks native setup and scrubs guest answer-file caches.
// No credentials are passed to exec or returned by this contract.
type WindowsGuestAgent interface {
	CompleteBootstrap(context.Context, string, string, string, string) (bool, error)
	// StageEnrollment writes one generation's enrollment token into the
	// guest agent data dir. The token authenticates nothing by itself;
	// the guest validates its binding/expiry and consumes it single-use.
	StageEnrollment(ctx context.Context, namespace, pod, vm string, token []byte) error
}

type windowsGuestAgent struct {
	config *rest.Config
	client kubernetes.Interface
}

func newWindowsGuestAgent(config *rest.Config) (WindowsGuestAgent, error) {
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &windowsGuestAgent{config: rest.CopyConfig(config), client: client}, nil
}

type boundedAgentOutput struct{ bytes.Buffer }

func (b *boundedAgentOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 64*1024 {
		return 0, fmt.Errorf("guest agent response exceeds its limit")
	}
	return b.Buffer.Write(data)
}

func (a *windowsGuestAgent) rpc(ctx context.Context, namespace, pod, domain, operation string, arguments map[string]interface{}, result interface{}) error {
	command, err := json.Marshal(map[string]interface{}{"execute": operation, "arguments": arguments})
	if err != nil {
		return err
	}
	request := a.client.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "compute", Command: []string{"virsh", "qemu-agent-command", domain, string(command)}, Stdout: true, Stderr: true,
	}, clientscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(a.config, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("unable to connect to Windows guest agent")
	}
	var stdout, stderr boundedAgentOutput
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return fmt.Errorf("windows guest agent request failed")
	}
	var response struct {
		Return json.RawMessage `json:"return"`
	}
	if json.Unmarshal(stdout.Bytes(), &response) != nil || len(response.Return) == 0 || json.Unmarshal(response.Return, result) != nil {
		return fmt.Errorf("invalid Windows guest agent response")
	}
	return nil
}

func (a *windowsGuestAgent) CompleteBootstrap(ctx context.Context, namespace, pod, vm, hostname string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	// hostname is generated internally from workspace UID/generation, never an
	// arbitrary script parameter. Setup may report COMPLETE before interactive
	// OOBE, so also require the generated account and expected computer identity.
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'
$state=(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Setup\State').ImageState
$user=Get-LocalUser -Name workspace -ErrorAction SilentlyContinue
$complete=$state -eq 'IMAGE_STATE_COMPLETE' -and $null -ne $user -and $user.Enabled -and $env:COMPUTERNAME -eq '%s'
if ($complete) {
  Remove-ItemProperty 'HKLM:\SYSTEM\Setup' -Name UnattendFile -ErrorAction SilentlyContinue
  foreach ($base in @('C:\Windows\Panther','C:\Windows\System32\Sysprep')) {
    Get-ChildItem $base -Recurse -File -Filter '*unattend*.xml' | Remove-Item -Force
  }
}
@{complete=[bool]$complete} | ConvertTo-Json -Compress`, hostname)
	output, err := a.execScript(ctx, namespace, pod, vm, script)
	if err != nil {
		return false, err
	}
	var state struct {
		Complete bool `json:"complete"`
	}
	if json.Unmarshal(output, &state) != nil {
		return false, fmt.Errorf("invalid Windows provisioning status")
	}
	return state.Complete, nil
}

// enrollmentStageScript renders the token write. Pure so the quoting
// guard is unit-testable without a guest.
func enrollmentStageScript(token []byte) (string, error) {
	if bytes.Contains(token, []byte("'")) {
		return "", fmt.Errorf("refusing to stage an enrollment token with quoting")
	}
	var parsed map[string]any
	if json.Unmarshal(token, &parsed) != nil {
		return "", fmt.Errorf("refusing to stage a malformed enrollment token")
	}
	return fmt.Sprintf(`$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'
$dir='C:\ProgramData\workspace-agent'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Set-Content -Path ($dir+'\enrollment-token.json') -Value '%s' -Encoding UTF8 -NoNewline -Force
@{staged=$true} | ConvertTo-Json -Compress`, token), nil
}

// StageEnrollment writes one generation's enrollment token into the guest
// agent data dir. The token JSON is embedded single-quoted: the renderer
// rejects any token containing a single quote first, so the script cannot
// break out. PowerShell 5.1 writes UTF8 with a BOM, which the agent
// tolerates. Nonzero exit fails provisioning; the token stays in the
// owned Secret for the next reconcile.
func (a *windowsGuestAgent) StageEnrollment(ctx context.Context, namespace, pod, vm string, token []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	script, err := enrollmentStageScript(token)
	if err != nil {
		return err
	}
	if _, err := a.execScript(ctx, namespace, pod, vm, script); err != nil {
		return fmt.Errorf("windows enrollment staging failed: %w", err)
	}
	return nil
}

// execScript runs a PowerShell script in the guest over the QEMU agent
// channel and returns its parsed JSON stdout. Nonzero exit is an error.
func (a *windowsGuestAgent) execScript(ctx context.Context, namespace, pod, vm, script string) ([]byte, error) {
	units := utf16.Encode([]rune(script))
	encoded := make([]byte, len(units)*2)
	for i, unit := range units {
		encoded[i*2], encoded[i*2+1] = byte(unit), byte(unit>>8)
	}
	var process struct {
		PID int `json:"pid"`
	}
	domain := namespace + "_" + vm
	if err := a.rpc(ctx, namespace, pod, domain, "guest-exec", map[string]interface{}{
		"path": `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		"arg":  []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded)}, "capture-output": true,
	}, &process); err != nil {
		return nil, err
	}
	for {
		var status struct {
			Exited bool   `json:"exited"`
			Exit   int    `json:"exitcode"`
			Output string `json:"out-data"`
		}
		if err := a.rpc(ctx, namespace, pod, domain, "guest-exec-status", map[string]interface{}{"pid": process.PID}, &status); err != nil {
			return nil, err
		}
		if status.Exited {
			if status.Exit != 0 {
				return nil, fmt.Errorf("windows guest script exited %d", status.Exit)
			}
			output, err := base64.StdEncoding.DecodeString(status.Output)
			if err != nil {
				return nil, fmt.Errorf("invalid Windows guest script output")
			}
			return output, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("windows guest script timed out")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

var _ io.Writer = (*boundedAgentOutput)(nil)
