import { beforeEach, describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useAuthStore } from "~/store/authStore";
import { useChatStore } from "../store/chatStore";
import { useChatStateReset } from "./useChatStateReset";

const CURRENT_SESSION_KEY = "nyx.chat.currentSessionId";

function makeUser(id: number, username: string) {
  return {
    id,
    username,
    created_at: "2026-10-14T00:00:00Z",
    updated_at: "2026-10-14T00:00:00Z",
  };
}

function resetStores() {
  useAuthStore.setState({ user: null, isAuthenticated: false });
  useChatStore.setState({
    messages: [],
    isStreaming: false,
    error: null,
    currentSessionId: null,
    ownerUserId: undefined,
    sessionList: [],
    sessionListNextCursor: null,
    isFetchingMore: false,
    runningSessionIds: [],
  });
  localStorage.clear();
  sessionStorage.clear();
}

describe("useChatStateReset", () => {
  beforeEach(resetStores);

  it("keeps the restored tab state when the first identity is adopted", () => {
    useAuthStore.setState({ user: makeUser(1, "alice"), isAuthenticated: true });
    useChatStore.setState({
      currentSessionId: "sess-a",
      messages: [{ role: "user", content: "hello" }],
      sessionList: [
        {
          id: "sess-a",
          user_id: 1,
          title: "",
          started_at: "2026-10-14T00:00:00Z",
          last_active_at: "2026-10-14T00:00:00Z",
          ended_at: null,
          message_count: 1,
        },
      ],
    });

    renderHook(() => useChatStateReset());

    expect(useChatStore.getState().currentSessionId).toBe("sess-a");
    expect(useChatStore.getState().messages).toHaveLength(1);
    expect(useChatStore.getState().sessionList).toHaveLength(1);
  });

  it("clears the panel, rail, and persisted id when another identity signs in", () => {
    useAuthStore.setState({ user: makeUser(1, "alice"), isAuthenticated: true });
    sessionStorage.setItem(CURRENT_SESSION_KEY, "sess-a");
    useChatStore.setState({
      currentSessionId: "sess-a",
      messages: [{ role: "user", content: "alice secret" }],
      sessionList: [
        {
          id: "sess-a",
          user_id: 1,
          title: "alice secret",
          started_at: "2026-10-14T00:00:00Z",
          last_active_at: "2026-10-14T00:00:00Z",
          ended_at: null,
          message_count: 1,
        },
      ],
    });

    renderHook(() => useChatStateReset());

    act(() => {
      useAuthStore.setState({
        user: makeUser(2, "bob"),
        isAuthenticated: true,
      });
    });

    expect(useChatStore.getState().messages).toEqual([]);
    expect(useChatStore.getState().currentSessionId).toBeNull();
    expect(useChatStore.getState().sessionList).toEqual([]);
    expect(sessionStorage.getItem(CURRENT_SESSION_KEY)).toBeNull();
  });

  it("clears the panel when the user logs out", () => {
    useAuthStore.setState({ user: makeUser(1, "alice"), isAuthenticated: true });
    useChatStore.setState({
      currentSessionId: "sess-a",
      messages: [{ role: "user", content: "alice secret" }],
    });

    renderHook(() => useChatStateReset());

    act(() => {
      useAuthStore.setState({ user: null, isAuthenticated: false });
    });

    expect(useChatStore.getState().messages).toEqual([]);
    expect(useChatStore.getState().currentSessionId).toBeNull();
  });
});
