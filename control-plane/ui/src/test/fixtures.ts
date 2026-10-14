// Customer-safe API fixtures for component tests and the local design
// preview. They carry the "Delight, trusted" prototype's own example (a
// Portuguese freight forwarder with the AI coworkers Quotes and Rates), so a
// screen rendered from them can be compared with its prototype board.
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
      en: "turns quote requests emailed to quotes@exemplo.pt into quotes you approve.",
      pt: "transforma os pedidos de cotação enviados para quotes@exemplo.pt em cotações que aprova.",
    },
    onItsOwn: [
      {
        en: "Reads customer emails, PDFs and handwriting",
        pt: "Lê emails, PDFs e letra manuscrita dos clientes",
      },
      {
        en: "Asks your 6 usual suppliers for rates",
        pt: "Pede preços aos seus 6 fornecedores habituais",
      },
      {
        en: "Prices with your margin rules",
        pt: "Calcula o preço com as suas regras de margem",
      },
    ],
    connections: [
      {
        en: "Microsoft 365 mailbox · quotes@exemplo.pt",
        pt: "Caixa de correio Microsoft 365 · quotes@exemplo.pt",
      },
      {
        en: "FreightNet portal · Iterabase browser",
        pt: "Portal FreightNet · browser Iterabase",
      },
      {
        en: "CargoHub portal · Iterabase browser",
        pt: "Portal CargoHub · browser Iterabase",
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
      key: "portals",
      kind: "agent_task",
      label: {
        en: "Checks portal rates",
        pt: "Verifica os preços nos portais",
      },
    },
    {
      key: "suppliers",
      kind: "agent_task",
      label: { en: "Asks suppliers", pt: "Pede preços aos fornecedores" },
    },
    {
      key: "collect",
      kind: "wait",
      label: { en: "Collects replies", pt: "Recolhe as respostas" },
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
        en: "When a detail of the request is unclear",
        pt: "Quando um detalhe do pedido não está claro",
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
      en: "checks your portal rates every Monday at 06:00.",
      pt: "verifica os preços dos seus portais todas as segundas-feiras às 06:00.",
    },
    onItsOwn: [
      {
        en: "Logs in to FreightNet and CargoHub",
        pt: "Entra no FreightNet e no CargoHub",
      },
      {
        en: "Reads rates, never submits anything",
        pt: "Lê preços, nunca submete nada",
      },
    ],
    connections: [
      {
        en: "FreightNet portal · rates@exemplo.pt",
        pt: "Portal FreightNet · rates@exemplo.pt",
      },
    ],
  },
  steps: [
    {
      key: "login",
      kind: "human_gate",
      label: { en: "You complete the login", pt: "Conclui o início de sessão" },
    },
    {
      key: "check",
      kind: "agent_task",
      label: {
        en: "Checks portal rates",
        pt: "Verifica os preços nos portais",
      },
    },
  ],
  alwaysAsks: [
    {
      nodeKey: "login",
      type: "information",
      title: {
        en: "When a portal asks for a login code",
        pt: "Quando um portal pede um código de acesso",
      },
    },
  ],
};

export const workflows = [quotes, rates];

const email = (from: string, subject: string) => ({
  kind: "email",
  title: from,
  subtitle: subject,
});

function item(
  partial: Partial<WorkItem> & Pick<WorkItem, "id" | "title" | "state">,
): WorkItem {
  const flow = partial.workflowKey === rates.key ? rates : quotes;
  return {
    workflowKey: flow.key,
    source: email("compras@outroexemplo.pt", "Pedido de cotação"),
    presentation: { title: flow.title, coworker: flow.coworker },
    currentAttemptId: `${partial.id}-a1`,
    createdAt: minutesAgo(60),
    updatedAt: minutesAgo(30),
    valueConfigured: false,
    valueDisputed: false,
    ...partial,
  };
}

const step = (flow: WorkflowSummary, key: string, state: string) => {
  const found = flow.steps.find((entry) => entry.key === key)!;
  return { key, label: found.label, state };
};

// Needs you.
export const approval = item({
  id: "W-1039",
  title: "Quote · Leixões → Santos, LCL",
  state: "blocked",
  source: email(
    "compras@outroexemplo.pt",
    "Pedido de cotação LCL Leixões–Santos",
  ),
  createdAt: minutesAgo(62),
  updatedAt: minutesAgo(22),
  blocker: {
    id: "b-1039",
    kind: "approval",
    title: {
      en: "Send this quote to Rui Costa",
      pt: "Enviar esta cotação ao Rui Costa",
    },
  },
  currentStep: step(quotes, "confirm", "blocked"),
});

export const question = item({
  id: "W-1045",
  title: "Quote · Lisbon → Luanda, reefer",
  state: "blocked",
  source: {
    kind: "chat",
    title: "Angola Trade",
    subtitle: "Pedido de cotação",
  },
  createdAt: minutesAgo(70),
  updatedAt: minutesAgo(35),
  blocker: {
    id: "b-1045",
    kind: "information",
    title: {
      en: "Which weight: 1.2 t or 7.2 t?",
      pt: "Qual o peso: 1,2 t ou 7,2 t?",
    },
  },
  currentStep: step(quotes, "clarify", "blocked"),
});

export const login = item({
  id: "W-2094",
  workflowKey: rates.key,
  title: "FreightNet login",
  state: "blocked",
  source: { kind: "schedule", title: "Every Monday 06:00" },
  createdAt: minutesAgo(45),
  updatedAt: minutesAgo(41),
  blocker: {
    id: "b-2094",
    kind: "information",
    title: {
      en: "SMS code sent to your phone",
      pt: "Código SMS enviado para o seu telemóvel",
    },
  },
  currentStep: step(rates, "login", "blocked"),
});

// Working.
export const working = item({
  id: "W-1050",
  title: "Quote · Lisbon → Luanda, reefer",
  state: "in_progress",
  source: email("ana.lopes@exemplo.pt", "Pedido de cotação"),
  createdAt: minutesAgo(1),
  updatedAt: minutesAgo(1),
  currentStep: step(quotes, "read", "running"),
});

export const working2 = item({
  id: "W-1053",
  title: "Quote · Madrid → Porto, groupage",
  state: "in_progress",
  createdAt: minutesAgo(2),
  updatedAt: minutesAgo(2),
  currentStep: step(quotes, "portals", "running"),
});

// Waiting: nothing needed from the customer.
export const waiting = item({
  id: "W-1051",
  title: "Quote · Porto → Rotterdam, 2×40′HC",
  state: "waiting",
  createdAt: minutesAgo(30),
  updatedAt: minutesAgo(10),
  wait: {
    completion: "all",
    expected: 3,
    received: 2,
    deadlineAt: inMinutes(180),
  },
  currentStep: step(quotes, "collect", "waiting"),
});

export const paused = item({
  id: "W-1052",
  title: "Quote · Sines → Antwerp, bulk",
  state: "paused",
  createdAt: minutesAgo(4),
  updatedAt: minutesAgo(4),
  pause: { reason: "provider_unavailable", since: minutesAgo(4) },
  currentStep: step(quotes, "price", "pending"),
});

// Done.
export const done = item({
  id: "W-1049",
  title: "Rotterdam shipment booked",
  state: "done",
  source: email("ana@portofreight.pt", "RE: Cotação Porto → Rotterdam"),
  createdAt: minutesAgo(180),
  updatedAt: minutesAgo(90),
  finishedAt: minutesAgo(90),
  currentStep: step(quotes, "confirm", "succeeded"),
});

export const done2 = item({
  id: "W-1042",
  title: "Quote sent · Bilbao → Porto, LTL",
  state: "done",
  createdAt: minutesAgo(200),
  updatedAt: minutesAgo(140),
  finishedAt: minutesAgo(140),
  currentStep: step(quotes, "confirm", "succeeded"),
});

// Failed in the last 7 days.
export const failed = item({
  id: "W-2093",
  workflowKey: rates.key,
  title: "Weekly portal rates · week 42",
  state: "failed",
  source: { kind: "schedule", title: "Every Monday 06:00" },
  createdAt: minutesAgo(220),
  updatedAt: minutesAgo(211),
  finishedAt: minutesAgo(211),
  failureSummary: {
    code: "execution_failed",
    message: "search form not found",
  },
  currentStep: step(rates, "check", "failed"),
});

export const items = [
  approval,
  question,
  login,
  working,
  working2,
  waiting,
  paused,
  done,
  done2,
  failed,
];

export const summary: DashboardSummary = {
  counts: {
    todo: 0,
    in_progress: 2,
    waiting: 1,
    paused: 1,
    blocked: 3,
    done: 2,
    failed: 1,
  },
  value: { configured: false, estimated: true, totals: [], models: [] },
  trend: [],
  timeBack: {
    configured: true,
    seconds: 6600,
    byWorkflow: [
      { workflowKey: quotes.key, seconds: 6000 },
      { workflowKey: rates.key, seconds: 600 },
    ],
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
      en: "Rui asked for LCL to Santos. NorthSea had the best rate, and the margin fits your rule.",
      pt: "O Rui pediu LCL para Santos. A NorthSea teve o melhor preço e a margem cumpre a sua regra.",
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
  [login.id]: {
    id: "b-2094",
    workItemId: login.id,
    attemptId: login.currentAttemptId,
    kind: "information",
    title: login.blocker!.title,
    description: {
      en: "FreightNet sent an SMS code to your phone. Enter it so Rates can carry on.",
      pt: "O FreightNet enviou um código SMS para o seu telemóvel. Introduza-o para o Rates continuar.",
    },
    responseSchema: {
      type: "object",
      required: ["code"],
      properties: { code: { type: "string" } },
    },
    allowedOutcomes: ["entered"],
    responsePresentation: {
      outcomes: [{ en: "Send code", pt: "Enviar código" }],
      fields: [{ key: "code", label: { en: "SMS code", pt: "Código SMS" } }],
    },
    state: "open",
    createdAt: minutesAgo(41),
  },
};

const quoteEmail = {
  to: "compras@outroexemplo.pt",
  subject: "RE: Pedido de cotação LCL Leixões–Santos",
  attached: "Cotacao_W-1039.pdf",
  body: "Bom dia Sr. Costa,\nSegue a nossa cotação para o envio LCL de 6 cbm Leixões → Santos: frete marítimo e taxas de origem 1.840,00 €, válida por 30 dias.\nCumprimentos,\nRita Mendes · Transitários Exemplo",
};

const quoteFields = [
  { path: ["to"], label: { en: "To", pt: "Para" } },
  { path: ["subject"], label: { en: "Subject", pt: "Assunto" } },
  { path: ["attached"], label: { en: "Attached", pt: "Anexo" } },
  { path: ["body"], label: { en: "Email", pt: "Email" } },
];

export function nodesFor(id: string): NodeExecution[] {
  const found = items.find((entry) => entry.id === id);
  if (!found || found.workflowKey !== quotes.key) return [];
  if (found.state !== "blocked" && found.state !== "done") return [];
  if (found.id !== approval.id && found.state !== "done") return [];
  return [
    {
      id: `${id}-n1`,
      attemptId: found.currentAttemptId,
      nodeKey: "price",
      businessLabel: quotes.steps[5].label,
      visit: 1,
      executionSeq: 1,
      kind: "agent_task",
      state: "succeeded",
      outcome: "priced",
      startedAt: minutesAgo(40),
      finishedAt: minutesAgo(31),
      createdAt: minutesAgo(40),
      output: quoteEmail,
      resultPresentation: {
        outcomes: [
          {
            outcome: "priced",
            summary: { en: "Quote sent", pt: "Cotação enviada" },
          },
        ],
        fields: quoteFields,
      },
    },
  ];
}

export function timelineFor(id: string): TimelineEvent[] {
  const events: TimelineEvent[] = [
    {
      cursor: 1,
      id: `${id}-e1`,
      workItemId: id,
      code: "work_item_created",
      params: {},
      createdAt: minutesAgo(60),
    },
  ];
  if (id === approval.id || id === done.id || id === done2.id)
    events.push(
      {
        cursor: 2,
        id: `${id}-e2`,
        workItemId: id,
        nodeExecutionId: `${id}-n1`,
        code: "node_completed",
        params: {},
        createdAt: minutesAgo(31),
      },
      {
        cursor: 3,
        id: `${id}-e3`,
        workItemId: id,
        code: "work_blocked",
        params: {},
        createdAt: minutesAgo(31),
      },
    );
  if (id === done.id || id === done2.id)
    events.push({
      cursor: 4,
      id: `${id}-e4`,
      workItemId: id,
      code: "blocker_resolved",
      params: {},
      actorIdentityId: "identity-2",
      actorDisplayName: "Rita Mendes",
      createdAt: minutesAgo(95),
    });
  return events;
}
