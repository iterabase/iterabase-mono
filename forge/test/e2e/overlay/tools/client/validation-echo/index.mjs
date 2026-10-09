export const identity = {
  name: "platform.validation.echo",
  version: "1.0.0",
};

export async function invoke(_context, argumentsValue) {
  const message =
    argumentsValue && typeof argumentsValue.message === "string"
      ? argumentsValue.message
      : "";

  return {
    result: {
      environment: "forge-e2e",
      message,
      ok: true,
    },
    artifactRefs: [],
  };
}
