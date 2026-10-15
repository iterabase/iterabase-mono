// Wire types of the customer work API (control-plane internal/work and
// internal/server). Customer-safe projections only.
export type Locale = "en" | "pt";
export type WorkState =
  "todo" | "in_progress" | "waiting" | "paused" | "blocked" | "done" | "failed";
export type PauseReason =
  "trial_ended" | "provider_unavailable" | "runner_offline";

export interface LocalizedText {
  en?: string;
  pt?: string;
}
export interface PresentationField {
  label: LocalizedText;
  value: string;
}
export interface SourcePresentation {
  kind: string;
  title: string;
  subtitle?: string;
  originalUrl?: string;
  evidence?: PresentationField[];
}
export type CoworkerShape = "round" | "square" | "petal";
export type CoworkerColor = "blue" | "green" | "sun" | "violet" | "teal";
export interface CoworkerMark {
  shape: CoworkerShape;
  color: CoworkerColor;
}
/** The workflow's AI coworker, declared in the overlay (DES-HOR-459-01). */
export interface Coworker {
  name: string;
  mark: CoworkerMark;
  role: LocalizedText;
  summary: LocalizedText;
  onItsOwn?: OnItsOwnLine[];
  connections?: LocalizedText[];
}
/** One thing the coworker does without asking (DES-HOR-459-09). */
export interface OnItsOwnLine {
  text: LocalizedText;
  kind: "reads" | "sends" | "does";
  detail?: LocalizedText;
  /** Plain values such as pre-approved addresses, shown as chips. */
  items?: string[];
}
/** Snapshotted from the immutable workflow definition for each attempt. */
export interface WorkPresentation {
  title: LocalizedText;
  coworker: Coworker;
}
export type StepKind = "agent_task" | "human_gate" | "wait";
/** Customer-safe workflow projection (GET /v1/workflows). */
export interface WorkflowSummary {
  key: string;
  version: string;
  title: LocalizedText;
  coworker: Coworker;
  steps: Array<{ key: string; kind: StepKind; label: LocalizedText }>;
  alwaysAsks: Array<{ nodeKey: string; type: string; title: LocalizedText }>;
}
export interface Attribution {
  id: string;
  displayName: string;
}
export interface BusinessStep {
  key: string;
  label: LocalizedText;
  state: string;
  startedAt?: string;
}
export interface BlockerSummary {
  id: string;
  kind: string;
  title: LocalizedText;
}
export interface WaitSummary {
  completion: "all" | "any";
  expected: number;
  received: number;
  deadlineAt: string;
}
export interface PauseSummary {
  reason: PauseReason;
  since: string;
}
export interface WorkItem {
  id: string;
  workflowKey: string;
  title: string;
  source: SourcePresentation;
  presentation: WorkPresentation;
  currentStep?: BusinessStep;
  blocker?: BlockerSummary;
  wait?: WaitSummary;
  pause?: PauseSummary;
  currentAttemptId: string;
  state: WorkState;
  createdAt: string;
  updatedAt: string;
  startedAt?: string;
  finishedAt?: string;
  valueConfigured: boolean;
  valueModel?: ValueModel;
  estimatedValue?: string;
  valueCurrency?: string;
  valueDisputed: boolean;
  failureSummary?: FailureSummary;
  /** Who answered the latest approval or decision (DES-HOR-459-10). */
  answeredBy?: { displayName: string };
}
export interface FailureSummary {
  code?: string;
  message?: string;
}
export interface ValueModel {
  ref?: string;
  version?: string;
  formula?: string;
  currency?: string;
  baselineSeconds?: number;
  loadedHourlyCost?: string;
  assumptions?: unknown;
  explanation?: LocalizedText | string;
}
export interface DashboardSummary {
  counts: Record<WorkState, number>;
  value: {
    configured: boolean;
    estimated: boolean;
    totals: CurrencyAmount[];
    models: ValueModel[];
  };
  trend: Array<{ date: string; amount: string; currency: string }>;
  /** Hours-only time back, visible to every role (DES-HOR-459-02). */
  timeBack: TimeBack;
}
export interface TimeBack {
  configured: boolean;
  seconds: number;
  byWorkflow: Array<{ workflowKey: string; seconds: number }>;
}
export interface CurrencyAmount {
  amount: string;
  currency: string;
}
export interface Attempt {
  id: string;
  workItemId: string;
  number: number;
  definitionKey: string;
  definitionVersion: string;
  revisedFromAttemptId?: string;
  actionableGuidance?: string;
  createdAt: string;
  finishedAt?: string;
}
export interface ResultPresentation {
  // Optional: a result node's presentation may omit `outcomes` (it can carry
  // only fields/artifacts). Consumers must treat it as optional, which the
  // strict `request<T>` cast cannot enforce at runtime.
  outcomes?: Array<{ outcome: string; summary: LocalizedText }>;
  fields?: ResultFieldPresentation[];
}
export interface ResultFieldPresentation {
  path: string[];
  label: LocalizedText;
  options?: Array<{ value: unknown; label: LocalizedText }>;
}
export interface NodeExecution {
  id: string;
  attemptId: string;
  nodeKey: string;
  businessLabel: LocalizedText;
  resultPresentation?: ResultPresentation;
  visit: number;
  executionSeq: number;
  kind: string;
  state: string;
  outcome?: string;
  summary?: string;
  output?: unknown;
  artifactRefs?: Array<{
    artifactId: string;
    role?: string;
    metadata?: unknown;
  }>;
  startedAt?: string;
  finishedAt?: string;
  createdAt: string;
}
export interface TimelineEvent {
  cursor: number;
  id: string;
  workItemId: string;
  attemptId?: string;
  nodeExecutionId?: string;
  code: string;
  params: Record<string, unknown>;
  artifactRefs?: Array<{ artifactId: string }>;
  actorIdentityId?: string;
  /** The person behind the event, for "Approved by Rita · 09:41". */
  actorDisplayName?: string;
  createdAt: string;
}
export interface Blocker {
  id: string;
  workItemId: string;
  attemptId: string;
  nodeExecutionId?: string;
  kind: string;
  title: LocalizedText;
  description: LocalizedText;
  responseSchema: JSONSchema;
  allowedOutcomes: string[];
  responsePresentation: HumanGatePresentation;
  requiredConsequences?: Consequence[];
  state: string;
  responseOutcome?: string;
  respondedBy?: Attribution;
  createdAt: string;
  resolvedAt?: string;
}
export interface HumanGatePresentation {
  outcomes: LocalizedText[];
  fields?: HumanGateFieldPresentation[];
}
export interface HumanGateFieldPresentation {
  key: string;
  label: LocalizedText;
  options?: LocalizedText[];
}
export interface JSONSchema {
  type?: string;
  title?: string;
  required?: string[];
  properties?: Record<string, JSONSchema>;
  enum?: unknown[];
  items?: JSONSchema;
  minLength?: number;
}
export interface Consequence {
  invocationId: string;
  summary: LocalizedText;
  state: string;
}
export interface Feedback {
  id: string;
  workItemId: string;
  attemptId: string;
  category: string;
  explanation?: string;
  correctedResult?: unknown;
  createdAt: string;
  revisedAttemptId?: string;
}
export interface WorkArtifact {
  artifactId: string;
  attemptId: string;
  nodeExecutionId?: string;
  role: string;
  metadata: Record<string, unknown>;
  mimeType: string;
  sizeBytes?: number;
  digest?: string;
  createdAt: string;
}
export interface DetailData {
  item: WorkItem;
  attempts: Attempt[];
  nodes: NodeExecution[];
  timeline: TimelineEvent[];
  blocker: Blocker | null;
  feedback: Feedback[];
  consequences: Consequence[];
  artifacts: WorkArtifact[];
}
