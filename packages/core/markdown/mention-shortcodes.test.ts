// @vitest-environment node

import { describe, expect, it } from "vitest";
import { preprocessMentionShortcodes } from "./mention-shortcodes";

describe("preprocessMentionShortcodes", () => {
  it("converts complete legacy member mentions and preserves malformed ones", () => {
    expect(
      preprocessMentionShortcodes(
        'Hello [@ id="member-1" label="Alice"] and [@ label="Missing id"]',
      ),
    ).toBe(
      "Hello [@Alice](mention://member/member-1) and [@ label=\"Missing id\"]",
    );
  });

  it("handles adversarial unclosed attributes in linear time", () => {
    const input = `[@ ${"  ".repeat(50_000)}id="member-1"`;
    const started = performance.now();
    expect(preprocessMentionShortcodes(input)).toBe(input);
    expect(performance.now() - started).toBeLessThan(50);
  });
});
