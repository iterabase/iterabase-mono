import { describe, expect, it } from "vitest";
import {
  formatDateTime,
  formatDay,
  formatMoney,
  formatNumber,
  formatTime,
} from "./format";
import { legacyT } from "./legacy";

// The approved reference forms from HOR-670 (PT: 1.840 €, 24h, seg., 13 out.).
const nbsp = / | /g;
const plain = (value: string) => value.replace(nbsp, " ");
const monday = new Date(Date.UTC(2026, 9, 12, 15, 5));

describe("locale formatting", () => {
  it("formats amounts", () => {
    expect(plain(formatMoney(1840, "EUR", "pt"))).toBe("1.840 €");
    expect(plain(formatMoney("1840.5", "EUR", "pt"))).toBe("1.840,50 €");
    expect(formatMoney(1840, "EUR", "en")).toBe("€1,840");
    expect(formatMoney(1840, "USD", "en")).toBe("$1,840");
    expect(formatMoney("12.3", "EUR", "en")).toBe("€12.30");
  });

  it("formats numbers", () => {
    expect(formatNumber(1234567.5, "pt")).toBe("1.234.567,5");
    expect(formatNumber(1234567.5, "en")).toBe("1,234,567.5");
    expect(formatNumber("n/a", "en")).toBe("n/a");
  });

  it("formats days, dates and 24-hour times", () => {
    expect(formatDay(monday, "pt", "UTC")).toBe("seg., 12 out.");
    expect(formatDay(monday, "en", "UTC")).toBe("Mon 12 Oct");
    expect(formatTime(monday, "pt", "UTC")).toBe("15:05");
    expect(formatTime(monday, "en", "UTC")).toBe("15:05");
    expect(formatDateTime(monday, "pt", "UTC")).toBe("12 out. 2026, 15:05");
    expect(formatDateTime(monday, "en", "UTC")).toBe("12 Oct 2026, 15:05");
    expect(formatDateTime(undefined, "en")).toBe("");
    expect(formatDateTime("not a date", "en")).toBe("not a date");
  });
});

describe("numbers inside catalog strings", () => {
  it("formats interpolated numbers per locale and keeps plural selection", () => {
    expect(legacyT("pt")("sizeBytes", { count: 1840 })).toBe("1.840 bytes");
    expect(legacyT("en")("sizeBytes", { count: 1840 })).toBe("1,840 bytes");
    expect(legacyT("en")("sizeBytes", { count: 1 })).toBe("1 byte");
    expect(legacyT("pt")("itemsShown", { count: 12000 })).toBe(
      "12.000 itens visíveis",
    );
    expect(
      legacyT("pt")("waitingProgress", { received: 1200, expected: 1840 }),
    ).toBe("1.200 de 1.840 recebidas");
  });
});
