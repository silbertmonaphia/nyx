import React, { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ChevronDown, KeyRound, User as UserIcon } from "lucide-react";
import { Button } from "~/components/ui/Button";
import { cn } from "~/utils/cn";

interface UserMenuProps {
  username: string;
  onChangePassword: () => void;
}

export const UserMenu: React.FC<UserMenuProps> = ({ username, onChangePassword }) => {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const itemRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    const handlePointerDown = (event: MouseEvent | TouchEvent) => {
      if (event.target instanceof Node && !containerRef.current?.contains(event.target)) {
        setOpen(false);
      }
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("mousedown", handlePointerDown);
    document.addEventListener("touchstart", handlePointerDown);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("mousedown", handlePointerDown);
      document.removeEventListener("touchstart", handlePointerDown);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [open]);

  useEffect(() => {
    if (open) itemRef.current?.focus();
  }, [open]);

  return (
    <div className="relative" ref={containerRef}>
      <Button
        ref={triggerRef}
        type="button"
        variant="outline"
        size="sm"
        className="gap-2"
        aria-label={`${t("nav.userMenu")}: ${username}`}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((value) => !value)}
        data-testid="user-menu-button"
      >
        <UserIcon className="h-4 w-4" />
        <span className="hidden md:inline">{username}</span>
        <ChevronDown className={cn("h-3 w-3 transition-transform", open && "rotate-180")} />
      </Button>
      {open && (
        <div
          role="menu"
          aria-label={t("nav.userMenu")}
          className="absolute right-0 top-full z-50 mt-2 min-w-44 rounded-md border bg-background p-1 shadow-lg"
          data-testid="user-menu"
        >
          <button
            ref={itemRef}
            type="button"
            role="menuitem"
            className="flex w-full items-center gap-2 rounded-sm px-2 py-2 text-sm text-foreground outline-none hover:bg-accent focus:bg-accent"
            onClick={() => {
              setOpen(false);
              onChangePassword();
            }}
            data-testid="user-menu-change-password"
          >
            <KeyRound className="h-4 w-4" />
            {t("nav.changePassword")}
          </button>
        </div>
      )}
    </div>
  );
};
