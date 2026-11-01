/**
 * Strip mention markdown syntax to plain text.
 *
 * Handles:
 * - Simple mentions: `[@Name](mention://agent/id)` → `@Name`
 * - Escaped brackets in names: `[@David\[TF\]](mention://agent/id)` → `@David[TF]`
 * - Issue mentions (no @): `[MUL-123](mention://issue/id)` → `MUL-123`
 * - Does NOT touch regular markdown links: `[docs](https://...)` stays unchanged
 * - Does NOT touch backslash-escaped mentions: `\[@Name](mention://...)` stays unchanged
 *
 * The scanner mirrors the tokenizer in mention-extension.ts.
 */
export function stripMentionMarkdown(text: string): string {
  const result: string[] = [];
  let copiedThrough = 0;
  let searchFrom = 0;

  for (;;) {
    const open = text.indexOf("[", searchFrom);
    if (open === -1) break;
    if (text[open - 1] === "\\") {
      searchFrom = open + 1;
      continue;
    }

    const prefix = text[open + 1] === "@" ? "@" : "";
    const labelStart = open + 1 + prefix.length;
    let close = labelStart;
    while (close < text.length) {
      if (text[close] === "\\") {
        close += Math.min(2, text.length - close);
        continue;
      }
      if (text[close] === "]") break;
      close++;
    }
    if (close === labelStart || text[close] !== "]") {
      searchFrom = open + 1;
      continue;
    }

    const destinationPrefix = "](mention://";
    if (!text.startsWith(destinationPrefix, close)) {
      searchFrom = open + 1;
      continue;
    }
    let cursor = close + destinationPrefix.length;
    const kindStart = cursor;
    while (
      cursor < text.length &&
      ((text[cursor]! >= "a" && text[cursor]! <= "z") ||
        (text[cursor]! >= "A" && text[cursor]! <= "Z") ||
        (text[cursor]! >= "0" && text[cursor]! <= "9") ||
        text[cursor] === "_")
    ) {
      cursor++;
    }
    if (cursor === kindStart || text[cursor] !== "/") {
      searchFrom = open + 1;
      continue;
    }
    const idStart = ++cursor;
    while (cursor < text.length && text[cursor] !== ")") cursor++;
    if (cursor === idStart || text[cursor] !== ")") {
      searchFrom = open + 1;
      continue;
    }

    const label = text
      .slice(labelStart, close)
      .replaceAll("\\[", "[")
      .replaceAll("\\]", "]");
    result.push(text.slice(copiedThrough, open), `${prefix}${label}`);
    copiedThrough = cursor + 1;
    searchFrom = cursor + 1;
  }

  if (copiedThrough === 0) return text;
  result.push(text.slice(copiedThrough));
  return result.join("");
}
