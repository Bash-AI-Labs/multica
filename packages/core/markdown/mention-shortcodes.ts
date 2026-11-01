/**
 * Convert legacy mention shortcodes [@ id="UUID" label="LABEL"] to the
 * standard markdown link format [@LABEL](mention://member/UUID).
 *
 * These shortcodes exist in older database records from a previous mention
 * serialization format. This function normalises them so downstream parsers
 * (Tiptap @tiptap/markdown on web/desktop, the mobile renderer in
 * apps/mobile/lib/markdown/) only need to handle one syntax.
 *
 * Single source of truth for all clients. Mobile imports this directly
 * because mobile is allowed to import pure functions from @multica/core/.
 * Web/desktop continue to access it via @multica/ui/markdown which now
 * re-exports from here, so existing import paths keep working.
 *
 * Pure regex transform — no IO, no global state. Idempotent: running it
 * twice on the same input produces the same output.
 */
export function preprocessMentionShortcodes(text: string): string {
  if (!text.includes("[@ ")) return text;
  const result: string[] = [];
  let copiedThrough = 0;
  let searchFrom = 0;

  for (;;) {
    const open = text.indexOf("[@", searchFrom);
    if (open === -1) break;

    let attrStart = open + 2;
    if (text[attrStart]?.trim() !== "") {
      searchFrom = attrStart;
      continue;
    }
    while (attrStart < text.length && text[attrStart]?.trim() === "") {
      attrStart++;
    }

    const close = text.indexOf("]", attrStart);
    if (close === -1) break;
    const attrString = text.slice(attrStart, close);
    const attrs: Record<string, string> = {};
    const re = /(\w+)="([^"]*)"/g;
    let m: RegExpExecArray | null;
    while ((m = re.exec(attrString)) !== null) {
      if (m[1] && m[2] !== undefined) attrs[m[1]] = m[2];
    }
    const { id, label } = attrs;
    if (id && label) {
      result.push(text.slice(copiedThrough, open));
      result.push(`[@${label}](mention://member/${id})`);
      copiedThrough = close + 1;
    }
    searchFrom = close + 1;
  }

  if (copiedThrough === 0) return text;
  result.push(text.slice(copiedThrough));
  return result.join("");
}
