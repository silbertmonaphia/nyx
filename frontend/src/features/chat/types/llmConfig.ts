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
  fallback: LlmConfigEndpointInput | null;
}
