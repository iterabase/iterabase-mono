package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkflowSpec defines one immutable, operator-authored customer workflow.
// Workflow execution is a single-active-node directed graph (ARCH-019): agent
// tasks use gateway tools as capabilities, while human gates pause the same
// attempt for a customer response. There is deliberately no top-level
// tool_call node.
// +kubebuilder:object:generate=true
type WorkflowSpec struct {
	// key is the stable logical workflow key. It excludes ':' because the
	// cross-schema definition identity is "<key>:<version>".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[^:]+$`
	Key string `json:"key"`

	// version is the immutable version component. Content changes require a new
	// version and never mutate an existing definition.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[^:]+$`
	Version string `json:"version"`

	// poolRef names the AgentPool security/integration boundary.
	// +kubebuilder:validation:Required
	PoolRef string `json:"poolRef"`

	// defaultModelRef is the Model natural key used by agent nodes that do not
	// declare a narrower override. It is required when the graph has agent nodes.
	// +optional
	DefaultModelRef string `json:"defaultModelRef,omitempty"`

	// source defines the installed source adapter and non-secret trigger routes.
	// +kubebuilder:validation:Required
	Source WorkflowSource `json:"source"`

	// graph is the immutable executable workflow graph.
	// +kubebuilder:validation:Required
	Graph WorkflowGraph `json:"graph"`

	// skills are exact immutable overlay skill identities available to the
	// workflow. Agent nodes may expose a subset by logical name.
	// +optional
	Skills []SkillReference `json:"skills,omitempty"`

	// requestedCapabilities are the workflow-level maximum gateway capabilities.
	// Agent nodes may expose a subset by tool name and can never widen this set.
	// +optional
	RequestedCapabilities []RequestedCapability `json:"requestedCapabilities,omitempty"`

	// valueModelRef references an operator-configured transparent value model.
	// +optional
	ValueModelRef string `json:"valueModelRef,omitempty"`

	// title is the localized customer-facing workflow name.
	// +kubebuilder:validation:Required
	Title RequiredLocalizedText `json:"title"`

	// coworker is the customer-facing identity of the AI coworker that runs
	// this workflow. What it always asks first is never declared here: it is
	// derived from the graph's human_gate nodes.
	// +kubebuilder:validation:Required
	Coworker CoworkerSpec `json:"coworker"`
}

// WorkflowGraph is a directed graph with one active node at a time. Every
// declared node outcome (including a wait node's implicit outcomes) must have
// exactly one edge or terminal declaration.
// Cycles are legal; maxTransitions is the non-convergence safety bound.
// +kubebuilder:object:generate=true
type WorkflowGraph struct {
	// entryNode is the first node for every initial or revised attempt.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EntryNode string `json:"entryNode"`

	// maxTransitions bounds edge traversals in one attempt.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +kubebuilder:default=100
	MaxTransitions int32 `json:"maxTransitions,omitempty"`

	// nodes are immutable node definitions keyed within this graph.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Nodes []WorkflowNode `json:"nodes"`

	// edges deterministically map a reported outcome to the next node.
	// +optional
	Edges []WorkflowEdge `json:"edges,omitempty"`

	// terminalOutcomes complete the attempt as Done without claiming correctness.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	TerminalOutcomes []WorkflowTerminalOutcome `json:"terminalOutcomes"`
}

// WorkflowNode is an agent task, a customer-actionable human gate, or a
// durable wait for correlated external events.
// +kubebuilder:object:generate=true
type WorkflowNode struct {
	// key is unique within the graph and stable across repeated visits.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// label is the localized business-stage name shown to customers. Both v1
	// locales are required by controller validation.
	// +kubebuilder:validation:Required
	Label LocalizedText `json:"label"`

	// kind selects agent execution, a durable customer blocker, or a durable
	// wait for external events that holds no worker.
	// +kubebuilder:validation:Enum=agent_task;human_gate;wait
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// prompt is required for agent_task and forbidden for human_gate.
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// modelRef optionally overrides spec.defaultModelRef for this agent node.
	// +optional
	ModelRef string `json:"modelRef,omitempty"`

	// skills narrows spec.skills by logical name for this agent node.
	// +optional
	Skills []string `json:"skills,omitempty"`

	// capabilities narrows spec.requestedCapabilities by tool name for this
	// agent node. Empty means expose none, not inherit all.
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// workspaceTools requests the fixed read/write/edit/bash set. The referenced
	// AgentPool must enable it; a workflow/node can never widen the pool.
	// +optional
	WorkspaceTools bool `json:"workspaceTools,omitempty"`

	// timeout bounds one node execution. Zero uses the runtime default.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// outcomes are the only values complete_step (or a human response) may
	// emit. Required for agent_task and human_gate. A wait node's outcomes are
	// the reserved received, deadline, and cancelled; omit them or list exactly
	// that set.
	// +optional
	Outcomes []string `json:"outcomes,omitempty"`

	// outputSchema validates complete_step.output for agent_task nodes. It is a
	// JSON Schema document and defaults to an unconstrained object when absent.
	// Terminal agent tasks may omit it for an artifact-only outcome, or declare
	// a closed direct object schema aligned with resultPresentation.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	OutputSchema *apiextensionsv1.JSON `json:"outputSchema,omitempty"`

	// resultPresentation defines the immutable, localized outcome receipt for
	// terminal outcomes. Controller validation requires it on every terminal
	// node and aligns its fields and controlled values with the node schema.
	// +optional
	ResultPresentation *ResultPresentation `json:"resultPresentation,omitempty"`

	// humanGate is required for human_gate and forbidden otherwise.
	// +optional
	HumanGate *HumanGateSpec `json:"humanGate,omitempty"`

	// wait is required for wait and forbidden otherwise.
	// +optional
	Wait *WaitSpec `json:"wait,omitempty"`
}

// WaitSpec declares the correlated external events a wait node collects
// without holding a worker, its completion rule, and its deadline.
// +kubebuilder:object:generate=true
type WaitSpec struct {
	// events names each expected event source and where its correlation keys
	// come from. A key path resolving to N strings expects N events.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Events []WaitEventSpec `json:"events"`

	// completion is all (every expected event) or any (the first one).
	// +kubebuilder:validation:Enum=all;any
	// +kubebuilder:validation:Required
	Completion string `json:"completion"`

	// deadline bounds the wait; exactly one of after or at is required.
	// +kubebuilder:validation:Required
	Deadline WaitDeadline `json:"deadline"`
}

// WaitEventSpec is one expected event source and its correlation-key origin.
// +kubebuilder:object:generate=true
type WaitEventSpec struct {
	// source is the event source that posts by correlation key, for example
	// email_reply.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]{0,62}$`
	Source string `json:"source"`

	// correlation names an earlier node's validated output path holding one
	// string or an array of strings.
	// +kubebuilder:validation:Required
	Correlation OutputReference `json:"correlation"`
}

// WaitDeadline is relative to entering the node, or an absolute RFC3339 time
// produced by an earlier step. Both are bounded to 1m-90d from entry.
// +kubebuilder:object:generate=true
type WaitDeadline struct {
	// +optional
	After *metav1.Duration `json:"after,omitempty"`
	// +optional
	At *OutputReference `json:"at,omitempty"`
}

// OutputReference addresses a value in the latest successful output of an
// earlier node in the same attempt. "*" expands every array item.
// +kubebuilder:object:generate=true
type OutputReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Path []string `json:"path"`
}

// ResultPresentation is the approved customer projection for a terminal node.
// Machine outcomes, schema keys, and controlled values never serve as labels.
// +kubebuilder:object:generate=true
type ResultPresentation struct {
	// outcomes provides one localized completion summary for each terminal
	// outcome declared for this node.
	// +kubebuilder:validation:MinItems=1
	Outcomes []ResultOutcomePresentation `json:"outcomes"`

	// fields presents every structured-output property by its path. Paths retain
	// object/array hierarchy without relying on raw schema keys as UI labels.
	// +optional
	Fields []ResultFieldPresentation `json:"fields,omitempty"`
}

// ResultOutcomePresentation localizes one terminal outcome receipt heading.
// +kubebuilder:object:generate=true
type ResultOutcomePresentation struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Outcome string `json:"outcome"`
	// +kubebuilder:validation:Required
	Summary LocalizedText `json:"summary"`
}

// ResultFieldPresentation localizes one structured-result property path.
// Array item objects use the same property path as one representative item;
// array indexes never enter the immutable presentation contract.
// +kubebuilder:object:generate=true
type ResultFieldPresentation struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Path []string `json:"path"`
	// +kubebuilder:validation:Required
	Label LocalizedText `json:"label"`
	// options provides localized labels for every enum value at this field (or
	// at the item schema for an array of controlled values).
	// +optional
	Options []ResultValuePresentation `json:"options,omitempty"`
}

// ResultValuePresentation localizes one exact JSON-Schema enum value.
// +kubebuilder:object:generate=true
type ResultValuePresentation struct {
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Required
	Value apiextensionsv1.JSON `json:"value"`
	// +kubebuilder:validation:Required
	Label LocalizedText `json:"label"`
}

// HumanGateSpec defines a business-readable blocker and its response contract.
// +kubebuilder:object:generate=true
type HumanGateSpec struct {
	// type is the customer action required.
	// +kubebuilder:validation:Enum=information;decision;approval;artifact
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// title and description are localized customer-facing copy.
	// +kubebuilder:validation:Required
	Title LocalizedText `json:"title"`
	// +kubebuilder:validation:Required
	Description LocalizedText `json:"description"`

	// responseSchema validates the response payload. V1 Dashboard forms accept
	// an object with direct properties of type string, boolean, number, integer,
	// unconstrained object, or unconstrained array (or enum). Validation keywords
	// the form cannot enforce—including nested schemas, ranges, patterns,
	// composition, and references—are rejected.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	ResponseSchema *apiextensionsv1.JSON `json:"responseSchema,omitempty"`

	// presentation supplies localized labels for every outcome and every
	// customer-visible response field/enum option.
	// +kubebuilder:validation:Required
	Presentation HumanGatePresentation `json:"presentation"`
}

// HumanGatePresentation keeps machine outcomes/schema keys out of customer UI.
// Labels follow the declaration order of WorkflowNode.outcomes and enum values.
// +kubebuilder:object:generate=true
type HumanGatePresentation struct {
	// +kubebuilder:validation:MinItems=1
	Outcomes []LocalizedText `json:"outcomes"`
	// +optional
	Fields []HumanGateFieldPresentation `json:"fields,omitempty"`
}

// HumanGateFieldPresentation localizes one top-level response-schema property.
// +kubebuilder:object:generate=true
type HumanGateFieldPresentation struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
	// +kubebuilder:validation:Required
	Label LocalizedText `json:"label"`
	// options follows the field enum order when the schema declares one.
	// +optional
	Options []LocalizedText `json:"options,omitempty"`
}

// LocalizedText carries approved English/Portuguese customer copy.
// +kubebuilder:object:generate=true
type LocalizedText struct {
	// +optional
	EN string `json:"en,omitempty"`
	// +optional
	PT string `json:"pt,omitempty"`
}

// WorkflowEdge maps exactly one node outcome to a target node.
// +kubebuilder:object:generate=true
type WorkflowEdge struct {
	// +kubebuilder:validation:Required
	From string `json:"from"`
	// +kubebuilder:validation:Required
	Outcome string `json:"outcome"`
	// +kubebuilder:validation:Required
	To string `json:"to"`
}

// WorkflowTerminalOutcome marks one node outcome as attempt completion.
// +kubebuilder:object:generate=true
type WorkflowTerminalOutcome struct {
	// +kubebuilder:validation:Required
	Node string `json:"node"`
	// +kubebuilder:validation:Required
	Outcome string `json:"outcome"`
}

// WorkflowSource defines the source adapter and trigger bindings.
// +kubebuilder:object:generate=true
type WorkflowSource struct {
	// +kubebuilder:validation:Enum=graph_email;operator_artifact;manual_api
	// +kubebuilder:validation:Required
	Type string `json:"type"`
	// +optional
	TriggerBindings []TriggerBinding `json:"triggerBindings,omitempty"`
}

// TriggerBinding is one typed, non-secret source route.
// +kubebuilder:object:generate=true
type TriggerBinding struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +optional
	GraphEmail *GraphEmailTriggerBinding `json:"graphEmail,omitempty"`
	// +optional
	OperatorArtifact *OperatorArtifactTriggerBinding `json:"operatorArtifact,omitempty"`
}

// GraphEmailTriggerBinding is a non-secret Graph mailbox route.
// +kubebuilder:object:generate=true
type GraphEmailTriggerBinding struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=3
	MailboxAddress string `json:"mailboxAddress"`
}

// OperatorArtifactTriggerBinding is a non-secret exported-artifact route.
// +kubebuilder:object:generate=true
type OperatorArtifactTriggerBinding struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SourceID string `json:"sourceID"`
}

// SkillReference identifies one immutable overlay skill.
// +kubebuilder:object:generate=true
type SkillReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Digest string `json:"digest"`
}

// RequestedCapability is one workflow-level gateway capability ceiling.
// +kubebuilder:object:generate=true
type RequestedCapability struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Tool string `json:"tool"`
	// +kubebuilder:validation:Enum=read_only;idempotent_write;non_idempotent_write
	// +kubebuilder:validation:Required
	MaxEffectClass string `json:"maxEffectClass"`
	// +optional
	Actions []string `json:"actions,omitempty"`
}

// RequiredLocalizedText is approved English/Portuguese customer copy where
// both locales are mandatory.
// +kubebuilder:object:generate=true
type RequiredLocalizedText struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EN string `json:"en"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PT string `json:"pt"`
}

// CoworkerSpec is the customer-facing AI coworker identity for a workflow.
// +kubebuilder:object:generate=true
type CoworkerSpec struct {
	// name is the coworker's proper name. It is never translated.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	Name string `json:"name"`

	// mark is the coworker's visual identity.
	// +kubebuilder:validation:Required
	Mark CoworkerMark `json:"mark"`

	// role is the one-line localized job title.
	// +kubebuilder:validation:Required
	Role RequiredLocalizedText `json:"role"`

	// summary is the localized description of what the coworker does.
	// +kubebuilder:validation:Required
	Summary RequiredLocalizedText `json:"summary"`

	// onItsOwn lists what the coworker does without asking, each line typed
	// by whether it reads, sends, or does something.
	// +optional
	// +kubebuilder:validation:MaxItems=12
	OnItsOwn []CoworkerOnItsOwnLine `json:"onItsOwn,omitempty"`

	// connections lists the customer systems the coworker works with.
	// +optional
	// +kubebuilder:validation:MaxItems=12
	Connections []RequiredLocalizedText `json:"connections,omitempty"`
}

// CoworkerOnItsOwnLine is one typed thing the coworker does without asking.
// +kubebuilder:object:generate=true
type CoworkerOnItsOwnLine struct {
	// text is the localized line itself.
	// +kubebuilder:validation:Required
	Text RequiredLocalizedText `json:"text"`

	// kind says whether the coworker reads, sends, or does something.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=reads;sends;does
	Kind string `json:"kind"`

	// detail is optional localized context shown under the line.
	// +optional
	Detail *RequiredLocalizedText `json:"detail,omitempty"`

	// items optionally enumerates what the line covers, such as recipients.
	// Items are literal values and are never translated.
	// +optional
	// +kubebuilder:validation:MaxItems=12
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=200
	Items []string `json:"items,omitempty"`
}

// CoworkerMark is the coworker's enumerated visual mark.
// +kubebuilder:object:generate=true
type CoworkerMark struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=round;square;petal
	Shape string `json:"shape"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=blue;green;sun;violet;teal
	Color string `json:"color"`
}

// WorkflowStatus is the reconciler-observed materialization state.
// +kubebuilder:object:generate=true
type WorkflowStatus struct {
	// +optional
	Ready bool `json:"ready,omitempty"`
	// +optional
	ValidationStatus string `json:"validationStatus,omitempty"`
	// +optional
	ValidationMessage string `json:"validationMessage,omitempty"`
	// +optional
	VersionDigest string `json:"versionDigest,omitempty"`
	// +optional
	DefinitionID string `json:"definitionID,omitempty"`
	// +optional
	ScopeIdentityID string `json:"scopeIdentityID,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=workflows,scope=Namespaced,shortName=wf
// +kubebuilder:singular=workflow
//
// Workflow is an immutable, versioned operator-authored workflow graph.
type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkflowSpec   `json:"spec,omitempty"`
	Status            WorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
//
// WorkflowList is a list of Workflow.
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

const (
	SourceGraphEmail       = "graph_email"
	SourceOperatorArtifact = "operator_artifact"
	SourceManualAPI        = "manual_api"

	WorkflowNodeAgentTask = "agent_task"
	WorkflowNodeHumanGate = "human_gate"
	WorkflowNodeWait      = "wait"

	HumanGateInformation = "information"
	HumanGateDecision    = "decision"
	HumanGateApproval    = "approval"
	HumanGateArtifact    = "artifact"

	ValidationValid   = "valid"
	ValidationInvalid = "invalid"
)
