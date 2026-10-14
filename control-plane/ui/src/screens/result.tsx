import { useTranslation } from "react-i18next";
import { useLocale, type Locale } from "../i18n";
import { formatNumber } from "../i18n/format";
import type {
  DetailData,
  NodeExecution,
  ResultFieldPresentation,
} from "../lib/types";
import { localized } from "../work/model";

// A step's structured result, shown through the workflow's declared field
// labels (REQ-021, HOR-396). Raw keys, prompts and traces never render.

function safeObject(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function equalJSON(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true;
  if (Array.isArray(left) || Array.isArray(right))
    return (
      Array.isArray(left) &&
      Array.isArray(right) &&
      left.length === right.length &&
      left.every((value, index) => equalJSON(value, right[index]))
    );
  if (!left || !right || typeof left !== "object" || typeof right !== "object")
    return false;
  const a = safeObject(left);
  const b = safeObject(right);
  const keysA = Object.keys(a).sort();
  const keysB = Object.keys(b).sort();
  return (
    keysA.length === keysB.length &&
    keysA.every(
      (key, index) => key === keysB[index] && equalJSON(a[key], b[key]),
    )
  );
}

function displayValue(value: unknown, locale: Locale): string {
  return typeof value === "number"
    ? formatNumber(value, locale)
    : String(value);
}

function direct(fields: ResultFieldPresentation[], prefix: string[]) {
  return fields.filter(
    (field) =>
      field.path.length === prefix.length + 1 &&
      prefix.every((segment, index) => field.path[index] === segment),
  );
}

/** The latest step that produced a presentable result (the "exact content"). */
export function producingNode(detail: DetailData): NodeExecution | undefined {
  return detail.nodes
    .filter(
      (node) =>
        node.attemptId === detail.item.currentAttemptId &&
        node.state === "succeeded" &&
        node.kind === "agent_task",
    )
    .sort((a, b) => a.executionSeq - b.executionSeq)
    .at(-1);
}

export function hasPresentedResult(node: NodeExecution | undefined): boolean {
  const fields = node?.resultPresentation?.fields ?? [];
  const value = node?.output;
  return (
    fields.length > 0 &&
    !!value &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    direct(fields, []).some((field) =>
      Object.prototype.hasOwnProperty.call(value, field.path[0]),
    )
  );
}

export function PresentedResult({ node }: { node: NodeExecution }) {
  const locale = useLocale();
  return (
    <Fields
      value={safeObject(node.output)}
      fields={node.resultPresentation?.fields ?? []}
      prefix={[]}
      locale={locale}
    />
  );
}

function Fields({
  value,
  fields,
  prefix,
  locale,
}: {
  value: Record<string, unknown>;
  fields: ResultFieldPresentation[];
  prefix: string[];
  locale: Locale;
}) {
  return (
    <dl className="m-0 grid gap-x-4 gap-y-2 grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
      {direct(fields, prefix).flatMap((field) => {
        const key = field.path.at(-1)!;
        if (!Object.prototype.hasOwnProperty.call(value, key)) return [];
        return [
          <div key={field.path.join("\u0000")} className="contents">
            <dt className="text-sm text-ink-3">
              {localized(field.label, locale)}
            </dt>
            <dd className="m-0 whitespace-pre-line">
              <FieldValue
                value={value[key]}
                field={field}
                fields={fields}
                locale={locale}
              />
            </dd>
          </div>,
        ];
      })}
    </dl>
  );
}

function FieldValue({
  value,
  field,
  fields,
  locale,
}: {
  value: unknown;
  field: ResultFieldPresentation;
  fields: ResultFieldPresentation[];
  locale: Locale;
}) {
  const { t } = useTranslation("work");
  const option = field.options?.find((candidate) =>
    equalJSON(candidate.value, value),
  );
  if (option) return <>{localized(option.label, locale)}</>;
  if (typeof value === "boolean") return <>{value ? t("yes") : t("no")}</>;
  const nested = direct(fields, field.path).length > 0;
  if (Array.isArray(value)) {
    if (nested)
      return (
        <div className="flex flex-col gap-2">
          {value.map((entry, index) =>
            entry && typeof entry === "object" && !Array.isArray(entry) ? (
              <Fields
                key={index}
                value={safeObject(entry)}
                fields={fields}
                prefix={field.path}
                locale={locale}
              />
            ) : null,
          )}
        </div>
      );
    return (
      <ul className="m-0 pl-5 list-disc">
        {value.map((entry, index) => {
          const entryOption = field.options?.find((candidate) =>
            equalJSON(candidate.value, entry),
          );
          return (
            <li key={index}>
              {entryOption
                ? localized(entryOption.label, locale)
                : typeof entry === "boolean"
                  ? entry
                    ? t("yes")
                    : t("no")
                  : displayValue(entry, locale)}
            </li>
          );
        })}
      </ul>
    );
  }
  if (value && typeof value === "object" && nested)
    return (
      <Fields
        value={safeObject(value)}
        fields={fields}
        prefix={field.path}
        locale={locale}
      />
    );
  return (
    <>
      {value === null || typeof value === "object"
        ? "—"
        : displayValue(value, locale)}
    </>
  );
}
