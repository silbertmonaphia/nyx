import api from "~/services/api";

export type RegistrationMode = "open" | "invite_only" | "closed";

const PUBLIC_CONFIG_SILENT_OPTIONS = { skipErrorToast: true, skipSentry: true };

export const publicConfigService = {
  async get(): Promise<RegistrationMode | null> {
    const response = await api.get("/config", PUBLIC_CONFIG_SILENT_OPTIONS);
    const row = response.data as Record<string, unknown> | null;
    const mode = row?.registration_mode;
    if (mode === "open" || mode === "invite_only" || mode === "closed") {
      return mode;
    }
    return null;
  },
};
