import { cn } from '~/utils/cn';

const ICP_BEIAN_URL = 'https://beian.miit.gov.cn/';

interface SiteFooterProps {
  raised?: boolean;
}

export function SiteFooter({ raised = false }: SiteFooterProps) {
  const beian = import.meta.env.VITE_ICP_BEIAN?.trim();
  if (!beian) return null;

  return (
    <a
      href={ICP_BEIAN_URL}
      target="_blank"
      rel="noopener noreferrer"
      className={cn(
        'pointer-events-auto text-xs text-muted-foreground transition-colors hover:text-foreground hover:underline',
        raised && 'relative z-[60] rounded bg-background px-2 py-1',
      )}
      data-testid="icp-beian-link"
    >
      {beian}
    </a>
  );
}

export default SiteFooter;
