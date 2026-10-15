package metrics

import "github.com/prometheus/client_golang/prometheus"

// registerArtifactMetrics builds the V2 artifact collectors (architecture
// section 17). Labels are bounded outcome/reason/stage codes only: never an
// artifact id, digest, filename, uploader, query, or object key.
func registerArtifactMetrics(m *Metrics) {
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	}
	gauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, nil)
	}
	m.ArtifactUploads = counter("control_plane_artifact_uploads_total",
		"Artifact admissions into quarantine by channel and bounded result.", "channel", "result")
	m.ArtifactUploadBytes = counter("control_plane_artifact_upload_bytes_total",
		"Bytes committed into quarantine by channel.", "channel")
	m.ArtifactSecurityAttempts = counter("control_plane_artifact_security_attempts_total",
		"Security attempt outcomes: cleared, malware, policy, transient, or lease_lost.", "outcome")
	m.ArtifactScanDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "control_plane_artifact_scan_duration_seconds", Help: "Security attempt duration by outcome.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"outcome"})
	m.ArtifactSecurityQueue = gauge("control_plane_artifact_security_queue_depth",
		"Queued or leased security attempts.")
	m.ArtifactSecurityOldest = gauge("control_plane_artifact_security_queue_oldest_seconds",
		"Age of the oldest open security attempt.")
	m.ArtifactQuarantineBytes = gauge("control_plane_artifact_quarantine_bytes",
		"Committed bytes awaiting security clearance.")
	m.ArtifactQuarantineObjects = gauge("control_plane_artifact_quarantine_artifacts",
		"Artifacts awaiting security clearance.")
	m.ArtifactQuarantineLimit = gauge("control_plane_artifact_quarantine_limit_bytes",
		"Configured quarantine backlog above which uploads are refused.")
	m.ArtifactRegistered = gauge("control_plane_artifact_registered_artifacts",
		"Artifacts currently registered in the shared library.")
	m.ArtifactDeletions = counter("control_plane_artifact_deletions_total",
		"Deletion lifecycle events: requested, blocked, completed, retried, retention_blocked.", "result")
	m.ArtifactDeleting = gauge("control_plane_artifact_deleting_artifacts",
		"Artifacts whose bytes are still being purged.")
	m.ArtifactDeletingOldest = gauge("control_plane_artifact_deleting_oldest_seconds",
		"Age of the oldest unfinished deletion.")
	m.ArtifactPurgedObjects = counter("control_plane_artifact_purged_objects_total",
		"Objects purged by zone class.", "zone")
	m.ArtifactReconciliations = counter("control_plane_artifact_reconciliations_total",
		"Reconciliation actions by bounded kind.", "kind")
	m.ArtifactScannerReady = gauge("control_plane_artifact_scanner_ready",
		"Whether the scanner adapter can issue a clean verdict (engine self-test and fresh signatures).")
	m.ArtifactSignatureAge = gauge("control_plane_artifact_signature_age_seconds",
		"Age of the scanner's validated signature bundle.")
	m.ArtifactScans = counter("control_plane_artifact_scans_total",
		"Scanner adapter verdicts: clean, infected, rejected, unready, or error.", "verdict")
}
