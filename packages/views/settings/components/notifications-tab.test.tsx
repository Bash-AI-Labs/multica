import type { ReactNode } from "react";
import {
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const preferences = vi.hoisted(() => ({
  current: {} as Record<string, string>,
}));
const mockGetNotificationPreferences = vi.hoisted(() => vi.fn());
const mockUpdateNotificationPreferences = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({
    id: "workspace-1",
    name: "Acme",
    slug: "acme",
  }),
  useRequiredWorkspaceSlug: () => "acme",
}));
vi.mock("@multica/core/api", () => ({
  api: {
    getNotificationPreferences: mockGetNotificationPreferences,
    updateNotificationPreferences: mockUpdateNotificationPreferences,
  },
}));
vi.mock("sonner", () => ({
  toast: {
    error: mockToastError,
  },
}));
vi.mock("./browser-notification-setting", () => ({
  BrowserNotificationSetting: () => null,
}));

import { NotificationsTab } from "./notifications-tab";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };
const MASKED_WEBHOOK =
  "https://chat.googleapis.com/v1/spaces/AAAA…/messages?••••";

function Wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  return (
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        {children}
      </I18nProvider>
    </QueryClientProvider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  preferences.current = {};
  mockGetNotificationPreferences.mockImplementation(async () => ({
    workspace_id: "workspace-1",
    preferences: { ...preferences.current },
  }));
  mockUpdateNotificationPreferences.mockImplementation(
    async (patch: Record<string, string>) => {
      for (const [key, value] of Object.entries(patch)) {
        if (value === "all" || (key === "google_chat_webhook" && value === "")) {
          delete preferences.current[key];
        } else if (key === "google_chat_webhook") {
          preferences.current[key] = MASKED_WEBHOOK;
        } else {
          preferences.current[key] = value;
        }
      }
      return {
        workspace_id: "workspace-1",
        preferences: { ...preferences.current },
      };
    },
  );
});

describe("NotificationsTab delivery settings", () => {
  it("wires email, Google Chat save, and removal to atomic preference patches", async () => {
    const user = userEvent.setup();
    render(<NotificationsTab />, { wrapper: Wrapper });

    expect(await screen.findByText("Delivery")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Mentions, assignments, and agents needing your input, if still unread after 10 minutes",
      ),
    ).toBeInTheDocument();

    await user.click(
      screen.getByRole("switch", { name: "Email notifications" }),
    );
    await waitFor(() => {
      expect(mockUpdateNotificationPreferences).toHaveBeenCalledWith(
        { email_notifications: "muted" },
        "acme",
      );
    });

    const webhook =
      "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=test&token=test";
    await user.type(
      screen.getByRole("textbox", { name: "Google Chat webhook URL" }),
      webhook,
    );
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => {
      expect(mockUpdateNotificationPreferences).toHaveBeenCalledWith(
        { google_chat_webhook: webhook },
        "acme",
      );
    });
    expect(await screen.findByText(MASKED_WEBHOOK)).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Replace" }),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => {
      expect(mockUpdateNotificationPreferences).toHaveBeenCalledWith(
        { google_chat_webhook: "" },
        "acme",
      );
    });
  });

  it("shows a server confirmation failure through the existing toast", async () => {
    const user = userEvent.setup();
    mockUpdateNotificationPreferences.mockRejectedValueOnce(
      new Error("failed to connect Google Chat webhook"),
    );
    render(<NotificationsTab />, { wrapper: Wrapper });

    await user.type(
      await screen.findByRole("textbox", {
        name: "Google Chat webhook URL",
      }),
      "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=test&token=test",
    );
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockToastError).toHaveBeenCalledWith(
        "failed to connect Google Chat webhook",
      );
    });
  });
});
