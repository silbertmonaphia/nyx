import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { llmConfigService } from "../services/llmConfigService";
import { useAuthStore } from "~/store/authStore";
import { LLM_CONFIG_QUERY_KEY } from "~/services/queryKeys";
import type {
  LlmConfigStatus,
  LlmConfigUpdateInput,
} from "../types/llmConfig";

const UNCONFIGURED: LlmConfigStatus = {
  configured: false,
  llm: null,
  embedding: null,
  tavily: null,
  fallback: null,
};

interface UseLlmConfigOptions {
  enabled?: boolean;
}

function isUnavailableError(error: unknown): boolean {
  const status = (error as { response?: { status?: number } })?.response
    ?.status;
  return status === 404 || status === 501;
}

function sameIdentity(capturedUserId: number | null): boolean {
  return (useAuthStore.getState().user?.id ?? null) === capturedUserId;
}

export function useLlmConfig(options: UseLlmConfigOptions = {}) {
  const enabled = options.enabled ?? true;
  const userId = useAuthStore((state) => state.user?.id ?? null);
  const queryClient = useQueryClient();
  const queryKey = [LLM_CONFIG_QUERY_KEY, userId] as const;

  const statusQuery = useQuery({
    queryKey,
    queryFn: () => llmConfigService.get(),
    enabled: enabled && userId !== null,
  });

  const save = useMutation({
    mutationFn: (input: LlmConfigUpdateInput) =>
      llmConfigService.update(input),
    onSuccess: (data) => {
      if (!sameIdentity(userId)) return;
      queryClient.setQueryData(queryKey, data);
    },
  });

  const remove = useMutation({
    mutationFn: () => llmConfigService.remove(),
    onSuccess: () => {
      if (!sameIdentity(userId)) return;
      queryClient.setQueryData(queryKey, UNCONFIGURED);
    },
  });

  return {
    userId,
    status: statusQuery.data ?? null,
    configured: statusQuery.data ? statusQuery.data.configured : null,
    agentReady: statusQuery.data ? statusQuery.data.llm !== null : null,
    isLoading: statusQuery.isLoading,
    isError: statusQuery.isError,
    isUnavailable: isUnavailableError(statusQuery.error),
    refetch: statusQuery.refetch,
    save,
    remove,
  };
}
