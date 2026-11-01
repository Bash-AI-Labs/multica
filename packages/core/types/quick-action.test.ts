// @vitest-environment node

import { describe, expect, it } from "vitest";
import { findQuickActionTemplateToken } from "./quick-action";

describe("findQuickActionTemplateToken", () => {
  it("returns the first complete token without crossing a stray brace", () => {
    expect(findQuickActionTemplateToken("Use {{issue.title}} here")).toBe(
      "{{issue.title}}",
    );
    expect(findQuickActionTemplateToken("Ignore {{broken} and {{valid}}")).toBe(
      "{{valid}}",
    );
  });

  it("handles adversarial opening delimiters in linear time", () => {
    const started = performance.now();
    expect(findQuickActionTemplateToken("{{".repeat(100_000))).toBeNull();
    expect(performance.now() - started).toBeLessThan(50);
  });
});
