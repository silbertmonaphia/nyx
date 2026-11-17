import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "~/store/authStore";
import { FEATURES_QUERY_KEY } from "~/services/queryKeys";
import { featuresService } from "../services/featuresService";

export function useAgentEnabled(): boolean | undefined {
  const userId = useAuthStore((state) => state.user?.id ?? null);
  const query = useQuery({
    queryKey: [FEATURES_QUERY_KEY, userId],
    queryFn: featuresService.get,
    enabled: userId !== null,
    staleTime: 60_000,
  });
  return query.data?.agentEnabled;
}
