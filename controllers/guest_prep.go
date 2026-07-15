package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	winrm "github.com/masterzen/winrm"
	migrationv1alpha1 "github.com/lightwell-tech/ahv-to-ove-operator/api/v1alpha1"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const defaultSSHTimeout = 300 * time.Second

// handleGuestPrepping: SSH経由で各VMにドライバー注入スクリプトを実行する
// 全VM完了後、次フェーズ(WarmPreSync/PreparingImages/ImportingDisks)へ遷移
func (r *AHVMigrationReconciler) handleGuestPrepping(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("GuestPrepping: running pre-migration scripts via SSH")

	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)

	allDone := true
	for i, vmSpec := range mig.Spec.VMs {
		needsPrep := (vmSpec.GuestPrepMode == "ssh" && vmSpec.GuestPrepConfig != nil) ||
			(vmSpec.GuestPrepMode == "winrm" && vmSpec.GuestPrepWinRMConfig != nil)

		if !needsPrep {
			if newVMStatuses[i].Phase == "GuestPrepping" || newVMStatuses[i].Phase == "Pending" {
				newVMStatuses[i].Phase = "GuestPrepped"
			}
			continue
		}

		switch newVMStatuses[i].Phase {
		case "GuestPrepped":
			continue
		case "GuestPrepFailed":
			return r.failMigration(ctx, mig, fmt.Sprintf("VM %q: guest prep failed", vmSpec.Name))
		default:
			newVMStatuses[i].Phase = "GuestPrepping"
			allDone = false

			var err error
			if vmSpec.GuestPrepMode == "winrm" {
				logger.Info("Running WinRM guest prep", "vm", vmSpec.Name)
				err = r.runWinRMGuestPrep(ctx, mig, vmSpec)
			} else {
				logger.Info("Running SSH guest prep", "vm", vmSpec.Name)
				err = r.runSSHGuestPrep(ctx, mig, vmSpec, i)
			}

			if err != nil {
				logger.Error(err, "Guest prep failed", "vm", vmSpec.Name, "mode", vmSpec.GuestPrepMode)
				newVMStatuses[i].Phase = "GuestPrepFailed"
				newVMStatuses[i].Error = err.Error()
			} else {
				logger.Info("Guest prep succeeded", "vm", vmSpec.Name)
				newVMStatuses[i].Phase = "GuestPrepped"
				newVMStatuses[i].Error = ""
			}
		}
	}

	// フェーズチェック（GuestPrepFailed があれば失敗、全部GuestPrepped なら次へ）
	completedCount := 0
	for _, st := range newVMStatuses {
		if st.Phase == "GuestPrepFailed" {
			patch := mig.DeepCopy()
			patch.Status.VMs = newVMStatuses
			_ = r.Status().Patch(ctx, patch, client.MergeFrom(mig))
			return r.failMigration(ctx, mig, fmt.Sprintf("VM %q: guest prep failed: %s", st.Name, st.Error))
		}
		if st.Phase == "GuestPrepped" {
			completedCount++
		}
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses

	if completedCount == len(mig.Spec.VMs) && !allDone {
		allDone = true
	}
	if completedCount == len(mig.Spec.VMs) {
		// 全VM prep完了 → 次のフェーズへ
		nextPhase := r.nextPhaseAfterGuestPrep(mig)
		logger.Info("All VMs guest-prepped, transitioning", "nextPhase", nextPhase)
		patch.Status.Phase = nextPhase
		setCondition(patch, "GuestPrepComplete", metav1.ConditionTrue, "Complete", "All VMs guest-prepped successfully")
	} else {
		_ = allDone
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}

	if completedCount < len(mig.Spec.VMs) {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{Requeue: true}, nil
}

// nextPhaseAfterGuestPrep は GuestPrepping 完了後に遷移すべきフェーズを返す
func (r *AHVMigrationReconciler) nextPhaseAfterGuestPrep(mig *migrationv1alpha1.AHVMigration) migrationv1alpha1.AHVMigrationPhase {
	if mig.Spec.Source.WarmMigration {
		return migrationv1alpha1.PhaseWarmPreSync
	}
	if mig.Spec.Source.ShutdownBeforeMigration {
		return migrationv1alpha1.PhasePreparingImages
	}
	return migrationv1alpha1.PhaseImportingDisks
}

// runSSHGuestPrep は対象VMにSSH接続してpreMigrationScriptを実行する
func (r *AHVMigrationReconciler) runSSHGuestPrep(ctx context.Context, mig *migrationv1alpha1.AHVMigration, vmSpec migrationv1alpha1.VMSpec, _ int) error {
	cfg := vmSpec.GuestPrepConfig

	// Secret から接続情報を取得
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cfg.SSHSecretRef.Name,
		Namespace: mig.Namespace,
	}, secret); err != nil {
		return fmt.Errorf("SSH secret %q not found: %w", cfg.SSHSecretRef.Name, err)
	}

	host := strings.TrimSpace(string(secret.Data["host"]))
	user := strings.TrimSpace(string(secret.Data["user"]))
	if host == "" || user == "" {
		return fmt.Errorf("SSH secret must contain 'host' and 'user' keys")
	}
	if !strings.Contains(host, ":") {
		host = host + ":22"
	}

	// 認証方式: privateKey 優先、なければ password
	var authMethod ssh.AuthMethod
	if pk, ok := secret.Data["privateKey"]; ok && len(pk) > 0 {
		signer, err := ssh.ParsePrivateKey(pk)
		if err != nil {
			return fmt.Errorf("parse SSH private key: %w", err)
		}
		authMethod = ssh.PublicKeys(signer)
	} else if pw, ok := secret.Data["password"]; ok && len(pw) > 0 {
		authMethod = ssh.Password(string(pw))
	} else {
		return fmt.Errorf("SSH secret must contain 'privateKey' or 'password'")
	}

	sshCfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{authMethod},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 社内環境想定
		Timeout:         15 * time.Second,
	}

	timeout := defaultSSHTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := sshDialWithContext(execCtx, host, sshCfg)
	if err != nil {
		return fmt.Errorf("SSH dial %s: %w", host, err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("SSH new session: %w", err)
	}
	defer sess.Close()

	script := cfg.PreMigrationScript
	out, err := sess.CombinedOutput(script)
	if err != nil {
		return fmt.Errorf("script failed: %w\noutput: %s", err, string(out))
	}

	log.FromContext(ctx).Info("SSH guest prep output", "vm", vmSpec.Name, "output", string(out))
	return nil
}

// sshDialWithContext はコンテキストキャンセルに対応した SSH dial
func sshDialWithContext(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ssh.Dial("tcp", addr, cfg)
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.client, r.err
	}
}

// defaultVirtIOWinScript は virtio-win ドライバーをインストールするデフォルト PowerShell スクリプト
// - AHV VM に影響を与えない（ドライバー追加のみ。削除・設定変更しない）
// - KubeVirt 起動時に新ハードウェアを検出して自動でドライバーが有効化される
const defaultVirtIOWinScript = `
$ErrorActionPreference = "Stop"
$isoPath = "{{ISO_PATH}}"

if (-not (Test-Path $isoPath)) {
    Write-Error "virtio-win ISO not found: $isoPath"
    exit 1
}

Write-Host "Mounting virtio-win ISO: $isoPath"
$disk = Mount-DiskImage -ImagePath $isoPath -PassThru
$driveLetter = ($disk | Get-Volume).DriveLetter
Write-Host "Mounted at ${driveLetter}:"

Write-Host "Installing virtio drivers (add-only, no removal)..."
$infFiles = Get-ChildItem "${driveLetter}:\amd64\w11\*.inf" -ErrorAction SilentlyContinue
if (-not $infFiles) {
    $infFiles = Get-ChildItem "${driveLetter}:\amd64\2k22\*.inf" -ErrorAction SilentlyContinue
}
if (-not $infFiles) {
    Write-Error "No .inf files found for w11 or 2k22 in ${driveLetter}:\amd64\"
    exit 1
}

foreach ($inf in $infFiles) {
    Write-Host "Adding driver: $($inf.FullName)"
    pnputil /add-driver $inf.FullName /install 2>&1 | Out-Null
}

Write-Host "Dismounting ISO..."
Dismount-DiskImage -ImagePath $isoPath | Out-Null

Write-Host "virtio-win driver installation complete."
`

// runWinRMGuestPrep は WinRM 経由で Windows VM にドライバーインストールスクリプトを実行する
func (r *AHVMigrationReconciler) runWinRMGuestPrep(ctx context.Context, mig *migrationv1alpha1.AHVMigration, vmSpec migrationv1alpha1.VMSpec) error {
	cfg := vmSpec.GuestPrepWinRMConfig

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cfg.WinRMSecretRef.Name,
		Namespace: mig.Namespace,
	}, secret); err != nil {
		return fmt.Errorf("WinRM secret %q not found: %w", cfg.WinRMSecretRef.Name, err)
	}

	host := strings.TrimSpace(string(secret.Data["host"]))
	username := strings.TrimSpace(string(secret.Data["username"]))
	password := strings.TrimSpace(string(secret.Data["password"]))
	if host == "" || username == "" || password == "" {
		return fmt.Errorf("WinRM secret must contain 'host', 'username', 'password'")
	}

	scheme := "http"
	port := "5985"
	if cfg.UseHTTPS {
		scheme = "https"
		port = "5986"
	}
	if !strings.Contains(host, ":") {
		host = host + ":" + port
	}

	// 実行スクリプト決定
	script := cfg.PreMigrationScript
	if script == "" {
		isoPath := cfg.VirtIOWinISOPath
		if isoPath == "" {
			isoPath = `C:\virtio-win.iso`
		}
		script = strings.ReplaceAll(defaultVirtIOWinScript, "{{ISO_PATH}}", isoPath)
	}

	timeout := 600 * time.Second
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return r.executeWinRM(execCtx, scheme, host, username, password, script, vmSpec.Name)
}

// executeWinRM は masterzen/winrm ライブラリ (NTLM/Negotiate 認証) 経由で
// PowerShell スクリプトを実行する。Windows 側での Basic auth 設定変更不要。
func (r *AHVMigrationReconciler) executeWinRM(ctx context.Context, scheme, host, username, password, script, vmName string) error {
	logger := log.FromContext(ctx)

	port := 5985
	useHTTPS := scheme == "https"
	if useHTTPS {
		port = 5986
	}
	// "host:port" 形式に対応
	h := host
	if strings.Contains(host, ":") {
		parts := strings.SplitN(host, ":", 2)
		h = parts[0]
		if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil {
			return fmt.Errorf("invalid port in WinRM host %q: %w", host, err)
		}
	}

	// コンテキストの残り時間をエンドポイントのタイムアウトに渡す
	epTimeout := time.Duration(0)
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 {
			epTimeout = rem
		}
	}

	endpoint := winrm.NewEndpoint(h, port, useHTTPS, true, nil, nil, nil, epTimeout)
	client, err := winrm.NewClient(endpoint, username, password)
	if err != nil {
		return fmt.Errorf("WinRM client init for %s: %w", vmName, err)
	}

	logger.Info("Executing WinRM PowerShell", "vm", vmName, "host", h, "port", port)

	stdout, stderr, exitCode, err := client.RunPSWithContextWithString(ctx, script, "")
	if err != nil {
		return fmt.Errorf("WinRM execute on %s: %w\nstdout: %s\nstderr: %s", vmName, err, stdout, stderr)
	}
	if exitCode != 0 {
		return fmt.Errorf("PowerShell on %s exited with code %d\nstdout: %s\nstderr: %s", vmName, exitCode, stdout, stderr)
	}

	logger.Info("WinRM guest prep complete", "vm", vmName, "stdout", stdout)
	return nil
}

