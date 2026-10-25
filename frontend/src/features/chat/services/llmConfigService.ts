import api from "~/services/api";
import type {
  LlmConfigStatus,
  LlmConfigUpdateInput,
  LlmEndpointSection,
  TavilySection,
} from "../types/llmConfig";

function asRecord(raw: unknown): Record<string, unknown> | null {
  return raw !== null && typeof raw === "object"
    ? (raw as Record<string, unknown>)
    : null;
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function toEndpointSection(raw: unknown): LlmEndpointSection | null {
  const row = asRecord(raw);
  if (!row) return null;
  return {
    base_url: asString(row.base_url),
    model: asString(row.model),
    api_key_masked: asString(row.api_key_masked),
  };
}

function toTavilySection(raw: unknown): TavilySection | null {
  const row = asRecord(raw);
  if (!row) return null;
  return {
    base_url: asString(row.base_url),
    api_key_masked: asString(row.api_key_masked),
  };
}

function toStatus(raw: unknown): LlmConfigStatus {
  const row = asRecord(raw);
  return {
    configured: row?.configured === true,
    llm: toEndpointSection(row?.llm),
    embedding: toEndpointSection(row?.embedding),
    tavily: toTavilySection(row?.tavily),
    fallback: toEndpointSection(row?.fallback),
  };
}

const SENSITIVE_REQUEST = {
  skipSentry: true,
  skipErrorToast: false,
} as const;

export const llmConfigService = {
  async get(): Promise<LlmConfigStatus> {
    const response = await api.get("/me/llm-config", SENSITIVE_REQUEST);
    return toStatus(response.data);
  },

  async update(input: LlmConfigUpdateInput): Promise<LlmConfigStatus> {
    const response = await api.put("/me/llm-config", input, SENSITIVE_REQUEST);
    return toStatus(response.data);
  },

  async remove(): Promise<void> {
    await api.delete("/me/llm-config", SENSITIVE_REQUEST);
  },
};
