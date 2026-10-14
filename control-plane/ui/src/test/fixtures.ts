// Customer-safe API fixtures for component tests and the local design preview:
// two AI coworkers and one item in every state the app shows.
import type {
  Blocker,
  DashboardSummary,
  NodeExecution,
  TimelineEvent,
  WorkflowSummary,
  WorkItem,
} from "../lib/types";

const now = Date.now();
export const minutesAgo = (minutes: number) =>
  new Date(now - minutes * 60_000).toISOString();
const inMinutes = (minutes: number) =>
  new Date(now + minutes * 60_000).toISOString();

export const quotes: WorkflowSummary = {
  key: "default/quotes",
  version: "1",
  title: { en: "Quotation requests", pt: "Pedidos de cotação" },
  coworker: {
    name: "Quotes",
    mark: { shape: "round", color: "blue" },
    role: { en: "Sales desk", pt: "Comercial" },
    summary: {
      en: "turns quote requests emailed to quotes@ into quotes you approve",
      pt: "transforma os pedidos de cotação enviados para quotes@ em cotações que aprova",
    },
    onItsOwn: [
      {
        en: "Reads customer emails and PDFs",
        pt: "Lê emails e PDFs dos clientes",
      },
      {
        en: "Asks your usual suppliers for rates",
        pt: "Pede preços aos fornecedores habituais",
      },
    ],
    connections: [
      {
        en: "Microsoft 365 mailbox quotes@",
        pt: "Caixa de correio Microsoft 365 quotes@",
      },
    ],
  },
  steps: [
    {
      key: "read",
      kind: "agent_task",
      label: { en: "Reads the request", pt: "Lê o pedido" },
    },
    {
      key: "clarify",
      kind: "human_gate",
      label: { en: "You clear up a detail", pt: "Esclarece um detalhe" },
    },
    {
      key: "suppliers",
      kind: "wait",
      label: {
        en: "Collects supplier replies",
        pt: "Recolhe as respostas dos fornecedores",
      },
    },
    {
      key: "price",
      kind: "agent_task",
      label: { en: "Picks the best rate", pt: "Escolhe o melhor preço" },
    },
    {
      key: "confirm",
      kind: "human_gate",
      label: { en: "You confirm the quote", pt: "Confirma a cotação" },
    },
  ],
  alwaysAsks: [
    {
      nodeKey: "clarify",
      type: "information",
      title: {
        en: "When a detail is unclear",
        pt: "Quando um detalhe não está claro",
      },
    },
    {
      nodeKey: "confirm",
      type: "approval",
      title: {
        en: "Before any email to a customer",
        pt: "Antes de qualquer email a um cliente",
      },
    },
  ],
};

export const rates: WorkflowSummary = {
  key: "default/rates",
  version: "1",
  title: { en: "Portal rate checks", pt: "Verificação de preços nos portais" },
  coworker: {
    name: "Rates",
    mark: { shape: "square", color: "green" },
    role: { en: "Pricing", pt: "Preços" },
    summary: {
      en: "checks portal rates every Monday",
      pt: "verifica os preços nos portais todas as segundas-feiras",
    },
    onItsOwn: [
      {
        en: "Reads rates, never submits anything",
        pt: "Lê preços, nunca submete nada",
      },
    ],
    connections: [],
  },
  steps: [
    {
      key: "check",
      kind: "agent_task",
      label: {
        en: "Checks portal rates",
        pt: "Verifica os preços nos portais",
      },
    },
  ],
  alwaysAsks: [],
};

export const workflows = [quotes, rates];

function item(
  partial: Partial<WorkItem> & Pick<WorkItem, "id" | "title" | "state">,
): WorkItem {
  const flow = partial.workflowKey === rates.key ? rates : quotes;
  return {
    workflowKey: flow.key,
    source: {
      kind: "email",
      title: "compras@exemplo.pt",
      subtitle: "Pedido de cotação",
    },
    presentation: { title: flow.title, coworker: flow.coworker },
    currentAttemptId: `${partial.id}-a1`,
    createdAt: minutesAgo(60),
    updatedAt: minutesAgo(30),
    valueConfigured: false,
    valueDisputed: false,
    ...partial,
  };
}

export const approval = item({
  id: "w-1039",
  title: "Quote for Rui Costa · Leixões → Santos",
  state: "blocked",
  updatedAt: minutesAgo(22),
  blocker: {
    id: "b-1039",
    kind: "approval",
    title: {
      en: "Send this quote to Rui Costa?",
      pt: "Enviar esta cotação ao Rui Costa?",
    },
  },
  currentStep: {
    key: "confirm",
    label: quotes.steps[4].label,
    state: "blocked",
  },
});

export const question = item({
  id: "w-1045",
  title: "Quote · Lisbon → Luanda, reefer",
  state: "blocked",
  updatedAt: minutesAgo(35),
  blocker: {
    id: "b-1045",
    kind: "information",
    title: {
      en: "Which weight: 1.2 t or 7.2 t?",
      pt: "Qual o peso: 1,2 t ou 7,2 t?",
    },
  },
  currentStep: {
    key: "clarify",
    label: quotes.steps[1].label,
    state: "blocked",
  },
});

export const working = item({
  id: "w-1050",
  title: "Quote · Aveiro → Hamburg",
  state: "in_progress",
  currentStep: { key: "read", label: quotes.steps[0].label, state: "running" },
});

export const waiting = item({
  id: "w-1051",
  title: "Quote · Porto → Rotterdam",
  state: "waiting",
  wait: {
    completion: "all",
    expected: 3,
    received: 2,
    deadlineAt: inMinutes(180),
  },
  currentStep: {
    key: "suppliers",
    label: quotes.steps[2].label,
    state: "waiting",
  },
});

export const paused = item({
  id: "w-1052",
  title: "Quote · Sines → Antwerp",
  state: "paused",
  pause: { reason: "provider_unavailable", since: minutesAgo(4) },
});

export const done = item({
  id: "w-1049",
  title: "Quote sent · Bilbao → Porto",
  state: "done",
  finishedAt: minutesAgo(90),
});

export const failed = item({
  id: "w-2093",
  workflowKey: rates.key,
  title: "Weekly portal rates · week 42",
  state: "failed",
  source: { kind: "schedule", title: "Every Monday 06:00" },
  finishedAt: minutesAgo(120),
  failureSummary: {
    code: "execution_failed",
    message: "search form not found",
  },
});

export const items = [
  approval,
  question,
  working,
  waiting,
  paused,
  done,
  failed,
];

export const summary: DashboardSummary = {
  counts: {
    todo: 0,
    in_progress: 1,
    waiting: 1,
    paused: 1,
    blocked: 2,
    done: 1,
    failed: 1,
  },
  value: { configured: false, estimated: true, totals: [], models: [] },
  trend: [],
  timeBack: {
    configured: true,
    seconds: 6600,
    byWorkflow: [{ workflowKey: quotes.key, seconds: 6600 }],
  },
};

export const blockers: Record<string, Blocker> = {
  [approval.id]: {
    id: "b-1039",
    workItemId: approval.id,
    attemptId: approval.currentAttemptId,
    kind: "approval",
    title: approval.blocker!.title,
    description: {
      en: "NorthSea had the best rate, and the margin fits your rule.",
      pt: "A NorthSea teve o melhor preço e a margem cumpre a sua regra.",
    },
    responseSchema: { type: "object", properties: {} },
    allowedOutcomes: ["approved", "rejected"],
    responsePresentation: {
      outcomes: [
        { en: "Approve & send", pt: "Aprovar e enviar" },
        { en: "Not this one", pt: "Esta não" },
      ],
    },
    state: "open",
    createdAt: minutesAgo(22),
  },
  [question.id]: {
    id: "b-1045",
    workItemId: question.id,
    attemptId: question.currentAttemptId,
    kind: "information",
    title: question.blocker!.title,
    description: {
      en: "The handwritten note on page 2 could read either.",
      pt: "A nota manuscrita na página 2 pode ler-se das duas formas.",
    },
    responseSchema: {
      type: "object",
      required: ["weight"],
      properties: { weight: { type: "string", enum: ["1.2t", "7.2t"] } },
    },
    allowedOutcomes: ["answered"],
    responsePresentation: {
      outcomes: [{ en: "Send answer", pt: "Enviar resposta" }],
      fields: [
        {
          key: "weight",
          label: { en: "Cargo weight", pt: "Peso da carga" },
          options: [
            { en: "1.2 t", pt: "1,2 t" },
            { en: "7.2 t", pt: "7,2 t" },
          ],
        },
      ],
    },
    state: "open",
    createdAt: minutesAgo(35),
  },
};

export function nodesFor(id: string): NodeExecution[] {
  if (id !== approval.id && id !== done.id) return [];
  const attemptId =
    id === approval.id ? approval.currentAttemptId : done.currentAttemptId;
  return [
    {
      id: `${id}-n1`,
      attemptId,
      nodeKey: "price",
      businessLabel: quotes.steps[3].label,
      visit: 1,
      executionSeq: 1,
      kind: "agent_task",
      state: "succeeded",
      outcome: "priced",
      startedAt: minutesAgo(40),
      finishedAt: minutesAgo(31),
      createdAt: minutesAgo(40),
      output: {
        to: "compras@outroexemplo.pt",
        subject: "RE: Pedido de cotação LCL Leixões–Santos",
        body: "Bom dia Sr. Costa,\nSegue a nossa cotação: 1.840,00 €.",
      },
      resultPresentation: {
        outcomes: [
          {
            outcome: "priced",
            summary: { en: "Quote sent", pt: "Cotação enviada" },
          },
        ],
        fields: [
          { path: ["to"], label: { en: "To", pt: "Para" } },
          { path: ["subject"], label: { en: "Subject", pt: "Assunto" } },
          { path: ["body"], label: { en: "Email", pt: "Email" } },
        ],
      },
    },
  ];
}

export function timelineFor(id: string): TimelineEvent[] {
  const base: TimelineEvent[] = [
    {
      cursor: 1,
      id: `${id}-e1`,
      workItemId: id,
      code: "work_item_created",
      params: {},
      createdAt: minutesAgo(60),
    },
  ];
  if (id === done.id)
    base.push({
      cursor: 2,
      id: `${id}-e2`,
      workItemId: id,
      code: "blocker_resolved",
      params: {},
      actorIdentityId: "identity-2",
      actorDisplayName: "Rita Mendes",
      createdAt: minutesAgo(95),
    });
  return base;
}
