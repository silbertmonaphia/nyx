import { queryClient } from "~/services/queryClient";
import { LLM_CONFIG_QUERY_KEY } from "~/services/queryKeys";

export const resetLlmConfigStateForNewUser = (currentUserId: number | null) => {
  queryClient.removeQueries({
    predicate: (query) => {
      const [scope, owner] = query.queryKey;
      return scope === LLM_CONFIG_QUERY_KEY && owner !== currentUserId;
    },
  });
};
