// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import i18next from "@/i18n";
import type { ServerCollaboratorsResponse } from "@/lib/types";
import { ServerAccessCard } from "./ServerAccessCard";

const calls = vi.hoisted(() => ({
  listServerCollaborators: vi.fn(),
  setServerProtection: vi.fn(),
  setServerCollaborator: vi.fn(),
  removeServerCollaborator: vi.fn(),
  reauthGuard: vi.fn((fn: () => Promise<unknown>) => fn()),
}));

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listServerCollaborators: calls.listServerCollaborators,
      setServerProtection: calls.setServerProtection,
      setServerCollaborator: calls.setServerCollaborator,
      removeServerCollaborator: calls.removeServerCollaborator,
    },
  };
});

vi.mock("@/components/ReauthDialog", async (importActual) => {
  const actual = await importActual<typeof import("@/components/ReauthDialog")>();
  return {
    ...actual,
    useReauth: () => ({ guard: calls.reauthGuard, dialog: null }),
  };
});

function sampleResponse(over: Partial<ServerCollaboratorsResponse> = {}): ServerCollaboratorsResponse {
  return {
    server: "survival",
    protected: false,
    collaborators: [
      {
        server_name: "survival",
        user_id: "u-op",
        username: "alice",
        email: "alice@example.com",
        user_role: "admin",
        role: "operator",
        created_at: "2026-11-28T00:00:00Z",
        updated_at: "2026-11-28T00:00:00Z",
      },
    ],
    candidates: [
      { id: "u-op", username: "alice", email: "alice@example.com", role: "admin" },
      { id: "u-2", username: "bob", email: "bob@example.com", role: "admin" },
      { id: "u-3", username: "carol", email: "carol@example.com", role: "user" },
    ],
    ...over,
  };
}

describe("ServerAccessCard", () => {
  beforeEach(async () => {
    Element.prototype.scrollIntoView ??= () => {};
    await i18next.changeLanguage("en-US");
    Object.values(calls).forEach((fn) => fn.mockReset());
    calls.reauthGuard.mockImplementation((fn: () => Promise<unknown>) => fn());
    calls.listServerCollaborators.mockResolvedValue(sampleResponse());
    calls.setServerProtection.mockResolvedValue({ name: "survival", protected: true });
    calls.setServerCollaborator.mockResolvedValue({});
    calls.removeServerCollaborator.mockResolvedValue(undefined);
  });

  afterEach(() => cleanup());

  it("shows the protected switch for Owner and invokes reauth on toggle", async () => {
    const onProtectionChanged = vi.fn();
    render(
      <ServerAccessCard
        serverName="survival"
        isProtected={false}
        isOwner={true}
        onProtectionChanged={onProtectionChanged}
      />,
    );

    const toggle = await screen.findByRole("switch", { name: "Protected server" });
    fireEvent.click(toggle);

    await waitFor(() => expect(calls.setServerProtection).toHaveBeenCalledWith("survival", true));
    expect(calls.reauthGuard).toHaveBeenCalledTimes(1);
    expect(onProtectionChanged).toHaveBeenCalledTimes(1);
  });

  it("hides the protected switch for non-owner Admin and lists collaborators", async () => {
    render(
      <ServerAccessCard
        serverName="survival"
        isProtected={false}
        isOwner={false}
        onProtectionChanged={vi.fn()}
      />,
    );

    expect(await screen.findByText("alice")).toBeTruthy();
    expect(screen.queryByRole("switch", { name: "Protected server" })).toBeNull();
  });

  it("filters candidates, adds a collaborator, updates role, and removes collaborator", async () => {
    render(
      <ServerAccessCard
        serverName="survival"
        isProtected={false}
        isOwner={true}
        onProtectionChanged={vi.fn()}
      />,
    );

    await screen.findByText("alice");

    const filterInput = screen.getByLabelText("Filter accounts by username or email…");
    fireEvent.change(filterInput, { target: { value: "carol" } });

    const picker = screen.getByRole("combobox", { name: "Select an account" });
    fireEvent.click(picker);
    const carolOption = await screen.findByRole("option", { name: /carol/ });
    expect(screen.queryByRole("option", { name: /bob/ })).toBeNull();
    fireEvent.click(carolOption);

    const roleSelect = screen.getByRole("combobox", { name: "Collaborator role" });
    fireEvent.click(roleSelect);
    fireEvent.click(await screen.findByRole("option", { name: "Manager" }));

    fireEvent.click(screen.getByRole("button", { name: "Add collaborator" }));
    await waitFor(() =>
      expect(calls.setServerCollaborator).toHaveBeenCalledWith("survival", "u-3", "manager"),
    );

    const aliceRole = screen.getByRole("combobox", { name: "Change collaborator role for alice" });
    fireEvent.click(aliceRole);
    fireEvent.click(await screen.findByRole("option", { name: "Manager" }));
    await waitFor(() =>
      expect(calls.setServerCollaborator).toHaveBeenCalledWith("survival", "u-op", "manager"),
    );

    fireEvent.click(screen.getByRole("button", { name: "Remove collaborator alice" }));
    await waitFor(() =>
      expect(calls.removeServerCollaborator).toHaveBeenCalledWith("survival", "u-op"),
    );
  });
});
