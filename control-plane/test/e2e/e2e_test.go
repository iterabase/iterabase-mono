package e2e_test

import (
	"slices"
	"testing"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
)

type exampleState struct{ events []string }

// TestE2E is the control-plane owner's single compiled suite entrypoint.
func TestE2E(t *testing.T) {
	suite := sharede2e.NewSuite(sharede2e.SuiteMetadata{
		Name: "control-plane", Owner: "control-plane", Entrypoint: "control-plane/test/e2e",
	}, sharede2e.FixtureFromEnv)
	suite.Add(
		hermeticExampleScenario(),
		deployedControlPlaneScenario(),
		deployedExecutionContractsScenario(),
	)
	suite.Run(t)
}

func deployedMetadata(name, description, makeTarget string, timeout int, references []string) sharede2e.ScenarioMetadata {
	return sharede2e.ScenarioMetadata{
		Name: name, Description: description, Tier: sharede2e.TierF2,
		References: references,
		RequiredArtifacts: []string{
			"control-plane-image", "control-plane-chart", "iterabase-platform-chart", "cert-manager-substrate-chart", "lvm-storage-substrate-chart",
		},
		FixtureModes: []sharede2e.FixtureMode{sharede2e.FixtureSource},
		MakeTarget:   makeTarget, TimeoutMinutes: timeout,
	}
}

func deployedExecutionMetadata(name, description, makeTarget string, timeout int, references []string) sharede2e.ScenarioMetadata {
	metadata := deployedMetadata(name, description, makeTarget, timeout, references)
	metadata.RequiredArtifacts = append(metadata.RequiredArtifacts,
		"harness-image", "tool-runner-image", "inference-gateway-image", "inference-gateway-chart", "runtime-fixture-image")
	return metadata
}

// deployedControlPlaneScenario installs the control plane once and runs the
// identity, work, artifact and browser journeys against it (C5). Each journey
// depends only on readiness, so one failing journey does not block the others.
func deployedControlPlaneScenario() sharede2e.Definition {
	diagnostics, cleanup := deployedBrowserScenarioHooks()
	metadata := deployedMetadata(
		"deployed-control-plane",
		"Installs the source-built control plane once on fresh Kind, then proves the IdentityMapping delegated-token path, JWKS across restart and revocation; concurrent work, projections, durable restart and SSE reconnect; artifact publication, durability and tombstones; and locked Chromium journeys.",
		"test-e2e-deployed", 50,
		[]string{"HOR-478", "HOR-483", "HOR-490", "HOR-590", "REQ-005", "REQ-009", "REQ-010", "REQ-018", "REQ-019", "REQ-020", "REQ-023", "REQ-024", "REQ-025", "REQ-033", "REQ-041", "REQ-043",
			"SCN-004", "SCN-005", "SCN-006", "SCN-007", "SCN-008", "SCN-009", "SCN-019", "SCN-020"},
	)
	metadata.Smoke = true
	return sharede2e.Define(sharede2e.Scenario[*deployedState]{
		Metadata: metadata,
		NewState: newDeployedState,
		Stages: []sharede2e.Stage[*deployedState]{
			{Name: "create-kind", Run: createControlPlaneKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "install-certificate-substrate", DependsOn: []string{"import-runtime-images"}, Run: installCertificateSubstrateStage},
			{Name: "install-lvm-storage-substrate", DependsOn: []string{"install-certificate-substrate"}, Run: installLVMStorageSubstrateStage},
			{Name: "install-control-plane", DependsOn: []string{"install-lvm-storage-substrate"}, Run: installControlPlanePlatformStage},
			{Name: "assert-deployment-ready", DependsOn: []string{"install-control-plane"}, Run: assertDeploymentReadyStage},
			{Name: "exercise-identity-api", DependsOn: []string{"assert-deployment-ready"}, Run: exerciseIdentityAPIStage},
			{Name: "setup-work-journey", DependsOn: []string{"assert-deployment-ready"}, Run: setupWorkJourneyStage},
			{Name: "start-concurrently", DependsOn: []string{"setup-work-journey"}, Run: concurrentIdempotentStartStage},
			{Name: "exercise-work-commands", DependsOn: []string{"start-concurrently"}, Run: workCommandsAndHistoryStage},
			{Name: "restart-and-reconnect", DependsOn: []string{"exercise-work-commands"}, Run: restartAndReconnectStage},
			{Name: "setup-artifact-journey", DependsOn: []string{"assert-deployment-ready"}, Run: setupArtifactJourneyStage},
			{Name: "upload-publish-and-link", DependsOn: []string{"setup-artifact-journey"}, Run: uploadPublishAndLinkArtifactStage},
			{Name: "restart-artifact-processes", DependsOn: []string{"upload-publish-and-link"}, Run: restartArtifactProcessesStage},
			{Name: "delete-and-assert-tombstone", DependsOn: []string{"restart-artifact-processes"}, Run: deleteAndAssertTombstoneStage},
			{Name: "setup-browser-fixtures", DependsOn: []string{"assert-deployment-ready"}, Run: setupBrowserFixturesStage},
			{Name: "run-playwright-journeys", DependsOn: []string{"setup-browser-fixtures"}, Run: runPlaywrightJourneysStage},
		},
		Diagnostics: diagnostics, Cleanup: cleanup,
	})
}

func deployedExecutionContractsScenario() sharede2e.Definition {
	diagnostics, cleanup := deployedScenarioHooks()
	return sharede2e.Define(sharede2e.Scenario[*deployedState]{
		Metadata: deployedExecutionMetadata(
			"deployed-execution-contracts",
			"Proves source-built AgentPool, dispatch, disposable harness children, model/tool gateways, immutable tools and artifacts, consequences, isolation, and durable recovery on fresh Kind.",
			"test-e2e-execution", 55,
			[]string{"HOR-477", "HOR-438", "HOR-488", "HOR-489", "HOR-545", "REQ-005", "REQ-009", "REQ-010", "REQ-018", "REQ-035", "SCN-008", "SCN-009", "SCN-012"},
		),
		NewState: newDeployedState,
		Stages: []sharede2e.Stage[*deployedState]{
			{Name: "create-kind", Run: createControlPlaneKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "install-certificate-substrate", DependsOn: []string{"import-runtime-images"}, Run: installCertificateSubstrateStage},
			{Name: "install-lvm-storage-substrate", DependsOn: []string{"install-certificate-substrate"}, Run: installLVMStorageSubstrateStage},
			{Name: "install-execution-fixtures", DependsOn: []string{"install-lvm-storage-substrate"}, Run: installExecutionFixtureStage},
			{Name: "install-execution-platform", DependsOn: []string{"install-execution-fixtures"}, Run: installExecutionPlatformStage},
			{Name: "assert-execution-platform-ready", DependsOn: []string{"install-execution-platform"}, Run: assertExecutionPlatformReadyStage},
			{Name: "setup-execution-resources", DependsOn: []string{"assert-execution-platform-ready"}, Run: setupExecutionResourcesStage},
			{Name: "exercise-agentpool-late-secret-recovery", DependsOn: []string{"setup-execution-resources"}, Run: exerciseAgentPoolLateSecretRecoveryStage},
			{Name: "exercise-worker-loss-cancellation", DependsOn: []string{"exercise-agentpool-late-secret-recovery"}, Run: exerciseWorkerLossCancellationStage},
			{Name: "exercise-concurrent-same-pool-rwo", DependsOn: []string{"exercise-worker-loss-cancellation"}, Run: exerciseConcurrentSamePoolWorkStage},
			{Name: "exercise-immutable-tool-generation", DependsOn: []string{"exercise-concurrent-same-pool-rwo"}, Run: exerciseImmutableToolGenerationStage},
			{Name: "exercise-representative-execution", DependsOn: []string{"exercise-immutable-tool-generation"}, Run: exerciseRepresentativeExecutionStage},
			{Name: "exercise-idempotent-invocation-race", DependsOn: []string{"exercise-representative-execution"}, Run: exerciseIdempotentInvocationRaceStage},
			{Name: "exercise-isolation-composition", DependsOn: []string{"exercise-idempotent-invocation-race"}, Run: exerciseIsolationCompositionStage},
			{Name: "exercise-outcome-unknown-recovery", DependsOn: []string{"exercise-isolation-composition"}, Run: exerciseOutcomeUnknownRecoveryStage},
			{Name: "exercise-consequence-confirmation", DependsOn: []string{"exercise-outcome-unknown-recovery"}, Run: exerciseConsequenceConfirmationStage},
		},
		Diagnostics: diagnostics, Cleanup: cleanup,
	})
}

func hermeticExampleScenario() sharede2e.Definition {
	return sharede2e.Define(sharede2e.Scenario[*exampleState]{
		Metadata: sharede2e.ScenarioMetadata{
			Name: "hermetic-example", Description: "Proves the control-plane suite composes typed dependent stages without infrastructure.",
			Tier: sharede2e.TierF0, References: []string{"HOR-476"},
			FixtureModes: []sharede2e.FixtureMode{sharede2e.FixtureSource},
		},
		NewState: func(*testing.T) *exampleState { return &exampleState{} },
		Stages: []sharede2e.Stage[*exampleState]{
			{Name: "arrange", Run: func(_ *testing.T, state *exampleState) { state.events = append(state.events, "arranged") }},
			{Name: "assert", DependsOn: []string{"arrange"}, Run: func(t *testing.T, state *exampleState) {
				if !slices.Equal(state.events, []string{"arranged"}) {
					t.Fatalf("events = %v", state.events)
				}
			}},
		},
	})
}
