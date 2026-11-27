import React, { useState } from "react";
import { useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import api, { tokenStore } from "../../../services/api";
import { useAuthStore } from "../../../store/authStore";
import { useUiStore } from "../../../store/uiStore";
import { logger } from "../../../services/logger";
import { t as translate } from "~/i18n";
import { Button } from "~/components/ui/Button";
import { Input } from "~/components/ui/Input";
import { Label } from "~/components/ui/Label";
import { usePublicConfig } from "../hooks/usePublicConfig";

const authSchema = z.object({
  username: z
    .string()
    .min(3, { error: () => translate("auth.usernameMin") }),
  password: z
    .string()
    .min(6, { error: () => translate("auth.passwordMin") }),
});

type AuthFormData = z.infer<typeof authSchema>;

interface AuthFormProps {
  onSuccess?: () => void;
  onCancel?: () => void;
  required?: boolean;
}

export const AuthForm: React.FC<AuthFormProps> = ({
  onSuccess,
  onCancel,
  required = false,
}) => {
  const { t } = useTranslation();
  const [isLogin, setIsLogin] = useState(true);
  const [remember, setRemember] = useState(true);
  const [serverError, setServerError] = useState<string | null>(null);
  const setAuth = useAuthStore((state) => state.setAuth);
  const addToast = useUiStore((state) => state.addToast);
  const { data: registrationMode } = usePublicConfig();
  const registrationClosed = registrationMode === "closed";
  const inviteOnly = registrationMode === "invite_only";
  const registerMode = !registrationClosed && !isLogin;

  const {
    register,
    handleSubmit,
    formState: { errors },
  } = useForm<AuthFormData>({
    resolver: zodResolver(authSchema),
    defaultValues: {
      username: "",
      password: "",
    },
  });

  const onSubmit = async (data: AuthFormData) => {
    setServerError(null);
    try {
      const endpoint = registerMode ? "/register" : "/login";
      const payload = { username: data.username, password: data.password };
      const response = await api.post(endpoint, payload);
      // Wire the token store (localStorage when "Remember me" is
      // checked, sessionStorage otherwise) AND the authStore
      // profile. setAuth takes only `{user}` (the pick'd
      // PersistedAuthPayload), so the bearer pair is handled
      // separately. Without this call, the wrapped-`api` request
      // interceptor has nothing to stamp onto subsequent requests,
      // AND the IIFE in services/api.ts finds empty storage after
      // F5 — useAuthReconciliation sees no access token and logs
      // out (CLAUDE.md F5 refresh must keep the session alive).
      setAuth(response.data);
      const body = response.data;
      if (body?.access_token && body?.refresh_token) {
        tokenStore.setTokens(body.access_token, body.refresh_token, remember);
      }
      addToast(
        registerMode ? t("auth.registerSuccess") : t("auth.loginSuccess"),
        "success",
      );
      onSuccess?.();
    } catch (err) {
      const status = (err as { response?: { status?: number } }).response?.status;
      if (!registerMode && status === 401) {
        setServerError(t("auth.invalidCredentials"));
      } else if (registerMode && status === 409) {
        setServerError(t("auth.usernameTaken"));
      } else if (registerMode && status === 403) {
        setServerError(t("auth.registrationNotAllowed"));
      } else if (status === 429) {
        setServerError(t("auth.tooManyAttempts"));
      } else {
        setServerError(t("auth.requestFailed"));
      }
      logger.error("Auth error", { error: String(err) });
    }
  };

  return (
    <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
      <div className="space-y-2 text-left">
        <Label htmlFor="username">{t("auth.username")}</Label>
        <Input
          id="username"
          {...register("username", { onChange: () => setServerError(null) })}
          aria-invalid={!!errors.username}
          className={errors.username ? "border-destructive" : ""}
        />
        {errors.username && (
          <p className="text-xs font-medium text-destructive">
            {errors.username.message}
          </p>
        )}
      </div>

      <div className="space-y-2 text-left">
        <Label htmlFor="password">{t("auth.password")}</Label>
        <Input
          id="password"
          type="password"
          {...register("password", { onChange: () => setServerError(null) })}
          aria-invalid={!!errors.password || !!serverError}
          className={errors.password || serverError ? "border-destructive" : ""}
        />
        {errors.password && (
          <p className="text-xs font-medium text-destructive">
            {errors.password.message}
          </p>
        )}
        {serverError && (
          <p role="alert" className="text-xs font-medium text-destructive">
            {serverError}
          </p>
        )}
      </div>

      <div className="flex items-center gap-2">
        <input
          id="remember-me"
          type="checkbox"
          checked={remember}
          onChange={(e) => setRemember(e.target.checked)}
          className="h-4 w-4 rounded border-input accent-primary"
        />
        <Label htmlFor="remember-me" className="text-sm font-normal">
          {t("auth.rememberMe")}
        </Label>
      </div>

      <div className="flex flex-col gap-4 pt-2">
        <div className="flex gap-2 w-full">
          <Button type="submit" className="flex-1">
            {registerMode ? t("auth.register") : t("auth.login")}
          </Button>
          {!required && (
            <Button
              type="button"
              variant="outline"
              onClick={onCancel}
              className="flex-1"
            >
              {t("common.cancel")}
            </Button>
          )}
        </div>
        {registrationClosed ? (
          <p
            className="text-sm text-center text-muted-foreground"
            data-testid="registration-closed-hint"
          >
            {t("auth.registrationClosed")}
          </p>
        ) : (
          <>
            <p className="text-sm text-center text-muted-foreground">
              {isLogin ? t("auth.noAccount") : t("auth.haveAccount")}{" "}
              <button
                type="button"
                className="text-primary font-semibold hover:underline underline-offset-4"
                onClick={() => {
                  setServerError(null);
                  setIsLogin(!isLogin);
                }}
              >
                {isLogin ? t("auth.register") : t("auth.login")}
              </button>
            </p>
            {inviteOnly && !isLogin && (
              <p
                className="text-xs text-center text-muted-foreground"
                data-testid="registration-invite-hint"
              >
                {t("auth.inviteOnlyHint")}
              </p>
            )}
          </>
        )}
      </div>
    </form>
  );
};
