"""Demo AI coworkers and work for a seeded preview (HOR-459, DES-HOR-459-06).

A preview has no customer workflows, so the app's screens would be empty and
could not be reviewed. In `seeded` admin mode the deploy applies three demo
coworkers modelled on the "Delight, trusted" prototype (Quotes, Rates and
Customs) and starts work through the real customer API as the preview Admin:

* Each workflow begins at a human gate, so new work is in Needs you at once
  with no worker or inference.
* Customs uses every node kind and gate type the runtime supports: an
  information gate with every field type, a file upload, a wait with a
  deadline, a decision with a loop back, an approval and an agent step.
* Answering gates (and stopping a wait) moves work on: to an agent step on a
  paused AgentPool (no replicas), where it honestly stays Working; to Done
  when a decision declines; to Failed when the loop passes maxTransitions.
* Some work starts with its source documents (the customer's email and PDF)
  attached, so Sources and documents shows real files.

Every start carries a fixed Idempotency-Key (work with attached sources is
looked up by title first, so nothing is uploaded twice), and only newly
created work is answered, so redeploying the same preview never adds or
changes demo work. The demo exists only on previews; nothing here ships.
"""

from __future__ import annotations

import base64
import json
import shlex

POOL = "preview-demo"
NAMESPACE = "iterabase-system"
RELEASE = "iterabase"


def _workflow(name: str, spec: dict) -> dict:
    return {
        "apiVersion": "platform.iterabase.com/v1alpha1",
        "kind": "Workflow",
        "metadata": {"name": name, "namespace": NAMESPACE},
        "spec": spec,
    }


def _text(en: str, pt: str) -> dict:
    return {"en": en, "pt": pt}


def _line(kind: str, text: dict, detail: dict | None = None, items: list[str] | None = None) -> dict:
    """One typed coworker "On its own" line (DES-HOR-459-09)."""
    line = {"text": text, "kind": kind}
    if detail:
        line["detail"] = detail
    if items:
        line["items"] = items
    return line


def _agent_step(key: str, label: dict, model_id: str, summary: dict) -> dict:
    return {
        "key": key,
        "label": label,
        "kind": "agent_task",
        "prompt": "Preview demo step. It never runs: the demo pool has no workers.",
        "modelRef": model_id,
        "timeout": "10m",
        "outcomes": ["done"],
        "outputSchema": {"type": "object", "additionalProperties": False},
        "resultPresentation": {"outcomes": [{"outcome": "done", "summary": summary}]},
    }


def manifests(model_id: str) -> list[dict]:
    """The demo pool and the Quotes and Rates workflows."""
    pool = {
        "apiVersion": "platform.iterabase.com/v1alpha1",
        "kind": "AgentPool",
        "metadata": {"name": POOL, "namespace": NAMESPACE},
        "spec": {
            "replicas": 0,
            "workerImage": "invalid.local/unused:fixed",
            "podSecurity": "baseline",
            "identity": {"trustDomain": "iterabase.local", "caSecretRef": {"name": f"{POOL}-ca"}},
            "sandbox": {
                "storageClassName": "iterabase-agentpool-lvm-xfs",
                "accessMode": "ReadWriteOnce",
                "size": "1Gi",
            },
            "gateways": {
                "controlPlane": {
                    "url": f"https://{RELEASE}-control-plane-dispatch.{NAMESPACE}.svc:8091",
                    "serverName": f"{RELEASE}-control-plane-dispatch.{NAMESPACE}.svc",
                    "selector": {"podSelector": {"matchLabels": {"app.kubernetes.io/component": "dispatch"}}},
                },
                "toolGateway": {
                    "url": f"https://{RELEASE}-control-plane-gateway.{NAMESPACE}.svc:8090",
                    "serverName": f"{RELEASE}-control-plane-gateway.{NAMESPACE}.svc",
                    "selector": {"podSelector": {"matchLabels": {"app.kubernetes.io/component": "gateway"}}},
                },
                "inferenceGateway": {
                    "url": f"https://{RELEASE}-gateway.{NAMESPACE}.svc:8443",
                    "serverName": f"{RELEASE}-gateway.{NAMESPACE}.svc",
                    "selector": {"podSelector": {"matchLabels": {"app.kubernetes.io/name": "inference-gateway"}}},
                },
            },
            "networkPolicy": {"egress": "denied"},
            "workspaceTools": False,
        },
    }
    ca = {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {"name": f"{POOL}-ca", "namespace": NAMESPACE},
        "type": "Opaque",
        "stringData": {"ca.crt": "preview-demo-not-a-private-key"},
    }
    quotes = _workflow("preview-demo-quotes", {
        "key": "demo/quotes",
        "version": "2",
        "poolRef": POOL,
        "defaultModelRef": model_id,
        "source": {"type": "manual_api"},
        "graph": {
            "entryNode": "confirm",
            "maxTransitions": 4,
            "nodes": [
                {
                    "key": "confirm",
                    "label": _text("You confirm the quote", "Confirma a cotação"),
                    "kind": "human_gate",
                    "outcomes": ["approved", "declined"],
                    "resultPresentation": {"outcomes": [
                        {"outcome": "declined", "summary": _text("Not sent · you declined", "Não enviada · recusou")},
                    ]},
                    "humanGate": {
                        "type": "approval",
                        "title": _text("Send this quote to the customer", "Enviar esta cotação ao cliente"),
                        "description": _text(
                            "The best supplier rate fits your margin rule.",
                            "O melhor preço dos fornecedores cumpre a sua regra de margem.",
                        ),
                        "responseSchema": {"type": "object", "additionalProperties": False, "properties": {}},
                        "presentation": {"outcomes": [
                            _text("Approve & send", "Aprovar e enviar"),
                            _text("Not this one", "Esta não"),
                        ]},
                    },
                },
                _agent_step("send", _text("Sends the quote", "Envia a cotação"), model_id,
                            _text("Quote sent", "Cotação enviada")),
            ],
            "edges": [{"from": "confirm", "outcome": "approved", "to": "send"}],
            "terminalOutcomes": [
                {"node": "confirm", "outcome": "declined"},
                {"node": "send", "outcome": "done"},
            ],
        },
        "title": _text("Quotation requests", "Pedidos de cotação"),
        "coworker": {
            "name": "Quotes",
            "mark": {"shape": "round", "color": "blue"},
            "role": _text("Sales desk", "Comercial"),
            "summary": _text(
                "turns quote requests emailed to quotes@ into quotes you approve.",
                "transforma os pedidos de cotação enviados para quotes@ em cotações que aprova.",
            ),
            "onItsOwn": [
                _line("reads", _text("Read the quotes mailbox", "Lê a caixa de correio de cotações"),
                      _text("quotes@exemplo.pt · emails and attachments", "quotes@exemplo.pt · emails e anexos")),
                _line("reads", _text("Read rates on 2 portals", "Lê preços em 2 portais"),
                      _text("FreightNet, CargoHub · never submits", "FreightNet, CargoHub · nunca submete")),
                _line("sends", _text("Email your 6 usual suppliers", "Envia email aos seus 6 fornecedores habituais"),
                      _text("Pre-approved recipients · sent without review",
                            "Destinatários pré-aprovados · enviado sem revisão"),
                      ["rates@northsealines.example", "pricing@atlanticfwd.example", "quotes@ibercargo.example",
                       "sales@portoshipping.example", "lcl@euroconsol.example", "ops@lusofreight.example"]),
                _line("does", _text("Price with your margin rules", "Calcula o preço com as suas regras de margem"),
                      _text("LCL at least 12% · FCL at least 10%", "LCL pelo menos 12% · FCL pelo menos 10%")),
            ],
            "connections": [_text("Microsoft 365 mailbox · quotes@", "Caixa de correio Microsoft 365 · quotes@")],
        },
    })
    rates = _workflow("preview-demo-rates", {
        "key": "demo/rates",
        "version": "2",
        "poolRef": POOL,
        "defaultModelRef": model_id,
        "source": {"type": "manual_api"},
        "graph": {
            "entryNode": "login",
            "maxTransitions": 4,
            "nodes": [
                {
                    "key": "login",
                    "label": _text("You complete the login", "Conclui o início de sessão"),
                    "kind": "human_gate",
                    "outcomes": ["entered"],
                    "humanGate": {
                        "type": "information",
                        "title": _text("Enter the FreightNet login code", "Introduza o código de acesso do FreightNet"),
                        "description": _text(
                            "FreightNet sent an SMS code to your phone.",
                            "O FreightNet enviou um código SMS para o seu telemóvel.",
                        ),
                        "responseSchema": {
                            "type": "object",
                            "additionalProperties": False,
                            "required": ["code"],
                            "properties": {"code": {"type": "string"}},
                        },
                        "presentation": {
                            "outcomes": [_text("Send code", "Enviar código")],
                            "fields": [{"key": "code", "label": _text("SMS code", "Código SMS")}],
                        },
                    },
                },
                _agent_step("check", _text("Checks portal rates", "Verifica os preços nos portais"), model_id,
                            _text("Rates checked", "Preços verificados")),
            ],
            "edges": [{"from": "login", "outcome": "entered", "to": "check"}],
            "terminalOutcomes": [{"node": "check", "outcome": "done"}],
        },
        "title": _text("Portal rate checks", "Verificação de preços nos portais"),
        "coworker": {
            "name": "Rates",
            "mark": {"shape": "square", "color": "green"},
            "role": _text("Pricing", "Preços"),
            "summary": _text("checks your portal rates every Monday.",
                             "verifica os preços dos seus portais todas as segundas-feiras."),
            "onItsOwn": [
                _line("does", _text("Log in to 2 portals", "Entra em 2 portais"),
                      _text("FreightNet, CargoHub · asks you for login codes",
                            "FreightNet, CargoHub · pede-lhe os códigos de acesso")),
                _line("reads", _text("Read this week's rates", "Lê os preços desta semana"),
                      _text("Every Monday 06:00 · never submits anything",
                            "Todas as segundas-feiras às 06:00 · nunca submete nada")),
            ],
            "connections": [_text("FreightNet portal", "Portal FreightNet")],
        },
    })
    customs = _workflow("preview-demo-customs", _customs(model_id))
    return [ca, pool, quotes, rates, customs]


def _customs(model_id: str) -> dict:
    """Every node kind and gate type the runtime supports, in one coworker."""
    details = {
        "key": "details",
        "label": _text("You confirm the shipment details", "Confirma os dados do envio"),
        "kind": "human_gate",
        "outcomes": ["submitted"],
        "humanGate": {
            "type": "information",
            "title": _text("Confirm the shipment details", "Confirme os dados do envio"),
            "description": _text(
                "The booking is missing details the customs declaration needs.",
                "Faltam na reserva dados de que a declaração aduaneira precisa.",
            ),
            "responseSchema": {
                "type": "object",
                "additionalProperties": False,
                "required": ["reference", "incoterm", "packages"],
                "properties": {
                    "reference": {"type": "string"},
                    "incoterm": {"enum": ["EXW", "FOB", "CIF", "DAP"]},
                    "packages": {"type": "integer"},
                    "grossWeightKg": {"type": "number"},
                    "hazardous": {"type": "boolean"},
                },
            },
            "presentation": {
                "outcomes": [_text("Confirm details", "Confirmar dados")],
                "fields": [
                    {"key": "reference", "label": _text("Booking reference", "Referência da reserva")},
                    {"key": "incoterm", "label": _text("Incoterm", "Incoterm"),
                     "options": [_text("EXW · ex works", "EXW · na fábrica"),
                                 _text("FOB · free on board", "FOB · franco a bordo"),
                                 _text("CIF · cost, insurance and freight", "CIF · custo, seguro e frete"),
                                 _text("DAP · delivered at place", "DAP · entregue no local")]},
                    {"key": "packages", "label": _text("Packages", "Volumes")},
                    {"key": "grossWeightKg", "label": _text("Gross weight (kg)", "Peso bruto (kg)")},
                    {"key": "hazardous", "label": _text("Dangerous goods", "Mercadorias perigosas")},
                ],
            },
        },
    }
    invoice = {
        "key": "invoice",
        "label": _text("You upload the invoice", "Envia a fatura"),
        "kind": "human_gate",
        "outcomes": ["uploaded"],
        "humanGate": {
            "type": "artifact",
            "title": _text("Upload the commercial invoice", "Envie a fatura comercial"),
            "description": _text(
                "The declaration needs the signed commercial invoice as a PDF.",
                "A declaração precisa da fatura comercial assinada em PDF.",
            ),
            "presentation": {"outcomes": [_text("Upload invoice", "Enviar fatura")]},
        },
    }
    broker = {
        "key": "broker",
        "label": _text("Waits for the broker's reply", "Aguarda a resposta do despachante"),
        "kind": "wait",
        "outcomes": ["received", "deadline", "cancelled"],
        "wait": {
            "events": [{"source": "email_reply", "correlation": {"node": "details", "path": ["reference"]}}],
            "completion": "any",
            "deadline": {"after": "48h"},
        },
    }
    next_step = {
        "key": "next",
        "label": _text("You decide what happens next", "Decide o que se segue"),
        "kind": "human_gate",
        "outcomes": ["retry", "file"],
        "humanGate": {
            "type": "decision",
            "title": _text("The broker hasn't replied. What now?", "O despachante não respondeu. E agora?"),
            "description": _text(
                "Customs can ask the broker again or prepare the declaration without them.",
                "O Customs pode voltar a pedir ao despachante ou preparar a declaração sem ele.",
            ),
            "presentation": {"outcomes": [
                _text("Ask the broker again", "Pedir de novo ao despachante"),
                _text("Prepare it without the broker", "Preparar sem o despachante"),
            ]},
        },
    }
    declare = {
        "key": "declare",
        "label": _text("You approve the declaration", "Aprova a declaração"),
        "kind": "human_gate",
        "outcomes": ["approved", "declined"],
        "resultPresentation": {"outcomes": [
            {"outcome": "declined", "summary": _text("Not filed · you declined", "Não submetida · recusou")},
        ]},
        "humanGate": {
            "type": "approval",
            "title": _text("File this customs declaration", "Submeter esta declaração aduaneira"),
            "description": _text(
                "Filing is final: customs can't recall a submitted declaration.",
                "A submissão é definitiva: a alfândega não anula uma declaração submetida.",
            ),
            "responseSchema": {"type": "object", "additionalProperties": False, "properties": {}},
            "presentation": {"outcomes": [
                _text("Approve & file", "Aprovar e submeter"),
                _text("Not this one", "Esta não"),
            ]},
        },
    }
    return {
        "key": "demo/customs",
        "version": "1",
        "poolRef": POOL,
        "defaultModelRef": model_id,
        "source": {"type": "manual_api"},
        "graph": {
            "entryNode": "details",
            # Asking the broker again loops broker → next → broker; past eight
            # transitions the work fails (transition_limit), which the demo uses
            # to show a Failed item.
            "maxTransitions": 8,
            "nodes": [
                details, invoice, broker, next_step, declare,
                _agent_step("file", _text("Files the declaration", "Submete a declaração"), model_id,
                            _text("Declaration filed", "Declaração submetida")),
            ],
            "edges": [
                {"from": "details", "outcome": "submitted", "to": "invoice"},
                {"from": "invoice", "outcome": "uploaded", "to": "broker"},
                {"from": "broker", "outcome": "received", "to": "declare"},
                {"from": "broker", "outcome": "deadline", "to": "next"},
                {"from": "broker", "outcome": "cancelled", "to": "next"},
                {"from": "next", "outcome": "retry", "to": "broker"},
                {"from": "next", "outcome": "file", "to": "declare"},
                {"from": "declare", "outcome": "approved", "to": "file"},
            ],
            "terminalOutcomes": [
                {"node": "declare", "outcome": "declined"},
                {"node": "file", "outcome": "done"},
            ],
        },
        "title": _text("Customs declarations", "Declarações aduaneiras"),
        "coworker": {
            "name": "Customs",
            "mark": {"shape": "petal", "color": "sun"},
            "role": _text("Customs desk", "Alfândega"),
            "summary": _text(
                "prepares customs declarations from your bookings and files them once you approve.",
                "prepara declarações aduaneiras a partir das suas reservas e submete-as depois de aprovar.",
            ),
            "onItsOwn": [
                _line("reads", _text("Read bookings and invoices", "Lê reservas e faturas"),
                      _text("From the operations mailbox", "Da caixa de correio de operações")),
                _line("sends", _text("Chase your customs broker", "Insiste com o seu despachante"),
                      _text("One reminder, then asks you", "Um lembrete e depois pergunta-lhe"),
                      ["despachos@brokerlusa.example"]),
                _line("does", _text("Prepare the declaration", "Prepara a declaração"),
                      _text("Codes, values and weights from your documents",
                            "Códigos, valores e pesos dos seus documentos")),
            ],
            "connections": [
                _text("Microsoft 365 mailbox · operacoes@", "Caixa de correio Microsoft 365 · operacoes@"),
                _text("Customs portal · e-Declarações", "Portal aduaneiro · e-Declarações"),
            ],
        },
    }


def _pdf(lines: list[str]) -> bytes:
    """A one-page PDF with the given lines, so the demo opens a real file."""
    text = "BT /F1 12 Tf 72 760 Td 16 TL " + " ".join(
        "(" + line.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)") + ") '" for line in lines
    ) + " ET"
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length %d >>\nstream\n%s\nendstream" % (len(text), text.encode("latin-1")),
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for number, body in enumerate(objects, start=1):
        offsets.append(len(out))
        out += b"%d 0 obj\n%s\nendobj\n" % (number, body)
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objects) + 1)
    out += b"".join(b"%010d 00000 n \n" % offset for offset in offsets)
    out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objects) + 1, xref)
    return bytes(out)


_EMAIL = "\r\n".join([
    "From: Rui Costa <compras@outroexemplo.pt>",
    "To: quotes@exemplo.pt",
    "Subject: Pedido de cotação LCL Leixões–Santos",
    "Date: Mon, 12 Oct 2026 08:40:00 +0100",
    "MIME-Version: 1.0",
    "Content-Type: text/plain; charset=utf-8",
    "Content-Transfer-Encoding: 8bit",
    "",
    "Bom dia,",
    "",
    "Pedimos cotação para um envio LCL de Leixões para Santos:",
    "6 cbm, 1,2 t, 4 paletes, pronto a 20 de outubro.",
    "Segue em anexo o pedido em PDF.",
    "",
    "Obrigado,",
    "Rui Costa",
    "",
]).encode("utf-8")

# name → (mime type, bytes). Files a person or a customer sends, never secrets.
DOCS: dict[str, tuple[str, bytes]] = {
    "Pedido de cotação LCL Leixões–Santos.eml": ("message/rfc822", _EMAIL),
    "pedido.pdf": ("application/pdf", _pdf([
        "Pedido de cotacao - Outro Exemplo, Lda.",
        "Origem: Leixoes (PT)   Destino: Santos (BR)",
        "Modo: LCL   Volume: 6 cbm   Peso: 1.2 t   4 paletes",
        "Pronto a carregar: 20/10/2026",
    ])),
    "fatura-comercial.pdf": ("application/pdf", _pdf([
        "Fatura comercial FC-2026/0418",
        "Exportador: Exemplo Ceramicas, S.A.",
        "Incoterm: FOB Leixoes   12 volumes   2 340 kg",
        "Valor: conforme contrato",
    ])),
}

# A demo step moves new work on: answer the open gate ("answer", outcome,
# response), upload a file to an artifact gate ("upload", outcome, doc name),
# stop the wait it is in ("stop",), or keep stopping the wait and asking again
# until the work passes maxTransitions and fails ("until_failed",).
Step = tuple
_DETAILS = ("answer", "submitted", {"reference": "BK-77120", "incoterm": "FOB", "packages": 12,
                                    "grossWeightKg": 2340.5, "hazardous": False})
_INVOICE = ("upload", "uploaded", "fatura-comercial.pdf")
_STOP = ("stop",)
_UNTIL_FAILED = ("until_failed",)


def _email(sender: str, subject: str) -> dict:
    return {"kind": "email", "title": sender, "subtitle": subject}


_SCHEDULE = {"kind": "schedule", "title": "Every Monday 06:00"}
_BOOKING = {"kind": "booking", "title": "Operations mailbox"}

# (key, workflow, title, source presentation, steps, attached source docs)
WORK: list[tuple[str, str, str, dict, list[Step], list[str]]] = [
    ("demo-1039", "demo/quotes", "Quote · Leixões → Santos, LCL",
     _email("compras@outroexemplo.pt", "Pedido de cotação LCL Leixões–Santos"), [], []),
    ("demo-1045", "demo/quotes", "Quote · Lisbon → Luanda, reefer",
     _email("Angola Trade", "Pedido de cotação"), [], []),
    ("demo-2094", "demo/rates", "FreightNet login", _SCHEDULE, [], []),
    ("demo-1051", "demo/quotes", "Quote · Porto → Rotterdam, 2×40′HC",
     _email("ana@portofreight.pt", "Pedido de cotação"), [("answer", "approved", {})], []),
    ("demo-2093", "demo/rates", "Weekly portal rates · week 42", _SCHEDULE,
     [("answer", "entered", {"code": "482913"})], []),
    ("demo-1042", "demo/quotes", "Quote · Bilbao → Porto, LTL",
     _email("compras@exemplo.pt", "Pedido de cotação"), [("answer", "declined", {})], []),
    # With its sources attached: the customer's email and PDF.
    ("demo-1039-docs", "demo/quotes", "Quote · Leixões → Santos, LCL 6 cbm",
     _email("compras@outroexemplo.pt", "Pedido de cotação LCL Leixões–Santos"), [],
     ["Pedido de cotação LCL Leixões–Santos.eml", "pedido.pdf"]),
    # Customs walks every gate type and state.
    ("demo-3101", "demo/customs", "Customs · Leixões export, ceramics", _BOOKING, [], []),
    ("demo-3102", "demo/customs", "Customs · Sines import, machinery", _BOOKING, [_DETAILS], []),
    ("demo-3103", "demo/customs", "Customs · Lisbon export, cork", _BOOKING, [_DETAILS, _INVOICE], []),
    ("demo-3104", "demo/customs", "Customs · Porto import, textiles", _BOOKING,
     [_DETAILS, _INVOICE, _STOP], []),
    ("demo-3105", "demo/customs", "Customs · Aveiro export, wine", _BOOKING,
     [_DETAILS, _INVOICE, _STOP, ("answer", "file", {})], []),
    ("demo-3106", "demo/customs", "Customs · Leixões import, steel", _BOOKING,
     [_DETAILS, _INVOICE, _STOP, ("answer", "file", {}), ("answer", "declined", {})], []),
    ("demo-3107", "demo/customs", "Customs · Setúbal export, paper", _BOOKING,
     [_DETAILS, _INVOICE, _STOP, ("answer", "file", {}), ("answer", "approved", {})], []),
    # Asking again and again passes maxTransitions: Failed (transition_limit).
    ("demo-3108", "demo/customs", "Customs · Sines export, granite", _BOOKING,
     [_DETAILS, _INVOICE, _UNTIL_FAILED], []),
]


# The remote command: read the password from stdin's first line into the
# environment, then run the rest of stdin (the seed script) as bash.
REMOTE = "bash -c 'IFS= read -r PREVIEW_DEMO_PASSWORD; export PREVIEW_DEMO_PASSWORD; exec bash -s'"


def seed_script(target: str, app_host: str, email: str, model_id: str) -> str:
    """Bash that applies the demo and starts its work as the preview Admin.

    The Admin password arrives in the environment as PREVIEW_DEMO_PASSWORD
    (see REMOTE), so it never appears in an argument list, the script text or
    the output.
    """
    origin = f"https://{app_host}"
    docs = "\n---\n".join(json.dumps(doc) for doc in manifests(model_id))
    lines = []
    for key, workflow, title, presentation, steps, attached in WORK:
        body = {
            "workflowKey": workflow,
            "title": title,
            "source": {"previewDemo": True},
            "sourcePresentation": presentation,
        }
        if attached:
            # Uploads give new artifact ids, so work with attached sources is
            # found by title instead of replayed by key.
            lines.append(f"if ! exists {shlex.quote(workflow)} {shlex.quote(title)}; then")
            lines.append("refs='[]'")
            for name in attached:
                lines.append(f"upload {shlex.quote(name)}; refs=$(add_ref \"$refs\" \"$artifact\" {shlex.quote(name)})")
            lines.append(f"start {shlex.quote(key)} \"$(with_refs {shlex.quote(json.dumps(body))} \"$refs\")\"")
            lines.append("fi")
        else:
            lines.append(f"start {shlex.quote(key)} {shlex.quote(json.dumps(body))}")
        for step in steps:
            if step[0] == "answer":
                answer = json.dumps({"outcome": step[1], "response": step[2]})
                lines.append(f"[ \"$created\" = 1 ] && answer {shlex.quote(answer)}")
            elif step[0] == "upload":
                lines.append(f"[ \"$created\" = 1 ] && upload_answer {shlex.quote(step[1])} {shlex.quote(step[2])}")
            elif step[0] == "stop":
                lines.append("[ \"$created\" = 1 ] && stop_wait")
            else:
                lines.append("[ \"$created\" = 1 ] && until_failed")
    docs_b64 = "\n".join(
        f"{shlex.quote(name)}) mime={shlex.quote(mime)}; data={base64.b64encode(data).decode()} ;;"
        for name, (mime, data) in DOCS.items()
    )
    return f"""set -euo pipefail
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
field() {{ python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2]) or "")' "$work/body" "$1"; }}
api() {{
  curl -sS -o "$work/body" -w '%{{http_code}}' -H {shlex.quote(f'Host: {app_host}')} \\
    -H {shlex.quote(f'Origin: {origin}')} -H 'Content-Type: application/json' "$@"
}}
sudo k3s kubectl apply -f - >/dev/null <<'MANIFESTS'
{docs}
MANIFESTS
for workflow in preview-demo-quotes preview-demo-rates preview-demo-customs; do
  sudo k3s kubectl wait --for=jsonpath='{{.status.ready}}'=true -n {NAMESPACE} "workflow/$workflow" --timeout=3m >/dev/null
done
python3 -c 'import json,os,sys; json.dump({{"email": sys.argv[1], "password": os.environ["PREVIEW_DEMO_PASSWORD"]}}, open(sys.argv[2], "w"))' \\
  {shlex.quote(email)} "$work/sign-in"
status=$(api -D "$work/headers" -X POST --data @"$work/sign-in" {shlex.quote(target + '/v1/auth/sign-in')})
rm -f "$work/sign-in"
[ "$status" = 200 ] || {{ echo "preview demo: sign-in returned $status"; exit 1; }}
cookie=$(sed -n 's/^[Ss]et-[Cc]ookie: \\(__Host-iterabase_session=[^;]*\\).*/\\1/p' "$work/headers" | head -1)
csrf=$(field csrfToken)
[ -n "$cookie" ] && [ -n "$csrf" ] || {{ echo 'preview demo: no session'; exit 1; }}
# A replayed start (200) was seeded by an earlier deploy and is left as it is;
# only newly created work (201) is moved on.
created=0 id=''
start() {{
  key=$1 body=$2
  status=$(api -H "Cookie: $cookie" -H "X-CSRF-Token: $csrf" -H "Idempotency-Key: $key" \\
    -X POST --data "$body" {shlex.quote(target + '/v1/work-items')})
  case "$status" in
    200) created=0 ;;
    201) created=1; id=$(field id) ;;
    *) echo "preview demo: start $key returned $status"; exit 1 ;;
  esac
}}
exists() {{
  status=$(api -H "Cookie: $cookie" -G --data-urlencode "workflowKey=$1" --data-urlencode "search=$2" \\
    {shlex.quote(target + '/v1/work-items')})
  [ "$status" = 200 ] || {{ echo "preview demo: lookup returned $status"; exit 1; }}
  created=0
  python3 -c 'import json,sys; sys.exit(0 if any(i.get("title")==sys.argv[2] for i in json.load(open(sys.argv[1]))) else 1)' \\
    "$work/body" "$2"
}}
# Waits until the new work asks for something, then answers it.
blocker() {{
  for _ in $(seq 1 60); do
    status=$(api -H "Cookie: $cookie" "{target}/v1/work-items/$id/blocker")
    [ "$status" = 200 ] && {{ blocker=$(field id); return 0; }}
    sleep 1
  done
  echo "preview demo: $id never asked"; exit 1
}}
respond() {{
  blocker
  status=$(api -H "Cookie: $cookie" -H "X-CSRF-Token: $csrf" -X POST --data "$1" \\
    "{target}/v1/work-blockers/$blocker/responses")
  [ "$status" = 200 ] || {{ echo "preview demo: answer for $id returned $status"; exit 1; }}
}}
answer() {{ respond "$1"; }}
upload() {{
  case "$1" in
{docs_b64}
    *) echo "preview demo: no document $1"; exit 1 ;;
  esac
  printf '%s' "$data" | base64 -d > "$work/doc"
  status=$(curl -sS -o "$work/body" -w '%{{http_code}}' -H {shlex.quote(f'Host: {app_host}')} \\
    -H {shlex.quote(f'Origin: {origin}')} -H "Content-Type: $mime" -H "Cookie: $cookie" -H "X-CSRF-Token: $csrf" \\
    -X POST --data-binary @"$work/doc" {shlex.quote(target + '/v1/artifacts')})
  [ "$status" = 201 ] || {{ echo "preview demo: upload $1 returned $status"; exit 1; }}
  artifact=$(field artifactId)
}}
add_ref() {{
  python3 -c 'import json,sys; r=json.loads(sys.argv[1]); r.append({{"artifactId": sys.argv[2], "metadata": {{"name": sys.argv[3]}}}}); print(json.dumps(r))' "$@"
}}
with_refs() {{
  python3 -c 'import json,sys; b=json.loads(sys.argv[1]); b["artifactRefs"]=json.loads(sys.argv[2]); print(json.dumps(b))' "$@"
}}
upload_answer() {{
  upload "$2"
  respond "$(python3 -c 'import json,sys; print(json.dumps({{"outcome": sys.argv[1], "response": {{}}, "artifactRefs": [{{"artifactId": sys.argv[2], "metadata": {{"name": sys.argv[3]}}}}]}}))' "$1" "$artifact" "$2")"
}}
# Waits until the new work is waiting, then stops the wait (as a person would).
stop_wait() {{
  for _ in $(seq 1 60); do
    status=$(api -H "Cookie: $cookie" "{target}/v1/work-items/$id")
    [ "$status" = 200 ] && [ "$(field state)" = waiting ] && break
    sleep 1
  done
  status=$(api -H "Cookie: $cookie" -H "X-CSRF-Token: $csrf" -X POST "{target}/v1/work-items/$id/wait/stop")
  [ "$status" = 200 ] || {{ echo "preview demo: stop wait for $id returned $status"; exit 1; }}
}}
# Stops the wait and asks again until the loop passes maxTransitions.
until_failed() {{
  for _ in $(seq 1 30); do
    status=$(api -H "Cookie: $cookie" "{target}/v1/work-items/$id")
    [ "$status" = 200 ] || {{ echo "preview demo: read $id returned $status"; exit 1; }}
    case "$(field state)" in
      failed) return 0 ;;
      waiting) stop_wait ;;
      blocked) answer '{{"outcome": "retry", "response": {{}}}}' ;;
      *) sleep 1 ;;
    esac
  done
  echo "preview demo: $id never failed"; exit 1
}}
{chr(10).join(lines)}
echo 'preview demo: Quotes, Rates and Customs seeded'
"""
