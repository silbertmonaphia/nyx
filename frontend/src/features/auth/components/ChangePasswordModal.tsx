import React, { useState } from "react";
import { useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import api from "../../../services/api";
import { useAuthStore } from "../../../store/authStore";
import { useUiStore } from "../../../store/uiStore";
import { logger } from "../../../services/logger";
import { t as translate } from "~/i18n";
import { Button } from "~/components/ui/Button";
import { Input } from "~/components/ui/Input";
import { Label } from "~/components/ui/Label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/Dialog";

const changePasswordSchema = z
  .object({
    currentPassword: z
      .string()
      .min(1, { error: () => translate("app.changePasswordDialog.currentPasswordRequired") })
      .max(128, { error: () => translate("app.changePasswordDialog.passwordTooLong") }),
    newPassword: z
      .string()
      .min(6, { error: () => translate("auth.passwordMin") })
      .max(72, { error: () => translate("app.changePasswordDialog.passwordTooLong") }),
    confirmPassword: z
      .string()
      .min(1, { error: () => translate("app.changePasswordDialog.confirmPasswordRequired") }),
  })
  .refine((data) => data.newPassword === data.confirmPassword, {
    error: () => translate("app.changePasswordDialog.passwordMismatch"),
    path: ["confirmPassword"],
  })
  .refine((data) => new TextEncoder().encode(data.newPassword).length <= 72, {
    error: () => translate("app.changePasswordDialog.passwordTooLong"),
    path: ["newPassword"],
  })
  .refine((data) => data.newPassword !== data.currentPassword, {
    error: () => translate("app.changePasswordDialog.samePassword"),
    path: ["newPassword"],
  });

type ChangePasswordFormData = z.infer<typeof changePasswordSchema>;

interface ChangePasswordModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export const ChangePasswordModal: React.FC<ChangePasswordModalProps> = ({
  open,
  onOpenChange,
}) => {
  const { t } = useTranslation();
  const [serverError, setServerError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const logout = useAuthStore((state) => state.logout);
  const addToast = useUiStore((state) => state.addToast);

  const {
    register,
    handleSubmit,
    reset,
    formState: { errors },
  } = useForm<ChangePasswordFormData>({
    resolver: zodResolver(changePasswordSchema),
    defaultValues: {
      currentPassword: "",
      newPassword: "",
      confirmPassword: "",
    },
  });

  const close = () => {
    reset();
    setServerError(null);
    onOpenChange(false);
  };

  const onSubmit = async (data: ChangePasswordFormData) => {
    if (submitting) return;
    setServerError(null);
    setSubmitting(true);
    try {
      await api.post(
        "/change-password",
        {
          current_password: data.currentPassword,
          new_password: data.newPassword,
        },
        { skipErrorToast: true, skipSentry: true },
      );
      addToast(t("app.changePasswordDialog.success"), "success");
      close();
      logout();
    } catch (err) {
      const status = (err as { response?: { status?: number } }).response?.status;
      if (status === 400) {
        setServerError(t("app.changePasswordDialog.currentPasswordWrong"));
      } else if (status === 429) {
        setServerError(t("auth.tooManyAttempts"));
      } else {
        setServerError(t("app.changePasswordDialog.failed"));
      }
      logger.error("Change password error", { error: String(err) });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) close();
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("app.changePasswordDialog.title")}</DialogTitle>
          <DialogDescription>{t("app.changePasswordDialog.description")}</DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
          <div className="space-y-2 text-left">
            <Label htmlFor="current-password">
              {t("app.changePasswordDialog.currentPassword")}
            </Label>
            <Input
              id="current-password"
              type="password"
              autoComplete="current-password"
              disabled={submitting}
              {...register("currentPassword", { onChange: () => setServerError(null) })}
              aria-invalid={!!errors.currentPassword}
              className={errors.currentPassword ? "border-destructive" : ""}
              data-testid="current-password-input"
            />
            {errors.currentPassword && (
              <p className="text-xs font-medium text-destructive">
                {errors.currentPassword.message}
              </p>
            )}
          </div>

          <div className="space-y-2 text-left">
            <Label htmlFor="new-password">{t("app.changePasswordDialog.newPassword")}</Label>
            <Input
              id="new-password"
              type="password"
              autoComplete="new-password"
              disabled={submitting}
              {...register("newPassword", { onChange: () => setServerError(null) })}
              aria-invalid={!!errors.newPassword}
              className={errors.newPassword ? "border-destructive" : ""}
              data-testid="new-password-input"
            />
            {errors.newPassword && (
              <p className="text-xs font-medium text-destructive">
                {errors.newPassword.message}
              </p>
            )}
          </div>

          <div className="space-y-2 text-left">
            <Label htmlFor="confirm-password">
              {t("app.changePasswordDialog.confirmPassword")}
            </Label>
            <Input
              id="confirm-password"
              type="password"
              autoComplete="new-password"
              disabled={submitting}
              {...register("confirmPassword", { onChange: () => setServerError(null) })}
              aria-invalid={!!errors.confirmPassword}
              className={errors.confirmPassword ? "border-destructive" : ""}
              data-testid="confirm-password-input"
            />
            {errors.confirmPassword && (
              <p className="text-xs font-medium text-destructive">
                {errors.confirmPassword.message}
              </p>
            )}
          </div>

          {serverError && (
            <p
              role="alert"
              className="text-xs font-medium text-destructive"
              data-testid="change-password-error"
            >
              {serverError}
            </p>
          )}

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={close}
              disabled={submitting}
            >
              {t("common.cancel")}
            </Button>
            <Button type="submit" disabled={submitting} data-testid="change-password-submit">
              {submitting
                ? t("app.changePasswordDialog.pending")
                : t("app.changePasswordDialog.confirm")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
};
