import type {
  NotificationGroupKey,
  NotificationGroupValue,
  NotificationPreferences,
} from "../types";

const NOTIFICATION_GROUP_KEYS: readonly NotificationGroupKey[] = [
  "assignments",
  "status_changes",
  "comments",
  "mentions",
  "updates",
  "agent_activity",
  "system_notifications",
  "email_notifications",
];

function preferenceValue(
  preferences: NotificationPreferences,
  key: NotificationGroupKey,
): NotificationGroupValue {
  return preferences[key] ?? "all";
}

/**
 * Convert the full preference object produced by the settings UI into the
 * smallest atomic patch. Missing keys mean the default value ("all").
 */
export function deriveNotificationPreferencePatch(
  previous: NotificationPreferences,
  next: NotificationPreferences,
): NotificationPreferences {
  const patch: NotificationPreferences = {};

  for (const key of NOTIFICATION_GROUP_KEYS) {
    const previousValue = preferenceValue(previous, key);
    const nextValue = preferenceValue(next, key);
    if (previousValue !== nextValue) {
      patch[key] = nextValue;
    }
  }

  const previousWebhook = previous.google_chat_webhook ?? "";
  const nextWebhook = next.google_chat_webhook ?? "";
  if (previousWebhook !== nextWebhook) {
    patch.google_chat_webhook = nextWebhook;
  }

  return patch;
}

/** Apply a preference patch while keeping default "all" values sparse. */
export function applyNotificationPreferencePatch(
  current: NotificationPreferences,
  patch: NotificationPreferences,
): NotificationPreferences {
  const next = { ...current };

  for (const key of NOTIFICATION_GROUP_KEYS) {
    const value = patch[key];
    if (value === "muted") {
      next[key] = value;
    } else if (value === "all") {
      delete next[key];
    }
  }

  if (patch.google_chat_webhook !== undefined) {
    if (patch.google_chat_webhook === "") {
      delete next.google_chat_webhook;
    } else {
      next.google_chat_webhook = patch.google_chat_webhook;
    }
  }

  return next;
}

/**
 * Roll back only values that still match this mutation's optimistic patch.
 * A later toggle that touched the same key wins.
 */
export function rollbackNotificationPreferencePatch(
  current: NotificationPreferences,
  patch: NotificationPreferences,
  previous: NotificationPreferences,
): NotificationPreferences {
  const next = { ...current };

  for (const key of NOTIFICATION_GROUP_KEYS) {
    const patchedValue = patch[key];
    if (
      patchedValue === undefined ||
      preferenceValue(current, key) !== patchedValue
    ) {
      continue;
    }

    const previousValue = preferenceValue(previous, key);
    if (previousValue === "all") {
      delete next[key];
    } else {
      next[key] = previousValue;
    }
  }

  if (
    patch.google_chat_webhook !== undefined &&
    (current.google_chat_webhook ?? "") === patch.google_chat_webhook
  ) {
    const previousWebhook = previous.google_chat_webhook ?? "";
    if (previousWebhook === "") {
      delete next.google_chat_webhook;
    } else {
      next.google_chat_webhook = previousWebhook;
    }
  }

  return next;
}
