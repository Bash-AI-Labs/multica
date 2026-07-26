/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { createAuthStore, registerAuthStore, useAuthStore } from "../auth";
import { configStore } from "../config";
import type { User } from "../types";
import type { StorageAdapter } from "../types/storage";
import { workspaceKeys } from "../workspace/queries";
import { AuthInitializer } from "./auth-initializer";

vi.mock("../analytics", () => ({
  captureSignupSource: vi.fn(),
  identify: vi.fn(),
  initAnalytics: vi.fn(),
  resetAnalytics: vi.fn(),
}));

const user: User = {
  id: "user-1",
  name: "Local User",
  email: "local@example.com",
  avatar_url: null,
  onboarded_at: null,
  onboarding_questionnaire: {},
  starter_content_state: null,
  language: null,
  profile_description: "",
  timezone: null,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function createStorage(): StorageAdapter & { values: Map<string, string> } {
  const values = new Map<string, string>();
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
  };
}

function wrapper(queryClient: QueryClient, children: ReactNode) {
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

describe("AuthInitializer local mode", () => {
  let queryClient: QueryClient;
  let storage: ReturnType<typeof createStorage>;

  beforeEach(() => {
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    storage = createStorage();
    configStore.setState({ localModeEnabled: false });
  });

  afterEach(() => {
    queryClient.clear();
    vi.restoreAllMocks();
  });

  it("uses the server config to auto-login in token mode and seeds workspaces", async () => {
    const workspaces = [{ id: "ws-1", name: "Local", slug: "local" }];
    const api = {
      getConfig: vi.fn().mockResolvedValue({
        cdn_domain: "",
        allow_signup: true,
        local_mode_enabled: true,
      }),
      localLogin: vi.fn().mockResolvedValue({ token: "local-token", user }),
      listWorkspaces: vi.fn().mockResolvedValue(workspaces),
      setToken: vi.fn(),
    } as unknown as ApiClient;
    setApiInstance(api);
    registerAuthStore(createAuthStore({ api, storage }));

    render(
      wrapper(
        queryClient,
        <AuthInitializer storage={storage}>
          <div>ready</div>
        </AuthInitializer>,
      ),
    );

    await waitFor(() => expect(api.localLogin).toHaveBeenCalledOnce());
    expect(storage.values.get("multica_token")).toBe("local-token");
    expect(api.setToken).toHaveBeenCalledWith("local-token");
    expect(useAuthStore.getState().user).toEqual(user);
    expect(queryClient.getQueryData(workspaceKeys.list())).toEqual(workspaces);
  });

  it.each([
    ["omitted", {}],
    ["false", { local_mode_enabled: false }],
  ])("does not auto-login when local mode is %s", async (_label, config) => {
    const api = {
      getConfig: vi.fn().mockResolvedValue({
        cdn_domain: "",
        allow_signup: true,
        ...config,
      }),
      localLogin: vi.fn(),
      setToken: vi.fn(),
    } as unknown as ApiClient;
    setApiInstance(api);
    registerAuthStore(createAuthStore({ api, storage }));

    render(
      wrapper(
        queryClient,
        <AuthInitializer storage={storage}>
          <div>ready</div>
        </AuthInitializer>,
      ),
    );

    await waitFor(() => expect(useAuthStore.getState().isLoading).toBe(false));
    expect(api.localLogin).not.toHaveBeenCalled();
    expect(storage.values.has("multica_token")).toBe(false);
  });
});
