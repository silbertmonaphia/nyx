import { Moon, Sun } from 'lucide-react';
import { Button } from '~/components/ui/Button';
import { setChoice, useTheme } from '~/theme/theme';

export function ThemeToggle() {
  const theme = useTheme();
  const next: 'light' | 'dark' = theme === 'dark' ? 'light' : 'dark';

  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      onClick={() => setChoice(next)}
      aria-label={`Switch to ${next} theme`}
      title={`Switch to ${next} theme`}
      data-testid="theme-toggle"
      className="hidden md:inline-flex h-9 w-9 shrink-0 text-white/70 hover:bg-white/10 hover:text-white"
    >
      {theme === 'dark' ? <Sun className="h-5 w-5" /> : <Moon className="h-5 w-5" />}
    </Button>
  );
}
