import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "~/store/authStore";
import { FEATURES_QUERY_KEY } from "~/services/queryKeys";
import { featuresService } from "../services/featuresService";

export function useFeatureFlags() {
  const userId = useAuthStore((state) => state.user?.id ?? null);
  return useQuery({
    queryKey: [FEATURES_QUERY_KEY, userId],
    queryFn: featuresService.get,
    enabled: userId !== null,
    staleTime: 60_000,
  });
}

export function useAgentEnabled(): boolean | undefined {
  return useFeatureFlags().data?.agentEnabled;
}

export function useChatAllowed(): boolean | undefined {
  return useFeatureFlags().data?.chatAllowed;
}
