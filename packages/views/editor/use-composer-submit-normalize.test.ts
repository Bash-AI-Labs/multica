// @vitest-environment node

import { describe, expect, it } from "vitest";
import { normalizeComposerMarkdown } from "./use-composer-submit";

describe("normalizeComposerMarkdown", () => {
  it("trims leading and trailing whitespace", () => {
    expect(normalizeComposerMarkdown("\n  hello \n\n")).toBe("hello");
  });

  it("normalizes adversarial trailing newlines in linear time", () => {
    const started = performance.now();
    expect(normalizeComposerMarkdown(`hello${"\n ".repeat(50_000)}`)).toBe(
      "hello",
    );
    expect(performance.now() - started).toBeLessThan(50);
  });
});
