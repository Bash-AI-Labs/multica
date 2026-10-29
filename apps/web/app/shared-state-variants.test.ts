// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import postcss from "postcss";
import tailwind from "@tailwindcss/postcss";
import { beforeAll, describe, expect, it } from "vitest";

const stateVariants = [
  ["data-open", ["[data-state=open]", "[data-open]:not([data-open=false])"]],
  ["data-closed", ["[data-state=closed]", "[data-closed]:not([data-closed=false])"]],
  ["data-checked", ["[data-state=checked]", "[data-checked]:not([data-checked=false])"]],
  ["data-unchecked", ["[data-state=unchecked]", "[data-unchecked]:not([data-unchecked=false])"]],
  ["data-selected", ["[data-selected=true]"]],
  ["data-disabled", ["[data-disabled=true]", "[data-disabled]:not([data-disabled=false])"]],
  ["data-active", ["[data-state=active]", "[data-active]:not([data-active=false])"]],
  ["data-horizontal", ["[data-orientation=horizontal]"]],
  ["data-vertical", ["[data-orientation=vertical]"]],
] as const;

describe("shared component styles without the shadcn CLI", () => {
  const rules = new Map<string, string>();
  const keyframes = new Map<string, string>();

  beforeAll(async () => {
    const entry = resolve(process.cwd(), "app/globals.css");
    const candidates = [
      ...stateVariants.map(([variant]) => `${variant}:block`),
      "no-scrollbar",
      "animate-accordion-down",
      "animate-accordion-up",
    ];
    const css = `${readFileSync(entry, "utf8")}\n@source inline("${candidates.join(" ")}");`;
    const built = await postcss([tailwind({ base: dirname(entry) })]).process(css, {
      from: entry,
    });
    built.root.walkRules((rule) => {
      rules.set(rule.selector, rule.toString());
    });
    built.root.walkAtRules("keyframes", (rule) => {
      keyframes.set(rule.params, rule.toString());
    });
  }, 60_000);

  it.each(stateVariants)("%s retains its state selectors", (variant, selectors) => {
    const css = rules.get(`.${variant}\\:block`)?.replace(/["']/g, "");
    for (const selector of selectors) {
      expect(css).toContain(selector);
    }
  });

  it("preserves hidden scrollbars in Firefox and WebKit", () => {
    const css = rules.get(".no-scrollbar");
    expect(css).toMatch(/scrollbar-width:\s*none/);
    expect(css).toContain("::-webkit-scrollbar");
    expect(css).toMatch(/display:\s*none/);
  });

  it.each(["accordion-down", "accordion-up"])("%s retains both panel-height APIs", (name) => {
    const css = keyframes.get(name);
    expect(css).toContain("--radix-accordion-content-height");
    expect(css).toContain("--accordion-panel-height");
  });
});
