package e2e

import (
	"slices"
	"testing"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
)

// TestE2E is Forge's single compiled suite entrypoint. Infrastructure scenarios
// remain selectable with go test -run while catalogue mode emits registrations
// without resolving fixtures or provisioning anything.
func TestE2E(t *testing.T) {
	suite := sharede2e.NewSuite(sharede2e.SuiteMetadata{
		Name: "forge", Owner: "forge", Entrypoint: "forge/test/e2e",
	}, sharede2e.FixtureFromEnv)
	suite.Add(
		hermeticExampleScenario(),
		sharede2e.Define(sharede2e.Scenario[*cpuFixtureState]{
			Metadata: forgeScenarioMetadata(
				cpuScenarioName,
				"Bootstraps a fresh CPU host with the exact-head Forge and proves GPU refusal on a CPU host, the exact Flux handoff, durable host inotify capacity, idempotent reapply, secret sync and Flux reconciliation. LVM and AgentPool behaviour is cpu-workspace's.",
				sharede2e.TierF3,
				[]string{"HOR-406", "HOR-545", "HOR-569", "HOR-590", "DES-HOR-545-01", "DES-HOR-538-03"},
				[]string{"forge", "control-plane", "control-plane-chart", "iterabase-platform-chart"},
				"test-e2e", 60, "cpu",
			),
			NewState: newCPUFixtureState,
			Stages: []sharede2e.Stage[*cpuFixtureState]{
				{Name: "prepare-fresh-host", Run: cpuDiagnosticStage(failureDomainFixturePrepare, prepareCPUFixtureStage)},
				{Name: "reject-gpu-on-cpu-host", DependsOn: []string{"prepare-fresh-host"}, Run: cpuDiagnosticStage(failureDomainSubstrate, rejectGPUOnCPUStage)},
				{Name: "fresh-current-with-exact-flux", DependsOn: []string{"reject-gpu-on-cpu-host"}, Run: cpuDiagnosticStage(failureDomainForgeHandoff, runOverlayStage)},
				{Name: "assert-inotify-capacity", DependsOn: []string{"fresh-current-with-exact-flux"}, Run: cpuDiagnosticStage(failureDomainForgeReconcile, assertHostInotifyCapacityStage)},
				{Name: "reapply-current-idempotently", DependsOn: []string{"assert-inotify-capacity"}, Run: cpuDiagnosticStage(failureDomainForgeReconcile, reapplyCurrentPlatformStage)},
				{Name: "assert-inotify-reapply-idempotent", DependsOn: []string{"reapply-current-idempotently"}, Run: cpuDiagnosticStage(failureDomainForgeReconcile, assertHostInotifyReapplyStage)},
				{Name: "sync-secrets", DependsOn: []string{"reapply-current-idempotently"}, Run: cpuDiagnosticStage(failureDomainForgeHandoff, runSecretsStage)},
				{Name: "reconcile-flux", DependsOn: []string{"sync-secrets"}, Run: cpuDiagnosticStage(failureDomainForgeHandoff, runFluxStage)},
			},
			Diagnostics: cpuScenarioDiagnostics(), Cleanup: cpuScenarioCleanup(),
		}),
		sharede2e.Define(sharede2e.Scenario[*cpuFixtureState]{
			Metadata: forgeScenarioMetadata(
				cpuWorkspaceScenarioName,
				"Fresh exact-head real-machine install proving process-open refusal, receipt-bound PV/VG identity, mounted general and AgentPool grow-only XFS/LVM expansion, insufficient-capacity refusal, active-turn continuity, authenticated same-pool isolation, durable 20/25 gating, reboot/reapply convergence, persisted bytes, safe claim release, and non-purging ordinary destroy.",
				sharede2e.TierF3,
				[]string{"HOR-545", "HOR-557", "HOR-590", "REQ-018", "REQ-035", "SCN-018", "DES-HOR-545-01", "DES-HOR-545-02", "DES-HOR-545-03", "DES-HOR-545-07", "DES-HOR-538-03"},
				[]string{"forge", "control-plane", "control-plane-chart", "iterabase-platform-chart"},
				"test-e2e-workspace", 120, "cpu",
			),
			NewState: newCPUWorkspaceFixtureState,
			Stages: []sharede2e.Stage[*cpuFixtureState]{
				{Name: "prepare-fresh-host", Run: cpuDiagnosticStage(failureDomainFixturePrepare, prepareCPUFixtureStage)},
				{Name: "refuse-process-held-raw-disk", DependsOn: []string{"prepare-fresh-host"}, Run: cpuDiagnosticStage(failureDomainSubstrate, refuseProcessHeldDataStorageDiskStage)},
				{Name: "fresh-exact-head-install", DependsOn: []string{"refuse-process-held-raw-disk"}, Run: cpuDiagnosticStage(failureDomainForgeHandoff, runOverlayStage)},
				{Name: "assert-pvs-vg-substrate-and-classes", DependsOn: []string{"fresh-exact-head-install"}, Run: cpuDiagnosticStage(failureDomainSubstrate, assertCurrentPlatformStage)},
				{Name: "setup-two-worker-rwo-agentpool", DependsOn: []string{"assert-pvs-vg-substrate-and-classes"}, Run: cpuDiagnosticStage(failureDomainDependentSmoke, setupLVMSharedAgentPoolStage)},
				{Name: "install-real-workspace-execution-fixture", DependsOn: []string{"setup-two-worker-rwo-agentpool"}, Run: cpuDiagnosticStage(failureDomainDependentSmoke, setupWorkspaceExecutionFixtureStage)},
				{Name: "run-authenticated-concurrent-isolated-work", DependsOn: []string{"install-real-workspace-execution-fixture"}, Run: cpuDiagnosticStage(failureDomainDependentSmoke, exerciseConcurrentWorkspaceWorkStage)},
				{Name: "cross-capacity-floor-during-active-turn", DependsOn: []string{"run-authenticated-concurrent-isolated-work"}, Run: cpuDiagnosticStage(failureDomainDependentSmoke, exerciseActiveWorkspaceCapacityStage)},
				{Name: "prove-aggregate-vg-pressure-and-new-claim-exhaustion", DependsOn: []string{"cross-capacity-floor-during-active-turn"}, Run: cpuDiagnosticStage(failureDomainSubstrate, exerciseAggregateVGCapacityStage)},
				{Name: "resume-human-gated-session-after-worker-replacement", DependsOn: []string{"prove-aggregate-vg-pressure-and-new-claim-exhaustion"}, Run: cpuDiagnosticStage(failureDomainDependentSmoke, exerciseHumanGateWorkspaceReplacementStage)},
				{Name: "seed-committed-workspace-bytes", DependsOn: []string{"resume-human-gated-session-after-worker-replacement"}, Run: cpuDiagnosticStage(failureDomainSubstrate, seedLVMReapplyStage)},
				{Name: "grow-mounted-general-xfs-in-place", DependsOn: []string{"seed-committed-workspace-bytes"}, Run: cpuDiagnosticStage(failureDomainSubstrate, growGeneralLVMClaimStage)},
				{Name: "reboot-with-unchanged-storage-identities", DependsOn: []string{"grow-mounted-general-xfs-in-place"}, Run: cpuDiagnosticStage(failureDomainSubstrate, rebootPreservesLVMStorageStage)},
				{Name: "reapply-with-unchanged-identities", DependsOn: []string{"reboot-with-unchanged-storage-identities"}, Run: cpuDiagnosticStage(failureDomainForgeReconcile, reapplyCurrentPlatformStage)},
				{Name: "assert-persisted-bytes", DependsOn: []string{"reapply-with-unchanged-identities"}, Run: cpuDiagnosticStage(failureDomainSubstrate, assertLVMReapplyStage)},
				{Name: "delete-general-claim-without-leaked-lv", DependsOn: []string{"assert-persisted-bytes"}, Run: cpuDiagnosticStage(failureDomainSubstrate, deleteLVMClaimStage)},
				{Name: "ordinary-destroy-preserves-data-vg", DependsOn: []string{"delete-general-claim-without-leaked-lv"}, Run: cpuDiagnosticStage(failureDomainForgeReconcile, destroyPreservesDataStorageStage)},
			},

			Diagnostics: cpuScenarioDiagnostics(), Cleanup: cpuScenarioCleanup(),
		}),
		sharede2e.Define(sharede2e.Scenario[*gpuFixtureState]{
			Metadata: forgeScenarioMetadata(
				gpuScenarioName,
				"Bootstraps a fresh GPU host and proves Forge GPU readiness, exact artifact handoff, managed-PVC model-cache seeding and one real-serving completion; the optional driver-upgrade stage proves an emptyDir-safe driver transition when driver inputs change and in full validation.",
				sharede2e.TierF3,
				[]string{"HOR-411", "HOR-406", "HOR-481", "HOR-485", "HOR-494", "HOR-557", "HOR-590", "DES-HOR-545-02", "DES-HOR-545-07"},
				[]string{"forge", "control-plane", "control-plane-chart", "iterabase-platform-chart"},
				"test-e2e-gpu", 90, "gpu",
			),
			NewState: newGPUFixtureState,
			Stages: []sharede2e.Stage[*gpuFixtureState]{
				{Name: "prepare-fresh-host", Run: gpuDiagnosticStage(failureDomainFixturePrepare, prepareGPUFixtureStage)},
				{Name: "apply-gpu-substrate", DependsOn: []string{"prepare-fresh-host"}, Run: gpuDiagnosticStage(failureDomainSubstrate, applyGPUSubstrateStage)},
				{Name: "assert-gpu-smoke", DependsOn: []string{"apply-gpu-substrate"}, Run: gpuDiagnosticStage(failureDomainSubstrate, assertGPUSmokeStage)},
				{Name: "apply-dependent-platform-smoke", DependsOn: []string{"assert-gpu-smoke"}, Run: gpuDiagnosticStage(failureDomainForgeHandoff, applyInferencePlatformStage)},
				{Name: "run-real-serving-smoke", DependsOn: []string{"apply-dependent-platform-smoke"}, Run: gpuDiagnosticStage(failureDomainDependentSmoke, runInferenceGPUStage)},
				{Name: gpuDriverUpgradeStage, DependsOn: []string{"run-real-serving-smoke"}, Optional: true, Run: gpuDiagnosticStage(failureDomainSubstrate, gpuDriverUpgradeStageRun)},
			},
			Diagnostics: gpuScenarioDiagnostics(), Cleanup: gpuScenarioCleanup(),
		}),
	)
	suite.Run(t)
}

func forgeScenarioMetadata(name, description string, tier sharede2e.Tier, references, targets []string, makeTarget string, timeout int, capacity string) sharede2e.ScenarioMetadata {
	artifacts := []string{"forge-binary", "control-plane-chart", "iterabase-platform-chart", "cert-manager-substrate-chart", "lvm-storage-substrate-chart", "control-plane-image", "tool-runner-image", "inference-gateway-image"}
	if name == cpuScenarioName || name == cpuWorkspaceScenarioName {
		artifacts = append(artifacts, "harness-image")
	}
	if name == cpuWorkspaceScenarioName {
		artifacts = append(artifacts, "runtime-fixture-image")
	}
	return sharede2e.ScenarioMetadata{
		Name: name, Description: description, Tier: tier,
		References: references, ReleaseTargets: targets, RequiredArtifacts: artifacts,
		Intents:      []sharede2e.ExecutionIntent{sharede2e.IntentPR, sharede2e.IntentCandidate},
		FixtureModes: []sharede2e.FixtureMode{sharede2e.FixtureSource, sharede2e.FixtureCandidate},
		MakeTarget:   makeTarget, TimeoutMinutes: timeout, Capacity: capacity, Mandatory: capacity != "",
		// Real-machine scenarios run on pull requests for Forge changes only; the
		// merge queue selects them for any artifact they deploy (DES-HOR-590-02).
		SelectedBy: []string{"forge-binary"},
		Smoke:      name == cpuScenarioName,
	}
}

func TestGPUScenarioSelectsEveryChartRuntimeImage(t *testing.T) {
	metadata := forgeScenarioMetadata(gpuScenarioName, "gpu", sharede2e.TierF3, nil, nil, "test-e2e-gpu", 110, "gpu")
	for _, artifact := range []string{"control-plane-image", "inference-gateway-image", "tool-runner-image"} {
		if !slices.Contains(metadata.RequiredArtifacts, artifact) {
			t.Fatalf("GPU scenario does not select chart runtime artifact %q: %v", artifact, metadata.RequiredArtifacts)
		}
	}
}

type hermeticExampleState struct{ events []string }

func hermeticExampleScenario() sharede2e.Definition {
	return sharede2e.Define(sharede2e.Scenario[*hermeticExampleState]{
		Metadata: sharede2e.ScenarioMetadata{
			Name: "hermetic-example", Description: "Proves the Forge suite composes typed dependent stages without infrastructure.",
			Tier: sharede2e.TierF0, References: []string{"HOR-476"},
			FixtureModes: []sharede2e.FixtureMode{sharede2e.FixtureSource, sharede2e.FixtureCandidate, sharede2e.FixturePublished},
		},
		NewState: func(*testing.T) *hermeticExampleState { return &hermeticExampleState{} },
		Stages: []sharede2e.Stage[*hermeticExampleState]{
			{Name: "arrange", Run: func(_ *testing.T, state *hermeticExampleState) { state.events = append(state.events, "arranged") }},
			{Name: "assert", DependsOn: []string{"arrange"}, Run: func(t *testing.T, state *hermeticExampleState) {
				if !slices.Equal(state.events, []string{"arranged"}) {
					t.Fatalf("events = %v", state.events)
				}
			}},
		},
	})
}
