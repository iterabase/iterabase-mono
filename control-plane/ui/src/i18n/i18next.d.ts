import "i18next";
import type { resources } from "./resources";

// English is the source catalog: translation keys are type-checked against it.
declare module "i18next" {
  interface CustomTypeOptions {
    defaultNS: "common";
    fallbackNS: "common";
    resources: (typeof resources)["en"];
  }
}
