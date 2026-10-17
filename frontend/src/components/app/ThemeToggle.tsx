import { Moon, Sun } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '~/components/ui/Button';
import { setChoice, useTheme } from '~/theme/theme';

export function ThemeToggle() {
  const { t } = useTranslation();
  const theme = useTheme();
  const next: 'light' | 'dark' = theme === 'dark' ? 'light' : 'dark';
  const label = next === 'dark' ? t('theme.dark') : t('theme.light');

  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      onClick={() => setChoice(next)}
      aria-label={label}
      title={label}
      data-testid="theme-toggle"
      className="hidden md:inline-flex h-9 w-9 shrink-0 text-muted-foreground"
    >
      {theme === 'dark' ? <Sun className="h-5 w-5" /> : <Moon className="h-5 w-5" />}
    </Button>
  );
}
