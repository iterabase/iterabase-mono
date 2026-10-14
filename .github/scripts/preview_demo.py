"""Demo AI coworkers and work for a seeded preview (HOR-459, DES-HOR-459-06).

A preview has no customer workflows, so the app's screens would be empty and
could not be reviewed. In `seeded` admin mode the deploy applies two demo
coworkers modelled on the "Delight, trusted" prototype (Quotes and Rates) and
starts work through the real customer API as the preview Admin:

* Each workflow begins at a human gate, so new work is in Needs you at once
  with no worker or inference.
* Answering a gate moves the work to an agent step on a paused AgentPool
  (no replicas), where it honestly stays Working.
* Declining a quote ends the work: Done.

Every start carries a fixed Idempotency-Key and every answer is conditional
on the item still being blocked, so redeploying the same preview never adds
or changes demo work. The demo exists only on previews; nothing here ships.
"""

from __future__ import annotations

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
        "version": "1",
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
                _text("Reads customer emails, PDFs and handwriting", "Lê emails, PDFs e letra manuscrita dos clientes"),
                _text("Asks your usual suppliers for rates", "Pede preços aos seus fornecedores habituais"),
                _text("Prices with your margin rules", "Calcula o preço com as suas regras de margem"),
            ],
            "connections": [_text("Microsoft 365 mailbox · quotes@", "Caixa de correio Microsoft 365 · quotes@")],
        },
    })
    rates = _workflow("preview-demo-rates", {
        "key": "demo/rates",
        "version": "1",
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
                _text("Logs in to FreightNet and CargoHub", "Entra no FreightNet e no CargoHub"),
                _text("Reads rates, never submits anything", "Lê preços, nunca submete nada"),
            ],
            "connections": [_text("FreightNet portal", "Portal FreightNet")],
        },
    })
    return [ca, pool, quotes, rates]


# (key, workflow, title, source presentation, answer): answer is None to leave
# the work in Needs you, or the (outcome, response) that moves it on.
WORK: list[tuple[str, str, str, dict, tuple[str, dict] | None]] = [
    ("demo-1039", "demo/quotes", "Quote · Leixões → Santos, LCL",
     {"kind": "email", "title": "compras@outroexemplo.pt", "subtitle": "Pedido de cotação LCL Leixões–Santos"}, None),
    ("demo-1045", "demo/quotes", "Quote · Lisbon → Luanda, reefer",
     {"kind": "email", "title": "Angola Trade", "subtitle": "Pedido de cotação"}, None),
    ("demo-2094", "demo/rates", "FreightNet login",
     {"kind": "schedule", "title": "Every Monday 06:00"}, None),
    ("demo-1051", "demo/quotes", "Quote · Porto → Rotterdam, 2×40′HC",
     {"kind": "email", "title": "ana@portofreight.pt", "subtitle": "Pedido de cotação"}, ("approved", {})),
    ("demo-2093", "demo/rates", "Weekly portal rates · week 42",
     {"kind": "schedule", "title": "Every Monday 06:00"}, ("entered", {"code": "482913"})),
    ("demo-1042", "demo/quotes", "Quote · Bilbao → Porto, LTL",
     {"kind": "email", "title": "compras@exemplo.pt", "subtitle": "Pedido de cotação"}, ("declined", {})),
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
    starts = []
    for key, workflow, title, presentation, answer in WORK:
        body = json.dumps({
            "workflowKey": workflow,
            "title": title,
            "source": {"previewDemo": True},
            "sourcePresentation": presentation,
        })
        line = f"start {shlex.quote(key)} {shlex.quote(body)}"
        if answer:
            outcome, response = answer
            line += f" {shlex.quote(json.dumps({'outcome': outcome, 'response': response}))}"
        starts.append(line)
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
for workflow in preview-demo-quotes preview-demo-rates; do
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
# only newly created work (201) is answered.
start() {{
  key=$1 body=$2 answer=${{3:-}}
  status=$(api -H "Cookie: $cookie" -H "X-CSRF-Token: $csrf" -H "Idempotency-Key: $key" \\
    -X POST --data "$body" {shlex.quote(target + '/v1/work-items')})
  case "$status" in 200) return 0 ;; 201) ;; *) echo "preview demo: start $key returned $status"; exit 1 ;; esac
  [ -n "$answer" ] || return 0
  id=$(field id)
  for _ in $(seq 1 60); do
    status=$(api -H "Cookie: $cookie" "{target}/v1/work-items/$id/blocker")
    [ "$status" = 200 ] && break
    sleep 1
  done
  [ "$status" = 200 ] || {{ echo "preview demo: $key never asked"; exit 1; }}
  blocker=$(field id)
  status=$(api -H "Cookie: $cookie" -H "X-CSRF-Token: $csrf" -X POST --data "$answer" \\
    "{target}/v1/work-blockers/$blocker/responses")
  [ "$status" = 200 ] || {{ echo "preview demo: answer $key returned $status"; exit 1; }}
}}
{chr(10).join(starts)}
echo 'preview demo: Quotes and Rates seeded'
"""
