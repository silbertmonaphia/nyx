import React from 'react';
import { useUiStore } from '../../store/uiStore';
import { Button } from './Button';
import { X, CheckCircle, AlertCircle, Info } from 'lucide-react';
import { cn } from '~/utils/cn';

export const ToastContainer: React.FC = () => {
  const { toasts, removeToast } = useUiStore();

  if (toasts.length === 0) return null;

  return (
    <div className="fixed top-4 right-4 z-[100] flex flex-col gap-2 pointer-events-none w-full max-w-sm">
      {toasts.map((toast) => (
        <div
          key={toast.id}
          className={cn(
            "pointer-events-auto flex items-center justify-between gap-3 p-4 rounded-lg shadow-lg border animate-in slide-in-from-right-full duration-300",
            toast.type === 'error' && "bg-destructive text-destructive-foreground border-destructive/50",
            toast.type === 'success' && "bg-green-600 text-white border-green-500",
            toast.type === 'info' && "bg-primary text-primary-foreground border-primary/50"
          )}
        >
          <div className="flex items-center gap-3">
            {toast.type === 'error' && <AlertCircle className="h-5 w-5 shrink-0" />}
            {toast.type === 'success' && <CheckCircle className="h-5 w-5 shrink-0" />}
            {toast.type === 'info' && <Info className="h-5 w-5 shrink-0" />}
            <span className="text-sm font-medium">{toast.message}</span>
          </div>
          <Button
            variant="ghost"
            size="icon"
            className="h-6 w-6 text-current hover:bg-black/10 shrink-0"
            onClick={() => removeToast(toast.id)}
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
      ))}
    </div>
  );
};
