export function Hardcoded({
  t,
  busy,
  name,
}: {
  t: (key: string) => string;
  busy: boolean;
  name?: string;
}) {
  return (
    <div title={t("ok")} className={busy ? "busy" : "idle"}>
      <button>Save changes</button>
      <button aria-label="Close dialog">×</button>
      <span>{"Cancel"}</span>
      <span>{t("fine")} · 3</span>
      <button>{busy ? "Saving" : "Save"}</button>
      <input placeholder={"Work email"} />
      <input placeholder={busy ? t("wait") : `Search ${name}`} />
      <span>{name || "Unknown"}</span>
      <span>{busy && t("busy")}</span>
      <span>{busy ? t("yes") : t("no")}</span>
    </div>
  );
}
