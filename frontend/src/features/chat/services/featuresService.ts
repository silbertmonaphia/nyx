import api from "~/services/api";

export interface FeatureFlags {
  agentEnabled: boolean;
  chatAllowed?: boolean;
}

const FEATURES_SILENT_OPTIONS = { skipErrorToast: true, skipSentry: true };

export const featuresService = {
  async get(): Promise<FeatureFlags> {
    const response = await api.get("/features", FEATURES_SILENT_OPTIONS);
    const row = response.data as Record<string, unknown> | null;
    return {
      agentEnabled: row?.agent_enabled === true,
      chatAllowed:
        typeof row?.chat_allowed === "boolean" ? row.chat_allowed : undefined,
    };
  },
};
