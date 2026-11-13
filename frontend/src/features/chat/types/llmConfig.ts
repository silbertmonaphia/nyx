export interface LlmEndpointSection {
  base_url: string;
  model: string;
  api_key_masked: string;
}

export interface TavilySection {
  base_url: string;
  api_key_masked: string;
}

export interface LlmConfigStatus {
  configured: boolean;
  llm: LlmEndpointSection | null;
  embedding: LlmEndpointSection | null;
  tavily: TavilySection | null;
  fallback: LlmEndpointSection | null;
}

export interface LlmConfigEndpointInput {
  base_url: string;
  api_key: string;
  model: string;
}

export interface LlmConfigTavilyInput {
  base_url: string;
  api_key: string;
}

export interface LlmConfigUpdateInput {
  llm: LlmConfigEndpointInput;
  embedding: LlmConfigEndpointInput;
  tavily: LlmConfigTavilyInput;
  fallback?: LlmConfigEndpointInput | null;
}

export type LlmSlotName = "chat" | "embedding" | "fallback" | "tavily";

export type LlmSlotProbeStatus =
  | "ok"
  | "auth_failed"
  | "rate_limited"
  | "invalid_model"
  | "invalid_config"
  | "width_mismatch"
  | "unreachable"
  | "timeout";

export interface LlmSlotProbeRequest {
  method: string;
  url: string;
  headers: Record<string, string>;
  body?: unknown;
}

export interface LlmSlotProbeResult {
  probed: boolean;
  status?: LlmSlotProbeStatus;
  http_status?: number;
  detail?: string;
  request?: LlmSlotProbeRequest;
}

export interface LlmConfigValidateInput {
  llm?: Partial<LlmConfigEndpointInput>;
  embedding?: Partial<LlmConfigEndpointInput>;
  tavily?: Partial<LlmConfigTavilyInput>;
  fallback?: Partial<LlmConfigEndpointInput>;
  slots?: LlmSlotName[];
}

export interface LlmConfigValidateResponse {
  slots: {
    chat: LlmSlotProbeResult | null;
    embedding: LlmSlotProbeResult | null;
    fallback: LlmSlotProbeResult | null;
    tavily: LlmSlotProbeResult | null;
  };
}
