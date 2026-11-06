import { describe, expect, it } from "vitest";
import {
  applyNotificationPreferencePatch,
  deriveNotificationPreferencePatch,
  rollbackNotificationPreferencePatch,
} from "./patch";

describe("notification preference patches", () => {
  it("sends only the changed key when another muted preference is present", () => {
    expect(
      deriveNotificationPreferencePatch(
        { status_changes: "muted" },
        { status_changes: "muted", comments: "muted" },
      ),
    ).toEqual({ comments: "muted" });
  });

  it("uses an explicit all value when enabling a previously muted group", () => {
    expect(
      deriveNotificationPreferencePatch(
        { status_changes: "muted", comments: "muted" },
        { comments: "muted" },
      ),
    ).toEqual({ status_changes: "all" });
  });

  it("merges optimistic patches without dropping unrelated preferences", () => {
    expect(
      applyNotificationPreferencePatch(
        { status_changes: "muted" },
        { comments: "muted" },
      ),
    ).toEqual({ status_changes: "muted", comments: "muted" });
  });

  it("carries the mentions group independently of comments", () => {
    // The point of the split (#6468): muting comment volume must not travel
    // to mentions. A missing key in NOTIFICATION_GROUP_KEYS would make the
    // toggle silently un-sendable, so derive and apply are both covered.
    expect(
      deriveNotificationPreferencePatch(
        { comments: "muted" },
        { comments: "muted", mentions: "muted" },
      ),
    ).toEqual({ mentions: "muted" });

    expect(
      deriveNotificationPreferencePatch({ comments: "muted" }, {}),
    ).toEqual({ comments: "all" });

    expect(
      applyNotificationPreferencePatch(
        { comments: "muted" },
        { mentions: "muted" },
      ),
    ).toEqual({ comments: "muted", mentions: "muted" });
  });

  it("patches email delivery like the other toggles", () => {
    expect(
      deriveNotificationPreferencePatch(
        {},
        { email_notifications: "muted" },
      ),
    ).toEqual({ email_notifications: "muted" });
    expect(
      applyNotificationPreferencePatch(
        { email_notifications: "muted" },
        { email_notifications: "all" },
      ),
    ).toEqual({});
  });

  it("sets, clears, and rolls back Google Chat webhook strings", () => {
    const masked =
      "https://chat.googleapis.com/v1/spaces/AAAA…/messages?••••";
    const replacement =
      "https://chat.googleapis.com/v1/spaces/BBBB/messages?key=x&token=y";

    expect(
      deriveNotificationPreferencePatch(
        { google_chat_webhook: masked },
        { google_chat_webhook: replacement },
      ),
    ).toEqual({ google_chat_webhook: replacement });
    expect(
      deriveNotificationPreferencePatch(
        { google_chat_webhook: masked },
        {},
      ),
    ).toEqual({ google_chat_webhook: "" });
    expect(
      applyNotificationPreferencePatch(
        { google_chat_webhook: masked },
        { google_chat_webhook: "" },
      ),
    ).toEqual({});
    expect(
      rollbackNotificationPreferencePatch(
        { google_chat_webhook: replacement },
        { google_chat_webhook: replacement },
        { google_chat_webhook: masked },
      ),
    ).toEqual({ google_chat_webhook: masked });
  });

  it("does not roll back a key that a later mutation already changed", () => {
    expect(
      rollbackNotificationPreferencePatch(
        { comments: "muted" },
        { comments: "muted" },
        {},
      ),
    ).toEqual({});

    expect(
      rollbackNotificationPreferencePatch(
        {},
        { comments: "muted" },
        {},
      ),
    ).toEqual({});
  });
});
