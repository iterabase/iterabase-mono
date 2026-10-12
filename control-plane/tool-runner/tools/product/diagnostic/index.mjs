// Reference product tool (HOR-651). It proves the release-versioned product
// layer end to end without touching any external system.
export const identity = { name: "platform.diagnostic", version: "1.0.0" };

export async function invoke(_context, args) {
  const message = typeof args?.message === "string" ? args.message : "";
  return { result: { status: "ok", layer: "product", tool: identity.name, version: identity.version, message } };
}
