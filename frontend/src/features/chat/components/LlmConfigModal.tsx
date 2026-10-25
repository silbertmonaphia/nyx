import React, { useEffect, useMemo, useState } from "react";
import { useForm, type UseFormRegisterReturn } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "~/components/ui/Dialog";
import { Button } from "~/components/ui/Button";
import { Input } from "~/components/ui/Input";
import { Label } from "~/components/ui/Label";
import { useUiStore } from "~/store/uiStore";
import { t as translate } from "~/i18n";
import { useLlmConfig } from "../hooks/useLlmConfig";
import type {
  LlmConfigStatus,
  LlmConfigUpdateInput,
} from "../types/llmConfig";

const EXAMPLE_BASE_URLS = {
  llm: "https://api.deepseek.com/v1",
  embedding: "https://api.siliconflow.cn/v1",
  tavily: "https://api.tavily.com/search",
};

const EXAMPLE_MODELS = {
  llm: "deepseek-flash",
  embedding: "Qwen/Qwen3-Embedding-0.6B",
};

const DOCS_URLS = {
  llm: "https://api-docs.deepseek.com/",
  embedding: "https://docs.siliconflow.cn/",
  tavily: "https://docs.tavily.com/documentation/api-reference/endpoint/search",
};

interface EndpointFormValues {
  base_url: string;
  api_key: string;
  model: string;
}

interface TavilyFormValues {
  base_url: string;
  api_key: string;
}

interface LlmConfigFormValues {
  llm: EndpointFormValues;
  embedding: EndpointFormValues;
  tavily: TavilyFormValues;
}

function isLoopbackUrl(value: string): boolean {
  try {
    const hostname = new URL(value).hostname;
    return (
      hostname === "localhost" ||
      hostname === "::1" ||
      hostname === "[::1]" ||
      /^127\./.test(hostname)
    );
  } catch {
    return false;
  }
}

function isHttpUrl(value: string): boolean {
  try {
    const parsed = new URL(value);
    return (
      (parsed.protocol === "http:" || parsed.protocol === "https:") &&
      parsed.hostname !== "" &&
      parsed.username === "" &&
      parsed.password === ""
    );
  } catch {
    return false;
  }
}

function buildLlmConfigSchema(requireKeys: boolean) {
  const required = () => translate("llmConfig.errors.required");
  const urlError = () => translate("llmConfig.errors.url");
  const requiredBaseUrl = z
    .string()
    .refine((value) => value.trim() !== "", { error: required })
    .refine((value) => value.trim() === "" || isHttpUrl(value.trim()), {
      error: urlError,
    });
  const requiredModel = z
    .string()
    .refine((value) => value.trim() !== "", { error: required });
  const keyRequired = (
    value: { base_url: string; api_key: string },
    ctx: z.RefinementCtx,
  ) => {
    if (!requireKeys) return;
    if (value.api_key.trim() !== "") return;
    if (isLoopbackUrl(value.base_url.trim())) return;
    ctx.addIssue({ code: "custom", path: ["api_key"], message: required() });
  };
  const optionalBaseUrl = z
    .string()
    .refine((value) => value.trim() === "" || isHttpUrl(value.trim()), {
      error: urlError,
    });
  return z.object({
    llm: z
      .object({
        base_url: requiredBaseUrl,
        api_key: z.string(),
        model: requiredModel,
      })
      .superRefine(keyRequired),
    embedding: z
      .object({
        base_url: optionalBaseUrl,
        api_key: z.string(),
        model: z.string(),
      })
      .superRefine((value, ctx) => {
        const url = value.base_url.trim();
        if (url === "") {
          if (value.api_key.trim() !== "" || value.model.trim() !== "") {
            ctx.addIssue({
              code: "custom",
              path: ["base_url"],
              message: required(),
            });
          }
          return;
        }
        if (value.model.trim() === "") {
          ctx.addIssue({
            code: "custom",
            path: ["model"],
            message: required(),
          });
        }
        if (requireKeys && value.api_key.trim() === "" && !isLoopbackUrl(url)) {
          ctx.addIssue({
            code: "custom",
            path: ["api_key"],
            message: required(),
          });
        }
      }),
    tavily: z
      .object({
        base_url: optionalBaseUrl,
        api_key: z.string(),
      })
      .superRefine((value, ctx) => {
        if (value.api_key.trim() !== "" && value.base_url.trim() === "") {
          ctx.addIssue({
            code: "custom",
            path: ["base_url"],
            message: required(),
          });
        }
      }),
  });
}

function emptyEndpoint(): EndpointFormValues {
  return { base_url: "", api_key: "", model: "" };
}

function toFormValues(status: LlmConfigStatus | null): LlmConfigFormValues {
  const endpoint = (section: LlmConfigStatus["llm"]): EndpointFormValues =>
    section
      ? { base_url: section.base_url, api_key: "", model: section.model }
      : emptyEndpoint();
  const tavily = (section: LlmConfigStatus["tavily"]): TavilyFormValues =>
    section ? { base_url: section.base_url, api_key: "" } : { base_url: "", api_key: "" };
  if (!status) {
    return {
      llm: emptyEndpoint(),
      embedding: emptyEndpoint(),
      tavily: { base_url: "", api_key: "" },
    };
  }
  return {
    llm: endpoint(status.llm),
    embedding: endpoint(status.embedding),
    tavily: tavily(status.tavily),
  };
}

function toUpdateInput(values: LlmConfigFormValues): LlmConfigUpdateInput {
  return {
    llm: {
      base_url: values.llm.base_url.trim(),
      api_key: values.llm.api_key.trim(),
      model: values.llm.model.trim(),
    },
    embedding: {
      base_url: values.embedding.base_url.trim(),
      api_key: values.embedding.api_key.trim(),
      model: values.embedding.model.trim(),
    },
    tavily: {
      base_url: values.tavily.base_url.trim(),
      api_key: values.tavily.api_key.trim(),
    },
  };
}

interface ConfigFieldProps {
  id: string;
  label: string;
  placeholder: string;
  type?: "text" | "password";
  error: string | undefined;
  registration: UseFormRegisterReturn;
  disabled?: boolean;
  hint?: string;
}

function ConfigField({
  id,
  label,
  placeholder,
  type = "text",
  error,
  registration,
  disabled = false,
  hint,
}: ConfigFieldProps) {
  return (
    <div className="space-y-1">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        type={type}
        placeholder={placeholder}
        aria-invalid={error ? true : undefined}
        disabled={disabled}
        className={error ? "border-destructive" : disabled ? "opacity-60" : ""}
        {...registration}
      />
      {error ? (
        <p className="text-xs font-medium text-destructive">{error}</p>
      ) : null}
      {hint ? <p className="text-xs font-medium text-destructive">{hint}</p> : null}
    </div>
  );
}

function Guidance({
  lines,
  docs,
}: {
  lines: string[];
  docs?: { href: string; label: string };
}) {
  return (
    <div className="space-y-1">
      {lines.map((line) => (
        <p key={line} className="text-xs text-muted-foreground">
          {line}
        </p>
      ))}
      {docs ? (
        <a
          href={docs.href}
          target="_blank"
          rel="noreferrer"
          className="text-xs underline underline-offset-2"
        >
          {docs.label}
        </a>
      ) : null}
    </div>
  );
}

interface LlmConfigModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  blocking?: boolean;
  onSaved?: () => void;
}

export const LlmConfigModal = ({
  open,
  onOpenChange,
  blocking = false,
  onSaved,
}: LlmConfigModalProps) => {
  const { t } = useTranslation();
  const addToast = useUiStore((state) => state.addToast);
  const { status, save, remove } = useLlmConfig({ enabled: open });
  const configured = status?.configured === true;
  const schema = useMemo(() => buildLlmConfigSchema(!configured), [configured]);
  const {
    register,
    handleSubmit,
    reset,
    formState: { errors, isDirty },
  } = useForm<LlmConfigFormValues>({
    resolver: zodResolver(schema),
    defaultValues: toFormValues(null),
    mode: "onSubmit",
  });
  const [embeddingOverride, setEmbeddingOverride] = useState<boolean | null>(
    null,
  );
  const [tavilyOverride, setTavilyOverride] = useState<boolean | null>(null);
  const embeddingError = Boolean(
    errors.embedding?.base_url ||
      errors.embedding?.api_key ||
      errors.embedding?.model,
  );
  const showEmbedding =
    embeddingOverride ?? (status?.embedding != null || embeddingError);
  const tavilyError = Boolean(errors.tavily?.base_url || errors.tavily?.api_key);
  const showTavily =
    tavilyOverride ?? (status?.tavily != null || tavilyError);
  const [confirmClearOpen, setConfirmClearOpen] = useState(false);

  useEffect(() => {
    if (!open) return;
    if (isDirty) return;
    reset(toFormValues(status));
  }, [open, status, reset, isDirty]);

  const keyPlaceholder = (
    section: { api_key_masked: string } | null | undefined,
  ): string =>
    section
      ? t("llmConfig.fields.keySaved", { masked: section.api_key_masked })
      : t("llmConfig.fields.keyPlaceholder");

  const submit = handleSubmit(async (values) => {
    try {
      await save.mutateAsync(toUpdateInput(values));
      addToast(t("llmConfig.toasts.saved"), "success");
      onSaved?.();
      onOpenChange(false);
    } catch (err) {
      if (!(err as { response?: unknown })?.response) {
        addToast(t("llmConfig.toasts.failed"), "error");
      }
    }
  });

  const executeClear = async () => {
    try {
      await remove.mutateAsync();
      addToast(t("llmConfig.toasts.cleared"), "success");
      setConfirmClearOpen(false);
      onOpenChange(false);
    } catch (err) {
      if (!(err as { response?: unknown })?.response) {
        addToast(t("llmConfig.toasts.failed"), "error");
      }
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="max-h-[85vh] overflow-y-auto sm:max-w-2xl"
        hideClose={blocking}
        onEscapeKeyDown={
          blocking ? (event) => event.preventDefault() : undefined
        }
        onPointerDownOutside={
          blocking ? (event) => event.preventDefault() : undefined
        }
        onInteractOutside={
          blocking ? (event) => event.preventDefault() : undefined
        }
      >
        <DialogHeader>
          <DialogTitle>
            {blocking
              ? t("llmConfig.gate.title")
              : t("llmConfig.title")}
          </DialogTitle>
          <DialogDescription>
            {blocking
              ? t("llmConfig.gate.description")
              : t("llmConfig.description")}
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={submit} className="space-y-6" noValidate>
          <fieldset className="space-y-3 rounded-md border p-4">
            <div className="text-sm font-semibold">
              {t("llmConfig.sections.llm.title")}
            </div>
            <Guidance
              lines={[
                t("llmConfig.sections.llm.drives"),
                t("llmConfig.sections.llm.capabilities"),
                t("llmConfig.sections.llm.credentials"),
                t("llmConfig.sections.llm.urlConvention"),
                t("llmConfig.sections.llm.modelNote"),
              ]}
              docs={{
                href: DOCS_URLS.llm,
                label: t("llmConfig.sections.llm.docsLabel"),
              }}
            />
            <ConfigField
              id="llm-base-url"
              label={t("llmConfig.fields.baseUrl")}
              placeholder={EXAMPLE_BASE_URLS.llm}
              error={errors.llm?.base_url?.message}
              registration={register("llm.base_url")}
            />
            <div className="grid gap-3 sm:grid-cols-2">
              <ConfigField
                id="llm-api-key"
                label={t("llmConfig.fields.apiKey")}
                placeholder={keyPlaceholder(status?.llm)}
                type="password"
                error={errors.llm?.api_key?.message}
                registration={register("llm.api_key")}
              />
              <ConfigField
                id="llm-model"
                label={t("llmConfig.fields.model")}
                placeholder={EXAMPLE_MODELS.llm}
                error={errors.llm?.model?.message}
                registration={register("llm.model")}
              />
            </div>
          </fieldset>

          <fieldset className="space-y-3 rounded-md border border-dashed p-4">
            <div className="flex items-center justify-between gap-2">
              <div className="text-sm font-semibold text-muted-foreground">
                {t("llmConfig.sections.embedding.title")}
              </div>
              <button
                type="button"
                aria-expanded={showEmbedding}
                data-testid="llm-embedding-toggle"
                onClick={() => setEmbeddingOverride(!showEmbedding)}
                className="text-xs font-medium text-primary hover:underline"
              >
                {showEmbedding
                  ? t("llmConfig.sections.embedding.collapse")
                  : t("llmConfig.sections.embedding.expand")}
              </button>
            </div>
            <p className="text-xs font-medium text-destructive">
              {t("llmConfig.sections.embedding.impact")}
            </p>
            {status?.embedding != null ? (
              <p className="text-xs font-medium text-destructive">
                {t("llmConfig.sections.embedding.removeHint")}
              </p>
            ) : null}
            {showEmbedding ? (
              <div className="space-y-3">
                <Guidance
                  lines={[
                    t("llmConfig.sections.embedding.drives"),
                    t("llmConfig.sections.embedding.separateProvider"),
                    t("llmConfig.sections.embedding.credentials"),
                    t("llmConfig.sections.embedding.urlConvention"),
                  ]}
                  docs={{
                    href: DOCS_URLS.embedding,
                    label: t("llmConfig.sections.embedding.docsLabel"),
                  }}
                />
                <ConfigField
                  id="embedding-base-url"
                  label={t("llmConfig.fields.baseUrl")}
                  placeholder={EXAMPLE_BASE_URLS.embedding}
                  error={errors.embedding?.base_url?.message}
                  registration={register("embedding.base_url")}
                />
                <div className="grid gap-3 sm:grid-cols-2">
                  <ConfigField
                    id="embedding-api-key"
                    label={t("llmConfig.fields.apiKey")}
                    placeholder={keyPlaceholder(status?.embedding)}
                    type="password"
                    error={errors.embedding?.api_key?.message}
                    registration={register("embedding.api_key")}
                  />
                  <ConfigField
                    id="embedding-model"
                    label={t("llmConfig.fields.model")}
                    placeholder={EXAMPLE_MODELS.embedding}
                    error={errors.embedding?.model?.message}
                    registration={register("embedding.model")}
                    disabled={status?.embedding != null}
                    hint={
                      status?.embedding != null
                        ? `${t("llmConfig.sections.embedding.modelRequirement")} ${t("llmConfig.sections.embedding.modelLocked")}`
                        : t("llmConfig.sections.embedding.modelRequirement")
                    }
                  />
                </div>
              </div>
            ) : null}
          </fieldset>

          <fieldset className="space-y-3 rounded-md border border-dashed p-4">
            <div className="flex items-center justify-between gap-2">
              <div className="text-sm font-semibold text-muted-foreground">
                {t("llmConfig.sections.tavily.title")}
              </div>
              <button
                type="button"
                aria-expanded={showTavily}
                data-testid="llm-tavily-toggle"
                onClick={() => setTavilyOverride(!showTavily)}
                className="text-xs font-medium text-primary hover:underline"
              >
                {showTavily
                  ? t("llmConfig.sections.tavily.collapse")
                  : t("llmConfig.sections.tavily.expand")}
              </button>
            </div>
            <p className="text-xs font-medium text-destructive">
              {t("llmConfig.sections.tavily.impact")}
            </p>
            {showTavily ? (
              <div className="space-y-3">
                <Guidance
                  lines={[
                    t("llmConfig.sections.tavily.drives"),
                    t("llmConfig.sections.tavily.credentials"),
                    t("llmConfig.sections.tavily.shapeNote"),
                  ]}
                  docs={{
                    href: DOCS_URLS.tavily,
                    label: t("llmConfig.sections.tavily.docsLabel"),
                  }}
                />
                <ConfigField
                  id="tavily-base-url"
                  label={t("llmConfig.fields.baseUrl")}
                  placeholder={EXAMPLE_BASE_URLS.tavily}
                  error={errors.tavily?.base_url?.message}
                  registration={register("tavily.base_url")}
                />
                <ConfigField
                  id="tavily-api-key"
                  label={t("llmConfig.fields.apiKey")}
                  placeholder={keyPlaceholder(status?.tavily)}
                  type="password"
                  error={errors.tavily?.api_key?.message}
                  registration={register("tavily.api_key")}
                />
              </div>
            ) : null}
          </fieldset>

          <p className="text-xs text-muted-foreground">
            {t("llmConfig.sections.agentNote")}
          </p>

          <DialogFooter className="flex-row items-center justify-between gap-2 sm:justify-between">
            {configured ? (
              <Button
                type="button"
                variant="destructive"
                onClick={() => setConfirmClearOpen(true)}
                disabled={remove.isPending}
                data-testid="llm-config-clear"
              >
                {remove.isPending
                  ? t("llmConfig.actions.clearing")
                  : t("llmConfig.actions.clear")}
              </Button>
            ) : (
              <span />
            )}
            <Button
              type="submit"
              disabled={save.isPending}
              data-testid="llm-config-save"
            >
              {save.isPending
                ? t("llmConfig.actions.saving")
                : t("llmConfig.actions.save")}
            </Button>
          </DialogFooter>
        </form>

        <Dialog open={confirmClearOpen} onOpenChange={setConfirmClearOpen}>
          <DialogContent className="sm:max-w-md">
            <DialogHeader>
              <DialogTitle>{t("llmConfig.confirm.title")}</DialogTitle>
              <DialogDescription>
                {t("llmConfig.confirm.description")}
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => setConfirmClearOpen(false)}
                disabled={remove.isPending}
              >
                {t("common.cancel")}
              </Button>
              <Button
                type="button"
                variant="destructive"
                onClick={executeClear}
                disabled={remove.isPending}
                data-testid="confirm-llm-clear"
              >
                {remove.isPending
                  ? t("llmConfig.actions.clearing")
                  : t("llmConfig.confirm.confirm")}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </DialogContent>
    </Dialog>
  );
};
