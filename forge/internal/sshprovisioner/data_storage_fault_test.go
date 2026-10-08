package sshprovisioner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

// fixtureLoopEnv is the INTERNAL test-process-only environment flag that tells
// the generated reconcile/purge script it may accept loop-backed by-id fixtures.
// It is NOT a Forge CLI or configuration knob (it cannot be set through Forge),
// and normal Forge apply never sets it, so ordinary production rejection of
// loop/logical devices and unsigned disks is fully retained. Only the privileged
// forge-fault-matrix CI harness injects it when it executes the real script over
// loop-backed by-id devices.
const (
	fixtureLoopEnv          = "FORGE_DATA_STORAGE_FIXTURE_LOOP"
	fixtureStageBarrierEnv  = "FORGE_DATA_STORAGE_STAGE_BARRIER_DIR"
	fixtureStageExpectedEnv = "FORGE_DATA_STORAGE_STAGE_BARRIER"
)

// fixtureRequiredEnv, when set to "1", makes lvmFaultMatrixEnv fail (not skip)
// whenever the privileged environment is unavailable, so the required defect-matrix
// CI job cannot silently pass without exercising the real transaction.
const fixtureRequiredEnv = "FORGE_FAULT_MATRIX_REQUIRED"

// faultByIDDir mirrors the stable whole-disk by-id identities Forge selects in
// production. The fault harness places iteration-specific aliases under it for
// loop-backed devices and removes them on teardown.
const faultByIDDir = "/dev/disk/by-id"

// lvmFaultMatrixEnv reports whether a privileged real-LVM environment capable of
// staging the actual reconcile transaction is available. It requires root, the
// LVM2 + losetup toolchain, mknod, and /dev/disk/by-id so the deterministic
// executable fault-stage matrix can create loop-backed by-id block devices. When
// unavailable it skips on dev workstations, but FAILS (rather than skips) when
// FORGE_FAULT_MATRIX_REQUIRED is set, so the required CI job is fail-closed if
// its prerequisites are missing (founder-approved Option B).
func lvmFaultMatrixEnv(t *testing.T) (bool, string) {
	t.Helper()
	required := os.Getenv(fixtureRequiredEnv) == "1"
	unavailable := func(reason string) (bool, string) {
		if required {
			t.Fatalf("required forge fault-matrix environment unavailable, must fail not skip: %s", reason)
		}
		return false, reason
	}
	if os.Geteuid() != 0 {
		return unavailable("real-LVM reconcile fault matrix requires root (losetup/mknod/by-id alias)")
	}
	for _, tool := range []string{"losetup", "pvcreate", "vgcreate", "pvs", "vgs", "lvs", "vgremove", "pvremove", "fuser", "mknod", "ln"} {
		if _, err := exec.LookPath(tool); err != nil {
			return unavailable(fmt.Sprintf("real-LVM reconcile fault matrix requires %q", tool))
		}
	}
	return true, ""
}

// writeFaultScript writes the generated reconcile script to disk so it can be
// executed with the real bash -ceu interpreter (exactly as Forge runs it over
// SSH) while a goroutine terminates the process at a chosen durable stage.
func writeFaultScript(t *testing.T, spec provisioner.DataStorageSpec, mode string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reconcile.sh")
	require.NoError(t, os.WriteFile(path, []byte("set -eu\n"+dataStorageReconcileScript(spec, mode)+"\n"), 0o600))
	return path
}

func writePurgeFaultScript(t *testing.T, spec provisioner.DataStorageSpec) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "purge.sh")
	require.NoError(t, os.WriteFile(path, []byte("set -eu\n"+dataStoragePurgeScript(spec)+"\n"), 0o600))
	return path
}

// runDataStorageBash executes an already-written reconcile script under the real
// bash interpreter in the internal fixture mode (fixtureLoopEnv set) and returns
// stdout together with a precise outcome tag: "ok" (exit 0, completed normally),
// "crashed" (SIGKILL injected at the requested durable stage), or "error" (the
// script refused/failed). This lets callers distinguish a clean success from an
// injected crash and from a refusal, which a single boolean cannot.
func runDataStorageBash(t *testing.T, path, barrier string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.Command("bash", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), fixtureLoopEnv+"=1")
	barrierDir := ""
	if barrier != "" {
		barrierDir = t.TempDir()
		cmd.Env = append(cmd.Env, fixtureStageBarrierEnv+"="+barrierDir, fixtureStageExpectedEnv+"="+barrier)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start data-storage script: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	if barrier != "" {
		reached := filepath.Join(barrierDir, barrier+".reached")
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case err := <-done:
				return stdout.String() + "\nstderr: " + stderr.String() + fmt.Sprintf("\nprocess exited before barrier: %v", err), "error"
			case <-ctx.Done():
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				t.Fatalf("data-storage script never reached required barrier %s; stdout=%s stderr=%s", barrier, stdout.String(), stderr.String())
			case <-ticker.C:
				if _, err := os.Stat(reached); err != nil {
					continue
				}
				require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL), "kill data-storage process group at %s", barrier)
				err := <-done
				var exitErr *exec.ExitError
				require.True(t, errors.As(err, &exitErr), "barrier process did not exit by signal: %v", err)
				require.Equal(t, syscall.SIGKILL, exitErr.ProcessState.Sys().(syscall.WaitStatus).Signal())
				return stdout.String() + "\nstderr: " + stderr.String(), "crashed"
			}
		}
	}

	select {
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("data-storage script timed out; stdout=%s stderr=%s", stdout.String(), stderr.String())
		return "", "error"
	case err := <-done:
		if err != nil {
			return stdout.String() + "\nstderr: " + stderr.String(), "error"
		}
		return stdout.String(), "ok"
	}
}

// makeFaultLoopDevices allocates count real loop-backed block devices and places
// stable /dev/disk/by-id aliases for them (mirroring the whole-disk by-id
// identities Forge selects in production). It returns the by-id paths to hand to
// the reconcile script and the raw loop device paths to detach on teardown.
func makeFaultLoopDevices(t *testing.T, dir string, count int) ([]string, []string) {
	t.Helper()
	var byID, loops []string
	require.NoError(t, os.MkdirAll(faultByIDDir, 0o755))
	for i := 0; i < count; i++ {
		img := filepath.Join(dir, fmt.Sprintf("disk%d.img", i))
		require.NoError(t, runCmd("truncate", "-s", "16M", img))
		out, err := exec.Command("losetup", "--find", "--show", "--nooverlap", img).CombinedOutput()
		require.NoError(t, err, "losetup failed: %s", out)
		loop := strings.TrimSpace(string(out))
		loops = append(loops, loop)
		alias := filepath.Join(faultByIDDir, "iterabase-fixture-"+strconv.Itoa(i))
		require.NoError(t, os.RemoveAll(alias))
		require.NoError(t, os.Symlink(loop, alias))
		byID = append(byID, alias)
	}
	return byID, loops
}

func teardownFaultLoopDevices(t *testing.T, loops, aliases []string) {
	t.Helper()
	for _, a := range aliases {
		_ = os.RemoveAll(a)
	}
	for _, loop := range loops {
		_ = runCmd("losetup", "-d", loop)
	}
}

// resetFaultStorage removes the durable fixture VG (production name
// iterabase-data) and receipt so a fresh matrix case starts from clean state.
func resetFaultStorage(t *testing.T) {
	t.Helper()
	_ = runCmd("vgremove", "--force", "--yes", provisioner.DataVolumeGroupName)
	_ = os.Remove(dataStorageReceiptPath)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

// lvmFaultDump returns the current LVM + receipt state for failure diagnostics in
// the privileged fault matrix.
func lvmFaultDump() string {
	out, err := exec.Command("sh", "-c", `vgs --noheadings -o vg_name,vg_uuid 2>/dev/null; pvs --noheadings -o pv_name,pv_uuid,vg_name 2>/dev/null; echo '--- receipt ---'; cat /var/lib/iterabase/data-storage.receipt 2>/dev/null || true`).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out))
}

// TestDataStorageFaultMatrixScriptCoversEveryDurableStage is the locally
// runnable structural companion: it asserts the generated script front-loads the
// durable receipt before every mutation, binds each stage to an explicit
// write_receipt, and gates the loop-by-id fixture escape behind the internal
// FORGE_DATA_STORAGE_FIXTURE_LOOP flag so ordinary production rejection is
// retained unless the test harness opt-in is present.
func TestDataStorageFaultMatrixScriptCoversEveryDurableStage(t *testing.T) {
	script := dataStorageReconcileScript(provisioner.DataStorageSpec{
		InstallName: "opo1", Devices: []string{"/dev/disk/by-id/scsi-a", "/dev/disk/by-id/scsi-b"},
	}, "reconcile")

	// Every pvcreate in the per-PV loop is immediately followed by a durable
	// pvs-created receipt that persists pv_done so a crash mid-write is resumable.
	pvcreateIdx := strings.Index(script, "pvcreate --yes --zero y --uuid")
	require.GreaterOrEqual(t, pvcreateIdx, 0, "reconcile must pvcreate each planned device")
	receipt := strings.Index(script, "write_receipt pvs-created")
	require.GreaterOrEqual(t, receipt, 0, "reconcile must persist a pvs-created receipt after each PV")
	require.Less(t, pvcreateIdx, receipt, "each pvcreate must be followed by the durable pvs-created receipt")
	require.Equal(t, 1, strings.Count(script, "write_receipt pvs-created"))

	// DES-HOR-545-03 monotonic receipt: the pvs-created write is gated on an
	// unbound VG UUID, so exact reapply of an already-complete receipt never
	// regresses to pvs-created.
	require.Contains(t, script, "if test -z \"$receipt_vg_uuid\"; then")
	require.Contains(t, script, "write_receipt pvs-created \"$pv_done\" 0")

	// The planned receipt precedes the first mutation and the VG-created receipt
	// binds the complete PV set before the UUID is persisted.
	require.Less(t, strings.Index(script, "write_receipt planned 0"), strings.Index(script, "pvcreate --yes --zero y --uuid"))
	require.Less(t, strings.Index(script, "vgcreate --yes --addtag"), strings.Index(script, "receipt_vg_uuid=$vg_uuid; write_receipt vg-created"))

	// Founder-approved Option B: the loop/unsigned-device fixture escape is gated
	// behind the internal FORGE_DATA_STORAGE_FIXTURE_LOOP flag. Its only
	// occurrences are the gate guards; ordinary production rejection of loop
	// devices and unsigned disks remains the default.
	// Founder-approved Option B: the loop/unsigned/whole-disk fixture escape is
	// gated behind the internal FORGE_DATA_STORAGE_FIXTURE_LOOP flag. Every
	// check that would otherwise reject the loop fixture must be gated (balanced),
	// and the ordinary production rejection messages must remain present so
	// normal forge apply (without the flag) rejects loop devices unchanged.
	gate := "if [ \"${FORGE_DATA_STORAGE_FIXTURE_LOOP:-}\" != \"1\" ]; then"
	wholeDisk := strings.Count(script, "is not a whole disk")
	loopReject := strings.Count(script, "unsupported logical/network device")
	unsigned := strings.Count(script, "exposes neither serial nor WWN")
	require.Equal(t, wholeDisk+loopReject+unsigned, strings.Count(script, gate),
		"every production device-rejection check must be gated behind the internal fixture flag")
	require.GreaterOrEqual(t, wholeDisk, 2, "both the resolution prelude and topology probe must keep the whole-disk rejection")
	require.Contains(t, script, "unsupported logical/network device")
	require.Contains(t, script, "exposes neither serial nor WWN")
}

// requireReceiptStatus asserts the durable receipt currently reports the given
// status field, proving the on-disk stage survived (or was not regressed by)
// the preceding run/crash.
func receiptFields(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(dataStorageReceiptPath)
	require.NoError(t, err, "read durable receipt")
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			fields[key] = value
		}
	}
	return fields
}

func requireReceiptStatus(t *testing.T, want string) {
	t.Helper()
	data, err := os.ReadFile(dataStorageReceiptPath)
	require.NoError(t, err, "read durable receipt")
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "status=") {
			require.Equal(t, "status="+want, strings.TrimSpace(line))
			return
		}
	}
	t.Fatalf("receipt has no status field: %s", string(data))
}

func requireLiveVG(t *testing.T, present bool) {
	t.Helper()
	err := exec.Command("vgs", provisioner.DataVolumeGroupName).Run()
	if present {
		require.NoError(t, err, "receipt-owned VG must exist at this durable stage")
	} else {
		require.Error(t, err, "receipt-owned VG must be absent at this durable stage")
	}
}

func requireLivePVPresence(t *testing.T, spec provisioner.DataStorageSpec, present ...bool) {
	t.Helper()
	require.Len(t, present, len(spec.Devices))
	fields := map[string]string{}
	for _, expected := range present {
		if expected {
			fields = receiptFields(t)
			break
		}
	}
	for i, device := range spec.Devices {
		resolved, err := filepath.EvalSymlinks(device)
		require.NoError(t, err)
		out, err := exec.Command("pvs", "--noheadings", "--separator", "|", "-o", "pv_uuid,vg_name", "--", resolved).CombinedOutput()
		if !present[i] {
			require.Error(t, err, "PV %d unexpectedly exists: %s", i, out)
			continue
		}
		require.NoError(t, err, "PV %d missing: %s", i, out)
		parts := strings.Split(strings.TrimSpace(string(out)), "|")
		require.Len(t, parts, 2)
		require.Equal(t, fields[fmt.Sprintf("pv_uuid_%d", i)], strings.TrimSpace(parts[0]))
	}
}

func requireReconcileBarrierState(t *testing.T, spec provisioner.DataStorageSpec, barrier string) {
	t.Helper()
	switch barrier {
	case "receipt-planned":
		requireReceiptStatus(t, "planned")
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, false, false)
	case "pv-1-created":
		requireReceiptStatus(t, "pvs-created")
		require.Equal(t, "1", receiptFields(t)["pv_done"])
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, true, false)
	case "pv-2-created":
		requireReceiptStatus(t, "pvs-created")
		require.Equal(t, "2", receiptFields(t)["pv_done"])
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, true, true)
	case "vg-created":
		requireReceiptStatus(t, "vg-created")
		requireLiveVG(t, true)
		requireLivePVPresence(t, spec, true, true)
	case "complete", "reapply-inspected-complete":
		requireReceiptStatus(t, "complete")
		requireLiveVG(t, true)
		requireLivePVPresence(t, spec, true, true)
	default:
		t.Fatalf("unknown reconcile barrier %q", barrier)
	}
}

// requireReceiptPVIdentityMatchesLive proves the durable receipt's planned PV
// UUID for every bound device exactly equals the on-disk PV UUID LVM reports for
// that device. The reconcile enforces this strictly; this assertion makes the
// two-device fault matrix prove it explicitly (both explicit pvcreate stages and
// exact receipt UUIDs), so any future relaxation is caught here, not only via a
// non-failure of the reconcile run.
func requireReceiptPVIdentityMatchesLive(t *testing.T, spec provisioner.DataStorageSpec) {
	t.Helper()
	data, err := os.ReadFile(dataStorageReceiptPath)
	require.NoError(t, err, "read durable receipt")
	resolved := map[int]string{}
	pvUUID := map[int]string{}
	for _, line := range strings.Split(string(data), "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := kv[0], strings.TrimSpace(kv[1])
		if rem, ok := strings.CutPrefix(key, "resolved_"); ok {
			idx, err := strconv.Atoi(strings.TrimSuffix(rem, "_b64"))
			if err != nil {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				continue
			}
			resolved[idx] = string(b)
		} else if rem, ok := strings.CutPrefix(key, "pv_uuid_"); ok {
			idx, err := strconv.Atoi(rem)
			if err != nil {
				continue
			}
			pvUUID[idx] = val
		}
	}
	for i := 0; i < len(spec.Devices); i++ {
		dev, ok := resolved[i]
		require.Truef(t, ok, "receipt missing resolved device %d", i)
		want, ok := pvUUID[i]
		require.Truef(t, ok, "receipt missing pv_uuid for device %d", i)
		out, err := exec.Command("pvs", "--noheadings", "-o", "pv_uuid", "--", dev).CombinedOutput()
		require.NoErrorf(t, err, "pvs on %s: %s", dev, string(out))
		got := strings.TrimSpace(string(out))
		require.Equalf(t, want, got, "receipt PV UUID for device %d (%s) does not match live on-disk UUID", i, dev)
	}
}

// TestDataStorageFaultStageMatrixExecutable stages a real LVM transaction across
// every durable boundary and proves Forge's crash-resumable and mismatch-refusal
// contract by actually executing the generated reconcile script over loop-backed
// by-id devices in the fixture mode. Each sub-test terminates the script with
// SIGKILL after a specific durable receipt stage, re-runs it idempotently, and
// asserts it resumes and completes. It runs in the privileged required
// forge-fault-matrix CI job (FORGE_FAULT_MATRIX_REQUIRED=1, root) and fails
// rather than skips if the environment or any stage is unavailable.
func TestDataStorageFaultStageMatrixExecutable(t *testing.T) {
	ok, reason := lvmFaultMatrixEnv(t)
	if !ok {
		t.Skip(reason)
	}

	var curLoops, curAliases []string
	t.Cleanup(func() { teardownFaultLoopDevices(t, curLoops, curAliases) })
	// newFixture tears down prior devices, resets the durable VG/receipt, and
	// creates a fresh loop-backed by-id fixture for an independent case.
	newFixture := func(t *testing.T, installName string) (provisioner.DataStorageSpec, string) {
		t.Helper()
		teardownFaultLoopDevices(t, curLoops, curAliases)
		resetFaultStorage(t)
		dir := t.TempDir()
		aliases, loops := makeFaultLoopDevices(t, dir, 2)
		curLoops, curAliases = loops, aliases
		spec := provisioner.DataStorageSpec{InstallName: installName, Devices: aliases}
		return spec, writeFaultScript(t, spec, "reconcile")
	}

	// A clean full run must converge to complete and bind both PVs + the VG.
	spec, script := newFixture(t, "opo1")
	out, outcome := runDataStorageBash(t, script, "")
	require.Equal(t, "ok", outcome, "reconcile failed on clean run: %s\n%s", out, lvmFaultDump())
	require.Contains(t, out, "FORGE_DATA_STORAGE_RESULT\tcomplete")
	// The bounded VG inspection parser must be present in the generated script.
	require.Contains(t, dataStorageReconcileScript(spec, "reconcile"), lvmReportPairParser)
	requireReceiptPVIdentityMatchesLive(t, spec)

	// Fault-stage matrix: crash after each durable receipt stage then resume.
	for _, sc := range []struct {
		name    string
		barrier string
	}{
		{name: "planned", barrier: "receipt-planned"},
		{name: "after-pv0", barrier: "pv-1-created"},
		{name: "after-pv1", barrier: "pv-2-created"},
		{name: "after-vg", barrier: "vg-created"},
		{name: "complete", barrier: "complete"},
	} {
		t.Run("resume-after-"+sc.name, func(t *testing.T) {
			rspec, fscript := newFixture(t, "opo1")
			out, outcome := runDataStorageBash(t, fscript, sc.barrier)
			require.Equal(t, "crashed", outcome, "expected SIGKILL crash at %s; out=%s", sc.name, out)
			requireReconcileBarrierState(t, rspec, sc.barrier)
			out2, outcome2 := runDataStorageBash(t, fscript, "")
			require.Equal(t, "ok", outcome2, "resume failed after %s crash: %s", sc.name, out2)
			require.Contains(t, out2, "FORGE_DATA_STORAGE_RESULT\tcomplete")
			requireReceiptPVIdentityMatchesLive(t, rspec)
		})
	}

	// Mismatch refusal: regenerate the script from mismatched install input and
	// run it against the same durable fixture - the receipt must refuse it.
	t.Run("refuses-foreign-install", func(t *testing.T) {
		basespec, base := newFixture(t, "opo1")
		out, outcome := runDataStorageBash(t, base, "")
		require.Equal(t, "ok", outcome, "seed complete receipt: %s", out)
		require.Contains(t, out, "FORGE_DATA_STORAGE_RESULT\tcomplete")
		requireReceiptPVIdentityMatchesLive(t, basespec)
		foreign := writeFaultScript(t, provisioner.DataStorageSpec{InstallName: "other", Devices: curAliases}, "reconcile")
		outF, outcomeF := runDataStorageBash(t, foreign, "")
		require.Equal(t, "error", outcomeF, "foreign install identity must be refused")
		require.Contains(t, outF, "receipt install mismatch")
	})
}

// TestDataStorageReapplyReceiptMonotonicExecutable proves exact reapply of an
// already-complete receipt is idempotent, crash-resumable, and never regresses
// the receipt to pvs-created (DES-HOR-545-03 / HOR-545 exact-reapply
// acceptance). It executes the real reconcile script over loop-backed by-id
// devices, injects SIGKILL crashes, and asserts the complete receipt is always
// preserved.
func TestDataStorageReapplyReceiptMonotonicExecutable(t *testing.T) {
	ok, reason := lvmFaultMatrixEnv(t)
	if !ok {
		t.Skip(reason)
	}
	resetFaultStorage(t)
	dir := t.TempDir()
	aliases, loops := makeFaultLoopDevices(t, dir, 2)
	t.Cleanup(func() { teardownFaultLoopDevices(t, loops, aliases) })
	spec := provisioner.DataStorageSpec{InstallName: "opo1", Devices: aliases}
	script := writeFaultScript(t, spec, "reconcile")

	// Fresh install must converge to complete and bind both PVs + the VG.
	out, outcome := runDataStorageBash(t, script, "")
	require.Equal(t, "ok", outcome, "reconcile failed on clean run: %s\n%s", out, lvmFaultDump())
	require.Contains(t, out, "FORGE_DATA_STORAGE_RESULT\tcomplete")
	requireReceiptStatus(t, "complete")
	requireReceiptPVIdentityMatchesLive(t, spec)

	// Exact reapply: with the monotonic guard the pvs-created marker is never
	// re-emitted for an already-complete receipt, so the run completes normally
	// (outcome "ok", not "crashed") and the receipt stays complete.
	out, outcome = runDataStorageBash(t, script, "")
	require.Equal(t, "ok", outcome, "exact reapply regressed the receipt to pvs-created:\n%s", out)
	require.Contains(t, out, "FORGE_DATA_STORAGE_RESULT\tcomplete")
	requireReceiptStatus(t, "complete")
	requireReceiptPVIdentityMatchesLive(t, spec)

	// Crash during reapply is synchronized after authoritative complete-receipt
	// inspection. The injected process-group SIGKILL is mandatory, and exact
	// receipt/live LVM state is asserted before resume.
	crashOut, crashOutcome := runDataStorageBash(t, script, "reapply-inspected-complete")
	require.Equal(t, "crashed", crashOutcome, "required reapply crash was not injected: %s", crashOut)
	requireReconcileBarrierState(t, spec, "reapply-inspected-complete")
	out2, outcome2 := runDataStorageBash(t, script, "")
	require.Equal(t, "ok", outcome2, "reapply did not resume to complete after crash: %s", out2)
	require.Contains(t, out2, "FORGE_DATA_STORAGE_RESULT\tcomplete")
	requireReceiptStatus(t, "complete")
	requireReceiptPVIdentityMatchesLive(t, spec)
}

// TestDataStorageReapplyAfterKernelNameReorderExecutable proves a reboot that
// renames the selected disks (for example NVMe probe order) does not block
// reapply, while hardware substitution under the same by-id link still refuses
// (DES-HOR-616-01).
func TestDataStorageReapplyAfterKernelNameReorderExecutable(t *testing.T) {
	ok, reason := lvmFaultMatrixEnv(t)
	if !ok {
		t.Skip(reason)
	}
	resetFaultStorage(t)
	dir := t.TempDir()
	aliases, loops := makeFaultLoopDevices(t, dir, 2)
	t.Cleanup(func() { teardownFaultLoopDevices(t, loops, aliases) })
	spec := provisioner.DataStorageSpec{InstallName: "opo1", Devices: aliases}
	reconcile := writeFaultScript(t, spec, "reconcile")
	inspect := writeFaultScript(t, spec, "inspect")

	out, outcome := runDataStorageBash(t, reconcile, "")
	require.Equal(t, "ok", outcome, "reconcile failed on clean run: %s\n%s", out, lvmFaultDump())
	requireReceiptStatus(t, "complete")

	// Simulate the previous boot's kernel names by swapping the recorded paths.
	fields := receiptFields(t)
	setReceiptFields(t, map[string]string{"resolved_0_b64": fields["resolved_1_b64"], "resolved_1_b64": fields["resolved_0_b64"]})
	for _, script := range []string{inspect, reconcile} {
		out, outcome = runDataStorageBash(t, script, "")
		require.Equal(t, "ok", outcome, "kernel-name reorder blocked %s: %s\n%s", filepath.Base(script), out, lvmFaultDump())
		require.Contains(t, out, "FORGE_DATA_STORAGE_RESULT\tcomplete")
	}
	requireReceiptStatus(t, "complete")

	// A different disk behind the same by-id link is still refused.
	setReceiptFields(t, map[string]string{"size_0": "1"})
	out, outcome = runDataStorageBash(t, inspect, "")
	require.Equal(t, "error", outcome, "hardware substitution was not refused: %s", out)
	require.Contains(t, out, "data-storage size identity drift")
}

// setReceiptFields rewrites receipt values in place, preserving its 0600 root
// ownership, to stage an on-disk receipt from a different boot or disk.
func setReceiptFields(t *testing.T, values map[string]string) {
	t.Helper()
	data, err := os.ReadFile(dataStorageReceiptPath)
	require.NoError(t, err, "read durable receipt")
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if key, _, ok := strings.Cut(line, "="); ok {
			if value, replace := values[key]; replace {
				lines[i] = key + "=" + value
			}
		}
	}
	require.NoError(t, os.WriteFile(dataStorageReceiptPath, []byte(strings.Join(lines, "\n")), 0o600))
}

func requirePurgeBarrierState(t *testing.T, spec provisioner.DataStorageSpec, barrier string) {
	t.Helper()
	switch barrier {
	case "purge-vg-pending":
		requireReceiptStatus(t, "purge-vg-pending")
		requireLiveVG(t, true)
		requireLivePVPresence(t, spec, true, true)
	case "purge-vg-removed":
		requireReceiptStatus(t, "purge-vg-removed")
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, true, true)
	case "purge-pv-0-pending":
		requireReceiptStatus(t, "purge-pv-pending")
		require.Equal(t, "0", receiptFields(t)["purge_done"])
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, true, true)
	case "purge-pv-0-removed":
		requireReceiptStatus(t, "purge-pvs-removed")
		require.Equal(t, "1", receiptFields(t)["purge_done"])
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, false, true)
	case "purge-pv-1-pending":
		requireReceiptStatus(t, "purge-pv-pending")
		require.Equal(t, "1", receiptFields(t)["purge_done"])
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, false, true)
	case "purge-pv-1-removed":
		requireReceiptStatus(t, "purge-pvs-removed")
		require.Equal(t, "2", receiptFields(t)["purge_done"])
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, false, false)
	case "purge-receipt-pending":
		requireReceiptStatus(t, "purge-receipt-pending")
		require.Equal(t, "2", receiptFields(t)["purge_done"])
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, false, false)
	case "receipt-removed":
		_, err := os.Stat(dataStorageReceiptPath)
		require.Error(t, err)
		require.True(t, os.IsNotExist(err))
		requireLiveVG(t, false)
		requireLivePVPresence(t, spec, false, false)
	default:
		t.Fatalf("unknown purge barrier %q", barrier)
	}
}

func TestDataStoragePurgeFaultStageMatrixExecutable(t *testing.T) {
	ok, reason := lvmFaultMatrixEnv(t)
	if !ok {
		t.Skip(reason)
	}
	var curLoops, curAliases []string
	t.Cleanup(func() { teardownFaultLoopDevices(t, curLoops, curAliases) })
	newCompleteFixture := func(t *testing.T) (provisioner.DataStorageSpec, string) {
		t.Helper()
		teardownFaultLoopDevices(t, curLoops, curAliases)
		resetFaultStorage(t)
		aliases, loops := makeFaultLoopDevices(t, t.TempDir(), 2)
		curLoops, curAliases = loops, aliases
		spec := provisioner.DataStorageSpec{InstallName: "opo1", Devices: aliases}
		out, outcome := runDataStorageBash(t, writeFaultScript(t, spec, "reconcile"), "")
		require.Equal(t, "ok", outcome, "seed complete purge fixture: %s", out)
		return spec, writePurgeFaultScript(t, spec)
	}
	for _, barrier := range []string{
		"purge-vg-pending", "purge-vg-removed",
		"purge-pv-0-pending", "purge-pv-0-removed",
		"purge-pv-1-pending", "purge-pv-1-removed",
		"purge-receipt-pending", "receipt-removed",
	} {
		t.Run("resume-after-"+barrier, func(t *testing.T) {
			spec, purge := newCompleteFixture(t)
			out, outcome := runDataStorageBash(t, purge, barrier)
			require.Equal(t, "crashed", outcome, "required purge crash at %s was not injected: %s", barrier, out)
			requirePurgeBarrierState(t, spec, barrier)
			resumed, resumeOutcome := runDataStorageBash(t, purge, "")
			require.Equal(t, "ok", resumeOutcome, "purge resume after %s: %s\n%s", barrier, resumed, lvmFaultDump())
			require.Contains(t, resumed, "FORGE_DATA_STORAGE_PURGE_RESULT")
			_, err := os.Stat(dataStorageReceiptPath)
			require.True(t, os.IsNotExist(err), "purge resume retained receipt: %v", err)
			requireLiveVG(t, false)
			requireLivePVPresence(t, spec, false, false)
		})
	}
}

// TestDataStorageLoopDevicesRejectedWithoutFixtureFlag proves the ordinary
// production prelude still rejects loop-backed identities when the internal
// FORGE_DATA_STORAGE_FIXTURE_LOOP flag is absent, so the founder-approved
// fixture escape can never affect normal forge apply.
func TestDataStorageLoopDevicesRejectedWithoutFixtureFlag(t *testing.T) {
	ok, reason := lvmFaultMatrixEnv(t)
	if !ok {
		t.Skip(reason)
	}
	resetFaultStorage(t)
	dir := t.TempDir()
	aliases, loops := makeFaultLoopDevices(t, dir, 1)
	t.Cleanup(func() { teardownFaultLoopDevices(t, loops, aliases) })
	spec := provisioner.DataStorageSpec{InstallName: "opo1", Devices: aliases}
	path := writeFaultScript(t, spec, "reconcile")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", path) // deliberately NO fixtureLoopEnv
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	require.Error(t, err, "loop-backed by-id device accepted without FORGE_DATA_STORAGE_FIXTURE_LOOP")
	outStr := out.String()
	require.Contains(t, outStr, "data-storage refusal")
	require.Contains(t, outStr, "is not a whole disk")
}
