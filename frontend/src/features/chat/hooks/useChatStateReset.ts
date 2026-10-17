import { useEffect } from "react";
import { useAuthStore } from "~/store/authStore";
import { resetChatStateForNewUser } from "../services/resetChatState";

export const useChatStateReset = (): void => {
  const userId = useAuthStore((state) => state.user?.id ?? null);
  useEffect(() => {
    resetChatStateForNewUser(userId);
  }, [userId]);
};
