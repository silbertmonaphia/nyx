import { useQuery } from "@tanstack/react-query";
import { PUBLIC_CONFIG_QUERY_KEY } from "~/services/queryKeys";
import { publicConfigService } from "../services/publicConfigService";

export function usePublicConfig() {
  return useQuery({
    queryKey: [PUBLIC_CONFIG_QUERY_KEY],
    queryFn: publicConfigService.get,
    staleTime: 5 * 60_000,
    retry: false,
  });
}
