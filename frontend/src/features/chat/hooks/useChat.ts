// Re-export the chat store under the `useChat` name components import.
// Mirrors the `useFeedUiStore` convention: feature-local Zustand is
// consumed via a feature-namespaced hook name, no extra wrapper layer.
export { useChatStore as useChat } from "../store/chatStore";
