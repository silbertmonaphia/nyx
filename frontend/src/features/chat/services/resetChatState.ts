import { useChatStore } from "../store/chatStore";

export const resetChatStateForNewUser = (userId: number | null) => {
  useChatStore.getState().resetForUser(userId);
};
