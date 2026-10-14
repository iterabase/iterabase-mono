import { useMemo, useState, type ReactElement } from "react";
import { useTranslation } from "react-i18next";
import { useSession } from "../app/session";
import { Kbd } from "../components/ui/button";
import { useLocale } from "../i18n";
import { respondToBlocker, uploadArtifact } from "../lib/api";
import type { Blocker, JSONSchema } from "../lib/types";
import { localized } from "../work/model";

// Answering a human gate (REQ-021): the workflow's response schema becomes a
// form, its declared outcomes become the decision buttons. Fields render in
// the body; the buttons live in the fixed decision footer.

type ParsedField = { present: boolean; valid: boolean; value?: unknown };

function parseField(field: JSONSchema, draft: unknown): ParsedField {
  if (draft === undefined || draft === "")
    return { present: false, valid: true };
  if (field.enum) {
    const index = Number(draft);
    return Number.isInteger(index) && index >= 0 && index < field.enum.length
      ? { present: true, valid: true, value: field.enum[index] }
      : { present: true, valid: false };
  }
  if (field.type === "boolean")
    return { present: true, valid: typeof draft === "boolean", value: draft };
  if (field.type === "number" || field.type === "integer") {
    const value = Number(draft);
    return {
      present: true,
      valid:
        Number.isFinite(value) &&
        (field.type !== "integer" || Number.isInteger(value)),
      value,
    };
  }
  if (field.type === "object" || field.type === "array") {
    try {
      const value = JSON.parse(String(draft)) as unknown;
      const valid =
        field.type === "array"
          ? Array.isArray(value)
          : !!value && typeof value === "object" && !Array.isArray(value);
      return { present: true, valid, value };
    } catch {
      return { present: true, valid: false };
    }
  }
  return { present: true, valid: true, value: String(draft) };
}

export interface BlockerResponse {
  fields: ReactElement | null;
  outcomes: Array<{ outcome: string; label: string }>;
  valid: boolean;
  busy: boolean;
  error: string;
  /** True when the gate can be answered with a button alone. */
  quick: boolean;
  /**
   * Set when the gate asks one question with a fixed list of answers, so each
   * answer can be its own button (XTItemQuestion): picking one submits the
   * first outcome with that answer. Any further outcomes are answered without
   * it ("None of these").
   */
  choice: Choice | null;
  /** Submits an outcome; `answers` (field key → draft) override the form. */
  submit: (
    outcome: string,
    answers?: Record<string, unknown>,
  ) => Promise<boolean>;
}

export interface Choice {
  key: string;
  label: string;
  outcome: string;
  options: Array<{ draft: string; label: string }>;
  others: Array<{ outcome: string; label: string }>;
}

const NO_BLOCKER: Blocker = {
  id: "",
  workItemId: "",
  attemptId: "",
  kind: "",
  title: {},
  description: {},
  responseSchema: {},
  allowedOutcomes: [],
  responsePresentation: { outcomes: [] },
  state: "resolved",
  createdAt: "",
};

export function useBlockerResponse(
  open: Blocker | null,
  onDone: (outcome: string) => Promise<void> | void,
): BlockerResponse {
  const blocker = open ?? NO_BLOCKER;
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const [drafts, setDrafts] = useState<Record<string, unknown>>({});
  const [confirmed, setConfirmed] = useState<string[]>([]);
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const schemaFields = useMemo(
    () => Object.entries(blocker.responseSchema.properties ?? {}),
    [blocker],
  );
  const presentation = new Map(
    (blocker.responsePresentation?.fields ?? []).map((field) => [
      field.key,
      field,
    ]),
  );
  const required = blocker.responseSchema.required ?? [];
  const consequences = blocker.requiredConsequences ?? [];
  const consequencesConfirmed =
    blocker.kind !== "consequence_confirmation" ||
    consequences.every((entry) => confirmed.includes(entry.invocationId));

  function evaluate(current: Record<string, unknown>) {
    const parsed = new Map(
      schemaFields.map(([key, field]) => [
        key,
        parseField(field, current[key]),
      ]),
    );
    const response = Object.fromEntries(
      schemaFields.flatMap(([key]) => {
        const value = parsed.get(key);
        return value?.present && value.valid ? [[key, value.value]] : [];
      }),
    );
    const valid =
      required.every(
        (key) => parsed.get(key)?.present && parsed.get(key)?.valid,
      ) &&
      [...parsed.values()].every((value) => !value.present || value.valid) &&
      (blocker.kind !== "artifact" || !!file) &&
      consequencesConfirmed;
    return { parsed, response, valid };
  }
  const { parsed, valid } = evaluate(drafts);
  const quick =
    schemaFields.length === 0 &&
    blocker.kind !== "artifact" &&
    blocker.kind !== "consequence_confirmation";

  const outcomeLabels = blocker.responsePresentation?.outcomes ?? [];
  const outcomes = blocker.allowedOutcomes.map((outcome, index) => ({
    outcome,
    label:
      localized(outcomeLabels[index], locale) ||
      t("respond.choiceNumbered", { position: index + 1 }),
  }));

  const only = schemaFields.length === 1 ? schemaFields[0] : undefined;
  const choice: Choice | null =
    only &&
    only[1].enum &&
    only[1].enum.length > 0 &&
    outcomes.length > 0 &&
    (outcomes.length === 1 || !required.includes(only[0])) &&
    blocker.kind !== "artifact" &&
    blocker.kind !== "consequence_confirmation"
      ? {
          key: only[0],
          label:
            localized(presentation.get(only[0])?.label, locale) ||
            t("respond.fieldNumbered", { position: 1 }),
          outcome: outcomes[0].outcome,
          options: only[1].enum.map((_, index) => ({
            draft: String(index),
            label:
              localized(presentation.get(only[0])?.options?.[index], locale) ||
              t("respond.choiceNumbered", { position: index + 1 }),
          })),
          others: outcomes.slice(1),
        }
      : null;

  async function submit(
    outcome: string,
    answers?: Record<string, unknown>,
  ): Promise<boolean> {
    const { response, valid } = evaluate(
      answers ? { ...drafts, ...answers } : drafts,
    );
    if (!valid || busy) return false;
    setBusy(true);
    setError("");
    try {
      const refs: Array<{ artifactId: string; metadata?: unknown }> = [];
      if (file) {
        const artifact = await session.withCSRF((csrf) =>
          uploadArtifact(csrf, file),
        );
        refs.push({
          artifactId: artifact.artifactId,
          metadata: { name: file.name },
        });
      }
      await session.withCSRF((csrf) =>
        respondToBlocker(csrf, blocker, outcome, response, refs, confirmed),
      );
      await onDone(outcome);
      return true;
    } catch (err) {
      if (!session.lost(err)) setError(t("respond.failed"));
      return false;
    } finally {
      setBusy(false);
    }
  }

  const set = (key: string, value: unknown) =>
    setDrafts((current) => ({ ...current, [key]: value }));

  const fields = quick ? null : (
    <div className="flex flex-col gap-4">
      {schemaFields.map(([key, field], index) => {
        const state = parsed.get(key);
        const label =
          localized(presentation.get(key)?.label, locale) ||
          t("respond.fieldNumbered", { position: index + 1 });
        const id = `field-${blocker.id}-${key}`;
        return (
          <div key={key} className="ib-field">
            {field.type === "boolean" && !field.enum ? (
              <label className="ib-check" htmlFor={id}>
                <input
                  id={id}
                  type="checkbox"
                  checked={drafts[key] === true}
                  onChange={(event) => set(key, event.target.checked)}
                />
                <span className="ib-check__box" aria-hidden="true">
                  <svg viewBox="0 0 12 12" fill="none">
                    <path
                      d="M2.5 6.5l2.2 2L9.5 3.5"
                      stroke="currentColor"
                      strokeWidth="1.8"
                      strokeLinecap="round"
                    />
                  </svg>
                </span>
                {label}
              </label>
            ) : (
              <>
                <label className="ib-label" htmlFor={id}>
                  {label}
                </label>
                {field.enum ? (
                  <select
                    id={id}
                    className="ib-select"
                    value={String(drafts[key] ?? "")}
                    onChange={(event) => set(key, event.target.value)}
                  >
                    <option value="">{t("respond.choose")}</option>
                    {field.enum.map((option, optionIndex) => (
                      <option value={optionIndex} key={JSON.stringify(option)}>
                        {localized(
                          presentation.get(key)?.options?.[optionIndex],
                          locale,
                        ) ||
                          t("respond.choiceNumbered", {
                            position: optionIndex + 1,
                          })}
                      </option>
                    ))}
                  </select>
                ) : (
                  <textarea
                    id={id}
                    className="ib-input py-2 min-h-24"
                    inputMode={
                      field.type === "number" || field.type === "integer"
                        ? "decimal"
                        : undefined
                    }
                    aria-invalid={
                      state?.present && !state.valid ? true : undefined
                    }
                    value={String(drafts[key] ?? "")}
                    onChange={(event) => set(key, event.target.value)}
                  />
                )}
              </>
            )}
            {state?.present && !state.valid && (
              <span className="ib-hint ib-hint--error" role="alert">
                {t(
                  `respond.invalid.${field.type === "integer" ? "integer" : field.type === "number" ? "number" : field.type === "array" ? "array" : "object"}`,
                )}
              </span>
            )}
          </div>
        );
      })}
      {blocker.kind === "artifact" && (
        <div className="ib-field">
          <label className="ib-label" htmlFor={`file-${blocker.id}`}>
            {t("respond.chooseFile")}
          </label>
          <input
            id={`file-${blocker.id}`}
            type="file"
            onChange={(event) => setFile(event.target.files?.[0] ?? null)}
          />
        </div>
      )}
      {blocker.kind === "consequence_confirmation" &&
        consequences.length > 0 && (
          <fieldset className="ib-card ib-card--pad m-0 flex flex-col gap-3">
            <legend className="font-semibold px-1">
              {t("respond.repeatTitle")}
            </legend>
            <p className="m-0 text-sm text-ink-2">{t("respond.repeatBody")}</p>
            {consequences.map((consequence) => (
              <label key={consequence.invocationId} className="ib-check">
                <input
                  type="checkbox"
                  checked={confirmed.includes(consequence.invocationId)}
                  onChange={(event) =>
                    setConfirmed((current) =>
                      event.target.checked
                        ? [...current, consequence.invocationId]
                        : current.filter(
                            (id) => id !== consequence.invocationId,
                          ),
                    )
                  }
                />
                <span className="ib-check__box" aria-hidden="true">
                  <svg viewBox="0 0 12 12" fill="none">
                    <path
                      d="M2.5 6.5l2.2 2L9.5 3.5"
                      stroke="currentColor"
                      strokeWidth="1.8"
                      strokeLinecap="round"
                    />
                  </svg>
                </span>
                {localized(consequence.summary, locale)}
              </label>
            ))}
          </fieldset>
        )}
    </div>
  );

  return { fields, outcomes, valid, busy, error, quick, choice, submit };
}

/**
 * The decision buttons: the first declared outcome is primary (A), the last of
 * several is "not this one" (N). Keyboard hints mirror visible buttons only.
 */
export function OutcomeButtons({
  response,
  keys = false,
  extra,
}: {
  response: BlockerResponse;
  keys?: boolean;
  extra?: ReactElement;
}) {
  const { t } = useTranslation("work");
  const { outcomes, valid, busy } = response;
  const last = outcomes.length - 1;
  return (
    <div className="outcome-buttons">
      {outcomes.map(({ outcome, label }, index) => (
        <button
          key={outcome}
          type="button"
          className={`btn btn-lg ${index === 0 ? "btn-primary outcome-buttons__primary" : "btn-secondary"}`}
          disabled={!valid || busy}
          onClick={() => void response.submit(outcome)}
        >
          {label}
          {keys && index === 0 && <Kbd>{t("key.a", { ns: "app" })}</Kbd>}
          {keys && index === last && last > 0 && (
            <Kbd>{t("key.n", { ns: "app" })}</Kbd>
          )}
        </button>
      ))}
      {extra}
    </div>
  );
}
