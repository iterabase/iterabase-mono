package e2e_test

import (
	"fmt"
	"strings"
	"testing"
)

func TestSelectBootstrapEvidencePodUsesExactNewSuccessfulPod(t *testing.T) {
	old := successfulBootstrapEvidencePod("api-old", "uid-old")
	newPod := successfulBootstrapEvidencePod("api-new", "uid-new")
	terminating := successfulBootstrapEvidencePod("api-terminating", "uid-terminating")
	deletingAt := "2026-08-16T19:00:00Z"
	terminating.Metadata.DeletionTimestamp = &deletingAt

	for name, pods := range map[string][]bootstrapEvidencePod{
		"old-first": {old, terminating, newPod},
		"new-first": {newPod, old, terminating},
	} {
		t.Run(name, func(t *testing.T) {
			selected, err := selectBootstrapEvidencePod(pods, map[string]struct{}{"uid-old": {}})
			if err != nil {
				t.Fatal(err)
			}
			if selected.Metadata.UID != "uid-new" {
				t.Fatalf("selected pod UID=%q want uid-new", selected.Metadata.UID)
			}
		})
	}
}

func TestSelectBootstrapEvidencePodRejectsStaleOrIncompleteCandidates(t *testing.T) {
	deletingAt := "2026-08-16T19:00:00Z"
	terminating := successfulBootstrapEvidencePod("api-terminating", "uid-terminating")
	terminating.Metadata.DeletionTimestamp = &deletingAt
	failed := successfulBootstrapEvidencePod("api-failed", "uid-failed")
	failed.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
	failed.Status.InitContainerStatuses[0].State.Terminated.Reason = "Error"
	waiting := successfulBootstrapEvidencePod("api-waiting", "uid-waiting")
	waiting.Status.InitContainerStatuses[0].State.Terminated = nil
	waiting.Status.InitContainerStatuses[0].State.Waiting = &bootstrapContainerWaiting{Reason: "PodInitializing"}
	restarted := successfulBootstrapEvidencePod("api-restarted", "uid-restarted")
	restarted.Status.InitContainerStatuses[0].RestartCount = 1

	for name, testCase := range map[string]struct {
		pods     []bootstrapEvidencePod
		excluded map[string]struct{}
		want     string
	}{
		"stale": {
			pods:     []bootstrapEvidencePod{successfulBootstrapEvidencePod("api-old", "uid-old")},
			excluded: map[string]struct{}{"uid-old": {}}, want: "excluded_uids=1",
		},
		"terminating":       {pods: []bootstrapEvidencePod{terminating}, want: "deleting=true"},
		"failed-init":       {pods: []bootstrapEvidencePod{failed}, want: "terminated(exit=1,reason=Error)"},
		"incomplete-init":   {pods: []bootstrapEvidencePod{waiting}, want: "waiting(reason=PodInitializing)"},
		"restarted-success": {pods: []bootstrapEvidencePod{restarted}, want: "restart_count=1"},
		"ambiguous": {
			pods: []bootstrapEvidencePod{
				successfulBootstrapEvidencePod("api-new-a", "uid-new-a"),
				successfulBootstrapEvidencePod("api-new-b", "uid-new-b"),
			},
			want: "eligible=2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := selectBootstrapEvidencePod(testCase.pods, testCase.excluded)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("selection error=%v, want substring %q", err, testCase.want)
			}
		})
	}
}

func TestBootstrapOutputPrintsNoCredential(t *testing.T) {
	pod := successfulBootstrapEvidencePod("api-new", "uid-new")
	if err := bootstrapOutputError(pod, "Bootstrap created for admin@example.com. First-time setup instructions are queued when needed; no credential is printed and no browser session is created.\n"); err != nil {
		t.Fatalf("clean bootstrap output rejected: %v", err)
	}
	for _, leaked := range []string{
		"Admin API key (scope=admin): cp-ABCDEFGHIJKLMNOPQRSTUVWXYZ234567\n",
		"Bootstrap created. https://app.example.com/auth/setup?token=AbCdEfGh12345678\n",
	} {
		err := bootstrapOutputError(pod, leaked)
		if err == nil {
			t.Fatalf("bootstrap output with a credential was accepted: %q", leaked)
		}
		if strings.Contains(err.Error(), "cp-ABCDEF") || strings.Contains(err.Error(), "AbCdEfGh") {
			t.Fatalf("bootstrap error leaked credential output: %s", err)
		}
		for _, want := range []string{`pod="api-new"`, `uid="uid-new"`, fmt.Sprintf("output_bytes=%d", len(leaked))} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("bootstrap error %q does not contain %q", err, want)
			}
		}
	}
	if err := bootstrapOutputError(pod, ""); err == nil {
		t.Fatal("empty bootstrap output was accepted as evidence")
	}
}

func successfulBootstrapEvidencePod(name, uid string) bootstrapEvidencePod {
	controller := true
	pod := bootstrapEvidencePod{
		Metadata: bootstrapEvidenceMetadata{
			Name: name, UID: uid, CreationTimestamp: "2026-08-16T18:00:00Z",
			OwnerReferences: []bootstrapOwnerReference{{
				Kind: "ReplicaSet", Name: "api-rs", UID: "rs-uid", Controller: &controller,
			}},
		},
		Status: bootstrapEvidenceStatus{
			Phase:      "Running",
			Conditions: []bootstrapPodCondition{{Type: "Ready", Status: "True"}},
			InitContainerStatuses: []bootstrapInitContainerStatus{{
				Name: "bootstrap",
				State: bootstrapContainerState{Terminated: &bootstrapContainerTerminated{
					ExitCode: 0, Reason: "Completed",
				}},
			}},
		},
	}
	return pod
}
