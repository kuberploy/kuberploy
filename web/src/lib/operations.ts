import { titleCase } from "./format";

export function operationTitle(kind: string): string {
  switch (kind) {
    case "deployment.git-write":
      return "Apply App change";
    case "deployment.config-draft-save":
      return "Save App draft";
    case "deployment.clone-draft":
      return "Clone App draft";
    case "variable-set.git-write":
      return "Apply variable changes";
    default:
      return titleCase(kind);
  }
}
