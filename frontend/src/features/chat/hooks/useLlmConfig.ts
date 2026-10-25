import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { llmConfigService } from "../services/llmConfigService";
import { useAuthStore } from "~/store/authStore";
import type {
  LlmConfigStatus,
  LlmConfigUpdateInput,
} from "../types/llmConfig";

export const LLM_CONFIG_QUERY_KEY = ["llm-config"] as const;

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

export function useLlmConfig(options: UseLlmConfigOptions = {}) {
  const enabled = options.enabled ?? true;
  const userId = useAuthStore((state) => state.user?.id ?? null);
  const queryClient = useQueryClient();
  const queryKey = [...LLM_CONFIG_QUERY_KEY, userId];

  const statusQuery = useQuery({
    queryKey,
    queryFn: () => llmConfigService.get(),
    enabled: enabled && userId !== null,
  });

  const save = useMutation({
    mutationFn: (input: LlmConfigUpdateInput) =>
      llmConfigService.update(input),
    onSuccess: (data) => {
      queryClient.setQueryData(queryKey, data);
    },
  });

  const remove = useMutation({
    mutationFn: () => llmConfigService.remove(),
    onSuccess: () => {
      queryClient.setQueryData(queryKey, UNCONFIGURED);
    },
  });

  return {
    status: statusQuery.data ?? null,
    configured: statusQuery.data ? statusQuery.data.configured : null,
    isLoading: statusQuery.isLoading,
    isError: statusQuery.isError,
    isUnavailable: isUnavailableError(statusQuery.error),
    refetch: statusQuery.refetch,
    save,
    remove,
  };
}
